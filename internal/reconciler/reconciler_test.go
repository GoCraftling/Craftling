package reconciler

import (
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/runspec"
)

func intp(v int) *int { return &v }

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
