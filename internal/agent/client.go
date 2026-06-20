package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client calls an agent's VM API. One Client is shared across all agents; the
// target agent's base URL is passed per call (resolved from the host inventory),
// since the control plane talks to many hosts.
type Client struct {
	http *http.Client
}

// NewClient constructs a Client over the given HTTP client (supply one with a
// sensible timeout). A nil httpClient falls back to http.DefaultClient.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient}
}

// BaseURL normalizes a host address (e.g. "10.0.0.1:9000") into an agent base
// URL, defaulting to http:// when no scheme is present.
func BaseURL(address string) string {
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return strings.TrimRight(address, "/")
	}
	return "http://" + strings.TrimRight(address, "/")
}

// Provision asks the agent to create and boot a VM for the spec.
func (c *Client) Provision(ctx context.Context, baseURL string, spec VMSpec) (*VM, error) {
	return c.doVM(ctx, http.MethodPost, baseURL+"/vms", spec)
}

// Start asks the agent to boot an existing VM.
func (c *Client) Start(ctx context.Context, baseURL, vmID string) (*VM, error) {
	return c.doVM(ctx, http.MethodPost, baseURL+"/vms/"+vmID+"/start", nil)
}

// Stop asks the agent to halt a VM.
func (c *Client) Stop(ctx context.Context, baseURL, vmID string) error {
	_, err := c.doVM(ctx, http.MethodPost, baseURL+"/vms/"+vmID+"/stop", nil)
	return err
}

// Snapshot asks the agent to take an on-demand world snapshot of a running VM.
func (c *Client) Snapshot(ctx context.Context, baseURL, vmID string) error {
	_, err := c.doVM(ctx, http.MethodPost, baseURL+"/vms/"+vmID+"/snapshot", nil)
	return err
}

// Deprovision asks the agent to destroy a VM.
func (c *Client) Deprovision(ctx context.Context, baseURL, vmID string) error {
	_, err := c.doVM(ctx, http.MethodDelete, baseURL+"/vms/"+vmID, nil)
	return err
}

// Status fetches a VM's observed state.
func (c *Client) Status(ctx context.Context, baseURL, vmID string) (*VM, error) {
	return c.doVM(ctx, http.MethodGet, baseURL+"/vms/"+vmID, nil)
}

// doVM performs an agent request and decodes a VM body when one is returned.
// Endpoints that reply with a plain {"status":"ok"} simply yield a nil VM and a
// nil error.
func (c *Client) doVM(ctx context.Context, method, url string, body any) (*VM, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call agent: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agent %s %s: status %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(data)))
	}

	// Lifecycle replies carry a VM; control replies ({"status":"ok"}) do not.
	var vm VM
	if err := json.Unmarshal(data, &vm); err != nil || vm.ID == "" {
		return nil, nil
	}
	return &vm, nil
}
