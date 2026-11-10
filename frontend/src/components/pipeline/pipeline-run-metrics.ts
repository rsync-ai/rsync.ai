/**
 * One pipeline's runs per day, counted in the browser from the runs its Runs tab
 * already loads, in the shape Home's charts draw (lib/api/activity-metrics.ts).
 * GET /activity/metrics has no pipeline filter, so the tab counts its own list,
 * the way the gateway does (activity_metrics.go): a day is a UTC day, by when the
 * run started; run time is for the runs that succeeded or failed and have an end;
 * rows written is every run's records_processed; the CDC stream's own row (its id
 * is the pipeline's) is not a run.
 */

import { PIPELINE_OUTCOMES, runOutcome } from "@/components/runs/run-chart-specs"
import type { MetricCount, MetricRows, MetricRunTime } from "@/lib/api/activity-metrics"

export interface LoadedRun {
  id: string
  pipeline_id: string
  status: string
  start_time: string
  end_time?: string | null
  metrics?: { records_processed?: number } | null
}

export interface PipelineRunMetrics {
  /** Every day on the axis, oldest first, "YYYY-MM-DD". */
  days: string[]
  pipeline_runs: MetricCount[]
  pipeline_run_time: MetricRunTime[]
  rows_written: MetricRows[]
  /**
   * The day the loaded runs start on, when the list was cut short of the window:
   * the axis starts there, since the days before it would read as days without runs.
   */
  since?: string
}

const DAY_MS = 86_400_000
const utcDay = (ms: number) => new Date(ms).toISOString().slice(0, 10)

/** The last `count` UTC days, ending today. */
function windowDays(count: number, now: number): string[] {
  return Array.from({ length: count }, (_, i) => utcDay(now - (count - 1 - i) * DAY_MS))
}

/** Postgres percentile_cont: linear between the two nearest values. */
function percentile(sorted: number[], p: number): number {
  const pos = p * (sorted.length - 1)
  const lo = Math.floor(pos)
  const hi = Math.ceil(pos)
  return sorted[lo] + (sorted[hi] - sorted[lo]) * (pos - lo)
}

/**
 * `truncated` says the list stopped at its limit, so runs older than the oldest
 * loaded one may exist and are not counted.
 */
export function pipelineRunMetrics(
  runs: readonly LoadedRun[],
  windowLength: number,
  { now = Date.now(), truncated = false }: { now?: number; truncated?: boolean } = {},
): PipelineRunMetrics {
  let days = windowDays(windowLength, now)
  const inWindow = new Set(days)
  const counted = runs
    .map((r) => ({ run: r, started: Date.parse(r.start_time) }))
    .filter(({ run, started }) => run.id !== run.pipeline_id && !Number.isNaN(started) && inWindow.has(utcDay(started)))

  let since: string | undefined
  if (truncated && runs.length > 0) {
    const oldest = Math.min(...runs.map((r) => Date.parse(r.start_time)).filter((t) => !Number.isNaN(t)))
    const first = Number.isFinite(oldest) ? utcDay(oldest) : undefined
    if (first && first > days[0]) {
      since = first
      days = days.filter((d) => d >= first)
    }
  }

  const counts = new Map<string, number>()
  const times = new Map<string, number[]>()
  const written = new Map<string, number>()
  for (const { run, started } of counted) {
    const day = utcDay(started)
    const outcome = runOutcome(run.status).status
    counts.set(`${day}|${outcome}`, (counts.get(`${day}|${outcome}`) ?? 0) + 1)
    written.set(day, (written.get(day) ?? 0) + (run.metrics?.records_processed ?? 0))
    const ended = run.end_time ? Date.parse(run.end_time) : NaN
    if ((outcome === "succeeded" || outcome === "failed") && !Number.isNaN(ended)) {
      const list = times.get(day) ?? []
      list.push(Math.max(0, (ended - started) / 1000))
      times.set(day, list)
    }
  }

  const pipeline_runs: MetricCount[] = []
  const pipeline_run_time: MetricRunTime[] = []
  const rows_written: MetricRows[] = []
  for (const day of days) {
    for (const o of PIPELINE_OUTCOMES) {
      const count = counts.get(`${day}|${o.status}`)
      if (count) pipeline_runs.push({ day, status: o.status, count })
    }
    const secs = times.get(day)
    if (secs) {
      const sorted = [...secs].sort((a, b) => a - b)
      pipeline_run_time.push({ day, runs: sorted.length, median_seconds: percentile(sorted, 0.5), p95_seconds: percentile(sorted, 0.95) })
    }
    if (written.has(day)) rows_written.push({ day, rows: written.get(day)! })
  }
  return { days, pipeline_runs, pipeline_run_time, rows_written, ...(since ? { since } : {}) }
}
