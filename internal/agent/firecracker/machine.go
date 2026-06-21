package firecracker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	fcclient "github.com/aarani/craftling-go/internal/firecracker/client/operations"
	fcmodels "github.com/aarani/craftling-go/internal/firecracker/models"
	"github.com/aarani/craftling-go/internal/runspec"
	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
)

// machine is a single Firecracker microVM: the host process plus an API client
// bound to its control socket. A machine outlives a stop (the process is killed
// but its metadata and world disk are kept so Start can re-boot it); it is
// gone only after destroy.
type machine struct {
	id       string
	serverID string
	dir      string // per-VM working directory
	socket   string // API Unix socket path
	rootfs   string // read-only squashfs rootfs image (shared, content-addressed)
	kernel   string
	binary   string // firecracker executable
	bootArgs string
	vcpus    int
	memoryMB int

	// runSpec, when non-nil, is published into this VM's MMDS at boot for
	// the in-VM init agent to fetch. tapName is the host TAP backing the
	// MMDS network interface (only created when runSpec is set).
	runSpec *runspec.RunSpec
	tapName string

	// worldDisk, when non-empty, is the host path of this VM's per-server
	// world disk (P5a). It is attached as a second, non-root virtio-blk
	// device (/dev/vdb) and survives stop/start; the guest overlays it onto
	// the workload's WorkingDir. Empty when world persistence is disabled.
	worldDisk string
	// worldKey is the server-scoped key the world disk and its durable
	// snapshot (P5b) are stored under. Empty when persistence is disabled.
	worldKey string
	// vsockUDS, when non-empty, is the host Unix-socket path Firecracker
	// exposes this VM's vsock on. The host connects to it to reach the guest
	// snapshot control server (P5c). Empty when live snapshots are disabled.
	vsockUDS string

	// dp, net, and servicePort are set only when the NAT dataplane is enabled
	// and this VM has a runSpec. dp is the shared eBPF collection; net is this
	// VM's allocated addressing; servicePort is the in-VM service port the
	// public host port DNATs to.
	dp          *natDataplane
	net         vmNet
	servicePort uint16

	cmd *exec.Cmd
	api fcclient.ClientService

	// healthMu guards health, the most recent deep-health probe of the workload
	// (P7). The health sweep writes it; vmView reads it. Nil until the first
	// successful probe — distinct from a probe that reported the workload
	// unreachable (a non-nil Health with Reachable=false).
	healthMu sync.Mutex
	health   *runspec.Health
}

// setHealth stores the latest deep-health probe result.
func (m *machine) setHealth(h *runspec.Health) {
	m.healthMu.Lock()
	m.health = h
	m.healthMu.Unlock()
}

// getHealth returns the latest deep-health probe result, or nil if none yet.
func (m *machine) getHealth() *runspec.Health {
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	return m.health
}

// socketTimeout bounds how long Provision/Start wait for Firecracker to create
// its API socket after the process launches. shutdownGrace bounds how long a
// graceful (ACPI) shutdown is given before the process is force-killed.
const (
	socketTimeout = 10 * time.Second
	shutdownGrace = 10 * time.Second
)

// logFileName is the per-VM file Firecracker's stdout/stderr is captured into.
// With console=ttyS0 in the boot args this also carries the guest serial
// console — kernel boot messages and the workload's own stdout/stderr — so it
// is the host-side source for a server's logs.
const logFileName = "firecracker.log"

// boot launches the Firecracker process, waits for its API socket, configures
// the machine, and starts the guest. On any failure it tears the process down
// so a half-built VM never lingers.
func (m *machine) boot(ctx context.Context) error {
	// A stale socket from a prior crash would make the dialer connect to nothing.
	_ = os.Remove(m.socket)

	logFile, err := os.Create(filepath.Join(m.dir, logFileName))
	if err != nil {
		return fmt.Errorf("create log: %w", err)
	}

	cmd := exec.Command(m.binary, "--api-sock", m.socket, "--id", m.id) //nolint:gosec // paths are driver-controlled
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start firecracker: %w", err)
	}
	m.cmd = cmd
	m.api = newAPIClient(m.socket)

	if err := m.waitForSocket(ctx); err != nil {
		m.kill()
		return err
	}
	if err := m.configure(ctx); err != nil {
		m.kill()
		return err
	}
	if err := m.action(ctx, fcmodels.InstanceActionInfoActionTypeInstanceStart); err != nil {
		m.kill()
		return fmt.Errorf("instance start: %w", err)
	}
	return nil
}

// configure pushes machine config, boot source, and the root drive onto a
// freshly launched (pre-boot) Firecracker.
func (m *machine) configure(ctx context.Context) error {
	vcpu := int64(m.vcpus)
	mem := int64(m.memoryMB)
	if _, err := m.api.PutMachineConfiguration(fcclient.NewPutMachineConfigurationParamsWithContext(ctx).
		WithBody(&fcmodels.MachineConfiguration{VcpuCount: &vcpu, MemSizeMib: &mem})); err != nil {
		return fmt.Errorf("machine config: %w", err)
	}

	kernel := m.kernel
	if _, err := m.api.PutGuestBootSource(fcclient.NewPutGuestBootSourceParamsWithContext(ctx).
		WithBody(&fcmodels.BootSource{KernelImagePath: &kernel, BootArgs: m.effectiveBootArgs()})); err != nil {
		return fmt.Errorf("boot source: %w", err)
	}

	// The rootfs is the immutable, content-addressed squashfs image, shared
	// across every VM booting the same image — attach it read-only. All
	// writable state rides the world disk (/dev/vdb) and tmpfs.
	driveID := "rootfs"
	isRoot := true
	if _, err := m.api.PutGuestDriveByID(fcclient.NewPutGuestDriveByIDParamsWithContext(ctx).
		WithDriveID(driveID).
		WithBody(&fcmodels.Drive{
			DriveID:      &driveID,
			IsRootDevice: &isRoot,
			IsReadOnly:   true,
			PathOnHost:   m.rootfs,
		})); err != nil {
		return fmt.Errorf("root drive: %w", err)
	}

	// Attach the per-server world disk as a second, non-root drive. It
	// surfaces in the guest as /dev/vdb (the root squashfs is /dev/vda);
	// the in-VM init overlays it onto WorkingDir from the runspec Persist
	// block. No-op when world persistence is disabled.
	if m.worldDisk != "" {
		wid := worldDriveID
		notRoot := false
		if _, err := m.api.PutGuestDriveByID(fcclient.NewPutGuestDriveByIDParamsWithContext(ctx).
			WithDriveID(wid).
			WithBody(&fcmodels.Drive{
				DriveID:      &wid,
				IsRootDevice: &notRoot,
				IsReadOnly:   false,
				PathOnHost:   m.worldDisk,
			})); err != nil {
			return fmt.Errorf("world drive: %w", err)
		}
	}

	// Attach a vsock device so the host can drive the guest snapshot control
	// server (P5c). No-op when live snapshots are disabled for this VM.
	if m.vsockUDS != "" {
		cid := int64(guestCID)
		uds := m.vsockUDS
		if _, err := m.api.PutGuestVsock(fcclient.NewPutGuestVsockParamsWithContext(ctx).
			WithBody(&fcmodels.Vsock{GuestCid: &cid, UdsPath: &uds})); err != nil {
			return fmt.Errorf("vsock: %w", err)
		}
	}

	// Publish the run spec via MMDS for the in-VM init agent. No-op when
	// this VM has no run spec (e.g. the legacy ext4 image path).
	if err := m.configureMMDS(ctx); err != nil {
		return fmt.Errorf("mmds: %w", err)
	}
	return nil
}

// action issues a synchronous instance action (e.g. InstanceStart, SendCtrlAltDel).
func (m *machine) action(ctx context.Context, actionType string) error {
	at := actionType
	_, err := m.api.CreateSyncAction(fcclient.NewCreateSyncActionParamsWithContext(ctx).
		WithInfo(&fcmodels.InstanceActionInfo{ActionType: &at}))
	return err
}

// running reports whether the Firecracker process is still alive.
func (m *machine) running() bool {
	if m.cmd == nil || m.cmd.Process == nil {
		return false
	}
	if m.cmd.ProcessState != nil {
		return false // already reaped
	}
	// Signal 0 probes liveness without affecting the process.
	return m.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// shutdown asks the guest to power off (ACPI), waits briefly for the process to
// exit, then force-kills if it lingers. It keeps the rootfs and metadata.
func (m *machine) shutdown(ctx context.Context) {
	if !m.running() {
		m.kill()
		return
	}
	_ = m.action(ctx, fcmodels.InstanceActionInfoActionTypeSendCtrlAltDel)

	done := make(chan struct{})
	go func() { _, _ = m.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
	m.kill()
}

// kill force-terminates the process and removes the API socket, leaving the
// rootfs intact. Safe to call repeatedly.
func (m *machine) kill() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
		_, _ = m.cmd.Process.Wait()
	}
	_ = os.Remove(m.socket)
}

// waitForSocket polls until the API socket accepts a connection or ctx/timeout
// fires. Firecracker creates the socket a short moment after launch.
func (m *machine) waitForSocket(ctx context.Context) error {
	deadline := time.Now().Add(socketTimeout)
	for {
		if !m.running() {
			return fmt.Errorf("firecracker exited before API socket was ready")
		}
		conn, err := net.Dial("unix", m.socket)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for API socket %s", m.socket)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// newAPIClient builds a Firecracker REST client bound to a Unix socket. The
// generated client speaks HTTP; we dial the socket regardless of the URL host.
func newAPIClient(socket string) fcclient.ClientService {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
	rt := httptransport.NewWithClient("localhost", "/", []string{"http"}, httpClient)
	return fcclient.New(rt, strfmt.Default)
}
