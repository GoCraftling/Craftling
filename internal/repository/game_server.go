package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/aarani/craftling-go/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GameServerRepository provides persistence operations for game servers.
type GameServerRepository struct {
	pool *pgxpool.Pool
}

// NewGameServerRepository constructs a GameServerRepository.
func NewGameServerRepository(pool *pgxpool.Pool) *GameServerRepository {
	return &GameServerRepository{pool: pool}
}

const gameServerColumns = `id, owner_id, name, game, version, cpus, memory_mb,
	desired_state, status, host_id, vm_id, host, port, status_message,
	backup_requested, last_backup_at, template_id, image_ref, env,
	created_at, updated_at`

// scannable is satisfied by both pgx.Row and pgx.Rows.
type scannable interface {
	Scan(dest ...any) error
}

func scanGameServer(row scannable) (*model.GameServer, error) {
	var s model.GameServer
	// env is decoded from its jsonb column through raw bytes (NULL -> nil) rather
	// than relying on pgx's map codec to round-trip a NULL.
	var envJSON []byte
	err := row.Scan(
		&s.ID, &s.OwnerID, &s.Name, &s.Game, &s.Version, &s.CPUs, &s.MemoryMB,
		&s.DesiredState, &s.Status, &s.HostID, &s.VMID, &s.Host, &s.Port, &s.StatusMessage,
		&s.BackupRequested, &s.LastBackupAt, &s.TemplateID, &s.ImageRef, &envJSON,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if len(envJSON) > 0 {
		if err := json.Unmarshal(envJSON, &s.Env); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// Create inserts a new game server, populating its ID and timestamps. The
// template columns (template_id, image_ref, env) are written when set and left
// NULL for a direct (name + version) create.
func (r *GameServerRepository) Create(ctx context.Context, s *model.GameServer) error {
	s.ID = uuid.NewString()
	var envJSON []byte
	if len(s.Env) > 0 {
		b, err := json.Marshal(s.Env)
		if err != nil {
			return err
		}
		envJSON = b
	}
	const q = `
		INSERT INTO game_servers
			(id, owner_id, name, game, version, cpus, memory_mb, desired_state, status,
			 template_id, image_ref, env)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at, updated_at`
	return r.pool.QueryRow(ctx, q,
		s.ID, s.OwnerID, s.Name, s.Game, s.Version, s.CPUs, s.MemoryMB, s.DesiredState, s.Status,
		s.TemplateID, s.ImageRef, envJSON,
	).Scan(&s.CreatedAt, &s.UpdatedAt)
}

// GetByID returns a server by ID, or ErrNotFound.
func (r *GameServerRepository) GetByID(ctx context.Context, id string) (*model.GameServer, error) {
	s, err := scanGameServer(r.pool.QueryRow(ctx,
		`SELECT `+gameServerColumns+` FROM game_servers WHERE id = $1 AND deleted_at IS NULL`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// ListByOwner returns all live servers belonging to a user, newest first.
func (r *GameServerRepository) ListByOwner(ctx context.Context, ownerID string) ([]model.GameServer, error) {
	return r.query(ctx,
		`SELECT `+gameServerColumns+` FROM game_servers
		 WHERE owner_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC`,
		ownerID)
}

// ListAll returns every live server across all owners, newest first.
func (r *GameServerRepository) ListAll(ctx context.Context) ([]model.GameServer, error) {
	return r.query(ctx,
		`SELECT `+gameServerColumns+` FROM game_servers
		 WHERE deleted_at IS NULL ORDER BY created_at DESC`)
}

// ListReconcilable returns live servers whose observed status does not yet
// match their desired state (plus anything marked for deletion). Bounded per
// call. Soft-deleted rows are excluded.
func (r *GameServerRepository) ListReconcilable(ctx context.Context) ([]model.GameServer, error) {
	const q = `
		SELECT ` + gameServerColumns + ` FROM game_servers
		WHERE deleted_at IS NULL
		  AND (desired_state = 'deleted'
		   OR backup_requested
		   OR (desired_state = 'running' AND status <> 'running')
		   OR (desired_state = 'stopped' AND status <> 'stopped'))
		ORDER BY updated_at
		LIMIT 100`
	return r.query(ctx, q)
}

// ListActiveIDs returns the ids of every live (non-deleted) server. The world
// GC reaper uses it to find stored worlds that no live server claims.
func (r *GameServerRepository) ListActiveIDs(ctx context.Context) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT id FROM game_servers WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *GameServerRepository) query(ctx context.Context, q string, args ...any) ([]model.GameServer, error) {
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []model.GameServer
	for rows.Next() {
		s, err := scanGameServer(rows)
		if err != nil {
			return nil, err
		}
		servers = append(servers, *s)
	}
	return servers, rows.Err()
}

// UpdateSpec updates user-editable fields.
func (r *GameServerRepository) UpdateSpec(ctx context.Context, id, name, version string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET name = $2, version = $3, updated_at = now() WHERE id = $1`,
		id, name, version)
	return err
}

// SetDesiredState records what the user wants the server to be.
func (r *GameServerRepository) SetDesiredState(ctx context.Context, id, desired string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET desired_state = $2, updated_at = now() WHERE id = $1`,
		id, desired)
	return err
}

// UsedCapacity returns the total cpu and memory currently committed to a host:
// the sum over every live (non-deleted) server assigned to it. A server keeps
// its reservation while stopped (the VM stays put), so this counts all assigned
// servers regardless of status. It lets the control plane rebuild a host's
// allocatable capacity from the durable record after a restart, instead of
// resetting it to total and forgetting in-flight placements.
func (r *GameServerRepository) UsedCapacity(ctx context.Context, hostID string) (cpus, memoryMB int, err error) {
	if hostID == "" {
		return 0, 0, nil
	}
	err = r.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(cpus), 0), COALESCE(SUM(memory_mb), 0)
		FROM game_servers
		WHERE host_id = $1 AND deleted_at IS NULL`,
		hostID).Scan(&cpus, &memoryMB)
	return cpus, memoryMB, err
}

// AssignHost records the fleet host the scheduler placed a server on (P2). The
// capacity reservation itself lives in the host inventory; this persists the
// assignment so it survives a reconciler restart and is visible in the API.
func (r *GameServerRepository) AssignHost(ctx context.Context, id, hostID string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET host_id = $2, updated_at = now() WHERE id = $1`,
		id, hostID)
	return err
}

// MarkStatus sets the observed status and an optional message (empty -> NULL).
func (r *GameServerRepository) MarkStatus(ctx context.Context, id, status, message string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET status = $2, status_message = NULLIF($3, ''), updated_at = now() WHERE id = $1`,
		id, status, message)
	return err
}

// MarkRunning records a successfully provisioned, running server.
func (r *GameServerRepository) MarkRunning(ctx context.Context, id, vmID, host string, port int) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE game_servers
		SET status = 'running', vm_id = $2, host = $3, port = $4,
		    status_message = NULL, updated_at = now()
		WHERE id = $1`,
		id, vmID, host, port)
	return err
}

// MarkStopped records a stopped server with its runtime details cleared.
func (r *GameServerRepository) MarkStopped(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE game_servers
		SET status = 'stopped', vm_id = NULL, host = NULL, port = NULL,
		    status_message = NULL, updated_at = now()
		WHERE id = $1`,
		id)
	return err
}

// RequestBackup flags a server for an on-demand world snapshot. The reconciler
// picks it up and performs the snapshot (the API never touches compute).
func (r *GameServerRepository) RequestBackup(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET backup_requested = true, updated_at = now() WHERE id = $1`,
		id)
	return err
}

// MarkBackedUp clears the backup request and stamps the time, once the
// reconciler has taken (or determined it need not take) the snapshot.
func (r *GameServerRepository) MarkBackedUp(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE game_servers SET backup_requested = false, last_backup_at = now(), updated_at = now() WHERE id = $1`,
		id)
	return err
}

// SoftDelete marks a server as deleted and clears its runtime details. The row
// is retained for audit/history but hidden from all reads.
func (r *GameServerRepository) SoftDelete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE game_servers
		SET status = 'deleted', host_id = NULL, vm_id = NULL, host = NULL, port = NULL,
		    status_message = NULL, deleted_at = now(), updated_at = now()
		WHERE id = $1`,
		id)
	return err
}
