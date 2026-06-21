package provisioner

import (
	"context"
	"errors"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/aarani/craftling-go/internal/runspec"
)

// ErrUnplaced means a server reached the provisioner without a host assignment.
// The scheduler (P2) assigns host_id before the reconciler provisions, so this
// indicates a logic error rather than a transient condition.
var ErrUnplaced = errors.New("server has no host assigned")

// Commander delivers VM lifecycle commands to a host's agent over the agent's
// live control-plane connection, keyed by host id. It replaces the old outbound
// HTTP client: the control plane no longer dials agents, it pushes commands down
// the stream each agent holds open. *agentlink.Hub satisfies it.
type Commander interface {
	Provision(ctx context.Context, hostID string, spec agent.VMSpec) (*agent.VM, error)
	Start(ctx context.Context, hostID, vmID string) (*agent.VM, error)
	Stop(ctx context.Context, hostID, vmID string) error
	Snapshot(ctx context.Context, hostID, vmID string) error
	Evict(ctx context.Context, hostID, vmID string) error
	Deprovision(ctx context.Context, hostID, vmID string) error
	Status(ctx context.Context, hostID, vmID string) (*agent.VM, error)
	Logs(ctx context.Context, hostID, vmID string, tailLines int) ([]byte, error)
}

// RemoteProvisioner implements Provisioner by sending commands to the agent on
// the host the scheduler assigned. The reconciler's calls keep the same shape as
// with Fake — they become a message down the host's open stream rather than an
// in-process call, honoring the invariant that the control plane never touches
// KVM itself.
type RemoteProvisioner struct {
	cmd Commander
}

// NewRemote constructs a RemoteProvisioner over a command channel (the hub).
func NewRemote(cmd Commander) *RemoteProvisioner {
	return &RemoteProvisioner{cmd: cmd}
}

// Provision asks the assigned host's agent to create and boot a VM.
func (p *RemoteProvisioner) Provision(ctx context.Context, s *model.GameServer) (*Instance, error) {
	hostID, err := assignedHost(s)
	if err != nil {
		return nil, err
	}
	spec := agent.VMSpec{
		ServerID: s.ID,
		Game:     s.Game,
		Version:  s.Version,
		CPUs:     s.CPUs,
		MemoryMB: s.MemoryMB,
	}
	// A template-launched server carries an authoritative image and a resolved
	// environment; pass both so the agent boots the chosen image and the guest
	// init merges the env over the image's baked-in defaults. A direct server
	// leaves these empty and the agent uses its configured default image.
	if s.ImageRef != nil {
		spec.ImageRef = *s.ImageRef
	}
	if len(s.Env) > 0 {
		spec.RunSpec = &runspec.RunSpec{Env: registry.SortedEnv(s.Env)}
	}
	vm, err := p.cmd.Provision(ctx, hostID, spec)
	if err != nil {
		return nil, err
	}
	return instanceOf(vm), nil
}

// Start resumes a previously provisioned VM. With no recorded VM it falls back
// to provisioning a fresh one, matching the reconciler's resume semantics.
func (p *RemoteProvisioner) Start(ctx context.Context, s *model.GameServer) (*Instance, error) {
	if s.VMID == nil || *s.VMID == "" {
		return p.Provision(ctx, s)
	}
	hostID, err := assignedHost(s)
	if err != nil {
		return nil, err
	}
	vm, err := p.cmd.Start(ctx, hostID, *s.VMID)
	if err != nil {
		return nil, err
	}
	return instanceOf(vm), nil
}

// Stop halts the VM on its host without destroying it (idempotent).
func (p *RemoteProvisioner) Stop(ctx context.Context, s *model.GameServer) error {
	if s.VMID == nil || *s.VMID == "" {
		return nil
	}
	hostID, err := assignedHost(s)
	if err != nil {
		return err
	}
	return p.cmd.Stop(ctx, hostID, *s.VMID)
}

// Evict releases the VM from its host while preserving the durable world, so the
// server can be rescheduled elsewhere on its next start (idempotent). A server
// with no backing VM has nothing to evict.
func (p *RemoteProvisioner) Evict(ctx context.Context, s *model.GameServer) error {
	if s.HostID == nil || *s.HostID == "" || s.VMID == nil || *s.VMID == "" {
		return nil
	}
	return p.cmd.Evict(ctx, *s.HostID, *s.VMID)
}

// Deprovision tears down the VM on its host (idempotent). A server that was
// never placed or provisioned has nothing to tear down.
func (p *RemoteProvisioner) Deprovision(ctx context.Context, s *model.GameServer) error {
	if s.HostID == nil || *s.HostID == "" || s.VMID == nil || *s.VMID == "" {
		return nil
	}
	return p.cmd.Deprovision(ctx, *s.HostID, *s.VMID)
}

// Status reports the VM's observed state as seen by its host's agent.
func (p *RemoteProvisioner) Status(ctx context.Context, s *model.GameServer) (State, error) {
	if s.VMID == nil || *s.VMID == "" {
		return StateMissing, nil
	}
	hostID, err := assignedHost(s)
	if err != nil {
		return "", err
	}
	vm, err := p.cmd.Status(ctx, hostID, *s.VMID)
	if err != nil {
		return "", err
	}
	return stateOf(vm), nil
}

// Snapshot asks the assigned host's agent to take a live world snapshot. A
// server with no backing VM has nothing running to snapshot, so it is a no-op.
func (p *RemoteProvisioner) Snapshot(ctx context.Context, s *model.GameServer) error {
	if s.VMID == nil || *s.VMID == "" {
		return nil
	}
	hostID, err := assignedHost(s)
	if err != nil {
		return err
	}
	return p.cmd.Snapshot(ctx, hostID, *s.VMID)
}

// Logs asks the assigned host's agent for the server's captured console output.
// A server with no backing VM has nothing to read, so it returns empty output
// rather than erroring.
func (p *RemoteProvisioner) Logs(ctx context.Context, s *model.GameServer, tailLines int) ([]byte, error) {
	if s.VMID == nil || *s.VMID == "" {
		return nil, nil
	}
	hostID, err := assignedHost(s)
	if err != nil {
		return nil, err
	}
	return p.cmd.Logs(ctx, hostID, *s.VMID, tailLines)
}

// assignedHost returns the server's assigned host id, or ErrUnplaced.
func assignedHost(s *model.GameServer) (string, error) {
	if s.HostID == nil || *s.HostID == "" {
		return "", ErrUnplaced
	}
	return *s.HostID, nil
}

// instanceOf maps an agent VM to a provisioner Instance.
func instanceOf(vm *agent.VM) *Instance {
	if vm == nil {
		return &Instance{}
	}
	return &Instance{VMID: vm.ID, Host: vm.Host, Port: vm.Port}
}

// stateOf maps an agent VM's state string onto a provisioner State, treating an
// unknown or absent value as missing.
func stateOf(vm *agent.VM) State {
	if vm == nil {
		return StateMissing
	}
	switch vm.State {
	case agent.StateRunning:
		return StateRunning
	case agent.StateStopped:
		return StateStopped
	default:
		return StateMissing
	}
}
