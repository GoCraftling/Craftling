# craftling-go

The control plane **and** host agent for a multi-host, Firecracker-microVM
Minecraft hosting platform. A [Gin](https://github.com/gin-gonic/gin) HTTP API
owns users, auth, and game-server desired state; a reconciler drives each server
onto a fleet host; and a per-host **agent** boots it as a real Firecracker
microVM. See [docs/PLAN.md](docs/PLAN.md) for the phased roadmap. **P0–P6 have
landed** (foundations, host fleet, scheduler, agent split, Firecracker runtime,
world persistence, and the eBPF NAT dataplane); P7–P10 — observability, reliability,
quotas, and hardening — remain.

## Requirements

- Go 1.26+
- PostgreSQL 13+
- For the real VM backend (`AGENT_RUNTIME=firecracker`): a Linux host with
  `/dev/kvm`, the `firecracker` binary, a `vmlinux` kernel, and rootfs images.
- For (re)building the eBPF dataplane: clang + libbpf + bpftool on a Linux ≥6.6
  host (see [docs/ebpf-nat-dataplane.md](docs/ebpf-nat-dataplane.md)).

## Getting started

```bash
cp .env.example .env   # set DATABASE_URL, JWT_SECRET, etc.

# Spin up Postgres (example via Docker):
docker run -d --name craftling-pg -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=craftling -p 5432:5432 postgres:16-alpine

make run               # or: go run ./cmd/server
```

The server listens on `:8080` by default and applies the embedded **goose
migrations** (`internal/db/migrations`) automatically on startup — clean on both
a fresh database and one already at an older revision.

## Configuration

### Control plane (`make run`)

| Variable       | Default                                                          | Description                      |
| -------------- | --------------------------------------------------------------- | -------------------------------- |
| `PORT`         | `8080`                                                          | HTTP listen port                 |
| `GRPC_PORT`    | `8090`                                                          | gRPC AgentLink listen port — agents dial it and hold a command stream open |
| `APP_ENV`      | `development`                                                   | `production` → release mode + JSON logs |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/craftling?sslmode=disable` | Postgres connection string |
| `JWT_SECRET`   | `dev-secret-change-me`                                          | HMAC signing secret (**set in prod**) |
| `ACCESS_TTL`   | `15m`                                                          | Access-token (JWT) lifetime      |
| `REFRESH_TTL`  | `720h` (30d)                                                  | Refresh-token lifetime           |
| `ADMIN_EMAIL`    | _(unset)_                                                    | If set with `ADMIN_PASSWORD`, seeds/promotes this admin on startup |
| `ADMIN_PASSWORD` | _(unset)_                                                    | Admin bootstrap password         |
| `TEMPLATE_INDEX_URL` | _(unset)_                                                | Marketplace registry index the `/templates` API fetches from |

### Host agent (`make run-agent`, `MODE=agent`)

| Variable          | Default            | Description                                            |
| ----------------- | ------------------ | ------------------------------------------------------ |
| `CONTROL_PLANE_GRPC_ADDR` | `localhost:8090` | Control plane's gRPC AgentLink the agent dials and holds a stream open to |
| `ADVERTISE_HOST`  | `127.0.0.1`        | Player-facing connect host VMs report                  |
| `AGENT_RUNTIME`   | `fake`             | VM backend: `fake` (in-memory) or `firecracker` (real microVMs) |
| `FC_BINARY`       | `firecracker`      | Firecracker executable (PATH lookup by default)        |
| `FC_KERNEL`       | _(required)_       | Uncompressed `vmlinux` all VMs boot                    |
| `FC_IMAGE_REF`    | _(required)_       | OCI image converted to a read-only squashfs rootfs; `{version}` is substituted with the server's version (a ref without it is used as-is) |
| `FC_IMAGE_REF_DEFAULT`| _(unset)_      | Fallback OCI ref when a server has no version and `FC_IMAGE_REF` is templated |
| `FC_IMAGE_CACHE_DIR`| `images/` under `FC_WORK_DIR` | Where converted, content-addressed squashfs rootfs files are cached |
| `FC_INIT_BIN_AMD64` / `FC_INIT_BIN_ARM64` | _(required)_ | Prebuilt `cmd/init` binaries injected as guest PID 1, one per guest arch |
| `FC_WORK_DIR`     | OS temp dir        | Per-VM working dirs (sockets, logs, vsock UDS)         |

World persistence (P5) is off until you opt in:

| Variable          | Default            | Description                                            |
| ----------------- | ------------------ | ------------------------------------------------------ |
| `FC_WORLD_PERSIST`| `false`            | Attach a per-server world disk overlaid on the rootfs (needs `mkfs.ext4` + a guest kernel with `CONFIG_OVERLAY_FS`/`CONFIG_EXT4_FS`) |
| `FC_DATA_DIR`     | `worlds/` under `FC_WORK_DIR` | Where per-server world disks live (survive stop/start) |
| `FC_WORLD_DISK_MB`| driver default     | Size of a freshly created world disk                   |
| `FC_MKFS_EXT4`    | PATH lookup        | `mkfs.ext4` executable                                 |
| `FC_WORLD_STORE_DIR` | _(unset)_       | Directory/NFS mount used as the durable world store (restore-on-boot, snapshot-on-stop, cross-host reschedule) |
| `FC_WORLD_STORE_S3_*` | _(unset)_       | S3-compatible world store (`_ENDPOINT`, `_BUCKET`, `_REGION`, `_ACCESS_KEY`, `_SECRET_KEY`, `_USE_SSL`, `_PREFIX`); **takes precedence** over `FC_WORLD_STORE_DIR` |
| `FC_SNAPSHOT_INTERVAL` | `0` (off)      | Periodic application-consistent snapshots of running VMs (needs a world store) |
| `FC_RCON_PORT` / `FC_RCON_PASSWORD` | _(unset)_ | Let the guest flush the workload via RCON before freezing its disk for a live snapshot |

The eBPF NAT dataplane (P6) activates automatically on a Linux ≥6.6 agent host
with `nf_conntrack`/`nf_nat` available; it needs no extra env to run.

`AGENT_ID`, `AGENT_HOSTNAME`, `ZONE`, `CPUS_TOTAL`, `MEMORY_MB_TOTAL`, and
`AGENT_VERSION` further describe the host in its registration.

## Endpoints

| Method | Path                     | Auth   | Description                          |
| ------ | ------------------------ | ------ | ----------------------------------- |
| GET    | `/healthz`               | —      | Liveness probe                      |
| GET    | `/api/v1/ping`           | —      | Example endpoint                    |
| POST   | `/api/v1/auth/register`  | —      | Create user, returns a token pair   |
| POST   | `/api/v1/auth/login`     | —      | Verify credentials, returns a pair  |
| POST   | `/api/v1/auth/refresh`   | —      | Rotate refresh token, returns a pair |
| POST   | `/api/v1/auth/logout`    | —      | Revoke a refresh token (`204`)      |
| GET    | `/api/v1/me`             | Bearer | Current authenticated user          |
| POST   | `/api/v1/servers`        | Bearer | Create a game server                |
| GET    | `/api/v1/servers`        | Bearer | List your game servers              |
| GET    | `/api/v1/servers/:id`    | Bearer | Get one of your servers             |
| PATCH  | `/api/v1/servers/:id`    | Bearer | Rename, or set `desired_state` (running/stopped) |
| DELETE | `/api/v1/servers/:id`    | Bearer | Tear down a server (`202`)          |
| POST   | `/api/v1/servers/:id/snapshot` | Bearer | Request an on-demand world backup |
| GET    | `/api/v1/templates`      | Bearer | List marketplace templates          |
| GET    | `/api/v1/templates/:id`  | Bearer | Get one template's manifest         |
| GET    | `/api/v1/admin/users`    | Admin  | List all users (role `admin` only)  |
| GET    | `/api/v1/admin/servers`  | Admin  | List all servers across all owners  |
| GET    | `/api/v1/admin/hosts`    | Admin  | List the fleet                      |

Agents do **not** use the HTTP API. Each host agent dials the control plane's
gRPC **AgentLink** service (`GRPC_PORT`, default `:8090`) and holds one
bidirectional stream open: it registers and heartbeats on that stream, the
control plane pushes VM lifecycle commands down it, and the agent answers on the
same stream. The agent exposes no inbound API of its own.

Auth endpoints return a token pair:

```json
{
  "access_token": "<jwt>",
  "refresh_token": "<opaque>",
  "token_type": "Bearer",
  "expires_in": 900
}
```

The short-lived **access token** is sent as `Authorization: Bearer <access_token>`
on protected routes. When it expires, exchange the **refresh token** at
`/auth/refresh` for a new pair. Refresh tokens are **rotated** on every use
(the old one is revoked); replaying a rotated token is treated as theft and
revokes all of that user's tokens.

```bash
# Register (or login) to get a token pair
PAIR=$(curl -s -X POST localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"alice@example.com","password":"hunter2pass"}')
ACCESS=$(echo "$PAIR" | jq -r .access_token)
REFRESH=$(echo "$PAIR" | jq -r .refresh_token)

# Call a protected route
curl localhost:8080/api/v1/me -H "Authorization: Bearer $ACCESS"

# Later: rotate for a fresh pair
curl -s -X POST localhost:8080/api/v1/auth/refresh \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"

# Log out (revoke the refresh token)
curl -s -X POST localhost:8080/api/v1/auth/logout \
  -H 'Content-Type: application/json' \
  -d "{\"refresh_token\":\"$REFRESH\"}"
```

### Roles

Each user has a single role (`user` by default, or `admin`). The role is
embedded in the access-token claims, and `RequireRole` middleware guards
admin routes — so authorization is checked without a database round-trip.

Because the role lives in the JWT, a role change takes effect on the user's
next token refresh (within `ACCESS_TTL`), not instantly.

Set `ADMIN_EMAIL` + `ADMIN_PASSWORD` to bootstrap an admin on startup: a
matching user is created if absent, or promoted to `admin` if it already
exists (its password is left untouched).

## Game servers & the reconciler

A game server separates **desired state** (`running` / `stopped` / `deleted`,
set by the API) from **observed status** (`pending` → `provisioning` →
`running` → `stopping` → `stopped` → `deleting`, plus `error`). A background
**reconciler** (`internal/reconciler`) ticks periodically, finds servers whose
status doesn't match their desired state, and drives them one step at a time
via a `Provisioner` backend.

**Placement (P2).** Before a server can run it must be placed on a fleet host.
The `internal/scheduler` picks a `ready` host with enough allocatable cpu/memory
(least-loaded) and **atomically reserves** that capacity; the assignment is
recorded as `game_servers.host_id`. A server that fits no host right now is
marked `unschedulable` and retried; a spec larger than any host is rejected at
create time.

**Agent split (P3).** The control plane never touches KVM. The reconciler's
backend is `provisioner.RemoteProvisioner`, which routes each call to the
assigned host's **agent** (`cmd/agent` / `internal/agent`) by pushing a command
down the persistent gRPC stream that agent holds open to the control plane
(`internal/agentlink`) — the control plane never dials the agent, so agents need
no inbound reachability. The agent runs a `Runtime`; which one is chosen by
`AGENT_RUNTIME` (`fake` for the in-memory stub, or `firecracker` for real
microVMs) — the API and reconciler are identical either way because both satisfy
the same `agent.Runtime` interface. Agents register and heartbeat over that same
stream, and dropping the stream marks the host down at once, so the scheduler
always knows the fleet.

```bash
# Create a server (desired_state defaults to running)
curl -s -X POST localhost:8080/api/v1/servers \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"name":"survival","version":"1.20.4"}'

# Or launch from a marketplace template: the control plane fetches the manifest
# from the trusted registry, validates the answers, and resolves the OCI image
# and environment server-side (the client never supplies the image or raw env).
curl -s -X POST localhost:8080/api/v1/servers \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"name":"survival","template_id":"vanilla-1","eula_accepted":true,
       "answers":{"DIFFICULTY":"hard"},"cpus":4,"memory_mb":8192}'

# Poll until the reconciler reports status "running" with host/port
curl -s localhost:8080/api/v1/servers/<id> -H "Authorization: Bearer $ACCESS"

# Stop / start
curl -s -X PATCH localhost:8080/api/v1/servers/<id> \
  -H "Authorization: Bearer $ACCESS" -H 'Content-Type: application/json' \
  -d '{"desired_state":"stopped"}'

# Tear down (reconciler deprovisions, then soft-deletes the row)
curl -s -X DELETE localhost:8080/api/v1/servers/<id> -H "Authorization: Bearer $ACCESS"
```

Deletes are **soft**: the reconciler deprovisions the VM and then sets
`status = deleted` with a `deleted_at` timestamp. The row is retained for
history/audit but hidden from every API read (and from reconciliation).

## Firecracker runtime, the image pipeline & networking

**Runtime (P4).** `internal/agent/firecracker` boots each server as a real
Firecracker microVM, driving Firecracker through the in-repo generated REST
client (`internal/firecracker`) over the per-VM API Unix socket and managing the
VMM process directly. `Provision` stages a per-VM writable copy of the version's
base rootfs, launches `firecracker`, configures machine/boot-source/drive, then
starts the instance; `Stop` sends `SendCtrlAltDel` (force-kill after a grace
period) **but keeps the rootfs** so the world survives a restart on that host;
`Start` re-boots from it. `make test-kvm` runs the gated lifecycle integration
test on a `/dev/kvm` host.

**World persistence (P5).** With `FC_WORLD_PERSIST` on, each server gets a
writable **world disk** (`/dev/vdb`, ext4) overlaid by the guest onto the
workload's working dir, so worlds survive stop/start. A durable **world store**
(`FC_WORLD_STORE_DIR` or `FC_WORLD_STORE_S3_*`) restores a world on Provision and
snapshots it on Stop — which is what makes a cross-host reschedule safe. Live
snapshots quiesce the running game (RCON `save-off`/`save-all flush` +
`fsfreeze`) over a vsock control channel, and a control-plane reaper GCs orphan
worlds.

**Image pipeline.** `internal/image` converts an OCI/Docker image into a
read-only **squashfs** rootfs (`internal/squashfs`, a from-scratch writer),
injecting the Go init binary (`cmd/init`) as PID 1. The init agent mounts the
kernel filesystems, fetches a **run spec** (`internal/runspec`) from Firecracker's
MMDS over the link-local address, applies per-VM networking + the persist overlay,
then execs and supervises the workload — powering the VM off when it exits.
Production `Provision` resolves the server's OCI ref (`FC_IMAGE_REF`), converts
it through this pipeline (cached by digest), attaches the squashfs **read-only**
as `/dev/vda`, and boots `cmd/init` with `root=/dev/vda ro init=/.craftling/init`
— the run-spec/persist/NAT machinery layers on top. The control plane resolves
launchable templates from a marketplace registry (`internal/registry`, the
`/templates` API).

**Networking (P6).** An eBPF NAT dataplane gives each VM real connectivity with
no Linux bridge and no iptables/nftables rules: TCX-attached programs SNAT egress
and DNAT a per-server host port (allocated by an in-agent IPAM) to the in-VM
service port, reusing the kernel's `nf_conntrack` via `bpf_ct_*` kfuncs. The
allocated host/port is written back to the game server, and the guest applies its
address, gateway neighbor, and default route from the run spec. Needs a Linux ≥6.6
agent host. Design and status: [docs/ebpf-nat-dataplane.md](docs/ebpf-nat-dataplane.md).

## Layout

```
cmd/server          control-plane entry point: DB connect/migrate + HTTP API + gRPC AgentLink + graceful shutdown
cmd/agent           host-agent entry point: dials the control plane, serves pushed VM commands over a persistent gRPC stream; selects the VM backend
cmd/init            in-VM PID 1: mount, fetch run spec from MMDS, apply networking, supervise workload
internal/config     environment configuration (control plane + agent + Firecracker)
internal/db         pgx pool + embedded goose migrations
internal/model      domain types (User, GameServer, Host)
internal/repository data access (Postgres; in-memory host inventory)
internal/auth       bcrypt password hashing + JWT issue/verify
internal/handler    control-plane routes (auth, servers, templates, admin)
internal/middleware request ID, request logging, JWT auth guard
internal/registry   template registry (marketplace) client
internal/scheduler  host placement + atomic capacity reservation (P2)
internal/reconciler desired-state → observed-status convergence loop
internal/provisioner Provisioner seam: Fake + RemoteProvisioner (P3)
internal/agent      host agent: Runtime interface, FakeRuntime, agent-side gRPC link loop (P3)
internal/agentlink  control-plane side of the agent gRPC stream: hub (connection registry + command push) + generated protobuf
internal/agent/firecracker  real Firecracker microVM Runtime + eBPF NAT dataplane (P4/P6)
internal/firecracker generated Firecracker REST client (go-openapi, over the API Unix socket)
internal/image      OCI/Docker image → squashfs rootfs converter (injects cmd/init)
internal/squashfs   squashfs writer/compressor used by the converter
internal/runspec    host↔guest run-spec contract delivered via MMDS
internal/reaper     periodic background cleanup (stale hosts → down)
internal/seed       initial-data bootstrap (admin account)
internal/logger     zap logger + request-scoped context helpers
```

## Commands

```bash
make run          # run the control plane
make run-agent    # run a host agent (MODE=agent; see Makefile/.env.example for env)
make build        # build control plane to ./bin/server
make build-agent  # build agent to ./bin/agent
make test         # run unit tests
make test-e2e     # run end-to-end tests (requires Docker)
make test-kvm     # Firecracker lifecycle test (requires /dev/kvm + host artifacts)
make test-bpf     # eBPF NAT/tapfilter dataplane in-kernel (sudo; Linux ≥6.6)
make bpf-generate # regenerate eBPF bindings/objects (Linux ≥6.6 + clang/libbpf/bpftool)
make tidy         # tidy go.mod
make fmt          # format code
```

## Testing

End-to-end tests live in `test/e2e/`. They are gated behind the `e2e` build
tag, so `make test` (plain `go test ./...`) never touches Docker. The e2e
suite uses [testcontainers](https://golang.testcontainers.org/) to start a
real Postgres, serves the actual router over HTTP, and drives the full
lifecycle (register → login → server placement → reconcile → teardown):

```bash
make test-e2e          # or: go test -tags e2e ./test/e2e/...
```

The Firecracker lifecycle test is gated behind the `kvm` build tag and kept out
of the default lane; run it on a KVM host with the `FC_*` artifacts in place:

```bash
# The KVM tests convert a tiny busybox image themselves, so only the kernel
# (and optionally FC_BINARY) is required:
sudo -E FC_KERNEL=/path/vmlinux make test-kvm
```
