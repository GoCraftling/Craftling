package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/agent"
	"github.com/aarani/craftling-go/internal/runspec"
)

// Host side of whitelist sync. The control plane owns each server's whitelist
// (the set of player usernames its owner granted onto it) and pushes the desired
// set down to the agent; the agent forwards it to the guest over the same vsock
// control channel snapshots and health probing already use, and the in-VM init
// reconciles the workload's whitelist over RCON. The host needs no network reach
// into the VM.

// whitelistApplyTimeout bounds one WHITELIST exchange (vsock dial + handshake +
// the guest's own RCON reconcile), so a wedged guest can't stall the sync sweep.
const whitelistApplyTimeout = 10 * time.Second

// SyncWhitelist reconciles a running VM's workload whitelist to usernames. It
// requires the VM's vsock control channel (the same one snapshots and health
// use), so it is only available on persistence hosts; a VM without one returns
// an error the control plane logs and retries. ErrVMNotFound for an unknown id.
func (r *Runtime) SyncWhitelist(_ context.Context, vmID string, usernames []string) error {
	r.mu.Lock()
	m, ok := r.vms[vmID]
	r.mu.Unlock()
	if !ok {
		return agent.ErrVMNotFound
	}
	if m.vsockUDS == "" {
		return fmt.Errorf("firecracker: whitelist sync needs a vsock control channel")
	}

	conn, err := dialVsockControl(m.vsockUDS, runspec.VsockControlPort)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(whitelistApplyTimeout))

	// Usernames are validated [A-Za-z0-9_], so the JSON array is always a single
	// line — safe for the line-delimited control protocol.
	payload, err := json.Marshal(usernames)
	if err != nil {
		return fmt.Errorf("firecracker: marshal whitelist: %w", err)
	}
	line, err := readLineAfter(conn, runspec.WhitelistApply+" "+string(payload))
	if err != nil {
		return fmt.Errorf("firecracker: whitelist sync: %w", err)
	}
	if strings.HasPrefix(line, runspec.SnapErrPrefix) {
		return fmt.Errorf("firecracker: whitelist sync rejected: %s",
			strings.TrimPrefix(line, runspec.SnapErrPrefix))
	}
	return nil
}
