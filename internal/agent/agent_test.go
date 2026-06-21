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

// TestFakeRuntimeEvict verifies Evict tears a VM down like Deprovision for the
// fake (which holds no durable world), freeing its capacity and host port.
func TestFakeRuntimeEvict(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.7", WithCapacity(4, 8192))

	vm, err := rt.Provision(ctx, VMSpec{ServerID: "a", CPUs: 4, MemoryMB: 8192})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := rt.Evict(ctx, vm.ID); err != nil {
		t.Fatalf("evict: %v", err)
	}
	assertState(t, rt, vm.ID, StateMissing)

	// Capacity and the host port are freed: a fresh VM fits and reuses the port.
	next, err := rt.Provision(ctx, VMSpec{ServerID: "b", CPUs: 4, MemoryMB: 8192})
	if err != nil {
		t.Fatalf("provision after evict should fit: %v", err)
	}
	if next.Port != vm.Port {
		t.Errorf("port after evict = %d, want freed port %d reused", next.Port, vm.Port)
	}
	// Evicting an unknown VM is a no-op.
	if err := rt.Evict(ctx, "ghost"); err != nil {
		t.Errorf("evict unknown = %v, want nil (idempotent)", err)
	}
}

// TestFakeRuntimeRefusesOvercommit verifies a capacity-bounded host agent will
// not boot a VM that would exceed its total: two 4/8192 servers cannot both run
// on a single 4/8192 host. Stopping the first frees its slot for another, and
// restarting it back over that taken slot is itself refused.
func TestFakeRuntimeRefusesOvercommit(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.7", WithCapacity(4, 8192))

	first, err := rt.Provision(ctx, VMSpec{ServerID: "a", CPUs: 4, MemoryMB: 8192})
	if err != nil {
		t.Fatalf("first 4/8192 should fit a 4/8192 host: %v", err)
	}
	if _, err := rt.Provision(ctx, VMSpec{ServerID: "b", CPUs: 4, MemoryMB: 8192}); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("second provision err = %v, want ErrInsufficientCapacity", err)
	}
	// A stopped VM frees its slot, so a second now fits.
	if err := rt.Stop(ctx, first.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	second, err := rt.Provision(ctx, VMSpec{ServerID: "b", CPUs: 4, MemoryMB: 8192})
	if err != nil {
		t.Fatalf("provision over a stopped VM should fit: %v", err)
	}
	// Restarting the first back over the now-taken slot is refused.
	if _, err := rt.Start(ctx, first.ID); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("restart over a taken slot err = %v, want ErrInsufficientCapacity", err)
	}
	// Freeing the second's slot lets the first restart.
	if err := rt.Deprovision(ctx, second.ID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	if _, err := rt.Start(ctx, first.ID); err != nil {
		t.Fatalf("restart after freeing capacity should fit: %v", err)
	}
}

// TestFakeRuntimeUniquePorts verifies two servers on one host never bind the
// same host port, and that a freed port is reused after deprovision.
func TestFakeRuntimeUniquePorts(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.7")

	a, err := rt.Provision(ctx, VMSpec{ServerID: "a", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("provision a: %v", err)
	}
	b, err := rt.Provision(ctx, VMSpec{ServerID: "b", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("provision b: %v", err)
	}
	if a.Port == b.Port {
		t.Fatalf("two servers on one host share port %d", a.Port)
	}
	if a.Port != defaultMinecraftPort {
		t.Errorf("first port = %d, want %d", a.Port, defaultMinecraftPort)
	}
	// Freeing a's port lets a new VM reuse it.
	if err := rt.Deprovision(ctx, a.ID); err != nil {
		t.Fatalf("deprovision a: %v", err)
	}
	c, err := rt.Provision(ctx, VMSpec{ServerID: "c", CPUs: 1, MemoryMB: 512})
	if err != nil {
		t.Fatalf("provision c: %v", err)
	}
	if c.Port != a.Port {
		t.Errorf("reused port = %d, want freed %d", c.Port, a.Port)
	}
	if c.Port == b.Port {
		t.Fatalf("reuse collided with live port %d", b.Port)
	}
}

// TestFakeRuntimeUnlimitedByDefault verifies that without a configured capacity
// the runtime imposes no limit (preserving its plain stand-in behavior).
func TestFakeRuntimeUnlimitedByDefault(t *testing.T) {
	ctx := context.Background()
	rt := NewFakeRuntime("10.0.0.7")
	for i := 0; i < 5; i++ {
		if _, err := rt.Provision(ctx, VMSpec{CPUs: 8, MemoryMB: 16384}); err != nil {
			t.Fatalf("provision %d under unlimited capacity: %v", i, err)
		}
	}
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

	// Logs returns the VM's console output as the raw result payload (not a VM).
	logsReq, _ := json.Marshal(LogsRequest{VMID: vm.ID})
	logsPayload, errStr := execOp(ctx, rt, &pb.Command{Op: OpLogs, Payload: logsReq})
	if errStr != "" {
		t.Fatalf("logs op error = %q, want none", errStr)
	}
	if len(logsPayload) == 0 {
		t.Error("logs op returned empty payload, want console output")
	}

	// Starting a VM the runtime does not know surfaces an error string.
	ghost, _ := json.Marshal(VMRef{VMID: "vm-ghost"})
	if _, errStr := execOp(ctx, rt, &pb.Command{Op: OpStart, Payload: ghost}); errStr == "" {
		t.Error("start unknown vm: expected error string, got none")
	}

	// Logs for a VM the runtime does not know surfaces an error string.
	ghostLogs, _ := json.Marshal(LogsRequest{VMID: "vm-ghost"})
	if _, errStr := execOp(ctx, rt, &pb.Command{Op: OpLogs, Payload: ghostLogs}); errStr == "" {
		t.Error("logs unknown vm: expected error string, got none")
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
