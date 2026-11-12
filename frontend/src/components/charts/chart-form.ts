/**
 * The chart page's fields (dashboards-design.md § 7.2) and the settings they make.
 * Picking a type changes the fields it asks for: toSettings sends a type only the
 * fields it draws, which is the shape the server holds it to (charts/settings.go
 * validateShape), so a field left over from another type is never sent.
 */

import { CHART_REGISTRY, chartSpec } from "@/components/charts/chart-registry"
import {
  MAX_TABLE_COLUMNS,
  one,
  all,
  type ChartAggregate,
  type ChartAxis,
  type ChartField,
  type ChartGrain,
  type ChartMap,
  type ChartMark,
  type ChartNumberFormat,
  type ChartSort,
  type ChartSourceColumn,
  type ChartType,
  type ServerChartSettings,
} from "@/lib/api/charts"

/** A pivot table's row or column field: a column, its title, and a time column's date grain. */
export interface PivotFieldForm {
  column: string
  title: string
  grain: ChartGrain | ""
}

/** A pivot table's row fields and column fields at most (§ 14.8). */
export const MAX_PIVOT_ROWS = 3
export const MAX_PIVOT_COLUMNS = 2
/** A treemap's or a sunburst's levels at most. */
export const MAX_LEVELS = 3
/** A candlestick's y columns, in the order it reads them. */
export const PRICE_FIELDS = ["Open", "High", "Low", "Close"] as const

export interface ChartForm {
  type: ChartType
  description: string
  x: string
  xTitle: string
  /** A time x grouped by day, week, month, quarter or year; "" groups by each value. */
  xGrain: ChartGrain | ""
  y: string
  yTitle: string
  yGrain: ChartGrain | ""
  /** A scatter's or a bubble chart's size, a number column; a bubble needs one. */
  size: string
  sizeTitle: string
  /** "" until one is picked: there is no default aggregate. */
  aggregate: ChartAggregate | ""
  measureColumn: string
  measureTitle: string
  format: ChartNumberFormat | ""
  /** A series token for a chart with one series; "" leaves it to the chart. */
  color: string
  series: string
  stacked: boolean
  /** A stacked chart's series as each one's share of its whole. */
  percent: boolean
  /** A combo's measures (§ 14.8): each drawn as a mark against an axis; "" is the type's own mark and the left axis. */
  mark: ChartMark | ""
  axis: ChartAxis | ""
  /** A bar, line or area chart's second measure; "" when it has one. */
  aggregate2: ChartAggregate | ""
  measureColumn2: string
  measureTitle2: string
  format2: ChartNumberFormat | ""
  mark2: ChartMark | ""
  axis2: ChartAxis | ""
  /** A pivot table's third measure; "" when it has two or fewer. */
  aggregate3: ChartAggregate | ""
  measureColumn3: string
  measureTitle3: string
  format3: ChartNumberFormat | ""
  /** A pivot table's row fields and column fields, in order; a field with no column is not sent. */
  pivotRows: PivotFieldForm[]
  pivotColumns: PivotFieldForm[]
  /** A treemap's or a sunburst's levels, outermost first; a level has no grain. */
  levels: PivotFieldForm[]
  /** A bullet chart's target: a number typed (null until one is) or its second measure. */
  bulletTarget: "number" | "measure"
  target: number | null
  /** A candlestick's open, high, low and close columns. */
  prices: string[]
  /** A map's shapes: world countries or US states, which its x's codes name. */
  map: ChartMap
  /** A histogram's bins between its least and greatest value, or each bin's width; 0 leaves it to the chart. */
  bins: number
  width: number
  sort: ChartSort | ""
  columns: string[]
  compareColumn: string
  compareDays: number
  /** A scorecard with a comparison draws its current period as a small line under the number. */
  sparkline: boolean
}

export const EMPTY_FORM: ChartForm = {
  type: "bar",
  description: "",
  x: "",
  xTitle: "",
  xGrain: "",
  y: "",
  yTitle: "",
  yGrain: "",
  size: "",
  sizeTitle: "",
  aggregate: "",
  measureColumn: "",
  measureTitle: "",
  format: "",
  color: "",
  series: "",
  stacked: false,
  percent: false,
  mark: "",
  axis: "",
  aggregate2: "",
  measureColumn2: "",
  measureTitle2: "",
  format2: "",
  mark2: "",
  axis2: "",
  aggregate3: "",
  measureColumn3: "",
  measureTitle3: "",
  format3: "",
  pivotRows: [],
  pivotColumns: [],
  levels: [],
  bulletTarget: "number",
  target: null,
  prices: ["", "", "", ""],
  map: "world",
  bins: 0,
  width: 0,
  sort: "",
  columns: [],
  compareColumn: "",
  compareDays: 7,
  sparkline: false,
}

/** The types, grouped by what they are for. */
export const TYPE_GROUPS: { label: string; types: { type: ChartType; label: string }[] }[] = [
  {
    label: "Change over time",
    types: [
      { type: "line", label: "Line" },
      { type: "area", label: "Area" },
      { type: "waterfall", label: "Waterfall" },
      { type: "range", label: "Range bands" },
      { type: "candlestick", label: "Candlestick" },
      { type: "calendar", label: "Calendar heatmap" },
    ],
  },
  {
    label: "Compare",
    types: [
      { type: "bar", label: "Bar" },
      { type: "hbar", label: "Horizontal bar" },
      { type: "funnel", label: "Funnel" },
    ],
  },
  {
    label: "Part of a whole",
    types: [
      { type: "pie", label: "Pie" },
      { type: "donut", label: "Donut" },
      { type: "treemap", label: "Treemap" },
      { type: "sunburst", label: "Sunburst" },
    ],
  },
  {
    label: "Distribution",
    types: [
      { type: "histogram", label: "Histogram" },
      { type: "boxplot", label: "Box plot" },
    ],
  },
  {
    label: "Relationship",
    types: [
      { type: "scatter", label: "Scatter" },
      { type: "bubble", label: "Bubble" },
    ],
  },
  { label: "Flow", types: [{ type: "sankey", label: "Sankey" }] },
  { label: "Two dimensions", types: [{ type: "heatmap", label: "Heatmap" }] },
  { label: "Places", types: [{ type: "map", label: "Choropleth map" }] },
  {
    label: "One number",
    types: [
      { type: "scorecard", label: "Scorecard" },
      { type: "bullet", label: "Bullet" },
    ],
  },
  {
    label: "Rows",
    types: [
      { type: "table", label: "Table" },
      { type: "pivot", label: "Pivot table" },
    ],
  },
]

export const TYPE_LABEL: Record<ChartType, string> = Object.fromEntries(
  CHART_REGISTRY.map((t) => [t.type, t.label]),
) as Record<ChartType, string>

export const AGGREGATE_LABEL: Record<ChartAggregate, string> = {
  count: "Count of rows",
  count_distinct: "Count distinct",
  sum: "Sum",
  avg: "Average",
  min: "Lowest",
  max: "Highest",
}

const XY: readonly ChartType[] = ["bar", "hbar", "line", "area"]
/** A bar, line or area chart: an x, a measure and an optional series. */
export const isXY = (t: ChartType) => XY.includes(t)
/** Bars and areas stack; a line does not (the server refuses it). */
export const canStack = (t: ChartType) => chartSpec(t)?.stackable ?? false
/** A scatter or a bubble chart: a point a row, at a number x and y. */
export const isPoints = (t: ChartType) => t === "scatter" || t === "bubble"
/**
 * A type with an x column, picked on its own (a scatter's points are picked together): a
 * column the chart groups by, a histogram's bins, a sankey's sources, a calendar's days.
 */
export const takesX = (t: ChartType) => !isPoints(t) && (chartSpec(t)?.roles.some((r) => r.name === "x") ?? false)
/** The column kinds a type's x takes: a number for a histogram, a date for a calendar or a candlestick; [] for any. */
export const xKinds = (t: ChartType): string[] => {
  const kinds = chartSpec(t)?.roles.find((r) => r.name === "x")?.kinds ?? []
  return kinds.includes("any") ? [] : kinds
}
/** The columns a field's picker offers; wrongColumn holds a pick kept from another type or aggregate to the same. */
type Takes = (c: ChartSourceColumn) => boolean
/** A number the chart reads to place or size a mark: a masked column's values are never read. */
export const numberColumns: Takes = (c) => c.kind === "number" && !c.masked
/** A point's colour: a text or a yes-or-no column. */
export const colourColumns: Takes = (c) => c.kind === "text" || c.kind === "bool"
/** A lowest or a highest a chart other than a table or a pivot table draws: it draws a number. */
const drawnAsNumber = (aggregate: ChartAggregate | "", t: ChartType) =>
  (aggregate === "min" || aggregate === "max") && t !== "table" && t !== "pivot"
/**
 * A measure's column: a sum and an average need numbers, as does a lowest or a highest
 * outside a table or a pivot table (settings.go CheckKinds), and a masked column is never
 * measured.
 */
export const measureColumns =
  (aggregate: ChartAggregate | "", t: ChartType): Takes =>
  (c) =>
    !c.masked && (aggregate === "sum" || aggregate === "avg" || drawnAsNumber(aggregate, t) ? c.kind === "number" : true)
/** A type's x, when it takes some kinds only: a histogram's numbers, a calendar's dates, a map's unmasked codes. */
export const xColumns = (t: ChartType): Takes | undefined => {
  const kinds = xKinds(t)
  return kinds.length > 0 ? (c) => kinds.includes(c.kind) && !(t === "map" && c.masked) : undefined
}
/** A type whose x takes a date grain: a funnel's stages and a histogram's bins do not. */
export const takesGrain = (t: ChartType) => chartSpec(t)?.roles.find((r) => r.name === "x")?.attrs.includes("grain") ?? false
/**
 * A type that takes a second measure: a bar, line or area chart (a combo, § 14.8), a pivot
 * table, a bullet chart's target or a range's high.
 */
export const takesSecondMeasure = (t: ChartType) => (chartSpec(t)?.roles.find((r) => r.name === "measure")?.max ?? 0) > 1
/** A type whose measure adds up to a whole, so a count or a sum: a pie, a donut, a waterfall. */
export const measureAggregates = (t: ChartType): readonly ChartAggregate[] => chartSpec(t)?.aggregates ?? []
/** A type whose one series takes its own colour; a pie's slices and a waterfall's steps have theirs. */
export const takesColor = (t: ChartType) => chartSpec(t)?.roles.find((r) => r.name === "measure")?.attrs.includes("color") ?? false
/**
 * A chart whose default order draws it largest first, as the gateway's chart_sql.go orders
 * it: a funnel, or a bar of one measure and no series whose x is not a date (`xKind`, the
 * x column's kind). A bar over dates, with a grain or without one, reads oldest first.
 */
export const defaultsLargestFirst = (form: ChartForm, xKind?: string) =>
  form.type === "funnel" ||
  ((form.type === "bar" || form.type === "hbar") && form.series === "" && form.aggregate2 === "" && form.xGrain === "" && xKind !== "time")

/**
 * Why a type cannot be drawn from a source on this engine, as the gateway's chart_sql.go
 * refuses it (and modelDialect names the engine); none when it can. A box plot needs an
 * exact percentile as an aggregate (dashboards-design.md § 14.8, open question 10).
 */
export function refusedTypes(connectorType = ""): Partial<Record<ChartType, string>> {
  const ct = connectorType.toLowerCase()
  if (ct.includes("postgres") || ct.includes("redshift")) return {}
  if (ct.includes("mysql") || ct.includes("mariadb")) {
    return { boxplot: "A box plot needs an exact percentile, which MySQL and MariaDB do not have (MariaDB has one only as a window function)." }
  }
  if (ct.includes("sqlserver") || ct.includes("mssql")) {
    return { boxplot: "A box plot needs an exact percentile, which SQL Server has only as a window function." }
  }
  return {}
}

/**
 * Line is suggested when the x is a time column, bar when it is text, and scatter when
 * the x and the y are both numbers (§ 14.4): the fit of the fields picked, never a guess
 * about the data.
 */
export function suggestedTypes(form: ChartForm, columns: ChartSourceColumn[]): ChartType[] {
  const kindOf = (name: string) => columns.find((c) => c.name === name)?.kind
  const kind = kindOf(form.x)
  if (kind === "time") return ["line"]
  if (kind === "text" || kind === "bool") return ["bar"]
  if (kind === "number" && kindOf(form.y) === "number") return ["scatter"]
  return []
}

export const COLUMN_GROUPS: { kind: string; label: string }[] = [
  { kind: "time", label: "Time" },
  { kind: "number", label: "Numbers" },
  { kind: "text", label: "Text" },
  { kind: "other", label: "Other" },
]

export function columnGroup(kind: string): string {
  return kind === "time" || kind === "number" || kind === "text" ? kind : "other"
}

/** The settings the fields make (version 2, by role), with only what this type draws. */
export function toSettings(form: ChartForm, pins?: { value: string; color: string }[]): ServerChartSettings {
  const s: ServerChartSettings = { version: 2, type: form.type, roles: {} }
  const description = form.description.trim()
  if (description) s.description = description
  const axis = (column: string, title: string, grain: ChartGrain | ""): ChartField => {
    const a: ChartField = { column }
    if (title.trim()) a.title = title.trim()
    if (grain) a.grain = grain
    return a
  }
  const measure = (
    aggregate = form.aggregate,
    column = form.measureColumn,
    title = form.measureTitle,
    format = form.format,
  ): ChartField | undefined => {
    if (!aggregate) return undefined
    const m: ChartField = { aggregate }
    if (aggregate !== "count" && column) m.column = column
    if (title.trim()) m.title = title.trim()
    if (format) m.format = format
    return m
  }
  const seriesOf = (column: string): ChartField => {
    const series: ChartField = { column }
    const kept = (pins ?? []).slice(0, 7)
    if (kept.length > 0) series.colors = kept
    return series
  }
  switch (form.type) {
    case "bar":
    case "hbar":
    case "line":
    case "area": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, form.xGrain)]
      const m = measure()
      // A second measure makes a combo, which has no series (§ 14.8).
      const m2 = takesSecondMeasure(form.type)
        ? measure(form.aggregate2, form.measureColumn2, form.measureTitle2, form.format2)
        : undefined
      if (m && m2) {
        if (form.mark) m.mark = form.mark
        if (form.axis) m.axis = form.axis
        if (form.mark2) m2.mark = form.mark2
        if (form.axis2) m2.axis = form.axis2
        if (form.color) m.color = form.color
        s.roles.measure = [m, m2]
      } else if (m) {
        if (!form.series && form.color) m.color = form.color
        s.roles.measure = [m]
        if (form.series) {
          s.roles.series = [seriesOf(form.series)]
          if (form.stacked && canStack(form.type)) {
            s.stacked = true
            if (form.percent) s.percent = true
          }
        }
      }
      if (form.sort) s.sort = form.sort
      break
    }
    case "pie":
    case "donut": {
      if (form.series) s.roles.series = [seriesOf(form.series)]
      const m = measure()
      if (m) s.roles.measure = [m]
      break
    }
    case "waterfall":
    case "funnel": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, takesGrain(form.type) ? form.xGrain : "")]
      const m = measure()
      if (m) {
        if (form.color && takesColor(form.type)) m.color = form.color
        s.roles.measure = [m]
      }
      if (form.sort) s.sort = form.sort
      break
    }
    case "histogram": {
      if (form.x) {
        const x = axis(form.x, form.xTitle, "")
        // A width when one is set, else a number of bins; never both (the server refuses it).
        if (form.width > 0) x.width = form.width
        else if (form.bins > 0) x.bins = form.bins
        s.roles.x = [x]
      }
      break
    }
    case "heatmap": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, form.xGrain)]
      if (form.y) s.roles.y = [axis(form.y, form.yTitle, form.yGrain)]
      const m = measure()
      if (m) {
        if (form.color) m.color = form.color
        s.roles.measure = [m]
      }
      if (form.sort === "x_asc" || form.sort === "x_desc") s.sort = form.sort
      break
    }
    case "scorecard": {
      const m = measure()
      if (m) s.roles.measure = [m]
      if (form.compareColumn) {
        s.roles.date = [{ column: form.compareColumn }]
        s.compare = { days: form.compareDays }
        // A sparkline is drawn over the period compared, which must be longer than a day.
        if (form.sparkline && form.compareDays > 1) s.sparkline = true
      }
      break
    }
    case "table": {
      s.roles.columns = form.columns.slice(0, MAX_TABLE_COLUMNS).map((column) => ({ column }))
      const m = measure()
      if (m) s.roles.measure = [m]
      if (form.sort && (m || form.sort === "x_asc" || form.sort === "x_desc")) s.sort = form.sort
      break
    }
    case "scatter":
    case "bubble": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, "")]
      if (form.y) s.roles.y = [axis(form.y, form.yTitle, "")]
      if (form.size) s.roles.size = [axis(form.size, form.sizeTitle, "")]
      // The colour of each point, a text column; past 7 values the rest are Other.
      if (form.series) s.roles.series = [seriesOf(form.series)]
      break
    }
    case "pivot": {
      const fields = (picked: PivotFieldForm[], max: number) =>
        picked.filter((f) => f.column).slice(0, max).map((f) => axis(f.column, f.title, f.grain))
      const rows = fields(form.pivotRows, MAX_PIVOT_ROWS)
      if (rows.length > 0) s.roles.rows = rows
      const cols = fields(form.pivotColumns, MAX_PIVOT_COLUMNS)
      if (cols.length > 0) s.roles.columns = cols
      // Each measure a column of the cells, in order; a later one only after an earlier.
      const ms: ChartField[] = []
      for (const m of [
        measure(),
        measure(form.aggregate2, form.measureColumn2, form.measureTitle2, form.format2),
        measure(form.aggregate3, form.measureColumn3, form.measureTitle3, form.format3),
      ]) {
        if (!m) break
        ms.push(m)
      }
      if (ms.length > 0) s.roles.measure = ms
      break
    }
    case "treemap":
    case "sunburst": {
      const levels = form.levels.filter((f) => f.column).slice(0, MAX_LEVELS).map((f) => axis(f.column, f.title, ""))
      if (levels.length > 0) s.roles.rows = levels
      const m = measure()
      if (m) s.roles.measure = [m]
      break
    }
    case "sankey":
    case "calendar": {
      // A sankey's x is where each flow starts and its y where it ends; a calendar's x its days.
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, "")]
      if (form.y && form.type === "sankey") s.roles.y = [axis(form.y, form.yTitle, "")]
      const m = measure()
      if (m) s.roles.measure = [m]
      break
    }
    case "bullet": {
      const m = measure()
      if (m) {
        if (form.color) m.color = form.color
        // The target is a second measure or a typed number, never both (the server refuses it).
        const target = form.bulletTarget === "measure" ? measure(form.aggregate2, form.measureColumn2, form.measureTitle2, form.format2) : undefined
        s.roles.measure = target ? [m, target] : [m]
      }
      if (form.bulletTarget === "number" && form.target !== null) s.target = form.target
      break
    }
    case "boxplot": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, form.xGrain)]
      if (form.y) {
        const y = axis(form.y, form.yTitle, "")
        if (form.format) y.format = form.format
        s.roles.y = [y]
      }
      if (form.sort === "x_asc" || form.sort === "x_desc") s.sort = form.sort
      break
    }
    case "candlestick": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, form.xGrain)]
      if (form.prices.length === PRICE_FIELDS.length && form.prices.every(Boolean)) s.roles.y = form.prices.map((column) => ({ column }))
      break
    }
    case "range": {
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, form.xGrain)]
      // The low, then the high.
      const low = measure()
      const high = measure(form.aggregate2, form.measureColumn2, form.measureTitle2, form.format2)
      if (low) s.roles.measure = high ? [low, high] : [low]
      if (form.sort === "x_asc" || form.sort === "x_desc") s.sort = form.sort
      break
    }
    case "map": {
      // The x is each shape's ISO code, read as the map's countries' or states' codes.
      if (form.x) s.roles.x = [axis(form.x, form.xTitle, "")]
      const m = measure()
      if (m) {
        if (form.color) m.color = form.color
        s.roles.measure = [m]
      }
      s.map = form.map
      break
    }
  }
  return s
}

/** The fields a saved chart's settings fill in. */
export function fromSettings(s: ServerChartSettings): ChartForm {
  const x = one(s, "x")
  const y = one(s, "y")
  const size = one(s, "size")
  const [m, m2, m3] = all(s, "measure")
  const pivot = s.type === "pivot"
  const pivotFields = (role: "rows" | "columns"): PivotFieldForm[] =>
    pivot ? all(s, role).map((f) => ({ column: f.column ?? "", title: f.title ?? "", grain: f.grain ?? "" })) : []
  const parts = s.type === "treemap" || s.type === "sunburst"
  // A candlestick's y is its four prices, not one y column.
  const candle = s.type === "candlestick"
  const prices = candle ? all(s, "y").map((f) => f.column ?? "") : []
  return {
    ...EMPTY_FORM,
    type: s.type,
    description: s.description ?? "",
    x: x?.column ?? "",
    xTitle: x?.title ?? "",
    xGrain: x?.grain ?? "",
    y: candle ? "" : (y?.column ?? ""),
    yTitle: candle ? "" : (y?.title ?? ""),
    yGrain: y?.grain ?? "",
    size: size?.column ?? "",
    sizeTitle: size?.title ?? "",
    aggregate: m?.aggregate ?? "",
    measureColumn: m?.column ?? "",
    measureTitle: m?.title ?? "",
    // A box plot's number format is its y column's.
    format: m?.format ?? (s.type === "boxplot" ? y?.format : undefined) ?? "",
    color: m?.color ?? "",
    series: one(s, "series")?.column ?? "",
    stacked: !!s.stacked,
    percent: !!s.percent,
    mark: m?.mark ?? "",
    axis: m?.axis ?? "",
    aggregate2: m2?.aggregate ?? "",
    measureColumn2: m2?.column ?? "",
    measureTitle2: m2?.title ?? "",
    format2: m2?.format ?? "",
    mark2: m2?.mark ?? "",
    axis2: m2?.axis ?? "",
    aggregate3: m3?.aggregate ?? "",
    measureColumn3: m3?.column ?? "",
    measureTitle3: m3?.title ?? "",
    format3: m3?.format ?? "",
    pivotRows: pivotFields("rows"),
    pivotColumns: pivotFields("columns"),
    levels: parts ? all(s, "rows").map((f) => ({ column: f.column ?? "", title: f.title ?? "", grain: "" })) : [],
    bulletTarget: s.type === "bullet" && m2 ? "measure" : "number",
    target: s.target ?? null,
    prices: [...prices, "", "", "", ""].slice(0, PRICE_FIELDS.length),
    map: s.map ?? EMPTY_FORM.map,
    bins: x?.bins ?? 0,
    width: x?.width ?? 0,
    sort: s.sort ?? "",
    // A pivot table's columns are its column fields, not a table's columns.
    columns: pivot ? [] : all(s, "columns").flatMap((f) => (f.column ? [f.column] : [])),
    compareColumn: s.compare ? (one(s, "date")?.column ?? "") : "",
    compareDays: s.compare?.days ?? EMPTY_FORM.compareDays,
    sparkline: !!s.sparkline,
  }
}

/** What the type still needs before it can be drawn, in words; null when nothing. */
export function missingField(form: ChartForm): string | null {
  const needsColumn = form.aggregate !== "" && form.aggregate !== "count" && !form.measureColumn
  const wrongAggregate = form.aggregate !== "" && !measureAggregates(form.type).includes(form.aggregate)
  const needsColumn2 = form.aggregate2 !== "" && form.aggregate2 !== "count" && !form.measureColumn2
  // The second measure is the first again: the result would name one column twice.
  const sameMeasures = form.aggregate2 === form.aggregate && (form.aggregate === "count" || form.measureColumn2 === form.measureColumn)
  switch (form.type) {
    case "bar":
    case "hbar":
    case "line":
    case "area": {
      if (!form.x) return "Pick the x column."
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      if (!takesSecondMeasure(form.type) || !form.aggregate2) {
        // toSettings sends 100% only with a series, stacked, on a type that stacks.
        const percent = form.series && form.stacked && form.percent && canStack(form.type)
        if (percent && form.aggregate !== "count" && form.aggregate !== "sum") {
          return "A share of 100% adds the series up: measure a count or a sum."
        }
        return null
      }
      if (needsColumn2) return "Pick the column for the second measure."
      if (sameMeasures) return "The two measures are the same; measure another column or use another aggregate."
      if (form.axis === "right" && form.axis2 === "right") return "Put one of the two measures on the left axis."
      return null
    }
    case "pie":
    case "donut":
      if (!form.series) return "Pick the column whose values are the slices."
      if (!form.aggregate) return "Pick what to measure."
      if (wrongAggregate) return "A slice is a share of a whole: measure a count or a sum."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "waterfall":
    case "funnel":
      if (!form.x) return form.type === "funnel" ? "Pick the column whose values are the stages." : "Pick the x column."
      if (!form.aggregate) return "Pick what to measure."
      if (wrongAggregate) return "Each step adds to a running total: measure a count or a sum."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "histogram":
      if (!form.x) return "Pick the number column to count in bins."
      return null
    case "heatmap":
      if (!form.x) return "Pick the x column."
      if (!form.y) return "Pick the y column."
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "scorecard":
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "table":
      if (form.columns.length === 0) return "Pick at least one column."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "scatter":
    case "bubble": {
      if (!form.x) return "Pick the number column for the x axis."
      if (!form.y) return "Pick the number column for the y axis."
      if (form.type === "bubble" && !form.size) return "Pick the number column each bubble's size reads."
      const picked = [form.x, form.y, form.size].filter(Boolean)
      if (new Set(picked).size < picked.length) return "Pick a different column for each of x, y and size."
      return null
    }
    case "pivot": {
      const rows = form.pivotRows.filter((f) => f.column)
      const cols = form.pivotColumns.filter((f) => f.column)
      if (rows.length === 0) return "Pick at least one row field."
      const fields = [...rows, ...cols].map((f) => f.column)
      if (new Set(fields).size < fields.length) return "A column is picked as a field twice; pick each once."
      if (!form.aggregate) return "Pick what to measure."
      // As toSettings sends them: a later measure only after an earlier one.
      const all3 = [
        { aggregate: form.aggregate, column: form.measureColumn },
        { aggregate: form.aggregate2, column: form.measureColumn2 },
        { aggregate: form.aggregate3, column: form.measureColumn3 },
      ]
      const gap = all3.findIndex((m) => m.aggregate === "")
      const ms = gap === -1 ? all3 : all3.slice(0, gap)
      for (const [i, m] of ms.entries()) {
        if (m.aggregate !== "count" && !m.column) return i === 0 ? "Pick the column to measure." : `Pick the column for measure ${i + 1}.`
        for (const prev of ms.slice(0, i)) {
          if (prev.aggregate === m.aggregate && (m.aggregate === "count" || prev.column === m.column)) {
            return "Two measures are the same; measure another column or use another aggregate."
          }
        }
      }
      return null
    }
    case "treemap":
    case "sunburst": {
      const levels = form.levels.filter((f) => f.column).map((f) => f.column)
      if (levels.length === 0) return "Pick at least one level."
      if (new Set(levels).size < levels.length) return "A column is picked as a level twice; pick each once."
      if (!form.aggregate) return "Pick what to measure."
      if (wrongAggregate) return "Each part is a share of a whole: measure a count or a sum."
      if (needsColumn) return "Pick the column to measure."
      return null
    }
    case "sankey":
      if (!form.x) return "Pick the column the flows start from."
      if (!form.y) return "Pick the column the flows end at."
      if (form.x === form.y) return "Pick a different column for where the flows start and where they end."
      if (!form.aggregate) return "Pick what to measure."
      if (wrongAggregate) return "Each flow is as wide as its share: measure a count or a sum."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "calendar":
      if (!form.x) return "Pick the date column for the days."
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      return null
    case "bullet":
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      if (form.bulletTarget === "number") return form.target === null ? "Type the target, or measure it." : null
      if (!form.aggregate2) return "Pick what the target measures."
      if (needsColumn2) return "Pick the column for the target."
      if (sameMeasures) return "The target is the measure itself; measure another column or use another aggregate."
      return null
    case "boxplot":
      if (!form.x) return "Pick the column whose values are the groups."
      if (!form.y) return "Pick the number column to draw the boxes of."
      return null
    case "candlestick":
      if (!form.x) return "Pick the date column for the periods."
      if (form.prices.length < PRICE_FIELDS.length || !form.prices.every(Boolean)) return "Pick the open, high, low and close columns."
      return null
    case "range":
      if (!form.x) return "Pick the x column."
      if (!form.aggregate) return "Pick what the low measures."
      if (needsColumn) return "Pick the column for the low."
      if (!form.aggregate2) return "Pick what the high measures."
      if (needsColumn2) return "Pick the column for the high."
      if (sameMeasures) return "The low and the high are the same measure; measure another column or use another aggregate."
      return null
    case "map":
      if (!form.x) return form.map === "us_states" ? "Pick the column of US state codes." : "Pick the column of country codes."
      if (!form.aggregate) return "Pick what to measure."
      if (needsColumn) return "Pick the column to measure."
      return null
  }
}

/**
 * A date's column as the server takes it (CheckKinds): a date, a number (a warehouse can
 * hold a date as 20261001), or text where the chart reads it as a date (`cast`: grouped
 * by a grain, or a calendar's day). The pickers offer the dates.
 */
const dateColumns =
  (cast: boolean): Takes =>
  (c) =>
    c.kind === "time" || c.kind === "number" || (cast && c.kind === "text")

const KIND_NOUN: Record<string, string> = {
  number: "a number column",
  time: "a date column",
  text: "a text column",
  bool: "a yes-or-no column",
}

/**
 * A picked column the field it sits in does not take, and what to pick; null when each
 * fits. A pick stays when the type or the aggregate changes, and each picker leaves
 * enabled the column it holds, so a column the chart cannot read is named here rather
 * than sent for the engine to refuse in its own words. It checks what toSettings sends,
 * by the rule each picker offers by.
 */
export function wrongColumn(form: ChartForm, columns: readonly ChartSourceColumn[]): string | null {
  const t = form.type
  const s = toSettings(form)
  const points = isPoints(t)
  // The words a field asks with when it is empty, else these.
  const ask = (empty: Partial<ChartForm>, otherwise: string) => missingField({ ...form, ...empty }) ?? otherwise
  const MEASURE_KEYS = ["measureColumn", "measureColumn2", "measureColumn3"] as const
  const x = one(s, "x")
  const xTakes = points ? numberColumns : xKinds(t).includes("time") ? dateColumns(t === "calendar" || !!x?.grain) : xColumns(t)
  const checks: { column?: string; takes?: Takes; ask: () => string; needs?: string; drawn?: string }[] = [
    { column: x?.column, takes: xTakes, ask: () => ask({ x: "" }, "Pick another column for the x axis.") },
    ...(points || t === "boxplot" ? [{ column: one(s, "y")?.column, takes: numberColumns, ask: () => ask({ y: "" }, "Pick another column for the y axis.") }] : []),
    { column: one(s, "size")?.column, takes: numberColumns, ask: () => ask({ size: "" }, "Pick a number column for the size, or none.") },
    ...(points ? [{ column: one(s, "series")?.column, takes: colourColumns, ask: () => "Pick a text or yes-or-no column for the colour, or none." }] : []),
    ...(t === "candlestick"
      ? all(s, "y").map((f, i) => ({ column: f.column, takes: numberColumns, ask: () => `Pick a number column for the ${PRICE_FIELDS[i] ? `${PRICE_FIELDS[i].toLowerCase()} ` : ""}price.` }))
      : []),
    { column: one(s, "date")?.column, takes: dateColumns(false), ask: () => "Pick a date column to compare by, or none." },
    ...all(s, "measure").map((m, i) => ({
      column: m.column,
      takes: measureColumns(m.aggregate ?? "", t),
      ask: () => ask({ [MEASURE_KEYS[i] ?? "measureColumn"]: "" }, "Pick another column to measure."),
      needs: m.aggregate === "sum" ? "a sum" : m.aggregate === "avg" ? "an average" : undefined,
      drawn: drawnAsNumber(m.aggregate ?? "", t) ? AGGREGATE_LABEL[m.aggregate as ChartAggregate].toLowerCase() : undefined,
    })),
  ]
  for (const check of checks) {
    const c = check.column ? columns.find((col) => col.name === check.column) : undefined
    if (!c || !check.takes || check.takes(c)) continue
    if (c.masked) return `${c.name} is masked, so its values are not read. ${check.ask()}`
    // A column the source read no kind for (its first value null, say) passes, as the
    // server lets it: the run says what it holds.
    const kind = KIND_NOUN[c.kind]
    if (!kind) continue
    if (check.needs) return `${c.name} is ${kind}, and ${check.needs} needs numbers. ${check.ask()}`
    if (check.drawn)
      return `${c.name} is ${kind}, and this chart draws the ${check.drawn} only of numbers: a table or a pivot table shows it as it is. ${check.ask()}`
    return `${c.name} is ${kind}. ${check.ask()}`
  }
  return null
}
