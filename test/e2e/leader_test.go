//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/leader"
	"go.uber.org/zap"
)

// TestLeaderElection verifies the P8d advisory-lock election: with two campaigners
// on one key only one leads at a time, and when the leader steps down the follower
// takes over.
func TestLeaderElection(t *testing.T) {
	// A test-specific key, distinct from the production leader key, so this test is
	// independent of anything else holding a lock.
	const key int64 = 0x7465_7374_38_64 // "test8d"

	leaderCh := make(chan int, 4)
	steppedCh := make(chan int, 4)
	runFor := func(id int) func(context.Context) {
		return func(lctx context.Context) {
			leaderCh <- id
			go func() {
				<-lctx.Done()
				steppedCh <- id
			}()
		}
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go leader.Campaign(ctx1, pool, key, 100*time.Millisecond, zap.NewNop(), runFor(1))

	// Campaigner 1 should win leadership.
	select {
	case got := <-leaderCh:
		if got != 1 {
			t.Fatalf("first leader = %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("campaigner 1 never became leader")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go leader.Campaign(ctx2, pool, key, 100*time.Millisecond, zap.NewNop(), runFor(2))

	// Campaigner 2 must NOT lead while 1 holds the lock.
	select {
	case got := <-leaderCh:
		t.Fatalf("campaigner %d became leader while 1 still held the lock", got)
	case <-time.After(time.Second):
	}

	// Step 1 down; 2 must take over.
	cancel1()
	select {
	case got := <-steppedCh:
		if got != 1 {
			t.Fatalf("stepped-down = %d, want 1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader 1 never stepped down after cancel")
	}
	select {
	case got := <-leaderCh:
		if got != 2 {
			t.Fatalf("successor leader = %d, want 2", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("campaigner 2 never took over leadership")
	}
}
