package repository

import (
	"context"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FenceRepository is the durable record of VMs abandoned on hosts the control
// plane could no longer reach when it rescheduled their servers (P8b). The
// reconciler adds a fence when it presumes a host dead and reschedules, then
// drains the table by evicting each orphan once its host is reachable again. A
// DB table — not in-memory hub state — so a fence survives a control-plane
// restart during the partition that created it.
type FenceRepository struct {
	pool *pgxpool.Pool
}

// NewFenceRepository constructs a FenceRepository.
func NewFenceRepository(pool *pgxpool.Pool) *FenceRepository {
	return &FenceRepository{pool: pool}
}

// Add records a fence for a VM abandoned on a host. Keyed by (host_id, vm_id) so a
// re-record of the same orphan is idempotent.
func (r *FenceRepository) Add(ctx context.Context, f model.FencedVM) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO fenced_vms (server_id, host_id, vm_id, generation)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (host_id, vm_id) DO NOTHING`,
		f.ServerID, f.HostID, f.VMID, f.Generation)
	return err
}

// List returns every outstanding fence, oldest first.
func (r *FenceRepository) List(ctx context.Context) ([]model.FencedVM, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT server_id, host_id, vm_id, generation, created_at
		FROM fenced_vms ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FencedVM
	for rows.Next() {
		var f model.FencedVM
		if err := rows.Scan(&f.ServerID, &f.HostID, &f.VMID, &f.Generation, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Delete removes the fence for a (host, vm) once its orphan has been evicted (or
// is known gone). Idempotent.
func (r *FenceRepository) Delete(ctx context.Context, hostID, vmID string) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM fenced_vms WHERE host_id = $1 AND vm_id = $2`, hostID, vmID)
	return err
}

// DeleteOlderThan removes fences created before cutoff. It GCs orphans on a host
// that never came back: the VM is unreachable forever and its world is long since
// superseded, so the fence is dead weight. Returns how many were removed.
func (r *FenceRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM fenced_vms WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
