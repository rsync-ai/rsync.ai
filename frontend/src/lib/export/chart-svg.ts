/**
 * One chart drawn to SVG on the server, for a dashboard's PDF and a workflow's email
 * (dashboards-design.md § 8, D3). The option is the screen's (chartOption) as a still
 * picture (stillOption: no tooltip, zoom, toolbox or brush, no animation); the SVG is
 * made printable (printable-svg.ts). There is no canvas
 * on the server to measure text with, so the caller measures it in the font the PDF
 * embeds — the labels are laid out for the letters they are printed in. Server only.
 */

import {
  BarChart,
  BoxplotChart,
  CandlestickChart,
  FunnelChart,
  HeatmapChart,
  LineChart,
  PieChart,
  SankeyChart,
  ScatterChart,
  SunburstChart,
  TreemapChart,
} from "echarts/charts"
import { CalendarComponent, GridComponent, VisualMapPiecewiseComponent } from "echarts/components"
import { init, setPlatformAPI, use as registerModules } from "echarts/core"
import { LabelLayout } from "echarts/features"
import { SVGRenderer } from "echarts/renderers"

import { chartOption, stillOption } from "@/components/charts/chart-option"
// The map series and the world and US-state shapes it fills (the page loads them with its first map).
import "@/components/charts/echart-maps"
import type { ChartSettings } from "@/components/charts/chart-settings"
import type { ChartTheme } from "@/components/charts/chart-theme"
import { printableSvg } from "@/lib/export/printable-svg"

registerModules([
  BarChart,
  LineChart,
  HeatmapChart,
  PieChart,
  FunnelChart,
  ScatterChart,
  TreemapChart,
  SunburstChart,
  SankeyChart,
  BoxplotChart,
  CandlestickChart,
  GridComponent,
  CalendarComponent,
  VisualMapPiecewiseComponent,
  // A funnel's labels beside it (labelLayout), as on the page (echart-shapes.ts).
  LabelLayout,
  SVGRenderer,
])

/** A string's width in points, in `size` points of the regular or the bold face. */
export type MeasureText = (text: string, size: number, bold: boolean) => number

const DEFAULT_SIZE = 12

/** A CSS font string as zrender writes it ("normal 600 11px Inter"): its size and weight. */
export function parseFont(font: string | undefined): { size: number; bold: boolean } {
  const size = /(\d+(?:\.\d+)?)px/.exec(font ?? "")
  return { size: size ? Number(size[1]) : DEFAULT_SIZE, bold: /\b(?:bold|bolder|[6-9]00)\b/.test(font ?? "") }
}

export function chartSvg(
  settings: ChartSettings,
  rows: readonly object[],
  theme: ChartTheme,
  size: { width: number; height: number },
  measure: MeasureText,
): string {
  // The platform is ECharts' own, for every chart in this process; a draw is
  // synchronous from here to dispose, so the measure set here is the one it uses.
  // zrender keeps each string's width per font string for the life of the process
  // (zrender/lib/contain/text.js), so a measure must give one text in one font the
  // same width every time — this one does: every PDF embeds the same Inter.
  setPlatformAPI({
    measureText: (text, font) => {
      const { size: px, bold } = parseFont(font)
      return { width: measure(text ?? "", px, bold) }
    },
  })
  const chart = init(null, undefined, { renderer: "svg", ssr: true, width: size.width, height: size.height })
  try {
    // The option measures with the same widths, so a name it fits is one zrender draws whole.
    chart.setOption(stillOption(chartOption(settings, rows, theme, size.width, size.height, (text, px) => measure(text, px, false))))
    return printableSvg(chart.renderToSVGString())
  } finally {
    chart.dispose()
  }
}
