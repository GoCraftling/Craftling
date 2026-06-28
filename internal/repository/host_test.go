package repository

import (
	"context"
	"testing"

	"github.com/aarani/craftling-go/internal/model"
)

func newHost(id string, cpus, memMB int) *model.Host {
	return &model.Host{ID: id, Hostname: "h-" + id, Address: "10.0.0.1:9000", CPUsTotal: cpus, MemoryMBTotal: memMB}
}

// TestRegisterReservedNewHost verifies a host new to the process comes up with
// allocatable seeded to total minus the reconstructed reservation.
func TestRegisterReservedNewHost(t *testing.T) {
	repo := NewHostRepository()
	h, err := repo.RegisterReserved(context.Background(), newHost("a", 8, 8192), 2, 2048)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.CPUsAllocatable != 6 || h.MemoryMBAllocatable != 6144 {
		t.Fatalf("allocatable = %d/%d, want 6/6144", h.CPUsAllocatable, h.MemoryMBAllocatable)
	}
}

// TestMarkDown verifies a host is marked down on demand (the hub's
// disconnect path), and that marking an unknown host is a harmless no-op.
func TestMarkDown(t *testing.T) {
	repo := NewHostRepository()
	if _, err := repo.RegisterReserved(context.Background(), newHost("a", 4, 4096), 0, 0); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := repo.MarkDown(context.Background(), "a"); err != nil {
		t.Fatalf("mark down: %v", err)
	}
	h, err := repo.GetByID(context.Background(), "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if h.Status != model.HostDown {
		t.Errorf("status = %q, want %q", h.Status, model.HostDown)
	}

	// Unknown host: no error (a control-plane restart can forget a host).
	if err := repo.MarkDown(context.Background(), "ghost"); err != nil {
		t.Errorf("mark down unknown = %v, want nil", err)
	}
}

// TestRegisterReservedClampsNegative guards against a reconstructed reservation
// exceeding the host's reported total (allocatable floors at zero).
func TestRegisterReservedClampsNegative(t *testing.T) {
	repo := NewHostRepository()
	h, err := repo.RegisterReserved(context.Background(), newHost("b", 2, 1024), 99, 99999)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.CPUsAllocatable != 0 || h.MemoryMBAllocatable != 0 {
		t.Fatalf("allocatable = %d/%d, want 0/0", h.CPUsAllocatable, h.MemoryMBAllocatable)
	}
}

// TestRegisterReservedExistingIgnoresReserved verifies a re-registration of a
// host already known to this process preserves its live in-memory allocatable
// (the authoritative reservation state) rather than recomputing it.
func TestRegisterReservedExistingIgnoresReserved(t *testing.T) {
	ctx := context.Background()
	repo := NewHostRepository()
	const id = "c"
	if _, err := repo.RegisterReserved(ctx, newHost(id, 8, 8192), 0, 0); err != nil {
		t.Fatalf("initial register: %v", err)
	}
	if err := repo.Reserve(ctx, id, 3, 3072); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Re-register with a bogus reserved arg: the existing host's allocatable must
	// stay at what the live reservations left it.
	h, err := repo.RegisterReserved(ctx, newHost(id, 8, 8192), 999, 999)
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if h.CPUsAllocatable != 5 || h.MemoryMBAllocatable != 5120 {
		t.Fatalf("allocatable = %d/%d, want 5/5120 (preserved across re-register)", h.CPUsAllocatable, h.MemoryMBAllocatable)
	}
}

// TestRegisterDefaultsAllocatableToTotal verifies the plain Register path (no
// reconstruction) still initialises allocatable to total.
func TestRegisterDefaultsAllocatableToTotal(t *testing.T) {
	repo := NewHostRepository()
	h, err := repo.Register(context.Background(), newHost("d", 4, 4096))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.CPUsAllocatable != 4 || h.MemoryMBAllocatable != 4096 {
		t.Fatalf("allocatable = %d/%d, want 4/4096", h.CPUsAllocatable, h.MemoryMBAllocatable)
	}
}

// TestDrainLifecycle verifies the P8c host draining transitions: a ready host can
// be drained (then it drops out of ListReady and into ListDraining), a heartbeat
// does not undo draining, undrain returns it to ready, and a down host cannot be
// drained.
func TestDrainLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := NewHostRepository()
	if _, err := repo.Register(ctx, newHost("a", 4, 4096)); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := repo.SetDraining(ctx, "a"); err != nil {
		t.Fatalf("set draining: %v", err)
	}
	h, _ := repo.GetByID(ctx, "a")
	if h.Status != model.HostDraining {
		t.Fatalf("status = %q, want draining", h.Status)
	}
	if ready, _ := repo.ListReady(ctx); len(ready) != 0 {
		t.Errorf("draining host still in ListReady: %v", ready)
	}
	if drn, _ := repo.ListDraining(ctx); len(drn) != 1 || drn[0].ID != "a" {
		t.Errorf("ListDraining = %v, want [a]", drn)
	}

	// A heartbeat keeps a draining host draining (only a down host is un-downed).
	if err := repo.Heartbeat(ctx, "a"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	h, _ = repo.GetByID(ctx, "a")
	if h.Status != model.HostDraining {
		t.Errorf("heartbeat changed status to %q; want draining preserved", h.Status)
	}

	// SetDraining is idempotent.
	if err := repo.SetDraining(ctx, "a"); err != nil {
		t.Fatalf("re-drain: %v", err)
	}

	if err := repo.Undrain(ctx, "a"); err != nil {
		t.Fatalf("undrain: %v", err)
	}
	h, _ = repo.GetByID(ctx, "a")
	if h.Status != model.HostReady {
		t.Fatalf("after undrain status = %q, want ready", h.Status)
	}

	// A down host cannot be drained.
	if err := repo.MarkDown(ctx, "a"); err != nil {
		t.Fatalf("mark down: %v", err)
	}
	if err := repo.SetDraining(ctx, "a"); err != ErrHostNotReady {
		t.Fatalf("drain down host = %v, want ErrHostNotReady", err)
	}

	// Draining/undraining an unknown host is a not-found.
	if err := repo.SetDraining(ctx, "nope"); err != ErrNotFound {
		t.Errorf("drain unknown = %v, want ErrNotFound", err)
	}
	if err := repo.Undrain(ctx, "nope"); err != ErrNotFound {
		t.Errorf("undrain unknown = %v, want ErrNotFound", err)
	}
}
