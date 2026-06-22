/* quotas-view.tsx — Quotas & Users view (admin-only): per-user resource quotas
 * (max servers / vCPU / memory) shown against live usage, with inline editing
 * and reset-to-default. Enforced server-side at server create (P9). */
import { useCallback, useEffect, useMemo, useState } from "react"
import { Icon } from "./icon"
import { Btn, Meter } from "./primitives"
import {
  api,
  ApiError,
  type ApiBillingSummary,
  type ApiQuota,
  type ApiQuotaUsage,
  type ApiUser,
} from "@/lib/api"
import { fmtMem } from "@/lib/data"

// A user joined with their effective quota, current usage, and current bill.
interface Row {
  user: ApiUser
  quota: ApiQuota
  usage: ApiQuotaUsage
  billing: ApiBillingSummary
}

// Format a currency amount with its ISO code (kept simple: code + 2 decimals).
const money = (amount: number, currency: string) => `${amount.toFixed(2)} ${currency}`

// 0 means "no cap" on a quota axis; render it as ∞.
const UNLIMITED = 0
const limitLabel = (n: number, fmt: (x: number) => string = String) =>
  n === UNLIMITED ? "∞" : fmt(n)

export function QuotasView() {
  const [rows, setRows] = useState<Row[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [q, setQ] = useState("")
  const [editing, setEditing] = useState<string | null>(null)

  const refresh = useCallback(() => {
    api
      .adminListUsers()
      .then(async (users) => {
        const joined = await Promise.all(
          users.map(async (user) => {
            const [q, billing] = await Promise.all([
              api.adminGetUserQuota(user.id),
              api.adminGetUserBilling(user.id),
            ])
            return { user, quota: q.quota, usage: q.usage, billing } as Row
          })
        )
        setRows(joined)
        setError(null)
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't reach the control plane.")
      )
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  // Apply a single updated row in place, so a save doesn't refetch the world.
  const applyRow = useCallback((next: Row) => {
    setRows((prev) => prev.map((r) => (r.user.id === next.user.id ? next : r)))
  }, [])

  const list = useMemo(() => {
    const t = q.trim().toLowerCase()
    if (!t) return rows
    return rows.filter((r) => r.user.email.toLowerCase().includes(t))
  }, [rows, q])

  const customCount = rows.filter((r) => r.quota.custom).length
  const currency = rows[0]?.billing.currency ?? ""
  const totalSpend = rows.reduce((a, r) => a + r.billing.total_cost, 0)
  const burn = rows.reduce((a, r) => a + r.billing.hourly_rate, 0)

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">Quotas &amp; Users</div>
          <div className="page-sub">
            Per-user limits on servers, vCPU, and memory — enforced at create time.
          </div>
        </div>
        <Btn variant="outline" onClick={refresh}>
          <Icon name="restart" size={15} /> Refresh
        </Btn>
      </div>

      {error && (
        <div
          className="row gap-2 t-sm"
          style={{
            color: "var(--danger-fg)",
            background: "color-mix(in oklab, var(--danger) 10%, transparent)",
            padding: "10px 12px",
            borderRadius: "var(--radius)",
            alignItems: "center",
          }}
        >
          <Icon name="alert" size={15} style={{ flex: "none" }} />
          <span>{error}</span>
          <button className="icon-btn sm" onClick={refresh} style={{ marginLeft: "auto" }}>
            <Icon name="restart" size={14} />
          </button>
        </div>
      )}

      {/* stat tiles */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 14 }}>
        <div className="card stat">
          <div className="k">
            <Icon name="users" size={14} /> Users
          </div>
          <div className="v tnum">{rows.length}</div>
          <div className="sub">{rows.filter((r) => r.user.role === "admin").length} admin</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="settings" size={14} /> Custom quotas
          </div>
          <div className="v tnum">{customCount}</div>
          <div className="sub">{rows.length - customCount} on default</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="bolt" size={14} /> Current burn
          </div>
          <div className="v tnum">{burn.toFixed(2)}</div>
          <div className="sub">{currency}/hr across running servers</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="gauge" size={14} /> Spend this month
          </div>
          <div className="v tnum">{totalSpend.toFixed(2)}</div>
          <div className="sub">{currency} pay-as-you-go, all owners</div>
        </div>
      </div>

      {/* toolbar */}
      <div className="between" style={{ gap: 12, flexWrap: "wrap" }}>
        <div className="t-sm muted">
          0 means unlimited on that axis. Limits apply when a user creates a server.
        </div>
        <div className="input-wrap" style={{ width: 240 }}>
          <Icon name="search" />
          <input
            className="input"
            placeholder="Search by email"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
      </div>

      {/* table */}
      <div className="card" style={{ overflow: "hidden" }}>
        <div style={{ overflowX: "auto" }}>
          <table className="tbl">
            <thead>
              <tr>
                <th>User</th>
                <th>Servers</th>
                <th>vCPU</th>
                <th>Memory</th>
                <th>Quota</th>
                <th>Cost (mo.)</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <QuotaRow
                  key={r.user.id}
                  row={r}
                  editing={editing === r.user.id}
                  onEdit={() => setEditing(r.user.id)}
                  onClose={() => setEditing(null)}
                  onSaved={(next) => {
                    applyRow(next)
                    setEditing(null)
                  }}
                  onError={setError}
                />
              ))}
            </tbody>
          </table>
        </div>
        {!list.length && (
          <div className="empty">
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading users…</div>
              </>
            ) : (
              <>
                <Icon name="users" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    {rows.length ? "No users match" : "No users yet"}
                  </div>
                  <div className="t-sm">
                    {rows.length ? "Try a different search." : "Users appear here once they register."}
                  </div>
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

// usageOver reports whether usage is at or past a (non-unlimited) limit, so the
// cell can flag a user who can no longer create on that axis.
const usageOver = (used: number, limit: number) => limit !== UNLIMITED && used >= limit

function QuotaRow({
  row,
  editing,
  onEdit,
  onClose,
  onSaved,
  onError,
}: {
  row: Row
  editing: boolean
  onEdit: () => void
  onClose: () => void
  onSaved: (next: Row) => void
  onError: (msg: string) => void
}) {
  const { user, quota, usage, billing } = row
  return (
    <>
      <tr>
        <td>
          <div className="col" style={{ gap: 2 }}>
            <span className="semibold row gap-2">
              {user.email}
              {user.role === "admin" && <span className="badge mono">admin</span>}
            </span>
            <span className="mono t-xs muted">{user.id}</span>
          </div>
        </td>
        <td>
          <span
            className="mono t-sm tnum"
            style={usageOver(usage.servers, quota.max_servers) ? { color: "var(--danger-fg)" } : undefined}
          >
            {usage.servers}
            <span className="muted">/{limitLabel(quota.max_servers)}</span>
          </span>
        </td>
        <td>
          <div className="col" style={{ gap: 3, minWidth: 92 }}>
            <span className="mono t-sm tnum">
              {usage.cpus}
              <span className="muted">/{limitLabel(quota.max_cpus)}</span>
            </span>
            {quota.max_cpus !== UNLIMITED && <Meter value={usage.cpus} max={quota.max_cpus || 1} />}
          </div>
        </td>
        <td>
          <div className="col" style={{ gap: 3, minWidth: 110 }}>
            <span className="mono t-sm tnum">
              {fmtMem(usage.memory_mb)}
              <span className="muted">/{limitLabel(quota.max_memory_mb, fmtMem)}</span>
            </span>
            {quota.max_memory_mb !== UNLIMITED && (
              <Meter value={usage.memory_mb} max={quota.max_memory_mb || 1} />
            )}
          </div>
        </td>
        <td>
          {quota.custom ? (
            <span className="badge soft s-info">Custom</span>
          ) : (
            <span className="badge">Default</span>
          )}
        </td>
        <td>
          <div className="col" style={{ gap: 2 }}>
            <span className="mono t-sm tnum">{money(billing.total_cost, billing.currency)}</span>
            {billing.hourly_rate > 0 && (
              <span className="row gap-1 t-xs" style={{ color: "var(--success-fg)" }}>
                <i className="dot pulse" style={{ background: "var(--success)" }} />
                {billing.hourly_rate.toFixed(2)} {billing.currency}/hr
              </span>
            )}
          </div>
        </td>
        <td style={{ textAlign: "right" }}>
          <Btn size="sm" variant="outline" onClick={editing ? onClose : onEdit}>
            <Icon name={editing ? "x" : "settings"} size={14} />
            {editing ? "Close" : "Edit"}
          </Btn>
        </td>
      </tr>
      {editing && (
        <tr>
          <td colSpan={7} style={{ background: "var(--muted)" }}>
            <QuotaEditor row={row} onClose={onClose} onSaved={onSaved} onError={onError} />
          </td>
        </tr>
      )}
    </>
  )
}

function QuotaEditor({
  row,
  onClose,
  onSaved,
  onError,
}: {
  row: Row
  onClose: () => void
  onSaved: (next: Row) => void
  onError: (msg: string) => void
}) {
  const [servers, setServers] = useState(String(row.quota.max_servers))
  const [cpus, setCpus] = useState(String(row.quota.max_cpus))
  const [memMB, setMemMB] = useState(String(row.quota.max_memory_mb))
  const [busy, setBusy] = useState(false)

  const num = (s: string) => Math.max(0, Math.floor(Number(s) || 0))

  const save = async () => {
    setBusy(true)
    try {
      const r = await api.adminSetUserQuota(row.user.id, {
        max_servers: num(servers),
        max_cpus: num(cpus),
        max_memory_mb: num(memMB),
      })
      onSaved({ user: row.user, quota: r.quota, usage: r.usage, billing: row.billing })
    } catch (e) {
      onError(e instanceof ApiError ? e.message : "Couldn't save the quota.")
      setBusy(false)
    }
  }

  const resetToDefault = async () => {
    setBusy(true)
    try {
      const r = await api.adminDeleteUserQuota(row.user.id)
      onSaved({ user: row.user, quota: r.quota, usage: r.usage, billing: row.billing })
    } catch (e) {
      onError(e instanceof ApiError ? e.message : "Couldn't reset the quota.")
      setBusy(false)
    }
  }

  return (
    <div className="col" style={{ gap: 12, padding: "14px 4px" }}>
      <div className="row gap-3 wrap" style={{ alignItems: "flex-end" }}>
        <Field label="Max servers" value={servers} onChange={setServers} />
        <Field label="Max vCPU" value={cpus} onChange={setCpus} />
        <Field label="Max memory (MB)" value={memMB} onChange={setMemMB} step={512} />
        <div className="row gap-2" style={{ marginLeft: "auto" }}>
          {row.quota.custom && (
            <Btn variant="ghost" onClick={resetToDefault} disabled={busy}>
              <Icon name="restart" size={14} /> Reset to default
            </Btn>
          )}
          <Btn variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Btn>
          <Btn variant="primary" onClick={save} disabled={busy}>
            {busy ? <Icon name="restart" className="spin" size={14} /> : <Icon name="check" size={14} />}
            Save
          </Btn>
        </div>
      </div>
      <div className="t-xs muted">Set any limit to 0 for unlimited on that axis.</div>
    </div>
  )
}

function Field({
  label,
  value,
  onChange,
  step = 1,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  step?: number
}) {
  return (
    <div className="field" style={{ width: 140 }}>
      <label className="label">{label}</label>
      <input
        className="input"
        type="number"
        min={0}
        step={step}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  )
}
