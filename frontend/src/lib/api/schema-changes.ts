/**
 * Schema-change (drift) approval API.
 *
 * Backs the approve-UX page at /pipelines/:id/schema-changes — the deep-link the
 * healer emits for CategorySchemaDrift (backend-orchestrator healer.go). The
 * api-gateway is the source of truth (schema_evolution.go):
 *   - LIST   GET  /pipelines/:id/schema-changes        -> { schema_changes }
 *   - APPROVE POST /pipelines/:id/schema-changes/:cid/approve
 *   - REJECT  POST /pipelines/:id/schema-changes/:cid/reject
 *   - POLICY GET/PUT /pipelines/:id/schema-drift-policy -> { schema_drift_policy, detector_enabled }
 * APPROVE/REJECT are owner-gated and only act on a `pending` row; a non-pending
 * row returns 404, which callers treat as a soft "already actioned" signal.
 *
 * Only a dropped source table waits here; every other change is handled on the
 * spot and listed as history. Both decisions on a dropped table take it out of the
 * pipeline — approve also drops the destination table, reject keeps it. When that
 * removal fails (409 for the pipeline's only table or a concurrent table edit, 502
 * when the CDC connector refuses the new list) nothing is saved and the row stays
 * pending; the response's `message` says why.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { extractErrorMessage } from "@/lib/utils/error-handling"

/** One schema-drift change as returned by the LIST endpoint. */
export interface SchemaChange {
  id: string
  pipeline_id: string
  change_type: string
  table_name: string
  ddl: string
  reasoning: string
  risks: string
  user_message: string
  /**
   * `withdrawn`: a dropped-table question the next run disproved — the table is
   * back in the source, or it was only hidden because a connection setting it
   * needs is empty. Nothing was dropped; `reasoning` says which (healer.go
   * WithdrawPendingTableDrops).
   */
  status: "pending" | "approved" | "rejected" | "applied" | "failed" | "withdrawn"
  /**
   * Whether the healer will actually run the DDL after approval. False when the
   * DDL trips the healer's DROP/TRUNCATE guard (applyMigration refuses it even
   * post-approval) — approving then only records the decision and the DDL must
   * be run manually on the destination. A dropped table is auto-applicable:
   * approving it drops the destination table. Optional for older gateways that
   * predate the field.
   */
  auto_applicable?: boolean
  reviewed_by: string | null
  reviewed_at: string | null
  applied_at: string | null
  error_message: string | null
  created_at: string
  updated_at: string
}

/**
 * Thrown when approve/reject targets a change that is no longer `pending`
 * (the backend returns 404). The caller should treat this as "already actioned"
 * — re-fetch the list rather than surfacing a hard error.
 */
export class AlreadyActionedError extends Error {
  constructor(message = "This change was already actioned") {
    super(message)
    this.name = "AlreadyActionedError"
  }
}

/** List all schema-drift changes (pending + resolved) for a pipeline. */
export async function listSchemaChanges(pipelineId: string): Promise<SchemaChange[]> {
  const res = await authFetch(API_ENDPOINTS.PIPELINES.SCHEMA_CHANGES(pipelineId), {
    cache: "no-store",
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: "Failed to load schema changes" }))
    throw new Error(extractErrorMessage(err) || "Failed to load schema changes")
  }
  const data = await res.json().catch(() => null)
  return (data?.schema_changes ?? []) as SchemaChange[]
}

async function postAction(url: string): Promise<Record<string, unknown> | null> {
  const res = await authFetch(url, { method: "POST" })
  if (res.ok) return (await res.json().catch(() => null)) as Record<string, unknown> | null
  if (res.status === 404) {
    throw new AlreadyActionedError()
  }
  const err = await res.json().catch(() => ({ error: "Action failed" }))
  throw new Error(extractErrorMessage(err) || "Action failed")
}

/**
 * What a decision actually did, read from the response rather than predicted here,
 * so the message the user reads is the one the server acted on.
 */
export interface SchemaDecision {
  /**
   * Approve only: `true` when the healer was asked to apply the change (for a
   * dropped table, to drop the destination table), `false` when approval only
   * recorded the decision. `null` when an older gateway omits the field — fall back
   * to the row's own `auto_applicable`.
   */
  dispatched: boolean | null
  /** The change was a dropped source table. */
  dropTable: boolean
  /** The dropped table is no longer part of the pipeline. */
  tableRemoved: boolean
}

function toDecision(body: Record<string, unknown> | null): SchemaDecision {
  return {
    dispatched: typeof body?.auto_applicable === "boolean" ? (body.auto_applicable as boolean) : null,
    dropTable: body?.drop_table === true,
    tableRemoved: body?.table_removed === true,
  }
}

/** Approve a pending change. */
export async function approveSchemaChange(pipelineId: string, changeId: string): Promise<SchemaDecision> {
  return toDecision(await postAction(API_ENDPOINTS.PIPELINES.SCHEMA_CHANGE_APPROVE(pipelineId, changeId)))
}

/** Reject a pending change. Never asks the healer for anything. */
export async function rejectSchemaChange(pipelineId: string, changeId: string): Promise<SchemaDecision> {
  const d = toDecision(await postAction(API_ENDPOINTS.PIPELINES.SCHEMA_CHANGE_REJECT(pipelineId, changeId)))
  return { ...d, dispatched: false }
}

/**
 * Which schema changes the batch detector files for review
 * (pipelines.config.schema_drift_policy; read by the orchestrator's
 * schema_drift.go filterChangesByPolicy). Type changes are not listed: they are
 * always reported, because one can silently break writes to the destination.
 */
export interface SchemaDriftPolicy {
  enabled: boolean
  notify_on_add: boolean
  notify_on_drop: boolean
}

export interface SchemaDriftPolicyState {
  policy: SchemaDriftPolicy
  /**
   * Whether this installation runs the detector at all (RSYNC_SCHEMA_DRIFT_ENABLED).
   * Null for an older gateway that predates the field, so the card says nothing
   * rather than guessing.
   */
  detectorEnabled: boolean | null
}

function toPolicyState(data: unknown): SchemaDriftPolicyState {
  const d = (data ?? {}) as { schema_drift_policy?: Partial<SchemaDriftPolicy>; detector_enabled?: unknown }
  const p = d.schema_drift_policy ?? {}
  // Absent fields read as true, the same default the gateway and orchestrator use.
  return {
    policy: {
      enabled: p.enabled !== false,
      notify_on_add: p.notify_on_add !== false,
      notify_on_drop: p.notify_on_drop !== false,
    },
    detectorEnabled: typeof d.detector_enabled === "boolean" ? d.detector_enabled : null,
  }
}

export async function getSchemaDriftPolicy(pipelineId: string): Promise<SchemaDriftPolicyState> {
  const res = await authFetch(API_ENDPOINTS.PIPELINES.SCHEMA_DRIFT_POLICY(pipelineId), { cache: "no-store" })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: "Failed to load alert settings" }))
    throw new Error(extractErrorMessage(err) || "Failed to load alert settings")
  }
  return toPolicyState(await res.json().catch(() => null))
}

/**
 * Save the whole policy. Always sends all three fields: the gateway resets an
 * absent field to true, so a partial body would silently undo an opt-out.
 */
export async function updateSchemaDriftPolicy(
  pipelineId: string,
  policy: SchemaDriftPolicy
): Promise<SchemaDriftPolicyState> {
  const res = await authFetch(API_ENDPOINTS.PIPELINES.SCHEMA_DRIFT_POLICY(pipelineId), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      enabled: policy.enabled,
      notify_on_add: policy.notify_on_add,
      notify_on_drop: policy.notify_on_drop,
    }),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: "Failed to save alert settings" }))
    throw new Error(extractErrorMessage(err) || "Failed to save alert settings")
  }
  return toPolicyState(await res.json().catch(() => null))
}
