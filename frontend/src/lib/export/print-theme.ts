/**
 * The light theme a chart is printed in (dashboards-design.md § 8, D3): a PDF is paper,
 * so it is light whatever the viewer's screen is. The tokens are globals.css's :root
 * ones (a test holds them equal), written as hex because svg-to-pdfkit reads neither
 * hsl() nor CSS variables. No DOM: this file is safe anywhere.
 */

import { CHART_COLORS, type ChartColor, type ChartTheme } from "@/components/charts/chart-theme"

/** globals.css :root, as written there. */
export const LIGHT_TOKENS: Record<string, string> = {
  "--foreground": "240 10% 3.9%",
  "--muted-foreground": "240 3.8% 46.1%",
  "--border": "240 5.9% 90%",
  "--card": "0 0% 100%",
  "--popover": "0 0% 100%",
  "--popover-foreground": "240 10% 3.9%",
  "--chart-succeeded": "142 76% 36%",
  "--chart-failed": "0 72% 51%",
  "--chart-error": "25 95% 53%",
  "--chart-waiting": "38 92% 50%",
  "--chart-running": "221 83% 53%",
  "--chart-cancelled": "240 5% 65%",
  "--chart-skipped": "240 5% 84%",
  "--chart-series-1": "226.1 68.3% 59.2%",
  "--chart-series-2": "27 73.2% 51.8%",
  "--chart-series-3": "195.2 97.2% 42.2%",
  "--chart-series-4": "48.8 98.9% 34.9%",
  "--chart-series-5": "343.9 90.5% 66.9%",
  "--chart-series-6": "259.5 34.7% 53.1%",
  "--chart-series-7": "162.7 71.3% 38.2%",
  "--chart-other": "240 4.6% 57.6%",
}

/** The font a printed chart names; the PDF maps it to the embedded Inter. */
export const PRINT_FONT = "Inter"

/** A token ("226.1 68.3% 59.2%") as #rrggbb. */
export function tokenHex(token: string): string {
  const [h, s, l] = token.trim().split(/\s+/).map((p) => parseFloat(p))
  const sat = s / 100
  const light = l / 100
  const a = sat * Math.min(light, 1 - light)
  const channel = (n: number) => {
    const k = (n + h / 30) % 12
    const v = light - a * Math.max(-1, Math.min(k - 3, 9 - k, 1))
    return Math.round(v * 255)
      .toString(16)
      .padStart(2, "0")
  }
  return `#${channel(0)}${channel(8)}${channel(4)}`
}

export function printChartTheme(): ChartTheme {
  const hex = (name: string) => tokenHex(LIGHT_TOKENS[name])
  const colors = {} as Record<ChartColor, string>
  for (const c of CHART_COLORS) colors[c] = hex(`--chart-${c}`)
  return {
    colors,
    text: hex("--foreground"),
    muted: hex("--muted-foreground"),
    border: hex("--border"),
    surface: hex("--card"),
    popover: hex("--popover"),
    popoverText: hex("--popover-foreground"),
    font: PRINT_FONT,
  }
}
