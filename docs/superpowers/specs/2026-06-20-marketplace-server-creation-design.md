# Marketplace → Server Creation Flow

**Status:** approved · **Date:** 2026-06-20

## Goal

Turn the marketplace from a styled gallery that dead-ends at a stub into a working
launch flow: selecting and configuring a template **creates a real game server
that boots that template's OCI image with the user's resolved environment**.

Today the seam stops in two places:

- `frontend/src/components/marketplace-view.tsx` — `onComplete` logs the resolved
  launch and shows a "provisioning hand-off is next" banner; it never calls the API.
- `internal/provisioner/remote.go` — `Provision` builds an `agent.VMSpec` with no
  `RunSpec` and no image override, so even if a server carried template env/image it
  would be dropped before reaching the agent.

The agent side is already capable: `agent.VMSpec.RunSpec` exists, and the
Firecracker runtime already merges a per-server env override over the image's baked
env (`applyRunSpecOverride` → `mergeEnv`) and resolves the image per server. The
work is to persist the template's resolution and thread it down to that seam.

## Decisions (from brainstorming)

1. **Full vertical slice** — persist env + image ref and deliver them to the VM.
2. **Server-side resolution** — the control plane is authoritative. The create
   request references a `template_id` + `answers`; the control plane re-fetches the
   manifest via its existing registry client, validates, and resolves env + image
   ref. Clients cannot inject arbitrary OCI images or env; only the trusted
   `TEMPLATE_INDEX_URL` introduces images.
3. **Post-launch UX** — route to the Servers view to watch provisioning.
4. **Size picker** — the template drawer reuses the `SIZES` presets.

## Architecture

### 1. Data model & DB

Migration `internal/db/migrations/00004_game_servers_template.sql`, default-safe
adds (idempotent `ADD COLUMN IF NOT EXISTS`, NULL-able):

- `image_ref TEXT` — full OCI ref the VM boots (e.g. `itzg/minecraft-server:java21`).
  Authoritative; overrides the agent's `FC_IMAGE_REF`.
- `env JSONB` — resolved env map (`{"DIFFICULTY":"hard",...}`).
- `template_id TEXT` — provenance / display.

`model.GameServer` gains `ImageRef *string` (`json:"image_ref,omitempty"`),
`Env map[string]string` (`json:"env,omitempty"`), `TemplateID *string`
(`json:"template_id,omitempty"`).

Repository (`internal/repository/game_server.go`): extend `gameServerColumns`,
`scanGameServer`, and `Create`. `env` is encoded/decoded explicitly through JSON
bytes (scan `jsonb` into `[]byte`, `json.Unmarshal` when non-empty; on insert
`json.Marshal` the map, or NULL when nil) to avoid relying on pgx map-codec NULL
semantics. Legacy/manual rows scan as NULL → nil and are unaffected.

### 2. Registry: typed manifest + resolver

`internal/registry`:

- Add `Manifest` and `Variable` structs mirroring the manifest JSON
  (`image_name`, `image_tag`, `template_name`, `thumbnail_url`, `eula_needed`,
  `guest_volumes`, `variables`, `env`).
- `ManifestParsed(ctx, id) (*Manifest, error)` — fetch raw (reusing the existing
  cache) and `json.Unmarshal`. The raw `Manifest()` and `GET /templates/:id` stay
  unchanged for the frontend.
- `Resolve(m *Manifest, answers map[string]string) (env map[string]string, imageRef string, err error)`:
  - select variables (non-empty `acceptable_answers`) require an answer that is one
    of the allowed values; free-text variables are optional.
  - substitute `$VarName$` in `m.env` using answers (same regex as the frontend's
    `resolveEnv`: `\$([A-Za-z0-9_]+)\$`).
  - `imageRef = image_name + ":" + image_tag`.

### 3. API: `ServerHandler.Create` (both shapes)

Handler gains a `TemplateResolver` dependency (interface satisfied by
`registry.Client`; faked in tests). Request body:

```
{ "name", "template_id"?, "answers"?, "eula_accepted"?, "version"?, "cpus"?, "memory_mb"? }
```

- **Template path** (`template_id` set): fetch manifest; if `eula_needed` and not
  `eula_accepted` → 400; resolve env + image ref; set `version = image_tag`;
  persist `ImageRef`/`Env`/`TemplateID`. Unknown template → 404; bad/missing answer
  → 400.
- **Direct path** (`version` set, no `template_id`): unchanged behaviour — preserves
  the manual CreateDrawer.

Capacity pre-check (`CanEverFit`) and the rest of the create path are unchanged.

### 4. Provisioner threading

- `agent.VMSpec` gains `ImageRef string` (`json:"image_ref,omitempty"`).
- `RemoteProvisioner.Provision` populates `ImageRef` from `s.ImageRef`, and when
  `s.Env` is non-empty sets `RunSpec: &runspec.RunSpec{Env: ["K=V",…]}` (keys
  sorted for determinism). `Start` is untouched — the Firecracker machine retains
  its `runSpec` from Provision.
- Firecracker `Provision`: a small extracted helper chooses `spec.ImageRef` when
  set, else `cfg.imageRefFor(spec.Version)`. Env override already flows through the
  existing `applyRunSpecOverride`/`mergeEnv`.

### 5. Frontend

- `lib/api.ts`: `CreateServerInput` gains optional `template_id`, `answers`,
  `eula_accepted`.
- `template-drawer.tsx`: add a `SIZES` size picker (default medium); `submit`
  emits `answers`, `eula`, `cpus`/`mem`, and the `template_id`.
- `marketplace-view.tsx`: `onComplete` calls `api.createServer(...)`; on success
  invokes a new `onLaunched` prop; failures surface in the existing error banner.
  Remove the stub success banner copy.
- `App.tsx`: pass `onLaunched={() => setRoute("servers")}`.

### 6. Testing

- Go unit tests:
  - `registry`: `ManifestParsed` + `Resolve` (valid, bad select answer, missing
    select answer, placeholder substitution, image-ref build).
  - `handler`: `Create` from template — success, unknown template (404), missing
    EULA (400), invalid answer (400) — against a fake resolver; direct path still
    works.
  - `provisioner/remote`: `Provision` carries `ImageRef` + `RunSpec.Env`.
  - `firecracker`: the ref-selection helper prefers `spec.ImageRef`.
- Frontend: no test harness exists; rely on `tsc`/Vite build.

## Invariants preserved

- Control plane never touches KVM; the reconciler stays the sole writer of compute
  side effects.
- Only the trusted `TEMPLATE_INDEX_URL` can introduce images.
- Manual create and all existing servers keep working (NULL template columns).
