package repository

import (
	"context"
	"time"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BillingRepository records and reads metered server running time for
// pay-as-you-go hourly billing (P9). It is the durable side of the meter: the
// reconciler opens an interval when a server starts running and closes it when
// the server stops, is lost, or is deleted; the billing handler reads the summed
// intervals back to price a bill.
type BillingRepository struct {
	pool *pgxpool.Pool
}

// NewBillingRepository constructs a BillingRepository.
func NewBillingRepository(pool *pgxpool.Pool) *BillingRepository {
	return &BillingRepository{pool: pool}
}

// StartRunning opens a metered interval for a server, capturing its spec. It is
// idempotent: the partial unique index on open intervals means a second open for
// an already-running server is a no-op (ON CONFLICT DO NOTHING), so a reconciler
// retry never double-bills. It satisfies the reconciler's Meter seam.
func (r *BillingRepository) StartRunning(ctx context.Context, s *model.GameServer) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO billing_usage (id, server_id, owner_id, cpus, memory_mb)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (server_id) WHERE ended_at IS NULL DO NOTHING`,
		uuid.NewString(), s.ID, s.OwnerID, s.CPUs, s.MemoryMB)
	return err
}

// StopRunning closes any open interval for a server, stamping ended_at. It is
// idempotent: a server with no open interval (already stopped) updates nothing.
func (r *BillingRepository) StopRunning(ctx context.Context, serverID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE billing_usage SET ended_at = now()
		WHERE server_id = $1 AND ended_at IS NULL`,
		serverID)
	return err
}

// LedgerRow is one server's metered running time within a queried period, joined
// to the server's name for display. Seconds clamps each interval to the period
// and measures still-open intervals to now.
type LedgerRow struct {
	ServerID string
	Name     string
	CPUs     int
	MemoryMB int
	Seconds  float64
	Running  bool
}

// OwnerLedger returns per-server metered running time for an owner since
// periodStart. Each interval is clamped to the period (an interval that started
// before it counts only from periodStart) and open intervals are measured to
// now, so the result is the owner's billable usage for the period. Deleted
// servers still appear (their rows are retained), keyed by name via a left join.
func (r *BillingRepository) OwnerLedger(ctx context.Context, ownerID string, periodStart time.Time) ([]LedgerRow, error) {
	const q = `
		SELECT b.server_id,
		       COALESCE(g.name, ''),
		       b.cpus, b.memory_mb,
		       SUM(EXTRACT(EPOCH FROM (
		           COALESCE(b.ended_at, now()) - GREATEST(b.started_at, $2)
		       ))) AS seconds,
		       bool_or(b.ended_at IS NULL) AS running
		FROM billing_usage b
		LEFT JOIN game_servers g ON g.id = b.server_id
		WHERE b.owner_id = $1
		  AND (b.ended_at IS NULL OR b.ended_at >= $2)
		GROUP BY b.server_id, g.name, b.cpus, b.memory_mb
		ORDER BY seconds DESC`
	rows, err := r.pool.Query(ctx, q, ownerID, periodStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LedgerRow
	for rows.Next() {
		var lr LedgerRow
		if err := rows.Scan(&lr.ServerID, &lr.Name, &lr.CPUs, &lr.MemoryMB, &lr.Seconds, &lr.Running); err != nil {
			return nil, err
		}
		// A negative span (period starts after an interval ended, edge of the
		// clamp) contributes nothing.
		if lr.Seconds < 0 {
			lr.Seconds = 0
		}
		out = append(out, lr)
	}
	return out, rows.Err()
}
