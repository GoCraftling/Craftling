# Craftling — Platform Roadmap

> **Status verified against the code on 2026-06-20.** This document was rewritten
> after a full read of the source because the prior version was stale — it listed
> P6 (networking) as "not started" and the P4 image-build pipeline as
> "deferred / out of band", both of which are in fact implemented and tested.
> Phase status below reflects what the code actually does, not what was planned.

## Context

Craftling (`craftling-go`) is a **multi-host, Firecracker-microVM Minecraft
hosting platform** with durable world storage. It is split into two roles of one
binary:

- **Control plane** (`cmd/server`, `Mode=server`): auth (JWT + rotating refresh
  tokens), roles, owner-scoped game-server CRUD, an in-memory host fleet, a
  scheduler, a desired-state/observed-status **reconciler** (the sole writer of
  compute side effects), and background reapers.
- **Host agent** (`cmd/agent`, `Mode=agent`): owns KVM. Boots each game server as
  a real Firecracker microVM, runs the eBPF NAT dataplane, and manages world
  disks/snapshots. It dials the control plane and holds one **agent-initiated
  gRPC stream** (`AgentLink`) open: it registers + heartbeats on that stream and
  the control plane pushes VM commands *down* it. The agent exposes no inbound
  API — so agents need no reachability — and the open stream is itself the host's
  liveness signal.

A third binary, **`cmd/init`**, is the in-guest PID 1 baked into every rootfs.

## Guiding principles / invariants (hold across all phases)

- **Reconciliation is the core loop** — `desired_state` vs observed `status`; the
  reconciler is the *sole writer of compute side effects*.
- **`Provisioner` is the backend seam** — compute backends slot in behind it
  (`Fake`, `RemoteProvisioner`) without touching the API.
- **The control plane never touches KVM** — only the host agent does.
- **Owner-scoped, admin-visible, no-leak** — the `ownedOr404` pattern; admins get
  fleet-wide views.
- **Soft deletes / audit retained** (`game_servers.deleted_at`).
- **Immutable, content-addressed images** — rootfs is read-only squashfs named by
  digest; per-boot config (entrypoint/env/net/persist) rides MMDS, not the image.

---

## Current state (what's actually built)

### Architecture seams (verified present)
- **Persistence:** `users`, `refresh_tokens`, `game_servers` are **Postgres-backed**
  (pgx). The **host fleet is in-memory only** (`internal/repository/host.go`,
  RWMutex map) — no `hosts` table; agents re-register with stable agent-owned IDs,
  and per-host capacity is rebuilt on (re)register from the durable
  `game_servers.host_id` rows. This is a deliberate P1 decision, not a gap.
- **Migrations:** goose, applied on startup —
  `00001_baseline`, `00002_game_servers_host_id`, `00003_game_servers_backup`.
- **API surface** (`internal/handler/router.go`): auth
  (register/login/refresh/logout/me), owner-scoped servers (CRUD +
  `POST /servers/:id/snapshot`), templates (list/get), admin
  (users/servers/hosts), `healthz`/`ping`. Agents are **not** on the HTTP API:
  they connect over the gRPC `AgentLink` stream (`internal/agentlink`, separate
  `GRPC_PORT` listener), which carries registration, heartbeats, and command
  push.
- **Reapers** (`internal/reaper`): refresh-token GC (hourly), host stale→`down`
  (10s sweep / 30s TTL), world-store GC (hourly, store-configured only).

### Phases complete ✅

**P0 — Foundations.** goose migrations (apply-on-startup, clean on fresh +
pre-existing DBs); `Provisioner` extended with `Start`/`Stop`/`Status`/`Snapshot`
(stopped ≠ destroyed); `Mode` (`server`/`agent`) in `internal/config`.

**P1 — Host fleet.** `model.Host` + in-memory `HostRepository`; agents register +
heartbeat over the gRPC `AgentLink` stream (originally HTTP `POST
/agent/hosts/register` + `/heartbeat`, later folded into the stream); admin `GET
/admin/hosts`; host reaper (stale→`down`, heartbeat recovers to `ready`) plus
immediate `MarkDown` when a stream drops. Identity is **agent-owned** so ids
survive a control-plane restart without a durable table.

**P2 — Scheduler / placement.** `internal/scheduler` — least-loaded first-fit over
the in-memory fleet with **atomic capacity reservation** (`Reserve`/`Release`
under the repo lock; lost race → next candidate). `game_servers.host_id`
(migration `00002`, nullable, **no FK** by design). Reconciler places a
`running`-desired unassigned server, marks `unschedulable` when nothing fits, and
releases capacity on delete. Create-time `CanEverFit` rejects specs no host could
ever hold.

**P3 — Agent split.** `cmd/agent` + `internal/agent` (`Runtime` interface,
`FakeRuntime`, agent-side gRPC link loop) + `internal/agentlink` (control-plane
hub: connection registry + command push, over generated protobuf).
`provisioner.RemoteProvisioner` routes each call to the assigned host by pushing
a command down that host's open stream via the hub — the reconciler's call
*shape* is unchanged, it just became a message on the stream (the control plane
never dials the agent). Per-VM observed status flows back via `Status`.

**P4 — Firecracker runtime + image pipeline.** *(Plan previously marked the image
build "deferred" — it is built.)*
- `internal/agent/firecracker`: a `Runtime` driver booting each server as a real
  microVM via the **in-repo generated REST client** (`internal/firecracker`) over
  the per-VM API Unix socket, managing the process lifecycle directly.
  Provision/Start/Stop (`SendCtrlAltDel` + force-kill, keeps the disk) /
  Deprovision / Status.
- **Image build is implemented in Go**, not shell and not out-of-band:
  - `internal/image/rootfs.go` — pulls an OCI/Docker image (go-containerregistry
    `crane`), verifies the digest, flattens layers, and **streams** the tar
    straight into a squashfs writer (no staging dir); injects the `cmd/init`
    binary at `/.craftling/init`; ensures standard mountpoints; distills the OCI
    config into a `RunSpec`. Output is a content-addressed `<algo>-<hex>.sqsh`.
    Hardened against tar-bombs (≤16 GiB / ~1M entries) and path traversal.
  - ✅ **Converter wired in (integration gap closed).** `Runtime.Provision`
    resolves the server's OCI ref (`Config.ImageRef`, a `{version}` template, or
    `DefaultImageRef`), pulls + converts it through `image.Store.Ensure`
    (content-addressed, cached by digest), attaches the resulting squashfs
    **read-only** as `/dev/vda`, and boots `cmd/init` with `root=/dev/vda ro
    init=/.craftling/init`. The RunSpec the converter distils is the one
    published into MMDS (a per-server spec can override command/env/workdir via
    `applyRunSpecOverride`), so the persist/NAT/quiesce layering now runs on the
    **production** path, not just the KVM e2e. The writable-`.ext4` catalog and
    `copyFile` rootfs staging are **retired**.
  - `internal/squashfs` — a **from-scratch squashfs 4.0 writer** (files, dirs,
    symlinks, hardlinks, device/FIFO/socket nodes; per-block gzip; UID/GID dedup;
    backpatched superblock). Deliberately omits fragment blocks, xattrs, the
    export table, and reading (the kernel/`unsquashfs` is the reader).
  - **Note:** the pipeline is a *generic* OCI→squashfs converter. Minecraft
    specifics (JRE, server jar, EULA, RCON) come from the **user-supplied base
    image** (e.g. `itzg/minecraft-server`), pulled as-is — there is no
    Minecraft-bespoke build step, by design.
- **Guest init** (`cmd/init`): mounts kernel filesystems, brings up MMDS net,
  fetches the `RunSpec` from MMDS, applies the NAT net config via a hand-rolled
  rtnetlink client (no deps), mounts the persistence overlay, starts the vsock
  snapshot-control server, then execs and supervises the workload (signal
  forwarding, orphan reaping, power-off on exit).
- **Image catalog / config:** `config.FirecrackerConfig` (`FC_*` env) + runtime
  selector (`RuntimeFake`/`RuntimeFirecracker`); rootfs resolved per-server from
  an OCI ref (`FC_IMAGE_REF`, `{version}`-templated, with an optional
  `FC_IMAGE_REF_DEFAULT`), converted to a cached squashfs under
  `FC_IMAGE_CACHE_DIR`; per-arch init binaries (`FC_INIT_BIN_AMD64/ARM64`)
  injected by the converter; shared `vmlinux` kernel.
- **Verify:** non-KVM unit tests (config/artifact validation, squashfs round-trip
  via `unsquashfs`, image resolution, idempotency edges); KVM-gated lifecycle +
  MMDS tests (`-tags kvm`, `make test-kvm`).

**P5 — World persistence (a/b/c all done).**
- **P5a** writable per-server world disk (`/dev/vdb`, ext4) created/formatted under
  a `DataDir` keyed by server id, attached as a non-root drive; the guest
  overlays it onto the workload `WorkingDir` (read-only image lower + world-disk
  upper). Gated by `FC_WORLD_PERSIST` / `FC_DATA_DIR` / `FC_WORLD_DISK_MB`.
- **P5b** `internal/storage`: `WorldStore` (`Exists`/`Put`/`Get`/`Delete`/`List`,
  `ErrWorldNotFound`) with a `DirStore` (filesystem/NFS, atomic temp+rename) **and
  an S3 backend** (`internal/storage/s3`, minio-go; AWS/MinIO/Ceph; bucket verified
  at startup). Store construction centralized in `internal/worldstore.FromConfig`.
  The driver owns the disk↔stream codec (gzip of the raw ext4). Lifecycle:
  **Provision restores** (else formats fresh), **Stop snapshots**, **Deprovision
  deletes**. Reschedule = Stop on host A → Provision on host B, store-mediated.
  `reaper.Worlds` GCs orphan snapshots no live server claims.
- **P5c** consistent live snapshots via a **vsock quiesce control channel**:
  guest RCON `save-off`/`save-all flush` + `FIFREEZE`, snapshot, `FITHAW` +
  `save-on` (`cmd/init/vsock_linux.go`, `rcon.go`; `firecracker/quiesce.go`). A
  **periodic sweep** (`FC_SNAPSHOT_INTERVAL`) snapshots every running VM;
  **on-demand** backups flow `POST /servers/:id/snapshot` → `backup_requested`
  flag (`00003`) → reconciler → `Provisioner.Snapshot` → agent, clearing the flag
  and stamping `last_backup_at` (failure leaves the flag set for retry). Live
  snapshots gzip to a local temp *while frozen*, thaw, then upload off the freeze
  window.
- **Verify:** storage/codec unit tests; MinIO-testcontainer e2e (`-tags e2e`);
  KVM e2e `TestKVMWorldStoreReschedule` and `TestKVMLiveSnapshot`.

**P6 — Networking / player access.** *(Plan previously marked this "not started" —
it is feature-complete for IPv4 TCP/UDP.)*
- **eBPF NAT dataplane, zero iptables/nftables rules**
  (`internal/agent/firecracker/bpf/nat.c`, loaded via cilium/ebpf). SNAT/DNAT ride
  the **kernel's own nf_conntrack** through `bpf_ct_*` kfuncs (the kernel allocates
  SNAT ports); TCX hooks on each VM TAP and on the uplink. Requires **kernel ≥6.6**
  with conntrack/NAT/BTF; compiled `.o`/bindings are committed (`make bpf-generate`
  to regenerate).
- **IPAM** (`netalloc.go`): in-memory allocator hands out a unique VM `/32` (+ a
  **deterministic MAC** derived from the IP) and a host port from a configurable
  range. **In-memory only — ports are renumbered if the agent restarts.**
- **TAP lifecycle** (`tap_linux.go`): persistent TAP per VM, up/down, optional
  opt-in `tapfilter` eBPF program for observability/drop
  (`CRAFTLING_TAP_FILTER_*`).
- **End-to-end DNAT:** `host_public_ip:allocated_port → vm_ip:25565` is wired —
  the driver publishes the `dnat_rules` map entry, and the allocated host/port is
  returned through `agent.VM` and written back to `game_servers` via the
  reconciler's `MarkRunning` (host/port columns already existed).
- **Guest side** (`cmd/init/net_linux.go` + `netlink_linux.go`): secondary VM
  address, permanent gateway ARP entry, default route — all via a hand-rolled
  rtnetlink client.
- **Verify:** `netalloc` unit tests (always-on); `-tags bpf` kernel e2e
  (`make test-bpf`, needs root + kernel ≥6.6) covering load/verifier, DNAT/SNAT
  round-trips, concurrent VMs, ARP, stats; a dedicated CI eBPF lane.
- **Not done in P6:** IPv6; ICMP (dropped); the ACL policy maps
  (`egress_policy`/`ingress_policy`) are allocated but unused (default-allow); the
  per-server hostname / TCP-SNI proxy is still future.

### Tooling / CI (verified)
- **Makefile:** `run`, `run-agent`, `build`, `build-agent`, `test`, `test-kvm`
  (Firecracker, needs `/dev/kvm` + `FC_*`), `test-bpf` (eBPF, root + kernel ≥6.6),
  `bpf-generate`, `tidy`, `fmt`.
- **CI** (`.github/workflows/ci.yml`): three jobs — (1) vet/build/unit
  (`-race`)/e2e (`-tags e2e`, testcontainers Postgres), (2) eBPF dataplane e2e
  (`-tags bpf`, skips below kernel 6.6), (3) Firecracker KVM e2e (`-tags kvm`,
  skips with no `/dev/kvm`). `pages.yml` publishes `web/` to GitHub Pages.
- **Deploy:** `Dockerfile` (control plane → distroless static nonroot),
  `Dockerfile.agent` (debian-slim + firecracker + e2fsprogs + kmod, privileged,
  `/dev/kvm`), `docker-compose.yml` (Postgres + server + nginx frontend + agent).
- **Frontend** (`frontend/src`, React/Vite): auth screen, servers view (CRUD,
  restart, snapshot), marketplace/templates view (configure a template and launch
  a real server — the control plane resolves image + env server-side); then it
  hands off to the servers view to watch provisioning. hosts/scheduler/quotas are
  stub routes. Calls the control-plane API via `lib/api.ts` with transparent
  refresh-token rotation. No hosts/metrics/health UI yet.

---

## Remaining work

### Housekeeping / tech debt (do before/alongside P7+)
- ✅ **`fc-assets/` is now git-ignored** (`/fc-assets/`, runtime/working data) and
  ownership fixed, so `go build ./...` / `go test ./...` walk cleanly from a fresh
  checkout. `Dockerfile.agent` (untracked) may still have uncommitted changes —
  land it.
- **Provisioning capacity race:** the scheduler reserves, but if real host
  capacity has shrunk by the time the agent provisions, there is no rollback — the
  server retries next tick. Acceptable for now; revisit with P8 fencing.

### P7 — Observability / deep health  ⏳ not started
- **Goal:** know the *Minecraft process* is up, not just the VM.
- Agent probes via RCON / Server List Ping → report `player_count`, `health`,
  `last_seen` (new columns or a `server_health` table) up through the existing
  `Status` seam.
- Prometheus `/metrics` on control plane + agent (currently only `/healthz` that
  always returns ok, and `/ping`); make `/healthz` a real readiness check (DB +
  store). Surface `status_message`/health in API + frontend.
- **Verify:** e2e asserts health transitions; scrape `/metrics`.

### P8 — Reliability  ⏳ not started
- **Goal:** survive reconcile and host failures.
- **Retry/backoff:** today a failed reconcile sets `status=error` and just relies
  on the 2s tick; replace with `attempts` + `next_attempt_at` (exponential
  backoff) and have `ListReconcilable` respect it.
- **Host-failure reschedule:** the host reaper marks a host `down` but **nothing
  reschedules its servers** — `host_id` is only cleared on delete. Add: on
  sustained host-down, clear `host_id`/`vm_id` and re-place, **with fencing**
  (generation token / ensure the old VM is gone) to avoid split-brain. P5's
  store-mediated reschedule is the data half; this is the control half.
- **Draining:** `model.HostDraining` exists but is never entered or honored — wire
  a drain that blocks new placement and migrates servers off.
- Optional leader election (advisory lock/lease) for multiple control-plane
  replicas.
- **Verify:** kill a host in test → servers rescheduled; error path backs off.

### P9 — Quotas / resource controls  ⏳ not started
- `user_quotas` table (`max_servers`, `max_cpus`, `max_memory_mb`); enforce in
  Create/Update against current usage; admin endpoints to view/set. (No quota code
  exists today; the frontend route is a stub.)
- **Verify:** e2e — exceed quota → `403`.

### P10 — Hardening & ops  ⏳ largely not started
- **Agent↔control-plane auth:** the gRPC `AgentLink` stream is **unauthenticated**
  (insecure transport, no per-host credential check on connect). Add per-host
  tokens (a metadata/stream interceptor) or mTLS with rotation and lock it down.
- **Secrets / fail-fast:** `JWT_SECRET` defaults to `dev-secret-change-me` with no
  startup check — **fail fast** if it (or other prod secrets) is the default in a
  production mode. Source DB/object-storage/JWT creds from env/secret store.
- **microVM isolation:** add jailer + seccomp + cgroups + network policy (the ACL
  maps from P6 are the hook); today VMs run without the jailer.
- **Deploy split polish:** images/compose exist; document and harden the
  control-plane-HA-behind-LB vs agent-on-KVM-host split; per-role config.
- **CI:** the KVM + eBPF lanes exist; consider a dedicated self-hosted KVM runner
  and wiring the OCI→squashfs build into release.
- **Verify:** security review; staged rollout.

---

## Dependency order

`P0 → P1 → P2 → P3 → P4 → P6` are **done** (compute + player-access path). `P5`
(done) sits on P3 and gates safe reschedule in P8. Remaining:
`P7` and `P9` depend on P3 (any time); `P8` depends on P2 + P5; `P10` is last and
cross-cutting. Housekeeping is independent.

## Components at a glance

| Phase | Status | Binaries | Packages | Tables/columns |
| --- | --- | --- | --- | --- |
| P0 | ✅ | — | `internal/db/migrations` (goose) | (migrations) |
| P1 | ✅ | — | `repository/host.go` (in-mem), host reaper | — (no `hosts` table, by design) |
| P2 | ✅ | — | `internal/scheduler` | `game_servers.host_id` (`00002`) |
| P3 | ✅ | `cmd/agent` | `internal/agent` (agent-side link), `internal/agentlink` (CP hub + gRPC), `provisioner.RemoteProvisioner` | — |
| P4 | ✅ | `cmd/init` | `internal/agent/firecracker`, `internal/image`, `internal/squashfs`, `internal/runspec`, `internal/registry` | — |
| P5 | ✅ | — | world disk + overlay; `internal/storage` (`DirStore` + `s3`); `internal/worldstore`; vsock quiesce; `reaper.Worlds` | world disk (host file) + snapshot (store blob); `game_servers.backup_requested/last_backup_at` (`00003`) |
| P6 | ✅ | — | `firecracker/{nat,tap,tapfilter,netalloc}*`, `bpf/`, `cmd/init/net*` | host/port written back (existing cols) |
| P7 | ⏳ | — | metrics, RCON/ping health probes | `server_health` / cols |
| P8 | ⏳ | — | backoff, host-failure reschedule + fencing, draining, leader election | `game_servers.attempts/next_attempt_at` |
| P9 | ⏳ | — | quota enforcement | `user_quotas` |
| P10 | ⏳ | — | agent auth, secret fail-fast, jailer/seccomp | per-host agent creds |
