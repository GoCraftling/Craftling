-- P7 observability / deep health: surface the *Minecraft process* health, not
-- just the VM state. The agent probes each running server via RCON / Server List
-- Ping and the reconciler records the result here.
--
-- players_online / players_max are the live player counts (NULL when the
-- workload isn't answering a probe); last_seen is the last time the process was
-- successfully reached — proof of life independent of the coarse `status`
-- column. All nullable and default-NULL so the add is non-disruptive and a
-- never-probed server reads as "unknown" rather than zero.

-- +goose Up
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS players_online INTEGER;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS players_max INTEGER;
ALTER TABLE game_servers ADD COLUMN IF NOT EXISTS last_seen TIMESTAMPTZ;

-- +goose Down
ALTER TABLE game_servers DROP COLUMN IF EXISTS last_seen;
ALTER TABLE game_servers DROP COLUMN IF EXISTS players_max;
ALTER TABLE game_servers DROP COLUMN IF EXISTS players_online;
