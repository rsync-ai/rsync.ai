/**
 * Words for why a model ran, why it did not, and what its trigger waits for.
 *
 * Shared by the Schedules page and the model dialog so the two cannot describe the same
 * trigger differently. Everything here reads fields the gateway joins or stores
 * (migration 104); nothing resolves an id itself.
 */

import { activityTriggerLabel } from "@/lib/api/activity"

export type UpstreamPolicy = "any" | "all"

/** The provenance half of a saved_query_runs row, as GET /explorer/saved/:id/runs returns it. */
export interface RunProvenanceFields {
  trigger_source: string
  upstream_kind?: string
  upstream_id?: string
  upstream_name?: string
  upstream_run_id?: string
  origin_execution_id?: string
  trigger_depth?: number
  coalesced_count?: number
  skip_reason?: string
}

/**
 * "After orders_sync runs", "After any of a, b runs" or "After all of a, b run".
 *
 * The id stands in for a name the join could not find: it is what the trigger still
 * points at when the producer behind it was deleted, or is a model the caller cannot see.
 */
export function describeUpstreamSet(
  upstreams: { id: string; name?: string }[] | undefined,
  policy?: string,
): string {
  const names = (upstreams ?? []).map((u) => u.name?.trim() || u.id).filter(Boolean)
  if (names.length === 0) return "After an upstream runs"
  if (names.length === 1) return `After ${names[0]} runs`
  // "any of" / "all of", never a bare comma list: a plain list reads as whichever of the
  // two the reader already assumed, and they rebuild at very different moments.
  if (policy === "all") return `After all of ${names.join(", ")} run`
  return `After any of ${names.join(", ")} runs`
}

/**
 * The trigger in the Runs page's words (activityTriggerLabel): "Manual", "Schedule",
 * "After orders_daily (one rebuild for 3 upstream runs)", or "By the workflow nightly"
 * for a run a workflow's refresh model step asked for.
 */
export function describeRunOrigin(r: RunProvenanceFields): string {
  if (r.trigger_source !== "triggered" || !r.upstream_kind) return activityTriggerLabel(r.trigger_source)
  // A missing name is a producer deleted since, or a private model of someone else's.
  const name = r.upstream_name?.trim()
  const who = name || (r.upstream_kind === "pipeline" ? "a pipeline" : r.upstream_kind === "workflow" ? "a workflow" : "a model")
  // No "depth 2" or "coalesced": a chain's depth and the merged triggers are the
  // scheduler's words. The upstream it names is the row to walk back to either way.
  // A workflow asked for this run; it did not run after the workflow finished.
  const byWorkflow = r.upstream_kind === "workflow"
  const n = r.coalesced_count ?? 0
  const merged = n > 1 ? ` (one rebuild for ${n} ${byWorkflow ? "requests" : "upstream runs"})` : ""
  const head = byWorkflow ? `By ${name ? `the workflow ${name}` : who}` : `After ${who}`
  return head + merged
}

const SKIP_REASON_TEXT: Record<string, string> = {
  upstream_failed: "not rebuilt: its upstream failed",
  upstream_skipped: "not rebuilt: a model above it did not rebuild",
  chain_depth_exceeded: "not rebuilt: the rebuild chain reached its hop limit",
  waiting_on_upstreams: "not rebuilt yet: waiting for every upstream",
  trigger_lost: "not rebuilt: its trigger could not be delivered",
  upstream_check_failed: "not rebuilt: a data check held its upstream's table back",
}

/**
 * The sentence for a deliberate skip, or null for a row that is not one. An unknown
 * reason from a newer gateway is shown verbatim rather than dropped: a skip with no
 * explanation is exactly what these rows exist to stop.
 */
export function describeSkipReason(r: { skip_reason?: string; upstream_name?: string; error?: string }): string | null {
  if (!r.skip_reason) return null
  // The model a check held back is the one to open; its name is on the row when the
  // reader may see it (a deleted or private upstream has none).
  const upstream = r.upstream_name?.trim()
  if (r.skip_reason === "upstream_check_failed" && upstream) {
    return `not rebuilt: ${upstream} was blocked by a data check`
  }
  // A fan-in's message says how many upstreams it still waits on
  // (saved_query_schedules.go): say the wait once, with the count.
  const message = r.error?.trim()
  if (r.skip_reason === "waiting_on_upstreams" && message?.startsWith("waiting on ")) {
    return `not rebuilt yet: ${message}`
  }
  return SKIP_REASON_TEXT[r.skip_reason] ?? `not rebuilt: ${r.skip_reason}`
}

/** A run's message, or null when it is empty or its skip sentence already says it. */
export function runMessage(r: { skip_reason?: string; upstream_name?: string; error?: string }): string | null {
  const message = r.error?.trim()
  if (!message) return null
  return describeSkipReason(r)?.includes(message) ? null : message
}
