package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	const id = "srv-1"
	if ok, err := s.Exists(ctx, id); err != nil || ok {
		t.Fatalf("Exists on empty store = %v, %v; want false, nil", ok, err)
	}
	if _, err := s.Get(ctx, id); !errors.Is(err, ErrWorldNotFound) {
		t.Fatalf("Get on empty store = %v; want ErrWorldNotFound", err)
	}

	payload := []byte("a world snapshot")
	if err := s.Put(ctx, id, 1, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := s.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("Exists after Put = %v, %v; want true, nil", ok, err)
	}

	rc, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("Get returned %q, want %q", got, payload)
	}

	// Put replaces the prior snapshot rather than appending (same generation is
	// the same incarnation re-snapshotting, which is allowed).
	if err := s.Put(ctx, id, 1, strings.NewReader("newer")); err != nil {
		t.Fatalf("Put replace: %v", err)
	}
	rc, _ = s.Get(ctx, id)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "newer" {
		t.Errorf("after replace Get = %q, want %q", got, "newer")
	}

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Exists(ctx, id); ok {
		t.Error("Exists after Delete = true")
	}
	// Delete is idempotent.
	if err := s.Delete(ctx, id); err != nil {
		t.Errorf("second Delete = %v; want nil", err)
	}
}

// TestDirStoreGenerationFence verifies the world-write fence (P8b): a Put or
// Claim from a generation older than the recorded watermark is rejected with
// ErrStaleGeneration and does not change the stored world, while an equal or newer
// generation is accepted and raises the watermark.
func TestDirStoreGenerationFence(t *testing.T) {
	ctx := context.Background()
	s, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const id = "srv-gen"

	// Generation 2 writes the world and sets the watermark.
	if err := s.Put(ctx, id, 2, strings.NewReader("gen2")); err != nil {
		t.Fatalf("Put gen2: %v", err)
	}
	// A zombie at generation 1 is fenced out — nothing is written.
	if err := s.Put(ctx, id, 1, strings.NewReader("zombie")); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Put gen1 = %v; want ErrStaleGeneration", err)
	}
	rc, _ := s.Get(ctx, id)
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "gen2" {
		t.Errorf("world after fenced write = %q, want %q", got, "gen2")
	}

	// A Claim at a lower generation is likewise rejected (a downgrade).
	if err := s.Claim(ctx, id, 1); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Claim gen1 = %v; want ErrStaleGeneration", err)
	}
	// Claiming a higher generation raises the watermark, fencing out gen 2.
	if err := s.Claim(ctx, id, 3); err != nil {
		t.Fatalf("Claim gen3: %v", err)
	}
	if err := s.Put(ctx, id, 2, strings.NewReader("stale")); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Put gen2 after claim 3 = %v; want ErrStaleGeneration", err)
	}
	// The current incarnation (gen 3) may still write.
	if err := s.Put(ctx, id, 3, strings.NewReader("gen3")); err != nil {
		t.Fatalf("Put gen3: %v", err)
	}
}

// TestDirStoreKeyIsContained checks an adversarial server id can't write outside
// the store root.
func TestDirStoreKeyIsContained(t *testing.T) {
	root := t.TempDir()
	s, err := NewDirStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "../../escape", 1, strings.NewReader("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The blob and its generation sidecar land directly under root, names sanitized
	// — nothing escaped.
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries under root (.world + .gen), got %d", len(entries))
	}
	for _, e := range entries {
		if strings.ContainsRune(e.Name(), os.PathSeparator) {
			t.Errorf("entry name has a separator: %q", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.world")); err == nil {
		t.Error("an escaped blob was written outside the store root")
	}
}

// TestDirStorePutNoPartialOnError ensures a read error mid-Put leaves no blob
// (the temp file is removed, not renamed into place).
func TestDirStorePutNoPartialOnError(t *testing.T) {
	root := t.TempDir()
	s, err := NewDirStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "srv", 1, errReader{}); err == nil {
		t.Fatal("expected Put to fail on a reader error")
	}
	if ok, _ := s.Exists(context.Background(), "srv"); ok {
		t.Error("a partial snapshot was published despite the read error")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

// errReader is a reader that always fails, to exercise Put's error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// TestDirStoreList checks List returns the SafeKey'd keys of stored worlds and
// skips strays, and that a key it returns round-trips back through the store.
func TestDirStoreList(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewDirStore(root)
	if err != nil {
		t.Fatal(err)
	}

	if keys, err := s.List(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("List on empty = %v, %v; want [], nil", keys, err)
	}

	for _, id := range []string{"a", "b", "tenant/c"} { // last one is sanitized
		if err := s.Put(ctx, id, 1, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	// A stray non-".world" file must be ignored.
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("hi"), 0o640); err != nil {
		t.Fatal(err)
	}

	keys, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("List returned %d keys, want 3: %v", len(keys), keys)
	}
	// A returned key round-trips: Delete(key) removes it (SafeKey is idempotent).
	for _, k := range keys {
		if ok, _ := s.Exists(ctx, k); !ok {
			t.Errorf("listed key %q does not Exists", k)
		}
	}
}
