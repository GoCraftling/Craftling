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
