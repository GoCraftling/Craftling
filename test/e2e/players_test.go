//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/aarani/craftling-go/internal/repository"
)

type playerPayload struct {
	ID        string   `json:"id"`
	OwnerID   string   `json:"owner_id"`
	Username  string   `json:"username"`
	ServerIDs []string `json:"server_ids"`
}

func decodePlayer(t *testing.T, body []byte) playerPayload {
	t.Helper()
	var p playerPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode player: %v (body=%s)", err, body)
	}
	return p
}

func listPlayers(t *testing.T, token string) []playerPayload {
	t.Helper()
	resp, body := get(t, "/api/v1/players", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list players status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Players []playerPayload `json:"players"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode players: %v", err)
	}
	return out.Players
}

func hasServer(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestPlayerRosterAndGrants walks the whitelist roster: add a player, grant it a
// subset of the owner's servers, then re-grant (check/uncheck) and remove.
func TestPlayerRosterAndGrants(t *testing.T) {
	user := registerUser(t, "roster-owner@example.com", "hunter2pass")
	tok := user.AccessToken

	srvA := createServerID(t, tok, "roster-a")
	srvB := createServerID(t, tok, "roster-b")

	// Create a player granted on server A only.
	resp, body := doJSON(t, http.MethodPost, "/api/v1/players", tok, map[string]any{
		"username":   "Steve",
		"server_ids": []string{srvA},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create player status = %d, body = %s", resp.StatusCode, body)
	}
	p := decodePlayer(t, body)
	if p.Username != "Steve" || !hasServer(p.ServerIDs, srvA) || hasServer(p.ServerIDs, srvB) {
		t.Fatalf("created player = %+v, want Steve granted only on A", p)
	}

	// Re-grant: uncheck A, check B (PATCH replaces the whole set).
	resp, body = doJSON(t, http.MethodPatch, "/api/v1/players/"+p.ID, tok, map[string]any{
		"server_ids": []string{srvB},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update grants status = %d, body = %s", resp.StatusCode, body)
	}
	up := decodePlayer(t, body)
	if hasServer(up.ServerIDs, srvA) || !hasServer(up.ServerIDs, srvB) {
		t.Fatalf("regranted player = %+v, want only B", up)
	}

	// Rename and clear all grants.
	resp, body = doJSON(t, http.MethodPatch, "/api/v1/players/"+p.ID, tok, map[string]any{
		"username":   "Alex",
		"server_ids": []string{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename status = %d, body = %s", resp.StatusCode, body)
	}
	if r := decodePlayer(t, body); r.Username != "Alex" || len(r.ServerIDs) != 0 {
		t.Fatalf("after rename+clear = %+v, want Alex with no servers", r)
	}

	// It shows in the roster listing.
	if players := listPlayers(t, tok); len(players) != 1 || players[0].Username != "Alex" {
		t.Fatalf("roster = %+v, want one player Alex", players)
	}

	// Delete it.
	resp, _ = doJSON(t, http.MethodDelete, "/api/v1/players/"+p.ID, tok, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if players := listPlayers(t, tok); len(players) != 0 {
		t.Fatalf("roster after delete = %+v, want empty", players)
	}
}

// TestWhitelistUsernamesForServer verifies the data source the reconciler feeds
// to RCON: a server's desired whitelist is exactly the granted players'
// usernames, ordered, and reflects grant changes. (The RCON application itself
// is unit-tested in internal/minecraft and exercised under the KVM lane; here we
// pin the control-plane query the sync loop reads.)
func TestWhitelistUsernamesForServer(t *testing.T) {
	user := registerUser(t, "wl-owner@example.com", "hunter2pass")
	tok := user.AccessToken
	srv := createServerID(t, tok, "wl-world")

	// Grant two players onto the server.
	for _, name := range []string{"Bravo", "Alpha"} {
		resp, body := doJSON(t, http.MethodPost, "/api/v1/players", tok, map[string]any{
			"username": name, "server_ids": []string{srv},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %s status = %d, body = %s", name, resp.StatusCode, body)
		}
	}

	repo := repository.NewPlayerRepository(pool)
	got, err := repo.UsernamesForServer(context.Background(), srv)
	if err != nil {
		t.Fatalf("usernames for server: %v", err)
	}
	if want := []string{"Alpha", "Bravo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("whitelist = %v, want %v (sorted)", got, want)
	}
}

// TestPlayerValidationAndIsolation covers username validation, duplicate
// rejection, cross-owner server grants, and roster ownership scoping.
func TestPlayerValidationAndIsolation(t *testing.T) {
	alice := registerUser(t, "roster-alice@example.com", "hunter2pass")
	bob := registerUser(t, "roster-bob@example.com", "hunter2pass")
	bobServer := createServerID(t, bob.AccessToken, "bob-world")

	t.Run("bad username is rejected", func(t *testing.T) {
		resp, _ := doJSON(t, http.MethodPost, "/api/v1/players", alice.AccessToken, map[string]any{
			"username": "x", // too short
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("cannot grant another owner's server", func(t *testing.T) {
		resp, body := doJSON(t, http.MethodPost, "/api/v1/players", alice.AccessToken, map[string]any{
			"username":   "Notch",
			"server_ids": []string{bobServer},
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body = %s", resp.StatusCode, body)
		}
	})

	t.Run("duplicate username is a conflict", func(t *testing.T) {
		resp, _ := doJSON(t, http.MethodPost, "/api/v1/players", alice.AccessToken, map[string]any{
			"username": "Herobrine",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("first create status = %d", resp.StatusCode)
		}
		resp, _ = doJSON(t, http.MethodPost, "/api/v1/players", alice.AccessToken, map[string]any{
			"username": "Herobrine",
		})
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("duplicate status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("a roster is private to its owner", func(t *testing.T) {
		// Alice has players (Herobrine above); Bob's roster must not see them.
		if players := listPlayers(t, bob.AccessToken); len(players) != 0 {
			t.Fatalf("bob's roster = %+v, want empty", players)
		}
		// And the same username can coexist across owners.
		resp, _ := doJSON(t, http.MethodPost, "/api/v1/players", bob.AccessToken, map[string]any{
			"username": "Herobrine",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("bob create status = %d, want 201 (names are per-owner)", resp.StatusCode)
		}
	})
}
