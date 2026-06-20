// Package registry is the control plane's client for the game-server template
// registry (the "marketplace"). It fetches a JSON index of available templates
// and, on demand, the full manifest for a single template — caching both for a
// short TTL so the upstream (e.g. raw.githubusercontent.com) isn't hammered by
// the frontend's polling.
//
// Manifests are returned as raw bytes and passed straight through to the client:
// the control plane only needs the index's id→url mapping to resolve a request,
// so it never parses (and never risks dropping fields from) the manifest itself.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a requested template id is absent from the index.
var ErrNotFound = errors.New("template not found")

// ErrInvalidAnswer is returned by Resolve when the operator's answers don't
// satisfy a template's variables (a select variable is missing or out of range).
var ErrInvalidAnswer = errors.New("invalid template answer")

// maxBodyBytes caps how much we read from the upstream, guarding against a
// hostile or runaway registry response.
const maxBodyBytes = 1 << 20 // 1 MiB

// TemplateSummary is one entry in the registry index.
type TemplateSummary struct {
	TemplateID   string `json:"template_id"`
	TemplateName string `json:"template_name"`
	ThumbnailURL string `json:"thumbnail_url"`
	TemplateURL  string `json:"template_url"`
}

// Variable is one question a template asks the operator before launch. An empty
// AcceptableAnswers means free text; a non-empty list constrains the answer to
// one of its values.
type Variable struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	AcceptableAnswers []string `json:"acceptable_answers"`
}

// Manifest is the full, parsed template definition. The control plane decodes it
// (server-side, from the trusted index) to resolve a launch into an image ref and
// a concrete environment — clients never supply the image or raw env directly.
type Manifest struct {
	ImageName    string            `json:"image_name"`
	ImageTag     string            `json:"image_tag"`
	TemplateName string            `json:"template_name"`
	ThumbnailURL string            `json:"thumbnail_url"`
	EULANeeded   bool              `json:"eula_needed"`
	GuestVolumes []string          `json:"guest_volumes"`
	Variables    []Variable        `json:"variables"`
	Env          map[string]string `json:"env"`
}

// placeholderRE matches a $VarName$ token in a manifest env value, mirroring the
// frontend's resolveEnv so server- and client-side previews agree.
var placeholderRE = regexp.MustCompile(`\$([A-Za-z0-9_]+)\$`)

// Client fetches the registry index and per-template manifests, caching each for
// a short TTL. It is safe for concurrent use.
type Client struct {
	indexURL string
	http     *http.Client
	ttl      time.Duration
	now      func() time.Time

	mu        sync.Mutex
	index     []TemplateSummary
	indexAt   time.Time
	manifests map[string]cachedManifest
}

type cachedManifest struct {
	body []byte
	at   time.Time
}

// New constructs a registry Client for the given index URL. If httpClient is nil
// a default with a 10s timeout is used.
func New(indexURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{
		indexURL:  indexURL,
		http:      httpClient,
		ttl:       60 * time.Second,
		now:       time.Now,
		manifests: make(map[string]cachedManifest),
	}
}

// Index returns the list of templates, served from cache when fresh.
func (c *Client) Index(ctx context.Context) ([]TemplateSummary, error) {
	c.mu.Lock()
	if c.index != nil && c.now().Sub(c.indexAt) < c.ttl {
		idx := c.index
		c.mu.Unlock()
		return idx, nil
	}
	c.mu.Unlock()

	body, err := c.fetch(ctx, c.indexURL)
	if err != nil {
		return nil, fmt.Errorf("fetch index: %w", err)
	}
	var idx []TemplateSummary
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("decode index: %w", err)
	}

	c.mu.Lock()
	c.index, c.indexAt = idx, c.now()
	c.mu.Unlock()
	return idx, nil
}

// Manifest returns the raw JSON manifest for the template with the given id. The
// template's URL is resolved from the trusted index (never taken from the
// caller), so this cannot be used to fetch arbitrary URLs.
func (c *Client) Manifest(ctx context.Context, id string) ([]byte, error) {
	c.mu.Lock()
	if m, ok := c.manifests[id]; ok && c.now().Sub(m.at) < c.ttl {
		body := m.body
		c.mu.Unlock()
		return body, nil
	}
	c.mu.Unlock()

	idx, err := c.Index(ctx)
	if err != nil {
		return nil, err
	}
	var url string
	for _, t := range idx {
		if t.TemplateID == id {
			url = t.TemplateURL
			break
		}
	}
	if url == "" {
		return nil, ErrNotFound
	}

	body, err := c.fetch(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}

	c.mu.Lock()
	c.manifests[id] = cachedManifest{body: body, at: c.now()}
	c.mu.Unlock()
	return body, nil
}

// ManifestParsed returns the decoded manifest for the template with the given
// id, fetched (and cached) through Manifest. ErrNotFound for an unknown id.
func (c *Client) ManifestParsed(ctx context.Context, id string) (*Manifest, error) {
	body, err := c.Manifest(ctx, id)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("decode manifest %q: %w", id, err)
	}
	return &m, nil
}

// Resolve turns a template manifest plus the operator's answers into the concrete
// inputs a server needs: the resolved environment (every $VarName$ in the
// manifest env replaced with its answer) and the OCI image ref. It validates that
// each select variable (one with a fixed answer set) has an answer drawn from that
// set; free-text variables are optional. Unknown answer keys are ignored.
func Resolve(m *Manifest, answers map[string]string) (env map[string]string, imageRef string, err error) {
	for _, v := range m.Variables {
		if len(v.AcceptableAnswers) == 0 {
			continue // free text: any value (or none) is fine
		}
		ans, ok := answers[v.Name]
		if !ok {
			return nil, "", fmt.Errorf("%w: missing answer for %q", ErrInvalidAnswer, v.Name)
		}
		if !contains(v.AcceptableAnswers, ans) {
			return nil, "", fmt.Errorf("%w: %q is not an accepted value for %q", ErrInvalidAnswer, ans, v.Name)
		}
	}

	env = make(map[string]string, len(m.Env))
	for key, val := range m.Env {
		env[key] = placeholderRE.ReplaceAllStringFunc(val, func(tok string) string {
			name := tok[1 : len(tok)-1] // strip the surrounding $…$
			if a, ok := answers[name]; ok {
				return a
			}
			return tok // leave an unanswered placeholder untouched
		})
	}

	imageRef = m.ImageName
	if m.ImageTag != "" {
		imageRef += ":" + m.ImageTag
	}
	return env, imageRef, nil
}

// contains reports whether s is in xs.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// SortedEnv flattens an env map into deterministic "KEY=VALUE" entries, sorted by
// key. It is the form the agent's run spec carries.
func SortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// fetch GETs url and returns its body, enforcing a 2xx status and a size cap.
func (c *Client) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	return body, nil
}
