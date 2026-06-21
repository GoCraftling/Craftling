-- P9 quotas / resource controls: cap how much a single user may allocate.
--
-- A user without a row here uses the system default quota (configured on the
-- control plane); a row is an admin-set per-user override. Each limit uses 0 as
-- the "unlimited" sentinel, so an admin can lift a cap on one axis without
-- raising the others. Enforced at server create against the user's current
-- usage; an admin views and sets these through the admin API.

-- +goose Up
CREATE TABLE IF NOT EXISTS user_quotas (
    user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_servers   INTEGER NOT NULL,
    max_cpus      INTEGER NOT NULL,
    max_memory_mb INTEGER NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS user_quotas;
