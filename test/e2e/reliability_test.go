//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/google/uuid"
)

// reliabilityUser inserts a user to satisfy the game_servers owner FK and returns
// its id.
func reliabilityUser(t *testing.T, ctx context.Context) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)`,
		id, id+"@example.com", "x",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// makeServer inserts a running-desired server in the given status and returns it.
func makeServer(t *testing.T, ctx context.Context, repo *repository.GameServerRepository, owner, status string) *model.GameServer {
	t.Helper()
	s := &model.GameServer{
		OwnerID: owner, Name: "rel-" + uuid.NewString()[:8], Game: model.GameMinecraft,
		Version: "1.21", CPUs: 1, MemoryMB: 1024,
		DesiredState: model.DesiredRunning, Status: status,
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if status != model.StatusPending {
		if err := repo.MarkStatus(ctx, s.ID, status, ""); err != nil {
			t.Fatalf("mark status: %v", err)
		}
	}
	return s
}

func inReconcilable(servers []model.GameServer, id string) bool {
	for i := range servers {
		if servers[i].ID == id {
			return true
		}
	}
	return false
}

// TestReconcileBackoffGate verifies ListReconcilable honors next_attempt_at: a
// failed server with a future retry time is withheld, then becomes eligible once
// the time passes, and an explicit desired-state change clears the backoff
// immediately (P8a).
func TestReconcileBackoffGate(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewGameServerRepository(pool)
	owner := reliabilityUser(t, ctx)

	s := makeServer(t, ctx, repo, owner, model.StatusError)

	// A failure scheduled for the future is withheld from reconciliation.
	if err := repo.MarkReconcileFailed(ctx, s.ID, "boom", 3, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("mark reconcile failed: %v", err)
	}
	list, err := repo.ListReconcilable(ctx)
	if err != nil {
		t.Fatalf("list reconcilable: %v", err)
	}
	if inReconcilable(list, s.ID) {
		t.Fatal("server with future next_attempt_at should be withheld from ListReconcilable")
	}
	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusError || got.Attempts != 3 || got.NextAttemptAt == nil {
		t.Fatalf("unexpected state after failure: status=%s attempts=%d next=%v", got.Status, got.Attempts, got.NextAttemptAt)
	}

	// A failure whose retry time has passed is eligible again.
	if err := repo.MarkReconcileFailed(ctx, s.ID, "boom", 4, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("mark reconcile failed (past): %v", err)
	}
	list, _ = repo.ListReconcilable(ctx)
	if !inReconcilable(list, s.ID) {
		t.Fatal("server with past next_attempt_at should be reconcilable")
	}

	// Re-arm a future backoff, then an explicit desired-state change clears it.
	if err := repo.MarkReconcileFailed(ctx, s.ID, "boom", 5, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("mark reconcile failed (re-arm): %v", err)
	}
	if err := repo.SetDesiredState(ctx, s.ID, model.DesiredStopped); err != nil {
		t.Fatalf("set desired state: %v", err)
	}
	got, _ = repo.GetByID(ctx, s.ID)
	if got.Attempts != 0 || got.NextAttemptAt != nil {
		t.Fatalf("desired-state change should clear backoff: attempts=%d next=%v", got.Attempts, got.NextAttemptAt)
	}
}

// TestNextGeneration verifies the per-server incarnation counter bumps
// monotonically (P8b fencing token).
func TestNextGeneration(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewGameServerRepository(pool)
	owner := reliabilityUser(t, ctx)
	s := makeServer(t, ctx, repo, owner, model.StatusPending)

	if s.Generation != 0 {
		t.Fatalf("fresh server generation = %d, want 0", s.Generation)
	}
	g1, err := repo.NextGeneration(ctx, s.ID)
	if err != nil {
		t.Fatalf("next generation: %v", err)
	}
	g2, err := repo.NextGeneration(ctx, s.ID)
	if err != nil {
		t.Fatalf("next generation: %v", err)
	}
	if g1 != 1 || g2 != 2 {
		t.Fatalf("generations = %d, %d; want 1, 2", g1, g2)
	}
	got, _ := repo.GetByID(ctx, s.ID)
	if got.Generation != 2 {
		t.Fatalf("persisted generation = %d, want 2", got.Generation)
	}
}

// TestFenceRepository verifies the orphan fence table CRUD + GC (P8b).
func TestFenceRepository(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewFenceRepository(pool)

	f := model.FencedVM{ServerID: "fs-" + uuid.NewString()[:8], HostID: "h-" + uuid.NewString()[:8], VMID: "vm-" + uuid.NewString()[:8], Generation: 3}
	if err := repo.Add(ctx, f); err != nil {
		t.Fatalf("add fence: %v", err)
	}
	// Re-adding the same (host, vm) is idempotent.
	if err := repo.Add(ctx, f); err != nil {
		t.Fatalf("re-add fence: %v", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("list fences: %v", err)
	}
	var found *model.FencedVM
	for i := range list {
		if list[i].HostID == f.HostID && list[i].VMID == f.VMID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("added fence not found in List")
	}
	if found.ServerID != f.ServerID || found.Generation != 3 {
		t.Fatalf("fence round-trip mismatch: %+v", found)
	}

	if err := repo.Delete(ctx, f.HostID, f.VMID); err != nil {
		t.Fatalf("delete fence: %v", err)
	}
	// Delete is idempotent.
	if err := repo.Delete(ctx, f.HostID, f.VMID); err != nil {
		t.Fatalf("re-delete fence: %v", err)
	}
	list, _ = repo.List(ctx)
	for i := range list {
		if list[i].HostID == f.HostID && list[i].VMID == f.VMID {
			t.Fatal("fence still present after delete")
		}
	}

	// DeleteOlderThan sweeps nothing newer than the cutoff and reports a count.
	g := model.FencedVM{ServerID: "fs-gc", HostID: "h-gc-" + uuid.NewString()[:8], VMID: "vm-gc", Generation: 1}
	if err := repo.Add(ctx, g); err != nil {
		t.Fatalf("add gc fence: %v", err)
	}
	if n, err := repo.DeleteOlderThan(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("DeleteOlderThan(recent) = %d, %v; want 0, nil", n, err)
	}
	if n, err := repo.DeleteOlderThan(ctx, time.Now().Add(time.Hour)); err != nil || n < 1 {
		t.Fatalf("DeleteOlderThan(future) = %d, %v; want >=1, nil", n, err)
	}
}

// TestReconcileBackoffResetOnSuccess verifies a successful transition clears the
// attempt counter and retry time (P8a).
func TestReconcileBackoffResetOnSuccess(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewGameServerRepository(pool)
	owner := reliabilityUser(t, ctx)

	s := makeServer(t, ctx, repo, owner, model.StatusError)
	if err := repo.MarkReconcileFailed(ctx, s.ID, "boom", 2, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("mark reconcile failed: %v", err)
	}
	if err := repo.MarkRunning(ctx, s.ID, "vm-x", "1.2.3.4", 25565); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	got, err := repo.GetByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Attempts != 0 || got.NextAttemptAt != nil {
		t.Fatalf("success should clear backoff: attempts=%d next=%v", got.Attempts, got.NextAttemptAt)
	}
}
