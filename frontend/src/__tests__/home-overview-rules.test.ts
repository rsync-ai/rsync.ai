import { describe, expect, it } from "vitest"

import type { ModelFreshnessBreach } from "@/components/explorer/liveState"
import type { ScheduledQuery } from "@/components/explorer/scheduledModel"
import type { Workflow } from "@/lib/api/workflows"
import {
  attentionItems,
  dailyTotals,
  pipelineTotal,
  summarizePipelines,
  summarizeSchedules,
  summarizeWorkflows,
} from "@/lib/home/overview"

// Home's counting rules. A count Home could not read must stay undefined, never
// 0: "we don't know" and "there are none" are different answers (read-stat.ts).

function schedule(over: Partial<ScheduledQuery>): ScheduledQuery {
  return {
    schedule_id: "s",
    saved_query_id: "q",
    name: "Query",
    connection_id: "c",
    schedule_type: "cron",
    schedule_spec: {} as ScheduledQuery["schedule_spec"],
    status: "active",
    materialization: "none",
    created_by: "u",
    created_at: "2026-10-02T00:00:00Z",
    updated_at: "2026-10-02T00:00:00Z",
    ...over,
  }
}

function breach(over: Partial<ModelFreshnessBreach>): ModelFreshnessBreach {
  return {
    breach_id: "b",
    saved_query_id: "q",
    name: "Query",
    target_table: "t",
    deadline_seconds: 3600,
    cause: "late",
    reference_at: "2026-10-02T00:00:00Z",
    never_succeeded: false,
    stale_seconds: 60,
    ...over,
  } as ModelFreshnessBreach
}

function workflow(over: Partial<Workflow>): Workflow {
  return {
    id: "w",
    name: "Workflow",
    status: "active",
    definition: {} as Workflow["definition"],
    trigger_description: "",
    condition_description: "",
    branch_descriptions: [],
    connection: null,
    channels: [],
    proposed_by: "assistant",
    created_at: "2026-10-02T00:00:00Z",
    updated_at: "2026-10-02T00:00:00Z",
    url: "",
    ...over,
  }
}

describe("pipelineTotal", () => {
  it("is unknown when the read failed", () => {
    expect(pipelineTotal(undefined)).toBeUndefined()
  })

  it("prefers the server's total, which honours the status filter, over the page length", () => {
    expect(pipelineTotal({ total: 42, pipelines: [{ id: "a" }] })).toBe(42)
    expect(pipelineTotal({ total: 0, pipelines: [] })).toBe(0)
  })

  it("falls back to the page length, and to unknown when the body has neither", () => {
    expect(pipelineTotal({ pipelines: [{ id: "a" }, { id: "b" }] })).toBe(2)
    expect(pipelineTotal({})).toBeUndefined()
  })

  it("keeps each of the three reads separate", () => {
    expect(summarizePipelines({ total: 5 }, undefined, { total: 0 })).toEqual({
      total: 5,
      failed: undefined,
      waiting: 0,
    })
  })
})

describe("summarizeSchedules", () => {
  const schedules = [
    schedule({ saved_query_id: "a", status: "active" }),
    schedule({ saved_query_id: "b", status: "active", auto_paused_at: "2026-10-01T00:00:00Z" }),
    schedule({ saved_query_id: "c", status: "paused", auto_paused_reason: "source gone" }),
    schedule({ saved_query_id: "d", status: "paused", last_run_status: "failed" }),
  ]

  it("does not count a schedule the system stopped as active", () => {
    expect(summarizeSchedules(schedules, [])).toEqual({
      total: 4,
      active: 1,
      failed: 1,
      autoPaused: 2,
      late: 0,
    })
  })

  it("counts a late query once however many breaches it has", () => {
    const late = [breach({ saved_query_id: "a" }), breach({ saved_query_id: "a" }), breach({ saved_query_id: "d" })]
    expect(summarizeSchedules(schedules, late).late).toBe(2)
  })

  it("leaves late unknown when the freshness read failed", () => {
    expect(summarizeSchedules(schedules, null).late).toBeUndefined()
  })
})

describe("summarizeWorkflows", () => {
  it("counts on, paused, drafts and last-run-failed independently", () => {
    expect(
      summarizeWorkflows([
        workflow({ id: "1", status: "active" }),
        workflow({ id: "2", status: "paused" }),
        workflow({ id: "3", status: "active", last_run: { status: "failed" } as Workflow["last_run"] }),
        workflow({ id: "4", status: "draft" }),
      ]),
    ).toEqual({ total: 4, on: 2, paused: 1, drafts: 1, failed: 1 })
  })

  // A draft was in the total but in none of the facts, so "4 · 2 on · 1 paused"
  // did not add up and the card read as wrong.
  it("accounts for every workflow in the total: on + paused + drafts", () => {
    const s = summarizeWorkflows([
      workflow({ id: "1", status: "draft" }),
      workflow({ id: "2", status: "draft" }),
      workflow({ id: "3", status: "active" }),
    ])
    expect(s.on + s.paused + s.drafts).toBe(s.total)
  })
})

describe("attentionItems", () => {
  it("lists the most urgent first, one line per thing, first reason wins", () => {
    const items = attentionItems({
      failedPipelines: [{ id: "p1", name: "Orders" }],
      // p1 is also waiting: it keeps "Failed", the reason found first.
      waitingPipelines: [{ id: "p1", name: "Orders" }, { id: "p2" }],
      schedules: [
        schedule({ saved_query_id: "q1", name: "Revenue", auto_paused_reason: "source gone", last_run_status: "failed" }),
        schedule({ saved_query_id: "q2", name: "Churn", auto_paused_at: "2026-10-01T00:00:00Z" }),
        schedule({ saved_query_id: "q3", name: "Signups", last_run_status: "failed" }),
      ],
      breaches: [
        breach({ saved_query_id: "q3", name: "Signups", target_table: "signups_daily" }),
        breach({ saved_query_id: "q4", name: "Refunds", target_table: "" }),
      ],
      workflows: [
        workflow({ id: "w1", name: "Weekly report", status: "paused", paused_reason: "channel removed" }),
        // Paused by a person, so no reason: nothing for Home to flag.
        workflow({ id: "w2", name: "Daily digest", status: "paused" }),
        workflow({ id: "w3", name: "Alerts", last_run: { status: "failed" } as Workflow["last_run"] }),
      ],
    })

    expect(items.map((i) => [i.kind, i.id, i.name, i.reason, i.href])).toEqual([
      ["pipeline", "p1", "Orders", "Failed", "/pipelines/p1"],
      ["pipeline", "p2", "Unnamed pipeline", "Waiting for you", "/pipelines/p2"],
      ["scheduled_query", "q1", "Revenue", "Stopped: source gone", "/models/q1"],
      ["scheduled_query", "q2", "Churn", "Stopped by the system", "/models/q2"],
      ["scheduled_query", "q3", "Signups", "Last run failed", "/models/q3"],
      ["scheduled_query", "q4", "Refunds", "Overdue", "/models/q4"],
      ["workflow", "w1", "Weekly report", "Paused: channel removed", "/workflows/w1"],
      ["workflow", "w3", "Alerts", "Last run failed", "/workflows/w3"],
    ])
    // The pill says the reason in a word or two; the rest rides beside it.
    expect(items.map((i) => [i.status, i.detail, i.tone])).toEqual([
      ["Failed", undefined, "fail"],
      ["Waiting for you", undefined, "wait"],
      ["Stopped", "source gone", "fail"],
      ["Stopped", "by the system", "fail"],
      ["Last run failed", undefined, "fail"],
      ["Overdue", undefined, "wait"],
      ["Paused", "channel removed", "wait"],
      ["Last run failed", undefined, "fail"],
    ])
  })

  it("keys by kind, so a pipeline and a workflow that share an id both show", () => {
    const items = attentionItems({
      failedPipelines: [{ id: "same", name: "P" }],
      workflows: [workflow({ id: "same", name: "W", last_run: { status: "failed" } as Workflow["last_run"] })],
    })
    expect(items.map((i) => i.kind)).toEqual(["pipeline", "workflow"])
  })

  it("encodes ids into the link", () => {
    const [item] = attentionItems({ failedPipelines: [{ id: "a/b", name: "Odd" }] })
    expect(item.href).toBe("/pipelines/a%2Fb")
  })

  it("adds nothing for a list that could not be read", () => {
    expect(attentionItems({ breaches: null })).toEqual([])
  })
})

describe("dailyTotals", () => {
  const days = ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"]
  it("adds every outcome up per day, a missing day as 0, oldest first", () => {
    expect(
      dailyTotals(days, [
        { day: "2026-10-02", count: 3 },
        { day: "2026-10-02", count: 2 },
        { day: "2026-10-04", count: 1 },
      ]),
    ).toEqual([0, 5, 0, 1])
  })

  it("keeps only the last days of a longer window", () => {
    expect(dailyTotals(days, [{ day: "2026-10-01", count: 9 }, { day: "2026-10-04", count: 1 }], 2)).toEqual([0, 1])
  })
})
