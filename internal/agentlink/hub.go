// Package agentlink is the control-plane side of the agent control channel. The
// agent dials in and holds one long-lived bidirectional gRPC stream open; the
// Hub registers the host, tracks the live connection, and pushes VM lifecycle
// commands down the stream on the provisioner's behalf. This inverts the older
// model where the control plane dialed each agent's HTTP API: agents now need no
// inbound reachability, and the open stream is itself the host's liveness signal.
package agentlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/aarani/craftling-go/internal/agent"
	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrHostNotConnected means the target host has no live agent stream, so a
// command cannot be delivered. The provisioner surfaces it like any other
// transport failure.
var ErrHostNotConnected = errors.New("host has no live agent connection")

// HostInventory is the slice of the fleet inventory the hub drives: it (re)adds
// a host on stream open, refreshes liveness on heartbeats, and marks it down on
// disconnect. *repository.HostRepository satisfies it.
type HostInventory interface {
	RegisterReserved(ctx context.Context, h *model.Host, reservedCPUs, reservedMemMB int) (*model.Host, error)
	Heartbeat(ctx context.Context, id string) error
	MarkDown(ctx context.Context, id string) error
}

// CapacityReconstructor reconstructs the capacity already committed to a host id
// from the durable record, so a host re-registering after a control-plane
// restart comes back with its real allocatable. *repository.GameServerRepository
// satisfies it (this is the same seam the old HTTP register handler used).
type CapacityReconstructor interface {
	UsedCapacity(ctx context.Context, hostID string) (cpus, memoryMB int, err error)
}

// Hub is the gRPC AgentLink server plus an in-memory registry of live agent
// connections keyed by host id. Like the host inventory it fronts, the registry
// is per-process: it assumes a single control-plane instance, the same
// assumption repository.HostRepository already makes.
type Hub struct {
	pb.UnimplementedAgentLinkServer

	hosts HostInventory
	cap   CapacityReconstructor
	log   *zap.Logger

	mu    sync.RWMutex
	conns map[string]*conn // by host id
}

// NewHub constructs a Hub over the fleet inventory and the capacity source.
func NewHub(hosts HostInventory, cap CapacityReconstructor, log *zap.Logger) *Hub {
	return &Hub{
		hosts: hosts,
		cap:   cap,
		log:   log,
		conns: make(map[string]*conn),
	}
}

// conn is one live agent stream and the commands awaiting their replies.
type conn struct {
	stream grpc.BidiStreamingServer[pb.AgentMessage, pb.ControlMessage]

	sendMu sync.Mutex // serializes Send (gRPC forbids concurrent Send)

	mu      sync.Mutex
	waiters map[string]chan *pb.Result // by command id
}

func (c *conn) send(m *pb.ControlMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(m)
}

func (c *conn) addWaiter(id string, ch chan *pb.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waiters[id] = ch
}

func (c *conn) removeWaiter(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.waiters, id)
}

// resolve hands a result to its waiter, if one is still listening.
func (c *conn) resolve(res *pb.Result) {
	c.mu.Lock()
	ch, ok := c.waiters[res.Id]
	c.mu.Unlock()
	if !ok {
		return // caller timed out and gave up
	}
	select {
	case ch <- res:
	default: // buffered chan; a duplicate result is dropped
	}
}

// Connect handles one agent's lifetime. The agent sends a Register frame first;
// the hub adds it to the inventory and registry, then services Results and
// Heartbeats until the stream ends, at which point the host is marked down.
func (h *Hub) Connect(stream grpc.BidiStreamingServer[pb.AgentMessage, pb.ControlMessage]) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message must be register")
	}

	// Reconstruct any capacity already committed to this host (only meaningful
	// when the agent supplies its stable id), mirroring the old register path.
	usedCPUs, usedMemMB, err := h.cap.UsedCapacity(ctx, reg.Id)
	if err != nil {
		h.log.Error("reconstruct host capacity", zap.Error(err))
		return status.Error(codes.Internal, "reconstruct host capacity")
	}

	host, err := h.hosts.RegisterReserved(ctx, &model.Host{
		ID:            reg.Id,
		Hostname:      reg.Hostname,
		Zone:          reg.Zone,
		CPUsTotal:     int(reg.CpusTotal),
		MemoryMBTotal: int(reg.MemoryMbTotal),
		AgentVersion:  reg.AgentVersion,
	}, usedCPUs, usedMemMB)
	if err != nil {
		h.log.Error("register host", zap.Error(err))
		return status.Error(codes.Internal, "register host")
	}
	hostID := host.ID

	c := &conn{stream: stream, waiters: make(map[string]chan *pb.Result)}
	h.add(hostID, c)
	h.log.Info("agent connected", zap.String("host_id", hostID), zap.String("hostname", reg.Hostname))

	defer func() {
		h.remove(hostID, c)
		// MarkDown on a fresh context: stream.Context() is already cancelled.
		if err := h.hosts.MarkDown(context.Background(), hostID); err != nil {
			h.log.Warn("mark host down", zap.String("host_id", hostID), zap.Error(err))
		}
		h.log.Info("agent disconnected", zap.String("host_id", hostID))
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err // io.EOF on clean close, or a transport error
		}
		switch {
		case msg.GetResult() != nil:
			c.resolve(msg.GetResult())
		case msg.GetHeartbeat() != nil:
			if err := h.hosts.Heartbeat(ctx, hostID); err != nil {
				h.log.Warn("host heartbeat", zap.String("host_id", hostID), zap.Error(err))
			}
		}
	}
}

func (h *Hub) add(hostID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns[hostID] = c
}

// remove drops c from the registry only if it is still the current connection
// for hostID, so a reconnect that raced ahead is not clobbered.
func (h *Hub) remove(hostID string, c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[hostID] == c {
		delete(h.conns, hostID)
	}
}

func (h *Hub) get(hostID string) *conn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.conns[hostID]
}

// Provision asks the host's agent to create and boot a VM for the spec.
func (h *Hub) Provision(ctx context.Context, hostID string, spec agent.VMSpec) (*agent.VM, error) {
	return h.call(ctx, hostID, agent.OpProvision, spec)
}

// Start asks the host's agent to boot an existing VM.
func (h *Hub) Start(ctx context.Context, hostID, vmID string) (*agent.VM, error) {
	return h.call(ctx, hostID, agent.OpStart, agent.VMRef{VMID: vmID})
}

// Stop asks the host's agent to halt a VM without destroying it.
func (h *Hub) Stop(ctx context.Context, hostID, vmID string) error {
	_, err := h.call(ctx, hostID, agent.OpStop, agent.VMRef{VMID: vmID})
	return err
}

// Snapshot asks the host's agent to take an on-demand world snapshot.
func (h *Hub) Snapshot(ctx context.Context, hostID, vmID string) error {
	_, err := h.call(ctx, hostID, agent.OpSnapshot, agent.VMRef{VMID: vmID})
	return err
}

// SyncWhitelist asks the host's agent to reconcile a VM's workload whitelist to
// the given usernames over RCON.
func (h *Hub) SyncWhitelist(ctx context.Context, hostID, vmID string, usernames []string) error {
	_, err := h.callRaw(ctx, hostID, agent.OpWhitelist, agent.WhitelistRequest{VMID: vmID, Usernames: usernames})
	return err
}

// Evict asks the host's agent to tear down a VM while preserving its durable
// world, releasing the host so the server can be rescheduled elsewhere.
func (h *Hub) Evict(ctx context.Context, hostID, vmID string) error {
	_, err := h.call(ctx, hostID, agent.OpEvict, agent.VMRef{VMID: vmID})
	return err
}

// Deprovision asks the host's agent to destroy a VM, including its durable world.
func (h *Hub) Deprovision(ctx context.Context, hostID, vmID string) error {
	_, err := h.call(ctx, hostID, agent.OpDeprovision, agent.VMRef{VMID: vmID})
	return err
}

// Status fetches a VM's observed state from the host's agent.
func (h *Hub) Status(ctx context.Context, hostID, vmID string) (*agent.VM, error) {
	return h.call(ctx, hostID, agent.OpStatus, agent.VMRef{VMID: vmID})
}

// Logs fetches a VM's captured console/VMM output from the host's agent,
// returning the raw log bytes (the last tailLines lines when tailLines > 0, all
// of it otherwise).
func (h *Hub) Logs(ctx context.Context, hostID, vmID string, tailLines int) ([]byte, error) {
	return h.callRaw(ctx, hostID, agent.OpLogs, agent.LogsRequest{VMID: vmID, TailLines: tailLines})
}

// call sends one command down the host's stream and blocks for the correlated
// reply (or until ctx is done). It returns the decoded VM when the op yields
// one, a nil VM for ops that don't, or an error from the agent or transport.
func (h *Hub) call(ctx context.Context, hostID, op string, reqPayload any) (*agent.VM, error) {
	payload, err := h.callRaw(ctx, hostID, op, reqPayload)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var vm agent.VM
	if err := json.Unmarshal(payload, &vm); err != nil {
		return nil, fmt.Errorf("decode %s result: %w", op, err)
	}
	if vm.ID == "" {
		return nil, nil
	}
	return &vm, nil
}

// callRaw sends one command down the host's stream and blocks for the
// correlated reply (or until ctx is done), returning the result payload bytes
// verbatim. It is the shared transport under call (which decodes the payload
// into a VM) and the raw-payload ops like logs.
func (h *Hub) callRaw(ctx context.Context, hostID, op string, reqPayload any) ([]byte, error) {
	c := h.get(hostID)
	if c == nil {
		return nil, ErrHostNotConnected
	}

	payload, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", op, err)
	}

	id := uuid.NewString()
	ch := make(chan *pb.Result, 1)
	c.addWaiter(id, ch)
	defer c.removeWaiter(id)

	if err := c.send(&pb.ControlMessage{Command: &pb.Command{Id: id, Op: op, Payload: payload}}); err != nil {
		return nil, fmt.Errorf("send %s command: %w", op, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Error != "" {
			return nil, fmt.Errorf("agent %s: %s", op, res.Error)
		}
		return res.Payload, nil
	}
}
