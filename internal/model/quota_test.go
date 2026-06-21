package model

import "testing"

func TestUserQuotaAllows(t *testing.T) {
	cases := []struct {
		name              string
		quota             UserQuota
		usage             QuotaUsage
		addCPUs, addMemMB int
		wantOK            bool
		wantReason        string
	}{
		{
			name:    "within all limits",
			quota:   UserQuota{MaxServers: 5, MaxCPUs: 16, MaxMemoryMB: 16384},
			usage:   QuotaUsage{Servers: 1, CPUs: 2, MemoryMB: 2048},
			addCPUs: 2, addMemMB: 2048,
			wantOK: true,
		},
		{
			name:    "exactly at the server cap is allowed",
			quota:   UserQuota{MaxServers: 2, MaxCPUs: 16, MaxMemoryMB: 16384},
			usage:   QuotaUsage{Servers: 1, CPUs: 2, MemoryMB: 2048},
			addCPUs: 2, addMemMB: 2048,
			wantOK: true,
		},
		{
			name:    "one over the server cap is denied",
			quota:   UserQuota{MaxServers: 2, MaxCPUs: 16, MaxMemoryMB: 16384},
			usage:   QuotaUsage{Servers: 2, CPUs: 4, MemoryMB: 4096},
			addCPUs: 2, addMemMB: 2048,
			wantOK: false, wantReason: "server count quota exceeded",
		},
		{
			name:    "cpu breach",
			quota:   UserQuota{MaxServers: 0, MaxCPUs: 4, MaxMemoryMB: 0},
			usage:   QuotaUsage{Servers: 1, CPUs: 3, MemoryMB: 2048},
			addCPUs: 2, addMemMB: 2048,
			wantOK: false, wantReason: "cpu quota exceeded",
		},
		{
			name:    "memory breach",
			quota:   UserQuota{MaxServers: 0, MaxCPUs: 0, MaxMemoryMB: 4096},
			usage:   QuotaUsage{Servers: 1, CPUs: 8, MemoryMB: 4096},
			addCPUs: 2, addMemMB: 1,
			wantOK: false, wantReason: "memory quota exceeded",
		},
		{
			name:    "unlimited everywhere always allows",
			quota:   UserQuota{MaxServers: 0, MaxCPUs: 0, MaxMemoryMB: 0},
			usage:   QuotaUsage{Servers: 1000, CPUs: 9000, MemoryMB: 9_000_000},
			addCPUs: 64, addMemMB: 65536,
			wantOK: true,
		},
		{
			name:    "server cap is checked before cpu/memory",
			quota:   UserQuota{MaxServers: 1, MaxCPUs: 1, MaxMemoryMB: 1},
			usage:   QuotaUsage{Servers: 1, CPUs: 0, MemoryMB: 0},
			addCPUs: 0, addMemMB: 0,
			wantOK: false, wantReason: "server count quota exceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := tc.quota.Allows(tc.usage, tc.addCPUs, tc.addMemMB)
			if ok != tc.wantOK {
				t.Fatalf("Allows ok = %v, want %v (reason %q)", ok, tc.wantOK, reason)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
