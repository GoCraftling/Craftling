// Package runspec is the contract between the image converter, the
// host-side Firecracker driver, and the in-VM init agent.
//
// The converter (internal/image) distills the OCI image config into a
// RunSpec. The host driver (internal/agent/firecracker) publishes that
// RunSpec into the microVM's MMDS (Firecracker's metadata service) at
// boot. The init agent (cmd/init) fetches it back from MMDS over HTTP
// at the link-local address and execs the entrypoint.
//
// Delivering the spec via MMDS rather than baking it into the rootfs
// keeps the rootfs image immutable and content-addressed: the same
// squashfs can boot with different commands, env, or working dirs by
// changing only the metadata the host pushes. MMDS is the
// Firecracker-native channel for exactly this — mutable, per-VM,
// host-controlled data the guest reads with no shared filesystem.
//
// The package is deliberately tiny and stdlib-only so the init binary —
// which ships inside every rootfs and wants to stay small — can depend
// on it without dragging in go-containerregistry or anything else the
// converter needs.
package runspec

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// InstallDir is the pipeline-owned namespace inside the rootfs. The
// converter strips any copy the user's image ships and writes its own
// init binary here. The kernel boots with init=/.craftling/init.
const InstallDir = "/.craftling"

// InitPath is where the init binary is dropped inside the rootfs.
const InitPath = InstallDir + "/init"

// MMDS addressing and the guest's link-local network. 169.254.169.254
// is Firecracker's default MMDS address (the same EC2 IMDS uses). The
// guest's own address shares the 169.254.0.0/16 link-local block so the
// MMDS address is on-link — no route needed. Host and guest both import
// these so the kernel ip= boot arg the driver emits and the interface
// the init agent configures can't drift apart.
const (
	// MMDSIPv4 is the link-local address the MMDS answers on.
	MMDSIPv4 = "169.254.169.254"

	// GuestIPv4 is the static address the guest's MMDS interface (eth0)
	// takes. GuestNetmask / GuestPrefixLen describe its /16 link-local
	// subnet; GuestGatewayIPv4 is a nominal gateway for the ip= boot arg
	// (unused for on-link MMDS traffic).
	GuestIPv4        = "169.254.169.2"
	GuestGatewayIPv4 = "169.254.169.1"
	GuestNetmask     = "255.255.0.0"
	GuestPrefixLen   = 16

	// MMDSInterface is the guest network-interface name the kernel and
	// init agent expect for MMDS.
	MMDSInterface = "eth0"

	// MMDSBaseURL is the root of the metadata service as seen by the
	// guest.
	MMDSBaseURL = "http://" + MMDSIPv4

	// MMDSRunspecPath is the MMDS path the RunSpec is published at. It
	// mirrors the nested object the host PUTs: {craftling:{runspec:…}}.
	MMDSRunspecPath = "/" + mmdsCraftlingKey + "/" + mmdsRunspecKey

	// mmdsTokenPath is the MMDS v2 session-token endpoint.
	mmdsTokenPath = "/latest/api/token"

	// mmdsTokenTTL is the requested lifetime (seconds) of an MMDS v2
	// session token. The init agent uses the token once, immediately.
	mmdsTokenTTL = "60"

	mmdsCraftlingKey = "craftling"
	mmdsRunspecKey   = "runspec"
)

// RunSpec is the subset of an OCI image's runtime configuration the
// init agent needs to launch the workload. Field semantics match the
// OCI image-spec config block.
type RunSpec struct {
	// Entrypoint is the fixed prefix of the command. May be empty.
	Entrypoint []string `json:"entrypoint,omitempty"`

	// Cmd is the default arguments appended after Entrypoint. When
	// Entrypoint is empty, Cmd is the whole command.
	Cmd []string `json:"cmd,omitempty"`

	// Env is the process environment as "KEY=VALUE" entries, copied
	// verbatim from the image config.
	Env []string `json:"env,omitempty"`

	// WorkingDir is the directory the init agent chdirs into before
	// exec. Empty means "/".
	WorkingDir string `json:"working_dir,omitempty"`

	// Net, when non-nil, is the per-VM networking the host's eBPF NAT
	// dataplane expects the guest to apply on top of the link-local MMDS
	// address: a private address, a default route through the shared
	// virtual gateway, and a static neighbor for it (the gateway IP is
	// never owned by a host interface, so nothing would answer an ARP).
	// Absent when the host runs MMDS-only (no NAT dataplane).
	Net *NetConfig `json:"net,omitempty"`

	// Persist, when non-nil, tells the guest init to make WorkingDir
	// durable: the host has attached a writable data disk (the world
	// disk), and the guest overlays it onto WorkingDir so the read-only
	// image content shows through while every runtime write lands on the
	// disk. Absent when the host runs without world persistence (P5).
	Persist *PersistConfig `json:"persist,omitempty"`

	// Quiesce, when non-nil, enables the in-VM snapshot control server
	// (P5c): the guest listens on vsock and, on the host's request, flushes
	// the workload (RCON) and freezes the world disk filesystem so the host
	// can take an application-consistent snapshot of a *running* server.
	// Absent when the host can't take live snapshots.
	Quiesce *QuiesceConfig `json:"quiesce,omitempty"`
}

// NetConfig is the guest-applied side of the NAT dataplane addressing. All
// addresses are dotted-quad IPv4 and the MAC is colon-separated; the in-VM
// init agent (cmd/init) assigns the address to the data interface, adds a
// default route via Gateway, and installs Gateway -> GatewayMAC as a permanent
// ARP entry so the guest never emits an (unanswered) ARP request for it.
type NetConfig struct {
	// Interface is the guest NIC to configure. Empty means MMDSInterface
	// (the data NIC and the MMDS NIC are the same TAP).
	Interface string `json:"interface,omitempty"`
	// Address is the guest's private IPv4 (e.g. "10.222.0.5").
	Address string `json:"address"`
	// PrefixLen is the address prefix length (e.g. 16).
	PrefixLen int `json:"prefix_len"`
	// Gateway is the shared virtual gateway the default route points at.
	Gateway string `json:"gateway"`
	// GatewayMAC is the MAC installed as Gateway's permanent neighbor.
	GatewayMAC string `json:"gateway_mac"`
	// Nameservers are the DNS resolvers written into the guest's
	// /etc/resolv.conf so the workload can resolve names over the NAT
	// dataplane (the read-only image ships no usable resolv.conf, and nothing
	// else populates it). Empty means the guest keeps the image's resolv.conf.
	Nameservers []string `json:"nameservers,omitempty"`
}

// PersistConfig is the guest-applied side of world persistence (P5). The
// host attaches a per-server writable ext4 data disk as Device and the
// in-VM init (cmd/init) overlays it onto Mountpoint: it mounts Device,
// then mount -t overlay with lowerdir=Mountpoint (the read-only image
// dir), upperdir/workdir on the disk, back onto Mountpoint. The result
// is that the workload sees the image's baked content but every write —
// the Minecraft world, logs, edited configs — is captured on the disk,
// which is the unit a backup snapshots.
type PersistConfig struct {
	// Device is the guest block device the host attached the world disk
	// as (e.g. "/dev/vdb").
	Device string `json:"device"`
	// Mountpoint is the directory the overlay is mounted at — the
	// workload's WorkingDir. Must be an absolute, non-root path so the
	// overlay has a real lowerdir to union over.
	Mountpoint string `json:"mountpoint"`
}

// QuiesceConfig is the guest side of application-consistent snapshots (P5c).
// The guest init runs a control server on VsockControlPort; when the host asks
// it to PREPARE, it flushes the workload through RCON (if configured) and then
// fsfreezes the world-disk filesystem, so the host snapshots a quiescent disk;
// RESUME thaws and re-enables saves. RCON fields are optional — without them
// the guest freezes only (filesystem-consistent, but in-memory unsaved state is
// not captured).
type QuiesceConfig struct {
	// RCONAddress is the in-VM address of the workload's RCON endpoint
	// (e.g. "127.0.0.1:25575"). Empty skips the application flush.
	RCONAddress string `json:"rcon_address,omitempty"`
}

// VsockControlPort is the guest AF_VSOCK port the snapshot control server
// listens on, and the port the host connects to through the VM's vsock UDS. It
// is shared here so host and guest can't drift.
const VsockControlPort = 1024

// Snapshot control protocol (P5c), spoken line-by-line over the vsock
// connection. The host sends a command; the guest replies OK or "ERR <msg>".
const (
	// SnapPrepare asks the guest to flush + freeze for a snapshot.
	SnapPrepare = "PREPARE"
	// SnapResume asks the guest to thaw + re-enable saves.
	SnapResume = "RESUME"
	// SnapOK is the guest's success reply.
	SnapOK = "OK"
	// SnapErrPrefix prefixes the guest's failure reply ("ERR <message>").
	SnapErrPrefix = "ERR "
	// HealthProbe asks the guest to probe the workload's deep health (P7) and
	// reply "OK <json>" with a marshalled Health, or "ERR <message>". The guest
	// proxies the probe over its loopback — Server List Ping and, when RCON is
	// configured, an authenticated "list" — so the host never needs network reach
	// into the VM.
	HealthProbe = "HEALTH"
	// WhitelistApply asks the guest to reconcile the workload's whitelist to a
	// desired set of usernames, sent as a JSON array after the verb:
	// "WHITELIST [\"alice\",\"bob\"]". The guest diffs it against the live
	// whitelist (read over RCON) and applies the minimal add/remove plus an
	// on/off toggle, replying "OK" or "ERR <message>". It needs RCON configured;
	// without it the guest answers ERR.
	WhitelistApply = "WHITELIST"
)

// Health is the deep-health of the workload inside a running VM (P7): proof the
// game process itself is up and answering, not merely that the VM is alive. The
// guest init produces it (proxying a probe over loopback) and the host carries it
// back through the agent's VM Status. It is the wire contract between the two, so
// it lives in this shared package.
type Health struct {
	// Reachable is true when at least one probe got a valid answer from the
	// workload — the single most important signal (the process is serving).
	Reachable bool `json:"reachable"`
	// Source records which probe produced the counts: "ping" (Server List Ping)
	// or "rcon" (authenticated "list", preferred when available).
	Source string `json:"source,omitempty"`
	// PlayersOnline / PlayersMax are the current and maximum player slots, valid
	// only when Reachable.
	PlayersOnline int `json:"players_online"`
	PlayersMax    int `json:"players_max"`
	// Version is the server version string the ping reported (e.g. "1.20.1"),
	// best-effort.
	Version string `json:"version,omitempty"`
}

// Argv returns the full command line (Entrypoint followed by Cmd).
// Returns nil when both are empty — the init agent treats that as
// "nothing to run".
func (s *RunSpec) Argv() []string {
	argv := make([]string, 0, len(s.Entrypoint)+len(s.Cmd))
	argv = append(argv, s.Entrypoint...)
	argv = append(argv, s.Cmd...)
	if len(argv) == 0 {
		return nil
	}
	return argv
}

// MMDSData builds the object the host PUTs into MMDS. The RunSpec is
// embedded as a single JSON-string leaf rather than as a nested object
// so the guest never depends on MMDS's handling of typed leaves or
// arrays — it fetches one string and unmarshals it. The shape is
// {"craftling": {"runspec": "<json>"}}, fetched by the guest at
// MMDSRunspecPath.
func (s *RunSpec) MMDSData() (any, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal run spec: %w", err)
	}
	return map[string]any{
		mmdsCraftlingKey: map[string]any{
			mmdsRunspecKey: string(raw),
		},
	}, nil
}

// FetchFromMMDS retrieves and parses the RunSpec from the guest's MMDS
// endpoint. It negotiates an MMDS v2 session token first and falls back
// to token-less v1 access if the host configured MMDS in v1 mode (the
// token endpoint then answers 404/405). client supplies the timeout
// and transport; ctx bounds the whole exchange.
func FetchFromMMDS(ctx context.Context, client *http.Client) (*RunSpec, error) {
	return fetchFromMMDS(ctx, client, MMDSBaseURL)
}

// fetchFromMMDS is FetchFromMMDS with the base URL injected, so tests
// can point it at an httptest server.
func fetchFromMMDS(ctx context.Context, client *http.Client, baseURL string) (*RunSpec, error) {
	token, err := mmdsToken(ctx, client, baseURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+MMDSRunspecPath, nil)
	if err != nil {
		return nil, fmt.Errorf("build mmds request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("X-metadata-token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get mmds %s: %w", MMDSRunspecPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read mmds body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mmds %s: status %d: %s", MMDSRunspecPath, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// The leaf is a JSON string whose contents are the RunSpec JSON.
	var inner string
	if err := json.Unmarshal(body, &inner); err != nil {
		return nil, fmt.Errorf("decode mmds leaf: %w", err)
	}
	var spec RunSpec
	if err := json.Unmarshal([]byte(inner), &spec); err != nil {
		return nil, fmt.Errorf("parse run spec from mmds: %w", err)
	}
	return &spec, nil
}

// mmdsToken requests an MMDS v2 session token. An empty token with a
// nil error means the host runs MMDS v1 (no token required); any other
// failure is returned.
func mmdsToken(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+mmdsTokenPath, nil)
	if err != nil {
		return "", fmt.Errorf("build mmds token request: %w", err)
	}
	req.Header.Set("X-metadata-token-ttl-seconds", mmdsTokenTTL)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request mmds token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", fmt.Errorf("read mmds token: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return strings.TrimSpace(string(body)), nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		// MMDS v1: tokens are not used.
		return "", nil
	default:
		return "", fmt.Errorf("mmds token: status %d", resp.StatusCode)
	}
}
