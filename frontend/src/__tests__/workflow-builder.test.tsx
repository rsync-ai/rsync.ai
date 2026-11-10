import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { WorkflowBuilder } from "@/components/workflows/WorkflowBuilder"
import { WorkflowEditor, refusalNode } from "@/components/workflows/WorkflowEditor"
import { workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { stepSentence, workflowSummary } from "@/components/workflows/workflowWords"
import { featureFlagsManager } from "@/config/features"
import { authFetch } from "@/lib/api/auth-fetch"
import {
  NEW_CONDITION_BODY,
  WorkflowError,
  addCondition,
  createWorkflow,
  isBuiltShape,
  moveCondition,
  newWorkflowDefinition,
  removeCondition,
  type Workflow,
  type WorkflowDefinition,
} from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { stashWorkflowQuery, takeWorkflowQuery } from "@/lib/explorer/queryTabs"
import { branchesDefinition, fixtureBranches } from "./workflowFixtures"

// A workflow built by hand (/workflows/new), with or without an LLM. The
// server checks every rule (POST /workflows: the shape, the query, the
// channels, the draft cap); what is pinned here is what only the UI can get wrong:
//   - it starts as a query to fill in, a daily schedule and one condition that
//     always sends, and Save posts the name, the definition and each send's
//     channels, then opens the saved draft in the editor, which says so once;
//   - the query's connection is "a connection you pick" until one is, never
//     "deleted", and a workspace with none is told where to add one;
//   - a refusal opens what it is about: the name box, the condition a
//     condition's refusal is about, or the send it names;
//   - it is an owner's or admin's: a member is told so, and nothing is offered;
//   - "ask rsync.ai" is said only where there is an assistant to ask;
//   - adding, moving and removing a condition keep each message on its own
//     condition and "always" last, and refuse anything else;
//   - opened from the Explorer's "Alert on this query", the query step holds
//     its statement and connection, taken once; opened any other way, a query
//     the Explorer left behind is not used.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
const replace = vi.fn()
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace, refresh: vi.fn() }) }))
// xyflow needs a layout engine jsdom lacks; the step panel carries the same steps.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000b1"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function saved(d: WorkflowDefinition): Workflow {
  return {
    id: WF,
    name: "Orders today",
    status: "draft",
    definition: d,
    trigger_description: "every day at 09:00 (UTC)",
    condition_description: "always send",
    branch_descriptions: ["always send"],
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [{ id: "ch1", name: "#orders", kind: "slack", node_id: "send_1" }],
    proposed_by: "none",
    created_at: "2026-10-12T09:00:00Z",
    updated_at: "2026-10-12T09:00:00Z",
    url: `https://app.example/workflows/${WF}`,
  }
}

const WAREHOUSE = { id: "c1", name: "Warehouse", connector_type: "postgresql", supports_explorer: true }

function route(opts: { canManage?: boolean; post?: Response; queryChecked?: boolean; connections?: (typeof WAREHOUSE)[] } = {}) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOWS.LIST && method === "POST") {
      if (opts.post) return Promise.resolve(opts.post)
      const body = JSON.parse(String(init?.body))
      return Promise.resolve(
        json(201, { workflow: saved(body.definition), can_manage: true, query_checked: opts.queryChecked ?? true }),
      )
    }
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF)) {
      return Promise.resolve(json(200, { workflow: saved(newWorkflowDefinition()), can_manage: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs: [] }))
    if (url === API_ENDPOINTS.DELIVERY_CHANNELS.LIST) {
      return Promise.resolve(
        json(200, {
          channels: [
            { id: "ch1", name: "#orders", kind: "slack", recipient_count: 0, used_by: [], created_at: "", updated_at: "" },
          ],
          can_manage: opts.canManage ?? true,
        }),
      )
    }
    if (url === API_ENDPOINTS.CONNECTIONS.LIST) {
      return Promise.resolve(json(200, { connections: opts.connections ?? [WAREHOUSE] }))
    }
    if (url.startsWith(API_ENDPOINTS.PIPELINES.LIST)) return Promise.resolve(json(200, { pipelines: [], total: 0 }))
    return Promise.resolve(json(404, { error: "not_found" }))
  })
}

const panel = () => screen.getByRole("complementary", { name: "Step settings" })
const posts = () => mockFetch.mock.calls.filter(([, i]) => i?.method === "POST")

async function fillIn() {
  render(<WorkflowBuilder />)
  fireEvent.change(await screen.findByLabelText("Workflow name"), { target: { value: "Orders today" } })
  fireEvent.change(await within(panel()).findByLabelText(/^Connection/), { target: { value: "c1" } })
  fireEvent.change(within(panel()).getByLabelText("SQL"), { target: { value: "SELECT count(*) AS orders FROM orders" } })
}

beforeEach(() => {
  mockFetch.mockReset()
  replace.mockReset()
  sessionStorage.clear()
})
afterEach(() => featureFlagsManager.resetFlags())

describe("Building a workflow by hand", () => {
  it("starts at the query to fill in, on a daily schedule with one condition that always sends", async () => {
    route()
    render(<WorkflowBuilder />)
    expect(await screen.findByLabelText("Workflow name")).toHaveValue("")
    expect(await within(panel()).findByLabelText(/^Connection/)).toHaveValue("")
    const nav = within(panel()).getByRole("navigation", { name: "Steps" })
    expect(within(nav).getAllByRole("button").map((b) => b.textContent)).toEqual([
      "1. Schedule",
      "2. Query",
      "3. If",
      "4. If → No channel",
    ])
    expect(within(nav).getByRole("button", { name: "2. Query" })).toHaveAttribute("aria-pressed", "true")
    // One condition: nothing to move or remove, only to add.
    fireEvent.click(within(nav).getByRole("button", { name: "3. If" }))
    const group = within(panel()).getByRole("group", { name: "Conditions" })
    expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual(["Add a condition"])
  })

  it("saves the name, the definition and each send's channels, then opens the draft", async () => {
    route()
    await fillIn()
    fireEvent.click(screen.getByRole("button", { name: "4. If → No channel" }))
    fireEvent.click(await within(panel()).findByRole("checkbox", { name: /#orders/ }))
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`/workflows/${WF}?created=checked`))

    expect(posts().map(([u]) => u)).toEqual([API_ENDPOINTS.WORKFLOWS.LIST])
    const body = JSON.parse(String(posts()[0][1].body))
    expect(Object.keys(body).sort()).toEqual(["definition", "name", "node_channel_ids"])
    expect(body.name).toBe("Orders today")
    expect(body.node_channel_ids).toEqual({ send_1: ["ch1"] })
    expect(body.definition.trigger).toEqual({ type: "schedule", cron: "0 9 * * *", timezone: "UTC" })
    expect(body.definition.steps.find((s: { id: string }) => s.id === "query")).toMatchObject({
      connection_id: "c1",
      sql: "SELECT count(*) AS orders FROM orders",
    })
    expect(fixtureBranches(body.definition)).toEqual([
      { condition: { type: "always", threshold: 0 }, message: { subject: "", body: NEW_CONDITION_BODY } },
    ])
  })

  it("says when the check stopped before the query finished", async () => {
    route({ queryChecked: false })
    await fillIn()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`/workflows/${WF}?created=unchecked`))
  })

  it("puts the cursor in the name box when the name is refused", async () => {
    route({ post: json(422, { error: "A workflow needs a short name, such as Weekly revenue digest.", code: "invalid_name" }) })
    render(<WorkflowBuilder />)
    await within(await screen.findByRole("complementary", { name: "Step settings" })).findByLabelText(/^Connection/)
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect((await screen.findByRole("alert")).textContent).toBe("A workflow needs a short name, such as Weekly revenue digest.")
    expect(document.activeElement).toBe(screen.getByLabelText("Workflow name"))
    expect(replace).not.toHaveBeenCalled()
  })

  it("opens the send a channel refusal names, though it was added in this build", async () => {
    route({
      post: json(422, { error: "Condition 1: Pick at least one delivery channel.", code: "no_channels", step: "send_2", branch: 1 }),
    })
    await fillIn()
    fireEvent.click(screen.getByRole("button", { name: "3. If" }))
    fireEvent.click(within(panel()).getByRole("button", { name: "Add a condition" }))
    fireEvent.click(await screen.findByRole("button", { name: "2. Query" }))
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByRole("alert")
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "4. Condition 1 → No channel" })).toHaveAttribute("aria-pressed", "true"),
    )
  })

  it("opens the condition a column refusal is about, though the refusal names its send", async () => {
    // workflowdef.CheckResult names each condition's send (step) on every refusal of that condition.
    route({
      post: json(422, {
        error: 'Nothing was saved. The query returns no column "late" to compare; its columns are orders.',
        code: "invalid_condition_column",
        step: "send_1",
        branch: 1,
      }),
    })
    await fillIn()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByRole("alert")
    await waitFor(() => expect(screen.getByRole("button", { name: "3. If" })).toHaveAttribute("aria-pressed", "true"))
  })

  it("says the query runs on a connection you pick, never a deleted one, until one is picked", async () => {
    route()
    render(<WorkflowBuilder />)
    await within(panel()).findByRole("option", { name: "Warehouse (postgresql)" })
    expect(panel().textContent).toContain("Runs one read-only query on a connection you pick;")
    expect(document.body.textContent).toContain("rsync.ai runs one read-only query on a connection you pick")
    expect(document.body.textContent).not.toMatch(/deleted/i)
    expect(within(panel()).queryByText(/No SQL connections/)).toBeNull()
    fireEvent.change(within(panel()).getByLabelText(/^Connection/), { target: { value: "c1" } })
    expect(panel().textContent).toContain("Runs one read-only query on Warehouse;")
    expect(document.body.textContent).toContain("rsync.ai runs one read-only query on Warehouse")
  })

  it("says where to add a connection when the workspace has none to query, once the list has loaded", async () => {
    route({ connections: [] })
    render(<WorkflowBuilder />)
    const link = await within(panel()).findByRole("link", { name: "Add one in Connections" })
    expect(link).toHaveAttribute("href", "/connections")
    expect(link.closest("p")?.textContent).toBe("No SQL connections to pick from. Add one in Connections.")
    // The link is beside the picker, not inside its label.
    expect(link.closest("label")).toBeNull()
  })

  it("says nothing about an empty list while the lists are still loading", async () => {
    route()
    const answer = mockFetch.getMockImplementation()!
    mockFetch.mockImplementation((url: string, init?: RequestInit) =>
      url === API_ENDPOINTS.CONNECTIONS.LIST ? new Promise(() => {}) : answer(url, init),
    )
    render(<WorkflowBuilder />)
    expect(await within(panel()).findByLabelText(/^Connection/)).toHaveValue("")
    await new Promise((r) => setTimeout(r, 50))
    expect(panel().textContent).not.toContain("No SQL connections")
  })

  it("says why at the draft cap and stays on the page", async () => {
    route({
      post: json(429, {
        error: "You have 20 workflow drafts. Turn some on or delete them before drafting more.",
        code: "too_many_drafts",
      }),
    })
    await fillIn()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect((await screen.findByRole("alert")).textContent).toContain("You have 20 workflow drafts.")
    expect(screen.getByLabelText("Workflow name")).toHaveValue("Orders today")
    expect(replace).not.toHaveBeenCalled()
  })

  it("is an owner's or admin's: a member is told so and offered nothing", async () => {
    route({ canManage: false })
    render(<WorkflowBuilder />)
    expect(await screen.findByText("Only workspace owners and admins can build a workflow.")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull()
    expect(screen.queryByLabelText("Workflow name")).toBeNull()
  })

  it("says ask rsync.ai only where there is an assistant to ask", async () => {
    route()
    featureFlagsManager.updateFlags({ assistant: true, llmConfigured: false })
    featureFlagsManager.markResolved()
    const { unmount } = render(<WorkflowBuilder />)
    await screen.findByLabelText("Workflow name")
    expect(screen.queryByText(/ask rsync\.ai/i)).toBeNull()
    unmount()
    featureFlagsManager.updateFlags({ assistant: true, llmConfigured: true })
    render(<WorkflowBuilder />)
    expect(await screen.findByText(/To run a pipeline, rebuild a model, wait, or send a chart or dashboard, ask rsync\.ai\./)).toBeTruthy()
  })
})

describe("The editor after a build", () => {
  it("says the draft was saved and checked, once", async () => {
    route()
    render(<WorkflowEditor workflowId={WF} created="checked" />)
    expect(await screen.findByText(/^Saved as a draft\. rsync\.ai ran the query once to check it; nothing was sent\./)).toBeTruthy()
    expect(screen.getByText(/^Built in the editor and off/)).toBeTruthy()
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`/workflows/${WF}`))
  })
})

describe("addCondition, moveCondition, removeCondition", () => {
  const ALWAYS = { type: "always" as const, threshold: 0 }
  const ROWS = { type: "rows_exist" as const, threshold: 0 }
  const NONE = { type: "no_rows" as const, threshold: 0 }
  const d = (conds: (typeof ALWAYS | typeof ROWS | typeof NONE)[]) =>
    branchesDefinition({
      query: { connection_id: "c1", sql: "SELECT 1" },
      branches: conds.map((condition, i) => ({ condition, message: { subject: "", body: `m${i + 1}` } })),
    })
  const cases = (x: WorkflowDefinition) =>
    Object.fromEntries(x.steps.filter((s) => s.type === "send").map((s) => [s.id, s.case]))

  it("adds before the otherwise, or last when there is none, with a send of its own", () => {
    const one = addCondition(d([ROWS, ALWAYS]))
    expect(one.at).toBe(1)
    expect(one.send).toBe("send_3")
    expect(fixtureBranches(one.definition).map((b) => [b.condition.type, b.message.body])).toEqual([
      ["rows_exist", "m1"],
      ["rows_exist", NEW_CONDITION_BODY],
      ["always", "m2"],
    ])
    expect(cases(one.definition)).toEqual({ send_1: 0, send_2: 2, send_3: 1 })
    expect(addCondition(d([ROWS, NONE])).at).toBe(2)
    expect(isBuiltShape(one.definition)).toBe(true)
  })

  it("moves one place at a time and never leaves always anywhere but last", () => {
    const x = d([ROWS, NONE, ALWAYS])
    expect(fixtureBranches(moveCondition(x, 1, 0)!).map((b) => b.message.body)).toEqual(["m2", "m1", "m3"])
    expect(moveCondition(x, 1, 2)).toBeNull()
    expect(moveCondition(x, 2, 1)).toBeNull()
    expect(moveCondition(x, 0, 2)).toBeNull()
    expect(moveCondition(x, 0, -1)).toBeNull()
  })

  it("removes a condition and its send, and keeps the last one", () => {
    const x = removeCondition(d([ROWS, NONE, ALWAYS]), 0)!
    expect(x.send).toBe("send_1")
    expect(fixtureBranches(x.definition).map((b) => b.message.body)).toEqual(["m2", "m3"])
    expect(cases(x.definition)).toEqual({ send_2: 0, send_3: 1 })
    expect(removeCondition(d([ALWAYS]), 0)).toBeNull()
    expect(removeCondition(d([ROWS, ALWAYS]), 2)).toBeNull()
  })

  it("is a built shape only as a query, one condition step and its sends", () => {
    expect(isBuiltShape(newWorkflowDefinition())).toBe(true)
    const x = d([ROWS, ALWAYS])
    expect(isBuiltShape({ ...x, steps: [{ id: "load", type: "run_pipeline", pipeline_id: "p" }, ...x.steps] })).toBe(false)
    expect(isBuiltShape({ ...x, steps: x.steps.filter((s) => s.type !== "condition") })).toBe(false)
  })
})

describe("createWorkflow", () => {
  it("posts the workflow and hands back the refusal's code and step", async () => {
    mockFetch.mockResolvedValueOnce(
      json(422, { error: "Condition 2: Pick at least one delivery channel.", code: "no_channels", step: "send_2", branch: 2 }),
    )
    const err = await createWorkflow({ name: "x", definition: newWorkflowDefinition(), node_channel_ids: {} }).catch((e) => e)
    expect(err).toBeInstanceOf(WorkflowError)
    expect(err).toMatchObject({ status: 422, code: "no_channels", step: "send_2", branch: 2 })
    expect(mockFetch.mock.calls[0][0]).toBe(API_ENDPOINTS.WORKFLOWS.LIST)
    expect(mockFetch.mock.calls[0][1].method).toBe("POST")
  })
})

describe("refusalNode", () => {
  const msg = { subject: "", body: "x" }
  const two = branchesDefinition({
    query: { connection_id: "c1", sql: "SELECT 1 AS n" },
    branches: [
      { condition: { type: "value", column: "n", op: ">", threshold: 5 }, message: msg },
      { condition: { type: "value", column: "late", op: ">", threshold: 0 }, message: msg },
    ],
  })
  const refused = (code: "condition" | "channel", branch?: number) => ({
    message: "refused",
    step: code,
    branch,
    stepId: "send_2",
    stale: false,
  })

  it("opens the condition a condition's refusal is about, not the send it names", () => {
    expect(refusalNode(two, refused("condition", 2), "query")).toBe("branch:1")
    // No branch on the refusal: the send it names says which condition.
    expect(refusalNode(two, refused("condition"), "query")).toBe("branch:1")
  })

  it("still opens the send for a message or channel refusal", () => {
    expect(refusalNode(two, refused("channel", 2), "query")).toBe("channel:send_2:first")
  })
})

describe("Naming the query's connection", () => {
  const query = (w: Workflow) => workflowSteps(w).find((s) => s.kind === "query")!

  it("says a saved workflow's connection was deleted when its id names none", () => {
    const w = { ...saved(newWorkflowDefinition()), connection: null }
    w.definition = { ...w.definition, steps: w.definition.steps.map((s) => (s.type === "query" ? { ...s, connection_id: "gone" } : s)) }
    expect(workflowSummary(w).lead).toContain("runs one read-only query on a deleted connection")
    expect(stepSentence(w, query(w))).toContain("Runs one read-only query on a connection that was deleted")
  })

  it("says a connection you pick when none is set", () => {
    const w = { ...saved(newWorkflowDefinition()), connection: null }
    expect(workflowSummary(w).lead).toContain("runs one read-only query on a connection you pick")
    expect(stepSentence(w, query(w))).toContain("Runs one read-only query on a connection you pick")
  })
})

describe("From the Explorer's Alert on this query", () => {
  const SQL = "SELECT count(*) AS late FROM orders WHERE shipped_at IS NULL"

  it("fills in the query step with its statement and connection, once, and saves them", async () => {
    route()
    stashWorkflowQuery({ sql: SQL, connectionId: "c1" })
    render(<WorkflowBuilder fromExplorer />)
    await waitFor(() => expect(within(panel()).getByLabelText(/^Connection/)).toHaveValue("c1"))
    expect(within(panel()).getByLabelText("SQL")).toHaveValue(SQL)
    expect(screen.getByRole("button", { name: "2. Query" })).toHaveAttribute("aria-pressed", "true")
    expect(takeWorkflowQuery()).toBeNull()

    // The rest is a new workflow's: a daily schedule and one condition that always sends.
    fireEvent.change(screen.getByLabelText("Workflow name"), { target: { value: "Late orders" } })
    fireEvent.click(screen.getByRole("button", { name: "4. If → No channel" }))
    fireEvent.click(await within(panel()).findByRole("checkbox", { name: /#orders/ }))
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(replace).toHaveBeenCalledWith(`/workflows/${WF}?created=checked`))
    const body = JSON.parse(String(posts()[0][1].body))
    expect(body.definition.steps.find((s: { id: string }) => s.id === "query")).toMatchObject({
      connection_id: "c1",
      sql: SQL,
    })
    expect(body.definition.trigger).toEqual({ type: "schedule", cron: "0 9 * * *", timezone: "UTC" })
  })

  it("leaves a query the Explorer left behind alone when opened another way", async () => {
    route()
    stashWorkflowQuery({ sql: SQL, connectionId: "c1" })
    render(<WorkflowBuilder />)
    expect(await within(await screen.findByRole("complementary", { name: "Step settings" })).findByLabelText(/^Connection/)).toHaveValue("")
    expect(within(panel()).getByLabelText("SQL")).toHaveValue("")
    expect(takeWorkflowQuery()).toEqual({ sql: SQL, connectionId: "c1" })
  })

  it("opens on a query to fill in when the Explorer handed nothing over", async () => {
    route()
    render(<WorkflowBuilder fromExplorer />)
    expect(await within(await screen.findByRole("complementary", { name: "Step settings" })).findByLabelText(/^Connection/)).toHaveValue("")
    expect(within(panel()).getByLabelText("SQL")).toHaveValue("")
  })
})
