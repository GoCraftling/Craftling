package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/model"
)

// fakeCommander routes commands to an in-process Runtime, ignoring the host id —
// it stands in for the hub so the provisioner can be driven without a real gRPC
// stream. It is the seam the control plane pushes commands through.
type fakeCommander struct{ rt agent.Runtime }

func (c fakeCommander) Provision(ctx context.Context, _ string, spec agent.VMSpec) (*agent.VM, error) {
	return c.rt.Provision(ctx, spec)
}
func (c fakeCommander) Start(ctx context.Context, _, vmID string) (*agent.VM, error) {
	return c.rt.Start(ctx, vmID)
}
func (c fakeCommander) Stop(ctx context.Context, _, vmID string) error {
	return c.rt.Stop(ctx, vmID)
}
func (c fakeCommander) Snapshot(ctx context.Context, _, vmID string) error {
	return c.rt.Snapshot(ctx, vmID)
}
func (c fakeCommander) Evict(ctx context.Context, _, vmID string) error {
	return c.rt.Evict(ctx, vmID)
}
func (c fakeCommander) Deprovision(ctx context.Context, _, vmID string) error {
	return c.rt.Deprovision(ctx, vmID)
}
func (c fakeCommander) Status(ctx context.Context, _, vmID string) (*agent.VM, error) {
	return c.rt.Status(ctx, vmID)
}

// errCommander fails loudly on every call, so a test can assert a code path is a
// no-op that never reaches the command channel.
type errCommander struct{ t *testing.T }

func (c errCommander) Provision(context.Context, string, agent.VMSpec) (*agent.VM, error) {
	c.t.Helper()
	c.t.Fatal("Provision called, want no-op")
	return nil, errors.New("unreachable")
}
func (c errCommander) Start(context.Context, string, string) (*agent.VM, error) {
	c.t.Helper()
	c.t.Fatal("Start called, want no-op")
	return nil, errors.New("unreachable")
}
func (c errCommander) Stop(context.Context, string, string) error {
	c.t.Helper()
	c.t.Fatal("Stop called, want no-op")
	return nil
}
func (c errCommander) Snapshot(context.Context, string, string) error {
	c.t.Helper()
	c.t.Fatal("Snapshot called, want no-op")
	return nil
}
func (c errCommander) Evict(context.Context, string, string) error {
	c.t.Helper()
	c.t.Fatal("Evict called, want no-op")
	return nil
}
func (c errCommander) Deprovision(context.Context, string, string) error {
	c.t.Helper()
	c.t.Fatal("Deprovision called, want no-op")
	return nil
}
func (c errCommander) Status(context.Context, string, string) (*agent.VM, error) {
	c.t.Helper()
	c.t.Fatal("Status called, want no-op")
	return nil, errors.New("unreachable")
}

func ptr(s string) *string { return &s }

// TestRemoteProvisionerLifecycle drives a game server through provision → stop →
// start → deprovision against an in-process runtime behind the command channel,
// asserting the observed state reported back at each step.
func TestRemoteProvisionerLifecycle(t *testing.T) {
	ctx := context.Background()
	p := NewRemote(fakeCommander{rt: agent.NewFakeRuntime("10.0.0.20")})
	s := &model.GameServer{
		ID:       "srv-1",
		HostID:   ptr("host-1"),
		Game:     "minecraft",
		Version:  "1.20.4",
		CPUs:     2,
		MemoryMB: 2048,
	}

	inst, err := p.Provision(ctx, s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if inst.VMID == "" || inst.Host != "10.0.0.20" || inst.Port != 25565 {
		t.Fatalf("instance = %+v, want vmid set and 10.0.0.20:25565", inst)
	}
	s.VMID = &inst.VMID

	assertRemoteState(t, p, s, StateRunning)

	if err := p.Stop(ctx, s); err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertRemoteState(t, p, s, StateStopped)

	if _, err := p.Start(ctx, s); err != nil {
		t.Fatalf("start: %v", err)
	}
	assertRemoteState(t, p, s, StateRunning)

	// Evict releases the VM from its host; the agent reports it gone.
	if err := p.Evict(ctx, s); err != nil {
		t.Fatalf("evict: %v", err)
	}
	assertRemoteState(t, p, s, StateMissing)

	if err := p.Deprovision(ctx, s); err != nil {
		t.Fatalf("deprovision: %v", err)
	}
	assertRemoteState(t, p, s, StateMissing)
}

// TestRemoteProvisionerUnplaced verifies provisioning without a host assignment
// is a logic error, while teardown of an unplaced/unprovisioned server is a
// harmless no-op that never reaches the command channel.
func TestRemoteProvisionerUnplaced(t *testing.T) {
	ctx := context.Background()
	p := NewRemote(errCommander{t: t})

	if _, err := p.Provision(ctx, &model.GameServer{ID: "x"}); !errors.Is(err, ErrUnplaced) {
		t.Errorf("provision unplaced = %v, want ErrUnplaced", err)
	}
	// No host and no VM: nothing to tear down, and we must not send a command.
	if err := p.Deprovision(ctx, &model.GameServer{ID: "x"}); err != nil {
		t.Errorf("deprovision unplaced = %v, want nil", err)
	}
	if err := p.Evict(ctx, &model.GameServer{ID: "x"}); err != nil {
		t.Errorf("evict unplaced = %v, want nil", err)
	}
	if err := p.Stop(ctx, &model.GameServer{ID: "x", HostID: ptr("h")}); err != nil {
		t.Errorf("stop with no vm = %v, want nil", err)
	}
}

// TestRemoteProvisionerStartProvisions verifies Start with no recorded VM falls
// back to provisioning a fresh one.
func TestRemoteProvisionerStartProvisions(t *testing.T) {
	ctx := context.Background()
	p := NewRemote(fakeCommander{rt: agent.NewFakeRuntime("10.0.0.21")})
	s := &model.GameServer{ID: "srv-2", HostID: ptr("host-2"), Version: "1.20.4", CPUs: 1, MemoryMB: 1024}

	inst, err := p.Start(ctx, s)
	if err != nil {
		t.Fatalf("start (no vm): %v", err)
	}
	if inst.VMID == "" {
		t.Error("start without a recorded vm should provision a fresh one")
	}
}

// TestRemoteProvisionerSnapshot verifies a snapshot of a provisioned server is
// forwarded to its host's agent, and that a server with no VM is a no-op.
func TestRemoteProvisionerSnapshot(t *testing.T) {
	ctx := context.Background()
	p := NewRemote(fakeCommander{rt: agent.NewFakeRuntime("10.0.0.22")})
	s := &model.GameServer{ID: "srv-3", HostID: ptr("host-3"), Version: "1.20.4", CPUs: 1, MemoryMB: 1024}

	inst, err := p.Provision(ctx, s)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	s.VMID = &inst.VMID
	if err := p.Snapshot(ctx, s); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// No VM: nothing to snapshot, and we must not send a command.
	dead := NewRemote(errCommander{t: t})
	if err := dead.Snapshot(ctx, &model.GameServer{ID: "x", HostID: ptr("h")}); err != nil {
		t.Errorf("snapshot with no vm = %v, want nil", err)
	}
}

// recordingRuntime captures the last VMSpec it was asked to provision so a test
// can assert what crossed the control-plane → agent seam.
type recordingRuntime struct {
	*agent.FakeRuntime
	last agent.VMSpec
}

func (r *recordingRuntime) Provision(ctx context.Context, spec agent.VMSpec) (*agent.VM, error) {
	r.last = spec
	return r.FakeRuntime.Provision(ctx, spec)
}

// TestRemoteProvisionerDeliversTemplate verifies a template-launched server's
// image ref and resolved env are threaded into the VMSpec, while a direct server
// leaves them empty.
func TestRemoteProvisionerDeliversTemplate(t *testing.T) {
	ctx := context.Background()
	rt := &recordingRuntime{FakeRuntime: agent.NewFakeRuntime("10.0.0.30")}
	p := NewRemote(fakeCommander{rt: rt})

	imageRef := "itzg/minecraft-server:java21"
	tmpl := &model.GameServer{
		ID: "srv-t", HostID: ptr("host-t"), Game: "minecraft", Version: "java21",
		CPUs: 2, MemoryMB: 2048,
		ImageRef: &imageRef,
		Env:      map[string]string{"DIFFICULTY": "hard", "EULA": "TRUE"},
	}
	if _, err := p.Provision(ctx, tmpl); err != nil {
		t.Fatalf("provision template: %v", err)
	}
	if rt.last.ImageRef != imageRef {
		t.Errorf("VMSpec.ImageRef = %q, want %q", rt.last.ImageRef, imageRef)
	}
	if rt.last.RunSpec == nil {
		t.Fatalf("VMSpec.RunSpec is nil, want resolved env")
	}
	// SortedEnv -> deterministic order.
	got := rt.last.RunSpec.Env
	want := []string{"DIFFICULTY=hard", "EULA=TRUE"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("RunSpec.Env = %v, want %v", got, want)
	}

	// A direct server carries no image override and no run spec.
	direct := &model.GameServer{ID: "srv-d", HostID: ptr("host-d"), Version: "1.20.4", CPUs: 1, MemoryMB: 1024}
	if _, err := p.Provision(ctx, direct); err != nil {
		t.Fatalf("provision direct: %v", err)
	}
	if rt.last.ImageRef != "" || rt.last.RunSpec != nil {
		t.Errorf("direct VMSpec carried template data: imageRef=%q runspec=%v", rt.last.ImageRef, rt.last.RunSpec)
	}
}

func assertRemoteState(t *testing.T, p *RemoteProvisioner, s *model.GameServer, want State) {
	t.Helper()
	got, err := p.Status(context.Background(), s)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got != want {
		t.Fatalf("state = %q, want %q", got, want)
	}
}
