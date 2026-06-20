/* hosts-view.tsx — Host Fleet view (admin-only): worker-host inventory with
 * capacity utilization, zone, agent version, and heartbeat liveness, polled live. */
import { useCallback, useEffect, useState } from "react"
import { Icon } from "./icon"
import { Btn, CopyBtn, Meter, StatusBadge } from "./primitives"
import { api, ApiError, type ApiHost, type ApiHostStatus } from "@/lib/api"
import { fmtMem } from "@/lib/data"

const POLL_MS = 2500

// A host that hasn't checked in within this window is treated as stale even if
// the control plane hasn't yet flipped it to "down".
const STALE_MS = 30_000

const FILTERS: { id: string; label: string; match?: (h: ApiHost) => boolean }[] = [
  { id: "all", label: "All" },
  { id: "ready", label: "Ready", match: (h) => h.status === "ready" },
  { id: "draining", label: "Draining", match: (h) => h.status === "draining" },
  { id: "down", label: "Down", match: (h) => h.status === "down" },
]

// Allocated capacity = physical total minus what's still allocatable.
const cpuUsed = (h: ApiHost) => Math.max(0, h.cpus_total - h.cpus_allocatable)
const memUsed = (h: ApiHost) => Math.max(0, h.memory_mb_total - h.memory_mb_allocatable)

function ago(iso: string): { label: string; stale: boolean } {
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return { label: "—", stale: true }
  const ms = Date.now() - t
  const stale = ms > STALE_MS
  const secs = Math.max(0, Math.floor(ms / 1000))
  if (secs < 60) return { label: `${secs}s ago`, stale }
  const mins = Math.floor(secs / 60)
  if (mins < 60) return { label: `${mins}m ago`, stale }
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return { label: `${hrs}h ago`, stale }
  return { label: `${Math.floor(hrs / 24)}d ago`, stale }
}

export function HostsView() {
  const [hosts, setHosts] = useState<ApiHost[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [filter, setFilter] = useState("all")
  const [q, setQ] = useState("")

  const refresh = useCallback(() => {
    api
      .adminListHosts()
      .then((list) => {
        setHosts(list)
        setError(null)
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't reach the control plane.")
      )
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    refresh()
    const t = setInterval(refresh, POLL_MS)
    return () => clearInterval(t)
  }, [refresh])

  // filter + query
  const fdef = FILTERS.find((f) => f.id === filter)!
  let list = hosts.filter((h) => filter === "all" || (fdef.match && fdef.match(h)))
  if (q.trim()) {
    const t = q.toLowerCase()
    list = list.filter(
      (h) =>
        h.hostname.toLowerCase().includes(t) ||
        h.zone.toLowerCase().includes(t) ||
        h.address.toLowerCase().includes(t)
    )
  }

  // fleet stats
  const ready = hosts.filter((h) => h.status === "ready")
  const cpuTotal = hosts.reduce((a, h) => a + h.cpus_total, 0)
  const cpuAlloc = hosts.reduce((a, h) => a + cpuUsed(h), 0)
  const memTotal = hosts.reduce((a, h) => a + h.memory_mb_total, 0)
  const memAlloc = hosts.reduce((a, h) => a + memUsed(h), 0)

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">Host Fleet</div>
          <div className="page-sub">
            Worker-host inventory — capacity, zone, agent version, and heartbeat liveness.
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
            <Icon name="hosts" size={14} /> Total hosts
          </div>
          <div className="v tnum">{hosts.length}</div>
          <div className="sub">
            {hosts.filter((h) => h.status === "draining").length} draining ·{" "}
            {hosts.filter((h) => h.status === "down").length} down
          </div>
        </div>
        <div className="card stat">
          <div className="k">
            <i className="dot" style={{ background: "var(--success)" }} /> Ready
          </div>
          <div className="v tnum" style={{ color: "var(--success-fg)" }}>
            {ready.length}
          </div>
          <div className="sub">eligible for placement</div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="cpu" size={14} /> vCPU capacity
          </div>
          <div className="v tnum">{cpuTotal}</div>
          <div className="sub">
            {cpuAlloc} allocated · {cpuTotal - cpuAlloc} free
          </div>
        </div>
        <div className="card stat">
          <div className="k">
            <Icon name="mem" size={14} /> Memory capacity
          </div>
          <div className="v tnum">{fmtMem(memTotal)}</div>
          <div className="sub">
            {fmtMem(memAlloc)} allocated · {fmtMem(Math.max(0, memTotal - memAlloc))} free
          </div>
        </div>
      </div>

      {/* toolbar */}
      <div className="between" style={{ gap: 12, flexWrap: "wrap" }}>
        <div className="row gap-2 wrap">
          {FILTERS.map((f) => {
            const n =
              f.id === "all" ? hosts.length : hosts.filter((h) => f.match && f.match(h)).length
            return (
              <button
                key={f.id}
                className={"chip" + (filter === f.id ? " on" : "")}
                onClick={() => setFilter(f.id)}
              >
                {f.label} <span className="num">{n}</span>
              </button>
            )
          })}
        </div>
        <div className="input-wrap" style={{ width: 240 }}>
          <Icon name="search" />
          <input
            className="input"
            placeholder="Search host, zone, address"
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
                <th>Host</th>
                <th>Status</th>
                <th>Zone</th>
                <th>vCPU</th>
                <th>Memory</th>
                <th>Agent</th>
                <th>Heartbeat</th>
                <th>Address</th>
              </tr>
            </thead>
            <tbody>
              {list.map((h) => (
                <HostRow key={h.id} h={h} />
              ))}
            </tbody>
          </table>
        </div>
        {!list.length && (
          <div className="empty">
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading hosts…</div>
              </>
            ) : (
              <>
                <Icon name="hosts" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    {hosts.length ? "No hosts match" : "No hosts registered"}
                  </div>
                  <div className="t-sm">
                    {hosts.length
                      ? "Try a different filter or search."
                      : "Hosts appear here once an agent registers and starts sending heartbeats."}
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

function HostRow({ h }: { h: ApiHost }) {
  const hb = ago(h.last_heartbeat_at)
  const status = h.status as ApiHostStatus
  return (
    <tr>
      <td>
        <div className="col" style={{ gap: 2 }}>
          <span className="semibold">{h.hostname}</span>
          <span className="mono t-xs muted">{h.id}</span>
        </div>
      </td>
      <td>
        <StatusBadge state={status} />
      </td>
      <td>
        {h.zone ? <span className="badge mono">{h.zone}</span> : <span className="muted">—</span>}
      </td>
      <td>
        <div className="col" style={{ gap: 3, minWidth: 92 }}>
          <span className="mono t-sm tnum">
            {cpuUsed(h)}
            <span className="muted">/{h.cpus_total}</span>
          </span>
          <Meter value={cpuUsed(h)} max={h.cpus_total || 1} />
        </div>
      </td>
      <td>
        <div className="col" style={{ gap: 3, minWidth: 110 }}>
          <span className="mono t-sm tnum">
            {fmtMem(memUsed(h))}
            <span className="muted">/{fmtMem(h.memory_mb_total)}</span>
          </span>
          <Meter value={memUsed(h)} max={h.memory_mb_total || 1} />
        </div>
      </td>
      <td>
        {h.agent_version ? (
          <span className="badge mono">{h.agent_version}</span>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td>
        <span
          className="row gap-1 mono t-sm"
          style={hb.stale ? { color: "var(--danger-fg)" } : undefined}
        >
          <i
            className={"dot" + (hb.stale ? "" : " pulse")}
            style={{ background: hb.stale ? "var(--danger)" : "var(--success)" }}
          />
          {hb.label}
        </span>
      </td>
      <td>
        {h.address ? (
          <div className="row gap-1">
            <span className="mono t-sm truncate" style={{ maxWidth: 160 }}>
              {h.address}
            </span>
            <CopyBtn text={h.address} />
          </div>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
    </tr>
  )
}
