/* api.ts — typed client for the craftling-go control-plane API.
 *
 * Talks to /api/v1 (proxied to the Go server in dev, same-origin in prod).
 * Handles bearer-token auth with transparent refresh-token rotation, and
 * adapts the backend's GameServer shape into the UI's richer Server model. */
import type { Server, ServerStatus } from "./data"

const BASE = "/api/v1"

const ACCESS_KEY = "cl-access"
const REFRESH_KEY = "cl-refresh"

// ── Wire types (match the Go JSON exactly) ──────────────────────────────────

export interface TokenResponse {
  access_token: string
  refresh_token: string
  token_type: string
  expires_in: number
}

export type ApiRole = "user" | "admin"

export interface ApiUser {
  id: string
  email: string
  role: ApiRole
  created_at: string
}

// ── Quotas (P9) ──────────────────────────────────────────────────────────────

// A user's effective resource quota. Each limit uses 0 to mean unlimited.
// `custom` is false when the user is on the system default, true once an admin
// has set a per-user override.
export interface ApiQuota {
  user_id: string
  max_servers: number
  max_cpus: number
  max_memory_mb: number
  custom: boolean
  created_at?: string
  updated_at?: string
}

// A user's current committed allocation across their live servers.
export interface ApiQuotaUsage {
  servers: number
  cpus: number
  memory_mb: number
}

export interface ApiQuotaResponse {
  quota: ApiQuota
  usage: ApiQuotaUsage
}

export interface SetQuotaInput {
  max_servers: number
  max_cpus: number
  max_memory_mb: number
}

// ── Billing (P9, pay-as-you-go hourly) ───────────────────────────────────────

export interface ApiBillingItem {
  server_id: string
  name: string
  cpus: number
  memory_mb: number
  hours: number
  hourly_rate: number
  cost: number
  running: boolean
}

export interface ApiBillingSummary {
  currency: string
  period_start: string
  cpu_hour: number
  memory_gb_hour: number
  items: ApiBillingItem[]
  total_cost: number
  // Live burn rate: the summed hourly price of everything currently running.
  hourly_rate: number
}

// ── Players / whitelist roster ───────────────────────────────────────────────

export interface ApiPlayer {
  id: string
  owner_id: string
  username: string
  // The caller's servers this player may use.
  server_ids: string[]
  created_at: string
  updated_at: string
}

export interface CreatePlayerInput {
  username: string
  server_ids?: string[]
}

export interface UpdatePlayerInput {
  username?: string
  // When present, replaces the whole grant set (check/uncheck).
  server_ids?: string[]
}

export type ApiDesiredState = "running" | "stopped" | "deleted"

export type ApiStatus =
  | "pending"
  | "provisioning"
  | "running"
  | "stopping"
  | "stopped"
  | "deleting"
  | "deleted"
  | "error"

export interface ApiServer {
  id: string
  owner_id: string
  name: string
  game: string
  version: string
  cpus: number
  memory_mb: number
  desired_state: ApiDesiredState
  status: ApiStatus
  vm_id?: string | null
  host?: string | null
  port?: number | null
  status_message?: string | null
  // Deep health (P7): live player counts and the last time the game process was
  // probed answering. Null/absent until a running server is probed.
  players_online?: number | null
  players_max?: number | null
  last_seen?: string | null
  created_at: string
  updated_at: string
}

// ── Host fleet (admin) ──────────────────────────────────────────────────────

export type ApiHostStatus = "ready" | "draining" | "down"

export interface ApiHost {
  id: string
  hostname: string
  address: string
  zone: string
  // *_total is physical capacity; *_allocatable is what's left for new placements.
  cpus_total: number
  memory_mb_total: number
  cpus_allocatable: number
  memory_mb_allocatable: number
  status: ApiHostStatus
  agent_version: string
  last_heartbeat_at: string
  created_at: string
  updated_at: string
}

export interface CreateServerInput {
  name: string
  cpus?: number
  memory_mb?: number
  // Direct create: the OCI image tag the agent's default image is templated with.
  // Omitted for a template launch.
  version?: string
  // Template launch: the control plane resolves the image + env server-side from
  // the trusted registry. answers maps each template variable to the chosen value;
  // eula_accepted must be true when the template requires it.
  template_id?: string
  answers?: Record<string, string>
  eula_accepted?: boolean
}

export interface UpdateServerInput {
  name?: string
  version?: string
  desired_state?: "running" | "stopped"
}

// ── Template registry (marketplace) ─────────────────────────────────────────-

/** One entry in the registry index. */
export interface TemplateSummary {
  template_id: string
  template_name: string
  thumbnail_url: string
  template_url: string
}

/** A single question the template asks the operator before launch. */
export interface TemplateVariable {
  name: string
  description: string
  acceptable_answers: string[]
}

/** The full manifest for one template, fetched on selection. */
export interface TemplateManifest {
  image_name: string
  image_tag: string
  template_name: string
  thumbnail_url: string
  eula_needed: boolean
  guest_volumes: string[]
  variables: TemplateVariable[]
  env: Record<string, string>
}

// ── Errors ──────────────────────────────────────────────────────────────────

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.name = "ApiError"
    this.status = status
  }
}

// ── Token storage ─────────────────────────────────────────────────────────--

export const tokenStore = {
  get access() {
    return localStorage.getItem(ACCESS_KEY)
  },
  get refresh() {
    return localStorage.getItem(REFRESH_KEY)
  },
  save(t: TokenResponse) {
    localStorage.setItem(ACCESS_KEY, t.access_token)
    localStorage.setItem(REFRESH_KEY, t.refresh_token)
  },
  clear() {
    localStorage.removeItem(ACCESS_KEY)
    localStorage.removeItem(REFRESH_KEY)
  },
}

// ── Transport ─────────────────────────────────────────────────────────────--

function send(path: string, init: RequestInit, withAuth: boolean): Promise<Response> {
  const headers = new Headers(init.headers)
  if (init.body) headers.set("Content-Type", "application/json")
  if (withAuth && tokenStore.access) {
    headers.set("Authorization", `Bearer ${tokenStore.access}`)
  }
  return fetch(BASE + path, { ...init, headers })
}

// In-flight refresh shared across concurrent 401s so we rotate the token once.
let refreshing: Promise<boolean> | null = null

function refreshTokens(): Promise<boolean> {
  if (!tokenStore.refresh) return Promise.resolve(false)
  if (!refreshing) {
    refreshing = (async () => {
      try {
        const res = await send(
          "/auth/refresh",
          { method: "POST", body: JSON.stringify({ refresh_token: tokenStore.refresh }) },
          false
        )
        if (!res.ok) {
          tokenStore.clear()
          return false
        }
        tokenStore.save((await res.json()) as TokenResponse)
        return true
      } catch {
        return false
      } finally {
        refreshing = null
      }
    })()
  }
  return refreshing
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  opts: { auth?: boolean } = {}
): Promise<T> {
  const withAuth = opts.auth ?? true

  let res = await send(path, init, withAuth)
  if (res.status === 401 && withAuth && (await refreshTokens())) {
    res = await send(path, init, withAuth)
  }

  if (!res.ok) {
    let message = res.statusText
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, message)
  }

  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms))

// ── API surface ─────────────────────────────────────────────────────────────

export const api = {
  async register(email: string, password: string): Promise<TokenResponse> {
    const t = await request<TokenResponse>(
      "/auth/register",
      { method: "POST", body: JSON.stringify({ email, password }) },
      { auth: false }
    )
    tokenStore.save(t)
    return t
  },

  async login(email: string, password: string): Promise<TokenResponse> {
    const t = await request<TokenResponse>(
      "/auth/login",
      { method: "POST", body: JSON.stringify({ email, password }) },
      { auth: false }
    )
    tokenStore.save(t)
    return t
  },

  async logout(): Promise<void> {
    const rt = tokenStore.refresh
    if (rt) {
      try {
        await request(
          "/auth/logout",
          { method: "POST", body: JSON.stringify({ refresh_token: rt }) },
          { auth: false }
        )
      } catch {
        /* best-effort; clear locally regardless */
      }
    }
    tokenStore.clear()
  },

  me(): Promise<ApiUser> {
    return request<ApiUser>("/me")
  },

  // Owner-scoped server list.
  listServers(): Promise<ApiServer[]> {
    return request<{ servers: ApiServer[] | null }>("/servers").then((r) => r.servers ?? [])
  },

  // Admin-only: every server across all owners.
  adminListServers(): Promise<ApiServer[]> {
    return request<{ servers: ApiServer[] | null }>("/admin/servers").then((r) => r.servers ?? [])
  },

  // Admin-only: the whole worker-host fleet inventory.
  adminListHosts(): Promise<ApiHost[]> {
    return request<{ hosts: ApiHost[] | null }>("/admin/hosts").then((r) => r.hosts ?? [])
  },

  // Admin-only: every user.
  adminListUsers(): Promise<ApiUser[]> {
    return request<{ users: ApiUser[] | null }>("/admin/users").then((r) => r.users ?? [])
  },

  // The authenticated caller's own effective quota and current usage (P9).
  myQuota(): Promise<ApiQuotaResponse> {
    return request<ApiQuotaResponse>("/quota")
  },

  // Admin-only: any user's effective quota and usage.
  adminGetUserQuota(userId: string): Promise<ApiQuotaResponse> {
    return request<ApiQuotaResponse>(`/admin/users/${userId}/quota`)
  },

  // Admin-only: set (upsert) a user's quota override.
  adminSetUserQuota(userId: string, input: SetQuotaInput): Promise<ApiQuotaResponse> {
    return request<ApiQuotaResponse>(`/admin/users/${userId}/quota`, {
      method: "PUT",
      body: JSON.stringify(input),
    })
  },

  // Admin-only: clear a user's override, reverting them to the system default.
  adminDeleteUserQuota(userId: string): Promise<ApiQuotaResponse> {
    return request<ApiQuotaResponse>(`/admin/users/${userId}/quota`, { method: "DELETE" })
  },

  // The authenticated caller's own pay-as-you-go bill for the current period.
  myBilling(): Promise<ApiBillingSummary> {
    return request<ApiBillingSummary>("/billing")
  },

  // Admin-only: any user's pay-as-you-go bill.
  adminGetUserBilling(userId: string): Promise<ApiBillingSummary> {
    return request<ApiBillingSummary>(`/admin/users/${userId}/billing`)
  },

  // ── Players / whitelist (owner-scoped) ──
  listPlayers(): Promise<ApiPlayer[]> {
    return request<{ players: ApiPlayer[] | null }>("/players").then((r) => r.players ?? [])
  },

  createPlayer(input: CreatePlayerInput): Promise<ApiPlayer> {
    return request<ApiPlayer>("/players", { method: "POST", body: JSON.stringify(input) })
  },

  updatePlayer(id: string, input: UpdatePlayerInput): Promise<ApiPlayer> {
    return request<ApiPlayer>(`/players/${id}`, { method: "PATCH", body: JSON.stringify(input) })
  },

  async deletePlayer(id: string): Promise<void> {
    await request<unknown>(`/players/${id}`, { method: "DELETE" })
  },

  // Registry index: the templates available to launch.
  listTemplates(): Promise<TemplateSummary[]> {
    return request<{ templates: TemplateSummary[] | null }>("/templates").then(
      (r) => r.templates ?? []
    )
  },

  // Full manifest for a single template.
  getTemplate(id: string): Promise<TemplateManifest> {
    return request<TemplateManifest>(`/templates/${id}`)
  },

  createServer(input: CreateServerInput): Promise<ApiServer> {
    return request<ApiServer>("/servers", { method: "POST", body: JSON.stringify(input) })
  },

  getServer(id: string): Promise<ApiServer> {
    return request<ApiServer>(`/servers/${id}`)
  },

  // Owner-scoped: the captured console output of one's own server. The control
  // plane reads it on demand from the backing VM's host.
  getServerLogs(id: string): Promise<string> {
    return request<{ logs: string }>(`/servers/${id}/logs`).then((r) => r.logs ?? "")
  },

  // Admin-only: the captured console output of any server, regardless of owner.
  adminGetServerLogs(id: string): Promise<string> {
    return request<{ logs: string }>(`/admin/servers/${id}/logs`).then((r) => r.logs ?? "")
  },

  updateServer(id: string, input: UpdateServerInput): Promise<ApiServer> {
    return request<ApiServer>(`/servers/${id}`, { method: "PATCH", body: JSON.stringify(input) })
  },

  async deleteServer(id: string): Promise<void> {
    await request<unknown>(`/servers/${id}`, { method: "DELETE" })
  },

  // The control plane has no atomic restart, so drive the desired state down to
  // stopped, wait for the reconciler to converge, then back up to running.
  async restartServer(id: string): Promise<void> {
    await this.updateServer(id, { desired_state: "stopped" })
    const deadline = Date.now() + 30_000
    while (Date.now() < deadline) {
      await delay(800)
      const s = await this.getServer(id)
      if (s.status === "stopped") break
    }
    await this.updateServer(id, { desired_state: "running" })
  },
}

// ── Template env resolution ──────────────────────────────────────────────────

/** Substitute `$VarName$` placeholders in a template's env with the operator's
 *  answers. Tokens whose variable has no answer are left untouched, so partially
 *  filled forms still render a meaningful preview. */
export function resolveEnv(
  env: Record<string, string>,
  answers: Record<string, string>
): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [key, val] of Object.entries(env)) {
    out[key] = val.replace(/\$([A-Za-z0-9_]+)\$/g, (whole, name) =>
      Object.prototype.hasOwnProperty.call(answers, name) ? answers[name] : whole
    )
  }
  return out
}

// ── Adapter: ApiServer → UI Server ──────────────────────────────────────────

const STATUS_MAP: Record<ApiStatus, ServerStatus> = {
  pending: "scheduling",
  provisioning: "provisioning",
  running: "running",
  stopping: "stopping",
  stopped: "stopped",
  deleting: "stopping",
  deleted: "stopped",
  error: "error",
}

function daysSince(iso: string): number {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return 0
  return Math.max(0, Math.floor((Date.now() - t) / 86_400_000))
}

/** Map a backend GameServer to the view model the UI renders. Player counts and
 *  health come from the agent's deep-health probe (P7); fields the control plane
 *  doesn't track yet (world size) are left empty and shown as "—" downstream. */
export function toServer(a: ApiServer): Server {
  // health summarises the deep-health probe: a running server we've seen answering
  // is "responsive"; one that's running but not yet (or no longer) answering is
  // "no response"; anything else has no meaningful health to report.
  const lastSeen = a.last_seen ?? null
  let health = "—"
  if (a.status === "running") {
    health = lastSeen ? "responsive" : "no response"
  }
  return {
    id: a.id,
    name: a.name,
    owner: a.owner_id,
    version: a.version,
    desired: a.desired_state === "running" ? "running" : "stopped",
    status: STATUS_MAP[a.status] ?? "stopped",
    hostId: null,
    cpus: a.cpus,
    mem: a.memory_mb,
    players: a.players_online ?? 0,
    maxPlayers: a.players_max ?? 0,
    address: a.host ?? null,
    port: a.port ?? null,
    health,
    lastSeen,
    statusMessage: a.status_message ?? null,
    attempts: 0,
    createdDays: daysSince(a.created_at),
    world: 0,
  }
}
