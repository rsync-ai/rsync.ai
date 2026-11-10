/**
 * The Runs page's charts as fixed chart settings (dashboards-design.md § 6): the
 * settings say what to draw, the rows come in at draw time, and the colours come
 * from the theme (charts/chart-theme.ts). Settings name their outcomes' colour
 * tokens, never a colour.
 *
 * No ECharts import here: settings are plain objects, and ECharts loads only with
 * EChartNoSSR.
 */

import type { XYSettings } from "@/components/charts/chart-settings"
import type { ChartColor } from "@/components/charts/chart-theme"
import { CHECK_STATUS_LABEL } from "@/components/explorer/modelChecks"
import { ACTIVITY_STATUS_LABELS } from "@/lib/api/activity"
import type { MetricCount, MetricRows, MetricRunTime } from "@/lib/api/activity-metrics"

/** One outcome a count chart stacks: the status the server sends, its words and its colour. */
export interface Outcome {
  status: string
  label: string
  color: ChartColor
}

// In the order they stack, the server's order (activity_metrics.go *MetricStatusOrder).
// The statuses are closed sets: pipelines are mapped to these five server-side,
// saved_query_runs.status and saved_query_check_results.status are CHECK constraints
// (migrations 086, 138).
export const PIPELINE_OUTCOMES: readonly Outcome[] = [
  { status: "succeeded", label: ACTIVITY_STATUS_LABELS.succeeded, color: "succeeded" },
  { status: "failed", label: ACTIVITY_STATUS_LABELS.failed, color: "failed" },
  { status: "cancelled", label: ACTIVITY_STATUS_LABELS.cancelled, color: "cancelled" },
  { status: "waiting", label: ACTIVITY_STATUS_LABELS.waiting, color: "waiting" },
  { status: "running", label: ACTIVITY_STATUS_LABELS.running, color: "running" },
]

export const MODEL_OUTCOMES: readonly Outcome[] = [
  { status: "succeeded", label: ACTIVITY_STATUS_LABELS.succeeded, color: "succeeded" },
  { status: "failed", label: ACTIVITY_STATUS_LABELS.failed, color: "failed" },
  { status: "skipped", label: ACTIVITY_STATUS_LABELS.skipped, color: "skipped" },
]

export const CHECK_OUTCOMES: readonly Outcome[] = [
  { status: "pass", label: CHECK_STATUS_LABEL.pass, color: "succeeded" },
  { status: "fail", label: CHECK_STATUS_LABEL.fail, color: "failed" },
  { status: "error", label: CHECK_STATUS_LABEL.error, color: "error" },
]

export const RUN_TIME_MEASURES = ["Median", "95th percentile"] as const

/** "2026-10-03" as the axis says it: "Oct 3". The day is a UTC day, so it is read as one. */
export function dayLabel(day: string): string {
  const d = new Date(`${day}T00:00:00Z`)
  if (Number.isNaN(d.getTime())) return day
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" })
}

export interface CountRow {
  day: string
  outcome: string
  count: number
}

/** A count chart's points; a status outside the outcomes is not drawn. */
export function countRows(points: readonly MetricCount[], outcomes: readonly Outcome[]): CountRow[] {
  const rows: CountRow[] = []
  for (const p of points) {
    const outcome = outcomes.find((o) => o.status === p.status)
    if (!outcome) continue
    rows.push({ day: dayLabel(p.day), outcome: outcome.label, count: p.count })
  }
  return rows
}

export interface RunTimeRow {
  day: string
  measure: (typeof RUN_TIME_MEASURES)[number]
  /** null on a day with no finished run, which breaks the line there. */
  seconds: number | null
  runs: number
}

/** Median and 95th percentile per day, with a gap on every day nothing finished. */
export function runTimeRows(days: readonly string[], points: readonly MetricRunTime[]): RunTimeRow[] {
  const byDay = new Map(points.map((p) => [p.day, p]))
  const rows: RunTimeRow[] = []
  for (const day of days) {
    const p = byDay.get(day)
    const label = dayLabel(day)
    rows.push({ day: label, measure: "Median", seconds: p ? p.median_seconds : null, runs: p?.runs ?? 0 })
    rows.push({ day: label, measure: "95th percentile", seconds: p ? p.p95_seconds : null, runs: p?.runs ?? 0 })
  }
  return rows
}

export interface RowsWrittenRow {
  day: string
  rows: number
}

export function rowsWrittenRows(points: readonly MetricRows[]): RowsWrittenRow[] {
  return points.map((p) => ({ day: dayLabel(p.day), rows: p.rows }))
}

/** Every day of the window on the axis, so a day with no runs still has its place. */
function dayAxis(days: readonly string[]) {
  return { field: "day", label: "Day", categories: days.map(dayLabel) }
}

/** Runs (or check results) per day, stacked by outcome in the outcomes' order. */
export function countChart(description: string, days: readonly string[], outcomes: readonly Outcome[]): XYSettings {
  return {
    type: "bar",
    stacked: true,
    description,
    x: dayAxis(days),
    y: { field: "count", format: "integer" },
    series: { field: "outcome", values: outcomes.map((o) => ({ key: o.label, label: o.label, color: o.color })) },
  }
}

/** Median and 95th-percentile run time per day, in seconds. */
export function runTimeChart(description: string, days: readonly string[]): XYSettings {
  return {
    type: "line",
    description,
    x: dayAxis(days),
    y: { field: "seconds", format: "decimal", title: "Seconds" },
    series: {
      field: "measure",
      values: [
        { key: "Median", label: "Median", color: "series-1" },
        { key: "95th percentile", label: "95th percentile", color: "series-2" },
      ] satisfies { key: (typeof RUN_TIME_MEASURES)[number]; label: string; color: ChartColor }[],
    },
    details: [{ field: "runs", label: "Finished runs", format: "integer" }],
  }
}

/** Rows the day's pipeline runs wrote. */
export function rowsWrittenChart(description: string, days: readonly string[]): XYSettings {
  return {
    type: "bar",
    stacked: false,
    description,
    x: dayAxis(days),
    y: { field: "rows", format: "integer", axisFormat: "compact" },
    series: { values: [{ key: "rows", label: "Rows written", color: "series-1" }] },
  }
}

// The gateway's map from a run's stored status to its outcome (activity.go
// pipelineRunStatuses); anything else has not finished yet and counts as running.
const PIPELINE_RUN_STATUSES: Record<string, string> = {
  completed: "succeeded",
  success: "succeeded",
  failed: "failed",
  error: "failed",
  silent_drop_detected: "failed",
  silent_partial_drop_detected: "failed",
  credential_check_failed: "failed",
  cancelled: "cancelled",
  waiting_for_user: "waiting",
}

/** A pipeline run's outcome, from the status the executions or trends endpoints report. */
export function runOutcome(status: string | undefined): Outcome {
  const key = PIPELINE_RUN_STATUSES[String(status ?? "").toLowerCase()] ?? "running"
  return PIPELINE_OUTCOMES.find((o) => o.status === key)!
}

/** One run as the trends endpoint lists it (pipeline_compare.go ExecutionSummary). */
export interface ChartRun {
  execution_id: string
  status: string
  start_time?: string
  duration_ms?: number | null
}

export interface RecentRunRow {
  run: string
  outcome: string
  seconds: number | null
}

/** "Oct 7, 17:02" in the viewer's zone: the chart draws in the browser only. */
function runLabel(start: string | undefined, index: number): string {
  const d = start ? new Date(start) : null
  if (!d || Number.isNaN(d.getTime())) return `Run ${index + 1}`
  return d.toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" })
}

/**
 * One category per run, named by when it started. Two runs can start in the same
 * minute; a category is one bar, so the second is "Oct 7, 17:02 (2)".
 */
export function uniqueRunLabels(starts: readonly (string | undefined)[]): string[] {
  const seen = new Map<string, number>()
  return starts.map((start, i) => {
    const base = runLabel(start, i)
    const n = (seen.get(base) ?? 0) + 1
    seen.set(base, n)
    return n === 1 ? base : `${base} (${n})`
  })
}

/**
 * A pipeline's recent runs, oldest first, one bar each: its height is how long
 * the run took and its colour its outcome. A run still going is drawn as long as
 * it has taken so far; a run with no end and no longer going (a cancelled one)
 * has no bar. `highlight` outlines one run, the one the page is about, when it
 * has a bar to outline. The categories are in the runs' order, so a bar's index
 * is its run's.
 */
export function recentRunsChart(
  description: string,
  runs: readonly ChartRun[],
  { highlight, now = Date.now() }: { highlight?: string; now?: number } = {},
): { settings: XYSettings; rows: RecentRunRow[] } {
  const categories = uniqueRunLabels(runs.map((r) => r.start_time))
  const rows = runs.map((r, i): RecentRunRow => {
    const outcome = runOutcome(r.status)
    let ms = typeof r.duration_ms === "number" ? r.duration_ms : null
    if (ms === null && outcome.status === "running" && r.start_time) {
      const started = Date.parse(r.start_time)
      if (!Number.isNaN(started)) ms = Math.max(0, now - started)
    }
    return { run: categories[i], outcome: outcome.label, seconds: ms === null ? null : ms / 1000 }
  })
  const marked = highlight === undefined ? -1 : runs.findIndex((r) => r.execution_id === highlight)
  const outlined = marked >= 0 && rows[marked].seconds !== null
  return {
    settings: {
      type: "bar",
      stacked: true,
      description,
      x: { field: "run", label: "Run started", categories },
      y: { field: "seconds", format: "decimal", title: "Seconds" },
      series: { field: "outcome", values: PIPELINE_OUTCOMES.map((o) => ({ key: o.label, label: o.label, color: o.color })) },
      ...(outlined ? { highlight: categories[marked] } : {}),
    },
    rows,
  }
}
