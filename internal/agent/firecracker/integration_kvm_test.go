//go:build kvm && linux

// This integration test boots a real Firecracker microVM and therefore needs
// /dev/kvm plus host artifacts. It is gated behind the `kvm` build tag and kept
// out of the default CI lane (run on a self-hosted KVM runner — see P10):
//
//	FC_KERNEL=/path/vmlinux [FC_BINARY=/path/firecracker] \
//	  sudo -E go test -tags kvm ./internal/agent/firecracker -run TestKVMLifecycle -v
//
// It converts a tiny busybox image to a squashfs rootfs on the fly (the same
// real rootfs-source path production uses) and boots it with a sleep workload,
// so the VM stays up through the lifecycle assertions.
package firecracker

import (
	"context"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
)

// TestKVMLifecycle drives a real microVM through the full Runtime contract over
// the agent seam: provision (boots), stop (process gone, VM kept), start
// (re-boots from the same rootfs), and deprovision (gone).
func TestKVMLifecycle(t *testing.T) {
	binPath, kernel := requireFirecracker(t)
	store, ref := e2eImage(t)

	rt, err := New(Config{
		BinaryPath:    binPath,
		KernelPath:    kernel,
		ImageStore:    store,
		ImageRef:      ref,
		WorkDir:       t.TempDir(),
		BootArgs:      "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath,
		AdvertiseHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	spec := agent.VMSpec{
		ServerID: "kvm-it",
		Game:     "minecraft",
		CPUs:     1,
		MemoryMB: 256,
		// Keep PID 1 alive so the VM stays running for the state assertions;
		// without this the busybox default would exit and init would power off.
		RunSpec: &runspec.RunSpec{
			Cmd:        []string{"/bin/sh", "-c", "exec sleep 600"},
			WorkingDir: "/",
		},
	}
	vm, err := rt.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Always clean up the VM even if a later assertion fails.
	defer func() { _ = rt.Deprovision(context.Background(), vm.ID) }()

	if vm.ID == "" || vm.State != agent.StateRunning {
		t.Fatalf("provisioned vm = %+v, want running with id", vm)
	}
	assertKVMState(t, rt, vm.ID, agent.StateRunning)

	if err := rt.Stop(ctx, vm.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateStopped)

	if _, err := rt.Start(ctx, vm.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateRunning)

	if err := rt.Deprovision(ctx, vm.ID); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	assertKVMState(t, rt, vm.ID, agent.StateMissing)
}

func assertKVMState(t *testing.T, rt *Runtime, vmID, want string) {
	t.Helper()
	// Boot/shutdown are asynchronous; poll briefly for the expected state.
	deadline := time.Now().Add(15 * time.Second)
	for {
		vm, err := rt.Status(context.Background(), vmID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if vm.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want %q", vm.State, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
