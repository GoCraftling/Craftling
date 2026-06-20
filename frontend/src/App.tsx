/* App.tsx — root: auth gate, theme, routing. Role comes from the API session. */
import { useEffect, useState } from "react"
import { AppShell, type Route } from "@/components/app-shell"
import { AuthScreen } from "@/components/auth-screen"
import { ServersView } from "@/components/servers-view"
import { MarketplaceView } from "@/components/marketplace-view"
import { StubView } from "@/components/stub-view"
import { Icon } from "@/components/icon"
import { usePersist } from "@/lib/use-persist"
import { useAuth } from "@/lib/auth"

export function App() {
  const { user, role, loading } = useAuth()
  const [theme, setTheme] = usePersist<"dark" | "light">("cl-theme", "dark")
  const [route, setRoute] = usePersist<Route>("cl-route", "servers")
  const [serverCount, setServerCount] = useState(0)

  useEffect(() => {
    document.documentElement.classList.toggle("dark", theme === "dark")
  }, [theme])
  const toggleTheme = () => setTheme((x) => (x === "dark" ? "light" : "dark"))

  // Owners can't reach operator-only views; bounce them back to servers.
  useEffect(() => {
    if (role === "owner" && ["hosts", "scheduler", "quotas"].includes(route)) setRoute("servers")
  }, [role, route, setRoute])

  if (loading) {
    return (
      <div style={{ minHeight: "100%", display: "grid", placeItems: "center" }}>
        <Icon name="restart" className="spin" size={22} />
      </div>
    )
  }

  if (!user) {
    return <AuthScreen theme={theme} toggleTheme={toggleTheme} />
  }

  const counts: Partial<Record<Route, number>> = { servers: serverCount }

  return (
    <AppShell
      route={route}
      setRoute={setRoute}
      role={role}
      theme={theme}
      toggleTheme={toggleTheme}
      counts={counts}
    >
      {route === "servers" ? (
        <ServersView role={role} onCountChange={setServerCount} />
      ) : route === "marketplace" ? (
        <MarketplaceView onLaunched={() => setRoute("servers")} />
      ) : (
        <StubView route={route} />
      )}
    </AppShell>
  )
}

export default App
