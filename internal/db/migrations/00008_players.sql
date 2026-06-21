-- Players / whitelist: a user maintains a roster of players (by username) and
-- chooses, per player, which of their own servers that player may use.
--
-- players holds the roster, unique per (owner, username); player_servers is the
-- many-to-many grant of a player onto a server. Both cascade on owner/player/
-- server hard-delete; a soft-deleted server's grants are simply filtered out on
-- read, so they neither show up nor block re-adding the server later.

-- +goose Up
CREATE TABLE IF NOT EXISTS players (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    username   TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_id, username)
);

CREATE INDEX IF NOT EXISTS idx_players_owner ON players (owner_id);

CREATE TABLE IF NOT EXISTS player_servers (
    player_id TEXT NOT NULL REFERENCES players(id) ON DELETE CASCADE,
    server_id TEXT NOT NULL REFERENCES game_servers(id) ON DELETE CASCADE,
    PRIMARY KEY (player_id, server_id)
);

CREATE INDEX IF NOT EXISTS idx_player_servers_server ON player_servers (server_id);

-- +goose Down
DROP TABLE IF EXISTS player_servers;
DROP TABLE IF EXISTS players;
