/* data.ts — domain types + formatting helpers shared across the UI.
 *
 * Live server data comes from the control-plane API (see lib/api.ts); this file
 * holds the view-model types those responses are adapted into, plus the small
 * set of static options the create form still offers. */

// UI role model: operator = admin (fleet-wide), owner = user (owner-scoped).
export type Role = "operator" | "owner"

export type ServerStatus =
  | "running"
  | "stopped"
  | "starting"
  | "stopping"
  | "provisioning"
  | "scheduling"
  | "unschedulable"
  | "error"
  | "draining"
  | "down"
  | "ready"

export interface Owner {
  id: string
  name: string
  email: string
  initials: string
}

export interface Server {
  id: string
  name: string
  owner: string
  version: string
  desired: "running" | "stopped"
  status: ServerStatus
  hostId: string | null
  cpus: number
  mem: number
  players: number
  maxPlayers: number
  address: string | null
  port: number | null
  health: string
  // ISO timestamp of the last successful deep-health probe (P7), or null if the
  // game process has never been seen answering.
  lastSeen: string | null
  statusMessage: string | null
  attempts: number
  createdDays: number
  world: number
}

export const MC_VERSIONS = ["1.20.4", "1.21.1", "1.20.1", "1.19.4", "1.21.4"]

export const fmtMem = (mb: number) =>
  mb >= 1024
    ? (mb / 1024) % 1 === 0
      ? `${mb / 1024} GB`
      : `${(mb / 1024).toFixed(1)} GB`
    : `${mb} MB`
export const fmtWorld = (mb: number) =>
  mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`
