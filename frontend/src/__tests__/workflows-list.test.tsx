import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import WorkflowsPage from "@/app/(dashboard)/workflows/page"
import { ASSISTANT_EXAMPLES } from "@/components/home/HomeAskBox"
import { featureFlagsManager } from "@/config/features"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// The Workflows list. Pinned here:
//   - each row says when the workflow was created;
//   - a workspace switch empties the list and asks again, because this is a
//     client page and nothing else re-reads it;
//   - a response from the workspace switched away from is dropped even when it
//     lands last, so one workspace's workflows never show under another's.
//   - "Sends to" names each channel once, though a channel on two conditions
//     comes back once per condition.
//   - the counts over the list: by status, and how many last ran into a failure;
//   - each row's steps as icons a screen reader reads in run order, its status
//     as a pill, its last run's outcome, and a click anywhere on it opens it;
//   - "New workflow" opens Ask rsync.ai with an example, and is not offered
//     when there is no assistant to draft one with;
//   - "Build it yourself" opens /workflows/new for an owner or admin, with or
//     without the assistant (beside it, the secondary button), never for a member.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const push = vi.fn()
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace: vi.fn(), refresh: vi.fn() }),
}))

const mockFetch = authFetch as unknown as Mock

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function workflow(id: string, name: string, createdAt: string): Workflow {
  return {
    id,
    name,
    status: "draft",
    definition: branchesDefinition({
      trigger: { type: "manual" },
      query: { connection_id: "c1", sql: "SELECT 1 AS one" },
      branches: [{ condition: { type: "always", threshold: 0 }, message: { subject: "", body: "{{one}}" } }],
    }),
    trigger_description: "when you click Run now",
    condition_description: "always send",
    branch_descriptions: ["always send"],
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [],
    proposed_by: "assistant",
    created_at: createdAt,
    updated_at: createdAt,
    url: `https://app.example/workflows/${id}`,
  }
}

const CREATED_A = "2026-09-14T08:30:00.000Z"
const A = workflow("a1", "Workspace A digest", CREATED_A)
const B = workflow("b1", "Workspace B alert", "2026-10-01T12:00:00.000Z")

function list(workflows: Workflow[]) {
  return json(200, { workflows, can_manage: true })
}

function lastRun(status: WorkflowRun["status"]): WorkflowRun {
  return { id: "r1", trigger: "schedule", status, deliveries: [], notes: [], started_at: "2026-10-06T09:00:00.000Z" }
}

/** A count card's number, by its title. */
function countOf(title: string): string | null {
  return within(screen.getByRole("region", { name: title })).getAllByRole("paragraph")[0].textContent
}

/** A row's cell under a column header. */
function cellOf(row: HTMLElement, header: string): HTMLElement {
  const headers = screen.getAllByRole("columnheader").map((h) => h.textContent)
  return within(row).getAllByRole("cell")[headers.indexOf(header)]
}

function assistantOn() {
  featureFlagsManager.updateFlags({ assistant: true, llmConfigured: true })
  featureFlagsManager.markResolved()
}

beforeEach(() => {
  mockFetch.mockReset()
  push.mockReset()
  setActiveWorkspaceId("ws-a")
})

afterEach(() => {
  setActiveWorkspaceId(null)
  featureFlagsManager.resetFlags()
  useAssistantStore.setState({ open: false, suggestion: null })
})

describe("Workflows list", () => {
  it("shows when each workflow was created", async () => {
    mockFetch.mockResolvedValue(list([A]))
    render(<WorkflowsPage />)
    const row = (await screen.findByRole("link", { name: "Workspace A digest" })).closest("tr")!
    expect(screen.getByRole("columnheader", { name: "Created" })).toBeInTheDocument()
    const cells = within(row).getAllByRole("cell")
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent)
    const created = cells[headers.indexOf("Created")]
    expect(created.querySelector("time")).toHaveAttribute("dateTime", CREATED_A)
  })

  it("names a channel once in Sends to when two branches share it", async () => {
    const shared = {
      ...A,
      channels: [
        caseChannel({ id: "ch1", name: "Rahul email", kind: "email", branch: 0 }),
        caseChannel({ id: "ch2", name: "Ops Slack", kind: "slack", branch: 0 }),
        caseChannel({ id: "ch1", name: "Rahul email", kind: "email", branch: 1 }),
      ],
    }
    mockFetch.mockResolvedValue(list([shared]))
    render(<WorkflowsPage />)
    const row = (await screen.findByRole("link", { name: "Workspace A digest" })).closest("tr")!
    const sendsTo = cellOf(row, "Sends to")
    expect(within(sendsTo).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["Rahul email", "Ops Slack"])
    expect(within(sendsTo).getByLabelText("Email")).toBeInTheDocument()
    expect(within(sendsTo).getByLabelText("Slack message")).toBeInTheDocument()
  })

  it("counts the workflows by status and how many last ran into a failure", async () => {
    mockFetch.mockResolvedValue(
      list([
        { ...A, id: "w1", status: "active", last_run: lastRun("failed") },
        { ...A, id: "w2", name: "Two", status: "active", last_run: lastRun("succeeded") },
        { ...A, id: "w3", name: "Three", status: "paused" },
        { ...A, id: "w4", name: "Four" },
      ]),
    )
    render(<WorkflowsPage />)
    await screen.findByTestId("workflow-counts")
    expect(countOf("Total")).toBe("4")
    expect(countOf("On")).toBe("2")
    expect(countOf("Paused")).toBe("1")
    expect(countOf("Drafts")).toBe("1")
    expect(countOf("Last run failed")).toBe("1")
    expect(screen.getByRole("region", { name: "Last run failed" })).toHaveTextContent("Its Runs tab has the error")
  })

  it("shows each row's steps in run order, its status and its last run's outcome", async () => {
    const on = {
      ...A,
      status: "active" as const,
      channels: [caseChannel({ id: "ch1", name: "Ops Slack", kind: "slack", branch: 0 })],
      last_run: lastRun("failed"),
    }
    mockFetch.mockResolvedValue(list([on, B]))
    render(<WorkflowsPage />)
    const row = (await screen.findByRole("link", { name: "Workspace A digest" })).closest("tr")!
    expect(cellOf(row, "Steps")).toHaveTextContent("Query, then Condition, then Slack message")
    expect(cellOf(row, "Status")).toHaveTextContent("On")
    expect(cellOf(row, "Last run")).toHaveTextContent("failed")
    const draft = screen.getByRole("link", { name: "Workspace B alert" }).closest("tr")!
    expect(cellOf(draft, "Status")).toHaveTextContent("Draft")
    expect(cellOf(draft, "Last run")).toHaveTextContent("Never")
  })

  it("opens a workflow from a click anywhere on its row", async () => {
    mockFetch.mockResolvedValue(list([A]))
    render(<WorkflowsPage />)
    const row = (await screen.findByRole("link", { name: "Workspace A digest" })).closest("tr")!
    await userEvent.click(cellOf(row, "Steps"))
    expect(push).toHaveBeenCalledWith("/workflows/a1")
  })

  it("says what a workflow is when there are none, and offers to draft one", async () => {
    assistantOn()
    mockFetch.mockResolvedValue(list([]))
    render(<WorkflowsPage />)
    expect(await screen.findByText("No workflows yet")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "New workflow" }))
    expect(useAssistantStore.getState()).toMatchObject({ open: true, suggestion: ASSISTANT_EXAMPLES[2].text })
  })

  it("offers no New workflow without an assistant to draft it", async () => {
    featureFlagsManager.updateFlags({ assistant: false, llmConfigured: true })
    featureFlagsManager.markResolved()
    mockFetch.mockResolvedValue(list([A]))
    render(<WorkflowsPage />)
    await screen.findByRole("link", { name: "Workspace A digest" })
    expect(screen.queryByRole("button", { name: "New workflow" })).toBeNull()
  })

  it("offers an owner or admin Build it yourself, secondary beside the assistant", async () => {
    assistantOn()
    mockFetch.mockResolvedValue(list([]))
    render(<WorkflowsPage />)
    expect(await screen.findByText(/Or build one yourself: a query, the conditions to check, and a message for each\./)).toBeInTheDocument()
    const build = screen.getByRole("link", { name: "Build it yourself" })
    expect(build).toHaveAttribute("href", "/workflows/new")
    expect(build.classList.contains("border")).toBe(true)
    expect(screen.getByRole("button", { name: "New workflow" })).toBeInTheDocument()
  })

  it("offers Build it yourself as the one way in without an LLM, in the header too", async () => {
    featureFlagsManager.updateFlags({ assistant: true, llmConfigured: false })
    featureFlagsManager.markResolved()
    mockFetch.mockResolvedValue(list([]))
    render(<WorkflowsPage />)
    expect(await screen.findByText(/^Build one yourself: a query, the conditions to check, and a message for each\./)).toBeInTheDocument()
    expect(screen.queryByText(/Ask rsync\.ai drafts one/)).toBeNull()
    expect(screen.queryByRole("button", { name: "New workflow" })).toBeNull()
    expect(screen.getByRole("link", { name: "Build it yourself" }).classList.contains("border")).toBe(false)
    cleanup()
    mockFetch.mockResolvedValue(list([A]))
    render(<WorkflowsPage />)
    await screen.findByRole("link", { name: "Workspace A digest" })
    expect(screen.getByRole("link", { name: "Build it yourself" })).toHaveAttribute("href", "/workflows/new")
  })

  it("offers a member no Build it yourself", async () => {
    mockFetch.mockResolvedValue(json(200, { workflows: [], can_manage: false }))
    render(<WorkflowsPage />)
    expect(await screen.findByText(/Workspace owners and admins build workflows\./)).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "Build it yourself" })).toBeNull()
  })

  it("shows placeholders, not an empty table, while the list loads", async () => {
    mockFetch.mockImplementation(() => new Promise<Response>(() => {}))
    render(<WorkflowsPage />)
    expect(screen.getByText("Loading workflows…")).toBeInTheDocument()
    expect(screen.queryByRole("table")).toBeNull()
  })

  it("asks again for the new workspace's workflows on a switch", async () => {
    mockFetch.mockResolvedValueOnce(list([A])).mockResolvedValueOnce(list([B]))
    render(<WorkflowsPage />)
    await screen.findByText("Workspace A digest")

    act(() => setActiveWorkspaceId("ws-b"))
    expect(await screen.findByText("Workspace B alert")).toBeInTheDocument()
    expect(screen.queryByText("Workspace A digest")).toBeNull()
    expect(mockFetch.mock.calls.map(([url]) => url)).toEqual([
      API_ENDPOINTS.WORKFLOWS.LIST,
      API_ENDPOINTS.WORKFLOWS.LIST,
    ])
  })

  it("drops the old workspace's answer when it lands after the switch", async () => {
    let answerA!: (r: Response) => void
    mockFetch
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (answerA = resolve)))
      .mockResolvedValueOnce(list([B]))
    render(<WorkflowsPage />)

    act(() => setActiveWorkspaceId("ws-b"))
    await screen.findByText("Workspace B alert")
    await act(async () => answerA(list([A])))
    expect(screen.queryByText("Workspace A digest")).toBeNull()
    expect(screen.getByText("Workspace B alert")).toBeInTheDocument()
  })
})
