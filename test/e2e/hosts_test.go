//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
)

// adminHostByID returns the fleet host with the given id from the admin view,
// or nil if absent.
func adminHostByID(t *testing.T, adminToken, id string) map[string]any {
	t.Helper()
	resp, body := get(t, "/api/v1/admin/hosts", adminToken)
	if resp.StatusCode != 200 {
		t.Fatalf("list hosts status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Hosts []map[string]any `json:"hosts"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode hosts: %v", err)
	}
	for _, h := range out.Hosts {
		if h["id"] == id {
			return h
		}
	}
	return nil
}

// waitForHostStatus polls the admin fleet view until host id reaches want.
func waitForHostStatus(t *testing.T, adminToken, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h := adminHostByID(t, adminToken, id); h != nil && h["status"] == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("host %s did not reach status %q within timeout", id, want)
}

// TestHostFleetLifecycle exercises a host's liveness over the gRPC stream:
// connecting registers it ready, dropping the stream marks it down, and
// reconnecting brings it back — the stream is the host's liveness signal now
// that the HTTP register/heartbeat endpoints are gone.
func TestHostFleetLifecycle(t *testing.T) {
	admin := makeAdmin(t, "host-fleet-admin@example.com", "hunter2pass")

	const id = "44444444-4444-4444-4444-444444444444"
	info := agent.LinkInfo{
		ID: id, Hostname: "host-lifecycle", Zone: "zone-a",
		CPUsTotal: 8, MemoryMBTotal: 16384, AgentVersion: "0.1.0",
	}

	// Connecting the agent registers the host as ready.
	stop := startAgent(t, info)
	waitForHostStatus(t, admin.AccessToken, id, "ready")

	h := adminHostByID(t, admin.AccessToken, id)
	if h == nil {
		t.Fatalf("host not present in admin fleet view after connect")
	}
	// Allocatable capacity is initialised to total on a fresh registration.
	if h["cpus_allocatable"] != h["cpus_total"] {
		t.Errorf("cpus_allocatable = %v, want %v", h["cpus_allocatable"], h["cpus_total"])
	}
	if h["memory_mb_allocatable"] != h["memory_mb_total"] {
		t.Errorf("memory_mb_allocatable = %v, want %v", h["memory_mb_allocatable"], h["memory_mb_total"])
	}

	// Dropping the stream marks the host down.
	stop()
	waitForHostStatus(t, admin.AccessToken, id, "down")

	// Reconnecting brings a downed host back to ready.
	stop2 := startAgent(t, info)
	defer stop2()
	waitForHostStatus(t, admin.AccessToken, id, "ready")
}

// TestRegisterWithAgentSuppliedID verifies the agent-owned id is the
// authoritative key on the stream: a reconnect under the same id updates the
// existing fleet record in place (here, with changed capacity) rather than
// minting a new one — the basis for host identity surviving a reconnect or a
// control-plane restart.
func TestRegisterWithAgentSuppliedID(t *testing.T) {
	admin := makeAdmin(t, "agent-id-admin@example.com", "hunter2pass")

	const id = "11111111-1111-1111-1111-111111111111"

	stop := startAgent(t, agent.LinkInfo{
		ID: id, Hostname: "host-owned-id", CPUsTotal: 4, MemoryMBTotal: 8192,
	})
	waitForHostStatus(t, admin.AccessToken, id, "ready")
	if h := adminHostByID(t, admin.AccessToken, id); h == nil || h["id"] != id {
		t.Fatalf("host not registered under agent-supplied id %s: %v", id, h)
	}

	// Disconnect, then reconnect under the same id with changed capacity.
	stop()
	waitForHostStatus(t, admin.AccessToken, id, "down")

	stop2 := startAgent(t, agent.LinkInfo{
		ID: id, Hostname: "host-owned-id", CPUsTotal: 16, MemoryMBTotal: 32768,
	})
	defer stop2()
	waitForHostStatus(t, admin.AccessToken, id, "ready")

	h := adminHostByID(t, admin.AccessToken, id)
	if h == nil {
		t.Fatalf("host vanished after reconnect")
	}
	if h["cpus_total"].(float64) != 16 {
		t.Errorf("cpus_total = %v, want 16 (updated in place on reconnect)", h["cpus_total"])
	}
	if h["memory_mb_total"].(float64) != 32768 {
		t.Errorf("memory_mb_total = %v, want 32768 (updated in place on reconnect)", h["memory_mb_total"])
	}
}
