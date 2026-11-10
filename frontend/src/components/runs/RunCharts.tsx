"use client"

import { useMemo } from "react"
import { AlertTriangle, ChartColumn } from "lucide-react"

import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import type { XYSettings } from "@/components/charts/chart-settings"
import { ChartCard, WindowToggle } from "@/components/kit/ChartCard"
import { ChartCardsSkeleton } from "@/components/kit/PageSkeleton"
import { ActivityMetricsProvider, useActivityMetrics } from "@/components/runs/ActivityMetricsProvider"
import {
  CHECK_OUTCOMES,
  MODEL_OUTCOMES,
  PIPELINE_OUTCOMES,
  countChart,
  countRows,
  rowsWrittenChart,
  rowsWrittenRows,
  runTimeChart,
  runTimeRows,
} from "@/components/runs/run-chart-specs"
import { METRICS_WINDOWS, type ActivityMetrics } from "@/lib/api/activity-metrics"

/** What a chart counts: a page about one kind of run draws only its own charts. */
type RunChartSubject = "pipelines" | "models" | "checks"

interface RunChart {
  subject: RunChartSubject
  title: string
  /** One line under the title, saying what is counted. */
  caption: string
  settings: XYSettings
  rows: readonly object[]
  /** False when the window has nothing to draw; the card says so instead. */
  hasData: boolean
  empty: string
}

export function runCharts(m: ActivityMetrics): RunChart[] {
  return [
    {
      subject: "pipelines",
      title: "Pipeline runs",
      caption: "Per day, by outcome",
      settings: countChart("Pipeline runs per day, by outcome", m.days, PIPELINE_OUTCOMES),
      rows: countRows(m.pipeline_runs, PIPELINE_OUTCOMES),
      hasData: m.pipeline_runs.length > 0,
      empty: "No pipeline runs in this window.",
    },
    {
      subject: "pipelines",
      title: "Pipeline run time",
      caption: "Median and 95th percentile, in seconds",
      settings: runTimeChart("Median and 95th-percentile pipeline run time per day, in seconds", m.days),
      rows: runTimeRows(m.days, m.pipeline_run_time),
      hasData: m.pipeline_run_time.length > 0,
      empty: "No pipeline run succeeded or failed in this window.",
    },
    {
      subject: "pipelines",
      title: "Rows written",
      caption: "By pipeline runs, per day",
      settings: rowsWrittenChart("Rows written by pipeline runs per day", m.days),
      rows: rowsWrittenRows(m.rows_written),
      hasData: m.rows_written.length > 0,
      empty: "No pipeline runs in this window.",
    },
    {
      subject: "models",
      title: "Model runs",
      caption: "Per day, by outcome",
      settings: countChart("Model runs per day, by outcome", m.days, MODEL_OUTCOMES),
      rows: countRows(m.model_runs, MODEL_OUTCOMES),
      hasData: m.model_runs.length > 0,
      empty: "No model runs in this window.",
    },
    {
      subject: "models",
      title: "Model run time",
      caption: "Median and 95th percentile, in seconds",
      settings: runTimeChart("Median and 95th-percentile model run time per day, in seconds", m.days),
      rows: runTimeRows(m.days, m.model_run_time),
      hasData: m.model_run_time.length > 0,
      empty: "No model run succeeded or failed in this window.",
    },
    {
      subject: "checks",
      title: "Data checks",
      caption: "Results per day",
      settings: countChart("Data check results per day", m.days, CHECK_OUTCOMES),
      rows: countRows(m.checks, CHECK_OUTCOMES),
      hasData: m.checks.length > 0,
      empty: "No data checks ran in this window.",
    },
  ]
}

/**
 * The run charts on Home's Activity card (dashboards-design.md § 6, step D1, and
 * decision 11): per day over the runs the Runs page lists, counted by the gateway,
 * drawn by ECharts in the browser. The card owns the landmark; `heading` sits
 * beside the window switch. Reads through the page's ActivityMetricsProvider, or
 * its own when there is none. `only` keeps one subject's charts, captioned as
 * counting all of them, for a list page that is about that one kind of run.
 */
export function RunCharts({ heading, only }: { heading?: React.ReactNode; only?: "models" }) {
  const shared = useActivityMetrics()
  const view = <RunChartsView heading={heading} only={only} />
  return shared ? view : <ActivityMetricsProvider>{view}</ActivityMetricsProvider>
}

function RunChartsView({ heading, only }: { heading?: React.ReactNode; only?: "models" }) {
  const { days, setDays, metrics, error, reload } = useActivityMetrics()!
  const charts = useMemo(() => (metrics ? chartsFor(runCharts(metrics), only) : null), [metrics, only])

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-end justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 space-y-0.5">
          {heading}
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Days are {metrics?.time_zone ?? "UTC"} days, by when each run started.
          </p>
        </div>
        <WindowToggle value={days} options={METRICS_WINDOWS} onChange={setDays} />
      </div>

      {error && (
        <div
          role="alert"
          className="flex flex-wrap items-center gap-2 rounded-xl border border-red-200 bg-red-50 p-4 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" aria-hidden />
          {error}
          <button type="button" className="font-medium underline" onClick={reload}>
            Retry
          </button>
        </div>
      )}
      {!charts && !error && <ChartCardsSkeleton count={only ? ONE_SUBJECT_CHART_COUNT : METRICS_CHART_COUNT} />}
      {charts && (
        <div className="grid gap-4 @3xl:grid-cols-2">
          {charts.map((c) => (
            <ChartCard
              key={c.title}
              title={c.title}
              description={c.caption}
              empty={c.hasData ? undefined : c.empty}
              emptyIcon={ChartColumn}
            >
              <EChartNoSSR settings={c.settings} rows={c.rows} />
            </ChartCard>
          ))}
        </div>
      )}
    </div>
  )
}

/** As many placeholders as charts, so the card keeps its height when they land. */
const METRICS_CHART_COUNT = 6
const ONE_SUBJECT_CHART_COUNT = 2

// On /models the list under the charts can be filtered by layer, and the charts are
// not: each caption says it counts every model, so the two are never read as one.
function chartsFor(charts: RunChart[], only?: "models"): RunChart[] {
  if (!only) return charts
  return charts
    .filter((c) => c.subject === only)
    .map((c) => ({ ...c, caption: `All models · ${c.caption.charAt(0).toLowerCase()}${c.caption.slice(1)}` }))
}
