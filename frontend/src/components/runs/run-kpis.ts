/**
 * The four numbers above the Runs list, from the same GET /activity/metrics read
 * Home's charts make. Pipeline and model runs only: workflow runs have no series
 * there yet, so the strip says so and hides under the Workflows filter.
 */

import type { ActivityMetrics, MetricCount } from "@/lib/api/activity-metrics"
import { dailyTotals } from "@/lib/home/overview"

export interface RunKpis {
  runs: number
  failed: number
  /** "98.8%", or null when nothing finished as succeeded or failed. */
  successRate: string | null
  rowsWritten: number
  /** Per day, oldest first, for the cards' trend lines. */
  runsByDay: number[]
  failedByDay: number[]
  rowsByDay: number[]
}

const sum = (xs: readonly number[]) => xs.reduce((a, b) => a + b, 0)
const withStatus = (counts: readonly MetricCount[], status: string) => counts.filter((c) => c.status === status)

/**
 * Succeeded out of succeeded + failed. Cancelled, skipped, waiting and running
 * runs are not an outcome either way. Rounded down, so one failure in thousands
 * still reads below 100%.
 */
export function successRate(succeeded: number, failed: number): string | null {
  const finished = succeeded + failed
  if (finished <= 0) return null
  if (failed === 0) return "100%"
  const pct = Math.floor((succeeded / finished) * 1000) / 10
  return `${pct.toLocaleString("en-US", { maximumFractionDigits: 1 })}%`
}

/** "3.4M", "12K", "940": a count that has to fit a card. */
export function compactCount(n: number): string {
  return new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(n)
}

export function runKpis(m: ActivityMetrics, last = 7): RunKpis {
  const counts = [...m.pipeline_runs, ...m.model_runs]
  const failed = withStatus(counts, "failed")
  const days = m.days.slice(-last)
  const inWindow = new Set(days)
  const total = (xs: readonly { day: string; count: number }[]) => sum(xs.filter((x) => inWindow.has(x.day)).map((x) => x.count))
  const rows = m.rows_written.map((r) => ({ day: r.day, count: r.rows }))
  return {
    runs: total(counts),
    failed: total(failed),
    successRate: successRate(total(withStatus(counts, "succeeded")), total(failed)),
    rowsWritten: total(rows),
    runsByDay: dailyTotals(m.days, counts, last),
    failedByDay: dailyTotals(m.days, failed, last),
    rowsByDay: dailyTotals(m.days, rows, last),
  }
}
