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

// defaultMinecraftPort is the in-VM Minecraft server port. Per-server host port
// allocation arrives in P6; until then every VM uses the standard port.
const defaultMinecraftPort = 25565

// ErrVMNotFound means the runtime has no VM with the requested id. Stop and
// Deprovision treat a missing VM as success (idempotent); Start and Status
// surface it so the caller can tell a stopped VM from a vanished one.
var ErrVMNotFound = errors.New("vm not found")

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
	// Deprovision destroys a VM. Idempotent (missing VM is success).
	Deprovision(ctx context.Context, vmID string) error
	// Status reports a VM's observed state, returning StateMissing for an
	// unknown id rather than an error.
	Status(ctx context.Context, vmID string) (*VM, error)
	// Snapshot takes an application-consistent snapshot of a running VM's
	// world into the durable store (P5c), on demand. ErrVMNotFound for an
	// unknown id; an error if the runtime has no world store configured.
	Snapshot(ctx context.Context, vmID string) error
}

// FakeRuntime is an in-memory Runtime that simulates VMs. It lets the control
// plane and agent API be exercised end-to-end before a real driver exists.
//
// advertiseHost is the player-facing address VMs report as their connect host
// (a real driver would derive this from networking, P6).
type FakeRuntime struct {
	advertiseHost string

	mu  sync.Mutex
	vms map[string]*VM
}

// NewFakeRuntime constructs a FakeRuntime advertising the given connect host.
func NewFakeRuntime(advertiseHost string) *FakeRuntime {
	if advertiseHost == "" {
		advertiseHost = "127.0.0.1"
	}
	return &FakeRuntime{advertiseHost: advertiseHost, vms: make(map[string]*VM)}
}

// Provision mints a new running VM for the spec.
func (r *FakeRuntime) Provision(_ context.Context, spec VMSpec) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	vm := &VM{
		ID:       "vm-" + uuid.NewString(),
		ServerID: spec.ServerID,
		Host:     r.advertiseHost,
		Port:     defaultMinecraftPort,
		State:    StateRunning,
	}
	r.vms[vm.ID] = vm
	return clone(vm), nil
}

// Start boots an existing VM back to running.
func (r *FakeRuntime) Start(_ context.Context, vmID string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	vm, ok := r.vms[vmID]
	if !ok {
		return nil, ErrVMNotFound
	}
	vm.State = StateRunning
	return clone(vm), nil
}

// Stop halts a VM, keeping it. Unknown VM is treated as already gone.
func (r *FakeRuntime) Stop(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if vm, ok := r.vms[vmID]; ok {
		vm.State = StateStopped
	}
	return nil
}

// Deprovision destroys a VM. Unknown VM is a no-op.
func (r *FakeRuntime) Deprovision(_ context.Context, vmID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.vms, vmID)
	return nil
}

// Status reports a VM's state, or a missing VM for an unknown id.
func (r *FakeRuntime) Status(_ context.Context, vmID string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if vm, ok := r.vms[vmID]; ok {
		return clone(vm), nil
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

func clone(vm *VM) *VM {
	c := *vm
	return &c
}
