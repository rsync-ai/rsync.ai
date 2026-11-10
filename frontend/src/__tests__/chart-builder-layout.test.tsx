import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import type { ChartSourceItem } from "@/lib/api/charts"

// The chart editor's layout (launch review, PR4). Pinned here:
//   - Run and Save chart sit in a bar that sticks under the app header, beside a
//     way back to Charts, so they stay in reach however long the fields get; the
//     middle column no longer holds them;
//   - each chart type is a tile with its icon, pressed when picked, and a type the
//     fields suggest says so to a screen reader too;
//   - the settings hold one row of tiles, the type picked and those suggested, however
//     many types there are; every type is in "All chart types", in its group, found by
//     its name or its group's;
//   - the source is the shared Select, its models and saved queries in groups, one
//     no chart reads disabled with its reason.

const api = vi.hoisted(() => ({
  listChartSources: vi.fn(),
  getChartSourceColumns: vi.fn(),
  previewChart: vi.fn(),
  createChart: vi.fn(),
  getChart: vi.fn(),
}))
vi.mock("@/lib/api/charts", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/charts")>()),
  ...api,
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ meets: () => true, isLoading: false }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { ChartBuilder } from "@/components/charts/ChartBuilder"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

function source(id: string, name: string, extra: Partial<ChartSourceItem> = {}): ChartSourceItem {
  return {
    id,
    name,
    kind: "model",
    visibility: "workspace",
    mine: true,
    connection_id: "conn-1",
    connection_name: "warehouse",
    connector_type: "postgresql",
    built: true,
    chartable: true,
    ...extra,
  }
}

beforeEach(() => {
  api.listChartSources.mockResolvedValue([
    source("m1", "daily_revenue"),
    source("m2", "churn_by_week", { chartable: false, built: false, reason: "never built" }),
    source("q1", "orders_by_region", { kind: "saved_query" }),
  ])
  api.getChartSourceColumns.mockResolvedValue([
    { name: "day", kind: "time", masked: false },
    { name: "amount", kind: "number", masked: false },
  ])
})

afterEach(() => vi.clearAllMocks())

const bar = () => screen.getByTestId("chart-editor-bar")

/** Opens the list of every type, which the row of the type picked and those suggested leaves out. */
async function allTypes() {
  fireEvent.click(screen.getByRole("button", { name: "All chart types" }))
  return screen.findByRole("dialog", { name: "Chart types" })
}

describe("the chart editor's layout", () => {
  it("keeps Run and Save chart in a bar that sticks under the header, beside the way back", async () => {
    render(<ChartBuilder />)
    const heading = await screen.findByRole("heading", { name: "New chart" })
    // A dashboard tile's name links here, so a phone reads the whole of a long name: two
    // lines, a long word broken to fit; a wide screen keeps one line.
    expect(heading).toHaveClass("line-clamp-2", "break-words", "@xl:block", "@xl:truncate")
    expect(heading).not.toHaveClass("truncate")
    expect(bar().className.split(/\s+/)).toEqual(expect.arrayContaining(["sticky", "top-16"]))
    expect(within(bar()).getByRole("link", { name: "Charts" })).toHaveAttribute("href", "/charts")

    // No source yet: Run says why it cannot draw; a new chart has nothing to delete.
    const run = within(bar()).getByRole("button", { name: "Run" })
    expect(run).toBeDisabled()
    expect(run).toHaveAttribute("title", "Pick a model or a saved query.")
    expect(within(bar()).getByRole("button", { name: "Save chart" })).toBeEnabled()
    expect(screen.queryByRole("button", { name: "Delete chart" })).toBeNull()

    const settings = screen.getByRole("region", { name: "Chart settings" })
    expect(within(settings).queryByRole("button", { name: "Run" })).toBeNull()
    expect(within(settings).queryByRole("button", { name: "Save chart" })).toBeNull()
  })

  it("draws each chart type as a tile with its icon, pressed when picked", async () => {
    render(<ChartBuilder />)
    const types = await screen.findByRole("group", { name: "Chart type" })
    // A new chart starts as bars, the one tile in the row until the fields suggest another.
    expect(within(types).getAllByRole("button").map((b) => b.textContent)).toEqual(["Bar", "All chart types"])
    expect(within(types).getByRole("button", { name: "Bar" })).toHaveAttribute("aria-pressed", "true")

    const all = await allTypes()
    // Each group is named by the heading over its tiles.
    const heading = (g: HTMLElement) => document.getElementById(g.getAttribute("aria-labelledby")!)?.textContent
    expect(within(all).getAllByRole("group").map(heading)).toEqual([
      "Change over time",
      "Compare",
      "Part of a whole",
      "Distribution",
      "Relationship",
      "Flow",
      "Two dimensions",
      "Places",
      "One number",
      "Rows",
    ])
    expect(within(all).getByRole("group", { name: "Part of a whole" })).toHaveTextContent("PieDonutTreemapSunburst")
    const tiles = within(all).getAllByRole("button")
    expect(tiles.map((t) => t.textContent)).toEqual([
      "Line",
      "Area",
      "Waterfall",
      "Range bands",
      "Candlestick",
      "Calendar heatmap",
      "Bar",
      "Horizontal bar",
      "Funnel",
      "Pie",
      "Donut",
      "Treemap",
      "Sunburst",
      "Histogram",
      "Box plot",
      "Scatter",
      "Bubble",
      "Sankey",
      "Heatmap",
      "Choropleth map",
      "Scorecard",
      "Bullet",
      "Table",
      "Pivot table",
    ])
    for (const t of tiles) expect(t.querySelector("svg")).not.toBeNull()
    expect(within(all).getByRole("button", { name: "Bar" })).toHaveAttribute("aria-pressed", "true")

    // Picking one closes the list and puts it in the row in place of the last.
    fireEvent.click(within(all).getByRole("button", { name: "Heatmap" }))
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Chart types" })).toBeNull())
    expect(within(types).getAllByRole("button").map((b) => b.textContent)).toEqual(["Heatmap", "All chart types"])
    expect(within(types).getByRole("button", { name: "Heatmap" })).toHaveAttribute("aria-pressed", "true")
    expect(within(types).getByRole("button", { name: "Heatmap" })).toHaveAttribute("title", "Two dimensions")
    const again = await allTypes()
    expect(within(again).getByRole("button", { name: "Bar" })).toHaveAttribute("aria-pressed", "false")
    expect(within(again).getByRole("button", { name: "Heatmap" })).toHaveAttribute("aria-pressed", "true")
  })

  it("finds a type by its name or its group's, and shows them all when opened again", async () => {
    render(<ChartBuilder />)
    await screen.findByRole("group", { name: "Chart type" })
    const all = await allTypes()
    const search = within(all).getByRole("searchbox", { name: "Find a chart type" })
    // Opened, the search has the focus: type, then Tab to a tile.
    await waitFor(() => expect(search).toHaveFocus())
    const names = () => within(all).queryAllByRole("button").map((t) => t.textContent)

    fireEvent.change(search, { target: { value: "  BAR " } })
    expect(names()).toEqual(["Bar", "Horizontal bar"])
    fireEvent.change(search, { target: { value: "part" } })
    expect(names()).toEqual(["Pie", "Donut", "Treemap", "Sunburst"])
    fireEvent.change(search, { target: { value: "gauge" } })
    expect(names()).toEqual([])
    expect(within(all).getByText("No chart type matches “gauge”.")).toBeInTheDocument()

    // Escape closes it and gives the focus back to the button that opened it.
    fireEvent.keyDown(search, { key: "Escape" })
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Chart types" })).toBeNull())
    expect(screen.getByRole("button", { name: "All chart types" })).toHaveFocus()
    const again = await allTypes()
    expect(within(again).getByRole("searchbox", { name: "Find a chart type" })).toHaveValue("")
    expect(within(again).getAllByRole("button")).toHaveLength(24)
  })

  it("marks the type the fields suggest, in words a screen reader reads", async () => {
    render(<ChartBuilder initialSource="m1" />)
    const x = (await screen.findByLabelText("X axis")) as HTMLSelectElement
    const types = screen.getByRole("group", { name: "Chart type" })
    expect(within(types).queryByText(/suggested/)).toBeNull()
    expect(screen.queryByText("Suggested for the fields you picked")).toBeNull()

    fireEvent.change(x, { target: { value: "day" } })
    // The suggestion sits in the row beside the type picked, a click away.
    expect(within(types).getAllByRole("button").map((b) => b.textContent)).toEqual(["Bar", "Line", "All chart types"])
    expect(within(types).getByRole("button", { name: "Line (suggested)" })).toHaveAttribute("title", "Change over time · suggested")
    expect(screen.getByText("Suggested for the fields you picked")).toBeInTheDocument()
    expect(within(await allTypes()).getByRole("button", { name: "Line (suggested)" })).toHaveAttribute("aria-pressed", "false")

    fireEvent.click(within(types).getByRole("button", { name: "Line (suggested)" }))
    expect(within(types).getAllByRole("button").map((b) => b.textContent)).toEqual(["Line", "All chart types"])
    expect(within(types).getByRole("button", { name: "Line (suggested)" })).toHaveAttribute("aria-pressed", "true")
  })

  it("picks the source from the shared Select, models and saved queries in groups", async () => {
    const user = userEvent.setup()
    render(<ChartBuilder />)
    const picker = await screen.findByRole("combobox", { name: "Model or saved query" })
    await vi.waitFor(() => expect(picker).toBeEnabled())
    expect(picker).toHaveTextContent("Pick a source")

    await user.click(picker)
    const list = await screen.findByRole("listbox")
    const groups = within(list).getAllByRole("group")
    expect(groups.map((g) => within(g).getAllByRole("option").map((o) => o.textContent))).toEqual([
      ["daily_revenue", "churn_by_week (never built)"],
      ["orders_by_region"],
    ])
    expect(within(groups[0]).getByText("Models")).toBeInTheDocument()
    expect(within(groups[1]).getByText("Saved queries")).toBeInTheDocument()
    expect(within(list).getByRole("option", { name: /churn_by_week/ })).toHaveAttribute("aria-disabled", "true")

    await user.click(within(list).getByRole("option", { name: "daily_revenue" }))
    expect(api.getChartSourceColumns).toHaveBeenCalledWith("m1")
    expect(picker).toHaveTextContent("daily_revenue")
    expect(await screen.findByText("warehouse · shared")).toBeInTheDocument()
  })
})
