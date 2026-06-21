//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aarani/craftling-go/internal/runspec"
	"go.uber.org/zap"
)

// initMount is one filesystem the agent brings up at boot. Order in
// bootMounts matters: /dev (devtmpfs) has to land before /dev/pts and
// /dev/shm so we can mkdir those under a writable parent — the rootfs
// itself is read-only.
type initMount struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
}

// bootMounts is the standard kernel filesystem set plus the writable
// scratch tmpfses the workload relies on. Everything writable lives in
// tmpfs (= guest RAM) by design — a RW rootfs would force per-VM copies
// and break the "image digest is the rootfs identity" rule.
//
// The MS_NOSUID/NODEV/NOEXEC flag combinations follow the
// systemd/mkinitcpio defaults for the same mountpoints.
var bootMounts = []initMount{
	{source: "proc", target: "/proc", fstype: "proc",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV},
	{source: "sysfs", target: "/sys", fstype: "sysfs",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV | syscall.MS_RDONLY},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs",
		flags: syscall.MS_NOSUID, data: "mode=0755"},
	{source: "devpts", target: "/dev/pts", fstype: "devpts",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC,
		data:  "newinstance,ptmxmode=0666,mode=0620,gid=5"},
	{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/tmp", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/run", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=0755"},
}

// run is the Linux init entrypoint: mount filesystems, launch the
// workload from the run spec, supervise it, and power the VM off when
// it exits. It never returns under normal operation.
func run(logger *zap.Logger) {
	if err := setupInit(); err != nil {
		logger.Fatal("init: mount", zap.Error(err))
	}

	// Ensure PATH is set on the init process itself so exec.LookPath can
	// resolve a bare command name from the run spec. LookPath reads the
	// CURRENT process's PATH, not the child env we build below.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", defaultPath)
	}

	// Bring the MMDS interface up so the metadata fetch below can reach
	// the link-local address. Best-effort: the kernel may have already
	// configured it, and fetchRunSpec reports the real failure if not.
	if err := setupNetwork(); err != nil {
		logger.Warn("init: network setup", zap.Error(err))
	}

	spec, err := fetchRunSpec(logger)
	if err != nil {
		logger.Fatal("init: fetch run spec from mmds", zap.Error(err))
	}

	// Apply per-VM networking (private address, gateway neighbor, default
	// route) once we have the spec, so the workload can reach the internet
	// through the host's NAT dataplane. Absent on MMDS-only hosts.
	if spec.Net != nil {
		if err := applyNetConfig(spec.Net); err != nil {
			logger.Fatal("init: apply network config", zap.Error(err))
		}
		logger.Info("init: network configured",
			zap.String("address", spec.Net.Address),
			zap.String("gateway", spec.Net.Gateway))
		// Give the workload a resolver: the read-only image ships no usable
		// /etc/resolv.conf, so without this DNS fails and the server can't even
		// download its jar. Best-effort — a workload that needs no DNS still runs.
		if err := writeResolvConf(spec.Net.Nameservers); err != nil {
			logger.Warn("init: write resolv.conf", zap.Error(err))
		} else if len(spec.Net.Nameservers) > 0 {
			logger.Info("init: resolv.conf written", zap.Strings("nameservers", spec.Net.Nameservers))
		}
	}

	// Make WorkingDir durable before launching the workload, so the world it
	// writes lands on the persistent disk instead of tmpfs. Absent on hosts
	// without world persistence (P5).
	if spec.Persist != nil {
		if err := applyPersist(spec.Persist); err != nil {
			logger.Fatal("init: set up world persistence", zap.Error(err))
		}
		logger.Info("init: world persistence enabled",
			zap.String("device", spec.Persist.Device),
			zap.String("mountpoint", spec.Persist.Mountpoint))
	}

	// Start the snapshot control server so the host can take consistent live
	// snapshots. Requires the world disk to be mounted (Persist), since it
	// freezes that filesystem. Best-effort and non-blocking.
	if spec.Quiesce != nil && spec.Persist != nil {
		startSnapshotControl(logger, spec.Quiesce)
	}

	argv := spec.Argv()
	if len(argv) == 0 {
		logger.Fatal("init: run spec has no entrypoint or cmd")
	}

	code := supervise(logger, spec, argv)
	powerOff(logger, code)
}

// mmdsClient is the HTTP client used to reach the link-local MMDS. The
// metadata service is on-link (the kernel configures eth0 from the ip=
// boot arg), so a short per-request timeout is plenty.
var mmdsClient = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		// MMDS is link-local and unproxied; never honour any proxy the
		// image's env might carry, and don't keep idle connections.
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
	},
}

// fetchRunSpec pulls the run spec from MMDS, retrying briefly: the
// kernel brings eth0 up from the ip= boot arg before init runs, but the
// interface can take a moment to settle, so a transient connect failure
// in the first second or two is expected rather than fatal.
func fetchRunSpec(logger *zap.Logger) (*runspec.RunSpec, error) {
	const attempts = 20
	var lastErr error
	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		spec, err := runspec.FetchFromMMDS(ctx, mmdsClient)
		cancel()
		if err == nil {
			return spec, nil
		}
		lastErr = err
		logger.Warn("init: mmds not ready, retrying", zap.Int("attempt", i+1), zap.Error(err))
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("mmds unreachable after %d attempts: %w", attempts, lastErr)
}

// supervise launches the workload as a child in its own process group,
// forwards termination signals to it, reaps any orphans that reparent
// to PID 1, and returns the workload's exit code once it terminates.
func supervise(logger *zap.Logger, spec *runspec.RunSpec, argv []string) int {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		logger.Fatal("init: resolve entrypoint", zap.String("cmd", argv[0]), zap.Error(err))
	}

	cmd := exec.Command(path, argv[1:]...)
	cmd.Args = argv
	cmd.Env = childEnv(spec.Env)
	cmd.Dir = spec.WorkingDir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group so we can signal the whole workload tree at once.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logger.Fatal("init: start workload", zap.String("cmd", path), zap.Error(err))
	}
	pid := cmd.Process.Pid
	pgid := pid
	logger.Info("init: workload started", zap.String("cmd", path), zap.Int("pid", pid))

	// Forward termination signals to the workload's process group.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigs {
			logger.Info("init: forwarding signal", zap.String("signal", sig.String()))
			_ = syscall.Kill(-pgid, sig.(syscall.Signal))
		}
	}()

	// Reap loop. As PID 1, every orphan in the VM reparents to us; we
	// must wait4 them or they linger as zombies. We block in wait4(-1)
	// and keep going until the workload itself is reaped.
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			// ECHILD: nothing left to wait for. The workload is gone and
			// we somehow missed its status — treat as a clean stop.
			logger.Warn("init: wait4 drained", zap.Error(err))
			return 0
		}
		if wpid != pid {
			// An orphan we reaped on PID 1's behalf; keep going.
			continue
		}
		code := exitCode(ws)
		logger.Info("init: workload exited", zap.Int("pid", pid), zap.Int("code", code))
		return code
	}
}

// childEnv builds the workload environment from the run spec, defaulting
// PATH when the image didn't set one so a bare-name entrypoint resolves.
func childEnv(specEnv []string) []string {
	env := append([]string(nil), specEnv...)
	hasPath := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH="+defaultPath)
	}
	return env
}

// exitCode maps a wait status to a conventional process exit code
// (128+signal for signalled deaths, matching shell convention).
func exitCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// powerOff shuts the microVM down. As PID 1, returning or exiting would
// trigger a kernel panic ("Attempted to kill init"); instead we ask the kernel
// to reset, which Firecracker traps (the guest's i8042 reset — the same
// mechanism the host-side SendCtrlAltDel uses) and turns into a clean VMM exit.
//
// We deliberately use RESTART, not POWER_OFF: microVMs expose no ACPI, so
// LINUX_REBOOT_CMD_POWER_OFF finds no power-off handler and the kernel falls
// back to halting the CPU ("reboot: System halted"). That leaves the Firecracker
// process alive forever, so the dead VM keeps holding its host capacity slot and
// the host slowly wedges. A reset exits the VMM, freeing the slot. Firecracker
// does not actually reboot the guest — a guest reset terminates the process.
// The exit code is logged for the host to correlate via the serial console.
func powerOff(logger *zap.Logger, code int) {
	logger.Info("init: powering off", zap.Int("workload_exit_code", code))
	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		// Reboot failed (no CAP_SYS_BOOT, or not really PID 1 — e.g. a
		// manual test run). Fall back to a normal exit.
		logger.Error("init: reset failed", zap.Error(err))
		os.Exit(code)
	}
	// Unreachable once the kernel acts on the reboot syscall.
	select {}
}

// setupInit mounts the kernel filesystems and scratch tmpfses.
func setupInit() error {
	for _, m := range bootMounts {
		// Best-effort mkdir for mountpoints whose parent is already
		// writable (e.g. /dev/pts after /dev is up). On the read-only
		// rootfs the mkdir fails with EROFS; that's expected — the mount
		// target almost always exists already (the converter pre-creates
		// the standard ones).
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			// EBUSY = kernel already mounted this for us. Common for /dev
			// when CONFIG_DEVTMPFS_MOUNT=y. Treat as success.
			if errors.Is(err, syscall.EBUSY) {
				continue
			}
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	return nil
}
