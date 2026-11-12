/**
 * A chart as rsync.ai describes it (dashboards-design.md § 3): plain JSON that
 * says what to draw, never how ECharts draws it. chart-option.ts turns settings,
 * rows and the theme into an ECharts option, one translator per type, so a saved
 * chart never holds an ECharts option and the library can change under it.
 * No ECharts import: this file is safe anywhere.
 */

import type { ChartColor } from "@/components/charts/chart-theme"
import type { ChartMap } from "@/lib/api/charts"

/** How a number is written: 1,234 · 40.5 · 1.2K · 8.1% (of 0.081). */
export type NumberFormat = "integer" | "decimal" | "compact" | "percent"

/** How a combo draws one of its measures. */
export type ChartMark = "bar" | "line" | "area"

export interface ChartSeries {
  /** The value of the series field that picks this series' rows. */
  key: string
  label: string
  color: ChartColor
  /**
   * A combo's measure (dashboards-design.md § 14.8): the row field it reads (default
   * `y.field`), its mark (default the type's), the axis it reads against (default the
   * left) and how it is written (default `y.format`).
   */
  value?: string
  mark?: ChartMark
  axis?: "left" | "right"
  format?: NumberFormat
}

/** A field the tooltip and the table show beside the measure. */
export interface ChartDetail {
  field: string
  label: string
  format: NumberFormat
}

interface XYChartSettings {
  /** What the chart shows, said to a screen reader. */
  description: string
  /** The category axis. Every category is on it, in order, so one no row has keeps its place. */
  x: { field: string; label: string; categories: string[] }
  /**
   * The measure. `format` is how the tooltip and table write it, `axisFormat`
   * (default `format`) how the axis does; `title` names the axis on the chart.
   */
  y: { field: string; format: NumberFormat; axisFormat?: NumberFormat; title?: string }
  /**
   * The series in the order they draw (and stack). Without `field` every row is in
   * each series, which reads its own `value`: one series, or a combo's two measures.
   * With it, one row per category and series.
   */
  series: { field?: string; values: ChartSeries[] }
  /** The right axis, when a combo's measure reads against it: titled with that measure. */
  y2?: { format: NumberFormat; title?: string }
  /** Stacked to 100%: each category's values drawn as their shares of its total. */
  percent?: boolean
  /** More of a category's fields, from its first row. */
  details?: ChartDetail[]
}

export interface BarChartSettings extends XYChartSettings {
  type: "bar"
  stacked: boolean
  /** A category drawn outlined: the one the page is about, such as this run among its neighbours. */
  highlight?: string
  /**
   * The least height a bar is drawn, in pixels: a run of 0 s, or one that did no
   * work, still shows as a bar beside one of minutes. A category no row has is
   * still not drawn.
   */
  minBarHeight?: number
}

/** Bars that run across: the categories down the side, the measure along the bottom. */
export interface HBarChartSettings extends XYChartSettings {
  type: "hbar"
  stacked: boolean
}

export interface LineChartSettings extends XYChartSettings {
  type: "line"
}

/** A line with the area under it filled; stacked, each series sits on the ones before it. */
export interface AreaChartSettings extends XYChartSettings {
  type: "area"
  stacked: boolean
}

/**
 * Each category's change, drawn from where the one before it ended, in the rise or the
 * fall colour; then a Total bar from zero.
 */
export interface WaterfallChartSettings extends XYChartSettings {
  type: "waterfall"
  /** A last bar with the sum of every step: left off when the rows were cut, as it would be short. */
  total: boolean
}

/** Stages in the order the rows give them, each as wide as its measure against the largest. */
export interface FunnelChartSettings extends XYChartSettings {
  type: "funnel"
}

/** Bins of one number, least to greatest, as bars that touch; a bin no row fell in is drawn empty. */
export interface HistogramChartSettings extends XYChartSettings {
  type: "histogram"
  /** Each category's least edge and the bins' width, for adding neighbours up into one bar where the chart is too narrow for a bar each. */
  bins?: { lo: number[]; width: number }
}

export type XYSettings =
  | BarChartSettings
  | HBarChartSettings
  | LineChartSettings
  | AreaChartSettings
  | WaterfallChartSettings
  | FunnelChartSettings
  | HistogramChartSettings

/** The rise, fall and total colours of a waterfall: blue up, orange down, grey for the total. */
export const WATERFALL_COLORS: readonly ChartColor[] = ["series-1", "series-2", "other"]

/**
 * A grid of cells, one per x and y category, each in one hue at one of five
 * steps from light to dark by its value (dashboards-design.md § 2.1): never a
 * rainbow, and the steps are named in the legend.
 */
export interface HeatmapChartSettings {
  type: "heatmap"
  description: string
  x: { field: string; label: string; categories: string[] }
  y: { field: string; label: string; categories: string[] }
  value: { field: string; label: string; format: NumberFormat }
  color: ChartColor
}

/**
 * Parts of a whole, largest first and Other last: a pie, or a donut with a hole. Only
 * parts above zero are drawn (saved-chart.ts says how many it left out).
 */
export interface PieChartSettings {
  type: "pie" | "donut"
  description: string
  /** The row field that names a row's slice, and the slices in the order they draw. */
  slices: { field: string; label: string; values: ChartSeries[] }
  value: { field: string; label: string; format: NumberFormat }
}

/**
 * A point a row at a number x and y, both value axes (dashboards-design.md § 14.8): a
 * scatter, or a bubble chart whose points are as large as their size (by area, so a
 * size twice another's is a point twice its area). Each point takes its colour's token;
 * without a colour field, every point is the first series. A saved chart's row is every
 * source row at one x, y, size and colour, and says how many.
 */
export interface ScatterChartSettings {
  type: "scatter" | "bubble"
  description: string
  /**
   * Each point's x and y, and its size (a number above zero) when it has one; each a
   * row field. Without a size every point is one size. `rows` is how many source rows
   * are at the point, when they were counted: the tooltip names a count above one.
   */
  points: { x: PointField; y: PointField; size?: PointField; rows?: PointField }
  /** The colours in the order they draw; with `field`, each row names its colour's key, else every row is the one. */
  series: { field?: string; label?: string; values: ChartSeries[] }
}

/** A point's number: the row field it is read from, its name, and how it is written. */
export interface PointField {
  field: string
  label: string
  format: NumberFormat
}

/**
 * Parts of a whole nested up to three levels deep (dashboards-design.md § 14.8): a treemap
 * of rectangles, or a sunburst of rings with the outermost level in the middle. Each row
 * is one part, a PartRow; a part takes its top-level group's colour.
 */
export interface PartsChartSettings {
  type: "treemap" | "sunburst"
  description: string
  /** Each level's name, outermost first. */
  levels: string[]
  value: { label: string; format: NumberFormat }
  /** The top-level groups that take a colour, largest first; every other group is Other. */
  groups: ChartSeries[]
  /** True when the row cap cut parts, so a share and a group's value are of the drawn parts only. */
  partial?: boolean
}

/** A treemap's or a sunburst's part: its name at each level, outermost first, and its measure above zero. */
export interface PartRow {
  path: string[]
  value: number
}

/**
 * Flows from one name to another, each as wide as its measure (§ 14.8). The nodes are
 * placed in the order given and never moved to untangle the links (§ 14.5); each row is
 * one link, a FlowRow, coloured as the node it leaves.
 */
export interface SankeyChartSettings {
  type: "sankey"
  description: string
  source: { label: string }
  target: { label: string }
  value: { label: string; format: NumberFormat }
  /** The nodes in the order they are placed, each keyed and labelled by its name. */
  nodes: ChartSeries[]
}

export interface FlowRow {
  source: string
  target: string
  value: number
}

/**
 * A day's number as a cell of a year's calendar, in one hue at one of five steps from
 * light to dark, as a heatmap's (§ 14.8); weeks start on Monday (ISO). Each row is one
 * day, a DayRow.
 */
export interface CalendarChartSettings {
  type: "calendar"
  description: string
  /** The first and the last day drawn, "YYYY-MM-DD". */
  range: [string, string]
  value: { label: string; format: NumberFormat }
  color: ChartColor
}

export interface DayRow {
  /** "YYYY-MM-DD". */
  day: string
  value: number
}

/**
 * A number against a target (§ 14.8): a bar as long as the number and a mark across it
 * at the target. One row, a BulletRow.
 */
export interface BulletChartSettings {
  type: "bullet"
  description: string
  value: { label: string; format: NumberFormat }
  /** The target's own format; the value's when it has none. */
  target: { label: string; format?: NumberFormat }
  color: ChartColor
}

export interface BulletRow {
  value: number
  target: number | null
}

/**
 * A box per category (§ 14.8): its lower and upper quartile, the median across it and
 * whiskers to the least and the greatest value. Each row is one category, a BoxRow, in
 * the categories' order.
 */
export interface BoxplotChartSettings {
  type: "boxplot"
  description: string
  x: { label: string; categories: string[] }
  y: { label: string; format: NumberFormat }
  color: ChartColor
}

export interface BoxRow {
  x: string
  min: number
  q1: number
  median: number
  q3: number
  max: number
}

/**
 * A period's open, high, low and close (§ 14.8): a body from the open to the close in
 * the rise colour when it closed above its open and the fall colour when below, and a
 * wick from the low to the high. Each row is one period, a PriceRow.
 */
export interface CandlestickChartSettings {
  type: "candlestick"
  description: string
  x: { label: string; categories: string[] }
  y: { label: string; format: NumberFormat }
}

export interface PriceRow {
  x: string
  open: number
  high: number
  low: number
  close: number
}

/**
 * The band between two measures along the categories (§ 14.8), such as a forecast's
 * low and high: drawn from the lesser to the greater of the two at each category, so
 * the band never folds. Each row is one category, a BandRow; a gap where either is null.
 */
export interface RangeChartSettings {
  type: "range"
  description: string
  x: { label: string; categories: string[] }
  low: { label: string; format: NumberFormat }
  high: { label: string; format: NumberFormat }
  color: ChartColor
}

export interface BandRow {
  x: string
  low: number | null
  high: number | null
}

/**
 * World countries or US states (§ 14.8), each shape a row names filled in one hue at one
 * of five steps from light to dark, as a heatmap's; a shape no row names in the border
 * colour. Each row is one shape, a RegionRow.
 */
export interface MapChartSettings {
  type: "map"
  description: string
  map: ChartMap
  region: { label: string }
  value: { label: string; format: NumberFormat }
  color: ChartColor
}

export interface RegionRow {
  /** The name ECharts knows the shape by (map-regions.ts): a country's alpha-3 code, a state's "US-CA". */
  region: string
  value: number
}

export type ChartSettings =
  | XYSettings
  | HeatmapChartSettings
  | PieChartSettings
  | ScatterChartSettings
  | PartsChartSettings
  | SankeyChartSettings
  | CalendarChartSettings
  | BulletChartSettings
  | BoxplotChartSettings
  | CandlestickChartSettings
  | RangeChartSettings
  | MapChartSettings

/** A chart's plot height in pixels when nothing sets one. */
export const CHART_HEIGHT = 180

/** What a chart is drawn from. Keep rows stable: a new array draws again; settings start over only when what they say changes. */
export interface ChartProps {
  settings: ChartSettings
  rows: readonly object[]
  /**
   * The plot's height in pixels; CHART_HEIGHT when absent. "fill" takes the room the
   * chart's box has, less its legend: the box must be a flex column of a set height.
   */
  height?: number | "fill"
  /** Called with a category's index when its mark is clicked, for a chart whose categories open a page. */
  onCategoryClick?: (index: number) => void
}

// Each format's writer for a count of fraction digits, made once.
const WRITERS = new Map<string, Intl.NumberFormat>()

function writer(format: NumberFormat, digits: number): Intl.NumberFormat {
  const key = `${format} ${digits}`
  let w = WRITERS.get(key)
  if (!w) {
    const style = format === "percent" ? { style: "percent" as const } : format === "compact" ? { notation: "compact" as const } : {}
    w = new Intl.NumberFormat("en-US", { ...style, maximumFractionDigits: digits })
    WRITERS.set(key, w)
  }
  return w
}

/** v in ten significant digits, so what a sum's float error leaves (0.1 + 0.2) reads as meant. */
const clean = (v: number) => Number(v.toPrecision(10))

/** The power of ten of |v|'s first digit: 2 for 403.6, -2 for 0.0123, 0 for 0. */
export function exponentOf(v: number): number {
  return Number(Math.abs(v).toExponential().split("e")[1])
}

/**
 * The digits after the point v is written with in full, once divided by 10^unit: 2 for
 * 1.25; 0 for 1,200; 2 for 1,010,000 in millions (unit 6); 2 for 0.0125 in percent (unit -2).
 */
export function decimalsOf(v: number, unit = 0): number {
  const [m, e] = Math.abs(v).toExponential().split("e")
  return Math.max(0, (m.split(".")[1] ?? "").length - Number(e) + unit)
}

/**
 * The digits an edge keeps of bins or steps this wide: the width's own decimals, to its
 * third significant digit at most, so neighbours read apart without the float tail
 * (8,075 for a width of 8,072.2165; 1.25 for 1.25). The gateway's chartEdgeDigits.
 */
export function edgeDigits(width: number, unit = 0): number {
  const w = clean(Math.abs(width))
  return Math.min(20, Math.max(0, Math.min(decimalsOf(w, unit), 2 - exponentOf(w) + unit)))
}

/** The power of ten a format writes value in: a percent in hundredths, compact in K, M, B or T. */
function unitOf(value: number, format: NumberFormat): number {
  if (format === "percent") return -2
  if (format !== "compact") return 0
  const e = exponentOf(value)
  return e < 3 ? 0 : Math.min(12, e - (e % 3))
}

/**
 * The fraction digits a format writes value with: none for an integer, else one, but two
 * significant digits under 1 (0.03, 0.45%), so a value that is not 0 never reads 0. The
 * gateway's chartFractionDigits.
 */
function fractionDigits(value: number, format: NumberFormat): number {
  if (format === "integer") return 0
  const v = Math.abs(value)
  if (!Number.isFinite(v) || v === 0 || (format === "compact" && v >= 1)) return 1
  return Math.min(20, Math.max(1, 1 - exponentOf(v) - (format === "percent" ? 2 : 0)))
}

export function formatNumber(value: number, format: NumberFormat): string {
  return writer(format, fractionDigits(value, format)).format(value)
}

/**
 * A value axis's tick: as formatNumber writes it, and with every decimal the tick has, as
 * ECharts writes a tick, so ticks finer than the format (0.01, 0.02) never read the same.
 */
export function formatTick(value: number, format: NumberFormat): string {
  if (format === "integer" || !Number.isFinite(value) || value === 0) return formatNumber(value, format)
  const own = decimalsOf(clean(value), unitOf(value, format))
  return writer(format, Math.min(20, Math.max(fractionDigits(value, format), own))).format(value)
}

/**
 * An edge of a step or bin this wide, with the digits edgeDigits gives it, never "-0":
 * neighbouring steps read apart (1.02–1.03, not 1–1) and an unround edge reads round.
 * A width of 0 (every value the same) writes the value in full, to ten significant digits.
 */
export function formatEdge(value: number, width: number, format: NumberFormat): string {
  if (format === "integer" || !Number.isFinite(value) || !Number.isFinite(width)) return formatNumber(value, format)
  const unit = unitOf(value, format)
  const w = writer(format, width === 0 ? Math.min(20, decimalsOf(clean(value), unit)) : edgeDigits(width, unit))
  const text = w.format(clean(value))
  return text.startsWith("-") && text.slice(1) === w.format(0) ? text.slice(1) : text
}

/**
 * A histogram's bin as "least–greatest", each edge with the digits the bin width needs to
 * tell neighbours apart (2–8,075, not 2.4–8074.6165), or its one value in full when every
 * row holds the same number (a width of 0). The gateway's chartBinLabel writes a
 * workflow's the same way: shared/chart_bin_labels_golden.json holds both to it.
 */
export function binLabel(lo: number, width: number): string {
  if (width === 0) return formatEdge(lo, 0, "decimal")
  return `${formatEdge(lo, width, "decimal")}–${formatEdge(lo + width, width, "decimal")}`
}
