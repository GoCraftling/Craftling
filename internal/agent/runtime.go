// Package agent is the host-side worker that actually runs game-server VMs, plus
// the thin HTTP contract the control plane uses to drive it. The control plane
// must never touch KVM (a core invariant); it calls down to an agent, and the
// agent's Runtime performs the local VM lifecycle. FakeRuntime ships first so the
// whole control-plane↔agent seam can be exercised before a real Firecracker
// driver (P4) exists.
package agent

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/google/uuid"
)

// VM lifecycle states, as observed by the runtime. The values mirror
// provisioner.State so the control plane can map them across the wire 1:1.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateMissing = "missing"
)

// defaultMinecraftPort is the standard Minecraft port and the base of the fake
// runtime's per-VM host-port pool: the first VM on a host binds it and each
// subsequent VM gets the next free port up, so no two share one.
const defaultMinecraftPort = 25565

// ErrVMNotFound means the runtime has no VM with the requested id. Stop and
// Deprovision treat a missing VM as success (idempotent); Start and Status
// surface it so the caller can tell a stopped VM from a vanished one.
var ErrVMNotFound = errors.New("vm not found")

// ErrInsufficientCapacity means the host lacks free cpu/memory to run a VM. A
// host agent is the authority over its own physical resources, so it refuses to
// boot a VM that would overcommit the host even if the control plane asked for
// it — the scheduler's in-memory view can drift (e.g. across a restart), and a
// host must never run more than it has. The control-plane scheduler is the
// primary guard; this is the host's own backstop.
var ErrInsufficientCapacity = errors.New("insufficient host capacity")

// VMSpec is what the control plane asks an agent to run. It is the VM-level view
// of a game server, deliberately decoupled from model.GameServer.
type VMSpec struct {
	ServerID string `json:"server_id"`
	Game     string `json:"game"`
	Version  string `json:"version"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`

	// ImageRef is the exact OCI image the VM must boot, resolved by the control
	// plane from a marketplace template. When set the Firecracker driver converts
	// it instead of its configured FC_IMAGE_REF default; empty falls back to that
	// default (the direct, version-templated path).
	ImageRef string `json:"image_ref,omitempty"`

	// RunSpec is the OCI-derived command/env/workdir the guest init
	// agent should exec, distilled by internal/image at image-pull time.
	// When set, the Firecracker driver publishes it into the VM's MMDS at
	// boot and the in-VM init fetches it from there (see cmd/init). When
	// nil — e.g. the legacy ext4 image path that has its own init — the
	// driver boots the VM with no MMDS and no extra network interface.
	RunSpec *runspec.RunSpec `json:"run_spec,omitempty"`
}

// VM is a runtime instance and its observed state. It is also the JSON the agent
// API returns.
type VM struct {
	ID       string `json:"id"`
	ServerID string `json:"server_id"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	State    string `json:"state"`

	// Health is the workload's deep health (P7) — proof the game process inside a
	// running VM is up and answering — as probed by the agent over the guest's
	// loopback. Nil when the VM isn't running or hasn't been probed yet; the
	// control plane treats a nil/absent value as "unknown, not unhealthy".
	Health *runspec.Health `json:"health,omitempty"`
}

// Runtime performs the local VM lifecycle on a host. Implementations run real
// microVMs (P4); FakeRuntime simulates them. A VM's existence is independent of
// whether it runs: Provision/Deprovision create and destroy it, Start/Stop
// toggle a provisioned VM between running and stopped.
type Runtime interface {
	// Provision creates and boots a VM for the spec, returning it running.
	Provision(ctx context.Context, spec VMSpec) (*VM, error)
	// Start boots an existing stopped VM. Idempotent for an already-running VM;
	// ErrVMNotFound if there is no such VM.
	Start(ctx context.Context, vmID string) (*VM, error)
	// Stop halts a VM without destroying it. Idempotent (missing VM is success).
	Stop(ctx context.Context, vmID string) error
	// Evict destroys a VM and its host-local footprint (working dir, local world
	// disk) while preserving the durable world snapshot, so the server can be
	// rescheduled onto another host and restore its world there. It is the
	// teardown half of releasing a stopped server from its host; Deprovision is
	// the stronger form that also deletes the durable world. Idempotent (missing
	// VM is success).
	Evict(ctx context.Context, vmID string) error
	// Deprovision destroys a VM, including its durable world. Idempotent (missing
	// VM is success).
	Deprovision(ctx context.Context, vmID string) error
	// Status reports a VM's observed state, returning StateMissing for an
	// unknown id rather than an error.
	Status(ctx context.Context, vmID string) (*VM, error)
	// Snapshot takes an application-consistent snapshot of a running VM's
	// world into the durable store (P5c), on demand. ErrVMNotFound for an
	// unknown id; an error if the runtime has no world store configured.
	Snapshot(ctx context.Context, vmID string) error
	// Logs returns the VM's captured console/VMM output, most recent last. When
	// tailLines > 0 only the last that many lines are returned; <= 0 returns all
	// available output. ErrVMNotFound for an unknown id.
	Logs(ctx context.Context, vmID string, tailLines int) ([]byte, error)
}

// FakeRuntime is an in-memory Runtime that simulates VMs. It lets the control
// plane and agent API be exercised end-to-end before a real driver exists.
//
// advertiseHost is the player-facing address VMs report as their connect host
// (a real driver would derive this from networking, P6).
type FakeRuntime struct {
	advertiseHost string

	// cpusTotal/memMBTotal cap how much the host will admit. A zero total means
	// that dimension is unlimited — the default for tests that don't exercise
	// capacity. The real agent passes its configured host capacity.
	cpusTotal  int
	memMBTotal int

	mu        sync.Mutex
	vms       map[string]*fakeVM
	usedPorts map[int]struct{} // host ports currently bound by a live VM
}

// fakeVM is a simulated VM plus the host resources it holds. A running VM
// occupies its cpu/memory; stopping it frees that slot back to the host —
// mirroring a real host that reclaims a halted VM's resources — so only running
// VMs count against capacity.
type fakeVM struct {
	vm    *VM
	cpus  int
	memMB int
}

// FakeOption configures a FakeRuntime.
type FakeOption func(*FakeRuntime)

// WithCapacity caps the host's admittable cpu/memory so the runtime refuses to
// overcommit itself. A zero total leaves that dimension unlimited.
func WithCapacity(cpusTotal, memMBTotal int) FakeOption {
	return func(r *FakeRuntime) {
		r.cpusTotal = cpusTotal
		r.memMBTotal = memMBTotal
	}
}

// NewFakeRuntime constructs a FakeRuntime advertising the given connect host.
func NewFakeRuntime(advertiseHost string, opts ...FakeOption) *FakeRuntime {
	if advertiseHost == "" {
		advertiseHost = "127.0.0.1"
	}
	r := &FakeRuntime{
		advertiseHost: advertiseHost,
		vms:           make(map[string]*fakeVM),
		usedPorts:     make(map[int]struct{}),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Provision mints a new running VM for the spec, refusing to overcommit the
// host's capacity.
func (r *FakeRuntime) Provision(_ context.Context, spec VMSpec) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.fitsLocked(spec.CPUs, spec.MemoryMB); err != nil {
		return nil, err
	}
	vm := &VM{
		ID:       "vm-" + uuid.NewString(),
		ServerID: spec.ServerID,
		Host:     r.advertiseHost,
		Port:     r.allocatePortLocked(),
		State:    StateRunning,
	}
	r.vms[vm.ID] = &fakeVM{vm: vm, cpus: spec.CPUs, memMB: spec.MemoryMB}
	return clone(vm), nil
}

// allocatePortLocked hands out the lowest free host port at or above the
// standard Minecraft port, so every VM on a host binds a distinct public port
// rather than colliding on 25565. Caller holds r.mu.
func (r *FakeRuntime) allocatePortLocked() int {
	for p := defaultMinecraftPort; ; p++ {
		if _, used := r.usedPorts[p]; !used {
			r.usedPorts[p] = struct{}{}
			return p
		}
	}
}

// fitsLocked reports whether a VM needing cpus/memMB fits the host's remaining
// capacity, returning ErrInsufficientCapacity if not. Only running VMs hold a
// slot — a stopped VM frees its cpu/memory so the host can pack other work into
// it, at the cost that restarting it may then fail. A zero total leaves that
// dimension unconstrained. Caller holds r.mu.
func (r *FakeRuntime) fitsLocked(cpus, memMB int) error {
	var usedCPUs, usedMem int
	for _, fv := range r.vms {
		if fv.vm.State != StateRunning {
			continue
		}
		usedCPUs += fv.cpus
		usedMem += fv.memMB
	}
	if r.cpusTotal > 0 && usedCPUs+cpus > r.cpusTotal {
		return ErrInsufficientCapacity
	}
	if r.memMBTotal > 0 && usedMem+memMB > r.memMBTotal {
		return ErrInsufficientCapacity
	}
	return nil
}

// Start boots an existing VM back to running, refusing to overcommit. Because a
// stopped VM gave up its slot, the cpu/memory it needs may have been handed to
// another VM while it was down, so a restart can fail with
// ErrInsufficientCapacity. Restarting an already-running VM is a no-op.
func (r *FakeRuntime) Start(_ context.Context, vmID string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fv, ok := r.vms[vmID]
	if !ok {
		return nil, ErrVMNotFound
	}
	if fv.vm.State != StateRunning {
		if err := r.fitsLocked(fv.cpus, fv.memMB); err != nil {
			return nil, err
		}
		fv.vm.State = StateRunning
	}
	return clone(fv.vm), nil
}

// Stop halts a VM, keeping it. Unknown VM is treated as already gone.
func (r *FakeRuntime) Stop(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if fv, ok := r.vms[vmID]; ok {
		fv.vm.State = StateStopped
	}
	return nil
}

// Evict destroys a VM, freeing its capacity and host port. The fake keeps no
// durable world, so it is indistinguishable from Deprovision here; the
// distinction (preserving the durable snapshot) only matters for the real
// runtime. Unknown VM is a no-op.
func (r *FakeRuntime) Evict(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.removeLocked(vmID)
	return nil
}

// Deprovision destroys a VM, freeing its capacity and host port. Unknown VM is
// a no-op.
func (r *FakeRuntime) Deprovision(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.removeLocked(vmID)
	return nil
}

// removeLocked drops a VM from the inventory and releases its host port. Caller
// holds r.mu; an unknown VM is a no-op.
func (r *FakeRuntime) removeLocked(vmID string) {
	if fv, ok := r.vms[vmID]; ok {
		delete(r.usedPorts, fv.vm.Port)
		delete(r.vms, vmID)
	}
}

// Status reports a VM's state, or a missing VM for an unknown id.
func (r *FakeRuntime) Status(_ context.Context, vmID string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if fv, ok := r.vms[vmID]; ok {
		return clone(fv.vm), nil
	}
	return &VM{ID: vmID, State: StateMissing}, nil
}

// Snapshot is a no-op for the fake runtime — it has no real world disk to
// capture. It reports ErrVMNotFound for an unknown id so callers can still tell
// a known VM from a missing one.
func (r *FakeRuntime) Snapshot(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.vms[vmID]; !ok {
		return ErrVMNotFound
	}
	return nil
}

// Logs returns synthetic console output for a known VM so the logs path can be
// exercised end-to-end before a real driver exists. ErrVMNotFound for an
// unknown id. tailLines is honored against the synthesized lines.
func (r *FakeRuntime) Logs(_ context.Context, vmID string, tailLines int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fv, ok := r.vms[vmID]
	if !ok {
		return nil, ErrVMNotFound
	}
	lines := []string{
		"[fake] vm " + vmID + " booting",
		"[fake] server " + fv.vm.ServerID + " listening on " + fv.vm.Host,
		"[fake] state " + fv.vm.State,
	}
	if tailLines > 0 && tailLines < len(lines) {
		lines = lines[len(lines)-tailLines:]
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func clone(vm *VM) *VM {
	c := *vm
	return &c
}
