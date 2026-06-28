package reaper

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/aarani/craftling-go/internal/storage"
	"go.uber.org/zap"
)

// fakeLister is a stand-in for the game-server repository's ListActiveIDs.
type fakeLister struct {
	ids []string
	err error
}

func (f fakeLister) ListActiveIDs(context.Context) ([]string, error) { return f.ids, f.err }

func seedWorld(t *testing.T, s storage.WorldStore, id string) {
	t.Helper()
	if err := s.Put(context.Background(), id, 1, bytes.NewReader([]byte("w"))); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func storedKeys(t *testing.T, s storage.WorldStore) []string {
	t.Helper()
	keys, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sort.Strings(keys)
	return keys
}

// TestReapWorldsRemovesOrphans checks worlds with no live server are deleted
// while live servers' worlds are kept.
func TestReapWorldsRemovesOrphans(t *testing.T) {
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWorld(t, store, "live-1")
	seedWorld(t, store, "live-2")
	seedWorld(t, store, "orphan-1")
	seedWorld(t, store, "orphan-2")

	reapWorlds(context.Background(), zap.NewNop(), store, fakeLister{ids: []string{"live-1", "live-2"}})

	got := storedKeys(t, store)
	want := []string{"live-1", "live-2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("after GC stored = %v, want %v", got, want)
	}
}

// TestReapWorldsKeepsAllOnListError ensures a failure to enumerate live servers
// deletes nothing — a transient DB error must never be read as "no servers".
func TestReapWorldsKeepsAllOnListError(t *testing.T) {
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedWorld(t, store, "a")
	seedWorld(t, store, "b")

	reapWorlds(context.Background(), zap.NewNop(), store, fakeLister{err: errors.New("db down")})

	if got := storedKeys(t, store); len(got) != 2 {
		t.Errorf("GC deleted worlds despite a lister error: %v", got)
	}
}

// TestReapWorldsSafeKeyMatch verifies live ids are matched against stored keys
// through SafeKey, so an id needing sanitization still protects its world.
func TestReapWorldsSafeKeyMatch(t *testing.T) {
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// An id with an unsafe char is stored under its SafeKey; GC must not treat
	// it as an orphan when the same id is live.
	const rawID = "tenant/srv:1"
	seedWorld(t, store, rawID)

	reapWorlds(context.Background(), zap.NewNop(), store, fakeLister{ids: []string{rawID}})

	if got := storedKeys(t, store); len(got) != 1 {
		t.Errorf("GC removed a live server's world due to key mismatch: %v", got)
	}
}
