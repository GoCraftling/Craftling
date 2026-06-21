package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/repository"
)

// futureTime is a cutoff after "now", so MarkStale treats a freshly registered
// host (heartbeat = now) as stale.
func futureTime() time.Time { return time.Now().Add(time.Hour) }

// registerHost adds a ready host of the given capacity and returns its id.
func registerHost(t *testing.T, repo *repository.HostRepository, hostname string, cpus, memMB int) string {
	t.Helper()
	h, err := repo.Register(context.Background(), &model.Host{
		Hostname:      hostname,
		Address:       hostname + ":9000",
		CPUsTotal:     cpus,
		MemoryMBTotal: memMB,
	})
	if err != nil {
		t.Fatalf("register %s: %v", hostname, err)
	}
	return h.ID
}

func spec(cpus, memMB int) *model.GameServer {
	return &model.GameServer{CPUs: cpus, MemoryMB: memMB}
}

// TestScheduleSpread verifies least-loaded placement balances servers evenly
// across equal hosts: 6 identical servers over 3 identical hosts → 2 each.
func TestScheduleSpread(t *testing.T) {
	repo := repository.NewHostRepository()
	ids := map[string]bool{
		registerHost(t, repo, "h1", 6, 6144): true,
		registerHost(t, repo, "h2", 6, 6144): true,
		registerHost(t, repo, "h3", 6, 6144): true,
	}
	s := New(repo)

	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		id, err := s.Schedule(context.Background(), spec(2, 2048))
		if err != nil {
			t.Fatalf("schedule %d: %v", i, err)
		}
		if !ids[id] {
			t.Fatalf("scheduled onto unknown host %q", id)
		}
		counts[id]++
	}
	for id := range ids {
		if counts[id] != 2 {
			t.Errorf("host %s got %d servers, want 2 (even spread); counts=%v", id, counts[id], counts)
		}
	}
}

// TestScheduleRespectsCapacity verifies a host accepts only as many servers as
// fit, then further placement reports ErrNoCapacity.
func TestScheduleRespectsCapacity(t *testing.T) {
	repo := repository.NewHostRepository()
	registerHost(t, repo, "only", 4, 4096) // room for exactly two 2-cpu servers
	s := New(repo)

	for i := 0; i < 2; i++ {
		if _, err := s.Schedule(context.Background(), spec(2, 2048)); err != nil {
			t.Fatalf("schedule %d should fit: %v", i, err)
		}
	}
	if _, err := s.Schedule(context.Background(), spec(2, 2048)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("third schedule err = %v, want ErrNoCapacity", err)
	}
}

// TestScheduleMemoryBound verifies memory is honored independently of cpu: a
// host with ample cpu but little memory cannot take a memory-heavy server.
func TestScheduleMemoryBound(t *testing.T) {
	repo := repository.NewHostRepository()
	registerHost(t, repo, "ramlight", 16, 2048)
	s := New(repo)

	if _, err := s.Schedule(context.Background(), spec(1, 4096)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity (over memory)", err)
	}
}

// TestScheduleSkipsDownHosts verifies only ready hosts receive placements.
func TestScheduleSkipsDownHosts(t *testing.T) {
	repo := repository.NewHostRepository()
	registerHost(t, repo, "doomed", 8, 8192)
	// Mark it down by sweeping with a cutoff in the far future.
	if _, err := repo.MarkStale(context.Background(), futureTime()); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	s := New(repo)

	if _, err := s.Schedule(context.Background(), spec(2, 2048)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity (host down)", err)
	}
}

// TestReleaseRestoresCapacity verifies released capacity becomes schedulable
// again.
func TestReleaseRestoresCapacity(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewHostRepository()
	id := registerHost(t, repo, "only", 2, 2048) // room for one 2-cpu server
	s := New(repo)

	if _, err := s.Schedule(ctx, spec(2, 2048)); err != nil {
		t.Fatalf("first schedule: %v", err)
	}
	if _, err := s.Schedule(ctx, spec(2, 2048)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("second schedule err = %v, want ErrNoCapacity", err)
	}
	if err := s.Release(ctx, id, 2, 2048); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.Schedule(ctx, spec(2, 2048)); err != nil {
		t.Fatalf("schedule after release should fit: %v", err)
	}
}

// TestCanEverFit covers create-time validation: allow when no fleet exists yet,
// allow specs that fit the biggest host, reject specs larger than any host.
func TestCanEverFit(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewHostRepository()
	s := New(repo)

	if ok, err := s.CanEverFit(ctx, 8, 8192); err != nil || !ok {
		t.Fatalf("empty fleet: ok=%v err=%v, want true", ok, err)
	}

	registerHost(t, repo, "small", 4, 4096)
	registerHost(t, repo, "big", 16, 32768)

	if ok, _ := s.CanEverFit(ctx, 16, 32768); !ok {
		t.Error("spec fitting the biggest host should be allowed")
	}
	if ok, _ := s.CanEverFit(ctx, 16, 65536); ok {
		t.Error("spec larger than any host should be rejected")
	}
	// A spec that exceeds the small host but fits the big one is allowed.
	if ok, _ := s.CanEverFit(ctx, 8, 8192); !ok {
		t.Error("spec fitting only the big host should still be allowed")
	}
}

// TestPlaceable verifies the stale-assignment check the reconciler relies on to
// rescue a server placed on a host that later vanished: a ready host is
// placeable; an unknown id (the host's agent reconnected under a fresh id) and a
// downed host are both not.
func TestPlaceable(t *testing.T) {
	ctx := context.Background()
	repo := repository.NewHostRepository()
	id := registerHost(t, repo, "live", 8, 8192)
	s := New(repo)

	if ok, err := s.Placeable(ctx, id); err != nil || !ok {
		t.Fatalf("ready host: ok=%v err=%v, want true", ok, err)
	}
	if ok, err := s.Placeable(ctx, "never-registered"); err != nil || ok {
		t.Fatalf("unknown host: ok=%v err=%v, want false", ok, err)
	}

	// Down the host (sweep with a far-future cutoff); it must stop being placeable.
	if _, err := repo.MarkStale(ctx, futureTime()); err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if ok, err := s.Placeable(ctx, id); err != nil || ok {
		t.Fatalf("downed host: ok=%v err=%v, want false", ok, err)
	}
}

// TestScheduleConcurrent verifies reservation is atomic under contention: with
// room for exactly 5 servers, 20 concurrent schedules grant exactly 5.
func TestScheduleConcurrent(t *testing.T) {
	repo := repository.NewHostRepository()
	registerHost(t, repo, "only", 10, 1<<20) // 10 cpu, ample memory
	s := New(repo)

	const attempts = 20
	var granted int64
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Schedule(context.Background(), spec(2, 1024)); err == nil {
				atomic.AddInt64(&granted, 1)
			}
		}()
	}
	wg.Wait()

	if granted != 5 {
		t.Fatalf("granted = %d, want 5 (10 cpu / 2 per server)", granted)
	}
}
