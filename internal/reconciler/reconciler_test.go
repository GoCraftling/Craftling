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
