//go:build kvm && linux

// This end-to-end test proves world persistence (P5a): a write the workload
// makes under WorkingDir survives a Stop/Start because it lands on the per-server
// world disk (overlaid onto WorkingDir by the in-VM init) rather than tmpfs.
//
//	Provision  → boot 1: WorkingDir is empty, so the workload writes a
//	             per-run token to a file under it and prints FIRSTBOOT
//	Stop       → power the VM down (the token is already flushed to /dev/vdb)
//	Start      → boot 2: the file is present (it came off the world disk),
//	             so the workload prints PERSIST_OK:<token>
//
// Seeing PERSIST_OK:<token> on the post-restart console can only happen if the
// write outlived the VM's RAM — i.e. it was captured on the world disk. Same
// prerequisites as the MMDS e2e (root, /dev/kvm, FC_KERNEL, firecracker) plus
// mkfs.ext4 on the host and a kernel with CONFIG_OVERLAY_FS + CONFIG_EXT4_FS:
//
//	FC_KERNEL=/path/vmlinux [FC_BINARY=/path/firecracker] \
//	  sudo -E go test -tags kvm -run TestKVMWorldPersistence -v \
//	  ./internal/agent/firecracker
package firecracker

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/storage"
)

func TestKVMWorldPersistence(t *testing.T) {
	binPath, kernel := requireFirecracker(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; world persistence needs e2fsprogs")
	}

	store, ref := e2eImage(t)

	rt, err := New(Config{
		BinaryPath:       binPath,
		KernelPath:       kernel,
		ImageStore:       store,
		ImageRef:         ref,
		WorkDir:          t.TempDir(),
		BootArgs:         "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath,
		AdvertiseHost:    "127.0.0.1",
		WorldPersistence: true,
		WorldDiskMB:      128, // small but real; mkfs is fast at this size
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token := randomToken(t)
	// /root exists in the busybox image and is non-root, so it is a valid
	// overlay mountpoint. The command is identical across boots: it writes the
	// token only if the file is absent, so boot 1 seeds it and boot 2 reads it
	// back off the world disk.
	const file = "/root/persist-token"
	spec := &runspec.RunSpec{
		Cmd: []string{"/bin/sh", "-c", fmt.Sprintf(
			`if [ -f %[1]s ]; then echo PERSIST_OK:$(cat %[1]s); else echo PERSIST_FIRSTBOOT; printf %%s %[2]s > %[1]s; sync; fi; exec sleep 600`,
			file, token)},
		WorkingDir: "/root",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	vm, err := rt.Provision(ctx, agent.VMSpec{
		ServerID: "e2e-persist",
		Game:     "test",
		CPUs:     1,
		MemoryMB: 256,
		RunSpec:  spec,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer func() { _ = rt.Deprovision(context.Background(), vm.ID) }()
	if vm.State != agent.StateRunning {
		t.Fatalf("provisioned vm = %+v, want running", vm)
	}

	consoleLog := findConsoleLog(t, rt.cfg.WorkDir)
	if !waitForConsole(t, consoleLog, "PERSIST_FIRSTBOOT", 90*time.Second) {
		t.Fatalf("first boot did not reach FIRSTBOOT\n--- console ---\n%s", tailFile(consoleLog, 4096))
	}

	// Power down and back up. boot() re-creates (truncates) the console log, so
	// after Start the file holds only boot 2's output.
	if err := rt.Stop(ctx, vm.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := rt.Start(ctx, vm.ID); err != nil {
		t.Fatalf("start: %v", err)
	}

	want := "PERSIST_OK:" + token
	if !waitForConsole(t, consoleLog, want, 90*time.Second) {
		t.Fatalf("world did not survive stop/start: did not observe %q after restart\n--- console ---\n%s",
			want, tailFile(consoleLog, 4096))
	}
	t.Logf("world survived stop/start: observed %s", want)
}

// TestKVMWorldStoreReschedule proves the cross-host story (P5b): a world saved to
// a shared WorldStore on Stop is restored when the same server is provisioned by
// a *different* runtime (a stand-in for another host). Two runtimes share one
// DirStore but have separate work/data dirs, so the only path the world can take
// from the first to the second is through the store.
func TestKVMWorldStoreReschedule(t *testing.T) {
	binPath, kernel := requireFirecracker(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; world persistence needs e2fsprogs")
	}

	imgStore, ref := e2eImage(t)

	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	newRT := func() *Runtime {
		rt, err := New(Config{
			BinaryPath:       binPath,
			KernelPath:       kernel,
			ImageStore:       imgStore,
			ImageRef:         ref,
			WorkDir:          t.TempDir(), // distinct per runtime → no shared local disk
			BootArgs:         "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath,
			AdvertiseHost:    "127.0.0.1",
			WorldPersistence: true,
			WorldDiskMB:      128,
			WorldStore:       store,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return rt
	}

	const serverID = "e2e-resched"
	const file = "/root/persist-token"
	token := randomToken(t)
	spec := &runspec.RunSpec{
		Cmd: []string{"/bin/sh", "-c", fmt.Sprintf(
			`if [ -f %[1]s ]; then echo PERSIST_OK:$(cat %[1]s); else echo PERSIST_FIRSTBOOT; printf %%s %[2]s > %[1]s; sync; fi; exec sleep 600`,
			file, token)},
		WorkingDir: "/root",
	}
	vmSpec := agent.VMSpec{ServerID: serverID, Game: "test", CPUs: 1, MemoryMB: 256, RunSpec: spec}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Host A: provision, let the workload seed the world, then Stop — which
	// snapshots the disk into the shared store.
	hostA := newRT()
	vmA, err := hostA.Provision(ctx, vmSpec)
	if err != nil {
		t.Fatalf("host A provision: %v", err)
	}
	defer func() { _ = hostA.Deprovision(context.Background(), vmA.ID) }()
	if !waitForConsole(t, findConsoleLog(t, hostA.cfg.WorkDir), "PERSIST_FIRSTBOOT", 90*time.Second) {
		t.Fatalf("host A first boot did not reach FIRSTBOOT")
	}
	if err := hostA.Stop(ctx, vmA.ID); err != nil {
		t.Fatalf("host A stop (snapshots world): %v", err)
	}
	if ok, err := store.Exists(ctx, serverID); err != nil || !ok {
		t.Fatalf("world not in store after stop: ok=%v err=%v", ok, err)
	}

	// Host B: a fresh runtime with its own dirs. Provisioning the same server
	// must restore the world from the store and surface the token.
	hostB := newRT()
	vmB, err := hostB.Provision(ctx, vmSpec)
	if err != nil {
		t.Fatalf("host B provision: %v", err)
	}
	defer func() { _ = hostB.Deprovision(context.Background(), vmB.ID) }()

	want := "PERSIST_OK:" + token
	if !waitForConsole(t, findConsoleLog(t, hostB.cfg.WorkDir), want, 120*time.Second) {
		t.Fatalf("world did not move across hosts via the store: did not observe %q on host B\n--- console ---\n%s",
			want, tailFile(findConsoleLog(t, hostB.cfg.WorkDir), 4096))
	}
	t.Logf("world rescheduled across runtimes via the store: observed %s", want)
}

// TestKVMLiveSnapshot proves the P5c loop: snapshot a *running* server via the
// vsock control channel (flush is freeze-only here — busybox has no RCON, but
// the workload syncs before idling), then restore that live snapshot on a
// second runtime and boot it, confirming the token written on host A's running
// VM is present on host B. No Stop happens on host A.
func TestKVMLiveSnapshot(t *testing.T) {
	binPath, kernel := requireFirecracker(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; world persistence needs e2fsprogs")
	}

	imgStore, ref := e2eImage(t)
	store, err := storage.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	newRT := func() *Runtime {
		rt, err := New(Config{
			BinaryPath:       binPath,
			KernelPath:       kernel,
			ImageStore:       imgStore,
			ImageRef:         ref,
			WorkDir:          t.TempDir(),
			BootArgs:         "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath,
			AdvertiseHost:    "127.0.0.1",
			WorldPersistence: true,
			WorldDiskMB:      128,
			WorldStore:       store, // enables live snapshots (vsock + Quiesce)
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return rt
	}

	const serverID = "e2e-live"
	const file = "/root/persist-token"
	token := randomToken(t)
	spec := &runspec.RunSpec{
		Cmd: []string{"/bin/sh", "-c", fmt.Sprintf(
			`if [ -f %[1]s ]; then echo PERSIST_OK:$(cat %[1]s); else echo PERSIST_FIRSTBOOT; printf %%s %[2]s > %[1]s; sync; fi; exec sleep 600`,
			file, token)},
		WorkingDir: "/root",
	}
	vmSpec := agent.VMSpec{ServerID: serverID, Game: "test", CPUs: 1, MemoryMB: 256, RunSpec: spec}

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	hostA := newRT()
	vmA, err := hostA.Provision(ctx, vmSpec)
	if err != nil {
		t.Fatalf("host A provision: %v", err)
	}
	defer func() { _ = hostA.Deprovision(context.Background(), vmA.ID) }()
	if !waitForConsole(t, findConsoleLog(t, hostA.cfg.WorkDir), "PERSIST_FIRSTBOOT", 90*time.Second) {
		t.Fatalf("host A first boot did not reach FIRSTBOOT")
	}

	// Live snapshot the still-running VM (vsock PREPARE → freeze → store → RESUME).
	hostA.mu.Lock()
	m := hostA.vms[vmA.ID]
	hostA.mu.Unlock()
	if m == nil {
		t.Fatal("host A machine not found")
	}
	if err := hostA.snapshotRunning(ctx, m); err != nil {
		t.Fatalf("live snapshot: %v", err)
	}
	if ok, err := store.Exists(ctx, serverID); err != nil || !ok {
		t.Fatalf("live snapshot not in store: ok=%v err=%v", ok, err)
	}

	// Restore the live snapshot on a second runtime and confirm the token.
	hostB := newRT()
	vmB, err := hostB.Provision(ctx, vmSpec)
	if err != nil {
		t.Fatalf("host B provision: %v", err)
	}
	defer func() { _ = hostB.Deprovision(context.Background(), vmB.ID) }()

	want := "PERSIST_OK:" + token
	if !waitForConsole(t, findConsoleLog(t, hostB.cfg.WorkDir), want, 120*time.Second) {
		t.Fatalf("live snapshot did not capture the running world: did not observe %q on host B\n--- console ---\n%s",
			want, tailFile(findConsoleLog(t, hostB.cfg.WorkDir), 4096))
	}
	t.Logf("live snapshot of a running server restored on another runtime: observed %s", want)
}
