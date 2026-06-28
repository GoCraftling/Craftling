-- P8 reliability: survive reconcile and host failures.
--
-- Three additions, one migration:
--
-- 1. Retry/backoff (P8a). attempts counts consecutive failed reconciles;
--    next_attempt_at is when the server is next eligible (NULL = now). The
--    reconciler spaces retries exponentially instead of hammering a persistently
--    failing server every tick, and resets both on success or an explicit
--    desired-state change.
--
-- 2. Generation token (P8b). generation is a monotonic per-server incarnation
--    counter, bumped each time the reconciler provisions a fresh VM. It is passed
--    down to the agent and stamped onto the durable world snapshot so a
--    partitioned host's zombie VM (an older generation) cannot clobber the world
--    the rescheduled VM now owns.
--
-- 3. Fence rows (P8b). fenced_vms records a VM abandoned on a host the control
--    plane could no longer reach when it rescheduled the server. The reconciler
--    drains the table by evicting each orphan once its host is reachable again,
--    reclaiming the zombie without touching the (now reassigned) durable world.
--    A DB table — not in-memory hub state — so the fence survives a control-plane
--    restart during the partition.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS generation BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS fenced_vms (
    server_id  TEXT NOT NULL,
    host_id    TEXT NOT NULL,
    vm_id      TEXT NOT NULL,
    generation BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (host_id, vm_id)
);

-- +goose Down
DROP TABLE IF EXISTS fenced_vms;
ALTER TABLE game_servers DROP COLUMN IF EXISTS generation;
ALTER TABLE game_servers DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE game_servers DROP COLUMN IF EXISTS attempts;
