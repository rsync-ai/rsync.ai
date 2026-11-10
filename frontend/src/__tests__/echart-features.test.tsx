// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs"
import { resolve } from "node:path"
import { describe, expect, it, vi } from "vitest"

import {
  BarChart,
  BoxplotChart,
  CandlestickChart,
  FunnelChart,
  HeatmapChart,
  LineChart,
  MapChart,
  PieChart,
  SankeyChart,
  ScatterChart,
  SunburstChart,
  TreemapChart,
} from "echarts/charts"
import { CalendarComponent, GridComponent, TooltipComponent, VisualMapPiecewiseComponent } from "echarts/components"
import { LabelLayout, UniversalTransition } from "echarts/features"

import { chartOption } from "@/components/charts/chart-option"
import type { ChartSettings } from "@/components/charts/chart-settings"
import { printChartTheme } from "@/lib/export/print-theme"

// What each chart type's option asks of ECharts, against what is registered where it is
// drawn: the page (EChart.tsx and the type's group, chart-modules.ts), the image the page
// downloads (chart-image.ts and the group) and the server's SVG (chart-svg.ts). ECharts
// leaves out what it was never handed: a funnel's labelLayout did nothing on prod, as the
// one echarts/core registers by itself is a module the bundler drops. Under vitest that
// module still runs, so a chart drawn here cannot show it; what each file registers can.

const registered = vi.hoisted(() => new Set<unknown>())
vi.mock("echarts/core", async (importOriginal) => {
  const core = await importOriginal<typeof import("echarts/core")>()
  return {
    ...core,
    use: (modules: Parameters<typeof core.use>[0]) => {
      for (const m of [modules].flat()) registered.add(m)
      return core.use(modules)
    },
  }
})

/** What importing these files, fresh, registers. */
async function registers(...load: (() => Promise<unknown>)[]): Promise<Set<unknown>> {
  vi.resetModules()
  registered.clear()
  for (const l of load) await l()
  return new Set(registered)
}

const DIR = resolve(__dirname, "__goldens__/charts")
const CHARTS: { name: string; settings: ChartSettings; rows: object[] }[] = readdirSync(DIR)
  .filter((f) => f.endsWith(".view.json"))
  .map((f) => ({ name: f.slice(0, -".view.json".length), view: JSON.parse(readFileSync(resolve(DIR, f), "utf8")) }))
  .filter(({ view }) => view.kind === "echart")
  .map(({ name, view }) => ({ name, settings: view.settings, rows: view.rows }))

type Option = Record<string, unknown> & { series?: Record<string, unknown>[] }

const SERIES: Record<string, unknown> = {
  bar: BarChart,
  line: LineChart,
  heatmap: HeatmapChart,
  pie: PieChart,
  funnel: FunnelChart,
  scatter: ScatterChart,
  treemap: TreemapChart,
  sunburst: SunburstChart,
  sankey: SankeyChart,
  boxplot: BoxplotChart,
  candlestick: CandlestickChart,
  map: MapChart,
}

/** The modules an option needs, and whether a still picture needs each. */
function needs(option: Option): { module: unknown; name: string; still: boolean }[] {
  const series = option.series ?? []
  const has = (key: string) => key in option
  return [
    ...series.map((s) => ({ module: SERIES[String(s.type)], name: `the ${String(s.type)} series`, still: true, used: true })),
    { module: LabelLayout, name: "LabelLayout", still: true, used: series.some((s) => "labelLayout" in s) },
    { module: UniversalTransition, name: "UniversalTransition", still: true, used: series.some((s) => "universalTransition" in s) },
    { module: GridComponent, name: "GridComponent", still: true, used: has("xAxis") || has("grid") },
    { module: CalendarComponent, name: "CalendarComponent", still: true, used: has("calendar") },
    { module: VisualMapPiecewiseComponent, name: "VisualMapPiecewiseComponent", still: true, used: has("visualMap") },
    // A still picture has no tooltip (stillOption).
    { module: TooltipComponent, name: "TooltipComponent", still: false, used: has("tooltip") },
  ].filter((n) => n.used)
}

const missing = (option: Option, modules: Set<unknown>, still: boolean) =>
  needs(option)
    .filter((n) => (still ? n.still : true) && !modules.has(n.module))
    .map((n) => n.name)

describe("the ECharts modules each chart type needs", () => {
  it("are registered on the page, in its image and on the server", async () => {
    const server = await registers(() => import("@/lib/export/chart-svg"))
    // A check that can fail: the server registers bars, and nothing it was not handed.
    expect(server.has(BarChart)).toBe(true)
    expect(server.has(UniversalTransition)).toBe(false)
    const gaps: string[] = []
    for (const { name, settings, rows } of CHARTS) {
      const option = chartOption(settings, rows, printChartTheme()) as unknown as Option
      const group = async () => (await import("@/components/charts/chart-modules")).chartModules(settings.type)
      const page = await registers(() => import("@/components/charts/EChart"), group)
      const image = await registers(() => import("@/lib/export/chart-image"), group)
      for (const [where, modules, still] of [
        ["the page", page, false],
        ["its image", image, true],
        ["the server", server, true],
      ] as const) {
        for (const m of missing(option, modules, still)) gaps.push(`${name}: ${m} on ${where}`)
      }
    }
    expect(gaps).toEqual([])
  })
})

describe("a label labelLayout places at an x or a y", () => {
  // On a merge redraw (a resize, a re-Run, a new theme) the series tweens the label's own
  // place back while labelLayout has moved it: the two add up, and a funnel's names stood
  // past the right edge. A series that places its labels so draws its update at once.
  it("is not tweened on a redraw", () => {
    const placed = CHARTS.flatMap(({ name, settings, rows }) => {
      const option = chartOption(settings, rows, printChartTheme()) as unknown as Option
      return (option.series ?? [])
        .filter((s) => {
          const layout = s.labelLayout as Record<string, unknown> | undefined
          return layout !== undefined && (layout.x !== undefined || layout.y !== undefined)
        })
        .map((s) => ({ name, update: s.animationDurationUpdate }))
    })
    // A check that can fail: the funnel places its names in one column.
    expect(placed.map((p) => p.name)).toContain("funnel")
    expect(placed.filter((p) => p.update !== 0)).toEqual([])
  })
})
