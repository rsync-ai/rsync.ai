import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"

import { ChartBoundary } from "@/components/charts/ChartBoundary"
import { EChart } from "@/components/charts/EChart"
import type { ChartProps, ChartSettings } from "@/components/charts/chart-settings"
import { PIPELINE_OUTCOMES, countChart, countRows, runTimeChart, runTimeRows } from "@/components/runs/run-chart-specs"

// The real thing, no mocks of ECharts: a Runs chart drawn into the page as SVG, its
// bars filled with the theme's tokens, every day on the axis, and its numbers in a
// table a screen reader reads.

// jsdom lays nothing out, so every element is 0 wide; ECharts draws nothing into a
// 0-wide box. The height is given to init.
// No canvas either: ECharts measures text with an estimate instead, and getContext
// returning null says so without jsdom's "not implemented" on every label.
beforeEach(() => {
  Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get: () => 400 })
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null)
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  )
  const root = document.documentElement.style
  root.setProperty("--chart-succeeded", "142 76% 36%")
  root.setProperty("--chart-failed", "0 72% 51%")
  root.setProperty("--chart-cancelled", "240 5% 65%")
  root.setProperty("--chart-waiting", "38 92% 50%")
  root.setProperty("--chart-running", "221 83% 53%")
  root.setProperty("--chart-series-1", "226.1 68.3% 59.2%")
  root.setProperty("--chart-series-2", "27 73.2% 51.8%")
})

afterEach(() => {
  delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  document.documentElement.removeAttribute("style")
})

const DAYS = ["2026-10-03", "2026-10-04", "2026-10-05"]

/** The fills of the drawn marks, as the SVG has them. */
const fills = (container: HTMLElement) =>
  [...container.querySelectorAll("svg path")].map((p) => p.getAttribute("fill")).filter((f): f is string => !!f && f !== "none")

describe("a Runs chart drawn by ECharts", () => {
  it("is an SVG whose bars are filled with the outcome tokens, with every day on the axis", async () => {
    const settings = countChart("Pipeline runs per day, by outcome", DAYS, PIPELINE_OUTCOMES)
    const rows = countRows(
      [
        { day: "2026-10-03", status: "succeeded", count: 3 },
        { day: "2026-10-05", status: "failed", count: 1 },
      ],
      PIPELINE_OUTCOMES,
    )
    const { container } = render(<EChart settings={settings} rows={rows} />)

    const chart = screen.getByRole("img", { name: "Pipeline runs per day, by outcome" })
    await waitFor(() => expect(chart.querySelector("svg")).not.toBeNull())
    expect(fills(container)).toContain("hsl(142, 76%, 36%)")
    expect(fills(container)).toContain("hsl(0, 72%, 51%)")
    // Oct 4 has no runs and still has its place.
    for (const day of ["Oct 3", "Oct 4", "Oct 5"]) expect(chart.textContent).toContain(day)
    // The legend names every outcome, the ones with no runs too.
    expect(screen.getByTestId("chart-legend").textContent).toContain("Waiting for you")
    // A screen reader reads the numbers from the table.
    const table = screen.getByRole("table")
    expect(table.parentElement).toHaveClass("sr-only")
    expect(screen.getByRole("rowheader", { name: "Oct 3" }).parentElement!.textContent).toContain("3")
    expect(container.textContent).not.toContain("could not be drawn")
  })

  it("draws a run-time line with a gap on the day nothing finished", async () => {
    const settings = runTimeChart("Median and 95th-percentile pipeline run time per day, in seconds", DAYS)
    const rows = runTimeRows(DAYS, [
      { day: "2026-10-03", runs: 2, median_seconds: 40, p95_seconds: 58 },
      { day: "2026-10-05", runs: 1, median_seconds: 12.5, p95_seconds: 12.5 },
    ])
    const { container } = render(<EChart settings={settings} rows={rows} />)

    const chart = screen.getByRole("img", { name: /run time per day/ })
    await waitFor(() => expect(chart.querySelector("svg")).not.toBeNull())
    expect(chart.textContent).toContain("Seconds")
    const strokes = [...container.querySelectorAll("svg path")].map((p) => p.getAttribute("stroke"))
    expect(strokes).toContain("hsl(226.1, 68.3%, 59.2%)")
    expect(strokes).toContain("hsl(27, 73.2%, 51.8%)")
    const median = [...container.querySelectorAll("svg path")].find(
      (p) => p.getAttribute("stroke") === "hsl(226.1, 68.3%, 59.2%)" && p.getAttribute("fill") === "none",
    )!
    // Oct 3 and Oct 5, each its own point: no segment is drawn across Oct 4.
    const d = median.getAttribute("d")!
    expect(d.match(/M/g)).toHaveLength(2)
    expect(d).not.toMatch(/[LC]/)
    expect(screen.getByRole("rowheader", { name: "Oct 5" }).parentElement!.textContent).toContain("12.5")
  })
})

describe("a chart whose type changes", () => {
  it("draws a pie where it drew bars, as the builder does when its first pie of a page replaces a bar chart", async () => {
    // Bars, drawn by the modules every chart registers: the chart is made before a pie's load.
    const bars = countChart("Pipeline runs per day, by outcome", DAYS, PIPELINE_OUTCOMES)
    const barRows = countRows([{ day: "2026-10-03", status: "succeeded", count: 3 }], PIPELINE_OUTCOMES)
    const pie: ChartSettings = {
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
    const pieRows = [
      { s: "s0", v: 12 },
      { s: "s1", v: 5 },
    ]
    const spy = vi.spyOn(console, "error").mockImplementation(() => {})
    // The boundary EChartNoSSR puts round it, which a pie that throws drawing reaches.
    const drawn = ({ settings, rows }: ChartProps) => (
      <ChartBoundary resetKeys={[settings, rows]}>
        <EChart settings={settings} rows={rows} />
      </ChartBoundary>
    )
    const { container, rerender } = render(drawn({ settings: bars, rows: barRows }))
    await waitFor(() => expect(screen.getByRole("img", { name: /Pipeline runs/ }).querySelector("svg")).not.toBeNull())

    rerender(drawn({ settings: pie, rows: pieRows }))
    // A pie's slices are unlabelled, in the series tokens the bars did not use.
    await waitFor(() => {
      const fill = fills(screen.getByRole("img", { name: "Customers by plan" }))
      expect(fill.filter((f) => f === "hsl(226.1, 68.3%, 59.2%)")).toHaveLength(1)
      expect(fill.filter((f) => f === "hsl(27, 73.2%, 51.8%)")).toHaveLength(1)
    })
    expect(container.textContent).not.toContain("could not be drawn")
    expect(spy).not.toHaveBeenCalled()
  })
})
