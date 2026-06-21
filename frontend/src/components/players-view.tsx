/* players-view.tsx — Players (whitelist) view: a user's roster of player
 * usernames, each granted onto a checkable subset of the user's own servers. */
import { useCallback, useEffect, useMemo, useState } from "react"
import { Icon } from "./icon"
import { Btn } from "./primitives"
import { api, ApiError, type ApiPlayer, type ApiServer } from "@/lib/api"

export function PlayersView() {
  const [players, setPlayers] = useState<ApiPlayer[]>([])
  const [servers, setServers] = useState<ApiServer[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [username, setUsername] = useState("")
  const [adding, setAdding] = useState(false)
  const [q, setQ] = useState("")

  const refresh = useCallback(() => {
    Promise.all([api.listPlayers(), api.listServers()])
      .then(([ps, srv]) => {
        setPlayers(ps)
        setServers(srv)
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

  // Patch one player in place after a grant toggle or rename.
  const applyPlayer = useCallback((next: ApiPlayer) => {
    setPlayers((prev) => prev.map((p) => (p.id === next.id ? next : p)))
  }, [])

  const addPlayer = async () => {
    const name = username.trim()
    if (!name) return
    setAdding(true)
    try {
      const created = await api.createPlayer({ username: name })
      setPlayers((prev) => [...prev, created].sort((a, b) => a.username.localeCompare(b.username)))
      setUsername("")
      setError(null)
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Couldn't add the player.")
    } finally {
      setAdding(false)
    }
  }

  const removePlayer = async (id: string) => {
    try {
      await api.deletePlayer(id)
      setPlayers((prev) => prev.filter((p) => p.id !== id))
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Couldn't remove the player.")
    }
  }

  const list = useMemo(() => {
    const t = q.trim().toLowerCase()
    if (!t) return players
    return players.filter((p) => p.username.toLowerCase().includes(t))
  }, [players, q])

  const grantCount = players.reduce((a, p) => a + p.server_ids.length, 0)

  return (
    <div className="page-inner">
      <div className="page-head">
        <div>
          <div className="page-title">Players</div>
          <div className="page-sub">
            Your whitelist roster — add players and choose which of your servers each may use.
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
          <button className="icon-btn sm" onClick={() => setError(null)} style={{ marginLeft: "auto" }}>
            <Icon name="x" size={14} />
          </button>
        </div>
      )}

      {/* add + search toolbar */}
      <div className="between" style={{ gap: 12, flexWrap: "wrap" }}>
        <div className="row gap-2">
          <div className="input-wrap" style={{ width: 220 }}>
            <Icon name="user" />
            <input
              className="input"
              placeholder="Player username"
              value={username}
              maxLength={16}
              onChange={(e) => setUsername(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") addPlayer()
              }}
            />
          </div>
          <Btn variant="primary" onClick={addPlayer} disabled={adding || !username.trim()}>
            {adding ? <Icon name="restart" className="spin" size={14} /> : <Icon name="plus" size={14} />}
            Add player
          </Btn>
        </div>
        <div className="input-wrap" style={{ width: 220 }}>
          <Icon name="search" />
          <input
            className="input"
            placeholder="Search roster"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>
      </div>

      {servers.length === 0 && players.length > 0 && (
        <div className="t-sm muted">
          You have no servers yet — create one to start granting players access.
        </div>
      )}

      {/* roster */}
      {!list.length ? (
        <div className="card" style={{ overflow: "hidden" }}>
          <div className="empty">
            {loading ? (
              <>
                <Icon name="restart" className="spin" size={26} style={{ opacity: 0.6 }} />
                <div className="t-sm">Loading roster…</div>
              </>
            ) : (
              <>
                <Icon name="user" size={30} style={{ opacity: 0.5 }} />
                <div className="col" style={{ gap: 3, alignItems: "center" }}>
                  <div className="semibold" style={{ color: "var(--foreground)" }}>
                    {players.length ? "No players match" : "No players yet"}
                  </div>
                  <div className="t-sm">
                    {players.length
                      ? "Try a different search."
                      : "Add a player above to start your whitelist."}
                  </div>
                </div>
              </>
            )}
          </div>
        </div>
      ) : (
        <div className="col" style={{ gap: 12 }}>
          <div className="t-xs muted">
            {players.length} player{players.length === 1 ? "" : "s"} · {grantCount} server grant
            {grantCount === 1 ? "" : "s"}
          </div>
          {list.map((p) => (
            <PlayerCard
              key={p.id}
              player={p}
              servers={servers}
              onChanged={applyPlayer}
              onRemove={() => removePlayer(p.id)}
              onError={setError}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function PlayerCard({
  player,
  servers,
  onChanged,
  onRemove,
  onError,
}: {
  player: ApiPlayer
  servers: ApiServer[]
  onChanged: (p: ApiPlayer) => void
  onRemove: () => void
  onError: (msg: string) => void
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const granted = useMemo(() => new Set(player.server_ids), [player.server_ids])

  const toggle = async (serverId: string) => {
    const next = new Set(granted)
    if (next.has(serverId)) next.delete(serverId)
    else next.add(serverId)
    setBusy(serverId)
    try {
      const updated = await api.updatePlayer(player.id, { server_ids: [...next] })
      onChanged(updated)
    } catch (e) {
      onError(e instanceof ApiError ? e.message : "Couldn't update the grant.")
    } finally {
      setBusy(null)
    }
  }

  return (
    <div className="card" style={{ padding: 16 }}>
      <div className="between" style={{ marginBottom: 12 }}>
        <div className="row gap-2" style={{ alignItems: "center" }}>
          <div
            className="center"
            style={{
              width: 34,
              height: 34,
              borderRadius: 9,
              background: "var(--muted)",
              color: "var(--primary)",
            }}
          >
            <Icon name="user" size={17} />
          </div>
          <div className="col" style={{ gap: 1 }}>
            <span className="semibold">{player.username}</span>
            <span className="t-xs muted">
              {granted.size} of {servers.length} server{servers.length === 1 ? "" : "s"}
            </span>
          </div>
        </div>
        <Btn size="sm" variant="ghost" onClick={onRemove}>
          <Icon name="trash" size={14} /> Remove
        </Btn>
      </div>

      {servers.length === 0 ? (
        <div className="t-sm muted">No servers to grant yet.</div>
      ) : (
        <div className="row gap-2 wrap">
          {servers.map((s) => {
            const on = granted.has(s.id)
            return (
              <button
                key={s.id}
                className={"chip" + (on ? " on" : "")}
                disabled={busy === s.id}
                onClick={() => toggle(s.id)}
                title={on ? "Click to revoke access" : "Click to grant access"}
              >
                {busy === s.id ? (
                  <Icon name="restart" className="spin" size={13} />
                ) : (
                  <Icon name={on ? "check" : "plus"} size={13} />
                )}
                {s.name}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
