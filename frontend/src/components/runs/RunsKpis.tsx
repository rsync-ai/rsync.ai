"use client"

import { StatCard } from "@/components/kit/StatCard"
import { StatCardsSkeleton } from "@/components/kit/PageSkeleton"
import { useActivityMetrics } from "@/components/runs/ActivityMetricsProvider"
import { compactCount, runKpis } from "@/components/runs/run-kpis"

const GRID = "grid gap-4 @xl:grid-cols-2 @3xl:grid-cols-4"

/**
 * Runs, failures, success rate and rows written over the last 7 days, above the
 * Runs list. Draws nothing outside an ActivityMetricsProvider.
 */
export function RunsKpis() {
  const shared = useActivityMetrics()
  if (!shared) return null
  const { metrics, error, reload } = shared

  if (error) {
    return (
      <p className="text-sm text-zinc-500 dark:text-zinc-400">
        Could not load the last 7 days&apos; counts.{" "}
        <button type="button" className="underline underline-offset-2" onClick={reload}>
          Retry
        </button>
      </p>
    )
  }
  if (!metrics) return <StatCardsSkeleton count={4} label="Loading the last 7 days…" className={GRID} />

  const k = runKpis(metrics)
  return (
    <section aria-label="Last 7 days" className="space-y-2">
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        Last 7 days · pipeline and model runs (workflow runs are listed below but not counted here)
      </p>
      <div className={GRID}>
        <StatCard title="Runs" value={k.runs} trend={k.runsByDay} trendLabel="Per day" />
        <StatCard
          title="Failed"
          value={k.failed}
          tone={k.failed > 0 ? "bad" : "default"}
          trend={k.failedByDay}
          trendColor="failed"
          trendLabel="Per day"
        />
        <StatCard
          title="Success rate"
          value={k.successRate ?? "—"}
          hint={k.successRate ? "Of runs that succeeded or failed" : "None succeeded or failed yet"}
        />
        <StatCard
          title="Rows written"
          value={compactCount(k.rowsWritten)}
          hint={`${k.rowsWritten.toLocaleString("en-US")} by pipeline runs`}
          trend={k.rowsByDay}
          trendColor="series-7"
          trendLabel="Per day"
        />
      </div>
    </section>
  )
}
