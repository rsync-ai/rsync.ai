import { afterEach, describe, expect, it, vi } from "vitest"

import { chartOption } from "@/components/charts/chart-option"
import type { ChartSettings } from "@/components/charts/chart-settings"
import { printChartTheme } from "@/lib/export/print-theme"

// On the page a funnel's names are measured on a canvas in the page's font, as zrender
// measures a label to cut it; the 0.6 em estimate, wider than the page's font, cut
// "Subscriptions · 2,372 · 38.6%" on prod where the whole line had room.

const STAGES = ["Groceries", "Electronics", "Restaurants", "Fast Food", "Subscriptions", "Digital Goods", "Home Improvement Store"]
const settings = {
  type: "funnel",
  description: "",
  x: { field: "x", label: "Stage", categories: STAGES },
  y: { field: "v", format: "integer", title: "Count of rows" },
  series: { values: [{ key: "", label: "Count of rows", color: "series-1" }] },
} as unknown as ChartSettings
const rows = STAGES.map((x, i) => ({ x, v: 9_800 - i * 1_200 }))

afterEach(() => vi.restoreAllMocks())

describe("a funnel on the page", () => {
  it("measures its names on a canvas in the page's font, cutting none that has room", () => {
    const fonts: string[] = []
    const ctx = {
      set font(f: string) {
        fonts.push(f)
      },
      measureText: (text: string) => ({ width: [...text].length * 11 * 0.5 }),
    }
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(ctx as unknown as CanvasRenderingContext2D)
    const option = chartOption(settings, rows, { ...printChartTheme(), font: "Inter, sans-serif" }, 375, 300)
    const [series] = option.series as { label: { formatter: (p: { dataIndex: number }) => string } }[]
    const names = STAGES.map((_, i) => series.label.formatter({ dataIndex: i }).split(" · ")[0])
    expect(fonts).toContain("11px Inter, sans-serif")
    expect(names).toEqual(STAGES)
  })

  it("falls back to the estimate where there is no canvas", () => {
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null)
    const option = chartOption(settings, rows, printChartTheme(), 375, 300)
    const [series] = option.series as { label: { formatter: (p: { dataIndex: number }) => string } }[]
    // At 0.6 em a phone's room holds fewer letters: the longest name is cut, with "…".
    const names = STAGES.map((_, i) => series.label.formatter({ dataIndex: i }).split(" · ")[0])
    expect(names.slice(0, -1)).toEqual(STAGES.slice(0, -1))
    expect(names.at(-1)).toMatch(/^Home Improv.*…$/)
  })
})
