import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { ReactFlowProvider } from "@xyflow/react"

import { workflowChangeIds } from "@/components/assistant/AssistantThread"
import { CanvasZoomControls, workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowChangeReview } from "@/components/workflows/WorkflowChangeReview"
import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"
import { runBranchLabel } from "@/components/workflows/WorkflowRuns"
import { SlackText, WorkflowTestResult } from "@/components/workflows/WorkflowTestResult"
import { describeBranch, describeBranches, describeCondition } from "@/components/workflows/useWorkflowEdit"
import { NEW_CONDITION_BODY } from "@/lib/api/workflows"
import { featureFlagsManager } from "@/config/features"
import type { AssistantAnswer } from "@/lib/api/assistant"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowChange, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { branchesDefinition, caseChannel, fixtureBranches, type FixtureBranch } from "./workflowFixtures"

// A workflow with several conditions (the API's "branches"): one query, then
// conditions and an optional "otherwise", the first that is true sending its
// own message to its own channels. The server owns every rule (Normalize, the
// run, applying a change); what is pinned here is what only the UI can get wrong:
//   - the steps run in condition order, each condition followed by its channels,
//     and a run marks every condition up to the one that sent;
//   - the editor edits a condition's settings but never offers "always" on a
//     condition that is not last, and saves each condition's channels in order;
//   - on a workflow of a query, its conditions and their messages, it adds,
//     moves and removes a condition, its message and channels going with it,
//     never leaving "always" anywhere but last; on any other, it does none of these;
//   - a refusal naming a condition opens that condition's step;
//   - a proposed change is applied with one POST, and only while it is open,
//     by someone who may;
//   - the zoom controls are magnifiers, never a "+" that reads as "add a step".

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))
// xyflow needs a layout engine jsdom lacks; the step panel carries the same steps.
// CanvasZoomControls stays real: it is rendered below inside a ReactFlowProvider.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000e2"
const CHANGE = "ffffffff-0000-4000-8000-0000000000c9"
const LOADED_AT = "2026-10-03T10:00:00.123456Z"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

const BRANCHES: FixtureBranch[] = [
  { condition: { type: "value", column: "failed", op: ">", threshold: 100 }, message: { subject: "Page", body: "page: {{failed}}" } },
  { condition: { type: "value", column: "failed", op: ">", threshold: 10 }, message: { subject: "Warn", body: "warn: {{failed}}" } },
  { condition: { type: "rows_exist", threshold: 0 }, message: { subject: "", body: "rows" } },
  { condition: { type: "always", threshold: 0 }, message: { subject: "", body: "fine" } },
]

const DESCRIPTIONS = [
  "send when failed is above 100",
  "send when failed is above 10",
  "send when the query returns rows",
  "otherwise send",
]

function definition(branches: FixtureBranch[]) {
  return branchesDefinition({
    trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
    query: { connection_id: "c1", sql: "SELECT count(*) AS failed FROM payments" },
    branches,
  })
}

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Failed payments",
    status: "active",
    definition: definition(BRANCHES),
    trigger_description: "every hour on the hour (UTC)",
    condition_description: describeBranches(BRANCHES.map((b) => b.condition)),
    branch_descriptions: DESCRIPTIONS,
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [
      caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 0 }),
      caseChannel({ id: "ch2", name: "Finance list", kind: "email", branch: 0 }),
      caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 1 }),
      caseChannel({ id: "ch2", name: "Finance list", kind: "email", branch: 3 }),
    ],
    proposed_by: "assistant",
    created_at: LOADED_AT,
    updated_at: LOADED_AT,
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function route(opts: { put?: Response; workflow?: Workflow } = {}) {
  let current = opts.workflow ?? workflow()
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "GET") {
      return Promise.resolve(json(200, { workflow: current, can_manage: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "PUT") {
      if (opts.put) return Promise.resolve(opts.put)
      const body = JSON.parse(String(init?.body))
      current = workflow({ definition: body.definition, updated_at: "2026-10-03T11:00:00Z" })
      return Promise.resolve(json(200, { workflow: current, query_checked: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs: [] }))
    if (url === API_ENDPOINTS.DELIVERY_CHANNELS.LIST) {
      return Promise.resolve(
        json(200, {
          channels: [
            { id: "ch1", name: "#payments", kind: "slack", recipient_count: 0, used_by: [], created_at: "", updated_at: "" },
            { id: "ch2", name: "Finance list", kind: "email", recipient_count: 2, used_by: [], created_at: "", updated_at: "" },
          ],
          can_manage: true,
        }),
      )
    }
    if (url === API_ENDPOINTS.CONNECTIONS.LIST) {
      return Promise.resolve(
        json(200, { connections: [{ id: "c1", name: "Warehouse", connector_type: "postgresql", supports_explorer: true }] }),
      )
    }
    if (url.startsWith(API_ENDPOINTS.PIPELINES.LIST)) return Promise.resolve(json(200, { pipelines: [], total: 0 }))
    return Promise.resolve(json(404, { error: "not_found" }))
  })
}

const writes = () => mockFetch.mock.calls.filter(([, i]) => i?.method && i.method !== "GET")
const panel = () => screen.getByRole("complementary", { name: "Step settings" })

async function startEditing() {
  render(<WorkflowEditor workflowId={WF} />)
  fireEvent.click(await screen.findByRole("button", { name: "Edit" }))
  await screen.findByLabelText("Workflow name")
}

beforeEach(() => {
  mockFetch.mockReset()
  featureFlagsManager.updateFlags({ llmConfigured: true })
})
afterEach(() => featureFlagsManager.resetFlags())

describe("workflowSteps", () => {
  it("lays out each condition in order, followed by its channels", () => {
    const steps = workflowSteps(workflow())
    expect(steps.map((s) => [s.id, s.title])).toEqual([
      ["trigger", "Schedule"],
      ["query", "Query"],
      ["branch:0", "If"],
      ["channel:send_1:ch1", "#payments"],
      ["channel:send_1:ch2", "Finance list"],
      ["branch:1", "Else if"],
      ["channel:send_2:ch1", "#payments"],
      ["branch:2", "Else if"],
      ["channel:send_3:none", "No channel"],
      ["branch:3", "Otherwise"],
      ["channel:send_4:ch2", "Finance list"],
    ])
    expect(steps.find((s) => s.id === "branch:3")?.subtitle).toBe("Otherwise send")
  })

  // A condition checked and not true is "untrue", never "ok": a green tick on it read as "condition met".
  it("marks a run that sent on branch 2: the branches above it checked and not true, only its channels sent", () => {
    const run: WorkflowRun = {
      id: "r1",
      trigger: "schedule",
      status: "succeeded",
      condition_met: true,
      branch: 1,
      row_count: 1,
      deliveries: [{ channel_id: "ch1", name: "#payments", kind: "slack", status: "sent" }],
      notes: [],
      started_at: LOADED_AT,
    }
    const states = Object.fromEntries(workflowSteps(workflow({ last_run: run })).map((s) => [s.id, s.state]))
    expect(states).toEqual({
      trigger: "ok",
      query: "ok",
      "branch:0": "untrue",
      "channel:send_1:ch1": "skipped",
      "channel:send_1:ch2": "skipped",
      "branch:1": "ok",
      "channel:send_2:ch1": "ok",
      "branch:2": "skipped",
      "channel:send_3:none": "skipped",
      "branch:3": "skipped",
      "channel:send_4:ch2": "skipped",
    })
  })

  it("marks every condition checked and no channel sent when none was true", () => {
    const run: WorkflowRun = {
      id: "r1",
      trigger: "schedule",
      status: "succeeded",
      condition_met: false,
      row_count: 0,
      deliveries: [],
      notes: [],
      started_at: LOADED_AT,
    }
    const steps = workflowSteps(workflow({ last_run: run }))
    expect(steps.filter((s) => s.kind === "condition").map((s) => s.state)).toEqual(["untrue", "untrue", "untrue", "untrue"])
    expect(steps.filter((s) => s.kind === "channel").every((s) => s.state === "skipped")).toBe(true)
  })
})

describe("describeBranches", () => {
  // The server's own sentences (definition_test.go TestDescribeBranches), so a
  // draft reads in the editor as it will once saved.
  it("says what the server says", () => {
    expect(describeBranches(BRANCHES.map((b) => b.condition))).toBe(
      "send when failed is above 100; else send when failed is above 10; else send when the query returns rows; otherwise send",
    )
    expect(BRANCHES.map((b, i) => describeBranch(b.condition, i, BRANCHES.length))).toEqual(DESCRIPTIONS)
    expect(describeBranches([BRANCHES[3].condition])).toBe("always send")
    expect(describeCondition({ type: "value", threshold: 1.5, op: "<=" })).toBe("send when the first column is at most 1.5")
    expect(describeCondition({ type: "no_rows", threshold: 0 })).toBe("send when the query returns no rows")
  })
})

describe("Editing a workflow with several conditions", () => {
  it("offers 'always' only on the last condition, beside every other test", async () => {
    route()
    await startEditing()
    const sends = () => within(panel()).getByLabelText(/^Sends/) as HTMLSelectElement
    for (const name of ["3. If", "6. Else if", "8. Else if"]) {
      fireEvent.click(screen.getByRole("button", { name }))
      expect(Array.from(sends().options).map((o) => o.value)).toEqual(["rows_exist", "no_rows", "value"])
    }
    fireEvent.click(screen.getByRole("button", { name: "10. Otherwise" }))
    expect(Array.from(sends().options).map((o) => o.value)).toEqual(["always", "rows_exist", "no_rows", "value"])
  })

  it("saves the edited condition in place and each condition's channels in order", async () => {
    route()
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "7. Condition 2 → #payments" }))
    fireEvent.change(within(panel()).getByLabelText(/^Message/), { target: { value: "warn: {{failed}} failed" } })
    fireEvent.click(await within(panel()).findByRole("checkbox", { name: /Finance list/ }))
    fireEvent.click(screen.getByRole("button", { name: "6. Else if" }))
    fireEvent.change(within(panel()).getByLabelText(/^Threshold/), { target: { value: "20" } })

    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByText(/ran the query once to check it; nothing was sent/)

    expect(writes().map(([u, i]) => [u, i.method])).toEqual([[API_ENDPOINTS.WORKFLOWS.GET(WF), "PUT"]])
    const body = JSON.parse(String(writes()[0][1].body))
    expect(fixtureBranches(body.definition)).toEqual([
      BRANCHES[0],
      {
        condition: { type: "value", column: "failed", op: ">", threshold: 20 },
        message: { subject: "Warn", body: "warn: {{failed}} failed" },
      },
      BRANCHES[2],
      BRANCHES[3],
    ])
    // Ids, order and every other step stay as they were: an edit moves no step.
    expect(body.definition.steps.map((s: { id: string }) => s.id)).toEqual(workflow().definition.steps.map((s) => s.id))
    expect(body.node_channel_ids).toEqual({ send_1: ["ch1", "ch2"], send_2: ["ch1", "ch2"], send_3: [], send_4: ["ch2"] })
  })

  it("opens the step of the condition a refusal names", async () => {
    route({ put: json(422, { error: "Condition 2: A value condition needs a threshold.", code: "invalid_threshold", branch: 2 }) })
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect((await screen.findByRole("alert")).textContent).toContain("Condition 2:")
    await waitFor(() => expect(screen.getByRole("button", { name: "6. Else if" })).toHaveAttribute("aria-pressed", "true"))
  })

  it("opens the first channel step of the condition a channel refusal names", async () => {
    route({ put: json(422, { error: "Condition 3: Pick at least one delivery channel.", code: "no_channels", branch: 3 }) })
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "4. Condition 1 → #payments" }))
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByRole("alert")
    await waitFor(() => expect(screen.getByRole("button", { name: "9. Condition 3 → No channel" })).toHaveAttribute("aria-pressed", "true"))
  })

  const saved = async () => {
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByText(/ran the query once to check it; nothing was sent/)
    return JSON.parse(String(writes()[0][1].body))
  }
  const pressed = (name: string) =>
    waitFor(() => expect(screen.getByRole("button", { name })).toHaveAttribute("aria-pressed", "true"))

  it("adds a condition before the otherwise, with its own message and no channel yet", async () => {
    route()
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "6. Else if" }))
    fireEvent.click(within(panel()).getByRole("button", { name: "Add a condition" }))
    await pressed("10. Else if")
    expect(screen.getByRole("button", { name: "11. Condition 4 → No channel" })).toBeTruthy()
    expect(screen.getByRole("button", { name: "12. Otherwise" })).toBeTruthy()

    const body = await saved()
    expect(fixtureBranches(body.definition)).toEqual([
      BRANCHES[0],
      BRANCHES[1],
      BRANCHES[2],
      { condition: { type: "rows_exist", threshold: 0 }, message: { subject: "", body: NEW_CONDITION_BODY } },
      BRANCHES[3],
    ])
    expect(body.node_channel_ids).toEqual({ send_1: ["ch1", "ch2"], send_2: ["ch1"], send_3: [], send_4: ["ch2"], send_5: [] })
  })

  it("moves a condition up with its message and channels, and never below the otherwise", async () => {
    route()
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "8. Else if" }))
    expect(within(panel()).getByRole("button", { name: "Move down" })).toBeDisabled()
    fireEvent.click(screen.getByRole("button", { name: "10. Otherwise" }))
    expect(within(panel()).getByRole("button", { name: "Move up" })).toBeDisabled()
    expect(within(panel()).getByRole("button", { name: "Move down" })).toBeDisabled()

    fireEvent.click(screen.getByRole("button", { name: "6. Else if" }))
    fireEvent.click(within(panel()).getByRole("button", { name: "Move up" }))
    await pressed("3. If")
    expect(screen.getByRole("button", { name: "4. Condition 1 → #payments" })).toBeTruthy()

    const body = await saved()
    expect(fixtureBranches(body.definition)).toEqual([BRANCHES[1], BRANCHES[0], BRANCHES[2], BRANCHES[3]])
    // A channel belongs to its message, not to a place in the order.
    expect(body.node_channel_ids).toEqual({ send_1: ["ch1", "ch2"], send_2: ["ch1"], send_3: [], send_4: ["ch2"] })
  })

  it("removes a condition with its message and channels; the ones after it move up", async () => {
    route()
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "3. If" }))
    fireEvent.click(within(panel()).getByRole("button", { name: "Remove this condition" }))
    await pressed("3. If")
    expect(screen.getByRole("button", { name: "4. Condition 1 → #payments" })).toBeTruthy()
    expect(screen.queryByRole("button", { name: /Finance list/ })?.textContent).toBe("8. Otherwise → Finance list")

    const body = await saved()
    expect(fixtureBranches(body.definition)).toEqual([BRANCHES[1], BRANCHES[2], BRANCHES[3]])
    expect(body.definition.steps.map((s: { id: string }) => s.id)).toEqual(["query", "condition", "send_2", "send_3", "send_4"])
    expect(body.node_channel_ids).toEqual({ send_2: ["ch1"], send_3: [], send_4: ["ch2"] })
  })

  it("adds, moves and removes no condition on a workflow with a pipeline step", async () => {
    const d = definition(BRANCHES)
    const withPipeline: Workflow = workflow({
      definition: {
        ...d,
        steps: [
          { id: "load", type: "run_pipeline", pipeline_id: "pl-1" },
          ...d.steps.map((s) => (s.type === "query" ? { ...s, after: ["load"], rule: "all_success" as const } : s)),
        ],
      },
      step_refs: { load: { id: "pl-1", name: "Orders" } },
    })
    route({ workflow: withPipeline })
    await startEditing()
    expect(screen.getByText(/Only Ask rsync\.ai adds or removes a step or a condition/)).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: /\. If$/ }))
    expect(within(panel()).queryByRole("group", { name: "Conditions" })).toBeNull()
    expect(within(panel()).queryByRole("button", { name: /add (a )?(branch|condition|step|node)/i })).toBeNull()
    expect(within(panel()).queryByRole("button", { name: /remove (a |this )?(branch|condition)/i })).toBeNull()
  })
})

describe("CanvasZoomControls", () => {
  it("zooms with magnifiers and fits, with no '+' that reads as 'add a step'", () => {
    render(
      <ReactFlowProvider>
        <CanvasZoomControls />
      </ReactFlowProvider>,
    )
    expect(screen.getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual([
      "Zoom in",
      "Zoom out",
      "Fit to view",
    ])
    expect(screen.queryByText("+")).toBeNull()
    expect(screen.queryByRole("button", { name: "+" })).toBeNull()
  })
})

describe("WorkflowTestResult with several conditions", () => {
  it("names the first condition that is true and groups each condition's messages", () => {
    render(
      <WorkflowTestResult
        workflow={workflow()}
        test={{
          columns: ["failed"],
          rows: [{ failed: 42 }],
          row_count: 1,
          truncated: false,
          execution_time_ms: 9,
          condition_met: true,
          branch: 1,
          value: 42,
          messages: [
            { channel: "#payments", kind: "slack", body: "page: 42", step: "send_1", branch: 0 },
            { channel: "#payments", kind: "slack", body: "warn: 42", step: "send_2", branch: 1 },
          ],
          notes: [],
        }}
      />,
    )
    expect(screen.getByText(/Condition 2 is the first that is true/)).toBeTruthy()
    expect(within(screen.getByRole("group", { name: "Condition 1" })).getByText("page: 42")).toBeTruthy()
    const second = screen.getByRole("group", { name: "Condition 2" })
    expect(within(second).getByText("warn: 42")).toBeTruthy()
    expect(within(second).getByText(/a run now would send this/)).toBeTruthy()
    expect(within(screen.getByRole("group", { name: "Condition 3" })).getByText("No channel.")).toBeTruthy()
  })
})

// "Condition 4 is the first that is true" named Otherwise as a condition, which it is
// not: it sends because none of the conditions is true.
describe("WorkflowTestResult when only Otherwise sends", () => {
  it("says no condition is true and names Otherwise, not a condition number", () => {
    render(
      <WorkflowTestResult
        workflow={workflow()}
        test={{
          columns: ["failed"],
          rows: [{ failed: 0 }],
          row_count: 1,
          truncated: false,
          execution_time_ms: 9,
          condition_met: true,
          branch: 3,
          value: 0,
          messages: [{ channel: "#payments", kind: "slack", body: "fine", step: "send_4", branch: 3 }],
          notes: [],
        }}
      />,
    )
    const result = screen.getByRole("region", { name: "Test result" })
    expect(result).toHaveTextContent("No condition is true, so Otherwise sends its message: a run now would send it.")
    expect(result).not.toHaveTextContent(/Condition 4/)
    const otherwise = screen.getByRole("group", { name: "Otherwise" })
    expect(within(otherwise).getByText("fine")).toBeTruthy()
    expect(within(otherwise).getByText(/a run now would send this/)).toBeTruthy()
  })

  // It renders under the canvas, out of sight of the Test button, so Test looked as if it did nothing.
  it("scrolls itself into view when a result arrives", () => {
    const scroll = vi.fn()
    Element.prototype.scrollIntoView = scroll
    const test = { columns: [], rows: [], row_count: 0, truncated: false, execution_time_ms: 1, condition_met: false, messages: [], notes: [] }
    const { rerender } = render(<WorkflowTestResult workflow={workflow()} test={test} />)
    expect(scroll).toHaveBeenCalledTimes(1)
    rerender(<WorkflowTestResult workflow={workflow()} test={{ ...test }} />)
    expect(scroll).toHaveBeenCalledTimes(2)
    // N-2: "nearest" left its first sentence under the fixed 64 px header; its top goes
    // to the top of the page, held below the header by scroll-mt-24.
    expect(scroll).toHaveBeenLastCalledWith({ behavior: "smooth", block: "start" })
    expect(screen.getByRole("region", { name: "Test result" })).toHaveClass("scroll-mt-24")
  })

  // N-7: "returned 1 row in 1032 ms".
  it("says how long the query took as every duration reads", () => {
    const test = { columns: [], rows: [], row_count: 1, truncated: false, execution_time_ms: 1032, condition_met: false, messages: [], notes: [] }
    render(<WorkflowTestResult workflow={workflow()} test={test} />)
    expect(screen.getByRole("region", { name: "Test result" })).toHaveTextContent("The query returned 1 row in 1s.")
  })
})

describe("runBranchLabel", () => {
  const run = (o: Partial<WorkflowRun>): WorkflowRun => ({
    id: "r",
    trigger: "schedule",
    status: "succeeded",
    deliveries: [],
    notes: [],
    started_at: LOADED_AT,
    ...o,
  })
  it("names the condition that sent, only when there are several", () => {
    const four = definition(BRANCHES)
    expect(runBranchLabel(run({ condition_met: true, branch: 2 }), four)).toBe("Condition 3")
    // The last, always true, is no condition: it sent because none of them was true.
    expect(runBranchLabel(run({ condition_met: true, branch: 3 }), four)).toBe("Otherwise")
    // A run from before several conditions has none recorded: it sent on the only one.
    expect(runBranchLabel(run({ condition_met: true }), four)).toBe("Condition 1")
    expect(runBranchLabel(run({ condition_met: false }), four)).toBeNull()
    expect(runBranchLabel(run({ condition_met: true, branch: 0 }), definition([BRANCHES[0]]))).toBeNull()
    // A run of an earlier version counts that version's conditions.
    const earlier = definition([BRANCHES[3]])
    expect(runBranchLabel(run({ condition_met: true, definition: earlier }), four)).toBeNull()
  })
})

function change(overrides: Partial<WorkflowChange> = {}): WorkflowChange {
  const current = workflow({ branch_descriptions: [DESCRIPTIONS[0]], definition: definition([BRANCHES[0]]) })
  return {
    id: CHANGE,
    workflow_id: WF,
    workflow_name: "Failed payments",
    summary: "Adds a warning condition and an otherwise.",
    status: "proposed",
    proposed_by: "assistant",
    current: { ...current, channels: [caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 0 })] },
    proposed: workflow(),
    created_at: LOADED_AT,
    expires_at: "2026-10-04T10:00:00Z",
    url: `https://app.example/workflows/${WF}?change=${CHANGE}`,
    ...overrides,
  }
}

function routeChange(c: WorkflowChange, canManage = true, apply?: Response) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOW_CHANGES.GET(CHANGE) && method === "GET") {
      return Promise.resolve(json(200, { change: c, can_manage: canManage }))
    }
    if (url === API_ENDPOINTS.WORKFLOW_CHANGES.APPLY(CHANGE) && method === "POST") {
      return Promise.resolve(apply ?? json(200, { workflow: workflow({ updated_at: "2026-10-03T11:00:00Z" }), query_checked: true }))
    }
    return Promise.resolve(json(404, { error: "not_found" }))
  })
}

describe("WorkflowChangeReview", () => {
  it("shows the conditions now and after, and loading it sends nothing", async () => {
    routeChange(change())
    render(<WorkflowChangeReview changeId={CHANGE} />)
    const now = await screen.findByRole("group", { name: "Now" })
    const after = screen.getByRole("group", { name: "After the change" })
    expect(within(now).getAllByRole("listitem")).toHaveLength(1)
    expect(within(after).getAllByRole("listitem")).toHaveLength(4)
    expect(within(after).getByText("Otherwise")).toBeTruthy()
    expect(within(after).getByText("To: no channel")).toBeTruthy()
    expect(screen.getByText("Waiting for you")).toBeTruthy()
    expect(writes()).toEqual([])
  })

  it("applies with one POST and says nothing was sent", async () => {
    routeChange(change())
    const onApplied = vi.fn()
    render(<WorkflowChangeReview changeId={CHANGE} onApplied={onApplied} />)
    const apply = await screen.findByRole("button", { name: "Apply change" })
    fireEvent.click(apply)
    fireEvent.click(apply)
    expect(await screen.findByText(/ran the query once to check it; nothing was sent/)).toBeTruthy()
    expect(writes().map(([u, i]) => [u, i.method])).toEqual([[API_ENDPOINTS.WORKFLOW_CHANGES.APPLY(CHANGE), "POST"]])
    expect(onApplied).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
    expect(screen.getByText("Applied")).toBeTruthy()
  })

  it.each([
    ["applied", "This change was applied."],
    ["expired", /expired 24 hours after it was proposed/],
    ["stale", /saved after this change was proposed/],
  ] as const)("offers no Apply on a change that is %s", async (status, says) => {
    routeChange(change({ status }))
    render(<WorkflowChangeReview changeId={CHANGE} />)
    expect(await screen.findByText(says)).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it("offers no Apply to a member", async () => {
    routeChange(change(), false)
    render(<WorkflowChangeReview changeId={CHANGE} />)
    expect(await screen.findByText(/Only workspace owners and admins can apply/)).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it("re-reads a change the server says is closed", async () => {
    let c = change()
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === API_ENDPOINTS.WORKFLOW_CHANGES.APPLY(CHANGE) && init?.method === "POST") {
        c = change({ status: "stale" })
        return Promise.resolve(
          json(409, { error: "The workflow was saved since this change was proposed.", code: "workflow_changed" }),
        )
      }
      return Promise.resolve(json(200, { change: c, can_manage: true }))
    })
    render(<WorkflowChangeReview changeId={CHANGE} />)
    fireEvent.click(await screen.findByRole("button", { name: "Apply change" }))
    expect((await screen.findByRole("alert")).textContent).toContain("saved since this change was proposed")
    expect(await screen.findByText("Out of date")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Apply change" })).toBeNull()
  })

  it("says a change it cannot read was not found", async () => {
    mockFetch.mockResolvedValue(json(404, { error: "not_found" }))
    render(<WorkflowChangeReview changeId={CHANGE} />)
    expect(await screen.findByText(/This proposed change was not found/)).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull()
  })
})

describe("workflowChangeIds", () => {
  it("returns each change an answer stored, never a refused call's", () => {
    const answer = {
      looked_at: [
        { tool: "list_workflows", ok: true },
        { tool: "plan_workflow_change", ok: false, resource_id: "refused" },
        { tool: "plan_workflow_change", ok: true, resource_id: CHANGE },
        { tool: "plan_workflow", ok: true, resource_id: WF },
      ],
    } as unknown as AssistantAnswer
    expect(workflowChangeIds(answer)).toEqual([CHANGE])
  })
})

// A Test's Slack preview is Slack's markup: the server escapes & < > and writes
// *bold*, which the preview printed raw as "&amp;" and stray stars.
describe("SlackText", () => {
  it("draws Slack's markup instead of printing it", () => {
    const { container } = render(
      <SlackText
        text={
          "*Fraud Spike Alert*\nCards &amp; wallets: 5 &lt;b&gt;\n*<https://app.example/charts/1|Spend>*\n" +
          "_2 more tile(s): open the dashboard to see them._ fraud_count_7d\n```\nday | n\n```"
        }
      />,
    )
    const text = container.textContent ?? ""
    expect(text).not.toMatch(/&amp;|&lt;|\*/)
    expect(text).toContain("Cards & wallets: 5 <b>")
    expect(container.querySelector("strong")?.textContent).toBe("Fraud Spike Alert")
    const link = container.querySelector("a")
    expect(link?.getAttribute("href")).toBe("https://app.example/charts/1")
    expect(link?.textContent).toBe("Spend")
    expect(container.querySelector("em")?.textContent).toBe("2 more tile(s): open the dashboard to see them.")
    expect(text).toContain("fraud_count_7d")
    expect(container.querySelector("code")?.textContent).toBe("day | n")
  })

  it("never makes a link of an escaped < in a value", () => {
    const { container } = render(<SlackText text={"&lt;https://evil.example|click&gt;"} />)
    expect(container.querySelector("a")).toBeNull()
    expect(container.textContent).toBe("<https://evil.example|click>")
  })
})
