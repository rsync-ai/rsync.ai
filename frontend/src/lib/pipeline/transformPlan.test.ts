import { afterEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"

import { authFetch } from "@/lib/api/auth-fetch"
import {
  countMasks,
  describeReplace,
  FORWARD_ONLY_NOTE,
  fromDefinitions,
  isPlanDirty,
  loadPipelineTransformPlan,
  operationRuns,
  planFingerprint,
  ruleTypeRuns,
  savePipelineTransformPlan,
  toDefinitions,
  type PipelineTransformsResponse,
  type PlanRule,
  type TransformDefinition,
} from "@/lib/pipeline/transformPlan"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as Mock
afterEach(() => vi.clearAllMocks())

const UUID_A = "11111111-1111-4111-8111-111111111111"
const UUID_B = "22222222-2222-4222-8222-222222222222"
const UUID_C = "33333333-3333-4333-8333-333333333333"

const rule = (over: Partial<PlanRule> = {}): PlanRule => ({
  id: UUID_A,
  order: 0,
  type: "both",
  operation: "filter",
  enabled: true,
  config: { column: "amount", operator: ">", value: "100" },
  ...over,
})

describe("toDefinitions", () => {
  it("folds the operation into transform_config, where every reader looks", () => {
    const [d] = toDefinitions([rule()])
    // PipelineTransformsTab's operationOf() reads transform_config.operation;
    // a row saved without it shows up there as a generic "transform".
    expect(d.transform_config).toEqual({ operation: "filter", column: "amount", operator: ">", value: "100" })
    expect(d.transform_type).toBe("producer")
    expect(d.enabled).toBe(true)
  })

  it("writes every rule to both sides, with one config", () => {
    // The batch executor reads only producer rows and the CDC sink only consumer
    // rows. The builder used to write one of the two, so a rule added on the
    // Batch tab never reached a CDC pipeline's changes, and one added on the CDC
    // tab never reached its initial load when that ran as batch.
    const out = toDefinitions([rule({ enabled: false })])
    expect(out.map((d) => d.transform_type)).toEqual(["producer", "consumer"])
    expect(out[1].transform_config).toEqual(out[0].transform_config)
    expect(out.map((d) => d.enabled)).toEqual([false, false])
  })

  it("keeps a legacy one-sided rule one-sided until the operator widens it", () => {
    const out = toDefinitions([rule({ type: "consumer" })])
    expect(out.map((d) => d.transform_type)).toEqual(["consumer"])
  })

  it("keeps both stored ids of a loaded pair, and mints the missing one of a widened rule", () => {
    expect(toDefinitions([rule({ id: UUID_A, pairId: UUID_B })]).map((d) => d.id)).toEqual([UUID_A, UUID_B])
    expect(toDefinitions([rule({ id: UUID_A })]).map((d) => d.id)).toEqual([UUID_A, ""])
  })

  it("drops a client-minted id that is not a UUID, so the server mints one", () => {
    // The column is uuid: a bad id fails the INSERT and rolls the whole
    // transaction back, taking every other row with it.
    expect(toDefinitions([rule({ id: "local-3" })])[0].id).toBe("")
    expect(toDefinitions([rule({ id: UUID_A })])[0].id).toBe(UUID_A)
  })

  it("numbers rows by position, which is what the handler stores anyway", () => {
    const out = toDefinitions([rule({ id: UUID_A }), rule({ id: UUID_B, order: 99 })])
    expect(out.map((d) => d.transform_order)).toEqual([0, 1, 2, 3])
  })
})

describe("fromDefinitions", () => {
  const response = {
    pipeline_id: "p1",
    producer_transforms: [
      { id: UUID_A, transform_type: "producer", transform_order: 0, transform_config: { operation: "filter", column: "amount" }, enabled: true },
    ],
    consumer_transforms: [
      { id: UUID_B, transform_type: "consumer", transform_order: 1, transform_config: { operation: "mask_pii", column: "email" }, enabled: false },
    ],
  }

  it("lifts the operation back out of the config", () => {
    const rules = fromDefinitions(response)
    expect(rules[0].operation).toBe("filter")
    expect(rules[0].config).toEqual({ column: "amount" })
    expect(rules[0].config).not.toHaveProperty("operation")
  })

  it("keeps one-sided rows one-sided, with their stored order and enabled flag", () => {
    const rules = fromDefinitions(response)
    expect(rules.map((r) => r.type)).toEqual(["producer", "consumer"])
    expect(rules[1].enabled).toBe(false)
  })

  const def = (over: Partial<TransformDefinition>): TransformDefinition => ({
    id: UUID_A,
    transform_type: "producer",
    transform_order: 0,
    transform_config: { operation: "mask_pii", column: "email", mask_type: "hash" },
    enabled: true,
    ...over,
  })

  it("shows a producer/consumer pair as ONE rule", () => {
    // The shape the NL gate writes: both rows at the same order. The builder
    // listed rows as they came, so every such rule appeared twice and an edit
    // to one card left its twin running the old config.
    const rules = fromDefinitions({
      producer_transforms: [def({ id: UUID_A })],
      consumer_transforms: [
        // jsonb does not keep key order; the pair must still match.
        def({ id: UUID_B, transform_type: "consumer", transform_config: { mask_type: "hash", column: "email", operation: "mask_pii" } }),
      ],
    })
    expect(rules).toHaveLength(1)
    expect(rules[0]).toMatchObject({ id: UUID_A, pairId: UUID_B, type: "both", operation: "mask_pii" })
  })

  it("does not merge halves that disagree on enabled", () => {
    // One switch for two states would hide that one path is off.
    const rules = fromDefinitions({
      producer_transforms: [def({ id: UUID_A, enabled: true })],
      consumer_transforms: [def({ id: UUID_B, transform_type: "consumer", enabled: false })],
    })
    expect(rules.map((r) => [r.type, r.enabled])).toEqual([["producer", true], ["consumer", false]])
  })

  it("matches rows one-to-one, so a duplicate is not hidden inside a pair", () => {
    const rules = fromDefinitions({
      producer_transforms: [def({ id: UUID_A, transform_order: 0 }), def({ id: UUID_C, transform_order: 2 })],
      consumer_transforms: [def({ id: UUID_B, transform_type: "consumer", transform_order: 1 })],
    })
    expect(rules.map((r) => [r.type, r.id])).toEqual([["both", UUID_A], ["producer", UUID_C]])
  })

  it("keeps the stored order across pairs and one-sided rows", () => {
    const rules = fromDefinitions({
      producer_transforms: [
        def({ id: UUID_A, transform_order: 0, transform_config: { operation: "filter", condition: "a > 1" } }),
      ],
      consumer_transforms: [
        def({ id: UUID_C, transform_type: "consumer", transform_order: 1, transform_config: { operation: "truncate", column: "x" } }),
        def({ id: UUID_B, transform_type: "consumer", transform_order: 2, transform_config: { operation: "filter", condition: "a > 1" } }),
      ],
    })
    expect(rules.map((r) => [r.operation, r.type])).toEqual([["filter", "both"], ["truncate", "consumer"]])
    expect(rules.map((r) => r.order)).toEqual([0, 1])
  })

  it("survives an empty or null response", () => {
    expect(fromDefinitions(null)).toEqual([])
    expect(fromDefinitions({ producer_transforms: null, consumer_transforms: null })).toEqual([])
  })

  // What GET returns after a POST of these rows: split by side, as the handler does.
  const stored = (defs: TransformDefinition[]): PipelineTransformsResponse => ({
    producer_transforms: defs.filter((d) => d.transform_type === "producer"),
    consumer_transforms: defs.filter((d) => d.transform_type === "consumer"),
  })

  it("round-trips one-sided rules without changing them", () => {
    const rules = fromDefinitions(response)
    const back = fromDefinitions(stored(toDefinitions(rules)))
    const shape = (r: PlanRule) => [r.type, r.operation, r.enabled, r.config]
    expect(back.map(shape)).toEqual(rules.map(shape))
  })

  it("round-trips a both-sides rule as one rule, not two", () => {
    const back = fromDefinitions(stored(toDefinitions([rule({ id: UUID_A, pairId: UUID_B })])))
    expect(back).toHaveLength(1)
    expect(back[0]).toMatchObject({ type: "both", id: UUID_A, pairId: UUID_B, operation: "filter" })
  })
})

describe("countMasks", () => {
  // "hash" counts. This assertion used to be 2, excluding it — which was the
  // frontend half of the same defect: the Hash Column card writes operation
  // "hash", it folds to mask_pii server-side, and a de-identifying rule the
  // replace warning does not count is one it silently lets you delete.
  it("counts every masking alias, including the builder's hash card", () => {
    expect(
      countMasks([rule({ operation: "mask" }), rule({ operation: "MASK_PII" }), rule({ operation: "hash" })])
    ).toBe(3)
  })

  it("counts nothing else", () => {
    expect(countMasks([rule({ operation: "filter" }), rule({ operation: "hash_join" })])).toBe(0)
  })
})

describe("describeReplace warns about a dropped hash rule", () => {
  it("names the column and the consequence", () => {
    const existing = [rule({ id: UUID_B, operation: "hash", config: { column: "email" } })]
    const s = describeReplace(existing, [rule({ id: UUID_A })])
    expect(s).toContain("email")
    expect(s).toContain("UNMASKED")
  })
})

describe("FORWARD_ONLY_NOTE", () => {
  it("names what a save leaves behind in the destination", () => {
    // Nothing re-reads rows already moved. An operator who adds a mask and is not
    // told so believes the destination is masked.
    expect(FORWARD_ONLY_NOTE).toMatch(/^Applies to data moved from now on/)
    for (const effect of ["mask", "rename", "excluded"]) expect(FORWARD_ONLY_NOTE).toContain(effect)
  })
})

describe("describeReplace", () => {
  it("says a first save adds rather than replaces", () => {
    expect(describeReplace([], [rule()])).toBe("This pipeline has no transforms yet. Saving writes 1 rule.")
  })

  it("puts the deletion in numbers", () => {
    const s = describeReplace([rule({ id: UUID_A }), rule({ id: UUID_B })], [rule({ id: UUID_A })])
    expect(s).toContain("the 2 existing rules are deleted")
    expect(s).toContain("1 rule is written")
  })

  it("names a masking rule about to disappear, and its consequence", () => {
    const existing = [rule({ id: UUID_B, operation: "mask_pii", config: { column: "email" } })]
    const s = describeReplace(existing, [rule({ id: UUID_A })])
    expect(s).toContain("email")
    expect(s).toContain("UNMASKED")
  })

  it("says a masking rule is safe when it is still in the plan", () => {
    const mask = rule({ id: UUID_B, operation: "mask_pii", config: { column: "email" } })
    const s = describeReplace([mask], [mask])
    expect(s).toContain("will be kept")
    expect(s).not.toContain("UNMASKED")
  })
})

describe("loadPipelineTransformPlan", () => {
  it("reads the pipeline's stored plan", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({
        producer_transforms: [
          { id: UUID_A, transform_type: "producer", transform_order: 0, transform_config: { operation: "filter" }, enabled: true },
        ],
      }),
    })
    const out = await loadPipelineTransformPlan("p1")

    expect(mockFetch.mock.calls[0][0]).toBe("/api/v1/transforms/pipeline/p1")
    expect(out.ok && out.data[0].operation).toBe("filter")
    // No consumer_rule_types: any rule type runs on this pipeline.
    expect(out.ok && out.consumerRuleTypes).toBeNull()
  })

  it("passes on the rule types a MongoDB CDC pipeline can run", async () => {
    mockFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ producer_transforms: [], consumer_transforms: [], consumer_rule_types: ["mask_pii", "type_convert"] }),
    })
    const out = await loadPipelineTransformPlan("p1")
    expect(out.ok && out.consumerRuleTypes).toEqual(["mask_pii", "type_convert"])
  })

  it("explains a 404 as a workspace miss, not as an empty plan", async () => {
    // Returning [] here would render as "this pipeline has no transforms",
    // and the next save would then delete rows the operator never saw.
    mockFetch.mockResolvedValue({ ok: false, status: 404, json: async () => ({}) })
    const out = await loadPipelineTransformPlan("p1")
    expect(out).toEqual({ ok: false, error: "Pipeline not found in this workspace." })
  })
})

describe("savePipelineTransformPlan", () => {
  it("posts the whole plan under `transforms`, two rows per rule", async () => {
    // The handler counts stored rows (4). The toast reports rules (2).
    mockFetch.mockResolvedValue({ ok: true, status: 200, json: async () => ({ message: "Transforms saved", count: 4 }) })
    const out = await savePipelineTransformPlan("p1", [rule({ id: UUID_A }), rule({ id: UUID_B })])

    expect(mockFetch.mock.calls[0][1].method).toBe("POST")
    const body = JSON.parse(mockFetch.mock.calls[0][1].body)
    expect(body.transforms.map((d: TransformDefinition) => d.transform_type)).toEqual([
      "producer", "consumer", "producer", "consumer",
    ])
    expect(out).toEqual({ ok: true, data: { count: 2 } })
  })

  it("reports a refusal instead of claiming a save", async () => {
    mockFetch.mockResolvedValue({ ok: false, status: 403, json: async () => ({ error: "forbidden" }) })
    expect(await savePipelineTransformPlan("p1", [rule()])).toEqual({ ok: false, error: "forbidden" })
  })

  it("shows why a rule was refused, not only that one was", async () => {
    // The handler's `error` is a fixed label; the reason each rule was refused
    // lives in `details`. Live on 2026-10-01 the step showed only the label, so
    // an operator could not tell that the fix was a server key or sha256.
    mockFetch.mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({
        error: "One or more transforms cannot be executed by any engine; nothing was saved.",
        details: [
          'transform[0]: mask_pii hash_function "hmac_sha256" needs the install\'s PII hash key',
          'transform[1]: mask_pii hash_function "hmac_sha256" needs the install\'s PII hash key',
        ],
      }),
    })
    const out = await savePipelineTransformPlan("p1", [rule()])

    expect(out.ok).toBe(false)
    expect(!out.ok && out.error).toMatch(/^One or more transforms cannot be executed by any engine; nothing was saved\./)
    expect(!out.ok && out.error).toContain('transform[0]: mask_pii hash_function "hmac_sha256" needs the install\'s PII hash key')
    expect(!out.ok && out.error).toContain("transform[1]:")
  })

  it("shows a reason sent as a single string too", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 400,
      json: async () => ({ error: "Invalid request", details: "transforms is required" }),
    })
    expect(await savePipelineTransformPlan("p1", [rule()])).toEqual({
      ok: false,
      error: "Invalid request: transforms is required",
    })
  })

  it("does not claim a save when the API is unreachable", async () => {
    mockFetch.mockRejectedValue(new Error("network"))
    const out = await savePipelineTransformPlan("p1", [rule()])
    expect(out).toEqual({ ok: false, error: "The API is unreachable — nothing was saved." })
  })

  it("bounds the save, and does not claim either outcome when it times out", async () => {
    mockFetch.mockRejectedValue(new DOMException("The request timed out. Please try again.", "TimeoutError"))
    const out = await savePipelineTransformPlan("p1", [rule()])

    expect(mockFetch.mock.calls[0][1].timeoutMs).toBe(30000)
    expect(out.ok).toBe(false)
    expect(!out.ok && out.error).toMatch(/may or may not have been stored/)
    expect(!out.ok && out.error).not.toMatch(/nothing was saved/)
  })
})

/**
 * The builder held its plan in React state with nothing watching it, so a
 * reload, a back button, or picking another pipeline discarded everything built
 * since the last save without a word.
 */
describe("planFingerprint / isPlanDirty", () => {
  it("does not call a freshly loaded plan dirty", () => {
    // Non-zero control: a false positive here puts a discard prompt in front of
    // an operator who changed nothing, and they learn to click through it.
    const loaded = [rule({ id: UUID_A }), rule({ id: UUID_B, operation: "select_columns" })]
    expect(isPlanDirty(planFingerprint(loaded), loaded)).toBe(false)
  })

  it("ignores the ids the server mints on save", () => {
    // Re-reading after a save returns the same plan with different row ids. If
    // that read as a change, the page would claim unsaved work forever.
    const before = [rule({ id: UUID_A })]
    const afterSave = [rule({ id: UUID_B })]
    expect(isPlanDirty(planFingerprint(before), afterSave)).toBe(false)
  })

  it("ignores the paired row's id, which the server also mints", () => {
    expect(isPlanDirty(planFingerprint([rule({ pairId: UUID_B })]), [rule({ pairId: UUID_C })])).toBe(false)
  })

  it("sees a one-sided rule widened to both", () => {
    // "Run on both" changes what the next save writes, so it has to be saveable.
    const saved = planFingerprint([rule({ type: "producer" })])
    expect(isPlanDirty(saved, [rule({ type: "both" })])).toBe(true)
  })

  it("ignores the order config keys happen to be written in", () => {
    const a = [rule({ config: { column: "amount", operator: ">", value: "100" } })]
    const b = [rule({ config: { value: "100", operator: ">", column: "amount" } })]
    expect(isPlanDirty(planFingerprint(a), b)).toBe(false)
  })

  it("sees an added rule", () => {
    const saved = planFingerprint([rule()])
    expect(isPlanDirty(saved, [rule(), rule({ id: UUID_B, operation: "mask_pii" })])).toBe(true)
  })

  it("sees a removed rule", () => {
    const saved = planFingerprint([rule(), rule({ id: UUID_B })])
    expect(isPlanDirty(saved, [rule()])).toBe(true)
  })

  it("sees an edited config", () => {
    const saved = planFingerprint([rule()])
    expect(isPlanDirty(saved, [rule({ config: { column: "amount", operator: ">", value: "500" } })])).toBe(true)
  })

  it("sees a toggled rule", () => {
    const saved = planFingerprint([rule({ enabled: true })])
    expect(isPlanDirty(saved, [rule({ enabled: false })])).toBe(true)
  })

  it("sees a reorder", () => {
    // The save writes position, so a drag is a real change even though the set
    // of rules is identical.
    const a = rule({ id: UUID_A, operation: "filter" })
    const b = rule({ id: UUID_B, operation: "select_columns" })
    expect(isPlanDirty(planFingerprint([a, b]), [b, a])).toBe(true)
  })

  it("treats an untouched empty scratchpad as clean, and a built one as dirty", () => {
    expect(isPlanDirty(planFingerprint([]), [])).toBe(false)
    expect(isPlanDirty(planFingerprint([]), [rule()])).toBe(true)
  })
})

describe("ruleTypeRuns / operationRuns", () => {
  const limit = ["mask_pii", "type_convert"]

  it("allows every type when the pipeline names no limit", () => {
    expect(ruleTypeRuns(null, "array_expand")).toBe(true)
    expect(operationRuns(null, "filter")).toBe(true)
  })

  it("allows only the named types, judging a builder operation by the type it saves as", () => {
    expect(ruleTypeRuns(limit, "type_convert")).toBe(true)
    expect(ruleTypeRuns(limit, "array_expand")).toBe(false)
    // mask and hash both save as mask_pii.
    expect(operationRuns(limit, "mask")).toBe(true)
    expect(operationRuns(limit, "hash")).toBe(true)
    expect(operationRuns(limit, "type_convert")).toBe(true)
    expect(operationRuns(limit, "filter")).toBe(false)
    expect(operationRuns(limit, "rename")).toBe(false)
  })
})
