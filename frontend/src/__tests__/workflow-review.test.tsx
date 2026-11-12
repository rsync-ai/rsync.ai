import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"

import { workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"
import { WorkflowReview } from "@/components/workflows/WorkflowReview"
import { authFetch } from "@/lib/api/auth-fetch"
import { workflowIds, type AssistantAnswer } from "@/lib/api/assistant"
import { parseRecipients, type Workflow } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// A workflow Ask rsync.ai drafted (plan_workflow). The server owns every rule:
// who may test or turn one on, the SQL being one read, the run-as user. What
// is pinned here is what only the card can get wrong:
//   - loading the card sends nothing: no test, no activate, no run;
//   - Test shows the result and the message and says nothing was sent;
//   - Turn on is one POST to activate, and the card then shows it on;
//   - a person who cannot manage gets no buttons;
//   - Ask rsync.ai shows a card for each draft an answer stored, never for a
//     refused call.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
const push = vi.fn()
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }))
// The canvas is xyflow, which needs a layout engine jsdom lacks; the card's
// screen-reader step list and the editor's step panel carry the same text.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000f1"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Failed payments",
    status: "draft",
    definition: branchesDefinition({
      trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
      query: { connection_id: "c1", sql: "SELECT count(*) AS failed FROM payments" },
      branches: [
        {
          condition: { type: "value", op: ">", threshold: 50 },
          message: { subject: "Failed payments", body: "{{failed}} failed in the last hour" },
        },
      ],
    }),
    trigger_description: "every hour on the hour (UTC)",
    condition_description: "send when the first column is above 50",
    branch_descriptions: ["send when the first column is above 50"],
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 0 })],
    proposed_by: "assistant",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function route(opts: { get?: Response; canManage?: boolean } = {}) {
  const canManage = opts.canManage ?? true
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "GET") {
      return Promise.resolve(opts.get ?? json(200, { workflow: workflow(), can_manage: canManage }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.TEST(WF) && method === "POST") {
      return Promise.resolve(
        json(200, {
          columns: ["failed"],
          rows: [{ failed: 51 }],
          row_count: 1,
          truncated: false,
          execution_time_ms: 12,
          condition_met: true,
          value: 51,
          messages: [
            { channel: "#payments", kind: "slack", body: "*Failed payments*\n51 failed in the last hour", step: "send_1", branch: 0 },
          ],
          notes: [],
        }),
      )
    }
    if (url === API_ENDPOINTS.WORKFLOWS.ACTIVATE(WF) && method === "POST") {
      return Promise.resolve(json(200, { workflow: workflow({ status: "active", next_run_at: new Date().toISOString() }) }))
    }
    return Promise.resolve(json(404, { error: "not_found" }))
  })
}

const posts = () => mockFetch.mock.calls.filter(([, i]) => i?.method === "POST").map(([u]) => u)

beforeEach(() => mockFetch.mockReset())

describe("WorkflowReview", () => {
  it("shows every step of a draft and sends nothing on load", async () => {
    route()
    render(<WorkflowReview workflowId={WF} />)
    const steps = await screen.findByRole("list", { name: "Steps" })
    expect(steps.textContent).toContain("Schedule: Every hour on the hour (UTC)")
    expect(steps.textContent).toContain("Query: Warehouse")
    expect(steps.textContent).toContain("If: Send when the first column is above 50")
    expect(steps.textContent).toContain("#payments: Slack")
    expect(steps.textContent).not.toContain("`")
    // A draft is off, but Run now on it is real (workflows.go RunWorkflowNow takes any status):
    // the status line must not promise that nothing sends.
    const status = screen.getByText(/won.t run on its own until you turn it on/)
    expect(status.textContent).toContain("Run now sends for real, even on a draft.")
    expect(status.textContent).not.toMatch(/nothing (runs or )?sends/i)
    expect(posts()).toEqual([])
  })

  it("tests without sending, then turns on with one POST", async () => {
    route()
    render(<WorkflowReview workflowId={WF} />)
    fireEvent.click(await screen.findByRole("button", { name: /Test/ }))
    const result = await screen.findByLabelText("Test result")
    expect(result.textContent).toContain("The condition is true")
    expect(result.textContent).toContain("Nothing was sent")
    expect(result.textContent).toContain("51 failed in the last hour")
    expect(posts()).toEqual([API_ENDPOINTS.WORKFLOWS.TEST(WF)])

    fireEvent.click(screen.getByRole("button", { name: /Turn on/ }))
    await screen.findByText(/Turned on/)
    expect(posts()).toEqual([API_ENDPOINTS.WORKFLOWS.TEST(WF), API_ENDPOINTS.WORKFLOWS.ACTIVATE(WF)])
    expect(screen.getByRole("button", { name: /Pause/ })).toBeTruthy()
  })

  // The pill beside the line says the status; the line says what it means. "On" in
  // the pill and "On." starting the line read the status twice.
  it.each([
    ["On", { status: "active", next_run_at: "2026-10-11T07:00:00Z" }, /^Next run .+\.$/],
    [
      "On",
      {
        status: "active",
        definition: { ...workflow().definition, trigger: { type: "after_pipeline", pipeline_id: "p1" } },
        trigger_description: "after Customers & Lending — Nightly finishes",
      },
      /^Runs after Customers & Lending — Nightly finishes\.$/,
    ],
    ["Paused", { status: "paused", paused_reason: "Its schedule could not be read: bad cron" }, /^Its schedule could not be read: bad cron\.$/],
    ["Paused", { status: "paused" }, /^It won't run on its own until you turn it on\.$/],
  ] as [string, Partial<Workflow>, RegExp][])("says %s once, and what it means in the line beside it", async (pill, over, line) => {
    route({ get: json(200, { workflow: workflow(over), can_manage: true }) })
    render(<WorkflowReview workflowId={WF} />)
    const header = (await screen.findByRole("heading", { name: "Failed payments" })).closest("header")!
    expect(header.querySelector("p")?.textContent).toMatch(line)
    expect(header.textContent?.match(new RegExp(`\\b${pill}\\b`, "g"))).toHaveLength(1)
  })

  it("gives a member no buttons", async () => {
    route({ canManage: false })
    render(<WorkflowReview workflowId={WF} />)
    await screen.findByText(/Only workspace owners and admins/)
    expect(screen.queryByRole("button", { name: /Turn on/ })).toBeNull()
    expect(screen.queryByRole("button", { name: /Test/ })).toBeNull()
  })

  it("says so when the draft is gone", async () => {
    route({ get: json(404, {}) })
    render(<WorkflowReview workflowId={WF} />)
    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/not found in this workspace/))
  })
})

describe("WorkflowEditor", () => {
  const CREATED = "2026-09-14T08:30:00.000Z"

  it("shows when it was created, the selected step's settings, the runs tab, and deletes on the second click", async () => {
    route()
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url === API_ENDPOINTS.WORKFLOWS.GET(WF)) {
        return Promise.resolve(json(200, { workflow: workflow({ created_at: CREATED }), can_manage: true }))
      }
      if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs: [] }))
      if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "DELETE") return Promise.resolve(json(200, {}))
      return Promise.resolve(json(200, { deleted: true }))
    })
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByRole("heading", { name: "Failed payments" })
    const created = screen.getByText(/Created/).querySelector("time")
    expect(created).toHaveAttribute("dateTime", CREATED)
    const panel = screen.getByRole("complementary", { name: "Step settings" })
    expect(panel.textContent).toContain("0 * * * *")
    fireEvent.click(screen.getByRole("button", { name: "2. Query" }))
    expect(screen.getByLabelText("SQL").textContent).toContain("SELECT count(*) AS failed FROM payments")
    fireEvent.click(screen.getByRole("button", { name: "4. If → #payments" }))
    expect(screen.getByLabelText("Message").textContent).toContain("{{failed}} failed in the last hour")

    fireEvent.click(screen.getByRole("button", { name: /Delete/ }))
    expect(posts()).toEqual([])
    expect(mockFetch.mock.calls.some(([, i]) => i?.method === "DELETE")).toBe(false)
    fireEvent.click(screen.getByRole("button", { name: /Click again to delete/ }))
    await waitFor(() => expect(push).toHaveBeenCalledWith("/workflows"))
  })
})

describe("workflowSteps", () => {
  it("marks how far the last run got", () => {
    const base = workflow({ status: "active" })
    const sent = workflowSteps({
      ...base,
      last_run: { id: "r", trigger: "schedule", status: "succeeded", condition_met: true, row_count: 1,
        deliveries: [{ channel_id: "ch1", name: "#payments", kind: "slack", status: "sent" }], notes: [], started_at: "" },
    })
    expect(sent.map((s) => s.state)).toEqual(["ok", "ok", "ok", "ok"])
    expect(sent.map((s) => s.id)).toEqual(["trigger", "query", "branch:0", "channel:send_1:ch1"])
    const quiet = workflowSteps({
      ...base,
      last_run: { id: "r", trigger: "schedule", status: "succeeded", condition_met: false, row_count: 0,
        deliveries: [], notes: [], started_at: "" },
    })
    // The condition was checked and was not true: its own mark, not a green tick.
    expect(quiet.map((s) => s.state)).toEqual(["ok", "ok", "untrue", "skipped"])
    const broke = workflowSteps({
      ...base,
      last_run: { id: "r", trigger: "schedule", status: "failed", deliveries: [], notes: [], error: "x", started_at: "" },
    })
    expect(broke[1].state).toBe("failed")
  })
})

describe("Ask rsync.ai's workflow cards", () => {
  it("shows a card for each stored draft, never for a refused call", () => {
    const answer = {
      answer: "Drafted.",
      looked_at: [
        { tool: "plan_workflow", ok: true, resource_id: WF },
        { tool: "plan_workflow", ok: false, resource_id: "refused" },
        { tool: "plan_pipeline", ok: true, resource_id: "a-plan" },
        { tool: "describe_schema", ok: true, resource_id: "c1" },
      ],
    } as unknown as AssistantAnswer
    expect(workflowIds(answer)).toEqual([WF])
  })
})

describe("parseRecipients", () => {
  it("splits on commas, semicolons and new lines", () => {
    expect(parseRecipients(" a@x.io, b@x.io;\nc@x.io  ")).toEqual(["a@x.io", "b@x.io", "c@x.io"])
  })
})
