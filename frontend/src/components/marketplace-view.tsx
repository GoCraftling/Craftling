/* marketplace-view.tsx — registry gallery. Lists templates from the control
 * plane; on selection opens the dynamic config form, and on launch creates a real
 * server (the control plane resolves the image + env from the template) before
 * handing off to the Servers view to watch it provision. */
import { useCallback, useEffect, useState } from "react"
import { Icon } from "./icon"
import { Btn } from "./primitives"
import { TemplateDrawer, type TemplateLaunch } from "./template-drawer"
import { api, ApiError, type TemplateSummary } from "@/lib/api"

export function MarketplaceView({ onLaunched }: { onLaunched?: () => void }) {
  const [templates, setTemplates] = useState<TemplateSummary[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<TemplateSummary | null>(null)
  const [launching, setLaunching] = useState(false)

  // All state updates happen in promise callbacks so this is safe to call from
  // an effect (no synchronous setState in the effect body).
  const load = useCallback(() => {
    api
      .listTemplates()
      .then((t) => {
        setTemplates(t)
        setError(null)
      })
      .catch((e) =>
        setError(e instanceof ApiError ? e.message : "Couldn't reach the template registry.")
      )
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    load()
  }, [load])

  // Create the server from the template, then hand off to the Servers view. The
  // control plane resolves the image + env from the template id and answers, so
  // we send those rather than the locally resolved env.
  const onComplete = useCallback(
    (launch: TemplateLaunch) => {
      setLaunching(true)
      setError(null)
      api
        .createServer({
          name: launch.name,
          template_id: launch.templateId,
          answers: launch.answers,
          eula_accepted: launch.eulaAccepted,
          cpus: launch.cpus,
          memory_mb: launch.mem,
        })
        .then(() => {
          setSelected(null)
          onLaunched?.()
        })
        .catch((e) =>
          setError(e instanceof ApiError ? e.message : "Couldn't launch this template.")
        )
        .finally(() => setLaunching(false))
    },
    [onLaunched]
  )

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">Marketplace</div>
          <div className="page-sub">
            Launch a game server from a template. Answer its questions and Craftling fills the
            container environment.
          </div>
        </div>
        <Btn variant="outline" onClick={load}>
          <Icon name="refresh" size={15} /> Refresh
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
          <button className="icon-btn sm" onClick={load} style={{ marginLeft: "auto" }}>
            <Icon name="restart" size={14} />
          </button>
        </div>
      )}

      <div
        style={{
          display: "grid",
          gridTemplateColumns: "repeat(auto-fill, minmax(240px, 1fr))",
          gap: 14,
        }}
      >
        {templates.map((t) => (
          <button
            key={t.template_id}
            className="card selectable"
            onClick={() => setSelected(t)}
            style={{
              padding: 0,
              overflow: "hidden",
              textAlign: "left",
              cursor: "pointer",
              display: "flex",
              flexDirection: "column",
            }}
          >
            <div
              style={{
                aspectRatio: "16 / 9",
                background: "var(--muted)",
                display: "grid",
                placeItems: "center",
              }}
            >
              {t.thumbnail_url ? (
                <img
                  src={t.thumbnail_url}
                  alt=""
                  style={{ width: "100%", height: "100%", objectFit: "cover" }}
                />
              ) : (
                <Icon name="cube" size={28} style={{ opacity: 0.5 }} />
              )}
            </div>
            <div className="between" style={{ padding: "11px 13px", alignItems: "center" }}>
              <span className="semibold t-sm">{t.template_name}</span>
              <Icon name="chevRight" size={16} style={{ color: "var(--muted-foreground)" }} />
            </div>
          </button>
        ))}
      </div>

      {!templates.length && (
        <div className="card">
          <div className="empty">
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading templates…</div>
              </>
            ) : (
              <>
                <Icon name="globe" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    No templates available
                  </div>
                  <div className="t-sm">The registry index returned no entries.</div>
                </div>
              </>
            )}
          </div>
        </div>
      )}

      {selected && (
        <TemplateDrawer
          summary={selected}
          launching={launching}
          onClose={() => setSelected(null)}
          onComplete={onComplete}
        />
      )}
    </div>
  )
}
