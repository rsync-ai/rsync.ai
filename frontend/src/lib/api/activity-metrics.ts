/**
 * The Runs page's charts: GET /api/v1/activity/metrics (api-gateway
 * handlers/activity_metrics.go).
 *
 * Per-day points over the runs GET /api/v1/activity lists: pipeline and model runs
 * by outcome, their run time, rows written and check results. The server counts;
 * the browser only draws. A day is a UTC day, by when the run started.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

/** The windows the page offers; the server accepts 1–90 days. */
export const METRICS_WINDOWS = [7, 30] as const
export type MetricsWindow = (typeof METRICS_WINDOWS)[number]

export interface MetricCount {
  day: string
  status: string
  count: number
}

export interface MetricRunTime {
  day: string
  runs: number
  median_seconds: number
  p95_seconds: number
}

export interface MetricRows {
  day: string
  rows: number
}

export interface ActivityMetrics {
  /** Every day in the window, oldest first, "YYYY-MM-DD". */
  days: string[]
  time_zone: string
  pipeline_runs: MetricCount[]
  pipeline_run_time: MetricRunTime[]
  rows_written: MetricRows[]
  model_runs: MetricCount[]
  model_run_time: MetricRunTime[]
  checks: MetricCount[]
}

export class ActivityMetricsError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message)
    this.name = "ActivityMetricsError"
  }
}

const arr = <T,>(v: unknown): T[] => (Array.isArray(v) ? (v as T[]) : [])

export async function getActivityMetrics(days: number): Promise<ActivityMetrics> {
  const res = await authFetch(`${API_ENDPOINTS.ACTIVITY.METRICS}?days=${encodeURIComponent(String(days))}`, {
    cache: "no-store",
  })
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: unknown } | null
    const message =
      typeof body?.error === "string" && body.error ? body.error : `Could not load run charts (HTTP ${res.status}).`
    throw new ActivityMetricsError(message, res.status)
  }
  const body = (await res.json()) as Partial<ActivityMetrics> | null
  return {
    days: arr<string>(body?.days),
    time_zone: typeof body?.time_zone === "string" && body.time_zone ? body.time_zone : "UTC",
    pipeline_runs: arr<MetricCount>(body?.pipeline_runs),
    pipeline_run_time: arr<MetricRunTime>(body?.pipeline_run_time),
    rows_written: arr<MetricRows>(body?.rows_written),
    model_runs: arr<MetricCount>(body?.model_runs),
    model_run_time: arr<MetricRunTime>(body?.model_run_time),
    checks: arr<MetricCount>(body?.checks),
  }
}
