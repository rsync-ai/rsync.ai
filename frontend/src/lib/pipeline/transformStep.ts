import {
  resumePipelineNodeInput,
  resumePipelineTables,
  type DestinationConfig,
} from "@/lib/api/pipelines"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import type { TableMetadata } from "@/components/chat/SuggestionsReviewDialog"

/**
 * The transform step of a table-selection pause: after the tables are chosen,
 * SuggestionsReviewDialog offers AI transform suggestions, saves the accepted
 * ones to transform_definitions, and only then is the run resumed. The chat
 * (useHITLState) and the pipeline pages (usePipelineTransformStep) share these
 * pieces so the two cannot drift apart.
 */

/** How the user left the transform step: rules saved (count), or skipped. */
export type TransformStepOutcome = { saved: number } | { skipped: true }

export interface TransformStepResume {
  executionId?: string
  // A DAG node-input pause names its node; the table signal is used otherwise.
  nodeId?: string
  tables: string[]
  destinationConfig?: DestinationConfig | null
  outcome: TransformStepOutcome
}

/**
 * Resumes the paused run after the transform step. The rules are already in
 * transform_definitions, the one place the batch executor and the CDC sink read
 * them from, so the resume carries only how many were added: a `transforms`
 * list here would override those rows on the batch path (executor.go
 * applyTransformsToData reads metadata.transforms first), and a later edit in
 * the Transforms tab would not reach this run's initial load.
 */
export async function resumeAfterTransformStep(pipelineId: string, r: TransformStepResume): Promise<void> {
  const metadata =
    "saved" in r.outcome
      ? { transforms_saved: r.outcome.saved, applied_at: new Date().toISOString() }
      : { skipped_suggestions: true, skipped_at: new Date().toISOString() }
  const destCfg = r.destinationConfig
  const nodeId = String(r.nodeId || "").trim()
  if (nodeId) {
    await resumePipelineNodeInput(pipelineId, {
      execution_id: r.executionId,
      node_id: nodeId,
      config_patch: {
        selected_tables: r.tables,
        metadata,
        ...(destCfg ? { destination_config: destCfg } : {}),
      },
    })
    return
  }
  await resumePipelineTables(pipelineId, {
    execution_id: r.executionId,
    selected_tables: r.tables,
    metadata,
    ...(destCfg ? { destination_config: destCfg } : {}),
  })
}

/**
 * The column metadata the transform step suggests from, fetched while the
 * dialog opens. Best effort: on any failure the dialog loads it itself.
 */
export async function fetchTransformStepSchema(sourceConnectionId: string, tables: string[]): Promise<TableMetadata[]> {
  try {
    const res = await authFetch(
      `${API_ENDPOINTS.CONNECTIONS.GET(sourceConnectionId)}/metadata?tables=${encodeURIComponent(tables.join(","))}&include_columns=true`,
      { cache: "no-store" }
    )
    if (!res.ok) return []
    const data = (await res.json()) as Record<string, unknown>
    return Array.isArray(data?.["tables"]) ? (data["tables"] as TableMetadata[]) : []
  } catch {
    return []
  }
}

/**
 * The tables of a wait raised only to show the transform step for tables that
 * were already chosen (`review_transforms_only` plus `preselected_tables`,
 * orchestrator transforms_review_gate.go), or null for an ordinary
 * table-selection wait.
 */
export function transformsReviewTables(details: Record<string, unknown> | null | undefined): string[] | null {
  if (!details || details["review_transforms_only"] !== true) return null
  const raw = details["preselected_tables"]
  if (!Array.isArray(raw)) return null
  const tables = raw.filter((t): t is string => typeof t === "string" && t.trim() !== "").map((t) => t.trim())
  return tables.length > 0 ? tables : null
}
