/* drawers.tsx — ServerDrawer (detail) + CreateDrawer. */
import { Fragment, useCallback, useEffect, useState, type ReactNode } from "react"
import { Icon } from "./icon"
import { Btn, CopyBtn, StatusBadge } from "./primitives"
import { SIZES, MAX_HOST_CPU, MAX_HOST_MEM } from "./servers-shared"
import { api, ApiError } from "@/lib/api"
import {
  MC_VERSIONS,
  fmtMem,
  fmtWorld,
  type Owner,
  type Server,
  type ServerStatus,
} from "@/lib/data"

export interface CreateSpec {
  name: string
  version: string
  cpus: number
  mem: number
}

function Row2({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div
      className="between"
      style={{ padding: "9px 0", borderBottom: "1px solid var(--border)" }}
    >
      <span className="t-sm muted">{k}</span>
      <span className="t-sm" style={{ textAlign: "right" }}>
        {children}
      </span>
    </div>
  )
}

// ServerLogs fetches and renders a server's captured console output on demand.
// It reads through the owner endpoint when the viewer owns the server, or the
// admin endpoint otherwise — mirroring the list views' owner/admin split.
function ServerLogs({ serverId, isOwner }: { serverId: string; isOwner: boolean }) {
  const [logs, setLogs] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  // All state updates happen in promise callbacks so this is safe to call from
  // an effect (no synchronous setState in the effect body), matching the
  // marketplace/hosts views' fetch pattern.
  const load = useCallback(() => {
    const fetch = isOwner ? api.getServerLogs(serverId) : api.adminGetServerLogs(serverId)
    fetch
      .then((text) => {
        setLogs(text)
        setError(null)
      })
      .catch((e) => setError(e instanceof ApiError ? e.message : "could not load logs"))
      .finally(() => setLoading(false))
  }, [serverId, isOwner])

  useEffect(() => {
    load()
  }, [load])

  const refresh = () => {
    setLoading(true)
    load()
  }

  return (
    <div>
      <div className="between" style={{ marginBottom: 9 }}>
        <div className="row gap-2 t-xs upper muted">
          <Icon name="activity" size={13} /> Logs
        </div>
        <Btn variant="outline" size="sm" onClick={refresh} disabled={loading}>
          <Icon name="refresh" size={13} className={loading ? "spin" : undefined} /> Refresh
        </Btn>
      </div>
      {error ? (
        <div className="row gap-2 t-sm" style={{ color: "var(--danger-fg)", alignItems: "flex-start" }}>
          <Icon name="alert" size={15} style={{ marginTop: 1, flex: "none" }} />
          <span>{error}</span>
        </div>
      ) : (
        <pre
          className="mono t-xs"
          style={{
            margin: 0,
            maxHeight: 320,
            overflow: "auto",
            whiteSpace: "pre-wrap",
            wordBreak: "break-word",
            padding: "10px 12px",
            borderRadius: 8,
            border: "1px solid var(--border)",
            background: "var(--muted)",
            color: "var(--foreground)",
          }}
        >
          {loading && logs === null ? "Loading…" : logs ? logs : "No logs yet."}
        </pre>
      )}
    </div>
  )
}

const TRANSITIONING: ServerStatus[] = ["scheduling", "provisioning", "starting", "stopping"]
const CAN_START: ServerStatus[] = ["stopped", "error", "unschedulable"]
const CAN_STOP: ServerStatus[] = ["running", "starting", "provisioning"]

export function ServerDrawer({
  s,
  isOwner,
  owner,
  onClose,
  onStart,
  onStop,
  onRestart,
  onDelete,
}: {
  s: Server
  isOwner: boolean
  owner?: Owner | null
  onClose: () => void
  onStart: () => void
  onStop: () => void
  onRestart: () => void
  onDelete: () => void
}) {
  const transitioning = TRANSITIONING.includes(s.status)
  const canStart = CAN_START.includes(s.status)
  const canStop = CAN_STOP.includes(s.status)

  const lifecycle: ServerStatus[] = ["stopped", "scheduling", "provisioning", "starting", "running"]
  const curIdx = s.status === "running" ? 4 : lifecycle.indexOf(s.status)

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <div className="drawer">
        <div className="drawer-head">
          <div className="row gap-3">
            <div
              className="avatar"
              style={{ borderRadius: 7, background: "var(--muted)", color: "var(--primary)" }}
            >
              <Icon name="cube" size={16} />
            </div>
            <div className="col" style={{ gap: 1 }}>
              <span className="semibold t-md">{s.name}</span>
              <span className="mono t-xs muted">{s.id}</span>
            </div>
          </div>
          <button className="icon-btn" onClick={onClose}>
            <Icon name="x" />
          </button>
        </div>

        <div
          className="drawer-body"
          style={{ display: "flex", flexDirection: "column", gap: 18 }}
        >
          {/* status + desired vs observed */}
          <div className="card pad" style={{ display: "flex", flexDirection: "column", gap: 12 }}>
            <div className="between">
              <StatusBadge state={s.status} />
              <span className="badge mono">{s.version}</span>
            </div>
            {s.statusMessage && (
              <div
                className="row gap-2 t-sm"
                style={{
                  color:
                    s.status === "error" || s.status === "unschedulable"
                      ? "var(--danger-fg)"
                      : "var(--muted-foreground)",
                  alignItems: "flex-start",
                }}
              >
                <Icon
                  name={s.status === "error" || s.status === "unschedulable" ? "alert" : "clock"}
                  size={15}
                  style={{ marginTop: 1, flex: "none" }}
                />
                <span>{s.statusMessage}</span>
              </div>
            )}
            {/* desired vs observed */}
            <div className="row gap-2" style={{ marginTop: 2 }}>
              <div
                className="card"
                style={{ flex: 1, padding: "9px 11px", boxShadow: "none", background: "var(--muted)" }}
              >
                <div className="t-xs muted">Desired</div>
                <div
                  className="row gap-1 semibold t-sm"
                  style={{ textTransform: "capitalize" }}
                >
                  <i
                    className="dot"
                    style={{
                      background:
                        s.desired === "running" ? "var(--success)" : "var(--neutral-dot)",
                    }}
                  />
                  {s.desired}
                </div>
              </div>
              <Icon name="chevRight" size={16} style={{ color: "var(--muted-foreground)" }} />
              <div
                className="card"
                style={{ flex: 1, padding: "9px 11px", boxShadow: "none", background: "var(--muted)" }}
              >
                <div className="t-xs muted">Observed</div>
                <div
                  className="row gap-1 semibold t-sm"
                  style={{ textTransform: "capitalize" }}
                >
                  <i
                    className={"dot" + (transitioning ? " pulse" : "")}
                    style={{
                      background:
                        s.status === "running"
                          ? "var(--success)"
                          : ["error", "unschedulable"].includes(s.status)
                            ? "var(--danger)"
                            : transitioning
                              ? "var(--warning)"
                              : "var(--neutral-dot)",
                    }}
                  />
                  {s.status}
                </div>
              </div>
            </div>
          </div>

          {/* lifecycle stepper */}
          <div>
            <div className="t-xs upper muted" style={{ marginBottom: 9 }}>
              Lifecycle
            </div>
            <div className="row" style={{ gap: 0 }}>
              {lifecycle.map((st, i) => (
                <Fragment key={st}>
                  <div className="col" style={{ alignItems: "center", gap: 5, flex: "none" }}>
                    <div
                      className="center"
                      style={{
                        width: 22,
                        height: 22,
                        borderRadius: 6,
                        fontSize: 11,
                        background: i <= curIdx ? "var(--primary)" : "var(--muted)",
                        color: i <= curIdx ? "var(--primary-foreground)" : "var(--muted-foreground)",
                        border: "1px solid " + (i <= curIdx ? "transparent" : "var(--border)"),
                      }}
                    >
                      {i < curIdx ? (
                        <Icon name="check" size={12} />
                      ) : i === curIdx && transitioning ? (
                        <Icon name="restart" size={12} className="spin" />
                      ) : (
                        i + 1
                      )}
                    </div>
                    <span
                      className="t-xs"
                      style={{
                        color: i <= curIdx ? "var(--foreground)" : "var(--muted-foreground)",
                        textTransform: "capitalize",
                      }}
                    >
                      {st}
                    </span>
                  </div>
                  {i < lifecycle.length - 1 && (
                    <div
                      style={{
                        flex: 1,
                        height: 2,
                        background: i < curIdx ? "var(--primary)" : "var(--border)",
                        margin: "0 4px",
                        marginBottom: 18,
                      }}
                    />
                  )}
                </Fragment>
              ))}
            </div>
          </div>

          {/* details */}
          <div>
            <div className="t-xs upper muted" style={{ marginBottom: 4 }}>
              Placement & resources
            </div>
            {!isOwner && (
              <Row2 k="Owner">
                {owner ? (
                  <span className="row gap-2" style={{ justifyContent: "flex-end" }}>
                    {owner.name}
                  </span>
                ) : (
                  "—"
                )}
              </Row2>
            )}
            <Row2 k="Host">
              {s.address ? (
                <span className="mono">{s.address}</span>
              ) : (
                <span className="muted">unplaced</span>
              )}
            </Row2>
            <Row2 k="vCPU / Memory">
              <span className="mono">
                {s.cpus} vCPU · {fmtMem(s.mem)}
              </span>
            </Row2>
            <Row2 k="World storage">
              <span className="mono">{s.world ? fmtWorld(s.world) : "—"}</span>{" "}
              <span className="muted t-xs">· S3</span>
            </Row2>
            <Row2 k="Created">{s.createdDays === 0 ? "just now" : `${s.createdDays}d ago`}</Row2>
          </div>

          <div>
            <div className="t-xs upper muted" style={{ marginBottom: 4 }}>
              Access & health
            </div>
            <Row2 k="Address">
              {s.address ? (
                <span className="row gap-1" style={{ justifyContent: "flex-end" }}>
                  <span className="mono">
                    {s.address}:{s.port}
                  </span>
                  <CopyBtn text={`${s.address}:${s.port}`} />
                </span>
              ) : (
                <span className="muted">—</span>
              )}
            </Row2>
            <Row2 k="Players">
              {s.status === "running" && s.maxPlayers > 0 ? (
                <span className="mono">
                  {s.players} / {s.maxPlayers}
                </span>
              ) : (
                <span className="muted">—</span>
              )}
            </Row2>
            <Row2 k="Health (RCON)">
              <span className="muted">—</span>
            </Row2>
          </div>

          {/* logs */}
          <ServerLogs serverId={s.id} isOwner={isOwner} />

          {/* danger */}
          <div
            className="card pad"
            style={{
              borderColor: "color-mix(in oklab, var(--danger) 30%, var(--border))",
              display: "flex",
              flexDirection: "column",
              gap: 10,
            }}
          >
            <div className="between">
              <div className="col" style={{ gap: 1 }}>
                <span className="semibold t-sm">Delete server</span>
                <span className="t-xs muted">Soft-delete; world archive retained for 30d.</span>
              </div>
              <Btn variant="danger" size="sm" onClick={onDelete}>
                <Icon name="trash" size={13} /> Delete
              </Btn>
            </div>
          </div>
        </div>

        <div className="drawer-foot">
          <Btn variant="outline" onClick={onRestart}>
            <Icon name="restart" size={15} /> Restart
          </Btn>
          {canStop ? (
            <Btn variant="outline" onClick={onStop} disabled={s.status === "stopping"}>
              <Icon name="stop" size={14} /> Stop
            </Btn>
          ) : (
            <Btn variant="primary" onClick={onStart} disabled={!canStart}>
              <Icon name="play" size={14} /> Start
            </Btn>
          )}
        </div>
      </div>
    </>
  )
}

export function CreateDrawer({
  onClose,
  onCreate,
}: {
  onClose: () => void
  onCreate: (spec: CreateSpec) => void
}) {
  const [name, setName] = useState("")
  const [version, setVersion] = useState(MC_VERSIONS[1])
  const [size, setSize] = useState("medium")
  const sz = SIZES.find((x) => x.id === size)!
  const oversize = sz.cpus > MAX_HOST_CPU || sz.mem > MAX_HOST_MEM
  const valid = name.trim().length >= 3

  const submit = () => {
    if (!valid) return
    onCreate({
      name: name.trim().toLowerCase().replace(/\s+/g, "-"),
      version,
      cpus: sz.cpus,
      mem: sz.mem,
    })
  }

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <div className="drawer">
        <div className="drawer-head">
          <div className="row gap-3">
            <div className="avatar" style={{ borderRadius: 7 }}>
              <Icon name="plus" size={16} />
            </div>
            <span className="semibold t-md">New game server</span>
          </div>
          <button className="icon-btn" onClick={onClose}>
            <Icon name="x" />
          </button>
        </div>

        <div className="drawer-body" style={{ display: "flex", flexDirection: "column", gap: 18 }}>
          <div className="field">
            <label className="label">Server name</label>
            <input
              className="input"
              placeholder="e.g. survival-smp"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoFocus
            />
            <span className="t-xs muted">Lowercase, hyphenated. Min 3 characters.</span>
          </div>

          <div className="field">
            <label className="label">Minecraft version</label>
            <div className="input-wrap">
              <select
                className="select"
                value={version}
                onChange={(e) => setVersion(e.target.value)}
              >
                {MC_VERSIONS.map((v) => (
                  <option key={v} value={v}>
                    {v}
                  </option>
                ))}
              </select>
              <Icon
                name="chevDown"
                size={14}
                style={{ position: "absolute", right: 10, left: "auto", color: "var(--muted-foreground)" }}
              />
            </div>
          </div>

          <div className="field">
            <label className="label">Size</label>
            <div className="col" style={{ gap: 8 }}>
              {SIZES.map((x) => {
                const big = x.cpus > MAX_HOST_CPU || x.mem > MAX_HOST_MEM
                return (
                  <button
                    key={x.id}
                    onClick={() => setSize(x.id)}
                    className="card"
                    style={{
                      padding: "11px 13px",
                      display: "flex",
                      alignItems: "center",
                      gap: 12,
                      cursor: "pointer",
                      textAlign: "left",
                      borderColor: size === x.id ? "var(--primary)" : "var(--border)",
                      boxShadow: size === x.id ? "0 0 0 3px oklch(0.62 0.16 150 / 0.16)" : "none",
                      background: "var(--card)",
                    }}
                  >
                    <div
                      className="center"
                      style={{
                        width: 18,
                        height: 18,
                        borderRadius: 5,
                        border: "1px solid " + (size === x.id ? "var(--primary)" : "var(--border)"),
                        background: size === x.id ? "var(--primary)" : "transparent",
                        color: "var(--primary-foreground)",
                        flex: "none",
                      }}
                    >
                      {size === x.id && <Icon name="check" size={12} />}
                    </div>
                    <div className="col grow" style={{ gap: 1 }}>
                      <span className="semibold t-sm">
                        {x.label}{" "}
                        <span className="muted" style={{ fontWeight: 400 }}>
                          · {x.hint}
                        </span>
                      </span>
                      <span className="mono t-xs muted">
                        {x.cpus} vCPU · {fmtMem(x.mem)}
                      </span>
                    </div>
                    {big && (
                      <span className="badge soft s-error">
                        <i className="dot" /> no fit
                      </span>
                    )}
                  </button>
                )
              })}
            </div>
          </div>

          {oversize && (
            <div
              className="row gap-2 t-sm"
              style={{
                color: "var(--danger-fg)",
                background: "color-mix(in oklab, var(--danger) 10%, transparent)",
                padding: "10px 12px",
                borderRadius: "var(--radius)",
                alignItems: "flex-start",
              }}
            >
              <Icon name="alert" size={15} style={{ marginTop: 1, flex: "none" }} />
              <span>
                This spec exceeds every ready host ({MAX_HOST_CPU} vCPU / {fmtMem(MAX_HOST_MEM)}{" "}
                max). The scheduler will mark it <b>unschedulable</b> — kept here to demo the
                rejection path.
              </span>
            </div>
          )}
        </div>

        <div className="drawer-foot">
          <Btn variant="ghost" onClick={onClose}>
            Cancel
          </Btn>
          <Btn variant="primary" onClick={submit} disabled={!valid}>
            <Icon name="bolt" size={15} /> Create & start
          </Btn>
        </div>
      </div>
    </>
  )
}
