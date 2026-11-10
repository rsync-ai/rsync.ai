import { describe, expect, it } from "vitest"

import {
  activePicks,
  clampTile,
  draftTiles,
  filterId,
  firstFreePlace,
  ignoredLabels,
  minTileRows,
  NEW_CHART_TILE,
  nextRow,
  TILE_WIDTHS,
  readingOrder,
  tileHeight,
  tileInputs,
  type DraftTile,
} from "@/components/dashboards/dashboard-layout"
import type { DashboardFilter, DashboardTile } from "@/lib/api/dashboards"

// A dashboard's grid and filters (dashboards-design.md § 4.2): the view and the editor
// place a tile the same way, and what they send is what charts/dashboard.go accepts.

/** The server's filter id rule (charts/dashboard.go). */
const FILTER_ID = /^[a-z0-9][a-z0-9_-]{0,31}$/

describe("tile size", () => {
  // The plot's share of a tile is not worked out here: the tile lays it out
  // (SavedChart's fill, dashboard-tile-fill.test.tsx).
  it("is h rows and the gaps between them", () => {
    expect(tileHeight(1)).toBe(40)
    expect(tileHeight(7)).toBe(7 * 40 + 6 * 12)
  })
})

describe("minTileRows", () => {
  // A 3-row tile is the smallest with room for a scorecard's line of badges, its
  // label and its number; 2 rows hid the number under the tile's scrollbar on prod.
  it("keeps a scorecard at 3 rows and any other tile at 2", () => {
    expect(minTileRows("scorecard")).toBe(3)
    expect(tileHeight(minTileRows("scorecard"))).toBe(144)
    for (const type of ["bar", "hbar", "line", "area", "heatmap", "table", undefined]) expect(minTileRows(type)).toBe(2)
  })
})

describe("readingOrder and nextRow", () => {
  it("stacks tiles top to bottom, then left to right", () => {
    const tiles = [
      { id: "c", x: 6, y: 7 },
      { id: "b", x: 0, y: 7 },
      { id: "a", x: 6, y: 0 },
      { id: "z", x: 0, y: 0 },
    ]
    expect(readingOrder(tiles).map((t) => t.id)).toEqual(["z", "a", "b", "c"])
    // The tiles given are left as they were.
    expect(tiles[0].id).toBe("c")
  })

  it("puts a new tile under the lowest one", () => {
    expect(nextRow([])).toBe(0)
    expect(
      nextRow([
        { y: 0, h: 7 },
        { y: 3, h: 8 },
        { y: 7, h: 2 },
      ]),
    ).toBe(11)
  })
})

describe("a new chart tile", () => {
  // It went under every tile at half the width, so a dashboard of charts only filled
  // its rows by hand; now it takes the first space it fits (dashboards-design.md § 7.2).
  const place = (...tiles: [number, number, number, number][]) =>
    firstFreePlace(
      tiles.map(([x, y, w, h]) => ({ x, y, w, h })),
      NEW_CHART_TILE.w,
      NEW_CHART_TILE.h,
    )

  it("is a third of the grid wide and 7 rows tall", () => {
    expect(NEW_CHART_TILE).toEqual({ w: 4, h: 7 })
  })

  it("goes into the first space it fits, top to bottom, then left to right", () => {
    expect(place()).toEqual({ x: 0, y: 0 })
    expect(place([0, 0, 6, 7])).toEqual({ x: 6, y: 0 })
    expect(place([0, 0, 4, 7], [8, 0, 4, 7])).toEqual({ x: 4, y: 0 })
    // Under a short tile, beside a tall one.
    expect(place([0, 0, 6, 3], [6, 0, 6, 10])).toEqual({ x: 0, y: 3 })
  })

  it("goes under every tile when no space is 4 columns wide and 7 rows tall", () => {
    expect(place([0, 0, 9, 7])).toEqual({ x: 0, y: 7 })
    expect(place([0, 0, 3, 5], [3, 0, 6, 7], [0, 7, 12, 2])).toEqual({ x: 0, y: 9 })
  })
})

describe("TILE_WIDTHS", () => {
  it("are the full row, three quarters, a half, a third and a quarter of 12 columns", () => {
    expect(TILE_WIDTHS.map((t) => [t.w, t.label])).toEqual([
      [12, "Full width"],
      [9, "¾ width"],
      [6, "½ width"],
      [4, "⅓ width"],
      [3, "¼ width"],
    ])
  })
})

describe("clampTile", () => {
  const t = (x: number, y: number, w: number, h: number) => clampTile({ x, y, w, h })

  it("keeps a tile inside the 12-column grid", () => {
    expect(t(10, 0, 6, 4)).toEqual({ x: 6, y: 0, w: 6, h: 4 })
    expect(t(3, 0, 20, 4)).toEqual({ x: 0, y: 0, w: 12, h: 4 })
    expect(t(-2, 0, 0, 4)).toEqual({ x: 0, y: 0, w: 1, h: 4 })
  })

  it("keeps its height 1 to 24 and its row at 0 or below", () => {
    expect(t(0, -3, 6, 30)).toEqual({ x: 0, y: 0, w: 6, h: 24 })
    expect(t(0, 2, 6, 0)).toEqual({ x: 0, y: 2, w: 6, h: 1 })
  })

  it("rounds a dragged position to whole cells and keeps the tile's other fields", () => {
    expect(clampTile({ key: "k", x: 2.6, y: 1.4, w: 3.5, h: 6.49 })).toEqual({ key: "k", x: 3, y: 1, w: 4, h: 6 })
  })
})

describe("filterId", () => {
  it("is the label in lower case, with dashes, and never one already taken", () => {
    expect(filterId("Order date", [])).toBe("order-date")
    expect(filterId("Order date", ["order-date"])).toBe("order-date-2")
    expect(filterId("Order date", ["order-date", "order-date-2"])).toBe("order-date-3")
    expect(filterId("  Order -- date!  ", [])).toBe("order-date")
  })

  it("drops an accent rather than make it a dash, as the server's draft does", () => {
    expect(filterId("Région", [])).toBe("region")
    expect(filterId("Ångström", [])).toBe("angstrom")
  })

  it("is filter when the label has no letter it can keep", () => {
    expect(filterId("日付", [])).toBe("filter")
    expect(filterId("", ["filter"])).toBe("filter-2")
  })

  it("is an id the server accepts, however long the label", () => {
    const long = "A very long label that goes on past the cap"
    expect(filterId(long, [])).toBe("a-very-long-label-that-goes")
    for (const label of [long, "x".repeat(80), "Région", "日付", "--a--"]) {
      const id = filterId(label, [filterId(label, [])])
      expect(id).toMatch(FILTER_ID)
    }
  })
})

const filters: DashboardFilter[] = [
  { id: "day", kind: "date_range", label: "Day", column: "order_date" },
  { id: "region", kind: "values", label: "Region", column: "region" },
]

describe("activePicks", () => {
  it("sends a range with both ends and a list with a value", () => {
    expect(
      activePicks(filters, {
        day: { from: "2026-10-01", to: "2026-10-05" },
        region: { values: ["EU", "US"] },
      }),
    ).toEqual({ day: { from: "2026-10-01", to: "2026-10-05" }, region: { values: ["EU", "US"] } })
  })

  it("leaves out a half range, a backwards one, an empty list and a filter it has not got", () => {
    expect(activePicks(filters, { day: { from: "2026-10-01" }, region: { values: [] } })).toEqual({})
    expect(activePicks(filters, { day: { from: "2026-10-05", to: "2026-10-01" } })).toEqual({})
    expect(activePicks(filters, { secret: { values: ["x"] } })).toEqual({})
  })

  it("sends only the fields of the filter's kind", () => {
    expect(activePicks(filters, { day: { from: "2026-10-01", to: "2026-10-01", values: ["x"] } })).toEqual({
      day: { from: "2026-10-01", to: "2026-10-01" },
    })
    expect(activePicks(filters, { region: { values: ["EU"], from: "2026-10-01", to: "2026-10-02" } })).toEqual({
      region: { values: ["EU"] },
    })
  })
})

describe("ignoredLabels", () => {
  it("names the filters a tile did not apply, by label", () => {
    expect(ignoredLabels(["region", "gone"], filters)).toEqual(["Region", "gone"])
  })
})

describe("draftTiles and tileInputs", () => {
  const saved: DashboardTile[] = [
    { id: "t2", kind: "text", body: "Notes", x: 0, y: 7, w: 12, h: 2 },
    { id: "t1", kind: "chart", chart_id: "c1", x: 6, y: 0, w: 6, h: 7 },
    { id: "t0", kind: "chart", chart_id: "c0", x: 0, y: 0, w: 6, h: 7 },
  ]

  it("sends the tiles in reading order, a chart tile with no text and a text tile with no chart", () => {
    const drafts: DraftTile[] = draftTiles(saved)
    // A stray field from the other kind is not sent.
    drafts[0] = { ...drafts[0], chart_id: "c9" }
    drafts[1] = { ...drafts[1], body: "left over" }
    expect(tileInputs(drafts)).toEqual([
      { kind: "chart", chart_id: "c0", x: 0, y: 0, w: 6, h: 7 },
      { kind: "chart", chart_id: "c1", x: 6, y: 0, w: 6, h: 7 },
      { kind: "text", body: "Notes", x: 0, y: 7, w: 12, h: 2 },
    ])
  })

  it("keeps a saved tile's id as its key and gives a chart tile empty text", () => {
    const [text, chart] = draftTiles(saved)
    expect(text).toMatchObject({ key: "t2", id: "t2", body: "Notes" })
    expect(chart).toMatchObject({ key: "t1", id: "t1", chart_id: "c1", body: "" })
  })
})
