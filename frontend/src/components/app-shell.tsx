/* app-shell.tsx — sidebar + topbar. Role-gated nav, real session identity. */
/* eslint-disable react-refresh/only-export-components */
import type { ReactNode } from "react"
import { Icon, Voxel, type IconName } from "./icon"
import { Menu } from "./primitives"
import { useAuth } from "@/lib/auth"
import type { Role } from "@/lib/data"

export type Route =
  | "servers"
  | "marketplace"
  | "hosts"
  | "observability"
  | "quotas"
  | "settings"

interface NavItem {
  id: Route
  label: string
  icon: IconName
  roles: Role[]
}

export const NAV: { group: string; items: NavItem[] }[] = [
  {
    group: "Operate",
    items: [
      { id: "servers", label: "Game Servers", icon: "cube", roles: ["operator", "owner"] },
      { id: "marketplace", label: "Marketplace", icon: "globe", roles: ["operator", "owner"] },
      { id: "hosts", label: "Host Fleet", icon: "hosts", roles: ["operator"] },
    ],
  },
  {
    group: "Insight",
    items: [
      { id: "observability", label: "Observability", icon: "activity", roles: ["operator", "owner"] },
      { id: "quotas", label: "Quotas & Users", icon: "users", roles: ["operator"] },
    ],
  },
  {
    group: "System",
    items: [{ id: "settings", label: "Settings", icon: "settings", roles: ["operator", "owner"] }],
  },
]

/** First two alphanumerics of the email local-part, e.g. anya@… → "AN". */
function initialsOf(email: string): string {
  const local = email.split("@")[0].replace(/[^a-z0-9]/gi, "")
  return (local.slice(0, 2) || "??").toUpperCase()
}

function Sidebar({
  route,
  setRoute,
  role,
  counts,
}: {
  route: Route
  setRoute: (r: Route) => void
  role: Role
  counts: Partial<Record<Route, number>>
}) {
  return (
    <aside className="sidebar">
      <div className="brand">
        <Voxel />
        <span className="name">Craftling</span>
        <span
          className="badge"
          style={{ marginLeft: "auto", height: 20, padding: "0 6px", fontSize: 10 }}
        >
          <i className="dot" style={{ background: "var(--success)", width: 6, height: 6 }} />{" "}
          live
        </span>
      </div>
      <nav className="nav">
        {NAV.map((g) => {
          const items = g.items.filter((i) => i.roles.includes(role))
          if (!items.length) return null
          return (
            <div key={g.group}>
              <div className="nav-group">{g.group}</div>
              {items.map((i) => (
                <div
                  key={i.id}
                  className={"nav-item" + (route === i.id ? " active" : "")}
                  onClick={() => setRoute(i.id)}
                >
                  <Icon name={i.icon} />
                  <span>{i.label}</span>
                  {counts[i.id] != null && <span className="count">{counts[i.id]}</span>}
                </div>
              ))}
            </div>
          )
        })}
      </nav>
      <div style={{ padding: 10, borderTop: "1px solid var(--border)" }}>
        <div
          className="card"
          style={{
            padding: 11,
            display: "flex",
            gap: 10,
            alignItems: "center",
            boxShadow: "none",
            background: "var(--muted)",
          }}
        >
          <Icon name="bolt" size={16} style={{ color: "var(--primary)" }} />
          <div className="col" style={{ gap: 1 }}>
            <div className="t-xs semibold">Reconciler</div>
            <div className="t-xs muted">healthy · 2s loop</div>
          </div>
          <i className="dot pulse" style={{ background: "var(--success)", marginLeft: "auto" }} />
        </div>
      </div>
    </aside>
  )
}

function Topbar({
  route,
  role,
  theme,
  toggleTheme,
}: {
  route: Route
  role: Role
  theme: "dark" | "light"
  toggleTheme: () => void
}) {
  const { user, logout } = useAuth()
  const titles: Record<Route, string> = {
    servers: role === "owner" ? "My Servers" : "Game Servers",
    marketplace: "Marketplace",
    hosts: "Host Fleet",
    observability: "Observability",
    quotas: "Quotas & Users",
    settings: "Settings",
  }
  const email = user?.email ?? ""
  const initials = email ? initialsOf(email) : "??"
  return (
    <header className="topbar">
      <div className="row gap-2">
        <span className="semibold t-md">{titles[route]}</span>
      </div>

      <div className="grow" />

      <div className="input-wrap" style={{ width: 200 }}>
        <Icon name="search" />
        <input className="input" placeholder="Search…" />
      </div>

      <button className="icon-btn" onClick={toggleTheme} title="Toggle theme">
        <Icon name={theme === "dark" ? "sun" : "moon"} />
      </button>

      <div style={{ width: 1, height: 24, background: "var(--border)" }} />

      <Menu
        align="right"
        width={240}
        trigger={(_open, t) => (
          <button
            className="row gap-2"
            onClick={t}
            style={{ background: "none", border: "none", cursor: "pointer", padding: 2 }}
          >
            <div className="avatar">{initials}</div>
            <div className="col" style={{ gap: 0, alignItems: "flex-start" }}>
              <span className="t-sm semibold" style={{ lineHeight: 1.2, maxWidth: 150 }}>
                <span className="truncate" style={{ display: "block" }}>
                  {email}
                </span>
              </span>
              <span className="t-xs muted" style={{ lineHeight: 1.2 }}>
                {role === "operator" ? "Operator · admin" : "Owner"}
              </span>
            </div>
            <Icon name="chevDown" size={14} style={{ color: "var(--muted-foreground)" }} />
          </button>
        )}
      >
        <div className="menu-label">{email}</div>
        <div className="menu-item">
          <Icon name="user" /> Account settings
        </div>
        <div className="menu-sep" />
        <div className="menu-item danger" onClick={logout}>
          <Icon name="logout" /> Sign out
        </div>
      </Menu>
    </header>
  )
}

export function AppShell({
  children,
  route,
  setRoute,
  role,
  theme,
  toggleTheme,
  counts,
}: {
  children: ReactNode
  route: Route
  setRoute: (r: Route) => void
  role: Role
  theme: "dark" | "light"
  toggleTheme: () => void
  counts: Partial<Record<Route, number>>
}) {
  return (
    <div style={{ display: "flex", height: "100%", overflow: "hidden" }}>
      <Sidebar route={route} setRoute={setRoute} role={role} counts={counts} />
      <div className="col" style={{ flex: 1, minWidth: 0 }}>
        <Topbar route={route} role={role} theme={theme} toggleTheme={toggleTheme} />
        <div className="page">{children}</div>
      </div>
    </div>
  )
}
