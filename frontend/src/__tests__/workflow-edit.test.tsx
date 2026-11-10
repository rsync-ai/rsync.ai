import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"
import { WorkflowRuns } from "@/components/workflows/WorkflowRuns"
import { refusalStep } from "@/components/workflows/useWorkflowEdit"
import { featureFlagsManager } from "@/config/features"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// Editing a workflow (PUT /workflows/:id). The server checks the edit as it
// checks a draft and refuses a stale one; what is pinned here is what only the
// editor can get wrong:
//   - Edit is offered only to an owner or admin, and only with an LLM set up;
//   - Save is one PUT carrying the whole draft and the updated_at it started
//     from, and sends nothing (no POST);
//   - the connection picker offers only what the server accepts (SQL explorer);
//   - a refusal opens the step its code names, keeping the edit;
//   - a 409 offers a reload that drops the edit;
//   - a run of an earlier version says so;
//   - one status line at a time: what the last action said, never a stale one beside it.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))
// xyflow needs a layout engine jsdom lacks; the step panel carries the same steps.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000e1"
const LOADED_AT = "2026-10-02T10:00:00.123456Z"

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
    created_at: LOADED_AT,
    updated_at: LOADED_AT,
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function route(opts: { canManage?: boolean; put?: Response; run?: Response } = {}) {
  let current = workflow()
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "GET") {
      return Promise.resolve(json(200, { workflow: current, can_manage: opts.canManage ?? true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "PUT") {
      if (opts.put) return Promise.resolve(opts.put)
      const body = JSON.parse(String(init?.body))
      current = workflow({
        name: body.name,
        definition: body.definition,
        channels: [caseChannel({ id: "ch2", name: "Finance list", kind: "email", branch: 0 })],
        updated_at: "2026-10-02T11:00:00Z",
      })
      return Promise.resolve(json(200, { workflow: current, query_checked: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs: [] }))
    if (url === API_ENDPOINTS.WORKFLOWS.RUN(WF) && method === "POST") {
      return Promise.resolve(opts.run ?? json(202, { run_id: "r1" }))
    }
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
        json(200, {
          connections: [
            { id: "c1", name: "Warehouse", connector_type: "postgresql", supports_explorer: true },
            { id: "c2", name: "Orders DB", connector_type: "mysql", supports_explorer: true, explorer_mode: "sql" },
            { id: "c3", name: "Events", connector_type: "mongodb", supports_explorer: true, explorer_mode: "document" },
            { id: "c4", name: "Leads API", connector_type: "hubspot", supports_explorer: false },
          ],
        }),
      )
    }
    if (url.startsWith(API_ENDPOINTS.PIPELINES.LIST)) {
      return Promise.resolve(json(200, { pipelines: [{ id: "p1", name: "Orders to BigQuery" }], total: 1 }))
    }
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

describe("Edit is offered", () => {
  it("to an owner or admin when an LLM is set up", async () => {
    route()
    render(<WorkflowEditor workflowId={WF} />)
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy()
  })

  it("also without an LLM: a person edits it by hand", async () => {
    featureFlagsManager.updateFlags({ llmConfigured: false })
    route()
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByRole("button", { name: "Test" })
    expect(screen.getByRole("button", { name: "Edit" })).toBeTruthy()
  })

  it("not to a member", async () => {
    route({ canManage: false })
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByText(/Only workspace owners and admins/)
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull()
  })
})

describe("Save", () => {
  it("is one PUT with the whole draft and the version it started from, and sends nothing", async () => {
    route()
    await startEditing()

    fireEvent.change(screen.getByLabelText("Workflow name"), { target: { value: "Failed payments, hourly" } })
    fireEvent.click(screen.getByRole("button", { name: "2. Query" }))
    const sql = within(panel()).getByLabelText("SQL")
    fireEvent.change(sql, { target: { value: "SELECT count(*) AS failed FROM payments WHERE status = 'failed'" } })
    // Only connections the server takes for a workflow: SQL explorer ones.
    const conn = within(panel()).getByLabelText(/^Connection/) as HTMLSelectElement
    await waitFor(() => expect(within(conn).queryByRole("option", { name: "Orders DB (mysql)" })).toBeTruthy())
    expect(Array.from(conn.options).map((o) => o.value)).toEqual(["", "c2", "c1"])
    fireEvent.change(conn, { target: { value: "c2" } })

    fireEvent.click(screen.getByRole("button", { name: "4. If → #payments" }))
    fireEvent.click(await within(panel()).findByRole("checkbox", { name: /Finance list/ }))
    fireEvent.click(within(panel()).getByRole("checkbox", { name: /#payments/ }))
    // The panel stays on a channel step: the one left.
    expect(screen.getByRole("button", { name: "4. If → Finance list" })).toHaveAttribute("aria-pressed", "true")

    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByText(/ran the query once to check it; nothing was sent/)

    expect(writes().map(([u, i]) => [u, i.method])).toEqual([[API_ENDPOINTS.WORKFLOWS.GET(WF), "PUT"]])
    const body = JSON.parse(String(writes()[0][1].body))
    expect(body).toEqual({
      name: "Failed payments, hourly",
      definition: branchesDefinition({
        trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
        query: { connection_id: "c2", sql: "SELECT count(*) AS failed FROM payments WHERE status = 'failed'" },
        branches: [
          {
            condition: { type: "value", op: ">", threshold: 50 },
            message: { subject: "Failed payments", body: "{{failed}} failed in the last hour" },
          },
        ],
      }),
      node_channel_ids: { send_1: ["ch2"] },
      updated_at: LOADED_AT,
    })
    // Back to reading, showing what was saved.
    expect(await screen.findByRole("heading", { name: "Failed payments, hourly" })).toBeTruthy()
    expect(screen.queryByLabelText("Workflow name")).toBeNull()
  })

  it("says to Test when the check stopped before the query finished", async () => {
    route({ put: json(200, { workflow: workflow({ updated_at: "2026-10-02T11:00:00Z" }), query_checked: false }) })
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect(await screen.findByText(/still running when the check stopped, so click Test/)).toBeTruthy()
  })

  it("shows the draft as it would save: a switched trigger renames the step", async () => {
    route()
    await startEditing()
    fireEvent.change(within(panel()).getByLabelText("Runs"), { target: { value: "manual" } })
    expect(screen.getByRole("button", { name: "1. Run now" })).toBeTruthy()
    expect(within(panel()).getByText("Starts the workflow only when someone clicks Run now.", { selector: "p" })).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    expect(screen.getByRole("button", { name: "1. Schedule" })).toBeTruthy()
    expect(writes()).toEqual([])
  })
})

describe("The status line", () => {
  const RUN_STARTED = /Run started/
  const SAVED = /ran the query once to check it; nothing was sent/

  it("after Run now, then Edit and Save, says only the save", async () => {
    route()
    render(<WorkflowEditor workflowId={WF} />)
    fireEvent.click(await screen.findByRole("button", { name: "Run now" }))
    await screen.findByText(RUN_STARTED)

    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    await screen.findByLabelText("Workflow name")
    expect(screen.queryByText(RUN_STARTED)).toBeNull()

    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByText(SAVED)
    expect(screen.queryByText(RUN_STARTED)).toBeNull()
  })

  it("after Save, then Run now, says only the run", async () => {
    route()
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByText(SAVED)

    fireEvent.click(screen.getByRole("button", { name: "Run now" }))
    await screen.findByText(RUN_STARTED)
    expect(screen.queryByText(SAVED)).toBeNull()
  })

  it("drops a failed action's error once an edit starts", async () => {
    route({ run: json(500, { error: "internal", message: "The run did not start" }) })
    render(<WorkflowEditor workflowId={WF} />)
    fireEvent.click(await screen.findByRole("button", { name: "Run now" }))
    await screen.findByRole("alert")

    fireEvent.click(screen.getByRole("button", { name: "Edit" }))
    await screen.findByLabelText("Workflow name")
    expect(screen.queryByRole("alert")).toBeNull()
  })
})

describe("The pipeline picker", () => {
  // It read one page (limit=200) and silently dropped every older pipeline. The fake
  // answers both paging styles as ListPipelines does (pipelines.go), so the old read
  // gets the newest 200 of 250 and only paging reaches the oldest.
  it("offers every pipeline in the workspace, not just the first page", async () => {
    route()
    const base = mockFetch.getMockImplementation()!
    const all = Array.from({ length: 250 }, (_, i) => ({ id: `p-${i}`, name: `Pipeline ${String(i).padStart(3, "0")}` }))
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (!url.includes("/api/v1/pipelines?")) return base(url, init)
      const q = new URL(url, "http://x").searchParams
      let offset = Number(q.get("offset") ?? 0)
      let limit = Math.min(Number(q.get("limit") ?? 50), 200)
      if (q.has("page")) {
        limit = Math.min(Number(q.get("per_page")), 100)
        offset = (Number(q.get("page")) - 1) * limit
      }
      const rows = all.slice(offset, offset + limit)
      return Promise.resolve(json(200, { pipelines: rows, total: all.length, pagination: { has_next: offset + limit < all.length } }))
    })
    await startEditing()
    fireEvent.change(within(panel()).getByLabelText("Runs"), { target: { value: "after_pipeline" } })
    const picker = await within(panel()).findByRole("combobox", { name: /Pipeline/ })
    await waitFor(() => expect(within(picker).getByRole("option", { name: "Pipeline 249" })).toBeTruthy())
    // 250 and "Pick a pipeline".
    expect(within(picker).getAllByRole("option")).toHaveLength(251)
  })
})

describe("A refused save", () => {
  it("opens the step its code names and keeps the edit", async () => {
    route({ put: json(422, { error: "The query must be one read-only SELECT.", code: "invalid_sql" }) })
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "2. Query" }))
    fireEvent.change(within(panel()).getByLabelText("SQL"), { target: { value: "DELETE FROM payments" } })
    fireEvent.click(screen.getByRole("button", { name: "1. Schedule" }))

    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect((await screen.findByRole("alert")).textContent).toContain("one read-only SELECT")
    await waitFor(() => expect(screen.getByRole("button", { name: "2. Query" })).toHaveAttribute("aria-pressed", "true"))
    expect((within(panel()).getByLabelText("SQL") as HTMLTextAreaElement).value).toBe("DELETE FROM payments")
  })

  it("on a channel opens a channel step", async () => {
    route({ put: json(422, { error: "Pick at least one delivery channel.", code: "no_channels" }) })
    await startEditing()
    fireEvent.click(screen.getByRole("button", { name: "4. If → #payments" }))
    fireEvent.click(await within(panel()).findByRole("checkbox", { name: /#payments/ }))
    fireEvent.click(screen.getByRole("button", { name: "1. Schedule" }))
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    await screen.findByRole("alert")
    await waitFor(() => expect(screen.getByRole("button", { name: "4. If → No channel" })).toHaveAttribute("aria-pressed", "true"))
  })

  it("of a workflow changed since it was loaded offers a reload that drops the edit", async () => {
    route({ put: json(409, { error: "This workflow was changed since you opened it, so your edit was not saved." }) })
    await startEditing()
    fireEvent.change(screen.getByLabelText("Workflow name"), { target: { value: "Mine" } })
    fireEvent.click(screen.getByRole("button", { name: "Save" }))
    expect((await screen.findByRole("alert")).textContent).toContain("changed since you opened it")

    const gets = () => mockFetch.mock.calls.filter(([u, i]) => u === API_ENDPOINTS.WORKFLOWS.GET(WF) && !i?.method).length
    const before = gets()
    fireEvent.click(screen.getByRole("button", { name: /Reload it/ }))
    await waitFor(() => expect(gets()).toBe(before + 1))
    expect(await screen.findByRole("heading", { name: "Failed payments" })).toBeTruthy()
    expect(screen.queryByLabelText("Workflow name")).toBeNull()
  })
})

describe("refusalStep", () => {
  it("names the step for each refusal code, and nothing for an unknown one", () => {
    expect(refusalStep("invalid_name")).toBe("name")
    for (const c of ["invalid_trigger", "invalid_cron", "invalid_timezone", "invalid_pipeline_id", "pipeline_not_found"]) {
      expect(refusalStep(c)).toBe("trigger")
    }
    for (const c of ["invalid_connection_id", "connection_not_found", "connection_not_sql", "invalid_sql", "query_failed"]) {
      expect(refusalStep(c)).toBe("query")
    }
    for (const c of ["invalid_condition", "invalid_condition_op", "invalid_threshold", "invalid_quiet_minutes", "invalid_branches"]) {
      expect(refusalStep(c)).toBe("condition")
    }
    for (const c of ["invalid_message", "invalid_subject", "no_channels", "too_many_channels", "channel_not_found"]) {
      expect(refusalStep(c)).toBe("channel")
    }
    expect(refusalStep("something_else")).toBeNull()
    expect(refusalStep(undefined)).toBeNull()
  })
})

describe("WorkflowRuns", () => {
  it("marks a run of an earlier version with the query it ran", async () => {
    mockFetch.mockResolvedValue(
      json(200, {
        runs: [
          { id: "r2", trigger: "schedule", status: "succeeded", deliveries: [], notes: [], started_at: LOADED_AT },
          {
            id: "r1",
            trigger: "schedule",
            status: "succeeded",
            deliveries: [],
            notes: [],
            started_at: LOADED_AT,
            earlier_version: true,
            definition: branchesDefinition({
              query: { connection_id: "c1", sql: "SELECT 1 AS old" },
              branches: [{ condition: { type: "always", threshold: 0 }, message: { subject: "", body: "x" } }],
            }),
          },
        ],
      }),
    )
    render(<WorkflowRuns workflow={workflow()} />)
    const badges = await screen.findAllByText("Ran an earlier version")
    expect(badges).toHaveLength(1)
    expect(screen.getByLabelText("SQL it ran").textContent).toBe("SELECT 1 AS old")
  })
})
