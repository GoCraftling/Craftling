package repository

import (
	"context"
	"errors"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlayerRepository persists a user's whitelist roster: players (by username) and
// the per-player grant onto the owner's servers.
type PlayerRepository struct {
	pool *pgxpool.Pool
}

// NewPlayerRepository constructs a PlayerRepository.
func NewPlayerRepository(pool *pgxpool.Pool) *PlayerRepository {
	return &PlayerRepository{pool: pool}
}

// playerSelect reads players with their granted server ids aggregated into a
// text[]. The grant join filters to live servers, so a grant onto a soft-deleted
// server neither appears nor lingers in the response; a player with no grants
// yields an empty array, not NULL.
const playerSelect = `
	SELECT p.id, p.owner_id, p.username, p.created_at, p.updated_at,
	       COALESCE(
	           array_agg(ps.server_id) FILTER (WHERE g.id IS NOT NULL),
	           '{}'
	       ) AS server_ids
	FROM players p
	LEFT JOIN player_servers ps ON ps.player_id = p.id
	LEFT JOIN game_servers g ON g.id = ps.server_id AND g.deleted_at IS NULL`

func scanPlayer(row scannable) (*model.Player, error) {
	var p model.Player
	if err := row.Scan(&p.ID, &p.OwnerID, &p.Username, &p.CreatedAt, &p.UpdatedAt, &p.ServerIDs); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListByOwner returns a user's whitelist roster, alphabetical by username.
func (r *PlayerRepository) ListByOwner(ctx context.Context, ownerID string) ([]model.Player, error) {
	rows, err := r.pool.Query(ctx, playerSelect+`
		WHERE p.owner_id = $1
		GROUP BY p.id
		ORDER BY p.username`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var players []model.Player
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		players = append(players, *p)
	}
	return players, rows.Err()
}

// GetByID returns a single player with its grants, or ErrNotFound. Ownership is
// the caller's to check.
func (r *PlayerRepository) GetByID(ctx context.Context, id string) (*model.Player, error) {
	p, err := scanPlayer(r.pool.QueryRow(ctx, playerSelect+`
		WHERE p.id = $1
		GROUP BY p.id`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// Create inserts a player and its server grants in one transaction, populating
// the player's id and timestamps. A duplicate (owner, username) surfaces as a
// Postgres unique violation for the handler to map to 409.
func (r *PlayerRepository) Create(ctx context.Context, p *model.Player, serverIDs []string) error {
	p.ID = uuid.NewString()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after a successful Commit

	if err := tx.QueryRow(ctx,
		`INSERT INTO players (id, owner_id, username) VALUES ($1, $2, $3)
		 RETURNING created_at, updated_at`,
		p.ID, p.OwnerID, p.Username,
	).Scan(&p.CreatedAt, &p.UpdatedAt); err != nil {
		return err
	}
	if err := insertGrants(ctx, tx, p.ID, serverIDs); err != nil {
		return err
	}
	p.ServerIDs = serverIDs
	return tx.Commit(ctx)
}

// Update replaces a player's username and its full set of server grants in one
// transaction (the grants are set-replaced, not merged). A duplicate username
// surfaces as a unique violation.
func (r *PlayerRepository) Update(ctx context.Context, id, username string, serverIDs []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE players SET username = $2, updated_at = now() WHERE id = $1`,
		id, username); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM player_servers WHERE player_id = $1`, id); err != nil {
		return err
	}
	if err := insertGrants(ctx, tx, id, serverIDs); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UsernamesForServer returns the usernames of every player granted onto a
// server — the server's desired whitelist. Ordered for a stable result. The
// reconciler reads it to feed the in-game whitelist over RCON.
func (r *PlayerRepository) UsernamesForServer(ctx context.Context, serverID string) ([]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT p.username
		FROM player_servers ps
		JOIN players p ON p.id = ps.player_id
		WHERE ps.server_id = $1
		ORDER BY p.username`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// Delete removes a player; its grants cascade.
func (r *PlayerRepository) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM players WHERE id = $1`, id)
	return err
}

// insertGrants writes the player→server grant rows. The ON CONFLICT guard makes
// a repeated server id in the request harmless.
func insertGrants(ctx context.Context, tx pgx.Tx, playerID string, serverIDs []string) error {
	for _, sid := range serverIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_servers (player_id, server_id) VALUES ($1, $2)
			 ON CONFLICT DO NOTHING`,
			playerID, sid); err != nil {
			return err
		}
	}
	return nil
}
