/**
 * Chart settings, rows and the theme as an ECharts option: one translator per
 * chart type, the only place that knows what ECharts calls things
 * (dashboards-design.md § 3). Type imports only: ECharts itself loads with
 * EChartNoSSR, so this file is safe anywhere.
 */

import type {
  BarSeriesOption,
  BoxplotSeriesOption,
  CandlestickSeriesOption,
  FunnelSeriesOption,
  HeatmapSeriesOption,
  LineSeriesOption,
  MapSeriesOption,
  PieSeriesOption,
  SankeySeriesOption,
  ScatterSeriesOption,
  SunburstSeriesOption,
  TreemapSeriesOption,
} from "echarts/charts"
import type {
  CalendarComponentOption,
  GridComponentOption,
  TooltipComponentOption,
  VisualMapComponentOption,
} from "echarts/components"
import type { ComposeOption } from "echarts/core"

import {
  binLabel,
  CHART_HEIGHT,
  formatEdge,
  formatNumber,
  formatTick,
  WATERFALL_COLORS,
  type BandRow,
  type BoxplotChartSettings,
  type BoxRow,
  type BulletChartSettings,
  type BulletRow,
  type CalendarChartSettings,
  type CandlestickChartSettings,
  type ChartMark,
  type ChartSeries,
  type ChartSettings,
  type DayRow,
  type FlowRow,
  type FunnelChartSettings,
  type HeatmapChartSettings,
  type HistogramChartSettings,
  type MapChartSettings,
  type NumberFormat,
  type PartRow,
  type PartsChartSettings,
  type PieChartSettings,
  type PointField,
  type PriceRow,
  type RangeChartSettings,
  type RegionRow,
  type SankeyChartSettings,
  type ScatterChartSettings,
  type WaterfallChartSettings,
  type XYSettings,
} from "@/components/charts/chart-settings"
import { FALLBACK_COLORS, type ChartColor, type ChartTheme } from "@/components/charts/chart-theme"
import { ECHARTS_MAPS, regionName } from "@/components/charts/map-regions"
import { tokenHex } from "@/lib/export/print-theme"

export type ChartOption = ComposeOption<
  | BarSeriesOption
  | LineSeriesOption
  | HeatmapSeriesOption
  | PieSeriesOption
  | FunnelSeriesOption
  | ScatterSeriesOption
  | TreemapSeriesOption
  | SunburstSeriesOption
  | SankeySeriesOption
  | BoxplotSeriesOption
  | CandlestickSeriesOption
  | MapSeriesOption
  | GridComponentOption
  | CalendarComponentOption
  | TooltipComponentOption
  | VisualMapComponentOption
>

export interface LegendItem {
  label: string
  color: string
}

/** The light "other" colour, for an Other series when the tokens are unset. */
const OTHER_FALLBACK = "#8e8e98"

function tokensOf(settings: ChartSettings): ChartColor[] {
  switch (settings.type) {
    case "heatmap":
      return [settings.color]
    case "pie":
    case "donut":
      return settings.slices.values.map((s) => s.color)
    case "waterfall":
      return [...WATERFALL_COLORS]
    case "treemap":
    case "sunburst":
      return [...settings.groups.map((g) => g.color), "other"]
    case "sankey":
      return settings.nodes.map((n) => n.color)
    case "calendar":
    case "bullet":
    case "boxplot":
    case "range":
    case "map":
      return [settings.color]
    case "candlestick":
      return [WATERFALL_COLORS[0], WATERFALL_COLORS[1]]
    default:
      return settings.series.values.map((s) => s.color)
  }
}

/**
 * The series' colours from the theme. If any token is unset the chart takes the
 * fallback colours in order instead, rather than leave one series to the
 * library's palette, where it could get another series' colour. Other keeps its
 * grey.
 */
export function seriesColors(settings: ChartSettings, theme: ChartTheme): string[] {
  const tokens = tokensOf(settings)
  const colors = tokens.map((t) => theme.colors[t])
  if (colors.every(Boolean)) return colors
  let next = 0
  return tokens.map((t) => (t === "other" ? OTHER_FALLBACK : FALLBACK_COLORS[next++ % FALLBACK_COLORS.length]))
}

/**
 * A legend only when there is more than one series: a lone series is named by the
 * chart's title. A heatmap's legend is its five steps and the values each covers, or
 * the one value when every cell holds it; a
 * pie's, every slice with its share, so a still picture with no tooltip says them; a
 * waterfall's, what its three colours mean.
 */
export function chartLegend(settings: ChartSettings, theme: ChartTheme, rows: readonly object[] = []): LegendItem[] {
  switch (settings.type) {
    case "pie":
    case "donut": {
      const colors = seriesColors(settings, theme)
      const values = pieValues(settings, rows)
      const total = values.reduce<number>((t, v) => t + (v !== null && v > 0 ? v : 0), 0)
      return settings.slices.values.map((s, i) => ({
        label: total > 0 ? `${s.label} · ${formatNumber(Math.max(values[i] ?? 0, 0) / total, "percent")}` : s.label,
        color: colors[i],
      }))
    }
    case "waterfall": {
      const [rise, fall, total] = seriesColors(settings, theme)
      const items = [
        { label: "Increase", color: rise },
        { label: "Decrease", color: fall },
      ]
      return settings.total ? [...items, { label: "Total", color: total }] : items
    }
    case "heatmap":
    case "calendar":
    case "map": {
      const steps =
        settings.type === "heatmap"
          ? heatmapSteps(settings, rows)
          : stepsOf((settings.type === "calendar" ? dayRows(rows) : regionRows(rows)).map((r) => r.value))
      const [hue] = seriesColors(settings, theme)
      // The steps are equally wide: each edge keeps the digits that tell them apart.
      const width = steps.length > 0 ? steps[0].to - steps[0].from : 0
      const edge = (v: number) => formatEdge(v, width, settings.value.format)
      // A swatch in the colour the chart shows the step in.
      const swatch = (i: number) => (settings.type === "heatmap" ? withAlpha(hue, STEP_ALPHA[i]) : filledStep(hue, i, theme))
      // Every value the same: one entry, in the last step, where stepOf draws them all.
      if (steps.length > 0 && width === 0) return [{ label: edge(steps[0].from), color: swatch(steps.length - 1) }]
      return steps.map((s, i) => ({ label: `${edge(s.from)}–${edge(s.to)}`, color: swatch(i) }))
    }
    case "treemap":
    case "sunburst": {
      // The top-level groups, and Other when a part is in none of them.
      const colors = seriesColors(settings, theme)
      const named = new Set(settings.groups.map((g) => g.key))
      const items = settings.groups.map((g, i) => ({ label: g.label, color: colors[i] }))
      if (partRows(rows).some((r) => !named.has(r.path[0]))) items.push({ label: "Other", color: colors[colors.length - 1] })
      return items.length < 2 ? [] : items
    }
    case "bullet": {
      // The two numbers, so a picture with no tooltip says them.
      const [color] = seriesColors(settings, theme)
      const row = bulletRow(rows)
      if (!row) return []
      const items = [{ label: `${settings.value.label} · ${formatNumber(row.value, settings.value.format)}`, color }]
      if (row.target !== null) {
        items.push({ label: `${settings.target.label} · ${formatNumber(row.target, targetFormat(settings))}`, color: theme.text || OTHER_FALLBACK })
      }
      return items
    }
    case "candlestick": {
      const [rise, fall] = seriesColors(settings, theme)
      return [
        { label: "Close above open", color: rise },
        { label: "Close below open", color: fall },
      ]
    }
    // A sankey's names are on its nodes; a box plot and a range band are one series.
    case "sankey":
    case "boxplot":
    case "range":
      return []
  }
  if (settings.series.values.length < 2) return []
  const colors = seriesColors(settings, theme)
  return settings.series.values.map((s, i) => ({ label: s.label, color: colors[i] }))
}

const field = (row: object, name: string): unknown => (row as Record<string, unknown>)[name]

const numberOf = (v: unknown): number | null => (typeof v === "number" && Number.isFinite(v) ? v : null)

/**
 * The measure per series and category, null where no row has one. Without a series
 * field each series reads its own value from every row: a combo's two measures.
 */
function valueGrid(settings: XYSettings, rows: readonly object[]): (number | null)[][] {
  const categories = new Map(settings.x.categories.map((c, i) => [c, i]))
  const series = new Map(settings.series.values.map((s, i) => [s.key, i]))
  const grid = settings.series.values.map(() => settings.x.categories.map((): number | null => null))
  for (const row of rows) {
    const ci = categories.get(String(field(row, settings.x.field)))
    if (ci === undefined) continue
    if (settings.series.field) {
      const si = series.get(String(field(row, settings.series.field)))
      if (si !== undefined) grid[si][ci] = numberOf(field(row, settings.y.field))
      continue
    }
    settings.series.values.forEach((s, si) => {
      grid[si][ci] = numberOf(field(row, s.value ?? settings.y.field))
    })
  }
  return grid
}

/** Each value as its share of its category's total (of the values' sizes), for a 100% chart. */
function shares(grid: (number | null)[][]): (number | null)[][] {
  const totals = (grid[0] ?? []).map((_, ci) => grid.reduce((t, values) => t + Math.abs(values[ci] ?? 0), 0))
  return grid.map((values) => values.map((v, ci) => (v === null ? null : totals[ci] === 0 ? 0 : v / totals[ci])))
}

/** How a series' values are written: its own format (a combo's second measure), else the chart's. */
const formatOf = (settings: XYSettings, s: ChartSeries | undefined): NumberFormat => s?.format ?? settings.y.format

/** A series' mark: its own (a combo's measure), else its type's. */
const markOf = (settings: XYSettings, s: ChartSeries): ChartMark =>
  s.mark ?? (settings.type === "line" ? "line" : settings.type === "area" ? "area" : "bar")

const stackedOf = (settings: XYSettings): boolean => ("stacked" in settings ? settings.stacked : false)

/** Each category's first row, where its details come from. */
function firstRows(settings: XYSettings, rows: readonly object[]): Map<string, object> {
  const first = new Map<string, object>()
  for (const row of rows) {
    const c = String(field(row, settings.x.field))
    if (!first.has(c)) first.set(c, row)
  }
  return first
}

function detailText(row: object | undefined, d: { field: string; format: NumberFormat }): string {
  const v = row ? numberOf(field(row, d.field)) : null
  return v === null ? "" : formatNumber(v, d.format)
}

const escapeHtml = (s: string) =>
  s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!)

interface AxisPoint {
  dataIndex: number
  seriesIndex?: number
}

const ROUNDED_TOP = [3, 3, 0, 0]
const ROUNDED_RIGHT = [0, 3, 3, 0]

// A key set to undefined is not left out: ECharts merges it over its default, and
// an axis label with an undefined colour is drawn with none. An unset token is
// left out instead.
const colorOf = (c: string) => (c ? { color: c } : {})

/** The widest a bar is drawn: 80% of its category's band (ECharts' gap between categories), up to this. */
const BAR_MAX = 28
/** The narrowest bar parted from the next by a line of the card's colour, so 4 px of its own colour show. */
const PARTED = 6
/** The least a treemap's group is across, as a square, to name itself in a strip along its top: the 16 px strip and its line take a third of it at most. */
const STRIP_ROOM = 64
/** The least a treemap's group is across, as a square, to keep its 2 px line: the line takes a quarter of it at most. */
const LINED = 30
/** A sunburst's names: their size, and the room kept between a name and its ring's edges. */
const SUNBURST_FONT = 10
/** A treemap's names. */
const PART_FONT = 11
const SUNBURST_PAD = 3

/**
 * Each series as a bar or a line, by its mark; a combo's measure on the right reads
 * against the second axis. `band` is each category's width (its height on an hbar), when it is known.
 */
function xySeries(
  settings: XYSettings,
  grid: (number | null)[][],
  colors: string[],
  theme: ChartTheme,
  band: number | undefined,
): (BarSeriesOption | LineSeriesOption)[] {
  const stacked = stackedOf(settings)
  const marks = settings.series.values.map((s) => markOf(settings, s))
  const histogram = settings.type === "histogram"
  // A line of the card's colour parts a histogram's bars and a stack's pieces. ECharts
  // draws it over the edge of each bar, so a bar under PARTED px would be all line.
  const drawn = band === undefined ? undefined : histogram ? band : Math.min(BAR_MAX, band * 0.8)
  const gap = ((stacked && grid.length > 1) || histogram) && (drawn === undefined || drawn >= PARTED)
  const rounded = settings.type === "hbar" ? ROUNDED_RIGHT : ROUNDED_TOP
  // The end of each stack is rounded: the last bar series with a value there. A
  // histogram's bars touch, square.
  const top = settings.x.categories.map((_, ci) =>
    grid.findLastIndex((values, si) => marks[si] === "bar" && (values[ci] ?? 0) !== 0),
  )
  // The highlighted category is outlined in the text colour, which stands apart from every series colour.
  const marked = settings.type === "bar" && settings.highlight !== undefined ? settings.x.categories.indexOf(settings.highlight) : -1
  const outline = theme.text ? { borderColor: theme.text, borderWidth: 2 } : {}
  return settings.series.values.map((s, si): BarSeriesOption | LineSeriesOption => {
    const axis = s.axis === "right" ? { yAxisIndex: 1 } : {}
    if (marks[si] !== "bar") return { ...lineOf(s, marks[si] === "area", stacked, grid[si], colors[si], band), ...axis }
    return {
      type: "bar",
      name: s.label,
      ...(stacked ? { stack: "total" } : {}),
      ...(histogram ? { barCategoryGap: "0%" } : { barMaxWidth: BAR_MAX }),
      ...(settings.type === "bar" && settings.minBarHeight ? { barMinHeight: settings.minBarHeight } : {}),
      // The line is set on every draw, none as undefined and 0: a redraw at a new size merges
      // into the chart (EChart.tsx), and a key left out would keep the last draw's line.
      itemStyle: { color: colors[si], borderColor: gap ? theme.surface : undefined, borderWidth: gap && theme.surface ? 1 : 0 },
      data: grid[si].map((v, ci) => {
        if (v === null || histogram) return v
        const end = !stacked || top[ci] === si
        if (!end && ci !== marked) return v
        return { value: v, itemStyle: { ...(end ? { borderRadius: rounded } : {}), ...(ci === marked ? outline : {}) } }
      }),
      ...axis,
    }
  })
}

/**
 * A line's points, `size` across, and its values. A value with no value either side has
 * no line to it: its point is all that draws it. ECharts draws only the points under a
 * label where a category's `band` is under 1.5 points across, so where a value is alone
 * every point is drawn; and where the categories are that close, a joined value's point
 * is left out, as the line shows it and the points would be drawn one over another.
 * `band` may be more than the drawn one, not less. Both keys are set on every draw: a
 * redraw at a new size merges into the chart (EChart.tsx).
 */
function pointsOf(data: (number | null)[], size: number, band: number | undefined): Pick<LineSeriesOption, "showAllSymbol" | "data"> {
  const alone = data.map((v, i) => v !== null && (data[i - 1] ?? null) === null && (data[i + 1] ?? null) === null)
  const some = alone.includes(true)
  const close = some && (band === undefined || band < size * 1.5)
  return {
    showAllSymbol: some ? true : "auto",
    data: close ? data.map((v, i) => (v === null || alone[i] ? v : { value: v, symbol: "none" })) : data,
  }
}

/** A line or an area of one series. `band` is each category's width, when it is known. */
function lineOf(s: ChartSeries, area: boolean, stacked: boolean, data: (number | null)[], color: string, band: number | undefined): LineSeriesOption {
  const stack = area && stacked
  return {
    type: "line",
    name: s.label,
    ...(stack ? { stack: "total" } : {}),
    // A day with no value breaks the line; a point marks a value with no neighbour.
    connectNulls: false,
    showSymbol: true,
    symbol: "circle",
    symbolSize: 7,
    lineStyle: { width: 2, color },
    itemStyle: { color },
    // Areas that overlap are faint, so each shows through the others; stacked
    // ones do not overlap.
    ...(area ? { areaStyle: { color, opacity: stack ? 0.6 : 0.15 } } : {}),
    ...pointsOf(data, 7, band),
  }
}

/** The five steps of a heatmap's hue, light to dark. */
const STEP_ALPHA = [0.2, 0.4, 0.6, 0.8, 1] as const

/** A colour ECharts reads ("hsl(…)" or "#rrggbb") at an opacity. */
export function withAlpha(color: string, alpha: number): string {
  const hsl = /^hsl\((.*)\)$/.exec(color)
  if (hsl) return `hsla(${hsl[1]}, ${alpha})`
  const hex = /^#([0-9a-f]{6})$/i.exec(color)
  if (hex) {
    const n = parseInt(hex[1], 16)
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
  }
  return color
}

/** A colour ECharts reads ("hsl(…)" or "#rrggbb") as its red, green and blue; null for any other. */
function channelsOf(color: string): number[] | null {
  const hsl = /^hsl\(([\d.]+),\s*([\d.]+%),\s*([\d.]+%)\)$/.exec(color)
  const hex = /^#([0-9a-f]{6})$/i.exec(hsl ? tokenHex(hsl.slice(1).join(" ")) : color)
  if (!hex) return null
  const n = parseInt(hex[1], 16)
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255]
}

/**
 * A step of a calendar or a map: the hue at its opacity laid over the border colour a day
 * or a shape with no number is filled with, as one solid colour. A calendar draws its days
 * so; a map's shape has one fill. On the card alone the faintest step is darker than that
 * grey in the dark theme, and lighter in the light one for most hues: it read as less than
 * no number. A heatmap's cells lie on the card: its steps are the hue's opacities.
 */
function filledStep(hue: string, step: number, theme: ChartTheme): string {
  const alpha = STEP_ALPHA[step]
  const top = channelsOf(hue)
  const under = theme.border ? channelsOf(theme.border) : null
  if (!top || !under) return withAlpha(hue, alpha)
  return `#${top.map((c, i) => Math.round(c * alpha + under[i] * (1 - alpha)).toString(16).padStart(2, "0")).join("")}`
}

export interface HeatmapStep {
  from: number
  to: number
}

/** The cells' values, by x and y category; a cell no row has is left out. */
function heatmapCells(settings: HeatmapChartSettings, rows: readonly object[]): { xi: number; yi: number; value: number }[] {
  const xs = new Map(settings.x.categories.map((c, i) => [c, i]))
  const ys = new Map(settings.y.categories.map((c, i) => [c, i]))
  const cells: { xi: number; yi: number; value: number }[] = []
  for (const row of rows) {
    const xi = xs.get(String(field(row, settings.x.field)))
    const yi = ys.get(String(field(row, settings.y.field)))
    const value = numberOf(field(row, settings.value.field))
    if (xi === undefined || yi === undefined || value === null) continue
    cells.push({ xi, yi, value })
  }
  return cells
}

/** Five equal steps from a heatmap's smallest value to its largest; none when there is no value. */
export function heatmapSteps(settings: HeatmapChartSettings, rows: readonly object[]): HeatmapStep[] {
  return stepsOf(heatmapCells(settings, rows).map((c) => c.value))
}

/** Five equal steps from the smallest value to the largest; none when there is no value. */
function stepsOf(values: number[]): HeatmapStep[] {
  if (values.length === 0) return []
  const min = Math.min(...values)
  const max = Math.max(...values)
  const width = (max - min) / STEP_ALPHA.length
  return STEP_ALPHA.map((_, i) => ({ from: min + width * i, to: i === STEP_ALPHA.length - 1 ? max : min + width * (i + 1) }))
}

/** The step a value is in: the last one when every value is the same. */
function stepOf(steps: HeatmapStep[], value: number): number {
  const min = steps[0].from
  const max = steps[steps.length - 1].to
  if (max === min) return steps.length - 1
  return Math.min(steps.length - 1, Math.floor(((value - min) / (max - min)) * steps.length))
}

function heatmapOption(settings: HeatmapChartSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number): ChartOption {
  const cells = heatmapCells(settings, rows)
  const steps = heatmapSteps(settings, rows)
  const [hue] = seriesColors(settings, theme)
  const muted = colorOf(theme.muted)
  const border = colorOf(theme.border)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const axis = (categories: string[]) => ({
    type: "category" as const,
    data: categories,
    axisTick: { show: false },
    axisLine: { lineStyle: border },
    axisLabel: { ...muted, fontSize: 11 },
    splitArea: { show: false },
  })
  const tooltip = (p: { data?: [number, number, number, number] }): string => {
    const [xi, yi, value] = p.data ?? [0, 0, 0]
    return (
      `<div style="font-weight:500;margin-bottom:2px">${escapeHtml(settings.x.categories[xi] ?? "")} · ${escapeHtml(settings.y.categories[yi] ?? "")}</div>` +
      `<div style="display:flex;gap:8px"><span style="flex:1">${escapeHtml(settings.value.label)}</span>` +
      `<span style="font-variant-numeric:tabular-nums;font-weight:500">${escapeHtml(formatNumber(value, settings.value.format))}</span></div>`
    )
  }
  // The x labels share what the y categories' names leave of the width.
  const room = Math.ceil(Math.max(0, ...settings.y.categories.map((c) => textWidth(c)))) + LABEL_MARGIN
  const labels = bottomLabels(settings.x.categories, width, room, 8, dated(settings.x.categories))
  // A cell's width and its height: the height the picture's less the room above the plot
  // and the labels and their title under it.
  const top = settings.y.label ? 28 : 10
  const down = height - top - LABEL_MARGIN - labels.depth - (settings.x.label ? 4 + AXIS_FONT + 2 : 0)
  const cell = Math.min(labels.plot / Math.max(1, settings.x.categories.length), down / Math.max(1, settings.y.categories.length))
  return {
    textStyle: font,
    grid: { ...labels.grid, top, bottom: 0 },
    xAxis: {
      ...axis(settings.x.categories),
      ...nameUnder(settings.x.label, muted, LABEL_MARGIN + labels.depth + 4, labels.plot),
      axisLabel: { ...muted, fontSize: 11, ...labels.axisLabel },
    },
    // The first y category at the top, as a table reads, its title above it.
    yAxis: {
      ...axis(settings.y.categories),
      inverse: true,
      ...nameAbove(settings.y.label, muted, labels.plot, "left", "start"),
      axisLabel: { ...muted, ...sideLabels(settings.y.categories.length, down) },
    },
    // ECharts draws a heatmap only through a visualMap. This one is hidden: each
    // cell carries its step, and the legend under the chart names the steps.
    visualMap: {
      type: "piecewise",
      show: false,
      dimension: 3,
      seriesIndex: 0,
      pieces: STEP_ALPHA.map((a, i) => ({ value: i, color: withAlpha(hue, a) })),
    },
    tooltip: {
      trigger: "item",
      confine: true,
      ...(theme.popover ? { backgroundColor: theme.popover } : {}),
      ...(theme.border ? { borderColor: theme.border } : {}),
      textStyle: { ...colorOf(theme.popoverText), ...font, fontSize: 12 },
      formatter: tooltip as TooltipComponentOption["formatter"],
    },
    series: [
      {
        type: "heatmap",
        name: settings.value.label,
        // The card's colour between the cells, so neighbours of one step stay apart: not
        // between cells under PARTED px either way, which the line would cover. Its width is
        // set on every draw, none included, as a redraw at a new size merges (xySeries).
        itemStyle: { borderColor: cell >= PARTED ? theme.surface : undefined, borderWidth: theme.surface && cell >= PARTED ? 2 : 0, borderRadius: 2 },
        data: cells.map((c) => [c.xi, c.yi, c.value, stepOf(steps, c.value)]),
      },
    ],
  }
}

/** The parts of an option a still picture has no use for: the pointer's tooltip, zoom sliders, the toolbox and brushing. */
const INTERACTIVE = ["tooltip", "dataZoom", "toolbox", "brush"] as const

/**
 * The screen's option for a still picture (Download PNG, an email, a PDF;
 * dashboards-design.md § 14.5): without its interaction parts, and not animated, so
 * the picture is drawn complete.
 */
export function stillOption(option: ChartOption): ChartOption {
  const still: Record<string, unknown> = { ...option, animation: false }
  for (const key of INTERACTIVE) delete still[key]
  return still as ChartOption
}

/** The tooltip's look, from the theme: the same box for every type. */
function tooltipStyle(theme: ChartTheme): TooltipComponentOption {
  return {
    confine: true,
    ...(theme.popover ? { backgroundColor: theme.popover } : {}),
    ...(theme.border ? { borderColor: theme.border } : {}),
    textStyle: { ...colorOf(theme.popoverText), ...(theme.font ? { fontFamily: theme.font } : {}), fontSize: 12 },
  }
}

const tooltipTitle = (text: string) => `<div style="font-weight:500;margin-bottom:2px">${escapeHtml(text)}</div>`

const tooltipLine = (swatch: string, label: string, value: string) =>
  `<div style="display:flex;align-items:center;gap:8px">${swatch}<span style="flex:1">${escapeHtml(label)}</span>` +
  `<span style="font-variant-numeric:tabular-nums;font-weight:500">${escapeHtml(value)}</span></div>`

const swatchOf = (color: string) =>
  `<span style="display:inline-block;width:8px;height:8px;border-radius:2px;background:${color}"></span>`

const NO_SWATCH = `<span style="display:inline-block;width:8px"></span>`

/** A measure where a category or a shape has no value: said, never written as 0. */
const noValue = (label: string, swatch = NO_SWATCH) => tooltipLine(swatch, label, "No value")

/** The option for a chart `width` pixels across: its axes' words are laid out to fit that. */
export function chartOption(
  settings: ChartSettings,
  rows: readonly object[],
  theme: ChartTheme,
  width = DEFAULT_WIDTH,
  height = CHART_HEIGHT,
  measure?: TextMeasure,
): ChartOption {
  switch (settings.type) {
    case "heatmap":
      return heatmapOption(settings, rows, theme, width, height)
    case "pie":
    case "donut":
      return pieOption(settings, rows, theme, width, height)
    case "funnel":
      return funnelOption(settings, rows, theme, width, height, measure ?? drawnWidths(theme.font))
    case "waterfall":
      return waterfallOption(settings, rows, theme, width)
    case "scatter":
    case "bubble":
      return scatterOption(settings, rows, theme, width, height)
    case "treemap":
    case "sunburst":
      return partsOption(settings, rows, theme, width, height)
    case "sankey":
      return sankeyOption(settings, rows, theme, height)
    case "calendar":
      return calendarOption(settings, rows, theme, width)
    case "bullet":
      return bulletOption(settings, rows, theme, width)
    case "boxplot":
      return boxplotOption(settings, rows, theme, width)
    case "candlestick":
      return candlestickOption(settings, rows, theme, width)
    case "range":
      return rangeOption(settings, rows, theme, width)
    case "map":
      return mapOption(settings, rows, theme)
    case "histogram": {
      const fitted = binsFitted(settings, rows, width)
      return xyOption(fitted.settings, fitted.rows, theme, width, height)
    }
    default:
      return xyOption(settings, rows, theme, width, height)
  }
}

/** The fewest pixels a histogram's bar is drawn across: more bins than that allows are added up. */
const BIN_MIN = 3

/**
 * A histogram's bins as many as fit the width at BIN_MIN pixels a bar: each `per` neighbours
 * added up into one bar, named for the range they cover. A histogram counts its rows, so a
 * bar's value is its bins' sum. The table still lists every bin.
 */
function binsFitted(
  settings: HistogramChartSettings,
  rows: readonly object[],
  width: number,
): { settings: HistogramChartSettings; rows: readonly object[]; per: number } {
  const bins = settings.bins
  const n = settings.x.categories.length
  const per = Math.ceil(n / Math.max(1, Math.floor(width / BIN_MIN)))
  if (!bins || bins.lo.length !== n || per <= 1) return { settings, rows, per: 1 }
  const values = new Map(rows.map((r) => [String(field(r, settings.x.field)), numberOf(field(r, settings.y.field)) ?? 0]))
  const categories: string[] = []
  const merged: object[] = []
  for (let i = 0; i < n; i += per) {
    const last = Math.min(n, i + per) - 1
    const label = binLabel(bins.lo[i], bins.lo[last] + bins.width - bins.lo[i])
    let v = 0
    for (let k = i; k <= last; k++) v += values.get(settings.x.categories[k]) ?? 0
    categories.push(label)
    merged.push({ [settings.x.field]: label, [settings.y.field]: v })
  }
  return { settings: { ...settings, x: { ...settings.x, categories } }, rows: merged, per }
}

// An axis's words are laid out for the width the chart is drawn at: ECharts left to
// itself hides a category's name when its neighbour's would touch it, and a reader
// cannot tell which bar is which. Text is measured at 0.6 em a letter, a little wider
// than the page's font draws it, so what fits here fits there.

/** The width a chart is laid out for when its caller does not say: the builder's preview. */
const DEFAULT_WIDTH = 568
const AXIS_FONT = 11
/** The room ECharts leaves between an axis and its labels. */
const LABEL_MARGIN = 8
/** Half the line of an 11 px name, rounded up: the room a name centred on a mark at the top or the bottom needs to stay in the picture. */
const HALF_LINE = 6
/** A funnel stage's label: its name, its value and its share of the first. */
const FUNNEL_FONT = 11
/** The least room between two labels side by side. */
const LABEL_GAP = 8
/** A turned label's line across, with a little room either side. */
const TURNED_LINE = AXIS_FONT + 3
/** A turned label's longest; a longer one is cut short. */
const TURNED_WIDTH = 64
/** A category that is a day, a month, a quarter or a year, as saved-chart.ts writes a date's grain. */
const DATED = /^\d{4}(-\d{2}(-\d{2})?| Q[1-4])?$/
const dated = (categories: string[]) => categories.length > 0 && categories.every((c) => DATED.test(c))

/** The letters a full em across: Chinese, Japanese and Korean, and the full-width forms. */
const WIDE_LETTERS = /[\u2e80-\u9fff\uac00-\ud7af\uf900-\ufaff\uff00-\uff60]/g
/**
 * How wide a text is written: 0.6 em a letter, wider than the page's font draws a Latin
 * one, and a full em for a letter of a wide script, as it is drawn.
 */
const textWidth = (text: string, size = AXIS_FONT) => size * (0.6 * text.length + 0.4 * (text.match(WIDE_LETTERS)?.length ?? 0))
/** How wide `text` is drawn at `size` px, as zrender measures it to cut a label. */
export type TextMeasure = (text: string, size: number) => number
/**
 * The widths the page draws in `font`, measured on a canvas as zrender measures a label;
 * the estimate where there is no canvas (the server, a test). The estimate is wider than
 * the page's font: a funnel fitted by it cut "Subscriptions" on prod where the whole name
 * had room.
 */
function drawnWidths(font: string | undefined): TextMeasure {
  let ctx: CanvasRenderingContext2D | null = null
  try {
    ctx = typeof document === "undefined" ? null : document.createElement("canvas").getContext("2d")
  } catch {
    ctx = null
  }
  if (!ctx || typeof ctx.measureText !== "function") return (text, size) => textWidth(text, size)
  const canvas = ctx
  return (text, size) => {
    canvas.font = `${size}px ${font || "sans-serif"}`
    return canvas.measureText(text).width
  }
}
/**
 * How wide a name is written at most: 0.72 em a letter, as wide as the page's font draws
 * a capital (0.6 em is under one: "US" at 10 px is 14 px, not 12), and a full em for a
 * letter of a wide script.
 */
const nameWidth = (text: string, size: number) => size * (0.72 * text.length + 0.28 * (text.match(WIDE_LETTERS)?.length ?? 0))

/** The least and greatest a value axis reaches: each category's stack, or each value, and zero. */
function reachOf(grid: (number | null)[][], stacked: boolean): [number, number] {
  let lo = 0
  let hi = 0
  for (let ci = 0; ci < (grid[0]?.length ?? 0); ci++) {
    let up = 0
    let down = 0
    for (const values of grid) {
      const v = values[ci] ?? 0
      if (stacked) [up, down] = v > 0 ? [up + v, down] : [up, down + v]
      else [up, down] = [Math.max(up, v), Math.min(down, v)]
    }
    ;[lo, hi] = [Math.min(lo, down), Math.max(hi, up)]
  }
  return [lo, hi]
}

/** The ticks ECharts puts on a value axis that reaches lo and hi, about `split` of them, each a round step apart. */
function ticksOf([lo, hi]: [number, number], split: number, integer: boolean): number[] {
  const span = hi - lo
  if (!(span > 0) || !Number.isFinite(span)) return [lo, hi]
  const raw = span / split
  const unit = 10 ** Math.floor(Math.log10(raw))
  const f = raw / unit
  const step = Math.max(integer ? 1 : 0, (f < 1.5 ? 1 : f < 2.5 ? 2 : f < 4 ? 3 : f < 7 ? 5 : 10) * unit)
  const first = Math.floor(lo / step)
  return Array.from({ length: Math.ceil(hi / step) - first + 1 }, (_, i) => Number(((first + i) * step).toPrecision(12)))
}

/** How wide a value axis's widest tick is written. */
const tickWidth = (reach: [number, number], format: NumberFormat, split = 5) =>
  Math.max(0, ...ticksOf(reach, split, format === "integer").map((t) => textWidth(formatTick(t, format))))

/**
 * The labels under a chart `width` across, a category a band, with `room` and `right`
 * taken by the value axes either side of the plot. Every category is named: flat when
 * each fits its band, else turned 45° (upright when the bands are narrower still) and
 * cut short past TURNED_WIDTH; only upright labels that still do not fit are thinned,
 * evenly. Labels in an order (dates, a histogram's bins) stay flat and are thinned evenly
 * instead, as a reader counts along them. Every key is set each time, so a redraw at a
 * new width merges cleanly over the last. Says the grid's margins, how far below the
 * axis the labels reach and how wide the plot is.
 */
function bottomLabels(categories: string[], width: number, room: number, right: number, ordered: boolean) {
  const n = Math.max(1, categories.length)
  const widest = Math.ceil(Math.max(0, ...categories.map((c) => textWidth(c))))
  const cut = { overflow: "truncate" as const, ellipsis: "…", hideOverlap: true }
  // A flat label is a little wider than its widest, so one is cut short only where the
  // page's font draws it wider than measured here.
  const box = widest + LABEL_GAP / 2
  // The first and last labels drawn sit on their bands' middles, and half of one may
  // reach past the plot: the margins that keep it in narrow every band, so they are
  // found in a few rounds (ECharts finds them once, from bands not yet narrowed).
  const flat = (every: number) => {
    const after = (n - 1) % every
    let [start, end, band] = [room, right, (width - room - right) / n]
    for (let round = 0; round < 6; round++) {
      start = Math.max(room, (box - band) / 2)
      end = Math.max(right, box / 2 - (after + 0.5) * band)
      band = (width - start - end) / n
    }
    return { every, start, end, clear: every * band >= widest + LABEL_GAP }
  }
  const plot = width - room - right
  const band = plot / n
  let fit = flat(Math.max(1, Math.ceil((widest + LABEL_GAP) / band)))
  while (ordered && !fit.clear && fit.every < n) fit = flat(fit.every + 1)
  if (ordered || (fit.every === 1 && fit.clear)) {
    // Where the value axes are wider, ECharts sets the margin by their ticks.
    const grid = { left: fit.start > room ? Math.ceil(fit.start) : 0, right: fit.end > right ? Math.ceil(fit.end) : 8 }
    return { axisLabel: { ...cut, interval: fit.every - 1, rotate: 0, width: box }, depth: AXIS_FONT, grid, plot: width - fit.start - fit.end }
  }
  const grid = { left: 0, right: 8 }
  // Turned, as flat, a little wider than its widest: zrender keeps a px of a label's
  // width back, so one given exactly its widest is cut short.
  const turned = Math.min(box, TURNED_WIDTH)
  // Past the room left of the plot, a 45° first label narrows the plot by the rest.
  const lean = Math.max(0, (turned + AXIS_FONT / 2) * Math.SQRT1_2 - band / 2 - room)
  if (((plot - lean) / n) * Math.SQRT1_2 >= TURNED_LINE) {
    return { axisLabel: { ...cut, interval: 0, rotate: 45, width: turned }, depth: (turned + AXIS_FONT) * Math.SQRT1_2, grid, plot: plot - lean }
  }
  const upright = Math.max(1, Math.ceil(TURNED_LINE / band))
  return { axisLabel: { ...cut, interval: upright - 1, rotate: 90, width: turned }, depth: turned, grid, plot }
}

/** The least a name down the side of a plot is written at; rows narrower than it leave some names out. */
const SIDE_FONT = 8

/**
 * The labels down the side of a plot `down` px tall, a category a row: every row named,
 * at the axis's size where a row has room for it and a line between, smaller, to
 * SIDE_FONT, where not; under that, every so many rows, evenly. Said outright, as
 * ECharts leaves out every other name where a row is under 1.3 times its line.
 */
function sideLabels(n: number, down: number) {
  const row = down / Math.max(1, n)
  const size = Math.min(AXIS_FONT, Math.floor(row) - 2)
  if (size >= SIDE_FONT) return { fontSize: size, interval: 0 }
  return { fontSize: SIDE_FONT, interval: Math.max(1, Math.ceil((SIDE_FONT + 2) / row)) - 1 }
}

/**
 * An axis's title under an axis across, its top `gap` below the axis's line: under its
 * labels. Cut short to `room`, so a long one never pushes the plot in from both sides.
 */
const nameUnder = (name: string | undefined, muted: object, gap: number, room: number) =>
  name
    ? {
        name,
        nameLocation: "middle" as const,
        nameGap: gap,
        nameTextStyle: { ...muted, fontSize: AXIS_FONT },
        nameTruncate: { maxWidth: Math.max(0, Math.floor(room)), ellipsis: "…" },
      }
    : {}

/**
 * An axis's title above an upright axis, read from its line into the plot (`align`
 * left) or, on the right, out of it; cut short to `room`. `at` is the axis's top end:
 * its "start" when it runs top down, a list's categories.
 */
const nameAbove = (name: string | undefined, muted: object, room: number, align: "left" | "right" = "left", at: "start" | "end" = "end") =>
  name
    ? {
        name,
        nameLocation: at,
        nameTextStyle: { ...muted, fontSize: AXIS_FONT, align },
        nameTruncate: { maxWidth: Math.max(0, Math.floor(room)), ellipsis: "…" },
      }
    : {}

/**
 * The ticks of a value axis that runs across a chart `width` wide, with `left` taken by
 * what is left of the plot: about five in the measure's format while each one's words
 * fit apart, else fewer, down to three, else written short (12B, not 12,000,000,000).
 * Words over each other read as one long number, and ECharts drops the last tick's
 * words where they touch the one before. Every key is set each time, as bottomLabels.
 */
function acrossTicks(reach: [number, number], format: NumberFormat, width: number, left: number) {
  const integer = format === "integer"
  const writes: NumberFormat[] = integer || format === "decimal" ? [format, "compact"] : [format]
  const tries = writes.flatMap((f) => [5, 4, 3].map((split) => ({ format: f, split })))
  const fits = ({ format: f, split }: { format: NumberFormat; split: number }) => {
    const ticks = ticksOf(reach, split, integer)
    const widest = Math.max(0, ...ticks.map((t) => textWidth(formatTick(t, f))))
    // The first and the last tick's words reach half their width past the plot's ends.
    const plot = width - Math.max(left, widest / 2) - Math.max(16, widest / 2)
    return plot / Math.max(1, ticks.length - 1) >= widest + LABEL_GAP
  }
  const fit = tries.find(fits) ?? tries[tries.length - 1]
  return { splitNumber: fit.split, axisLabel: { hideOverlap: true, formatter: (v: number) => formatTick(v, fit.format) } }
}

/**
 * The category axis and the measure's axis of a chart drawn on a grid, `width` across,
 * and how wide each category's band is: across, of the width; down, of the `height`.
 */
function axes(
  settings: XYSettings,
  theme: ChartTheme,
  categories: string[],
  reach: { left: [number, number]; right?: [number, number] },
  width: number,
  height?: number,
) {
  const muted = colorOf(theme.muted)
  const border = colorOf(theme.border)
  const across = settings.type === "hbar"
  const axisFormat = settings.percent ? "percent" : (settings.y.axisFormat ?? settings.y.format)
  const categoryAxis = {
    type: "category" as const,
    data: categories,
    axisTick: { alignWithLabel: true, lineStyle: border },
    axisLine: { lineStyle: border },
    axisLabel: { ...muted, fontSize: 11 },
    // Across, the first category is at the top, as a list reads.
    ...(across ? { inverse: true } : {}),
  }
  const valueAxis = {
    type: "value" as const,
    ...(axisFormat === "integer" ? { minInterval: 1 } : {}),
    ...(settings.percent ? { max: 1 } : {}),
    axisLabel: { ...muted, fontSize: 11, formatter: (v: number) => formatTick(v, axisFormat) },
    splitLine: { lineStyle: border },
  }
  // A combo's second measure reads against its own axis on the right, titled with
  // that measure, its ticks on the left axis's grid lines (§ 14.8).
  const y2 = settings.y2
  const rightAxis = y2 && {
    type: "value" as const,
    ...(y2.format === "integer" ? { minInterval: 1 } : {}),
    alignTicks: true,
    axisLabel: { ...muted, fontSize: 11, formatter: (v: number) => formatTick(v, y2.format) },
    splitLine: { show: false },
  }
  const titled = Boolean(settings.y.title || (rightAxis && y2.title))
  if (across) {
    // The categories' names take the room left of the plot; their title is above them.
    const left = Math.ceil(Math.max(0, ...categories.map((c) => textWidth(c)))) + LABEL_MARGIN
    const ticks = acrossTicks(reach.left, axisFormat, width, left)
    const plot = width - left - 16
    const [top, bottom] = [settings.x.label ? 28 : 10, settings.y.title ? 28 : 0]
    // Down, the plot is the height less the room above it and its ticks and title under it.
    const down = height === undefined ? undefined : height - top - bottom - LABEL_MARGIN - AXIS_FONT
    return {
      layout: {
        grid: { left: 0, right: 16, top, bottom },
        xAxis: {
          ...valueAxis,
          ...nameUnder(settings.y.title, muted, 24, plot),
          splitNumber: ticks.splitNumber,
          axisLabel: { ...valueAxis.axisLabel, ...ticks.axisLabel },
        },
        yAxis: {
          ...categoryAxis,
          ...nameAbove(settings.x.label, muted, plot, "left", "start"),
          ...(down === undefined ? {} : { axisLabel: { ...categoryAxis.axisLabel, ...sideLabels(categories.length, down) } }),
        },
      },
      band: down === undefined ? undefined : down / Math.max(1, categories.length),
    }
  }
  // The plot is what the value axes' ticks leave of the width, each with its margin.
  const room = tickWidth(reach.left, axisFormat) + LABEL_MARGIN
  const right = 8 + (y2 && reach.right ? tickWidth(reach.right, y2.format) : 0)
  const labels = bottomLabels(categories, width, room, right, settings.type === "histogram" || dated(categories))
  // Two measures' titles share the top, each half of it.
  const top = rightAxis && settings.y.title && y2.title ? labels.plot / 2 - LABEL_GAP / 2 : labels.plot
  const leftAxis = { ...valueAxis, ...nameAbove(settings.y.title, muted, top) }
  return {
    layout: {
      grid: { ...labels.grid, top: titled ? 28 : 10, bottom: 0 },
      xAxis: {
        ...categoryAxis,
        ...nameUnder(settings.x.label, muted, LABEL_MARGIN + labels.depth + 4, labels.plot),
        axisLabel: { ...categoryAxis.axisLabel, ...labels.axisLabel },
      },
      yAxis: rightAxis ? [leftAxis, { ...rightAxis, ...nameAbove(y2.title, muted, top, "right") }] : leftAxis,
    },
    band: labels.plot / Math.max(1, categories.length),
  }
}

/** Bars, lines and areas on a grid: one measure by series, or a combo's two. */
function xyOption(settings: XYSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number): ChartOption {
  const values = valueGrid(settings, rows)
  // A 100% chart draws shares; its tooltip and table still say the values.
  const grid = settings.percent ? shares(values) : values
  const onRight = settings.series.values.map((s) => s.axis === "right")
  const reach = {
    left: settings.percent ? ([0, 1] as [number, number]) : reachOf(grid.filter((_, si) => !onRight[si]), stackedOf(settings)),
    right: reachOf(grid.filter((_, si) => onRight[si]), stackedOf(settings)),
  }
  const colors = seriesColors(settings, theme)
  const first = firstRows(settings, rows)
  const font = theme.font ? { fontFamily: theme.font } : {}

  const tooltip = (points: AxisPoint | AxisPoint[]): string => {
    const list = Array.isArray(points) ? points : [points]
    const ci = list[0]?.dataIndex ?? 0
    const category = settings.x.categories[ci] ?? ""
    const lines = settings.series.values.flatMap((s, si) => {
      const v = values[si][ci]
      if (v === null) return []
      const share = settings.percent ? ` (${formatNumber(grid[si][ci] ?? 0, "percent")})` : ""
      return [tooltipLine(swatchOf(colors[si]), s.label, formatNumber(v, formatOf(settings, s)) + share)]
    })
    const details = (settings.details ?? []).flatMap((d) => {
      const text = detailText(first.get(category), d)
      return text ? [tooltipLine(NO_SWATCH, d.label, text)] : []
    })
    // A category with no value in any series, a gap in the line, says so rather than nothing.
    const none = lines.length === 0 ? settings.series.values.map((s, si) => noValue(s.label, swatchOf(colors[si]))) : []
    return [tooltipTitle(category), ...lines, ...none, ...details].join("")
  }

  const bars = settings.type === "bar" || settings.type === "hbar" || settings.type === "histogram"
  const { layout, band } = axes(settings, theme, settings.x.categories, reach, width, height)
  return {
    color: colors,
    textStyle: font,
    ...layout,
    tooltip: {
      trigger: "axis",
      axisPointer: { type: bars ? "shadow" : "line" },
      ...tooltipStyle(theme),
      formatter: tooltip as TooltipComponentOption["formatter"],
    },
    series: xySeries(settings, grid, colors, theme, band),
  }
}

/** One step of a waterfall: where its bar starts and how tall it is. */
interface WaterfallStep {
  change: number
  /** The running total after this step. */
  end: number
  base: number
  size: number
  color: string
}

/**
 * Each category's change from where the one before it ended, and a Total from zero
 * when the settings ask for one. A category with no value is skipped: its bar is
 * missing and the next one starts where the last ended.
 */
function waterfallSteps(settings: WaterfallChartSettings, rows: readonly object[], theme: ChartTheme) {
  const [values] = valueGrid(settings, rows)
  const [rise, fall, sum] = seriesColors(settings, theme)
  let run = 0
  const steps = settings.x.categories.map((_, ci): WaterfallStep | null => {
    const v = values?.[ci] ?? null
    if (v === null) return null
    const end = run + v
    const step = { change: v, end, base: Math.min(run, end), size: Math.abs(v), color: v >= 0 ? rise : fall }
    run = end
    return step
  })
  if (!settings.total) return { categories: settings.x.categories, steps, total: null }
  const total = { change: run, end: run, base: Math.min(0, run), size: Math.abs(run), color: sum }
  return { categories: [...settings.x.categories, WATERFALL_TOTAL], steps: [...steps, total], total }
}

const WATERFALL_TOTAL = "Total"

function waterfallOption(settings: WaterfallChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const { categories, steps, total } = waterfallSteps(settings, rows, theme)
  const reach = reachOf([steps.map((s) => (s ? s.base : null)), steps.map((s) => (s ? s.base + s.size : null))], false)
  const format = settings.y.format
  const label = settings.series.values[0]?.label ?? ""
  const font = theme.font ? { fontFamily: theme.font } : {}
  const signed = (v: number) => (v > 0 ? "+" : "") + formatNumber(v, format)
  const tooltip = (points: AxisPoint | AxisPoint[]): string => {
    const list = Array.isArray(points) ? points : [points]
    const ci = list[0]?.dataIndex ?? 0
    const step = steps[ci]
    if (!step) return tooltipTitle(categories[ci] ?? "") + noValue(label)
    const lines =
      total && ci === steps.length - 1
        ? [tooltipLine(swatchOf(step.color), label, formatNumber(step.end, format))]
        : [
            tooltipLine(swatchOf(step.color), label, signed(step.change)),
            tooltipLine(NO_SWATCH, "Running total", formatNumber(step.end, format)),
          ]
    return [tooltipTitle(categories[ci] ?? ""), ...lines].join("")
  }
  // Two stacked bars per category: an invisible one up to where the step starts,
  // and the step itself. "all" stacks a negative base too, so a step that crosses
  // zero still starts where it should.
  const stack = { stack: "waterfall", stackStrategy: "all" as const }
  return {
    textStyle: font,
    ...axes(settings, theme, categories, { left: reach }, width).layout,
    tooltip: {
      trigger: "axis",
      axisPointer: { type: "shadow" },
      ...tooltipStyle(theme),
      formatter: tooltip as TooltipComponentOption["formatter"],
    },
    series: [
      {
        type: "bar",
        ...stack,
        silent: true,
        barMaxWidth: BAR_MAX,
        itemStyle: { color: "transparent" },
        emphasis: { disabled: true },
        data: steps.map((s) => (s ? s.base : null)),
      },
      {
        type: "bar",
        name: label,
        ...stack,
        barMaxWidth: BAR_MAX,
        data: steps.map((s) => (s ? { value: s.size, itemStyle: { color: s.color, borderRadius: 2 } } : null)),
      },
    ],
  }
}

/** A pie's value per slice, in the slices' order; null where no row has one. */
function pieValues(settings: PieChartSettings, rows: readonly object[]): (number | null)[] {
  const slices = new Map(settings.slices.values.map((s, i) => [s.key, i]))
  const values = settings.slices.values.map((): number | null => null)
  for (const row of rows) {
    const i = slices.get(String(field(row, settings.slices.field)))
    if (i !== undefined) values[i] = numberOf(field(row, settings.value.field))
  }
  return values
}

/** Slices clockwise from twelve o'clock, largest first; only those above zero have a size. */
function pieOption(settings: PieChartSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number): ChartOption {
  const values = pieValues(settings, rows)
  const colors = seriesColors(settings, theme)
  const total = values.reduce<number>((t, v) => t + (v !== null && v > 0 ? v : 0), 0)
  // How far round the middle of the ring a slice of the whole goes. A slice under PARTED px
  // there has no line of the card's colour at its sides: the line would cover it.
  const round = 2 * Math.PI * (settings.type === "donut" ? 0.3625 : 0.225) * Math.min(width, height)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const tooltip = (p: { dataIndex?: number }): string => {
    const i = p.dataIndex ?? 0
    const slice = settings.slices.values[i]
    const v = values[i] ?? 0
    return (
      tooltipTitle(slice?.label ?? "") +
      tooltipLine(swatchOf(colors[i]), settings.value.label, formatNumber(v, settings.value.format)) +
      (total > 0 ? tooltipLine(NO_SWATCH, "Share", formatNumber(v / total, "percent")) : "")
    )
  }
  return {
    textStyle: font,
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "pie",
        name: settings.value.label,
        radius: settings.type === "donut" ? ["55%", "90%"] : [0, "90%"],
        // The legend under the chart names each slice and its share.
        label: { show: false },
        labelLine: { show: false },
        itemStyle: theme.surface ? { borderColor: theme.surface, borderWidth: 1 } : {},
        data: settings.slices.values.map((s, i) => {
          const v = values[i]
          const value = v !== null && v > 0 ? v : 0
          const narrow = total > 0 && (value / total) * round < PARTED
          return { name: s.label, value, itemStyle: { color: colors[i], ...(narrow ? { borderWidth: 0 } : {}) } }
        }),
      },
    ],
  }
}

/** Stages top to bottom in the rows' order, each as wide as its value against the largest, labelled at its right. */
function funnelOption(settings: FunnelChartSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number, drawn: TextMeasure): ChartOption {
  const [values] = valueGrid(settings, rows)
  const [color] = seriesColors(settings, theme)
  const format = settings.y.format
  const stages = settings.x.categories.flatMap((name, ci) => {
    const v = values?.[ci] ?? null
    return v === null ? [] : [{ name, value: v }]
  })
  const first = stages[0]?.value ?? 0
  const max = Math.max(0, ...stages.map((s) => s.value))
  // ECharts puts the gap between every two stages whatever their count: past 91 stages in
  // 180 px the gaps alone overfill it, and every stage is drawn upside down. The gaps take
  // at most half the height.
  const gap = stages.length > 1 ? Math.min(2, height / 2 / (stages.length - 1)) : 2
  // A name is centred on its stage: on stages under a line tall the first and last names
  // would stand past the top and the bottom, so the funnel leaves half a line there.
  const edge = (height - gap * (stages.length - 1)) / Math.max(1, stages.length) < 2 * HALF_LINE ? HALF_LINE : 0
  const tall = (height - 2 * edge - gap * (stages.length - 1)) / Math.max(1, stages.length)
  const ofFirst = (v: number) => (first > 0 ? formatNumber(v / first, "percent") : "")
  const numbers = (v: number) => [formatNumber(v, format), ofFirst(v)].filter(Boolean).join(" · ")
  const line = (name: string, v: number) => [name, numbers(v)].filter(Boolean).join(" · ")
  // The funnel takes 60% of the width and its labels the rest, 2% past it; where the
  // labels need more room (a phone's), the funnel narrows to 40%. A label still too wide
  // has its name cut short with "…" and keeps its numbers: on prod a phone's names ran
  // past the right edge. Each is measured as the page draws it, so a name is cut only
  // where zrender would cut it.
  const widest = Math.max(0, ...stages.map((st) => drawn(line(st.name, st.value), FUNNEL_FONT)))
  const share = Math.min(0.6, Math.max(0.4, (width - widest - 1) / Math.max(1, width) - 0.02))
  const labelX = (share + 0.02) * width
  const room = Math.max(0, width - labelX - 1)
  // A name is cut to three letters at the least; past that ECharts cuts the line's end.
  const fit = (name: string, v: number) => {
    const left = room - drawn(` · ${numbers(v)}`, FUNNEL_FONT)
    let n = name.length
    if (n <= 3 || drawn(name, FUNNEL_FONT) <= left) return line(name, v)
    while (n > 3 && drawn(`${name.slice(0, n)}…`, FUNNEL_FONT) > left) n--
    return line(`${name.slice(0, n)}…`, v)
  }
  const texts = stages.map((st) => fit(st.name, st.value))
  // ECharts tapers each stage to the next one's width, which reads as a funnel only where
  // the stages fall: in their names' order a stage of 100 before one of 900 was drawn
  // widening to 900. Stages that do not fall stand each as wide as its own value, an
  // unseen join between each two in place of the gap.
  const stepped = stages.some((st, i) => i > 0 && st.value > stages[i - 1].value)
  const stageOf = (dataIndex = 0) => (stepped ? (dataIndex % 2 === 0 ? dataIndex / 2 : -1) : dataIndex)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const label = settings.series.values[0]?.label ?? ""
  const tooltip = (p: { dataIndex?: number }): string => {
    const stage = stages[stageOf(p.dataIndex)]
    if (!stage) return ""
    return (
      tooltipTitle(stage.name) +
      tooltipLine(swatchOf(color), label, formatNumber(stage.value, format)) +
      (first > 0 ? tooltipLine(NO_SWATCH, `Of ${stages[0].name}`, ofFirst(stage.value)) : "")
    )
  }
  return {
    color: [color],
    textStyle: font,
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "funnel",
        name: label,
        // The rows' order is the stages' order: the server sorted them.
        sort: "none",
        min: 0,
        max,
        minSize: "0%",
        maxSize: "100%",
        left: 0,
        top: edge,
        bottom: edge,
        width: share * width,
        gap: stepped ? 0 : gap,
        label: {
          position: "right",
          ...colorOf(theme.text),
          fontSize: FUNNEL_FONT,
          // ECharts cuts what the page's font still draws past the room; zrender cuts a
          // line a px under its width, so it is given that px back.
          width: room + 1,
          overflow: "truncate",
          ellipsis: "…",
          formatter: (p: { dataIndex?: number }) => texts[stageOf(p.dataIndex)] ?? "",
        },
        labelLine: { length: 8, lineStyle: colorOf(theme.border) },
        // Each label starts past the widest stage, so the labels read as one column
        // rather than stepping in along the funnel's edge.
        // A name with no room beside its neighbours' is hidden, the larger stage keeping its own.
        labelLayout: { x: labelX, align: "left", hideOverlap: true },
        // Drawn again at once, never tweened: on a merge redraw (a resize, a re-Run) the
        // series tweens each name back to its place beside the stage while labelLayout has
        // moved it to the column, the two add up, and the names stood past the right edge.
        animationDurationUpdate: 0,
        itemStyle: { color, ...(theme.surface ? { borderColor: theme.surface, borderWidth: 1 } : {}) },
        // A stage tapers to the next one's width; one under PARTED px across halfway down, or
        // tall, has no line of the card's colour round it, which would cover it.
        data: stages.flatMap((st, i) => {
          const next = stepped ? st.value : (stages[i + 1]?.value ?? 0)
          const across = ((Math.max(st.value, 0) + Math.max(next, 0)) / 2 / (max || 1)) * share * width
          const bare = across < PARTED || tall < PARTED
          if (!stepped) return [bare ? { ...st, itemStyle: { borderWidth: 0 } } : st]
          // The join is as wide as the stage at its top, so the stage stands square; the
          // last one's has no height and only ends the stage at its own width.
          const join = {
            name: "",
            value: st.value,
            itemStyle: { height: i < stages.length - 1 ? gap : 0, color: "transparent", borderWidth: 0 },
            label: { show: false },
            labelLine: { show: false },
            tooltip: { show: false },
            emphasis: { disabled: true },
          }
          return [{ ...st, itemStyle: { height: tall, ...(bare ? { borderWidth: 0 } : {}) } }, join]
        }),
      },
    ],
  }
}

/** A point's x, y and size, read from its row; null when x or y is not a number. */
interface Point {
  x: number
  y: number
  size: number | null
  row: object
}

/** Each series' points, in the series' order: with a colour field a row is in the series it names, else in the one. */
function seriesPoints(settings: ScatterChartSettings, rows: readonly object[]): Point[][] {
  const { points, series } = settings
  const index = new Map(series.values.map((s, i) => [s.key, i]))
  const out = series.values.map((): Point[] => [])
  for (const row of rows) {
    const x = numberOf(field(row, points.x.field))
    const y = numberOf(field(row, points.y.field))
    if (x === null || y === null) continue
    const i = series.field === undefined ? 0 : index.get(String(field(row, series.field)))
    if (i === undefined || !out[i]) continue
    out[i].push({ x, y, size: points.size ? numberOf(field(row, points.size.field)) : null, row })
  }
  return out
}

/** The tooltip's line for the source rows at a point, when there is more than one. */
function rowsLine(rows: PointField | undefined, row: object): string {
  const n = rows ? numberOf(field(row, rows.field)) : null
  return rows && n !== null && n > 1 ? tooltipLine(NO_SWATCH, rows.label, formatNumber(n, rows.format)) : ""
}

/** A scatter's point, in pixels across; a bubble's widest, whose area its largest size fills. */
const POINT_SIZE = 8
const BUBBLE_MAX = 40
const BUBBLE_MIN = 4

/**
 * Points on two number axes, a colour per series (§ 14.8). A bubble's area is its size
 * against the largest (its width the square root), so twice the size reads as twice
 * the ink; a size of zero or less has no bubble (saved-chart.ts counts them).
 */
function scatterOption(settings: ScatterChartSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number): ChartOption {
  const { points } = settings
  const all = seriesPoints(settings, rows)
  const largest = Math.max(0, ...all.flat().map((p) => p.size ?? 0))
  const widthOf = (size: number | null) =>
    !points.size ? POINT_SIZE : size === null || size <= 0 || largest <= 0 ? 0 : Math.max(BUBBLE_MIN, BUBBLE_MAX * Math.sqrt(size / largest))
  // The drawn points, so a tooltip's index is the point it is over.
  const drawn = all.map((list) => list.filter((p) => widthOf(p.size) > 0))
  const colors = seriesColors(settings, theme)
  const muted = colorOf(theme.muted)
  const border = colorOf(theme.border)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const [top, bottom] = [points.y.label ? 28 : 10, points.x.label ? 28 : 0]
  // A bubble is drawn round its point, so the axes reach past the points by the largest
  // bubble's half and its line, as a share of their span either side for a plot `px`
  // long: the axes fitted the points, and a bubble at an end stood over the axis's line,
  // its numbers and its title.
  const half = points.size ? Math.max(0, ...drawn.flat().map((p) => widthOf(p.size))) / 2 + 1 : 0
  const gap = (px: number) => (half > 0 ? half / Math.max(half, px - 2 * half) : 0)
  const past = ([lo, hi]: [number, number], g: number): [number, number] => [lo - g * (hi - lo), hi + g * (hi - lo)]
  const yGap = gap(height - top - bottom - LABEL_MARGIN - AXIS_FONT)
  // Across, the y axis's ticks take the room left of the plot.
  const ys = past(spanOf(drawn.flat().map((p) => p.y)), yGap)
  const left = tickWidth(ys, points.y.format) + LABEL_MARGIN
  const plot = width - left - 16
  const xGap = gap(plot)
  const ticks = acrossTicks(past(spanOf(drawn.flat().map((p) => p.x)), xGap), points.x.format, width, left)
  const axis = (a: ScatterChartSettings["points"]["x"], across: boolean) => ({
    type: "value" as const,
    // A scatter's axes fit its points: zero is on them only when the points reach it.
    scale: true,
    boundaryGap: (across ? [xGap, xGap] : [yGap, yGap]) as [number, number],
    ...(across ? nameUnder(a.label, muted, 24, plot) : nameAbove(a.label, muted, plot)),
    ...(a.format === "integer" ? { minInterval: 1 } : {}),
    axisLine: { lineStyle: border },
    axisTick: { lineStyle: border },
    ...(across ? { splitNumber: ticks.splitNumber } : {}),
    axisLabel: { ...muted, fontSize: 11, ...(across ? ticks.axisLabel : { formatter: (v: number) => formatTick(v, a.format) }) },
    splitLine: { lineStyle: border },
  })
  const lone = settings.series.field === undefined
  const tooltip = (p: { seriesIndex?: number; dataIndex?: number }): string => {
    const si = p.seriesIndex ?? 0
    const point = drawn[si]?.[p.dataIndex ?? 0]
    if (!point) return ""
    const s = settings.series.values[si]
    return [
      lone ? "" : tooltipTitle(s?.label ?? ""),
      tooltipLine(swatchOf(colors[si]), points.x.label, formatNumber(point.x, points.x.format)),
      tooltipLine(NO_SWATCH, points.y.label, formatNumber(point.y, points.y.format)),
      points.size && point.size !== null ? tooltipLine(NO_SWATCH, points.size.label, formatNumber(point.size, points.size.format)) : "",
      rowsLine(points.rows, point.row),
    ].join("")
  }
  return {
    color: colors,
    textStyle: font,
    grid: { left: 0, right: 16, top, bottom },
    xAxis: axis(points.x, true),
    yAxis: axis(points.y, false),
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: settings.series.values.map((s, si) => ({
      type: "scatter" as const,
      name: s.label,
      itemStyle: { color: colors[si], opacity: 0.7, ...(points.size && theme.surface ? { borderColor: theme.surface, borderWidth: 1 } : {}) },
      emphasis: { focus: "series" as const },
      data: drawn[si].map((p) => ({ value: p.size === null ? [p.x, p.y] : [p.x, p.y, p.size], symbolSize: widthOf(p.size) })),
    })),
  }
}

// The second tier (dashboards-design.md § 14.8): parts of a whole, flows, days, a
// number against its target, and a box, a candle or a band per category. Each reads
// the rows saved-chart.ts gives it, and skips one that is not what it should be.

const byLabel = (a: string, b: string) => a.localeCompare(b, undefined, { numeric: true })

/** A treemap's or a sunburst's parts that have a name at each level and a value above zero. */
function partRows(rows: readonly object[]): PartRow[] {
  return rows.flatMap((row) => {
    const path = field(row, "path")
    const value = numberOf(field(row, "value"))
    return Array.isArray(path) && path.length > 0 && value !== null && value > 0 ? [{ path: path.map(String), value }] : []
  })
}

interface PartNode {
  name: string
  value: number
  /** Its name at each level down to it, for the tooltip. */
  path: string[]
  /** Its top-level group's index in the chart's colours: the last, Other, past the named groups. */
  group: number
  children?: PartNode[]
}

/**
 * The parts as a tree, each node's value the sum of its parts and each level's nodes
 * largest first (then by name), so the chart draws them in that order rather than in
 * the library's (§ 14.5).
 */
function partTree(settings: PartsChartSettings, rows: readonly object[]): PartNode[] {
  const groups = new Map(settings.groups.map((g, i) => [g.key, i]))
  const top: PartNode[] = []
  const nodes = new Map<string, PartNode>()
  for (const r of partRows(rows)) {
    let siblings = top
    for (let depth = 0; depth < r.path.length; depth++) {
      const path = r.path.slice(0, depth + 1)
      const key = JSON.stringify(path)
      let node = nodes.get(key)
      if (!node) {
        node = { name: r.path[depth], value: 0, path, group: groups.get(r.path[0]) ?? settings.groups.length }
        nodes.set(key, node)
        siblings.push(node)
      }
      node.value += r.value
      if (depth < r.path.length - 1) siblings = node.children ??= []
    }
  }
  const order = (list: PartNode[]) => {
    list.sort((a, b) => b.value - a.value || byLabel(a.name, b.name))
    for (const n of list) if (n.children) order(n.children)
  }
  order(top)
  return top
}

/** How many levels a tree goes down. */
const depthOf = (list: PartNode[]): number => list.reduce((d, n) => Math.max(d, 1 + (n.children ? depthOf(n.children) : 0)), 0)

/**
 * Where ECharts' squarify (echarts/lib/chart/treemap/treemapLayout.js) puts each of
 * `areas`, in order, in a rectangle `width` by `height` with no gap between them: each
 * one's width and height. Parts are laid in a row along the rectangle's shorter side
 * while that keeps them nearer the golden ratio, then the next row in what is left.
 */
function squarify(areas: number[], width: number, height: number): [number, number][] {
  const ratio = 0.5 * (1 + Math.sqrt(5))
  const box = { width, height }
  const out: [number, number][] = []
  let row: number[] = []
  let fixed = Math.min(width, height)
  let best = Infinity
  const sumOf = (list: number[]) => list.reduce((t, a) => t + a, 0)
  const worst = (list: number[]) => {
    const sum = sumOf(list)
    const drawn = list.filter((a) => a > 0)
    const f = fixed * fixed * ratio
    return sum ? Math.max((f * Math.max(...drawn)) / (sum * sum), (sum * sum) / (f * Math.min(...drawn))) : Infinity
  }
  const place = (list: number[], flush: boolean) => {
    // A row runs across the box's width, or down its height, and is as deep as its parts need.
    const along = fixed === box.width
    const side = along ? box.height : box.width
    const need = fixed ? sumOf(list) / fixed : 0
    const deep = flush || need > side ? side : need
    let left = along ? box.width : box.height
    list.forEach((a, i) => {
      const step = deep ? a / deep : 0
      const long = i === list.length - 1 || left < step ? left : step
      out.push(along ? [Math.max(0, long), deep] : [deep, Math.max(0, long)])
      left -= long
    })
    if (along) box.height -= deep
    else box.width -= deep
  }
  for (let i = 0; i < areas.length; ) {
    row.push(areas[i])
    const score = worst(row)
    if (score <= best) {
      i++
      best = score
    } else {
      row.pop()
      place(row, false)
      fixed = Math.min(box.width, box.height)
      row = []
      best = Infinity
    }
  }
  if (row.length) place(row, true)
  return out
}

/**
 * A note of what a chart cannot draw at this size, or null when it draws it all. A
 * treemap's or a sunburst's parts under a pixel (a rectangle under a pixel of area, a
 * slice under a pixel along the middle of its ring) are laid out, too small to see or
 * point at; a group's value still counts them. A histogram with more bins than bars fit
 * adds neighbours up. EChart.tsx and dashboard-pdf.ts write it under the chart.
 */
export function sizeNote(
  settings: ChartSettings,
  rows: readonly object[],
  theme: ChartTheme,
  width = DEFAULT_WIDTH,
  height = CHART_HEIGHT,
): string | null {
  if (settings.type === "histogram") {
    const { per } = binsFitted(settings, rows, width)
    if (per <= 1) return null
    return `${settings.x.categories.length.toLocaleString("en-US")} bins are more than fit at this width: each bar adds up ${per} of them.`
  }
  if (settings.type !== "treemap" && settings.type !== "sunburst") return null
  const n = partsLayout(settings, rows, theme, width, height).unseen
  if (n === 0) return null
  return `${n.toLocaleString("en-US")} ${n === 1 ? "part is" : "parts are"} under a pixel at this size, too small to see.`
}

/**
 * Rectangles or rings, each part in its top-level group's colour, with the card's
 * colour between them. Nothing opens on a click: the chart is the whole of its data.
 */
function partsOption(settings: PartsChartSettings, rows: readonly object[], theme: ChartTheme, width: number, height: number): ChartOption {
  return partsLayout(settings, rows, theme, width, height).option
}

function partsLayout(
  settings: PartsChartSettings,
  rows: readonly object[],
  theme: ChartTheme,
  width: number,
  height: number,
): { option: ChartOption; unseen: number } {
  const tree = partTree(settings, rows)
  const colors = seriesColors(settings, theme)
  const total = tree.reduce((t, n) => t + n.value, 0)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const label = { ...colorOf(theme.surface), ...font, fontSize: PART_FONT, overflow: "truncate" as const, ellipsis: "…" }
  const border = theme.surface ? { borderColor: theme.surface } : {}
  const sunburst = settings.type === "sunburst"
  const rings = depthOf(tree)
  // How wide a part is drawn, to choose its line and its name by: a slice along the
  // middle of its ring (90% of the picture's half), a rectangle as the side of a square
  // of its area, where squarify puts it in the room its group leaves (below). A line of
  // the card's colour parts one part from the next; a part under PARTED px would be all
  // line, and a fixed gap between parts left one under 2 px out of a treemap. A group
  // names itself in a strip along its top only where the strip has room: on a smaller
  // group, ECharts draws it over its neighbours.
  const across = (n: PartNode, depth: number) => 2 * Math.PI * (total > 0 ? n.value / total : 0) * 0.45 * Math.min(width, height) * ((depth + 0.5) / rings)
  // A sunburst names a part along its radius, in the middle of its ring (90% of the
  // picture's half, a ring a level), cut short so the name stays inside the ring and,
  // nearer the middle where the slice narrows, no nearer than where the slice is a line
  // of text across: a name past either runs into the next ring's or a neighbour's.
  const ring = (0.45 * Math.min(width, height)) / Math.max(1, rings)
  const nameRoom = (n: PartNode, depth: number) => {
    const angle = total > 0 ? (2 * Math.PI * n.value) / total : 0
    const inner = Math.max(depth * ring + SUNBURST_PAD, angle >= Math.PI ? 0 : (1.2 * SUNBURST_FONT) / (2 * Math.sin(angle / 2)))
    return Math.floor(Math.min(ring - 2 * SUNBURST_PAD, 2 * ((depth + 0.5) * ring - inner)))
  }
  // A treemap writes a name across its part inside the part's line, cut short to the
  // room left, less the px zrender keeps back, and keeps the ellipsis only where two
  // letters and it fit (TreemapView's truncateMinChar): in a narrower part, a name was
  // cut to a letter or two with no sign it was cut ("Ph", "S"). A name is written where
  // it fits whole, or two letters and the ellipsis do (2.2 em at most), at the page's
  // widest: a part's 5 px in from each side, a group's strip from edge to edge.
  const named = (n: PartNode, room: number) => room >= Math.min(nameWidth(n.name, PART_FONT), 2.2 * PART_FONT)
  // A treemap draws a group's parts inside its line and under its strip (squarify takes
  // both off the group's rectangle). A group kept its line at any size, and at the
  // 3,500-part cap the lines of groups a few px across left most of the picture the
  // card's colour and most parts no area at all. A part too small to see is counted.
  let unseen = 0
  const data = (list: PartNode[], depth = 0, box: [number, number] = [width, height]): object[] => {
    const sum = list.reduce((t, n) => t + Math.max(n.value, 0), 0)
    const rects = sunburst ? [] : squarify(list.map((n) => (sum > 0 ? (Math.max(n.value, 0) / sum) * box[0] * box[1] : 0)), ...box)
    return list.map((n, i) => {
      const [w, h] = rects[i] ?? [0, 0]
      const size = sunburst ? across(n, depth) : Math.sqrt(w * h)
      const group = !sunburst && n.children !== undefined
      const lined = size >= (group ? LINED : PARTED)
      const line = lined ? (group ? 2 : 1) : 0
      const strip = group && lined && size >= STRIP_ROOM && named(n, w - 2 * line - 1)
      const name = sunburst ? nameRoom(n, depth) : 0
      if (!n.children && (sunburst ? size < 1 : w * h < 1)) unseen++
      return {
        name: n.name,
        value: n.value,
        path: n.path,
        itemStyle: { color: colors[n.group], ...(lined ? {} : { borderWidth: 0 }) },
        ...(group && !strip ? { upperLabel: { show: false } } : {}),
        // Room for the whole name, or for a letter and the ellipsis (an em each at most),
        // and the px zrender keeps back; or no name: under a letter's room, it wrote a lone "…".
        ...(sunburst ? { label: name >= Math.min(nameWidth(n.name, SUNBURST_FONT), 2 * SUNBURST_FONT) + 1 ? { width: name } : { show: false } } : {}),
        ...(!sunburst && !group && !named(n, w - 2 * line - 11) ? { label: { show: false } } : {}),
        ...(n.children ? { children: data(n.children, depth + 1, [Math.max(0, w - 2 * line), Math.max(0, h - line - Math.max(line, strip ? 16 : 0))]) } : {}),
      }
    })
  }
  // When the source has more parts than were drawn, a share is of the drawn total and
  // a group's value the sum of its drawn parts, and the tooltip says so; a part's own
  // value is the source's. The words follow the measure's label in brackets, as a label
  // ending "of amount" would read "of amount of drawn parts".
  const drawn = settings.partial
  const tooltip = (p: { color?: unknown; data?: { path?: string[]; value?: number; children?: unknown[] } }): string => {
    // The library's own node for the whole chart has no path: it shows through the
    // gaps between parts, and has no tooltip.
    if (!p.data?.path) return ""
    const v = p.data.value ?? 0
    const value = settings.value.label + (drawn && p.data.children?.length ? " (drawn parts only)" : "")
    return (
      tooltipTitle(p.data.path.join(" › ")) +
      tooltipLine(swatchOf(typeof p.color === "string" ? p.color : ""), value, formatNumber(v, settings.value.format)) +
      (total > 0 ? tooltipLine(NO_SWATCH, drawn ? "Share of drawn total" : "Share", formatNumber(v / total, "percent")) : "")
    )
  }
  const common = {
    name: settings.value.label,
    nodeClick: false as const,
    data: data(tree),
  }
  const option: ChartOption = {
    textStyle: font,
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      settings.type === "treemap"
        ? {
            ...common,
            type: "treemap",
            left: 0,
            top: 0,
            right: 0,
            bottom: 0,
            roam: false,
            breadcrumb: { show: false },
            // The tree is in order already: largest first, a tie by name.
            sort: false,
            label: { show: true, ...label },
            // levels[0] is the whole chart; then one per level, outermost first. No gap
            // between parts: each part's own line parts it from the next. A level with
            // parts under it names itself in a strip along its top, in the card's
            // colour, so its name is in the text colour.
            levels: [
              { itemStyle: { borderWidth: 0, gapWidth: 0 }, upperLabel: { show: false } },
              ...settings.levels.map((_, depth) =>
                depth < settings.levels.length - 1
                  ? {
                      itemStyle: { ...border, borderWidth: 2, gapWidth: 0 },
                      upperLabel: { show: true, height: 16, ...label, ...colorOf(theme.text) },
                    }
                  : { itemStyle: { ...border, borderWidth: 1 } },
              ),
            ],
          }
        : {
            ...common,
            type: "sunburst",
            radius: [0, "90%"],
            // Each ring in the tree's order, not the library's sort by size.
            sort: (a: { dataIndex: number }, b: { dataIndex: number }) => a.dataIndex - b.dataIndex,
            itemStyle: { ...border, borderWidth: 2 },
            // Each part's name is cut short to the room it has (data, above).
            label: { ...label, fontSize: SUNBURST_FONT, rotate: "radial", minAngle: 10, ellipsis: "…" },
            emphasis: { focus: "ancestor" },
          },
    ],
  }
  return { option, unseen }
}

/** A sankey's links that join two names with a value above zero. */
function flowRows(rows: readonly object[]): FlowRow[] {
  return rows.flatMap((row) => {
    const source = field(row, "source")
    const target = field(row, "target")
    const value = numberOf(field(row, "value"))
    return typeof source === "string" && typeof target === "string" && source !== target && value !== null && value > 0
      ? [{ source, target, value }]
      : []
  })
}

/**
 * How many nodes the fullest of a sankey's columns holds, each node in the column ECharts
 * puts it in (sankeyLayout.js): one right of the last of the nodes that flow into it, and
 * a node no link leaves in the last column (nodeAlign "justify").
 */
function fullestColumn(keys: readonly string[], flows: readonly FlowRow[]): number {
  const known = new Set(keys)
  const links = flows.filter((f) => known.has(f.source) && known.has(f.target))
  const into = new Map([...known].map((k) => [k, 0]))
  const out = new Map([...known].map((k): [string, string[]] => [k, []]))
  for (const f of links) {
    into.set(f.target, (into.get(f.target) ?? 0) + 1)
    out.get(f.source)?.push(f.target)
  }
  const column = new Map<string, number>()
  let step = [...known].filter((k) => into.get(k) === 0)
  let depth = 0
  for (; step.length > 0; depth++) {
    const next: string[] = []
    for (const k of step) {
      column.set(k, depth)
      for (const t of out.get(k) ?? []) {
        const left = (into.get(t) ?? 0) - 1
        into.set(t, left)
        if (left === 0) next.push(t)
      }
    }
    step = next
  }
  const counts = new Map<number, number>()
  for (const [k, c] of column) {
    const at = out.get(k)?.length ? c : depth - 1
    counts.set(at, (counts.get(at) ?? 0) + 1)
  }
  return Math.max(0, ...counts.values())
}

/**
 * Nodes in the order given, links as wide as their value in the colour of the node they
 * leave. No pass moves the nodes to untangle the links (§ 14.5), so the same rows are
 * the same picture. A node no link leaves names itself on its left, inside the chart.
 * The gaps between a column's nodes take at most half its height, so a column of many
 * names keeps every node; a name with no room beside its neighbours is hidden, the
 * larger node keeping its own.
 */
function sankeyOption(settings: SankeyChartSettings, rows: readonly object[], theme: ChartTheme, height: number): ChartOption {
  const flows = flowRows(rows)
  // ECharts puts nodeGap between every two nodes of a column whatever their count: past
  // 59 names in 360 px the gaps alone overfill it, and every node is drawn with a
  // height below zero, out of the picture.
  const fullest = fullestColumn(settings.nodes.map((n) => n.key), flows)
  const plot = height - 2 * HALF_LINE
  const nodeGap = fullest > 1 ? Math.min(6, plot / 2 / (fullest - 1)) : 6
  const colors = seriesColors(settings, theme)
  const sources = new Set(flows.map((f) => f.source))
  const font = theme.font ? { fontFamily: theme.font } : {}
  const tooltip = (p: { dataType?: string; name?: string; value?: unknown; color?: unknown; data?: { source?: string; target?: string } }) => {
    const value = typeof p.value === "number" ? formatNumber(p.value, settings.value.format) : ""
    const swatch = typeof p.color === "string" ? swatchOf(p.color) : NO_SWATCH
    const title = p.dataType === "edge" ? `${p.data?.source ?? ""} → ${p.data?.target ?? ""}` : (p.name ?? "")
    return tooltipTitle(title) + tooltipLine(swatch, settings.value.label, value)
  }
  return {
    textStyle: font,
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "sankey",
        name: settings.value.label,
        left: 0,
        top: HALF_LINE,
        right: 0,
        bottom: HALF_LINE,
        layoutIterations: 0,
        draggable: false,
        nodeWidth: 10,
        nodeGap,
        label: { ...colorOf(theme.text), ...font, fontSize: 11 },
        // ECharts ranks the names by the area of their node, so the larger one keeps its name.
        labelLayout: { hideOverlap: true },
        lineStyle: { color: "source", opacity: 0.35, curveness: 0.5 },
        emphasis: { focus: "adjacency" },
        data: settings.nodes.map((n, i) => ({
          name: n.key,
          itemStyle: { color: colors[i], borderWidth: 0 },
          ...(sources.has(n.key) ? {} : { label: { position: "left" as const } }),
        })),
        links: flows.map((f) => ({ source: f.source, target: f.target, value: f.value })),
      },
    ],
  }
}

const ISO_DAY = /^\d{4}-\d{2}-\d{2}$/

/** A calendar's days that have a number. */
function dayRows(rows: readonly object[]): DayRow[] {
  return rows.flatMap((row) => {
    const day = field(row, "day")
    const value = numberOf(field(row, "value"))
    return typeof day === "string" && ISO_DAY.test(day) && value !== null ? [{ day, value }] : []
  })
}

// Index 0 is Sunday, as ECharts reads it; the week drawn starts on Monday.
const WEEKDAYS = ["S", "M", "T", "W", "T", "F", "S"]
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]

const DAY_MS = 86_400_000
/** A calendar's room left of its weeks, for the days' letters, and right of them. */
const CALENDAR_LEFT = 20
const CALENDAR_RIGHT = 2
const MONTH_FONT = 10
/** The least room between two months' names, measured at 0.6 em: about a space at 10 px. */
const MONTH_GAP = 3

/**
 * What each month of a calendar `width` px across says over its weeks, by "YYYY-MM": its
 * name where it has room, "" where it has none. ECharts centres a month's name over the
 * weeks from its first day to the next month's first Monday (CalendarView.js), whatever
 * room the names beside it need. The first month's name and January's say their year and
 * are placed first; one with no room passes its year to the next month. The other months
 * take the room left.
 */
function monthNames(range: readonly [string, string], width: number): Map<string, string> {
  const utc = (iso: string) => Date.UTC(Number(iso.slice(0, 4)), Number(iso.slice(5, 7)) - 1, Number(iso.slice(8, 10)))
  const [start, end] = [utc(range[0]), utc(range[1])]
  // Days before the first in its week, Monday first; then the weeks as ECharts counts them (Calendar.js).
  const lead = (new Date(start).getUTCDay() + 6) % 7
  const weeks = Math.floor((Math.round((end - start) / DAY_MS) + 1 + lead + 6) / 7)
  const cell = (width - CALENDAR_LEFT - CALENDAR_RIGHT) / Math.max(1, weeks)
  const x = (t: number) => CALENDAR_LEFT + Math.floor((Math.round((t - start) / DAY_MS) + lead) / 7) * cell
  const monday = (t: number) => t + ((8 - new Date(t).getUTCDay()) % 7) * DAY_MS
  const months: { key: string; name: string; year: string; mid: number; dated: boolean }[] = []
  for (let first = start; first <= end; ) {
    const d = new Date(first)
    const next = Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1)
    months.push({
      key: `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, "0")}`,
      name: MONTHS[d.getUTCMonth()],
      year: String(d.getUTCFullYear()),
      mid: (x(first) + x(monday(Math.min(next, end + DAY_MS)))) / 2,
      dated: first === start || d.getUTCMonth() === 0,
    })
    first = next
  }
  const names = new Map(months.map((m) => [m.key, ""]))
  const placed: [number, number][] = []
  const place = (m: (typeof months)[number], text: string): boolean => {
    const half = textWidth(text, MONTH_FONT) / 2
    const [l, r] = [m.mid - half, m.mid + half]
    if (l < 0 || r > width || placed.some(([a, b]) => l < b + MONTH_GAP && r > a - MONTH_GAP)) return false
    placed.push([l, r])
    names.set(m.key, text)
    return true
  }
  let owed = false
  for (const m of months) if (m.dated || owed) owed = !place(m, `${m.name} ${m.year}`)
  for (const m of months) if (names.get(m.key) === "") place(m, m.name)
  return names
}

/**
 * A cell per day from the first to the last, a week a column starting on Monday (ISO),
 * each day with a number in one of five steps of the hue, as a heatmap's; a day with
 * none in the border colour. January and the first month say their year; a month's name
 * with no room beside the others' is left out (monthNames).
 */
function calendarOption(settings: CalendarChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const days = dayRows(rows)
  const steps = stepsOf(days.map((d) => d.value))
  const [hue] = seriesColors(settings, theme)
  const muted = colorOf(theme.muted)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const names = monthNames(settings.range, width)
  const tooltip = (p: { data?: [string, number, number] }): string => {
    const [day, value] = p.data ?? ["", 0]
    return tooltipTitle(day) + tooltipLine(swatchOf(hue), settings.value.label, formatNumber(value, settings.value.format))
  }
  return {
    textStyle: font,
    calendar: {
      range: settings.range,
      left: CALENDAR_LEFT,
      top: 18,
      right: CALENDAR_RIGHT,
      bottom: 2,
      cellSize: ["auto", "auto"],
      splitLine: { show: false },
      itemStyle: { ...colorOf(theme.border), ...(theme.surface ? { borderColor: theme.surface } : {}), borderWidth: 2 },
      dayLabel: { firstDay: 1, nameMap: WEEKDAYS, ...muted, fontSize: 10 },
      monthLabel: {
        nameMap: MONTHS,
        ...muted,
        fontSize: MONTH_FONT,
        // ECharts' 10 px between a name and the weeks put its top 2 px above the picture.
        margin: 4,
        formatter: (m: { yyyy: string; MM: string }) => names.get(`${m.yyyy}-${m.MM}`) ?? "",
      },
      yearLabel: { show: false },
    },
    // Drawn only through a hidden visualMap, as a heatmap is: each day carries its step.
    visualMap: {
      type: "piecewise",
      show: false,
      dimension: 2,
      seriesIndex: 0,
      pieces: STEP_ALPHA.map((a, i) => ({ value: i, color: withAlpha(hue, a) })),
    },
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "heatmap",
        name: settings.value.label,
        coordinateSystem: "calendar",
        data: days.map((d) => [d.day, d.value, stepOf(steps, d.value)]),
      },
    ],
  }
}

/** A bullet's target in its own format, else in the number's. */
const targetFormat = (settings: BulletChartSettings): NumberFormat => settings.target.format ?? settings.value.format

/** A bullet chart's one row: null without a number. */
function bulletRow(rows: readonly object[]): BulletRow | null {
  const row = rows[0]
  const value = row ? numberOf(field(row, "value")) : null
  return value === null ? null : { value, target: numberOf(field(row!, "target")) }
}

/** One bar as long as the number, and a mark across it at the target, in the text colour. */
function bulletOption(settings: BulletChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const row = bulletRow(rows)
  const [color] = seriesColors(settings, theme)
  const mark = theme.text || OTHER_FALLBACK
  const format = settings.value.format
  const muted = colorOf(theme.muted)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const tooltip = (): string => {
    if (!row) return ""
    const lines = [tooltipLine(swatchOf(color), settings.value.label, formatNumber(row.value, format))]
    if (row.target !== null) {
      lines.push(tooltipLine(swatchOf(mark), settings.target.label, formatNumber(row.target, targetFormat(settings))))
      // A share of a target at or below zero reads backwards: a number past it would read as a share below zero.
      if (row.target > 0) lines.push(tooltipLine(NO_SWATCH, "Of the target", formatNumber(row.value / row.target, "percent")))
    }
    return lines.join("")
  }
  // A bar's axis reaches zero; nothing is left of the plot.
  const ticks = acrossTicks(spanOf([0, ...(row ? [row.value, row.target ?? 0] : [])]), format, width, 0)
  return {
    textStyle: font,
    grid: { left: 0, right: 16, top: 10, bottom: 0 },
    xAxis: {
      type: "value",
      ...(format === "integer" ? { minInterval: 1 } : {}),
      splitNumber: ticks.splitNumber,
      axisLabel: { ...muted, fontSize: 11, ...ticks.axisLabel },
      splitLine: { lineStyle: colorOf(theme.border) },
    },
    yAxis: { type: "category", data: [settings.value.label], show: false },
    tooltip: { trigger: "axis", axisPointer: { type: "none" }, ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "bar",
        name: settings.value.label,
        barWidth: 20,
        itemStyle: { color, borderRadius: row && row.value < 0 ? [3, 0, 0, 3] : ROUNDED_RIGHT },
        data: row ? [row.value] : [],
      },
      ...(row && row.target !== null
        ? [
            {
              type: "scatter" as const,
              name: settings.target.label,
              symbol: "rect",
              symbolSize: [3, 36],
              itemStyle: { color: mark },
              z: 3,
              data: [[row.target, 0]],
            },
          ]
        : []),
    ],
  }
}

/** The least and greatest of a chart's marks; zero and zero when there is none. */
const spanOf = (values: number[]): [number, number] => (values.length ? [Math.min(...values), Math.max(...values)] : [0, 0])

/**
 * The category axis and a value axis that fits its marks (zero only when they reach
 * it), each titled, `width` across; `reach` is the least and greatest of the marks.
 */
function statAxes(x: { label: string; categories: string[] }, title: string, format: NumberFormat, theme: ChartTheme, reach: [number, number], width: number) {
  const muted = colorOf(theme.muted)
  const border = colorOf(theme.border)
  const room = tickWidth(reach, format) + LABEL_MARGIN
  const labels = bottomLabels(x.categories, width, room, 8, dated(x.categories))
  return {
    grid: { ...labels.grid, top: title ? 28 : 10, bottom: 0 },
    xAxis: {
      type: "category" as const,
      data: x.categories,
      ...nameUnder(x.label, muted, LABEL_MARGIN + labels.depth + 4, labels.plot),
      axisTick: { alignWithLabel: true, lineStyle: border },
      axisLine: { lineStyle: border },
      axisLabel: { ...muted, fontSize: 11, ...labels.axisLabel },
    },
    yAxis: {
      type: "value" as const,
      scale: true,
      ...nameAbove(title, muted, labels.plot),
      ...(format === "integer" ? { minInterval: 1 } : {}),
      axisLabel: { ...muted, fontSize: 11, formatter: (v: number) => formatTick(v, format) },
      splitLine: { lineStyle: border },
    },
  }
}

const BOX_KEYS = ["min", "q1", "median", "q3", "max"] as const

/** A box plot's statistics, greatest first, as the tooltip lists them. */
const BOX_LINES = [
  ["Maximum", "max"],
  ["Upper quartile", "q3"],
  ["Median", "median"],
  ["Lower quartile", "q1"],
  ["Minimum", "min"],
] as const

/** A box plot's categories that have all five numbers. */
function boxRows(rows: readonly object[]): BoxRow[] {
  return rows.flatMap((row) => {
    const x = field(row, "x")
    const n = BOX_KEYS.map((k) => numberOf(field(row, k)))
    if (typeof x !== "string" || n.some((v) => v === null)) return []
    const [min, q1, median, q3, max] = n as number[]
    return [{ x, min, q1, median, q3, max }]
  })
}

/** A box from the lower to the upper quartile, a line across at the median, whiskers to the least and the greatest. */
function boxplotOption(settings: BoxplotChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const boxes = new Map(boxRows(rows).map((b) => [b.x, b]))
  const reach = spanOf([...boxes.values()].flatMap((b) => [b.min, b.max]))
  const [color] = seriesColors(settings, theme)
  const format = settings.y.format
  const font = theme.font ? { fontFamily: theme.font } : {}
  const tooltip = (p: { dataIndex?: number }): string => {
    const x = settings.x.categories[p.dataIndex ?? 0] ?? ""
    const b = boxes.get(x)
    if (!b) return tooltipTitle(x)
    return [tooltipTitle(x), ...BOX_LINES.map(([label, key]) => tooltipLine(key === "median" ? swatchOf(color) : NO_SWATCH, label, formatNumber(b[key], format)))].join("")
  }
  return {
    textStyle: font,
    ...statAxes(settings.x, settings.y.label, format, theme, reach, width),
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "boxplot",
        name: settings.y.label,
        boxWidth: [7, 40],
        itemStyle: { color: withAlpha(color, 0.25), borderColor: color, borderWidth: 1.5 },
        data: settings.x.categories.map((c) => {
          const b = boxes.get(c)
          return b ? BOX_KEYS.map((k) => b[k]) : []
        }),
      },
    ],
  }
}

const PRICE_KEYS = ["open", "high", "low", "close"] as const
const PRICE_LINES = [
  ["Open", "open"],
  ["High", "high"],
  ["Low", "low"],
  ["Close", "close"],
] as const

/** A candlestick's periods that have all four prices. */
function priceRows(rows: readonly object[]): PriceRow[] {
  return rows.flatMap((row) => {
    const x = field(row, "x")
    const n = PRICE_KEYS.map((k) => numberOf(field(row, k)))
    if (typeof x !== "string" || n.some((v) => v === null)) return []
    const [open, high, low, close] = n as number[]
    return [{ x, open, high, low, close }]
  })
}

/** A candle per period: the rise colour when it closed above its open, the fall colour when below. */
function candlestickOption(settings: CandlestickChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const prices = new Map(priceRows(rows).map((p) => [p.x, p]))
  const reach = spanOf([...prices.values()].flatMap((p) => [p.low, p.high]))
  const [rise, fall] = seriesColors(settings, theme)
  const format = settings.y.format
  const font = theme.font ? { fontFamily: theme.font } : {}
  const tooltip = (points: AxisPoint | AxisPoint[]): string => {
    const list = Array.isArray(points) ? points : [points]
    const x = settings.x.categories[list[0]?.dataIndex ?? 0] ?? ""
    const p = prices.get(x)
    if (!p) return tooltipTitle(x) + noValue(settings.y.label)
    const swatch = swatchOf(p.close >= p.open ? rise : fall)
    return [tooltipTitle(x), ...PRICE_LINES.map(([label, key]) => tooltipLine(key === "close" ? swatch : NO_SWATCH, label, formatNumber(p[key], format)))].join("")
  }
  return {
    textStyle: font,
    ...statAxes(settings.x, settings.y.label, format, theme, reach, width),
    tooltip: { trigger: "axis", axisPointer: { type: "shadow" }, ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "candlestick",
        name: settings.y.label,
        barMaxWidth: 16,
        itemStyle: { color: rise, color0: fall, borderColor: rise, borderColor0: fall },
        // ECharts reads a candle as open, close, low, high.
        data: settings.x.categories.map((c) => {
          const p = prices.get(c)
          return p ? [p.open, p.close, p.low, p.high] : []
        }),
      },
    ],
  }
}

/** A range band's categories, each with its two measures or null. */
function bandRows(rows: readonly object[]): BandRow[] {
  return rows.flatMap((row) => {
    const x = field(row, "x")
    return typeof x === "string" ? [{ x, low: numberOf(field(row, "low")), high: numberOf(field(row, "high")) }] : []
  })
}

/**
 * The band between two measures: an unseen line at the lesser of the two and, stacked
 * on it, the gap up to the greater, filled. "all" stacks onto a base below zero too.
 * The tooltip says each measure as it is.
 */
function rangeOption(settings: RangeChartSettings, rows: readonly object[], theme: ChartTheme, width: number): ChartOption {
  const byX = new Map(bandRows(rows).map((b) => [b.x, b]))
  const bands = settings.x.categories.map((c) => {
    const b = byX.get(c)
    return b && b.low !== null && b.high !== null ? { low: b.low, high: b.high } : null
  })
  const [color] = seriesColors(settings, theme)
  const font = theme.font ? { fontFamily: theme.font } : {}
  const say = (swatch: string, measure: { label: string; format: NumberFormat }, v: number | null | undefined) =>
    v === null || v === undefined ? noValue(measure.label, swatch) : tooltipLine(swatch, measure.label, formatNumber(v, measure.format))
  const tooltip = (points: AxisPoint | AxisPoint[]): string => {
    const list = Array.isArray(points) ? points : [points]
    const x = settings.x.categories[list[0]?.dataIndex ?? 0] ?? ""
    // A band with a measure missing is not drawn; the tooltip still says the other.
    const b = byX.get(x)
    return tooltipTitle(x) + say(swatchOf(color), settings.low, b?.low) + say(NO_SWATCH, settings.high, b?.high)
  }
  // A band is at most the width over the categories: the plot is narrower.
  const band = width / Math.max(1, settings.x.categories.length)
  const edge = {
    stack: "band",
    stackStrategy: "all" as const,
    connectNulls: false,
    showSymbol: true,
    symbol: "circle",
    symbolSize: 5,
    lineStyle: { width: 1.5, color },
    itemStyle: { color },
  }
  return {
    textStyle: font,
    ...statAxes(
      settings.x,
      `${settings.low.label} · ${settings.high.label}`,
      settings.low.format,
      theme,
      spanOf(bands.flatMap((b) => (b ? [b.low, b.high] : []))),
      width,
    ),
    tooltip: { trigger: "axis", axisPointer: { type: "line" }, ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      { type: "line", name: settings.low.label, ...edge, ...pointsOf(bands.map((b) => (b ? Math.min(b.low, b.high) : null)), 5, band) },
      {
        type: "line",
        name: settings.high.label,
        ...edge,
        areaStyle: { color, opacity: 0.25 },
        ...pointsOf(bands.map((b) => (b ? Math.abs(b.high - b.low) : null)), 5, band),
      },
    ],
  }
}

/** A map's rows: a shape and its number. */
function regionRows(rows: readonly object[]): RegionRow[] {
  return rows.flatMap((row) => {
    const region = field(row, "region")
    const value = numberOf(field(row, "value"))
    return typeof region === "string" && value !== null ? [{ region, value }] : []
  })
}

/**
 * The world's countries or the US states, each shape a row names in one of five steps of
 * the hue laid over the border colour, and a shape no row names in that colour. Each shape's
 * colour is its own, not a visualMap's: a map series reads one value a shape. Alaska and
 * Hawaii are drawn under the lower 48 (echart-maps.ts). No zoom, pan or labels.
 */
function mapOption(settings: MapChartSettings, rows: readonly object[], theme: ChartTheme): ChartOption {
  const regions = regionRows(rows)
  const steps = stepsOf(regions.map((r) => r.value))
  const [hue] = seriesColors(settings, theme)
  const values = new Map(regions.map((r) => [r.region, r.value]))
  const tooltip = (p: { name?: string }): string => {
    const region = p.name ?? ""
    const value = values.get(region)
    return (
      tooltipTitle(regionName(settings.map, region)) +
      (value === undefined
        ? noValue(settings.value.label)
        : tooltipLine(swatchOf(filledStep(hue, stepOf(steps, value), theme)), settings.value.label, formatNumber(value, settings.value.format)))
    )
  }
  const empty = theme.border ? { areaColor: theme.border } : {}
  return {
    textStyle: theme.font ? { fontFamily: theme.font } : {},
    tooltip: { trigger: "item", ...tooltipStyle(theme), formatter: tooltip as TooltipComponentOption["formatter"] },
    series: [
      {
        type: "map",
        map: ECHARTS_MAPS[settings.map],
        name: settings.value.label,
        roam: false,
        selectedMode: false,
        left: 0,
        right: 0,
        top: 0,
        bottom: 0,
        label: { show: false },
        // The card's colour between the shapes, so neighbours of one step stay apart.
        itemStyle: { ...empty, ...(theme.surface ? { borderColor: theme.surface } : {}), borderWidth: 0.5 },
        // ECharts' own hover fill is yellow and names the shape: a shape keeps its fill and gains an outline.
        emphasis: { label: { show: false }, itemStyle: { ...empty, ...(theme.text ? { borderColor: theme.text } : {}), borderWidth: 1 } },
        data: regions.map((r) => {
          const fill = filledStep(hue, stepOf(steps, r.value), theme)
          return { name: r.region, value: r.value, itemStyle: { areaColor: fill }, emphasis: { itemStyle: { areaColor: fill } } }
        }),
      },
    ],
  }
}

export interface ChartTable {
  head: string[]
  body: string[][]
}

/** The chart's numbers as a table, for a screen reader: SVG marks say nothing on their own. */
export function chartTable(settings: ChartSettings, rows: readonly object[]): ChartTable {
  switch (settings.type) {
    case "heatmap":
      return {
        head: [settings.x.label, settings.y.label, settings.value.label],
        body: heatmapCells(settings, rows).map((c) => [
          settings.x.categories[c.xi],
          settings.y.categories[c.yi],
          formatNumber(c.value, settings.value.format),
        ]),
      }
    case "pie":
    case "donut": {
      const values = pieValues(settings, rows)
      const total = values.reduce<number>((t, v) => t + (v !== null && v > 0 ? v : 0), 0)
      return {
        head: [settings.slices.label, settings.value.label, "Share"],
        body: settings.slices.values.map((s, i) => {
          const v = values[i]
          return [
            s.label,
            v === null ? "" : formatNumber(v, settings.value.format),
            v === null || total <= 0 ? "" : formatNumber(Math.max(v, 0) / total, "percent"),
          ]
        }),
      }
    }
    case "waterfall": {
      // A table needs no theme: the colours are not in it.
      const { categories, steps } = waterfallSteps(settings, rows, { colors: {} } as ChartTheme)
      const label = settings.series.values[0]?.label ?? ""
      return {
        head: [settings.x.label, label, "Running total"],
        body: categories.map((c, ci) => {
          const step = steps[ci]
          return [c, step ? formatNumber(step.change, settings.y.format) : "", step ? formatNumber(step.end, settings.y.format) : ""]
        }),
      }
    }
    case "scatter":
    case "bubble": {
      const { points, series } = settings
      const coloured = series.field !== undefined
      return {
        head: [
          points.x.label,
          points.y.label,
          ...(points.size ? [points.size.label] : []),
          ...(coloured ? [series.label ?? ""] : []),
          ...(points.rows ? [points.rows.label] : []),
        ],
        body: seriesPoints(settings, rows).flatMap((list, si) =>
          list.map((p) => {
            const n = points.rows ? numberOf(field(p.row, points.rows.field)) : null
            return [
              formatNumber(p.x, points.x.format),
              formatNumber(p.y, points.y.format),
              ...(points.size ? [p.size === null ? "" : formatNumber(p.size, points.size.format)] : []),
              ...(coloured ? [series.values[si].label] : []),
              ...(points.rows ? [n === null ? "" : formatNumber(n, points.rows.format)] : []),
            ]
          }),
        ),
      }
    }
    case "treemap":
    case "sunburst":
      return {
        head: [...settings.levels, settings.value.label],
        body: partRows(rows).map((r) => [...settings.levels.map((_, i) => r.path[i] ?? ""), formatNumber(r.value, settings.value.format)]),
      }
    case "sankey":
      return {
        head: [settings.source.label, settings.target.label, settings.value.label],
        body: flowRows(rows).map((f) => [f.source, f.target, formatNumber(f.value, settings.value.format)]),
      }
    case "calendar":
      return {
        head: ["Day", settings.value.label],
        body: dayRows(rows).map((d) => [d.day, formatNumber(d.value, settings.value.format)]),
      }
    case "map":
      return {
        head: [settings.region.label, settings.value.label],
        body: regionRows(rows).map((r) => [regionName(settings.map, r.region), formatNumber(r.value, settings.value.format)]),
      }
    case "bullet": {
      const row = bulletRow(rows)
      return {
        head: [settings.value.label, settings.target.label],
        body: row ? [[formatNumber(row.value, settings.value.format), row.target === null ? "" : formatNumber(row.target, targetFormat(settings))]] : [],
      }
    }
    case "boxplot": {
      const boxes = new Map(boxRows(rows).map((b) => [b.x, b]))
      const lines = [...BOX_LINES].reverse()
      return {
        head: [settings.x.label, ...lines.map(([label]) => label)],
        body: settings.x.categories.map((c) => {
          const b = boxes.get(c)
          return [c, ...lines.map(([, key]) => (b ? formatNumber(b[key], settings.y.format) : ""))]
        }),
      }
    }
    case "candlestick": {
      const prices = new Map(priceRows(rows).map((p) => [p.x, p]))
      return {
        head: [settings.x.label, ...PRICE_LINES.map(([label]) => label)],
        body: settings.x.categories.map((c) => {
          const p = prices.get(c)
          return [c, ...PRICE_LINES.map(([, key]) => (p ? formatNumber(p[key], settings.y.format) : ""))]
        }),
      }
    }
    case "range": {
      const bands = new Map(bandRows(rows).map((b) => [b.x, b]))
      const text = (v: number | null | undefined, format: NumberFormat) => (v === null || v === undefined ? "" : formatNumber(v, format))
      return {
        head: [settings.x.label, settings.low.label, settings.high.label],
        body: settings.x.categories.map((c) => {
          const b = bands.get(c)
          return [c, text(b?.low, settings.low.format), text(b?.high, settings.high.format)]
        }),
      }
    }
  }
  const grid = valueGrid(settings, rows)
  const first = firstRows(settings, rows)
  const details = settings.details ?? []
  return {
    head: [settings.x.label, ...settings.series.values.map((s) => s.label), ...details.map((d) => d.label)],
    body: settings.x.categories.map((c, ci) => [
      c,
      ...grid.map((values, si) => (values[ci] === null ? "" : formatNumber(values[ci], formatOf(settings, settings.series.values[si])))),
      ...details.map((d) => detailText(first.get(c), d)),
    ]),
  }
}
