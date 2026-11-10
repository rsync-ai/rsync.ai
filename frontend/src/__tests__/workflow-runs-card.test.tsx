import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { formatAbsoluteTime } from "@/lib/utils"
import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"
import {
  PANEL_WIDTH_DEFAULT,
  PANEL_WIDTH_MAX,
  PANEL_WIDTH_MIN,
  PANEL_WIDTH_STORAGE_KEY,
} from "@/components/workflows/WorkflowPanelResizeHandle"
import { WorkflowRuns } from "@/components/workflows/WorkflowRuns"
import { RUN_OUTCOME_LABEL, RunOutcomeBadge, WORKFLOW_RUN_KINDS, runOutcome } from "@/components/workflows/runOutcome"
import type { RunOutcome } from "@/components/workflows/runOutcome"
import { authFetch } from "@/lib/api/auth-fetch"
import { PAGE_MIN_WIDTH } from "@/lib/assistant/dock-width"
import type { Workflow, WorkflowRun } from "@/lib/api/workflows"
import { API_ENDPOINTS } from "@/lib/config/api"
import { branchesDefinition, caseChannel } from "./workflowFixtures"

// A workflow's Runs tab reads the way a scheduled query's does: a duration chart,
// All/Failed/Sent/Not sent filters, a Started/Duration/Trigger/Status/Result table,
// and a Details card beside it. Its step settings beside the canvas can be widened
// to read a long query.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
// ECharts draws on a canvas jsdom does not have: the run chart stands in as a button
// per bar, holding its row, so a test reads what each bar is and how tall.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({
    settings,
    rows,
    onCategoryClick,
  }: {
    settings: { description: string; x: { categories: string[] }; y: { title?: string } }
    rows: { kind: string; value: number }[]
    onCategoryClick?: (i: number) => void
  }) => (
    <div role="img" aria-label={settings.description} data-unit={settings.y.title}>
      {settings.x.categories.map((c, i) => (
        <button
          key={c}
          type="button"
          data-kind={rows[i].kind}
          data-value={rows[i].value}
          onClick={() => onCategoryClick?.(i)}
        >
          {c}
        </button>
      ))}
    </div>
  ),
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))
// xyflow needs a layout engine jsdom lacks; the step panel carries the same steps.
vi.mock("@/components/workflows/WorkflowCanvas", async (orig) => {
  const actual = await orig<typeof import("@/components/workflows/WorkflowCanvas")>()
  return { ...actual, WorkflowCanvas: () => null }
})

const mockFetch = authFetch as unknown as Mock
const WF = "ffffffff-0000-4000-8000-0000000000e2"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WF,
    name: "Failed payments",
    status: "active",
    definition: branchesDefinition({
      trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
      query: { connection_id: "c1", sql: "SELECT count(*) AS failed FROM payments" },
      branches: [
        {
          condition: { type: "value", op: ">", threshold: 50 },
          message: { subject: "Failed payments", body: "{{failed}} failed in the last hour" },
        },
      ],
      quiet_minutes: 60,
    }),
    trigger_description: "every hour on the hour (UTC)",
    condition_description: "send when the first column is above 50",
    branch_descriptions: ["send when the first column is above 50"],
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: [
      caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 0 }),
      caseChannel({ id: "ch2", name: "Finance list", kind: "email", branch: 0 }),
    ],
    proposed_by: "assistant",
    created_at: "2026-10-01T08:00:00Z",
    updated_at: "2026-10-02T09:30:00Z",
    url: `https://app.example/workflows/${WF}`,
    ...overrides,
  }
}

function run(id: string, startedAt: string, overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return { id, trigger: "schedule", status: "succeeded", deliveries: [], notes: [], started_at: startedAt, ...overrides }
}

// Newest first, the order GET /workflows/:id/runs sends them in.
const RUNNING = run("r5", "2026-10-03T05:00:00Z", { status: "running", trigger: "manual" })
const QUIET = run("r4", "2026-10-03T04:00:00Z", {
  finished_at: "2026-10-03T04:00:01Z",
  condition_met: true,
  row_count: 1,
  notes: ["The condition held, but a message was sent less than 60 minutes ago, so this one was not."],
})
const NOTHING = run("r3", "2026-10-03T03:00:00Z", {
  finished_at: "2026-10-03T03:00:00.250Z",
  condition_met: false,
  row_count: 1,
  notes: ["The condition did not hold, so nothing was sent."],
})
const FAILED = run("r2", "2026-10-03T02:00:00Z", {
  status: "failed",
  finished_at: "2026-10-03T02:00:30Z",
  condition_met: true,
  row_count: 1,
  error: "1 of 2 delivery channels failed.",
  deliveries: [
    { channel_id: "ch1", name: "#payments", kind: "slack", status: "sent" },
    { channel_id: "ch2", name: "Finance list", kind: "email", status: "failed", error: "mailbox full" },
  ],
})
const SENT = run("r1", "2026-10-03T01:00:00Z", {
  trigger: "after_pipeline",
  finished_at: "2026-10-03T01:00:02Z",
  condition_met: true,
  row_count: 3,
  deliveries: [{ channel_id: "ch1", name: "#payments", kind: "slack", status: "sent" }],
})
const RUNS = [RUNNING, QUIET, NOTHING, FAILED, SENT]

// Two branches: more than 500 failed goes to Finance, otherwise more than 50 to #payments.
const TWO_BRANCHES: Partial<Workflow> = {
  definition: branchesDefinition({
    trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
    query: { connection_id: "c1", sql: "SELECT count(*) AS failed FROM payments" },
    branches: [
      {
        condition: { type: "value", op: ">", threshold: 500 },
        message: { subject: "Payments down", body: "{{failed}} failed" },
      },
      {
        condition: { type: "value", op: ">", threshold: 50 },
        message: { subject: "Failed payments", body: "{{failed}} failed in the last hour" },
      },
    ],
    quiet_minutes: 60,
  }),
  condition_description: "send when the first column is above 500; else send when the first column is above 50",
  branch_descriptions: ["send when the first column is above 500", "send when the first column is above 50"],
  channels: [
    caseChannel({ id: "ch2", name: "Finance list", kind: "email", branch: 0 }),
    caseChannel({ id: "ch1", name: "#payments", kind: "slack", branch: 1 }),
  ],
}

function routeRuns(runs: WorkflowRun[], wf: Workflow = workflow()) {
  mockFetch.mockImplementation((url: string) => {
    if (url === API_ENDPOINTS.WORKFLOWS.GET(WF)) {
      return Promise.resolve(json(200, { workflow: wf, can_manage: true }))
    }
    if (url === API_ENDPOINTS.WORKFLOWS.RUNS(WF)) return Promise.resolve(json(200, { runs }))
    return Promise.resolve(json(404, {}))
  })
}

function runChart() {
  return screen.getByRole("img", { name: /How long each run on this page took/ })
}

function bars() {
  return within(runChart()).getAllByRole("button")
}

function rows() {
  // The first row is the header.
  return within(screen.getByRole("table")).getAllByRole("row").slice(1)
}

beforeEach(() => {
  mockFetch.mockReset()
  window.localStorage.clear()
})
afterEach(cleanup)

describe("runOutcome", () => {
  it("says what each run did, as the server decides it", () => {
    expect(RUNS.map((r) => RUN_OUTCOME_LABEL[runOutcome(r)])).toEqual([
      "running",
      "not sent (quiet period)",
      "nothing to send",
      "failed",
      "sent",
    ])
  })

  // A run whose steps all ran read indigo while one that sent read green, so a
  // workflow with no send step never showed a green run.
  it("colours every run that finished well green, on the badge and on the chart", () => {
    const tone = (o: RunOutcome) => {
      render(<RunOutcomeBadge outcome={o} />)
      const cls = screen.getByText(RUN_OUTCOME_LABEL[o]).className
      cleanup()
      return cls
    }
    for (const good of ["sent", "succeeded"] as const) {
      expect(tone(good)).toMatch(/\btext-emerald-600\b/)
      expect(WORKFLOW_RUN_KINDS.find((k) => k.status === good)?.color).toBe("succeeded")
    }
    expect(tone("failed")).toMatch(/\btext-red-600\b/)
  })
})

describe("WorkflowRuns", () => {
  it("charts the finished runs by outcome, oldest first, and tables every run", async () => {
    routeRuns(RUNS)
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByRole("table")

    // A run still going has no duration yet: it is in the table, not the chart.
    expect(bars().map((b) => [b.dataset.kind, Number(b.dataset.value)])).toEqual([
      ["sent", 2],
      ["failed", 30],
      ["nothing_to_send", 0.25],
      ["held_back", 1],
    ])
    expect(runChart()).toHaveAttribute("data-unit", "Seconds")
    expect(screen.getByText(/longest 30s/)).toBeInTheDocument()

    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Started",
      "Duration",
      "Trigger",
      "Status",
      "Result",
    ])
    const [running, quiet, nothing, failed, sent] = rows()
    expect(within(running).getByText("Run now")).toBeInTheDocument()
    expect(within(running).getByText("running")).toBeInTheDocument()
    expect(within(nothing).getByText("250ms")).toBeInTheDocument()
    expect(within(nothing).getByText("nothing to send")).toBeInTheDocument()
    expect(within(nothing).getByText("The condition did not hold, so nothing was sent.")).toBeInTheDocument()
    expect(within(quiet).getByText("not sent (quiet period)")).toBeInTheDocument()
    expect(within(failed).getByText("1 of 2 delivery channels failed.")).toBeInTheDocument()
    expect(within(failed).getByText("Finance list: failed (mailbox full)")).toBeInTheDocument()
    expect(within(sent).getByText("After pipeline")).toBeInTheDocument()
    expect(within(sent).getByText("#payments: sent")).toBeInTheDocument()
    expect(within(sent).getByText("3 rows")).toBeInTheDocument()
  })

  it("finds a clicked bar's run in the table", async () => {
    const user = userEvent.setup()
    routeRuns(RUNS)
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByRole("table")

    await user.click(bars()[1])
    const [running, quiet, nothing, failed, sent] = rows()
    expect(failed).toHaveAttribute("aria-current", "true")
    for (const r of [running, quiet, nothing, sent]) expect(r).not.toHaveAttribute("aria-current")
  })

  it("filters the chart and the table together", async () => {
    const user = userEvent.setup()
    routeRuns(RUNS)
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByRole("table")
    const filters = screen.getByRole("group", { name: "Filter runs by outcome" })
    expect(within(filters).getAllByRole("button").map((b) => b.textContent)).toEqual(["All", "Failed", "Sent", "Not sent"])
    expect(within(filters).getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true")

    await user.click(within(filters).getByRole("button", { name: "Failed" }))
    expect(within(filters).getByRole("button", { name: "Failed" })).toHaveAttribute("aria-pressed", "true")
    expect(rows()).toHaveLength(1)
    expect(bars()).toHaveLength(1)
    expect(within(rows()[0]).getByText("failed")).toBeInTheDocument()

    // Not sent: the condition did not hold, or a quiet period held the message back.
    await user.click(within(filters).getByRole("button", { name: "Not sent" }))
    expect(rows().map((r) => within(r).getAllByRole("cell")[3].textContent)).toEqual([
      "not sent (quiet period)",
      "nothing to send",
    ])

    await user.click(within(filters).getByRole("button", { name: "Sent" }))
    expect(rows()).toHaveLength(1)
    expect(within(rows()[0]).getByText("#payments: sent")).toBeInTheDocument()
  })

  it("says so when a filter matches no run", async () => {
    const user = userEvent.setup()
    routeRuns([SENT])
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByRole("table")
    await user.click(screen.getByRole("button", { name: "Failed" }))
    expect(screen.queryByRole("table")).not.toBeInTheDocument()
    expect(screen.getByText("No failed runs.")).toBeInTheDocument()
  })

  it("says how to get a first run", async () => {
    routeRuns([])
    render(<WorkflowRuns workflow={workflow()} />)
    expect(await screen.findByText("No runs yet. Test it, then turn it on or click Run now.")).toBeInTheDocument()
    expect(screen.queryByTestId("run-health-kpis")).not.toBeInTheDocument()
  })

  // A run that finished without failing did its work, sent or not; one still going is
  // not counted. The cards read every run on the page, whatever the filter shows.
  it("cards how the runs have been doing, whatever the filter", async () => {
    const user = userEvent.setup()
    routeRuns(RUNS)
    render(<WorkflowRuns workflow={workflow()} />)
    await screen.findByRole("table")
    const card = (name: string) => within(screen.getByRole("region", { name }))
    const value = (name: string) => card(name).getAllByRole("paragraph")[0].textContent

    expect(value("Success rate")).toBe("75%")
    expect(card("Success rate").getByText("3 of 4 succeeded, last 5 runs (1 running: not counted)")).toBeInTheDocument()
    // 250ms, 1s, 2s and 30s.
    expect(value("Median run time")).toBe("1.5s")
    expect(card("Last failure").getByText(formatAbsoluteTime(FAILED.started_at))).toBeInTheDocument()

    await user.click(screen.getByRole("button", { name: "Sent" }))
    expect(value("Success rate")).toBe("75%")
  })
})

describe("WorkflowEditor's Runs tab", () => {
  it("puts a Details card beside the runs", async () => {
    const user = userEvent.setup()
    routeRuns([SENT])
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByRole("heading", { name: "Failed payments" })
    await user.click(screen.getByRole("tab", { name: "Runs" }))
    await screen.findByRole("table")

    const details = screen.getByTestId("workflow-details-card")
    const value = (term: string) =>
      within(details).getByText(term, { selector: "dt" }).nextElementSibling?.textContent
    expect(value("Trigger")).toBe("every hour on the hour (UTC)")
    expect(value("Query")).toBe("Warehouse (postgresql)")
    expect(value("Condition")).toBe("send when the first column is above 50")
    // 360 read as "360 minutes"; it is said in hours, as the wait step is.
    expect(value("Quiet period")).toBe("1 hour after each send")
    expect(value("Sends to")).toBe("#payments (Slack)Finance list (email)")
    expect(value("Edited")).toBe(formatAbsoluteTime("2026-10-02T09:30:00Z"))
  })

  it("names each condition, whose each channel is, and the condition a run sent", async () => {
    const user = userEvent.setup()
    routeRuns([{ ...SENT, branch: 1 }, NOTHING], workflow(TWO_BRANCHES))
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByRole("heading", { name: "Failed payments" })
    await user.click(screen.getByRole("tab", { name: "Runs" }))
    await screen.findByRole("table")

    const details = screen.getByTestId("workflow-details-card")
    const value = (term: string) =>
      within(details).queryByText(term, { selector: "dt" })?.nextElementSibling?.textContent
    expect(value("Condition")).toBeUndefined()
    expect(value("Conditions")).toBe(
      "Condition 1: send when the first column is above 500Condition 2: send when the first column is above 50",
    )
    expect(value("Sends to")).toBe("Condition 1: Finance list (email)Condition 2: #payments (Slack)")

    // A run that sent says which condition was true; one where none was names none.
    const [sent, nothing] = rows()
    expect(within(sent).getByText("Condition 2")).toBeInTheDocument()
    expect(within(nothing).queryByText(/^Condition /)).not.toBeInTheDocument()
  })
})

describe("the step settings' width", () => {
  async function editor() {
    routeRuns([])
    render(<WorkflowEditor workflowId={WF} />)
    await screen.findByRole("heading", { name: "Failed payments" })
  }

  async function separator() {
    await editor()
    return screen.getByRole("separator", { name: "Resize step settings" })
  }

  function gridWidth() {
    return screen.getByTestId("workflow-editor-grid").style.getPropertyValue("--workflow-panel-width")
  }

  it("starts at the default and controls the step settings", async () => {
    const handle = await separator()
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_DEFAULT))
    expect(handle).toHaveAttribute("aria-valuemin", String(PANEL_WIDTH_MIN))
    expect(gridWidth()).toBe(`${PANEL_WIDTH_DEFAULT}px`)
    const panel = document.getElementById(handle.getAttribute("aria-controls") ?? "")
    expect(panel).toContainElement(screen.getByRole("complementary", { name: "Step settings" }))
  })

  it("widens with ArrowLeft, keeps the width, and resets on a double-click", async () => {
    const handle = await separator()
    fireEvent.keyDown(handle, { key: "ArrowLeft" })
    fireEvent.keyDown(handle, { key: "ArrowLeft" })
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_DEFAULT + 32))
    expect(gridWidth()).toBe(`${PANEL_WIDTH_DEFAULT + 32}px`)
    expect(window.localStorage.getItem(PANEL_WIDTH_STORAGE_KEY)).toBe(String(PANEL_WIDTH_DEFAULT + 32))

    fireEvent.keyDown(handle, { key: "ArrowRight" })
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_DEFAULT + 16))

    fireEvent.doubleClick(handle)
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_DEFAULT))
    expect(window.localStorage.getItem(PANEL_WIDTH_STORAGE_KEY)).toBeNull()
  })

  it("stops at its narrowest and widest", async () => {
    const handle = await separator()
    fireEvent.keyDown(handle, { key: "Home" })
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_MIN))
    fireEvent.keyDown(handle, { key: "ArrowRight" })
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_MIN))
    // jsdom lays nothing out, so the grid measures 0 and the cap is the fixed one.
    fireEvent.keyDown(handle, { key: "End" })
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_MAX))
  })

  it("never squeezes the canvas under the room the docked Ask rsync.ai panel leaves a page", async () => {
    const handle = await separator()
    const grid = screen.getByTestId("workflow-editor-grid")
    // A 1000px grid: past the canvas's 480px and the 12px gap, the panel may take 508px.
    Object.defineProperty(grid, "clientWidth", { configurable: true, value: 1000 })
    fireEvent.keyDown(handle, { key: "End" })
    expect(handle).toHaveAttribute("aria-valuenow", String(1000 - PAGE_MIN_WIDTH - 12))
    expect(handle).toHaveAttribute("aria-valuemax", String(1000 - PAGE_MIN_WIDTH - 12))
    // CSS holds the same line while the window resizes: the panel's column
    // reads the canvas's room from a variable, not a number of its own.
    expect(grid.style.getPropertyValue("--workflow-canvas-reserve")).toBe(`${PAGE_MIN_WIDTH + 12}px`)
    expect(grid.className).toContain("calc(100%_-_var(--workflow-canvas-reserve))")
  })

  it("comes back at the width this browser kept", async () => {
    window.localStorage.setItem(PANEL_WIDTH_STORAGE_KEY, "520")
    const handle = await separator()
    expect(handle).toHaveAttribute("aria-valuenow", "520")
    expect(gridWidth()).toBe("520px")
  })

  it("ignores a kept width it cannot use", async () => {
    window.localStorage.setItem(PANEL_WIDTH_STORAGE_KEY, "not a number")
    const handle = await separator()
    expect(handle).toHaveAttribute("aria-valuenow", String(PANEL_WIDTH_DEFAULT))
  })

  it("shows the whole query, not a scroll box capped at 16rem", async () => {
    await editor()
    fireEvent.click(screen.getByRole("button", { name: "2. Query" }))
    expect(screen.getByLabelText("SQL").className).not.toMatch(/\bmax-h-/)
  })
})
