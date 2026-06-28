package model

import "time"

// FencedVM records a VM the control plane abandoned on a host it could no longer
// reach when it rescheduled the server elsewhere (P8b). The agent on that host may
// still be running the VM (a network partition, not a true death), so the
// reconciler keeps the fence until the host is reachable again, then evicts the
// orphan — reclaiming its resources without touching the durable world the
// rescheduled, higher-generation VM now owns. Persisted (not held in memory) so
// the fence survives a control-plane restart during the partition.
type FencedVM struct {
	ServerID   string
	HostID     string
	VMID       string
	Generation int64
	CreatedAt  time.Time
}
