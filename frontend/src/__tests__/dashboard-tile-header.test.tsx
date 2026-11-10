import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { tileHeight } from "@/components/dashboards/dashboard-layout"
import type { ChartRecord, ChartResult, ServerChartSettings } from "@/lib/api/charts"
import type { Dashboard, DashboardTile, TileDraw } from "@/lib/api/dashboards"

// A dashboard tile as a viewer reads it: its header names the chart in full, even when
// the name is cut to fit; its warnings name each filter as the filter bar does; and on
// a phone a text tile is as tall as its text. jsdom has no layout, so the phone rule is
// pinned by its classes and the height each tile is given.

vi.mock("@/components/dashboards/DashboardGridEditorNoSSR", () => ({
  DashboardGridEditorNoSSR: () => <div data-testid="grid-editor-stub" />,
}))

const api = vi.hoisted(() => ({ getDashboard: vi.fn(), drawTile: vi.fn(), filterValues: vi.fn() }))
vi.mock("@/lib/api/dashboards", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/dashboards")>()),
  getDashboard: api.getDashboard,
  drawTile: api.drawTile,
  filterValues: api.filterValues,
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ meets: () => true, isLoading: false }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { DashboardPage } from "@/components/dashboards/DashboardPage"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  // Radix popovers open on pointer events jsdom only half implements.
  Element.prototype.hasPointerCapture ||= () => false
  Element.prototype.scrollIntoView ||= () => {}
})

const NAME = "Declined card transactions by merchant category and country"

const CARD: ServerChartSettings = { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } }

const chart: ChartRecord = {
  id: "c1",
  name: NAME,
  source_state: "ok",
  settings: CARD,
  settings_version: 2,
  source_columns: ["activity_date", "currency"],
  visibility: "workspace",
  proposed_by: "",
  mine: true,
  created_at: "",
  updated_at: "",
}

const result = (unfiltered: string[]): ChartResult => ({
  columns: [{ alias: "count", role: "the measure" }],
  rows: [[42]],
  truncated: false,
  masked: [],
  unfiltered,
  sql: "",
  read_at: "2026-10-12T09:00:00Z",
  execution_time_ms: 3,
})

const chartTile: DashboardTile = { id: "t1", kind: "chart", chart_id: "c1", chart, x: 0, y: 0, w: 4, h: 4 }
const textTile: DashboardTile = { id: "t2", kind: "text", body: "What the fraud team reads first.", x: 4, y: 0, w: 8, h: 2 }

const dashboard: Dashboard = {
  id: "d1",
  name: "Fraud & Risk",
  description: "",
  filters: [
    { id: "currency", kind: "values", label: "Currency", column: "currency" },
    { id: "day", kind: "date_range", label: "Day", column: "activity_date" },
    { id: "channel", kind: "values", label: "Channel", column: "channel" },
    { id: "created", kind: "date_range", label: "Created", column: "activity_date" },
  ],
  tiles: [chartTile, textTile],
  tile_count: 2,
  visibility: "workspace",
  proposed_by: "none",
  mine: true,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-06T09:30:00Z",
}

/** A tile's draw: the columns its connection could not filter by, and the filters (by id) left out. */
const draw = (unfiltered: string[], ignored: string[], without: string[] = []): TileDraw => ({
  chart,
  result: result(unfiltered),
  break: null,
  badges: null,
  filters: { applied: ["currency", "day"].filter((id) => !without.includes(id)), ignored, unfiltered: without },
})

beforeEach(() => {
  api.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
  api.drawTile.mockResolvedValue(draw([], []))
  api.filterValues.mockResolvedValue({ values: [], truncated: false })
})

afterEach(() => vi.clearAllMocks())

/** The grid cell a tile sits in. */
const cell = (el: HTMLElement) => el.closest('[data-testid="dashboard-grid"] > div') as HTMLElement

describe("a dashboard tile's header", () => {
  it("names each filter a tile was not filtered by as the filter bar does, never by its column", async () => {
    // Created is on activity_date too, but was not picked, so the tile was not drawn without it.
    api.drawTile.mockResolvedValue(draw(["currency", "activity_date"], ["channel"], ["currency", "day"]))
    render(<DashboardPage dashboardId="d1" />)
    const info = await screen.findByRole("button", { name: /About this chart: 2 warnings/ })
    await userEvent.setup().click(info)
    const popover = await screen.findByRole("dialog")
    expect(within(popover).getByText("Not filtered by Currency, Day: this connection cannot take a filter.")).toBeInTheDocument()
    expect(within(popover).getByText("Not filtered by Channel: this chart's source has no such column.")).toBeInTheDocument()
    expect(within(popover).queryByText(/activity_date/)).toBeNull()
    expect(within(popover).queryByText(/Created/)).toBeNull()
  })

  it("wraps a chart's name to two lines on a phone, where there is no hover, and shows it whole on hover", async () => {
    render(<DashboardPage dashboardId="d1" />)
    const link = await screen.findByRole("link", { name: NAME })
    expect(link).toHaveAttribute("href", "/charts/c1")
    // A touch screen has no hover to show a title, so a phone gets two lines, a long
    // word broken to fit; a wide screen keeps one line, cut to fit.
    expect(link).toHaveClass("line-clamp-2", "break-words", "@xl:block", "@xl:truncate")
    expect(link).not.toHaveClass("truncate")
    expect(link).toHaveAttribute("title", NAME)
  })
})

describe("a dashboard's grid on a phone", () => {
  it("makes a text tile as tall as its text, and a chart tile as tall as its rows", async () => {
    render(<DashboardPage dashboardId="d1" />)
    await screen.findByRole("link", { name: NAME })
    const text = cell(screen.getByText("What the fraud team reads first."))
    const plot = cell(screen.getByRole("region", { name: NAME }))
    // One column: each tile is its own row, its height the rows it spans on a wide screen.
    expect(text.style.getPropertyValue("--tile-h")).toBe(`${tileHeight(2)}px`)
    expect(plot.style.getPropertyValue("--tile-h")).toBe(`${tileHeight(4)}px`)
    // At least that tall for text, which wraps to more lines on a narrow screen ...
    expect(text).toHaveClass("min-h-[var(--tile-h)]", "@xl:min-h-0")
    expect(text).not.toHaveClass("h-[var(--tile-h)]")
    // ... and exactly that tall for a chart, which takes the room it is given.
    expect(plot).toHaveClass("h-[var(--tile-h)]", "@xl:h-auto")
    // A wide screen keeps the editor's rows.
    for (const c of [text, plot]) expect(c).toHaveClass("@xl:[grid-row:var(--tile-row)]", "@xl:[grid-column:var(--tile-col)]")
    expect(screen.getByTestId("dashboard-grid")).toHaveClass("@xl:[grid-auto-rows:var(--row-h)]")
  })
})
