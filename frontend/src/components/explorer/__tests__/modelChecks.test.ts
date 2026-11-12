import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { afterEach, describe, expect, it } from "vitest"
import {
  CANNOT_BLOCK_MESSAGE,
  MODEL_CHECK_BLOCKED_ERROR,
  changeDraftType,
  describeCheckSettings,
  describeObserved,
  draftFromCheck,
  draftsFromAssistant,
  emptyDraft,
  isCheckBlocked,
  storeAssistantDraft,
  takeAssistantDraft,
  validateDraft,
  type CheckDraft,
} from "@/components/explorer/modelChecks"

const goChecks = () =>
  readFileSync(resolve(process.cwd(), "../api-gateway/internal/handlers/saved_query_checks.go"), "utf8")

describe("a blocked run", () => {
  // The Runs views tell a blocked run from a broken one only by this sentence. A reworded
  // Go constant would show every blocked run as an ordinary failure.
  it("is the gateway's own sentence", () => {
    const m = goChecks().match(/const modelCheckBlockedError = "([^"]+)"/)
    expect(m?.[1]).toBe(MODEL_CHECK_BLOCKED_ERROR)
  })

  it("is a failed run with exactly that error", () => {
    expect(isCheckBlocked({ status: "failed", error: MODEL_CHECK_BLOCKED_ERROR })).toBe(true)
    expect(isCheckBlocked({ status: "failed", error: "relation does not exist" })).toBe(false)
    expect(isCheckBlocked({ status: "success", error: MODEL_CHECK_BLOCKED_ERROR })).toBe(false)
    expect(isCheckBlocked({ status: "failed" })).toBe(false)
  })

  it("refuses a block with the gateway's sentence", () => {
    expect(goChecks()).toContain(`const cannotBlockMessage = "${CANNOT_BLOCK_MESSAGE}"`)
  })
})

describe("check words", () => {
  it("says what each type counted", () => {
    expect(describeObserved({ type: "row_count", observed: 1 })).toBe("1 row")
    expect(describeObserved({ type: "row_count_change", observed: 1200 })).toBe(`${(1200).toLocaleString()} rows`)
    expect(describeObserved({ type: "not_null", observed: 3 })).toBe("3 NULL rows")
    expect(describeObserved({ type: "unique", observed: 1 })).toBe("1 duplicate row")
    expect(describeObserved({ type: "custom_sql", observed: 0 })).toBe("0 rows returned")
    expect(describeObserved({ type: "not_null" })).toBe("")
  })

  it("says what each check holds", () => {
    expect(describeCheckSettings({ type: "row_count", params: { min: 1 } })).toBe("At least 1 row")
    expect(describeCheckSettings({ type: "row_count", params: { min: 5, max: 10 } })).toBe("At least 5 rows, at most 10")
    expect(describeCheckSettings({ type: "row_count_change", params: { max_drop_pct: 50 } })).toBe("Count falls at most 50%")
    expect(describeCheckSettings({ type: "not_null", columns: ["id"] })).toBe("id has no NULLs")
    expect(describeCheckSettings({ type: "unique", columns: ["a", "b"] })).toBe("a, b unique together")
    expect(describeCheckSettings({ type: "custom_sql" })).toBe("Query returns no rows")
  })
})

describe("the check form", () => {
  const draft = (over: Partial<CheckDraft>): CheckDraft => ({ ...emptyDraft(true), name: "c", ...over })

  it("defaults severity by type, and to warn where the engine cannot block", () => {
    expect(emptyDraft(true, "not_null").severity).toBe("block")
    expect(emptyDraft(true, "row_count_change").severity).toBe("warn")
    expect(emptyDraft(false, "not_null").severity).toBe("warn")
    expect(changeDraftType(draft({ type: "unique", columns: ["a", "b"] }), "not_null", true).columns).toEqual(["a"])
  })

  it("builds the request body the gateway takes", () => {
    expect(validateDraft(draft({ type: "row_count", min: "", max: "" }), true)).toEqual({
      ok: true,
      body: { name: "c", type: "row_count", columns: [], params: { min: 1 }, severity: "block", enabled: true },
    })
    const v = validateDraft(
      draft({ type: "row_count_change", max_drop_pct: "50%", max_growth_pct: "200", severity: "warn" }),
      true,
    )
    expect(v).toMatchObject({ ok: true, body: { params: { max_drop_pct: 50, max_growth_pct: 200 } } })
    const s = validateDraft(draft({ type: "custom_sql", sql_text: " SELECT * FROM {{ this }} WHERE x < 0 " }), true)
    expect(s).toMatchObject({ ok: true, body: { sql_text: "SELECT * FROM {{ this }} WHERE x < 0", columns: [] } })
  })

  // Each refusal mirrors one in normalizeModelCheck.
  it.each<[Partial<CheckDraft>, string]>([
    [{ name: " " }, "needs a name"],
    [{ name: "x".repeat(101) }, "at most 100"],
    [{ name: "a\u0007b" }, "control characters"],
    // A row count error names its field as the form labels it.
    [{ type: "row_count", min: "-1" }, "“At least” cannot be negative"],
    [{ type: "row_count", min: "5", max: "4" }, "“At most” cannot be below “At least”"],
    [{ type: "row_count", min: "1.5" }, "“At least” must be a whole number, such as 1"],
    [{ type: "row_count", min: "1", max: "x" }, "“At most” must be a whole number"],
    [{ severity: "error" as CheckDraft["severity"] }, "“When it fails” must be Block or Warn"],
    [{ type: "row_count_change", max_drop_pct: "" }, "largest drop"],
    [{ type: "row_count_change", max_drop_pct: "101" }, "between 0 and 100"],
    [{ type: "row_count_change", max_drop_pct: "10", max_growth_pct: "-1" }, "cannot be negative"],
    [{ type: "not_null", columns: [] }, "exactly one column"],
    [{ type: "not_null", columns: ["a", "b"] }, "exactly one column"],
    [{ type: "unique", columns: [] }, "1 to 10 columns"],
    [{ type: "unique", columns: Array.from({ length: 11 }, (_, i) => `c${i}`) }, "1 to 10 columns"],
    [{ type: "unique", columns: ["a", "a"] }, "listed twice"],
    [{ type: "unique", columns: [" a"] }, "start or end with a space"],
    [{ type: "custom_sql", sql_text: "SELECT * FROM orders" }, "{{this}}"],
    [{ type: "custom_sql", sql_text: "" }, "needs its query"],
  ])("refuses %j", (over, error) => {
    const v = validateDraft(draft(over), true)
    expect(v.ok).toBe(false)
    expect(v.ok ? "" : v.error).toContain(error)
  })

  it("refuses a block the engine cannot hold back", () => {
    const v = validateDraft(draft({ type: "not_null", columns: ["id"], severity: "block" }), false)
    expect(v).toEqual({ ok: false, error: CANNOT_BLOCK_MESSAGE })
    expect(validateDraft(draft({ type: "not_null", columns: ["id"], severity: "warn" }), false).ok).toBe(true)
  })

  it("round-trips a saved check", () => {
    const d = draftFromCheck({
      name: "drop",
      type: "row_count_change",
      columns: [],
      params: { max_drop_pct: 25 },
      severity: "warn",
      enabled: false,
    })
    expect(validateDraft(d, true)).toEqual({
      ok: true,
      body: {
        name: "drop",
        type: "row_count_change",
        columns: [],
        params: { max_drop_pct: 25 },
        severity: "warn",
        enabled: false,
      },
    })
  })
})

describe("a draft from Ask rsync.ai", () => {
  afterEach(() => sessionStorage.clear())

  // Looked-at arguments are the model's own: string numbers, comma-separated columns and
  // the gateway's synonyms, unmapped.
  it("reads the agent's arguments", () => {
    const got = draftsFromAssistant({
      model_id: "m1",
      checks: [
        { name: "has rows", type: "count", min: "1" },
        { name: "id key", type: "No Duplicates", columns: "id, region", severity: "Blocking" },
        { name: "volume", type: "row_change", max_drop_pct: "40", severity: "warning" },
        { name: "neg", type: "sql", sql: "SELECT * FROM {{this}} WHERE amount < 0" },
        { name: "?", type: "nonsense" },
      ],
    })
    expect(got?.modelId).toBe("m1")
    expect(got?.drafts.map((d) => [d.name, d.type, d.severity])).toEqual([
      ["has rows", "row_count", "block"],
      ["id key", "unique", "block"],
      ["volume", "row_count_change", "warn"],
      ["neg", "custom_sql", "block"],
    ])
    expect(got?.drafts[1].columns).toEqual(["id", "region"])
    expect(got?.drafts[2].max_drop_pct).toBe("40")
    for (const d of got?.drafts ?? []) expect(validateDraft(d, true).ok).toBe(true)
  })

  it("reads checks sent as a JSON string, and nothing from no checks", () => {
    expect(draftsFromAssistant({ model_id: "m1", checks: '[{"name":"n","type":"not_null","columns":"id"}]' })?.drafts).toHaveLength(1)
    expect(draftsFromAssistant({ model_id: "m1", checks: [] })).toBeNull()
    expect(draftsFromAssistant({ checks: [{ name: "n", type: "row_count" }] })).toBeNull()
    expect(draftsFromAssistant(null)).toBeNull()
  })

  it("drafts warnings where the engine cannot block", () => {
    const got = draftsFromAssistant({ model_id: "m1", checks: [{ name: "n", type: "not_null", columns: "id", severity: "block" }] }, false)
    expect(got?.drafts[0].severity).toBe("warn")
  })

  it("hands the draft to that model's page once", () => {
    const got = draftsFromAssistant({ model_id: "m1", checks: [{ name: "n", type: "row_count" }] })!
    storeAssistantDraft(got)
    expect(takeAssistantDraft("m2")).toBeNull()
    expect(takeAssistantDraft("m1")?.[0].name).toBe("n")
    expect(takeAssistantDraft("m1")).toBeNull()
  })
})
