import { clsx, type ClassValue } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/** Format an ISO timestamp as a short relative age ("just now", "5s ago",
 *  "3m ago", "2h ago", "4d ago"). Returns "—" for a missing/unparseable value. */
export function fmtAgo(iso: string | null | undefined): string {
  if (!iso) return "—"
  const t = new Date(iso).getTime()
  if (Number.isNaN(t)) return "—"
  const sec = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (sec < 5) return "just now"
  if (sec < 60) return `${sec}s ago`
  if (sec < 3600) return `${Math.floor(sec / 60)}m ago`
  if (sec < 86_400) return `${Math.floor(sec / 3600)}h ago`
  return `${Math.floor(sec / 86_400)}d ago`
}
