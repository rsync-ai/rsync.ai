/**
 * The colours a chart is drawn with, read from the tokens in globals.css, so a
 * chart follows light and dark and its settings never name a colour
 * (dashboards-design.md § 2). No ECharts import: this file is safe anywhere.
 */

/** Run outcomes. Reserved for outcomes: a chart of anything else uses the series colours. */
export const STATUS_COLORS = ["succeeded", "failed", "error", "waiting", "running", "cancelled", "skipped"] as const

/**
 * The seven categorical colours, in the fixed order series take them (Blue, Orange,
 * Sky, Ochre, Pink, Violet, Green). An eighth series is never a new colour: it
 * folds into "Other".
 */
export const SERIES_COLORS = ["series-1", "series-2", "series-3", "series-4", "series-5", "series-6", "series-7"] as const

/** The `--chart-*` tokens in globals.css. */
export const CHART_COLORS = [...STATUS_COLORS, ...SERIES_COLORS, "other"] as const
export type ChartColor = (typeof CHART_COLORS)[number]

/**
 * The light series colours, which a chart uses when a token it needs is unset,
 * rather than give one series another's colour.
 */
export const FALLBACK_COLORS = ["#5071de", "#de7b2a", "#039fd4", "#b19001", "#f75e87", "#795eb1", "#1ca77f"] as const

export interface ChartTheme {
  colors: Record<ChartColor, string>
  text: string
  muted: string
  border: string
  /** The card a chart sits on: the gap between stacked bars. */
  surface: string
  popover: string
  popoverText: string
  font: string
}

/** A globals.css token ("142 76% 36%") as a colour ECharts reads; "" when it is unset. */
export function hslToken(raw: string): string {
  const parts = raw.trim().split(/\s+/)
  return parts.length === 3 ? `hsl(${parts.join(", ")})` : ""
}

/**
 * The tokens as the page has them now. EChart reads them when the class on <html>
 * changes, not when next-themes' state does: next-themes sets the class in an
 * effect that runs after the chart's.
 */
export function readChartTheme(root: HTMLElement = document.documentElement): ChartTheme {
  const style = getComputedStyle(root)
  const token = (name: string) => hslToken(style.getPropertyValue(name))
  const colors = {} as Record<ChartColor, string>
  for (const c of CHART_COLORS) colors[c] = token(`--chart-${c}`)
  return {
    colors,
    text: token("--foreground"),
    muted: token("--muted-foreground"),
    border: token("--border"),
    surface: token("--card"),
    popover: token("--popover"),
    popoverText: token("--popover-foreground"),
    font: getComputedStyle(document.body).fontFamily,
  }
}
