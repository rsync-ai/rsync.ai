"use client"

import { useCallback, useEffect, useState } from "react"
import Link from "next/link"
import { Loader2 } from "lucide-react"

import { AGGREGATE_LABEL, TYPE_LABEL } from "@/components/charts/chart-form"
import { SERIES_COLORS } from "@/components/charts/chart-theme"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { ChartError, all, one, type ChartRole, type ChartType } from "@/lib/api/charts"
import {
  applyDashboardChange,
  getDashboardChange,
  GRID_COLUMNS,
  type DashboardChange,
  type DashboardChangeSide,
} from "@/lib/api/dashboards"
import { notifyDashboardChanged } from "@/lib/events/dashboardChanged"
import { captureWorkspace } from "@/lib/workspace/active-workspace"

/** Refusals after which the change is no longer open: show what it is now. */
const CLOSED_CODES = ["already_applied", "expired", "dashboard_changed"]

/** The series colours' names, in SERIES_COLORS order (chart-theme.ts). */
const COLOR_NAMES = ["Blue", "Orange", "Sky", "Ochre", "Pink", "Violet", "Green"]

const SORT_WORDS: Record<string, string> = {
  x_asc: "A to Z or oldest first",
  x_desc: "Z to A or newest first",
  value_desc: "Largest first",
  value_asc: "Smallest first",
}

type Status = "proposed" | "applied" | "expired" | "stale"

/**
 * A change Ask rsync.ai or an agent proposed to one dashboard tile
 * (plan_dashboard_change, dashboards-design.md § 7.6): the tile as it is and as it
 * would be. Nothing changes until an owner or admin clicks Apply, which saves the
 * chart and the dashboard with their session and keeps the dashboard's version
 * before it (handlers/dashboard_changes.go). A chart on another dashboard too is
 * copied for this tile. Shown under an Ask rsync.ai answer and above the
 * dashboard when its link (?change=) is opened. Either way an applied change is
 * announced (notifyDashboardChanged), and the dashboard open for it reads itself
 * again.
 */
export function DashboardChangeReview({
  changeId,
  hideOpenLink = false,
}: {
  changeId: string
  hideOpenLink?: boolean
}) {
  const [data, setData] = useState<{ change: DashboardChange; can_manage: boolean } | null>(null)
  const [loadError, setLoadError] = useState<Error | null>(null)
  const [applying, setApplying] = useState(false)
  const [applied, setApplied] = useState<{ copied: boolean } | null>(null)
  const [applyError, setApplyError] = useState<string | null>(null)

  const asError = (err: unknown) => (err instanceof Error ? err : new Error("Could not load the proposed change"))
  const load = useCallback(async () => {
    setLoadError(null)
    try {
      setData(await getDashboardChange(changeId))
    } catch (err) {
      setLoadError(asError(err))
    }
  }, [changeId])

  useEffect(() => {
    // A late answer for an earlier changeId, or another workspace, is dropped.
    let alive = true
    const isStale = captureWorkspace()
    getDashboardChange(changeId).then(
      (d) => {
        if (!alive || isStale()) return
        setData(d)
        setLoadError(null)
      },
      (err: unknown) => {
        if (alive && !isStale()) setLoadError(asError(err))
      },
    )
    return () => {
      alive = false
    }
  }, [changeId])

  const apply = async () => {
    if (!data || applying || applied) return
    setApplying(true)
    setApplyError(null)
    try {
      const res = await applyDashboardChange(data.change.id)
      setApplied({ copied: res.copied })
      notifyDashboardChanged(res.dashboard_id)
    } catch (err) {
      setApplyError(err instanceof Error ? err.message : "Could not apply the change")
      if (err instanceof ChartError && err.code && CLOSED_CODES.includes(err.code)) await load()
    } finally {
      setApplying(false)
    }
  }

  if (loadError) {
    const notFound = loadError instanceof ChartError && loadError.status === 404
    return (
      <div
        role="alert"
        className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
      >
        <span>{notFound ? "This proposed change was not found. It may be for someone else." : loadError.message}</span>
        {!notFound && (
          <button
            type="button"
            onClick={() => void load()}
            className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
          >
            Retry
          </button>
        )}
      </div>
    )
  }
  if (!data) {
    return (
      <div role="status" className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading the proposed change…
      </div>
    )
  }

  const { change, can_manage: canManage } = data
  const status: Status = applied || change.status === "applied"
    ? "applied"
    : change.expired
      ? "expired"
      : change.stale
        ? "stale"
        : "proposed"
  const open = status === "proposed"
  const rows = changeRows(change.before, change.after)
  const moved = change.moves.length
  const others = change.shared_elsewhere
  const copied = applied ? applied.copied : change.copied
  return (
    <section
      aria-label="Proposed dashboard change"
      className="space-y-3 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="font-semibold text-zinc-900 dark:text-white">
            Proposed change to <span className="break-all">{change.dashboard_name}</span>
          </h3>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Proposed by {change.proposed_by === "agent" ? "an AI agent" : "Ask rsync.ai"}. Nothing changes until you
            click Apply.
          </p>
        </div>
        <ChangeStatusBadge status={status} />
      </header>

      {change.summary && <p className="text-zinc-800 dark:text-zinc-200">{change.summary}</p>}

      <div className="overflow-x-auto">
        <table className="w-full min-w-[18rem] text-xs">
          <caption className="sr-only">The tile now and after the change</caption>
          <thead>
            <tr className="text-left text-zinc-500 dark:text-zinc-400">
              <th scope="col" className="w-24 pb-1 font-medium">
                <span className="sr-only">Setting</span>
              </th>
              <th scope="col" className="pb-1 pr-2 font-medium uppercase tracking-wide">
                Now
              </th>
              <th scope="col" className="pb-1 font-medium uppercase tracking-wide">
                After the change
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.label} className="border-t border-zinc-100 align-top dark:border-zinc-800">
                <th scope="row" className="py-1 pr-2 text-left font-normal text-zinc-500 dark:text-zinc-400">
                  {r.label}
                </th>
                <td className="py-1 pr-2 text-zinc-700 dark:text-zinc-300">
                  <Value value={r.before} />
                </td>
                <td
                  className={
                    r.changed
                      ? "py-1 font-medium text-zinc-900 dark:text-white"
                      : "py-1 text-zinc-700 dark:text-zinc-300"
                  }
                >
                  <Value value={r.after} />
                  {r.changed && <span className="sr-only"> (changed)</span>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <ul className="space-y-1 text-xs text-zinc-500 dark:text-zinc-400">
        {change.source_name && <li>The chart reads {change.source_name}; the change keeps that source.</li>}
        {moved > 0 && (
          <li>
            {moved === 1 ? "1 tile below it moves" : `${moved} tiles below it move`} down to make room.
          </li>
        )}
        {change.copy && (
          <li>
            The chart is on {others === 1 ? "1 other dashboard" : `${others} other dashboards`} too, so Apply copies it
            for this tile and {others === 1 ? "that dashboard keeps its chart" : "those dashboards keep theirs"}.
          </li>
        )}
        {change.chart_changed && !change.copy && <li>Apply changes the chart in place: it is on no other dashboard.</li>}
        {/* A version keeps each tile's chart id, never a chart's settings (§ 7.7):
            Restore undoes a copy or a size change, not a chart changed in place. */}
        {change.chart_changed && !change.copy ? (
          <li>
            Apply keeps the dashboard as it is now in its History, but not this chart&apos;s settings: a Restore will
            not change the chart back.
          </li>
        ) : (
          <li>Apply keeps the dashboard as it is now in its History, so the change can be undone.</li>
        )}
      </ul>

      {applyError && (
        <p
          role="alert"
          className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
        >
          {applyError}
        </p>
      )}

      <footer className="flex flex-wrap items-center gap-3">
        {open && canManage && (
          <>
            <Button size="sm" onClick={() => void apply()} loading={applying}>
              Apply change
            </Button>
            <span className="text-xs text-zinc-500 dark:text-zinc-400">
              Open until <LocalDateTime value={change.expires_at} />.
            </span>
          </>
        )}
        {open && !canManage && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            Only workspace owners and admins can apply a change to a dashboard.
          </span>
        )}
        {applied && (
          <span role="status" className="text-xs text-emerald-700 dark:text-emerald-300">
            Applied.{copied ? " This tile now has its own copy of the chart." : ""}
          </span>
        )}
        {!applied && status === "applied" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            This change was applied{change.copied ? ", with a copy of the chart for this tile" : ""}.
          </span>
        )}
        {status === "expired" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            This change expired 24 hours after it was proposed. Ask again for a new one.
          </span>
        )}
        {status === "stale" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            The dashboard or its chart was saved after this change was proposed. Ask again so the change starts from
            what it is now.
          </span>
        )}
        {!hideOpenLink && (
          <Link
            href={`/dashboards/${change.dashboard_id}?change=${change.id}`}
            className="ml-auto text-xs font-medium text-violet-700 hover:underline dark:text-violet-300"
          >
            Open dashboard
          </Link>
        )}
      </footer>
    </section>
  )
}

function ChangeStatusBadge({ status }: { status: Status }) {
  if (status === "applied") return <Badge variant="success">Applied</Badge>
  if (status === "expired") return <Badge variant="secondary">Expired</Badge>
  if (status === "stale") return <Badge variant="warning">Out of date</Badge>
  return <Badge variant="info">Waiting for you</Badge>
}

/** A row's value: words, or a colour's swatch and name. */
type RowValue = string | { color: string }

function Value({ value }: { value: RowValue }) {
  if (typeof value === "string") return <span className="break-words">{value}</span>
  const i = (SERIES_COLORS as readonly string[]).indexOf(value.color)
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        aria-hidden
        className="inline-block h-3 w-3 shrink-0 rounded-sm"
        style={{ background: `hsl(var(--chart-${value.color}))` }}
      />
      {i >= 0 ? COLOR_NAMES[i] : value.color}
    </span>
  )
}

interface ChangeRow {
  label: string
  before: RowValue
  after: RowValue
  changed: boolean
}

/**
 * The tile before and after, a row per thing a change can set: the chart's name,
 * type, columns, measure, series, sort and colour, and the tile's size and place.
 * A row neither side has is left out.
 */
export function changeRows(before: DashboardChangeSide, after: DashboardChangeSide): ChangeRow[] {
  const typeOf = (s: DashboardChangeSide) => TYPE_LABEL[s.type as ChartType] ?? s.type
  const field = (s: DashboardChangeSide, role: ChartRole) => (s.settings ? one(s.settings, role) : undefined)
  // An axis by its column, and its date grain when it has one, so a grain alone shows as a change.
  const axis = (s: DashboardChangeSide, role: ChartRole) => {
    const a = field(s, role)
    if (!a?.column) return ""
    return a.grain ? `${a.column} by ${a.grain}` : a.column
  }
  const measure = (s: DashboardChangeSide) => {
    const m = field(s, "measure")
    if (!m) return ""
    const aggregate = m.aggregate ?? "count"
    const agg = AGGREGATE_LABEL[aggregate] ?? aggregate
    return aggregate === "count" ? agg : `${agg} of ${m.column ?? ""}`.trim()
  }
  const sort = (s: DashboardChangeSide) => (s.settings?.sort ? SORT_WORDS[s.settings.sort] ?? s.settings.sort : "")
  const color = (s: DashboardChangeSide): RowValue => {
    const c = field(s, "measure")?.color
    return c ? { color: c } : ""
  }
  const size = (s: DashboardChangeSide) =>
    `${s.w} of ${GRID_COLUMNS} columns, ${s.h} ${s.h === 1 ? "row" : "rows"} high`
  const place = (s: DashboardChangeSide) => `Column ${s.x + 1}, row ${s.y + 1}`

  const things: Array<[string, (s: DashboardChangeSide) => RowValue]> = [
    ["Title", (s) => s.name],
    ["Chart", typeOf],
    ["X", (s) => axis(s, "x")],
    ["Y", (s) => axis(s, "y")],
    ["Measure", measure],
    ["Series", (s) => field(s, "series")?.column ?? ""],
    ["Columns", (s) => (s.settings ? all(s.settings, "columns").map((f) => f.column ?? "").join(", ") : "")],
    ["Order", sort],
    ["Colour", color],
    ["Size", size],
    ["Place", place],
  ]
  const key = (v: RowValue) => (typeof v === "string" ? v : `color:${v.color}`)
  return things
    .map(([label, of]) => {
      const b = of(before)
      const a = of(after)
      return { label, before: b === "" ? "—" : b, after: a === "" ? "—" : a, changed: key(b) !== key(a) }
    })
    .filter((r) => r.before !== "—" || r.after !== "—")
}
