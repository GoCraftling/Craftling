// Package storage is the durable home of game-server world data (P5b).
//
// On the squashfs+init image path a VM's rootfs is read-only and its
// writable world lives on a per-server ext4 "world disk" (see
// internal/agent/firecracker). That disk makes a world survive a stop/start on
// one host, but it is still local: deleting the VM, or moving a server to
// another host, would lose it. A WorldStore is the off-host system of record —
// an object store (S3) or shared filesystem (NFS) — that the agent snapshots
// the disk into and restores it from, so a world outlives any single VM or
// host.
//
// The agent owns the disk<->stream codec (gzip of the raw ext4 image); the
// store moves opaque bytes keyed by server id and knows nothing about their
// shape. That keeps backends trivial to add: a new WorldStore only has to
// stream bytes in and out.
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
)

// WorldSuffix is appended to a SafeKey to name a stored snapshot. The contents
// are opaque to the store (the agent gzips a raw ext4 image into them); the
// suffix only marks the object/file as a world snapshot. Shared by both
// backends so their naming can't drift.
const WorldSuffix = ".world"

// GenSuffix names the per-server generation watermark that sits beside a
// snapshot. The watermark is the highest VM incarnation (generation token) that
// has claimed or written this server's world; the store rejects any Put/Claim
// from a lower generation so a partitioned host's zombie VM (P8b) cannot clobber
// the world a rescheduled, higher-generation VM now owns.
const GenSuffix = ".gen"

// ErrWorldNotFound is returned by Get when no snapshot is stored for a server.
// Callers use it to distinguish "this server has no saved world yet" (boot a
// fresh disk) from a real I/O failure.
var ErrWorldNotFound = errors.New("storage: world not found")

// ErrStaleGeneration is returned by Put/Claim when the caller's generation is
// older than the watermark already recorded for the server — i.e. a newer VM
// incarnation has superseded this one. The caller (the agent's snapshot path)
// treats it as "my write was correctly fenced out", not a fault.
var ErrStaleGeneration = errors.New("storage: stale generation")

// WorldStore is the durable store of per-server world snapshots. A snapshot is
// an opaque byte stream the agent produces from a world disk; the store keys it
// by server id. Implementations must be safe for concurrent use by different
// servers (the agent runs many VMs at once); concurrent operations on the same
// server id are the caller's responsibility to avoid (the agent serializes a
// server's lifecycle).
type WorldStore interface {
	// Exists reports whether a snapshot is stored for serverID. The agent
	// checks this on Provision to decide between restoring and formatting fresh.
	Exists(ctx context.Context, serverID string) (bool, error)

	// Put stores (replacing any prior) the snapshot for serverID, reading the
	// stream to EOF, and raises the server's generation watermark to generation.
	// A failure must not leave a partial snapshot a later Get would hand back as
	// if whole. Returns ErrStaleGeneration — without writing — when generation is
	// older than the recorded watermark, fencing a superseded VM's write out.
	Put(ctx context.Context, serverID string, generation int64, r io.Reader) error

	// Claim raises the server's generation watermark to generation without writing
	// a snapshot. The agent calls it when a fresh VM restores/boots a world, so the
	// new incarnation fences out any lower-generation zombie before it ever
	// snapshots. Returns ErrStaleGeneration when generation is older than the
	// recorded watermark (a downgrade, which must not happen for a live VM).
	Claim(ctx context.Context, serverID string, generation int64) error

	// Get opens the stored snapshot for serverID. The caller closes the
	// returned reader. Returns ErrWorldNotFound when nothing is stored.
	Get(ctx context.Context, serverID string) (io.ReadCloser, error)

	// Delete removes the snapshot for serverID (and its generation watermark). A
	// missing snapshot is not an error — delete is idempotent teardown.
	Delete(ctx context.Context, serverID string) error

	// List returns the keys of all stored snapshots. Each key is a SafeKey'd
	// server id (SafeKey is idempotent, so a returned key round-trips back
	// through the other methods unchanged). The world GC reaper uses it to find
	// snapshots no live server claims.
	List(ctx context.Context) ([]string, error)
}

// SafeKey maps a server id to a single safe object/path token, replacing
// anything outside [A-Za-z0-9._-] with '_' so an id can never traverse a
// filesystem path or smuggle separators into an object key. It mirrors the
// firecracker driver's disk-keying guard; an empty/all-unsafe id collapses to
// "_". Both the DirStore and S3 backends use it to derive their on-disk/object
// name from a server id.
func SafeKey(serverID string) string {
	var b strings.Builder
	for _, r := range serverID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}
