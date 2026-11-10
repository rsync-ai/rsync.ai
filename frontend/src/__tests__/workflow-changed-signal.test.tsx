import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"

import WorkflowsPage from "@/app/(dashboard)/workflows/page"
import { WorkflowChangeReview } from "@/components/workflows/WorkflowChangeReview"
import { WorkflowReview } from "@/components/workflows/WorkflowReview"
import { askAssistant, type AssistantAnswer, type LookedAt } from "@/lib/api/assistant"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowChange } from "@/lib/api/workflows"
import { sendQuestion } from "@/lib/assistant/ask"
import { API_ENDPOINTS } from "@/lib/config/api"
import { notifyWorkflowChanged, onWorkflowChanged } from "@/lib/events/workflowChanged"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// A workflow drafted or changed from Ask rsync.ai shows on the Workflows list
// without a reload (inbox 2026-10-02). The panel sits beside the list and has
// no handle on it, so each place a workflow is drafted or changed outside the
// list says so, and the list reads itself again. Pinned here:
//   - the list asks again on the signal, and shows the new draft;
//   - an answer whose plan_workflow stored a draft signals it, once per draft;
//     a refused call, or another tool's write, signals nothing;
//   - Turn on from the draft's card signals it;
//   - applying a proposed change signals the workflow it changed.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }) }))
vi.mock("@/lib/api/assistant", async (orig) => {
  const actual = await orig<typeof import("@/lib/api/assistant")>()
  return { ...actual, askAssistant: vi.fn() }
})
// The canvas is xyflow, which needs a layout engine jsdom lacks.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const mockAsk = askAssistant as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000f7"
const CHANGE = "ffffffff-0000-4000-8000-0000000000c7"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Failed payments",
    status: "draft",
    definition: branchesDefinition({
      trigger: { type: "manual" },
      query: { connection_id: "c1", sql: "SELECT count(*) AS failed FROM payments" },
      branches: [{ condition: { type: "always", threshold: 0 }, message: { subject: "", body: "{{failed}}" } }],
    }),
    trigger_description: "when you click Run now",
    condition_description: "always send",
    branch_descriptions: ["always send"],
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 0 })],
    proposed_by: "assistant",
    created_at: "2026-10-12T08:00:00.000Z",
    updated_at: "2026-10-12T08:00:00.000Z",
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function answer(looked_at: LookedAt[]): AssistantAnswer {
  return {
    conversation_id: "conv-1",
    answer: "Drafted it.",
    model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
    looked_at,
    cost_cents: 0,
  }
}

/** Every workflow id signalled while the test runs. */
let signalled: string[] = []
let unsubscribe: () => void = () => {}

beforeEach(() => {
  mockFetch.mockReset()
  mockAsk.mockReset()
  setActiveWorkspaceId("ws-a")
  useAssistantStore.setState({ chats: [], activeChatId: null })
  signalled = []
  unsubscribe = onWorkflowChanged((id) => signalled.push(id))
})

afterEach(() => {
  unsubscribe()
  setActiveWorkspaceId(null)
})

describe("the Workflows list", () => {
  it("reads itself again on the signal and shows the new draft", async () => {
    const before = workflow({ id: "w-old", name: "Weekly revenue" })
    mockFetch.mockResolvedValueOnce(json(200, { workflows: [before], can_manage: true }))
    render(<WorkflowsPage />)
    await screen.findByRole("link", { name: "Weekly revenue" })
    expect(mockFetch).toHaveBeenCalledTimes(1)

    mockFetch.mockResolvedValueOnce(json(200, { workflows: [workflow(), before], can_manage: true }))
    act(() => notifyWorkflowChanged(WF))
    expect(await screen.findByRole("link", { name: "Failed payments" })).toBeTruthy()
    expect(mockFetch).toHaveBeenCalledTimes(2)
    expect(mockFetch.mock.calls.map(([u]) => u)).toEqual([API_ENDPOINTS.WORKFLOWS.LIST, API_ENDPOINTS.WORKFLOWS.LIST])
  })
})

describe("an Ask rsync.ai answer", () => {
  const send = () => sendQuestion({ chatId: null, question: "email me failed payments", workspaceId: "ws-a", page: null })

  it("signals each draft its plan_workflow stored, once", async () => {
    mockAsk.mockResolvedValue(
      answer([
        { tool: "list_connections", arguments: {}, ok: true },
        { tool: "plan_workflow", arguments: {}, ok: true, resource_id: WF },
        { tool: "plan_workflow", arguments: {}, ok: true, resource_id: WF },
      ]),
    )
    await send()
    expect(signalled).toEqual([WF])
  })

  it("signals nothing for a refused call or another tool's write", async () => {
    mockAsk.mockResolvedValue(
      answer([
        { tool: "plan_workflow", arguments: {}, ok: false },
        { tool: "plan_pipeline", arguments: {}, ok: true, resource_id: "plan-1" },
        { tool: "plan_dashboard", arguments: {}, ok: true, resource_id: "dash-1" },
      ]),
    )
    await send()
    // Control: the answer did land in the chat.
    expect(useAssistantStore.getState().chats[0]?.items.some((i) => i.kind === "answer")).toBe(true)
    expect(signalled).toEqual([])
  })
})

describe("the draft's card", () => {
  it("signals the workflow it turned on", async () => {
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url === API_ENDPOINTS.WORKFLOWS.GET(WF) && method === "GET") {
        return Promise.resolve(json(200, { workflow: workflow(), can_manage: true }))
      }
      if (url === API_ENDPOINTS.WORKFLOWS.ACTIVATE(WF) && method === "POST") {
        return Promise.resolve(json(200, { workflow: workflow({ status: "active" }) }))
      }
      return Promise.resolve(json(404, { error: "not_found" }))
    })
    render(<WorkflowReview workflowId={WF} />)
    // Control: loading the card signals nothing.
    await screen.findByRole("button", { name: /Turn on/ })
    expect(signalled).toEqual([])

    fireEvent.click(screen.getByRole("button", { name: /Turn on/ }))
    await screen.findByText(/Turned on/)
    expect(signalled).toEqual([WF])
  })
})

describe("a proposed change", () => {
  it("signals the workflow it applied to", async () => {
    const c: WorkflowChange = {
      id: CHANGE,
      workflow_id: WF,
      workflow_name: "Failed payments",
      summary: "Sends to #payments only when it fails.",
      status: "proposed",
      proposed_by: "assistant",
      current: workflow(),
      proposed: workflow(),
      created_at: "2026-10-12T08:00:00.000Z",
      expires_at: "2026-10-13T08:00:00.000Z",
      url: `https://app.example/workflows/${WF}?change=${CHANGE}`,
    }
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET"
      if (url === API_ENDPOINTS.WORKFLOW_CHANGES.GET(CHANGE) && method === "GET") {
        return Promise.resolve(json(200, { change: c, can_manage: true }))
      }
      if (url === API_ENDPOINTS.WORKFLOW_CHANGES.APPLY(CHANGE) && method === "POST") {
        return Promise.resolve(json(200, { workflow: workflow({ updated_at: "2026-10-12T09:00:00Z" }), query_checked: true }))
      }
      return Promise.resolve(json(404, { error: "not_found" }))
    })
    render(<WorkflowChangeReview changeId={CHANGE} />)
    fireEvent.click(await screen.findByRole("button", { name: "Apply change" }))
    await screen.findByText("Applied")
    expect(signalled).toEqual([WF])
  })
})
