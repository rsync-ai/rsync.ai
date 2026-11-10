/**
 * The Runs page's data: GET /api/v1/activity (api-gateway handlers/activity.go).
 *
 * One list of the active workspace's pipeline runs, scheduled-query runs and
 * workflow runs, newest first. A page is keyset: pass the `next_cursor` of the
 * previous page to get the next one; no `next_cursor` means the last page.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

export type ActivityKind = "pipeline" | "scheduled_query" | "workflow"

/** One status word across the three kinds; `source_status` keeps the kind's own. */
export type ActivityStatus = "running" | "waiting" | "succeeded" | "failed" | "cancelled" | "skipped"

export interface ActivityRun {
  kind: ActivityKind
  id: string
  /** The pipeline, saved query or workflow the run belongs to. */
  subject_id: string
  subject_name: string
  status: ActivityStatus
  source_status: string
  trigger: string
  started_at: string
  finished_at?: string
  error?: string
  /** A pipeline run a schedule started: that schedule. */
  schedule_id?: string
}

export interface ActivityPage {
  runs: ActivityRun[]
  next_cursor?: string
}

export interface ActivityQuery {
  kind?: ActivityKind
  status?: ActivityStatus
  limit?: number
  cursor?: string
}

export class ActivityError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message)
    this.name = "ActivityError"
  }
}

export async function listActivity(query: ActivityQuery = {}): Promise<ActivityPage> {
  const params = new URLSearchParams()
  if (query.kind) params.set("kind", query.kind)
  if (query.status) params.set("status", query.status)
  if (query.limit) params.set("limit", String(query.limit))
  if (query.cursor) params.set("cursor", query.cursor)
  const qs = params.toString()
  const res = await authFetch(qs ? `${API_ENDPOINTS.ACTIVITY.LIST}?${qs}` : API_ENDPOINTS.ACTIVITY.LIST, {
    cache: "no-store",
  })
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: unknown } | null
    const message = typeof body?.error === "string" && body.error ? body.error : `Could not load runs (HTTP ${res.status}).`
    throw new ActivityError(message, res.status)
  }
  const body = (await res.json()) as Partial<ActivityPage> | null
  return {
    runs: Array.isArray(body?.runs) ? body.runs : [],
    next_cursor: typeof body?.next_cursor === "string" && body.next_cursor ? body.next_cursor : undefined,
  }
}

/**
 * Where a run opens. A pipeline run has its own page; a scheduled-query run and
 * a workflow run are listed on their query's or workflow's page.
 */
export function activityHref(run: Pick<ActivityRun, "kind" | "id" | "subject_id">): string {
  switch (run.kind) {
    case "pipeline":
      return `/executions/${encodeURIComponent(run.id)}`
    case "scheduled_query":
      return `/models/${encodeURIComponent(run.subject_id)}`
    case "workflow":
      return `/workflows/${encodeURIComponent(run.subject_id)}`
  }
}

/** Where a scheduled pipeline run's trigger links: its schedule on the pipeline's page. */
export function activityScheduleHref(run: Pick<ActivityRun, "kind" | "subject_id" | "schedule_id">): string | undefined {
  if (run.kind !== "pipeline" || !run.schedule_id) return undefined
  return `/pipelines/${encodeURIComponent(run.subject_id)}?scheduleId=${encodeURIComponent(run.schedule_id)}#schedules`
}

export const ACTIVITY_KIND_LABELS: Record<ActivityKind, string> = {
  pipeline: "Pipeline",
  scheduled_query: "Model",
  workflow: "Workflow",
}

/** The Runs page's kind filters. Spelled out, so a label never has to be a plural rule. */
export const ACTIVITY_KIND_PLURAL_LABELS: Record<ActivityKind, string> = {
  pipeline: "Pipelines",
  scheduled_query: "Models",
  workflow: "Workflows",
}

export const ACTIVITY_STATUS_LABELS: Record<ActivityStatus, string> = {
  running: "Running",
  waiting: "Waiting for you",
  succeeded: "Succeeded",
  failed: "Failed",
  cancelled: "Cancelled",
  skipped: "Skipped",
}

export function isActivityKind(v: unknown): v is ActivityKind {
  return typeof v === "string" && Object.hasOwn(ACTIVITY_KIND_LABELS, v)
}

export function isActivityStatus(v: unknown): v is ActivityStatus {
  return typeof v === "string" && Object.hasOwn(ACTIVITY_STATUS_LABELS, v)
}

/**
 * The trigger as the page says it. The stored words: executions
 * manual/scheduled/cdc/batch (migration 065), saved_query_runs
 * manual/scheduled/triggered (095), workflow_runs schedule/after_pipeline/manual
 * (130), and "workflow" for a model a workflow step rebuilt (activity.go). An
 * unknown word is shown as stored.
 */
export function activityTriggerLabel(trigger: string): string {
  switch (trigger) {
    case "manual":
      return "Manual"
    case "schedule":
    case "scheduled":
      return "Schedule"
    case "after_pipeline":
      return "After a pipeline"
    case "triggered":
      return "After its upstream"
    case "workflow":
      return "By a workflow"
    case "cdc":
      return "CDC"
    case "batch":
      return "Batch"
    default:
      return trigger ? trigger.replace(/_/g, " ") : "—"
  }
}
