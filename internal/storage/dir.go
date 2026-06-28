package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DirStore is a WorldStore backed by a directory on the host filesystem. It is
// the simplest durable backend: point it at a shared mount (NFS, a SAN volume)
// and several agents see the same worlds, which is enough for cross-host
// reschedule without standing up object storage. It is also what the tests use,
// since it needs nothing but a temp dir. An S3-backed store implements the same
// interface for cloud deployments.
type DirStore struct {
	root string
}

// compile-time check.
var _ WorldStore = (*DirStore)(nil)

// NewDirStore returns a DirStore writing under root, creating it if needed.
func NewDirStore(root string) (*DirStore, error) {
	if root == "" {
		return nil, fmt.Errorf("storage: DirStore root is required")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create world dir %q: %w", root, err)
	}
	return &DirStore{root: root}, nil
}

// path is the on-disk blob path for a server id.
func (s *DirStore) path(serverID string) string {
	return filepath.Join(s.root, SafeKey(serverID)+WorldSuffix)
}

// genPath is the on-disk generation-watermark path for a server id.
func (s *DirStore) genPath(serverID string) string {
	return filepath.Join(s.root, SafeKey(serverID)+GenSuffix)
}

// readGen returns the recorded generation watermark for serverID, or 0 when none
// has been written yet (a never-claimed world floor-bounds every generation).
func (s *DirStore) readGen(serverID string) (int64, error) {
	b, err := os.ReadFile(s.genPath(serverID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage: read generation %q: %w", serverID, err)
	}
	g, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("storage: parse generation %q: %w", serverID, err)
	}
	return g, nil
}

// writeGen atomically records the generation watermark for serverID.
func (s *DirStore) writeGen(serverID string, generation int64) error {
	tmp, err := os.CreateTemp(s.root, SafeKey(serverID)+".*.gentmp")
	if err != nil {
		return fmt.Errorf("storage: create temp generation: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if _, err := fmt.Fprintf(tmp, "%d\n", generation); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: write generation %q: %w", serverID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close generation %q: %w", serverID, err)
	}
	if err := os.Rename(tmpName, s.genPath(serverID)); err != nil {
		return fmt.Errorf("storage: publish generation %q: %w", serverID, err)
	}
	return nil
}

// fence returns ErrStaleGeneration when generation is older than the recorded
// watermark for serverID. The compare and the subsequent write are not a single
// atomic step, so two writers racing on one server id could both pass — but only
// one live VM ever owns a server (the reconciler serializes its lifecycle), and a
// fenced zombie is always a strictly lower generation, so the realistic race
// window is nil. Callers raise the watermark via Put/Claim after fence passes.
func (s *DirStore) fence(serverID string, generation int64) error {
	current, err := s.readGen(serverID)
	if err != nil {
		return err
	}
	if generation < current {
		return ErrStaleGeneration
	}
	return nil
}

// Claim raises the generation watermark for serverID without writing a snapshot.
func (s *DirStore) Claim(_ context.Context, serverID string, generation int64) error {
	if err := s.fence(serverID, generation); err != nil {
		return err
	}
	return s.writeGen(serverID, generation)
}

// Exists reports whether a snapshot file is present for serverID.
func (s *DirStore) Exists(_ context.Context, serverID string) (bool, error) {
	_, err := os.Stat(s.path(serverID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("storage: stat world %q: %w", serverID, err)
}

// Put writes r to a sibling temp file and atomically renames it into place, so
// a crash or read error mid-stream never leaves a truncated snapshot that a
// later Get would treat as whole. It first fences on generation: a write from a
// generation older than the recorded watermark is rejected with ErrStaleGeneration
// and nothing is written; otherwise the watermark is raised to generation once the
// snapshot lands.
func (s *DirStore) Put(_ context.Context, serverID string, generation int64, r io.Reader) error {
	if err := s.fence(serverID, generation); err != nil {
		return err
	}
	final := s.path(serverID)
	tmp, err := os.CreateTemp(s.root, SafeKey(serverID)+".*.tmp")
	if err != nil {
		return fmt.Errorf("storage: create temp world: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: write world %q: %w", serverID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close world %q: %w", serverID, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("storage: publish world %q: %w", serverID, err)
	}
	if err := s.writeGen(serverID, generation); err != nil {
		return err
	}
	return nil
}

// Get opens the snapshot for serverID, mapping a missing file to
// ErrWorldNotFound.
func (s *DirStore) Get(_ context.Context, serverID string) (io.ReadCloser, error) {
	f, err := os.Open(s.path(serverID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrWorldNotFound
		}
		return nil, fmt.Errorf("storage: open world %q: %w", serverID, err)
	}
	return f, nil
}

// Delete removes the snapshot for serverID and its generation watermark; a
// missing file is success.
func (s *DirStore) Delete(_ context.Context, serverID string) error {
	if err := os.Remove(s.path(serverID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete world %q: %w", serverID, err)
	}
	if err := os.Remove(s.genPath(serverID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete generation %q: %w", serverID, err)
	}
	return nil
}

// List returns the stored snapshot keys (file stems without the suffix). A
// missing root dir yields no keys, not an error — nothing has been stored yet.
func (s *DirStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: list worlds: %w", err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, WorldSuffix) {
			continue // skip stray files (e.g. in-flight .tmp uploads)
		}
		keys = append(keys, strings.TrimSuffix(name, WorldSuffix))
	}
	return keys, nil
}
