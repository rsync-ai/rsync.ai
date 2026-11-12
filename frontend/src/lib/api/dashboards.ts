/**
 * Dashboards (dashboards design § 4): saved charts and text placed on a 12-column
 * grid, with at most 4 filters every tile whose source has the column applies as
 * a bound parameter. Every member views them; owners and admins save, change and
 * delete them (can_manage). Each tile is read live, with the viewer's rights, and
 * says when it read its rows. A dashboard Ask rsync.ai drafted (plan_dashboard,
 * § 7.3) is a proposal only its person reads; an owner or admin saves it. A change
 * to one tile it drafted (plan_dashboard_change, § 7.6) is applied the same way.
 * Every save keeps the dashboard as it was (§ 7.7); a restore is a save.
 * handlers/dashboards_api.go, handlers/dashboard_proposals.go, handlers/dashboard_changes.go
 *   GET    /dashboards                           -> { dashboards, can_manage }
 *   POST   /dashboards                           { name, description, filters, tiles, visibility } -> 201 { dashboard }
 *   GET    /dashboards/:id                       -> { dashboard, can_manage }
 *   PUT    /dashboards/:id                       { …, updated_at, restored_from? } -> { dashboard }; 409 stale
 *                                                422 version_not_found
 *   GET    /dashboards/:id/versions              -> { versions, count, current, can_manage }
 *   DELETE /dashboards/:id                       -> { deleted }
 *   POST   /dashboards/:id/tiles/:tileId/draw    { filters } -> TileDraw
 *   GET    /dashboards/:id/filters/:filterId/values -> { values, truncated, error? }
 *   GET    /dashboard-proposals/:id              -> { proposal, can_manage }
 *   POST   /dashboard-proposals/:id/save         { visibility } -> 201 { dashboard_id, chart_ids }
 *                                                409 already_saved | 410 expired
 *   GET    /dashboard-changes/:id                -> { change, can_manage }
 *   POST   /dashboard-changes/:id/apply          -> { dashboard_id, chart_id, copied }
 *                                                409 already_applied | dashboard_changed | 410 expired
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { filenameFromDisposition } from "@/lib/export/download"
import {
  asDraw,
  ChartError,
  readChartJSON,
  sendJSON,
  type ChartDraw,
  type ChartRecord,
  type ChartSourceKind,
  type ChartType,
  type ChartVisibility,
  type ServerChartSettings,
} from "@/lib/api/charts"

/** The server's limits (charts/dashboard.go). */
export const GRID_COLUMNS = 12
export const MAX_TILES = 30
export const MAX_FILTERS = 4
export const MAX_TILE_HEIGHT = 24
export const MAX_TEXT_TILE = 2000
export const MAX_FILTER_VALUES = 50

export type DashboardFilterKind = "date_range" | "values"

export interface DashboardFilter {
  /** Lower case letters, digits, - and _: the key a tile draw names it by. */
  id: string
  kind: DashboardFilterKind
  label: string
  column: string
}

/** A tile as saved: a chart tile names its chart, a text tile carries plain text. */
export interface DashboardTileInput {
  kind: "chart" | "text"
  chart_id?: string
  body?: string
  x: number
  y: number
  w: number
  h: number
}

export interface DashboardTile extends DashboardTileInput {
  id: string
  /** The tile's chart, when the viewer may see it; absent when it is private to its author. */
  chart?: ChartRecord
  chart_state?: string
}

export interface Dashboard {
  id: string
  name: string
  description: string
  filters: DashboardFilter[]
  /** Only on GET /dashboards/:id. */
  tiles?: DashboardTile[]
  tile_count: number
  visibility: ChartVisibility
  proposed_by: string
  mine: boolean
  created_at: string
  updated_at: string
}

/** A filter's pick: a date range (both ends, YYYY-MM-DD, the end day included) or a list of values. */
export interface DashboardFilterValue {
  from?: string
  to?: string
  values?: string[]
}

export interface TileDraw extends ChartDraw {
  chart: ChartRecord | null
  /**
   * The filter ids this tile applied, those whose column its source has not, and those
   * it was drawn without as its connection could not take them (on `result.unfiltered`).
   */
  filters: { applied: string[]; ignored: string[]; unfiltered: string[] }
}

export async function listDashboards(): Promise<{ dashboards: Dashboard[]; can_manage: boolean }> {
  const data = await readChartJSON<{ dashboards?: Dashboard[]; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.DASHBOARDS.LIST),
    "Could not load dashboards",
  )
  return { dashboards: Array.isArray(data.dashboards) ? data.dashboards : [], can_manage: !!data.can_manage }
}

function dashboardOf(data: { dashboard?: Dashboard }): Dashboard {
  if (!data.dashboard || typeof data.dashboard.id !== "string") {
    throw new ChartError("Unexpected dashboard response", 500)
  }
  return {
    ...data.dashboard,
    filters: Array.isArray(data.dashboard.filters) ? data.dashboard.filters : [],
    tiles: Array.isArray(data.dashboard.tiles) ? data.dashboard.tiles : [],
  }
}

export async function getDashboard(id: string): Promise<{ dashboard: Dashboard; can_manage: boolean }> {
  const data = await readChartJSON<{ dashboard?: Dashboard; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.DASHBOARDS.ITEM(id)),
    "Could not load the dashboard",
  )
  return { dashboard: dashboardOf(data), can_manage: !!data.can_manage }
}

export interface DashboardInput {
  name: string
  description: string
  filters: DashboardFilter[]
  tiles: DashboardTileInput[]
  visibility: ChartVisibility
}

export async function createDashboard(input: DashboardInput): Promise<Dashboard> {
  return dashboardOf(
    await readChartJSON<{ dashboard?: Dashboard }>(
      await sendJSON(API_ENDPOINTS.DASHBOARDS.LIST, "POST", input),
      "Could not save the dashboard",
    ),
  )
}

/**
 * Saves a change; refused (409 stale) when the dashboard was saved since updated_at.
 * restored_from names the version a restore puts back, which the History keeps.
 */
export async function updateDashboard(
  id: string,
  input: DashboardInput & { updated_at: string; restored_from?: number },
): Promise<Dashboard> {
  return dashboardOf(
    await readChartJSON<{ dashboard?: Dashboard }>(
      await sendJSON(API_ENDPOINTS.DASHBOARDS.ITEM(id), "PUT", input),
      "Could not save the dashboard",
    ),
  )
}

export async function deleteDashboard(id: string): Promise<void> {
  await readChartJSON(
    await authFetch(API_ENDPOINTS.DASHBOARDS.ITEM(id), { method: "DELETE" }),
    "Could not delete the dashboard",
  )
}

/** Draws one chart tile live with the dashboard's filters; a filter with no value is left out. */
export async function drawTile(
  dashboardId: string,
  tileId: string,
  filters: Record<string, DashboardFilterValue>,
): Promise<TileDraw> {
  const data = await readChartJSON<Partial<TileDraw>>(
    await sendJSON(API_ENDPOINTS.DASHBOARDS.DRAW_TILE(dashboardId, tileId), "POST", { filters }),
    "Could not draw the tile",
  )
  return {
    ...asDraw(data),
    chart: data.chart ?? null,
    filters: {
      applied: Array.isArray(data.filters?.applied) ? data.filters.applied : [],
      ignored: Array.isArray(data.filters?.ignored) ? data.filters.ignored : [],
      unfiltered: Array.isArray(data.filters?.unfiltered) ? data.filters.unfiltered : [],
    },
  }
}

/**
 * The dashboard as a PDF, with the filters picked (dashboards-design.md § 8, D3). The
 * frontend's own server makes it (app/proxy/dashboards/[id]/pdf), drawing each tile as
 * this viewer, so it takes as long as the slowest tile: the wait is the proxy's 100 s.
 */
export async function exportDashboardPdf(
  dashboardId: string,
  filters: Record<string, DashboardFilterValue>,
): Promise<{ blob: Blob; filename: string }> {
  // An absolute URL, so authFetch asks this site rather than the gateway.
  const url = new URL(`/proxy/dashboards/${encodeURIComponent(dashboardId)}/pdf`, window.location.origin).href
  const res = await sendJSON(url, "POST", { filters }, 100_000)
  if (!res.ok) await readChartJSON(res, "Could not export the dashboard")
  return { blob: await res.blob(), filename: filenameFromDisposition(res.headers.get("Content-Disposition"), "dashboard.pdf") }
}

/** A value-list filter's choices, read live from the first tile's source that has the column. */
export async function filterValues(
  dashboardId: string,
  filterId: string,
): Promise<{ values: string[]; truncated: boolean; error?: string }> {
  const data = await readChartJSON<{ values?: string[]; truncated?: boolean; error?: string }>(
    await authFetch(API_ENDPOINTS.DASHBOARDS.FILTER_VALUES(dashboardId, filterId), { timeoutMs: 120_000 }),
    "Could not list the filter's values",
  )
  return {
    values: Array.isArray(data.values) ? data.values : [],
    truncated: data.truncated === true,
    error: typeof data.error === "string" && data.error ? data.error : undefined,
  }
}

export interface DashboardProposalChart {
  name: string
  source_id: string
  source_name: string
  source_kind: ChartSourceKind | string
  type: ChartType
  settings: ServerChartSettings
}

export interface DashboardProposalTile {
  /** The 0-based chart of the proposal this tile draws. */
  chart: number
  x: number
  y: number
  w: number
  h: number
}

export type DashboardProposalStatus = "proposed" | "saved"

/** A dashboard Ask rsync.ai drafted: nothing is saved until an owner or admin saves it here. */
export interface DashboardProposal {
  id: string
  name: string
  description: string
  charts: DashboardProposalChart[]
  filters: DashboardFilter[]
  tiles: DashboardProposalTile[]
  status: DashboardProposalStatus | string
  expired: boolean
  saved_dashboard_id?: string
  proposed_by: string
  created_at: string
  expires_at: string
  url: string
}

export async function getDashboardProposal(id: string): Promise<{ proposal: DashboardProposal; can_manage: boolean }> {
  const data = await readChartJSON<{ proposal?: DashboardProposal; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.DASHBOARD_PROPOSALS.GET(id)),
    "Could not load the dashboard draft",
  )
  const p = data.proposal
  if (!p || typeof p.id !== "string") {
    throw new ChartError("Unexpected dashboard draft response", 500)
  }
  return {
    proposal: {
      ...p,
      charts: Array.isArray(p.charts) ? p.charts : [],
      filters: Array.isArray(p.filters) ? p.filters : [],
      tiles: Array.isArray(p.tiles) ? p.tiles : [],
    },
    can_manage: !!data.can_manage,
  }
}

/** Saves the draft's charts and its dashboard, checked as a person's would be. */
export async function saveDashboardProposal(
  id: string,
  visibility: ChartVisibility,
): Promise<{ dashboard_id: string; chart_ids: string[] }> {
  const data = await readChartJSON<{ dashboard_id?: string; chart_ids?: string[] }>(
    await sendJSON(API_ENDPOINTS.DASHBOARD_PROPOSALS.SAVE(id), "POST", { visibility }),
    "Could not save the dashboard",
  )
  if (typeof data.dashboard_id !== "string" || !data.dashboard_id) {
    throw new ChartError("Unexpected dashboard response", 500)
  }
  return { dashboard_id: data.dashboard_id, chart_ids: Array.isArray(data.chart_ids) ? data.chart_ids : [] }
}

/** The tile before or after a change. */
export interface DashboardChangeSide {
  name: string
  type: ChartType | string
  settings: ServerChartSettings
  x: number
  y: number
  w: number
  h: number
}

/** A tile moved down to make room for the changed one. */
export interface DashboardChangeMove {
  tile_id: string
  from_y: number
  y: number
}

export type DashboardChangeStatus = "proposed" | "applied"

/**
 * A change to one tile Ask rsync.ai drafted: nothing changes until an owner or admin
 * applies it. stale: the dashboard, the tile or its chart was saved since, so it is
 * never applied. copy: the chart is on another dashboard too, so Apply copies it for
 * this tile and the other dashboards keep theirs.
 */
export interface DashboardChange {
  id: string
  dashboard_id: string
  dashboard_name: string
  tile_id: string
  chart_id: string
  before: DashboardChangeSide
  after: DashboardChangeSide
  moves: DashboardChangeMove[]
  summary: string
  chart_changed: boolean
  size_changed: boolean
  shared_elsewhere: number
  copy: boolean
  source_id: string
  source_name: string
  status: DashboardChangeStatus | string
  expired: boolean
  stale: boolean
  applied_chart_id?: string
  copied: boolean
  proposed_by: string
  created_at: string
  expires_at: string
  applied_at?: string
  url: string
}

export async function getDashboardChange(id: string): Promise<{ change: DashboardChange; can_manage: boolean }> {
  const data = await readChartJSON<{ change?: DashboardChange; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.DASHBOARD_CHANGES.GET(id)),
    "Could not load the change",
  )
  const c = data.change
  if (!c || typeof c.id !== "string" || !c.before || !c.after) {
    throw new ChartError("Unexpected change response", 500)
  }
  return { change: { ...c, moves: Array.isArray(c.moves) ? c.moves : [] }, can_manage: !!data.can_manage }
}

/** Applies the change, checked again with the applier's rights. */
export async function applyDashboardChange(
  id: string,
): Promise<{ dashboard_id: string; chart_id: string; copied: boolean }> {
  const data = await readChartJSON<{ dashboard_id?: string; chart_id?: string; copied?: boolean }>(
    await sendJSON(API_ENDPOINTS.DASHBOARD_CHANGES.APPLY(id), "POST"),
    "Could not apply the change",
  )
  if (typeof data.dashboard_id !== "string" || typeof data.chart_id !== "string") {
    throw new ChartError("Unexpected change response", 500)
  }
  return { dashboard_id: data.dashboard_id, chart_id: data.chart_id, copied: data.copied === true }
}

/** How a version's save came about. */
export type DashboardSavedBy = "edit" | "ask" | "restore"

/** The dashboard as it was before one save. Its chart tiles name charts; a chart keeps its current settings. */
export interface DashboardVersion {
  version: number
  name: string
  description: string
  filters: DashboardFilter[]
  visibility: ChartVisibility
  tiles: DashboardTileInput[]
  saved_by: string
  how: DashboardSavedBy | string
  change_id?: string
  restored_from?: number
  created_at: string
}

export interface DashboardVersions {
  /** Newest first, at most 100. */
  versions: DashboardVersion[]
  count: number
  /** What a restore saves over: it keeps this visibility and needs this updated_at. */
  current: { name: string; visibility: ChartVisibility; updated_at: string } | null
  can_manage: boolean
}

export async function listDashboardVersions(id: string): Promise<DashboardVersions> {
  const data = await readChartJSON<Partial<DashboardVersions>>(
    await authFetch(API_ENDPOINTS.DASHBOARDS.VERSIONS(id)),
    "Could not load the history",
  )
  return {
    versions: Array.isArray(data.versions)
      ? data.versions.map((v) => ({
          ...v,
          filters: Array.isArray(v.filters) ? v.filters : [],
          tiles: Array.isArray(v.tiles) ? v.tiles : [],
        }))
      : [],
    count: typeof data.count === "number" ? data.count : 0,
    current: data.current ?? null,
    can_manage: !!data.can_manage,
  }
}
