/**
 * A dashboard as a PDF (dashboards-design.md § 8, D3): A4 landscape, each tile where
 * the 12-column grid puts it (pdf-layout.ts), drawn from the rows the viewer's own
 * draw returned with the filters they had picked. A chart is the screen's ECharts
 * option drawn to SVG and then into the PDF as vectors; a number, a table and a text
 * tile are PDF text. Everything is set in Inter, embedded. Server only.
 */

import path from "node:path"

import PDFDocument from "pdfkit"
import SVGtoPDF from "svg-to-pdfkit"

import { chartLegend, sizeNote } from "@/components/charts/chart-option"
import { isStatic, NOT_STATIC } from "@/components/charts/chart-registry"
import { formatNumber } from "@/components/charts/chart-settings"
import type { ChartTheme } from "@/components/charts/chart-theme"
import { changeText, chartWarnings, savedChartView, type SavedChartView } from "@/components/charts/saved-chart"
import { ignoredLabels } from "@/components/dashboards/dashboard-layout"
import type { DashboardFilter, DashboardFilterValue, DashboardTile, TileDraw } from "@/lib/api/dashboards"
import { chartSvg, type MeasureText } from "@/lib/export/chart-svg"
import { A4_LANDSCAPE, layoutTiles, type GridTile, type PdfBox, type PdfPage } from "@/lib/export/pdf-layout"
import { printChartTheme } from "@/lib/export/print-theme"

/** Inter (OFL-1.1), from the official release's static TTFs; copied into the server build (next.config.ts). */
export const FONT_DIR = path.join(process.cwd(), "src/assets/fonts")
const REGULAR = "Inter"
const BOLD = "Inter-SemiBold"

export const PDF_PAGE: PdfPage = { ...A4_LANDSCAPE, margin: 28, headerHeight: 46, footerHeight: 18 }

/**
 * A chart's text against its box: the SVG is drawn this much larger than the box and
 * scaled into it, so an 11 px axis label prints at about 8 pt.
 */
const CHART_TEXT_SCALE = 0.75
const PAD = 8
const TITLE = 8.5
const SMALL = 7
const TABLE_TEXT = 7
const TABLE_ROW = 11
/** A scorecard's sparkline, in points. */
const SPARK_WIDTH = 120
const SPARK_HEIGHT = 14

export type PdfTileContent =
  | { kind: "text"; body: string }
  | { kind: "view"; view: SavedChartView; notes: string[] }
  | { kind: "message"; message: string }

export interface PdfTile extends GridTile {
  /** Empty for a text tile, which has none on screen either. */
  title: string
  content: PdfTileContent
}

export interface DashboardPdfInput {
  name: string
  /** The filters the viewer picked, in words ("Region: EU, US"); empty for none. */
  filters: string[]
  generatedAt: Date
  tiles: PdfTile[]
}

/** A dashboard tile and its draw (or the reason there is none) as the PDF prints it. */
export function pdfTileOf(tile: DashboardTile, draw: TileDraw | null, failure: string | null, filters: readonly DashboardFilter[]): PdfTile {
  const place = { x: tile.x, y: tile.y, w: tile.w, h: tile.h }
  if (tile.kind === "text") return { ...place, title: "", content: { kind: "text", body: tile.body ?? "" } }
  const chart = draw?.chart ?? tile.chart
  const title = chart?.name ?? "A chart you cannot see"
  const message = (text: string): PdfTile => ({ ...place, title, content: { kind: "message", message: text } })
  if (!tile.chart) return message("This chart is private to its author.")
  if (failure !== null || !draw) return message(`This tile could not be drawn: ${failure ?? "no answer"}`)
  if (draw.break) return message(`This chart cannot be drawn: ${draw.break.message}`)
  if (draw.error) return message(`The source did not run: ${draw.error}`)
  if (!draw.result || !chart) return message("This tile has no rows.")
  if (!isStatic(chart.settings.type)) return message(NOT_STATIC)
  const view = savedChartView(chart.settings, draw.result)
  const warnings = chartWarnings(
    draw,
    ignoredLabels(draw.filters.ignored, filters),
    chart.settings.type,
    ignoredLabels(draw.filters.unfiltered, filters),
  )
  const notes = [...warnings, ...("notes" in view ? view.notes : [])]
  return { ...place, title, content: { kind: "view", view, notes } }
}

/** The picked filters in words, in the dashboard's order. */
export function filterLines(filters: readonly DashboardFilter[], picks: Record<string, DashboardFilterValue>): string[] {
  const MAX_VALUES = 5
  return filters.flatMap((f) => {
    const p = picks[f.id]
    if (!p) return []
    if (p.from && p.to) return [`${f.label}: ${p.from} to ${p.to}`]
    if (p.values && p.values.length > 0) {
      const shown = p.values.slice(0, MAX_VALUES).join(", ")
      const more = p.values.length - MAX_VALUES
      return [`${f.label}: ${shown}${more > 0 ? ` and ${more} more` : ""}`]
    }
    return []
  })
}

/** "2026-10-06 20:10 UTC". */
export function generatedText(at: Date): string {
  return `${at.toISOString().slice(0, 16).replace("T", " ")} UTC`
}

export function dashboardPdf(input: DashboardPdfInput, opts: { compress?: boolean; fontDir?: string } = {}): Promise<Buffer> {
  const fontDir = opts.fontDir ?? FONT_DIR
  const regular = path.join(fontDir, "Inter-Regular.ttf")
  const doc = new PDFDocument({
    size: [PDF_PAGE.width, PDF_PAGE.height],
    margin: 0,
    bufferPages: true,
    compress: opts.compress ?? true,
    // Inter from the start, so pdfkit never loads its Helvetica metrics.
    font: regular,
    info: { Title: input.name, Creator: "rsync.ai", Producer: "rsync.ai" },
  })
  doc.registerFont(REGULAR, regular)
  doc.registerFont(BOLD, path.join(fontDir, "Inter-SemiBold.ttf"))

  const chunks: Buffer[] = []
  const done = new Promise<Buffer>((resolve, reject) => {
    doc.on("data", (c: Buffer) => chunks.push(c))
    doc.on("end", () => resolve(Buffer.concat(chunks)))
    doc.on("error", reject)
  })

  const theme = printChartTheme()
  const measure: MeasureText = (text, size, bold) => doc.font(bold ? BOLD : REGULAR).fontSize(size).widthOfString(text)
  const ctx: Ctx = { doc, theme, measure }

  header(ctx, input)
  const { placements, pages } = layoutTiles(input.tiles, PDF_PAGE)
  let page = 0
  for (const p of placements) {
    while (page < p.page) {
      doc.addPage()
      page += 1
    }
    tile(ctx, p.tile, p.box)
  }
  for (let i = 0; i < pages; i++) {
    doc.switchToPage(i)
    footer(ctx, input.name, i + 1, pages)
  }
  doc.end()
  return done
}

interface Ctx {
  doc: PDFKit.PDFDocument
  theme: ChartTheme
  measure: MeasureText
}

function header({ doc, theme }: Ctx, input: DashboardPdfInput) {
  const { margin } = PDF_PAGE
  const width = PDF_PAGE.width - 2 * margin
  doc.font(BOLD).fontSize(15).fillColor(theme.text)
  doc.text(fit(doc, input.name, width), margin, margin, { lineBreak: false })
  const filters = input.filters.length > 0 ? `Filters: ${input.filters.join(" · ")}` : "No filters"
  doc.font(REGULAR).fontSize(SMALL + 1).fillColor(theme.muted)
  doc.text(fit(doc, `${filters}   ·   Generated ${generatedText(input.generatedAt)}`, width), margin, margin + 22, {
    lineBreak: false,
  })
}

function footer({ doc, theme }: Ctx, name: string, n: number, of: number) {
  const { margin, width, height, footerHeight } = PDF_PAGE
  const y = height - margin - footerHeight + 8
  const right = `Page ${n} of ${of}`
  doc.font(REGULAR).fontSize(SMALL).fillColor(theme.muted)
  const rightWidth = doc.widthOfString(right)
  doc.text(right, width - margin - rightWidth, y, { lineBreak: false })
  doc.text(fit(doc, `${name} · rsync.ai`, width - 2 * margin - rightWidth - 24), margin, y, { lineBreak: false })
}

function tile(ctx: Ctx, t: PdfTile, box: PdfBox) {
  const { doc, theme } = ctx
  doc.save()
  doc.roundedRect(box.x, box.y, box.width, box.height, 4).fillAndStroke(theme.surface, theme.border)
  doc.restore()
  const inner = { x: box.x + PAD, y: box.y + PAD, width: box.width - 2 * PAD, height: box.height - 2 * PAD }
  if (t.content.kind === "text") {
    doc.font(REGULAR).fontSize(TITLE).fillColor(theme.text)
    doc.text(t.content.body, inner.x, inner.y, { width: inner.width, height: inner.height, ellipsis: true })
    return
  }
  doc.font(BOLD).fontSize(TITLE).fillColor(theme.text)
  doc.text(fit(doc, t.title, inner.width), inner.x, inner.y, { lineBreak: false })
  let body: PdfBox = { ...inner, y: inner.y + TITLE + 6, height: inner.height - TITLE - 6 }
  if (t.content.kind === "message") {
    muted(ctx, t.content.message, body)
    return
  }
  body = notes(ctx, t.content.notes, body)
  view(ctx, t.content.view, body)
}

/** The notes and warnings under a tile's body, at most two lines; returns the room left above them. */
function notes({ doc, theme }: Ctx, lines: string[], body: PdfBox): PdfBox {
  if (lines.length === 0) return body
  doc.font(REGULAR).fontSize(SMALL)
  const text = lines.join(" ")
  const height = Math.min(doc.heightOfString(text, { width: body.width }), 2 * (SMALL + 2.5))
  doc.fillColor(theme.muted).text(text, body.x, body.y + body.height - height, { width: body.width, height, ellipsis: true })
  return { ...body, height: body.height - height - 4 }
}

function muted({ doc, theme }: Ctx, text: string, body: PdfBox) {
  doc.font(REGULAR).fontSize(SMALL + 1).fillColor(theme.muted)
  doc.text(text, body.x, body.y, { width: body.width, height: Math.max(body.height, SMALL + 2), ellipsis: true })
}

function view(ctx: Ctx, v: SavedChartView, body: PdfBox) {
  switch (v.kind) {
    case "message":
      return muted(ctx, v.message, body)
    case "scorecard":
      return scorecard(ctx, v, body)
    case "table":
      return table(ctx, v, body)
    case "echart":
      if (v.rows.length === 0) return muted(ctx, "No rows to draw.", body)
      return echart(ctx, v, body)
  }
}

function scorecard({ doc, theme }: Ctx, v: Extract<SavedChartView, { kind: "scorecard" }>, body: PdfBox) {
  const label = v.label + (v.days !== undefined ? ` · last ${v.days === 1 ? "day" : `${v.days} days`}` : "")
  doc.font(REGULAR).fontSize(SMALL + 0.5).fillColor(theme.muted)
  doc.text(fit(doc, label, body.width), body.x, body.y, { lineBreak: false })
  const value = v.value === null ? "–" : formatNumber(v.value, v.format)
  let size = Math.max(10, Math.min(26, body.height * 0.45))
  doc.font(BOLD).fontSize(size)
  while (size > 10 && doc.widthOfString(value) > body.width) doc.fontSize((size -= 1))
  doc.fillColor(theme.text).text(fit(doc, value, body.width), body.x, body.y + SMALL + 5, { lineBreak: false })
  if (v.days === undefined) return
  const change =
    changeText(v.value, v.previous, v.days) +
    (v.previous !== null && v.previous !== undefined ? ` (${formatNumber(v.previous, v.format)})` : "")
  const y = body.y + SMALL + 5 + size * 1.25
  if (y + SMALL > body.y + body.height) return
  doc.font(REGULAR).fontSize(SMALL + 0.5).fillColor(theme.muted)
  doc.text(fit(doc, change, body.width), body.x, y, { lineBreak: false })
  // The sparkline under it, as the page draws it (kit/Sparkline): from 0, or from
  // the lowest value when one is below it, a period with no value a break and a lone
  // value a dot; left out when the tile has no room.
  const values = v.spark?.map((p) => p.value) ?? []
  const numbers = values.filter((n): n is number => n !== null)
  const top = y + SMALL + 6
  if (values.length < 2 || numbers.length === 0 || top + SPARK_HEIGHT > body.y + body.height) return
  const min = Math.min(...numbers, 0)
  const max = Math.max(...numbers, min + 1)
  const width = Math.min(body.width, SPARK_WIDTH)
  const step = width / (values.length - 1)
  doc.save()
  values.forEach((value, i) => {
    if (value === null) return
    const x = body.x + i * step
    const at = top + SPARK_HEIGHT - ((value - min) / (max - min)) * SPARK_HEIGHT
    const alone = (i === 0 || values[i - 1] === null) && (i === values.length - 1 || values[i + 1] === null)
    if (i === 0 || values[i - 1] === null) doc.moveTo(x, at)
    // A zero-length line with round caps prints as a dot.
    if (alone) doc.lineTo(x, at)
    else if (i > 0 && values[i - 1] !== null) doc.lineTo(x, at)
  })
  doc.lineWidth(1).lineJoin("round").lineCap("round").strokeColor(theme.colors["series-1"]).stroke().restore()
}

function table({ doc, theme }: Ctx, v: Extract<SavedChartView, { kind: "table" }>, body: PdfBox) {
  const cols = v.head.length
  if (cols === 0) return
  const width = body.width / cols
  const cell = (text: string, i: number, y: number, numeric: boolean) => {
    const shown = fit(doc, text, width - 4)
    const x = body.x + i * width + (numeric ? width - 2 - doc.widthOfString(shown) : 0)
    doc.text(shown, x, y, { lineBreak: false })
  }
  doc.font(BOLD).fontSize(TABLE_TEXT).fillColor(theme.muted)
  v.head.forEach((h, i) => cell(h, i, body.y, !!v.numeric[i]))
  let y = body.y + TABLE_ROW
  doc.save().moveTo(body.x, y - 2).lineTo(body.x + body.width, y - 2).lineWidth(0.5).strokeColor(theme.border).stroke().restore()
  const room = Math.floor((body.y + body.height - y) / TABLE_ROW)
  const fits = v.body.length <= room
  const shown = fits ? v.body : v.body.slice(0, Math.max(room - 1, 0))
  doc.fontSize(TABLE_TEXT).fillColor(theme.text)
  shown.forEach((row, r) => {
    // A pivot table's totals in bold, as on the page.
    doc.font(v.totals?.[r] ? BOLD : REGULAR)
    row.forEach((c, i) => cell(c, i, y, !!v.numeric[i]))
    y += TABLE_ROW
  })
  doc.font(REGULAR)
  if (!fits && room > 0) {
    const more = v.body.length - shown.length
    doc.fillColor(theme.muted).text(`… ${more.toLocaleString("en-US")} more ${more === 1 ? "row" : "rows"}`, body.x, y, { lineBreak: false })
  }
}

function echart(ctx: Ctx, v: Extract<SavedChartView, { kind: "echart" }>, body: PdfBox) {
  const { doc, theme, measure } = ctx
  const plot = noteUnder(ctx, v, legend(ctx, chartLegend(v.settings, theme, v.rows), body))
  if (plot.height < 20) return
  const svg = chartSvg(v.settings, v.rows, theme, { width: plot.width / CHART_TEXT_SCALE, height: plot.height / CHART_TEXT_SCALE }, measure)
  SVGtoPDF(doc, svg, plot.x, plot.y, {
    width: plot.width,
    height: plot.height,
    // Without it svg-to-pdfkit draws the SVG at its own width and height, larger than
    // the plot by CHART_TEXT_SCALE; with it the viewBox is fitted to the plot.
    preserveAspectRatio: "xMidYMid meet",
    assumePt: true,
    fontCallback: (_family, bold) => (bold ? BOLD : REGULAR),
  })
}

/** What the chart cannot draw at its size (sizeNote), under its plot as on the screen; returns the plot's room. */
function noteUnder({ doc, theme }: Ctx, v: Extract<SavedChartView, { kind: "echart" }>, room: PdfBox): PdfBox {
  const LINE = SMALL + 4
  if (room.height - LINE < 20) return room
  // Counted at the size the chart is drawn at below, the line under it taken off.
  const note = sizeNote(v.settings, v.rows, theme, room.width / CHART_TEXT_SCALE, (room.height - LINE) / CHART_TEXT_SCALE)
  if (!note) return room
  doc.font(REGULAR).fontSize(SMALL).fillColor(theme.muted)
  doc.text(fit(doc, note, room.width), room.x, room.y + room.height - LINE + 2, { lineBreak: false })
  return { ...room, height: room.height - LINE }
}

/** A chart's legend under its plot, wrapped like the screen's; returns the plot's room. */
function legend({ doc, theme }: Ctx, items: { label: string; color: string }[], body: PdfBox): PdfBox {
  if (items.length === 0) return body
  const SWATCH = 5
  const GAP = 8
  const LINE = SMALL + 4
  doc.font(REGULAR).fontSize(SMALL)
  const placed: { x: number; line: number; label: string; color: string }[] = []
  let x = 0
  let line = 0
  for (const item of items) {
    const label = fit(doc, item.label, body.width - SWATCH - 3)
    const w = SWATCH + 3 + doc.widthOfString(label)
    if (x > 0 && x + w > body.width) {
      line += 1
      x = 0
    }
    placed.push({ x, line, label, color: item.color })
    x += w + GAP
  }
  const lines = Math.min(line + 1, 3)
  const top = body.y + body.height - lines * LINE
  for (const p of placed) {
    if (p.line >= lines) break
    const y = top + p.line * LINE
    doc.save().rect(body.x + p.x, y + 1.5, SWATCH, SWATCH).fill(p.color).restore()
    doc.fillColor(theme.muted).text(p.label, body.x + p.x + SWATCH + 3, y, { lineBreak: false })
  }
  return { ...body, height: body.height - lines * LINE - 4 }
}

/** The text cut to fit a width in the current font, with an ellipsis. */
function fit(doc: PDFKit.PDFDocument, text: string, width: number): string {
  if (doc.widthOfString(text) <= width) return text
  let lo = 0
  let hi = text.length
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2)
    if (doc.widthOfString(text.slice(0, mid) + "…") <= width) lo = mid
    else hi = mid - 1
  }
  return lo === 0 ? "" : text.slice(0, lo).trimEnd() + "…"
}
