"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { CalendarClock, CheckCircle2, GitBranch } from "lucide-react"

import { WorkflowNodes } from "@/components/icons/WorkflowNodes"
import { StatCard } from "@/components/kit/StatCard"
import { StatusPill } from "@/components/kit/StatusPill"
import { StatCardsSkeleton } from "@/components/kit/PageSkeleton"
import { Skeleton } from "@/components/ui/skeleton"
import { useActivityMetrics } from "@/components/runs/ActivityMetricsProvider"
import type { ActivityMetrics, MetricCount } from "@/lib/api/activity-metrics"
import { parseOpenBreaches, type ModelFreshnessBreach } from "@/components/explorer/liveState"
import type { ScheduledQuery } from "@/components/explorer/scheduledModel"
import { readStatOrUnknown } from "@/lib/api/read-stat"
import type { Workflow } from "@/lib/api/workflows"
import {
  attentionItems,
  dailyTotals,
  summarizePipelines,
  summarizeSchedules,
  summarizeWorkflows,
  type AttentionKind,
  type PipelineListBody,
} from "@/lib/home/overview"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/** How many things "Needs attention" lists before it says how many more there are. */
export const ATTENTION_LIMIT = 6
/** The most failed (and waiting) pipelines Home asks for by name. */
const PIPELINE_ATTENTION_LIMIT = 10
/** The days a card's trend line covers, whichever window the charts show. */
const TREND_DAYS = 7

interface OverviewReads {
  all?: PipelineListBody
  failed?: PipelineListBody
  waiting?: PipelineListBody
  schedules?: ScheduledQuery[]
  breaches: ModelFreshnessBreach[] | null
  workflows?: Workflow[]
}

async function readOverview(): Promise<OverviewReads> {
  const [all, failed, waiting, schedules, freshness, workflows] = await Promise.all([
    readStatOrUnknown<PipelineListBody>("/api/v1/pipelines?limit=1"),
    readStatOrUnknown<PipelineListBody>(`/api/v1/pipelines?status=failed&limit=${PIPELINE_ATTENTION_LIMIT}`),
    readStatOrUnknown<PipelineListBody>(`/api/v1/pipelines?status=waiting_for_user&limit=${PIPELINE_ATTENTION_LIMIT}`),
    readStatOrUnknown<{ schedules?: ScheduledQuery[] }>("/api/v1/explorer/schedules"),
    readStatOrUnknown("/api/v1/explorer/freshness"),
    readStatOrUnknown<{ workflows?: Workflow[] }>("/api/v1/workflows"),
  ])
  return {
    all,
    failed,
    waiting,
    schedules: Array.isArray(schedules?.schedules) ? schedules.schedules : undefined,
    breaches: freshness === undefined ? null : parseOpenBreaches(freshness),
    workflows: Array.isArray(workflows?.workflows) ? workflows.workflows : undefined,
  }
}

const KIND_LABEL: Record<AttentionKind, string> = {
  pipeline: "Pipeline",
  scheduled_query: "Model",
  workflow: "Workflow",
}

/**
 * Home's overview: how many pipelines, scheduled queries and workflows the
 * workspace has, and the ones that need someone. Counts only; each run is on
 * Runs, each thing on its own page. A list it could not read shows "—", never 0.
 * Under an ActivityMetricsProvider the pipeline and model cards add a 7-day
 * trend line from the same read as the charts.
 */
export function HomeOverview() {
  const [reads, setReads] = useState<OverviewReads | null>(null)
  const [version, setVersion] = useState(0)
  const seq = useRef(0)
  const activity = useActivityMetrics()

  const reload = useCallback(() => {
    setReads(null)
    setVersion((v) => v + 1)
  }, [])

  useEffect(() => onActiveWorkspaceChange(reload), [reload])

  useEffect(() => {
    const mine = ++seq.current
    const isStale = captureWorkspace()
    void readOverview().then((next) => {
      if (mine !== seq.current || isStale()) return
      setReads(next)
    })
  }, [version])

  if (!reads) {
    return (
      <div className="space-y-4">
        <StatCardsSkeleton count={3} label="Loading the overview…" />
        <Skeleton aria-hidden className="h-24 rounded-xl" />
      </div>
    )
  }

  // No provider: no trend. Loading: the last answer's line (another window
  // covers the same 7 days), else a placeholder of the same height. Failed: none.
  const trend = (pick: (m: ActivityMetrics) => MetricCount[]): readonly number[] | null | undefined => {
    if (!activity || activity.error) return undefined
    const m = activity.metrics ?? activity.latest
    return m ? dailyTotals(m.days, pick(m), TREND_DAYS) : null
  }

  const pipelines = summarizePipelines(reads.all, reads.failed, reads.waiting)
  const schedules = reads.schedules ? summarizeSchedules(reads.schedules, reads.breaches) : undefined
  const workflows = reads.workflows ? summarizeWorkflows(reads.workflows) : undefined
  const items = attentionItems({
    failedPipelines: reads.failed?.pipelines,
    waitingPipelines: reads.waiting?.pipelines,
    schedules: reads.schedules,
    breaches: reads.breaches,
    workflows: reads.workflows,
  })
  const unreadable =
    !reads.all || !reads.failed || !reads.waiting || !reads.schedules || reads.breaches === null || !reads.workflows

  return (
    <div className="space-y-4">
      <div className="grid gap-4 @3xl:grid-cols-3">
        <StatCard
          title="Pipelines"
          href="/pipelines"
          icon={GitBranch}
          value={pipelines.total}
          facts={[
            { label: "failed", value: pipelines.failed, alert: true },
            { label: "waiting for you", value: pipelines.waiting, alert: true },
          ]}
          trend={trend((m) => m.pipeline_runs)}
          trendLabel={`Pipeline runs per day, last ${TREND_DAYS} days`}
        />
        <StatCard
          title="Scheduled models"
          href="/models"
          icon={CalendarClock}
          value={schedules?.total}
          facts={[
            { label: "active", value: schedules?.active },
            { label: "failed last run", value: schedules?.failed, alert: true },
            { label: "stopped by the system", value: schedules?.autoPaused, alert: true },
            { label: "overdue", value: schedules?.late, alert: true },
          ]}
          trend={trend((m) => m.model_runs)}
          trendLabel={`Model runs per day, last ${TREND_DAYS} days`}
        />
        <StatCard
          title="Workflows"
          href="/workflows"
          icon={WorkflowNodes}
          value={workflows?.total}
          facts={[
            { label: "on", value: workflows?.on },
            { label: "paused", value: workflows?.paused },
            { label: workflows?.drafts === 1 ? "draft" : "drafts", value: workflows?.drafts },
            { label: "failed last run", value: workflows?.failed, alert: true },
          ]}
        />
      </div>

      <section
        aria-labelledby="home-attention-heading"
        className="rounded-xl border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950"
      >
        <div className="flex items-center gap-2">
          <h2 id="home-attention-heading" className="text-sm font-semibold text-zinc-900 dark:text-white">
            Needs attention
          </h2>
          {items.length > 0 && (
            <span className="rounded-full bg-red-50 px-2 py-0.5 text-xs font-medium tabular-nums text-red-700 dark:bg-red-400/10 dark:text-red-300">
              {items.length}
            </span>
          )}
        </div>
        {items.length === 0 ? (
          <p className="mt-2 flex items-center gap-2 text-sm text-zinc-600 dark:text-zinc-300">
            <CheckCircle2 className="h-4 w-4 text-emerald-600 dark:text-emerald-400" aria-hidden />
            {unreadable ? "Nothing in the lists that loaded." : "Nothing needs you right now."}
          </p>
        ) : (
          <ul className="mt-2 divide-y divide-zinc-100 dark:divide-zinc-800">
            {items.slice(0, ATTENTION_LIMIT).map((item) => (
              <li key={`${item.kind}:${item.id}`} className="flex flex-wrap items-center gap-x-2 gap-y-1 py-2 text-sm">
                <StatusPill tone={item.tone} title={item.reason}>
                  {item.status}
                </StatusPill>
                <Link href={item.href} className="min-w-0 truncate font-medium text-zinc-900 hover:underline dark:text-white">
                  {item.name || "Unnamed"}
                </Link>
                <span className="text-xs text-zinc-500 dark:text-zinc-400">{KIND_LABEL[item.kind]}</span>
                {item.detail && (
                  <span className="min-w-0 basis-full truncate text-xs text-zinc-600 dark:text-zinc-300 @xl:basis-auto">
                    {item.detail}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
        {items.length > ATTENTION_LIMIT && (
          <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            And {items.length - ATTENTION_LIMIT} more.
          </p>
        )}
        {unreadable && (
          <p className="mt-2 text-xs text-amber-700 dark:text-amber-300">
            Some of this could not be loaded, so a count shows “—”.{" "}
            <button type="button" onClick={reload} className="underline">
              Retry
            </button>
          </p>
        )}
      </section>
    </div>
  )
}
