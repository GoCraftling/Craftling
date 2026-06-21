-- P9 billing: pay-as-you-go, hourly metering of running servers.
--
-- Each row is a metered interval: a server's billable clock starts when the
-- reconciler marks it running (a row with ended_at NULL) and stops when it is
-- stopped, lost, or deleted (ended_at stamped). A server accrues one open
-- interval at a time; cost is the summed running duration priced at the
-- per-vCPU-hour and per-GB-hour rates the control plane is configured with.
-- cpus/memory_mb are captured on the interval so historical cost is stable even
-- if a server's spec or the owner's row later changes. Rows are retained after a
-- server is deleted so its usage still bills.

-- +goose Up
CREATE TABLE IF NOT EXISTS billing_usage (
    id         TEXT PRIMARY KEY,
    server_id  TEXT NOT NULL,
    owner_id   TEXT NOT NULL,
    cpus       INTEGER NOT NULL,
    memory_mb  INTEGER NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_billing_usage_owner ON billing_usage (owner_id);

-- At most one open interval per server: a partial unique index makes the
-- idempotent "start running" a hard guarantee, not just a WHERE NOT EXISTS race.
CREATE UNIQUE INDEX IF NOT EXISTS idx_billing_usage_open
    ON billing_usage (server_id) WHERE ended_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS billing_usage;
