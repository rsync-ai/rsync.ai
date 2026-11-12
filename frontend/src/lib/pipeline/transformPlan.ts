/**
 * Load and save a pipeline's whole transform plan — GET/POST
 * /api/v1/transforms/pipeline/:pipeline_id.
 *
 * Used by the transform editor in a pipeline's Transforms tab
 * (TransformPlanEditor.tsx) and by the transform step of pipeline creation
 * (SuggestionsReviewDialog.tsx). The standalone Transform Builder page it was
 * first written for shipped its "Save Plan" button with no `onClick` at all.
 *
 * SAVE IS A REPLACE, NOT AN APPEND. SavePipelineTransforms
 * (transforms.go:432-462) opens a transaction, runs
 * `DELETE FROM transform_definitions WHERE pipeline_id = $1`, and re-inserts
 * whatever the request carried. So a save from an empty builder DELETES every
 * transform the pipeline had — including the `mask_pii` rows the NL gate
 * materialized, whose absence makes the next run copy those columns to the
 * destination in the clear, and nothing fails to say so.
 *
 * That is why this module leads with `loadPipelineTransformPlan`: the editor
 * has to show what is already there before it can be allowed to overwrite it,
 * and `describeReplace` has to say — in the confirm dialog, in numbers — what
 * the save is about to remove.
 */

import { authFetch } from "@/lib/api/auth-fetch"
import { toEngineTransform, type RuleSides, type TransformOperation } from "@/lib/pipeline/transformOps"
import { transformTypeLabel } from "@/lib/transform-display"
import { withErrorDetails } from "@/lib/utils/error-handling"

export type PlanSide = "producer" | "consumer"

/**
 * A builder row. Structurally the page's TransformRule, minus its operation union.
 *
 * ONE RULE IS TWO STORED ROWS. The batch executor reads only `producer` rows
 * (executor.go applyTransformsToData) and the CDC sink only `consumer` rows
 * (kafka-sink-worker loadConsumerTransformsRaw), and a CDC pipeline's initial load
 * can run on the batch path. A rule saved to one side therefore holds for half the
 * data: the builder used to do exactly that, filing each rule under whichever tab
 * it was added on. `type: "both"` is what every new rule gets; a one-sided `type`
 * survives only for rows an older builder saved that way, so the operator sees it
 * and chooses to widen it rather than having it widened for them.
 */
export type PlanRule = {
  id: string
  order: number
  type: RuleSides
  operation: string
  enabled: boolean
  config: Record<string, unknown>
  description?: string
  /** For a "both" rule loaded from the DB: the consumer row's id (`id` is the producer's). */
  pairId?: string
}

/** The stored sides a rule writes, in the order they are written. */
export function sidesOf(rule: Pick<PlanRule, "type">): PlanSide[] {
  return rule.type === "both" ? ["producer", "consumer"] : [rule.type]
}

/** A row as the API stores it. */
export type TransformDefinition = {
  id: string
  pipeline_id?: string
  transform_type: string
  transform_order: number
  transform_config: Record<string, unknown>
  enabled: boolean
}

export type PipelineTransformsResponse = {
  pipeline_id?: string
  producer_transforms?: TransformDefinition[] | null
  consumer_transforms?: TransformDefinition[] | null
  /**
   * Set only on a MongoDB CDC pipeline: the only rule types its consumer side can
   * run (each change reaches the rules as one packed document), so the only ones
   * to offer. Absent: any type runs.
   */
  consumer_rule_types?: string[] | null
}

export function transformPlanUrl(pipelineId: string): string {
  return `/api/v1/transforms/pipeline/${encodeURIComponent(pipelineId)}`
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/**
 * Deterministic JSON (sorted keys), so two configs that differ only in key order
 * compare equal. The pair-merge below depends on it: the gateway stores
 * transform_config as jsonb, which does not keep the order the client sent.
 */
export function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value) ?? "null"
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`
  const obj = value as Record<string, unknown>
  const keys = Object.keys(obj).sort()
  return `{${keys.map((k) => `${JSON.stringify(k)}:${stableStringify(obj[k])}`).join(",")}}`
}

/**
 * Builder rows → stored rows: one per side the rule runs on, so a "both" rule is a
 * producer row and a consumer row with the same config, written next to each other.
 *
 * `transform_config` carries `operation` alongside the operation's own fields,
 * because that is where every reader looks for it (`operationOf` in
 * PipelineTransformsTab.tsx reads `transform_config.operation`). An id that
 * is not a UUID is dropped rather than sent: the column is `uuid`, so a
 * client-minted non-UUID would fail the INSERT and roll the whole save back,
 * whereas an empty id makes the handler mint one (transforms.go SavePipelineTransforms).
 */
export function toDefinitions(rules: PlanRule[]): TransformDefinition[] {
  const out: TransformDefinition[] = []
  for (const r of rules) {
    const transform_config = {
      operation: r.operation,
      ...(r.description ? { description: r.description } : {}),
      ...(r.config || {}),
    }
    for (const side of sidesOf(r)) {
      // A "both" rule keeps both of its stored ids; a legacy rule widened to
      // "both" has only one, and the handler mints the other.
      const id = r.type === "both" && side === "consumer" ? r.pairId ?? "" : r.id
      out.push({
        id: UUID_RE.test(id) ? id : "",
        transform_type: side,
        // The handler overwrites this with the array index anyway; sending the same
        // number keeps the request readable rather than pretending to disagree.
        transform_order: out.length,
        transform_config: { ...transform_config },
        enabled: r.enabled,
      })
    }
  }
  return out
}

/** A producer row, a consumer row, or both when they are the same rule. */
export type DefinitionPair = {
  producer?: TransformDefinition
  consumer?: TransformDefinition
}

const byOrder = (a: TransformDefinition, b: TransformDefinition) =>
  (a.transform_order ?? 0) - (b.transform_order ?? 0)

/**
 * Stored rows → logical rules. A producer row and a consumer row with the same
 * config and the same enabled flag are one rule; anything left over is a
 * one-sided rule.
 *
 * Every writer stores a rule as such a pair — toDefinitions above, the NL gate
 * (nl_transforms_gate.go insertNLTransformDefinitions), the creation dialog — and
 * a reader that listed the rows as they came showed each rule twice, once per
 * side, with an edit to one card desyncing it from its twin.
 *
 * Matching is one-to-one, in order: two identical producer rows and one consumer
 * row are one pair plus a producer-only rule, never one rule hiding a duplicate.
 * A pair whose halves disagree on `enabled` is NOT merged — one switch for two
 * different states would hide that one path is off.
 */
export function pairDefinitions(resp: PipelineTransformsResponse | null): DefinitionPair[] {
  const keyOf = (t: TransformDefinition) =>
    `${t.enabled ? 1 : 0}|${stableStringify(t.transform_config || {})}`

  const waiting = new Map<string, TransformDefinition[]>()
  for (const c of [...(resp?.consumer_transforms || [])].sort(byOrder)) {
    const k = keyOf(c)
    waiting.set(k, [...(waiting.get(k) || []), c])
  }

  const pairs: DefinitionPair[] = []
  for (const p of [...(resp?.producer_transforms || [])].sort(byOrder)) {
    const consumer = waiting.get(keyOf(p))?.shift()
    pairs.push(consumer ? { producer: p, consumer } : { producer: p })
  }
  for (const rest of waiting.values()) {
    for (const consumer of rest) pairs.push({ consumer })
  }

  const firstOrder = (pair: DefinitionPair) =>
    Math.min(
      pair.producer?.transform_order ?? Infinity,
      pair.consumer?.transform_order ?? Infinity
    )
  // Array.prototype.sort is stable, so equal orders keep producers first.
  return pairs.sort((a, b) => firstOrder(a) - firstOrder(b))
}

/** Stored rows → builder rules, one per logical rule, in stored order. */
export function fromDefinitions(resp: PipelineTransformsResponse | null): PlanRule[] {
  return pairDefinitions(resp).map((pair, i) => {
    const t = (pair.producer ?? pair.consumer) as TransformDefinition
    const cfg = { ...(t.transform_config || {}) }
    const operation = String(cfg.operation || "")
    const description = typeof cfg.description === "string" ? cfg.description : undefined
    delete cfg.operation
    delete cfg.description
    const both = Boolean(pair.producer && pair.consumer)
    return {
      id: t.id,
      order: i,
      type: both ? "both" : pair.producer ? "producer" : "consumer",
      operation,
      enabled: Boolean(t.enabled),
      config: cfg,
      description,
      ...(both ? { pairId: (pair.consumer as TransformDefinition).id } : {}),
    }
  })
}

/**
 * Rows whose whole purpose is keeping values out of the destination.
 * `hash` counts: the Hash Column card is mask_pii with mask_type=hash
 * (transforms/validate.go normalizeType), so dropping one uncounted would
 * under-report exactly the rules describeReplace exists to warn about.
 */
const MASKING_OPS = new Set(["mask", "mask_pii", "hash"])

export function isMaskingOp(operation: unknown): boolean {
  return MASKING_OPS.has(String(operation || "").trim().toLowerCase())
}

export function countMasks(rules: PlanRule[]): number {
  return rules.filter((r) => isMaskingOp(r.operation)).length
}

/**
 * What a save does NOT do, said wherever a plan is saved. A transform runs on rows
 * as they move; nothing re-reads the destination, so rows already there keep the
 * shape they landed in. Schema changes are additive (the sink and the batch writer
 * ADD columns, never drop or rename them), which is why a rename and an exclude
 * leave the old column behind rather than removing it.
 */
export const FORWARD_ONLY_NOTE =
  "Applies to data moved from now on. Rows already in the destination are not rewritten: " +
  "a new mask leaves earlier rows unmasked, a rename writes the new column while earlier " +
  "rows keep the old one, and an excluded column keeps the values already written. " +
  "To apply a change to those rows, reload the pipeline."

/**
 * The sentence the confirm dialog shows. Written in counts, because "this will
 * replace your transforms" is exactly the phrasing people click through: the
 * number of rows about to be deleted is what makes it land.
 */
export function describeReplace(existing: PlanRule[], next: PlanRule[]): string {
  if (existing.length === 0) {
    return `This pipeline has no transforms yet. Saving writes ${next.length} ${
      next.length === 1 ? "rule" : "rules"
    }.`
  }

  const masks = countMasks(existing)
  const keptIds = new Set(next.map((r) => r.id))
  const droppedMasks = existing.filter((r) => isMaskingOp(r.operation) && !keptIds.has(r.id))

  const base =
    `Saving REPLACES every transform on this pipeline: the ${existing.length} existing ` +
    `${existing.length === 1 ? "rule is" : "rules are"} deleted and ${next.length} ` +
    `${next.length === 1 ? "rule is" : "rules are"} written in their place.`

  if (droppedMasks.length === 0) {
    return masks > 0
      ? `${base} All ${masks} masking ${masks === 1 ? "rule is" : "rules are"} still in the editor and will be kept.`
      : base
  }

  const cols = droppedMasks
    .map((r) => String(r.config?.column || r.config?.columns || r.operation))
    .join(", ")
  return (
    `${base} ${droppedMasks.length} masking ${droppedMasks.length === 1 ? "rule" : "rules"} ` +
    `(${cols}) ${droppedMasks.length === 1 ? "is" : "are"} NOT in the editor and will be deleted — ` +
    `the next run copies those columns to the destination UNMASKED.`
  )
}

/**
 * Whether a rule of this engine type can run on the pipeline. `limit` is its
 * consumer_rule_types; null means any type can. A UI offers only what runs: the
 * save refuses the rest, and with them everything else it was sent.
 */
export function ruleTypeRuns(limit: string[] | null, engineType: string): boolean {
  return !limit || limit.includes(engineType.trim().toLowerCase())
}

/** The same for a builder operation, by the engine type it is saved as (mask and hash are mask_pii). */
export function operationRuns(limit: string[] | null, operation: TransformOperation): boolean {
  if (!limit) return true
  const t = toEngineTransform({ id: "", order: 0, type: "both", operation, enabled: true, config: {} })
  return Boolean(t) && ruleTypeRuns(limit, String(t?.type))
}

/** Why only some rule types are offered, and what was left out for it. */
export function describeRuleTypeLimit(
  limit: string[],
  leftOut?: { count: number; noun: string; types: string[] }
): string {
  const names = limit.map(transformTypeLabel)
  const only = names.length > 1 ? `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}` : names[0]
  let out =
    `Each change on this MongoDB CDC pipeline reaches the rules as one document, ` +
    `so only ${only} can run on it.`
  if (leftOut && leftOut.count > 0) {
    out +=
      ` Left out: ${leftOut.count} ${leftOut.noun}${leftOut.count === 1 ? "" : "s"}` +
      ` (${leftOut.types.map(transformTypeLabel).join(", ")}).`
  }
  return out
}

export type PlanResult<T> = { ok: true; data: T } | { ok: false; error: string }

async function readError(res: Response): Promise<string> {
  const data = (await res.json().catch(() => null)) as
    | { error?: string; message?: string; details?: unknown }
    | null
  if (res.status === 404) {
    return data?.message || "Pipeline not found in this workspace."
  }
  // The handler's `error` is a fixed label ("One or more transforms cannot be
  // executed…"); why each rule was refused — a missing server key, an unknown
  // column — is only in `details`, a list or a single string.
  return withErrorDetails(data?.error || data?.message || `HTTP ${res.status}`, data?.details)
}

/** A loaded plan; `consumerRuleTypes` is null unless the pipeline limits the types (consumer_rule_types). */
export type LoadedPlan =
  | { ok: true; data: PlanRule[]; consumerRuleTypes: string[] | null }
  | { ok: false; error: string }

export async function loadPipelineTransformPlan(pipelineId: string): Promise<LoadedPlan> {
  let res: Response
  try {
    res = await authFetch(transformPlanUrl(pipelineId), { cache: "no-store" })
  } catch {
    return { ok: false, error: "The API is unreachable." }
  }
  if (!res.ok) return { ok: false, error: await readError(res) }
  const data = (await res.json().catch(() => null)) as PipelineTransformsResponse | null
  const types = data?.consumer_rule_types
  return {
    ok: true,
    data: fromDefinitions(data),
    consumerRuleTypes: Array.isArray(types) ? types.map((t) => String(t).trim().toLowerCase()) : null,
  }
}

export const SAVE_TIMEOUT_MS = 30000

export async function savePipelineTransformPlan(
  pipelineId: string,
  rules: PlanRule[]
): Promise<PlanResult<{ count: number }>> {
  let res: Response
  try {
    res = await authFetch(transformPlanUrl(pipelineId), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ transforms: toDefinitions(rules) }),
      // Bounded, so a stalled socket cannot pin a Saving… spinner forever.
      timeoutMs: SAVE_TIMEOUT_MS,
    })
  } catch (err) {
    // A timeout is not "nothing was saved": the handler may have committed and
    // only the answer was lost. Saying either would be a guess.
    if (err instanceof DOMException && err.name === "TimeoutError") {
      return {
        ok: false,
        error: "The save timed out, so it may or may not have been stored. Reload to check before saving again.",
      }
    }
    return { ok: false, error: "The API is unreachable — nothing was saved." }
  }
  if (!res.ok) return { ok: false, error: await readError(res) }
  // The handler counts stored rows, which is two per "both" rule. The operator
  // saved rules, so that is the number the toast reports.
  await res.json().catch(() => null)
  return { ok: true, data: { count: rules.length } }
}

/**
 * A stable fingerprint of a plan's *content*, for answering "is there unsaved
 * work here".
 *
 * The builder held a plan entirely in React state with nothing watching it. A
 * reload, a back button, or picking a different pipeline from the selector
 * replaced that state without a word, and everything built since the last save
 * was gone — including a plan the NL generator had just produced, which can
 * take a minute to regenerate and does not come back identical.
 *
 * Content, not identity: row `id` is deliberately excluded, because the server
 * mints ids on save and a re-read would otherwise always look like a change.
 * Order is included by position rather than by the `order` field, since that is
 * what the save actually writes.
 */
export function planFingerprint(rules: PlanRule[]): string {
  // `type` is in, so widening a one-sided rule to both is a change to save;
  // `pairId` is out for the same reason `id` is.
  return JSON.stringify(
    rules.map((r) => [
      r.type,
      r.operation,
      r.enabled,
      r.description ?? "",
      // Keys sorted so a config rebuilt in a different order is not a change.
      Object.keys(r.config ?? {})
        .sort()
        .map((k) => [k, (r.config ?? {})[k]]),
    ])
  )
}

/** True when the editor holds work that the last load/save does not account for. */
export function isPlanDirty(savedFingerprint: string, rules: PlanRule[]): boolean {
  return planFingerprint(rules) !== savedFingerprint
}
