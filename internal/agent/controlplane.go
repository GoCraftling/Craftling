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

// CPClient is the agent's view of the control plane: it registers the host and
// keeps it alive via heartbeats (the P1 agent endpoints). This is the "push
// status up" half of the control-plane-authoritative model; the control plane
// pushes desired state back down via the agent VM API.
type CPClient struct {
	http    *http.Client
	baseURL string
}

// NewCPClient constructs a control-plane client for the given base URL
// (e.g. "http://control-plane:8080").
func NewCPClient(baseURL string, httpClient *http.Client) *CPClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &CPClient{http: httpClient, baseURL: strings.TrimRight(baseURL, "/")}
}

// RegisterRequest is the host registration payload. ID is the agent's own stable
// id; supplying it keeps the host's identity stable across a control-plane
// restart (P1 agent-owned ids).
type RegisterRequest struct {
	ID            string `json:"id,omitempty"`
	Hostname      string `json:"hostname"`
	Address       string `json:"address"`
	Zone          string `json:"zone,omitempty"`
	CPUsTotal     int    `json:"cpus_total"`
	MemoryMBTotal int    `json:"memory_mb_total"`
	AgentVersion  string `json:"agent_version,omitempty"`
}

// Register registers (or re-registers) this host and returns its assigned id.
func (c *CPClient) Register(ctx context.Context, req RegisterRequest) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal register: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/agent/hosts/register", bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("build register request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("register: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("register: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var host struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &host); err != nil {
		return "", fmt.Errorf("decode register response: %w", err)
	}
	return host.ID, nil
}

// Heartbeat reports liveness for the host. It returns found=false when the
// control plane returns 404 (it has forgotten this host), signalling the agent
// to re-register.
func (c *CPClient) Heartbeat(ctx context.Context, id string) (found bool, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/agent/hosts/"+id+"/heartbeat", nil)
	if err != nil {
		return false, fmt.Errorf("build heartbeat request: %w", err)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return false, fmt.Errorf("heartbeat: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("heartbeat: status %d", resp.StatusCode)
	}
}
