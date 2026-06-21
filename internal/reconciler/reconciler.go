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
	"github.com/aarani/craftling-go/internal/scheduler"
	"go.uber.org/zap"
)

// livenessProbeTimeout bounds a single agent Status round-trip in the liveness
// pass, so one slow or wedged host can't stall the whole reconcile tick.
const livenessProbeTimeout = 10 * time.Second

// Reconciler periodically converges game servers toward their desired state.
type Reconciler struct {
	servers *repository.GameServerRepository
	prov    provisioner.Provisioner
	sched   *scheduler.Scheduler
	log     *zap.Logger
	// hostDeadAfter is how long a running server's host may stay unreachable
	// before the reconciler presumes the VM dead and reschedules. It must exceed
	// the heartbeat TTL so a brief agent restart isn't mistaken for a loss.
	hostDeadAfter time.Duration
}

// New constructs a Reconciler. hostDeadAfter is the grace a running server's host
// gets to come back before its VM is presumed dead (see Reconciler.hostDeadAfter).
func New(servers *repository.GameServerRepository, prov provisioner.Provisioner, sched *scheduler.Scheduler, hostDeadAfter time.Duration, log *zap.Logger) *Reconciler {
	return &Reconciler{servers: servers, prov: prov, sched: sched, hostDeadAfter: hostDeadAfter, log: log}
}

// Run reconciles on each tick until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
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

	servers, err := r.servers.ListReconcilable(ctx)
	if err != nil {
		r.log.Error("list reconcilable servers", zap.Error(err))
		return
	}

	for i := range servers {
		s := &servers[i]
		if err := r.reconcile(ctx, s); err != nil {
			r.log.Error("reconcile server", zap.String("id", s.ID), zap.Error(err))
			_ = r.servers.MarkStatus(ctx, s.ID, model.StatusError, err.Error())
		}
	}
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
	state, err := r.prov.Status(probeCtx, s)
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
		r.markLost(ctx, s, "host unreachable; reprovisioning")
		return
	}
	if state == provisioner.StateRunning {
		return
	}
	r.log.Warn("running server has no live VM; reprovisioning",
		zap.String("id", s.ID), zap.Stringp("vm_id", s.VMID), zap.String("observed", string(state)))
	r.markLost(ctx, s, "vm not found on host; reprovisioning")
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

// markLost drops a presumed-dead running server back to pending for re-provision,
// logging (not propagating) a failure since the next tick simply retries.
func (r *Reconciler) markLost(ctx context.Context, s *model.GameServer, message string) {
	if err := r.servers.MarkLost(ctx, s.ID, message); err != nil {
		r.log.Error("mark server lost", zap.String("id", s.ID), zap.Error(err))
	}
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
	inst, err := r.provisionOrStart(ctx, s, provisioned)
	if err != nil {
		return err
	}
	r.log.Info("server running",
		zap.String("id", s.ID), zap.String("vm_id", inst.VMID),
		zap.Stringp("host_id", s.HostID), zap.Bool("resumed", provisioned))
	return r.servers.MarkRunning(ctx, s.ID, inst.VMID, inst.Host, inst.Port)
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
	r.log.Info("server stopped", zap.String("id", s.ID))
	return r.servers.MarkStopped(ctx, s.ID)
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
	r.log.Info("server deleted", zap.String("id", s.ID))
	return r.servers.SoftDelete(ctx, s.ID)
}
