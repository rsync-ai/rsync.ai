import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { WorkflowCanvas, workflowGraph } from "@/components/workflows/WorkflowCanvas"
import { WorkflowChangeReview } from "@/components/workflows/WorkflowChangeReview"
import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"
import { runBranchLabel } from "@/components/workflows/WorkflowRuns"
import { WorkflowTestResult } from "@/components/workflows/WorkflowTestResult"
import { describeBranch, describeBranches, describeCondition } from "@/components/workflows/useWorkflowEdit"
import { featureFlagsManager } from "@/config/features"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowChange, WorkflowCondition, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { branchesDefinition, caseChannel, type FixtureBranch } from "./workflowFixtures"

// A workflow checks any number of conditions in order, and the first that is
// true sends. A person reads it in plain words: "condition" and "node", never
// the API's "branch"; "is true", never "holds"; "is above 5,000", never "> 5000".
// Pinned here, on a workflow of six conditions:
//   - the editor has no limit of its own: every condition offers every test,
//     and the last one also offers "otherwise";
//   - a summary above the canvas says what the whole workflow does, and each
//     node's settings open with one sentence saying what that node does;
//   - the canvas draws all six, one under another, with no "true"/"else" on its lines;
//   - a condition reads as a sentence; its column, test and number are fields in Edit only;
//   - "branch" appears nowhere on screen: the editor, Edit, Test, Runs, Details,
//     a proposed change.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))

// The real canvas, so its nodes are on screen too. React Flow reads its
// renderer's size and watches it; jsdom has neither.
const offsetWidth = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetWidth")
const offsetHeight = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight")
beforeAll(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  )
  Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
    configurable: true,
    get(this: HTMLElement) {
      return this.classList.contains("react-flow__renderer") ? 900 : 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return this.classList.contains("react-flow__renderer") ? 420 : 0
    },
  })
})
afterAll(() => {
  vi.unstubAllGlobals()
  if (offsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth)
  if (offsetHeight) Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeight)
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000e6"
const CHANGE = "ffffffff-0000-4000-8000-0000000000c6"
const LOADED_AT = "2026-10-03T10:00:00.123456Z"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

const SIX: FixtureBranch[] = [
  { condition: { type: "value", column: "failed", op: ">", threshold: 5000 }, message: { subject: "Page", body: "page: {{failed}}" } },
  { condition: { type: "value", column: "failed", op: ">=", threshold: 1000 }, message: { subject: "High", body: "high: {{failed}}" } },
  { condition: { type: "value", column: "failed", op: ">", threshold: 100 }, message: { subject: "Warn", body: "warn: {{failed}}" } },
  { condition: { type: "value", column: "failed", op: ">", threshold: 10 }, message: { subject: "", body: "note: {{failed}}" } },
  { condition: { type: "rows_exist", threshold: 0 }, message: { subject: "", body: "rows" } },
  { condition: { type: "always", threshold: 0 }, message: { subject: "", body: "fine" } },
]

// The server's sentences for SIX (workflow_test.go TestDescribeBranches).
const DESCRIPTIONS = [
  "send when failed is above 5,000",
  "send when failed is at least 1,000",
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
    quiet_minutes: 30,
  })
}

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Failed payments",
    status: "active",
    definition: definition(SIX),
    trigger_description: "every hour on the hour (UTC)",
    condition_description: DESCRIPTIONS.map((d, i) => (i > 0 && i < 5 ? `else ${d}` : d)).join("; "),
    branch_descriptions: DESCRIPTIONS,
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [
      caseChannel({ id: "ops", name: "#ops", kind: "slack", branch: 0 }),
      caseChannel({ id: "fin", name: "Finance list", kind: "email", branch: 1 }),
      caseChannel({ id: "ops", name: "#ops", kind: "slack", branch: 2 }),
      caseChannel({ id: "fin", name: "Finance list", kind: "email", branch: 4 }),
      caseChannel({ id: "ops", name: "#ops", kind: "slack", branch: 5 }),
      caseChannel({ id: "fin", name: "Finance list", kind: "email", branch: 5 }),
    ],
    proposed_by: "assistant",
    created_at: LOADED_AT,
    updated_at: LOADED_AT,
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

const RUNS: WorkflowRun[] = [
  {
    id: "r1",
    trigger: "schedule",
    status: "succeeded",
    condition_met: true,
    branch: 2,
    row_count: 1,
    deliveries: [{ channel_id: "ops", name: "#ops", kind: "slack", status: "sent" }],
    notes: [],
    started_at: LOADED_AT,
    finished_at: LOADED_AT,
  },
]

function route() {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "GET") {
      return Promise.resolve(json(200, { workflow: workflow(), can_manage: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs: RUNS }))
    if (url === API_ENDPOINTS.DELIVERY_CHANNELS.LIST) {
      return Promise.resolve(
        json(200, {
          channels: [
            { id: "ops", name: "#ops", kind: "slack", recipient_count: 0, used_by: [], created_at: "", updated_at: "" },
            { id: "fin", name: "Finance list", kind: "email", recipient_count: 2, used_by: [], created_at: "", updated_at: "" },
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

/** Everything a person can read on screen: the text, and what a screen reader or a tooltip says. */
function visibleWords(root: HTMLElement = document.body): string {
  const attrs = Array.from(root.querySelectorAll("[aria-label],[title],[placeholder]")).flatMap((el) =>
    ["aria-label", "title", "placeholder"].map((a) => el.getAttribute(a) ?? ""),
  )
  return [root.textContent ?? "", ...attrs].join("\n")
}
const API_WORDS = /branch|\bholds?\b/i

const panel = () => screen.getByRole("complementary", { name: "Step settings" })
const tabs = () => within(screen.getByRole("navigation", { name: "Steps" })).getAllByRole("button")

// A condition's node is "If", "Else if" or "Otherwise", as the canvas draws it; a channel
// names its condition as runs, Details and Test do: "Condition 2", not a fourth "Else if".
const TAB_NAMES = [
  "Schedule",
  "Query",
  "If",
  "Condition 1 → #ops",
  "Else if",
  "Condition 2 → Finance list",
  "Else if",
  "Condition 3 → #ops",
  "Else if",
  "Condition 4 → No channel",
  "Else if",
  "Condition 5 → Finance list",
  "Otherwise",
  "Otherwise → #ops",
  "Otherwise → Finance list",
]

// What each node does, in the order of TAB_NAMES.
const SENTENCES = [
  "Starts the workflow every hour on the hour (UTC).",
  "Runs one read-only query on Warehouse; the 6 conditions after it read its result.",
  "Checked first: when failed is above 5,000, it sends its message and the conditions below are not checked.",
  "Posts condition 1's message to #ops when failed is above 5,000.",
  "Checked only when no condition above is true: when failed is at least 1,000, it sends its message and the conditions below are not checked.",
  "Emails condition 2's message to Finance list when failed is at least 1,000 and no condition above is true.",
  "Checked only when no condition above is true: when failed is above 100, it sends its message and the conditions below are not checked.",
  "Posts condition 3's message to #ops when failed is above 100 and no condition above is true.",
  "Checked only when no condition above is true: when failed is above 10, it sends its message and the conditions below are not checked.",
  "Condition 4 has no channel, so it sends nothing.",
  "Checked only when no condition above is true: when the query returns rows, it sends its message and the condition below is not checked.",
  "Emails condition 5's message to Finance list when the query returns rows and no condition above is true.",
  "Sends its message when no condition above is true.",
  "Posts the Otherwise message to #ops when no condition above is true.",
  "Emails the Otherwise message to Finance list when no condition above is true.",
]

async function openEditor() {
  route()
  render(<WorkflowEditor workflowId={WF} />)
  await screen.findByRole("heading", { name: "Failed payments" })
}

beforeEach(() => {
  mockFetch.mockReset()
  featureFlagsManager.updateFlags({ llmConfigured: true })
})
afterEach(() => featureFlagsManager.resetFlags())

describe("describeCondition", () => {
  // The server's own cases (workflow_test.go TestDescribeCondition), so a
  // draft reads in the editor as it will once saved.
  it.each([
    [{ type: "value", column: "count", op: ">", threshold: 5000 }, "send when count is above 5,000"],
    [{ type: "value", column: "count", op: ">=", threshold: 1234567.5 }, "send when count is at least 1,234,567.5"],
    [{ type: "value", column: "count", op: "<", threshold: -12000 }, "send when count is below -12,000"],
    [{ type: "value", column: "count", op: "<=", threshold: 999 }, "send when count is at most 999"],
    [{ type: "value", column: "count", op: "=", threshold: 0 }, "send when count is 0"],
    [{ type: "value", column: "count", op: "!=", threshold: 0.25 }, "send when count is not 0.25"],
    [{ type: "value", op: ">", threshold: 1 }, "send when the first column is above 1"],
    [{ type: "rows_exist", threshold: 0 }, "send when the query returns rows"],
    [{ type: "no_rows", threshold: 0 }, "send when the query returns no rows"],
    [{ type: "always", threshold: 0 }, "always send"],
  ] as [WorkflowCondition, string][])("%o reads %s", (c, says) => {
    expect(describeCondition(c)).toBe(says)
  })

  it("describes six conditions as the server does", () => {
    expect(SIX.map((b, i) => describeBranch(b.condition, i, SIX.length))).toEqual(DESCRIPTIONS)
    expect(describeBranches(SIX.map((b) => b.condition))).toBe(workflow().condition_description)
  })
})

describe("workflowGraph", () => {
  it("draws six conditions one under another, every node joined, no words on the lines", () => {
    const { nodes, edges } = workflowGraph(workflow(), { compact: false, selectedId: null, interactive: true })
    const conditions = nodes.filter((n) => n.data.step.kind === "condition")
    expect(conditions).toHaveLength(6)
    expect(new Set(conditions.map((n) => n.position.x)).size).toBe(1)
    expect(new Set(conditions.map((n) => n.position.y)).size).toBe(6)
    expect(edges.map((e) => e.label)).toEqual(edges.map(() => undefined))
    const targets = new Set(edges.map((e) => e.target))
    expect(nodes.filter((n) => n.id !== "trigger" && !targets.has(n.id))).toEqual([])
  })

  it("joins each condition to the next on their left, out past the labels, not down through them", () => {
    // A node's label is wider than its icon and centred under it: 180px to 80px, or 112px to 48px in the chat card.
    for (const [compact, overhang] of [
      [false, (180 - 80) / 2],
      [true, (112 - 48) / 2],
    ] as const) {
      const { edges } = workflowGraph(workflow(), { compact, selectedId: null, interactive: true })
      const chain = edges.filter((e) => e.sourceHandle === "else")
      expect(chain.map((e) => `${e.source}->${e.target}`)).toEqual([1, 2, 3, 4, 5].map((k) => `branch:${k - 1}->branch:${k}`))
      for (const e of chain) {
        expect(e.targetHandle).toBe("prev")
        expect((e as { pathOptions?: { offset?: number } }).pathOptions?.offset).toBeGreaterThan(overhang)
      }
    }
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    const handles = (id: string) => [...document.querySelectorAll(`.react-flow__handle[data-handleid="${id}"]`)]
    expect(handles("else")).toHaveLength(5)
    expect(handles("prev")).toHaveLength(5)
    for (const h of [...handles("else"), ...handles("prev")]) expect(h.classList).toContain("react-flow__handle-left")
  })

  it("renders all fifteen nodes on the canvas; a condition with no channel sends nothing", () => {
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    const canvas = screen.getByRole("group", { name: "Workflow canvas" })
    const nodes = Array.from(canvas.querySelectorAll(".react-flow__node"))
    expect(nodes).toHaveLength(TAB_NAMES.length)
    expect(nodes.filter((n) => n.textContent?.startsWith("No channel")).map((n) => n.textContent)).toEqual([
      "No channelSends nothing",
    ])
    expect(visibleWords(canvas)).not.toMatch(API_WORDS)
    expect(visibleWords(canvas)).not.toMatch(/\btrue\b|\belse\b/)
  })
})

describe("A workflow of six conditions, in plain words", () => {
  it("says what the whole workflow does above the canvas", async () => {
    await openEditor()
    const summary = screen.getByRole("region", { name: "What this workflow does" })
    expect(within(summary).getByText(/^Every hour on the hour \(UTC\), rsync\.ai runs one read-only query on Warehouse/).textContent).toBe(
      "Every hour on the hour (UTC), rsync.ai runs one read-only query on Warehouse and checks its 6 conditions in order: only the first that is true sends its message, to its own channels.",
    )
    const items = (name: string) =>
      within(within(summary).getByRole("list", { name })).getAllByRole("listitem").map((li) => li.textContent)
    // A condition's sends are alternatives: one stop, each channel kind once.
    expect(items("Steps in order")).toEqual(["Schedule", "Query", "Condition", "Slack message or Email or Send"])
    expect(items("Conditions in order")).toEqual([
      "If failed is above 5,000 → #ops",
      "Else if failed is at least 1,000 → Finance list",
      "Else if failed is above 100 → #ops",
      "Else if failed is above 10 → no channel",
      "Else if the query returns rows → Finance list",
      "Otherwise → #ops and Finance list",
    ])
  })

  it("names each tab after its node and opens each with one sentence saying what it does", async () => {
    await openEditor()
    expect(tabs().map((t) => t.textContent)).toEqual(TAB_NAMES.map((n, i) => `${i + 1}. ${n}`))
    TAB_NAMES.forEach((_, i) => {
      fireEvent.click(tabs()[i])
      expect(within(panel()).getByText(SENTENCES[i])).toBeTruthy()
    })
  })

  // The view showed only the raw cron while Edit said "Every hour on the hour" under it.
  it("says the schedule in the edit form's words, with the cron under it", async () => {
    await openEditor()
    fireEvent.click(tabs()[0])
    const value = (term: string) => within(panel()).getByText(term, { selector: "dt" }).nextElementSibling?.textContent
    expect(value("Runs")).toBe("Every hour on the hour")
    expect(value("Cron")).toBe("0 * * * *")
  })

  it("shows a condition as a sentence; its column, test and number only in Edit", async () => {
    await openEditor()
    fireEvent.click(screen.getByRole("button", { name: "5. Else if" }))
    for (const field of ["Column", "Test", "Threshold"]) expect(within(panel()).queryByText(field)).toBeNull()
    expect(within(panel()).getByText("After it sends, it waits 30 minutes before it sends again.")).toBeTruthy()

    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    await screen.findByLabelText("Workflow name")
    fireEvent.click(screen.getByRole("button", { name: "5. Else if" }))
    for (const field of ["Column", "Test", "Threshold"]) expect(within(panel()).getByText(field)).toBeTruthy()
    expect((within(panel()).getByLabelText(/^Test/) as HTMLSelectElement).selectedOptions[0].textContent).toBe("is at least")
  })

  it("offers every test on every condition, and the otherwise on the last, with no limit of its own", async () => {
    await openEditor()
    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    await screen.findByLabelText("Workflow name")
    const sends = () => within(panel()).getByLabelText(/^Sends/) as HTMLSelectElement
    for (const name of ["3. If", "5. Else if", "7. Else if", "9. Else if", "11. Else if"]) {
      fireEvent.click(screen.getByRole("button", { name }))
      expect(Array.from(sends().options).map((o) => o.value)).toEqual(["rows_exist", "no_rows", "value"])
    }
    fireEvent.click(screen.getByRole("button", { name: "13. Otherwise" }))
    expect(Array.from(sends().options).map((o) => [o.value, o.textContent])).toEqual([
      ["always", "Otherwise (when no condition above is true)"],
      ["rows_exist", "When the query returns rows"],
      ["no_rows", "When the query returns no rows"],
      ["value", "When a value passes a test"],
    ])
  })

  it("never says branch on screen: viewing, editing, Runs and Details", async () => {
    await openEditor()
    const seen: string[] = []
    for (let i = 0; i < TAB_NAMES.length; i++) {
      fireEvent.click(tabs()[i])
      seen.push(visibleWords())
    }
    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    await screen.findByLabelText("Workflow name")
    // Edit keeps the step that was open; start the sweep from the trigger. The
    // edit preview words the cron itself (cronSentence), not as the server does.
    fireEvent.click(tabs()[0])
    await within(panel()).findByText(/^Starts the workflow every hour/)
    for (let i = 0; i < TAB_NAMES.length; i++) {
      fireEvent.click(tabs()[i])
      seen.push(visibleWords())
    }
    expect(screen.getByText(/Add, remove or move a condition in its settings\./)).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))

    await userEvent.setup().click(screen.getByRole("tab", { name: "Runs" }))
    const table = await screen.findByRole("table")
    expect(within(table).getByText("Condition 3")).toBeTruthy()
    const details = screen.getByTestId("workflow-details-card")
    expect(within(details).getByText("Conditions", { selector: "dt" })).toBeTruthy()
    expect(within(details).getByText("Condition 1: send when failed is above 5,000")).toBeTruthy()
    seen.push(visibleWords())

    for (const words of seen) expect(words).not.toMatch(API_WORDS)
  })
})

describe("WorkflowTestResult with six conditions", () => {
  const base = {
    columns: ["failed"],
    rows: [{ failed: 420 }],
    row_count: 1,
    truncated: false,
    execution_time_ms: 9,
    notes: [],
  }

  it("names the first condition that is true and groups each condition's messages", () => {
    const { container } = render(
      <WorkflowTestResult
        workflow={workflow()}
        test={{
          ...base,
          condition_met: true,
          branch: 2,
          value: 420,
          messages: [
            { channel: "#ops", kind: "slack", body: "warn: 420", step: "send_3", branch: 2 },
            { channel: "#ops", kind: "slack", body: "fine", step: "send_6", branch: 5 },
          ],
        }}
      />,
    )
    expect(screen.getByText("Condition 3 is the first that is true: a run now would send its message.")).toBeTruthy()
    expect(within(screen.getByRole("group", { name: "Condition 3" })).getByText("warn: 420")).toBeTruthy()
    expect(within(screen.getByRole("group", { name: "Condition 4" })).getByText("No channel.")).toBeTruthy()
    expect(visibleWords(container)).not.toMatch(API_WORDS)
  })

  it("says when no condition is true", () => {
    const { container } = render(
      <WorkflowTestResult workflow={workflow()} test={{ ...base, condition_met: false, messages: [] }} />,
    )
    expect(screen.getByText("No condition is true: a run now would send nothing.")).toBeTruthy()
    expect(visibleWords(container)).not.toMatch(API_WORDS)
  })

  it("says the one condition is true, or is not", () => {
    const one = workflow({ definition: definition([SIX[3]]) })
    render(<WorkflowTestResult workflow={one} test={{ ...base, condition_met: true, messages: [] }} />)
    expect(screen.getByText("The condition is true: a run now would send this.")).toBeTruthy()
    render(<WorkflowTestResult workflow={one} test={{ ...base, condition_met: false, messages: [] }} />)
    expect(screen.getByText("The condition is not true: a run now would send nothing.")).toBeTruthy()
  })
})

describe("runBranchLabel", () => {
  it("names the condition that sent", () => {
    const run = { ...RUNS[0], branch: 2 }
    expect(runBranchLabel(run, definition(SIX))).toBe("Condition 3")
    expect(runBranchLabel({ ...run, branch: 5 }, definition(SIX))).toBe("Otherwise")
  })
})

describe("A proposed change to six conditions", () => {
  it("lists them now and after, and never says branch", async () => {
    const current = workflow({
      branch_descriptions: [DESCRIPTIONS[0]],
      definition: definition([SIX[0]]),
      channels: [caseChannel({ id: "ops", name: "#ops", kind: "slack", branch: 0 })],
    })
    const change: WorkflowChange = {
      id: CHANGE,
      workflow_id: WF,
      workflow_name: "Failed payments",
      summary: "Adds five conditions.",
      status: "proposed",
      proposed_by: "assistant",
      current,
      proposed: workflow(),
      created_at: LOADED_AT,
      expires_at: "2026-10-04T10:00:00Z",
      url: `https://app.example/workflows/${WF}?change=${CHANGE}`,
    }
    mockFetch.mockResolvedValue(json(200, { change, can_manage: true }))
    const { container } = render(<WorkflowChangeReview changeId={CHANGE} />)
    const after = await screen.findByRole("group", { name: "After the change" })
    expect(within(after).getAllByRole("listitem")).toHaveLength(6)
    expect(screen.getByText("A change only edits conditions. The name, schedule and query stay as they are.")).toBeTruthy()
    expect(visibleWords(container)).not.toMatch(API_WORDS)
  })
})
