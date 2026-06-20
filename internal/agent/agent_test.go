package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
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

// TestExecOpDispatch verifies the link's command dispatch: each op reaches the
// runtime, results are JSON-encoded the way the hub decodes them, and an unknown
// op surfaces an error rather than panicking. This is the agent half of the
// control-plane → agent command contract.
func TestExecOpDispatch(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.9")

	// Provision returns a running VM payload, no error.
	specJSON, _ := json.Marshal(VMSpec{ServerID: "s2", Version: "1.20.4", CPUs: 1, MemoryMB: 1024})
	payload, errStr := execOp(ctx, rt, &pb.Command{Op: OpProvision, Payload: specJSON})
	if errStr != "" {
		t.Fatalf("provision op error = %q, want none", errStr)
	}
	var vm VM
	if err := json.Unmarshal(payload, &vm); err != nil {
		t.Fatalf("decode provision result: %v", err)
	}
	if vm.ID == "" || vm.State != StateRunning {
		t.Fatalf("provisioned vm = %+v, want running with id", vm)
	}

	ref, _ := json.Marshal(VMRef{VMID: vm.ID})

	// Stop then Status reflects the stopped state across the seam.
	if _, errStr := execOp(ctx, rt, &pb.Command{Op: OpStop, Payload: ref}); errStr != "" {
		t.Fatalf("stop op error = %q, want none", errStr)
	}
	statusPayload, errStr := execOp(ctx, rt, &pb.Command{Op: OpStatus, Payload: ref})
	if errStr != "" {
		t.Fatalf("status op error = %q, want none", errStr)
	}
	var stopped VM
	_ = json.Unmarshal(statusPayload, &stopped)
	if stopped.State != StateStopped {
		t.Errorf("status after stop = %q, want stopped", stopped.State)
	}

	// Starting a VM the runtime does not know surfaces an error string.
	ghost, _ := json.Marshal(VMRef{VMID: "vm-ghost"})
	if _, errStr := execOp(ctx, rt, &pb.Command{Op: OpStart, Payload: ghost}); errStr == "" {
		t.Error("start unknown vm: expected error string, got none")
	}

	// An unrecognized op is reported, not fatal.
	if _, errStr := execOp(ctx, rt, &pb.Command{Op: "bogus"}); errStr == "" {
		t.Error("unknown op: expected error string, got none")
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
