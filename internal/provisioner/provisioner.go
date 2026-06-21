// Package provisioner abstracts the backend that actually runs game servers.
// The Fake implementation simulates provisioning; a real microVM backend
// (e.g. Firecracker / Cloud Hypervisor) implements the same interface.
package provisioner

import (
	"context"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/google/uuid"
)

// Instance describes a provisioned server's runtime details.
type Instance struct {
	VMID string
	Host string
	Port int
}

// StatusReport is what Status observes about a backing VM: its lifecycle State
// (always present) plus, for a running VM, the workload's deep Health (P7) when
// the agent has a probe for it. Health is nil when the VM isn't running or the
// agent hasn't probed it yet — the caller treats nil as "unknown", not
// "unhealthy".
type StatusReport struct {
	State  State
	Health *runspec.Health
}

// State is the observed lifecycle state of a backing instance, as reported by
// Status. It distinguishes a stopped-but-present VM from one that is gone.
type State string

const (
	// StateRunning means the backing VM exists and is running.
	StateRunning State = "running"
	// StateStopped means the backing VM exists but is halted (not destroyed).
	StateStopped State = "stopped"
	// StateMissing means there is no backing VM.
	StateMissing State = "missing"
)

// Provisioner manages the compute backing a game server. A server's VM has a
// lifecycle independent of its existence: Provision/Deprovision create and
// destroy it, while Start/Stop toggle a provisioned VM between running and
// stopped — so a *stopped* server keeps its VM (and, later, its disk) rather
// than being torn down.
type Provisioner interface {
	// Provision creates the backing VM and boots it, returning runtime details.
	Provision(ctx context.Context, s *model.GameServer) (*Instance, error)
	// Start boots an already-provisioned but stopped VM, returning its runtime
	// details. It must be idempotent for an already-running VM.
	Start(ctx context.Context, s *model.GameServer) (*Instance, error)
	// Stop halts the backing VM without destroying it. It must be idempotent.
	Stop(ctx context.Context, s *model.GameServer) error
	// Evict releases the backing VM from its host while preserving the durable
	// world, so the server can be rescheduled onto another host on its next
	// start. It must be idempotent.
	Evict(ctx context.Context, s *model.GameServer) error
	// Deprovision tears down the backing VM, including its durable world. It must
	// be idempotent.
	Deprovision(ctx context.Context, s *model.GameServer) error
	// Status reports the observed state of the backing VM, and — for a running
	// VM — the workload's deep health when available (P7).
	Status(ctx context.Context, s *model.GameServer) (StatusReport, error)
	// Snapshot takes an on-demand, application-consistent world snapshot of a
	// running server into the durable store (P5). A no-op when the server has
	// no backing VM (nothing live to capture).
	Snapshot(ctx context.Context, s *model.GameServer) error
	// Logs returns the server's captured console output from its backing VM (the
	// last tailLines lines when tailLines > 0, all of it otherwise). A server
	// with no backing VM has no logs, so it returns empty output and no error.
	Logs(ctx context.Context, s *model.GameServer, tailLines int) ([]byte, error)
}

// defaultMinecraftPort is the standard Minecraft server port.
const defaultMinecraftPort = 25565

// Fake is an in-memory Provisioner that pretends to manage VMs. It lets the
// reconciler and API be exercised end-to-end before a real backend exists.
type Fake struct{}

// NewFake returns a Fake provisioner.
func NewFake() *Fake { return &Fake{} }

// Provision returns synthetic runtime details for the server.
func (Fake) Provision(_ context.Context, _ *model.GameServer) (*Instance, error) {
	return &Instance{
		VMID: "vm-" + uuid.NewString(),
		Host: "127.0.0.1",
		Port: defaultMinecraftPort,
	}, nil
}

// Start resumes a previously provisioned server, reusing its recorded runtime
// details. If the server was never provisioned it synthesizes new ones.
func (f Fake) Start(ctx context.Context, s *model.GameServer) (*Instance, error) {
	if s.VMID == nil || *s.VMID == "" {
		return f.Provision(ctx, s)
	}
	inst := &Instance{VMID: *s.VMID, Host: "127.0.0.1", Port: defaultMinecraftPort}
	if s.Host != nil {
		inst.Host = *s.Host
	}
	if s.Port != nil {
		inst.Port = *s.Port
	}
	return inst, nil
}

// Stop is a no-op for the fake backend; the VM is considered halted but kept.
func (Fake) Stop(_ context.Context, _ *model.GameServer) error { return nil }

// Evict is a no-op for the fake backend; there is no host-local footprint to
// release.
func (Fake) Evict(_ context.Context, _ *model.GameServer) error { return nil }

// Deprovision is a no-op for the fake backend.
func (Fake) Deprovision(_ context.Context, _ *model.GameServer) error { return nil }

// Snapshot is a no-op for the fake backend; there is no real world to capture.
func (Fake) Snapshot(_ context.Context, _ *model.GameServer) error { return nil }

// Logs returns synthetic console output for a server with a backing VM, or
// empty output for one that was never provisioned, so the logs path can be
// exercised against the fake backend.
func (Fake) Logs(_ context.Context, s *model.GameServer, _ int) ([]byte, error) {
	if s.VMID == nil || *s.VMID == "" {
		return nil, nil
	}
	return []byte("[fake] logs for server " + s.ID + " (vm " + *s.VMID + ")\n"), nil
}

// Status infers state from the server's recorded VM id, since the fake holds no
// real backend state: a server with a VM is running, otherwise it is missing.
// The fake reports no deep health (it runs no real workload to probe).
func (Fake) Status(_ context.Context, s *model.GameServer) (StatusReport, error) {
	if s.VMID != nil && *s.VMID != "" {
		return StatusReport{State: StateRunning}, nil
	}
	return StatusReport{State: StateMissing}, nil
}
