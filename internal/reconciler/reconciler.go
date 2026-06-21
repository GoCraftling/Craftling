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

// Reconciler periodically converges game servers toward their desired state.
type Reconciler struct {
	servers *repository.GameServerRepository
	prov    provisioner.Provisioner
	sched   *scheduler.Scheduler
	log     *zap.Logger
}

// New constructs a Reconciler.
func New(servers *repository.GameServerRepository, prov provisioner.Provisioner, sched *scheduler.Scheduler, log *zap.Logger) *Reconciler {
	return &Reconciler{servers: servers, prov: prov, sched: sched, log: log}
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
		return err
	}
	// The VM is gone, so return its reserved capacity to the host.
	if s.HostID != nil {
		_ = r.sched.Release(ctx, *s.HostID, s.CPUs, s.MemoryMB)
	}
	r.log.Info("server deleted", zap.String("id", s.ID))
	return r.servers.SoftDelete(ctx, s.ID)
}
