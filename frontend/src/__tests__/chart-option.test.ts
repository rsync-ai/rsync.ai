import { readdirSync, readFileSync } from "node:fs"
import { resolve } from "node:path"

import { describe, expect, it } from "vitest"

import {
  chartLegend,
  chartOption,
  chartTable,
  heatmapSteps,
  seriesColors,
  stillOption,
  withAlpha,
  type ChartOption,
} from "@/components/charts/chart-option"
import type {
  AreaChartSettings,
  BarChartSettings,
  BoxplotChartSettings,
  BulletChartSettings,
  CalendarChartSettings,
  CandlestickChartSettings,
  ChartSettings,
  HBarChartSettings,
  HeatmapChartSettings,
  LineChartSettings,
  MapChartSettings,
  PartsChartSettings,
  RangeChartSettings,
  SankeyChartSettings,
  ScatterChartSettings,
  WaterfallChartSettings,
} from "@/components/charts/chart-settings"
import { CHART_COLORS, FALLBACK_COLORS, hslToken, type ChartColor, type ChartTheme } from "@/components/charts/chart-theme"

// Settings, rows and a theme as the option ECharts is handed: what each chart type
// draws, in which colours, and what its tooltip and table say. ECharts itself is
// not loaded here; echart-draws.test.tsx draws for real.

function theme(colors: Partial<Record<ChartColor, string>> = {}, rest: Partial<Omit<ChartTheme, "colors">> = {}): ChartTheme {
  const all = Object.fromEntries(CHART_COLORS.map((c) => [c, colors[c] ?? ""])) as Record<ChartColor, string>
  return { colors: all, text: "", muted: "", border: "", surface: "", popover: "", popoverText: "", font: "", ...rest }
}

const FULL = theme(
  { succeeded: "hsl(142, 76%, 36%)", failed: "hsl(0, 72%, 51%)", "series-1": "hsl(226.1, 68.3%, 59.2%)", "series-2": "hsl(27, 73.2%, 51.8%)" },
  {
    text: "hsl(240, 10%, 4%)",
    muted: "hsl(240, 4%, 46%)",
    border: "hsl(240, 6%, 90%)",
    surface: "hsl(0, 0%, 100%)",
    popover: "hsl(0, 0%, 100%)",
    popoverText: "hsl(240, 10%, 4%)",
    font: "Inter",
  },
)

const STACKED: BarChartSettings = {
  type: "bar",
  stacked: true,
  description: "Runs per day, by outcome",
  x: { field: "day", label: "Day", categories: ["Oct 3", "Oct 4", "Oct 5"] },
  y: { field: "count", format: "integer" },
  series: {
    field: "outcome",
    values: [
      { key: "Succeeded", label: "Succeeded", color: "succeeded" },
      { key: "Failed", label: "Failed", color: "failed" },
    ],
  },
}

const COUNTS = [
  { day: "Oct 3", outcome: "Succeeded", count: 3 },
  { day: "Oct 3", outcome: "Failed", count: 1 },
  { day: "Oct 5", outcome: "Succeeded", count: 2 },
  // Not drawn: a day off the axis, and an outcome the chart does not have.
  { day: "Oct 9", outcome: "Succeeded", count: 7 },
  { day: "Oct 4", outcome: "Skipped", count: 4 },
]

const LINE: LineChartSettings = {
  type: "line",
  description: "Run time per day",
  x: { field: "day", label: "Day", categories: ["Oct 3", "Oct 4", "Oct 5"] },
  y: { field: "seconds", format: "decimal", title: "Seconds" },
  series: {
    field: "measure",
    values: [
      { key: "Median", label: "Median", color: "series-1" },
      { key: "95th percentile", label: "95th percentile", color: "series-2" },
    ],
  },
  details: [{ field: "runs", label: "Finished runs", format: "integer" }],
}

const TIMES = [
  { day: "Oct 3", measure: "Median", seconds: 40, runs: 2 },
  { day: "Oct 3", measure: "95th percentile", seconds: 58.25, runs: 2 },
  { day: "Oct 4", measure: "Median", seconds: null, runs: 0 },
  { day: "Oct 4", measure: "95th percentile", seconds: null, runs: 0 },
  { day: "Oct 5", measure: "Median", seconds: 12.5, runs: 1 },
  { day: "Oct 5", measure: "95th percentile", seconds: 12.5, runs: 1 },
]

interface Bar {
  stack?: string
  barMinHeight?: number
  itemStyle: { color: string; borderColor?: string; borderWidth?: number }
  data: (number | null | { value: number; itemStyle: { borderRadius: number[] } })[]
}
interface Line {
  connectNulls: boolean
  lineStyle: { color: string }
  data: (number | null)[]
}
interface Axis {
  name?: string
  minInterval?: number
  axisLabel: { color?: string; formatter: (v: number) => string }
}
type Formatter = (points: { dataIndex: number }[]) => string

// The option's shapes as these tests read them: ECharts' own types are unions of
// every form an option may take.
const parts = (option: ReturnType<typeof chartOption>) => {
  const o = option as unknown as {
    color: string[]
    grid: { top: number }
    yAxis: Axis
    tooltip: { formatter: Formatter; axisPointer: { type: string } }
    series: unknown[]
  }
  return { ...o, bars: o.series as Bar[], lines: o.series as Line[] }
}

/** Every path to an undefined value in the option. */
function undefinedPaths(value: unknown, path = "option"): string[] {
  if (value === undefined) return [path]
  if (value === null || typeof value !== "object") return []
  return Object.entries(value).flatMap(([k, v]) => undefinedPaths(v, `${path}.${k}`))
}

describe("a stacked bar chart", () => {
  it("stacks the series in the theme's colours, rounding only the top of each stack", () => {
    const { color, bars, tooltip } = parts(chartOption(STACKED, COUNTS, FULL))
    expect(color).toEqual(["hsl(142, 76%, 36%)", "hsl(0, 72%, 51%)"])
    expect(bars.map((b) => b.stack)).toEqual(["total", "total"])
    expect(tooltip.axisPointer.type).toBe("shadow")
    const round = (value: number) => ({ value, itemStyle: { borderRadius: [3, 3, 0, 0] } })
    // Oct 3: Failed sits on Succeeded, so Failed is the top. Oct 5: Succeeded alone is.
    expect(bars[0].data).toEqual([3, null, round(2)])
    expect(bars[1].data).toEqual([round(1), null, null])
  })

  it("parts stacked segments with the card's colour", () => {
    const { bars } = parts(chartOption(STACKED, COUNTS, FULL))
    for (const b of bars) expect(b.itemStyle).toMatchObject({ borderColor: "hsl(0, 0%, 100%)", borderWidth: 1 })
  })

  it("leaves zero out of the rounding, so the top is the highest segment with a value", () => {
    const rows = [
      { day: "Oct 3", outcome: "Succeeded", count: 3 },
      { day: "Oct 3", outcome: "Failed", count: 0 },
    ]
    const { bars } = parts(chartOption(STACKED, rows, FULL))
    expect(bars[0].data[0]).toEqual({ value: 3, itemStyle: { borderRadius: [3, 3, 0, 0] } })
    expect(bars[1].data[0]).toBe(0)
  })
})

// Run detail's last runs chart outlines the run the page is about.
describe("a highlighted category", () => {
  it("outlines that category's segments in the text colour, and keeps the top's rounding", () => {
    const { bars } = parts(chartOption({ ...STACKED, highlight: "Oct 3" }, COUNTS, FULL))
    const outline = { borderColor: "hsl(240, 10%, 4%)", borderWidth: 2 }
    expect(bars[0].data[0]).toEqual({ value: 3, itemStyle: outline })
    expect(bars[1].data[0]).toEqual({ value: 1, itemStyle: { borderRadius: [3, 3, 0, 0], ...outline } })
    // Other categories are drawn as before.
    expect(bars[0].data[2]).toEqual({ value: 2, itemStyle: { borderRadius: [3, 3, 0, 0] } })
  })

  it("outlines nothing when the category is not on the axis", () => {
    const plain = parts(chartOption(STACKED, COUNTS, FULL)).bars.map((b) => b.data)
    expect(parts(chartOption({ ...STACKED, highlight: "Oct 9" }, COUNTS, FULL)).bars.map((b) => b.data)).toEqual(plain)
  })
})

// A model's or a workflow's runs: a skipped run, or one of a few ms, beside one of minutes.
describe("a least bar height", () => {
  it("draws a value of 0 that tall and leaves a category no row has undrawn", () => {
    const { bars } = parts(chartOption({ ...STACKED, minBarHeight: 3 }, [{ day: "Oct 3", outcome: "Succeeded", count: 0 }], FULL))
    expect(bars.map((b) => b.barMinHeight)).toEqual([3, 3])
    expect(bars[0].data[0]).toBe(0)
    expect(bars[0].data[1]).toBeNull()
  })

  it("is left to ECharts when the settings do not set one", () => {
    expect(parts(chartOption(STACKED, COUNTS, FULL)).bars.every((b) => !("barMinHeight" in b))).toBe(true)
  })
})

describe("a bar chart of one series", () => {
  const ONE: BarChartSettings = {
    type: "bar",
    stacked: false,
    description: "Rows written per day",
    x: { field: "day", label: "Day", categories: ["Oct 3", "Oct 4"] },
    y: { field: "rows", format: "integer", axisFormat: "compact" },
    series: { values: [{ key: "rows", label: "Rows written", color: "series-1" }] },
  }

  it("rounds every bar, does not stack, and labels the axis compactly", () => {
    const { bars, yAxis, color } = parts(chartOption(ONE, [{ day: "Oct 3", rows: 1_250_000 }], FULL))
    expect(color).toEqual(["hsl(226.1, 68.3%, 59.2%)"])
    expect(bars[0].stack).toBeUndefined()
    expect(bars[0].itemStyle.borderColor).toBeUndefined()
    expect(bars[0].data).toEqual([{ value: 1_250_000, itemStyle: { borderRadius: [3, 3, 0, 0] } }, null])
    // A tick reads as itself, so a 1.25M tick is not a second 1.3M.
    expect(yAxis.axisLabel.formatter(1_250_000)).toBe("1.25M")
    expect(yAxis.axisLabel.formatter(1_500_000)).toBe("1.5M")
    // Whole ticks only for an integer axis; a compact one is not.
    expect(yAxis.minInterval).toBeUndefined()
  })

  it("reads every row as the one series when the settings name no series field", () => {
    expect(chartTable(ONE, [{ day: "Oct 4", rows: 9 }]).body).toEqual([
      ["Oct 3", ""],
      ["Oct 4", "9"],
    ])
  })
})

describe("a line chart", () => {
  it("breaks the line where a day has no value, in the series colours", () => {
    const { lines, color, tooltip } = parts(chartOption(LINE, TIMES, FULL))
    expect(color).toEqual(["hsl(226.1, 68.3%, 59.2%)", "hsl(27, 73.2%, 51.8%)"])
    expect(lines.map((l) => l.connectNulls)).toEqual([false, false])
    expect(lines[0].data).toEqual([40, null, 12.5])
    expect(lines[1].data).toEqual([58.25, null, 12.5])
    expect(lines[0].lineStyle.color).toBe("hsl(226.1, 68.3%, 59.2%)")
    expect(tooltip.axisPointer.type).toBe("line")
  })

  it("leaves a gap at a day with no row, filled in as no value, and says so rather than 0", () => {
    // The app fills each missing day between the first and the last with a row of no values.
    const days = ["2026-10-08", "2026-10-09", "2026-10-10"]
    const rows = [
      { day: "2026-10-08", measure: "Median", seconds: 4 },
      { day: "2026-10-08", measure: "95th percentile", seconds: 6 },
      { day: "2026-10-09", measure: "Median", seconds: null },
      { day: "2026-10-09", measure: "95th percentile", seconds: null },
      { day: "2026-10-10", measure: "Median", seconds: 5 },
      { day: "2026-10-10", measure: "95th percentile", seconds: 7 },
    ]
    const line: LineChartSettings = { ...LINE, x: { ...LINE.x, categories: days }, details: [] }
    const area: AreaChartSettings = { ...line, type: "area", stacked: true }
    for (const settings of [line, area, { ...area, percent: true }]) {
      const { lines, tooltip } = parts(chartOption(settings, rows, FULL))
      expect(lines.map((l) => l.connectNulls)).toEqual([false, false])
      expect(lines.map((l) => l.data[1])).toEqual([null, null])
      const html = tooltip.formatter([{ dataIndex: 1 }])
      expect(html).toContain("2026-10-09")
      expect(html).toContain(">No value<")
      expect(html).not.toMatch(/>0%?</)
    }
  })

  it("names the axis and leaves room above the plot for the name", () => {
    const { yAxis, grid } = parts(chartOption(LINE, TIMES, FULL))
    expect(yAxis.name).toBe("Seconds")
    expect(grid.top).toBe(28)
    expect(parts(chartOption(STACKED, COUNTS, FULL)).grid.top).toBe(10)
    expect(parts(chartOption(STACKED, COUNTS, FULL)).yAxis.name).toBeUndefined()
  })
})

describe("the axis of an integer chart", () => {
  it("ticks only whole numbers", () => {
    const { yAxis } = parts(chartOption(STACKED, COUNTS, FULL))
    expect(yAxis.minInterval).toBe(1)
    expect(yAxis.axisLabel.formatter(1200)).toBe("1,200")
  })
})

describe("colours", () => {
  it("takes the fallback colours in order for every series when one token is unset", () => {
    const half = theme({ succeeded: "hsl(142, 76%, 36%)" })
    expect(seriesColors(STACKED, half)).toEqual([FALLBACK_COLORS[0], FALLBACK_COLORS[1]])
  })

  it("hands ECharts no undefined value when the theme has no tokens at all", () => {
    // ECharts merges an undefined over its default: an axis label with an
    // undefined colour is drawn with none.
    for (const settings of [STACKED, LINE]) {
      const option = chartOption(settings, settings === LINE ? TIMES : COUNTS, theme())
      expect(undefinedPaths(option)).toEqual([])
      expect(parts(option).yAxis.axisLabel.color).toBeUndefined()
      expect("color" in parts(option).yAxis.axisLabel).toBe(false)
    }
  })

  it("draws the axis labels in the muted text token, never a series colour", () => {
    expect(parts(chartOption(STACKED, COUNTS, FULL)).yAxis.axisLabel.color).toBe("hsl(240, 4%, 46%)")
  })
})

describe("the tooltip", () => {
  it("names the day, each series with a value, and the day's details", () => {
    const html = parts(chartOption(LINE, TIMES, FULL)).tooltip.formatter([{ dataIndex: 0 }])
    expect(html).toContain("Oct 3")
    expect(html).toContain("Median")
    expect(html).toContain(">40<")
    expect(html).toContain(">58.3<")
    expect(html).toContain("Finished runs")
    expect(html).toContain(">2<")
  })

  it("leaves out a series with no value that day", () => {
    const html = parts(chartOption(STACKED, COUNTS, FULL)).tooltip.formatter([{ dataIndex: 2 }])
    expect(html).toContain("Succeeded")
    expect(html).not.toContain("Failed")
  })

  it("escapes what the rows and settings say", () => {
    const settings: BarChartSettings = {
      ...STACKED,
      x: { ...STACKED.x, categories: ["<img src=x onerror=alert(1)>"] },
      series: { field: "outcome", values: [{ key: "a&b", label: `"a" & <b>`, color: "succeeded" }] },
    }
    const html = parts(chartOption(settings, [{ day: "<img src=x onerror=alert(1)>", outcome: "a&b", count: 1 }], FULL)).tooltip.formatter([
      { dataIndex: 0 },
    ])
    expect(html).not.toContain("<img")
    expect(html).not.toContain("<b>")
    expect(html).toContain("&lt;img src=x onerror=alert(1)&gt;")
    expect(html).toContain("&quot;a&quot; &amp; &lt;b&gt;")
  })
})

describe("the table a screen reader reads", () => {
  it("has a row per day with each series and the details, blank where there is no value", () => {
    expect(chartTable(LINE, TIMES)).toEqual({
      head: ["Day", "Median", "95th percentile", "Finished runs"],
      body: [
        ["Oct 3", "40", "58.3", "2"],
        ["Oct 4", "", "", "0"],
        ["Oct 5", "12.5", "12.5", "1"],
      ],
    })
  })

  it("leaves out rows off the axis and series the chart does not have", () => {
    expect(chartTable(STACKED, COUNTS).body).toEqual([
      ["Oct 3", "3", "1"],
      ["Oct 4", "", ""],
      ["Oct 5", "2", ""],
    ])
  })
})

describe("the legend", () => {
  it("names each series in its colour when there is more than one", () => {
    expect(chartLegend(STACKED, FULL)).toEqual([
      { label: "Succeeded", color: "hsl(142, 76%, 36%)" },
      { label: "Failed", color: "hsl(0, 72%, 51%)" },
    ])
  })

  it("is empty for one series", () => {
    expect(chartLegend({ ...STACKED, series: { field: "outcome", values: [STACKED.series.values[0]] } }, FULL)).toEqual([])
  })
})

// The types a saved chart adds (dashboards-design.md § 3.1): bars across, areas and a heatmap.

describe("a horizontal bar chart", () => {
  const HBAR: HBarChartSettings = { ...STACKED, type: "hbar", stacked: false, y: { field: "count", format: "integer", title: "Runs" } }
  const across = (option: ReturnType<typeof chartOption>) =>
    option as unknown as {
      grid: { top: number; bottom: number }
      xAxis: { type: string; name?: string; nameLocation?: string }
      yAxis: { type: string; data: string[]; inverse?: boolean; name?: string; nameLocation?: string }
      tooltip: { axisPointer: { type: string } }
      series: Bar[]
    }

  it("lays the categories down the side, the first at the top, and the values across", () => {
    const o = across(chartOption(HBAR, COUNTS, FULL))
    // The categories' title is above them: "start" is the top of an axis drawn top-down.
    expect(o.yAxis).toMatchObject({ type: "category", data: ["Oct 3", "Oct 4", "Oct 5"], inverse: true, name: "Day", nameLocation: "start" })
    expect(o.xAxis).toMatchObject({ type: "value", name: "Runs", nameLocation: "middle" })
    // The measure's title sits under the plot and the categories' above it, so the room for each is kept.
    expect(o.grid).toMatchObject({ top: 28, bottom: 28 })
    expect(o.tooltip.axisPointer.type).toBe("shadow")
  })

  it("rounds the right end of every bar when it does not stack", () => {
    const right = { borderRadius: [0, 3, 3, 0] }
    expect(across(chartOption(HBAR, COUNTS, FULL)).series.map((b) => b.data)).toEqual([
      [{ value: 3, itemStyle: right }, null, { value: 2, itemStyle: right }],
      [{ value: 1, itemStyle: right }, null, null],
    ])
  })

  it("rounds only the end of each stack when it stacks", () => {
    const [succeeded, failed] = across(chartOption({ ...HBAR, stacked: true }, COUNTS, FULL)).series
    expect(succeeded.stack).toBe("total")
    expect(succeeded.data[0]).toBe(3)
    expect(failed.data[0]).toEqual({ value: 1, itemStyle: { borderRadius: [0, 3, 3, 0] } })
    expect(succeeded.data[2]).toEqual({ value: 2, itemStyle: { borderRadius: [0, 3, 3, 0] } })
  })
})

describe("an area chart", () => {
  const AREA: AreaChartSettings = { ...LINE, type: "area", stacked: false }
  interface Area extends Line {
    stack?: string
    areaStyle?: { color: string; opacity: number }
  }
  const areas = (settings: AreaChartSettings | LineChartSettings) =>
    (chartOption(settings, TIMES, FULL) as unknown as { series: Area[] }).series

  it("fills under each line faintly, so overlapping areas show through", () => {
    const [median, p95] = areas(AREA)
    expect(median.areaStyle).toEqual({ color: "hsl(226.1, 68.3%, 59.2%)", opacity: 0.15 })
    expect(p95.areaStyle).toEqual({ color: "hsl(27, 73.2%, 51.8%)", opacity: 0.15 })
    expect(median.stack).toBeUndefined()
    // A day with no value still breaks the line.
    expect(median.connectNulls).toBe(false)
    expect(median.data).toEqual([40, null, 12.5])
  })

  it("stacks the areas, filled more strongly, when it stacks", () => {
    for (const a of areas({ ...AREA, stacked: true })) {
      expect(a.stack).toBe("total")
      expect(a.areaStyle?.opacity).toBe(0.6)
    }
  })

  it("leaves a line chart unfilled and unstacked", () => {
    for (const l of areas(LINE)) {
      expect(l.areaStyle).toBeUndefined()
      expect(l.stack).toBeUndefined()
    }
  })
})

describe("a heatmap", () => {
  const HEAT: HeatmapChartSettings = {
    type: "heatmap",
    description: "Orders by day and region",
    x: { field: "x", label: "Day", categories: ["Oct 3", "Oct 4"] },
    y: { field: "y", label: "Region", categories: ["APAC", "EU"] },
    value: { field: "v", label: "Orders", format: "integer" },
    color: "series-1",
  }
  const CELLS = [
    { x: "Oct 3", y: "APAC", v: 0 },
    { x: "Oct 3", y: "EU", v: 100 },
    { x: "Oct 4", y: "EU", v: 45 },
    // Not drawn: a day off the axis, a region the chart does not have, a cell with no number.
    { x: "Oct 9", y: "EU", v: 7 },
    { x: "Oct 4", y: "US", v: 7 },
    { x: "Oct 4", y: "APAC", v: null },
  ]
  const HUE = "hsl(226.1, 68.3%, 59.2%)"
  const heat = (option: ReturnType<typeof chartOption>) =>
    option as unknown as {
      yAxis: { data: string[]; inverse: boolean }
      visualMap: { show: boolean; dimension: number; pieces: { value: number; color: string }[] }
      tooltip: { formatter: (p: { data: number[] }) => string }
      series: { type: string; data: number[][] }[]
    }

  it("splits the values into five equal steps, smallest to largest", () => {
    expect(heatmapSteps(HEAT, CELLS)).toEqual([
      { from: 0, to: 20 },
      { from: 20, to: 40 },
      { from: 40, to: 60 },
      { from: 60, to: 80 },
      { from: 80, to: 100 },
    ])
    expect(heatmapSteps(HEAT, [])).toEqual([])
  })

  it("draws each cell a row has, with the step it is in, the first region at the top", () => {
    const o = heat(chartOption(HEAT, CELLS, FULL))
    expect(o.series[0].type).toBe("heatmap")
    // [x, y, value, step]: the largest value is in the last step, not past it.
    expect(o.series[0].data).toEqual([
      [0, 0, 0, 0],
      [0, 1, 100, 4],
      [1, 1, 45, 2],
    ])
    expect(o.yAxis).toMatchObject({ data: ["APAC", "EU"], inverse: true })
    expect(o.visualMap).toMatchObject({ show: false, dimension: 3 })
    expect(o.visualMap.pieces.map((p) => p.color)).toEqual(
      [0.2, 0.4, 0.6, 0.8, 1].map((a) => `hsla(226.1, 68.3%, 59.2%, ${a})`),
    )
  })

  it("puts every cell in the darkest step when every value is the same", () => {
    const same = [
      { x: "Oct 3", y: "EU", v: 5 },
      { x: "Oct 4", y: "EU", v: 5 },
    ]
    expect(heat(chartOption(HEAT, same, FULL)).series[0].data.map((c) => c[3])).toEqual([4, 4])
  })

  it("names the steps and the values each covers in its legend", () => {
    expect(chartLegend(HEAT, FULL, CELLS)).toEqual([
      { label: "0–20", color: withAlpha(HUE, 0.2) },
      { label: "20–40", color: withAlpha(HUE, 0.4) },
      { label: "40–60", color: withAlpha(HUE, 0.6) },
      { label: "60–80", color: withAlpha(HUE, 0.8) },
      { label: "80–100", color: withAlpha(HUE, 1) },
    ])
    expect(chartLegend(HEAT, FULL, [])).toEqual([])
    // Every value the same: one entry, in the step its cells are drawn in, never five of "5–5".
    const same = [
      { x: "Oct 3", y: "EU", v: 5 },
      { x: "Oct 4", y: "EU", v: 5 },
    ]
    expect(chartLegend(HEAT, FULL, same)).toEqual([{ label: "5", color: withAlpha(HUE, 1) }])
  })

  it("takes the first fallback colour when its token is unset, and hands ECharts no undefined value", () => {
    const option = chartOption(HEAT, CELLS, theme())
    expect(heat(option).visualMap.pieces[0].color).toBe("rgba(80, 113, 222, 0.2)")
    expect(undefinedPaths(option)).toEqual([])
  })

  it("names the cell and its value in the tooltip, escaped", () => {
    const html = heat(chartOption({ ...HEAT, y: { ...HEAT.y, categories: ["APAC", "<b>EU</b>"] } }, CELLS, FULL)).tooltip.formatter({
      data: [0, 1, 100, 4],
    })
    expect(html).toContain("Oct 3 · &lt;b&gt;EU&lt;/b&gt;")
    expect(html).toContain("Orders")
    expect(html).toContain(">100<")
  })

  it("is a table of its cells for a screen reader", () => {
    expect(chartTable(HEAT, CELLS)).toEqual({
      head: ["Day", "Region", "Orders"],
      body: [
        ["Oct 3", "APAC", "0"],
        ["Oct 3", "EU", "100"],
        ["Oct 4", "EU", "45"],
      ],
    })
  })
})

describe("a waterfall", () => {
  const WATERFALL: WaterfallChartSettings = {
    type: "waterfall",
    total: true,
    description: "Net revenue by month",
    x: { field: "x", label: "Month", categories: ["Jan", "Feb", "Mar", "Apr"] },
    y: { field: "v", format: "integer" },
    series: { values: [{ key: "", label: "Net", color: "series-1" }] },
  }
  // Feb crosses zero; Mar has no row.
  const STEPS = [
    { x: "Jan", v: 10 },
    { x: "Feb", v: -15 },
    { x: "Apr", v: 8 },
  ]
  const COLORED = theme({ "series-1": "#rise", "series-2": "#fall", other: "#sum" })
  const bars = (settings: WaterfallChartSettings) => {
    const o = chartOption(settings, STEPS, COLORED) as unknown as {
      xAxis: { data: string[] }
      series: { data: (number | null | { value: number; itemStyle: { color: string } })[] }[]
    }
    const [base, step] = o.series
    return {
      categories: o.xAxis.data,
      base: base.data,
      size: step.data.map((d) => (d && typeof d === "object" ? d.value : d)),
      color: step.data.map((d) => (d && typeof d === "object" ? d.itemStyle.color : d)),
    }
  }

  it("starts each step where the one before it ended, and draws the Total from zero", () => {
    expect(bars(WATERFALL)).toEqual({
      categories: ["Jan", "Feb", "Mar", "Apr", "Total"],
      // An invisible bar up to where each step starts: Feb falls from 10 to -5, so it
      // stands on -5 and is 15 tall; Mar is skipped and Apr starts where Feb ended.
      base: [0, -5, null, -5, 0],
      size: [10, 15, null, 8, 3],
      color: ["#rise", "#fall", null, "#rise", "#sum"],
    })
  })

  it("says No value in the tooltip for a month with no row, never 0", () => {
    const { formatter } = (chartOption(WATERFALL, STEPS, COLORED) as unknown as { tooltip: { formatter: Formatter } }).tooltip
    const html = formatter([{ dataIndex: 2 }])
    expect(html).toMatch(/>Mar<.*>Net<.*>No value</)
    expect(html).not.toMatch(/>[+-]?0</)
  })

  it("leaves the Total off when the settings say so", () => {
    expect(bars({ ...WATERFALL, total: false })).toMatchObject({ categories: ["Jan", "Feb", "Mar", "Apr"], size: [10, 15, null, 8] })
  })

  it("is a table of each change and the running total for a screen reader", () => {
    expect(chartTable(WATERFALL, STEPS)).toEqual({
      head: ["Month", "Net", "Running total"],
      body: [
        ["Jan", "10", "10"],
        ["Feb", "-15", "-5"],
        ["Mar", "", ""],
        ["Apr", "8", "3"],
        ["Total", "3", "3"],
      ],
    })
  })
})

describe("a scatter and a bubble chart", () => {
  const SCATTER: ScatterChartSettings = {
    type: "scatter",
    description: "Discount against amount",
    points: { x: { field: "x", label: "Amount", format: "decimal" }, y: { field: "y", label: "Discount", format: "decimal" } },
    series: { field: "s", label: "Region", values: [{ key: "s0", label: "EU", color: "series-1" }, { key: "s1", label: "<b>US</b>", color: "series-2" }] },
  }
  const BUBBLE: ScatterChartSettings = {
    type: "bubble",
    description: "",
    points: { ...SCATTER.points, size: { field: "size", label: "Orders", format: "integer" } },
    series: { values: [{ key: "", label: "Discount", color: "series-1" }] },
  }
  type Scatter = {
    xAxis: { scale: boolean; name?: string }
    yAxis: { scale: boolean; name?: string }
    tooltip: { formatter: (p: { seriesIndex: number; dataIndex: number }) => string }
    series: { type: string; name: string; data: { value: number[]; symbolSize: number }[] }[]
  }
  const scatter = (o: ChartOption) => o as unknown as Scatter

  it("draws a series a colour, each point one size, on axes that fit the points", () => {
    const rows = [{ x: 1, y: 2, s: "s0" }, { x: 3, y: 4, s: "s1" }, { x: 5, y: 6, s: "s1" }, { x: "n/a", y: 1, s: "s0" }]
    const o = scatter(chartOption(SCATTER, rows, FULL))
    expect(o.series.map((s) => [s.type, s.name, s.data.length])).toEqual([["scatter", "EU", 1], ["scatter", "<b>US</b>", 2]])
    expect(o.series.flatMap((s) => s.data.map((d) => d.symbolSize))).toEqual([8, 8, 8])
    expect([o.xAxis.scale, o.yAxis.scale, o.xAxis.name, o.yAxis.name]).toEqual([true, true, "Amount", "Discount"])
    const html = o.tooltip.formatter({ seriesIndex: 1, dataIndex: 1 })
    expect(html).toContain("&lt;b&gt;US&lt;/b&gt;")
    expect(html).toContain(">5<")
    expect(html).toContain(">6<")
  })

  it("draws a bubble's area as its size, the largest at 40 across, none for a size of zero, and the tooltip on the one it is over", () => {
    const rows = [{ x: 1, y: 1, size: 100 }, { x: 2, y: 2, size: 0 }, { x: 3, y: 3, size: 25 }, { x: 4, y: 4, size: 0.01 }]
    const o = scatter(chartOption(BUBBLE, rows, FULL))
    // A quarter of the size is half as wide; a speck is drawn at 4, never at nothing.
    expect(o.series[0].data).toEqual([
      { value: [1, 1, 100], symbolSize: 40 },
      { value: [3, 3, 25], symbolSize: 20 },
      { value: [4, 4, 0.01], symbolSize: 4 },
    ])
    const html = o.tooltip.formatter({ seriesIndex: 0, dataIndex: 1 })
    expect(html).toContain("Orders")
    expect(html).toContain(">25<")
    expect(html).not.toContain(">100<")
  })

  // A saved chart's point is every source row at one x, y and colour: the tooltip says how many, past one.
  const COUNTED: ScatterChartSettings = { ...SCATTER, points: { ...SCATTER.points, rows: { field: "n", label: "Rows", format: "integer" } } }

  it("is a table of its points for a screen reader", () => {
    expect(chartTable(BUBBLE, [{ x: 1.5, y: 2, size: 3 }])).toEqual({ head: ["Amount", "Discount", "Orders"], body: [["1.5", "2", "3"]] })
    expect(chartTable(SCATTER, [{ x: 1, y: 2, s: "s1" }])).toEqual({ head: ["Amount", "Discount", "Region"], body: [["1", "2", "<b>US</b>"]] })
    expect(chartTable(COUNTED, [{ x: 1, y: 2, s: "s1", n: 20000 }])).toEqual({
      head: ["Amount", "Discount", "Region", "Rows"],
      body: [["1", "2", "<b>US</b>", "20,000"]],
    })
  })

  it("names the rows at a point in its tooltip when there is more than one", () => {
    const rows = [{ x: 1, y: 2, s: "s0", n: 20000 }, { x: 3, y: 4, s: "s0", n: 1 }]
    const o = scatter(chartOption(COUNTED, rows, FULL))
    expect(o.series[0].data).toHaveLength(2)
    const many = o.tooltip.formatter({ seriesIndex: 0, dataIndex: 0 })
    expect(many).toContain("Rows")
    expect(many).toContain(">20,000<")
    expect(o.tooltip.formatter({ seriesIndex: 0, dataIndex: 1 })).not.toContain("Rows")
    expect(scatter(chartOption(SCATTER, rows, FULL)).tooltip.formatter({ seriesIndex: 0, dataIndex: 0 })).not.toContain("Rows")
  })
})

// The second tier (dashboards-design.md § 14.8). Every colour token set, so each
// node and group takes its own and none falls back.
const ALL = theme(Object.fromEntries(CHART_COLORS.map((c, i) => [c, `hsl(${i * 20}, 50%, 50%)`])), {
  text: FULL.text,
  muted: FULL.muted,
  border: FULL.border,
  surface: FULL.surface,
  font: FULL.font,
})

const SERIES = CHART_COLORS.filter((c) => c.startsWith("series-"))

type Item = { formatter: (p: object) => string }

describe("a treemap and a sunburst", () => {
  const TREEMAP: PartsChartSettings = {
    type: "treemap",
    description: "Revenue by region and channel",
    levels: ["Region", "Channel"],
    value: { label: "Revenue", format: "integer" },
    groups: [
      { key: "US", label: "US", color: "series-1" },
      { key: "EU", label: "EU", color: "series-2" },
    ],
  }
  const SUNBURST: PartsChartSettings = { ...TREEMAP, type: "sunburst" }
  const ROWS = [
    { path: ["EU", "web"], value: 320 },
    { path: ["US", "web"], value: 300 },
    { path: ["US", "store"], value: 270 },
    { path: ["EU", "store"], value: 180 },
    { path: ["APAC", "web"], value: 40 },
    // Not drawn: nothing to draw, no path, and no number.
    { path: ["EU", "app"], value: 0 },
    { path: [], value: 5 },
    { path: ["EU", "fax"], value: "n/a" },
  ]
  type Node = { name: string; value: number; itemStyle: { color: string }; children?: Node[] }
  type Parts = {
    tooltip: Item
    series: {
      type: string
      sort: unknown
      nodeClick: boolean
      data: Node[]
      levels?: { upperLabel?: { show: boolean; color?: string } }[]
    }[]
  }
  const tree = (o: ChartOption) => o as unknown as Parts
  const names = (list: Node[]): unknown[] => list.map((n) => (n.children ? [n.name, n.value, names(n.children)] : [n.name, n.value]))

  it("sums each level's parts and puts them largest first, each in its top-level group's colour and Other past the named groups", () => {
    for (const settings of [TREEMAP, SUNBURST]) {
      const [series] = tree(chartOption(settings, ROWS, ALL)).series
      expect(series.type).toBe(settings.type)
      expect(series.nodeClick).toBe(false)
      expect(names(series.data)).toEqual([
        ["US", 570, [["web", 300], ["store", 270]]],
        ["EU", 500, [["web", 320], ["store", 180]]],
        ["APAC", 40, [["web", 40]]],
      ])
      expect(series.data.map((n) => n.itemStyle.color)).toEqual([ALL.colors["series-1"], ALL.colors["series-2"], ALL.colors.other])
      expect(series.data[0].children!.map((n) => n.itemStyle.color)).toEqual([ALL.colors["series-1"], ALL.colors["series-1"]])
    }
  })

  it("keeps the tree's order, not the library's sort", () => {
    expect(tree(chartOption(TREEMAP, ROWS, ALL)).series[0].sort).toBe(false)
    const sort = tree(chartOption(SUNBURST, ROWS, ALL)).series[0].sort as (a: { dataIndex: number }, b: { dataIndex: number }) => number
    expect([{ dataIndex: 2 }, { dataIndex: 0 }, { dataIndex: 1 }].sort(sort).map((n) => n.dataIndex)).toEqual([0, 1, 2])
  })

  it("names a treemap's outer level in a strip in the text colour, as the strip is the card's colour", () => {
    const { levels } = tree(chartOption(TREEMAP, ROWS, ALL)).series[0]
    // The whole chart, then Region, then Channel.
    expect(levels).toHaveLength(3)
    expect(levels![0].upperLabel?.show).toBe(false)
    expect(levels![1].upperLabel).toMatchObject({ show: true, color: FULL.text })
    expect(levels![2].upperLabel).toBeUndefined()
  })

  it("names a part's path, its value and its share of the whole in the tooltip, escaped", () => {
    const html = tree(chartOption(TREEMAP, ROWS, ALL)).tooltip.formatter({ color: "#fff", data: { path: ["US", "<b>web</b>"], value: 300 } })
    expect(html).toContain("US › &lt;b&gt;web&lt;/b&gt;")
    expect(html).toContain(">300<")
    // 300 of 1,110.
    expect(html).toContain(">27%<")
  })

  it("says a share and a group's value are of the drawn parts when the source has more", () => {
    // Share was of the drawn total, and a group summed only its drawn parts, both under plain labels.
    // The words follow any measure's label: "Sum of amount of drawn parts" read as two "of"s.
    for (const settings of [TREEMAP, SUNBURST]) {
      const summed = { ...settings, partial: true, value: { ...settings.value, label: "Sum of amount" } }
      const { formatter } = tree(chartOption(summed, ROWS, ALL)).tooltip
      const part = formatter({ color: "#fff", data: { path: ["US", "web"], value: 300 } })
      expect(part).toContain(">Share of drawn total<")
      // A part's own value is the source's.
      expect(part).toContain(">Sum of amount<")
      const group = formatter({ color: "#fff", data: { path: ["US"], value: 570, children: [{}, {}] } })
      expect(group).toContain(">Sum of amount (drawn parts only)<")
      expect(group).toContain(">Share of drawn total<")
      // When nothing was cut, the plain words.
      const whole = tree(chartOption(settings, ROWS, ALL)).tooltip.formatter({ color: "#fff", data: { path: ["US"], value: 570, children: [{}, {}] } })
      expect(whole).toContain(">Revenue<")
      expect(whole).toContain(">Share<")
      expect(whole).not.toContain("drawn")
    }
  })

  it("has no tooltip on the gaps between parts, where the pointer is over the whole chart", () => {
    // The whole chart is a node of the library's own, with no path: it said "Revenue 1,110, Share 100%" with no title.
    for (const settings of [TREEMAP, SUNBURST]) {
      expect(tree(chartOption(settings, ROWS, ALL)).tooltip.formatter({ color: "#5070dd", data: { value: 1110 } })).toBe("")
    }
  })

  it("names its groups in the legend, and Other when a part is in none", () => {
    expect(chartLegend(TREEMAP, ALL, ROWS).map((l) => l.label)).toEqual(["US", "EU", "Other"])
    expect(chartLegend(TREEMAP, ALL, ROWS.slice(0, 4)).map((l) => l.label)).toEqual(["US", "EU"])
  })

  it("is a table of its parts for a screen reader", () => {
    expect(chartTable(TREEMAP, ROWS)).toEqual({
      head: ["Region", "Channel", "Revenue"],
      body: [
        ["EU", "web", "320"],
        ["US", "web", "300"],
        ["US", "store", "270"],
        ["EU", "store", "180"],
        ["APAC", "web", "40"],
      ],
    })
  })
})

describe("a sankey", () => {
  const SANKEY: SankeyChartSettings = {
    type: "sankey",
    description: "Visits by page",
    source: { label: "From page" },
    target: { label: "To page" },
    value: { label: "Visits", format: "integer" },
    nodes: (["visit", "cart", "left", "paid", "refund"] as const).map((key, i) => ({ key, label: key, color: SERIES[i] })),
  }
  const ROWS = [
    { source: "visit", target: "cart", value: 50 },
    { source: "visit", target: "left", value: 30 },
    { source: "cart", target: "left", value: 25 },
    { source: "cart", target: "paid", value: 20 },
    { source: "paid", target: "refund", value: 2 },
    // Not drawn: a flow back to where it started, no flow, and no name.
    { source: "cart", target: "cart", value: 9 },
    { source: "visit", target: "paid", value: 0 },
    { source: null, target: "paid", value: 4 },
  ]
  type Sankey = {
    tooltip: Item
    series: {
      layoutIterations: number
      draggable: boolean
      nodeGap: number
      data: { name: string; itemStyle: { color: string }; label?: { position: string } }[]
      links: { source: string; target: string; value: number }[]
    }[]
  }
  const sankey = (o: ChartOption) => o as unknown as Sankey

  it("places the nodes in the order given and never moves them, each in its colour", () => {
    const [series] = sankey(chartOption(SANKEY, ROWS, ALL)).series
    expect(series.layoutIterations).toBe(0)
    expect(series.draggable).toBe(false)
    expect(series.data.map((n) => n.name)).toEqual(["visit", "cart", "left", "paid", "refund"])
    expect(series.data.map((n) => n.itemStyle.color)).toEqual(SERIES.slice(0, 5).map((c) => ALL.colors[c]))
  })

  it("draws a link per flow above zero between two names, never one back to where it started", () => {
    expect(sankey(chartOption(SANKEY, ROWS, ALL)).series[0].links).toEqual(ROWS.slice(0, 5))
  })

  it("names a node no flow leaves on its left, inside the chart", () => {
    const { data } = sankey(chartOption(SANKEY, ROWS, ALL)).series[0]
    expect(data.filter((n) => n.label?.position === "left").map((n) => n.name)).toEqual(["left", "refund"])
  })

  it("spaces a column's nodes so the gaps fill at most half of it, a node nothing leaves counted in the last column", () => {
    // One into 70 into one, and one straight to an end: ECharts draws that end in the last
    // column, beside the other, so the fullest column is the 70.
    const middle = Array.from({ length: 70 }, (_, i) => `step ${i}`)
    const nodes = ["start", ...middle, "end", "lost"].map((key, i) => ({ key, label: key, color: SERIES[i % SERIES.length] }))
    const rows = [
      ...middle.flatMap((m) => [{ source: "start", target: m, value: 1 }, { source: m, target: "end", value: 1 }]),
      { source: "start", target: "lost", value: 1 },
    ]
    const gap = (height: number) => sankey(chartOption({ ...SANKEY, nodes }, rows, ALL, 568, height)).series[0].nodeGap
    expect(gap(360)).toBeCloseTo((360 - 12) / 2 / 69)
    expect(gap(180)).toBeCloseTo((180 - 12) / 2 / 69)
    // A few nodes keep the gap they always had.
    expect(sankey(chartOption(SANKEY, ROWS, ALL)).series[0].nodeGap).toBe(6)
  })

  it("names a flow's two ends or a node in the tooltip", () => {
    const { formatter } = sankey(chartOption(SANKEY, ROWS, ALL)).tooltip
    const flow = formatter({ dataType: "edge", value: 50, color: "#fff", data: { source: "visit", target: "<b>cart</b>" } })
    expect(flow).toContain("visit → &lt;b&gt;cart&lt;/b&gt;")
    expect(flow).toContain(">50<")
    expect(formatter({ dataType: "node", name: "cart", value: 45 })).toContain(">45<")
  })

  it("has no legend, as its names are on its nodes, and is a table of its flows", () => {
    expect(chartLegend(SANKEY, ALL, ROWS)).toEqual([])
    expect(chartTable(SANKEY, ROWS.slice(0, 2))).toEqual({
      head: ["From page", "To page", "Visits"],
      body: [
        ["visit", "cart", "50"],
        ["visit", "left", "30"],
      ],
    })
  })
})

// The page's tokens as readChartTheme reads them: light from globals.css's :root, dark from .dark.
function pageTheme(block: ":root" | ".dark"): ChartTheme {
  const css = readFileSync(resolve(process.cwd(), "src/app/globals.css"), "utf8")
  const tokens = new RegExp(`${block.replace(".", "\\.")}\\s*\\{([^}]*)\\}`).exec(css)?.[1] ?? ""
  const token = (name: string) => hslToken(new RegExp(`${name}:\\s*([^;]+);`).exec(tokens)?.[1] ?? "")
  return theme(Object.fromEntries(CHART_COLORS.map((c) => [c, token(`--chart-${c}`)])), {
    text: token("--foreground"),
    muted: token("--muted-foreground"),
    border: token("--border"),
    surface: token("--card"),
  })
}
const PAGE_THEMES = [pageTheme(":root"), pageTheme(".dark")]

/** A colour ECharts reads (hsl, hsla, rgb, rgba or #rrggbb) as red, green and blue from 0 to 255, and its opacity. */
function channels(color: string): [number, number, number, number] {
  const hex = /^#([0-9a-f]{6})$/i.exec(color)
  if (hex) {
    const n = parseInt(hex[1], 16)
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1]
  }
  const fn = /^(rgb|hsl)a?\(([^)]*)\)$/.exec(color)
  if (!fn) throw new Error(`not a colour: ${color}`)
  const [a, b, c, alpha = 1] = fn[2].split(",").map((p) => parseFloat(p))
  if (fn[1] === "rgb") return [a, b, c, alpha]
  const [s, l] = [b / 100, c / 100]
  const f = (n: number) => {
    const k = (n + a / 30) % 12
    return 255 * (l - s * Math.min(l, 1 - l) * Math.max(-1, Math.min(k - 3, 9 - k, 1)))
  }
  return [f(0), f(8), f(4), alpha]
}

/** A colour as it shows laid over a solid one. */
function laid(color: string, under: string): number[] {
  const [r, g, b, alpha] = channels(color)
  const base = channels(under)
  return [r, g, b].map((v, i) => v * alpha + base[i] * (1 - alpha))
}

/** How far a colour laid on the card stands from the card: WCAG's contrast ratio. */
function standing(color: string, card: string): number {
  const luminance = (rgb: number[]) => {
    const [r, g, b] = rgb.map((v) => (v / 255 <= 0.04045 ? v / 255 / 12.92 : ((v / 255 + 0.055) / 1.055) ** 2.4))
    return 0.2126 * r + 0.7152 * g + 0.0722 * b
  }
  const [one, two] = [luminance(laid(color, card)), luminance(laid(card, card))]
  return (Math.max(one, two) + 0.05) / (Math.min(one, two) + 0.05)
}

describe("a calendar heatmap", () => {
  const CALENDAR: CalendarChartSettings = {
    type: "calendar",
    description: "Orders by day",
    range: ["2026-09-28", "2026-10-04"],
    value: { label: "Orders", format: "integer" },
    color: "series-1",
  }
  const ROWS = [
    { day: "2026-09-28", value: 1 },
    { day: "2026-09-29", value: 5 },
    { day: "2026-09-30", value: 9 },
    // Not drawn: no number, and not a day.
    { day: "2026-10-01", value: null },
    { day: "Oct 2", value: 3 },
    { day: "2026-10-03", value: 0 },
  ]
  type Calendar = {
    calendar: {
      range: string[]
      dayLabel: { firstDay: number; nameMap: string[] }
      monthLabel: { formatter: (m: { nameMap: string; yyyy: string; MM: string; M: number }) => string }
      itemStyle: { color: string }
    }
    visualMap: { pieces: { value: number; color: string }[] }
    series: { coordinateSystem: string; data: [string, number, number][] }[]
  }
  const calendar = (o: ChartOption) => o as unknown as Calendar

  it("draws its days from the first to the last, a week a column starting on Monday", () => {
    const o = calendar(chartOption(CALENDAR, ROWS, ALL))
    expect(o.calendar.range).toEqual(["2026-09-28", "2026-10-04"])
    expect(o.calendar.dayLabel.firstDay).toBe(1)
    expect(o.calendar.dayLabel.nameMap[1]).toBe("M")
  })

  it("puts each day with a number in one of five steps of its hue", () => {
    const o = calendar(chartOption(CALENDAR, ROWS, ALL))
    expect(o.series[0].coordinateSystem).toBe("calendar")
    // Five steps from 0 to 9, each 1.8 wide.
    expect(o.series[0].data).toEqual([
      ["2026-09-28", 1, 0],
      ["2026-09-29", 5, 2],
      ["2026-09-30", 9, 4],
      ["2026-10-03", 0, 0],
    ])
    expect(o.visualMap.pieces.map((p) => p.color)).toEqual([0.2, 0.4, 0.6, 0.8, 1].map((a) => withAlpha(ALL.colors["series-1"], a)))
    expect(chartLegend(CALENDAR, ALL, ROWS).map((l) => l.label)).toEqual(["0–2", "2–4", "4–5", "5–7", "7–9"])
    const twelve = [
      { day: "2026-09-28", value: 12 },
      { day: "2026-09-29", value: 12 },
    ]
    expect(chartLegend(CALENDAR, ALL, twelve).map((l) => l.label)).toEqual(["12"])
    expect(chartLegend(CALENDAR, ALL, twelve)[0].color).toBe(chartLegend(CALENDAR, ALL, ROWS)[4].color)
  })

  it("shows each step in its legend as a day draws it, over the grey of a day with no number, so the faintest reads as more in either theme", () => {
    const unlike: string[] = []
    const fainter: string[] = []
    for (const t of PAGE_THEMES) {
      for (const c of SERIES) {
        const o = calendar(chartOption({ ...CALENDAR, color: c }, ROWS, t))
        const empty = o.calendar.itemStyle.color
        const legend = chartLegend({ ...CALENDAR, color: c }, t, ROWS)
        o.visualMap.pieces.forEach((p, i) => {
          const drawn = laid(p.color, empty)
          if (laid(legend[i].color, t.surface).some((v, j) => Math.abs(v - drawn[j]) > 1)) unlike.push(`${c} step ${i + 1} on ${t.surface}`)
        })
        if (standing(legend[0].color, t.surface) <= standing(empty, t.surface)) fainter.push(`${c} on ${t.surface}`)
      }
    }
    expect(unlike).toEqual([])
    expect(fainter).toEqual([])
  })

  it("says the year with January and with the first month drawn, and names a month only where it has room", () => {
    const names = (width: number) => {
      const { formatter } = calendar(chartOption({ ...CALENDAR, range: ["2026-11-20", "2027-02-10"] }, ROWS, ALL, width)).calendar.monthLabel
      return (["2026-11", "2026-12", "2027-01", "2027-02"] as const).map((k) => formatter({ nameMap: "", yyyy: k.slice(0, 4), MM: k.slice(5), M: Number(k.slice(5)) }))
    }
    expect(names(568)).toEqual(["Nov 2026", "Dec", "Jan 2027", "Feb"])
    // Thirteen weeks in 98 px: a name that says its year goes first, the others where room is left.
    expect(names(120)).toEqual(["Nov 2026", "", "Jan 2027", ""])
    // Two days, both of one week: September's name and October's would be one over the other.
    const { formatter } = calendar(chartOption(CALENDAR, ROWS, ALL)).calendar.monthLabel
    expect(formatter({ nameMap: "Sep", yyyy: "2026", MM: "09", M: 9 })).toBe("Sep 2026")
    expect(formatter({ nameMap: "Oct", yyyy: "2026", MM: "10", M: 10 })).toBe("")
  })

  it("is a table of its days for a screen reader", () => {
    expect(chartTable(CALENDAR, ROWS.slice(0, 2))).toEqual({
      head: ["Day", "Orders"],
      body: [
        ["2026-09-28", "1"],
        ["2026-09-29", "5"],
      ],
    })
  })
})

describe("a bullet chart", () => {
  const BULLET: BulletChartSettings = {
    type: "bullet",
    description: "Revenue against the goal",
    value: { label: "Revenue", format: "integer" },
    target: { label: "Goal" },
    color: "series-1",
  }
  type Bullet = {
    tooltip: { formatter: () => string }
    series: { type: string; data: unknown[]; symbol?: string; itemStyle: { color: string; borderRadius?: number[] } }[]
  }
  const bullet = (o: ChartOption) => o as unknown as Bullet

  it("draws a bar as long as the number and a mark across it at the target, in the text colour", () => {
    const o = bullet(chartOption(BULLET, [{ value: 800, target: 1000 }], ALL))
    expect(o.series.map((s) => [s.type, s.data])).toEqual([
      ["bar", [800]],
      ["scatter", [[1000, 0]]],
    ])
    expect(o.series[1].symbol).toBe("rect")
    expect(o.series[1].itemStyle.color).toBe(FULL.text)
    const html = o.tooltip.formatter()
    expect(html).toContain("Goal")
    expect(html).toContain(">1,000<")
    expect(html).toContain("Of the target")
    expect(html).toContain(">80%<")
  })

  it("draws no mark without a target, and says no share of a target of zero", () => {
    expect(bullet(chartOption(BULLET, [{ value: 800, target: null }], ALL)).series).toHaveLength(1)
    expect(bullet(chartOption(BULLET, [{ value: 800, target: 0 }], ALL)).tooltip.formatter()).not.toContain("Of the target")
  })

  it("rounds a bar below zero on its left, and draws nothing without a number", () => {
    expect(bullet(chartOption(BULLET, [{ value: -5, target: 10 }], ALL)).series[0].itemStyle.borderRadius).toEqual([3, 0, 0, 3])
    expect(bullet(chartOption(BULLET, [{ value: null, target: 10 }], ALL)).series.map((s) => s.data)).toEqual([[]])
  })

  it("says both numbers in the legend and the table, so a still picture says them", () => {
    const rows = [{ value: 800, target: 1000 }]
    expect(chartLegend(BULLET, ALL, rows)).toEqual([
      { label: "Revenue · 800", color: ALL.colors["series-1"] },
      { label: "Goal · 1,000", color: FULL.text },
    ])
    expect(chartTable(BULLET, rows)).toEqual({ head: ["Revenue", "Goal"], body: [["800", "1,000"]] })
  })

  it("says no share of a target below zero, as a number past it would read as a share below zero", () => {
    // 853.6M against -500M was "Of the target -170.7%".
    for (const target of [-500, -0.5]) {
      expect(bullet(chartOption(BULLET, [{ value: 800, target }], ALL)).tooltip.formatter()).not.toContain("Of the target")
    }
  })

  it("writes the target in its own format, in the tooltip, the legend and the table", () => {
    const settings: BulletChartSettings = { ...BULLET, value: { label: "Paid", format: "decimal" }, target: { label: "Due", format: "compact" } }
    const rows = [{ value: 5_000_000_000.5, target: 6_195_252_867.7 }]
    expect(bullet(chartOption(settings, rows, ALL)).tooltip.formatter()).toContain(">6.2B<")
    expect(chartLegend(settings, ALL, rows)[1].label).toBe("Due · 6.2B")
    expect(chartTable(settings, rows).body).toEqual([["5,000,000,000.5", "6.2B"]])
    // A target with no format of its own is in the number's.
    expect(chartLegend({ ...settings, target: { label: "Due" } }, ALL, rows)[1].label).toBe("Due · 6,195,252,867.7")
  })
})

describe("a box plot", () => {
  const BOXPLOT: BoxplotChartSettings = {
    type: "boxplot",
    description: "Order value by plan",
    x: { label: "Plan", categories: ["free", "pro", "team"] },
    y: { label: "Order value", format: "integer" },
    color: "series-1",
  }
  const ROWS = [
    { x: "free", min: 1, q1: 2, median: 3, q3: 4, max: 5 },
    // Not drawn: a category short of a number.
    { x: "team", min: 10, q1: 20, median: 30, q3: 40, max: null },
  ]
  type Box = {
    yAxis: { scale: boolean; name: string; minInterval?: number }
    tooltip: { formatter: (p: { dataIndex: number }) => string }
    series: { type: string; data: number[][] }[]
  }
  const box = (o: ChartOption) => o as unknown as Box

  it("draws each category's five numbers, least first, in the categories' order, on an axis that fits them", () => {
    const o = box(chartOption(BOXPLOT, ROWS, ALL))
    expect(o.series[0].type).toBe("boxplot")
    expect(o.series[0].data).toEqual([[1, 2, 3, 4, 5], [], []])
    expect([o.yAxis.scale, o.yAxis.name, o.yAxis.minInterval]).toEqual([true, "Order value", 1])
  })

  it("lists the five in the tooltip, greatest first", () => {
    const html = box(chartOption(BOXPLOT, ROWS, ALL)).tooltip.formatter({ dataIndex: 0 })
    const at = ["Maximum", "Upper quartile", "Median", "Lower quartile", "Minimum"].map((l) => html.indexOf(l))
    expect(at.every((i) => i >= 0)).toBe(true)
    expect([...at].sort((a, b) => a - b)).toEqual(at)
  })

  it("has no legend, and is a table of every category for a screen reader", () => {
    expect(chartLegend(BOXPLOT, ALL, ROWS)).toEqual([])
    expect(chartTable(BOXPLOT, ROWS)).toEqual({
      head: ["Plan", "Minimum", "Lower quartile", "Median", "Upper quartile", "Maximum"],
      body: [
        ["free", "1", "2", "3", "4", "5"],
        ["pro", "", "", "", "", ""],
        ["team", "", "", "", "", ""],
      ],
    })
  })
})

describe("a candlestick", () => {
  const CANDLES: CandlestickChartSettings = {
    type: "candlestick",
    description: "Price by month",
    x: { label: "Month", categories: ["2026-01", "2026-02", "2026-03"] },
    y: { label: "Price", format: "decimal" },
  }
  const ROWS = [
    { x: "2026-01", open: 10, high: 12, low: 9, close: 11 },
    { x: "2026-02", open: 11, high: 11.5, low: 8, close: 9 },
    // It closed where it opened: drawn as a rise.
    { x: "2026-03", open: 9, high: 9, low: 9, close: 9 },
  ]
  type Candle = {
    tooltip: { formatter: Formatter }
    series: { data: number[][]; itemStyle: { color: string; color0: string } }[]
  }
  const candle = (o: ChartOption) => o as unknown as Candle

  it("hands ECharts each period as open, close, low and high, rising in the rise colour and falling in the fall colour", () => {
    const [series] = candle(chartOption(CANDLES, ROWS, ALL)).series
    expect(series.data).toEqual([
      [10, 11, 9, 12],
      [11, 9, 8, 11.5],
      [9, 9, 9, 9],
    ])
    expect([series.itemStyle.color, series.itemStyle.color0]).toEqual([ALL.colors["series-1"], ALL.colors["series-2"]])
  })

  it("lists the open, high, low and close in the tooltip, in that order, and the fall colour for a fall", () => {
    const html = candle(chartOption(CANDLES, ROWS, ALL)).tooltip.formatter([{ dataIndex: 1 }])
    const at = ["Open", "High", "Low", "Close"].map((l) => html.indexOf(l))
    expect([...at].sort((a, b) => a - b)).toEqual(at)
    expect(html).toContain(ALL.colors["series-2"])
    expect(html).not.toContain(ALL.colors["series-1"])
  })

  it("says No value in the tooltip for a period with no row, never 0", () => {
    const html = candle(chartOption(CANDLES, ROWS.slice(0, 1), ALL)).tooltip.formatter([{ dataIndex: 1 }])
    expect(html).toMatch(/>2026-02<.*>Price<.*>No value</)
    expect(html).not.toMatch(/>0</)
  })

  it("says what its two colours mean, and is a table of its periods", () => {
    expect(chartLegend(CANDLES, ALL, ROWS).map((l) => l.label)).toEqual(["Close above open", "Close below open"])
    expect(chartTable(CANDLES, ROWS.slice(0, 1))).toEqual({
      head: ["Month", "Open", "High", "Low", "Close"],
      body: [
        ["2026-01", "10", "12", "9", "11"],
        ["2026-02", "", "", "", ""],
        ["2026-03", "", "", "", ""],
      ],
    })
  })
})

describe("a range band", () => {
  const RANGE: RangeChartSettings = {
    type: "range",
    description: "The forecast by day",
    x: { label: "Day", categories: ["Oct 1", "Oct 2", "Oct 3", "Oct 4"] },
    low: { label: "Low", format: "integer" },
    high: { label: "High", format: "integer" },
    color: "series-1",
  }
  const ROWS = [
    { x: "Oct 1", low: 5, high: 9 },
    // The two the wrong way round: the band never folds.
    { x: "Oct 2", low: 7, high: 3 },
    { x: "Oct 3", low: null, high: 4 },
    { x: "Oct 4", low: -2, high: 2 },
  ]
  type Band = {
    yAxis: { name: string }
    tooltip: { formatter: Formatter }
    series: { stack: string; stackStrategy: string; connectNulls: boolean; areaStyle?: object; data: (number | null)[] }[]
  }
  const band = (o: ChartOption) => o as unknown as Band

  it("stacks the gap up to the greater on an unseen line at the lesser, filled, and leaves a gap where either is missing", () => {
    const o = band(chartOption(RANGE, ROWS, ALL))
    expect(o.series.map((s) => s.data)).toEqual([
      [5, 3, null, -2],
      [4, 4, null, 4],
    ])
    expect(o.series.map((s) => [s.stack, s.stackStrategy, s.connectNulls, "areaStyle" in s])).toEqual([
      ["band", "all", false, false],
      ["band", "all", false, true],
    ])
    expect(o.yAxis.name).toBe("Low · High")
  })

  it("says each measure as it is in the tooltip, not the gap it is drawn as", () => {
    const { formatter } = band(chartOption(RANGE, ROWS, ALL)).tooltip
    const html = formatter([{ dataIndex: 1 }])
    expect(html).toContain("Oct 2")
    expect(html).toContain(">7<")
    expect(html).toContain(">3<")
    expect(html).not.toContain(">4<")
    // A band with one measure missing is not drawn; the tooltip says the other, and No value, never 0.
    const half = formatter([{ dataIndex: 2 }])
    expect(half).toMatch(/>Low<.*>No value<.*>High<.*>4</)
    expect(half).not.toMatch(/>0</)
  })

  it("has no legend, and is a table of each category's two measures", () => {
    expect(chartLegend(RANGE, ALL, ROWS)).toEqual([])
    expect(chartTable(RANGE, ROWS)).toEqual({
      head: ["Day", "Low", "High"],
      body: [
        ["Oct 1", "5", "9"],
        ["Oct 2", "7", "3"],
        ["Oct 3", "", "4"],
        ["Oct 4", "-2", "2"],
      ],
    })
  })
})

describe("a choropleth map", () => {
  const MAP: MapChartSettings = {
    type: "map",
    description: "Revenue by country",
    map: "world",
    region: { label: "Country" },
    value: { label: "Revenue", format: "integer" },
    color: "series-1",
  }
  const ROWS = [
    { region: "FRA", value: 100 },
    { region: "USA", value: 500 },
    { region: "JPN", value: 300 },
    // Not drawn: no shape, or no number.
    { region: null, value: 7 },
    { region: "DEU", value: null },
  ]
  type Shape = { name: string; value: number; itemStyle: { areaColor: string }; emphasis: { itemStyle: { areaColor: string } } }
  type Map = {
    tooltip: Item
    series: { type: string; map: string; roam: boolean; selectedMode: boolean; label: object; itemStyle: object; emphasis: object; data: Shape[] }[]
  }
  const map = (o: ChartOption) => o as unknown as Map
  // A step: the hue at its opacity laid over the border colour of a shape no row names.
  const step = (i: number) => laid(withAlpha(ALL.colors["series-1"], [0.2, 0.4, 0.6, 0.8, 1][i]), ALL.border)
  // Which step a colour is, to a unit of a channel; -1 for none.
  const stepOf = (color: string) => [0, 1, 2, 3, 4].findIndex((i) => laid(color, ALL.surface).every((v, j) => Math.abs(v - step(i)[j]) <= 1))

  it("fills each shape a row names at its step of the hue, the rest in the border colour, with the card's colour between", () => {
    const [s] = map(chartOption(MAP, ROWS, ALL)).series
    expect([s.type, s.map, s.roam, s.selectedMode, s.label]).toEqual(["map", "rsync-world", false, false, { show: false }])
    expect(s.data.map((d) => [d.name, d.value, stepOf(d.itemStyle.areaColor)])).toEqual([
      ["FRA", 100, 0],
      ["USA", 500, 4],
      ["JPN", 300, 2],
    ])
    expect(s.itemStyle).toEqual({ areaColor: ALL.border, borderColor: ALL.surface, borderWidth: 0.5 })
    expect(map(chartOption({ ...MAP, map: "us_states" }, [{ region: "US-CA", value: 1 }], ALL)).series[0].map).toBe("rsync-us-states")
  })

  it("fills its faintest step further from the card than a shape no row names, in either theme and any hue", () => {
    const fainter: string[] = []
    for (const t of PAGE_THEMES) {
      for (const c of SERIES) {
        const o = map(chartOption({ ...MAP, color: c }, ROWS, t))
        const [s] = o.series
        const none = (s.itemStyle as { areaColor: string }).areaColor
        const [fra, usa, jpn] = s.data.map((d) => d.itemStyle.areaColor)
        if (standing(fra, t.surface) <= standing(none, t.surface)) fainter.push(`${c} on ${t.surface}`)
        // The legend and the tooltip show a step in the colour its shapes are filled.
        const legend = chartLegend({ ...MAP, color: c }, t, ROWS).map((l) => l.color)
        expect([legend[0], legend[4], legend[2]]).toEqual([fra, usa, jpn])
        expect(o.tooltip.formatter({ name: "FRA" })).toContain(`background:${fra}`)
      }
    }
    expect(fainter).toEqual([])
  })

  it("keeps a shape's fill on hover and outlines it, rather than ECharts' yellow and the shape's name", () => {
    const [s] = map(chartOption(MAP, ROWS, ALL)).series
    expect(s.emphasis).toEqual({ label: { show: false }, itemStyle: { areaColor: ALL.border, borderColor: ALL.text, borderWidth: 1 } })
    expect(s.data.map((d) => d.emphasis.itemStyle.areaColor)).toEqual(s.data.map((d) => d.itemStyle.areaColor))
  })

  it("names the shape in the tooltip, not its code, and says No value for a shape no row fills", () => {
    const { formatter } = map(chartOption(MAP, ROWS, ALL)).tooltip
    expect(formatter({ name: "FRA" })).toContain("France")
    expect(formatter({ name: "FRA" })).toContain(">100<")
    expect(formatter({ name: "FRA" })).not.toContain("FRA")
    expect(formatter({ name: "DEU" })).toContain("Germany")
    expect(formatter({ name: "DEU" })).toContain("No value")
    // A shape with no ISO code keeps world-atlas's name.
    expect(formatter({ name: "Kosovo" })).toContain("Kosovo")
  })

  it("is a legend of its five steps, and a table of each shape it fills by name", () => {
    expect(chartLegend(MAP, ALL, ROWS).map((l) => [l.label, stepOf(l.color)])).toEqual([
      ["100–180", 0],
      ["180–260", 1],
      ["260–340", 2],
      ["340–420", 3],
      ["420–500", 4],
    ])
    expect(chartTable(MAP, ROWS)).toEqual({
      head: ["Country", "Revenue"],
      body: [
        ["France", "100"],
        ["United States of America", "500"],
        ["Japan", "300"],
      ],
    })
    expect(chartLegend(MAP, ALL, [])).toEqual([])
  })
})

describe("the second tier with no theme", () => {
  it("hands ECharts no undefined value", () => {
    const cases: [ChartSettings, object[]][] = [
      [{ type: "treemap", description: "", levels: ["A", "B"], value: { label: "N", format: "integer" }, groups: [] }, [{ path: ["a", "b"], value: 1 }]],
      [{ type: "sunburst", description: "", levels: ["A"], value: { label: "N", format: "integer" }, groups: [] }, [{ path: ["a"], value: 1 }]],
      [
        { type: "sankey", description: "", source: { label: "A" }, target: { label: "B" }, value: { label: "N", format: "integer" }, nodes: [] },
        [{ source: "a", target: "b", value: 1 }],
      ],
      [{ type: "calendar", description: "", range: ["2026-10-01", "2026-10-02"], value: { label: "N", format: "integer" }, color: "series-1" }, [{ day: "2026-10-01", value: 1 }]],
      [{ type: "bullet", description: "", value: { label: "N", format: "integer" }, target: { label: "T" }, color: "series-1" }, [{ value: 1, target: 2 }]],
      [{ type: "boxplot", description: "", x: { label: "X", categories: ["a"] }, y: { label: "Y", format: "integer" }, color: "series-1" }, [{ x: "a", min: 1, q1: 2, median: 3, q3: 4, max: 5 }]],
      [{ type: "candlestick", description: "", x: { label: "X", categories: ["a"] }, y: { label: "Y", format: "integer" } }, [{ x: "a", open: 1, high: 2, low: 0, close: 1 }]],
      [
        { type: "range", description: "", x: { label: "X", categories: ["a"] }, low: { label: "L", format: "integer" }, high: { label: "H", format: "integer" }, color: "series-1" },
        [{ x: "a", low: 1, high: 2 }],
      ],
      [{ type: "map", description: "", map: "world", region: { label: "C" }, value: { label: "N", format: "integer" }, color: "series-1" }, [{ region: "FRA", value: 1 }]],
    ]
    for (const [settings, rows] of cases) expect(undefinedPaths(chartOption(settings, rows, theme())), settings.type).toEqual([])
  })
})

describe("withAlpha", () => {
  it("reads an hsl or a hex colour at an opacity, and leaves any other as it is", () => {
    expect(withAlpha("hsl(220, 70%, 50%)", 0.4)).toBe("hsla(220, 70%, 50%, 0.4)")
    expect(withAlpha("#5071de", 0.4)).toBe("rgba(80, 113, 222, 0.4)")
    expect(withAlpha("red", 0.4)).toBe("red")
  })
})

// A still picture (Download PNG, an email, a PDF) has no pointer: dashboards-design.md
// § 14.5 takes the interaction parts out, and the animation, so it is drawn complete.
describe("stillOption", () => {
  it("drops the tooltip, zoom, toolbox and brush, stops the animation, and keeps the rest", () => {
    const screen = {
      tooltip: { trigger: "axis" },
      dataZoom: [{ type: "slider" }],
      toolbox: { feature: { saveAsImage: {} } },
      brush: { toolbox: ["rect"] },
      animation: true,
      xAxis: { type: "category" },
      series: [{ type: "bar", data: [1, 2] }],
    } as unknown as ChartOption
    expect(stillOption(screen)).toEqual({ animation: false, xAxis: { type: "category" }, series: [{ type: "bar", data: [1, 2] }] })
    // The screen's option is left as it was.
    expect(Object.keys(screen)).toEqual(["tooltip", "dataZoom", "toolbox", "brush", "animation", "xAxis", "series"])
  })
})

// KI-CHART-DECIMAL-AXIS-REPEATS-TICKS: a format's fixed fraction digits wrote ticks finer
// than it as one label (a rate axis read 0, 0, 0, 0.1, 0.1, 0.1) and a step's edges as one
// number. Every value axis of every chart type the goldens draw, in each format with
// decimals, writes each tick as the tick, so neighbouring ticks never read the same.
describe("a value axis's ticks", () => {
  const GOLDENS = resolve(__dirname, "__goldens__/charts")
  const views = readdirSync(GOLDENS)
    .filter((f) => f.endsWith(".view.json"))
    .map((f) => ({ name: f, ...(JSON.parse(readFileSync(resolve(GOLDENS, f), "utf8")) as { kind: string; settings: ChartSettings; rows: object[] }) }))
    .filter((v) => v.kind === "echart")
  // Every measure of the settings in one format, the axis's own format too.
  const inFormat = (settings: ChartSettings, format: string): ChartSettings =>
    JSON.parse(JSON.stringify(settings), (k, v: unknown) => (k === "format" || k === "axisFormat" ? format : v)) as ChartSettings
  type ValueAxis = { type?: string; axisLabel?: { formatter?: (v: number) => string } }
  const valueAxes = (o: ChartOption) => {
    const { xAxis, yAxis } = o as { xAxis?: ValueAxis | ValueAxis[]; yAxis?: ValueAxis | ValueAxis[] }
    return [xAxis, yAxis].flat().filter((a): a is ValueAxis => a?.type === "value")
  }
  const TICKS = [
    [0, 0.01, 0.02, 0.03, 0.04, 0.05, 0.06],
    [1.02, 1.04, 1.06, 1.08],
    [0.0101, 0.0102, 0.0103],
    [-0.15, -0.1, -0.05, 0, 0.05],
    [1_010_000, 1_020_000, 1_030_000],
    [1_200, 1_250, 1_300],
  ]

  it("is on every chart type drawn on a grid, and on both sides of a combo", () => {
    const types = new Set(views.filter((v) => valueAxes(chartOption(v.settings, v.rows, FULL)).length > 0).map((v) => v.settings.type))
    expect([...types].sort()).toEqual(
      ["area", "bar", "boxplot", "bubble", "bullet", "candlestick", "hbar", "histogram", "line", "range", "scatter", "waterfall"].sort(),
    )
    const combo = views.find((v) => v.name === "combo_two_axes.view.json")
    expect(combo && valueAxes(chartOption(combo.settings, combo.rows, FULL))).toHaveLength(2)
  })

  for (const format of ["decimal", "compact", "percent"]) {
    it(`writes each tick apart from its neighbours in ${format}, on every chart type`, () => {
      for (const v of views) {
        for (const axis of valueAxes(chartOption(inFormat(v.settings, format), v.rows, FULL))) {
          const label = axis.axisLabel?.formatter
          expect(label, v.name).toBeTypeOf("function")
          for (const ticks of TICKS) {
            const labels = ticks.map((t) => label!(t))
            expect(new Set(labels).size, `${v.name}: ${labels.join(" ")}`).toBe(ticks.length)
          }
        }
      }
    })
  }

  it("writes a rate's tick as the rate", () => {
    const rate = views.find((v) => v.name === "line_count_distinct.view.json")!
    const [axis] = valueAxes(chartOption(inFormat(rate.settings, "decimal"), rate.rows, FULL))
    expect([0, 0.01, 0.02, 0.03, 0.04, 0.05, 0.06].map((t) => axis.axisLabel!.formatter!(t))).toEqual(["0", "0.01", "0.02", "0.03", "0.04", "0.05", "0.06"])
  })
})

describe("a step's edges in a legend", () => {
  const HEAT: HeatmapChartSettings = {
    type: "heatmap",
    description: "A rate by day and region",
    x: { field: "x", label: "Day", categories: ["Oct 3", "Oct 4", "Oct 5"] },
    y: { field: "y", label: "Region", categories: ["APAC", "EU"] },
    value: { field: "v", label: "Rate", format: "decimal" },
    color: "series-1",
  }
  const cells = (values: number[]) => values.map((v, i) => ({ x: HEAT.x.categories[i % 3], y: HEAT.y.categories[Math.floor(i / 3)], v }))

  it.each([
    ["decimal", [1.02, 1.03, 1.04, 1.05, 1.06, 1.07], ["1.02–1.03", "1.03–1.04", "1.04–1.05", "1.05–1.06", "1.06–1.07"]],
    ["percent", [0.01, 0.012, 0.014, 0.016, 0.018, 0.02], ["1%–1.2%", "1.2%–1.4%", "1.4%–1.6%", "1.6%–1.8%", "1.8%–2%"]],
    ["compact", [1_000_000, 1_020_000, 1_040_000, 1_060_000, 1_080_000, 1_100_000], ["1M–1.02M", "1.02M–1.04M", "1.04M–1.06M", "1.06M–1.08M", "1.08M–1.1M"]],
  ] as const)("names five steps apart in %s, each edge as precise as the steps are wide", (format, values, labels) => {
    const settings = { ...HEAT, value: { ...HEAT.value, format } }
    expect(chartLegend(settings, FULL, cells([...values])).map((l) => l.label)).toEqual(labels)
  })
})

describe("a measure under 1", () => {
  const RANGE: RangeChartSettings = {
    type: "range",
    description: "A rate's band by day",
    x: { label: "Day", categories: ["Oct 1"] },
    low: { label: "Low", format: "decimal" },
    high: { label: "High", format: "decimal" },
    color: "series-1",
  }

  it("reads as itself in the tooltip and the table, not as 0", () => {
    const rows = [{ x: "Oct 1", low: 0.01, high: 0.03 }]
    const { formatter } = (chartOption(RANGE, rows, ALL) as unknown as { tooltip: { formatter: Formatter } }).tooltip
    const html = formatter([{ dataIndex: 0 }])
    expect(html).toContain(">0.01<")
    expect(html).toContain(">0.03<")
    expect(chartTable(RANGE, rows).body).toEqual([["Oct 1", "0.01", "0.03"]])
  })
})
