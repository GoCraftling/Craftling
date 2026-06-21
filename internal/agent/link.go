package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	pb "github.com/aarani/craftling-go/internal/agentlink/pb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Command op names. They are the contract between the control-plane hub (which
// emits Commands) and the agent link (which dispatches them to the Runtime).
// They live here, in the shared agent package the hub already imports, so both
// ends reference the same constants.
const (
	OpProvision   = "provision"
	OpStart       = "start"
	OpStop        = "stop"
	OpSnapshot    = "snapshot"
	OpEvict       = "evict"
	OpDeprovision = "deprovision"
	OpStatus      = "status"
	OpLogs        = "logs"
)

// VMRef is the JSON payload for the ops that act on an existing VM by id
// (everything except provision, which carries a full VMSpec). The hub marshals
// it; the agent link decodes it.
type VMRef struct {
	VMID string `json:"vm_id"`
}

// LogsRequest is the JSON payload for OpLogs: the VM to read plus how many
// trailing lines to return (0 = all). The Result payload is the raw log bytes,
// not a JSON-encoded VM.
type LogsRequest struct {
	VMID      string `json:"vm_id"`
	TailLines int    `json:"tail_lines"`
}

// LinkInfo is what the agent announces about itself when it opens the stream.
// It mirrors the old HTTP registration minus the advertise address: the control
// plane no longer dials the agent, so there is nothing to advertise.
type LinkInfo struct {
	ID            string
	Hostname      string
	Zone          string
	AgentVersion  string
	CPUsTotal     int
	MemoryMBTotal int
}

const (
	// linkHeartbeatInterval is how often the agent sends a heartbeat over the
	// open stream. It must stay comfortably below the control plane's host TTL.
	linkHeartbeatInterval = 10 * time.Second
	// linkReconnectInterval is how long to wait before redialing after the
	// stream drops or the control plane is unreachable.
	linkReconnectInterval = 5 * time.Second
)

// RunLink keeps a persistent control-plane connection open for the agent's
// lifetime: it dials the control plane's gRPC AgentLink service, registers,
// then serves Commands the control plane pushes down the stream until ctx is
// cancelled. A dropped stream is retried with a fixed backoff, so a control
// plane restart heals on its own.
func RunLink(ctx context.Context, cpAddr string, rt Runtime, info LinkInfo, log *zap.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectOnce(ctx, cpAddr, rt, info, log); err != nil && ctx.Err() == nil {
			log.Warn("control-plane link dropped; reconnecting", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(linkReconnectInterval):
		}
	}
}

// connectOnce dials the control plane, opens the stream, registers, and serves
// commands until the stream ends (returning the terminating error, if any).
func connectOnce(ctx context.Context, cpAddr string, rt Runtime, info LinkInfo, log *zap.Logger) error {
	cc, err := grpc.NewClient(cpAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()

	stream, err := pb.NewAgentLinkClient(cc).Connect(ctx)
	if err != nil {
		return err
	}

	// A single mutex serializes Send across the heartbeat ticker and the
	// per-command result goroutines (gRPC forbids concurrent Send on a stream).
	send := &sender{stream: stream}

	if err := send.message(&pb.AgentMessage{Body: &pb.AgentMessage_Register{Register: &pb.Register{
		Id:            info.ID,
		Hostname:      info.Hostname,
		Zone:          info.Zone,
		CpusTotal:     int32(info.CPUsTotal),
		MemoryMbTotal: int32(info.MemoryMBTotal),
		AgentVersion:  info.AgentVersion,
	}}}); err != nil {
		return err
	}
	log.Info("connected to control plane", zap.String("addr", cpAddr))

	// streamCtx is cancelled when this stream ends, stopping the heartbeat loop.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go heartbeatLoop(streamCtx, send, log)

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		cmd := msg.GetCommand()
		if cmd == nil {
			continue
		}
		// Handle each command on its own goroutine so a slow op (e.g. a VM boot)
		// doesn't stall the receive loop or other in-flight commands.
		go func(cmd *pb.Command) {
			payload, errStr := execOp(streamCtx, rt, cmd)
			if err := send.message(&pb.AgentMessage{Body: &pb.AgentMessage_Result{Result: &pb.Result{
				Id:      cmd.Id,
				Payload: payload,
				Error:   errStr,
			}}}); err != nil {
				log.Warn("send command result", zap.String("op", cmd.Op), zap.Error(err))
			}
		}(cmd)
	}
}

// heartbeatLoop sends a heartbeat on an interval until the stream ends.
func heartbeatLoop(ctx context.Context, send *sender, log *zap.Logger) {
	ticker := time.NewTicker(linkHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := send.message(&pb.AgentMessage{Body: &pb.AgentMessage_Heartbeat{Heartbeat: &pb.Heartbeat{}}}); err != nil {
				log.Warn("send heartbeat", zap.Error(err))
				return
			}
		}
	}
}

// execOp dispatches a command to the runtime and returns the JSON-encoded VM
// result (nil when the op returns no VM) and an error string ("" on success).
func execOp(ctx context.Context, rt Runtime, cmd *pb.Command) (payload []byte, errStr string) {
	switch cmd.Op {
	case OpProvision:
		var spec VMSpec
		if err := json.Unmarshal(cmd.Payload, &spec); err != nil {
			return nil, "decode spec: " + err.Error()
		}
		vm, err := rt.Provision(ctx, spec)
		return marshalVM(vm), errString(err)
	case OpStart:
		vm, err := rt.Start(ctx, vmRef(cmd))
		return marshalVM(vm), errString(err)
	case OpStop:
		return nil, errString(rt.Stop(ctx, vmRef(cmd)))
	case OpSnapshot:
		return nil, errString(rt.Snapshot(ctx, vmRef(cmd)))
	case OpEvict:
		return nil, errString(rt.Evict(ctx, vmRef(cmd)))
	case OpDeprovision:
		return nil, errString(rt.Deprovision(ctx, vmRef(cmd)))
	case OpStatus:
		vm, err := rt.Status(ctx, vmRef(cmd))
		return marshalVM(vm), errString(err)
	case OpLogs:
		var req LogsRequest
		if err := json.Unmarshal(cmd.Payload, &req); err != nil {
			return nil, "decode logs request: " + err.Error()
		}
		// The result payload is the raw log bytes; the hub returns them verbatim
		// rather than decoding a VM.
		out, err := rt.Logs(ctx, req.VMID, req.TailLines)
		return out, errString(err)
	default:
		return nil, "unknown op " + cmd.Op
	}
}

// vmRef extracts the target VM id from a command payload (best-effort; an
// undecodable payload yields an empty id, which the runtime treats as missing).
func vmRef(cmd *pb.Command) string {
	var ref VMRef
	_ = json.Unmarshal(cmd.Payload, &ref)
	return ref.VMID
}

// marshalVM JSON-encodes a VM, or returns nil for a nil VM (ops with no result).
func marshalVM(vm *VM) []byte {
	if vm == nil {
		return nil
	}
	b, _ := json.Marshal(vm)
	return b
}

// errString renders an error for the wire: "" for success, else its message.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sender serializes Send calls on a client stream.
type sender struct {
	mu     sync.Mutex
	stream grpc.BidiStreamingClient[pb.AgentMessage, pb.ControlMessage]
}

func (s *sender) message(m *pb.AgentMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(m)
}
