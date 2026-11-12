/**
 * A dashboard's grid and filters, as plain functions (dashboards-design.md § 4.2,
 * § 7.2): the view and the editor place tiles the same way, so a tile is the same
 * size in both, and the server holds the result to charts/dashboard.go's limits.
 */

import {
  GRID_COLUMNS,
  MAX_TILE_HEIGHT,
  type DashboardFilter,
  type DashboardFilterValue,
  type DashboardTile,
  type DashboardTileInput,
} from "@/lib/api/dashboards"

/** One grid row, in pixels, and the gap between tiles: the same in the view and the editor. */
export const ROW_HEIGHT = 40
export const GRID_GAP = 12

/**
 * A new chart tile's size: a third of the grid wide, so three fill a row; it goes in
 * the first space it fits (firstFreePlace), and its width menu or its edges size it.
 */
export const NEW_CHART_TILE = { w: 4, h: 7 }
export const NEW_TEXT_TILE = { w: 12, h: 2 }

/**
 * The fewest rows the editor lets a tile shrink to. A scorecard needs 3: its label and
 * its number take 56 px, more than a 2-row tile's 38 px body (SavedChart's Scorecard).
 */
export const MIN_TILE_ROWS = 2
export const MIN_SCORECARD_ROWS = 3

/** The fewest rows a tile of this chart type may have; a text tile has no type. */
export function minTileRows(chartType?: string): number {
  return chartType === "scorecard" ? MIN_SCORECARD_ROWS : MIN_TILE_ROWS
}

/** A tile h rows tall, in pixels. */
export function tileHeight(h: number): number {
  return h * ROW_HEIGHT + (h - 1) * GRID_GAP
}

/** The tiles top to bottom, then left to right: the order a narrow screen stacks them in. */
export function readingOrder<T extends { x: number; y: number }>(tiles: readonly T[]): T[] {
  return [...tiles].sort((a, b) => a.y - b.y || a.x - b.x)
}

/** The first row below every tile, where a new tile goes. */
export function nextRow(tiles: readonly { y: number; h: number }[]): number {
  return tiles.reduce((bottom, t) => Math.max(bottom, t.y + t.h), 0)
}

/**
 * Where a w-by-h tile goes: the first free space it fits, top to bottom, then left to
 * right, else under every tile.
 */
export function firstFreePlace(tiles: readonly { x: number; y: number; w: number; h: number }[], w: number, h: number): { x: number; y: number } {
  const bottom = nextRow(tiles)
  for (let y = 0; y < bottom; y++) {
    for (let x = 0; x + w <= GRID_COLUMNS; x++) {
      if (!tiles.some((t) => x < t.x + t.w && t.x < x + w && y < t.y + t.h && t.y < y + h)) return { x, y }
    }
  }
  return { x: 0, y: bottom }
}

/** The widths a tile's width menu offers, in columns: a size by one click, beside the drag. */
export const TILE_WIDTHS = [
  { w: 12, label: "Full width" },
  { w: 9, label: "¾ width" },
  { w: 6, label: "½ width" },
  { w: 4, label: "⅓ width" },
  { w: 3, label: "¼ width" },
] as const

/** A tile's width in words: the menu's name for it, else its columns. */
export function widthLabel(w: number): string {
  return TILE_WIDTHS.find((t) => t.w === w)?.label ?? `${w} of ${GRID_COLUMNS} columns`
}

/** A tile kept inside the 12-column grid and the server's height limit. */
export function clampTile<T extends { x: number; y: number; w: number; h: number }>(t: T): T {
  const w = Math.min(Math.max(1, Math.round(t.w)), GRID_COLUMNS)
  const x = Math.min(Math.max(0, Math.round(t.x)), GRID_COLUMNS - w)
  const h = Math.min(Math.max(1, Math.round(t.h)), MAX_TILE_HEIGHT)
  return { ...t, x, w, h, y: Math.max(0, Math.round(t.y)) }
}

/**
 * A filter id from its label: lower case letters, digits, - and _, at most 32,
 * starting with a letter or digit (the server's rule), and not one already taken.
 */
export function filterId(label: string, taken: readonly string[]): string {
  const base =
    label
      .toLowerCase()
      .normalize("NFKD")
      // An accent splits off its letter (é is e and ´): drop it, so "Région" is region.
      .replace(/\p{M}+/gu, "")
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 28)
      .replace(/-+$/, "") || "filter"
  if (!taken.includes(base)) return base
  for (let n = 2; ; n++) {
    const id = `${base}-${n}`
    if (!taken.includes(id)) return id
  }
}

/**
 * The labels of the filters a tile did not apply, from their ids: those its source has
 * no column for, or those its connection could not take (`filters.unfiltered`). By id,
 * so a filter on the same column that was not picked is not named.
 */
export function ignoredLabels(ignored: readonly string[], filters: readonly DashboardFilter[]): string[] {
  return ignored.map((id) => filters.find((f) => f.id === id)?.label ?? id)
}

/** The picks to send: a date range with both ends, a value list with a value; nothing else. */
export function activePicks(
  filters: readonly DashboardFilter[],
  picks: Record<string, DashboardFilterValue>,
): Record<string, DashboardFilterValue> {
  const out: Record<string, DashboardFilterValue> = {}
  for (const f of filters) {
    const p = picks[f.id]
    if (!p) continue
    if (f.kind === "date_range" && p.from && p.to && p.from <= p.to) out[f.id] = { from: p.from, to: p.to }
    if (f.kind === "values" && p.values && p.values.length > 0) out[f.id] = { values: p.values }
  }
  return out
}

/** A tile in the editor: a saved tile keeps its id, a new one has only its key. */
export interface DraftTile extends DashboardTileInput {
  key: string
  id?: string
  body: string
}

export function draftTiles(tiles: readonly DashboardTile[]): DraftTile[] {
  return tiles.map((t) => ({
    key: t.id,
    id: t.id,
    kind: t.kind,
    chart_id: t.chart_id,
    body: t.body ?? "",
    x: t.x,
    y: t.y,
    w: t.w,
    h: t.h,
  }))
}

/** The tiles as the server takes them: a chart tile names its chart, a text tile has its text. */
export function tileInputs(tiles: readonly DraftTile[]): DashboardTileInput[] {
  return readingOrder(tiles).map((t) =>
    t.kind === "chart"
      ? { kind: "chart", chart_id: t.chart_id, x: t.x, y: t.y, w: t.w, h: t.h }
      : { kind: "text", body: t.body, x: t.x, y: t.y, w: t.w, h: t.h },
  )
}
