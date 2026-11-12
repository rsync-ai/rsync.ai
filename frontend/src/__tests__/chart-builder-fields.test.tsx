import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import type { ServerChartSettings } from "@/lib/api/charts"

// The chart editor's fields for the D4b to D4e types (dashboards design § 14.9). Pinned here:
//   - each type shows the fields its registry entry takes and no others: a histogram
//     has no measure and no order, a pie no order and no colour;
//   - an aggregate a type does not take reads disabled;
//   - a second measure makes a combo, which has no series, and each measure gets a
//     mark and an axis;
//   - 100% shows once the series stack, and refuses a measure that does not add up;
//   - a scorecard's sparkline is offered over more than one day only;
//   - a scatter's and a bubble's x, y and size are number columns, a bubble's size needed;
//   - a pivot table's next row or column field shows once the last is picked, a time
//     field gets a grain, a column is a field once, and a third measure follows a second;
//   - a treemap's levels follow one another, with no grain; a sankey's From and To differ;
//     a calendar's day is a date column; a bullet's target is a typed number or a measure,
//     never both; a box plot's values and a candlestick's prices are unmasked number
//     columns; a range's low and high are two different measures;
//   - a map's shapes are world countries or US states, its codes an unmasked text or number
//     column, with no order and no series;
//   - a box plot is greyed out, with the reason, on an engine with no exact percentile.

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

beforeEach(() => {
  api.listChartSources.mockResolvedValue([
    {
      id: "m1",
      name: "orders",
      kind: "model",
      visibility: "workspace",
      mine: true,
      connection_id: "conn-1",
      connection_name: "warehouse",
      connector_type: "postgresql",
      built: true,
      chartable: true,
    },
  ])
  api.getChartSourceColumns.mockResolvedValue([
    { name: "day", kind: "time", masked: false },
    { name: "amount", kind: "number", masked: false },
    { name: "plan", kind: "text", masked: false },
  ])
  api.previewChart.mockResolvedValue({ columns: [], rows: [], truncated: false })
})

afterEach(() => vi.clearAllMocks())

/** A type's tile in the list of every type, which the row of the type picked and those suggested leaves out. */
async function tile(type: string) {
  // The button toggles it: a click on it open closes it.
  if (!screen.queryByRole("dialog", { name: "Chart types" })) fireEvent.click(screen.getByRole("button", { name: "All chart types" }))
  return within(await screen.findByRole("dialog", { name: "Chart types" })).getByRole("button", { name: new RegExp(`^${type}`) })
}

async function open(type: string) {
  render(<ChartBuilder initialSource="m1" />)
  await screen.findByLabelText("X axis")
  fireEvent.click(await tile(type))
}

const pick = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } })
const options = (label: string) =>
  [...(screen.getByLabelText(label) as HTMLSelectElement).options].map((o) => `${o.value}${o.disabled ? " (disabled)" : ""}`)
const run = () => within(screen.getByTestId("chart-editor-bar")).getByRole("button", { name: "Run" })
const sent = (): ServerChartSettings => api.previewChart.mock.calls.at(-1)![1]

describe("the chart editor's fields for each type", () => {
  it("bins a number column for a histogram: no measure and no order, a number of bins or a width", async () => {
    await open("Histogram")
    expect(options("Column to count in bins")).toEqual(["", "day (disabled)", "amount", "plan (disabled)"])
    expect(screen.queryByLabelText("Measure")).toBeNull()
    expect(screen.queryByLabelText("Order")).toBeNull()

    pick("Column to count in bins", "amount")
    fireEvent.change(screen.getByLabelText("Bins"), { target: { value: "500" } })
    // More than the most bins is the most.
    expect(screen.getByLabelText("Bins")).toHaveValue("100")
    // A width takes the place of a number of bins: the server takes one or the other.
    fireEvent.change(screen.getByLabelText("Bin width"), { target: { value: "2.5" } })
    expect(screen.getByLabelText("Bins")).toHaveValue("")
    fireEvent.click(run())
    expect(sent().roles.x).toEqual([{ column: "amount", width: 2.5 }])
  })

  it("takes a pie's slices from a column and measures them by a count or a sum, with no order or colour", async () => {
    await open("Pie")
    expect(screen.getByLabelText("Slices")).toBeInTheDocument()
    expect(options("Measure")).toEqual([
      "",
      "count",
      "count_distinct (disabled)",
      "sum",
      "avg (disabled)",
      "min (disabled)",
      "max (disabled)",
    ])
    expect(screen.queryByLabelText("Order")).toBeNull()
    expect(screen.queryByRole("group", { name: "Colour" })).toBeNull()

    pick("Slices", "plan")
    pick("Measure", "count")
    expect(screen.getByText(/More than 7 slices draw the top 7 and Other/)).toBeInTheDocument()
    fireEvent.click(run())
    expect(sent().roles.series).toEqual([{ column: "plan" }])
  })

  it("draws two measures as a combo with no series, each with its mark and its axis", async () => {
    await open("Bar")
    pick("X axis", "day")
    pick("Measure", "count")
    expect(screen.getByLabelText("Series (optional)")).toBeInTheDocument()

    pick("Second measure", "sum")
    expect(screen.queryByLabelText("Series (optional)")).toBeNull()
    pick("Second measure: drawn", "line")
    pick("Second measure: axis", "right")
    // A sum needs its column before it draws.
    expect(run()).toHaveAttribute("title", "Pick the column for the second measure.")
    fireEvent.change(document.getElementById("chart-measure-column-2")!, { target: { value: "amount" } })
    fireEvent.click(run())
    expect(sent().roles.measure).toEqual([
      { aggregate: "count" },
      { aggregate: "sum", column: "amount", mark: "line", axis: "right" },
    ])
    expect(sent().roles.series).toBeUndefined()

    // With no second measure, the series comes back.
    pick("Second measure", "")
    expect(screen.getByLabelText("Series (optional)")).toBeInTheDocument()
  })

  it("offers 100% once the series stack, and refuses a measure that does not add up", async () => {
    await open("Bar")
    pick("X axis", "day")
    pick("Measure", "avg")
    fireEvent.change(document.getElementById("chart-measure-column")!, { target: { value: "amount" } })
    pick("Series (optional)", "plan")
    expect(screen.queryByLabelText("Show each as a share of 100%")).toBeNull()

    fireEvent.click(screen.getByLabelText("Stack the series"))
    fireEvent.click(screen.getByLabelText("Show each as a share of 100%"))
    expect(run()).toBeDisabled()
    expect(run()).toHaveAttribute("title", "A share of 100% adds the series up: measure a count or a sum.")

    pick("Measure", "sum")
    fireEvent.click(run())
    expect(sent()).toMatchObject({ stacked: true, percent: true })
  })

  it("offers a scorecard's sparkline over more than one day only", async () => {
    await open("Scorecard")
    pick("Measure", "count")
    pick("Compare with the period before (optional)", "day")
    pick("Days", "1")
    expect(screen.queryByLabelText(/as a line under the number/)).toBeNull()

    pick("Days", "30")
    fireEvent.click(screen.getByLabelText("Draw the last 30 days as a line under the number"))
    fireEvent.click(run())
    expect(sent()).toMatchObject({ compare: { days: 30 }, sparkline: true })
  })

  it("orders a heatmap by its x only, as its registry entry says", async () => {
    await open("Heatmap")
    expect(options("Order")).toEqual(["", "x_asc", "x_desc"])
  })

  it("says a bar's default order is largest first, unless its x is a date or a series orders it by x", async () => {
    await open("Bar")
    const first = () => (screen.getByLabelText("Order") as HTMLSelectElement).options[0].text
    pick("X axis", "plan")
    pick("Measure", "count")
    expect(first()).toBe("Default (largest first)")
    pick("Series (optional)", "plan")
    expect(first()).toBe("Default")
    pick("Series (optional)", "")

    // Dates read oldest first, with a grain or without one.
    pick("X axis", "day")
    expect(first()).toBe("Default")
    pick("X axis grain", "month")
    expect(first()).toBe("Default")
  })
})

describe("the chart editor's fields for a scatter, a bubble and a pivot table", () => {
  const COLUMNS = [
    { name: "day", kind: "time", masked: false },
    { name: "amount", kind: "number", masked: false },
    { name: "discount", kind: "number", masked: false },
    { name: "quantity", kind: "number", masked: false },
    { name: "salary", kind: "number", masked: true },
    { name: "plan", kind: "text", masked: false },
  ]
  beforeEach(() => api.getChartSourceColumns.mockResolvedValue(COLUMNS))

  // A select with no label of its own, read by its id.
  const byId = (id: string) => document.getElementById(id) as HTMLSelectElement | null
  const choose = (id: string, value: string) => fireEvent.change(byId(id)!, { target: { value } })
  const offered = (id: string) => [...byId(id)!.options].map((o) => `${o.value}${o.disabled ? " (disabled)" : ""}`)

  it("places a scatter's points by number columns only, a size and a colour optional", async () => {
    await open("Scatter")
    // A masked number places nothing, so it is not offered.
    const numbers = ["", "day (disabled)", "amount", "discount", "quantity", "salary (disabled)", "plan (disabled)"]
    expect(options("X axis")).toEqual(numbers)
    expect(options("Y axis")).toEqual(numbers)
    expect(options("Size (optional)")).toEqual(numbers)
    expect(screen.queryByLabelText("Measure")).toBeNull()
    expect(screen.queryByLabelText("Order")).toBeNull()
    expect(screen.getByText("At most 10,000 points are drawn, the same ones each time.")).toBeInTheDocument()

    pick("X axis", "amount")
    expect(run()).toHaveAttribute("title", "Pick the number column for the y axis.")
    pick("Y axis", "amount")
    expect(run()).toHaveAttribute("title", "Pick a different column for each of x, y and size.")
    pick("Y axis", "discount")
    fireEvent.change(screen.getByLabelText("Y axis title"), { target: { value: "Discount %" } })
    pick("Colour (optional)", "plan")
    // The server ranks the colours by how many rows each holds: a point can hold many.
    expect(screen.getByText(/More than 7 colours draw the 7 with the most rows and Other/)).toBeInTheDocument()
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "scatter",
      roles: { x: [{ column: "amount" }], y: [{ column: "discount", title: "Discount %" }], series: [{ column: "plan" }] },
    })
    expect(sent().roles.size).toBeUndefined()
    expect(sent().roles.measure).toBeUndefined()
  })

  it("needs a bubble's size, a column of its own, and says what its area reads", async () => {
    await open("Bubble")
    pick("X axis", "amount")
    pick("Y axis", "discount")
    expect(run()).toHaveAttribute("title", "Pick the number column each bubble's size reads.")
    expect(screen.queryByLabelText("Size title")).toBeNull()

    pick("Size", "amount")
    expect(run()).toHaveAttribute("title", "Pick a different column for each of x, y and size.")
    pick("Size", "quantity")
    expect(screen.getByText(/a point with no size above zero is not drawn/)).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText("Size title"), { target: { value: "Units" } })
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "bubble",
      roles: { x: [{ column: "amount" }], y: [{ column: "discount" }], size: [{ column: "quantity", title: "Units" }] },
    })
  })

  it("adds a pivot table's next row field once the last is picked, with a grain for a time column", async () => {
    await open("Pivot table")
    expect(screen.getByRole("group", { name: "Rows (0/3)" })).toBeInTheDocument()
    expect(byId("chart-pivot-rows-2")).toBeNull()
    expect(run()).toHaveAttribute("title", "Pick at least one row field.")

    choose("chart-pivot-rows-1", "plan")
    expect(screen.queryByLabelText("Row field 1 grain")).toBeNull()
    expect(screen.getByRole("group", { name: "Rows (1/3)" })).toBeInTheDocument()
    // A column is a field once, in the rows or the columns.
    expect(offered("chart-pivot-rows-2")).toContain("plan (disabled)")
    expect(offered("chart-pivot-columns-1")).toContain("plan (disabled)")

    choose("chart-pivot-rows-2", "day")
    expect(screen.getByLabelText("Row field 2 grain")).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText("Row field 2 grain"), { target: { value: "month" } })
    fireEvent.change(screen.getByLabelText("Row field 2 title"), { target: { value: "Month" } })
    choose("chart-pivot-rows-3", "amount")
    // Three row fields are the most: no fourth slot.
    expect(byId("chart-pivot-rows-4")).toBeNull()

    // Clearing a field closes the gap; the ones after it move up.
    choose("chart-pivot-rows-1", "")
    expect(screen.getByRole("group", { name: "Rows (2/3)" })).toBeInTheDocument()
    expect(byId("chart-pivot-rows-1")!.value).toBe("day")
    expect(byId("chart-pivot-rows-2")!.value).toBe("amount")

    choose("chart-pivot-columns-1", "plan")
    choose("chart-pivot-columns-2", "discount")
    expect(byId("chart-pivot-columns-3")).toBeNull()

    expect(run()).toHaveAttribute("title", "Pick what to measure.")
    pick("Measure", "count")
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "pivot",
      roles: {
        rows: [{ column: "day", title: "Month", grain: "month" }, { column: "amount" }],
        columns: [{ column: "plan" }, { column: "discount" }],
        measure: [{ aggregate: "count" }],
      },
    })
  })

  it("offers a pivot table's third measure after its second, and says how its totals are counted", async () => {
    await open("Pivot table")
    choose("chart-pivot-rows-1", "plan")
    expect(screen.queryByLabelText("Second measure (optional)")).toBeNull()
    pick("Measure", "sum")
    choose("chart-measure-column", "amount")
    expect(screen.queryByLabelText("Third measure (optional)")).toBeNull()

    pick("Second measure", "avg")
    expect(run()).toHaveAttribute("title", "Pick the column for measure 2.")
    choose("chart-measure-column-2", "amount")
    expect(screen.getByText(/an average's total is the average of every row, not of the cells/)).toBeInTheDocument()
    pick("Third measure", "sum")
    choose("chart-measure-column-3", "amount")
    expect(run()).toHaveAttribute("title", "Two measures are the same; measure another column or use another aggregate.")
    pick("Third measure", "count")
    fireEvent.click(run())
    expect(sent().roles.measure).toEqual([
      { aggregate: "sum", column: "amount" },
      { aggregate: "avg", column: "amount" },
      { aggregate: "count" },
    ])
    // A pivot's measures are its cells, never a combo: no mark and no axis.
    expect(screen.queryByLabelText("Second measure: drawn")).toBeNull()
  })
})

describe("the chart editor's fields for the D4d types", () => {
  const COLUMNS = [
    { name: "day", kind: "time", masked: false },
    { name: "amount", kind: "number", masked: false },
    { name: "open", kind: "number", masked: false },
    { name: "close", kind: "number", masked: false },
    { name: "salary", kind: "number", masked: true },
    { name: "plan", kind: "text", masked: false },
    { name: "region", kind: "text", masked: false },
  ]
  beforeEach(() => api.getChartSourceColumns.mockResolvedValue(COLUMNS))

  const byId = (id: string) => document.getElementById(id) as HTMLSelectElement | null
  const choose = (id: string, value: string) => fireEvent.change(byId(id)!, { target: { value } })
  const offered = (id: string) => [...byId(id)!.options].map((o) => `${o.value}${o.disabled ? " (disabled)" : ""}`)

  it("adds a treemap's next level once the last is picked, each column once, with no grain", async () => {
    await open("Treemap")
    expect(screen.queryByLabelText("X axis")).toBeNull()
    expect(screen.getByRole("group", { name: "Levels, outermost first (0/3)" })).toBeInTheDocument()
    expect(run()).toHaveAttribute("title", "Pick at least one level.")
    choose("chart-pivot-levels-1", "region")
    expect(offered("chart-pivot-levels-2")).toContain("region (disabled)")
    choose("chart-pivot-levels-2", "day")
    // A level is grouped by its values: a time column gets no grain.
    expect(screen.queryByLabelText("Level 2 grain")).toBeNull()
    choose("chart-pivot-levels-3", "plan")
    expect(byId("chart-pivot-levels-4")).toBeNull()
    expect(screen.getByText(/Each part takes its outermost level's colour/)).toBeInTheDocument()
    expect(options("Measure")).toEqual(["", "count", "count_distinct (disabled)", "sum", "avg (disabled)", "min (disabled)", "max (disabled)"])
    expect(screen.queryByLabelText("Order")).toBeNull()
    pick("Measure", "count")
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "treemap",
      roles: { rows: [{ column: "region" }, { column: "day" }, { column: "plan" }], measure: [{ aggregate: "count" }] },
    })
  })

  it("draws a sankey's flows from one column to another, never the same, with no grain or order", async () => {
    await open("Sankey")
    pick("From", "day")
    expect(screen.queryByLabelText("X axis grain")).toBeNull()
    pick("To", "day")
    pick("Measure", "count")
    expect(run()).toHaveAttribute("title", "Pick a different column for where the flows start and where they end.")
    pick("To", "region")
    expect(screen.getByText(/The 100 largest names are drawn/)).toBeInTheDocument()
    expect(screen.queryByLabelText("Order")).toBeNull()
    fireEvent.click(run())
    expect(sent()).toMatchObject({ type: "sankey", roles: { x: [{ column: "day" }], y: [{ column: "region" }] } })
  })

  it("lays a calendar's days out by a date column only", async () => {
    await open("Calendar heatmap")
    expect(options("Day")).toEqual(["", "day", "amount (disabled)", "open (disabled)", "close (disabled)", "salary (disabled)", "plan (disabled)", "region (disabled)"])
    expect(screen.queryByLabelText("X axis grain")).toBeNull()
    pick("Day", "day")
    pick("Measure", "sum")
    choose("chart-measure-column", "amount")
    fireEvent.click(run())
    expect(sent()).toMatchObject({ type: "calendar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount" }] } })
  })

  it("takes a bullet's target as a typed number, zero or below too, or as a second measure", async () => {
    await open("Bullet")
    expect(screen.queryByLabelText("X axis")).toBeNull()
    pick("Measure", "count")
    expect(run()).toHaveAttribute("title", "Type the target, or measure it.")
    fireEvent.change(screen.getByLabelText("Target number"), { target: { value: "-" } })
    expect(screen.getByLabelText("Target number")).toHaveValue("-")
    fireEvent.change(screen.getByLabelText("Target number"), { target: { value: "-12.5" } })
    fireEvent.click(run())
    expect(sent()).toMatchObject({ type: "bullet", target: -12.5, roles: { measure: [{ aggregate: "count" }] } })
    await waitFor(() => expect(run()).toBeEnabled())

    pick("Target", "measure")
    expect(screen.queryByLabelText("Target number")).toBeNull()
    expect(run()).toHaveAttribute("title", "Pick what the target measures.")
    pick("Target measure", "sum")
    choose("chart-measure-column-2", "amount")
    fireEvent.click(run())
    expect(sent().target).toBeUndefined()
    expect(sent().roles.measure).toEqual([{ aggregate: "count" }, { aggregate: "sum", column: "amount" }])
  })

  it("draws a box of an unmasked number column per group, with its format and no measure", async () => {
    await open("Box plot")
    expect(screen.getByLabelText("Groups")).toBeInTheDocument()
    expect(screen.queryByLabelText("Measure")).toBeNull()
    expect(options("Values")).toEqual(["", "day (disabled)", "amount", "open", "close", "salary (disabled)", "plan (disabled)", "region (disabled)"])
    pick("Groups", "day")
    pick("X axis grain", "month")
    expect(run()).toHaveAttribute("title", "Pick the number column to draw the boxes of.")
    pick("Values", "amount")
    pick("Number format", "compact")
    expect(options("Order")).toEqual(["", "x_asc", "x_desc"])
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "boxplot",
      roles: { x: [{ column: "day", grain: "month" }], y: [{ column: "amount", format: "compact" }] },
    })
    expect(sent().roles.measure).toBeUndefined()
  })

  it("greys out a box plot on a MySQL source and says why", async () => {
    api.listChartSources.mockResolvedValue([
      { id: "m1", name: "orders", kind: "model", visibility: "workspace", mine: true, connection_id: "c", connection_name: "shop", connector_type: "mysql", built: true, chartable: true },
    ])
    render(<ChartBuilder initialSource="m1" />)
    await screen.findByLabelText("X axis")
    const boxplot = await tile("Box plot")
    expect(boxplot).toBeDisabled()
    expect(boxplot).toHaveAttribute("title", expect.stringMatching(/exact percentile, which MySQL and MariaDB do not have/))
    expect(await tile("Candlestick")).toBeEnabled()
  })

  it("reads a candlestick's open, high, low and close from four number columns, in order", async () => {
    await open("Candlestick")
    expect(options("Date")).toContain("plan (disabled)")
    expect(options("Open")).toEqual(["", "day (disabled)", "amount", "open", "close", "salary (disabled)", "plan (disabled)", "region (disabled)"])
    pick("Date", "day")
    pick("Open", "open")
    pick("High", "amount")
    pick("Low", "amount")
    expect(run()).toHaveAttribute("title", "Pick the open, high, low and close columns.")
    pick("Close", "close")
    fireEvent.click(run())
    expect(sent()).toMatchObject({
      type: "candlestick",
      roles: { x: [{ column: "day" }], y: [{ column: "open" }, { column: "amount" }, { column: "amount" }, { column: "close" }] },
    })
  })

  it("draws range bands between a low and a high measure", async () => {
    await open("Range bands")
    pick("X axis", "day")
    pick("Low", "min")
    choose("chart-measure-column", "amount")
    expect(run()).toHaveAttribute("title", "Pick what the high measures.")
    pick("High", "min")
    choose("chart-measure-column-2", "amount")
    expect(run()).toHaveAttribute("title", "The low and the high are the same measure; measure another column or use another aggregate.")
    pick("High", "max")
    expect(screen.queryByLabelText("Series (optional)")).toBeNull()
    fireEvent.click(run())
    expect(sent().roles.measure).toEqual([
      { aggregate: "min", column: "amount" },
      { aggregate: "max", column: "amount" },
    ])
  })
})

describe("the chart editor's fields for a map", () => {
  beforeEach(() =>
    api.getChartSourceColumns.mockResolvedValue([
      { name: "day", kind: "time", masked: false },
      { name: "amount", kind: "number", masked: false },
      { name: "plan", kind: "text", masked: false },
      { name: "email", kind: "text", masked: true },
    ]),
  )

  it("fills world countries or US states from an unmasked column of codes, with no order or series", async () => {
    await open("Choropleth map")
    expect(options("Shapes")).toEqual(["world", "us_states"])
    expect(screen.getByLabelText("Shapes")).toHaveValue("world")
    expect(options("Country codes")).toEqual(["", "day (disabled)", "amount", "plan", "email (disabled)"])
    expect(screen.getByText(/a country by its ISO code: FR, FRA or 250\. A name is not read/)).toBeInTheDocument()
    expect(screen.queryByLabelText("Order")).toBeNull()
    expect(screen.queryByLabelText("Series (optional)")).toBeNull()
    expect(run()).toHaveAttribute("title", "Pick the column of country codes.")

    pick("Shapes", "us_states")
    expect(screen.queryByLabelText("Country codes")).toBeNull()
    expect(run()).toHaveAttribute("title", "Pick the column of US state codes.")
    pick("US state codes", "plan")
    pick("Measure", "count")
    fireEvent.click(run())
    expect(sent()).toMatchObject({ type: "map", map: "us_states", roles: { x: [{ column: "plan" }], measure: [{ aggregate: "count" }] } })
  })
})

describe("a pick the chart can no longer take, and a chart drawn from older fields", () => {
  beforeEach(() =>
    api.getChartSourceColumns.mockResolvedValue([
      { name: "day", kind: "time", masked: false },
      { name: "amount", kind: "number", masked: false },
      { name: "fee", kind: "number", masked: false },
      { name: "salary", kind: "number", masked: true },
      { name: "plan", kind: "text", masked: false },
    ]),
  )

  const stale = () => screen.queryByText(/^This chart is out of date/)
  const drawn = async (times: number) => {
    await waitFor(() => expect(api.previewChart).toHaveBeenCalledTimes(times))
    await waitFor(() => expect(screen.queryByText("Run it to draw the chart.")).toBeNull())
  }

  // A type keeps the fields the last one had: one it cannot read is named, never sent
  // for the engine to refuse in its own words.
  it("names a kept column the new type cannot read, on the page, and sends nothing", async () => {
    await open("Heatmap")
    pick("X axis", "plan")
    pick("Y axis", "day")
    pick("Measure", "count")
    fireEvent.click(run())
    await drawn(1)

    fireEvent.click(await tile("Histogram"))
    const why = "plan is a text column. Pick the number column to count in bins."
    expect(run()).toBeDisabled()
    expect(run()).toHaveAttribute("title", why)
    expect(stale()).toHaveTextContent(`This chart is out of date: the fields changed since it was drawn. ${why}`)

    fireEvent.click(await tile("Scatter"))
    expect(run()).toHaveAttribute("title", "plan is a text column. Pick the number column for the x axis.")
    fireEvent.click(await tile("Box plot"))
    expect(run()).toHaveAttribute("title", "day is a date column. Pick the number column to draw the boxes of.")
    fireEvent.click(run())
    expect(api.previewChart).toHaveBeenCalledTimes(1)
    // A calendar reads a text column as a date, as the server does (CheckKinds): it is
    // tried, and the database says if it cannot.
    fireEvent.click(await tile("Calendar heatmap"))
    expect(run()).toBeEnabled()
    expect(run()).not.toHaveAttribute("title")
  })

  it("names a point's masked number, and a colour that is not text or yes-or-no", async () => {
    await open("Bar")
    pick("X axis", "amount")
    pick("Measure", "count")
    pick("Series (optional)", "amount")
    fireEvent.click(await tile("Scatter"))
    pick("Y axis", "plan")
    expect(run()).toHaveAttribute("title", "plan is a text column. Pick the number column for the y axis.")
    // As a saved chart's kept pick would be: the picker offers neither.
    pick("Y axis", "salary")
    expect(run()).toHaveAttribute("title", "salary is masked, so its values are not read. Pick the number column for the y axis.")
    pick("Y axis", "fee")
    // A yes-or-no column colours points too, so the words name both kinds it takes.
    expect(run()).toHaveAttribute("title", "amount is a number column. Pick a text or yes-or-no column for the colour, or none.")
    pick("Colour (optional)", "plan")
    expect(run()).toBeEnabled()
    fireEvent.click(run())
    expect(sent().roles).toMatchObject({ x: [{ column: "amount" }], y: [{ column: "fee" }], series: [{ column: "plan" }] })
  })

  // A chart draws a Lowest or a Highest of numbers only (a table shows any column's); a
  // Sum or an Average picked after keeps the column, and names it in its own words.
  it("names a measure's column a lowest, a sum or an average cannot take, kept as the measure changes", async () => {
    await open("Bar")
    pick("X axis", "day")
    pick("Measure", "min")
    fireEvent.change(document.getElementById("chart-measure-column")!, { target: { value: "plan" } })
    expect(run()).toBeDisabled()
    expect(run()).toHaveAttribute(
      "title",
      "plan is a text column, and this chart draws the lowest only of numbers: a table or a pivot table shows it as it is. Pick the column to measure.",
    )

    pick("Measure", "sum")
    expect(run()).toBeDisabled()
    expect(run()).toHaveAttribute("title", "plan is a text column, and a sum needs numbers. Pick the column to measure.")
    pick("Measure", "avg")
    expect(run()).toHaveAttribute("title", "plan is a text column, and an average needs numbers. Pick the column to measure.")
    expect(screen.getByText(/^plan is a text column, and an average needs numbers/)).toBeInTheDocument()
    fireEvent.click(run())
    expect(api.previewChart).not.toHaveBeenCalled()

    fireEvent.change(document.getElementById("chart-measure-column")!, { target: { value: "amount" } })
    fireEvent.click(run())
    expect(sent().roles.measure).toEqual([{ aggregate: "avg", column: "amount" }])
  })

  it("marks a chart drawn from fields since changed, and says why Run is off beside it", async () => {
    await open("Bar")
    pick("X axis", "plan")
    pick("Measure", "avg")
    fireEvent.change(document.getElementById("chart-measure-column")!, { target: { value: "amount" } })
    fireEvent.click(run())
    await drawn(1)
    expect(stale()).toBeNull()

    // A donut keeps the average, which a slice of a whole cannot be.
    fireEvent.click(await tile("Donut"))
    expect(stale()).toHaveTextContent(
      "This chart is out of date: the fields changed since it was drawn. Pick the column whose values are the slices.",
    )
    pick("Slices", "plan")
    expect(stale()).toHaveTextContent(
      "This chart is out of date: the fields changed since it was drawn. A slice is a share of a whole: measure a count or a sum.",
    )
    expect(stale()).toHaveAttribute("role", "status")
    pick("Measure", "count")
    expect(stale()).toHaveTextContent("This chart is out of date: the fields changed since it was drawn. Run it to draw them.")
    fireEvent.click(run())
    await drawn(2)
    await waitFor(() => expect(stale()).toBeNull())

    // A description is read to a screen reader, never drawn.
    fireEvent.change(screen.getByLabelText(/^Description/), { target: { value: "Plans by count" } })
    expect(stale()).toBeNull()
    // Back to the fields that drew it: nothing is out of date.
    pick("Measure", "sum")
    expect(stale()).not.toBeNull()
    pick("Measure", "count")
    expect(stale()).toBeNull()
  })
})
