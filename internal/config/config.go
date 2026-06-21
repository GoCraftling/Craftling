package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Process modes. The control plane runs as ModeServer; host workers will run
// as ModeAgent once the binary is split (P3). Today only the server exists.
const (
	ModeServer = "server"
	ModeAgent  = "agent"
)

// Config holds runtime configuration sourced from environment variables.
type Config struct {
	// Mode selects which role this process runs as: "server" (control plane)
	// or "agent" (host worker).
	Mode        string
	Port        string
	Env         string
	DatabaseURL string
	JWTSecret   string
	AccessTTL   time.Duration
	RefreshTTL  time.Duration

	// GRPCPort is the control plane's gRPC AgentLink listener (ModeServer). It
	// is separate from the HTTP API on Port: agents dial it and hold a stream
	// open for the control plane to push VM commands down.
	GRPCPort string

	// TemplateIndexURL is the registry/marketplace index the control plane fetches
	// the list of game-server templates from.
	TemplateIndexURL string

	// Optional admin bootstrap; when both are set, the admin is seeded on startup.
	AdminEmail    string
	AdminPassword string

	// Agent configuration (ModeAgent only). The host worker dials the control
	// plane and holds a stream open over which the control plane pushes VM
	// commands; the agent never exposes an inbound API.
	Agent AgentConfig
}

// Agent runtime kinds: "fake" simulates VMs in memory (default, no KVM needed);
// "firecracker" boots real microVMs and requires /dev/kvm.
const (
	RuntimeFake        = "fake"
	RuntimeFirecracker = "firecracker"
)

// AgentConfig holds the host-worker settings used when Mode == ModeAgent.
type AgentConfig struct {
	// ControlPlaneGRPCAddr is the control plane's gRPC AgentLink address
	// (host:port) the agent dials and keeps a stream open to.
	ControlPlaneGRPCAddr string
	// Runtime selects the VM backend: "fake" (default) or "firecracker".
	Runtime string
	// Firecracker holds the real-microVM driver settings (Runtime == "firecracker").
	Firecracker FirecrackerConfig
	// ID is this agent's stable, self-owned host id (kept across CP restarts).
	ID string
	// Hostname identifies the host in the fleet view.
	Hostname string
	// AdvertiseHost is the player-facing connect address VMs report.
	AdvertiseHost string
	// Zone is an optional placement/locality label.
	Zone string
	// Version is reported to the control plane on register.
	Version string
	// CPUsTotal / MemoryMBTotal advertise this host's capacity to the scheduler.
	CPUsTotal     int
	MemoryMBTotal int
}

// FirecrackerConfig holds the paths the Firecracker driver needs (P4). It mirrors
// firecracker.Config; cmd/agent maps it across to avoid a config→driver import.
type FirecrackerConfig struct {
	// BinaryPath is the firecracker executable (empty: look up on PATH).
	BinaryPath string
	// KernelPath is the uncompressed kernel (vmlinux) all VMs boot.
	KernelPath string
	// ImageRef is the OCI image reference the driver converts to a squashfs
	// rootfs. A "{version}" placeholder is substituted with the server's
	// version (e.g. "myrepo/minecraft:{version}").
	ImageRef string
	// DefaultImageRef is the OCI reference used when a server carries no
	// version and ImageRef is templated.
	DefaultImageRef string
	// CacheDir is where converted content-addressed squashfs rootfs files are
	// cached (empty: "images" under WorkDir).
	CacheDir string
	// InitBinAmd64 / InitBinArm64 are the host paths of the prebuilt cmd/init
	// binaries injected as PID 1 into the rootfs, one per guest architecture.
	InitBinAmd64 string
	InitBinArm64 string
	// WorkDir is where per-VM working dirs live (empty: OS temp dir).
	WorkDir string

	// UplinkDevice is the host NIC the eBPF NAT dataplane attaches to (e.g.
	// "eth0", "ens5"). Setting it turns on the dataplane and, with it, the
	// per-server public host-port pool — without it every VM falls back to the
	// standard in-VM port, so multiple servers on one host all report 25565.
	// Empty keeps the legacy MMDS-only networking and the other NAT fields below
	// are ignored.
	UplinkDevice string
	// VMSubnet is the CIDR private VM addresses are drawn from (driver default
	// when empty). Only used when UplinkDevice is set.
	VMSubnet string
	// GatewayIP is the shared virtual gateway VMs route through; must fall inside
	// VMSubnet. Empty defaults to the subnet's first usable host.
	GatewayIP string
	// GuestDNS are the resolvers written into each guest's /etc/resolv.conf so
	// workloads can resolve names over the egress NAT. Driver default when empty.
	// Only used when UplinkDevice is set.
	GuestDNS []string
	// HostPortMin / HostPortMax bound the public host-port pool DNAT'd to in-VM
	// services (driver defaults when zero). Only used when UplinkDevice is set.
	HostPortMin int
	HostPortMax int

	// WorldPersistence enables the per-server world disk + guest overlay
	// (P5a). Requires mkfs.ext4 on the host and a guest kernel with
	// CONFIG_OVERLAY_FS + CONFIG_EXT4_FS.
	WorldPersistence bool
	// DataDir is where per-server world disks live (empty: "worlds" under
	// WorkDir). Only used when WorldPersistence is set.
	DataDir string
	// WorldDiskMB is the size of a freshly created world disk (0: driver default).
	WorldDiskMB int
	// MkfsExt4Path is the mkfs.ext4 executable (empty: look up on PATH).
	MkfsExt4Path string
	// WorldStoreDir, when set, points at a directory (e.g. an NFS mount)
	// used as the durable world store (P5b): worlds are restored from and
	// snapshotted into it, so they survive a server delete or host
	// reschedule. Empty keeps worlds local-only. Ignored when an S3 endpoint
	// is configured (S3 takes precedence).
	WorldStoreDir string
	// WorldStoreS3 configures an S3-compatible durable world store (P5b),
	// taking precedence over WorldStoreDir when Endpoint is set.
	WorldStoreS3 S3StoreConfig
	// SnapshotInterval, when > 0, turns on periodic application-consistent
	// snapshots of running servers (P5c). Needs a world store.
	SnapshotInterval time.Duration
	// RCONPort / RCONPassword let the guest flush the workload via RCON
	// before freezing its disk for a live snapshot. Empty password = freeze
	// only (filesystem-consistent).
	RCONPort     int
	RCONPassword string
}

// S3StoreConfig configures an S3-compatible world store. It is a plain mirror of
// storage/s3.Config so internal/config (and the control-plane binary) need not
// import the S3 SDK; cmd/agent maps it across.
type S3StoreConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Prefix          string
}

// Load reads configuration from the environment, applying sensible defaults.
func Load() *Config {
	return &Config{
		Mode:        getEnv("MODE", ModeServer),
		Port:        getEnv("PORT", "8080"),
		Env:         getEnv("APP_ENV", "development"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/craftling?sslmode=disable"),
		JWTSecret:   getEnv("JWT_SECRET", "dev-secret-change-me"),
		AccessTTL:   getDurationEnv("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:  getDurationEnv("REFRESH_TTL", 30*24*time.Hour),
		GRPCPort:    getEnv("GRPC_PORT", "8090"),

		TemplateIndexURL: getEnv("TEMPLATE_INDEX_URL", "https://registry.craftling.io/manifest.json"),

		AdminEmail:    getEnv("ADMIN_EMAIL", ""),
		AdminPassword: getEnv("ADMIN_PASSWORD", ""),

		Agent: AgentConfig{
			ControlPlaneGRPCAddr: getEnv("CONTROL_PLANE_GRPC_ADDR", "localhost:8090"),
			Runtime:              getEnv("AGENT_RUNTIME", RuntimeFake),
			Firecracker: FirecrackerConfig{
				BinaryPath:       getEnv("FC_BINARY", ""),
				KernelPath:       getEnv("FC_KERNEL", ""),
				ImageRef:         getEnv("FC_IMAGE_REF", ""),
				DefaultImageRef:  getEnv("FC_IMAGE_REF_DEFAULT", ""),
				CacheDir:         getEnv("FC_IMAGE_CACHE_DIR", ""),
				InitBinAmd64:     getEnv("FC_INIT_BIN_AMD64", ""),
				InitBinArm64:     getEnv("FC_INIT_BIN_ARM64", ""),
				WorkDir:          getEnv("FC_WORK_DIR", ""),
				UplinkDevice:     getEnv("FC_UPLINK", ""),
				VMSubnet:         getEnv("FC_VM_SUBNET", ""),
				GatewayIP:        getEnv("FC_GATEWAY_IP", ""),
				GuestDNS:         getCSVEnv("FC_GUEST_DNS", nil),
				HostPortMin:      getIntEnv("FC_HOST_PORT_MIN", 0),
				HostPortMax:      getIntEnv("FC_HOST_PORT_MAX", 0),
				WorldPersistence: getBoolEnv("FC_WORLD_PERSIST", false),
				DataDir:          getEnv("FC_DATA_DIR", ""),
				WorldDiskMB:      getIntEnv("FC_WORLD_DISK_MB", 0),
				MkfsExt4Path:     getEnv("FC_MKFS_EXT4", ""),
				WorldStoreDir:    getEnv("FC_WORLD_STORE_DIR", ""),
				WorldStoreS3: S3StoreConfig{
					Endpoint:        getEnv("FC_WORLD_STORE_S3_ENDPOINT", ""),
					Bucket:          getEnv("FC_WORLD_STORE_S3_BUCKET", ""),
					Region:          getEnv("FC_WORLD_STORE_S3_REGION", ""),
					AccessKeyID:     getEnv("FC_WORLD_STORE_S3_ACCESS_KEY", ""),
					SecretAccessKey: getEnv("FC_WORLD_STORE_S3_SECRET_KEY", ""),
					UseSSL:          getBoolEnv("FC_WORLD_STORE_S3_USE_SSL", false),
					Prefix:          getEnv("FC_WORLD_STORE_S3_PREFIX", ""),
				},
				SnapshotInterval: getDurationEnv("FC_SNAPSHOT_INTERVAL", 0),
				RCONPort:         getIntEnv("FC_RCON_PORT", 0),
				RCONPassword:     getEnv("FC_RCON_PASSWORD", ""),
			},
			ID:            getEnv("AGENT_ID", ""),
			Hostname:      getEnv("AGENT_HOSTNAME", defaultHostname()),
			AdvertiseHost: getEnv("ADVERTISE_HOST", "127.0.0.1"),
			Zone:          getEnv("ZONE", ""),
			Version:       getEnv("AGENT_VERSION", "0.1.0"),
			CPUsTotal:     getIntEnv("CPUS_TOTAL", 4),
			MemoryMBTotal: getIntEnv("MEMORY_MB_TOTAL", 8192),
		},
	}
}

// defaultHostname returns the OS hostname, or "agent" if it cannot be read.
func defaultHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "agent"
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// getCSVEnv reads a comma-separated list, trimming whitespace and dropping
// empty fields. Returns nil (not the fallback) only when the key is unset.
func getCSVEnv(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getIntEnv(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getBoolEnv(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
