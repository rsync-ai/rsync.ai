import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import type { Mock } from "vitest"

import type { XYSettings } from "@/components/charts/chart-settings"
import { RunCharts } from "@/components/runs/RunCharts"
import { authFetch } from "@/lib/api/auth-fetch"
import { getActivityMetrics, type ActivityMetrics } from "@/lib/api/activity-metrics"
import { ACTIVE_WORKSPACE_EVENT, ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// Home's run charts (D1): what the page asks the gateway for, what it does
// with the answer, and that an answer for an older window or workspace never
// lands. ECharts itself is stubbed here (echart*.test.tsx draw for real); the
// stub shows what each chart was handed.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings, rows }: { settings: XYSettings; rows: readonly object[] }) => (
    <div
      data-testid="chart"
      data-description={settings.description}
      data-rows={rows.length}
      data-colors={settings.series.values.map((s) => s.color).join(",")}
    />
  ),
}))

const fetchMock = authFetch as Mock

const WS_A = "0df2f291-0000-0000-0000-000000000001"
const WS_B = "f71b4b51-0000-0000-0000-000000000002"

function metrics(over: Partial<ActivityMetrics> = {}): ActivityMetrics {
  return {
    days: ["2026-10-03", "2026-10-04", "2026-10-05"],
    time_zone: "UTC",
    pipeline_runs: [{ day: "2026-10-03", status: "succeeded", count: 1 }],
    pipeline_run_time: [{ day: "2026-10-03", runs: 1, median_seconds: 10, p95_seconds: 10 }],
    rows_written: [{ day: "2026-10-03", rows: 105 }],
    model_runs: [],
    model_run_time: [],
    checks: [],
    ...over,
  }
}

function answer(body: unknown, status = 200) {
  return { ok: status < 400, status, json: async () => body }
}

function held<T>() {
  let resolve!: (v: T) => void
  const promise = new Promise<T>((r) => (resolve = r))
  return { promise, resolve }
}

const urls = () => fetchMock.mock.calls.map((c) => String(c[0]))

beforeEach(() => {
  vi.clearAllMocks()
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_A)
})

afterEach(() => {
  window.localStorage.clear()
})

describe("getActivityMetrics", () => {
  it("asks the metrics endpoint for the window, uncached", async () => {
    fetchMock.mockResolvedValueOnce(answer(metrics()))
    await getActivityMetrics(30)
    expect(urls()[0]).toMatch(/\/api\/v1\/activity\/metrics\?days=30$/)
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ cache: "no-store" })
  })

  it("reads a missing series as none and a missing zone as UTC", async () => {
    fetchMock.mockResolvedValueOnce(answer({ days: ["2026-10-05"], pipeline_runs: null }))
    expect(await getActivityMetrics(7)).toEqual({
      days: ["2026-10-05"],
      time_zone: "UTC",
      pipeline_runs: [],
      pipeline_run_time: [],
      rows_written: [],
      model_runs: [],
      model_run_time: [],
      checks: [],
    })
  })

  it("passes the server's error on, or says which status came back", async () => {
    fetchMock.mockResolvedValueOnce(answer({ error: "workspace access denied" }, 403))
    await expect(getActivityMetrics(7)).rejects.toMatchObject({ message: "workspace access denied", status: 403 })
    fetchMock.mockResolvedValueOnce({ ok: false, status: 502, json: async () => Promise.reject(new Error("html")) })
    await expect(getActivityMetrics(7)).rejects.toMatchObject({ message: "Could not load run charts (HTTP 502).", status: 502 })
  })
})

describe("RunCharts", () => {
  it("draws the six charts, and says so where the window has nothing", async () => {
    fetchMock.mockResolvedValueOnce(answer(metrics()))
    render(<RunCharts />)
    const figures = await screen.findAllByRole("figure")
    expect(figures.map((f) => within(f).getByText(/./, { selector: "figcaption" }).textContent)).toEqual([
      "Pipeline runs",
      "Pipeline run time",
      "Rows written",
      "Model runs",
      "Model run time",
      "Data checks",
    ])
    const charts = screen.getAllByTestId("chart")
    expect(charts.map((c) => c.dataset.description)).toEqual([
      "Pipeline runs per day, by outcome",
      "Median and 95th-percentile pipeline run time per day, in seconds",
      "Rows written by pipeline runs per day",
    ])
    expect(charts[0].dataset.colors).toBe("succeeded,failed,cancelled,waiting,running")
    // Two measures for each of the window's three days.
    expect(charts[1].dataset.rows).toBe("6")
    expect(screen.getByText("No model runs in this window.")).toBeInTheDocument()
    expect(screen.getByText("No model run succeeded or failed in this window.")).toBeInTheDocument()
    expect(screen.getByText("No data checks ran in this window.")).toBeInTheDocument()
    expect(screen.getByText("Days are UTC days, by when each run started.")).toBeInTheDocument()
    expect(urls()).toHaveLength(1)
    expect(urls()[0]).toMatch(/days=7$/)
  })

  // The Models page shows the model charts alone. They count every model in the
  // workspace while the table may be filtered to one layer, so each says so.
  it("draws one subject's charts, each saying it counts all of them", async () => {
    fetchMock.mockResolvedValueOnce(answer(metrics()))
    render(<RunCharts only="models" />)
    const figures = await screen.findAllByRole("figure")
    expect(figures.map((f) => within(f).getByText(/./, { selector: "figcaption" }).textContent)).toEqual([
      "Model runs",
      "Model run time",
    ])
    expect(screen.getByText("All models · per day, by outcome")).toBeInTheDocument()
    expect(screen.getByText("All models · median and 95th percentile, in seconds")).toBeInTheDocument()
    expect(screen.queryByText("Pipeline runs")).not.toBeInTheDocument()
    expect(screen.queryByText("No data checks ran in this window.")).not.toBeInTheDocument()
  })

  // WCAG 2.5.3: a button's name starts with the words it shows, so a voice user
  // can say "click 30d". The kit's toggle showed "30d" but was named "Last 30 days".
  it("names each window button by what it shows, then in full", async () => {
    fetchMock.mockResolvedValueOnce(answer(metrics()))
    render(<RunCharts />)
    const buttons = within(screen.getByRole("group", { name: "Chart window" })).getAllByRole("button")
    expect(buttons.map((b) => b.textContent)).toEqual(["7d", "30d"])
    expect(buttons[0]).toHaveAccessibleName("7d, last 7 days")
    expect(buttons[1]).toHaveAccessibleName("30d, last 30 days")
    await screen.findAllByRole("figure")
  })

  it("re-reads for 30 days and drops the 7-day answer that comes back late", async () => {
    const week = held<ReturnType<typeof answer>>()
    fetchMock.mockReturnValueOnce(week.promise).mockResolvedValueOnce(
      answer(metrics({ pipeline_runs: [], pipeline_run_time: [], rows_written: [] })),
    )
    render(<RunCharts />)
    const toggle = screen.getByRole("button", { name: "30d, last 30 days" })
    expect(toggle).toHaveAttribute("aria-pressed", "false")
    fireEvent.click(toggle)
    expect(toggle).toHaveAttribute("aria-pressed", "true")
    // Runs and rows written both say it: no run, no rows.
    expect(await screen.findAllByText("No pipeline runs in this window.")).toHaveLength(2)
    expect(urls()[1]).toMatch(/days=30$/)

    await act(async () => {
      week.resolve(answer(metrics()))
      await week.promise
    })
    expect(screen.queryAllByTestId("chart")).toHaveLength(0)
  })

  it("re-reads on a workspace switch and drops the previous workspace's answer", async () => {
    const first = held<ReturnType<typeof answer>>()
    fetchMock.mockReturnValueOnce(first.promise).mockResolvedValueOnce(answer(metrics({ checks: [{ day: "2026-10-05", status: "pass", count: 2 }] })))
    render(<RunCharts />)
    await act(async () => {
      window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS_B)
      window.dispatchEvent(new Event(ACTIVE_WORKSPACE_EVENT))
    })
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(4))

    await act(async () => {
      first.resolve(answer(metrics()))
      await first.promise
    })
    expect(screen.getAllByTestId("chart")).toHaveLength(4)
    expect(screen.queryByText("No data checks ran in this window.")).toBeNull()
  })

  it("shows the server's error with a Retry that reads again", async () => {
    fetchMock.mockResolvedValueOnce(answer({ error: "database not available" }, 503)).mockResolvedValueOnce(answer(metrics()))
    render(<RunCharts />)
    const alert = await screen.findByRole("alert")
    expect(alert).toHaveTextContent("database not available")
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }))
    await waitFor(() => expect(screen.getAllByTestId("chart")).toHaveLength(3))
    expect(screen.queryByRole("alert")).toBeNull()
    expect(urls()).toHaveLength(2)
  })
})
