//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// doJSON issues a request with an optional JSON body and Bearer token.
func doJSON(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp, readBody(t, resp)
}

// createServerID creates a server for the given token and returns its id.
func createServerID(t *testing.T, token, name string) string {
	t.Helper()
	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", token, map[string]any{
		"name": name, "version": "1.20.4",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode, body)
	}
	id, _ := decodeServer(t, body)["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %s", body)
	}
	return id
}

func decodeServer(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode server: %v (body=%s)", err, body)
	}
	return m
}

// waitForStatus polls a server until it reaches want, or fails after timeout.
func waitForStatus(t *testing.T, token, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, body := get(t, "/api/v1/servers/"+id, token)
		if resp.StatusCode == http.StatusOK {
			s := decodeServer(t, body)
			if s["status"] == want {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server %s did not reach status %q within timeout", id, want)
	return nil
}

// waitForGone polls a server until it returns 404.
func waitForGone(t *testing.T, token, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, _ := get(t, "/api/v1/servers/"+id, token)
		if resp.StatusCode == http.StatusNotFound {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server %s was not deleted within timeout", id)
}

// TestServerBackupRequest exercises the on-demand backup flow (P5): the API
// records intent, and the reconciler — the sole writer of compute side effects —
// snapshots via the agent and clears the flag. With the in-process FakeRuntime
// agent the snapshot is a no-op, so the observable effect is backup_requested
// flipping back to false with last_backup_at stamped.
func TestServerBackupRequest(t *testing.T) {
	user := registerUser(t, "backup-user@example.com", "hunter2pass")
	tok := user.AccessToken

	id := createServerID(t, tok, "backup-world")
	running := waitForStatus(t, tok, id, "running")
	if running["backup_requested"] != false {
		t.Fatalf("new running server backup_requested = %v, want false", running["backup_requested"])
	}

	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers/"+id+"/snapshot", tok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("request backup status = %d, body = %s", resp.StatusCode, body)
	}

	// The reconciler should perform the backup and clear the flag shortly.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := get(t, "/api/v1/servers/"+id, tok)
		s := decodeServer(t, b)
		if s["backup_requested"] == false && s["last_backup_at"] != nil {
			return // success: flag cleared and timestamp stamped
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server %s backup request was not honored within timeout", id)
}

// TestServerBackupRequiresOwnership verifies a backup request on someone else's
// server is a 404 (no existence leak), like the other owner-scoped routes.
func TestServerBackupRequiresOwnership(t *testing.T) {
	owner := registerUser(t, "backup-owner@example.com", "hunter2pass")
	other := registerUser(t, "backup-other@example.com", "hunter2pass")
	id := createServerID(t, owner.AccessToken, "owned-world")

	resp, _ := doJSON(t, http.MethodPost, "/api/v1/servers/"+id+"/snapshot", other.AccessToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner backup status = %d, want 404", resp.StatusCode)
	}
}

// TestGameServerLifecycle exercises create -> reconcile-to-running -> stop ->
// start -> delete through the real HTTP API and reconciler.
func TestGameServerLifecycle(t *testing.T) {
	user := registerUser(t, "gamer@example.com", "hunter2pass")
	tok := user.AccessToken

	// Create.
	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", tok, map[string]any{
		"name": "survival", "version": "1.20.4",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode, body)
	}
	created := decodeServer(t, body)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %s", body)
	}
	if created["desired_state"] != "running" {
		t.Errorf("desired_state = %v, want running", created["desired_state"])
	}
	if created["game"] != "minecraft" {
		t.Errorf("game = %v, want minecraft", created["game"])
	}

	// Reconciler should bring it to running with runtime details.
	running := waitForStatus(t, tok, id, "running")
	if running["host"] == nil || running["port"] == nil || running["vm_id"] == nil {
		t.Errorf("running server missing runtime details: %v", running)
	}
	// The fake runtime hands each VM the lowest free host port at or above the
	// standard Minecraft port, so a server sharing the suite's host gets 25565 or
	// the next free port up — assert it is a valid pool port, not an exact value
	// that depends on how many other VMs are live when this test runs.
	if p, ok := running["port"].(float64); !ok || p < 25565 {
		t.Errorf("port = %v, want a host port >= 25565", running["port"])
	}

	// Stop.
	resp, body = doJSON(t, http.MethodPatch, "/api/v1/servers/"+id, tok, map[string]any{
		"desired_state": "stopped",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d, body = %s", resp.StatusCode, body)
	}
	stopped := waitForStatus(t, tok, id, "stopped")
	if stopped["host"] != nil || stopped["vm_id"] != nil {
		t.Errorf("stopped server should have cleared runtime: %v", stopped)
	}

	// Start again.
	resp, _ = doJSON(t, http.MethodPatch, "/api/v1/servers/"+id, tok, map[string]any{
		"desired_state": "running",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart status = %d", resp.StatusCode)
	}
	waitForStatus(t, tok, id, "running")

	// Rename via spec update.
	resp, body = doJSON(t, http.MethodPatch, "/api/v1/servers/"+id, tok, map[string]any{
		"name": "creative",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d", resp.StatusCode)
	}
	if decodeServer(t, body)["name"] != "creative" {
		t.Errorf("name not updated: %s", body)
	}

	// Delete -> reconciler tears it down; the API stops serving it.
	resp, _ = doJSON(t, http.MethodDelete, "/api/v1/servers/"+id, tok, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d, want 202", resp.StatusCode)
	}
	waitForGone(t, tok, id)

	// Soft delete: the row is retained in the DB (hidden from the API) with
	// status "deleted" and a deleted_at timestamp.
	var status string
	var deletedAt *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT status, deleted_at FROM game_servers WHERE id = $1`, id,
	).Scan(&status, &deletedAt); err != nil {
		t.Fatalf("expected soft-deleted row to remain in DB: %v", err)
	}
	if status != "deleted" {
		t.Errorf("status = %q, want deleted", status)
	}
	if deletedAt == nil {
		t.Error("expected deleted_at to be set")
	}
}

// TestGameServerOwnership verifies servers are scoped to their owner.
func TestGameServerOwnership(t *testing.T) {
	alice := registerUser(t, "alice-srv@example.com", "hunter2pass")
	bob := registerUser(t, "bob-srv@example.com", "hunter2pass")

	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", alice.AccessToken, map[string]any{
		"name": "alice-world", "version": "1.20.4",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode, body)
	}
	id := decodeServer(t, body)["id"].(string)

	t.Run("owner can read it", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/servers/"+id, alice.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	})

	t.Run("other user gets 404", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/servers/"+id, bob.AccessToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("other user cannot delete it", func(t *testing.T) {
		resp, _ := doJSON(t, http.MethodDelete, "/api/v1/servers/"+id, bob.AccessToken, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("bob's list does not include it", func(t *testing.T) {
		resp, body := get(t, "/api/v1/servers", bob.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out struct {
			Servers []map[string]any `json:"servers"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, s := range out.Servers {
			if s["id"] == id {
				t.Error("bob should not see alice's server")
			}
		}
	})

	t.Run("requires authentication", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/servers", "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestCreateServerValidation covers request validation.
func TestCreateServerValidation(t *testing.T) {
	user := registerUser(t, "validate-srv@example.com", "hunter2pass")
	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing name", map[string]any{"version": "1.20.4"}},
		{"missing version", map[string]any{"name": "x"}},
		{"cpus too high", map[string]any{"name": "x", "version": "1.20.4", "cpus": 999}},
		{"memory too low", map[string]any{"name": "x", "version": "1.20.4", "memory_mb": 16}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", user.AccessToken, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
			}
		})
	}
}
