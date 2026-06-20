/* primitives.tsx — Btn, Badge, StatusBadge, Menu, CopyBtn, Meter. */
import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
} from "react"
import { createPortal } from "react-dom"
import { Icon } from "./icon"
import type { ServerStatus } from "@/lib/data"

type BtnVariant = "outline" | "primary" | "ghost" | "danger"
type BtnSize = "sm" | "lg"

export function Btn({
  variant = "outline",
  size,
  className = "",
  children,
  ...rest
}: {
  variant?: BtnVariant
  size?: BtnSize
  className?: string
  children: ReactNode
} & ButtonHTMLAttributes<HTMLButtonElement>) {
  const cls = ["btn", variant, size, className].filter(Boolean).join(" ")
  return (
    <button className={cls} {...rest}>
      {children}
    </button>
  )
}

export function Badge({
  status,
  className = "",
  children,
}: {
  status?: string
  className?: string
  children: ReactNode
}) {
  return (
    <span className={"badge soft " + (status ? "s-" + status : "") + " " + className}>
      {children}
    </span>
  )
}

const STATUS_MAP: Record<string, { c: string; t: string; pulse?: boolean }> = {
  running: { c: "running", t: "Running" },
  stopped: { c: "stopped", t: "Stopped" },
  starting: { c: "pending", t: "Starting", pulse: true },
  stopping: { c: "pending", t: "Stopping", pulse: true },
  provisioning: { c: "pending", t: "Provisioning", pulse: true },
  scheduling: { c: "pending", t: "Scheduling", pulse: true },
  unschedulable: { c: "error", t: "Unschedulable" },
  error: { c: "error", t: "Error" },
  draining: { c: "info", t: "Draining" },
  down: { c: "error", t: "Down" },
  ready: { c: "running", t: "Ready" },
}

export function StatusBadge({ state }: { state: ServerStatus }) {
  const m = STATUS_MAP[state] || { c: "stopped", t: state }
  return (
    <span className={"badge soft s-" + m.c}>
      <i className={"dot" + (m.pulse ? " pulse" : "")} />
      {m.t}
    </span>
  )
}

/* dismiss-on-outside-click menu.
   The dropdown is portaled to <body> with fixed positioning anchored to the
   trigger, so it overlays the page instead of being clipped by an ancestor's
   overflow (e.g. the table card's `overflow: hidden`). */
export function Menu({
  trigger,
  children,
  align = "right",
  width,
}: {
  trigger: (open: boolean, toggle: () => void) => ReactNode
  children: ReactNode
  align?: "left" | "right"
  width?: number
}) {
  const [open, setOpen] = useState(false)
  const triggerRef = useRef<HTMLDivElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const [pos, setPos] = useState<{ top: number; left?: number; right?: number }>()
  useEffect(() => {
    if (!open) return
    const place = () => {
      const el = triggerRef.current
      if (!el) return
      const r = el.getBoundingClientRect()
      setPos(
        align === "right"
          ? { top: r.bottom + 6, right: window.innerWidth - r.right }
          : { top: r.bottom + 6, left: r.left },
      )
    }
    place()
    const onClick = (e: MouseEvent) => {
      const t = e.target as Node
      if (
        !triggerRef.current?.contains(t) &&
        !menuRef.current?.contains(t)
      )
        setOpen(false)
    }
    document.addEventListener("mousedown", onClick)
    window.addEventListener("resize", place)
    window.addEventListener("scroll", place, true)
    return () => {
      document.removeEventListener("mousedown", onClick)
      window.removeEventListener("resize", place)
      window.removeEventListener("scroll", place, true)
    }
  }, [open, align])
  return (
    <div ref={triggerRef} style={{ position: "relative" }}>
      {trigger(open, () => setOpen((o) => !o))}
      {open &&
        pos &&
        createPortal(
          <div
            ref={menuRef}
            className="menu"
            style={{
              position: "fixed",
              top: pos.top,
              left: pos.left,
              right: pos.right,
              minWidth: width,
            }}
            onClick={() => setOpen(false)}
          >
            {children}
          </div>,
          document.body,
        )}
    </div>
  )
}

export function CopyBtn({ text }: { text: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      className="icon-btn sm"
      title="Copy"
      onClick={(e) => {
        e.stopPropagation()
        navigator.clipboard?.writeText(text)
        setDone(true)
        setTimeout(() => setDone(false), 1200)
      }}
    >
      <Icon name={done ? "check" : "copy"} size={14} />
    </button>
  )
}

export function Meter({ value, max }: { value: number; max: number }) {
  const pct = Math.min(100, Math.round((value / max) * 100))
  const cls = pct >= 88 ? "hot" : pct >= 70 ? "warn" : ""
  return (
    <div className={"meter " + cls}>
      <i style={{ width: pct + "%" }} />
    </div>
  )
}
