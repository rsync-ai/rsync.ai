/**
 * The pipeline Runs tab's three charts (runs per day by outcome, run time, rows
 * written), counted in the browser from the runs the tab already loaded.
 *
 * Bug class: a chart fed from filtered data. The status filter narrows the run
 * list; if the charts read the same list, filtering to Failed would redraw
 * "runs per day" as if the pipeline only ever failed.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/pipelines/p1",
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const listExecutionsResponse = vi.fn()
vi.mock("@/lib/api/executions", () => ({
  listExecutionsResponse: (...a: unknown[]) => listExecutionsResponse(...a),
}))
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: () => Promise.resolve({ ok: false, status: 500, json: async () => ({}) }),
}))
vi.mock("@/lib/api/pipelines", () => {
  class AssessmentRequiredError extends Error {}
  return { executePipelineWithRunMode: vi.fn(), AssessmentRequiredError }
})
// One element per chart, carrying what it was asked to draw.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings, rows }: { settings: { description: string; x: { categories: string[] } }; rows: object[] }) => (
    <div
      data-testid="chart"
      data-description={settings.description}
      data-days={settings.x.categories.length}
      data-rows={JSON.stringify(rows)}
    />
  ),
}))

import { ExecutionHistoryTab } from "@/components/pipeline/ExecutionHistoryTab"

const HOUR = 3_600_000

function run(i: number, status: string, startedAgoMs: number) {
  const start = Date.now() - startedAgoMs
  return {
    id: `e${String(i).padStart(7, "0")}`,
    pipeline_id: "p1",
    status,
    start_time: new Date(start).toISOString(),
    end_time: new Date(start + 60_000).toISOString(),
    metrics: { records_processed: 10 },
  }
}

function runsChart(): { outcome: string; count: number }[] {
  const chart = screen.getAllByTestId("chart").find((c) => c.dataset.description?.startsWith("This pipeline's runs per day"))
  return JSON.parse(chart!.dataset.rows!)
}

const total = (rows: { count: number }[]) => rows.reduce((n, r) => n + r.count, 0)

beforeEach(() => {
  vi.clearAllMocks()
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

describe("the Runs tab charts", () => {
  it("draws three charts from the loaded runs, by outcome", async () => {
    listExecutionsResponse.mockResolvedValue({
      executions: [run(1, "success", HOUR), run(2, "completed", 2 * HOUR), run(3, "failed", 3 * HOUR)],
    })
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(3))

    const rows = runsChart()
    expect(total(rows)).toBe(3)
    expect(rows.filter((r) => r.outcome === "Succeeded").reduce((n, r) => n + r.count, 0)).toBe(2)
    expect(rows.filter((r) => r.outcome === "Failed").reduce((n, r) => n + r.count, 0)).toBe(1)
    expect(screen.getAllByTestId("chart").map((c) => Number(c.dataset.days))).toEqual([7, 7, 7])
  })

  it("keeps counting every run when the list is filtered to one status", async () => {
    listExecutionsResponse.mockResolvedValue({
      executions: [run(1, "success", HOUR), run(2, "success", 2 * HOUR), run(3, "failed", 3 * HOUR)],
    })
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(3))
    expect(total(runsChart())).toBe(3)

    listExecutionsResponse.mockResolvedValue({ executions: [run(3, "failed", 3 * HOUR)] })
    await user.click(screen.getByRole("combobox", { name: "Status" }))
    await user.click(await screen.findByRole("option", { name: /^failed$/i }))
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(2))

    // The table narrowed; the charts did not.
    expect(total(runsChart())).toBe(3)
  })

  it("loads every run for the charts when a filter is picked after the first list failed", async () => {
    listExecutionsResponse.mockRejectedValueOnce(new Error("Failed to load execution history"))
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    expect(await screen.findByRole("button", { name: "Retry" })).toBeInTheDocument()
    expect(screen.queryAllByTestId("chart")).toHaveLength(0)

    listExecutionsResponse.mockImplementation(({ status }: { status?: string }) =>
      Promise.resolve({
        executions: status ? [run(3, "failed", 3 * HOUR)] : [run(1, "success", HOUR), run(3, "failed", 3 * HOUR)],
      }),
    )
    await user.click(screen.getByRole("combobox", { name: "Status" }))
    await user.click(await screen.findByRole("option", { name: /^failed$/i }))
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(2))

    // The table shows the failed run; the charts count both.
    await waitFor(() => expect(total(runsChart())).toBe(2))
  })

  it("switches to 30 days", async () => {
    listExecutionsResponse.mockResolvedValue({ executions: [run(1, "success", 10 * 24 * HOUR)] })
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    // Ten days ago is outside 7 days: each card says so instead of drawing.
    await waitFor(() => expect(screen.getAllByText("No runs in this window.")).toHaveLength(2))
    expect(screen.queryAllByTestId("chart")).toHaveLength(0)

    await user.click(screen.getByRole("button", { name: /^30d/ }))
    await waitFor(() => expect(total(runsChart())).toBe(1))
    expect(Number(screen.getAllByTestId("chart")[0].dataset.days)).toBe(30)
  })

  it("says how far back the runs go when the list stopped at its limit", async () => {
    // 250 runs over the last three days: older runs may exist and are not counted.
    listExecutionsResponse.mockResolvedValue({
      executions: Array.from({ length: 250 }, (_, i) => run(i + 1, "success", HOUR + (i * 3 * 24 * HOUR) / 250)),
    })
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(3))
    expect(screen.getByTestId("run-charts-caption")).toHaveTextContent(/Counted from the 250 most recent runs, which go back to /)
    // The axis starts on that day, not seven days ago.
    expect(Number(screen.getAllByTestId("chart")[0].dataset.days)).toBeLessThan(7)
  })

  it("says nothing about a limit when every run was loaded", async () => {
    listExecutionsResponse.mockResolvedValue({ executions: [run(1, "success", HOUR)] })
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(3))
    expect(screen.getByTestId("run-charts-caption")).not.toHaveTextContent(/most recent runs/)
  })

  it("says a filter matched nothing, not that the pipeline never ran", async () => {
    listExecutionsResponse.mockResolvedValue({ executions: [run(1, "success", HOUR)] })
    const user = userEvent.setup()
    render(<ExecutionHistoryTab pipelineId="p1" />)
    await waitFor(() => expect(screen.getAllByRole("row")).toHaveLength(2))

    listExecutionsResponse.mockResolvedValue({ executions: [] })
    await user.click(screen.getByRole("combobox", { name: "Status" }))
    await user.click(await screen.findByRole("option", { name: /^failed$/i }))
    const list = await screen.findByRole("region", { name: "Run list" })
    await waitFor(() => expect(within(list).getByText("No runs with this status")).toBeInTheDocument())
    expect(screen.queryByText("No runs yet")).toBeNull()
  })

  it("shows skeletons, not a spinner, while the first list loads", () => {
    listExecutionsResponse.mockReturnValue(new Promise(() => {}))
    render(<ExecutionHistoryTab pipelineId="p1" />)
    const loading = screen.getAllByRole("status").map((s) => s.textContent)
    expect(loading).toEqual(expect.arrayContaining(["Loading run charts…", "Loading runs…"]))
  })
})
