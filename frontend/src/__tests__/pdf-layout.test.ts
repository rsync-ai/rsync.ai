import { describe, expect, it } from "vitest"

import { A4_LANDSCAPE, layoutTiles, type GridTile, type PdfPage } from "@/lib/export/pdf-layout"

// Where a dashboard's tiles go in its PDF (dashboards-design.md § 8, D3): each keeps
// its column and width from the 12-column grid, a row that does not fit what is left
// of a page starts the next one, and a row taller than a page is shrunk onto one.

const PAGE: PdfPage = { ...A4_LANDSCAPE, margin: 28, headerHeight: 46, footerHeight: 18 }
const CONTENT = PAGE.width - 2 * PAGE.margin
const BOTTOM = PAGE.height - PAGE.margin - PAGE.footerHeight
const tile = (x: number, y: number, w: number, h: number, id = `${x},${y}`): GridTile & { id: string } => ({ id, x, y, w, h })
const close = (a: number, b: number) => expect(Math.abs(a - b)).toBeLessThan(0.01)

describe("layoutTiles", () => {
  it("gives a full-width tile the page's width and puts it under the header", () => {
    const { placements, pages } = layoutTiles([tile(0, 0, 12, 4)], PAGE)
    expect(pages).toBe(1)
    close(placements[0].box.x, PAGE.margin)
    close(placements[0].box.width, CONTENT)
    close(placements[0].box.y, PAGE.margin + PAGE.headerHeight)
  })

  it("keeps two tiles of one row side by side, at their columns and widths", () => {
    const { placements } = layoutTiles([tile(6, 0, 6, 4, "right"), tile(0, 0, 6, 4, "left")], PAGE)
    const left = placements.find((p) => p.tile.id === "left")!.box
    const right = placements.find((p) => p.tile.id === "right")!.box
    expect(left.y).toBe(right.y)
    close(left.width, right.width)
    close(left.x, PAGE.margin)
    close(right.x + right.width, PAGE.margin + CONTENT)
    expect(right.x).toBeGreaterThan(left.x + left.width)
  })

  it("starts a row that does not fit on the next page, at the top margin", () => {
    // Each row is 6 grid rows, about 197 pt with its gaps: two fit under the header, a third does not.
    const tiles = [tile(0, 0, 12, 6, "a"), tile(0, 6, 12, 6, "b"), tile(0, 12, 12, 6, "c")]
    const { placements, pages } = layoutTiles(tiles, PAGE)
    const c = placements.find((p) => p.tile.id === "c")!
    expect(pages).toBe(2)
    expect(c.page).toBe(1)
    close(c.box.y, PAGE.margin)
    for (const p of placements) expect(p.box.y + p.box.height).toBeLessThanOrEqual(BOTTOM + 0.01)
  })

  it("moves a whole row together: a short tile beside a tall one breaks with it", () => {
    const tiles = [tile(0, 0, 12, 12, "top"), tile(0, 12, 4, 2, "short"), tile(4, 12, 8, 9, "tall")]
    const { placements } = layoutTiles(tiles, PAGE)
    const short = placements.find((p) => p.tile.id === "short")!
    const tall = placements.find((p) => p.tile.id === "tall")!
    expect(short.page).toBe(tall.page)
    expect(short.page).toBe(1)
    expect(short.box.y).toBe(tall.box.y)
  })

  it("shrinks a row taller than a page onto one page", () => {
    const { placements, pages } = layoutTiles([tile(0, 0, 12, 40)], PAGE)
    expect(pages).toBe(1)
    close(placements[0].box.y + placements[0].box.height, BOTTOM)
  })

  it("has one page for a dashboard with no tiles", () => {
    expect(layoutTiles([], PAGE)).toEqual({ placements: [], pages: 1 })
  })
})
