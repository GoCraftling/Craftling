package repository

import (
	"context"
	"errors"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QuotaRepository provides persistence for per-user quota overrides (P9). The
// user_quotas table holds only overrides; a user without a row falls back to the
// system default the repository is constructed with, so Get never returns
// ErrNotFound — it always yields an effective quota.
type QuotaRepository struct {
	pool  *pgxpool.Pool
	deflt model.UserQuota
}

// NewQuotaRepository constructs a QuotaRepository. deflt is the system default
// quota applied to any user without a stored override; its UserID/Custom/time
// fields are ignored (filled in per request).
func NewQuotaRepository(pool *pgxpool.Pool, deflt model.UserQuota) *QuotaRepository {
	return &QuotaRepository{pool: pool, deflt: deflt}
}

// Default returns the system default quota (no user binding), for surfacing what
// an unconfigured user would receive.
func (r *QuotaRepository) Default() model.UserQuota {
	d := r.deflt
	d.Custom = false
	return d
}

// Get returns a user's effective quota: their stored override if present
// (Custom = true), otherwise the system default stamped with the user's id
// (Custom = false). It does not verify the user exists; callers that need that
// check the user repository first.
func (r *QuotaRepository) Get(ctx context.Context, userID string) (model.UserQuota, error) {
	q := model.UserQuota{UserID: userID}
	const sql = `SELECT max_servers, max_cpus, max_memory_mb, created_at, updated_at
		FROM user_quotas WHERE user_id = $1`
	err := r.pool.QueryRow(ctx, sql, userID).Scan(
		&q.MaxServers, &q.MaxCPUs, &q.MaxMemoryMB, &q.CreatedAt, &q.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		d := r.deflt
		d.UserID = userID
		d.Custom = false
		return d, nil
	}
	if err != nil {
		return model.UserQuota{}, err
	}
	q.Custom = true
	return q, nil
}

// Set upserts a user's quota override, returning the stored row with its
// timestamps. The limits are taken as given (0 means unlimited on that axis);
// the caller validates them non-negative.
func (r *QuotaRepository) Set(ctx context.Context, q model.UserQuota) (model.UserQuota, error) {
	const sql = `
		INSERT INTO user_quotas (user_id, max_servers, max_cpus, max_memory_mb)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE
		SET max_servers = EXCLUDED.max_servers,
		    max_cpus = EXCLUDED.max_cpus,
		    max_memory_mb = EXCLUDED.max_memory_mb,
		    updated_at = now()
		RETURNING created_at, updated_at`
	out := q
	out.Custom = true
	if err := r.pool.QueryRow(ctx, sql, q.UserID, q.MaxServers, q.MaxCPUs, q.MaxMemoryMB).
		Scan(&out.CreatedAt, &out.UpdatedAt); err != nil {
		return model.UserQuota{}, err
	}
	return out, nil
}

// Delete removes a user's override, reverting them to the system default. It is
// a no-op (no error) when no override exists.
func (r *QuotaRepository) Delete(ctx context.Context, userID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM user_quotas WHERE user_id = $1`, userID)
	return err
}
