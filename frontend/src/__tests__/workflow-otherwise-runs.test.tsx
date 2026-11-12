import { afterEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, render, screen, within } from "@testing-library/react"

import { workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowDetails } from "@/components/workflows/WorkflowDetails"
import { WorkflowRuns, runBranchLabel } from "@/components/workflows/WorkflowRuns"
import { stepName, stepSentence } from "@/components/workflows/workflowWords"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowDefinition, WorkflowRun } from "@/lib/api/workflows"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// The 2026-10-10 re-test of #1505 (N-3, N-4): Test and the editor called the
// last "always" condition of several "Otherwise", but the runs card, its steps
// and Details still called it "Condition 2", and a run with one condition said
// twice that it was not true and called its only message "Condition 1's message".

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000a7"
const at = "2026-10-10T02:00:00Z"
const query = { connection_id: "c1", sql: "SELECT count(*) AS failed FROM loans" }

// If failed > 100 → #risk; Otherwise → #ops.
const SEVERAL = branchesDefinition({
  query,
  branches: [
    { condition: { type: "value", column: "failed", op: ">", threshold: 100 }, message: { subject: "", body: "high" } },
    { condition: { type: "always", threshold: 0 }, message: { subject: "", body: "fine" } },
  ],
})
// One condition: rows exist → #fraud.
const ONE = branchesDefinition({
  query,
  branches: [{ condition: { type: "rows_exist", threshold: 0 }, message: { subject: "", body: "spike" } }],
})

function workflow(definition: WorkflowDefinition, overrides: Partial<Workflow> = {}): Workflow {
  const several = (definition.steps.find((s) => s.type === "condition")?.conditions?.length ?? 0) > 1
  return {
    id: WF,
    name: "Nightly close",
    status: "active",
    definition,
    trigger_description: "every day at 02:00",
    condition_description: several ? "" : "send when the query returns rows",
    branch_descriptions: several ? ["send when failed is above 100", "otherwise send"] : ["send when the query returns rows"],
    connection: { id: "c1", name: "Loans", connector_type: "postgresql" },
    channels: several
      ? [
          caseChannel({ id: "ch1", name: "#risk", kind: "slack", branch: 0 }),
          caseChannel({ id: "ch2", name: "#ops", kind: "slack", branch: 1 }),
        ]
      : [caseChannel({ id: "ch3", name: "#fraud", kind: "slack", branch: 0 })],
    proposed_by: "assistant",
    created_at: at,
    updated_at: at,
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function run(o: Partial<WorkflowRun>): WorkflowRun {
  return { id: "r1", trigger: "schedule", status: "succeeded", deliveries: [], notes: [], started_at: at, finished_at: at, ...o }
}

const lines = (tr: HTMLElement) =>
  within(within(tr).getByRole("list", { name: "Steps" }))
    .getAllByRole("listitem")
    .map((li) => li.textContent)

afterEach(() => {
  cleanup()
  mockFetch.mockReset()
})

describe("runBranchLabel", () => {
  it("names the last always condition of several Otherwise", () => {
    expect(runBranchLabel(run({ condition_met: true, branch: 1 }), SEVERAL)).toBe("Otherwise")
    expect(runBranchLabel(run({ condition_met: true, branch: 0 }), SEVERAL)).toBe("Condition 1")
    expect(runBranchLabel(run({ condition_met: false }), SEVERAL)).toBeNull()
    expect(runBranchLabel(run({ condition_met: true, branch: 0 }), ONE)).toBeNull()
    // A run of an earlier version reads that version: there, condition 2 was a real one.
    const earlier = branchesDefinition({
      query,
      branches: [
        { condition: { type: "value", column: "failed", op: ">", threshold: 100 }, message: { subject: "", body: "a" } },
        { condition: { type: "rows_exist", threshold: 0 }, message: { subject: "", body: "b" } },
      ],
    })
    expect(runBranchLabel(run({ condition_met: true, branch: 1, definition: earlier }), SEVERAL)).toBe("Condition 2")
  })
})

describe("stepName of a condition's message", () => {
  it("says the message for one condition, the Otherwise message for Otherwise", () => {
    const send = (d: WorkflowDefinition, i: number) => d.steps.find((s) => s.type === "send" && s.case === i)!
    expect(stepName(workflow(ONE), send(ONE, 0))).toBe("the message")
    expect(stepName(workflow(SEVERAL), send(SEVERAL, 0))).toBe("condition 1's message")
    expect(stepName(workflow(SEVERAL), send(SEVERAL, 1))).toBe("the Otherwise message")
  })
})

describe("stepSentence of Otherwise's channels", () => {
  it("calls them Otherwise's, not condition 2's", () => {
    const w = workflow(SEVERAL)
    const ops = workflowSteps(w).find((s) => s.id === "channel:send_2:ch2")!
    expect(stepSentence(w, ops)).toBe("Posts the Otherwise message to #ops when no condition above is true.")
    const bare = workflow(SEVERAL, { channels: [caseChannel({ id: "ch1", name: "#risk", kind: "slack", branch: 0 })] })
    const none = workflowSteps(bare).find((s) => s.id === "channel:send_2:none")!
    expect(stepSentence(bare, none)).toBe("Otherwise has no channel, so it sends nothing.")
  })
})

describe("WorkflowDetails", () => {
  it("lists Otherwise by name, with its channels", () => {
    render(<WorkflowDetails workflow={workflow(SEVERAL)} />)
    expect(screen.getByText("Condition 1: send when failed is above 100")).toBeInTheDocument()
    expect(screen.getByText("Otherwise: send when no condition above is true")).toBeInTheDocument()
    expect(screen.getByText("Otherwise: #ops (Slack)")).toBeInTheDocument()
    expect(screen.queryByText(/Condition 2/)).not.toBeInTheDocument()
  })
})

describe("WorkflowRuns: a condition's skipped messages", () => {
  const serve = (runs: WorkflowRun[]) => mockFetch.mockResolvedValue({ ok: true, status: 200, json: () => Promise.resolve({ runs }) } as Response)

  it("says Otherwise sent, and not again under the message it skipped", async () => {
    serve([
      run({
        condition_met: true,
        branch: 1,
        steps: [
          { step_id: "query", type: "query", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "condition", type: "condition", attempt: 1, status: "succeeded", started_at: at },
          // As the adapter wrote it before this fix.
          { step_id: "send_1", type: "send", attempt: 1, status: "skipped", started_at: at, note: "Condition 2 was true, not this one." },
          { step_id: "send_2", type: "send", attempt: 1, status: "succeeded", started_at: at },
        ],
      }),
    ])
    render(<WorkflowRuns workflow={workflow(SEVERAL)} />)
    const row = (await screen.findByText(/^Condition 1's message: skipped/)).closest("tr") as HTMLElement
    expect(within(row).getByText("Otherwise")).toBeInTheDocument()
    expect(lines(row)).toEqual([
      "The query: succeeded",
      "The condition: succeeded",
      "Condition 1's message: skipped",
      "The Otherwise message: succeeded",
    ])
    expect(row).not.toHaveTextContent(/Condition 2/)
  })

  it("says once that the one condition was not true, and calls its message the message", async () => {
    const NOT_TRUE = "The condition was not true, so nothing was sent."
    serve([
      run({
        condition_met: false,
        notes: [NOT_TRUE],
        steps: [
          { step_id: "query", type: "query", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "condition", type: "condition", attempt: 1, status: "succeeded", started_at: at, note: NOT_TRUE },
          { step_id: "send_1", type: "send", attempt: 1, status: "skipped", started_at: at, note: "No condition was true." },
        ],
      }),
    ])
    render(<WorkflowRuns workflow={workflow(ONE)} />)
    const row = (await screen.findByText(/^The message: skipped/)).closest("tr") as HTMLElement
    expect(within(row).getAllByText(/not true/)).toHaveLength(1)
    expect(row).not.toHaveTextContent("No condition was true.")
    expect(row).not.toHaveTextContent(/Condition 1/)
  })

  it("keeps a skipped message's note when its condition did not succeed", async () => {
    const NOTE = "It runs only when every step before it passes, and condition did not."
    serve([
      run({
        status: "failed",
        steps: [
          { step_id: "query", type: "query", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "condition", type: "condition", attempt: 1, status: "failed", started_at: at, error: "The query's result is gone." },
          { step_id: "send_1", type: "send", attempt: 1, status: "skipped", started_at: at, note: NOTE },
        ],
      }),
    ])
    render(<WorkflowRuns workflow={workflow(ONE)} />)
    expect(await screen.findByText(NOTE)).toBeInTheDocument()
  })
})
