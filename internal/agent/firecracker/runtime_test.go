package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/image"
	"github.com/aarani/craftling-go/internal/runspec"
)

// testImageStore returns an image.Store over a throwaway cache dir. The
// non-KVM unit tests never actually convert an image (they stop before any
// Ensure call), so the store only needs to satisfy Config.validate.
func testImageStore(t *testing.T) *image.Store {
	t.Helper()
	return &image.Store{CacheDir: t.TempDir()}
}

// newTestRuntime builds a Runtime over throwaway host artifacts so the
// non-KVM unit tests can exercise validation, image resolution, and the
// no-process idempotency edges without ever launching Firecracker.
func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatalf("write kernel: %v", err)
	}
	rt, err := New(Config{
		KernelPath:    kernel,
		ImageStore:    testImageStore(t),
		ImageRef:      "example.invalid/mc:{version}",
		WorkDir:       filepath.Join(dir, "work"),
		AdvertiseHost: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

func TestNewValidatesArtifacts(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := testImageStore(t)

	if _, err := New(Config{ImageStore: store, ImageRef: "x:{version}"}); err == nil {
		t.Error("missing kernel: expected error")
	}
	if _, err := New(Config{KernelPath: filepath.Join(dir, "nope"), ImageStore: store, ImageRef: "x"}); err == nil {
		t.Error("nonexistent kernel: expected error")
	}
	if _, err := New(Config{KernelPath: kernel, ImageRef: "x"}); err == nil {
		t.Error("missing image store: expected error")
	}
	if _, err := New(Config{KernelPath: kernel, ImageStore: store}); err == nil {
		t.Error("missing image ref and default: expected error")
	}
}

func TestImageRefFor(t *testing.T) {
	templated := Config{ImageRef: "repo/mc:{version}", DefaultImageRef: "repo/mc:latest"}
	if got, err := templated.imageRefFor("1.20.4"); err != nil || got != "repo/mc:1.20.4" {
		t.Errorf("imageRefFor(version) = %q, %v; want repo/mc:1.20.4", got, err)
	}
	if got, err := templated.imageRefFor(""); err != nil || got != "repo/mc:latest" {
		t.Errorf("imageRefFor(\"\") = %q, %v; want the default ref", got, err)
	}

	fixed := Config{ImageRef: "repo/mc@sha256:abc"}
	if got, err := fixed.imageRefFor("1.20.4"); err != nil || got != "repo/mc@sha256:abc" {
		t.Errorf("imageRefFor(fixed) = %q, %v; want the ref verbatim", got, err)
	}

	noDefault := Config{ImageRef: "repo/mc:{version}"}
	if _, err := noDefault.imageRefFor(""); err == nil {
		t.Error("templated ref with no version and no default: expected error")
	}

	defaultOnly := Config{DefaultImageRef: "repo/d:latest"}
	if got, err := defaultOnly.imageRefFor(""); err != nil || got != "repo/d:latest" {
		t.Errorf("imageRefFor(default only) = %q, %v; want repo/d:latest", got, err)
	}
}

func TestApplyRunSpecOverride(t *testing.T) {
	base := runspec.RunSpec{
		Entrypoint: []string{"/entry"},
		Cmd:        []string{"default"},
		Env:        []string{"PATH=/bin", "VERSION=LATEST"},
		WorkingDir: "/data",
	}

	// A no-op override leaves the base untouched.
	noop := base
	applyRunSpecOverride(&noop, &runspec.RunSpec{})
	if !reflect.DeepEqual(noop, base) {
		t.Errorf("empty override mutated base: got %+v", noop)
	}

	// Cmd replaces both Entrypoint and Cmd; Env merges by key; WorkingDir wins.
	got := base
	applyRunSpecOverride(&got, &runspec.RunSpec{
		Cmd:        []string{"/bin/sh", "-c", "echo hi"},
		Env:        []string{"VERSION=1.20.4", "EXTRA=1"},
		WorkingDir: "/root",
	})
	if len(got.Entrypoint) != 0 {
		t.Errorf("Entrypoint = %v, want cleared when Cmd overridden", got.Entrypoint)
	}
	wantCmd := []string{"/bin/sh", "-c", "echo hi"}
	if !reflect.DeepEqual(got.Cmd, wantCmd) {
		t.Errorf("Cmd = %v, want %v", got.Cmd, wantCmd)
	}
	wantEnv := []string{"PATH=/bin", "VERSION=1.20.4", "EXTRA=1"}
	if !reflect.DeepEqual(got.Env, wantEnv) {
		t.Errorf("Env = %v, want %v (merged by key)", got.Env, wantEnv)
	}
	if got.WorkingDir != "/root" {
		t.Errorf("WorkingDir = %q, want /root", got.WorkingDir)
	}
}

// TestLifecycleIdempotencyNoProcess covers the contract edges the control plane
// relies on, none of which touch a real VM: operations on unknown ids.
func TestLifecycleIdempotencyNoProcess(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()

	if err := rt.Stop(ctx, "ghost"); err != nil {
		t.Errorf("stop unknown = %v, want nil (idempotent)", err)
	}
	if err := rt.Deprovision(ctx, "ghost"); err != nil {
		t.Errorf("deprovision unknown = %v, want nil (idempotent)", err)
	}
	if _, err := rt.Start(ctx, "ghost"); !errors.Is(err, agent.ErrVMNotFound) {
		t.Errorf("start unknown = %v, want ErrVMNotFound", err)
	}
	vm, err := rt.Status(ctx, "ghost")
	if err != nil {
		t.Fatalf("status unknown: %v", err)
	}
	if vm.State != agent.StateMissing {
		t.Errorf("status unknown state = %q, want missing", vm.State)
	}
}

func TestProvisionRejectsInvalidSpec(t *testing.T) {
	rt := newTestRuntime(t)
	ctx := context.Background()
	for _, spec := range []agent.VMSpec{
		{Version: "1.20.4", CPUs: 0, MemoryMB: 1024},
		{Version: "1.20.4", CPUs: 2, MemoryMB: 0},
	} {
		if _, err := rt.Provision(ctx, spec); err == nil {
			t.Errorf("Provision(%+v): expected error", spec)
		}
	}
}

// TestProvisionUnresolvableImage checks Provision fails fast — before any
// network pull — when the image reference can't be resolved. newTestRuntime's
// ImageRef is templated with no DefaultImageRef, so a version-less spec has no
// ref to convert.
func TestProvisionUnresolvableImage(t *testing.T) {
	rt := newTestRuntime(t)
	if _, err := rt.Provision(context.Background(),
		agent.VMSpec{Version: "", CPUs: 2, MemoryMB: 1024}); err == nil {
		t.Error("Provision with unresolvable image ref: expected error")
	}
}
