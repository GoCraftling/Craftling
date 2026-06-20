package agentlink

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
	"github.com/aarani/craftling-go/internal/model"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// fakeInventory records the inventory transitions the hub drives so a test can
// assert that a stream's lifecycle (register -> heartbeat -> disconnect) is
// reflected in the fleet.
type fakeInventory struct {
	registered chan string // host id, on RegisterReserved
	heartbeat  chan string // host id, on Heartbeat
	down       chan string // host id, on MarkDown
}

func newFakeInventory() *fakeInventory {
	return &fakeInventory{
		registered: make(chan string, 1),
		heartbeat:  make(chan string, 1),
		down:       make(chan string, 1),
	}
}

func (f *fakeInventory) RegisterReserved(_ context.Context, h *model.Host, _, _ int) (*model.Host, error) {
	f.registered <- h.ID
	return &model.Host{ID: h.ID, Hostname: h.Hostname, Status: model.HostReady}, nil
}
func (f *fakeInventory) Heartbeat(_ context.Context, id string) error {
	select {
	case f.heartbeat <- id:
	default:
	}
	return nil
}
func (f *fakeInventory) MarkDown(_ context.Context, id string) error {
	f.down <- id
	return nil
}

// zeroCapacity is a CapacityReconstructor that reports no committed capacity.
type zeroCapacity struct{}

func (zeroCapacity) UsedCapacity(context.Context, string) (int, int, error) { return 0, 0, nil }

// TestHubRoundTrip exercises the full control channel against a real (in-memory)
// gRPC stream: an agent registers, the hub pushes a Provision command and gets
// the VM back, a heartbeat refreshes liveness, and dropping the stream marks the
// host down.
func TestHubRoundTrip(t *testing.T) {
	inv := newFakeInventory()
	hub := NewHub(inv, zeroCapacity{}, zap.NewNop())

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterAgentLinkServer(srv, hub)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	stream, err := pb.NewAgentLinkClient(cc).Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var sendMu sync.Mutex
	send := func(m *pb.AgentMessage) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(m)
	}

	// Register, then wait until the hub has wired the connection in.
	if err := send(&pb.AgentMessage{Body: &pb.AgentMessage_Register{Register: &pb.Register{
		Id: "h1", Hostname: "host-1", CpusTotal: 4, MemoryMbTotal: 4096,
	}}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	if got := <-inv.registered; got != "h1" {
		t.Fatalf("registered host = %q, want h1", got)
	}

	// Agent responder: answer the one Provision command with a running VM.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			cmd := msg.GetCommand()
			if cmd == nil {
				continue
			}
			vm, _ := json.Marshal(agent.VM{ID: "vm-1", ServerID: "s1", Host: "10.0.0.5", Port: 25565, State: agent.StateRunning})
			_ = send(&pb.AgentMessage{Body: &pb.AgentMessage_Result{Result: &pb.Result{Id: cmd.Id, Payload: vm}}})
		}
	}()

	vm, err := hub.Provision(ctx, "h1", agent.VMSpec{ServerID: "s1", CPUs: 1, MemoryMB: 1024})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if vm == nil || vm.ID != "vm-1" || vm.State != agent.StateRunning || vm.Host != "10.0.0.5" {
		t.Fatalf("provisioned vm = %+v, want vm-1 running on 10.0.0.5", vm)
	}

	// A command to a host with no connection is reported, not blocked.
	if _, err := hub.Provision(ctx, "unknown", agent.VMSpec{}); err != ErrHostNotConnected {
		t.Errorf("provision unknown host = %v, want ErrHostNotConnected", err)
	}

	// Heartbeat refreshes liveness.
	if err := send(&pb.AgentMessage{Body: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{}}}); err != nil {
		t.Fatalf("send heartbeat: %v", err)
	}
	if got := <-inv.heartbeat; got != "h1" {
		t.Fatalf("heartbeat host = %q, want h1", got)
	}

	// Dropping the stream marks the host down.
	_ = cc.Close()
	select {
	case got := <-inv.down:
		if got != "h1" {
			t.Fatalf("marked down host = %q, want h1", got)
		}
	case <-ctx.Done():
		t.Fatal("host was not marked down after disconnect")
	}
}
