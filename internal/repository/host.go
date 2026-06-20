package repository

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
)

// Capacity-reservation errors returned by Reserve (P2 scheduler).
var (
	// ErrHostNotReady means the host exists but is not eligible for placement.
	ErrHostNotReady = errors.New("host not ready")
	// ErrInsufficientCapacity means the host lacks free cpu/memory for the spec.
	ErrInsufficientCapacity = errors.New("insufficient host capacity")
)

// HostRepository is an in-memory inventory of fleet hosts. P1 keeps the fleet
// in process memory (no durable table yet); it is concurrency-safe so the HTTP
// handlers and the host reaper can share one instance. A later phase can swap
// this for a Postgres-backed store behind the same method set.
type HostRepository struct {
	mu    sync.RWMutex
	hosts map[string]*model.Host // keyed by host id
}

// NewHostRepository constructs an empty in-memory HostRepository.
func NewHostRepository() *HostRepository {
	return &HostRepository{hosts: make(map[string]*model.Host)}
}

// now is overridable in tests; defaults to wall-clock time.
var now = time.Now

// Register upserts a host into the inventory and returns the stored record.
//
// Identity is agent-owned: when the caller supplies h.ID (a stable id the agent
// generates and persists), that id is authoritative. Because the fleet lives in
// process memory, a control-plane restart drops it — but the agent rebuilds its
// own entry on the next register with the *same* id, so anything referencing the
// host (e.g. a future game_servers.host_id) stays valid across restarts without
// a durable table. When no id is supplied we fall back to matching an existing
// record by hostname, then to a freshly generated id.
//
// A registered or recovering host is marked ready and its heartbeat stamped now.
// Allocatable capacity is initialised to total when the record is first created.
func (r *HostRepository) Register(ctx context.Context, h *model.Host) (*model.Host, error) {
	return r.RegisterReserved(ctx, h, 0, 0)
}

// RegisterReserved is Register with a known amount of already-committed capacity.
// When the host is *new* to this process — the case after a control-plane
// restart, where the in-memory fleet was lost but the agent re-registers with
// its stable id — allocatable is initialised to total minus the reserved cpu/mem
// the caller reconstructed from the durable record, rather than to total. This
// keeps existing placements (including ones whose VM never booted) accounted for
// across a restart. For a host already known to this process the reserved
// arguments are ignored: its in-memory allocatable already reflects live
// reservations and is authoritative.
func (r *HostRepository) RegisterReserved(_ context.Context, h *model.Host, reservedCPUs, reservedMemMB int) (*model.Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t := now()
	if existing := r.matchExisting(h); existing != nil {
		existing.Hostname = h.Hostname
		existing.Address = h.Address
		existing.Zone = h.Zone
		existing.CPUsTotal = h.CPUsTotal
		existing.MemoryMBTotal = h.MemoryMBTotal
		existing.AgentVersion = h.AgentVersion
		existing.Status = model.HostReady
		existing.LastHeartbeatAt = t
		existing.UpdatedAt = t
		return clone(existing), nil
	}

	stored := *h
	if stored.ID == "" {
		stored.ID = uuid.NewString()
	}
	stored.Status = model.HostReady
	stored.CPUsAllocatable = clampNonNeg(h.CPUsTotal - reservedCPUs)
	stored.MemoryMBAllocatable = clampNonNeg(h.MemoryMBTotal - reservedMemMB)
	stored.LastHeartbeatAt = t
	stored.CreatedAt = t
	stored.UpdatedAt = t
	r.hosts[stored.ID] = &stored
	return clone(&stored), nil
}

// clampNonNeg floors a value at zero, guarding against reconstructed reservations
// that somehow exceed a host's reported total.
func clampNonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// matchExisting finds the record a registration refers to: by agent-supplied id
// when present (the authoritative key), otherwise by hostname. Returns nil when
// there is no match. Caller must hold the lock.
func (r *HostRepository) matchExisting(h *model.Host) *model.Host {
	if h.ID != "" {
		return r.hosts[h.ID]
	}
	return r.findByHostname(h.Hostname)
}

// Heartbeat records liveness for a host, refreshing its heartbeat timestamp. A
// host previously marked down is brought back to ready. Returns ErrNotFound for
// an unknown id.
func (r *HostRepository) Heartbeat(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	h, ok := r.hosts[id]
	if !ok {
		return ErrNotFound
	}
	t := now()
	h.LastHeartbeatAt = t
	h.UpdatedAt = t
	if h.Status == model.HostDown {
		h.Status = model.HostReady
	}
	return nil
}

// GetByID returns a host by id, or ErrNotFound.
func (r *HostRepository) GetByID(_ context.Context, id string) (*model.Host, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	h, ok := r.hosts[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(h), nil
}

// List returns every host, newest first.
func (r *HostRepository) List(_ context.Context) ([]model.Host, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot(func(*model.Host) bool { return true }), nil
}

// ListReady returns only hosts eligible for placement (status ready), newest
// first. This is the seam the P2 scheduler will build on.
func (r *HostRepository) ListReady(_ context.Context) ([]model.Host, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.snapshot(func(h *model.Host) bool { return h.Status == model.HostReady }), nil
}

// Reserve atomically deducts cpus/memMB from a host's allocatable capacity,
// committing a scheduler placement. It is the authoritative, race-safe step:
// the scheduler may pick a host from a stale snapshot, but only the host that
// still has room under the lock here actually accepts the reservation. Returns
// ErrNotFound for an unknown id, ErrHostNotReady if the host is not ready, or
// ErrInsufficientCapacity if it no longer fits.
func (r *HostRepository) Reserve(_ context.Context, id string, cpus, memMB int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	h, ok := r.hosts[id]
	if !ok {
		return ErrNotFound
	}
	if h.Status != model.HostReady {
		return ErrHostNotReady
	}
	if h.CPUsAllocatable < cpus || h.MemoryMBAllocatable < memMB {
		return ErrInsufficientCapacity
	}
	h.CPUsAllocatable -= cpus
	h.MemoryMBAllocatable -= memMB
	h.UpdatedAt = now()
	return nil
}

// Release returns cpus/memMB to a host's allocatable capacity, clamped to its
// total so repeated or stale releases cannot inflate it past physical capacity.
// An unknown host is a no-op: the fleet lives in memory, so a control-plane
// restart can legitimately forget a host that still has servers assigned.
func (r *HostRepository) Release(_ context.Context, id string, cpus, memMB int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	h, ok := r.hosts[id]
	if !ok {
		return nil
	}
	h.CPUsAllocatable = min(h.CPUsAllocatable+cpus, h.CPUsTotal)
	h.MemoryMBAllocatable = min(h.MemoryMBAllocatable+memMB, h.MemoryMBTotal)
	h.UpdatedAt = now()
	return nil
}

// MarkStale marks every host whose last heartbeat predates cutoff as down, and
// returns how many transitioned. Already-down hosts are left untouched.
func (r *HostRepository) MarkStale(_ context.Context, cutoff time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var n int
	t := now()
	for _, h := range r.hosts {
		if h.Status == model.HostDown || !h.LastHeartbeatAt.Before(cutoff) {
			continue
		}
		h.Status = model.HostDown
		h.UpdatedAt = t
		n++
	}
	return n, nil
}

// findByHostname returns the stored host with the given hostname, or nil.
// Caller must hold the lock.
func (r *HostRepository) findByHostname(hostname string) *model.Host {
	for _, h := range r.hosts {
		if h.Hostname == hostname {
			return h
		}
	}
	return nil
}

// snapshot returns copies of hosts passing keep, sorted newest-created first.
// Caller must hold (at least) the read lock.
func (r *HostRepository) snapshot(keep func(*model.Host) bool) []model.Host {
	out := make([]model.Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		if keep(h) {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func clone(h *model.Host) *model.Host {
	c := *h
	return &c
}
