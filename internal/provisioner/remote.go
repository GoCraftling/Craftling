package provisioner

import (
	"context"
	"errors"
	"fmt"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/registry"
	"github.com/aarani/craftling-go/internal/runspec"
)

// ErrUnplaced means a server reached the provisioner without a host assignment.
// The scheduler (P2) assigns host_id before the reconciler provisions, so this
// indicates a logic error rather than a transient condition.
var ErrUnplaced = errors.New("server has no host assigned")

// HostResolver looks up a host by id to find the agent's address. The in-memory
// repository.HostRepository satisfies it.
type HostResolver interface {
	GetByID(ctx context.Context, id string) (*model.Host, error)
}

// RemoteProvisioner implements Provisioner by calling the agent on the host the
// scheduler assigned. The reconciler's calls keep the same shape as with Fake —
// they just become a network hop to the host that actually runs the VM, honoring
// the invariant that the control plane never touches KVM itself.
type RemoteProvisioner struct {
	hosts  HostResolver
	client *agent.Client
}

// NewRemote constructs a RemoteProvisioner over a host resolver and agent client.
func NewRemote(hosts HostResolver, client *agent.Client) *RemoteProvisioner {
	return &RemoteProvisioner{hosts: hosts, client: client}
}

// Provision asks the assigned host's agent to create and boot a VM.
func (p *RemoteProvisioner) Provision(ctx context.Context, s *model.GameServer) (*Instance, error) {
	base, err := p.baseURL(ctx, s)
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
	vm, err := p.client.Provision(ctx, base, spec)
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
	base, err := p.baseURL(ctx, s)
	if err != nil {
		return nil, err
	}
	vm, err := p.client.Start(ctx, base, *s.VMID)
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
	base, err := p.baseURL(ctx, s)
	if err != nil {
		return err
	}
	return p.client.Stop(ctx, base, *s.VMID)
}

// Deprovision tears down the VM on its host (idempotent). A server that was
// never placed or provisioned has nothing to tear down.
func (p *RemoteProvisioner) Deprovision(ctx context.Context, s *model.GameServer) error {
	if s.HostID == nil || *s.HostID == "" || s.VMID == nil || *s.VMID == "" {
		return nil
	}
	base, err := p.baseURL(ctx, s)
	if err != nil {
		return err
	}
	return p.client.Deprovision(ctx, base, *s.VMID)
}

// Status reports the VM's observed state as seen by its host's agent.
func (p *RemoteProvisioner) Status(ctx context.Context, s *model.GameServer) (State, error) {
	if s.VMID == nil || *s.VMID == "" {
		return StateMissing, nil
	}
	base, err := p.baseURL(ctx, s)
	if err != nil {
		return "", err
	}
	vm, err := p.client.Status(ctx, base, *s.VMID)
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
	base, err := p.baseURL(ctx, s)
	if err != nil {
		return err
	}
	return p.client.Snapshot(ctx, base, *s.VMID)
}

// baseURL resolves the agent base URL for the server's assigned host.
func (p *RemoteProvisioner) baseURL(ctx context.Context, s *model.GameServer) (string, error) {
	if s.HostID == nil || *s.HostID == "" {
		return "", ErrUnplaced
	}
	h, err := p.hosts.GetByID(ctx, *s.HostID)
	if err != nil {
		return "", fmt.Errorf("resolve host %s: %w", *s.HostID, err)
	}
	return agent.BaseURL(h.Address), nil
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
