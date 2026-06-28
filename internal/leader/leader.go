// Package leader provides single-leader election across control-plane replicas
// using a Postgres session-level advisory lock (P8d).
//
// Every replica serves the HTTP API and accepts agent gRPC streams, but only one
// may run the single-writer goroutines — the reconciler and the reapers — or two
// replicas would race to drive the same VMs. A replica campaigns for leadership by
// taking a session advisory lock on a shared key over a dedicated connection it
// holds for as long as it leads; Postgres releases the lock automatically if that
// connection (and thus the replica) dies, so a crashed leader frees the role
// without a heartbeat protocol. Followers re-campaign on an interval and take over
// when the lock frees.
package leader

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// healthInterval is how often a leader pings its lock-holding connection to notice
// it has died (and so lost the lock) and step down.
const healthInterval = 5 * time.Second

// Campaign runs leader election until ctx is cancelled. While this replica holds
// the advisory lock on key it calls run with a context that is cancelled the
// moment leadership is lost (the connection died) or ctx ends; run must start the
// leader-only work and return promptly (spawning its own goroutines bound to the
// context), not block. While a follower — another replica holds the lock — it
// waits retry and tries again. retry also bounds how quickly a follower takes over
// after a leader steps down.
func Campaign(ctx context.Context, pool *pgxpool.Pool, key int64, retry time.Duration, log *zap.Logger, run func(context.Context)) {
	for ctx.Err() == nil {
		if err := campaignOnce(ctx, pool, key, log, run); err != nil && ctx.Err() == nil {
			log.Warn("leader campaign cycle ended", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// campaignOnce acquires a dedicated connection, tries the advisory lock once, and
// — if it wins — runs the leader work until leadership or ctx ends. It returns nil
// when it was merely a follower (caller waits and retries) and an error when it
// led and then lost the connection.
func campaignOnce(ctx context.Context, pool *pgxpool.Pool, key int64, log *zap.Logger, run func(context.Context)) error {
	// Hold a dedicated connection for the whole campaign: a session advisory lock
	// lives on the backend session, so returning the connection to the pool (where
	// it could be reused or closed) would drop the lock.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	var acquired bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil // another replica leads; the caller waits and re-campaigns
	}
	log.Info("acquired control-plane leadership", zap.Int64("lock_key", key))

	leaderCtx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the leader-only goroutines when we step down
	run(leaderCtx)

	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: drop the lock promptly so a peer can take over
			// without waiting for the session to time out. Best-effort on a fresh
			// context since ctx is already done.
			relCtx, relCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = conn.Exec(relCtx, "SELECT pg_advisory_unlock($1)", key)
			relCancel()
			log.Info("released control-plane leadership on shutdown")
			return nil
		case <-ticker.C:
			if err := conn.Ping(ctx); err != nil {
				log.Warn("lost control-plane leadership (lock connection unhealthy)", zap.Error(err))
				return err
			}
		}
	}
}
