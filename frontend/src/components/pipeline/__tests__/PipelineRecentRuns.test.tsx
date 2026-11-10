/**
 * The Overview "Recent runs" card (GET /pipelines/:id/trends).
 *
 * The rate is succeeded / finished over the same window; a run still in flight
 * counts toward neither side. Below three finished runs no percentage is shown,
 * because it would overstate what we know.
 *
 * Launch review PR2: the ten pills are a bar chart of the last 50 runs (the
 * gateway's cap), and a click on a bar opens its run.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
}))
const push = vi.fn()
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }))
// The chart as its bars: one button per category, so a test can click one.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({
    settings,
    rows,
    onCategoryClick,
  }: {
    settings: { x: { categories: string[] } }
    rows: { outcome: string }[]
    onCategoryClick?: (i: number) => void
  }) => (
    <div data-testid="recent-runs-chart">
      {settings.x.categories.map((c, i) => (
        <button key={c} type="button" data-outcome={rows[i].outcome} onClick={() => onCategoryClick?.(i)}>
          bar {i}
        </button>
      ))}
    </div>
  ),
}))

import { PipelineRecentRuns, summarizeRecentRuns, type TrendRun } from "@/components/pipeline/PipelineRecentRuns"

function run(id: string, status: string, duration_ms?: number): TrendRun {
  return { execution_id: id, status, start_time: "2026-09-18T10:00:00Z", duration_ms }
}

function respond(body: unknown, ok = true) {
  authFetch.mockResolvedValue({ ok, status: ok ? 200 : 500, json: async () => body })
}

beforeEach(() => {
  authFetch.mockReset()
  push.mockReset()
})
afterEach(() => cleanup())

describe("summarizeRecentRuns", () => {
  it("orders oldest first and uses the gateway's counts", () => {
    const s = summarizeRecentRuns({
      finished_runs: 3,
      succeeded_runs: 2,
      avg_duration_ms: 252_000,
      recent_executions: [run("new", "running"), run("mid", "failed", 60_000), run("old", "completed", 1000), run("older", "completed", 1000)],
    })
    expect(s.runs.map((r) => r.execution_id)).toEqual(["older", "old", "mid", "new"])
    expect(s).toMatchObject({ finished: 3, succeeded: 2, running: 1, ratePercent: 67, avgDurationMs: 252_000 })
  })

  it("derives the counts from the list for an older gateway", () => {
    const s = summarizeRecentRuns({
      success_rate: 0.1, // the old, wrong all-time denominator — must be ignored
      recent_executions: [run("a", "completed"), run("b", "completed"), run("c", "failed"), run("d", "running")],
    })
    expect(s).toMatchObject({ finished: 3, succeeded: 2, running: 1, ratePercent: 67 })
  })

  it("shows no percentage below three finished runs", () => {
    expect(summarizeRecentRuns({ recent_executions: [run("a", "completed"), run("b", "failed")] }).ratePercent).toBeNull()
  })
})

describe("PipelineRecentRuns", () => {
  it("draws one bar per run, oldest first, and the N-of-M sentence", async () => {
    respond({
      finished_runs: 10,
      succeeded_runs: 9,
      avg_duration_ms: 252_000,
      recent_executions: Array.from({ length: 10 }, (_, i) => ({
        ...run(`e${i}`, i === 3 ? "failed" : "completed", 250_000),
        start_time: `2026-09-18T${String(20 - i).padStart(2, "0")}:00:00Z`,
      })),
    })
    render(<PipelineRecentRuns pipelineId="p1" />)
    expect(await screen.findByTestId("pipeline-recent-runs-summary")).toHaveTextContent(
      "9 of the last 10 finished runs succeeded · average 4m 12s"
    )
    expect(screen.getByText("Recent runs · 90% succeeded")).toBeInTheDocument()
    expect(screen.getByText(/^Last 10 runs\./)).toBeInTheDocument()
    const bars = within(screen.getByTestId("recent-runs-chart")).getAllByRole("button")
    expect(bars).toHaveLength(10)
    // e3 is the fourth-newest, so the seventh bar from the left.
    expect(bars[6]).toHaveAttribute("data-outcome", "Failed")
    expect(authFetch.mock.calls[0][0]).toContain("/pipelines/p1/trends?limit=50")
  })

  it("opens the run a bar stands for", async () => {
    respond({ recent_executions: [run("newer", "completed", 1000), { ...run("older", "failed", 1000), start_time: "2026-09-18T09:00:00Z" }] })
    render(<PipelineRecentRuns pipelineId="p1" />)
    fireEvent.click(await screen.findByRole("button", { name: "bar 0" }))
    expect(push).toHaveBeenCalledWith("/executions/older")
  })

  it("does not draw the CDC stream's row as a run", async () => {
    respond({ recent_executions: [run("p1", "running"), run("e1", "completed", 1000)] })
    render(<PipelineRecentRuns pipelineId="p1" />)
    expect(await screen.findByText(/^Last 1 run\./)).toBeInTheDocument()
    expect(within(screen.getByTestId("recent-runs-chart")).getAllByRole("button")).toHaveLength(1)
  })

  it("says No runs yet for a pipeline that never ran", async () => {
    respond({ finished_runs: 0, succeeded_runs: 0, recent_executions: [] })
    render(<PipelineRecentRuns pipelineId="p1" />)
    expect(await screen.findByText(/No runs yet/)).toBeInTheDocument()
    expect(screen.queryByText(/% succeeded/)).toBeNull()
  })

  it("says so when nothing has finished yet", async () => {
    respond({ finished_runs: 0, succeeded_runs: 0, recent_executions: [run("e1", "running")] })
    render(<PipelineRecentRuns pipelineId="p1" />)
    expect(await screen.findByTestId("pipeline-recent-runs-summary")).toHaveTextContent("No run has finished yet · 1 running")
  })

  // The gateway reads each run's executions row since the postflight-failed fix,
  // so it can now say "cancelled". That must not be drawn or counted as running.
  it("a cancelled run is its own outcome, not a running one", async () => {
    respond({ finished_runs: 1, succeeded_runs: 1, recent_executions: [run("c1", "cancelled"), run("d1", "completed", 1000)] })
    render(<PipelineRecentRuns pipelineId="p1" />)
    const summary = await screen.findByTestId("pipeline-recent-runs-summary")
    const bars = within(screen.getByTestId("recent-runs-chart")).getAllByRole("button")
    expect(bars[1]).toHaveAttribute("data-outcome", "Cancelled")
    expect(summary).toHaveTextContent("1 of the last 1 finished run succeeded")
    expect(summary).not.toHaveTextContent("running")
  })

  it("shows an error line instead of vanishing when the request fails", async () => {
    respond({}, false)
    render(<PipelineRecentRuns pipelineId="p1" />)
    expect(await screen.findByText("Couldn't load recent runs.")).toBeInTheDocument()
  })
})
