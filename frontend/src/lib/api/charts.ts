/**
 * Saved charts (dashboards design § 3, § 7.2): a chart is a source (a model or a
 * saved query) and settings the server checks (§ 3.2) before it saves or draws
 * them. Every member views and draws them; owners and admins save, change and
 * delete them (can_manage). A chart is drawn live, read-only, with the viewer's
 * rights; at most 10,000 rows are drawn and the chart says so.
 * handlers/charts_api.go, handlers/chart_run.go, handlers/chart_badges.go
 *   GET    /charts/sources              -> { sources }
 *   GET    /charts/sources/:id/columns  -> { columns }
 *   POST   /charts/preview              { source_id, settings } -> ChartDraw (nothing is saved)
 *   GET    /charts                      -> { charts, can_manage }
 *   POST   /charts                      { name, source_id, settings, visibility } -> 201 { chart }
 *   GET    /charts/:id                  -> { chart, can_manage }
 *   PUT    /charts/:id                  { name, source_id, settings, visibility, updated_at } -> { chart }
 *                                       409 stale | on_shared_dashboards
 *   DELETE /charts/:id                  -> { deleted }; 409 on_dashboards
 *   POST   /charts/:id/draw             -> ChartDraw & { chart }
 * A setting the server refuses is a 422 { error, code: "invalid_settings", rule? }:
 * rule is the § 3.2 rule (1-5) it broke.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

export type ChartType =
  | "bar"
  | "hbar"
  | "line"
  | "area"
  | "pie"
  | "donut"
  | "waterfall"
  | "funnel"
  | "histogram"
  | "heatmap"
  | "scatter"
  | "bubble"
  | "scorecard"
  | "table"
  | "pivot"
  | "treemap"
  | "sunburst"
  | "sankey"
  | "calendar"
  | "bullet"
  | "boxplot"
  | "candlestick"
  | "range"
  | "map"
export type ChartAggregate = "count" | "count_distinct" | "sum" | "avg" | "min" | "max"
export type ChartSort = "x_asc" | "x_desc" | "value_asc" | "value_desc"
export type ChartNumberFormat = "integer" | "decimal" | "compact" | "percent"
export type ChartVisibility = "private" | "workspace"

/** The fixed lists the server accepts (charts/settings.go). */
export const CHART_TYPES: readonly ChartType[] = [
  "bar",
  "hbar",
  "line",
  "area",
  "pie",
  "donut",
  "waterfall",
  "funnel",
  "histogram",
  "heatmap",
  "scatter",
  "bubble",
  "scorecard",
  "table",
  "pivot",
  "treemap",
  "sunburst",
  "sankey",
  "calendar",
  "bullet",
  "boxplot",
  "candlestick",
  "range",
  "map",
]
export const CHART_AGGREGATES: readonly ChartAggregate[] = ["count", "count_distinct", "sum", "avg", "min", "max"]
export const COMPARE_DAYS: readonly number[] = [1, 7, 30, 90, 365]
export const MAX_TABLE_COLUMNS = 20
export const MAX_CHART_ROWS = 10_000

export type ChartGrain = "day" | "week" | "month" | "quarter" | "year"
/** The date grains an axis may group a time column by; unset groups by the raw value. */
export const CHART_GRAINS: readonly ChartGrain[] = ["day", "week", "month", "quarter", "year"]

/** How a combo's measure is drawn, and the y axis it reads against; unset is the type's own mark and the left axis. */
export type ChartMark = "bar" | "line" | "area"
/** The shapes a map chart fills: world countries by ISO 3166-1 code, US states by ISO 3166-2 code. */
export type ChartMap = "world" | "us_states"
export const CHART_MAPS: readonly ChartMap[] = ["world", "us_states"]
export type ChartAxis = "left" | "right"
export const CHART_MARKS: readonly ChartMark[] = ["bar", "line", "area"]

/** A histogram's bins when neither bins nor a width is set, and the most it takes (charts.DefaultBins, MaxBins). */
export const DEFAULT_BINS = 20
export const MAX_BINS = 100

/** The roles a chart's fields fill (charts/registry.go Role*). */
export type ChartRole = "x" | "y" | "size" | "series" | "rows" | "columns" | "measure" | "date"

/**
 * One column in a role (charts.Field): an axis (column, title, date grain, a histogram's
 * bins or bin width), the measure (what is counted or summed, its title, number format and
 * colour, and a combo's mark and axis), the series (colors pins a value to a token, so it
 * keeps its colour), a scatter's point size, a table column, a pivot table's row or column
 * field (its title and date grain), or a scorecard's date.
 */
export interface ChartField {
  column?: string
  aggregate?: ChartAggregate
  grain?: ChartGrain
  title?: string
  format?: ChartNumberFormat
  /** A series token (series-1…series-7) for a chart with one series. */
  color?: string
  colors?: { value: string; color: string }[]
  /** A combo's measure: drawn as a bar, line or area, against the left or right axis. */
  mark?: ChartMark
  axis?: ChartAxis
  /** A histogram's x: this many bins between its least and greatest value, or bins this wide from zero. */
  bins?: number
  width?: number
}

/**
 * A chart's settings as the server stores and checks them (charts.Settings, version 2):
 * its fields by the role each fills, as chart-registry.ts names a type's roles. The
 * server hands every chart on at version 2, whichever version it was saved at.
 */
export interface ServerChartSettings {
  version: 2
  type: ChartType
  description?: string
  roles: Partial<Record<ChartRole, ChartField[]>>
  stacked?: boolean
  /** A stacked chart's series as shares of each x's whole (100% stacking). */
  percent?: boolean
  sort?: ChartSort
  /** scorecard: the change against the same span this many days earlier, by its date role's column. */
  compare?: { days: number }
  /** scorecard: its current period drawn under the number, a point a day for 7 and 30 days, a week for 90, a month for 365. */
  sparkline?: boolean
  /** bullet: a typed target, when the target is not a second measure (exactly one of the two). */
  target?: number
  /** map: the shapes its x's codes name (exactly one for a map; none on another type). */
  map?: ChartMap
}

/** A role's first field, if it has one. */
export const one = (s: ServerChartSettings, role: ChartRole): ChartField | undefined => s.roles[role]?.[0]

/** Every field of a role, in order. */
export const all = (s: ServerChartSettings, role: ChartRole): ChartField[] => s.roles[role] ?? []

export type ChartSourceKind = "model" | "saved_query"

export interface ChartSourceItem {
  id: string
  name: string
  kind: ChartSourceKind
  visibility: string
  mine: boolean
  connection_id: string
  connection_name: string
  connector_type: string
  built: boolean
  /** False for a model never built, or a source no chart reads; reason says why. */
  chartable: boolean
  reason?: string
}

export type ChartColumnKind = "number" | "time" | "text" | "bool" | "unknown"

export interface ChartSourceColumn {
  name: string
  kind: ChartColumnKind | string
  /** The Explorer masks it for this person: it is not drawn as a number or listed as values. */
  masked: boolean
}

export type ChartSourceState = "ok" | "hidden" | "deleted"

export interface ChartRecord {
  id: string
  name: string
  source_id?: string
  source_name?: string
  source_kind?: ChartSourceKind
  source_state: ChartSourceState
  settings: ServerChartSettings
  settings_version: number
  source_columns: string[]
  visibility: ChartVisibility
  proposed_by: string
  mine: boolean
  created_at: string
  updated_at: string
}

/** One column of a drawn result: its alias in the rows and the part of the chart it is. */
export interface ChartOutput {
  alias: string
  /**
   * "the x", "the y", "the series", "the measure", "the second measure", "the earlier
   * period", "the bin width", "the other series", "the series count", "the point's rows"
   * (a scatter's rows at one point) or "table column <name>".
   */
  role: string
}

export interface ChartResult {
  columns: ChartOutput[]
  rows: unknown[][]
  /** The query hit 10,000 rows (a scatter's, points): the chart says it drew 10,000. */
  truncated: boolean
  /** The aliases the Explorer's masking redacted. */
  masked: string[]
  /** The filter columns this connection could not apply. */
  unfiltered: string[]
  sql: string
  read_at: string
  execution_time_ms: number
  /** A scorecard's sparkline: a [date, value] per point of its current period. */
  spark?: unknown[][]
  /** A box plot drawn from the engine's approximate quartiles (BigQuery, ClickHouse). */
  approximate?: boolean
  /** A table drawn without the order of each column, which the engine refused or took too long over. */
  unordered?: boolean
}

export type ChartBreakReason =
  | "source_invalid"
  | "column_gone"
  | "source_deleted"
  | "never_built"
  | "source_hidden"
  | "settings_invalid"
  | "chart_hidden"

/** A structural break replaces the chart (§ 5): nothing is drawn, and the message says why. */
export interface ChartBreak {
  reason: ChartBreakReason | string
  message: string
}

export interface ChartCheck {
  name: string
  severity: string
  status: string
}

/** What a chart says about its rows (§ 5): data as of, on time or late, checks, lineage. */
export interface ChartBadges {
  source_kind: ChartSourceKind | string
  source_id: string
  source_name: string
  data_as_of?: string
  data_as_of_from?: "cdc_watermark" | "run_finished" | string
  run_id?: string
  deadline_seconds?: number
  late: boolean
  late_cause?: string
  checks: ChartCheck[]
  /** Amber warnings: they never hide the rows. */
  warnings: string[]
  lineage_url: string
}

export interface ChartDraw {
  result: ChartResult | null
  break: ChartBreak | null
  badges: ChartBadges | null
  /** The source did not run; the message is the database's, scrubbed. */
  error?: string
}

export class ChartError extends Error {
  constructor(
    message: string,
    public status: number,
    /** "invalid_settings", "stale", "on_dashboards", "on_shared_dashboards"… */
    public code?: string,
    /** The § 3.2 rule (1-5) a refused setting broke. */
    public rule?: number,
    /** The dashboards a chart is on, when that is why it was refused. */
    public dashboards?: string[],
    /** The saved dashboard, when a draft is already saved. */
    public dashboardId?: string,
  ) {
    super(message)
    this.name = "ChartError"
  }
}

export async function readChartJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as {
      error?: unknown
      code?: unknown
      rule?: unknown
      dashboards?: unknown
      dashboard_id?: unknown
    } | null
    const error = typeof body?.error === "string" && body.error ? body.error : undefined
    throw new ChartError(
      res.status === 404 && !error ? "Not found in this workspace." : (error ?? fallback),
      res.status,
      typeof body?.code === "string" && body.code ? body.code : undefined,
      typeof body?.rule === "number" ? body.rule : undefined,
      Array.isArray(body?.dashboards) ? body.dashboards.filter((d): d is string => typeof d === "string") : undefined,
      typeof body?.dashboard_id === "string" && body.dashboard_id ? body.dashboard_id : undefined,
    )
  }
  return res.json() as Promise<T>
}

export function sendJSON(url: string, method: "POST" | "PUT", body?: unknown, timeoutMs = 120_000): Promise<Response> {
  return authFetch(url, {
    method,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body ?? {}),
    timeoutMs,
  })
}

/** A draw's parts, each null when the server sent none. */
export function asDraw(data: Partial<ChartDraw> | null | undefined): ChartDraw {
  return {
    result: data?.result ?? null,
    break: data?.break ?? null,
    badges: data?.badges ?? null,
    error: typeof data?.error === "string" && data.error ? data.error : undefined,
  }
}

export async function listChartSources(): Promise<ChartSourceItem[]> {
  const data = await readChartJSON<{ sources?: ChartSourceItem[] }>(
    await authFetch(API_ENDPOINTS.CHARTS.SOURCES),
    "Could not load the sources",
  )
  return Array.isArray(data.sources) ? data.sources : []
}

/** The source's columns, read with a one-row probe: names, kinds and masks only. */
export async function getChartSourceColumns(id: string): Promise<ChartSourceColumn[]> {
  const data = await readChartJSON<{ columns?: ChartSourceColumn[] }>(
    await authFetch(API_ENDPOINTS.CHARTS.SOURCE_COLUMNS(id), { timeoutMs: 120_000 }),
    "Could not read the source's columns",
  )
  return Array.isArray(data.columns) ? data.columns : []
}

/** Draws settings on a source without saving them. */
export async function previewChart(sourceId: string, settings: ServerChartSettings): Promise<ChartDraw> {
  const data = await readChartJSON<Partial<ChartDraw>>(
    await sendJSON(API_ENDPOINTS.CHARTS.PREVIEW, "POST", { source_id: sourceId, settings }),
    "Could not draw the chart",
  )
  return asDraw(data)
}

export async function listCharts(): Promise<{ charts: ChartRecord[]; can_manage: boolean }> {
  const data = await readChartJSON<{ charts?: ChartRecord[]; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.CHARTS.LIST),
    "Could not load charts",
  )
  return { charts: Array.isArray(data.charts) ? data.charts : [], can_manage: !!data.can_manage }
}

export async function getChart(id: string): Promise<{ chart: ChartRecord; can_manage: boolean }> {
  const data = await readChartJSON<{ chart?: ChartRecord; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.CHARTS.ITEM(id)),
    "Could not load the chart",
  )
  if (!data.chart || typeof data.chart.id !== "string") {
    throw new ChartError("Unexpected chart response", 500)
  }
  return { chart: data.chart, can_manage: !!data.can_manage }
}

export interface ChartInput {
  name: string
  source_id: string
  settings: ServerChartSettings
  visibility: ChartVisibility
}

function chartOf(data: { chart?: ChartRecord }): ChartRecord {
  if (!data.chart || typeof data.chart.id !== "string") {
    throw new ChartError("Unexpected chart response", 500)
  }
  return data.chart
}

export async function createChart(input: ChartInput): Promise<ChartRecord> {
  return chartOf(
    await readChartJSON<{ chart?: ChartRecord }>(
      await sendJSON(API_ENDPOINTS.CHARTS.LIST, "POST", input),
      "Could not save the chart",
    ),
  )
}

/** Saves a change; refused (409 stale) when the chart was saved since updated_at. */
export async function updateChart(id: string, input: ChartInput & { updated_at: string }): Promise<ChartRecord> {
  return chartOf(
    await readChartJSON<{ chart?: ChartRecord }>(
      await sendJSON(API_ENDPOINTS.CHARTS.ITEM(id), "PUT", input),
      "Could not save the chart",
    ),
  )
}

export async function deleteChart(id: string): Promise<void> {
  await readChartJSON(await authFetch(API_ENDPOINTS.CHARTS.ITEM(id), { method: "DELETE" }), "Could not delete the chart")
}

export async function drawChart(id: string): Promise<ChartDraw & { chart: ChartRecord | null }> {
  const data = await readChartJSON<Partial<ChartDraw> & { chart?: ChartRecord | null }>(
    await sendJSON(API_ENDPOINTS.CHARTS.DRAW(id), "POST"),
    "Could not draw the chart",
  )
  return { ...asDraw(data), chart: data.chart ?? null }
}
