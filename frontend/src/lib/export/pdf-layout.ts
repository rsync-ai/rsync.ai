/**
 * Where each tile goes in a dashboard's PDF (dashboards-design.md § 8, D3): A4
 * landscape, and every tile keeps its column and width from the 12-column grid, so the
 * page reads like the screen. Tiles are placed a band at a time — a band is the tiles
 * whose rows overlap, the same row on screen — and a band that does not fit what is
 * left of a page starts the next one. A band taller than a whole page is shrunk to fit
 * it. No DOM: this file is safe anywhere.
 */

import { GRID_GAP, ROW_HEIGHT } from "@/components/dashboards/dashboard-layout"
import { GRID_COLUMNS } from "@/lib/api/dashboards"

/** A4 landscape, in PDF points (1/72 in). */
export const A4_LANDSCAPE = { width: 841.89, height: 595.28 }

/**
 * The grid's width on a laptop screen, in CSS pixels: a row is as tall against a
 * column as it is there.
 */
export const SCREEN_GRID_WIDTH = 1200

export interface GridTile {
  x: number
  y: number
  w: number
  h: number
}

export interface PdfPage {
  width: number
  height: number
  margin: number
  /** The first page's header (name, filters, time), below the margin. */
  headerHeight: number
  /** The page number line, above the bottom margin. */
  footerHeight: number
}

export interface PdfBox {
  x: number
  y: number
  width: number
  height: number
}

export interface PdfPlacement<T> {
  tile: T
  /** 0 for the first page. */
  page: number
  box: PdfBox
}

export function layoutTiles<T extends GridTile>(
  tiles: readonly T[],
  page: PdfPage,
): { placements: PdfPlacement<T>[]; pages: number } {
  const contentWidth = page.width - 2 * page.margin
  const scale = contentWidth / SCREEN_GRID_WIDTH
  const gap = GRID_GAP * scale
  const column = (contentWidth - (GRID_COLUMNS - 1) * gap) / GRID_COLUMNS
  const row = ROW_HEIGHT * scale
  const span = (cells: number, unit: number) => cells * unit + Math.max(cells - 1, 0) * gap

  const bottom = page.height - page.margin - page.footerHeight
  const topOf = (p: number) => page.margin + (p === 0 ? page.headerHeight : 0)

  const placements: PdfPlacement<T>[] = []
  let current = 0
  let cursor = topOf(0)
  let empty = true

  for (const band of bands(tiles)) {
    let height = span(band.rows, row)
    if (!empty && cursor + gap + height > bottom) {
      current += 1
      cursor = topOf(current)
      empty = true
    }
    if (!empty) cursor += gap
    const room = bottom - cursor
    const fit = height > room ? room / height : 1
    height *= fit
    for (const t of band.tiles) {
      placements.push({
        tile: t,
        page: current,
        box: {
          x: page.margin + t.x * (column + gap),
          y: cursor + (t.y - band.top) * (row + gap) * fit,
          width: span(t.w, column),
          height: span(t.h, row) * fit,
        },
      })
    }
    cursor += height
    empty = false
  }
  return { placements, pages: current + 1 }
}

/** The tiles in bands, top to bottom: each band holds the tiles whose rows overlap. */
function bands<T extends GridTile>(tiles: readonly T[]): { top: number; rows: number; tiles: T[] }[] {
  const sorted = [...tiles].sort((a, b) => a.y - b.y || a.x - b.x)
  const out: { top: number; rows: number; tiles: T[] }[] = []
  let end = -Infinity
  for (const t of sorted) {
    const last = out[out.length - 1]
    if (last && t.y < end) {
      last.tiles.push(t)
      end = Math.max(end, t.y + t.h)
      last.rows = end - last.top
    } else {
      out.push({ top: t.y, rows: t.h, tiles: [t] })
      end = t.y + t.h
    }
  }
  return out
}
