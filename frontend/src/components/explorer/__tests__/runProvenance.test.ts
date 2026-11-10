import { describe, expect, it } from "vitest"
import { activityTriggerLabel } from "@/lib/api/activity"
import {
  describeRunOrigin,
  describeSkipReason,
  describeUpstreamSet,
  runMessage,
} from "@/components/explorer/runProvenance"

describe("describeUpstreamSet", () => {
  const two = [
    { id: "p1", name: "orders_sync" },
    { id: "m1", name: "customer_dim" },
  ]

  it("says any of, or all of, never a bare list", () => {
    expect(describeUpstreamSet(two)).toBe("After any of orders_sync, customer_dim runs")
    expect(describeUpstreamSet(two, "any")).toBe("After any of orders_sync, customer_dim runs")
    expect(describeUpstreamSet(two, "all")).toBe("After all of orders_sync, customer_dim run")
  })

  // With one upstream the two policies are the same trigger, so the sentence is too.
  it("names a single upstream the same way under either policy", () => {
    expect(describeUpstreamSet([two[0]], "all")).toBe("After orders_sync runs")
    expect(describeUpstreamSet([two[0]], "any")).toBe("After orders_sync runs")
  })

  it("falls back to the id for a producer the join found no name for", () => {
    expect(describeUpstreamSet([{ id: "p9", name: " " }])).toBe("After p9 runs")
    expect(describeUpstreamSet(undefined)).toBe("After an upstream runs")
  })
})

describe("describeRunOrigin", () => {
  // The model's Runs table and its run panel showed the stored word ("manual",
  // "scheduled", "triggered"), while the Runs page says the same run in words
  // (inbox 2026-10-04). Bug class: a run's stored trigger word on screen
  // without the one label function, activityTriggerLabel.
  it("says each stored trigger in the Runs page's words, never the stored word", () => {
    for (const word of ["manual", "scheduled", "triggered"]) {
      const said = describeRunOrigin({ trigger_source: word })
      expect(said).toBe(activityTriggerLabel(word))
      expect(said).not.toBe(word)
    }
    expect(describeRunOrigin({ trigger_source: "manual" })).toBe("Manual")
    expect(describeRunOrigin({ trigger_source: "scheduled" })).toBe("Schedule")
  })

  // Rows written before migration 104 carry no provenance, and must not claim any.
  it("names no upstream for a triggered row with no provenance", () => {
    expect(describeRunOrigin({ trigger_source: "triggered" })).toBe("After its upstream")
  })

  it("names the upstream, never the chain depth, and merged triggers in plain words", () => {
    expect(
      describeRunOrigin({
        trigger_source: "triggered",
        upstream_kind: "pipeline",
        upstream_name: "orders_sync",
        trigger_depth: 1,
        coalesced_count: 1,
      })
    ).toBe("After orders_sync")
    expect(
      describeRunOrigin({
        trigger_source: "triggered",
        upstream_kind: "model",
        upstream_name: "orders_daily",
        trigger_depth: 2,
        coalesced_count: 3,
      })
    ).toBe("After orders_daily (one rebuild for 3 upstream runs)")
    expect(
      describeRunOrigin({
        trigger_source: "triggered",
        upstream_kind: "model",
        upstream_name: "orders_daily",
        coalesced_count: 2,
      })
    ).toBe("After orders_daily (one rebuild for 2 upstream runs)")
    // The models list read "after Loan payments (silver) (depth 2)": depth is the scheduler's word.
    expect(
      describeRunOrigin({ trigger_source: "triggered", upstream_kind: "model", upstream_name: "Loan payments (silver)", trigger_depth: 2 })
    ).toBe("After Loan payments (silver)")
  })

  // No name is a deleted producer or someone else's private model: say what kind it was
  // without inventing, or leaking, which one.
  it("says which kind of upstream when there is no name to show", () => {
    expect(describeRunOrigin({ trigger_source: "triggered", upstream_kind: "pipeline" })).toBe(
      "After a pipeline"
    )
    expect(describeRunOrigin({ trigger_source: "triggered", upstream_kind: "model" })).toBe(
      "After a model"
    )
    expect(describeRunOrigin({ trigger_source: "triggered", upstream_kind: "workflow" })).toBe(
      "By a workflow"
    )
  })

  // A workflow's refresh model step asked for the run while the workflow was running, so
  // "after" would say the wrong thing.
  it("says a workflow asked for the run", () => {
    expect(
      describeRunOrigin({ trigger_source: "triggered", upstream_kind: "workflow", upstream_name: "nightly" })
    ).toBe("By the workflow nightly")
    expect(
      describeRunOrigin({
        trigger_source: "triggered",
        upstream_kind: "workflow",
        upstream_name: "nightly",
        coalesced_count: 2,
      })
    ).toBe("By the workflow nightly (one rebuild for 2 requests)")
  })
})

describe("describeSkipReason", () => {
  it("is null for a row that is not a skip", () => {
    expect(describeSkipReason({})).toBeNull()
    expect(describeSkipReason({ skip_reason: "" })).toBeNull()
  })

  it("gives each reason the gateway writes its own sentence", () => {
    const reasons = [
      "upstream_failed",
      "upstream_skipped",
      "chain_depth_exceeded",
      "waiting_on_upstreams",
      "trigger_lost",
      "upstream_check_failed",
    ]
    const sentences = reasons.map((skip_reason) => describeSkipReason({ skip_reason }))
    expect(new Set(sentences).size).toBe(reasons.length)
    for (const s of sentences) {
      expect(s).toMatch(/^not rebuilt/)
      // A known reason is translated, never echoed back as its code.
      expect(reasons.some((r) => s!.includes(r))).toBe(false)
    }
  })

  // data-checks-design.md § 9: the row names the model whose check held its table back.
  it("names the upstream a data check blocked, when the reader may see it", () => {
    expect(describeSkipReason({ skip_reason: "upstream_check_failed", upstream_name: "orders" })).toBe(
      "not rebuilt: orders was blocked by a data check",
    )
    expect(describeSkipReason({ skip_reason: "upstream_check_failed", upstream_name: " " })).toBe(
      "not rebuilt: a data check held its upstream's table back",
    )
  })

  it("shows an unknown reason rather than dropping it", () => {
    expect(describeSkipReason({ skip_reason: "quota_exhausted" })).toBe(
      "not rebuilt: quota_exhausted"
    )
  })
  // N-8 (prod 2026-10-10): a fan-in's skip read "not rebuilt yet: waiting for every
  // upstream" and then its message "waiting on 1 upstream that has not rebuilt since
  // this model's last build": one wait, said twice.
  it("says a fan-in's wait once, with how many upstreams it waits on", () => {
    const r = {
      skip_reason: "waiting_on_upstreams",
      error: "waiting on 1 upstream that has not rebuilt since this model's last build",
    }
    expect(describeSkipReason(r)).toBe(
      "not rebuilt yet: waiting on 1 upstream that has not rebuilt since this model's last build"
    )
    expect(runMessage(r)).toBeNull()
    // A row with no message keeps the general sentence.
    expect(describeSkipReason({ skip_reason: "waiting_on_upstreams" })).toBe("not rebuilt yet: waiting for every upstream")
  })
})

describe("runMessage", () => {
  it("keeps a run's message that its skip line does not say", () => {
    expect(runMessage({})).toBeNull()
    expect(runMessage({ error: " " })).toBeNull()
    expect(runMessage({ error: "relation orders does not exist" })).toBe("relation orders does not exist")
    expect(runMessage({ skip_reason: "upstream_failed", error: "orders_sync failed" })).toBe("orders_sync failed")
  })
})
