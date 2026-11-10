import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, render, screen, waitFor } from "@testing-library/react"

import { ChartBoundary } from "@/components/charts/ChartBoundary"
import { CHART_HEIGHT, EChart } from "@/components/charts/EChart"
import type { ChartSettings } from "@/components/charts/chart-settings"
import { PIPELINE_OUTCOMES, countChart, countRows, rowsWrittenChart, rowsWrittenRows } from "@/components/runs/run-chart-specs"

// One chart: how it is handed to ECharts. Pins the modules it registers, that it
// draws as SVG at a fixed height, that new settings start the chart over while new
// rows or a new theme merge into it, that the colours are the tokens for the theme
// <html> has, that it is disposed of, and that a chart that cannot be drawn says
// so in its card. ECharts draws for real in echart-draws.test.tsx, and the
// ordering against next-themes is echart-theme-switch.test.tsx.

const m = vi.hoisted(() => ({
  chart: { setOption: vi.fn(), resize: vi.fn(), dispose: vi.fn(), on: vi.fn(), off: vi.fn(), isDisposed: vi.fn(() => false) },
  init: vi.fn(),
  use: vi.fn(),
  shapesFail: false,
  resized: () => {},
  observe: vi.fn(),
  disconnect: vi.fn(),
}))

vi.mock("echarts/core", () => ({ init: m.init, use: m.use }))
vi.mock("echarts/charts", () => ({
  BarChart: "BarChart",
  LineChart: "LineChart",
  HeatmapChart: "HeatmapChart",
  PieChart: "PieChart",
  FunnelChart: "FunnelChart",
  ScatterChart: "ScatterChart",
  TreemapChart: "TreemapChart",
  SunburstChart: "SunburstChart",
}))
vi.mock("echarts/components", () => ({
  GridComponent: "GridComponent",
  TooltipComponent: "TooltipComponent",
  VisualMapPiecewiseComponent: "VisualMapPiecewiseComponent",
}))
vi.mock("echarts/renderers", () => ({ SVGRenderer: "SVGRenderer" }))
vi.mock("echarts/features", () => ({ LabelLayout: "LabelLayout" }))
// A pie's modules as a network that drops their chunk would leave them.
vi.mock("@/components/charts/chart-modules", async (actual) => {
  const real = await actual<typeof import("@/components/charts/chart-modules")>()
  return {
    ...real,
    chartModules: (type: Parameters<typeof real.chartModules>[0]) =>
      m.shapesFail && type === "pie" ? Promise.reject(new Error("chunk failed")) : real.chartModules(type),
  }
})

// Registered when EChart.tsx loads, before any test clears the mocks.
const registered = m.use.mock.calls.flat(2)

const LIGHT = { "--chart-succeeded": "142 76% 36%", "--chart-failed": "0 72% 51%", "--chart-cancelled": "240 5% 65%", "--chart-waiting": "38 92% 50%", "--chart-running": "221 83% 53%" }
const DARK = { "--chart-succeeded": "142 69% 58%", "--chart-failed": "0 91% 71%", "--chart-cancelled": "240 5% 46%", "--chart-waiting": "43 96% 56%", "--chart-running": "213 94% 68%" }

function setTokens(tokens: Record<string, string>) {
  for (const [k, v] of Object.entries(tokens)) document.documentElement.style.setProperty(k, v)
}

const DAYS = ["2026-10-03", "2026-10-04"]
const SETTINGS = countChart("Pipeline runs per day, by outcome", DAYS, PIPELINE_OUTCOMES)
const ROWS = countRows([{ day: "2026-10-03", status: "failed", count: 2 }], PIPELINE_OUTCOMES)

let reduceMotion = false

beforeEach(() => {
  vi.clearAllMocks()
  m.init.mockReturnValue(m.chart)
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor(callback: () => void) {
        m.resized = callback
      }
      observe = m.observe
      disconnect = m.disconnect
    },
  )
  reduceMotion = false
  m.shapesFail = false
  vi.stubGlobal("matchMedia", (query: string) => ({ matches: reduceMotion && query === "(prefers-reduced-motion: reduce)" }))
  setTokens(LIGHT)
})

afterEach(() => {
  vi.unstubAllGlobals()
  document.documentElement.removeAttribute("style")
  document.documentElement.removeAttribute("class")
})

const PIE: ChartSettings = {
  type: "pie",
  description: "Customers by plan",
  slices: {
    field: "s",
    label: "Plan",
    values: [
      { key: "s0", label: "Free", color: "series-1" },
      { key: "s1", label: "Pro", color: "series-2" },
    ],
  },
  value: { field: "v", label: "Customers", format: "integer" },
}
const PIE_ROWS = [
  { s: "s0", v: 12 },
  { s: "s1", v: 5 },
]

const lastOption = () => m.chart.setOption.mock.lastCall![0]
const lastOpts = () => m.chart.setOption.mock.lastCall![1]

describe("EChart", () => {
  it("registers only what the chart types draw, and the SVG renderer", () => {
    expect(registered).toEqual([
      "BarChart",
      "LineChart",
      "HeatmapChart",
      "GridComponent",
      "TooltipComponent",
      "VisualMapPiecewiseComponent",
      "SVGRenderer",
    ])
  })

  it("draws as SVG at a fixed height into a box that says what the chart shows", () => {
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    const box = screen.getByRole("img", { name: "Pipeline runs per day, by outcome" })
    expect(m.init).toHaveBeenCalledWith(box, undefined, { renderer: "svg", height: CHART_HEIGHT })
    expect(box).toHaveStyle({ height: `${CHART_HEIGHT}px` })
    expect(m.observe).toHaveBeenCalledWith(box)
  })

  it("hands ECharts the settings' option in the light tokens, starting over", () => {
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    expect(m.chart.setOption).toHaveBeenCalledTimes(1)
    expect(lastOpts()).toEqual({ notMerge: true })
    expect(lastOption().color).toEqual([
      "hsl(142, 76%, 36%)",
      "hsl(0, 72%, 51%)",
      "hsl(240, 5%, 65%)",
      "hsl(38, 92%, 50%)",
      "hsl(221, 83%, 53%)",
    ])
    expect(lastOption().animation).toBe(true)
  })

  it("does not animate for someone who asked for less motion", () => {
    reduceMotion = true
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    expect(lastOption().animation).toBe(false)
  })

  it("merges new rows into the chart, and starts over for new settings", () => {
    const { rerender } = render(<EChart settings={SETTINGS} rows={ROWS} />)
    const more = countRows([{ day: "2026-10-04", status: "succeeded", count: 5 }], PIPELINE_OUTCOMES)
    rerender(<EChart settings={SETTINGS} rows={more} />)
    expect(m.chart.setOption).toHaveBeenCalledTimes(2)
    expect(lastOpts()).toEqual({ notMerge: false })

    rerender(<EChart settings={countChart("Other", DAYS, PIPELINE_OUTCOMES)} rows={more} />)
    expect(m.chart.setOption).toHaveBeenCalledTimes(3)
    expect(lastOpts()).toEqual({ notMerge: true })
  })

  it("merges settings equal to the drawn ones, as a page that polls hands it a new copy each time", () => {
    const { rerender } = render(<EChart settings={SETTINGS} rows={ROWS} />)
    rerender(<EChart settings={structuredClone(SETTINGS)} rows={structuredClone(ROWS)} />)
    expect(m.chart.setOption).toHaveBeenCalledTimes(2)
    expect(lastOpts()).toEqual({ notMerge: false })
  })

  it("recolours in the dark tokens when <html> turns dark, merging", async () => {
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    // The class alone, with no re-render from outside: what next-themes does last.
    await act(async () => {
      setTokens(DARK)
      document.documentElement.classList.add("dark")
    })
    expect(m.chart.setOption).toHaveBeenCalledTimes(2)
    expect(lastOpts()).toEqual({ notMerge: false })
    expect(lastOption().color.slice(0, 2)).toEqual(["hsl(142, 69%, 58%)", "hsl(0, 91%, 71%)"])
    expect(m.init).toHaveBeenCalledTimes(1)
  })

  it("lays its labels out for the width it is drawn at, and again at a new one, merging", async () => {
    const channels = ["pos", "mobile_app", "atm", "branch", "online", "ach", "system"]
    const bars = {
      type: "bar",
      description: "Amount by channel",
      stacked: false,
      total: false,
      x: { field: "x", label: "Channel", categories: channels },
      y: { field: "v", format: "integer", title: "Amount" },
      series: { values: [{ key: "", label: "Amount", color: "series-1" }] },
    } as unknown as ChartSettings
    const rows = channels.map((x, i) => ({ x, v: 412_000 - i * 50_000 }))
    let across = 1400
    const spy = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(() => across)
    // Its fixed height, which jsdom, laying nothing out, says is 0.
    const high = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(() => CHART_HEIGHT)
    render(<EChart settings={bars} rows={rows} />)
    expect(lastOption().xAxis.axisLabel).toMatchObject({ rotate: 0, interval: 0 })

    // A phone's width: the names turn to fit rather than leave some out.
    across = 317
    await act(async () => m.resized())
    expect(m.chart.resize).toHaveBeenCalled()
    expect(m.chart.setOption).toHaveBeenCalledTimes(2)
    expect(lastOpts()).toEqual({ notMerge: false })
    expect(lastOption().xAxis.axisLabel).toMatchObject({ rotate: 45, interval: 0 })

    // The same size draws nothing again.
    await act(async () => m.resized())
    expect(m.chart.setOption).toHaveBeenCalledTimes(2)
    spy.mockRestore()
    high.mockRestore()
  })

  it("lays a treemap's parts out for the height it is drawn at", async () => {
    const treemap = {
      type: "treemap",
      description: "Orders by category and city",
      levels: ["Category", "City"],
      value: { label: "Orders", format: "integer" },
      groups: [],
    } as unknown as ChartSettings
    const rows = [
      { path: ["Groceries", "Leeds"], value: 60 },
      { path: ["Groceries", "York"], value: 32 },
      { path: ["Travel", "Leeds"], value: 4 },
      { path: ["Travel", "York"], value: 4 },
    ]
    let tall = 300
    const wide = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(() => 400)
    const high = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(() => tall)
    const travel = () => lastOption().series[0].data.find((n: { name: string }) => n.name === "Travel")
    const first = render(<EChart settings={treemap} rows={rows} height="fill" />)
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalled())
    // A group of 8 in 100 is a column 32 px wide at 400x300, 98 px as a square: room to
    // name it along its top.
    expect(travel().upperLabel).toBeUndefined()
    first.unmount()

    // At 400x100 it is 32x100 px, 57 px as a square: its name would run over its neighbours'.
    tall = 100
    m.chart.setOption.mockClear()
    render(<EChart settings={treemap} rows={rows} height="fill" />)
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalled())
    expect(travel().upperLabel).toEqual({ show: false })
    wide.mockRestore()
    high.mockRestore()
  })

  it("lays its parts out again for a new height alone, merging", async () => {
    const treemap = {
      type: "treemap",
      description: "Orders by category and city",
      levels: ["Category", "City"],
      value: { label: "Orders", format: "integer" },
      groups: [],
    } as unknown as ChartSettings
    const rows = [
      { path: ["Groceries", "Leeds"], value: 60 },
      { path: ["Groceries", "York"], value: 32 },
      { path: ["Travel", "Leeds"], value: 4 },
      { path: ["Travel", "York"], value: 4 },
    ]
    let tall = 300
    const wide = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(() => 400)
    const high = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(() => tall)
    const travel = () => lastOption().series[0].data.find((n: { name: string }) => n.name === "Travel")
    render(<EChart settings={treemap} rows={rows} height="fill" />)
    // Its first resize, as the page lays it out, at the size it was drawn at.
    await act(async () => m.resized())
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalled())
    const drawn = m.chart.setOption.mock.calls.length
    expect(travel().upperLabel).toBeUndefined()

    // A dashboard's row made shorter: the group's name no longer fits along its top.
    tall = 100
    await act(async () => m.resized())
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalledTimes(drawn + 1))
    expect(lastOpts()).toEqual({ notMerge: false })
    expect(travel().upperLabel).toEqual({ show: false })

    // The same size again draws nothing again.
    await act(async () => m.resized())
    expect(m.chart.setOption).toHaveBeenCalledTimes(drawn + 1)
    wide.mockRestore()
    high.mockRestore()
  })

  // ECharts lays a map out in a box 0 across or 0 high with a scale of 0, and the inverse
  // of its transform throws: on prod a map's box squeezed to 0 px logged an uncaught
  // TypeError. A box with no size is neither resized nor drawn into.
  it("is neither resized nor drawn into while its box has no size, and is drawn again when it has one", async () => {
    let [across, tall] = [582, 360]
    const wide = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(() => across)
    const high = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(() => tall)
    const { rerender } = render(<EChart settings={SETTINGS} rows={ROWS} />)
    await act(async () => m.resized())

    for (const [w, h] of [[0, 360], [582, 0]]) {
      m.chart.resize.mockClear()
      const drawn = m.chart.setOption.mock.calls.length
      ;[across, tall] = [w, h]
      await act(async () => m.resized())
      // New rows while hidden, as a dashboard's filter changes them.
      rerender(<EChart settings={SETTINGS} rows={structuredClone(ROWS)} />)
      expect(m.chart.resize).not.toHaveBeenCalled()
      expect(m.chart.setOption).toHaveBeenCalledTimes(drawn)

      // Back at the size it had: resized and drawn again, merging, so the new rows show.
      ;[across, tall] = [582, 360]
      await act(async () => m.resized())
      expect(m.chart.resize).toHaveBeenCalledTimes(1)
      expect(m.chart.setOption).toHaveBeenCalledTimes(drawn + 1)
      expect(lastOpts()).toEqual({ notMerge: false })
    }
    wide.mockRestore()
    high.mockRestore()
  })

  it("is disposed of on unmount", () => {
    const { unmount } = render(<EChart settings={SETTINGS} rows={ROWS} />)
    unmount()
    expect(m.disconnect).toHaveBeenCalledTimes(1)
    expect(m.chart.dispose).toHaveBeenCalledTimes(1)
  })

  // Run detail and the pipeline's Overview open a run from its bar.
  it("hands a clicked mark's category index to onCategoryClick, the latest one given", () => {
    const first = vi.fn()
    const { rerender, unmount } = render(<EChart settings={SETTINGS} rows={ROWS} onCategoryClick={first} />)
    expect(m.chart.on).toHaveBeenCalledTimes(1)
    const [event, handler] = m.chart.on.mock.calls[0]
    expect(event).toBe("click")
    handler({ dataIndex: 1 })
    expect(first).toHaveBeenCalledWith(1)

    const second = vi.fn()
    rerender(<EChart settings={SETTINGS} rows={ROWS} onCategoryClick={second} />)
    // A new callback is not a new handler.
    expect(m.chart.on).toHaveBeenCalledTimes(1)
    handler({ dataIndex: 0 })
    expect(second).toHaveBeenCalledWith(0)
    expect(first).toHaveBeenCalledTimes(1)
    handler({})
    expect(second).toHaveBeenCalledTimes(1)
    unmount()
  })

  it("listens for no click when nothing opens", () => {
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    expect(m.chart.on).not.toHaveBeenCalled()
  })

  it("names each series under a chart of more than one, and none under a chart of one", () => {
    const { rerender } = render(<EChart settings={SETTINGS} rows={ROWS} />)
    const legend = screen.getByTestId("chart-legend")
    expect(legend).toHaveAttribute("aria-hidden", "true")
    expect(legend).toHaveClass("flex-wrap")
    expect([...legend.querySelectorAll("li")].map((li) => li.textContent)).toEqual(PIPELINE_OUTCOMES.map((o) => o.label))

    rerender(<EChart settings={rowsWrittenChart("Rows written by pipeline runs per day", DAYS)} rows={rowsWrittenRows([{ day: "2026-10-03", rows: 1200 }])} />)
    expect(screen.queryByTestId("chart-legend")).toBeNull()
  })

  it("says under a treemap how many of its parts are under a pixel at the size it is drawn", async () => {
    const treemap = { type: "treemap", description: "Rows by merchant", levels: ["Merchant"], value: { label: "Rows", format: "integer" }, groups: [] } as unknown as ChartSettings
    // One merchant of a million rows and 199 of one row each.
    const rows = Array.from({ length: 200 }, (_, i) => ({ path: [`Merchant ${i}`], value: i === 0 ? 1_000_000 : 1 }))
    const wide = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockImplementation(() => 400)
    const high = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(() => 300)
    const { rerender } = render(<EChart settings={treemap} rows={rows} />)
    await act(async () => m.resized())
    expect(screen.getByTestId("chart-size-note")).toHaveTextContent("199 parts are under a pixel at this size, too small to see.")
    // Parts that can all be seen say nothing.
    rerender(<EChart settings={treemap} rows={rows.slice(0, 1)} />)
    expect(screen.queryByTestId("chart-size-note")).toBeNull()
    wide.mockRestore()
    high.mockRestore()
  })

  it("gives a screen reader the numbers as a table", () => {
    render(<EChart settings={SETTINGS} rows={ROWS} />)
    const table = screen.getByRole("table", { name: "Pipeline runs per day, by outcome" })
    // A table ignores sr-only's 1px width and its overflow, so on the table itself
    // it kept its full width and pushed a phone's page sideways. A block wrapper
    // honours both and clips it.
    expect(table).not.toHaveClass("sr-only")
    expect(table.parentElement!.tagName).toBe("DIV")
    expect(table.parentElement).toHaveClass("sr-only")
    const cells = (day: string) => [...screen.getByRole("rowheader", { name: day }).parentElement!.children].map((c) => c.textContent)
    // Day, then the outcomes in order: Failed is the second.
    expect(cells("Oct 3")).toEqual(["Oct 3", "", "2", "", "", ""])
    expect(cells("Oct 4")).toEqual(["Oct 4", "", "", "", "", ""])
  })
})

describe("a type whose modules load with its first chart", () => {
  it("is drawn once they are registered, and the first draw registers them", async () => {
    render(<EChart settings={PIE} rows={PIE_ROWS} onCategoryClick={() => {}} />)
    expect(m.chart.setOption).not.toHaveBeenCalled()
    // Outside act, which would hold the chart made again back until it returned.
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalledTimes(1))
    expect(m.use).toHaveBeenCalledWith(["PieChart", "FunnelChart", "LabelLayout"])
    expect(lastOption().series[0].type).toBe("pie")
    // Made before they registered, the chart would not lay a pie out: it is made again,
    // and drawn and listened to there.
    expect(m.init).toHaveBeenCalledTimes(2)
    expect(m.chart.dispose).toHaveBeenCalledTimes(1)
    expect(m.chart.on).toHaveBeenCalledTimes(2)
    expect(lastOpts()).toEqual({ notMerge: true })
  })

  it("is made once when its modules registered before it", async () => {
    render(<EChart settings={PIE} rows={PIE_ROWS} />)
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalledTimes(1))
    expect(m.init).toHaveBeenCalledTimes(1)
    expect(m.chart.dispose).not.toHaveBeenCalled()
  })

  it("loads a scatter's points with its first one", async () => {
    const scatter: ChartSettings = {
      type: "scatter",
      description: "Amount by quantity",
      points: { x: { field: "x", label: "Quantity", format: "decimal" }, y: { field: "y", label: "Amount", format: "decimal" } },
      series: { values: [{ key: "", label: "Orders", color: "series-1" }] },
    }
    render(<EChart settings={scatter} rows={[{ x: 1, y: 2 }]} />)
    await waitFor(() => expect(m.chart.setOption).toHaveBeenCalledTimes(1))
    expect(m.use).toHaveBeenCalledWith(["ScatterChart"])
    expect(lastOption().series[0].type).toBe("scatter")
  })

  it("says so in its card when they do not load", async () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {})
    m.shapesFail = true
    render(
      <ChartBoundary resetKeys={[PIE, PIE_ROWS]}>
        <EChart settings={PIE} rows={PIE_ROWS} />
      </ChartBoundary>,
    )
    expect(await screen.findByText("This chart could not be drawn.")).toBeInTheDocument()
    expect(m.chart.setOption).not.toHaveBeenCalled()
    spy.mockRestore()
  })

  it("says so in its card when they load and drawing throws", async () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {})
    m.chart.setOption.mockImplementationOnce(() => {
      throw new Error("bad option")
    })
    render(
      <ChartBoundary resetKeys={[PIE, PIE_ROWS]}>
        <EChart settings={PIE} rows={PIE_ROWS} />
      </ChartBoundary>,
    )
    expect(await screen.findByText("This chart could not be drawn.")).toBeInTheDocument()
    expect(m.chart.setOption).toHaveBeenCalledTimes(1)
    spy.mockRestore()
  })
})

describe("a chart that cannot be drawn", () => {
  it("says so in its card, and tries again with new settings", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {})
    m.chart.setOption.mockImplementationOnce(() => {
      throw new Error("bad option")
    })
    const { rerender } = render(
      <ChartBoundary resetKeys={[SETTINGS, ROWS]}>
        <EChart settings={SETTINGS} rows={ROWS} />
      </ChartBoundary>,
    )
    expect(screen.getByText("This chart could not be drawn.")).toBeInTheDocument()
    expect(spy).toHaveBeenCalledWith("[charts] could not draw a chart", expect.any(Error))
    expect(m.chart.dispose).toHaveBeenCalled()

    const next = countChart("Pipeline runs per day, by outcome", ["2026-10-04"], PIPELINE_OUTCOMES)
    rerender(
      <ChartBoundary resetKeys={[next, ROWS]}>
        <EChart settings={next} rows={ROWS} />
      </ChartBoundary>,
    )
    expect(screen.queryByText("This chart could not be drawn.")).toBeNull()
    expect(screen.getByRole("img", { name: "Pipeline runs per day, by outcome" })).toBeInTheDocument()
    spy.mockRestore()
  })
})
