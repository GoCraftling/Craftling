package model

import "time"

// QuotaUnlimited is the sentinel value (0) for a quota dimension with no cap.
// An admin sets a limit to 0 to grant a user unlimited headroom on that axis;
// negative values are never stored (the API rejects them).
const QuotaUnlimited = 0

// UserQuota caps how much a single user may allocate (P9). A user without a
// stored row falls back to the system default quota; an admin may override any
// dimension per user. All three limits use QuotaUnlimited (0) to mean "no cap".
type UserQuota struct {
	UserID      string `json:"user_id"`
	MaxServers  int    `json:"max_servers"`
	MaxCPUs     int    `json:"max_cpus"`
	MaxMemoryMB int    `json:"max_memory_mb"`

	// Custom is false when this quota is the system default (no stored override),
	// true once an admin has set a per-user row. It is derived, not persisted, so
	// the API can tell the two apart.
	Custom bool `json:"custom"`

	// Timestamps are populated only for a stored (custom) quota.
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// QuotaUsage is a user's current committed allocation: the count of their live
// (non-deleted) servers and the sum of those servers' cpu and memory. A stopped
// server still counts — the quota caps what a user may own, not only what is
// currently running.
type QuotaUsage struct {
	Servers  int `json:"servers"`
	CPUs     int `json:"cpus"`
	MemoryMB int `json:"memory_mb"`
}

// Allows reports whether adding one more server with the given cpu and memory
// spec to the current usage would stay within the quota. On a breach it returns
// false and a human-readable reason naming the dimension exceeded; a dimension
// whose limit is QuotaUnlimited is never the cause. The caller has already
// counted usage excluding the prospective server.
func (q UserQuota) Allows(u QuotaUsage, addCPUs, addMemoryMB int) (bool, string) {
	if q.MaxServers != QuotaUnlimited && u.Servers+1 > q.MaxServers {
		return false, "server count quota exceeded"
	}
	if q.MaxCPUs != QuotaUnlimited && u.CPUs+addCPUs > q.MaxCPUs {
		return false, "cpu quota exceeded"
	}
	if q.MaxMemoryMB != QuotaUnlimited && u.MemoryMB+addMemoryMB > q.MaxMemoryMB {
		return false, "memory quota exceeded"
	}
	return true, ""
}
