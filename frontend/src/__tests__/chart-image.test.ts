import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { BarChartSettings } from "@/components/charts/chart-settings"
import type { ChartTheme } from "@/components/charts/chart-theme"
import { printChartTheme } from "@/lib/export/print-theme"

// A chart downloaded as an image (dashboards-design.md § 8, D3): drawn again by
// ECharts at its size on the page, in the light theme a PDF prints in, with its name
// above it and its legend under it, as one SVG. What is pinned: the layout of the
// parts, that text is escaped and cut to fit, that the legend wraps as the page's
// does, and that the drawing is the light print theme's in fonts a computer has —
// never the page's dark theme or its tooltip — with its ECharts instance let go.

const made = vi.hoisted(() => ({ options: [] as Record<string, unknown>[], disposed: 0 }))
vi.mock("echarts/core", async (actual) => {
  const real = await actual<typeof import("echarts/core")>()
  return {
    ...real,
    init: (...args: Parameters<typeof real.init>) => {
      const chart = real.init(...args)
      const setOption = chart.setOption.bind(chart)
      const dispose = chart.dispose.bind(chart)
      chart.setOption = ((option: Record<string, unknown>) => {
        made.options.push(option)
        return setOption(option)
      }) as typeof chart.setOption
      chart.dispose = () => {
        made.disposed += 1
        dispose()
      }
      return chart
    },
  }
})

import { IMAGE_FONT, chartImage, composeChartImage, type TextWidth } from "@/lib/export/chart-image"

// No canvas in jsdom: ECharts and the legend fall back to estimated widths.
beforeEach(() => {
  made.options.length = 0
  made.disposed = 0
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null)
})
afterEach(() => vi.restoreAllMocks())

/** Every character half its size wide: easy to sum by hand. */
const half: TextWidth = (text, size) => text.length * size * 0.5

const THEME: ChartTheme = { ...printChartTheme(), surface: "#ffffff", text: "#111111", muted: "#666666" }
const CHART = '<svg width="400" height="200" xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>'

describe("composeChartImage", () => {
  it("puts the name above the chart and the legend under it, on the theme's surface", () => {
    const image = composeChartImage({
      title: "Orders",
      chart: CHART,
      plot: { width: 400, height: 200 },
      legend: [
        { label: "EU", color: "#123456" },
        { label: "US", color: "#654321" },
      ],
      theme: THEME,
      measure: half,
    })
    // 16 px either side; the plot at 16 + 20 (title) + 8; the legend 8 under it, one 16 px line, 16 under that.
    expect(image.width).toBe(432)
    expect(image.height).toBe(44 + 200 + 8 + 16 + 16)
    expect(image.svg).toMatch(
      /^<svg xmlns="http:\/\/www\.w3\.org\/2000\/svg" width="432" height="284" viewBox="0 0 432 284" role="img" aria-label="Orders"><title>Orders<\/title>/,
    )
    expect(image.svg).toContain('<rect width="432" height="284" fill="#ffffff"/>')
    expect(image.svg).toContain(`font-family="${IMAGE_FONT}" font-size="14" font-weight="600" fill="#111111">Orders</text>`)
    expect(image.svg).toContain('<svg x="16" y="44" width="400" height="200"')
    // EU at 0: swatch 8 + gap 6 + 11 px text; US after it and a 12 px gap.
    expect(image.svg).toContain('<rect x="16" y="256" width="8" height="8" rx="2" fill="#123456"/>')
    expect(image.svg).toContain('<text x="30" y="264"')
    expect(image.svg).toContain(`<rect x="${16 + 8 + 6 + 11 + 12}" y="256"`)
    expect(image.svg).toMatch(/fill="#666666">US<\/text><\/svg>$/)
  })

  it("has no legend room without a legend", () => {
    const image = composeChartImage({ title: "Orders", chart: CHART, plot: { width: 400, height: 200 }, legend: [], theme: THEME, measure: half })
    expect(image.height).toBe(44 + 200 + 16)
  })

  it("wraps the legend to the plot's width, as the page does", () => {
    // Each item is 8 + 6 + 10 × 5.5 = 69 wide, 81 with its gap: 4 fit in 300, the 5th wraps.
    const legend = ["Category A", "Category B", "Category C", "Category D", "Category E"].map((label) => ({ label, color: "#000000" }))
    const image = composeChartImage({ title: "T", chart: CHART, plot: { width: 300, height: 200 }, legend, theme: THEME, measure: half })
    expect(image.height).toBe(44 + 200 + 8 + 2 * 16 + 16)
    expect(image.svg).toContain('<rect x="16" y="272" width="8" height="8" rx="2" fill="#000000"/>')
  })

  it("escapes the name and the labels, and cuts a name too long for the plot", () => {
    const image = composeChartImage({
      title: `Orders <by> "region" & 'day'`,
      chart: CHART,
      plot: { width: 400, height: 200 },
      legend: [{ label: "<script>", color: '"red"' }],
      theme: THEME,
      measure: half,
    })
    expect(image.svg).toContain("Orders &lt;by&gt; &quot;region&quot; &amp; &#39;day&#39;</text>")
    expect(image.svg).toContain("&lt;script&gt;</text>")
    expect(image.svg).toContain('fill="&quot;red&quot;"')
    expect(image.svg).not.toContain("<script>")

    const long = composeChartImage({ title: "x".repeat(200), chart: CHART, plot: { width: 100, height: 50 }, legend: [], theme: THEME, measure: half })
    // 7 px a letter at 14 px: 13 letters and the ellipsis fit in 100.
    expect(long.svg).toContain(`>${"x".repeat(13)}…</text>`)
  })
})

describe("chartImage", () => {
  const BAR: BarChartSettings = {
    type: "bar",
    description: "Orders by region",
    stacked: false,
    x: { field: "region", label: "Region", categories: ["EU", "US"] },
    y: { field: "orders", format: "integer" },
    series: {
      field: "kind",
      values: [
        { key: "new", label: "New", color: "series-1" },
        { key: "repeat", label: "Repeat", color: "series-2" },
      ],
    },
  }
  const ROWS = [
    { region: "EU", kind: "new", orders: 3 },
    { region: "US", kind: "repeat", orders: 5 },
  ]

  it("draws the chart in the light print theme, in fonts a computer has, without the page's tooltip", async () => {
    const theme = printChartTheme()
    const image = await chartImage("Orders by region", BAR, ROWS, { width: 480, height: 240 })

    expect(image.width).toBe(512)
    expect(image.svg).toContain(`<rect width="512" height="${image.height}" fill="${theme.surface}"/>`)
    expect(image.svg).toContain('<svg x="16" y="44"')
    // The chart's own labels name the same fonts the name and the legend do.
    const start = image.svg.indexOf('<svg x="16"')
    const nested = image.svg.slice(start, image.svg.indexOf("</svg>", start))
    expect(nested).toContain("system-ui")
    expect(nested).toContain(">EU<")
    // Its bars in the print theme's series colours, and both in the legend.
    expect(image.svg).toContain(theme.colors["series-1"])
    expect(image.svg).toContain(theme.colors["series-2"])
    expect(image.svg).toMatch(/>New<\/text>.*>Repeat<\/text><\/svg>$/)

    expect(made.options).toHaveLength(1)
    expect(made.options[0]).not.toHaveProperty("tooltip")
    expect(made.options[0].animation).toBe(false)
    expect(made.disposed).toBe(1)
  })
})
