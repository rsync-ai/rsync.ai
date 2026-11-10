/**
 * Home's overview, worked out from what the three list endpoints return. Pure,
 * so the counting rules are tested without rendering anything.
 *
 * A count Home could not read is `undefined`, never 0: "we don't know" and
 * "there are none" must not look the same (see lib/api/read-stat.ts).
 */

import type { ModelFreshnessBreach } from "@/components/explorer/liveState"
import type { ScheduledQuery } from "@/components/explorer/scheduledModel"
import type { Workflow } from "@/lib/api/workflows"

/** The fields Home reads from one GET /api/v1/pipelines row. */
export interface PipelineRow {
  id: string
  name?: string
  derived_status?: string
}

/** GET /api/v1/pipelines; `total` honours the `status` filter. */
export interface PipelineListBody {
  pipelines?: PipelineRow[]
  total?: number
}

export interface PipelineSummary {
  total?: number
  failed?: number
  waiting?: number
}

export interface ScheduleSummary {
  total: number
  /** Active and not stopped by the system. */
  active: number
  failed: number
  autoPaused: number
  /** Distinct queries whose table is past its freshness deadline; unknown when the read failed. */
  late?: number
}

export interface WorkflowSummary {
  total: number
  on: number
  paused: number
  /** Never turned on; with on and paused, adds up to the total. */
  drafts: number
  failed: number
}

export type AttentionKind = "pipeline" | "scheduled_query" | "workflow"

export interface AttentionItem {
  kind: AttentionKind
  id: string
  name: string
  /** The whole reason, as one sentence. */
  reason: string
  /** The reason in a word or two, for the status pill, and the rest of it. */
  status: string
  detail?: string
  /** fail: something broke; wait: something is held up. */
  tone: "fail" | "wait"
  href: string
}

/** A pipeline list's total, or undefined if the body did not carry one. */
export function pipelineTotal(body: PipelineListBody | undefined): number | undefined {
  if (!body) return undefined
  if (typeof body.total === "number") return body.total
  return Array.isArray(body.pipelines) ? body.pipelines.length : undefined
}

export function summarizePipelines(
  all: PipelineListBody | undefined,
  failed: PipelineListBody | undefined,
  waiting: PipelineListBody | undefined,
): PipelineSummary {
  return { total: pipelineTotal(all), failed: pipelineTotal(failed), waiting: pipelineTotal(waiting) }
}

const isAutoPaused = (s: Pick<ScheduledQuery, "auto_paused_at" | "auto_paused_reason">) =>
  !!(s.auto_paused_at || s.auto_paused_reason)

export function summarizeSchedules(
  schedules: ScheduledQuery[],
  breaches: ModelFreshnessBreach[] | null,
): ScheduleSummary {
  return {
    total: schedules.length,
    active: schedules.filter((s) => s.status === "active" && !isAutoPaused(s)).length,
    failed: schedules.filter((s) => s.last_run_status === "failed").length,
    autoPaused: schedules.filter(isAutoPaused).length,
    late: breaches === null ? undefined : new Set(breaches.map((b) => b.saved_query_id)).size,
  }
}

export function summarizeWorkflows(workflows: Workflow[]): WorkflowSummary {
  return {
    total: workflows.length,
    on: workflows.filter((w) => w.status === "active").length,
    paused: workflows.filter((w) => w.status === "paused").length,
    drafts: workflows.filter((w) => w.status === "draft").length,
    failed: workflows.filter((w) => w.last_run?.status === "failed").length,
  }
}

/**
 * Runs per day for a card's trend line: every outcome added up, for the last
 * `last` days of the window, oldest first. A day with no runs is 0.
 */
export function dailyTotals(
  days: readonly string[],
  counts: readonly { day: string; count: number }[],
  last = 7,
): number[] {
  const byDay = new Map<string, number>()
  for (const c of counts) byDay.set(c.day, (byDay.get(c.day) ?? 0) + c.count)
  return days.slice(-last).map((d) => byDay.get(d) ?? 0)
}

const pipelineHref = (id: string) => `/pipelines/${encodeURIComponent(id)}`
const scheduleHref = (savedQueryId: string) => `/models/${encodeURIComponent(savedQueryId)}`
const workflowHref = (id: string) => `/workflows/${encodeURIComponent(id)}`

/**
 * What needs someone, most urgent first: a failed or waiting pipeline, a
 * scheduled query the system stopped, failed or let go stale, a workflow the
 * system paused or whose last run failed. One line per thing; the first reason
 * found for it wins. A list that could not be read adds nothing.
 */
export function attentionItems(input: {
  failedPipelines?: PipelineRow[]
  waitingPipelines?: PipelineRow[]
  schedules?: ScheduledQuery[]
  breaches?: ModelFreshnessBreach[] | null
  workflows?: Workflow[]
}): AttentionItem[] {
  const items: AttentionItem[] = []
  const seen = new Set<string>()
  const add = (item: AttentionItem) => {
    const key = `${item.kind}:${item.id}`
    if (seen.has(key)) return
    seen.add(key)
    items.push(item)
  }

  for (const p of input.failedPipelines ?? []) {
    add({
      kind: "pipeline",
      id: p.id,
      name: p.name || "Unnamed pipeline",
      reason: "Failed",
      status: "Failed",
      tone: "fail",
      href: pipelineHref(p.id),
    })
  }
  for (const p of input.waitingPipelines ?? []) {
    add({
      kind: "pipeline",
      id: p.id,
      name: p.name || "Unnamed pipeline",
      reason: "Waiting for you",
      status: "Waiting for you",
      tone: "wait",
      href: pipelineHref(p.id),
    })
  }

  const schedules = input.schedules ?? []
  for (const s of schedules.filter(isAutoPaused)) {
    add({
      kind: "scheduled_query",
      id: s.saved_query_id,
      name: s.name,
      reason: s.auto_paused_reason ? `Stopped: ${s.auto_paused_reason}` : "Stopped by the system",
      status: "Stopped",
      detail: s.auto_paused_reason || "by the system",
      tone: "fail",
      href: scheduleHref(s.saved_query_id),
    })
  }
  for (const s of schedules.filter((s) => s.last_run_status === "failed")) {
    add({
      kind: "scheduled_query",
      id: s.saved_query_id,
      name: s.name,
      reason: "Last run failed",
      status: "Last run failed",
      tone: "fail",
      href: scheduleHref(s.saved_query_id),
    })
  }
  for (const b of input.breaches ?? []) {
    add({
      kind: "scheduled_query",
      id: b.saved_query_id,
      name: b.name,
      reason: b.target_table ? `${b.target_table} is overdue` : "Overdue",
      status: "Overdue",
      detail: b.target_table || undefined,
      tone: "wait",
      href: scheduleHref(b.saved_query_id),
    })
  }

  const workflows = input.workflows ?? []
  for (const w of workflows.filter((w) => w.status === "paused" && w.paused_reason)) {
    add({
      kind: "workflow",
      id: w.id,
      name: w.name,
      reason: `Paused: ${w.paused_reason}`,
      status: "Paused",
      detail: w.paused_reason,
      tone: "wait",
      href: workflowHref(w.id),
    })
  }
  for (const w of workflows.filter((w) => w.last_run?.status === "failed")) {
    add({
      kind: "workflow",
      id: w.id,
      name: w.name,
      reason: "Last run failed",
      status: "Last run failed",
      tone: "fail",
      href: workflowHref(w.id),
    })
  }

  return items
}
