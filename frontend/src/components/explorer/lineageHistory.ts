/**
 * An asset's stored lineage history (GET /api/v1/lineage/history, api-gateway
 * lineage_history.go): the runs recorded against it, newest first, and for a model the
 * times its SQL changed with the columns that came and went.
 *
 * The graph is derived on every request and says what is linked now; this is what ran,
 * kept for the backend's retention (LINEAGE_EVENT_RETENTION_DAYS, 365 by default). A run's
 * tables are names only, never their rows.
 */

import type { AssetNode } from "@/components/explorer/assetLineage"
import { formatDurationBetween } from "@/lib/duration"

export const LINEAGE_HISTORY_URL = "/api/v1/lineage/history"

/** A table a run read or wrote. `captured`: written under no destination name. */
export interface LineageDataset {
  connection_id: string
  name: string
  captured: boolean
}

export interface LineageHistoryRun {
  id: string
  /** "model" or "pipeline". */
  job_kind: string
  job_id: string
  job_name: string
  /** The model run, the pipeline execution, or the CDC window it was recorded for. */
  run_ref: string
  /** COMPLETE, FAIL or ABORT: a model run that was skipped, or a pipeline run that was cancelled. */
  event_type: string
  started_at: string | null
  ended_at: string
  inputs: LineageDataset[]
  outputs: LineageDataset[]
}

/** How a model's output columns changed when its SQL did. */
export interface LineageColumnChange {
  at: string
  /** False when either version's columns were not read: added and dropped are then empty. */
  columns_known: boolean
  added: string[]
  dropped: string[]
}

export interface LineageHistory {
  node: string
  /** False for an asset nothing runs against that rsync.ai records: a lake table, a chart. */
  supported: boolean
  retention_days: number
  /**
   * Every run recorded against the asset that the list covers; `runs` holds only the newest
   * of them. A model's skipped runs are not among them unless they were asked for.
   */
  total_runs: number
  runs: LineageHistoryRun[]
  /**
   * A model's skipped runs on record. The list leaves them out unless they were asked for
   * (a model skipped every hour has a few runs that ran and hundreds that did not, and they
   * would fill the page), so this is what the page says is not shown. 0 for the other kinds.
   */
  skipped_runs: number
  /** When the latest of them was skipped, or null when there are none. */
  last_skipped_at: string | null
  /** A model's only; null for the other kinds. */
  column_changes: LineageColumnChange[] | null
}

const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : [])
const str = (v: unknown): string => (typeof v === "string" ? v : "")
const obj = (v: unknown): Record<string, unknown> => (v && typeof v === "object" ? (v as Record<string, unknown>) : {})

function parseDatasets(v: unknown): LineageDataset[] {
  return arr(v).flatMap((d) => {
    const o = obj(d)
    const name = str(o.name)
    return name ? [{ connection_id: str(o.connection_id), name, captured: o.captured === true }] : []
  })
}

function parseNames(v: unknown): string[] {
  return arr(v).filter((s): s is string => typeof s === "string")
}

export function parseLineageHistory(body: unknown): LineageHistory {
  const b = obj(body)
  const days = typeof b.retention_days === "number" && b.retention_days > 0 ? b.retention_days : 0
  const runs = arr(b.runs).map((r) => {
    const o = obj(r)
    return {
      id: str(o.id),
      job_kind: str(o.job_kind),
      job_id: str(o.job_id),
      job_name: str(o.job_name),
      run_ref: str(o.run_ref),
      event_type: str(o.event_type),
      started_at: str(o.started_at) || null,
      ended_at: str(o.ended_at),
      inputs: parseDatasets(o.inputs),
      outputs: parseDatasets(o.outputs),
    }
  })
  // A server that does not send the total (an older one) is counted by what it sent; a total
  // below that is not believed.
  const total = typeof b.total_runs === "number" && Number.isFinite(b.total_runs) ? b.total_runs : 0
  const skipped = typeof b.skipped_runs === "number" && Number.isFinite(b.skipped_runs) && b.skipped_runs > 0 ? b.skipped_runs : 0
  return {
    node: str(b.node),
    supported: b.supported === true,
    retention_days: days,
    total_runs: Math.max(total, runs.length),
    runs,
    skipped_runs: skipped,
    last_skipped_at: skipped > 0 ? str(b.last_skipped_at) || null : null,
    column_changes: Array.isArray(b.column_changes)
      ? b.column_changes.map((c) => {
          const o = obj(c)
          return {
            at: str(o.at),
            columns_known: o.columns_known === true,
            added: parseNames(o.added),
            dropped: parseNames(o.dropped),
          }
        })
      : null,
  }
}

/** `skipped`: list a model's skipped runs too (the server leaves them out, and counts them). */
export function lineageHistoryUrl(node: string, limit?: number, skipped = false): string {
  const q = `node=${encodeURIComponent(node)}`
  return `${LINEAGE_HISTORY_URL}?${q}${limit ? `&limit=${limit}` : ""}${skipped ? "&skipped=1" : ""}`
}

/**
 * Whether the backend keeps runs for this card (lineageHistoryFilterFor): a pipeline, a
 * model, and a table a pipeline or a model writes. A lake table, a chart and a dashboard
 * have none, so the page does not ask.
 */
export function hasLineageHistory(node: Pick<AssetNode, "id" | "kind">): boolean {
  if (node.kind === "pipeline" || node.kind === "model") return true
  if (node.kind !== "table") return false
  return node.id.split("\x00")[1] !== "lake"
}

/**
 * How a run ended, in the page's words. The job kind is part of the answer: ABORT is a
 * model run that was skipped (it never ran) but a pipeline run that was cancelled (it ran
 * until it was stopped), and one word for both says the wrong thing about one of them.
 */
export function runOutcome(eventType: string, jobKind: string): { text: string; tone: "ok" | "failed" | "neutral" } {
  switch (eventType) {
    case "COMPLETE":
      return { text: "Completed", tone: "ok" }
    case "FAIL":
      return { text: "Failed", tone: "failed" }
    case "ABORT":
      return { text: jobKind === "pipeline" ? "Cancelled" : "Skipped", tone: "neutral" }
    default:
      return { text: eventType ? eventType.toLowerCase() : "Unknown", tone: "neutral" }
  }
}

/** A CDC pipeline never ends; each of its runs is a window of changes that landed. */
export function isCDCWindow(run: Pick<LineageHistoryRun, "job_kind" | "run_ref">): boolean {
  return run.job_kind === "pipeline" && run.run_ref.startsWith("cdc-trail-")
}

/**
 * How long a run took, in the shared words; "" when its start was not recorded, or it
 * began and ended in the same instant (a skipped run: "took 0ms" would claim a run).
 */
export function runDuration(run: Pick<LineageHistoryRun, "started_at" | "ended_at">): string {
  if (!run.started_at || !run.ended_at) return ""
  if (Date.parse(run.started_at) === Date.parse(run.ended_at)) return ""
  const took = formatDurationBetween(run.started_at, run.ended_at)
  return took === "—" ? "" : took
}

/**
 * The graph's card for a run's table, or undefined when the graph does not draw it now.
 * Its id first (buildAssetGraph: `table:<conn>\0<name>`, a captured one with `captured`
 * between), then the same without case, then the one table of that connection whose
 * last name part it is: a model that reads `orders` unqualified is recorded so.
 */
export function datasetNode(d: LineageDataset, byId: ReadonlyMap<string, AssetNode>): AssetNode | undefined {
  const id = d.captured ? `table:${d.connection_id}\x00captured\x00${d.name}` : `table:${d.connection_id}\x00${d.name}`
  const exact = byId.get(id)
  if (exact) return exact
  const lower = id.toLowerCase()
  const bare = d.name.toLowerCase()
  let byLast: AssetNode | undefined
  let lastCount = 0
  for (const n of byId.values()) {
    if (n.kind !== "table") continue
    if (n.id.toLowerCase() === lower) return n
    if (d.captured || bare.includes(".")) continue
    const parts = n.id.split("\x00")
    if (parts.length !== 2 || parts[0] !== `table:${d.connection_id}`) continue
    const last = parts[1].slice(parts[1].lastIndexOf(".") + 1).toLowerCase()
    if (last === bare) {
      byLast = n
      lastCount++
    }
  }
  return lastCount === 1 ? byLast : undefined
}

/** What a column change says, in a sentence. */
export function columnChangeText(c: LineageColumnChange): string {
  if (!c.columns_known) return "Its SQL changed; its columns were not read for both versions."
  const parts: string[] = []
  if (c.added.length) parts.push(`added ${c.added.join(", ")}`)
  if (c.dropped.length) parts.push(`dropped ${c.dropped.join(", ")}`)
  if (!parts.length) return "Its SQL changed; its columns stayed the same."
  const text = parts.join("; ")
  return text.charAt(0).toUpperCase() + text.slice(1) + "."
}
