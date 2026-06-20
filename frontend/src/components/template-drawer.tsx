/* template-drawer.tsx — fetches a template manifest, renders its variables as a
 * dynamic form, enforces EULA acceptance, and live-resolves the env. The
 * resolved env is the hand-off point for the future provisioning step. */
import { useEffect, useMemo, useState } from "react"
import { Icon } from "./icon"
import { Btn } from "./primitives"
import { SIZES, MAX_HOST_CPU, MAX_HOST_MEM } from "./servers-shared"
import {
  api,
  resolveEnv,
  ApiError,
  type TemplateManifest,
  type TemplateSummary,
} from "@/lib/api"

export interface TemplateLaunch {
  name: string
  templateId: string
  manifest: TemplateManifest
  answers: Record<string, string>
  env: Record<string, string>
  eulaAccepted: boolean
  cpus: number
  mem: number
}

export function TemplateDrawer({
  summary,
  launching = false,
  onClose,
  onComplete,
}: {
  summary: TemplateSummary
  launching?: boolean
  onClose: () => void
  onComplete: (launch: TemplateLaunch) => void
}) {
  const [manifest, setManifest] = useState<TemplateManifest | null>(null)
  const [error, setError] = useState<string | null>(null)

  const [name, setName] = useState("")
  const [answers, setAnswers] = useState<Record<string, string>>({})
  const [eula, setEula] = useState(false)
  const [size, setSize] = useState("medium")
  const sz = SIZES.find((x) => x.id === size)!

  // Fetch the manifest and seed each variable with its first acceptable answer.
  useEffect(() => {
    let live = true
    api
      .getTemplate(summary.template_id)
      .then((m) => {
        if (!live) return
        setManifest(m)
        setAnswers(
          Object.fromEntries(
            m.variables.map((v) => [v.name, v.acceptable_answers[0] ?? ""])
          )
        )
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't load this template.")
      )
    return () => {
      live = false
    }
  }, [summary.template_id])

  const resolved = useMemo(
    () => (manifest ? resolveEnv(manifest.env, answers) : {}),
    [manifest, answers]
  )

  const nameValid = name.trim().length >= 3
  const eulaOk = !manifest?.eula_needed || eula
  const oversize = sz.cpus > MAX_HOST_CPU || sz.mem > MAX_HOST_MEM
  const valid = !!manifest && nameValid && eulaOk && !oversize

  const submit = () => {
    if (!valid || !manifest) return
    onComplete({
      name: name.trim().toLowerCase().replace(/\s+/g, "-"),
      templateId: summary.template_id,
      manifest,
      answers,
      env: resolved,
      eulaAccepted: eula,
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
            <div
              className="avatar"
              style={{ borderRadius: 7, overflow: "hidden", padding: 0 }}
            >
              {summary.thumbnail_url ? (
                <img
                  src={summary.thumbnail_url}
                  alt=""
                  style={{ width: "100%", height: "100%", objectFit: "cover" }}
                />
              ) : (
                <Icon name="cube" size={16} />
              )}
            </div>
            <div className="col" style={{ gap: 1 }}>
              <span className="semibold t-md">{summary.template_name}</span>
              <span className="mono t-xs muted">
                {manifest ? `${manifest.image_name}:${manifest.image_tag}` : "loading…"}
              </span>
            </div>
          </div>
          <button className="icon-btn" onClick={onClose}>
            <Icon name="x" />
          </button>
        </div>

        <div className="drawer-body" style={{ display: "flex", flexDirection: "column", gap: 18 }}>
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
            </div>
          )}

          {!manifest && !error && (
            <div className="row gap-2 muted t-sm" style={{ padding: "8px 0" }}>
              <Icon name="restart" className="spin" size={16} /> Loading template…
            </div>
          )}

          {manifest && (
            <>
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
                <label className="label">Size</label>
                <div className="input-wrap">
                  <select
                    className="select"
                    value={size}
                    onChange={(e) => setSize(e.target.value)}
                  >
                    {SIZES.map((x) => (
                      <option key={x.id} value={x.id}>
                        {x.label} — {x.cpus} vCPU · {Math.round(x.mem / 1024)} GB ({x.hint})
                      </option>
                    ))}
                  </select>
                  <Icon
                    name="chevDown"
                    size={14}
                    style={{
                      position: "absolute",
                      right: 10,
                      left: "auto",
                      color: "var(--muted-foreground)",
                    }}
                  />
                </div>
                {(sz.cpus > MAX_HOST_CPU || sz.mem > MAX_HOST_MEM) && (
                  <span className="t-xs" style={{ color: "var(--danger-fg)" }}>
                    Exceeds the capacity of any host in the fleet.
                  </span>
                )}
              </div>

              {manifest.variables.map((v) => (
                <div className="field" key={v.name}>
                  <label className="label">{v.name}</label>
                  {v.acceptable_answers.length > 0 ? (
                    <div className="input-wrap">
                      <select
                        className="select"
                        value={answers[v.name] ?? ""}
                        onChange={(e) =>
                          setAnswers((a) => ({ ...a, [v.name]: e.target.value }))
                        }
                      >
                        {v.acceptable_answers.map((opt) => (
                          <option key={opt} value={opt}>
                            {opt}
                          </option>
                        ))}
                      </select>
                      <Icon
                        name="chevDown"
                        size={14}
                        style={{
                          position: "absolute",
                          right: 10,
                          left: "auto",
                          color: "var(--muted-foreground)",
                        }}
                      />
                    </div>
                  ) : (
                    <input
                      className="input"
                      value={answers[v.name] ?? ""}
                      onChange={(e) =>
                        setAnswers((a) => ({ ...a, [v.name]: e.target.value }))
                      }
                    />
                  )}
                  {v.description && <span className="t-xs muted">{v.description}</span>}
                </div>
              ))}

              {manifest.guest_volumes.length > 0 && (
                <div className="field">
                  <label className="label">Persistent volumes</label>
                  <div className="row gap-2 wrap">
                    {manifest.guest_volumes.map((vol) => (
                      <span className="badge mono" key={vol}>
                        {vol}
                      </span>
                    ))}
                  </div>
                </div>
              )}

              {manifest.eula_needed && (
                <label
                  className="row gap-2"
                  style={{
                    alignItems: "flex-start",
                    cursor: "pointer",
                    padding: "11px 13px",
                    border: "1px solid " + (eula ? "var(--primary)" : "var(--border)"),
                    borderRadius: "var(--radius)",
                    background: "var(--card)",
                  }}
                >
                  <input
                    type="checkbox"
                    checked={eula}
                    onChange={(e) => setEula(e.target.checked)}
                    style={{ marginTop: 2, accentColor: "var(--primary)" }}
                  />
                  <span className="t-sm">
                    I accept the{" "}
                    <a
                      href="https://www.minecraft.net/eula"
                      target="_blank"
                      rel="noreferrer"
                      style={{ color: "var(--primary)" }}
                    >
                      Minecraft EULA
                    </a>
                    . Required to run this server.
                  </span>
                </label>
              )}

              {/* Live resolved env — the hand-off to the future init/rootfs step. */}
              <div>
                <div className="t-xs upper muted" style={{ marginBottom: 6 }}>
                  Resolved environment
                </div>
                <div
                  className="card"
                  style={{
                    padding: "10px 12px",
                    background: "var(--muted)",
                    boxShadow: "none",
                    fontFamily: "var(--font-mono, monospace)",
                    fontSize: 12,
                    lineHeight: 1.7,
                    overflowX: "auto",
                  }}
                >
                  {Object.entries(resolved).map(([k, val]) => (
                    <div key={k}>
                      <span style={{ color: "var(--primary)" }}>{k}</span>
                      <span className="muted">=</span>
                      <span>{val}</span>
                    </div>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>

        <div className="drawer-foot">
          <Btn variant="ghost" onClick={onClose} disabled={launching}>
            Cancel
          </Btn>
          <Btn variant="primary" onClick={submit} disabled={!valid || launching}>
            {launching ? (
              <>
                <Icon name="restart" className="spin" size={15} /> Launching…
              </>
            ) : (
              <>
                <Icon name="bolt" size={15} /> Configure & launch
              </>
            )}
          </Btn>
        </div>
      </div>
    </>
  )
}
