package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// TestFakeRuntimeLifecycle exercises the in-memory runtime directly through its
// full VM lifecycle and idempotency edges.
func TestFakeRuntimeLifecycle(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.7")

	vm, err := rt.Provision(ctx, VMSpec{ServerID: "s1", Game: "minecraft", Version: "1.20.4", CPUs: 2, MemoryMB: 2048})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if vm.ID == "" || vm.State != StateRunning {
		t.Fatalf("provisioned vm = %+v, want running with id", vm)
	}
	if vm.Host != "10.0.0.7" || vm.Port != defaultMinecraftPort {
		t.Errorf("vm connect = %s:%d, want 10.0.0.7:%d", vm.Host, vm.Port, defaultMinecraftPort)
	}
	if vm.ServerID != "s1" {
		t.Errorf("server_id = %q, want s1", vm.ServerID)
	}

	assertState(t, rt, vm.ID, StateRunning)

	if err := rt.Stop(ctx, vm.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertState(t, rt, vm.ID, StateStopped)

	if _, err := rt.Start(ctx, vm.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertState(t, rt, vm.ID, StateRunning)

	if err := rt.Deprovision(ctx, vm.ID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	assertState(t, rt, vm.ID, StateMissing)
}

// TestFakeRuntimeIdempotency covers the edges the control plane relies on.
func TestFakeRuntimeIdempotency(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("")

	if err := rt.Stop(ctx, "ghost"); err != nil {
		t.Errorf("stop unknown vm = %v, want nil (idempotent)", err)
	}
	if err := rt.Deprovision(ctx, "ghost"); err != nil {
		t.Errorf("deprovision unknown vm = %v, want nil (idempotent)", err)
	}
	if _, err := rt.Start(ctx, "ghost"); !errors.Is(err, ErrVMNotFound) {
		t.Errorf("start unknown vm = %v, want ErrVMNotFound", err)
	}
}

// TestAgentServerClientRoundTrip drives the runtime through the HTTP API the
// control plane uses, verifying the wire contract end-to-end.
func TestAgentServerClientRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(NewRouter(NewFakeRuntime("10.0.0.9"), zap.NewNop()))
	defer srv.Close()

	client := NewClient(nil)
	base := srv.URL

	vm, err := client.Provision(ctx, base, VMSpec{ServerID: "s2", Version: "1.20.4", CPUs: 1, MemoryMB: 1024})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if vm == nil || vm.ID == "" || vm.State != StateRunning {
		t.Fatalf("provisioned vm = %+v, want running with id", vm)
	}
	if vm.Host != "10.0.0.9" || vm.Port != defaultMinecraftPort {
		t.Errorf("connect = %s:%d, want 10.0.0.9:%d", vm.Host, vm.Port, defaultMinecraftPort)
	}

	if got := statusOf(t, client, base, vm.ID); got != StateRunning {
		t.Errorf("after provision state = %q, want running", got)
	}
	if err := client.Stop(ctx, base, vm.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := statusOf(t, client, base, vm.ID); got != StateStopped {
		t.Errorf("after stop state = %q, want stopped", got)
	}
	if _, err := client.Start(ctx, base, vm.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := statusOf(t, client, base, vm.ID); got != StateRunning {
		t.Errorf("after start state = %q, want running", got)
	}
	if err := client.Deprovision(ctx, base, vm.ID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if got := statusOf(t, client, base, vm.ID); got != StateMissing {
		t.Errorf("after deprovision state = %q, want missing", got)
	}

	// Starting a VM the agent does not know is an error over the wire.
	if _, err := client.Start(ctx, base, "vm-ghost"); err == nil {
		t.Error("start unknown vm: expected error, got nil")
	}
}

// TestAgentSnapshotRoundTrip exercises the on-demand snapshot endpoint over the
// real HTTP seam: a known VM succeeds (the fake runtime no-ops), an unknown one
// surfaces a not-found error to the caller.
func TestAgentSnapshotRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(NewRouter(NewFakeRuntime("10.0.0.9"), zap.NewNop()))
	defer srv.Close()
	client := NewClient(nil)
	base := srv.URL

	vm, err := client.Provision(ctx, base, VMSpec{ServerID: "s3", CPUs: 1, MemoryMB: 1024})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := client.Snapshot(ctx, base, vm.ID); err != nil {
		t.Fatalf("snapshot known vm: %v", err)
	}
	if err := client.Snapshot(ctx, base, "vm-ghost"); err == nil {
		t.Error("snapshot unknown vm: expected error, got nil")
	}
}

func assertState(t *testing.T, rt Runtime, vmID, want string) {
	t.Helper()
	vm, err := rt.Status(context.Background(), vmID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if vm.State != want {
		t.Fatalf("state = %q, want %q", vm.State, want)
	}
}

func statusOf(t *testing.T, c *Client, base, vmID string) string {
	t.Helper()
	vm, err := c.Status(context.Background(), base, vmID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return vm.State
}
