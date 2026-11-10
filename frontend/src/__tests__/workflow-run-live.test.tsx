import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, renderHook, screen, within } from "@testing-library/react"

import WorkflowsPage from "@/app/(dashboard)/workflows/page"
import { workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowRuns } from "@/components/workflows/WorkflowRuns"
import { useWorkflow } from "@/components/workflows/useWorkflow"
import { authFetch } from "@/lib/api/auth-fetch"
import type { StepRunStatus, Workflow, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// #1402: after Run now the canvas kept the run as it was just after the click,
// until a page refresh. The class: a view shows a run's
// progress from data it read once, or a re-read is cut off by the next one.
// Every workflow view that shows a run reads it again while the run is going.
// Pinned here:
//   - the canvas (useWorkflow) follows a run step by step, and stops reading
//     once the run has finished;
//   - a failed re-read keeps the workflow shown (the error view would replace
//     the page), and the next one tries again;
//   - a change saved while a re-read is in flight is not put back by that
//     older read;
//   - a read slower than the interval lands, in the canvas and in Runs;
//   - the Workflows list follows a running last run, and drops a re-read that
//     lands after a switch to another workspace.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000f7"
const at = "2026-10-05T09:00:00Z"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function lastRun(status: WorkflowRun["status"], steps: [string, StepRunStatus][]): WorkflowRun {
  return {
    id: "r1",
    trigger: "manual",
    status,
    deliveries:
      status === "succeeded" ? [{ step_id: "send_1", channel_id: "ch1", name: "#data", kind: "slack", status: "sent" }] : [],
    notes: [],
    started_at: at,
    steps: steps.map(([step_id, s]) => ({
      step_id,
      type: step_id === "send_1" ? "send" : "run_pipeline",
      attempt: 1,
      status: s,
      started_at: at,
    })),
  }
}

// Run now → pipeline Orders → pipeline Order items → a message to #data.
function workflow(run?: WorkflowRun, overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Nightly load",
    status: "active",
    definition: {
      version: 3,
      trigger: { type: "manual" },
      steps: [
        { id: "load_orders", type: "run_pipeline", pipeline_id: "pl-1" },
        { id: "load_items", type: "run_pipeline", after: ["load_orders"], rule: "all_success", pipeline_id: "pl-2" },
        {
          id: "send_1",
          type: "send",
          after: ["load_items"],
          rule: "all_success",
          message: { subject: "Loaded", body: "Done" },
          required: true,
        },
      ],
    },
    trigger_description: "when someone clicks Run now",
    condition_description: "",
    branch_descriptions: [],
    connection: null,
    channels: [{ id: "ch1", name: "#data", kind: "slack", node_id: "send_1" }],
    step_refs: { load_orders: { id: "pl-1", name: "Orders" }, load_items: { id: "pl-2", name: "Order items" } },
    proposed_by: "assistant",
    created_at: at,
    updated_at: at,
    url: `https://app.example/workflows/${WF}`,
    last_run: run,
    ...overrides,
  }
}

const STARTED = lastRun("running", [["load_orders", "running"]])
const HALFWAY = lastRun("running", [
  ["load_orders", "succeeded"],
  ["load_items", "running"],
])
const DONE = lastRun("succeeded", [
  ["load_orders", "succeeded"],
  ["load_items", "succeeded"],
  ["send_1", "succeeded"],
])

/** Each node after the trigger, with the state the canvas draws it in. */
function nodes(w: Workflow | null) {
  return workflowSteps(w!)
    .slice(1)
    .map((s) => [s.id, s.state])
}
const AT_START = [
  ["step:load_orders", "running"],
  ["step:load_items", undefined],
  ["channel:send_1:ch1", undefined],
]
const AT_HALFWAY = [
  ["step:load_orders", "ok"],
  ["step:load_items", "running"],
  ["channel:send_1:ch1", undefined],
]
const AT_END = [
  ["step:load_orders", "ok"],
  ["step:load_items", "ok"],
  ["channel:send_1:ch1", "ok"],
]

const read = (run?: WorkflowRun, overrides: Partial<Workflow> = {}) =>
  json(200, { workflow: workflow(run, overrides), can_manage: true })

/** Answers each request with the next of these, and with the last one after that. */
function answers(...rs: Array<Response | Promise<Response>>) {
  let i = 0
  mockFetch.mockImplementation(() => Promise.resolve(rs[Math.min(i++, rs.length - 1)]))
}

const calls = (url: string) => mockFetch.mock.calls.filter(([u]) => u === url).length

async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

beforeEach(() => {
  vi.useFakeTimers()
  mockFetch.mockReset()
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe("useWorkflow: the canvas follows a run", () => {
  it("reads the workflow again while its last run is going, and each node follows its step", async () => {
    answers(read(STARTED), read(HALFWAY), read(DONE))
    const { result } = renderHook(() => useWorkflow(WF))
    await tick(0)
    expect(nodes(result.current.workflow)).toEqual(AT_START)

    await tick(3000)
    expect(nodes(result.current.workflow)).toEqual(AT_HALFWAY)
    await tick(3000)
    expect(nodes(result.current.workflow)).toEqual(AT_END)

    // The run has finished: nothing reads it again.
    await tick(9000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.GET(WF))).toBe(3)
  })

  it("does not read again a workflow whose last run has finished, or that has none", async () => {
    answers(read(DONE))
    renderHook(() => useWorkflow(WF))
    await tick(9000)
    cleanup()
    answers(read())
    renderHook(() => useWorkflow(WF))
    await tick(9000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.GET(WF))).toBe(2)
  })

  it("keeps the workflow shown when a re-read fails, and the next one tries again", async () => {
    answers(read(STARTED), json(502, { error: "bad gateway" }), read(HALFWAY))
    const { result } = renderHook(() => useWorkflow(WF))
    await tick(0)
    await tick(3000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.GET(WF))).toBe(2)
    expect(result.current.error).toBeNull()
    expect(nodes(result.current.workflow)).toEqual(AT_START)

    await tick(3000)
    expect(nodes(result.current.workflow)).toEqual(AT_HALFWAY)
  })

  it("does not put back a change saved while a re-read was in flight", async () => {
    let answer: (r: Response) => void = () => {}
    answers(read(STARTED), new Promise<Response>((resolve) => (answer = resolve)))
    const { result } = renderHook(() => useWorkflow(WF))
    await tick(0)
    await tick(3000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.GET(WF))).toBe(2)

    const saved = workflow(STARTED, { name: "Nightly load, edited", updated_at: "2026-10-05T09:00:05Z" })
    act(() => result.current.replace(saved))
    await act(async () => answer(read(HALFWAY)))
    expect(result.current.workflow?.name).toBe("Nightly load, edited")
    expect(result.current.workflow?.updated_at).toBe("2026-10-05T09:00:05Z")
  })

  it("lets a re-read slower than the interval land instead of starting another", async () => {
    let answer: (r: Response) => void = () => {}
    answers(read(STARTED), new Promise<Response>((resolve) => (answer = resolve)), read(DONE))
    const { result } = renderHook(() => useWorkflow(WF))
    await tick(0)
    await tick(3000)
    await tick(6000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.GET(WF))).toBe(2)

    await act(async () => answer(read(HALFWAY)))
    expect(nodes(result.current.workflow)).toEqual(AT_HALFWAY)
  })
})

describe("WorkflowRuns: a slow answer still lands", () => {
  it("shows a list of runs that took longer than the interval to answer", async () => {
    let n = 0
    mockFetch.mockImplementation(() =>
      n++ === 0
        ? Promise.resolve(json(200, { runs: [STARTED] }))
        : new Promise<Response>((resolve) => setTimeout(() => resolve(json(200, { runs: [DONE] })), 4000)),
    )
    render(<WorkflowRuns workflow={workflow()} />)
    await tick(0)
    expect(screen.getByText(/Pipeline Orders: running/)).toBeInTheDocument()

    await tick(7000)
    expect(screen.queryByText(/Pipeline Orders: running/)).toBeNull()
    expect(screen.getByText(/Pipeline Orders: succeeded/)).toBeInTheDocument()
  })
})

describe("Workflows list: a running last run", () => {
  const B = workflow(undefined, { id: "b1", name: "Workspace B alert" })
  const list = (workflows: Workflow[]) => json(200, { workflows, can_manage: true })
  const lastRunOf = (name: string) => {
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent)
    const row = screen.getByRole("link", { name }).closest("tr")!
    return within(row).getAllByRole("cell")[headers.indexOf("Last run")].textContent
  }

  beforeEach(() => setActiveWorkspaceId("ws-a"))
  afterEach(() => setActiveWorkspaceId(null))

  it("reads the list again while a last run is going, and stops after", async () => {
    answers(list([workflow(STARTED)]), list([workflow(HALFWAY)]), list([workflow(DONE)]))
    render(<WorkflowsPage />)
    await tick(0)
    expect(lastRunOf("Nightly load")).toMatch(/^running/)

    await tick(6000)
    expect(lastRunOf("Nightly load")).toMatch(/^sent/)
    await tick(9000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.LIST)).toBe(3)
  })

  it("drops a re-read that lands after a switch to another workspace", async () => {
    let answerA: (r: Response) => void = () => {}
    mockFetch
      .mockResolvedValueOnce(list([workflow(STARTED)]))
      .mockImplementationOnce(() => new Promise<Response>((resolve) => (answerA = resolve)))
      .mockResolvedValueOnce(list([B]))
    render(<WorkflowsPage />)
    await tick(0)
    await tick(3000)
    expect(calls(API_ENDPOINTS.WORKFLOWS.LIST)).toBe(2)

    act(() => setActiveWorkspaceId("ws-b"))
    await tick(0)
    expect(screen.getByText("Workspace B alert")).toBeInTheDocument()
    await act(async () => answerA(list([workflow(DONE)])))
    expect(screen.queryByText("Nightly load")).toBeNull()
    expect(screen.getByText("Workspace B alert")).toBeInTheDocument()
  })
})
