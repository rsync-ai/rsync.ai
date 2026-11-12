/**
 * A workflow's chart as a PNG for the email its send step mails (dashboards-design.md
 * § 8, D3), drawn on the server from the rows the gateway read as the run's user. It is
 * the drawing the chart's Download PNG makes (chart-image.ts composeChartImage): the
 * chart's name, its plot in the light print theme and its legend, 600 px wide — the
 * width the email shows it at — and turned into pixels by resvg at twice that size.
 * The plot is chart-svg.ts's, its labels measured in Inter as the PDF measures them,
 * and resvg draws with the Inter files only, never a system font, so the letters it
 * draws are the ones the labels were laid out for, on any host. Server only.
 */

import path from "node:path"

import { Resvg } from "@resvg/resvg-js"
import PDFDocument from "pdfkit"

import { chartLegend } from "@/components/charts/chart-option"
import { CHART_REGISTRY, isStatic, NOT_STATIC } from "@/components/charts/chart-registry"
import { savedChartView } from "@/components/charts/saved-chart"
import type { ChartResult, ChartType, ServerChartSettings } from "@/lib/api/charts"
import { composeChartImage, type ChartImage, type TextWidth } from "@/lib/export/chart-image"
import { chartSvg } from "@/lib/export/chart-svg"
import { FONT_DIR } from "@/lib/export/dashboard-pdf"
import { printChartTheme, PRINT_FONT } from "@/lib/export/print-theme"

/** The plot's size: with composeChartImage's padding the image is 600 px wide. */
export const EMAIL_PLOT = { width: 568, height: 300 }
/** Pixels per image pixel, so the picture stays sharp on a high-density screen. */
export const SCALE = 2

/**
 * The types an email shows as a picture, as the gateway's chartPlotted reads them: every
 * type in the registry but a scorecard, a table and a pivot table, which go as text.
 */
export const PLOTTED_TYPES: readonly ChartType[] = CHART_REGISTRY.map((t) => t.type).filter(
  (t) => t !== "scorecard" && t !== "table" && t !== "pivot",
)

/** A chart this route does not draw: not a plotted type, or its rows give nothing to plot. */
export class NotPlotted extends Error {}

const REGULAR = "Inter-Regular.ttf"
const SEMIBOLD = "Inter-SemiBold.ttf"

/** Widths in the Inter files the PNG is drawn with, measured as the PDF measures them. */
function interWidth(fontDir: string): TextWidth {
  const doc = new PDFDocument({ autoFirstPage: false })
  doc.registerFont("Inter", path.join(fontDir, REGULAR))
  doc.registerFont("Inter-SemiBold", path.join(fontDir, SEMIBOLD))
  return (text, size, bold) => doc.font(bold ? "Inter-SemiBold" : "Inter").fontSize(size).widthOfString(text)
}

/** The chart laid out as Download PNG lays it out, at the email's width, as one SVG. */
export function emailChartImage(name: string, settings: ServerChartSettings, result: ChartResult, fontDir = FONT_DIR): ChartImage {
  if (!PLOTTED_TYPES.includes(settings.type)) {
    throw new NotPlotted(`A ${settings.type} chart goes in the message as text, not as a picture.`)
  }
  if (!isStatic(settings.type)) throw new NotPlotted(NOT_STATIC)
  const view = savedChartView(settings, result)
  if (view.kind !== "echart") {
    throw new NotPlotted(view.kind === "message" ? view.message : "This chart has nothing to plot.")
  }
  const theme = printChartTheme()
  const measure = interWidth(fontDir)
  return composeChartImage({
    title: name,
    chart: chartSvg(view.settings, view.rows, theme, EMAIL_PLOT, measure),
    plot: EMAIL_PLOT,
    legend: chartLegend(view.settings, theme, view.rows),
    theme,
    measure,
  })
}

/** The chart as a PNG: emailChartImage drawn by resvg in Inter, at SCALE times its size. */
export function emailChartPng(name: string, settings: ServerChartSettings, result: ChartResult, fontDir = FONT_DIR): Buffer {
  const image = emailChartImage(name, settings, result, fontDir)
  const png = new Resvg(image.svg, {
    fitTo: { mode: "zoom", value: SCALE },
    font: {
      loadSystemFonts: false,
      fontFiles: [path.join(fontDir, REGULAR), path.join(fontDir, SEMIBOLD)],
      defaultFontFamily: PRINT_FONT,
    },
    logLevel: "off",
  })
  return png.render().asPng()
}
