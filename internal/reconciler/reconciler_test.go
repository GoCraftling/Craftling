package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/scheduler"
	"go.uber.org/zap"
)

func intp(v int) *int { return &v }

// recordingProv is a Fake provisioner that records EvictVM calls and can be made
// to fail eviction for a given vm id, so the fence pass can be exercised offline.
type recordingProv struct {
	provisioner.Fake
	evicted [][2]string
	failVM  string
}

func (p *recordingProv) EvictVM(_ context.Context, hostID, vmID string) error {
	if vmID == p.failVM {
		return errors.New("evict failed")
	}
	p.evicted = append(p.evicted, [2]string{hostID, vmID})
	return nil
}

// fakeFences is an in-memory FenceStore for unit tests.
type fakeFences struct {
	items   []model.FencedVM
	deleted [][2]string
}

func (f *fakeFences) Add(_ context.Context, fv model.FencedVM) error {
	f.items = append(f.items, fv)
	return nil
}

func (f *fakeFences) List(_ context.Context) ([]model.FencedVM, error) {
	return append([]model.FencedVM(nil), f.items...), nil
}

func (f *fakeFences) Delete(_ context.Context, hostID, vmID string) error {
	f.deleted = append(f.deleted, [2]string{hostID, vmID})
	kept := f.items[:0]
	for _, fv := range f.items {
		if fv.HostID == hostID && fv.VMID == vmID {
			continue
		}
		kept = append(kept, fv)
	}
	f.items = kept
	return nil
}

func (f *fakeFences) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	var n int64
	kept := f.items[:0]
	for _, fv := range f.items {
		if fv.CreatedAt.Before(cutoff) {
			n++
			continue
		}
		kept = append(kept, fv)
	}
	f.items = kept
	return n, nil
}

func (f *fakeFences) has(hostID, vmID string) bool {
	for _, fv := range f.items {
		if fv.HostID == hostID && fv.VMID == vmID {
			return true
		}
	}
	return false
}

// TestReconcileFences verifies the orphan fence pass (P8b): a fence whose host is
// reachable again is evicted and cleared; one whose host is still gone is kept; a
// failed eviction leaves the fence for retry; and a fence older than the GC
// horizon is swept regardless.
func TestReconcileFences(t *testing.T) {
	ctx := context.Background()
	hosts := repository.NewHostRepository()
	// h-up is reachable (registered → ready); h-down is not in the fleet.
	if _, err := hosts.Register(ctx, &model.Host{ID: "h-up", CPUsTotal: 4, MemoryMBTotal: 4096}); err != nil {
		t.Fatal(err)
	}
	prov := &recordingProv{failVM: "vm-fail"}
	fences := &fakeFences{items: []model.FencedVM{
		{ServerID: "s1", HostID: "h-up", VMID: "vm-1", Generation: 2, CreatedAt: time.Now()},
		{ServerID: "s2", HostID: "h-down", VMID: "vm-2", Generation: 1, CreatedAt: time.Now()},
		{ServerID: "s3", HostID: "h-up", VMID: "vm-fail", Generation: 3, CreatedAt: time.Now()},
		{ServerID: "s4", HostID: "h-down", VMID: "vm-old", Generation: 1, CreatedAt: time.Now().Add(-2 * fenceGCAfter)},
	}}
	r := &Reconciler{prov: prov, sched: scheduler.New(hosts), fences: fences, log: zap.NewNop()}

	r.reconcileFences(ctx)

	// The reachable host's evictable orphan was evicted and its fence cleared.
	if len(prov.evicted) != 1 || prov.evicted[0] != [2]string{"h-up", "vm-1"} {
		t.Fatalf("evicted = %v; want [[h-up vm-1]]", prov.evicted)
	}
	if fences.has("h-up", "vm-1") {
		t.Error("evicted orphan's fence should be cleared")
	}
	// The unreachable host's fence is kept (host may still return).
	if !fences.has("h-down", "vm-2") {
		t.Error("fence for an unreachable host should be retained")
	}
	// A failed eviction leaves the fence in place to retry.
	if !fences.has("h-up", "vm-fail") {
		t.Error("fence for a failed eviction should be retained")
	}
	// The aged-out fence was GC'd even though its host is gone.
	if fences.has("h-down", "vm-old") {
		t.Error("fence older than the GC horizon should be swept")
	}
}

func TestHealthNeedsWrite(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Second)
	stale := now.Add(-2 * healthRefreshInterval)

	cases := []struct {
		name string
		s    model.GameServer
		h    runspec.Health
		want bool
	}{
		{
			name: "first reachable reading writes",
			s:    model.GameServer{},
			h:    runspec.Health{Reachable: true, PlayersOnline: 0, PlayersMax: 20},
			want: true,
		},
		{
			name: "unchanged counts with fresh last_seen skips",
			s:    model.GameServer{PlayersOnline: intp(3), PlayersMax: intp(20), LastSeen: &fresh},
			h:    runspec.Health{Reachable: true, PlayersOnline: 3, PlayersMax: 20},
			want: false,
		},
		{
			name: "changed online count writes",
			s:    model.GameServer{PlayersOnline: intp(3), PlayersMax: intp(20), LastSeen: &fresh},
			h:    runspec.Health{Reachable: true, PlayersOnline: 4, PlayersMax: 20},
			want: true,
		},
		{
			name: "stale last_seen refreshes even when counts hold",
			s:    model.GameServer{PlayersOnline: intp(3), PlayersMax: intp(20), LastSeen: &stale},
			h:    runspec.Health{Reachable: true, PlayersOnline: 3, PlayersMax: 20},
			want: true,
		},
		{
			name: "becoming unreachable clears stored counts",
			s:    model.GameServer{PlayersOnline: intp(3), PlayersMax: intp(20), LastSeen: &fresh},
			h:    runspec.Health{Reachable: false},
			want: true,
		},
		{
			name: "staying unreachable with no stored counts skips",
			s:    model.GameServer{},
			h:    runspec.Health{Reachable: false},
			want: false,
		},
	}

	r := &Reconciler{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.healthNeedsWrite(&c.s, &c.h); got != c.want {
				t.Errorf("healthNeedsWrite = %v, want %v", got, c.want)
			}
		})
	}
}

func TestBackoffDelay(t *testing.T) {
	base := time.Second
	max := 30 * time.Second
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{1, time.Second},      // first failure waits the base
		{2, 2 * time.Second},  // doubles
		{3, 4 * time.Second},  // doubles
		{4, 8 * time.Second},  // doubles
		{5, 16 * time.Second}, // doubles
		{6, 30 * time.Second}, // would be 32s, capped at max
		{7, 30 * time.Second}, // stays capped
		{20, 30 * time.Second},
	}
	for _, c := range cases {
		if got := backoffDelay(base, max, c.attempts); got != c.want {
			t.Errorf("backoffDelay(attempts=%d) = %s, want %s", c.attempts, got, c.want)
		}
	}

	// A non-positive base disables spacing entirely (retry on the next tick).
	if got := backoffDelay(0, max, 3); got != 0 {
		t.Errorf("backoffDelay with zero base = %s, want 0", got)
	}
}

func TestEqIntPtr(t *testing.T) {
	a, b := 1, 1
	cases := []struct {
		x, y *int
		want bool
	}{
		{nil, nil, true},
		{&a, nil, false},
		{nil, &b, false},
		{&a, &b, true},
		{intp(1), intp(2), false},
	}
	for _, c := range cases {
		if got := eqIntPtr(c.x, c.y); got != c.want {
			t.Errorf("eqIntPtr(%v, %v) = %v, want %v", c.x, c.y, got, c.want)
		}
	}
}
