import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { workflowGraph, workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowDetails } from "@/components/workflows/WorkflowDetails"
import { WorkflowRuns } from "@/components/workflows/WorkflowRuns"
import { WorkflowStepPanel } from "@/components/workflows/WorkflowStepPanel"
import { WorkflowSummary } from "@/components/workflows/WorkflowSummary"
import { WorkflowTestResult } from "@/components/workflows/WorkflowTestResult"
import { RUN_OUTCOME_LABEL, runOutcome } from "@/components/workflows/runOutcome"
import { afterWords, draftRunNote, stepSentence, workflowSummary } from "@/components/workflows/workflowWords"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Workflow, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { formatAbsoluteTime } from "@/lib/utils"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// Workflows design § 9 step 5: a workflow can run pipelines, refresh models and
// wait. The screens show those steps read-only (only Ask rsync.ai changes
// them), each run's steps with links to the pipeline run or model it started,
// and a Stop button for a running run.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000f5"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

// Run now → pipeline Orders → pipeline Order items (2 retries) → model Revenue
// (90-minute timeout) → wait 30 minutes → a message to #data. No query, no condition.
function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Nightly load",
    status: "active",
    definition: {
      version: 3,
      trigger: { type: "manual" },
      steps: [
        { id: "load_orders", type: "run_pipeline", pipeline_id: "pl-1" },
        { id: "load_items", type: "run_pipeline", after: ["load_orders"], rule: "all_success", pipeline_id: "pl-2", retries: 2 },
        { id: "revenue", type: "refresh_model", after: ["load_items"], rule: "all_success", model_id: "mo-1", timeout_minutes: 90 },
        { id: "settle", type: "wait", after: ["revenue"], rule: "all_success", minutes: 30 },
        {
          id: "send_1",
          type: "send",
          after: ["settle"],
          rule: "all_success",
          message: { subject: "Loaded", body: "Done at {{run_time}}" },
          required: true,
        },
      ],
    },
    trigger_description: "when someone clicks Run now",
    condition_description: "",
    branch_descriptions: [],
    connection: null,
    channels: [{ id: "ch1", name: "#data", kind: "slack", node_id: "send_1" }],
    step_refs: {
      load_orders: { id: "pl-1", name: "Orders" },
      load_items: { id: "pl-2", name: "Order items" },
      revenue: { id: "mo-1", name: "Revenue" },
    },
    proposed_by: "assistant",
    created_at: "2026-10-04T08:00:00Z",
    updated_at: "2026-10-04T08:00:00Z",
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function run(id: string, overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return {
    id,
    trigger: "manual",
    status: "succeeded",
    deliveries: [],
    notes: [],
    started_at: "2026-10-04T09:00:00Z",
    ...overrides,
  }
}

const at = "2026-10-04T09:00:00Z"

afterEach(() => {
  cleanup()
  mockFetch.mockReset()
})

describe("workflowSteps: pipeline, model and wait steps", () => {
  it("draws each as its own node, in the order they run", () => {
    expect(workflowSteps(workflow()).map((s) => [s.id, s.kind, s.title, s.subtitle])).toEqual([
      ["trigger", "trigger", "Run now", "When someone clicks Run now"],
      ["step:load_orders", "run_pipeline", "Orders", "Pipeline run"],
      ["step:load_items", "run_pipeline", "Order items", "Pipeline run, 2 retries"],
      ["step:revenue", "refresh_model", "Revenue", "Model refresh"],
      ["step:settle", "wait", "Wait", "30 minutes"],
      ["channel:send_1:ch1", "channel", "#data", "Slack"],
    ])
  })

  it("names a step whose pipeline or model was deleted", () => {
    const titles = workflowSteps(workflow({ step_refs: {} })).map((s) => s.title)
    expect(titles.slice(1, 4)).toEqual(["Deleted pipeline", "Deleted pipeline", "Deleted model"])
  })

  it("marks each node with its step's outcome in the last run", () => {
    const stopped = run("r1", {
      status: "stopped",
      finished_at: at,
      steps: [
        { step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "succeeded", started_at: at },
        { step_id: "load_items", type: "run_pipeline", attempt: 2, status: "succeeded", started_at: at },
        { step_id: "revenue", type: "refresh_model", attempt: 1, status: "warned", started_at: at },
        { step_id: "settle", type: "wait", attempt: 1, status: "stopped", started_at: at },
        { step_id: "send_1", type: "send", attempt: 1, status: "skipped", started_at: at },
      ],
    })
    const states = (r: WorkflowRun) => workflowSteps(workflow({ last_run: r })).map((s) => [s.id, s.state])
    expect(states(stopped)).toEqual([
      ["trigger", "ok"],
      ["step:load_orders", "ok"],
      ["step:load_items", "ok"],
      ["step:revenue", "warned"],
      ["step:settle", "stopped"],
      ["channel:send_1:ch1", "skipped"],
    ])

    // Its pipeline was busy with another run: the step waits, and shows as running.
    const waiting = run("r2", {
      status: "running",
      steps: [{ step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "waiting", started_at: at }],
    })
    expect(states(waiting).slice(1, 3)).toEqual([
      ["step:load_orders", "running"],
      ["step:load_items", undefined],
    ])

    // A channel's state is its own delivery's, matched on the send step.
    const failedSend = run("r3", {
      status: "failed",
      steps: [{ step_id: "send_1", type: "send", attempt: 1, status: "failed", started_at: at }],
      deliveries: [{ step_id: "send_1", channel_id: "ch1", name: "#data", kind: "slack", status: "failed", error: "gone" }],
    })
    expect(states(failedSend).at(-1)).toEqual(["channel:send_1:ch1", "failed"])
  })
})

describe("WorkflowStepPanel: a pipeline, model or wait step is read-only", () => {
  const panel = (selectedId: string, edit = false) => {
    const w = workflow()
    render(
      <WorkflowStepPanel
        workflow={w}
        steps={workflowSteps(w)}
        selectedId={selectedId}
        onSelect={() => {}}
        edit={
          edit
            ? {
                draft: { name: w.name, definition: w.definition, nodeChannelIds: { send_1: ["ch1"] } },
                onChange: () => {},
                options: { channels: [], connections: [], pipelines: [], loaded: true, error: null },
              }
            : undefined
        }
      />,
    )
    return screen.getByRole("complementary", { name: "Step settings" })
  }
  const dd = (aside: HTMLElement, term: string) => within(aside).getByText(term).nextElementSibling?.textContent

  it("shows a pipeline step's pipeline, timeout and retries", () => {
    const aside = panel("step:load_items")
    expect(within(aside).getByRole("heading", { name: "Order items" })).toBeInTheDocument()
    expect(
      within(aside).getByText(
        "Runs pipeline Order items after pipeline Orders succeeds and waits for it to finish; when the pipeline is " +
          "already running, it waits for that run, then starts its own. A failed run is started again, up to 2 times. " +
          "It may take up to 12 hours, retries included.",
      ),
    ).toBeInTheDocument()
    expect(within(aside).getByRole("link", { name: "Order items" })).toHaveAttribute("href", "/pipelines/pl-2")
    expect(dd(aside, "Timeout")).toBe("12 hours, retries included")
    expect(dd(aside, "Retries")).toBe("2")
  })

  it("shows a model step's model and its own timeout, and a wait step's minutes", () => {
    const model = panel("step:revenue")
    expect(within(model).getByRole("link", { name: "Revenue" })).toHaveAttribute("href", "/models/mo-1")
    expect(dd(model, "Timeout")).toBe("1 hour 30 minutes")
    expect(within(model).getByText(/a failing Warn check marks the step warned, a failing Block check fails it/)).toBeInTheDocument()
    cleanup()
    const wait = panel("step:settle")
    expect(dd(wait, "Waits")).toBe("30 minutes")
    expect(within(wait).getByText("Waits 30 minutes after model Revenue succeeds, then the steps after it start.")).toBeInTheDocument()
  })

  it("names a message that follows no condition in the step buttons", () => {
    const aside = panel("trigger")
    const nav = within(aside).getByRole("navigation", { name: "Steps" })
    expect(within(nav).getAllByRole("button").map((b) => b.textContent)).toEqual([
      "1. Run now",
      "2. Orders",
      "3. Order items",
      "4. Revenue",
      "5. Wait",
      "6. Message → #data",
    ])
  })

  it("has no fields in edit mode: only Ask rsync.ai changes it", () => {
    const aside = panel("step:load_orders", true)
    expect(within(aside).getByText("Only Ask rsync.ai changes this step.")).toBeInTheDocument()
    expect(within(aside).queryAllByRole("textbox")).toHaveLength(0)
    expect(within(aside).queryAllByRole("combobox")).toHaveLength(0)
    expect(within(aside).queryAllByRole("spinbutton")).toHaveLength(0)
    expect(within(aside).getByRole("link", { name: "Orders" })).toHaveAttribute("href", "/pipelines/pl-1")
  })
})

// The panel's Settings and Last run sections: what the canvas mark on a node stands
// for, in words, with when it ran, how long it took, what it started and its error.
describe("WorkflowStepPanel: the step in the last run", () => {
  const panel = (w: Workflow, selectedId: string, edit = false) => {
    render(
      <WorkflowStepPanel
        workflow={w}
        steps={workflowSteps(w)}
        selectedId={selectedId}
        onSelect={() => {}}
        edit={
          edit
            ? {
                draft: { name: w.name, definition: w.definition, nodeChannelIds: { send_1: ["ch1"] } },
                onChange: () => {},
                options: { channels: [], connections: [], pipelines: [], loaded: true, error: null },
              }
            : undefined
        }
      />,
    )
    return screen.getByRole("complementary", { name: "Step settings" })
  }
  const lastRun = (aside: HTMLElement) => within(aside).getByRole("region", { name: "Last run" })
  const dd = (el: HTMLElement, term: string) => within(el).getByText(term).nextElementSibling?.textContent

  const STOPPED = run("r1", {
    status: "stopped",
    started_at: "2026-10-04T09:00:00Z",
    finished_at: "2026-10-04T09:41:00Z",
    steps: [
      { step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "succeeded", execution_id: "ex-1", started_at: at, finished_at: "2026-10-04T09:12:30Z" },
      { step_id: "load_items", type: "run_pipeline", attempt: 2, status: "failed", execution_id: "ex-2", started_at: "2026-10-04T09:13:00Z", finished_at: "2026-10-04T09:20:00Z", error: "The pipeline run failed: sink unreachable" },
      { step_id: "revenue", type: "refresh_model", attempt: 1, status: "warned", model_id: "mo-1", model_run_id: "mr-1", started_at: at, note: "1 warn check failed" },
    ],
  })

  it("heads the settings and the last run, and says how a pipeline step went", () => {
    const aside = panel(workflow({ last_run: STOPPED }), "step:load_items")
    expect(within(aside).getByRole("region", { name: "Settings" })).toHaveTextContent("Order items")
    const section = lastRun(aside)
    expect(within(section).getByText("Failed")).toBeInTheDocument()
    expect(dd(section, "Started")).toBe(formatAbsoluteTime("2026-10-04T09:13:00Z"))
    expect(dd(section, "Took")).toBe("7m")
    expect(dd(section, "Attempt")).toBe("2")
    expect(within(section).getByText("The pipeline run failed: sink unreachable")).toBeInTheDocument()
    expect(within(section).getByRole("link", { name: "Open this pipeline run" })).toHaveAttribute("href", "/executions/ex-2")
  })

  it("links a model step to its model, with its warning and note", () => {
    const section = lastRun(panel(workflow({ last_run: STOPPED }), "step:revenue"))
    expect(within(section).getByText("Succeeded with a warning")).toBeInTheDocument()
    expect(within(section).getByText("1 warn check failed")).toBeInTheDocument()
    expect(within(section).getByRole("link", { name: "Open the model's runs" })).toHaveAttribute("href", "/models/mo-1")
    expect(within(section).queryByText("Attempt")).not.toBeInTheDocument()
  })

  it("says a step the run never reached was not reached, and one with no run yet was not run", () => {
    expect(lastRun(panel(workflow({ last_run: STOPPED }), "step:settle"))).toHaveTextContent("Not reached in the last run.")
    cleanup()
    expect(lastRun(panel(workflow(), "step:settle"))).toHaveTextContent("Not run yet.")
  })

  it("says a step missing from a run of an earlier version was not in that version, not that it was not reached", () => {
    const section = lastRun(panel(workflow({ last_run: { ...STOPPED, status: "succeeded", earlier_version: true } }), "step:settle"))
    expect(section).toHaveTextContent("Not in the last run, which used an earlier version of the workflow.")
    expect(section).not.toHaveTextContent("Not reached")
  })

  // "Started" read as a run still going on a run that had finished; the trigger fired.
  it("says the trigger fired and who started the run", () => {
    const section = lastRun(panel(workflow({ last_run: STOPPED }), "trigger"))
    expect(within(section).getByText("Triggered", { selector: "span" })).toBeInTheDocument()
    expect(within(section).queryByText("Started", { selector: "span" })).not.toBeInTheDocument()
    expect(dd(section, "Started by")).toBe("Run now")
  })

  it("keeps a draft's outcome green and says Run now ran it for real", () => {
    const section = lastRun(panel(workflow({ status: "draft", last_run: STOPPED }), "step:load_orders"))
    const pill = within(section).getByText("Succeeded")
    expect(pill).toHaveClass("bg-emerald-50")
    expect(pill).not.toHaveClass("bg-zinc-50")
    // A draft's run came from Run now and was real, so it must not be called a try.
    expect(within(section).getByText(draftRunNote(workflow()))).toBeInTheDocument()
    // It has no condition, so its message was always sent: no "if a condition was true".
    expect(section).toHaveTextContent("its steps ran and its message was sent.")
    expect(section).not.toHaveTextContent(/condition/)
    expect(section).not.toHaveTextContent(/a try/)
    cleanup()
    expect(within(lastRun(panel(workflow({ last_run: STOPPED }), "step:load_orders"))).getByText("Succeeded")).toHaveClass(
      "bg-emerald-50",
    )
  })

  it("is left out while the step is being edited", () => {
    const aside = panel(workflow({ last_run: STOPPED }), "step:load_items", true)
    expect(within(aside).getByRole("region", { name: "Settings" })).toBeInTheDocument()
    expect(within(aside).queryByRole("region", { name: "Last run" })).not.toBeInTheDocument()
  })

  // A run from before steps: the query, then each condition up to the one that sent.
  it("says which condition was true, which was not checked, and what the query read", () => {
    const branch = { condition: { type: "value" as const, column: "n", op: ">" as const, threshold: 1 }, message: { subject: "", body: "x" } }
    const w = workflow({
      definition: branchesDefinition({ query: { connection_id: "c1", sql: "SELECT 1 AS n" }, branches: [branch, branch] }),
      branch_descriptions: ["send when n > 1", "else send when n > 1"],
      connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
      channels: [caseChannel({ id: "ch1", name: "#ops", kind: "slack", branch: 0 }), caseChannel({ id: "ch2", name: "#all", kind: "slack", branch: 1 })],
      step_refs: {},
      last_run: run("r9", {
        condition_met: true,
        branch: 0,
        row_count: 1234,
        deliveries: [{ channel_id: "ch1", name: "#ops", kind: "slack", status: "sent" }],
      }),
    })
    expect(dd(lastRun(panel(w, "query")), "Rows")).toBe((1234).toLocaleString())
    cleanup()
    expect(within(lastRun(panel(w, "branch:0"))).getByText("True")).toBeInTheDocument()
    cleanup()
    expect(within(lastRun(panel(w, "branch:1"))).getByText("Not checked")).toBeInTheDocument()
    cleanup()
    expect(within(lastRun(panel(w, "channel:send_1:ch1"))).getByText("Sent")).toBeInTheDocument()
    cleanup()
    expect(within(lastRun(panel(w, "channel:send_2:ch2"))).getByText("Not sent")).toBeInTheDocument()
  })

  // A send step fails as a whole ("1 of 2 delivery channels failed.") when any of its
  // channels does; each channel node says how its own delivery went.
  it("gives each channel of a send its own delivery's error, not the step's", () => {
    const w = workflow({
      channels: [
        { id: "ch1", name: "#data", kind: "slack", node_id: "send_1" },
        { id: "ch2", name: "Finance", kind: "email", node_id: "send_1" },
      ],
      last_run: run("r2", {
        status: "failed",
        steps: [
          { step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "load_items", type: "run_pipeline", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "revenue", type: "refresh_model", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "settle", type: "wait", attempt: 1, status: "succeeded", started_at: at },
          { step_id: "send_1", type: "send", attempt: 1, status: "failed", started_at: at, error: "1 of 2 delivery channels failed." },
        ],
        deliveries: [
          { channel_id: "ch1", step_id: "send_1", name: "#data", kind: "slack", status: "sent" },
          { channel_id: "ch2", step_id: "send_1", name: "Finance", kind: "email", status: "failed", error: "SMTP 550 mailbox unavailable" },
        ],
      }),
    })
    const sent = lastRun(panel(w, "channel:send_1:ch1"))
    expect(within(sent).getByText("Sent")).toBeInTheDocument()
    expect(within(sent).queryByText("1 of 2 delivery channels failed.")).not.toBeInTheDocument()
    cleanup()
    const failed = lastRun(panel(w, "channel:send_1:ch2"))
    expect(within(failed).getByText("Not sent: failed")).toBeInTheDocument()
    expect(within(failed).getByText("SMTP 550 mailbox unavailable")).toBeInTheDocument()
    expect(within(failed).queryByText("1 of 2 delivery channels failed.")).not.toBeInTheDocument()
  })

  // A run from before steps names a delivery by its channel only. One channel on two
  // conditions: the delivery is the one condition's that sent, as the canvas marks it.
  it("gives a run from before steps' delivery to the condition that sent, not every one with its channel", () => {
    const branch = { condition: { type: "value" as const, column: "n", op: ">" as const, threshold: 1 }, message: { subject: "", body: "x" } }
    const w = workflow({
      definition: branchesDefinition({ query: { connection_id: "c1", sql: "SELECT 1 AS n" }, branches: [branch, branch] }),
      branch_descriptions: ["send when n > 1", "else send when n > 1"],
      connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
      channels: [caseChannel({ id: "ch1", name: "#ops", kind: "slack", branch: 0 }), caseChannel({ id: "ch1", name: "#ops", kind: "slack", branch: 1 })],
      step_refs: {},
      last_run: run("r9", {
        status: "failed",
        condition_met: true,
        branch: 1,
        row_count: 3,
        deliveries: [{ channel_id: "ch1", name: "#ops", kind: "slack", status: "failed", error: "Slack returned 500" }],
      }),
    })
    const notSent = lastRun(panel(w, "channel:send_1:ch1"))
    expect(within(notSent).getByText("Not sent")).toBeInTheDocument()
    expect(within(notSent).queryByText("Slack returned 500")).not.toBeInTheDocument()
    cleanup()
    const failed = lastRun(panel(w, "channel:send_2:ch1"))
    expect(within(failed).getByText("Not sent: failed")).toBeInTheDocument()
    expect(within(failed).getByText("Slack returned 500")).toBeInTheDocument()
  })
})

describe("WorkflowRuns: each step's outcome, and Stop", () => {
  const RUNNING = run("r3", {
    status: "running",
    started_at: "2026-10-04T10:00:00Z",
    steps: [
      { step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "succeeded", execution_id: "ex-1", pipeline_id: "pl-1", started_at: at },
      { step_id: "load_items", type: "run_pipeline", attempt: 2, status: "running", execution_id: "ex-2", pipeline_id: "pl-2", started_at: at },
    ],
  })
  const DONE = run("r2", {
    finished_at: "2026-10-04T09:40:00Z",
    steps: [
      { step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "succeeded", execution_id: "ex-0", started_at: at },
      { step_id: "load_items", type: "run_pipeline", attempt: 1, status: "succeeded", execution_id: "ex-00", started_at: at },
      { step_id: "revenue", type: "refresh_model", attempt: 1, status: "warned", model_id: "mo-1", model_run_id: "mr-1", started_at: at },
      { step_id: "settle", type: "wait", attempt: 1, status: "succeeded", started_at: at },
      { step_id: "send_1", type: "send", attempt: 1, status: "succeeded", started_at: at },
    ],
    deliveries: [{ step_id: "send_1", channel_id: "ch1", name: "#data", kind: "slack", status: "sent" }],
  })
  const STOPPED = run("r1", {
    status: "stopped",
    started_at: "2026-10-04T08:00:00Z",
    finished_at: "2026-10-04T08:05:00Z",
    steps: [{ step_id: "load_orders", type: "run_pipeline", attempt: 1, status: "stopped", execution_id: "ex-9", started_at: at }],
  })

  let runs: WorkflowRun[]
  let stopResponse: Response
  beforeEach(() => {
    runs = [RUNNING, DONE, STOPPED]
    stopResponse = json(202, { status: "stopping" })
    mockFetch.mockImplementation((_url: string, init?: RequestInit) =>
      Promise.resolve(init?.method === "POST" ? stopResponse : json(200, { runs })),
    )
  })

  const lines = (tr: HTMLElement) =>
    within(within(tr).getByRole("list", { name: "Steps" }))
      .getAllByRole("listitem")
      .map((li) => li.textContent)

  it("says a condition step's note once, under the step, not again for the run", async () => {
    const NOT_TRUE = "The condition was not true, so nothing was sent."
    runs = [
      run("r4", {
        finished_at: "2026-10-04T09:40:00Z",
        steps: [{ step_id: "check", type: "condition", attempt: 1, status: "succeeded", started_at: at, note: NOT_TRUE }],
        notes: [NOT_TRUE, "Only the run says this."],
      }),
    ]
    render(<WorkflowRuns workflow={workflow()} />)
    const row = (await screen.findByText(/^Check: succeeded/)).closest("tr") as HTMLElement
    expect(within(row).getAllByText(NOT_TRUE)).toHaveLength(1)
    expect(within(row).getByText("Only the run says this.")).toBeInTheDocument()
  })

  it("lists each step of a run with links to the pipeline run and the model", async () => {
    render(<WorkflowRuns workflow={workflow()} />)
    const done = (await screen.findByText("Model Revenue: succeeded with a warning", { exact: false })).closest("tr") as HTMLElement
    expect(lines(done)).toEqual([
      "Pipeline Orders: succeeded · Open the run",
      "Pipeline Order items: succeeded · Open the run",
      "Model Revenue: succeeded with a warning · Open the model",
      "The 30-minute wait: succeeded",
      "The message to #data: succeeded",
    ])
    expect(within(done).getAllByRole("link", { name: "Open the run" }).map((a) => a.getAttribute("href"))).toEqual([
      "/executions/ex-0",
      "/executions/ex-00",
    ])
    expect(within(done).getByRole("link", { name: "Open the model" })).toHaveAttribute("href", "/models/mo-1")
    expect(within(done).getByText("sent")).toBeInTheDocument()

    const running = (await screen.findByText(/Pipeline Order items: running/)).closest("tr") as HTMLElement
    expect(lines(running)).toEqual(["Pipeline Orders: succeeded · Open the run", "Pipeline Order items: running (attempt 2) · Open the run"])

    const stopped = (await screen.findByText(/Pipeline Orders: stopped/)).closest("tr") as HTMLElement
    expect(within(stopped).getByText("stopped", { selector: "div" })).toBeInTheDocument()
  })

  it("stops a running run, then shows it stopping", async () => {
    const onChanged = vi.fn()
    render(<WorkflowRuns workflow={workflow()} canManage onChanged={onChanged} />)
    const stop = await screen.findByRole("button", { name: "Stop" })
    // Only the running run has one.
    expect(screen.getAllByRole("button", { name: "Stop" })).toHaveLength(1)

    runs = [{ ...RUNNING, stop_requested: true }, DONE, STOPPED]
    await userEvent.click(stop)

    const post = mockFetch.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === "POST")
    expect(post?.[0]).toBe(API_ENDPOINTS.WORKFLOWS.STOP(WF, "r3"))
    expect(onChanged).toHaveBeenCalledTimes(1)
    expect(await screen.findByText("Stopping…")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Stop/ })).not.toBeInTheDocument()
  })

  it("says why a Stop was refused", async () => {
    stopResponse = json(409, { error: "not_running", message: "This run has already finished." })
    const onChanged = vi.fn()
    render(<WorkflowRuns workflow={workflow()} canManage onChanged={onChanged} />)
    await userEvent.click(await screen.findByRole("button", { name: "Stop" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("This run has already finished.")
    expect(onChanged).not.toHaveBeenCalled()
  })

  it("offers no Stop to someone who cannot manage the workflow", async () => {
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByText(/Pipeline Order items: running/)
    expect(screen.queryByRole("button", { name: "Stop" })).not.toBeInTheDocument()
  })
})

describe("runOutcome: a stopped run, and a run with no condition", () => {
  it("reads a stopped run as stopped, and one that ran its steps and sent nothing as succeeded", () => {
    expect(runOutcome({ status: "stopped", deliveries: [] })).toBe("stopped")
    expect(RUN_OUTCOME_LABEL.stopped).toBe("stopped")
    expect(runOutcome({ status: "succeeded", deliveries: [] })).toBe("succeeded")
    expect(RUN_OUTCOME_LABEL.succeeded).toBe("succeeded")
    // A condition that held but sent nothing is still the quiet period.
    expect(runOutcome({ status: "succeeded", condition_met: true, deliveries: [] })).toBe("held_back")
  })
})

describe("a workflow with no query", () => {
  it("tests to the message alone: no rows and no condition", () => {
    render(
      <WorkflowTestResult
        workflow={workflow()}
        test={{
          columns: [],
          rows: [],
          condition_met: false,
          messages: [{ channel: "#data", kind: "slack", subject: "Loaded", body: "Done at 09:00 UTC", step: "send_1" }],
          notes: [],
        }}
      />,
    )
    const result = screen.getByRole("region", { name: "Test result" })
    expect(result).not.toHaveTextContent("The query returned")
    expect(result).not.toHaveTextContent("condition")
    expect(result).toHaveTextContent("Nothing was sent.")
    const group = within(result).getByRole("group", { name: "Message step send_1" })
    expect(group).toHaveTextContent("Sent after the 30-minute wait succeeds")
    expect(group).toHaveTextContent("Done at 09:00 UTC")
  })

  it("draws its steps as icons in run order above the sentence, side-by-side steps as one stop", () => {
    const stops = () =>
      within(screen.getByRole("list", { name: "Steps in order" }))
        .getAllByRole("listitem")
        .map((li) => li.textContent)
    const { unmount } = render(<WorkflowSummary workflow={workflow()} />)
    expect(stops()).toEqual(["Run now", "Pipeline run", "Pipeline run", "Model refresh", "Wait", "Slack message"])
    unmount()

    // The model refresh runs after the first pipeline, beside the second.
    const w = workflow()
    const beside = {
      ...w,
      definition: {
        ...w.definition,
        steps: w.definition.steps.map((s) => (s.id === "revenue" ? { ...s, after: ["load_orders"] } : s)),
      },
    }
    render(<WorkflowSummary workflow={beside} />)
    expect(stops()).toEqual(["Run now", "Pipeline run", "Pipeline run and Model refresh", "Wait", "Slack message"])
  })

  it("lists its pipelines, model and wait in Details, with links", () => {
    render(<WorkflowDetails workflow={workflow()} />)
    const card = screen.getByTestId("workflow-details-card")
    expect(within(card).getByText("Steps")).toBeInTheDocument()
    expect(within(card).queryByText("Query")).not.toBeInTheDocument()
    expect(within(card).getByRole("link", { name: "Orders" })).toHaveAttribute("href", "/pipelines/pl-1")
    expect(within(card).getByRole("link", { name: "Order items" })).toHaveAttribute("href", "/pipelines/pl-2")
    expect(within(card).getByRole("link", { name: "Revenue" })).toHaveAttribute("href", "/models/mo-1")
    expect(within(card).getByText("Wait 30 minutes")).toBeInTheDocument()
  })
})

// Dashboards D3 PR 2: a chart step draws a saved chart into the message after
// it. Like a pipeline or model step it is read-only here, and links to its chart.
// "(whatever happened before it)" did not say what it meant: the step runs even when a
// step before it failed. Bug class: a rule worded so the reader cannot tell it from the
// default. Both places the rule is put into words are pinned.
describe("a step that runs even if a step before it failed", () => {
  function regardless(): Workflow {
    return workflow({
      definition: {
        version: 3,
        trigger: { type: "manual" },
        steps: [
          { id: "load_orders", type: "run_pipeline", pipeline_id: "pl-1" },
          { id: "load_items", type: "run_pipeline", pipeline_id: "pl-2" },
          { id: "settle", type: "wait", after: ["load_orders", "load_items"], rule: "all_done", minutes: 5 },
          { id: "revenue", type: "refresh_model", after: ["settle"], rule: "all_done", model_id: "mo-1" },
        ],
      },
      channels: [],
    })
  }

  it("says so in the summary and in when it runs", () => {
    const w = regardless()
    const steps = w.definition.steps
    expect(workflowSummary(w).lead).toContain("waits 5 minutes (even if a step before it failed)")
    expect(afterWords(w, steps[2])).toBe("after pipeline Orders and pipeline Order items all finish, even if any of them failed")
    expect(afterWords(w, steps[3])).toBe("after the 5-minute wait finishes, even if it failed")
    const all = [workflowSummary(w).lead, afterWords(w, steps[2]), afterWords(w, steps[3])].join(" ")
    expect(all).not.toMatch(/whatever happened/)
  })

  // The query's panel never said when it ran, so the summary's "even if a step before
  // it failed" had nothing beside it to agree with. Bug class: a step kind whose
  // sentence leaves out the when that every other kind's sentence says.
  it("says when its query runs, as every other step's panel does", () => {
    const w = workflow({
      definition: {
        version: 3,
        trigger: { type: "manual" },
        steps: [
          { id: "revenue", type: "refresh_model", model_id: "mo-1" },
          { id: "settle", type: "wait", after: ["revenue"], rule: "all_success", minutes: 10 },
          { id: "q", type: "query", after: ["settle"], rule: "all_done", connection_id: "c1", sql: "SELECT 1 AS n" },
        ],
      },
      connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
      channels: [],
    })
    const query = workflowSteps(w).find((s) => s.kind === "query")!
    expect(stepSentence(w, query)).toBe(
      "Runs one read-only query on Warehouse after the 10-minute wait finishes, even if it failed.",
    )
  })
})

describe("a chart step", () => {
  const CHART = "ffffffff-0000-4000-8000-0000000000c1"
  function charted(overrides: Partial<Workflow> = {}): Workflow {
    return workflow({
      definition: {
        version: 3,
        trigger: { type: "manual" },
        steps: [
          { id: "revenue", type: "refresh_model", model_id: "mo-1" },
          { id: "orders", type: "chart", after: ["revenue"], rule: "all_success", chart_id: CHART },
          { id: "send_1", type: "send", after: ["orders"], rule: "all_success", message: { subject: "Daily", body: "Today" } },
        ],
      },
      channels: [{ id: "ch-e", name: "Ops email", kind: "email", node_id: "send_1" }],
      step_refs: { revenue: { id: "mo-1", name: "Revenue" }, orders: { id: CHART, name: "Orders by day" } },
      ...overrides,
    })
  }

  it("draws a chart node between its step and the message, and names a deleted chart", () => {
    const w = charted()
    expect(workflowSteps(w).map((s) => [s.id, s.kind, s.title, s.subtitle])).toEqual([
      ["trigger", "trigger", "Run now", "When someone clicks Run now"],
      ["step:revenue", "refresh_model", "Revenue", "Model refresh"],
      ["step:orders", "chart", "Orders by day", "Chart in the message"],
      ["channel:send_1:ch-e", "channel", "Ops email", "Email"],
    ])
    expect(workflowGraph(w, { compact: false, interactive: false }).edges.map((e) => e.id)).toEqual([
      "trigger->step:revenue",
      "step:revenue->step:orders",
      "step:orders->channel:send_1:ch-e",
    ])
    expect(workflowSteps(charted({ step_refs: {} }))[2].title).toBe("Deleted chart")
  })

  it("says what it draws in the workflow's summary", () => {
    expect(workflowSummary(charted()).lead).toBe(
      // The message carries the drawing, so it reads as the next "then", not a sentence of its own.
      "When someone clicks Run now, rsync.ai refreshes model Revenue, then draws chart Orders by day, then sends it to Ops email.",
    )
  })

  it("is read-only in the step panel, with what it does and a link to its chart", () => {
    const w = charted()
    render(
      <WorkflowStepPanel
        workflow={w}
        steps={workflowSteps(w)}
        selectedId="step:orders"
        onSelect={() => {}}
        edit={{
          draft: { name: w.name, definition: w.definition, nodeChannelIds: { send_1: ["ch-e"] } },
          onChange: () => {},
          options: { channels: [], connections: [], pipelines: [], loaded: true, error: null },
        }}
      />,
    )
    const aside = screen.getByRole("complementary", { name: "Step settings" })
    expect(
      within(aside).getByText(
        "Draws chart Orders by day after model Revenue succeeds, as its page shows it to the person the run acts " +
          "as, for the message after it: email shows a bar, line, area or heatmap chart as a picture and a scorecard " +
          "or table as its numbers; Slack gets its numbers and a link to the chart.",
      ),
    ).toBeInTheDocument()
    expect(within(aside).getByText("Only Ask rsync.ai changes this step.")).toBeInTheDocument()
    expect(within(aside).queryAllByRole("textbox")).toHaveLength(0)
    expect(within(aside).getByRole("link", { name: "Orders by day" })).toHaveAttribute("href", `/charts/${CHART}`)
  })

  it("is listed in Details with a link to its chart", () => {
    render(<WorkflowDetails workflow={charted()} />)
    const card = screen.getByTestId("workflow-details-card")
    expect(within(card).getByText(/^Draw chart/)).toBeInTheDocument()
    expect(within(card).getByRole("link", { name: "Orders by day" })).toHaveAttribute("href", `/charts/${CHART}`)
  })
})

// Dashboards D3 PR 3: a dashboard step draws every tile of a saved dashboard into
// the message after it, with the board as a PDF on an email. Read-only here like a
// chart step, and links to its dashboard.
describe("a dashboard step", () => {
  const BOARD = "ffffffff-0000-4000-8000-0000000000d1"
  function boarded(overrides: Partial<Workflow> = {}): Workflow {
    return workflow({
      definition: {
        version: 3,
        trigger: { type: "manual" },
        steps: [
          { id: "revenue", type: "refresh_model", model_id: "mo-1" },
          { id: "ops", type: "dashboard", after: ["revenue"], rule: "all_success", dashboard_id: BOARD },
          { id: "send_1", type: "send", after: ["ops"], rule: "all_success", message: { subject: "Weekly", body: "This week" } },
        ],
      },
      channels: [{ id: "ch-e", name: "Ops email", kind: "email", node_id: "send_1" }],
      step_refs: { revenue: { id: "mo-1", name: "Revenue" }, ops: { id: BOARD, name: "Ops review" } },
      ...overrides,
    })
  }

  it("draws a dashboard node between its step and the message, and names a deleted dashboard", () => {
    const w = boarded()
    expect(workflowSteps(w).map((s) => [s.id, s.kind, s.title, s.subtitle])).toEqual([
      ["trigger", "trigger", "Run now", "When someone clicks Run now"],
      ["step:revenue", "refresh_model", "Revenue", "Model refresh"],
      ["step:ops", "dashboard", "Ops review", "Dashboard in the message"],
      ["channel:send_1:ch-e", "channel", "Ops email", "Email"],
    ])
    expect(workflowGraph(w, { compact: false, interactive: false }).edges.map((e) => e.id)).toEqual([
      "trigger->step:revenue",
      "step:revenue->step:ops",
      "step:ops->channel:send_1:ch-e",
    ])
    expect(workflowSteps(boarded({ step_refs: {} }))[2].title).toBe("Deleted dashboard")
  })

  it("says what it draws in the workflow's summary", () => {
    expect(workflowSummary(boarded()).lead).toBe(
      "When someone clicks Run now, rsync.ai refreshes model Revenue, then draws dashboard Ops review, then sends it to Ops email.",
    )
  })

  it("is read-only in the step panel, with what it does and a link to its dashboard", () => {
    const w = boarded()
    render(
      <WorkflowStepPanel
        workflow={w}
        steps={workflowSteps(w)}
        selectedId="step:ops"
        onSelect={() => {}}
        edit={{
          draft: { name: w.name, definition: w.definition, nodeChannelIds: { send_1: ["ch-e"] } },
          onChange: () => {},
          options: { channels: [], connections: [], pipelines: [], loaded: true, error: null },
        }}
      />,
    )
    const aside = screen.getByRole("complementary", { name: "Step settings" })
    expect(
      within(aside).getByText(
        "Draws every tile of dashboard Ops review after model Revenue succeeds, as its page shows it to the person " +
          "the run acts as, with no filters picked, for the message after it: email shows the tiles in the board's " +
          "order, each chart as a picture and each scorecard or table as its numbers, and attaches the dashboard as " +
          "a PDF; Slack gets its numbers and a link to the dashboard. A tile that cannot be drawn says so and the " +
          "rest still go.",
      ),
    ).toBeInTheDocument()
    expect(within(aside).getByText("Only Ask rsync.ai changes this step.")).toBeInTheDocument()
    expect(within(aside).queryAllByRole("textbox")).toHaveLength(0)
    expect(within(aside).getByRole("link", { name: "Ops review" })).toHaveAttribute("href", `/dashboards/${BOARD}`)
  })

  it("is listed in Details with a link to its dashboard", () => {
    render(<WorkflowDetails workflow={boarded()} />)
    const card = screen.getByTestId("workflow-details-card")
    expect(within(card).getByText(/^Draw dashboard/)).toBeInTheDocument()
    expect(within(card).getByRole("link", { name: "Ops review" })).toHaveAttribute("href", `/dashboards/${BOARD}`)
  })
})
