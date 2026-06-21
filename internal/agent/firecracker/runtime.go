package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/storage"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Runtime is the agent.Runtime backed by real Firecracker microVMs. It owns the
// VMs running on this host: their processes, working directories, and writable
// rootfs images. It is safe for concurrent use by the agent HTTP server.
type Runtime struct {
	cfg Config

	// dp and ipam are non-nil only when the NAT dataplane is enabled
	// (cfg.UplinkDevice set). dp is the shared eBPF collection loaded once;
	// ipam hands out per-VM addresses and host ports.
	dp   *natDataplane
	ipam *ipam

	// store is the durable world store (P5b), non-nil when WorldPersistence is
	// on and a store is configured. Provision restores from it, Stop snapshots
	// into it, Deprovision deletes from it.
	store storage.WorldStore

	// done is closed by Close to stop the periodic snapshot sweep (P5c); sweepWG
	// tracks the sweeper goroutine so Close can wait for it to exit.
	done    chan struct{}
	sweepWG sync.WaitGroup

	// baseCtx is the runtime's lifetime context, cancelled by Close. Long,
	// idempotent work that must outlive the control-plane command that triggered
	// it — notably the shared image build — derives from baseCtx instead of the
	// per-command context, so a dropped/reconnected agent stream doesn't abort a
	// half-finished build. Cancelling baseCtx at shutdown still tears those
	// builds down.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	mu  sync.Mutex
	vms map[string]*machine
}

// compile-time check that the driver satisfies the agent contract.
var _ agent.Runtime = (*Runtime)(nil)

// New constructs a Firecracker Runtime, validating host artifacts and ensuring
// the working directory exists.
func New(cfg Config) (*Runtime, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: work dir: %w", err)
	}

	r := &Runtime{cfg: cfg, vms: make(map[string]*machine), done: make(chan struct{})}
	r.baseCtx, r.baseCancel = context.WithCancel(context.Background())
	if cfg.persistEnabled() {
		r.store = cfg.WorldStore
	}
	if r.store != nil && cfg.SnapshotInterval > 0 {
		r.sweepWG.Add(1)
		go r.snapshotSweep(cfg.SnapshotInterval)
	}

	// Bring up the shared eBPF NAT dataplane once, here, when an uplink is
	// configured. A failure is fatal: a half-networked host would boot VMs that
	// can't reach the internet, which is worse than refusing to start.
	if cfg.natEnabled() {
		dc, err := cfg.dataplaneConfig()
		if err != nil {
			return nil, err
		}
		ip, err := newIPAM(dc.subnet, dc.gatewayIP, dc.gatewayMAC, dc.portMin, dc.portMax)
		if err != nil {
			return nil, err
		}
		dp, err := newDataplane(dc)
		if err != nil {
			return nil, err
		}
		r.dp = dp
		r.ipam = ip
	}
	return r, nil
}

// Close stops the periodic snapshot sweep and tears down the NAT dataplane
// (detaching all eBPF programs). It is safe to call when neither was enabled.
func (r *Runtime) Close() {
	r.baseCancel()
	close(r.done)
	r.sweepWG.Wait()
	if r.dp != nil {
		r.dp.Close()
	}
}

// snapshotSweep periodically takes an application-consistent snapshot of every
// running VM, bounding crash data-loss to one interval. It runs each VM's
// snapshot serially (each freezes that VM's disk only briefly); failures are
// logged, never fatal — a missed interval is recoverable, a crashed sweeper is
// not.
func (r *Runtime) snapshotSweep(interval time.Duration) {
	defer r.sweepWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			for _, m := range r.snapshotCandidates() {
				ctx, cancel := context.WithTimeout(context.Background(), snapshotDeadline)
				if err := r.snapshotRunning(ctx, m); err != nil {
					r.cfg.Logger.Warn("periodic world snapshot failed",
						zap.String("vm", m.id), zap.String("server", m.serverID), zap.Error(err))
				} else {
					r.cfg.Logger.Debug("periodic world snapshot taken",
						zap.String("vm", m.id), zap.String("server", m.serverID))
				}
				cancel()
			}
		}
	}
}

// snapshotCandidates returns a snapshot of the running, live-snapshot-capable
// machines, taken under the lock so the sweep can then snapshot each without
// holding it (a freeze + disk read is slow and must not block the VM API).
func (r *Runtime) snapshotCandidates() []*machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*machine
	for _, m := range r.vms {
		if m.vsockUDS != "" && m.worldDisk != "" && m.running() {
			out = append(out, m)
		}
	}
	return out
}

// Provision converts the spec's image to a read-only squashfs, creates a per-VM
// working dir, and boots a microVM whose PID 1 is the injected init agent. The
// init fetches its RunSpec from MMDS; world state rides a separate writable
// disk, never the rootfs.
func (r *Runtime) Provision(ctx context.Context, spec agent.VMSpec) (*agent.VM, error) {
	if spec.CPUs <= 0 || spec.MemoryMB <= 0 {
		return nil, fmt.Errorf("firecracker: invalid spec: cpus=%d memory_mb=%d", spec.CPUs, spec.MemoryMB)
	}
	// Refuse to overcommit the host before doing any expensive build/boot work.
	// The control-plane scheduler is the primary capacity guard; this is the
	// host's own backstop for when its view drifts (e.g. across a restart).
	if err := r.checkCapacity(spec.CPUs, spec.MemoryMB); err != nil {
		return nil, err
	}

	// Resolve and (on first use) build the content-addressed read-only squashfs
	// rootfs for this server's image, and take the RunSpec the converter
	// distilled from the OCI config. The rootfs is shared across VMs booting the
	// same image — it is attached read-only, so there is no per-VM copy.
	ref, err := r.cfg.provisionRef(spec.ImageRef, spec.Version)
	if err != nil {
		return nil, err
	}
	// Build the rootfs under the runtime's lifetime, not the command ctx: the
	// build is content-addressed, idempotent, and shared across every VM booting
	// this ref, so it must not die because this command's context was cancelled
	// (a stream reconnect, a caller deadline). A cancelled provision then still
	// leaves a populated cache for the next attempt instead of restarting from
	// zero. ImagePullTimeout is the only deadline on a stuck pull.
	imgCtx, cancelImg := context.WithTimeout(r.baseCtx, r.cfg.ImagePullTimeout)
	defer cancelImg()
	rootfs, baseSpec, err := r.cfg.ImageStore.Ensure(imgCtx, ref, hostPlatform())
	if err != nil {
		return nil, fmt.Errorf("firecracker: resolve image %q: %w", ref, err)
	}

	id := "vm-" + uuid.NewString()
	dir := filepath.Join(r.cfg.WorkDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: vm dir: %w", err)
	}

	// Start from the converter's RunSpec and let the caller's spec (when set)
	// override command/env/workdir — that is how a server selects a non-default
	// entrypoint without rebuilding the image. The NAT dataplane and world
	// persistence then layer their guest-applied config on top.
	rs := baseSpec
	if spec.RunSpec != nil {
		applyRunSpecOverride(&rs, spec.RunSpec)
	}
	var vmnet vmNet
	var worldDisk, worldKey string

	if r.dp != nil {
		n, err := r.ipam.allocate()
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		vmnet = n
		rs.Net = &runspec.NetConfig{
			Interface:  runspec.MMDSInterface,
			Address:    vmnet.VMIP.String(),
			PrefixLen:  vmnet.PrefixLen,
			Gateway:    vmnet.GatewayIP.String(),
			GatewayMAC: vmnet.GatewayMAC.String(),
		}
	}

	if r.cfg.persistEnabled() {
		target, ok := persistTarget(rs.WorkingDir)
		if !ok {
			r.releaseNet(vmnet)
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("firecracker: world persistence requires an absolute, non-root WorkingDir, got %q", rs.WorkingDir)
		}
		// Key the disk by server id so it can outlive this VM instance and
		// a host reschedule (the world store is keyed the same way); fall
		// back to the VM id when a spec carries no server id.
		worldKey = spec.ServerID
		if worldKey == "" {
			worldKey = id
		}
		wd := r.cfg.worldDiskPath(worldKey)
		if err := r.prepareWorldDisk(ctx, worldKey, wd); err != nil {
			r.releaseNet(vmnet)
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("firecracker: world disk: %w", err)
		}
		worldDisk = wd
		rs.Persist = &runspec.PersistConfig{Device: worldDevice, Mountpoint: target}

		// When a store is configured, enable live snapshots: the guest
		// gets a Quiesce block (flush + freeze) and we attach a vsock
		// device below so the host can drive it.
		if r.cfg.liveSnapshotEnabled() {
			q := &runspec.QuiesceConfig{}
			if r.cfg.RCONPassword != "" {
				q.RCONAddress = fmt.Sprintf("127.0.0.1:%d", r.cfg.RCONPort)
				q.RCONPassword = r.cfg.RCONPassword
			}
			rs.Quiesce = q
		}
	}

	runSpec := &rs

	// The host-side vsock UDS lives in the per-VM dir; set it when this VM has
	// a Quiesce block so configure() attaches the device.
	var vsockUDS string
	if runSpec != nil && runSpec.Quiesce != nil {
		vsockUDS = filepath.Join(dir, "vsock.sock")
	}

	m := &machine{
		id:          id,
		serverID:    spec.ServerID,
		dir:         dir,
		socket:      filepath.Join(dir, "firecracker.sock"),
		rootfs:      rootfs,
		kernel:      r.cfg.KernelPath,
		binary:      r.cfg.BinaryPath,
		bootArgs:    r.cfg.BootArgs,
		vcpus:       spec.CPUs,
		memoryMB:    spec.MemoryMB,
		runSpec:     runSpec,
		tapName:     tapNameFor(id),
		worldDisk:   worldDisk,
		worldKey:    worldKey,
		vsockUDS:    vsockUDS,
		dp:          r.dp,
		net:         vmnet,
		servicePort: defaultMinecraftPort,
	}
	if err := m.boot(ctx); err != nil {
		r.releaseNet(vmnet)
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("firecracker: boot vm: %w", err)
	}

	r.mu.Lock()
	r.vms[id] = m
	r.mu.Unlock()
	return r.vmView(m), nil
}

// checkCapacity reports whether a VM needing cpus/memMB fits the host's
// remaining capacity, returning agent.ErrInsufficientCapacity if not. Only a
// running VM holds its slot — a stopped VM's process is gone, freeing its
// cpu/memory for other work, at the cost that restarting it may then fail. A
// zero configured total leaves that dimension unconstrained.
func (r *Runtime) checkCapacity(cpus, memMB int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var usedCPUs, usedMem int
	for _, m := range r.vms {
		if !m.running() {
			continue
		}
		usedCPUs += m.vcpus
		usedMem += m.memoryMB
	}
	if r.cfg.CPUsTotal > 0 && usedCPUs+cpus > r.cfg.CPUsTotal {
		return agent.ErrInsufficientCapacity
	}
	if r.cfg.MemoryMBTotal > 0 && usedMem+memMB > r.cfg.MemoryMBTotal {
		return agent.ErrInsufficientCapacity
	}
	return nil
}

// Start re-boots a stopped VM from its existing rootfs. It is a no-op for an
// already-running VM and ErrVMNotFound for an unknown id.
func (r *Runtime) Start(ctx context.Context, vmID string) (*agent.VM, error) {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return nil, agent.ErrVMNotFound
	}
	if m.running() {
		return r.vmView(m), nil
	}
	// A stopped VM gave up its capacity slot, so restarting it must clear the
	// host backstop again — the cpu/memory it needs may have been handed to
	// another VM while it was down.
	if err := r.checkCapacity(m.vcpus, m.memoryMB); err != nil {
		return nil, err
	}
	if err := m.boot(ctx); err != nil {
		return nil, fmt.Errorf("firecracker: restart vm: %w", err)
	}
	return r.vmView(m), nil
}

// Stop halts a VM's process without destroying its rootfs (idempotent). An
// unknown VM is treated as already gone.
func (r *Runtime) Stop(ctx context.Context, vmID string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	m.shutdown(ctx)
	// The guest has powered off (synced) by the time shutdown returns, so the
	// disk image is consistent — snapshot it to the durable store now, so a
	// later delete or reschedule can restore the world. A snapshot failure is
	// returned, not swallowed: the stop succeeded but the world wasn't saved,
	// and the reconciler should retry rather than silently risk data loss.
	if r.store != nil && m.worldDisk != "" {
		if err := snapshotWorldDisk(ctx, r.store, m.worldKey, m.worldDisk); err != nil {
			return fmt.Errorf("firecracker: snapshot world on stop: %w", err)
		}
	}
	return nil
}

// Evict force-stops a VM and removes its host-local footprint — working dir and
// local world disk — but keeps the durable world snapshot, so the server can be
// rescheduled onto another host and restore its world from the store there. It
// backs releasing a stopped server from its host (idempotent).
func (r *Runtime) Evict(_ context.Context, vmID string) error {
	return r.teardown(vmID, false)
}

// Deprovision force-stops a VM and removes it entirely, including its durable
// world snapshot — it is a server delete (idempotent).
func (r *Runtime) Deprovision(_ context.Context, vmID string) error {
	return r.teardown(vmID, true)
}

// teardown force-stops a VM and removes its host-local footprint. When
// deleteWorld is set it also deletes the durable world snapshot (a server
// delete); otherwise the durable copy is preserved so the world can be restored
// after a reschedule. An unknown VM is a no-op.
func (r *Runtime) teardown(vmID string, deleteWorld bool) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	if ok {
		delete(r.vms, vmID)
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	m.kill()
	if m.runSpec != nil {
		// Detach the dataplane and release the VM's address/port before the
		// TAP disappears. Best-effort: the MMDS TAP outlives the Firecracker
		// process, so destroy it here too. A failure only leaks a host device;
		// it must not block teardown of the VM's working directory.
		if r.dp != nil {
			r.dp.withdrawVM(m.tapName, m.net)
			r.releaseNet(m.net)
		}
		_ = deleteTAP(m.tapName)
	}
	// Remove the host-local world disk. Removing the keyed parent dir
	// (DataDir/<key>) takes the disk with it. The durable copy in the store is
	// the system of record across hosts; we delete it only on a true server
	// delete, keeping it for a reschedule.
	if m.worldDisk != "" {
		if err := os.RemoveAll(filepath.Dir(m.worldDisk)); err != nil {
			return fmt.Errorf("firecracker: remove world disk: %w", err)
		}
		if deleteWorld && r.store != nil {
			// Best-effort — an orphaned blob is harmless (a later GC can sweep
			// it) and must not block teardown.
			_ = r.store.Delete(context.Background(), m.worldKey)
		}
	}
	if err := os.RemoveAll(m.dir); err != nil {
		return fmt.Errorf("firecracker: remove vm dir: %w", err)
	}
	return nil
}

// Status reports a VM's observed state: running if its process is alive, stopped
// if it is tracked but not running, missing for an unknown id.
func (r *Runtime) Status(_ context.Context, vmID string) (*agent.VM, error) {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return &agent.VM{ID: vmID, State: agent.StateMissing}, nil
	}
	return r.vmView(m), nil
}

// prepareWorldDisk readies a server's world disk at diskPath before boot. When
// a world store holds a snapshot for this server it restores that (the
// reschedule / re-create path); otherwise it formats a fresh empty disk. A
// restored image already carries an ext4, so no mkfs is needed.
func (r *Runtime) prepareWorldDisk(ctx context.Context, serverID, diskPath string) error {
	if r.store != nil {
		ok, err := r.store.Exists(ctx, serverID)
		if err != nil {
			return fmt.Errorf("check world store: %w", err)
		}
		if ok {
			return restoreWorldDisk(ctx, r.store, serverID, diskPath)
		}
	}
	return ensureWorldDisk(diskPath, r.cfg.WorldDiskMB, r.cfg.MkfsExt4Path)
}

// Snapshot takes an on-demand application-consistent snapshot of a running VM's
// world (P5c). It is the same freeze→store→thaw exchange the periodic sweep
// uses. ErrVMNotFound for an unknown id; an error when live snapshots aren't
// configured for this VM (no store / no vsock).
func (r *Runtime) Snapshot(ctx context.Context, vmID string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return agent.ErrVMNotFound
	}
	if r.store == nil || m.vsockUDS == "" || m.worldDisk == "" {
		return fmt.Errorf("firecracker: live snapshot not available for vm %s", vmID)
	}
	return r.snapshotRunning(ctx, m)
}

// releaseNet returns a VM's address/port to the IPAM pool. No-op when the
// dataplane is disabled or the vmNet is empty.
func (r *Runtime) releaseNet(n vmNet) {
	if r.ipam != nil {
		r.ipam.release(n)
	}
}

// vmView renders a machine as the agent.VM the API returns, deriving state from
// process liveness. With the NAT dataplane the connect endpoint is the host's
// advertise address and the VM's IPAM-allocated public host port; otherwise it
// falls back to the standard in-VM port.
func (r *Runtime) vmView(m *machine) *agent.VM {
	state := agent.StateStopped
	if m.running() {
		state = agent.StateRunning
	}
	port := defaultMinecraftPort
	if r.dp != nil && m.net.HostPort != 0 {
		port = int(m.net.HostPort)
	}
	return &agent.VM{
		ID:       m.id,
		ServerID: m.serverID,
		Host:     r.cfg.AdvertiseHost,
		Port:     port,
		State:    state,
	}
}

// applyRunSpecOverride layers a caller-supplied RunSpec over the one the image
// converter distilled from the OCI config. Only the fields the caller set take
// effect; everything else keeps the image's value. Command precedence follows
// OCI semantics — supplying either Entrypoint or Cmd replaces both, so a caller
// can fully control argv — while Env is merged by key (the override wins) so a
// caller can inject a single variable without dropping the image's environment.
func applyRunSpecOverride(base *runspec.RunSpec, over *runspec.RunSpec) {
	if len(over.Entrypoint) > 0 || len(over.Cmd) > 0 {
		base.Entrypoint = over.Entrypoint
		base.Cmd = over.Cmd
	}
	if len(over.Env) > 0 {
		base.Env = mergeEnv(base.Env, over.Env)
	}
	if over.WorkingDir != "" {
		base.WorkingDir = over.WorkingDir
	}
}

// mergeEnv returns base with over's entries applied: an over entry "K=V"
// replaces base's "K=..." in place (preserving order), and any over key not in
// base is appended. Entries without an '=' are treated as bare keys.
func mergeEnv(base, over []string) []string {
	out := append([]string(nil), base...)
	for _, e := range over {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		replaced := false
		for j, b := range out {
			bk := b
			if i := strings.IndexByte(b, '='); i >= 0 {
				bk = b[:i]
			}
			if bk == key {
				out[j] = e
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, e)
		}
	}
	return out
}
