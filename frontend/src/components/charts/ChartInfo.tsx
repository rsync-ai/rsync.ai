"use client"

/**
 * A dashboard tile's "about this chart" (dashboards-design.md § 5): one button in the
 * tile's header whose icons say, at a glance, on time or overdue, whether a check failed
 * and whether there are warnings; it opens the badges and warnings SavedChart draws
 * above a chart anywhere else. In a tile they would take the chart's room.
 */

import Link from "next/link"
import { ArrowRight, CircleCheck, CircleX, Clock, ClockAlert, Info, TriangleAlert } from "lucide-react"
import { useId } from "react"

import { chartWarnings } from "@/components/charts/saved-chart"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import type { ChartDraw } from "@/lib/api/charts"

const AMBER = "text-amber-600 dark:text-amber-400"
const ICON = "h-3.5 w-3.5 shrink-0"

export function ChartInfo({
  draw,
  ignored = [],
  unfiltered,
  type,
}: {
  draw: ChartDraw | null
  ignored?: string[]
  /** The filters its connection could not take, by label; the server's columns when absent. */
  unfiltered?: string[]
  type?: string
}) {
  const headingId = useId()
  if (!draw) return null
  const badges = draw.badges
  const warnings = chartWarnings(draw, ignored, type, unfiltered)
  if (!badges && warnings.length === 0) return null

  const isModel = badges?.source_kind === "model"
  const deadline = isModel && !!badges?.deadline_seconds
  const late = deadline && !!badges?.late
  const checks = isModel ? (badges?.checks ?? []) : []
  const passed = checks.filter((c) => c.status === "pass").length
  const failing = passed < checks.length
  const readAt = draw.result?.read_at

  const summary = [
    deadline ? (late ? "overdue" : "on time") : null,
    checks.length > 0 ? `${passed} of ${checks.length} checks passed` : null,
    warnings.length > 0 ? `${warnings.length} ${warnings.length === 1 ? "warning" : "warnings"}` : null,
  ].filter(Boolean)
  const label = summary.length > 0 ? `About this chart: ${summary.join(", ")}` : "About this chart"

  return (
    <Popover>
      <PopoverTrigger
        className="-my-0.5 inline-flex h-6 min-w-6 shrink-0 items-center justify-center gap-1 rounded px-1 text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400 data-[state=open]:bg-zinc-100 data-[state=open]:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100 dark:data-[state=open]:bg-zinc-800 dark:data-[state=open]:text-zinc-100"
        aria-label={label}
        title={label}
        data-testid="chart-info"
      >
        {isModel && (late ? <ClockAlert className={`${ICON} ${AMBER}`} aria-hidden /> : <Clock className={ICON} aria-hidden />)}
        {checks.length > 0 &&
          (failing ? <CircleX className={`${ICON} ${AMBER}`} aria-hidden /> : <CircleCheck className={ICON} aria-hidden />)}
        {warnings.length > 0 && (
          <span className={`inline-flex items-center gap-0.5 text-[11px] font-medium tabular-nums ${AMBER}`}>
            <TriangleAlert className={ICON} aria-hidden />
            {warnings.length}
          </span>
        )}
        {!isModel && warnings.length === 0 && <Info className={ICON} aria-hidden />}
      </PopoverTrigger>
      <PopoverContent aria-labelledby={headingId}>
        <p id={headingId} className="mb-2 text-xs font-medium text-zinc-900 dark:text-zinc-100">
          About this chart
        </p>
        {(isModel || readAt) && (
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
            {isModel && (
              <>
                <dt className="text-zinc-500 dark:text-zinc-400">Data as of</dt>
                <dd>
                  <LocalDateTime value={badges?.data_as_of} fallback="never built" />
                </dd>
              </>
            )}
            {deadline && (
              <>
                <dt className="text-zinc-500 dark:text-zinc-400">Deadline</dt>
                <dd className={late ? `inline-flex items-center gap-1 font-medium ${AMBER}` : undefined}>
                  {late && <TriangleAlert className="h-3 w-3" aria-hidden />}
                  {/* "Overdue", the word the models pages use for a missed deadline (#1503). */}
                  {late ? "Overdue" : "On time"}
                </dd>
              </>
            )}
            {readAt && (
              <>
                <dt className="text-zinc-500 dark:text-zinc-400">Read</dt>
                <dd>
                  <LocalDateTime value={readAt} />
                </dd>
              </>
            )}
          </dl>
        )}
        {checks.length > 0 && (
          <div className="mt-2 border-t border-zinc-100 pt-2 dark:border-zinc-800">
            <p className={failing ? `font-medium ${AMBER}` : "font-medium"}>
              Checks {passed} of {checks.length} passed
            </p>
            <ul className="mt-1 space-y-0.5" aria-label="Checks">
              {checks.map((c) => {
                const pass = c.status === "pass"
                return (
                  <li key={c.name} className="flex items-center gap-1.5">
                    {pass ? (
                      <CircleCheck className="h-3 w-3 shrink-0 text-zinc-400" aria-hidden />
                    ) : (
                      <CircleX className={`h-3 w-3 shrink-0 ${AMBER}`} aria-hidden />
                    )}
                    <span className="min-w-0 flex-1 truncate" title={c.name}>
                      {c.name}
                    </span>
                    <span className={pass ? "text-zinc-500 dark:text-zinc-400" : AMBER}>{c.status}</span>
                  </li>
                )
              })}
            </ul>
          </div>
        )}
        {warnings.length > 0 && (
          <ul
            className="mt-2 space-y-1 border-t border-zinc-100 pt-2 text-amber-800 dark:border-zinc-800 dark:text-amber-200"
            aria-label="Warnings"
          >
            {warnings.map((w) => (
              <li key={w} className="flex items-start gap-1.5">
                <TriangleAlert className="mt-0.5 h-3 w-3 shrink-0" aria-hidden />
                <span>{w}</span>
              </li>
            ))}
          </ul>
        )}
        {isModel && badges?.lineage_url && (
          <div className="mt-2 border-t border-zinc-100 pt-2 dark:border-zinc-800">
            <Link
              href={badges.lineage_url}
              className="inline-flex items-center gap-1 rounded font-medium text-zinc-900 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400 dark:text-zinc-100"
            >
              Lineage <ArrowRight className="h-3 w-3" aria-hidden />
            </Link>
          </div>
        )}
      </PopoverContent>
    </Popover>
  )
}
