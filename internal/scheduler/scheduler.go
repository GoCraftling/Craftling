// Package scheduler decides which fleet host a game server runs on. It is the
// placement half of P2: pick a ready host with spare capacity for a server's
// spec and atomically reserve that capacity. Reconciliation stays the sole
// writer of compute side effects — the scheduler only chooses and reserves; the
// reconciler persists the assignment and drives the provisioner.
package scheduler

import (
	"context"
	"errors"
	"sort"

	"github.com/aarani/craftling-go/internal/model"
)

// ErrNoCapacity means no ready host can currently accommodate the server. It is
// a transient condition (a host may free up or join), so the reconciler treats
// it as "unschedulable, retry next tick" rather than a hard error.
var ErrNoCapacity = errors.New("no host with sufficient capacity")

// hostStore is the slice of the host inventory the scheduler needs. The
// in-memory repository.HostRepository satisfies it; a future Postgres-backed
// store can too.
type hostStore interface {
	List(ctx context.Context) ([]model.Host, error)
	ListReady(ctx context.Context) ([]model.Host, error)
	Reserve(ctx context.Context, id string, cpus, memMB int) error
	Release(ctx context.Context, id string, cpus, memMB int) error
}

// Scheduler places servers onto hosts. It is stateless beyond its host store, so
// it is safe to construct one per caller (handler, reconciler) over a shared
// inventory.
type Scheduler struct {
	hosts hostStore
}

// New constructs a Scheduler over the given host inventory.
func New(hosts hostStore) *Scheduler { return &Scheduler{hosts: hosts} }

// Schedule selects a ready host with capacity for the server's spec, reserves
// that capacity, and returns the chosen host id. Placement is least-loaded:
// candidates are tried most-free first, which spreads servers across the fleet.
// Reservation is atomic, so if a racing scheduler grabbed the capacity between
// our snapshot and our reserve, we simply fall through to the next candidate.
// Returns ErrNoCapacity if nothing fits.
func (s *Scheduler) Schedule(ctx context.Context, gs *model.GameServer) (string, error) {
	ready, err := s.hosts.ListReady(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range fittingHosts(ready, gs.CPUs, gs.MemoryMB) {
		switch err := s.hosts.Reserve(ctx, h.ID, gs.CPUs, gs.MemoryMB); {
		case err == nil:
			return h.ID, nil
		default:
			// Lost a race, or the host went away / not-ready since the snapshot.
			// Try the next candidate.
			continue
		}
	}
	return "", ErrNoCapacity
}

// Release returns a server's reserved capacity to its host. The reconciler calls
// it when a placed server is torn down.
func (s *Scheduler) Release(ctx context.Context, hostID string, cpus, memMB int) error {
	return s.hosts.Release(ctx, hostID, cpus, memMB)
}

// Placeable reports whether the given host is currently a ready placement target.
// The reconciler uses it to detect a stale assignment: a server placed on a host
// that has since gone down — or whose agent reconnected under a new host id,
// leaving the old id behind — should release that assignment and re-schedule,
// rather than retry a host that can no longer accept work forever.
func (s *Scheduler) Placeable(ctx context.Context, hostID string) (bool, error) {
	ready, err := s.hosts.ListReady(ctx)
	if err != nil {
		return false, err
	}
	for _, h := range ready {
		if h.ID == hostID {
			return true, nil
		}
	}
	return false, nil
}

// CanEverFit reports whether any known host has enough *total* capacity to ever
// run this spec, regardless of current load or liveness. It backs create-time
// validation: a spec larger than the biggest host can never be placed, so it is
// rejected up front. With no hosts known yet it returns true — the server is
// allowed to wait unscheduled until a host joins rather than being rejected
// before the fleet exists.
func (s *Scheduler) CanEverFit(ctx context.Context, cpus, memMB int) (bool, error) {
	all, err := s.hosts.List(ctx)
	if err != nil {
		return false, err
	}
	if len(all) == 0 {
		return true, nil
	}
	for _, h := range all {
		if h.CPUsTotal >= cpus && h.MemoryMBTotal >= memMB {
			return true, nil
		}
	}
	return false, nil
}

// fittingHosts returns the hosts whose allocatable capacity covers the spec,
// ordered least-loaded first (most free memory, then most free cpu).
func fittingHosts(hosts []model.Host, cpus, memMB int) []model.Host {
	out := make([]model.Host, 0, len(hosts))
	for _, h := range hosts {
		if h.CPUsAllocatable >= cpus && h.MemoryMBAllocatable >= memMB {
			out = append(out, h)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MemoryMBAllocatable != out[j].MemoryMBAllocatable {
			return out[i].MemoryMBAllocatable > out[j].MemoryMBAllocatable
		}
		return out[i].CPUsAllocatable > out[j].CPUsAllocatable
	})
	return out
}
