//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// billingPayload mirrors model.BillingSummary.
type billingPayload struct {
	Currency     string  `json:"currency"`
	PeriodStart  string  `json:"period_start"`
	CPUHour      float64 `json:"cpu_hour"`
	MemoryGBHour float64 `json:"memory_gb_hour"`
	TotalCost    float64 `json:"total_cost"`
	HourlyRate   float64 `json:"hourly_rate"`
	Items        []struct {
		ServerID   string  `json:"server_id"`
		Name       string  `json:"name"`
		CPUs       int     `json:"cpus"`
		MemoryMB   int     `json:"memory_mb"`
		Hours      float64 `json:"hours"`
		HourlyRate float64 `json:"hourly_rate"`
		Cost       float64 `json:"cost"`
		Running    bool    `json:"running"`
	} `json:"items"`
}

func decodeBilling(t *testing.T, body []byte) billingPayload {
	t.Helper()
	var b billingPayload
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode billing: %v (body=%s)", err, body)
	}
	return b
}

// TestBillingMetersRunningServer walks the pay-as-you-go meter end to end: a
// fresh user has an empty bill; once a server is created and the reconciler
// brings it to running, the user's bill carries a running line item with the
// configured rate; stopping the server closes the interval (no longer running)
// while the accrued cost is retained.
func TestBillingMetersRunningServer(t *testing.T) {
	user := registerUser(t, "billing-user@example.com", "hunter2pass")
	tok := user.AccessToken

	// Empty to start.
	resp, body := get(t, "/api/v1/billing", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("billing status = %d, body = %s", resp.StatusCode, body)
	}
	empty := decodeBilling(t, body)
	if empty.Currency == "" {
		t.Errorf("expected a currency in the bill, got empty")
	}
	if len(empty.Items) != 0 || empty.TotalCost != 0 || empty.HourlyRate != 0 {
		t.Fatalf("fresh user should have an empty bill, got %+v", empty)
	}

	// Create a server and let it reach running; the reconciler opens a billing
	// interval on MarkRunning.
	id := createServerID(t, tok, "billed-world")
	waitForStatus(t, tok, id, "running")

	// The bill should now show this server as a running line item with a positive
	// hourly rate. Poll briefly: the meter write follows the running transition.
	var running billingPayload
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := get(t, "/api/v1/billing", tok)
		running = decodeBilling(t, b)
		if len(running.Items) == 1 && running.Items[0].Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(running.Items) != 1 {
		t.Fatalf("expected one billed item while running, got %+v", running)
	}
	it := running.Items[0]
	if it.ServerID != id {
		t.Errorf("billed server_id = %q, want %q", it.ServerID, id)
	}
	if !it.Running {
		t.Errorf("server should be billing as running")
	}
	if it.HourlyRate <= 0 {
		t.Errorf("hourly_rate = %v, want > 0", it.HourlyRate)
	}
	if running.HourlyRate <= 0 {
		t.Errorf("summary burn rate = %v, want > 0 while a server runs", running.HourlyRate)
	}

	// Stop the server: the interval closes, so the item is no longer running and
	// the live burn rate drops to zero, but the accrued line item remains.
	resp, _ = doJSON(t, http.MethodPatch, "/api/v1/servers/"+id, tok, map[string]any{
		"desired_state": "stopped",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop status = %d", resp.StatusCode)
	}
	waitForStatus(t, tok, id, "stopped")

	var stopped billingPayload
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := get(t, "/api/v1/billing", tok)
		stopped = decodeBilling(t, b)
		if len(stopped.Items) == 1 && !stopped.Items[0].Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(stopped.Items) != 1 || stopped.Items[0].Running {
		t.Fatalf("after stop expected one closed item, got %+v", stopped)
	}
	if stopped.HourlyRate != 0 {
		t.Errorf("burn rate after stop = %v, want 0", stopped.HourlyRate)
	}
}

// TestBillingAdminView confirms an admin can read any user's bill and a
// non-admin cannot, and an unknown user is 404.
func TestBillingAdminView(t *testing.T) {
	user := registerUser(t, "billing-target@example.com", "hunter2pass")
	admin := makeAdmin(t, "billing-admin@example.com", "hunter2pass")
	uid := meID(t, user.AccessToken)

	t.Run("non-admin is forbidden", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/admin/users/"+uid+"/billing", user.AccessToken)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("admin can read it", func(t *testing.T) {
		resp, body := get(t, "/api/v1/admin/users/"+uid+"/billing", admin.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
		}
		if decodeBilling(t, body).Currency == "" {
			t.Error("expected a currency in the admin bill view")
		}
	})

	t.Run("unknown user is 404", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/admin/users/00000000-0000-0000-0000-000000000000/billing", admin.AccessToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}
