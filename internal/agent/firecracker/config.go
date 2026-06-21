// Package firecracker is the real agent Runtime (P4): it boots each game server
// as a Firecracker microVM instead of simulating it like agent.FakeRuntime.
//
// It drives Firecracker through the in-repo generated REST client
// (internal/firecracker), spoken over the per-VM API Unix socket, and manages
// the Firecracker process lifecycle directly. When WorldPersistence is enabled
// (P5a) a runspec VM also gets a per-server writable world disk attached as a
// second drive, which the in-VM init overlays onto WorkingDir so the world
// survives a stop/start; backup-to-store and cross-host reschedule build on
// that in P5b/P5c.
//
// This package only runs on a Linux host with /dev/kvm; its integration test is
// gated behind the `kvm` build tag and kept out of the default CI lane.
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/aarani/craftling-go/internal/runspec"
	"github.com/aarani/craftling-go/internal/storage"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"go.uber.org/zap"
)

// ImageEnsurer resolves an OCI ref (pinned to a platform) to a
// content-addressed read-only squashfs rootfs path and the RunSpec distilled
// from the image's OCI config, building the rootfs on first use and reusing the
// cached artifact thereafter. *image.Store is the production implementation; the
// seam exists so the non-KVM tests can substitute a fake and assert what context
// the (long, idempotent) build runs under.
type ImageEnsurer interface {
	Ensure(ctx context.Context, ref string, platform *v1.Platform) (string, runspec.RunSpec, error)
}

// Config configures the Firecracker Runtime. Paths point at host artifacts
// (kernel, the squashfs image cache) provided out of band on the agent host.
type Config struct {
	// BinaryPath is the firecracker executable. Defaults to "firecracker" on PATH.
	BinaryPath string
	// KernelPath is the uncompressed kernel (vmlinux) all VMs boot.
	KernelPath string
	// ImageStore converts OCI images into content-addressed read-only squashfs
	// rootfs files and resolves their RunSpec. Provision pulls the spec's image
	// through it, attaches the resulting squashfs as a read-only /dev/vda, and
	// publishes the RunSpec into MMDS for the in-VM init. Required.
	// *image.Store is the production implementation; the interface seam lets the
	// non-KVM tests substitute a fake.
	ImageStore ImageEnsurer
	// ImagePullTimeout caps a single image build (pull + flatten + squashfs).
	// The build is deliberately decoupled from the per-command context — it is
	// content-addressed, idempotent, and shared across every VM booting the ref,
	// so it must not be aborted because a control-plane command's context was
	// cancelled (a stream reconnect, a caller deadline). Anchored to the
	// runtime's lifetime instead, this is the only deadline bounding a stuck
	// pull. Default DefaultImagePullTimeout.
	ImagePullTimeout time.Duration
	// ImageRef is the OCI image reference the driver converts for a server. A
	// "{version}" placeholder is substituted with the spec's version, so one
	// template (e.g. "myrepo/minecraft:{version}") covers every version. A ref
	// with no placeholder is used verbatim regardless of version.
	ImageRef string
	// DefaultImageRef is the OCI reference used when a spec carries no version
	// and ImageRef needs one (placeholder present). Empty rejects a
	// version-less spec against a templated ImageRef.
	DefaultImageRef string
	// WorkDir is where per-VM working directories (sockets, logs, vsock UDS)
	// live. Defaults to a "craftling-fc" dir under the OS temp dir.
	WorkDir string
	// AdvertiseHost is the player-facing connect address VMs report. With the
	// eBPF NAT dataplane enabled (UplinkDevice set) this is the host's public
	// address; the per-server port is the IPAM-allocated host port (see vmNet).
	AdvertiseHost string
	// BootArgs overrides the kernel command line. Empty uses DefaultBootArgs.
	BootArgs string

	// CPUsTotal / MemoryMBTotal are the host's total schedulable capacity, the
	// same figures the agent advertises to the control-plane scheduler. The
	// runtime refuses to boot a VM that would push the sum of its live VMs past
	// either total, so a host never overcommits itself even if the scheduler's
	// in-memory view drifts. A zero total leaves that dimension unconstrained.
	CPUsTotal     int
	MemoryMBTotal int

	// UplinkDevice is the host NIC the NAT dataplane attaches to for egress
	// SNAT and inbound DNAT (e.g. "eth0", "ens5"). When empty the NAT dataplane
	// is disabled and VMs get MMDS-only networking as before — every other
	// dataplane field below is then ignored.
	UplinkDevice string
	// VMSubnet is the CIDR private VM addresses are drawn from. Default
	// DefaultVMSubnet.
	VMSubnet string
	// GuestDNS are the resolvers written into each guest's /etc/resolv.conf so
	// the workload can resolve names over the egress NAT (the read-only image
	// ships no usable resolv.conf). Reached through the dataplane, so they must
	// be routable from the VM subnet — public resolvers by default. Only used
	// when the NAT dataplane is enabled. Empty defaults to DefaultGuestDNS.
	GuestDNS []string
	// GatewayIP is the shared virtual gateway address VMs route through. It is
	// never assigned to a host interface (the dataplane redirects, it does not
	// route). Must fall inside VMSubnet. Empty defaults to the first usable host.
	GatewayIP string
	// GatewayMAC is the MAC the guest installs as a static neighbor for
	// GatewayIP. Default DefaultGatewayMAC.
	GatewayMAC string
	// HostPortMin/HostPortMax bound the public host-port pool DNAT'd to in-VM
	// services. Defaults DefaultHostPortMin/Max.
	HostPortMin uint16
	HostPortMax uint16

	// WorldPersistence enables the per-server writable world disk + guest
	// overlay (P5a). It applies only to runspec/init VMs — the legacy ext4
	// image is already a fully writable per-VM copy. When false (the
	// default) VMs boot with no data disk and a write to WorkingDir lives
	// in tmpfs (lost on stop). Enabling it requires mkfs.ext4 on the host
	// and CONFIG_OVERLAY_FS + CONFIG_EXT4_FS in the guest kernel.
	WorldPersistence bool
	// DataDir is where per-server world disks live, deliberately separate
	// from the per-VM WorkDir (which Deprovision wipes) so a world can
	// survive stop/start and outlive a single VM instance. Defaults to a
	// "worlds" dir under WorkDir. Only used when WorldPersistence is set.
	DataDir string
	// WorldDiskMB is the size of a freshly created world disk. The ext4 is
	// created sparse, so this is a ceiling, not an upfront allocation.
	// Default DefaultWorldDiskMB.
	WorldDiskMB int
	// MkfsExt4Path is the mkfs.ext4 executable used to format a new world
	// disk. Defaults to "mkfs.ext4" resolved on PATH.
	MkfsExt4Path string
	// WorldStore, when non-nil, is the durable off-host home of world
	// snapshots (P5b): Provision restores a server's world from it before
	// boot, Stop snapshots the disk into it, and Deprovision deletes it.
	// Nil keeps worlds local-only (they survive stop/start on this host but
	// not delete or reschedule). Only consulted when WorldPersistence is set.
	WorldStore storage.WorldStore

	// SnapshotInterval, when > 0, turns on periodic application-consistent
	// snapshots (P5c) of every running VM, bounding crash data-loss to one
	// interval. Requires a WorldStore. 0 disables the periodic sweep (a live
	// snapshot can still be taken on demand).
	SnapshotInterval time.Duration
	// HealthInterval is how often the agent probes each running VM's deep health
	// (P7) over the vsock control channel and caches it for Status. Probing rides
	// the same channel as live snapshots, so it applies only to persistence+store
	// hosts. <= 0 defaults to DefaultHealthInterval; set explicitly to tune.
	HealthInterval time.Duration
	// RCONPort is the in-VM RCON port the guest flushes through before a live
	// snapshot. Default DefaultRCONPort.
	RCONPort int
	// RCONPassword authenticates to the in-VM RCON. Empty means snapshots
	// freeze the disk without an application flush (filesystem-consistent
	// only). Shared across the agent's servers for now.
	RCONPassword string
	// Logger is used for best-effort background work (the periodic snapshot
	// sweep). Nil is replaced with a no-op logger.
	Logger *zap.Logger
}

// Dataplane defaults. The VM subnet is a private RFC1918 block unlikely to
// clash with host or upstream networks; the gateway MAC is locally-administered
// (0x02 high byte) so it never collides with a real NIC.
const (
	DefaultVMSubnet    = "10.222.0.0/16"
	DefaultGatewayMAC  = "02:00:00:00:00:01"
	DefaultHostPortMin = 30000
	DefaultHostPortMax = 40000
)

// DefaultGuestDNS are the resolvers written into a guest's /etc/resolv.conf when
// GuestDNS is unset: Cloudflare and Google public DNS, reached over the egress
// NAT. Both are anycast IPv4, so they work from any VM subnet.
var DefaultGuestDNS = []string{"1.1.1.1", "8.8.8.8"}

// World-persistence defaults (P5a).
const (
	// DefaultWorldDiskMB is the size of a freshly created world disk. It is
	// created sparse, so this caps growth rather than reserving the space.
	DefaultWorldDiskMB = 4096
	// defaultMkfsExt4 is the mkfs.ext4 executable resolved on PATH when
	// MkfsExt4Path is unset.
	defaultMkfsExt4 = "mkfs.ext4"
	// worldDriveID is the Firecracker drive id of the per-server world disk.
	worldDriveID = "world"
	// worldDevice is the guest block device the world disk surfaces as. The
	// root squashfs is /dev/vda; the world disk, attached second, is /dev/vdb.
	worldDevice = "/dev/vdb"
	// DefaultRCONPort is the in-VM RCON port flushed before a live snapshot.
	DefaultRCONPort = 25575
)

// DefaultHealthInterval is how often each running VM's deep health (P7) is
// probed when HealthInterval is unset. Frequent enough that the player count and
// liveness in the UI feel live, loose enough that a fleet of VMs isn't
// continuously probed.
const DefaultHealthInterval = 15 * time.Second

// DefaultImagePullTimeout bounds a single image build (pull + flatten +
// squashfs) when ImagePullTimeout is unset. Loose on purpose: a cold pull of a
// large game-server image over a slow link can run into minutes, and the build
// is shared, so over-tight is worse than over-loose.
const DefaultImagePullTimeout = 10 * time.Minute

// DefaultBootArgs is a minimal serial-console boot line that mounts the
// read-only squashfs rootfs off the first virtio block device and hands PID 1
// to the injected init agent. Writable state rides the world disk (/dev/vdb)
// and tmpfs, never the rootfs.
const DefaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=" + runspec.InitPath

// versionPlaceholder is the token in ImageRef replaced with a spec's version.
const versionPlaceholder = "{version}"

// defaultMinecraftPort is the in-VM Minecraft server port. Per-server host port
// allocation arrives in P6; until then every VM uses the standard port.
const defaultMinecraftPort = 25565

// validate fills defaults and checks that the required host artifacts exist.
func (c *Config) validate() error {
	if c.BinaryPath == "" {
		c.BinaryPath = "firecracker"
	}
	if c.WorkDir == "" {
		c.WorkDir = filepath.Join(os.TempDir(), "craftling-fc")
	}
	if c.AdvertiseHost == "" {
		c.AdvertiseHost = "127.0.0.1"
	}
	if c.BootArgs == "" {
		c.BootArgs = DefaultBootArgs
	}
	if c.KernelPath == "" {
		return errors.New("firecracker: KernelPath is required")
	}
	if _, err := os.Stat(c.KernelPath); err != nil {
		return fmt.Errorf("firecracker: kernel image: %w", err)
	}
	if c.ImageStore == nil {
		return errors.New("firecracker: ImageStore is required")
	}
	if c.ImagePullTimeout <= 0 {
		c.ImagePullTimeout = DefaultImagePullTimeout
	}
	if c.ImageRef == "" && c.DefaultImageRef == "" {
		return errors.New("firecracker: ImageRef or DefaultImageRef is required")
	}
	if c.natEnabled() {
		if err := c.validateDataplane(); err != nil {
			return err
		}
	}
	if c.persistEnabled() {
		if err := c.validatePersistence(); err != nil {
			return err
		}
	}
	return nil
}

// natEnabled reports whether the eBPF NAT dataplane should be wired up. It is
// gated on UplinkDevice so a host without a configured uplink keeps the legacy
// MMDS-only behaviour.
func (c *Config) natEnabled() bool { return c.UplinkDevice != "" }

// persistEnabled reports whether per-server world disks should be created and
// overlaid (P5a). Off by default so MMDS-only hosts are unchanged.
func (c *Config) persistEnabled() bool { return c.WorldPersistence }

// liveSnapshotEnabled reports whether VMs should get a vsock control device and
// a Quiesce runspec so the host can snapshot them while running (P5c). It needs
// both a world disk (to freeze) and a store (to snapshot into).
func (c *Config) liveSnapshotEnabled() bool {
	return c.persistEnabled() && c.WorldStore != nil
}

// validatePersistence fills world-disk defaults and checks that mkfs.ext4 is
// available, so a misconfigured host fails fast at startup rather than at the
// first Provision. It is only called when persistEnabled.
func (c *Config) validatePersistence() error {
	if c.DataDir == "" {
		c.DataDir = filepath.Join(c.WorkDir, "worlds")
	}
	if c.WorldDiskMB <= 0 {
		c.WorldDiskMB = DefaultWorldDiskMB
	}
	if c.MkfsExt4Path == "" {
		c.MkfsExt4Path = defaultMkfsExt4
	}
	if c.RCONPort == 0 {
		c.RCONPort = DefaultRCONPort
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = DefaultHealthInterval
	}
	if c.Logger == nil {
		c.Logger = zap.NewNop()
	}
	if err := resolveExecutable(c.MkfsExt4Path); err != nil {
		return fmt.Errorf("firecracker: world persistence needs mkfs.ext4: %w", err)
	}
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("firecracker: world data dir: %w", err)
	}
	return nil
}

// resolveExecutable verifies a configured tool is runnable: an explicit path
// (containing a separator) must exist, a bare name must resolve on PATH.
func resolveExecutable(name string) error {
	if strings.ContainsRune(name, os.PathSeparator) {
		if _, err := os.Stat(name); err != nil {
			return err
		}
		return nil
	}
	if _, err := exec.LookPath(name); err != nil {
		return err
	}
	return nil
}

// validateDataplane fills dataplane defaults and checks the addressing is
// self-consistent (subnet parses, gateway sits inside it, port range is sane).
// It is only called when natEnabled.
func (c *Config) validateDataplane() error {
	if c.VMSubnet == "" {
		c.VMSubnet = DefaultVMSubnet
	}
	if c.GatewayMAC == "" {
		c.GatewayMAC = DefaultGatewayMAC
	}
	if c.HostPortMin == 0 {
		c.HostPortMin = DefaultHostPortMin
	}
	if c.HostPortMax == 0 {
		c.HostPortMax = DefaultHostPortMax
	}
	if len(c.GuestDNS) == 0 {
		c.GuestDNS = DefaultGuestDNS
	}
	// dataplaneConfig does the cross-field validation (parse + containment).
	_, err := c.dataplaneConfig()
	return err
}

// dataplaneConfig parses the addressing fields into a ready-to-use form,
// defaulting GatewayIP to the first usable host of VMSubnet when unset.
func (c *Config) dataplaneConfig() (dataplaneConfig, error) {
	_, subnet, err := net.ParseCIDR(c.VMSubnet)
	if err != nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: VMSubnet %q: %w", c.VMSubnet, err)
	}
	gwMAC, err := net.ParseMAC(c.GatewayMAC)
	if err != nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayMAC %q: %w", c.GatewayMAC, err)
	}
	if len(gwMAC) != 6 {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayMAC %q is not 48-bit", c.GatewayMAC)
	}

	gwIP := net.ParseIP(c.GatewayIP).To4()
	if c.GatewayIP == "" {
		// First usable host = network address + 1.
		host := append(net.IP(nil), subnet.IP.To4()...)
		host[3]++
		gwIP = host
	}
	if gwIP == nil {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayIP %q is not IPv4", c.GatewayIP)
	}
	if !subnet.Contains(gwIP) {
		return dataplaneConfig{}, fmt.Errorf("firecracker: GatewayIP %s not in VMSubnet %s", gwIP, subnet)
	}
	if c.HostPortMin == 0 || c.HostPortMax < c.HostPortMin {
		return dataplaneConfig{}, fmt.Errorf("firecracker: invalid host-port range %d-%d", c.HostPortMin, c.HostPortMax)
	}
	return dataplaneConfig{
		uplink:     c.UplinkDevice,
		subnet:     subnet,
		gatewayIP:  gwIP,
		gatewayMAC: gwMAC,
		portMin:    c.HostPortMin,
		portMax:    c.HostPortMax,
	}, nil
}

// dataplaneConfig is the validated, parsed form of the NAT addressing knobs.
type dataplaneConfig struct {
	uplink     string
	subnet     *net.IPNet
	gatewayIP  net.IP
	gatewayMAC net.HardwareAddr
	portMin    uint16
	portMax    uint16
}

// provisionRef chooses the OCI image to boot for a provision: a marketplace
// template's authoritative image (imageRef, resolved by the control plane) when
// set, otherwise the host's configured, version-templated default. Keeping the
// choice here makes it unit-testable without a microVM.
func (c *Config) provisionRef(imageRef, version string) (string, error) {
	if imageRef != "" {
		return imageRef, nil
	}
	return c.imageRefFor(version)
}

// imageRefFor resolves the OCI image reference the driver converts for a
// server. A templated ImageRef (one containing "{version}") is filled in from
// the spec's version, falling back to DefaultImageRef when the spec carries no
// version. A non-templated ImageRef is used verbatim regardless of version.
// Returns an error when nothing can be resolved, so a misconfigured spec fails
// the provision rather than booting the wrong image.
func (c *Config) imageRefFor(version string) (string, error) {
	if c.ImageRef != "" {
		if strings.Contains(c.ImageRef, versionPlaceholder) {
			if version == "" {
				if c.DefaultImageRef != "" {
					return c.DefaultImageRef, nil
				}
				return "", fmt.Errorf("firecracker: image ref %q needs a version and no DefaultImageRef is set", c.ImageRef)
			}
			return strings.ReplaceAll(c.ImageRef, versionPlaceholder, version), nil
		}
		return c.ImageRef, nil
	}
	if c.DefaultImageRef != "" {
		return c.DefaultImageRef, nil
	}
	return "", fmt.Errorf("firecracker: no image ref configured for version %q", version)
}

// hostPlatform is the OCI platform the driver pulls images for: linux on the
// host's architecture, since a Firecracker guest runs the host's ISA.
func hostPlatform() *v1.Platform {
	return &v1.Platform{OS: "linux", Architecture: goruntime.GOARCH}
}
