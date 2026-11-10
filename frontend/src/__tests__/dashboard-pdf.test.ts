// @vitest-environment node
import fs from "node:fs"
import path from "node:path"

import { BarChart } from "echarts/charts"
import { GridComponent } from "echarts/components"
import { init, use as registerModules } from "echarts/core"
import { SVGRenderer } from "echarts/renderers"
import PDFDocument from "pdfkit"
import SVGtoPDF from "svg-to-pdfkit"
import { afterEach, describe, expect, it, vi } from "vitest"

import type { ChartRecord, ChartResult, ServerChartSettings } from "@/lib/api/charts"
import type { DashboardFilter, DashboardTile, TileDraw } from "@/lib/api/dashboards"
import { chartSvg } from "@/lib/export/chart-svg"
import { dashboardPdf, filterLines, FONT_DIR, pdfTileOf, PDF_PAGE, type PdfTile } from "@/lib/export/dashboard-pdf"
import { layoutTiles } from "@/lib/export/pdf-layout"
import { LIGHT_TOKENS, printChartTheme, tokenHex } from "@/lib/export/print-theme"
import { printableSvg } from "@/lib/export/printable-svg"
import { levelRole, pivotRole, pivotTotalRole, ROLE_SECOND_MEASURE, savedChartView } from "@/components/charts/saved-chart"
import { chartSpec, NOT_STATIC } from "@/components/charts/chart-registry"

// A dashboard as a PDF (dashboards-design.md § 8, D3): the tiles where the grid puts
// them, each from its own draw, charts as vectors, everything in the embedded Inter.

const result = (columns: [alias: string, role: string][], rows: unknown[][]): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked: [],
  unfiltered: [],
  sql: "",
  read_at: "",
  execution_time_ms: 0,
})

const record = (name: string, settings: ServerChartSettings): ChartRecord => ({
  id: name,
  name,
  source_state: "ok",
  settings,
  settings_version: 2,
  source_columns: [],
  visibility: "workspace",
  proposed_by: "",
  mine: true,
  created_at: "",
  updated_at: "",
})

const draw = (chart: ChartRecord, r: ChartResult, over: Partial<TileDraw> = {}): TileDraw => ({
  chart,
  result: r,
  break: null,
  badges: null,
  filters: { applied: [], ignored: [], unfiltered: [] },
  ...over,
})

const XY: [string, string][] = [
  ["x", "the x"],
  ["s", "the series"],
  ["v", "the measure"],
]
const XY_ROWS = ["2026-10-01", "2026-10-02", "2026-10-03"].flatMap((d, i) => [
  [d, "EU", String(10 + i)],
  [d, "US", String(20 - i)],
])
const BAR = record("Orders by region", {
  version: 2,
  type: "bar",
  roles: { x: [{ column: "day" }], measure: [{ aggregate: "sum", column: "amount" }], series: [{ column: "region" }] },
})
const CARD = record("Orders this week", {
  version: 2,
  type: "scorecard",
  roles: { measure: [{ aggregate: "count" }], date: [{ column: "day" }] },
  compare: { days: 7 },
})
const TABLE = record("Every order", { version: 2, type: "table", roles: { columns: [{ column: "id" }, { column: "region" }] } })
const TABLE_ROWS = Array.from({ length: 60 }, (_, i) => [String(i + 1), i % 2 ? "EU" : "US"])

const FILTERS: DashboardFilter[] = [
  { id: "f1", kind: "date_range", label: "Day", column: "day" },
  { id: "f2", kind: "values", label: "Region", column: "region" },
]

const tile = (id: string, over: Partial<DashboardTile>): DashboardTile => ({ id, kind: "chart", x: 0, y: 0, w: 6, h: 6, ...over })

/** The board the smoke test prints: a chart, a number, a long table, a note, a hidden chart, a broken one. */
function boardTiles(): PdfTile[] {
  return [
    pdfTileOf(tile("bar", { chart: BAR, chart_id: BAR.id, x: 0, y: 0, w: 8, h: 7 }), draw(BAR, result(XY, XY_ROWS)), null, FILTERS),
    pdfTileOf(
      tile("card", { chart: CARD, chart_id: CARD.id, x: 8, y: 0, w: 4, h: 3 }),
      draw(CARD, result([["count", "the measure"], ["previous", "the earlier period"]], [[120, "100"]])),
      null,
      FILTERS,
    ),
    pdfTileOf(tile("note", { kind: "text", body: "Weekly numbers for the ops review.", x: 8, y: 3, w: 4, h: 4 }), null, null, FILTERS),
    pdfTileOf(
      tile("table", { chart: TABLE, chart_id: TABLE.id, x: 0, y: 7, w: 6, h: 6 }),
      draw(TABLE, result([["id", "table column id"], ["region", "table column region"]], TABLE_ROWS), {
        filters: { applied: ["f1"], ignored: ["f2"], unfiltered: [] },
      }),
      null,
      FILTERS,
    ),
    pdfTileOf(tile("hidden", { chart_id: "someone-elses", x: 6, y: 7, w: 6, h: 3 }), null, null, FILTERS),
    pdfTileOf(tile("broken", { chart: BAR, chart_id: BAR.id, x: 6, y: 10, w: 6, h: 3 }), null, "the gateway timed out", FILTERS),
    // Far enough down the grid to need a second page.
    pdfTileOf(tile("late", { chart: BAR, chart_id: BAR.id, x: 0, y: 13, w: 12, h: 8 }), draw(BAR, result(XY, XY_ROWS)), null, FILTERS),
  ]
}

afterEach(() => {
  vi.restoreAllMocks()
})

// dashboards-design.md § 14.5: a tile whose type the registry marks not static prints a
// placeholder in its place. Every type is static today, so the test marks bar not static.
describe("a tile the server does not draw", () => {
  it("says it is drawn only in rsync.ai", () => {
    const bar = chartSpec("bar")!
    bar.static = false
    try {
      const t = pdfTileOf(tile("bar", { chart: BAR, chart_id: BAR.id }), draw(BAR, result(XY, XY_ROWS)), null, FILTERS)
      expect(t.content).toEqual({ kind: "message", message: NOT_STATIC })
      expect(t.title).toBe(BAR.name)
    } finally {
      bar.static = true
    }
    expect(pdfTileOf(tile("bar", { chart: BAR, chart_id: BAR.id }), draw(BAR, result(XY, XY_ROWS)), null, FILTERS).content.kind).toBe("view")
  })
})

describe("dashboardPdf", () => {
  it("prints every tile's title, in Inter, on as many pages as the grid needs", async () => {
    const written: string[] = []
    const text = PDFDocument.prototype.text
    vi.spyOn(PDFDocument.prototype, "text").mockImplementation(function (this: PDFKit.PDFDocument, ...args: unknown[]) {
      if (typeof args[0] === "string") written.push(args[0])
      return (text as (...a: unknown[]) => PDFKit.PDFDocument).apply(this, args)
    })
    const tiles = boardTiles()
    const pdf = await dashboardPdf(
      { name: "Ops review", filters: ["Day: 2026-10-01 to 2026-10-07"], generatedAt: new Date("2026-10-06T20:10:00Z"), tiles },
      { compress: false },
    )
    const raw = pdf.toString("latin1")
    expect(raw.startsWith("%PDF-")).toBe(true)

    const pages = layoutTiles(tiles, PDF_PAGE).pages
    expect(pages).toBe(2)
    expect(raw.match(/\/Type \/Page\b(?!s)/g)?.length).toBe(pages)

    // Both faces embedded (a subset's name is "ABCDEF+Inter-…"), and pdfkit's Helvetica never.
    expect(raw).toMatch(/\/FontName \/[A-Z]{6}\+Inter-Regular/)
    expect(raw).toMatch(/\/FontName \/[A-Z]{6}\+Inter-SemiBold/)
    expect(raw).not.toMatch(/Helvetica/)

    for (const title of ["Ops review", "Orders by region", "Orders this week", "Every order", "A chart you cannot see"]) {
      expect(written).toContain(title)
    }
    expect(written).toContain("Weekly numbers for the ops review.")
    expect(written).toContain("Page 1 of 2")
    expect(written).toContain("Page 2 of 2")
    expect(written.some((t) => t.startsWith("Filters: Day: 2026-10-01 to 2026-10-07") && t.endsWith("Generated 2026-10-06 20:10 UTC"))).toBe(true)
    // The number, its comparison, the long table cut short, and the chart's legend.
    expect(written).toContain("120")
    expect(written.some((t) => /^… \d+ more rows$/.test(t))).toBe(true)
    expect(written).toEqual(expect.arrayContaining(["EU", "US"]))
    expect(written).toContain("This chart is private to its author.")
    expect(written).toContain("This tile could not be drawn: the gateway timed out")
  })
})

describe("a chart in the PDF", () => {
  it("says under a treemap how many of its parts are under a pixel, as the screen does", async () => {
    const written: string[] = []
    const text = PDFDocument.prototype.text
    vi.spyOn(PDFDocument.prototype, "text").mockImplementation(function (this: PDFKit.PDFDocument, ...args: unknown[]) {
      if (typeof args[0] === "string") written.push(args[0])
      return (text as (...a: unknown[]) => PDFKit.PDFDocument).apply(this, args)
    })
    const TREE = record("Rows by merchant", { version: 2, type: "treemap", roles: { rows: [{ column: "merchant" }], measure: [{ aggregate: "count" }] } })
    // One merchant of a million rows and 199 of one row each.
    const rows = Array.from({ length: 200 }, (_, i) => [`Merchant ${i}`, String(i === 0 ? 1_000_000 : 1)])
    const cols: [string, string][] = [["merchant", levelRole(1)], ["count", "the measure"]]
    const tiles = [
      pdfTileOf(tile("steep", { chart: TREE, chart_id: TREE.id, w: 6, h: 6 }), draw(TREE, result(cols, rows)), null, FILTERS),
      pdfTileOf(tile("even", { chart: TREE, chart_id: TREE.id, y: 6, w: 6, h: 6 }), draw(TREE, result(cols, rows.slice(0, 1))), null, FILTERS),
    ]
    await dashboardPdf({ name: "Merchants", filters: [], generatedAt: new Date("2026-10-13T09:00:00Z"), tiles }, { compress: false })
    expect(written.filter((t) => t === "199 parts are under a pixel at this size, too small to see.")).toHaveLength(1)
  })

  it("is scaled into its tile, its text with it", async () => {
    // The chart is drawn at 1/0.75 of its plot and fitted back, so an 11 px label prints at about 8 pt.
    const transform = vi.spyOn(PDFDocument.prototype, "transform")
    const tiles = [pdfTileOf(tile("bar", { chart: BAR, chart_id: BAR.id, w: 12, h: 8 }), draw(BAR, result(XY, XY_ROWS)), null, [])]
    await dashboardPdf({ name: "One chart", filters: [], generatedAt: new Date(), tiles })
    const scales = (transform.mock.calls as unknown as number[][]).filter(([a, b, c, d]) => b === 0 && c === 0 && Math.abs(a - 0.75) < 0.001 && Math.abs(d - 0.75) < 0.001)
    expect(scales.length).toBe(1)
  })
})

// D4b (dashboards-design.md § 14.9): a scorecard's sparkline, as the page draws it.
describe("a scorecard in the PDF", () => {
  const SPARK = record("Orders, last 7 days", { ...CARD.settings, sparkline: true })
  const sparkTile = (h: number, spark: unknown[][] | undefined) =>
    pdfTileOf(
      tile("card", { chart: SPARK, chart_id: SPARK.id, w: 4, h }),
      draw(SPARK, { ...result([["count", "the measure"], ["previous", "the earlier period"]], [[120, "100"]]), spark }),
      null,
      [],
    )
  const POINTS = [["2026-10-01", 5], ["2026-10-02", 9], ["2026-10-03", -2], ["2026-10-04", -6]]

  /** The lines the PDF draws in the first series colour, as [x, y] points. */
  async function sparkLines(tiles: PdfTile[]) {
    // A spy over a spy calls itself: each print starts from pdfkit's own methods.
    vi.restoreAllMocks()
    const seriesOne = printChartTheme().colors["series-1"]
    const lines: number[][][] = []
    let open: number[][] = []
    const proto = PDFDocument.prototype as unknown as Record<string, (...a: unknown[]) => unknown>
    for (const name of ["moveTo", "lineTo"] as const) {
      const real = proto[name]
      vi.spyOn(proto, name).mockImplementation(function (this: unknown, ...args: unknown[]) {
        if (name === "moveTo") open = []
        open.push(args as number[])
        return real.apply(this, args)
      })
    }
    const stroke = proto.strokeColor
    vi.spyOn(proto, "strokeColor").mockImplementation(function (this: unknown, ...args: unknown[]) {
      if (args[0] === seriesOne && open.length > 0) lines.push(open)
      return stroke.apply(this, args)
    })
    await dashboardPdf({ name: "Cards", filters: [], generatedAt: new Date(), tiles })
    return lines
  }

  it("draws the current period as a line under the change, from the lowest value up", async () => {
    const [line, ...more] = await sparkLines([sparkTile(4, POINTS)])
    expect(more).toEqual([])
    expect(line).toHaveLength(4)
    const ys = line.map(([, y]) => y)
    // y grows down the page: the lowest value is lowest, and two below zero are apart.
    expect(ys[3]).toBe(Math.max(...ys))
    expect(ys[3]).toBeGreaterThan(ys[2])
    expect(ys[1]).toBe(Math.min(...ys))
    // It spans its 14 pt box, lowest value to highest, and never runs out of it.
    expect(Math.max(...ys) - Math.min(...ys)).toBeCloseTo(14)
  })

  // Spaced a point apart, a day with no rows printed the days either side of it a day apart.
  it("puts a day with no rows back as a break, its days a day apart, a lone value a dot", async () => {
    vi.restoreAllMocks()
    const seriesOne = printChartTheme().colors["series-1"]
    const proto = PDFDocument.prototype as unknown as Record<string, (...a: unknown[]) => unknown>
    let paths: number[][][] = []
    let printed: number[][][] = []
    for (const name of ["moveTo", "lineTo"] as const) {
      const real = proto[name]
      vi.spyOn(proto, name).mockImplementation(function (this: unknown, ...args: unknown[]) {
        if (name === "moveTo") paths.push([])
        paths[paths.length - 1]?.push(args as number[])
        return real.apply(this, args)
      })
    }
    for (const name of ["stroke", "fill", "clip", "fillAndStroke"] as const) {
      const real = proto[name]
      vi.spyOn(proto, name).mockImplementation(function (this: unknown, ...args: unknown[]) {
        paths = []
        return real.apply(this, args)
      })
    }
    const color = proto.strokeColor
    vi.spyOn(proto, "strokeColor").mockImplementation(function (this: unknown, ...args: unknown[]) {
      if (args[0] === seriesOne && paths.length > 0) printed = paths
      return color.apply(this, args)
    })
    const gappy = [["2026-10-01", 5], ["2026-10-02", 9], ["2026-10-04", -6], ["2026-10-06", 3], ["2026-10-07", 4]]
    await dashboardPdf({ name: "Cards", filters: [], generatedAt: new Date(), tiles: [sparkTile(4, gappy)] })
    // Three runs: the 1st and 2nd, the 4th alone, the 6th and 7th; the 3rd and the 5th are breaks.
    expect(printed.map((p) => p.length)).toEqual([2, 2, 2])
    const xs = printed.map((p) => p.map(([x]) => x))
    const day = xs[0][1] - xs[0][0]
    expect(xs[1][0] - xs[0][1]).toBeCloseTo(2 * day)
    expect(xs[1][1]).toBe(xs[1][0])
    expect(xs[2][0] - xs[1][0]).toBeCloseTo(2 * day)
  })

  it("leaves it out with fewer than two points, or in a tile with no room under the change", async () => {
    expect(await sparkLines([sparkTile(4, POINTS.slice(0, 1))])).toEqual([])
    expect(await sparkLines([sparkTile(4, undefined)])).toEqual([])
    // A 3-row tile prints the number and its change, with no room left under them.
    expect(await sparkLines([sparkTile(3, POINTS)])).toEqual([])
  })
})

// D4c (dashboards-design.md § 14.9): a pivot table's totals print in bold, as the page draws them.
describe("a pivot table in the PDF", () => {
  it("prints its total row in the bold face and its other rows in the regular one", async () => {
    const PIVOT = record("Revenue by region", {
      version: 2,
      type: "pivot",
      roles: {
        rows: [{ column: "region", title: "Region" }],
        measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }, { aggregate: "avg", column: "amount", format: "decimal" }],
      },
    })
    const r = result(
      [["region", pivotRole("row", 1)], ["amount", "the measure"], ["amount_2", ROLE_SECOND_MEASURE], ["rsync_total_r1", pivotTotalRole("row", 1)]],
      [[null, "1070", "118.88888888888889", 1], ["EU", "500", "100.0", 0], ["US", "570", "142.5", 0]],
    )
    // The face each line of text is written in.
    const faces = new Map<string, string>()
    let face = ""
    const proto = PDFDocument.prototype as unknown as Record<string, (...a: unknown[]) => unknown>
    const font = proto.font
    vi.spyOn(proto, "font").mockImplementation(function (this: unknown, ...args: unknown[]) {
      face = args[0] as string
      return font.apply(this, args)
    })
    const text = proto.text
    vi.spyOn(proto, "text").mockImplementation(function (this: unknown, ...args: unknown[]) {
      if (typeof args[0] === "string") faces.set(args[0], face)
      return text.apply(this, args)
    })
    const tiles = [pdfTileOf(tile("pivot", { chart: PIVOT, chart_id: PIVOT.id, w: 6, h: 5 }), draw(PIVOT, r), null, [])]
    await dashboardPdf({ name: "Pivot", filters: [], generatedAt: new Date(), tiles })
    // Control: the rows were printed, so a table that printed nothing cannot pass.
    expect([...faces.keys()]).toEqual(expect.arrayContaining(["EU", "US", "Total", "1,070", "118.9"]))
    expect([faces.get("Total"), faces.get("1,070"), faces.get("118.9")]).toEqual(["Inter-SemiBold", "Inter-SemiBold", "Inter-SemiBold"])
    expect([faces.get("EU"), faces.get("500"), faces.get("US"), faces.get("142.5")]).toEqual(["Inter", "Inter", "Inter", "Inter"])
  })
})

describe("pdfTileOf", () => {
  const t = tile("t", { chart: BAR, chart_id: BAR.id })

  it("says why a tile has no chart to print, as the screen does", () => {
    const message = (p: PdfTile) => (p.content.kind === "message" ? p.content.message : p.content.kind)
    expect(message(pdfTileOf(t, draw(BAR, result(XY, []), { break: { reason: "column_missing", message: "day is gone" } }), null, []))).toBe(
      "This chart cannot be drawn: day is gone",
    )
    expect(message(pdfTileOf(t, draw(BAR, result(XY, []), { error: "relation does not exist" }), null, []))).toBe(
      "The source did not run: relation does not exist",
    )
    expect(message(pdfTileOf(t, draw(BAR, result(XY, []), { result: null }), null, []))).toBe("This tile has no rows.")
    expect(pdfTileOf(tile("h", {}), null, null, []).title).toBe("A chart you cannot see")
  })

  it("keeps the screen's warnings with the chart, by the filter's label", () => {
    const p = pdfTileOf(t, draw(BAR, result(XY, XY_ROWS), { filters: { applied: [], ignored: ["f2"], unfiltered: [] } }), null, FILTERS)
    expect(p.content.kind === "view" && p.content.notes).toEqual(["Not filtered by Region: this chart's source has no such column."])
    // The server names each picked filter its connection could not take by id, as the
    // result's columns would name Created too, on day but not picked.
    const created: DashboardFilter = { id: "f3", kind: "date_range", label: "Created", column: "day" }
    const without = { filters: { applied: [], ignored: [], unfiltered: ["f2", "f1"] } }
    const unfiltered = pdfTileOf(t, draw(BAR, { ...result(XY, XY_ROWS), unfiltered: ["region", "day"] }, without), null, [...FILTERS, created])
    expect(unfiltered.content.kind === "view" && unfiltered.content.notes).toEqual(["Not filtered by Region, Day: this connection cannot take a filter."])
  })
})

describe("filterLines", () => {
  it("writes each picked filter in the dashboard's order, at most five values", () => {
    const picks = { f2: { values: ["a", "b", "c", "d", "e", "f", "g"] }, f1: { from: "2026-10-01", to: "2026-10-07" } }
    expect(filterLines(FILTERS, picks)).toEqual(["Day: 2026-10-01 to 2026-10-07", "Region: a, b, c, d, e and 2 more"])
    expect(filterLines(FILTERS, {})).toEqual([])
  })
})

describe("the printed chart", () => {
  it("is drawn in the light theme, whatever the screen's is", () => {
    const css = fs.readFileSync(path.join(process.cwd(), "src/app/globals.css"), "utf8")
    const root = /:root\s*\{([^}]*)\}/.exec(css)?.[1] ?? ""
    for (const [name, value] of Object.entries(LIGHT_TOKENS)) {
      expect(new RegExp(`${name}:\\s*${value.replace(/[.%]/g, "\\$&")};`).test(root), name).toBe(true)
    }
    expect(tokenHex("0 0% 100%")).toBe("#ffffff")
    expect(tokenHex("0 72% 51%")).toBe("#dc2828")
    expect(printChartTheme().font).toBe("Inter")
  })

  it("measures its text in the font the PDF embeds", () => {
    // zrender keeps a string's width for the life of the process, so these days are
    // ones no other test draws.
    const rows = ["2031-01-01", "2031-01-02"].map((d) => [d, "EU", "4"])
    const v = savedChartView(BAR.settings, result(XY, rows))
    if (v.kind !== "echart") throw new Error(v.kind)
    const measured: [string, number, boolean][] = []
    chartSvg(v.settings, v.rows, printChartTheme(), { width: 400, height: 240 }, (text, size, bold) => {
      measured.push([text, size, bold])
      return text.length * size * 0.6
    })
    expect(measured.length).toBeGreaterThan(0)
    expect(measured.some(([text, size]) => text === "2031-01-01" && size === 11)).toBe(true)
  })
})

describe("printableSvg", () => {
  // No chart type today labels its marks, so the outline comes from a bar with its
  // values inside — what ECharts writes for any label on a shape.
  registerModules([BarChart, GridComponent, SVGRenderer])
  const labelledSvg = () => {
    const chart = init(null, undefined, { renderer: "svg", ssr: true, width: 300, height: 160 })
    try {
      chart.setOption({
        animation: false,
        xAxis: { type: "category", data: ["a", "b"] },
        yAxis: {},
        series: [{ type: "bar", data: [3, 5], label: { show: true, position: "inside" } }],
      })
      return chart.renderToSVGString()
    } finally {
      chart.dispose()
    }
  }
  const outlined = (svg: string) => (svg.match(/<text\b[^>]*>/g) ?? []).filter((t) => /\s(?:stroke|paint-order)=/.test(t))

  it("drops a label's outline, so the PDF fills its letters and never strokes over them", async () => {
    const svg = labelledSvg()
    expect(outlined(svg).length).toBeGreaterThan(0)
    const printable = printableSvg(svg)
    expect(outlined(printable)).toEqual([])
    // Every label is still there, letters and all.
    expect(printable.match(/<text\b/g)?.length).toBe(svg.match(/<text\b/g)?.length)

    const pdfOf = async (s: string) => {
      const doc = new PDFDocument({ compress: false, font: path.join(FONT_DIR, "Inter-Regular.ttf") })
      const chunks: Buffer[] = []
      const done = new Promise<string>((resolve) => doc.on("end", () => resolve(Buffer.concat(chunks).toString("latin1"))))
      doc.on("data", (c: Buffer) => chunks.push(c))
      SVGtoPDF(doc, s, 0, 0, { assumePt: true, fontCallback: () => path.join(FONT_DIR, "Inter-Regular.ttf") })
      doc.end()
      return done
    }
    // 2 Tr = fill, then stroke over the fill: the outline painted on top of the letters.
    expect(await pdfOf(svg)).toMatch(/\b2 Tr\b/)
    expect(await pdfOf(printable)).not.toMatch(/\b2 Tr\b/)
  })
})
