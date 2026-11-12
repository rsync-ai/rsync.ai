/**
 * Pipeline plans: a pipeline Ask rsync.ai or an agent token proposed with the
 * plan_pipeline tool. A plan creates nothing; the person it was proposed for
 * opens it and clicks Create, with their own session. handlers/pipeline_plans.go
 *   GET  /pipeline-plans/:id        -> PipelinePlan (only its own person's; 404 otherwise)
 *   POST /pipeline-plans/:id/apply  { tables? } -> AppliedPlan
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

export interface PlanConnection {
  id: string
  name: string
  connector_type: string
}

/** A table name the source has in more than one schema: the person picks one. */
export interface PlanTableChoice {
  name: string
  options: string[]
}

export interface PlanSchedule {
  type: "cron" | "interval"
  cron?: string
  every_seconds?: number
  timezone?: string
  /** In words, e.g. "every hour (UTC)". */
  description: string
}

export interface PipelinePlan {
  id: string
  status: "proposed" | "applied" | "expired"
  /** Who proposed it: "assistant" (Ask rsync.ai) or "agent" (an agent token). */
  proposed_by: string
  source: PlanConnection
  destination: PlanConnection
  sync_mode: "batch" | "cdc"
  cdc_mode?: string
  /** Checked against the source when tables_checked; schema-qualified then. */
  tables: string[]
  table_choices?: PlanTableChoice[]
  missing_tables?: string[]
  tables_checked: boolean
  schedule?: PlanSchedule
  destination_namespace?: string
  notes?: string[]
  url: string
  created_at: string
  expires_at: string
  applied_pipeline_id?: string
}

export interface AppliedPlan {
  pipeline_id: string
  status: "running" | "scheduled"
  schedule?: string
  note?: string
  /** The pipeline exists, but its first run or its schedule did not start. */
  warning?: string
}

/**
 * A refused read or apply. code is the server's: choose_tables, table_not_in_plan,
 * already_applied (with appliedPipelineId), expired, plan_changed, create_failed,
 * or "" when the server sent none (a 404, a 403).
 */
export class PipelinePlanError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
    readonly appliedPipelineId?: string,
  ) {
    super(message)
    this.name = "PipelinePlanError"
  }
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as
      | { error?: unknown; code?: unknown; applied_pipeline_id?: unknown }
      | null
    const message = typeof body?.error === "string" && body.error ? body.error : fallback
    throw new PipelinePlanError(
      res.status === 404 ? "This plan was not found. A plan opens only for the person it was proposed for, in its workspace." : message,
      res.status,
      typeof body?.code === "string" ? body.code : "",
      typeof body?.applied_pipeline_id === "string" ? body.applied_pipeline_id : undefined,
    )
  }
  return res.json() as Promise<T>
}

export async function getPipelinePlan(id: string): Promise<PipelinePlan> {
  const res = await authFetch(API_ENDPOINTS.PIPELINE_PLANS.GET(id))
  const plan = await readJSON<PipelinePlan>(res, "Could not load the plan")
  if (typeof plan?.id !== "string" || !plan.source || !plan.destination) {
    throw new PipelinePlanError("Unexpected plan response", res.status, "bad_response")
  }
  return { ...plan, tables: Array.isArray(plan.tables) ? plan.tables : [] }
}

/**
 * Creates the plan's pipeline. tables, when given, replaces the plan's own list
 * and may name only tables the plan holds or offers as a choice; leave it out
 * to create with the plan's tables (refused while a choice is open).
 */
export async function applyPipelinePlan(id: string, tables?: string[]): Promise<AppliedPlan> {
  const res = await authFetch(API_ENDPOINTS.PIPELINE_PLANS.APPLY(id), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(tables && tables.length > 0 ? { tables } : {}),
    timeoutMs: 60_000,
  })
  const data = await readJSON<AppliedPlan>(res, "Could not create the pipeline")
  if (typeof data?.pipeline_id !== "string" || !data.pipeline_id) {
    throw new PipelinePlanError("Unexpected response from create", res.status, "bad_response")
  }
  return data
}

/** The tables to create with: the plan's, plus the person's pick for each choice. */
export function tablesToApply(plan: PipelinePlan, picks: Record<string, string>): string[] | undefined {
  const choices = plan.table_choices ?? []
  if (choices.length === 0) return undefined
  return [...plan.tables, ...choices.map((c) => picks[c.name]).filter((t): t is string => !!t)]
}
