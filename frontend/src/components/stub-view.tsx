/* stub-view.tsx — tasteful placeholders for not-yet-built views, anchored to roadmap phases. */
import { Icon, type IconName } from "./icon"
import type { Route } from "./app-shell"

const STUBS: Record<string, { icon: IconName; phase: string; title: string; desc: string }> = {
  observability: {
    icon: "activity",
    phase: "P7",
    title: "Observability",
    desc: "Per-server RCON / Server-List-Ping health, player counts, MOTD, and Prometheus metrics across the fleet.",
  },
  quotas: {
    icon: "users",
    phase: "P9",
    title: "Quotas & Users",
    desc: "Per-user limits — max servers, vCPU, and memory — enforced at create/update with admin overrides.",
  },
  settings: {
    icon: "settings",
    phase: "P10",
    title: "Settings",
    desc: "Agent ↔ control-plane auth, secrets, migration status, and reconciler tuning.",
  },
}

export function StubView({ route }: { route: Route }) {
  const s = STUBS[route] || STUBS.settings
  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">{s.title}</div>
          <div className="page-sub">Part of the Craftling platform roadmap.</div>
        </div>
      </div>
      <div className="card" style={{ padding: 0, overflow: "hidden" }}>
        <div className="empty" style={{ padding: "72px 24px" }}>
          <div
            className="center"
            style={{
              width: 56,
              height: 56,
              borderRadius: 14,
              background: "var(--muted)",
              color: "var(--primary)",
            }}
          >
            <Icon name={s.icon} size={26} />
          </div>
          <div className="col" style={{ gap: 6, alignItems: "center", maxWidth: 420 }}>
            <div className="row gap-2">
              <span
                className="badge mono"
                style={{
                  color: "var(--primary)",
                  borderColor: "color-mix(in oklab, var(--primary) 40%, var(--border))",
                }}
              >
                {s.phase}
              </span>
              <span className="semibold t-lg" style={{ color: "var(--foreground)" }}>
                {s.title}
              </span>
            </div>
            <div className="t-sm" style={{ textAlign: "center", lineHeight: 1.6 }}>
              {s.desc}
            </div>
          </div>
          <div className="row gap-2">
            <span className="badge">
              <Icon name="clock" size={13} /> On the roadmap
            </span>
          </div>
        </div>
      </div>
    </div>
  )
}
