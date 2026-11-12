import { readdirSync, readFileSync } from "node:fs"
import { resolve } from "node:path"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, render, waitFor } from "@testing-library/react"

import { ChartBoundary } from "@/components/charts/ChartBoundary"
import { EChart } from "@/components/charts/EChart"
import { chartModules } from "@/components/charts/chart-modules"
import type { ChartProps } from "@/components/charts/chart-settings"

// Every chart type drawn by ECharts for real, on a page that has loaded none of the
// modules past the bars and lines every chart registers: what a dashboard, a saved chart
// and the builder's first chart of a type draw on. ECharts copies the layouts registered
// so far into a chart when it makes one, so a chart made before its type's modules loaded
// laid none of it out: a pie, a treemap, a map, a box plot and a sankey threw, and a
// scatter drew its axes and no points (found on prod after D4e). Each has to draw as the
// same chart made once they had loaded. The charts are the golden files' (chart-golden.test.ts),
// one or more of each type.

const DIR = resolve(__dirname, "__goldens__/charts")
const CHARTS: { name: string; props: ChartProps }[] = readdirSync(DIR)
  .filter((f) => f.endsWith(".view.json"))
  .map((f) => ({ name: f.slice(0, -".view.json".length), view: JSON.parse(readFileSync(resolve(DIR, f), "utf8")) }))
  .filter(({ view }) => view.kind === "echart")
  .map(({ name, view }) => ({ name, props: { settings: view.settings, rows: view.rows } }))

// jsdom lays nothing out (echart-draws.test.tsx): each box is 400 wide, the height is given
// to init, and text is measured by estimate. No motion, so each chart is drawn at its end.
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
  vi.stubGlobal("matchMedia", (query: string) => ({ matches: query === "(prefers-reduced-motion: reduce)" }))
})

afterEach(() => {
  delete (HTMLElement.prototype as { clientWidth?: number }).clientWidth
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

/** Every chart, each in the boundary EChartNoSSR puts round it. */
const page = () => (
  <>
    {CHARTS.map(({ name, props }) => (
      <div key={name} data-chart={name}>
        <ChartBoundary resetKeys={[props.settings, props.rows]}>
          <EChart {...props} />
        </ChartBoundary>
      </div>
    ))}
  </>
)

/** Each chart's drawn shapes, by name. */
const shapes = (root: HTMLElement) =>
  Object.fromEntries(CHARTS.map(({ name }) => [name, [...root.querySelectorAll(`[data-chart="${name}"] svg path`)].map((p) => p.getAttribute("d"))]))

const counts = (drawn: Record<string, unknown[]>) => Object.fromEntries(Object.entries(drawn).map(([name, d]) => [name, d.length]))

describe("every chart type", () => {
  it("covers each type the golden files draw with ECharts", () => {
    const types = new Set(CHARTS.map((c) => c.props.settings.type))
    for (const type of ["pie", "donut", "funnel", "scatter", "bubble", "bullet", "treemap", "sunburst", "sankey", "boxplot", "candlestick", "calendar", "map"]) {
      expect(types, type).toContain(type)
    }
  })

  it("draws on a page that has loaded none of its modules as on one that has", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {})
    // Made before any type's modules load, as on a fresh page.
    const cold = render(page())
    expect(cold.container.querySelectorAll("[_echarts_instance_]")).toHaveLength(CHARTS.length)
    await act(() => Promise.all(CHARTS.map(({ props }) => chartModules(props.settings.type))))
    // Made once they have: the drawing each chart should have.
    const warm = render(page())

    await waitFor(() => {
      const want = shapes(warm.container)
      for (const [name, d] of Object.entries(want)) expect(d.length, name).toBeGreaterThan(0)
      const got = shapes(cold.container)
      expect(counts(got)).toEqual(counts(want))
      expect(got).toEqual(want)
    })
    expect(cold.container.textContent).not.toContain("could not be drawn")
  })
})
