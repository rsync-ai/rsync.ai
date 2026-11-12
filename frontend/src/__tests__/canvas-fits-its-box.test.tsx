import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"

import { DAGVisualizationV2 } from "@/components/pipeline/DAGVisualizationV2"
import type { ExecutionPlanStage } from "@/components/pipeline/dagTypes"
import { WorkflowCanvas } from "@/components/workflows/WorkflowCanvas"
import { describeBranch, describeBranches } from "@/components/workflows/useWorkflowEdit"
import type { Workflow } from "@/lib/api/workflows"
import { branchesDefinition, caseChannel, type FixtureBranch } from "./workflowFixtures"

// A graph canvas fits its nodes to its box whenever the box changes, not only
// when it first lays out. React Flow's `fitView` prop fits once, so dragging
// the step settings wider (or opening the docked Ask rsync.ai panel) narrowed
// the workflow canvas and left its branches and channels cut off past the new
// edge. Pinned here, on the real canvas and the real React Flow store:
//   - it fits on its first layout, and again each time the box resizes;
//   - it fits again when the steps change, not when a step is selected;
//   - Fit to view frames the steps as the canvas does, not at React Flow's
//     default (which zooms a small workflow to twice its size);
//   - a pipeline's stage graph, the other canvas that shows a whole graph,
//     fits again the same way (the docked panel narrows it). The lineage
//     canvas is left out: it keeps the user's place on purpose;
//   - a workflow too long to read when fitted whole opens at a readable size
//     from its trigger, not shrunk to fit (only the editor's canvas, which
//     can be dragged).
// Only the fit and the view it leaves are spies: jsdom has no layout to fit into.

const fitView = vi.fn(() => Promise.resolve(true))
/** Where the last fit left the view, and the steps' bounds it framed. */
const view = { zoom: 1 }
const bounds = { x: 0, y: -30, width: 1020, height: 310 }
const setViewport = vi.fn(() => Promise.resolve(true))
const flow = {
  fitView,
  zoomIn: vi.fn(),
  zoomOut: vi.fn(),
  getViewport: () => ({ x: 0, y: 0, ...view }),
  getNodes: () => [],
  getNodesBounds: () => bounds,
  setViewport,
}
vi.mock("@xyflow/react", async (orig) => {
  const actual = await orig<typeof import("@xyflow/react")>()
  return { ...actual, useReactFlow: () => flow }
})

/** The canvas's box. React Flow reads its renderer's offset size on mount and on a window resize. */
const pane = { width: 900, height: 420 }
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
      return this.classList.contains("react-flow__renderer") ? pane.width : 0
    },
  })
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return this.classList.contains("react-flow__renderer") ? pane.height : 0
    },
  })
})

afterAll(() => {
  vi.unstubAllGlobals()
  if (offsetWidth) Object.defineProperty(HTMLElement.prototype, "offsetWidth", offsetWidth)
  if (offsetHeight) Object.defineProperty(HTMLElement.prototype, "offsetHeight", offsetHeight)
})

beforeEach(() => {
  fitView.mockClear()
  setViewport.mockClear()
  view.zoom = 1
  pane.width = 900
  pane.height = 420
})

const BRANCHES: FixtureBranch[] = [
  { condition: { type: "value", column: "users", op: ">", threshold: 10 }, message: { subject: "Users", body: "{{users}}" } },
  { condition: { type: "always", threshold: 0 }, message: { subject: "", body: "fine" } },
]

function workflow(branches = BRANCHES): Workflow {
  return {
    id: "ffffffff-0000-4000-8000-0000000000f1",
    name: "Users count",
    status: "draft",
    definition: branchesDefinition({
      trigger: { type: "schedule", cron: "0 * * * *", timezone: "UTC" },
      query: { connection_id: "c1", sql: "SELECT count(*) AS users FROM users" },
      branches,
    }),
    trigger_description: "every hour on the hour (UTC)",
    condition_description: describeBranches(branches.map((b) => b.condition)),
    branch_descriptions: branches.map((b, i) => describeBranch(b.condition, i, branches.length)),
    connection: { id: "c1", name: "Warehouse", connector_type: "postgresql" },
    channels: branches.map((_, i) => caseChannel({ id: "ch1", name: "Ops list", kind: "email", branch: i })),
    proposed_by: "assistant",
    created_at: "2026-10-03T08:00:00Z",
    updated_at: "2026-10-03T08:00:00Z",
    url: "https://app.example/workflows/f1",
  }
}

const FIT = { padding: 0.1, maxZoom: 1.5 }

function resize(width: number) {
  pane.width = width
  act(() => {
    window.dispatchEvent(new Event("resize"))
  })
}

describe("the workflow canvas fits its box", () => {
  it("fits on its first layout, and again each time the box gets narrower or wider", () => {
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    expect(fitView).toHaveBeenCalledWith(FIT)

    // The step settings dragged wider: the canvas is down to its 480px floor.
    fitView.mockClear()
    resize(480)
    expect(fitView).toHaveBeenCalledTimes(1)
    expect(fitView).toHaveBeenCalledWith(FIT)

    fitView.mockClear()
    resize(1100)
    expect(fitView).toHaveBeenCalledTimes(1)
  })

  it("does not fit again when nothing about the box changed", () => {
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    fitView.mockClear()
    resize(900)
    expect(fitView).not.toHaveBeenCalled()
  })

  it("fits again when the steps change, but not when a step is selected", () => {
    const onSelect = () => {}
    const { rerender } = render(<WorkflowCanvas workflow={workflow()} selectedId={null} onSelect={onSelect} />)
    fitView.mockClear()

    rerender(<WorkflowCanvas workflow={workflow()} selectedId="query" onSelect={onSelect} />)
    expect(fitView).not.toHaveBeenCalled()

    // Ask rsync.ai removed a condition: a different graph to frame.
    rerender(<WorkflowCanvas workflow={workflow(BRANCHES.slice(0, 1))} selectedId="query" onSelect={onSelect} />)
    expect(fitView).toHaveBeenCalledTimes(1)
  })

  it("frames the chat card's compact canvas tighter, and fits it again too", () => {
    render(<WorkflowCanvas workflow={workflow()} compact />)
    expect(fitView).toHaveBeenCalledWith({ padding: 0.15, maxZoom: 1 })
    fitView.mockClear()
    resize(360)
    expect(fitView).toHaveBeenCalledWith({ padding: 0.15, maxZoom: 1 })
  })

  it("Fit to view frames the steps as the canvas does when it opens", () => {
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    fitView.mockClear()
    fireEvent.click(screen.getByRole("button", { name: "Fit to view" }))
    expect(fitView).toHaveBeenCalledWith(FIT)
  })
})

describe("a workflow too long to read when fitted whole", () => {
  /** Lets the fit's promise settle, and the view it leaves be read. */
  const settle = () => act(async () => {})

  it("opens at a readable size from its trigger, centred top to bottom, not shrunk to fit", async () => {
    // Fitted whole into 900px, the 1020px of steps came out at 0.45: labels too small to read.
    view.zoom = 0.45
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    await settle()
    expect(fitView).toHaveBeenCalledWith(FIT)
    // React Flow's own margin for a 0.1 fit is 40px of 900 and 19px of 420; the
    // 217px of steps at 0.7 sit in the middle of the 420px box.
    expect(setViewport).toHaveBeenLastCalledWith({ x: 40, y: (420 - 310 * 0.7) / 2 + 30 * 0.7, zoom: 0.7 })
  })

  it("starts at the top when the steps are taller than the box, too", async () => {
    view.zoom = 0.3
    bounds.height = 900
    try {
      render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
      await settle()
      expect(setViewport).toHaveBeenLastCalledWith({ x: 40, y: 19 + 30 * 0.7, zoom: 0.7 })
    } finally {
      bounds.height = 310
    }
  })

  it("is fitted whole as before when it reads at that size", async () => {
    view.zoom = 0.9
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    await settle()
    expect(fitView).toHaveBeenCalledWith(FIT)
    expect(setViewport).not.toHaveBeenCalled()
  })

  it("is fitted whole in the chat card, which cannot be dragged", async () => {
    view.zoom = 0.3
    render(<WorkflowCanvas workflow={workflow()} compact />)
    await settle()
    expect(setViewport).not.toHaveBeenCalled()
  })

  it("is framed whole by Fit to view, which asks for all of it", async () => {
    view.zoom = 0.45
    render(<WorkflowCanvas workflow={workflow()} onSelect={() => {}} />)
    await settle()
    setViewport.mockClear()
    fitView.mockClear()
    fireEvent.click(screen.getByRole("button", { name: "Fit to view" }))
    await settle()
    expect(fitView).toHaveBeenCalledWith(FIT)
    expect(setViewport).not.toHaveBeenCalled()
  })
})

describe("a pipeline's stage graph fits its box", () => {
  const stages: ExecutionPlanStage[] = [
    { id: "source", display_name: "Read Postgres", status: "complete" },
    { id: "sink", display_name: "Write BigQuery", status: "running" },
  ]

  it("fits again when the box resizes, but not when a stage's status changes", () => {
    const { rerender } = render(<DAGVisualizationV2 stages={stages} />)
    expect(fitView).toHaveBeenCalledWith({ padding: 0.15, maxZoom: 1.2 })

    fitView.mockClear()
    resize(520)
    expect(fitView).toHaveBeenCalledTimes(1)

    fitView.mockClear()
    rerender(<DAGVisualizationV2 stages={[stages[0], { ...stages[1], status: "complete" }]} />)
    expect(fitView).not.toHaveBeenCalled()
  })
})

// A draft's runs were tries before it was turned on. Its marks keep the run's own
// colours — a grey tick read the same whether a step passed, failed or was still
// running, so a finished try looked unfinished — and their label says it was a draft.
describe("a draft's last-run marks", () => {
  const lastRun = {
    id: "r1",
    trigger: "manual" as const,
    status: "succeeded" as const,
    condition_met: true,
    branch: 0,
    row_count: 3,
    deliveries: [{ channel_id: "ch1", name: "Ops list", kind: "email" as const, status: "sent" as const }],
    notes: [],
    started_at: "2026-10-03T09:00:00Z",
    finished_at: "2026-10-03T09:00:02Z",
  }
  // React Flow hides a node until it has measured it, and jsdom measures nothing, so
  // the marks are read from the DOM rather than by their (hidden) accessible names.
  const marks = () => [...document.querySelectorAll('[role="img"][aria-label^="Last run"]')]

  it("are green on a draft whose try passed, and say it was a draft", () => {
    render(<WorkflowCanvas workflow={{ ...workflow(), last_run: lastRun }} onSelect={() => {}} />)
    expect(marks().length).toBeGreaterThan(0)
    for (const m of marks()) {
      expect(m).toHaveAttribute("aria-label", "Last run, as a draft: passed")
      expect(m).toHaveClass("bg-emerald-700")
      expect(m).not.toHaveClass("bg-zinc-500")
    }
  })

  it("are red on a draft whose try failed", () => {
    const failed = { ...lastRun, status: "failed" as const, deliveries: [{ ...lastRun.deliveries[0], status: "failed" as const }] }
    render(<WorkflowCanvas workflow={{ ...workflow(), last_run: failed }} onSelect={() => {}} />)
    expect(marks().some((m) => m.getAttribute("aria-label") === "Last run, as a draft: failed" && m.classList.contains("bg-red-600"))).toBe(true)
  })

  // A green tick on a condition the run found false read as "condition met".
  it("are grey and say not true on a condition that was checked and was false", () => {
    const otherwise = { ...lastRun, branch: 1 }
    render(<WorkflowCanvas workflow={{ ...workflow(), status: "active", last_run: otherwise }} onSelect={() => {}} />)
    const untrue = marks().filter((m) => m.getAttribute("aria-label") === "Last run: not true")
    expect(untrue).toHaveLength(1)
    expect(untrue[0]).toHaveClass("bg-zinc-500")
    expect(untrue[0]).not.toHaveClass("bg-emerald-700")
  })

  it("are green once it is on", () => {
    render(<WorkflowCanvas workflow={{ ...workflow(), status: "active", last_run: lastRun }} onSelect={() => {}} />)
    expect(marks().length).toBeGreaterThan(0)
    for (const m of marks()) {
      expect(m).toHaveAttribute("aria-label", "Last run: passed")
      expect(m).toHaveClass("bg-emerald-700")
    }
  })
})
