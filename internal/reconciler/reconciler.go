// Package reconciler drives game servers from their observed status toward
// their desired state, using a provisioner backend.
package reconciler

import (
	"context"
	"errors"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/aarani/craftling-go/internal/provisioner"
	"github.com/aarani/craftling-go/internal/repository"
	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/scheduler"
	"go.uber.org/zap"
)

// livenessProbeTimeout bounds a single agent Status round-trip in the liveness
// pass, so one slow or wedged host can't stall the whole reconcile tick.
const livenessProbeTimeout = 10 * time.Second

// Meter records billable running time for pay-as-you-go hourly billing (P9). A
// server's clock starts when the reconciler marks it running and stops when it
// stops, is lost, or is deleted. Both methods are idempotent so a retry can't
// double-bill. It is optional: a nil Meter disables billing entirely.
type Meter interface {
	StartRunning(ctx context.Context, s *model.GameServer) error
	StopRunning(ctx context.Context, serverID string) error
}

// WhitelistSource yields a server's desired in-game whitelist: the usernames of
// the players its owner granted onto it. The reconciler periodically feeds this
// to each running server over RCON. *repository.PlayerRepository satisfies it.
// Optional: a nil source disables whitelist sync.
type WhitelistSource interface {
	UsernamesForServer(ctx context.Context, serverID string) ([]string, error)
}

// FenceStore records and drains VMs orphaned on hosts the control plane could no
// longer reach when it rescheduled their servers (P8b). The reconciler adds a
// fence when it presumes a host dead, then evicts the orphan once the host is
// reachable again. *repository.FenceRepository satisfies it. Optional: a nil store
// disables orphan fencing.
type FenceStore interface {
	Add(ctx context.Context, f model.FencedVM) error
	List(ctx context.Context) ([]model.FencedVM, error)
	Delete(ctx context.Context, hostID, vmID string) error
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// whitelistSyncInterval is how often the reconciler re-feeds every running
// server's whitelist. The guest reconciles to the exact set (a steady-state push
// is just one "whitelist list" read), so re-pushing on an interval is cheap and
// covers both grant changes and freshly started servers without a change feed.
const whitelistSyncInterval = 30 * time.Second

// Reconciler periodically converges game servers toward their desired state.
type Reconciler struct {
	servers   *repository.GameServerRepository
	prov      provisioner.Provisioner
	sched     *scheduler.Scheduler
	meter     Meter
	whitelist WhitelistSource
	fences    FenceStore
	log       *zap.Logger
	// hostDeadAfter is how long a running server's host may stay unreachable
	// before the reconciler presumes the VM dead and reschedules. It must exceed
	// the heartbeat TTL so a brief agent restart isn't mistaken for a loss.
	hostDeadAfter time.Duration
	// backoffBase and backoffMax bound the exponential retry backoff applied to a
	// server whose reconcile step fails (P8a): the nth consecutive failure waits
	// min(backoffBase * 2^(n-1), backoffMax) before the server is eligible again.
	backoffBase time.Duration
	backoffMax  time.Duration
}

// New constructs a Reconciler. hostDeadAfter is the grace a running server's host
// gets to come back before its VM is presumed dead (see Reconciler.hostDeadAfter).
// backoffBase/backoffMax bound the exponential retry backoff on a failed reconcile.
// meter may be nil to disable billing metering; whitelist may be nil to disable
// in-game whitelist sync; fences may be nil to disable orphan fencing (P8b).
func New(servers *repository.GameServerRepository, prov provisioner.Provisioner, sched *scheduler.Scheduler, meter Meter, whitelist WhitelistSource, fences FenceStore, hostDeadAfter, backoffBase, backoffMax time.Duration, log *zap.Logger) *Reconciler {
	return &Reconciler{servers: servers, prov: prov, sched: sched, meter: meter, whitelist: whitelist, fences: fences, hostDeadAfter: hostDeadAfter, backoffBase: backoffBase, backoffMax: backoffMax, log: log}
}

// startBilling opens a server's metered running interval (best-effort: a billing
// failure is logged, never blocks reconciliation).
func (r *Reconciler) startBilling(ctx context.Context, s *model.GameServer) {
	if r.meter == nil {
		return
	}
	if err := r.meter.StartRunning(ctx, s); err != nil {
		r.log.Warn("billing: start running", zap.String("id", s.ID), zap.Error(err))
	}
}

// stopBilling closes a server's open metered interval (best-effort).
func (r *Reconciler) stopBilling(ctx context.Context, serverID string) {
	if r.meter == nil {
		return
	}
	if err := r.meter.StopRunning(ctx, serverID); err != nil {
		r.log.Warn("billing: stop running", zap.String("id", serverID), zap.Error(err))
	}
}

// Run reconciles on each tick until ctx is cancelled. It also runs a slower,
// independent loop that feeds each running server's in-game whitelist over RCON,
// so player-grant changes converge without burdening the fast desired-state tick.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if r.whitelist != nil {
		go r.runWhitelistSync(ctx, whitelistSyncInterval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.ReconcileOnce(ctx)
		}
	}
}

// runWhitelistSync periodically pushes every running server's desired whitelist
// to its host. It is decoupled from the desired-state loop: a slow or unreachable
// host only delays its own whitelist, never the core reconcile.
func (r *Reconciler) runWhitelistSync(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.syncWhitelists(ctx)
		}
	}
}

// syncWhitelists feeds each running server's granted-player set to its workload
// over RCON. Failures are logged, not fatal — a host that is unreachable or not
// RCON-capable (the fake runtime, or a server without RCON configured) simply
// keeps its current whitelist until the next sweep.
func (r *Reconciler) syncWhitelists(ctx context.Context) {
	servers, err := r.servers.ListRunning(ctx)
	if err != nil {
		r.log.Error("whitelist sync: list running servers", zap.Error(err))
		return
	}
	for i := range servers {
		s := &servers[i]
		names, err := r.whitelist.UsernamesForServer(ctx, s.ID)
		if err != nil {
			r.log.Warn("whitelist sync: load grants", zap.String("id", s.ID), zap.Error(err))
			continue
		}
		if err := r.prov.SyncWhitelist(ctx, s, names); err != nil {
			r.log.Debug("whitelist sync: push", zap.String("id", s.ID), zap.Error(err))
		}
	}
}

// ReconcileOnce processes one batch of servers needing reconciliation.
//
// It runs under the reconciler's lifetime context, with no per-batch deadline:
// a single step can legitimately take minutes (a cold image build pulls and
// flattens a multi-hundred-MB image), and a short request-scoped timeout here
// would abort that work and flip the server to an error status while the agent
// is still making progress. Provisioning is bounded by the agent-side image
// pull timeout and by process shutdown (ctx cancellation), not by this loop.
func (r *Reconciler) ReconcileOnce(ctx context.Context) {
	// First re-check servers we believe are already running: the convergence loop
	// below never revisits them (their status matches desire), so this is the only
	// place a VM that died under us gets noticed and dropped back to pending.
	r.checkLiveness(ctx)

	// Reclaim VMs orphaned on hosts that have since come back (P8b fencing).
	r.reconcileFences(ctx)

	servers, err := r.servers.ListReconcilable(ctx)
	if err != nil {
		r.log.Error("list reconcilable servers", zap.Error(err))
		return
	}

	for i := range servers {
		s := &servers[i]
		if err := r.reconcile(ctx, s); err != nil {
			r.backOff(ctx, s, err)
		}
	}
}

// backOff records a failed reconcile and schedules the next retry with exponential
// backoff (P8a). Instead of flipping the server to 'error' and letting it be
// re-picked on the very next 2s tick, the nth consecutive failure waits
// min(backoffBase * 2^(n-1), backoffMax) before ListReconcilable will return it
// again — so a persistently failing server (a bad image, a wedged host) is retried
// with widening spacing rather than hammered. A later success or an explicit
// desired-state change resets the counter.
func (r *Reconciler) backOff(ctx context.Context, s *model.GameServer, cause error) {
	attempts := s.Attempts + 1
	delay := backoffDelay(r.backoffBase, r.backoffMax, attempts)
	r.log.Error("reconcile server",
		zap.String("id", s.ID), zap.Int("attempts", attempts),
		zap.Duration("retry_in", delay), zap.Error(cause))
	if err := r.servers.MarkReconcileFailed(ctx, s.ID, cause.Error(), attempts, time.Now().Add(delay)); err != nil {
		r.log.Error("record reconcile failure", zap.String("id", s.ID), zap.Error(err))
	}
}

// backoffDelay returns the wait before the nth consecutive failed attempt is
// retried: base doubled per attempt, capped at max. attempts is 1-based, so the
// first failure waits base. A non-positive base disables spacing (retry next tick).
func backoffDelay(base, max time.Duration, attempts int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
}

// checkLiveness re-examines every server we believe is running and asks its
// host's agent whether the VM is still there. A server at its desired state is
// otherwise never revisited, so without this a VM that vanishes — its agent
// restarted and lost it, or its host dropped off the fleet — leaves the server
// stuck "running" behind a dead port forever. Anything it finds dead is dropped
// to pending; the convergence loop in the same tick then re-provisions it.
func (r *Reconciler) checkLiveness(ctx context.Context) {
	servers, err := r.servers.ListRunning(ctx)
	if err != nil {
		r.log.Error("list running servers", zap.Error(err))
		return
	}
	for i := range servers {
		r.checkServerLiveness(ctx, &servers[i])
	}
}

// checkServerLiveness probes one running server's VM and recovers it if dead.
// The three outcomes:
//   - the agent reports the VM running: healthy, nothing to do;
//   - the agent answers but the VM is gone (it is alive, the VM is not): drop to
//     pending so it re-provisions in place — the host is up;
//   - the agent is unreachable: the host may just be restarting, so wait out
//     hostDeadAfter before presuming the VM dead; past it, drop to pending and
//     let start() reschedule onto a live host.
func (r *Reconciler) checkServerLiveness(ctx context.Context, s *model.GameServer) {
	probeCtx, cancel := context.WithTimeout(ctx, livenessProbeTimeout)
	report, err := r.prov.Status(probeCtx, s)
	cancel()
	if err != nil {
		dead, derr := r.hostDeadTooLong(ctx, s)
		if derr != nil {
			r.log.Error("liveness: host check", zap.String("id", s.ID), zap.Error(derr))
			return
		}
		if !dead {
			return // within grace; give the host a chance to come back
		}
		r.log.Warn("running server's host unreachable past grace; reprovisioning",
			zap.String("id", s.ID), zap.Stringp("host_id", s.HostID), zap.Error(err))
		// The host is unreachable, not confirmed dead: its agent may still be
		// running the VM behind a partition. Fence the abandoned VM so that, once
		// the host comes back, the reconciler evicts the orphan rather than leaving
		// a zombie that could keep snapshotting the world the reschedule now owns
		// (the generation guard already blocks those writes; this reclaims it). The
		// world-store key is reassigned to the new incarnation, so the fence drains
		// via Evict, which never touches the durable world.
		r.recordFence(ctx, s)
		r.markLost(ctx, s, "host unreachable; reprovisioning")
		return
	}
	if report.State == provisioner.StateRunning {
		// The VM is alive; fold in the workload's deep health (player count,
		// liveness) the agent probed, so the API and UI reflect the game process,
		// not just the VM. Best-effort: a failed write is logged, never fatal.
		r.recordHealth(ctx, s, report.Health)
		return
	}
	r.log.Warn("running server has no live VM; reprovisioning",
		zap.String("id", s.ID), zap.Stringp("vm_id", s.VMID), zap.String("observed", string(report.State)))
	r.markLost(ctx, s, "vm not found on host; reprovisioning")
}

// healthRefreshInterval bounds how often a still-running server's last_seen is
// re-stamped when its player counts are unchanged. The liveness pass runs every
// reconcile tick, but health is telemetry, not a state transition, so we throttle
// the write: it lands when the counts change or when last_seen has gone stale,
// not on every tick.
const healthRefreshInterval = 10 * time.Second

// recordHealth persists the workload's probed deep health onto the server, when
// the agent reported any. It skips the write when nothing has changed since the
// last reading (same counts and a still-fresh last_seen), so a stable server
// isn't re-written every tick.
func (r *Reconciler) recordHealth(ctx context.Context, s *model.GameServer, h *runspec.Health) {
	if h == nil {
		return
	}
	if !r.healthNeedsWrite(s, h) {
		return
	}
	if err := r.servers.MarkHealth(ctx, s.ID, h.Reachable, h.PlayersOnline, h.PlayersMax); err != nil {
		r.log.Warn("record server health", zap.String("id", s.ID), zap.Error(err))
	}
}

// healthNeedsWrite reports whether a fresh probe differs enough from what is
// already stored to be worth persisting: a changed reachability or player count,
// or a reachable server whose last_seen has aged past healthRefreshInterval (so
// proof-of-life keeps advancing even while the count holds steady).
func (r *Reconciler) healthNeedsWrite(s *model.GameServer, h *runspec.Health) bool {
	var online, max *int
	if h.Reachable {
		online, max = &h.PlayersOnline, &h.PlayersMax
	}
	if !eqIntPtr(s.PlayersOnline, online) || !eqIntPtr(s.PlayersMax, max) {
		return true
	}
	if h.Reachable && (s.LastSeen == nil || time.Since(*s.LastSeen) >= healthRefreshInterval) {
		return true
	}
	return false
}

// eqIntPtr reports whether two optional ints are equal (both nil, or both set to
// the same value).
func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// hostDeadTooLong reports whether a server's assigned host has been unreachable
// long enough (hostDeadAfter since its last heartbeat) to presume its VM dead. A
// server with no host, or whose host has fallen out of the inventory entirely,
// is dead by definition.
func (r *Reconciler) hostDeadTooLong(ctx context.Context, s *model.GameServer) (bool, error) {
	if s.HostID == nil {
		return true, nil
	}
	last, known, err := r.sched.LastHeartbeat(ctx, *s.HostID)
	if err != nil {
		return false, err
	}
	if !known {
		return true, nil
	}
	return time.Since(last) > r.hostDeadAfter, nil
}

// fenceGCAfter is how long an undrained fence is kept before it is GC'd: a host
// down this long is presumed gone for good (its VM unreachable forever and its
// world long since superseded), so the fence is dead weight.
const fenceGCAfter = 24 * time.Hour

// recordFence persists a fence for the VM a server is abandoning on an unreachable
// host (P8b), so the reconciler can evict the orphan once the host returns. A
// no-op when fencing is disabled or the server has no host/VM to abandon. A write
// failure is logged, not propagated: the markLost reschedule must still proceed
// (the generation guard already protects the world), and the fence is best-effort
// resource reclaim.
func (r *Reconciler) recordFence(ctx context.Context, s *model.GameServer) {
	if r.fences == nil || s.HostID == nil || *s.HostID == "" || s.VMID == nil || *s.VMID == "" {
		return
	}
	f := model.FencedVM{ServerID: s.ID, HostID: *s.HostID, VMID: *s.VMID, Generation: s.Generation}
	if err := r.fences.Add(ctx, f); err != nil {
		r.log.Error("record fence", zap.String("id", s.ID), zap.String("vm_id", *s.VMID), zap.Error(err))
	}
}

// reconcileFences drains outstanding orphan fences (P8b). For each fenced VM whose
// host has become reachable again — the partition healed and the agent reconnected
// — it evicts the orphan (reclaiming its resources while preserving the durable
// world the new incarnation now owns) and clears the fence. Fences for hosts still
// unreachable are left for a later tick; ones older than fenceGCAfter are swept,
// since a host gone that long won't return. Best-effort throughout: a failure is
// logged and retried next tick.
func (r *Reconciler) reconcileFences(ctx context.Context) {
	if r.fences == nil {
		return
	}
	if _, err := r.fences.DeleteOlderThan(ctx, time.Now().Add(-fenceGCAfter)); err != nil {
		r.log.Warn("gc stale fences", zap.Error(err))
	}
	fences, err := r.fences.List(ctx)
	if err != nil {
		r.log.Error("list fences", zap.Error(err))
		return
	}
	for i := range fences {
		f := &fences[i]
		reachable, err := r.sched.Placeable(ctx, f.HostID)
		if err != nil {
			r.log.Error("fence: host check", zap.String("host_id", f.HostID), zap.Error(err))
			continue
		}
		if !reachable {
			continue // host still gone; keep the fence until it returns or ages out
		}
		if err := r.prov.EvictVM(ctx, f.HostID, f.VMID); err != nil {
			r.log.Warn("fence: evict orphan vm", zap.String("host_id", f.HostID),
				zap.String("vm_id", f.VMID), zap.Error(err))
			continue // leave the fence; retry next tick
		}
		if err := r.fences.Delete(ctx, f.HostID, f.VMID); err != nil {
			r.log.Error("fence: clear", zap.String("host_id", f.HostID), zap.String("vm_id", f.VMID), zap.Error(err))
			continue
		}
		r.log.Info("fenced orphan VM evicted",
			zap.String("server_id", f.ServerID), zap.String("host_id", f.HostID), zap.String("vm_id", f.VMID))
	}
}

// markLost drops a presumed-dead running server back to pending for re-provision,
// logging (not propagating) a failure since the next tick simply retries.
func (r *Reconciler) markLost(ctx context.Context, s *model.GameServer, message string) {
	if err := r.servers.MarkLost(ctx, s.ID, message); err != nil {
		r.log.Error("mark server lost", zap.String("id", s.ID), zap.Error(err))
		return
	}
	// The VM is gone, so stop the billing clock; the re-provision in a later tick
	// opens a fresh interval, so only observed-running time is ever billed.
	r.stopBilling(ctx, s.ID)
}

// reconcile advances a single server one step toward its desired state.
func (r *Reconciler) reconcile(ctx context.Context, s *model.GameServer) error {
	// A pending delete supersedes everything else (deprovision destroys the
	// world anyway, so a queued backup is moot).
	if s.DesiredState == model.DesiredDeleted {
		return r.delete(ctx, s)
	}
	// Honor an on-demand backup request out of band from the desired-state
	// convergence. Its failures are self-contained (logged, retried), never
	// flipping the running server to an error status.
	if s.BackupRequested {
		r.backup(ctx, s)
	}
	switch s.DesiredState {
	case model.DesiredStopped:
		return r.stop(ctx, s)
	case model.DesiredRunning:
		return r.start(ctx, s)
	default:
		return nil
	}
}

// backup performs a pending on-demand world snapshot. A running server is
// snapshotted live via the agent; a stopped server already had its world
// snapshotted on stop, so the request is satisfied immediately. Either way the
// flag is cleared. A live-snapshot failure leaves the flag set so the next tick
// retries, without disturbing the server's status.
func (r *Reconciler) backup(ctx context.Context, s *model.GameServer) {
	if s.Status != model.StatusRunning || s.VMID == nil || *s.VMID == "" {
		if err := r.servers.MarkBackedUp(ctx, s.ID); err != nil {
			r.log.Error("clear backup request", zap.String("id", s.ID), zap.Error(err))
		}
		return
	}
	if err := r.prov.Snapshot(ctx, s); err != nil {
		r.log.Warn("on-demand backup failed; will retry", zap.String("id", s.ID), zap.Error(err))
		return // leave backup_requested set for the next tick
	}
	r.log.Info("server backed up", zap.String("id", s.ID))
	if err := r.servers.MarkBackedUp(ctx, s.ID); err != nil {
		r.log.Error("clear backup request", zap.String("id", s.ID), zap.Error(err))
	}
}

func (r *Reconciler) start(ctx context.Context, s *model.GameServer) error {
	if s.Status == model.StatusRunning {
		return nil
	}
	// A server that still has a VM (e.g. an interrupted provision, retried before
	// it was torn down) is resumed in place rather than rebuilt from scratch.
	provisioned := s.VMID != nil && *s.VMID != ""
	// A server placed but never booted (provisioning failed before a VM existed)
	// holds only a capacity reservation, no host-local footprint. If its assigned
	// host is no longer a ready placement target — it went down, or its agent
	// reconnected under a new host id, orphaning the old one — drop that stale
	// assignment so we re-schedule below. Without this the server is trapped:
	// every retry re-targets the dead host and fails with "host has no live agent
	// connection". A provisioned server keeps its host (its VM and world live
	// there); only an unbooted one is free to move.
	if !provisioned {
		if err := r.dropStalePlacement(ctx, s); err != nil {
			return err
		}
	}
	// Place an unassigned server on a host before booting a VM. A stopped server
	// was unplaced on stop, so it re-schedules here and may land on a new host
	// (its world rides the durable store). A mid-flight server that still has a
	// VM or a host assignment keeps it across a retry, so neither re-schedules.
	if !provisioned && s.HostID == nil {
		if err := r.place(ctx, s); err != nil {
			return err
		}
		if s.HostID == nil {
			return nil // unschedulable; retried next tick
		}
	}
	if err := r.servers.MarkStatus(ctx, s.ID, model.StatusProvisioning, ""); err != nil {
		return err
	}
	// A fresh provision is a new VM incarnation: bump the generation fencing token
	// (P8b) so it strictly exceeds any prior incarnation's. The token rides down in
	// the VMSpec and the agent stamps it onto the durable world write, fencing out a
	// superseded zombie. A resume keeps the existing generation (same incarnation).
	if !provisioned {
		gen, err := r.servers.NextGeneration(ctx, s.ID)
		if err != nil {
			return err
		}
		s.Generation = gen
	}
	inst, err := r.provisionOrStart(ctx, s, provisioned)
	if err != nil {
		return err
	}
	r.log.Info("server running",
		zap.String("id", s.ID), zap.String("vm_id", inst.VMID),
		zap.Stringp("host_id", s.HostID), zap.Bool("resumed", provisioned))
	if err := r.servers.MarkRunning(ctx, s.ID, inst.VMID, inst.Host, inst.Port); err != nil {
		return err
	}
	// The server is now running: start (or resume) its billing clock.
	r.startBilling(ctx, s)
	return nil
}

// place asks the scheduler for a host, reserves its capacity, and persists the
// assignment onto s (both in the DB and the in-memory struct). When nothing
// fits it marks the server unschedulable and leaves s.HostID nil; the caller
// detects that and yields until the next tick. A reservation that cannot be
// persisted is released so capacity is not leaked.
func (r *Reconciler) place(ctx context.Context, s *model.GameServer) error {
	hostID, err := r.sched.Schedule(ctx, s)
	if errors.Is(err, scheduler.ErrNoCapacity) {
		r.log.Warn("no capacity to place server", zap.String("id", s.ID),
			zap.Int("cpus", s.CPUs), zap.Int("memory_mb", s.MemoryMB))
		return r.servers.MarkStatus(ctx, s.ID, model.StatusUnschedulable,
			"no host with sufficient capacity")
	}
	if err != nil {
		return err
	}
	if err := r.servers.AssignHost(ctx, s.ID, hostID); err != nil {
		_ = r.sched.Release(ctx, hostID, s.CPUs, s.MemoryMB)
		return err
	}
	s.HostID = &hostID
	return nil
}

// dropStalePlacement releases a server's reservation on a host that is no longer
// a ready placement target and clears the assignment, so start() re-schedules it
// onto a live host. It is a no-op for a server with no host or a host that is
// still placeable. Callers must restrict it to unbooted servers (no VM): such a
// server has no host-local footprint to lose, so moving it costs nothing, whereas
// a running server's VM and world are pinned to its host.
func (r *Reconciler) dropStalePlacement(ctx context.Context, s *model.GameServer) error {
	if s.HostID == nil {
		return nil
	}
	placeable, err := r.sched.Placeable(ctx, *s.HostID)
	if err != nil {
		return err
	}
	if placeable {
		return nil
	}
	_ = r.sched.Release(ctx, *s.HostID, s.CPUs, s.MemoryMB)
	if err := r.servers.UnassignHost(ctx, s.ID); err != nil {
		return err
	}
	r.log.Info("released stale host placement; will reschedule",
		zap.String("id", s.ID), zap.Stringp("stale_host_id", s.HostID))
	s.HostID = nil
	return nil
}

// hostReachable reports whether a server's assigned host is still a live member
// of the fleet — i.e. a ready placement target the agent is connected to. It
// lets a failed teardown distinguish "the host is gone, give up on the remote
// call" from "the host is up but the op failed, retry". A server with no host
// has nothing to reach.
func (r *Reconciler) hostReachable(ctx context.Context, s *model.GameServer) (bool, error) {
	if s.HostID == nil {
		return false, nil
	}
	return r.sched.Placeable(ctx, *s.HostID)
}

// provisionOrStart resumes an existing VM or provisions a new one.
func (r *Reconciler) provisionOrStart(ctx context.Context, s *model.GameServer, provisioned bool) (*provisioner.Instance, error) {
	if provisioned {
		return r.prov.Start(ctx, s)
	}
	return r.prov.Provision(ctx, s)
}

func (r *Reconciler) stop(ctx context.Context, s *model.GameServer) error {
	if s.Status == model.StatusStopped {
		return nil
	}
	if err := r.servers.MarkStatus(ctx, s.ID, model.StatusStopping, ""); err != nil {
		return err
	}
	// Halt the VM, snapshotting its world to the durable store.
	if err := r.prov.Stop(ctx, s); err != nil {
		return err
	}
	// Release the host: evict the VM's host-local footprint (keeping the durable
	// world) and return its reservation, so a stopped server ties up no capacity
	// and is free to reschedule onto any host when it next starts. The world
	// rides the durable store, keyed by server id, so the restart restores it
	// wherever it lands. NOTE: on a host with world persistence but no durable
	// store, the world is host-local only, so a reschedule to a different host
	// starts from an empty world — an accepted tradeoff for freeing capacity.
	if err := r.prov.Evict(ctx, s); err != nil {
		return err
	}
	if s.HostID != nil {
		_ = r.sched.Release(ctx, *s.HostID, s.CPUs, s.MemoryMB)
	}
	if err := r.servers.MarkStopped(ctx, s.ID); err != nil {
		return err
	}
	r.log.Info("server stopped", zap.String("id", s.ID))
	r.stopBilling(ctx, s.ID)
	return nil
}

func (r *Reconciler) delete(ctx context.Context, s *model.GameServer) error {
	if s.Status != model.StatusDeleting {
		if err := r.servers.MarkStatus(ctx, s.ID, model.StatusDeleting, ""); err != nil {
			return err
		}
	}
	if err := r.prov.Deprovision(ctx, s); err != nil {
		// A delete is terminal user intent, so it must not be trapped by a host we
		// can no longer reach. If the assigned host is gone — down, or its agent
		// reconnected under a new id, orphaning the old one — its VM went with it
		// (or is orphaned beyond reach), so there is nothing left to tear down:
		// finalize the removal anyway. A failure on a still-live host is a real
		// teardown error; surface it and retry rather than leak a running VM behind
		// a "deleted" record.
		reachable, perr := r.hostReachable(ctx, s)
		if perr != nil {
			return perr
		}
		if reachable {
			return err
		}
		r.log.Warn("deprovision failed on unreachable host; finalizing delete anyway",
			zap.String("id", s.ID), zap.Stringp("host_id", s.HostID), zap.Error(err))
	}
	// The VM is gone, so return its reserved capacity to the host.
	if s.HostID != nil {
		_ = r.sched.Release(ctx, *s.HostID, s.CPUs, s.MemoryMB)
	}
	if err := r.servers.SoftDelete(ctx, s.ID); err != nil {
		return err
	}
	r.log.Info("server deleted", zap.String("id", s.ID))
	// Close any open interval so a deleted server stops accruing; its retained
	// rows still bill for the time it ran.
	r.stopBilling(ctx, s.ID)
	return nil
}
