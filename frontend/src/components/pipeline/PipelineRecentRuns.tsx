"use client"

/**
 * PipelineRecentRuns — "has this pipeline been reliable?" for a batch pipeline.
 *
 * One bar per recent run (oldest → newest, its height how long the run took,
 * its colour the outcome; a click opens the run) and one sentence: "9 of the
 * last 10 finished runs succeeded · average 4m 12s".
 * Data is GET /pipelines/:id/trends (pipeline_compare.go GetPipelineTrends),
 * whose success rate is succeeded / finished over the same window — a run still
 * in flight counts toward neither side.
 *
 * Batch only: a CDC pipeline has no discrete runs, and the health header already
 * answers "is it keeping up?" for it.
 */

import Link from "next/link"
import { useRouter } from "next/navigation"
import { useCallback, useEffect, useMemo, useState } from "react"
import { History } from "lucide-react"

import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import { ChartCard } from "@/components/kit/ChartCard"
import { recentRunsChart } from "@/components/runs/run-chart-specs"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { formatDuration } from "@/lib/duration"

// The gateway's cap (pipeline_compare.go GetPipelineTrends).
const WINDOW = 50
const POLL_MS = 30_000
// Below this many finished runs a percentage overstates what we know; the
// "N of M" sentence still says it honestly.
const MIN_RUNS_FOR_RATE = 3

export interface TrendRun {
  execution_id: string
  status: string
  start_time?: string
  duration_ms?: number | null
}

export interface PipelineTrendsResponse {
  success_rate?: number
  finished_runs?: number
  succeeded_runs?: number
  avg_duration_ms?: number
  recent_executions?: TrendRun[]
}

export interface RecentRunsSummary {
  /** Oldest first, so the newest run sits at the right, where the eye ends. */
  runs: TrendRun[]
  finished: number
  succeeded: number
  running: number
  /** Null when there are too few finished runs for a percentage to mean much. */
  ratePercent: number | null
  avgDurationMs: number | null
}

function isSuccess(status: string) {
  return status === "completed" || status === "success"
}

export function summarizeRecentRuns(t: PipelineTrendsResponse): RecentRunsSummary {
  const newestFirst = Array.isArray(t.recent_executions) ? t.recent_executions : []
  const runs = [...newestFirst].reverse()
  // Prefer the gateway's counts; derive them from the list for an older gateway
  // that predates finished_runs / succeeded_runs.
  const derivedFinished = runs.filter((r) => isSuccess(r.status) || r.status === "failed").length
  const derivedSucceeded = runs.filter((r) => isSuccess(r.status)).length
  const finished = typeof t.finished_runs === "number" ? t.finished_runs : derivedFinished
  const succeeded = typeof t.succeeded_runs === "number" ? t.succeeded_runs : derivedSucceeded
  const running = runs.filter((r) => r.status === "running").length
  return {
    runs,
    finished,
    succeeded,
    running,
    ratePercent: finished >= MIN_RUNS_FOR_RATE ? Math.round((succeeded / finished) * 100) : null,
    avgDurationMs: typeof t.avg_duration_ms === "number" && t.avg_duration_ms > 0 ? t.avg_duration_ms : null,
  }
}

export function PipelineRecentRuns({ pipelineId }: { pipelineId: string }) {
  const router = useRouter()
  const [data, setData] = useState<PipelineTrendsResponse | null>(null)
  const [error, setError] = useState(false)

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const res = await authFetch(API_ENDPOINTS.PIPELINES.TRENDS(pipelineId, WINDOW), { cache: "no-store", signal })
        if (!res.ok) {
          setError(true)
          return
        }
        setData((await res.json()) as PipelineTrendsResponse)
        setError(false)
      } catch (e) {
        if ((e as { name?: string })?.name === "AbortError") return
        setError(true)
      }
    },
    [pipelineId]
  )

  useEffect(() => {
    const ac = new AbortController()
    void load(ac.signal)
    const t = window.setInterval(() => void load(ac.signal), POLL_MS)
    return () => {
      ac.abort()
      window.clearInterval(t)
    }
  }, [load])

  const summary = useMemo(() => (data ? summarizeRecentRuns(data) : null), [data])
  // A CDC stream's stats carry the pipeline id as their execution id; that row is not a run.
  const runs = useMemo(() => summary?.runs.filter((r) => r.execution_id !== pipelineId) ?? [], [summary, pipelineId])
  const chart = useMemo(() => recentRunsChart("This pipeline's recent runs: how long each took, coloured by outcome", runs), [runs])

  if (!data && !error) return null

  return (
    <div data-testid="pipeline-recent-runs">
      <ChartCard
        title={summary?.ratePercent != null ? `Recent runs · ${summary.ratePercent}% succeeded` : "Recent runs"}
        description={
          runs.length > 0
            ? `Last ${runs.length} ${runs.length === 1 ? "run" : "runs"}. Bar height is how long a run took, colour its outcome. Click a bar to open its run.`
            : undefined
        }
        empty={
          !summary ? "Couldn't load recent runs." : runs.length === 0 ? "No runs yet. Runs appear here once the pipeline has run." : undefined
        }
        emptyIcon={History}
      >
        <EChartNoSSR
          settings={chart.settings}
          rows={chart.rows}
          onCategoryClick={(i) => {
            const id = runs[i]?.execution_id
            if (id) router.push(`/executions/${id}`)
          }}
        />
      </ChartCard>
      {summary && runs.length > 0 && (
        <div className="mt-2 flex flex-wrap items-baseline justify-between gap-2 px-1">
          <p className="text-xs text-muted-foreground" data-testid="pipeline-recent-runs-summary">
            {summary.finished > 0
              ? `${summary.succeeded} of the last ${summary.finished} finished run${summary.finished === 1 ? "" : "s"} succeeded`
              : "No run has finished yet"}
            {summary.running > 0 && ` · ${summary.running} running`}
            {summary.avgDurationMs != null && ` · average ${formatDuration(summary.avgDurationMs)}`}
          </p>
          <Link
            href={`/pipelines/${pipelineId}?tab=history`}
            className="text-xs font-medium text-zinc-600 hover:underline dark:text-zinc-300"
          >
            All runs →
          </Link>
        </div>
      )}
    </div>
  )
}
