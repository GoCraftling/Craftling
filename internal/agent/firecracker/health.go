package firecracker

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/runspec"
	"go.uber.org/zap"
)

// Host side of deep-health probing (P7). The control plane wants to know the
// game process — not just the VM — is up. The host can't cleanly reach a VM's
// service address (the NAT gateway is virtual and the TAP carries no host IP),
// so instead of probing the VM over the network the host asks the guest to probe
// itself: it sends HEALTH down the same vsock control channel the snapshot
// quiesce already uses, and the in-VM init proxies a Server List Ping / RCON
// "list" over loopback and returns the result. A background sweep refreshes each
// running VM's health on an interval and caches it on the machine, so Status —
// which the reconciler already calls every tick — returns the latest reading
// without paying a probe round-trip inline.

// healthProbeTimeout bounds one HEALTH exchange (vsock dial + handshake + the
// guest's own loopback probe), so a wedged guest can't stall the sweep.
const healthProbeTimeout = 10 * time.Second

// probeHealth asks a running VM's guest to probe its workload and returns the
// reported health. It requires the VM's vsock control channel (the same one
// snapshots use), so it is only available on persistence+store hosts; callers
// must skip VMs without a vsock UDS.
func (r *Runtime) probeHealth(m *machine) (*runspec.Health, error) {
	if m.vsockUDS == "" {
		return nil, fmt.Errorf("firecracker: health probe needs a vsock control channel")
	}
	conn, err := dialVsockControl(m.vsockUDS, runspec.VsockControlPort)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(healthProbeTimeout))

	line, err := readLineAfter(conn, runspec.HealthProbe)
	if err != nil {
		return nil, fmt.Errorf("firecracker: health probe: %w", err)
	}
	if strings.HasPrefix(line, runspec.SnapErrPrefix) {
		return nil, fmt.Errorf("firecracker: health probe rejected: %s",
			strings.TrimPrefix(line, runspec.SnapErrPrefix))
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, runspec.SnapOK))
	if payload == "" {
		return nil, fmt.Errorf("firecracker: empty health reply %q", line)
	}
	var h runspec.Health
	if err := json.Unmarshal([]byte(payload), &h); err != nil {
		return nil, fmt.Errorf("firecracker: decode health reply: %w", err)
	}
	return &h, nil
}

// healthSweep periodically probes every running, vsock-capable VM's deep health
// and caches it on the machine. It mirrors snapshotSweep: probes run serially off
// a snapshot of the candidate set (so the runtime lock is never held across a
// probe), and a failure is logged, never fatal — a missed reading is recoverable.
func (r *Runtime) healthSweep(interval time.Duration) {
	defer r.sweepWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			for _, m := range r.healthCandidates() {
				h, err := r.probeHealth(m)
				if err != nil {
					r.cfg.Logger.Debug("health probe failed",
						zap.String("vm", m.id), zap.String("server", m.serverID), zap.Error(err))
					continue
				}
				m.setHealth(h)
			}
		}
	}
}

// healthCandidates returns the running VMs that have a vsock control channel,
// taken under the lock so the sweep can then probe each without holding it.
func (r *Runtime) healthCandidates() []*machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*machine
	for _, m := range r.vms {
		if m.vsockUDS != "" && m.running() {
			out = append(out, m)
		}
	}
	return out
}
