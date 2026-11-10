/**
 * A chart as an image, made in the browser (dashboards-design.md § 8, D3): the chart
 * drawn again at the size it has on the page, in the light theme a PDF prints in, with
 * its name above it and its legend under it, as one SVG. The PNG is that SVG drawn on
 * a canvas at twice its size. Drawing it again, rather than copying the page's SVG,
 * keeps the page's dark theme and its tooltip out of the file.
 *
 * An image cannot load the page's web font, so the chart names fonts a computer has —
 * Inter where it is installed, else the system's sans-serif — and ECharts measures its
 * labels in those same fonts, so they are laid out for the letters drawn. A label in a
 * script those fonts lack is drawn in whatever font the computer falls back to.
 *
 * Browser only, and only through import(): it carries ECharts. It leaves ECharts' text
 * measuring as it is (the page's charts share it), unlike the server's chart-svg.ts.
 * composeChartImage alone has no DOM: the server's email-chart.ts lays a workflow
 * email's chart out with it, so the email shows what Download PNG gives.
 */

import { BarChart, HeatmapChart, LineChart } from "echarts/charts"
import { GridComponent, VisualMapPiecewiseComponent } from "echarts/components"
import { init, use as registerModules } from "echarts/core"
import { SVGRenderer } from "echarts/renderers"

import { chartModules } from "@/components/charts/chart-modules"
import { chartLegend, chartOption, stillOption, type LegendItem } from "@/components/charts/chart-option"
import type { ChartSettings } from "@/components/charts/chart-settings"
import type { ChartTheme } from "@/components/charts/chart-theme"
import { printChartTheme } from "@/lib/export/print-theme"

// The modules every chart registers (EChart.tsx). A type's own load with its group
// (chart-modules.ts), the chunk the page drew the chart with: registered here too, a
// treemap's or a sankey's series would be a second copy in this file's chunk.
registerModules([BarChart, LineChart, HeatmapChart, GridComponent, VisualMapPiecewiseComponent, SVGRenderer])

/** The fonts an image's text names: no quotes, so it sits in an SVG attribute as it is. */
export const IMAGE_FONT = "Inter, system-ui, -apple-system, Segoe UI, Roboto, Helvetica Neue, Arial, sans-serif"

const PAD = 16
const TITLE_SIZE = 14
const TITLE_LINE = 20
const TITLE_GAP = 8
const LEGEND_SIZE = 11
const LEGEND_LINE = 16
const LEGEND_GAP = 8
const SWATCH = 8
const SWATCH_GAP = 6
const ITEM_GAP = 12

/** A string's width in pixels, at `size` pixels, regular or semibold. */
export type TextWidth = (text: string, size: number, bold: boolean) => number

export interface ChartImage {
  svg: string
  width: number
  height: number
}

const ENTITY: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }
const esc = (s: string) => s.replace(/[&<>"']/g, (c) => ENTITY[c])

/** The text cut to fit a width, with an ellipsis. */
function fit(text: string, width: number, size: number, bold: boolean, measure: TextWidth): string {
  if (measure(text, size, bold) <= width) return text
  let lo = 0
  let hi = text.length
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2)
    if (measure(text.slice(0, mid) + "…", size, bold) <= width) lo = mid
    else hi = mid - 1
  }
  return lo === 0 ? "" : text.slice(0, lo).trimEnd() + "…"
}

/**
 * The image: the chart's name, the chart's SVG at `plot` size, and its legend wrapped
 * under it as the page wraps it, on the theme's surface.
 */
export function composeChartImage({
  title,
  chart,
  plot,
  legend,
  theme,
  measure,
}: {
  title: string
  /** The chart, as ECharts writes it: one <svg> of `plot` size. */
  chart: string
  plot: { width: number; height: number }
  legend: LegendItem[]
  theme: ChartTheme
  measure: TextWidth
}): ChartImage {
  const width = Math.round(plot.width + 2 * PAD)
  const plotY = PAD + TITLE_LINE + TITLE_GAP

  const placed: { x: number; line: number; label: string; color: string }[] = []
  let x = 0
  let line = 0
  for (const item of legend) {
    const label = fit(item.label, plot.width - SWATCH - SWATCH_GAP, LEGEND_SIZE, false, measure)
    const w = SWATCH + SWATCH_GAP + measure(label, LEGEND_SIZE, false)
    if (x > 0 && x + w > plot.width) {
      line += 1
      x = 0
    }
    placed.push({ x, line, label, color: item.color })
    x += w + ITEM_GAP
  }
  const legendTop = plotY + plot.height + LEGEND_GAP
  const height = Math.round(placed.length > 0 ? legendTop + (line + 1) * LEGEND_LINE + PAD : plotY + plot.height + PAD)

  const parts = [
    // ECharts writes no aria description when it draws on the server, so the picture
    // names itself: a screen reader, or a mail client showing the SVG, reads the title.
    `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-label="${esc(title)}">`,
    `<title>${esc(title)}</title>`,
    `<rect width="${width}" height="${height}" fill="${esc(theme.surface)}"/>`,
    `<text x="${PAD}" y="${PAD + 15}" font-family="${IMAGE_FONT}" font-size="${TITLE_SIZE}" font-weight="600" fill="${esc(theme.text)}">${esc(fit(title, plot.width, TITLE_SIZE, true, measure))}</text>`,
    chart.replace(/^\s*<svg\b/, `<svg x="${PAD}" y="${plotY}"`),
  ]
  for (const p of placed) {
    const top = legendTop + p.line * LEGEND_LINE
    parts.push(
      `<rect x="${PAD + p.x}" y="${top + (LEGEND_LINE - SWATCH) / 2}" width="${SWATCH}" height="${SWATCH}" rx="2" fill="${esc(p.color)}"/>`,
      `<text x="${PAD + p.x + SWATCH + SWATCH_GAP}" y="${top + 12}" font-family="${IMAGE_FONT}" font-size="${LEGEND_SIZE}" fill="${esc(theme.muted)}">${esc(p.label)}</text>`,
    )
  }
  parts.push("</svg>")
  return { svg: parts.join(""), width, height }
}

/** Widths as this browser draws IMAGE_FONT. */
function canvasWidth(): TextWidth {
  const ctx = document.createElement("canvas").getContext("2d")
  return (text, size, bold) => {
    if (!ctx) return text.length * size * 0.6
    ctx.font = `${bold ? 600 : 400} ${size}px ${IMAGE_FONT}`
    return ctx.measureText(text).width
  }
}

/** The chart as an image, `plot` the size its plot has on the page, once its type's modules are loaded. */
export async function chartImage(
  title: string,
  settings: ChartSettings,
  rows: readonly object[],
  plot: { width: number; height: number },
  measure: TextWidth = canvasWidth(),
): Promise<ChartImage> {
  await chartModules(settings.type)
  const theme: ChartTheme = { ...printChartTheme(), font: IMAGE_FONT }
  const chart = init(null, undefined, { renderer: "svg", ssr: true, width: plot.width, height: plot.height })
  let svg: string
  try {
    // The tooltip and any zoom or toolbox are the page's; an image has no pointer.
    chart.setOption(stillOption(chartOption(settings, rows, theme, plot.width, plot.height)))
    svg = chart.renderToSVGString()
  } finally {
    chart.dispose()
  }
  return composeChartImage({ title, chart: svg, plot, legend: chartLegend(settings, theme, rows), theme, measure })
}

/** The image as a PNG, `scale` times its size so it stays sharp on a high-density screen or in print. */
export async function chartPng(image: ChartImage, scale = 2): Promise<Blob> {
  const img = new Image()
  img.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(image.svg)}`
  await img.decode()
  const canvas = document.createElement("canvas")
  canvas.width = Math.ceil(image.width * scale)
  canvas.height = Math.ceil(image.height * scale)
  const ctx = canvas.getContext("2d")
  if (!ctx) throw new Error("This browser cannot draw the image")
  ctx.scale(scale, scale)
  ctx.drawImage(img, 0, 0, image.width, image.height)
  return new Promise((resolve, reject) =>
    canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error("This browser could not make the PNG"))), "image/png"),
  )
}
