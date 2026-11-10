import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import HomePage from "@/app/(dashboard)/page"
import RunsPage from "@/app/(dashboard)/runs/page"
import type { ChartSettings } from "@/components/charts/chart-settings"
import { authFetch } from "@/lib/api/auth-fetch"

// Where the run charts are (dashboards-design.md decision 11): on Home, in the
// Activity card, and not on Runs, which draws no chart: only its list, with the
// last 7 days' totals above it from the same metrics read. ECharts is stubbed;
// run-charts.test.tsx covers what each chart is handed.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/headers", () => ({ cookies: async () => ({ getAll: () => [] }) }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/",
}))
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings }: { settings: ChartSettings }) => <div data-testid="chart" data-description={settings.description} />,
}))

const fetchMock = authFetch as Mock

function answer(body: unknown) {
  return { ok: true, status: 200, json: async () => body } as unknown as Response
}

const METRICS = {
  days: ["2026-10-05"],
  time_zone: "UTC",
  pipeline_runs: [{ day: "2026-10-05", status: "succeeded", count: 2 }],
  rows_written: [{ day: "2026-10-05", rows: 40 }],
}

const metricsCalls = () => fetchMock.mock.calls.filter((c) => String(c[0]).includes("/api/v1/activity/metrics"))
const metricsAsked = () => metricsCalls().length > 0

beforeEach(() => {
  vi.clearAllMocks()
  vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 503, json: async () => ({}) })))
  fetchMock.mockImplementation(async (url: string) =>
    answer(String(url).includes("/api/v1/activity/metrics") ? METRICS : { runs: [], pipelines: [], workflows: [], schedules: [], total: 0 }),
  )
})

describe("the run charts' place", () => {
  it("Home draws the six charts in its Activity card", async () => {
    render(await HomePage())
    const card = screen.getByRole("region", { name: "Activity" })
    await waitFor(() => expect(within(card).getAllByRole("figure")).toHaveLength(6))
    expect(within(card).getAllByTestId("chart").map((c) => c.dataset.description)).toEqual([
      "Pipeline runs per day, by outcome",
      "Rows written by pipeline runs per day",
    ])
    expect(within(card).getByRole("button", { name: "30d, last 30 days" })).toBeInTheDocument()
    expect(metricsAsked()).toBe(true)
  })

  it("Runs draws no chart: its list, and the last 7 days' totals from one metrics read", async () => {
    render(await RunsPage({ searchParams: Promise.resolve({}) }))
    await waitFor(() => expect(fetchMock.mock.calls.some((c) => String(c[0]).includes("/api/v1/activity?"))).toBe(true))
    const strip = await screen.findByRole("region", { name: "Last 7 days" })
    const value = (card: string) => within(strip).getByRole("region", { name: card }).querySelector("p")?.textContent
    expect([value("Runs"), value("Failed"), value("Success rate"), value("Rows written")]).toEqual(["2", "0", "100%", "40"])
    expect(screen.queryAllByRole("figure")).toHaveLength(0)
    expect(screen.queryByRole("group", { name: "Chart window" })).toBeNull()
    expect(metricsCalls().map((c) => String(c[0]).replace(/^https?:\/\/[^/]+/, ""))).toEqual(["/api/v1/activity/metrics?days=7"])
  })

  it("Runs hides the totals under the Workflows filter, which they do not count, without reading them again", async () => {
    render(await RunsPage({ searchParams: Promise.resolve({}) }))
    await screen.findByRole("region", { name: "Last 7 days" })

    fireEvent.click(screen.getByRole("button", { name: "Workflows" }))
    expect(screen.queryByRole("region", { name: "Last 7 days" })).toBeNull()

    fireEvent.click(screen.getByRole("button", { name: "All" }))
    expect(screen.getByRole("region", { name: "Last 7 days" })).toBeInTheDocument()
    expect(metricsCalls()).toHaveLength(1)
  })
})
