//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
)

// quotaPayload mirrors the quota endpoints' JSON response.
type quotaPayload struct {
	Quota struct {
		UserID      string `json:"user_id"`
		MaxServers  int    `json:"max_servers"`
		MaxCPUs     int    `json:"max_cpus"`
		MaxMemoryMB int    `json:"max_memory_mb"`
		Custom      bool   `json:"custom"`
	} `json:"quota"`
	Usage struct {
		Servers  int `json:"servers"`
		CPUs     int `json:"cpus"`
		MemoryMB int `json:"memory_mb"`
	} `json:"usage"`
}

func decodeQuota(t *testing.T, body []byte) quotaPayload {
	t.Helper()
	var q quotaPayload
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("decode quota: %v (body=%s)", err, body)
	}
	return q
}

// setQuota is an admin helper that sets a user's quota override.
func setQuota(t *testing.T, adminTok, userID string, servers, cpus, memMB int) quotaPayload {
	t.Helper()
	resp, body := doJSON(t, http.MethodPut, "/api/v1/admin/users/"+userID+"/quota", adminTok, map[string]any{
		"max_servers": servers, "max_cpus": cpus, "max_memory_mb": memMB,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set quota status = %d, body = %s", resp.StatusCode, body)
	}
	return decodeQuota(t, body)
}

// TestQuotaDefaultSelfView confirms a brand-new user sees the system default
// quota (Custom=false) and zero usage through their own /quota endpoint.
func TestQuotaDefaultSelfView(t *testing.T) {
	user := registerUser(t, "quota-default@example.com", "hunter2pass")
	resp, body := get(t, "/api/v1/quota", user.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	q := decodeQuota(t, body)
	if q.Quota.Custom {
		t.Errorf("a fresh user should be on the default quota (custom=false), got custom=true")
	}
	if q.Quota.MaxServers <= 0 {
		t.Errorf("default max_servers = %d, want a positive cap", q.Quota.MaxServers)
	}
	if q.Usage.Servers != 0 {
		t.Errorf("fresh user usage.servers = %d, want 0", q.Usage.Servers)
	}
}

// TestQuotaServerCountEnforced is the milestone's headline check: once a user is
// at their server-count cap, the next create is rejected with 403.
func TestQuotaServerCountEnforced(t *testing.T) {
	user := registerUser(t, "quota-count@example.com", "hunter2pass")
	admin := makeAdmin(t, "quota-count-admin@example.com", "hunter2pass")
	uid := meID(t, user.AccessToken)

	// Cap at a single server; leave cpu/memory unlimited so the count is the only
	// dimension under test.
	set := setQuota(t, admin.AccessToken, uid, 1, 0, 0)
	if !set.Quota.Custom || set.Quota.MaxServers != 1 {
		t.Fatalf("set quota = %+v, want custom max_servers=1", set.Quota)
	}

	// First server fits.
	createServerID(t, user.AccessToken, "within-quota")

	// Second server breaches the count cap -> 403.
	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", user.AccessToken, map[string]any{
		"name": "over-quota", "version": "1.20.4",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("over-quota create status = %d, want 403, body = %s", resp.StatusCode, body)
	}

	// Usage should reflect exactly the one admitted server.
	_, qbody := get(t, "/api/v1/quota", user.AccessToken)
	if u := decodeQuota(t, qbody).Usage; u.Servers != 1 {
		t.Errorf("usage.servers = %d, want 1", u.Servers)
	}
}

// TestQuotaResourceEnforced checks the cpu/memory dimensions independently of the
// server count: a generous count cap but a tight cpu cap still blocks the create
// that would push total cpu over the limit.
func TestQuotaResourceEnforced(t *testing.T) {
	user := registerUser(t, "quota-cpu@example.com", "hunter2pass")
	admin := makeAdmin(t, "quota-cpu-admin@example.com", "hunter2pass")
	uid := meID(t, user.AccessToken)

	// Up to 10 servers, but only 3 vCPU total. One 2-cpu server fits (2 <= 3); a
	// second would be 4 > 3.
	setQuota(t, admin.AccessToken, uid, 10, 3, 0)

	resp, body := doJSON(t, http.MethodPost, "/api/v1/servers", user.AccessToken, map[string]any{
		"name": "cpu-ok", "version": "1.20.4", "cpus": 2,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, http.MethodPost, "/api/v1/servers", user.AccessToken, map[string]any{
		"name": "cpu-over", "version": "1.20.4", "cpus": 2,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cpu-over create status = %d, want 403, body = %s", resp.StatusCode, body)
	}
}

// TestQuotaAdminEndpoints exercises the admin view/set/delete surface and its
// authorization: only an admin may set, a non-admin is forbidden, an unknown
// user is 404, and deleting an override reverts to the default.
func TestQuotaAdminEndpoints(t *testing.T) {
	user := registerUser(t, "quota-admin-target@example.com", "hunter2pass")
	admin := makeAdmin(t, "quota-admin-actor@example.com", "hunter2pass")
	uid := meID(t, user.AccessToken)

	t.Run("non-admin cannot set a quota", func(t *testing.T) {
		resp, _ := doJSON(t, http.MethodPut, "/api/v1/admin/users/"+uid+"/quota", user.AccessToken, map[string]any{
			"max_servers": 1, "max_cpus": 1, "max_memory_mb": 1024,
		})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("admin set then get round-trips", func(t *testing.T) {
		setQuota(t, admin.AccessToken, uid, 3, 8, 8192)
		resp, body := get(t, "/api/v1/admin/users/"+uid+"/quota", admin.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("get status = %d, body = %s", resp.StatusCode, body)
		}
		q := decodeQuota(t, body)
		if !q.Quota.Custom || q.Quota.MaxServers != 3 || q.Quota.MaxCPUs != 8 || q.Quota.MaxMemoryMB != 8192 {
			t.Errorf("quota = %+v, want custom 3/8/8192", q.Quota)
		}
	})

	t.Run("negative limit is rejected", func(t *testing.T) {
		resp, _ := doJSON(t, http.MethodPut, "/api/v1/admin/users/"+uid+"/quota", admin.AccessToken, map[string]any{
			"max_servers": -1, "max_cpus": 1, "max_memory_mb": 1024,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown user is 404", func(t *testing.T) {
		resp, _ := get(t, "/api/v1/admin/users/00000000-0000-0000-0000-000000000000/quota", admin.AccessToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("delete reverts to the default", func(t *testing.T) {
		setQuota(t, admin.AccessToken, uid, 3, 8, 8192)
		resp, _ := doJSON(t, http.MethodDelete, "/api/v1/admin/users/"+uid+"/quota", admin.AccessToken, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete status = %d", resp.StatusCode)
		}
		_, body := get(t, "/api/v1/admin/users/"+uid+"/quota", admin.AccessToken)
		if decodeQuota(t, body).Quota.Custom {
			t.Error("after delete the quota should be the default (custom=false)")
		}
	})
}
