"use client"

import { useCallback, useMemo, useState } from "react"

import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import type { BarChartSettings, NumberFormat } from "@/components/charts/chart-settings"
import type { ChartColor } from "@/components/charts/chart-theme"
import { type SavedQueryRun } from "@/components/explorer/scheduledModel"
import { uniqueRunLabels } from "@/components/runs/run-chart-specs"
import { formatDurationOrDash } from "@/lib/duration"
import { cn } from "@/lib/utils"

// The runs on the page as bars, oldest on the left. A model that has started taking three
// times as long, or fails every other night, shows as a shape before anyone reads a row.
// It is drawn from exactly the runs the table under it lists, same filter and same page,
// so the two cannot disagree about which runs they mean.

/** What a bar needs from a run; a saved query's run has it as is. */
export type ChartRun = Pick<SavedQueryRun, "run_id" | "status" | "duration_ms" | "started_at">

/**
 * One kind of bar: the status it matches, the word the legend uses, and its colour
 * token. A noWork kind (a skip) did no work, so its duration is not a measurement.
 */
export interface RunBarKind {
  status: string
  label: string
  color: ChartColor
  noWork?: boolean
}

/** A saved query's three statuses, in the green, red and grey its badges use (skipped is grey on every page). */
export const SAVED_QUERY_RUN_KINDS: RunBarKind[] = [
  { status: "succeeded", label: "succeeded", color: "succeeded" },
  { status: "failed", label: "failed", color: "failed" },
  { status: "skipped", label: "skipped", color: "skipped", noWork: true },
]

// saved_query_runs allows only those three, but a status from a newer gateway is drawn
// in its own grey series rather than dropped: a missing bar would read as a run that
// never happened.
const OTHER_KIND = "__other__"

/**
 * The ModelRunGridTable's cell bars: a skipped run did no work, so its duration (about
 * 0 ms, and -1 ms under clock jitter) is not a measurement. It gets a fixed stub
 * instead: a streak of skips stays visible and cannot pass for very fast rebuilds.
 */
export const SKIPPED_BAR_PERCENT = 8

/** The floor for a run that did work, so 5 ms next to 4 minutes is still a bar. */
export const MIN_BAR_PERCENT = 3

/** The chart's floor, in pixels: a skip or a 5 ms run beside one of minutes is still a bar. */
const MIN_BAR_PX = 3

function kindOf(status: string, kinds: RunBarKind[]): RunBarKind | undefined {
  return kinds.find((k) => k.status === status)
}

function didNoWork(status: string, kinds: RunBarKind[]): boolean {
  return kindOf(status, kinds)?.noWork === true
}

function measuredMs(r: Pick<ChartRun, "duration_ms">): number {
  return Number.isFinite(r.duration_ms) ? Math.max(0, r.duration_ms) : 0
}

/** The longest run on the page that did work; skips are not measurements. */
export function longestRunMs(
  runs: Pick<ChartRun, "status" | "duration_ms">[],
  kinds: RunBarKind[] = SAVED_QUERY_RUN_KINDS,
): number {
  return runs.reduce((max, r) => (didNoWork(r.status, kinds) ? max : Math.max(max, measuredMs(r))), 0)
}

export function barHeightPercent(
  r: Pick<ChartRun, "status" | "duration_ms">,
  longestMs: number,
  kinds: RunBarKind[] = SAVED_QUERY_RUN_KINDS,
): number {
  if (didNoWork(r.status, kinds)) return SKIPPED_BAR_PERCENT
  if (longestMs <= 0) return MIN_BAR_PERCENT
  return Math.max(MIN_BAR_PERCENT, (measuredMs(r) / longestMs) * 100)
}

// The axis counts in the unit the longest run reads best in: a page of 300 ms runs in
// milliseconds, one of 40-minute rebuilds in minutes.
const UNITS: { below: number; title: string; ms: number; format: NumberFormat }[] = [
  { below: 1_000, title: "Milliseconds", ms: 1, format: "integer" },
  { below: 120_000, title: "Seconds", ms: 1_000, format: "decimal" },
  { below: 7_200_000, title: "Minutes", ms: 60_000, format: "decimal" },
  { below: Infinity, title: "Hours", ms: 3_600_000, format: "decimal" },
]

const capitalize = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

interface DurationRow {
  run: string
  kind: string
  value: number
}

function durationChart(
  ordered: ChartRun[],
  kinds: RunBarKind[],
  longestMs: number,
): { settings: BarChartSettings; rows: DurationRow[] } {
  const unit = UNITS.find((u) => longestMs < u.below)!
  const categories = uniqueRunLabels(ordered.map((r) => r.started_at))
  const rows = ordered.map((r, i) => {
    const kind = kindOf(r.status, kinds)
    return { run: categories[i], kind: kind?.status ?? OTHER_KIND, value: kind?.noWork ? 0 : measuredMs(r) / unit.ms }
  })
  // Only the kinds on the page are keyed: a workflow has six, a page usually two.
  const present = new Set(rows.map((r) => r.kind))
  return {
    settings: {
      type: "bar",
      stacked: true,
      minBarHeight: MIN_BAR_PX,
      description: "How long each run on this page took, oldest first, coloured by outcome",
      x: { field: "run", label: "Run started", categories },
      y: { field: "value", format: unit.format, title: unit.title },
      series: {
        field: "kind",
        values: [
          ...kinds
            .filter((k) => present.has(k.status))
            .map((k) => ({ key: k.status, label: capitalize(k.label), color: k.color })),
          ...(present.has(OTHER_KIND) ? [{ key: OTHER_KIND, label: "Other", color: "other" as const }] : []),
        ],
      },
    },
    rows,
  }
}

/**
 * The run a bar was clicked for: its row in the table under the chart scrolls into
 * view and is tinted, and says it is the current one. `rowProps` goes on each row.
 */
export function usePointedRun() {
  const [pointed, setPointed] = useState<string | null>(null)
  const point = useCallback((runId: string) => {
    setPointed(runId)
    document.getElementById(`run-row-${runId}`)?.scrollIntoView?.({ block: "nearest", behavior: "smooth" })
  }, [])
  const rowProps = (runId: string) => ({
    id: `run-row-${runId}`,
    "aria-current": pointed === runId ? ("true" as const) : undefined,
    className: pointed === runId ? "bg-violet-50 dark:bg-violet-950/40" : undefined,
  })
  return { point, rowProps }
}

/**
 * The runs as bars on a shared axis: a bar's height is how long its run took, its
 * colour the outcome. `onRunClick` opens the run a bar stands for.
 */
export function RunDurationChart({
  runs,
  kinds = SAVED_QUERY_RUN_KINDS,
  onRunClick,
  className,
}: {
  runs: ChartRun[]
  kinds?: RunBarKind[]
  onRunClick?: (runId: string) => void
  className?: string
}) {
  // The route sends newest first; a time axis reads left to right.
  const ordered = useMemo(() => [...runs].reverse(), [runs])
  const longestMs = longestRunMs(ordered, kinds)
  const chart = useMemo(() => durationChart(ordered, kinds, longestMs), [ordered, kinds, longestMs])
  if (runs.length === 0) return null

  return (
    <figure className={cn("border-b px-4 py-3 dark:border-zinc-800", className)}>
      <figcaption className="mb-1 flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1 text-xs">
        <span>
          <span className="font-medium text-zinc-700 dark:text-zinc-300">Run time</span>
          {longestMs > 0 && <span className="text-zinc-500 dark:text-zinc-400"> · longest {formatDurationOrDash(longestMs)}</span>}
        </span>
        {onRunClick && <span className="text-zinc-500 dark:text-zinc-400">Click a bar to find its run below.</span>}
      </figcaption>
      <EChartNoSSR
        settings={chart.settings}
        rows={chart.rows}
        height={140}
        onCategoryClick={onRunClick ? (i) => onRunClick(ordered[i].run_id) : undefined}
      />
    </figure>
  )
}
