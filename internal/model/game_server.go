package model

import "time"

// Game kinds.
const (
	GameMinecraft = "minecraft"
)

// Desired states — what the user wants the server to be.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
	DesiredDeleted = "deleted"
)

// Actual statuses — where the reconciler has driven the server to.
const (
	StatusPending      = "pending"
	StatusProvisioning = "provisioning"
	StatusRunning      = "running"
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusDeleting     = "deleting"
	StatusDeleted      = "deleted"
	StatusError        = "error"
	// StatusUnschedulable means the server wants to run but no host currently has
	// the spare capacity to place it. The reconciler retries on each tick.
	StatusUnschedulable = "unschedulable"
)

// GameServer is a user-owned game server backed (eventually) by a microVM.
// It separates desired state from observed status; a reconciler converges them.
type GameServer struct {
	ID       string `json:"id"`
	OwnerID  string `json:"owner_id"`
	Name     string `json:"name"`
	Game     string `json:"game"`
	Version  string `json:"version"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`

	DesiredState string `json:"desired_state"`
	Status       string `json:"status"`

	// Marketplace provenance (set only for servers launched from a template).
	// TemplateID records which template the server came from; ImageRef is the
	// authoritative OCI image the VM boots (overriding the agent's FC_IMAGE_REF
	// default); Env is the resolved environment (operator answers substituted
	// into the template manifest) the agent merges over the image's baked-in env.
	// All nil for servers created the direct way (name + version).
	TemplateID *string           `json:"template_id,omitempty"`
	ImageRef   *string           `json:"image_ref,omitempty"`
	Env        map[string]string `json:"env,omitempty"`

	// HostID is the fleet host the scheduler placed this server on (P2). It is
	// set before provisioning and cleared on stop and on delete — a stopped
	// server holds no host, so its next start re-runs placement and may land on a
	// different host. Nil until placed.
	HostID *string `json:"host_id,omitempty"`

	// Runtime details, populated once provisioned.
	VMID          *string `json:"vm_id,omitempty"`
	Host          *string `json:"host,omitempty"`
	Port          *int    `json:"port,omitempty"`
	StatusMessage *string `json:"status_message,omitempty"`

	// BackupRequested is a user-set flag asking for an on-demand world snapshot
	// (P5). The reconciler — the sole writer of compute side effects — performs
	// the snapshot via the agent, then clears the flag and stamps LastBackupAt.
	BackupRequested bool       `json:"backup_requested"`
	LastBackupAt    *time.Time `json:"last_backup_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
