/**
 * A saved chart's settings (as the server checks them, dashboards-design.md § 3)
 * and a drawn result, turned into what the page shows: an ECharts chart, a
 * scorecard, a table or a message. The server names each result column by the
 * part of the chart it is (its role), so nothing here guesses from a column name.
 * No ECharts import: this file is safe anywhere.
 *
 * Series (§ 2.1, § 14.3): at most the type's cap (7) are drawn. The query keeps the
 * largest and folds the rest into Other when the measure adds up (count, sum) or keeps
 * its extreme (min, max), flags each Other row and gives the number of series; an
 * average or a distinct count cannot be folded, so the rest are left out and the chart
 * says how many. A series takes the colour its chart pins to it, else the next free
 * token in name order (charts/settings.go Series), so every viewer sees the same colours.
 */

import { SERIES_COLORS, type ChartColor } from "@/components/charts/chart-theme"
import { binLabel, formatNumber, type ChartSeries, type ChartSettings, type NumberFormat } from "@/components/charts/chart-settings"
import { chartSpec, seriesCap } from "@/components/charts/chart-registry"
import { regionOf } from "@/components/charts/map-regions"
import { formatAbsoluteTime } from "@/lib/utils"
import {
  MAX_CHART_ROWS,
  one,
  all,
  type ChartAggregate,
  type ChartDraw,
  type ChartField,
  type ChartGrain,
  type ChartResult,
  type ServerChartSettings,
} from "@/lib/api/charts"

export const ROLE_X = "the x"
export const ROLE_Y = "the y"
export const ROLE_SERIES = "the series"
export const ROLE_MEASURE = "the measure"
export const ROLE_PREVIOUS = "the earlier period"
/** A combo's second measure (charts.RoleSecondMeasure), and a histogram's bin width on each row (charts.RoleBinWidth). */
export const ROLE_SECOND_MEASURE = "the second measure"
export const ROLE_BIN_WIDTH = "the bin width"
/** A pivot table's third measure (charts.RoleThirdMeasure), and a scatter's point size (charts.RolePointSize). */
export const ROLE_THIRD_MEASURE = "the third measure"
export const ROLE_SIZE = "the size"
/** How many source rows are at a scatter's point (charts.RolePointRows): the server draws them as one. */
export const ROLE_POINT_ROWS = "the point's rows"
/** A pivot table's nth row and column field (from 1), and the flag on a row where it is totalled (charts.PivotRowRole …). */
export const pivotRole = (part: "row" | "column", n: number) => `${part} field ${n}`
export const pivotTotalRole = (part: "row" | "column", n: number) => `${part} field ${n} total`
/** A series chart's flag on each row folded into Other (1), and its number of series. */
export const ROLE_OTHER = "the other series"
export const ROLE_SERIES_COUNT = "the series count"
export const tableRole = (column: string) => `table column ${column}`

const EMPTY = "(empty)"
const MIDNIGHT = /^(\d{4}-\d{2}-\d{2})[T ]00:00:00(\.0+)?(Z|[+-]00(:?00)?)?$/
const NUMERIC = /^-?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/

/** A number, or a database's numeric text (PostgreSQL's NUMERIC arrives as text); null otherwise. */
export function toNumber(v: unknown): number | null {
  if (typeof v === "number") return Number.isFinite(v) ? v : null
  if (typeof v === "string" && NUMERIC.test(v.trim())) {
    const n = Number(v)
    return Number.isFinite(n) ? n : null
  }
  return null
}

/** A value as a category or a series name: a midnight timestamp is its date, null is "(empty)". */
export function labelOf(v: unknown): string {
  if (v === null || v === undefined) return EMPTY
  if (typeof v === "string") {
    const m = MIDNIGHT.exec(v)
    return m ? m[1] : v
  }
  if (typeof v === "object") return JSON.stringify(v)
  return String(v)
}

const DAY = /^(\d{4})-(\d{2})-(\d{2})/
/** A label that is a day and nothing else: a DATE, or a time at midnight (labelOf). */
const PLAIN_DAY = /^\d{4}-\d{2}-\d{2}$/
/** How many days apart two "YYYY-MM-DD" days are. */
const daysApart = (a: string, b: string) => Math.abs(Date.parse(`${a}T00:00:00Z`) - Date.parse(`${b}T00:00:00Z`)) / 86_400_000
const QUARTER_STARTS = ["01", "04", "07", "10"]

/**
 * A grouped date as its period (§ 14.3): a month as "2026-03", a calendar quarter as
 * "2026 Q2", a calendar year as "2026"; a week, a day, or a quarter or year starting in
 * another month (a fiscal one) as the day it starts. Any other value is labelOf's.
 */
export function grainLabel(v: unknown, grain: ChartGrain | undefined): string {
  const m = grain && typeof v === "string" ? DAY.exec(v) : null
  if (!m) return labelOf(v)
  const [, year, month, day] = m
  switch (grain) {
    case "month":
      return `${year}-${month}`
    case "quarter":
      return day === "01" && QUARTER_STARTS.includes(month) ? `${year} Q${QUARTER_STARTS.indexOf(month) + 1}` : m[0]
    case "year":
      return month === "01" && day === "01" ? year : m[0]
    default:
      return m[0]
  }
}

const AGGREGATE_TITLE: Record<ChartAggregate, (column: string) => string> = {
  count: (c) => (c ? `Count of ${c}` : "Count"),
  count_distinct: (c) => `Distinct ${c}`,
  sum: (c) => `Sum of ${c}`,
  avg: (c) => `Average ${c}`,
  min: (c) => `Lowest ${c}`,
  max: (c) => `Highest ${c}`,
}

/** The measure's name on the chart: its title, else what it counts or adds up; a histogram counts its rows. */
export function measureTitle(s: ServerChartSettings): string {
  return s.type === "histogram" ? "Count" : fieldTitle(one(s, "measure"))
}

/** How the measure is written: its format, else whole numbers for a count and one decimal otherwise. */
export function measureFormat(s: ServerChartSettings): NumberFormat {
  return s.type === "histogram" ? "integer" : fieldFormat(one(s, "measure"))
}

/** One measure's name: its title, else what it counts or adds up. */
function fieldTitle(m: ChartField | undefined): string {
  if (!m) return ""
  return m.title?.trim() || AGGREGATE_TITLE[m.aggregate ?? "count"](m.column ?? "")
}

function fieldFormat(m: ChartField | undefined): NumberFormat {
  if (m?.format) return m.format
  return m?.aggregate === "count" || m?.aggregate === "count_distinct" ? "integer" : "decimal"
}

const axisTitle = (a: ChartField | undefined) => a?.title?.trim() || a?.column || ""

const byName = (a: string, b: string) => a.localeCompare(b, undefined, { numeric: true })

/**
 * The tokens of the drawn series, by name: a pinned series keeps its token; the rest
 * take the next token no pin holds, in name order, then any token no drawn series has.
 */
export function assignSeriesColors(
  names: readonly string[],
  pins: readonly { value: string; color: string }[] = [],
): Map<string, ChartColor> {
  const pinned = new Map<string, ChartColor>()
  for (const p of pins) {
    if ((SERIES_COLORS as readonly string[]).includes(p.color)) pinned.set(p.value, p.color as ChartColor)
  }
  const reserved = new Set<ChartColor>(pinned.values())
  const out = new Map<string, ChartColor>()
  const used = new Set<ChartColor>()
  for (const n of names) {
    const c = pinned.get(n)
    if (c && !used.has(c)) {
      out.set(n, c)
      used.add(c)
    }
  }
  for (const n of [...names].sort(byName)) {
    if (out.has(n)) continue
    const c =
      SERIES_COLORS.find((t) => !used.has(t) && !reserved.has(t)) ?? SERIES_COLORS.find((t) => !used.has(t))
    if (!c) break
    out.set(n, c)
    used.add(c)
  }
  return out
}

export interface SavedChartSeries {
  /** The series' name, as a pin names it. */
  value: string
  color: ChartColor
}

export type SavedChartView =
  | {
      kind: "echart"
      settings: ChartSettings
      rows: object[]
      /** The drawn series and their tokens, to pin on save; empty without a series column. */
      series: SavedChartSeries[]
      notes: string[]
    }
  | {
      kind: "scorecard"
      label: string
      value: number | null
      format: NumberFormat
      /** With a compare: the earlier period's number and its length in days. */
      previous?: number | null
      days?: number
      /** With a sparkline: the current period's points, oldest first, a period with no value null. */
      spark?: { date: string; value: number | null }[]
      /** What its author wrote it shows, read to a screen reader; none when they wrote none. */
      description?: string
      notes: string[]
    }
  | {
      kind: "table"
      head: string[]
      body: string[][]
      numeric: boolean[]
      /** A pivot table's rows that are totals, to set apart; none on a table chart. */
      totals?: boolean[]
      /** What its author wrote it shows, read to a screen reader; none when they wrote none. */
      description?: string
      notes: string[]
    }
  | { kind: "message"; message: string }

/** Where each role's values are in the rows; -1 when the result has no such column. */
function rolesOf(result: ChartResult): Map<string, number> {
  const roles = new Map<string, number>()
  result.columns.forEach((c, i) => roles.set(c.role, i))
  return roles
}

const isMasked = (result: ChartResult, i: number | undefined) =>
  i !== undefined && i >= 0 && result.masked.includes(result.columns[i]?.alias)

const MASKED = "This chart's measure is masked for you, so it is not drawn."

/** The measures whose series past the cap the query adds into Other (charts.FoldsIntoOther). */
const foldsIntoOther = (aggregate: ChartAggregate | undefined) =>
  aggregate === "count" || aggregate === "sum" || aggregate === "min" || aggregate === "max"

/**
 * A scorecard or a table with the description its author wrote. A plot carries it in its
 * settings, with one made up when none was written; these read as text, so need none.
 */
function described(s: ServerChartSettings, view: SavedChartView): SavedChartView {
  const description = s.description?.trim()
  return description && (view.kind === "scorecard" || view.kind === "table") ? { ...view, description } : view
}

/** The page's view of a drawn chart. */
export function savedChartView(s: ServerChartSettings, result: ChartResult): SavedChartView {
  const roles = rolesOf(result)
  switch (s.type) {
    case "scorecard":
      return described(s, scorecardView(s, result, roles))
    case "table":
      return described(s, tableView(s, result))
    case "pivot":
      return described(s, pivotView(s, result))
    case "scatter":
    case "bubble":
      return scatterView(s, result, roles)
    case "heatmap":
      return heatmapView(s, result, roles)
    case "pie":
    case "donut":
      return pieView(s, result, roles)
    case "histogram":
      return histogramView(s, result, roles)
    case "treemap":
    case "sunburst":
      return partsView(s, result, roles)
    case "sankey":
      return sankeyView(s, result, roles)
    case "calendar":
      return calendarView(s, result, roles)
    case "bullet":
      return bulletView(s, result, roles)
    case "boxplot":
      return boxplotView(s, result, roles)
    case "candlestick":
      return candlestickView(s, result, roles)
    case "range":
      return rangeView(s, result, roles)
    case "map":
      return mapView(s, result, roles)
    default:
      return xyView(s, result, roles)
  }
}

function scorecardView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const mi = roles.get(ROLE_MEASURE)
  if (mi === undefined) return { kind: "message", message: "The result has no number to show." }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const row = result.rows[0] ?? []
  const value = toNumber(row[mi])
  const label = measureTitle(s)
  const format = measureFormat(s)
  if (!s.compare) return { kind: "scorecard", label, value, format, notes: [] }
  const pi = roles.get(ROLE_PREVIOUS)
  // The server sends no sparkline when a column it reads is masked.
  const spark = s.sparkline && result.spark ? sparkPoints(result.spark, s.compare.days) : undefined
  return {
    kind: "scorecard",
    label,
    value,
    format,
    previous: pi === undefined ? null : toNumber(row[pi]),
    days: s.compare.days,
    ...(spark ? { spark } : {}),
    notes: [],
  }
}

/** A table chart's columns: each named column, then the measure. */
function tableColumns(s: ServerChartSettings, roles: Map<string, number>) {
  const cols = all(s, "columns").map(({ column = "" }) => ({ head: column, index: roles.get(tableRole(column)), measure: false }))
  if (one(s, "measure")) cols.push({ head: measureTitle(s), index: roles.get(ROLE_MEASURE), measure: true })
  return cols
}

/**
 * A table chart's cells; `number` writes the measure (the masked one is left as the
 * server sent it, a lowest or a highest of dates or text as `text` writes it) and `text`
 * every other value. A measure is flagged a number only while it holds nothing else, so
 * a download defuses its text like any other (csv.ts).
 */
function tableCells(
  s: ServerChartSettings,
  result: ChartResult,
  number: (n: number) => string,
  text: (v: unknown) => string = labelOf,
): { head: string[]; body: string[][]; numeric: boolean[] } {
  const cols = tableColumns(s, rolesOf(result))
  const body = result.rows.map((row) =>
    cols.map((c) => {
      if (c.index === undefined) return ""
      const v = row[c.index]
      if (v === null || v === undefined) return ""
      const n = c.measure && !isMasked(result, c.index) ? toNumber(v) : null
      return n === null ? text(v) : number(n)
    }),
  )
  return {
    head: cols.map((c) => c.head),
    body,
    numeric: cols.map((c) => c.measure && (c.index === undefined || isMasked(result, c.index) || onlyNumbers(result, c.index))),
  }
}

/** True when every value a column holds is a number (an empty column's too). */
function onlyNumbers(result: ChartResult, index: number): boolean {
  return result.rows.every((row) => row[index] === null || row[index] === undefined || toNumber(row[index]) !== null)
}

const TIMESTAMP = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2})(:\d{2})?(\.\d+)?(Z|[+-]\d{2}(:?\d{2})?)?$/

/**
 * A value as a table cell (on screen, in the PDF and in the email): a timestamp reads
 * as its day and time (seconds only when there are some, "UTC" for a zero offset)
 * rather than as ISO text.
 */
function cellText(v: unknown): string {
  if (typeof v !== "string") return labelOf(v)
  const m = TIMESTAMP.exec(v)
  if (!m || MIDNIGHT.test(v)) return labelOf(v)
  const seconds = m[3] && m[3] !== ":00" ? m[3] : ""
  const zone = !m[5] ? "" : /^(Z|\+00(:?00)?)$/.test(m[5]) ? " UTC" : ` ${m[5]}`
  return `${m[1]} ${m[2]}${seconds}${zone}`
}

/** True when a column holds at least one value and every value it holds is a number. */
function isNumberColumn(result: ChartResult, index: number): boolean {
  let any = false
  for (const row of result.rows) {
    const v = row[index]
    if (v === null || v === undefined) continue
    if (toNumber(v) === null) return false
    any = true
  }
  return any
}

function tableView(s: ServerChartSettings, result: ChartResult): SavedChartView {
  const format = measureFormat(s)
  const cells = tableCells(s, result, (n) => formatNumber(n, format), cellText)
  // A named column of numbers lines up on the right like the measure (on screen, in the
  // PDF and in the email); a CSV keeps flagging only the measure (csv.ts defuses every other cell).
  const roles = rolesOf(result)
  const numeric = tableColumns(s, roles).map(
    (c, i) => cells.numeric[i] || (c.index !== undefined && isNumberColumn(result, c.index)),
  )
  return { kind: "table", ...cells, numeric, notes: result.unordered ? [UNORDERED] : [] }
}

/** A table drawn without the order of each column (the gateway's chartUnorderedNote). */
const UNORDERED = "Rows may come in a different order each time: the table could not be sorted by all its columns."

/**
 * A table chart's rows as a download takes them (D3): the columns and rows the table
 * shows, with the measure as its whole number rather than the screen's rounding.
 */
export function tableExport(s: ServerChartSettings, result: ChartResult): { head: string[]; body: string[][]; numeric: boolean[] } {
  if (s.type === "pivot") {
    const cells = pivotCells(s, result, () => (n) => String(n))
    return "message" in cells ? { head: [], body: [], numeric: [] } : { head: cells.head, body: cells.body, numeric: cells.numeric }
  }
  return tableCells(s, result, (n) => String(n))
}

const PIVOT_MEASURES = [ROLE_MEASURE, ROLE_SECOND_MEASURE, ROLE_THIRD_MEASURE]
const TOTAL = "Total"
/** Two values' join in a key: a character no label holds. */
const SEP = "\u0001"

/** A pivot table's value past the row cap, apart from a blank, which has no value. */
const PAST = "…"

/**
 * A pivot table's result as a cross-tab (§ 14.8). The server sends a row per cell and
 * per total (GROUPING SETS, charts.PivotRowRole …): each row says which fields are
 * totalled on it, so a row's level is how many of its leading row fields, and of its
 * leading column fields, it holds; its other fields' values are not the field's (on
 * ClickHouse they are the type's default) and are not read. Every total is the
 * source's own, never a sum of the cells shown: an average's total is the average
 * over its rows.
 *
 * The rows are the row fields' values, in the server's order, each group followed by
 * its subtotal and the whole table by its total; the columns are the column fields'
 * values (each first field's group followed by its subtotal) and Total, each by the
 * measures, or only the measures without column fields. `number` writes measure k.
 */
function pivotCells(
  s: ServerChartSettings,
  result: ChartResult,
  number: (k: number) => (n: number) => string,
): { head: string[]; body: string[][]; numeric: boolean[]; totals: boolean[]; past: boolean } | { message: string } {
  const roles = rolesOf(result)
  const rowFields = all(s, "rows")
  const colFields = all(s, "columns")
  const measures = all(s, "measure")
  const part = (fields: ChartField[], name: "row" | "column") =>
    fields.map((f, n) => ({ field: f, index: roles.get(pivotRole(name, n + 1)), total: roles.get(pivotTotalRole(name, n + 1)) }))
  const rowParts = part(rowFields, "row")
  const colParts = part(colFields, "column")
  const measureIndex = measures.map((_, k) => roles.get(PIVOT_MEASURES[k]))
  if (
    rowFields.length === 0 ||
    measures.length === 0 ||
    [...rowParts, ...colParts].some((p) => p.index === undefined || p.total === undefined) ||
    measureIndex.some((i) => i === undefined)
  ) {
    return { message: "The result is missing a column the chart needs." }
  }

  // A row's held values: its leading fields up to the first totalled one.
  const held = (row: unknown[], parts: typeof rowParts) => {
    const values: string[] = []
    for (const p of parts) {
      if (toNumber(row[p.total!]) === 1) break
      values.push(grainLabel(row[p.index!], p.field.grain))
    }
    return values
  }
  const keyOf = (values: string[]) => `${values.length}${SEP}${values.join(SEP)}`
  const cells = new Map<string, unknown[]>()
  const rowKeys: string[][] = []
  const colKeys: string[][] = []
  const seenRows = new Set<string>()
  const seenCols = new Set<string>()
  for (const row of result.rows) {
    const r = held(row, rowParts)
    const c = held(row, colParts)
    cells.set(`${keyOf(r)}#${keyOf(c)}`, measureIndex.map((i) => row[i!]))
    if (r.length === rowParts.length && !seenRows.has(keyOf(r))) {
      seenRows.add(keyOf(r))
      rowKeys.push(r)
    }
    if (c.length === colParts.length && !seenCols.has(keyOf(c))) {
      seenCols.add(keyOf(c))
      colKeys.push(c)
    }
  }

  // Each group's members together, the groups in the order their first member came.
  const grouped = (keys: string[][]) => {
    const first = new Map<string, number>()
    keys.forEach((k, i) => {
      for (let l = 1; l < k.length; l++) if (!first.has(keyOf(k.slice(0, l)))) first.set(keyOf(k.slice(0, l)), i)
    })
    const rank = (k: string[]) => Array.from({ length: k.length - 1 }, (_, l) => first.get(keyOf(k.slice(0, l + 1)))!)
    return keys
      .map((k, i) => ({ k, r: [...rank(k), i] }))
      .sort((a, b) => {
        for (let l = 0; l < a.r.length; l++) if (a.r[l] !== b.r[l]) return a.r[l] - b.r[l]
        return 0
      })
      .map((x) => x.k)
  }
  // The keys with each group's subtotal after it, deepest first, and the whole's last.
  const withTotals = (keys: string[][], depth: number) => {
    const out: { values: string[]; total: boolean }[] = []
    keys.forEach((k, i) => {
      out.push({ values: k, total: false })
      const next = keys[i + 1]
      for (let l = depth - 1; l >= 1; l--) {
        const prefix = k.slice(0, l)
        if (!next || keyOf(next.slice(0, l)) !== keyOf(prefix)) out.push({ values: prefix, total: true })
      }
    })
    out.push({ values: [], total: true })
    return out
  }
  const lines = withTotals(grouped(rowKeys), rowParts.length)
  const groups = colParts.length === 0 ? [{ values: [], total: false }] : withTotals(grouped(colKeys), colParts.length)

  // A value past the row cap is marked, not left blank like one the source has no rows
  // for. The server orders the rows by their grouping sets, the fewer fields held first
  // and, as many held, the fewer row fields first, then by the fields (pivotSQL): each
  // set before the last row's is whole and each after it is past the cap. In the last
  // row's set, a row group it came with is before the cut and one it did not come with
  // is past it; in the last row's own group, a column group's place is its place in the
  // column fields' set as deep, which comes earlier and is whole.
  const last = result.truncated ? result.rows[result.rows.length - 1] : undefined
  const isPast = ((): ((r: string[], c: string[]) => boolean) => {
    if (!last) return () => false
    const lastRow = held(last, rowParts)
    const lastCol = held(last, colParts)
    const came = new Set<string>()
    const place = new Map<string, number>()
    for (const row of result.rows) {
      const r = held(row, rowParts)
      const c = held(row, colParts)
      if (r.length === lastRow.length && c.length === lastCol.length) came.add(keyOf(r))
      if (r.length === 0 && c.length === lastCol.length && !place.has(keyOf(c))) place.set(keyOf(c), place.size)
    }
    return (r, c) => {
      if (r.length + c.length !== lastRow.length + lastCol.length) return r.length + c.length > lastRow.length + lastCol.length
      if (r.length !== lastRow.length) return r.length > lastRow.length
      if (keyOf(r) !== keyOf(lastRow)) return !came.has(keyOf(r))
      return (place.get(keyOf(c)) ?? Infinity) > place.get(keyOf(lastCol))!
    }
  })()
  let past = false

  const titles = measures.map((m) => fieldTitle(m))
  const head = [
    ...rowFields.map((f) => axisTitle(f)),
    ...groups.flatMap((g) => {
      if (colParts.length === 0) return titles
      const name = g.values.length === 0 ? TOTAL : [...g.values, ...(g.total ? [TOTAL] : [])].join(" · ")
      return titles.map((t) => `${name} · ${t}`)
    }),
  ]
  const writers = measures.map((_, k) => number(k))
  const measureNumbers = measureIndex.map((i) => isMasked(result, i!) || onlyNumbers(result, i!))
  const body = lines.map((line) => {
    const names = rowFields.map((_, n) =>
      n < line.values.length ? line.values[n] : line.total && n === line.values.length ? TOTAL : "",
    )
    const values = groups.flatMap((g) => {
      const v = cells.get(`${keyOf(line.values)}#${keyOf(g.values)}`)
      if (!v && isPast(line.values, g.values)) {
        past = true
        return measures.map(() => PAST)
      }
      return measures.map((_, k) => {
        const x = v?.[k]
        if (x === null || x === undefined) return ""
        // A masked measure as the server sent it; a lowest or a highest of dates or text as the column holds it.
        const n = isMasked(result, measureIndex[k]) ? null : toNumber(x)
        return n === null ? labelOf(x) : writers[k](n)
      })
    })
    return [...names, ...values]
  })
  return {
    head,
    body,
    // A measure's columns are flagged numbers only while it holds nothing else (tableCells).
    numeric: head.map(
      (_, i) => i >= rowFields.length && measureNumbers[(i - rowFields.length) % measures.length],
    ),
    totals: lines.map((l) => l.total),
    past,
  }
}

function pivotView(s: ServerChartSettings, result: ChartResult): SavedChartView {
  const measures = all(s, "measure")
  const cells = pivotCells(s, result, (k) => {
    const format = fieldFormat(measures[k])
    return (n) => formatNumber(n, format)
  })
  if ("message" in cells) return { kind: "message", message: cells.message }
  const { past, ...table } = cells
  // The server orders the totals before the cells, so a cut falls on cells first; but
  // the totals alone can reach the cap, and then no row is a cell (no field totalled).
  const roles = rolesOf(result)
  const flags = [
    ...all(s, "rows").map((_, n) => roles.get(pivotTotalRole("row", n + 1))!),
    ...all(s, "columns").map((_, n) => roles.get(pivotTotalRole("column", n + 1))!),
  ]
  const anyCell = result.rows.some((row) => flags.every((i) => toNumber(row[i]) !== 1))
  const notes = !result.truncated
    ? []
    : anyCell
      ? ["Each total counts every row of the source; it is cells past the cap that are not shown."]
      : ["The totals alone reach the cap, so no cells are shown and a total may be missing. Each total shown counts every row of the source."]
  if (past) notes.push(`A ${PAST} is past the cap and not shown; a blank has no value.`)
  return { kind: "table", ...table, notes }
}

/**
 * A scatter's or a bubble chart's points, coloured by a field's values when it has
 * one (the top 7 by rows, the rest Other); a row with no x or y, or a size not above
 * zero, has no point, and the chart says how many rows had no size. The server sends
 * the rows at one x, y, size and colour as one row with their count (ROLE_POINT_ROWS);
 * a result without the count is a point a row.
 */
function scatterView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xAxis = one(s, "x")
  const yAxis = one(s, "y")
  const sizeField = one(s, "size")
  const colour = one(s, "series")
  const xi = roles.get(ROLE_X)
  const yi = roles.get(ROLE_Y)
  const zi = sizeField ? roles.get(ROLE_SIZE) : undefined
  const si = colour ? roles.get(ROLE_SERIES) : undefined
  if (xi === undefined || yi === undefined || (sizeField && zi === undefined) || (colour && si === undefined)) {
    return { kind: "message", message: "The result is missing a column the chart needs." }
  }
  if (isMasked(result, xi) || isMasked(result, yi) || isMasked(result, zi)) {
    return { kind: "message", message: "A column this chart places its points by is masked for you, so it is not drawn." }
  }
  const oi = si === undefined ? undefined : roles.get(ROLE_OTHER)
  const ni = roles.get(ROLE_POINT_ROWS)
  let unsized = 0
  const points = result.rows.flatMap((row) => {
    const x = toNumber(row[xi])
    const y = toNumber(row[yi])
    if (x === null || y === null) return []
    const n = ni === undefined ? 1 : (toNumber(row[ni]) ?? 1)
    const size = zi === undefined ? null : toNumber(row[zi])
    if (zi !== undefined && (size === null || size <= 0)) {
      unsized += n
      return []
    }
    return [{ x, y, size, n, s: si === undefined ? "" : labelOf(row[si]), other: oi !== undefined && toNumber(row[oi]) === 1 }]
  })
  // The largest drawn first, so a small bubble is on top of a large one, not under it.
  if (zi !== undefined) points.sort((a, b) => b.size! - a.size!)

  const xLabel = axisTitle(xAxis)
  const yLabel = axisTitle(yAxis)
  const sizeLabel = axisTitle(sizeField)
  const notes: string[] = []
  if (unsized > 0) {
    // The server keeps only sized points under its cap; a result that holds unsized rows
    // counts only the rows it read, never the source's.
    const rowsText = unsized.toLocaleString("en-US")
    const past = result.truncated ? " Past the cap, the source may have more." : ""
    notes.push(unsized === 1 ? `1 row has no size above zero, so it is not drawn.${past}` : `${rowsText} rows have no size above zero, so they are not drawn.${past}`)
  }
  let values: ChartSeries[]
  let series: SavedChartSeries[] = []
  let rows: object[]
  // A point's place, its size only on a chart sized by one, and its rows when counted.
  const at = (p: (typeof points)[number]) => ({
    x: p.x,
    y: p.y,
    ...(zi === undefined ? {} : { size: p.size }),
    ...(ni === undefined ? {} : { n: p.n }),
  })
  if (si === undefined) {
    values = [{ key: "", label: yLabel, color: "series-1" }]
    rows = points.map(at)
  } else {
    // A colour's rows are counted, as the server ranks the colours (foldPointsSQL).
    const counts = new Map<string, number>()
    for (const p of points) if (!p.other) counts.set(p.s, (counts.get(p.s) ?? 0) + p.n)
    const kept = [...counts.keys()]
    const ci = roles.get(ROLE_SERIES_COUNT)
    const count = ci === undefined ? null : toNumber(result.rows[0]?.[ci])
    const rest = count === null ? 0 : count - seriesCap(s.type)
    // Other is the points of the colours past the cap, so it is a count and always folds.
    const note = otherNote(s.type, "count", rest)
    if (note) notes.push(note)
    if (rest > 0) kept.sort((a, b) => counts.get(b)! - counts.get(a)! || byName(a, b))
    const colors = assignSeriesColors(kept, colour?.colors)
    const drawn = [...kept].sort(byName)
    const keyOf = new Map(drawn.map((n, i) => [n, `s${i}`]))
    values = drawn.map((n) => ({ key: keyOf.get(n)!, label: n, color: colors.get(n) ?? "series-1" }))
    series = drawn.map((n) => ({ value: n, color: colors.get(n) ?? "series-1" }))
    if (points.some((p) => p.other)) values.push({ key: "other", label: "Other", color: "other" })
    rows = points.map((p) => ({ ...at(p), s: p.other ? "other" : keyOf.get(p.s) }))
  }
  const format: NumberFormat = "decimal"
  const sized = sizeField ? `, sized by ${sizeLabel}` : ""
  const coloured = colour ? `, coloured by ${colour.column}` : ""
  return {
    kind: "echart",
    settings: {
      type: s.type === "bubble" ? "bubble" : "scatter",
      description: s.description?.trim() || `${yLabel} against ${xLabel}${sized}${coloured}`,
      points: {
        x: { field: "x", label: xLabel, format },
        y: { field: "y", label: yLabel, format },
        ...(sizeField ? { size: { field: "size", label: sizeLabel, format } } : {}),
        ...(ni === undefined ? {} : { rows: { field: "n", label: "Rows", format: "integer" as NumberFormat } }),
      },
      series: si === undefined ? { values } : { field: "s", label: colour?.column ?? "", values },
    },
    rows,
    series,
    notes,
  }
}

function heatmapView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xi = roles.get(ROLE_X)
  const yi = roles.get(ROLE_Y)
  const mi = roles.get(ROLE_MEASURE)
  if (xi === undefined || yi === undefined || mi === undefined) {
    return { kind: "message", message: "The result is missing a column the chart needs." }
  }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const xAxis = one(s, "x")
  const yAxis = one(s, "y")
  const xs: string[] = []
  const ys = new Set<string>()
  const seenX = new Set<string>()
  const rows = result.rows.map((row) => {
    const x = grainLabel(row[xi], xAxis?.grain)
    const y = grainLabel(row[yi], yAxis?.grain)
    if (!seenX.has(x)) {
      seenX.add(x)
      xs.push(x)
    }
    ys.add(y)
    return { x, y, v: toNumber(row[mi]) }
  })
  const title = measureTitle(s)
  const xLabel = axisTitle(xAxis)
  const yLabel = axisTitle(yAxis)
  const color = measureColor(s)
  // The rows come by x, then y (testdata/chart_sql/heatmap.golden): the cap cuts inside the last x
  // drawn, where a blank cell may be past the cap rather than one with no rows.
  const lastX = result.truncated ? xs[xs.length - 1] : undefined
  return {
    kind: "echart",
    settings: {
      type: "heatmap",
      description: s.description?.trim() || `${title} by ${xLabel} and ${yLabel}`,
      x: { field: "x", label: xLabel, categories: xs },
      y: { field: "y", label: yLabel, categories: [...ys].sort(byName) },
      value: { field: "v", label: title, format: measureFormat(s) },
      color,
    },
    rows,
    series: [],
    notes: lastX === undefined ? [] : [`The cap cut the rows inside ${lastX}: its blank cells may be past the cap, not empty.`],
  }
}

/** The measure's pinned token for a chart with no series, else the first. */
function measureColor(s: ServerChartSettings): ChartColor {
  const c = one(s, "measure")?.color ?? ""
  return (SERIES_COLORS as readonly string[]).includes(c) ? (c as ChartColor) : "series-1"
}

/**
 * The note under a series chart that has more series than its type draws (the gateway's
 * chartOtherNote says the same in a workflow's message; shared/chart_notes_golden.json
 * holds both to it): the number past the cap, named as the builder names them (a pie's or
 * a donut's are slices, a scatter's or a bubble chart's colours), and whether they are
 * Other or left out.
 */
export function otherNote(type: string, aggregate: ChartAggregate | undefined, rest: number): string | null {
  if (rest <= 0) return null
  const [one, many] =
    type === "pie" || type === "donut" ? ["slice", "slices"] : type === "scatter" || type === "bubble" ? ["colour", "colours"] : ["series", "series"]
  const more = `${rest} more ${rest === 1 ? one : many} ${rest === 1 ? "is" : "are"}`
  if (foldsIntoOther(aggregate)) return `${more} drawn as Other.`
  return `${more} left out: ${aggregate === "avg" ? "an average" : "a distinct count"} cannot be added into Other.`
}

/** A grain's period: a day, a week, or a number of months. */
const GRAIN_STEP: Record<ChartGrain, { months: number; days: number }> = {
  day: { months: 0, days: 1 },
  week: { months: 0, days: 7 },
  month: { months: 1, days: 0 },
  quarter: { months: 3, days: 0 },
  year: { months: 12, days: 0 },
}

/**
 * Every period from the first day given to the last, a grain's period apart, oldest
 * first or newest first as the days are given (each "YYYY-MM-DD", its period's start,
 * as the server groups them). Null when there are no days (a chart with no rows), the
 * days are out of that order, one falls between the periods, or there would be more
 * than `cap`.
 */
function everyPeriod(days: string[], grain: ChartGrain, newestFirst: boolean, cap: number): string[] | null {
  if (days.length === 0) return null
  for (let i = 1; i < days.length; i++) if (newestFirst ? days[i] >= days[i - 1] : days[i] <= days[i - 1]) return null
  const [first, last] = newestFirst ? [days[days.length - 1], days[0]] : [days[0], days[days.length - 1]]
  const [y, m, d] = first.split("-").map(Number)
  const step = GRAIN_STEP[grain]
  const out: string[] = []
  for (let k = 0; ; k++) {
    const day = new Date(Date.UTC(y, m - 1 + k * step.months, d + k * step.days)).toISOString().slice(0, 10)
    if (day > last) break
    if (out.length === cap) return null
    out.push(day)
  }
  const on = new Set(out)
  if (!days.every((day) => on.has(day))) return null
  return newestFirst ? out.reverse() : out
}

/**
 * The dated stretch of an x's categories, from its first date to its last, with every
 * period between them (`dayOf` gives each grouped date's day, its period's start). An x
 * of days with no grain is laid out a day at a time when it is daily, most of its steps a
 * day long; weekly, monthly or scattered dates are kept as they are. An x with no date (a
 * NULL, say) keeps its place before or after the dates. Null when the stretch cannot be
 * laid out a period at a time.
 */
function periodsOf(
  categories: string[],
  dayOf: Map<string, string>,
  grain: ChartGrain | undefined,
  newestFirst: boolean,
  cap: number,
): { from: number; length: number; periods: string[] } | null {
  const from = categories.findIndex((c) => dayOf.has(c))
  const to = categories.findLastIndex((c) => dayOf.has(c))
  const dated = from < 0 ? [] : categories.slice(from, to + 1)
  const steps = dated.slice(1).map((c, i) => (dayOf.has(c) && dayOf.has(dated[i]) ? daysApart(dayOf.get(c)!, dayOf.get(dated[i])!) : 0))
  const daily = steps.length > 0 && steps.filter((d) => d === 1).length * 2 >= steps.length
  const by = grain ?? (daily ? "day" : undefined)
  if (!by || !dated.every((c) => dayOf.has(c))) return null
  const days = everyPeriod(dated.map((c) => dayOf.get(c)!), by, newestFirst, cap)
  return days && { from, length: dated.length, periods: days.map((d) => grainLabel(d, by)) }
}

/** Each x that is a date's day: with a grain, the period's start the server groups it by; with none, an x that is a day. */
function dayOfX(raw: unknown, x: string, grain: ChartGrain | undefined): string | undefined {
  return grain ? (typeof raw === "string" ? DAY.exec(raw)?.[0] : undefined) : PLAIN_DAY.test(x) ? x : undefined
}

/** The period a sparkline's point stands for, as the server picks it from the comparison's days (sparkGrain, chart_sql.go). */
const sparkGrain = (days: number): ChartGrain => (days <= 30 ? "day" : days <= 90 ? "week" : "month")
/** A sparkline's points: a year of days at the most. */
const SPARK_CAP = 400

/**
 * A scorecard's sparkline, a point a period from the first to the last, a period with no
 * rows put back with no value, which draws as a break. Spaced a point apart, a day with
 * no rows drew the days either side of it a day apart. Dates that are not the grain's
 * periods are kept as they come.
 */
function sparkPoints(raw: unknown[][], days: number): { date: string; value: number | null }[] {
  const points = raw.map(([date, v]) => ({ date: labelOf(date), value: toNumber(v) }))
  const starts = points.map((p) => DAY.exec(p.date)?.[0])
  if (starts.some((d) => !d)) return points
  const periods = everyPeriod(starts as string[], sparkGrain(days), false, SPARK_CAP)
  if (!periods) return points
  const byDay = new Map(points.map((p, i) => [starts[i]!, p]))
  return periods.map((d) => byDay.get(d) ?? { date: d, value: null })
}

function xyView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xAxis = one(s, "x")
  const seriesField = one(s, "series")
  const measures = all(s, "measure")
  const xi = roles.get(ROLE_X)
  const mi = roles.get(ROLE_MEASURE)
  const si = seriesField ? roles.get(ROLE_SERIES) : undefined
  // A combo's second measure, which the server allows only without a series.
  const m2i = measures.length > 1 && !seriesField ? roles.get(ROLE_SECOND_MEASURE) : undefined
  if (xi === undefined || mi === undefined || (seriesField && si === undefined) || (measures.length > 1 && m2i === undefined)) {
    return { kind: "message", message: "The result is missing a column the chart needs." }
  }
  if (isMasked(result, mi) || isMasked(result, m2i)) return { kind: "message", message: MASKED }
  // Without the flag (a gateway from before § 14.3), every row is its own series.
  const oi = si === undefined ? undefined : roles.get(ROLE_OTHER)

  const categories: string[] = []
  const seen = new Set<string>()
  // Each grouped date's day, the period's start, to put back the periods with no rows;
  // with no grain, each x that is a day (a DATE, or a time at midnight).
  const dayOf = new Map<string, string>()
  const cells = result.rows.map((row) => {
    const x = grainLabel(row[xi], xAxis?.grain)
    if (!seen.has(x)) {
      seen.add(x)
      categories.push(x)
      const day = dayOfX(row[xi], x, xAxis?.grain)
      if (day) dayOf.set(x, day)
    }
    return {
      x,
      s: si === undefined ? "" : labelOf(row[si]),
      other: oi !== undefined && toNumber(row[oi]) === 1,
      v: toNumber(row[mi]),
      v2: m2i === undefined ? null : toNumber(row[m2i]),
    }
  })

  const title = measureTitle(s)
  const xLabel = axisTitle(xAxis)
  const format = measureFormat(s)
  const notes: string[] = []
  let values: ChartSeries[]
  let rows: object[]
  let series: SavedChartSeries[] = []

  let combo: Combo | undefined
  if (m2i !== undefined) {
    combo = comboOf(measures[0], measures[1])
    values = combo.values
    rows = cells.map((c) => ({ x: c.x, v: c.v, v2: c.v2 }))
  } else if (si === undefined) {
    values = [{ key: "", label: title, color: measureColor(s) }]
    rows = cells.map((c) => ({ x: c.x, v: c.v }))
  } else {
    const aggregate = one(s, "measure")?.aggregate
    const totals = new Map<string, number>()
    for (const c of cells) if (!c.other) totals.set(c.s, (totals.get(c.s) ?? 0) + Math.abs(c.v ?? 0))
    const kept = [...totals.keys()]
    const ci = roles.get(ROLE_SERIES_COUNT)
    const count = ci === undefined ? null : toNumber(result.rows[0]?.[ci])
    const rest = count === null ? 0 : count - seriesCap(s.type)
    const note = otherNote(s.type, aggregate, rest)
    if (note) notes.push(note)
    // A pin two series share goes to the larger one, as when the browser picked them.
    if (rest > 0) kept.sort((a, b) => totals.get(b)! - totals.get(a)! || byName(a, b))
    const colors = assignSeriesColors(kept, seriesField?.colors)
    const drawn = [...kept].sort(byName)
    const keyOf = new Map(drawn.map((n, i) => [n, `s${i}`]))
    values = drawn.map((n) => ({ key: keyOf.get(n)!, label: n, color: colors.get(n) ?? "series-1" }))
    series = drawn.map((n) => ({ value: n, color: colors.get(n) ?? "series-1" }))
    rows = []
    const other: { x: string; v: number }[] = []
    for (const c of cells) {
      if (!c.other) rows.push({ x: c.x, s: keyOf.get(c.s), v: c.v })
      else if (foldsIntoOther(aggregate) && c.v !== null) other.push({ x: c.x, v: c.v })
    }
    if (other.length > 0) {
      values.push({ key: "other", label: "Other", color: "other" })
      for (const o of other) rows.push({ x: o.x, s: "other", v: o.v })
    }
  }

  // A line or an area joins its points across a period with no rows, as if nothing
  // were missing: each period between the first and the last is put back with no value,
  // which draws as a gap. Not on a cut result, in a value's order, or past the row cap.
  if ((s.type === "line" || s.type === "area") && !result.truncated && s.sort !== "value_asc" && s.sort !== "value_desc") {
    const cap = chartSpec(s.type)?.cap.rows ?? MAX_CHART_ROWS
    const filled = periodsOf(categories, dayOf, xAxis?.grain, s.sort === "x_desc", cap)
    const missing = (filled?.periods ?? []).filter((x) => !seen.has(x))
    const blank = (x: string): object[] =>
      m2i !== undefined ? [{ x, v: null, v2: null }] : si === undefined ? [{ x, v: null }] : values.map((v) => ({ x, s: v.key, v: null }))
    const added = missing.flatMap(blank)
    if (filled && added.length > 0 && rows.length + added.length <= cap) {
      categories.splice(filled.from, filled.length, ...filled.periods)
      const place = new Map(categories.map((c, i) => [c, i]))
      rows = [...rows, ...added].sort((a, b) => place.get((a as { x: string }).x)! - place.get((b as { x: string }).x)!)
    }
  }

  // A bar of one measure and no series comes largest first by default (chart_sql.go),
  // unless its x is grouped by a date grain. An x of dates with no grain is read in time
  // order too, oldest first, a row with no date last as an x's order puts it. Not on a cut
  // result, which kept the largest.
  if ((s.type === "bar" || s.type === "hbar") && !s.sort && !xAxis?.grain && si === undefined && m2i === undefined && !result.truncated) {
    const dates = categories.filter((c) => DAY.test(c)).length
    if (dates > 0 && dates + (seen.has(EMPTY) ? 1 : 0) === categories.length) {
      categories.sort((a, b) => (a === EMPTY ? 1 : b === EMPTY ? -1 : a < b ? -1 : a > b ? 1 : 0))
      const place = new Map(categories.map((c, i) => [c, i]))
      rows.sort((a, b) => place.get((a as { x: string }).x)! - place.get((b as { x: string }).x)!)
    }
  }

  const seriesLabel = seriesField ? ` and ${seriesField.column}` : ""
  const base = {
    description: s.description?.trim() || `${combo?.title ?? title} by ${xLabel}${seriesLabel}`,
    x: { field: "x", label: xLabel, categories },
    y: { field: "v", format, title: combo?.left ?? title },
    series: { field: si === undefined ? undefined : "s", values },
    ...(combo?.y2 ? { y2: combo.y2 } : {}),
  }
  const stacked = !!s.stacked
  // The server allows 100% only on a stacked chart.
  const percent = s.percent && stacked ? { percent: true } : {}
  let settings: ChartSettings
  switch (s.type) {
    case "line":
      settings = { ...base, type: "line" }
      break
    case "area":
      settings = { ...base, type: "area", stacked, ...percent }
      break
    case "hbar":
      settings = { ...base, type: "hbar", stacked, ...percent }
      break
    case "waterfall":
      // A Total from rows that were cut would be short: it is left off.
      if (result.truncated) notes.push("No Total bar: the source has more rows than were drawn.")
      settings = { ...base, type: "waterfall", total: !result.truncated }
      break
    case "funnel":
      settings = { ...base, type: "funnel" }
      break
    default:
      settings = { ...base, type: "bar", stacked, ...percent }
  }
  return { kind: "echart", settings, rows, series, notes }
}

interface Combo {
  values: ChartSeries[]
  /** Both measures' names, for the description. */
  title: string
  /** The left axis's title: the measure on it, or both when neither is on the right. */
  left: string
  y2?: { format: NumberFormat; title: string }
}

/**
 * A combo's two measures as series (§ 14.8): each reads its own row field, with its mark,
 * axis and format; the first keeps its pinned token and the second takes its own or the
 * next one, so the two never share a colour.
 */
function comboOf(m1: ChartField, m2: ChartField): Combo {
  const token = (c: string | undefined) => ((SERIES_COLORS as readonly string[]).includes(c ?? "") ? (c as ChartColor) : undefined)
  const c1 = token(m1.color) ?? "series-1"
  const c2 = token(m2.color) !== c1 ? (token(m2.color) ?? SERIES_COLORS.find((t) => t !== c1)!) : SERIES_COLORS.find((t) => t !== c1)!
  const seriesOf = (m: ChartField, key: string, value: string, color: ChartColor): ChartSeries => ({
    key,
    label: fieldTitle(m),
    color,
    value,
    ...(m.mark ? { mark: m.mark } : {}),
    ...(m.axis === "right" ? { axis: "right" as const } : {}),
    format: fieldFormat(m),
  })
  const values = [seriesOf(m1, "", "v", c1), seriesOf(m2, "2", "v2", c2)]
  const right = values.find((v) => v.axis === "right")
  const left = values.filter((v) => v.axis !== "right").map((v) => v.label)
  return {
    values,
    title: `${values[0].label} and ${values[1].label}`,
    left: left.join(" · "),
    ...(right ? { y2: { format: right.format ?? "decimal", title: right.label } } : {}),
  }
}

/**
 * A pie's or a donut's slices in the server's order, largest first and Other last. A
 * slice of zero or below has no size, so it is left out and the chart says how many.
 */
function pieView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const seriesField = one(s, "series")
  const si = roles.get(ROLE_SERIES)
  const mi = roles.get(ROLE_MEASURE)
  if (si === undefined || mi === undefined) return { kind: "message", message: "The result is missing a column the chart needs." }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const oi = roles.get(ROLE_OTHER)
  const slices: { name: string; v: number | null }[] = []
  let other: number | null = null
  for (const row of result.rows) {
    const v = toNumber(row[mi])
    if (oi !== undefined && toNumber(row[oi]) === 1) {
      if (v !== null) other = (other ?? 0) + v
    } else {
      slices.push({ name: labelOf(row[si]), v })
    }
  }
  const kept = slices.filter((sl): sl is { name: string; v: number } => sl.v !== null && sl.v > 0)
  const notes: string[] = []
  const ci = roles.get(ROLE_SERIES_COUNT)
  const count = ci === undefined ? null : toNumber(result.rows[0]?.[ci])
  const note = otherNote(s.type, one(s, "measure")?.aggregate, count === null ? 0 : count - seriesCap(s.type))
  if (note) notes.push(note)
  const left = slices.length - kept.length + (other !== null && other <= 0 ? 1 : 0)
  if (left > 0) notes.push(left === 1 ? "1 slice is zero or below, so it is not drawn." : `${left} slices are zero or below, so they are not drawn.`)

  // The order is the server's: a pin two slices share goes to the larger one.
  const colors = assignSeriesColors(
    kept.map((k) => k.name),
    seriesField?.colors,
  )
  const values: ChartSeries[] = kept.map((k, i) => ({ key: `s${i}`, label: k.name, color: colors.get(k.name) ?? "series-1" }))
  const rows: object[] = kept.map((k, i) => ({ s: `s${i}`, v: k.v }))
  if (other !== null && other > 0) {
    values.push({ key: "other", label: "Other", color: "other" })
    rows.push({ s: "other", v: other })
  }
  const title = measureTitle(s)
  const column = seriesField?.column ?? ""
  return {
    kind: "echart",
    settings: {
      type: s.type === "donut" ? "donut" : "pie",
      description: s.description?.trim() || `${title} by ${column}`,
      slices: { field: "s", label: column, values },
      value: { field: "v", label: title, format: measureFormat(s) },
    },
    rows,
    series: kept.map((k) => ({ value: k.name, color: colors.get(k.name) ?? "series-1" })),
    notes,
  }
}

/**
 * A histogram's bins, least to greatest. The server sends only the bins a row fell in;
 * the rest are drawn empty, so the bars stand where their numbers are. When there would
 * be more bins than a chart draws rows, the empty ones are left out and the chart says so.
 */
function histogramView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xi = roles.get(ROLE_X)
  const wi = roles.get(ROLE_BIN_WIDTH)
  const mi = roles.get(ROLE_MEASURE)
  if (xi === undefined || wi === undefined || mi === undefined) {
    return { kind: "message", message: "The result is missing a column the chart needs." }
  }
  if (isMasked(result, xi) || isMasked(result, wi)) {
    return { kind: "message", message: "This chart's column is masked for you, so it is not drawn." }
  }
  const found = result.rows.flatMap((row) => {
    const lo = toNumber(row[xi])
    const width = toNumber(row[wi])
    return lo === null || width === null ? [] : [{ lo, width, v: toNumber(row[mi]) ?? 0 }]
  })
  const notes: string[] = []
  let bins = found
  const width = found[0]?.width ?? 0
  if (width > 0) {
    const first = found[0].lo
    const span = Math.round((found[found.length - 1].lo - first) / width) + 1
    if (span <= MAX_CHART_ROWS) {
      bins = Array.from({ length: span }, (_, k) => ({ lo: first + k * width, width, v: 0 }))
      for (const b of found) bins[Math.round((b.lo - first) / width)] = b
    } else if (span > found.length) {
      notes.push("Bins no row fell in are left out: there are too many to draw.")
    }
  }
  const categories = bins.map((b) => binLabel(b.lo, b.width))
  const xLabel = axisTitle(one(s, "x"))
  const title = measureTitle(s)
  return {
    kind: "echart",
    settings: {
      type: "histogram",
      description: s.description?.trim() || `${title} by ${xLabel}`,
      x: { field: "x", label: xLabel, categories },
      y: { field: "v", format: measureFormat(s), title },
      series: { values: [{ key: "", label: title, color: "series-1" }] },
      ...(width > 0 ? { bins: { lo: bins.map((b) => b.lo), width } } : {}),
    },
    rows: bins.map((b, i) => ({ x: categories[i], v: b.v })),
    series: [],
    notes,
  }
}

const MISSING = "The result is missing a column the chart needs."
const COLUMN_MASKED = "A column this chart draws is masked for you, so it is not drawn."

/** "A, B and C"; "A, B, C and more" when there are more than the names given. */
function listed(names: string[], more = false): string {
  if (more && names.length > 0) return `${names.join(", ")} and more`
  return names.length < 2 ? (names[0] ?? "") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`
}

/** The categories in the order the rows give them, each once. */
function categoriesOf(labels: string[]): string[] {
  return [...new Set(labels)]
}

/**
 * A treemap or a sunburst (§ 14.8): a part a row, its name at each level and its measure.
 * The 7 largest top-level groups take the series colours, the rest Other's; a part at
 * zero or below has no area, so it is left out and counted.
 */
function partsView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const levels = all(s, "rows")
  const lis = levels.map((_, i) => roles.get(levelRole(i + 1)))
  const mi = roles.get(ROLE_MEASURE)
  if (levels.length === 0 || lis.some((li) => li === undefined) || mi === undefined) return { kind: "message", message: MISSING }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const rows: { path: string[]; value: number }[] = []
  let left = 0
  for (const row of result.rows) {
    const value = toNumber(row[mi])
    if (value === null || value <= 0) left++
    else rows.push({ path: lis.map((li) => labelOf(row[li!])), value })
  }
  const totals = new Map<string, number>()
  for (const r of rows) totals.set(r.path[0], (totals.get(r.path[0]) ?? 0) + r.value)
  const top = [...totals.keys()].sort((a, b) => totals.get(b)! - totals.get(a)! || byName(a, b)).slice(0, SERIES_COLORS.length)
  const colors = assignSeriesColors(top)
  const notes: string[] = []
  if (left > 0) notes.push(left === 1 ? "1 part is zero or below, so it is not drawn." : `${left} parts are zero or below, so they are not drawn.`)
  const title = measureTitle(s)
  const names = levels.map(axisTitle)
  return {
    kind: "echart",
    settings: {
      type: s.type === "sunburst" ? "sunburst" : "treemap",
      description: s.description?.trim() || `${title} by ${listed(names)}`,
      levels: names,
      value: { label: title, format: measureFormat(s) },
      groups: top.map((n) => ({ key: n, label: n, color: colors.get(n) ?? "series-1" })),
      // The cap cut parts: a share or a group's total is of the drawn parts only.
      ...(result.truncated ? { partial: true } : {}),
    },
    rows,
    series: [],
    notes,
  }
}

/** The most names a sankey places: more is a picture no one reads. */
export const MAX_SANKEY_NODES = 100

/**
 * A sankey (§ 14.8): a flow a row, from its x to its y, as wide as its measure. Taken
 * largest first, as the server sorts them; a flow is left out, and counted, when it is
 * zero or below, goes from a name to itself, would close a loop (a sankey's flows only
 * go forward) or would place a name past the 100th.
 */
function sankeyView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xi = roles.get(ROLE_X)
  const yi = roles.get(ROLE_Y)
  const mi = roles.get(ROLE_MEASURE)
  if (xi === undefined || yi === undefined || mi === undefined) return { kind: "message", message: MISSING }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const nodes: string[] = []
  const placed = new Set<string>()
  const next = new Map<string, string[]>()
  // Whether a flow already leads from one name to the other.
  const reaches = (from: string, to: string): boolean => {
    const seen = new Set<string>()
    const stack = [from]
    while (stack.length > 0) {
      const n = stack.pop()!
      if (n === to) return true
      if (seen.has(n)) continue
      seen.add(n)
      stack.push(...(next.get(n) ?? []))
    }
    return false
  }
  const rows: { source: string; target: string; value: number }[] = []
  const left = { none: 0, self: 0, loop: 0, past: 0 }
  for (const row of result.rows) {
    const source = labelOf(row[xi])
    const target = labelOf(row[yi])
    const value = toNumber(row[mi])
    if (value === null || value <= 0) left.none++
    else if (source === target) left.self++
    else if (placed.size + [source, target].filter((n) => !placed.has(n)).length > MAX_SANKEY_NODES) left.past++
    else if (reaches(target, source)) left.loop++
    else {
      for (const n of [source, target]) {
        if (!placed.has(n)) {
          placed.add(n)
          nodes.push(n)
        }
      }
      next.set(source, [...(next.get(source) ?? []), target])
      rows.push({ source, target, value })
    }
  }
  const flows = (n: number, one: string, many: string) => (n === 1 ? `1 flow ${one}` : `${n} flows ${many}`)
  const notes: string[] = []
  if (left.none > 0) notes.push(flows(left.none, "is zero or below, so it is not drawn.", "are zero or below, so they are not drawn."))
  if (left.self > 0) notes.push(flows(left.self, "goes from a name to itself, so it is not drawn.", "go from a name to itself, so they are not drawn."))
  if (left.loop > 0) {
    notes.push(flows(left.loop, "would close a loop, so it is not drawn:", "would close a loop, so they are not drawn:") + " a sankey's flows only go forward.")
  }
  if (left.past > 0) {
    const why = `past the ${MAX_SANKEY_NODES} names a sankey places`
    notes.push(flows(left.past, `reaches ${why}, so it is not drawn.`, `reach ${why}, so they are not drawn.`))
  }
  const title = measureTitle(s)
  const xLabel = axisTitle(one(s, "x"))
  const yLabel = axisTitle(one(s, "y"))
  return {
    kind: "echart",
    settings: {
      type: "sankey",
      description: s.description?.trim() || `${title} from ${xLabel} to ${yLabel}`,
      source: { label: xLabel },
      target: { label: yLabel },
      value: { label: title, format: measureFormat(s) },
      nodes: nodes.map((n, i) => ({ key: n, label: n, color: SERIES_COLORS[i % SERIES_COLORS.length] })),
    },
    rows,
    series: [],
    notes,
  }
}

/** The most days a calendar spans, ending on its latest day (charts.CalendarDays). */
const CALENDAR_DAYS = 366

/**
 * A calendar heatmap (§ 14.8): a day a row, from the first day the rows have to the
 * last, over the latest 366 days at most. The server caps rows, not days: sparse days
 * (a month's first, say) could stretch the calendar over years, each day too thin to see.
 */
function calendarView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xi = roles.get(ROLE_X)
  const mi = roles.get(ROLE_MEASURE)
  if (xi === undefined || mi === undefined) return { kind: "message", message: MISSING }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const days = result.rows.flatMap((row) => {
    const day = typeof row[xi] === "string" ? DAY.exec(row[xi] as string)?.[0] : undefined
    const value = toNumber(row[mi])
    return day && value !== null ? [{ day, value }] : []
  })
  if (days.length === 0) return { kind: "message", message: "The result has no day with a number to show." }
  const last = days.reduce((m, r) => (r.day > m ? r.day : m), days[0].day)
  const end = Date.parse(`${last}T00:00:00Z`)
  const from = Number.isNaN(end) ? "" : new Date(end - (CALENDAR_DAYS - 1) * 86_400_000).toISOString().slice(0, 10)
  const rows = days.filter((r) => r.day >= from)
  const first = rows.reduce((m, r) => (r.day < m ? r.day : m), last)
  const earlier = days.length - rows.length
  const title = measureTitle(s)
  return {
    kind: "echart",
    settings: {
      type: "calendar",
      description: s.description?.trim() || `${title} by day of ${axisTitle(one(s, "x"))}`,
      range: [first, last],
      value: { label: title, format: measureFormat(s) },
      color: measureColor(s),
    },
    rows,
    series: [],
    // A cut result cannot count what it left out: its banner already says the source has more.
    notes:
      earlier > 0 && !result.truncated
        ? [`${earlier} earlier ${earlier === 1 ? "day is" : "days are"} not drawn: the calendar shows the latest ${CALENDAR_DAYS} days.`]
        : [],
  }
}

/** A bullet chart (§ 14.8): one number against its target, a second measure or a typed number. */
function bulletView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const measures = all(s, "measure")
  const mi = roles.get(ROLE_MEASURE)
  const ti = measures.length > 1 ? roles.get(ROLE_SECOND_MEASURE) : undefined
  if (mi === undefined || (measures.length > 1 && ti === undefined)) return { kind: "message", message: MISSING }
  if (isMasked(result, mi) || isMasked(result, ti)) return { kind: "message", message: MASKED }
  const row = result.rows[0]
  const value = row ? toNumber(row[mi]) : null
  if (value === null) return { kind: "message", message: "The result has no number to show." }
  const target = ti !== undefined ? toNumber(row![ti]) : (s.target ?? null)
  const title = measureTitle(s)
  const targetLabel = measures.length > 1 ? fieldTitle(measures[1]) : "Target"
  return {
    kind: "echart",
    settings: {
      type: "bullet",
      description: s.description?.trim() || `${title} against ${targetLabel}`,
      value: { label: title, format: measureFormat(s) },
      // A measured target keeps its own format; a typed one takes the value's.
      target: measures.length > 1 ? { label: targetLabel, format: fieldFormat(measures[1]) } : { label: targetLabel },
      color: measureColor(s),
    },
    rows: [{ value, target }],
    series: [],
    notes: [],
  }
}

/** A box plot's result roles, in the order the server returns them (charts.BoxRoles). */
export const BOX_ROLES = ["the minimum", "the lower quartile", "the median", "the upper quartile", "the maximum"] as const
/** A candlestick's (charts.PriceRoles). */
export const PRICE_ROLES = ["the open", "the high", "the low", "the close"] as const
/** A treemap's or a sunburst's nth level, from 1 (charts.LevelRole). */
export const levelRole = (n: number) => `level ${n}`

/**
 * A box plot's or a candlestick's categories and numbers: the x of each row, as its
 * grain writes it, and the row's numbers under the names given; null when a role is
 * missing, a string when a column is masked.
 */
function statRows<K extends string>(
  s: ServerChartSettings,
  result: ChartResult,
  roles: Map<string, number>,
  wanted: readonly string[],
  keys: readonly K[],
): { categories: string[]; rows: ({ x: string } & Record<K, number | null>)[] } | string {
  const xi = roles.get(ROLE_X)
  const is = wanted.map((r) => roles.get(r))
  if (xi === undefined || is.some((i) => i === undefined)) return MISSING
  if (is.some((i) => isMasked(result, i))) return COLUMN_MASKED
  const grain = one(s, "x")?.grain
  const rows = result.rows.map((row) => {
    const out = { x: grainLabel(row[xi], grain) } as { x: string } & Record<K, number | null>
    keys.forEach((k, j) => ((out as Record<K, number | null>)[k] = toNumber(row[is[j]!])))
    return out
  })
  return { categories: categoriesOf(rows.map((r) => r.x)), rows }
}

/**
 * How many of statRows' rows have a value that is not a number: the renderer cannot draw
 * them. The server leaves out empty values, so each came from a column of another kind.
 */
const notNumbers = (rows: readonly object[]) => rows.filter((r) => Object.values(r).includes(null)).length

/**
 * A box plot (§ 14.8): each group's least, quartiles and greatest. Where the engine
 * estimates its quartiles (BigQuery, ClickHouse) the chart says so (open question 10).
 */
function boxplotView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const stats = statRows(s, result, roles, BOX_ROLES, ["min", "q1", "median", "q3", "max"] as const)
  if (typeof stats === "string") return { kind: "message", message: stats }
  const y = one(s, "y")
  const yLabel = axisTitle(y)
  const xLabel = axisTitle(one(s, "x"))
  const notes = result.approximate ? ["The quartiles are approximate: this engine estimates them."] : []
  const left = notNumbers(stats.rows)
  if (left > 0)
    notes.push(left === 1 ? "1 group has values that are not numbers, so its box is not drawn." : `${left} groups have values that are not numbers, so their boxes are not drawn.`)
  return {
    kind: "echart",
    settings: {
      type: "boxplot",
      description: s.description?.trim() || `${yLabel} by ${xLabel}: the least, the quartiles and the greatest`,
      x: { label: xLabel, categories: stats.categories },
      y: { label: yLabel, format: y?.format ?? "decimal" },
      color: "series-1",
    },
    rows: stats.rows,
    series: [],
    notes,
  }
}

/** A candlestick (§ 14.8): each period's open, high, low and close, from its four y columns. */
function candlestickView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const stats = statRows(s, result, roles, PRICE_ROLES, ["open", "high", "low", "close"] as const)
  if (typeof stats === "string") return { kind: "message", message: stats }
  const xLabel = axisTitle(one(s, "x"))
  // One column may give more than one price (or all four): name each once.
  const prices = [...new Set(all(s, "y").map((f) => f.column ?? ""))]
  const left = notNumbers(stats.rows)
  return {
    kind: "echart",
    settings: {
      type: "candlestick",
      description: s.description?.trim() || `${listed(prices)} by ${xLabel}`,
      x: { label: xLabel, categories: stats.categories },
      // The tooltip names each price: the axis needs no title.
      y: { label: "", format: "decimal" },
    },
    rows: stats.rows,
    series: [],
    notes:
      left === 0
        ? []
        : [left === 1 ? "1 period has a price that is not a number, so it is not drawn." : `${left} periods have a price that is not a number, so they are not drawn.`],
  }
}

/** Range bands (§ 14.8): two measures by x, the band between them. */
function rangeView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const [low, high] = all(s, "measure")
  const xi = roles.get(ROLE_X)
  const mi = roles.get(ROLE_MEASURE)
  const m2i = roles.get(ROLE_SECOND_MEASURE)
  if (!low || !high || xi === undefined || mi === undefined || m2i === undefined) return { kind: "message", message: MISSING }
  if (isMasked(result, mi) || isMasked(result, m2i)) return { kind: "message", message: MASKED }
  const xAxis = one(s, "x")
  const rows = result.rows.map((row) => ({ x: grainLabel(row[xi], xAxis?.grain), low: toNumber(row[mi]), high: toNumber(row[m2i]) }))
  const categories = categoriesOf(rows.map((r) => r.x))
  // A band breaks at a period with no rows, as a line does (above): each is put back with
  // no low or high. Not on a cut result or past the row cap.
  const dayOf = new Map<string, string>()
  result.rows.forEach((row, i) => {
    const day = dayOfX(row[xi], rows[i].x, xAxis?.grain)
    if (day) dayOf.set(rows[i].x, day)
  })
  const cap = chartSpec("range")?.cap.rows ?? MAX_CHART_ROWS
  const filled = result.truncated ? null : periodsOf(categories, dayOf, xAxis?.grain, s.sort === "x_desc", cap)
  const had = new Set(categories)
  const missing = (filled?.periods ?? []).filter((x) => !had.has(x))
  if (filled && missing.length > 0 && rows.length + missing.length <= cap) {
    categories.splice(filled.from, filled.length, ...filled.periods)
    const place = new Map(categories.map((c, i) => [c, i]))
    rows.push(...missing.map((x) => ({ x, low: null, high: null })))
    rows.sort((a, b) => place.get(a.x)! - place.get(b.x)!)
  }
  const lowLabel = fieldTitle(low)
  const highLabel = fieldTitle(high)
  const xLabel = axisTitle(xAxis)
  return {
    kind: "echart",
    settings: {
      type: "range",
      description: s.description?.trim() || `${lowLabel} to ${highLabel} by ${xLabel}`,
      x: { label: xLabel, categories },
      low: { label: lowLabel, format: fieldFormat(low) },
      high: { label: highLabel, format: fieldFormat(high) },
      color: measureColor(s),
    },
    rows,
    series: [],
    notes: [],
  }
}

/** The codes a map's x is read as, in words, for a chart none of whose rows names a shape. */
const MAP_CODES = {
  world: "a country by its ISO code (FR, FRA or 250)",
  us_states: "a US state by its ISO code (US-CA or CA)",
} as const

/**
 * A choropleth map (§ 14.8): a shape a row, named by its ISO code, never by its name. A
 * row whose code names no shape, or a shape an earlier row already fills (the rows come
 * largest first), is left out and counted, with up to three of the codes it read.
 */
function mapView(s: ServerChartSettings, result: ChartResult, roles: Map<string, number>): SavedChartView {
  const xi = roles.get(ROLE_X)
  const mi = roles.get(ROLE_MEASURE)
  if (xi === undefined || mi === undefined) return { kind: "message", message: MISSING }
  if (isMasked(result, mi)) return { kind: "message", message: MASKED }
  const map = s.map ?? "world"
  const rows: { region: string; value: number }[] = []
  const filled = new Set<string>()
  const unknown: string[] = []
  let twice = 0
  for (const row of result.rows) {
    const value = toNumber(row[mi])
    if (value === null) continue
    const region = regionOf(map, row[xi])
    if (!region) unknown.push(labelOf(row[xi]))
    else if (filled.has(region.region)) twice++
    else {
      filled.add(region.region)
      rows.push({ region: region.region, value })
    }
  }
  if (rows.length === 0) return { kind: "message", message: `No row names ${MAP_CODES[map]}, so there is nothing to fill.` }
  const shape = map === "us_states" ? "US state" : "country"
  const notes: string[] = []
  if (unknown.length > 0) {
    const distinct = [...new Set(unknown)]
    const codes = listed(distinct.slice(0, 3), distinct.length > 3)
    notes.push(
      unknown.length === 1
        ? `1 row names no ${shape} by its ISO code, so it is not drawn: ${codes}.`
        : `${unknown.length} rows name no ${shape} by its ISO code, so they are not drawn: ${codes}.`,
    )
  }
  if (twice > 0) {
    notes.push(
      twice === 1
        ? `1 row names a ${shape} a row above it already fills, so it is not drawn.`
        : `${twice} rows name a ${shape} a row above them already fills, so they are not drawn.`,
    )
  }
  const x = one(s, "x")
  const title = measureTitle(s)
  return {
    kind: "echart",
    settings: {
      type: "map",
      description: s.description?.trim() || `${title} by ${axisTitle(x)}`,
      map,
      // The table lists each shape by its name, not the code the column holds.
      region: { label: x?.title?.trim() || (map === "us_states" ? "State" : "Country") },
      value: { label: title, format: measureFormat(s) },
      color: measureColor(s),
    },
    rows,
    series: [],
    notes,
  }
}

/** A scorecard's change against its earlier period, in words: "+12% on the 7 days before". */
export function changeText(value: number | null, previous: number | null | undefined, days: number): string {
  const short = changeShort(value, previous)
  const before = days === 1 ? "the day before" : `the ${days} days before`
  return short === null ? `No number for ${before}` : `${short} on ${before}`
}

/** The same change in a few characters, for a tile: "+12%", "No change"; null without both numbers. */
export function changeShort(value: number | null, previous: number | null | undefined): string | null {
  if (value === null || previous === null || previous === undefined) return null
  if (previous === 0) return value === 0 ? "No change" : "Up from 0"
  const pct = ((value - previous) / Math.abs(previous)) * 100
  if (pct === 0) return "No change"
  const text = `${Math.abs(pct) >= 10 ? Math.round(Math.abs(pct)).toLocaleString("en-US") : Math.abs(pct).toFixed(1)}%`
  return `${pct > 0 ? "+" : "−"}${text}`
}

// The gateway writes its times as "2026-10-05 11:30 UTC": the same text goes into a
// workflow's dashboard PDF, which has no viewer. Next to "Data as of", in the viewer's
// zone, a UTC time read as a different moment.
const UTC_STAMP = /\b(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}) UTC\b/g
const localStamps = (w: string) => w.replace(UTC_STAMP, (stamp, day, time) => formatAbsoluteTime(`${day}T${time}:00Z`) || stamp)

/**
 * What a chart whose rows were cut says: a treemap or a sunburst keeps its largest parts,
 * a calendar its latest days (the type's row cap, charts.MaxLeaves and CalendarDays), a
 * scatter or a bubble chart its points with the most rows (ties to the least x, then y, as
 * the SQL orders them), and every other type its first MAX_CHART_ROWS rows. The gateway's
 * chartTruncatedNote says the same in a workflow's message: shared/chart_notes_golden.json
 * holds both to it.
 */
export function truncatedNote(type?: string): string {
  const n = ((type && chartSpec(type)?.cap.rows) || MAX_CHART_ROWS).toLocaleString("en-US")
  switch (type) {
    case "treemap":
    case "sunburst":
      return `The largest ${n} parts drawn: the source has more.`
    case "calendar":
      return `The latest ${n} days drawn: the source has more.`
    case "scatter":
    case "bubble":
      return `The ${n} points with the most rows drawn, ties broken by the smallest x then y: the source has more.`
  }
  return `First ${n} rows drawn: the source has more.`
}

/**
 * A drawn chart's warnings, in amber where it is drawn (§ 5): the source's own (its
 * times in the viewer's zone), the rows cut at the type's cap, and each dashboard
 * filter it was not filtered by. None when it broke: the break replaces the chart and
 * says why. The server names a filter its connection could not take by its column; a
 * dashboard passes `unfiltered` as the filter bar's labels instead.
 */
export function chartWarnings(
  draw: ChartDraw,
  ignored: string[] = [],
  type?: string,
  unfiltered: string[] = draw.result?.unfiltered ?? [],
): string[] {
  if (draw.break) return []
  const warnings = (draw.badges?.warnings ?? []).map(localStamps)
  if (draw.result?.truncated) warnings.push(truncatedNote(type))
  if (draw.result && unfiltered.length > 0) {
    warnings.push(`Not filtered by ${unfiltered.join(", ")}: this connection cannot take a filter.`)
  }
  for (const label of ignored) warnings.push(`Not filtered by ${label}: this chart's source has no such column.`)
  return warnings
}
