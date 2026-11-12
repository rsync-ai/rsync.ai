// @vitest-environment node
import { init, setPlatformAPI } from "echarts/core"
import { describe, expect, it } from "vitest"

import { chartOption, stillOption, sizeNote, type TextMeasure } from "@/components/charts/chart-option"
import { binLabel, type ChartSettings } from "@/components/charts/chart-settings"
import { chartSvg, parseFont, type MeasureText } from "@/lib/export/chart-svg"
import { printChartTheme } from "@/lib/export/print-theme"
import { printableSvg } from "@/lib/export/printable-svg"

// Where a chart's words land, read off the SVG ECharts draws at a tile's width: a
// phone's (317, 343 and 375 px) and the builder's. Every letter is measured at 0.6 em,
// wider than the page's font draws a date (0.58 em) or a number (0.54 em), so what
// fits here fits there. zrender takes a line's height from the width of one CJK
// letter, a full em in the page's font: it is measured so here too.

const measure: MeasureText = (text, size, bold) =>
  [...text].reduce((w, ch) => w + size * (/[\u2e80-\u9fff]/.test(ch) ? 1 : bold ? 0.65 : 0.6), 0)
const PHONES = [317, 343, 375]
const HEIGHT = 300

interface Word {
  text: string
  /** Its corners on the page, after its transform. */
  corners: [number, number][]
}

const ENTITIES: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', "#39": "'" }

/** Each word the SVG writes, as the box it covers. */
function words(svg: string): Word[] {
  return [...svg.matchAll(/<text\b([^>]*)>([^<]*)<\/text>/g)].map(([, attrs, raw]) => {
    const text = raw.replace(/&(amp|lt|gt|quot|#39);/g, (_, e: string) => ENTITIES[e])
    const size = Number(/font-size:\s*([\d.]+)px/.exec(attrs)?.[1] ?? 12)
    const bold = /font-weight:\s*(bold|[6-9]00)/.test(attrs)
    const w = measure(text, size, bold)
    const x = Number(/\sx="([-\d.e]+)"/.exec(attrs)?.[1] ?? 0)
    const y = Number(/\sy="([-\d.e]+)"/.exec(attrs)?.[1] ?? 0)
    const anchor = /text-anchor="(\w+)"/.exec(attrs)?.[1] ?? "start"
    const baseline = /dominant-baseline="(\w+)"/.exec(attrs)?.[1] ?? "alphabetic"
    const left = anchor === "middle" ? x - w / 2 : anchor === "end" ? x - w : x
    const top = baseline === "central" || baseline === "middle" ? y - size / 2 : baseline === "hanging" ? y : y - 0.8 * size
    const t = /transform="(translate|matrix)\(([^)]*)\)"/.exec(attrs)
    const n = t ? t[2].split(/[\s,]+/).map(Number) : []
    const [a, b, c, d, e, f] = !t ? [1, 0, 0, 1, 0, 0] : t[1] === "translate" ? [1, 0, 0, 1, n[0], n[1] ?? 0] : n
    const at = (px: number, py: number): [number, number] => [a * px + c * py + e, b * px + d * py + f]
    return { text, corners: [at(left, top), at(left + w, top), at(left + w, top + size), at(left, top + size)] }
  })
}

/** Whether two boxes cover each other by more than half a pixel (separating axes). */
function overlap(p: Word, q: Word): boolean {
  for (const poly of [p.corners, q.corners]) {
    for (let i = 0; i < poly.length; i++) {
      const [x1, y1] = poly[i]
      const [x2, y2] = poly[(i + 1) % poly.length]
      const [nx, ny] = [y1 - y2, x2 - x1]
      const len = Math.hypot(nx, ny) || 1
      const span = (c: [number, number][]) => c.map(([x, y]) => (x * nx + y * ny) / len)
      const [ps, qs] = [span(p.corners), span(q.corners)]
      if (Math.min(Math.max(...ps), Math.max(...qs)) - Math.max(Math.min(...ps), Math.min(...qs)) <= 0.5) return false
    }
  }
  return true
}

/** The pairs of words drawn over each other, and the words that run past the picture's edge. */
function clashes(svg: string, width: number, height: number): string[] {
  const all = words(svg)
  const found: string[] = []
  all.forEach((p, i) => {
    for (const q of all.slice(i + 1)) if (overlap(p, q)) found.push(`"${p.text}" over "${q.text}"`)
    if (p.corners.some(([x, y]) => x < -0.5 || x > width + 0.5 || y < -0.5 || y > height + 0.5)) found.push(`"${p.text}" past the edge`)
  })
  return found
}

/** Which categories have a label: their own words, or, unless `whole`, the start of them cut short with an ellipsis. */
function labelled(svg: string, categories: string[], whole = false): number[] {
  const texts = words(svg).map((w) => w.text)
  return categories.flatMap((c, i) =>
    texts.some((t) => t === c || (!whole && t.endsWith("…") && t.length > 1 && c.startsWith(t.slice(0, -1)))) ? [i] : [],
  )
}

const evenly = (drawn: number[]) => drawn.length >= 2 && drawn[0] === 0 && drawn.every((d, i) => i === 0 || d - drawn[i - 1] === drawn[1])

const draw = (settings: ChartSettings, rows: object[], width: number, height = HEIGHT) =>
  chartSvg(settings, rows, printChartTheme(), { width, height }, measure)

const CHANNELS = ["pos", "mobile_app", "atm", "branch", "online", "ach", "system"]
const CATEGORIES = [
  "Groceries",
  "Electronics",
  "Restaurants",
  "Fast Food",
  "Subscriptions",
  "Digital Goods",
  "Books",
  "Hotels",
  "Airlines",
  "Fuel",
  "Pharmacy",
  "Clothing",
  "Home Improvement",
  "Entertainment",
  "Utilities",
  "Travel",
  "Gaming",
]
/** Names in a script whose every letter is a full em across, twice a Latin letter's room. */
const WIDE = ["食料品", "家電製品", "レストラン", "ファストフード", "定期購読サービス", "デジタル商品", "書籍"]
const DAYS = Array.from({ length: 30 }, (_, i) => new Date(Date.UTC(2026, 7, 23 + i)).toISOString().slice(0, 10))
const YEAR = Array.from({ length: 367 }, (_, i) => new Date(Date.UTC(2025, 9, 8 + i)).toISOString().slice(0, 10))
const MONTHS = Array.from({ length: 13 }, (_, i) => new Date(Date.UTC(2025, 9 + i, 1)).toISOString().slice(0, 7))

const falling = (n: number, top: number) => Array.from({ length: n }, (_, i) => Math.round(top / (1 + i * 0.6)))

function xy(type: string, categories: string[], values: number[], x = "channel", y = "Sum of amount", format = "integer"): [ChartSettings, object[]] {
  const settings = {
    description: "",
    type,
    stacked: false,
    total: false,
    x: { field: "x", label: x, categories },
    y: { field: "v", format, title: y },
    series: { values: [{ key: "", label: y, color: "series-1" }] },
  } as unknown as ChartSettings
  return [settings, categories.map((c, i) => ({ x: c, v: values[i] }))]
}

function boxplot(categories: string[]): [ChartSettings, object[]] {
  const settings = { type: "boxplot", description: "", x: { label: "channel", categories }, y: { label: "Amount", format: "integer" }, color: "series-1" }
  return [settings as ChartSettings, categories.map((x, i) => ({ x, min: 10 + i, q1: 200, median: 900 + i * 40, q3: 2400, max: 9000 + i * 100 }))]
}

function range(categories: string[]): [ChartSettings, object[]] {
  const settings = {
    type: "range",
    description: "",
    x: { label: "channel", categories },
    low: { label: "Lowest amount", format: "decimal" },
    high: { label: "Highest amount", format: "decimal" },
    color: "series-1",
  }
  return [settings as ChartSettings, categories.map((x, i) => ({ x, low: 16 + i, high: 9000 - i * 100 }))]
}

function candlestick(categories: string[]): [ChartSettings, object[]] {
  const settings = { type: "candlestick", description: "", x: { label: "month", categories }, y: { label: "Price", format: "decimal" } }
  return [settings as ChartSettings, categories.map((x, i) => ({ x, open: 100 + i, high: 130 + i, low: 95, close: 120 - i }))]
}

function heatmap(xs: string[], ys: string[]): [ChartSettings, object[]] {
  const settings = {
    type: "heatmap",
    description: "",
    x: { field: "x", label: "channel", categories: xs },
    y: { field: "y", label: "month", categories: ys },
    value: { field: "v", label: "Count", format: "integer" },
    color: "series-1",
  }
  return [settings as ChartSettings, xs.flatMap((x, i) => ys.map((y, j) => ({ x, y, v: 1 + ((i * 7 + j * 3) % 40) })))]
}

function bullet(value: number, target: number): [ChartSettings, object[]] {
  const settings = { type: "bullet", description: "", value: { label: "Sum of amount paid", format: "decimal" }, target: { label: "Sum of amount due" }, color: "series-1" }
  return [settings as ChartSettings, [{ value, target }]]
}

function scatter(points: [number, number][]): [ChartSettings, object[]] {
  const settings = {
    type: "scatter",
    description: "",
    points: { x: { field: "x", label: "Principal", format: "decimal" }, y: { field: "y", label: "Rate", format: "decimal" } },
    series: { values: [{ key: "", label: "Loans", color: "series-1" }] },
  }
  return [settings as ChartSettings, points.map(([x, y]) => ({ x, y }))]
}

function bubble(points: [number, number, number][]): [ChartSettings, object[]] {
  const [settings, rows] = scatter(points.map(([x, y]) => [x, y]))
  const { points: axes } = settings as unknown as { points: object }
  const sized = { ...settings, type: "bubble", points: { ...axes, size: { field: "size", label: "Refunds", format: "integer" } } }
  return [sized as unknown as ChartSettings, rows.map((r, i) => ({ ...r, size: points[i][2] }))]
}

/** Each bubble drawn: its middle and its radius (a unit circle ECharts scales and moves). */
function circles(svg: string): { x: number; y: number; r: number }[] {
  return [...svg.matchAll(/<path d="M1 0A1 1 0 1 1[^"]*" transform="matrix\(([^)]*)\)"/g)].map(([, m]) => {
    const [r, , , , x, y] = m.split(/[\s,]+/).map(Number)
    return { x, y, r }
  })
}

const UNITS: Record<string, number> = { "": 1, K: 1e3, M: 1e6, B: 1e9, T: 1e12, "%": 0.01 }

/** The numbers written along the lowest row of numbers, left to right: the ticks of an axis across. */
function ticksAcross(svg: string): number[] {
  const ticks = words(svg).flatMap((w) => {
    const m = /^(-?[\d,]*\.?\d+)([KMBT%]?)$/.exec(w.text)
    return m ? [{ v: Number(m[1].replaceAll(",", "")) * UNITS[m[2]], x: w.corners[0][0], y: w.corners[0][1] }] : []
  })
  const bottom = Math.max(...ticks.map((t) => t.y))
  return ticks
    .filter((t) => Math.abs(t.y - bottom) < 1)
    .sort((a, b) => a.x - b.x)
    .map((t) => t.v)
}

describe("a value axis across at a phone's width and the builder's", () => {
  // Its ticks are thinned or written short to fit, each apart from the next, the last
  // past the longest mark: words over each other read as one long number.
  const across: [string, [ChartSettings, object[]], number][] = [
    ["a horizontal bar in hundreds of millions", xy("hbar", ["sme", "personal", "auto"], [277_656_929, 78_546_463, 49_510_853.51], "loan_type", "Sum of outstanding_balance", "decimal"), 277_656_929],
    ["a horizontal bar of 17 categories in thousands", xy("hbar", CATEGORIES, falling(17, 6_900), "category", "Declined"), 6_900],
    ["a horizontal bar of 7 channels in tens of thousands", xy("hbar", CHANNELS, falling(7, 19_800), "channel", "Declined"), 19_800],
    ["a bullet in billions", bullet(5_789_538_622.16, 6_195_252_867.66), 6_195_252_867.66],
    ["a bullet of millions against billions", bullet(405_714_245.5, 12_237_502_800), 12_237_502_800],
    ["a scatter in hundreds of millions", scatter([[120_000_000, 4.5], [880_000_000, 7.25], [2_400_000_000, 11.5]]), 2_400_000_000],
  ]
  for (const [name, [settings, rows], greatest] of across) {
    for (const width of [...PHONES, 568, 582]) {
      it(`${name} at ${width} px has its ticks evenly apart, the last past the longest mark, no two words over each other`, () => {
        const svg = draw(settings, rows, width)
        const ticks = ticksAcross(svg)
        const steps = ticks.slice(1).map((t, i) => t - ticks[i])
        expect(ticks.length, `ticks ${ticks.join(", ")}`).toBeGreaterThanOrEqual(3)
        expect(steps.every((s) => Math.abs(s - steps[0]) <= steps[0] * 1e-9), `ticks ${ticks.join(", ")}`).toBe(true)
        expect(ticks.at(-1)).toBeGreaterThanOrEqual(greatest)
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
  }
})

const BINS = Array.from({ length: 12 }, (_, i) => `${(i * 250).toLocaleString("en-US")}–${((i + 1) * 250).toLocaleString("en-US")}`)
const LONG = "The payment channel each card transaction went through, as the processor reports it"

describe("an axis's title at a phone's width and the builder's", () => {
  // Each axis the settings name is titled on the chart, as the measure's is: under the
  // labels across, above them upright. A title too long for the plot is cut short.
  const titled: [string, [ChartSettings, object[]], string[]][] = [
    ["a bar", xy("bar", CHANNELS, falling(7, 412_000), "Channel", "Sum of amount"), ["Channel", "Sum of amount"]],
    ["a line of 30 days", xy("line", DAYS, falling(30, 9_800), "Day", "Card spend"), ["Day", "Card spend"]],
    ["an area of 13 months", xy("area", MONTHS, falling(13, 120_000), "Month of activity", "Active customers"), ["Month of activity", "Active customers"]],
    ["a horizontal bar", xy("hbar", ["sme", "personal", "auto"], [277_656_929, 78_546_463, 49_510_853.51], "Product", "Outstanding balance", "decimal"), ["Product", "Outstanding balance"]],
    ["a horizontal bar of 17 categories", xy("hbar", CATEGORIES, falling(17, 6_900), "Merchant category", "Declined"), ["Merchant category", "Declined"]],
    ["a waterfall", xy("waterfall", CHANNELS, falling(7, 4_000), "Channel", "Net change"), ["Channel", "Net change"]],
    ["a histogram", xy("histogram", BINS, falling(12, 900), "Amount", "Count"), ["Amount", "Count"]],
    ["a box plot of 17 categories", boxplot(CATEGORIES), ["channel", "Amount"]],
    ["a candlestick of 13 months", candlestick(MONTHS), ["month", "Price"]],
    ["a range of 30 days", range(DAYS), ["channel", "Lowest amount · Highest amount"]],
    ["a heatmap of 17 categories by month", heatmap(CATEGORIES, MONTHS.slice(0, 6)), ["channel", "month"]],
  ]
  for (const [name, [settings, rows], titles] of titled) {
    for (const width of [...PHONES, 568]) {
      it(`${name} at ${width} px titles each axis, no two words over each other, none past the edge`, () => {
        const svg = draw(settings, rows, width)
        const texts = words(svg).map((w) => w.text)
        for (const t of titles) expect(texts).toContain(t)
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
  }

  for (const width of [...PHONES, 568]) {
    it(`a bar at ${width} px cuts a long title short, inside the picture`, () => {
      const svg = draw(...xy("bar", CHANNELS, falling(7, 412_000), LONG), width)
      const cut = words(svg).filter((w) => w.text.endsWith("…") && LONG.startsWith(w.text.slice(0, -1)))
      expect(cut.map((w) => w.text)).toHaveLength(1)
      expect(clashes(svg, width, HEIGHT)).toEqual([])
    })

    it(`a scatter at ${width} px cuts both its long titles short, inside the picture`, () => {
      const [settings, rows] = scatter([[120_000_000, 4.5], [880_000_000, 7.25], [2_400_000_000, 11.5]])
      const { points } = settings as unknown as { points: { x: object; y: object } }
      const long = { ...settings, points: { x: { ...points.x, label: LONG }, y: { ...points.y, label: LONG } } } as ChartSettings
      const svg = draw(long, rows, width)
      const cut = words(svg).filter((w) => w.text.endsWith("…") && LONG.startsWith(w.text.slice(0, -1)))
      expect(cut.map((w) => w.text)).toHaveLength(2)
      expect(clashes(svg, width, HEIGHT)).toEqual([])
    })
  }
})

describe("a category axis at a phone's width and the builder's", () => {
  // Words for categories: each is named, turned or cut short to fit, never left out.
  const named: [string, [ChartSettings, object[]]][] = [
    ["a bar of 7 channels", xy("bar", CHANNELS, falling(7, 412_000))],
    ["a bar of 17 categories", xy("bar", CATEGORIES, falling(17, 7_000), "category")],
    ["an area of 7 channels", xy("area", CHANNELS, falling(7, 412_000))],
    ["a waterfall of 7 channels", xy("waterfall", CHANNELS, falling(7, 4_000))],
    ["a box plot of 7 channels", boxplot(CHANNELS)],
    ["a box plot of 17 categories", boxplot(CATEGORIES)],
    ["a range of 7 channels", range(CHANNELS)],
    ["a heatmap of 7 channels by month", heatmap(CHANNELS, MONTHS)],
    ["a heatmap of 17 categories by month", heatmap(CATEGORIES, MONTHS.slice(0, 6))],
    ["a bar of 7 Japanese names", xy("bar", WIDE, falling(7, 412_000), "category")],
    ["a horizontal bar of 7 Japanese names", xy("hbar", WIDE, falling(7, 412_000), "category")],
    ["a heatmap of 7 Japanese names by month", heatmap(WIDE, MONTHS.slice(0, 6))],
  ]
  for (const [name, [settings, rows]] of named) {
    const categories = (settings as { x: { categories: string[] } }).x.categories
    for (const width of [...PHONES, 568]) {
      it(`${name} at ${width} px names every category, no two words over each other, none past the edge`, () => {
        const svg = draw(settings, rows, width)
        expect(labelled(svg, categories)).toEqual(categories.map((_, i) => i))
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
  }

  // A row's name down the side: ECharts left out every other one where a row was under
  // 1.3 times the label's height, so a phone's heatmap of 17 categories named 9.
  const rows: [string, [ChartSettings, object[]], string[]][] = [
    ["a heatmap of 7 countries by 17 categories", heatmap(["US", "GB", "IN", "DE", "FR", "JP", "BR"], CATEGORIES), CATEGORIES],
    ["a horizontal bar of 17 categories", xy("hbar", CATEGORIES, falling(17, 6_900), "Merchant category", "Declined"), CATEGORIES],
  ]
  for (const [name, [settings, data], categories] of rows) {
    for (const width of [...PHONES, 568]) {
      it(`${name} at ${width}x${HEIGHT} px names every row, no two words over each other`, () => {
        const svg = draw(settings, data, width)
        expect(labelled(svg, categories, true)).toEqual(categories.map((_, i) => i))
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
    it(`${name} at 568x180 px names its rows evenly, no two words over each other`, () => {
      const svg = draw(settings, data, 568, 180)
      const drawn = labelled(svg, categories, true)
      expect(evenly(drawn), `labelled ${drawn.join(", ")}`).toBe(true)
      expect(clashes(svg, 568, 180)).toEqual([])
    })
  }

  // A name in a wide script was measured as a Latin one, so its box was too small and it
  // was cut short, at any width.
  for (const [name, [settings, rows]] of [
    ["a bar of 7 Japanese names", xy("bar", WIDE, falling(7, 412_000), "category")],
    ["a box plot of 7 Japanese names", boxplot(WIDE)],
    ["a heatmap of 7 Japanese names by month", heatmap(WIDE, MONTHS.slice(0, 6))],
  ] as [string, [ChartSettings, object[]]][]) {
    it(`${name} at 1300 px names each category whole`, () => {
      const svg = draw(settings, rows, 1300)
      expect(labelled(svg, WIDE, true)).toEqual(WIDE.map((_, i) => i))
      expect(clashes(svg, 1300, HEIGHT)).toEqual([])
    })
  }

  // Dates read in order: some may be left out, evenly, and each one drawn is whole, as a
  // date cut short names no day.
  const dated: [string, [ChartSettings, object[]]][] = [
    ["a line of 7 days", xy("line", DAYS.slice(0, 7), falling(7, 9_800), "day", "Card spend")],
    ["a line of 20 days", xy("line", DAYS.slice(0, 20), falling(20, 9_800), "day", "Card spend")],
    ["a line of 30 days", xy("line", DAYS, falling(30, 9_800), "day", "Card spend")],
    ["a line of 367 days", xy("line", YEAR, falling(367, 9_800), "day", "Card spend")],
    ["an area of 367 days with values under ten", xy("area", YEAR, falling(367, 8), "day", "Declines")],
    ["a line of 29 days", xy("line", DAYS.slice(0, 29), falling(29, 9_800), "day", "Card spend")],
    ["an area of 25 days", xy("area", DAYS.slice(0, 25), falling(25, 9_800), "day", "Card spend")],
    ["an area of 30 days", xy("area", DAYS, falling(30, 9_800), "day", "Card spend")],
    ["a bar of 13 months", xy("bar", MONTHS, falling(13, 120_000), "month", "Active customers")],
    ["a candlestick of 13 months", candlestick(MONTHS)],
    ["a range of 30 days", range(DAYS)],
  ]
  for (const [name, [settings, rows]] of dated) {
    const categories = (settings as { x: { categories: string[] } }).x.categories
    for (const width of [...PHONES, 568, 1400]) {
      it(`${name} at ${width} px labels its dates evenly, no two words over each other, none past the edge`, () => {
        const svg = draw(settings, rows, width)
        const drawn = labelled(svg, categories, true)
        expect(evenly(drawn), `labelled ${drawn.join(", ")}`).toBe(true)
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
  }

  // A turned label under TURNED_WIDTH was given its widest as measured and no more, and
  // zrender keeps a px of it back: on prod a waterfall's months (turned, as its Total is a
  // name, not a date) were cut to "2026-…", 7 of 13.
  const CODES = Array.from({ length: 13 }, (_, i) => `SKU-${1001 + i}`)
  const turned: [string, [ChartSettings, object[]], string[]][] = [
    ["a waterfall of 13 months and its Total", ((s, r) => [{ ...s, total: true } as ChartSettings, r])(...xy("waterfall", MONTHS, falling(13, 120_000), "month", "Card spend")), [...MONTHS, "Total"]],
    ["a bar of 13 codes", xy("bar", CODES, falling(13, 120_000), "code", "Units"), CODES],
  ]
  for (const [name, [settings, rows], categories] of turned) {
    for (const width of [...PHONES, 568, 1400]) {
      it(`${name} at ${width} px writes each category it names whole`, () => {
        const svg = draw(settings, rows, width)
        expect(words(svg).filter((w) => w.text.endsWith("…")).map((w) => w.text)).toEqual([])
        expect(labelled(svg, categories, true)).toEqual(categories.map((_, i) => i))
        expect(clashes(svg, width, HEIGHT)).toEqual([])
      })
    }
  }
})

/**
 * Each bar drawn, upright: its fill and how much of it shows inside the line around it
 * (ECharts draws that line on the bar's edge, half of it over the bar).
 */
function bars(svg: string): { line: number; width: number; height: number }[] {
  return [...svg.matchAll(/<path d="M[-\d.e]+ [-\d.e]+l([-\d.e]+) 0l0 ([-\d.e]+)l[-\d.e]+ 0Z"([^>]*)>/g)].flatMap(([, w, h, attrs]) => {
    if (!attrs.includes("ecmeta_series_index")) return []
    const stroke = /\sstroke="([^"]+)"/.exec(attrs)?.[1]
    const line = stroke && stroke !== "none" ? Number(/stroke-width="([\d.]+)"/.exec(attrs)?.[1] ?? 1) : 0
    return [{ line, width: Math.abs(Number(w)) - line, height: Math.abs(Number(h)) - line }]
  })
}

/** A histogram of `n` bins `size` wide, the first the largest by far, as amounts mostly are. */
function bins(n: number, size: number): [ChartSettings, object[]] {
  const categories = Array.from({ length: n }, (_, i) => `${(i * size).toLocaleString("en-US")}–${((i + 1) * size).toLocaleString("en-US")}`)
  return xy("histogram", categories, categories.map((_, i) => (i === 0 ? 1_578_417 : Math.round(400_000 / (1 + i)))), "amount", "Count")
}

function stackedDays(): [ChartSettings, object[]] {
  const [settings] = xy("bar", YEAR, [])
  const series = {
    values: [
      { key: "a", label: "Settled", color: "series-1", value: "a" },
      { key: "b", label: "Declined", color: "series-2", value: "b" },
    ],
  }
  return [{ ...settings, stacked: true, series } as unknown as ChartSettings, YEAR.map((x, i) => ({ x, a: 900 + (i % 40) * 20, b: 300 + (i % 9) * 30 }))]
}

describe("a bubble chart's largest bubbles at the edges of its points", () => {
  // The axes fitted the points' middles, so a bubble at the least or the greatest x or y
  // stood half over the axis's line, its numbers and its title, and hid them.
  const edges = bubble([[0, 0, 900], [80, 0, 900], [0, 0.12, 900], [80, 0.12, 900], [40, 0.06, 100], [12, 0.03, 30]])
  for (const [width, height] of [...PHONES.map((w): [number, number] => [w, HEIGHT]), [568, HEIGHT], [568, 180], [1300, 360]] as [number, number][]) {
    it(`at ${width}x${height} px draws no bubble over a word`, () => {
      const svg = draw(...edges, width, height)
      const drawn = circles(svg)
      expect(drawn).toHaveLength(6)
      const over = drawn.flatMap((c) =>
        words(svg).flatMap((w) => {
          const xs = w.corners.map(([x]) => x)
          const ys = w.corners.map(([, y]) => y)
          // The nearest point of the word's box to the bubble's middle.
          const nx = Math.min(Math.max(c.x, Math.min(...xs)), Math.max(...xs))
          const ny = Math.min(Math.max(c.y, Math.min(...ys)), Math.max(...ys))
          return Math.hypot(nx - c.x, ny - c.y) < c.r - 0.5 ? [`"${w.text}" under the bubble at ${c.x.toFixed(0)},${c.y.toFixed(0)}`] : []
        }),
      )
      expect(over).toEqual([])
      expect(clashes(svg, width, height)).toEqual([])
    })
  }
})

describe("a bar a pixel or two wide", () => {
  // A line of the card's colour between bars that touch is drawn over half of each: a
  // bar not much wider than the line is all line, and the plot is empty.
  const thin: [string, [ChartSettings, object[]], number[]][] = [
    ["a histogram of 674 bins", bins(674, 1_000), [...PHONES, 568, 1300]],
    ["a histogram of 6,739 bins", bins(6_739, 100), [375, 1300]],
    ["a histogram of 120 bins", bins(120, 50), [...PHONES, 568]],
    ["a stacked bar of 367 days", stackedDays(), [...PHONES, 568, 1300]],
  ]
  for (const [name, [settings, rows], widths] of thin) {
    for (const width of widths) {
      it(`${name} at ${width} px shows the colour of every bar a pixel tall or more`, () => {
        const drawn = bars(draw(settings, rows, width)).filter((b) => b.height >= 1)
        expect(drawn.length).toBeGreaterThan(0)
        expect(drawn.filter((b) => b.width <= 0).map((b) => b.width)).toEqual([])
      })
    }
  }

  const DAYS_120 = Array.from({ length: 120 }, (_, i) => new Date(Date.UTC(2026, 0, 1 + i)).toISOString().slice(0, 10))
  for (const width of [...PHONES, 568]) {
    it(`a heatmap of 120 days by weekday at ${width} px shows its colour over most of every cell`, () => {
      const svg = draw(...heatmap(DAYS_120, ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]), width)
      // Each cell's width across, from its path's corners, and the line drawn half over it.
      const cells = [...svg.matchAll(/<path d="M([-\d.e]+) [^"]*"([^>]*ecmeta_series_index[^>]*)>/g)].map(([d, , attrs]) => {
        const xs = [...d.matchAll(/[ML]([-\d.e]+) /g)].map((m) => Number(m[1]))
        const stroke = /\sstroke="([^"]+)"/.exec(attrs)?.[1]
        const line = stroke && stroke !== "none" ? Number(/stroke-width="([\d.]+)"/.exec(attrs)?.[1] ?? 1) : 0
        return { width: Math.max(...xs) - Math.min(...xs), shows: Math.max(...xs) - Math.min(...xs) - line }
      })
      expect(cells).toHaveLength(120 * 7)
      expect(cells.filter((c) => c.shows < c.width / 2).map((c) => `${c.shows} of ${c.width}`)).toEqual([])
    })
  }

  // Down the chart, a bar's or a cell's thickness is the drawn height's share.
  const MERCHANTS = Array.from({ length: 120 }, (_, i) => `Merchant ${i + 1}`)
  for (const [width, height] of [...PHONES.map((w): [number, number] => [w, HEIGHT]), [568, 180]]) {
    it(`a stacked hbar of 120 merchants at ${width}x${height} px shows the colour of every bar a pixel long or more`, () => {
      const [settings] = xy("hbar", MERCHANTS, [])
      const series = { values: [{ key: "a", label: "Settled", color: "series-1", value: "a" }, { key: "b", label: "Declined", color: "series-2", value: "b" }] }
      const rows = MERCHANTS.map((x, i) => ({ x, a: 900 + (i % 40) * 20, b: 300 + (i % 9) * 30 }))
      const drawn = bars(draw({ ...settings, stacked: true, series } as unknown as ChartSettings, rows, width, height)).filter((b) => b.width >= 1)
      expect(drawn.length).toBeGreaterThan(0)
      expect(drawn.filter((b) => b.height <= 0).map((b) => b.height)).toEqual([])
    })

    it(`a funnel of 120 merchants at ${width}x${height} px draws every stage inside the picture, its colour over most of it`, () => {
      const falling = MERCHANTS.map((_, i) => Math.round(90_000 / (1 + i)))
      const svg = draw(...xy("funnel", MERCHANTS, falling, "Stage", "Count of rows"), width, height)
      const drawn = stages(svg)
      expect(drawn).toHaveLength(120)
      expect(drawn.filter((s) => !(s.tall > 0 && s.top >= 0 && s.top + s.tall <= height))).toEqual([])
      expect(drawn.filter((s) => s.line > s.tall / 2)).toEqual([])
      expect(clashes(svg, width, height).filter((c) => c.includes(" over "))).toEqual([])
      // A name centred on a stage under a line tall stands past neither the top nor the bottom.
      expect(words(svg).filter((w) => w.corners.some(([, y]) => y < -0.5 || y > height + 0.5)).map((w) => w.text)).toEqual([])
    })

    it(`a heatmap of 7 weekdays by 120 merchants at ${width}x${height} px shows its colour over most of every cell`, () => {
      const svg = draw(...heatmap(["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"], MERCHANTS), width, height)
      // Each cell's height down, from its path's corners, and the line drawn half over it.
      const cells = [...svg.matchAll(/<path d="M[^"]*"([^>]*ecmeta_series_index[^>]*)>/g)].map(([d, attrs]) => {
        const ys = [...d.matchAll(/[ML][-\d.e]+ ([-\d.e]+)/g)].map((m) => Number(m[1]))
        const stroke = /\sstroke="([^"]+)"/.exec(attrs)?.[1]
        const line = stroke && stroke !== "none" ? Number(/stroke-width="([\d.]+)"/.exec(attrs)?.[1] ?? 1) : 0
        return { height: Math.max(...ys) - Math.min(...ys), shows: Math.max(...ys) - Math.min(...ys) - line }
      })
      expect(cells).toHaveLength(7 * 120)
      expect(cells.filter((c) => c.shows < c.height / 2).map((c) => `${c.shows} of ${c.height}`)).toEqual([])
    })
  }

  for (const width of [...PHONES, 568]) {
    it(`a histogram of 12 bins at ${width} px keeps the card's colour between its bars`, () => {
      const drawn = bars(draw(...bins(12, 250), width))
      expect(drawn).toHaveLength(12)
      expect(drawn.every((b) => b.line === 1 && b.width >= 4)).toBe(true)
    })
  }
})

describe("a histogram of more bins than bars fit", () => {
  // A bin width set by hand made up to 10,000 bins, a bar each at any width: thinner
  // than a pixel at a phone's, so the bars stood on each other and a tooltip named one
  // bin of the dozens under the pointer.
  const edged = (n: number, size: number): [ChartSettings, object[]] => {
    const lo = Array.from({ length: n }, (_, i) => i * size)
    const categories = lo.map((l) => binLabel(l, size))
    const [settings, rows] = xy("histogram", categories, lo.map((_, i) => (i % 7) + 1), "amount", "Count")
    return [{ ...settings, bins: { lo, width: size } } as ChartSettings, rows]
  }
  const categoriesOf = (o: ReturnType<typeof chartOption>) => (o.xAxis as { data: string[] }).data
  const valuesOf = (o: ReturnType<typeof chartOption>) =>
    ((o.series as { data: unknown[] }[])[0].data).map((d) => (typeof d === "number" ? d : (d as { value: number }).value))
  const total = (rows: object[]) => rows.reduce((t, r) => t + (r as { v: number }).v, 0)

  for (const [width, per] of [[375, 80], [568, 53], [1300, 24]]) {
    it(`of 10,000 bins at ${width} px adds up ${per} neighbours a bar, each named for its range, and says so`, () => {
      const [settings, rows] = edged(10_000, 5)
      const option = chartOption(settings, rows, printChartTheme(), width, HEIGHT)
      const names = categoriesOf(option)
      expect(names).toHaveLength(Math.ceil(10_000 / per))
      expect(names.length).toBeLessThanOrEqual(Math.floor(width / 3))
      expect(names[0]).toBe(binLabel(0, per * 5))
      expect(names[1]).toBe(binLabel(per * 5, per * 5))
      // The last bar ends where the last bin does, however few bins it holds.
      expect(names[names.length - 1]).toBe(binLabel((names.length - 1) * per * 5, 10_000 * 5 - (names.length - 1) * per * 5))
      const values = valuesOf(option)
      expect(values.reduce((t, v) => t + v, 0)).toBe(total(rows))
      expect(values[0]).toBe(rows.slice(0, per).reduce((t, r) => t + (r as { v: number }).v, 0))
      expect(sizeNote(settings, rows, printChartTheme(), width, HEIGHT)).toBe(`10,000 bins are more than fit at this width: each bar adds up ${per} of them.`)
      // Every bar drawn is wide enough to see.
      const drawn = bars(draw(settings, rows, width)).filter((b) => b.height >= 1)
      expect(drawn.length).toBeGreaterThan(0)
      expect(drawn.filter((b) => b.width + b.line < 2).map((b) => b.width)).toEqual([])
    })
  }

  for (const width of [...PHONES, 568]) {
    it(`of 12 bins at ${width} px draws a bar a bin and says nothing`, () => {
      const [settings, rows] = edged(12, 250)
      const option = chartOption(settings, rows, printChartTheme(), width, HEIGHT)
      expect(categoriesOf(option)).toEqual((settings as { x: { categories: string[] } }).x.categories)
      expect(valuesOf(option)).toEqual(rows.map((r) => (r as { v: number }).v))
      expect(sizeNote(settings, rows, printChartTheme(), width, HEIGHT)).toBeNull()
    })
  }
})

const CITIES = ["Phoenix", "Seattle", "Atlanta", "Boston", "New York", "Edinburgh", "Madrid", "Paris"]

/** A treemap or a sunburst at the 3,500-part cap: 473 merchants, each in seven or eight cities, each in one currency. */
function merchants(type: "treemap" | "sunburst"): [ChartSettings, object[]] {
  const rows: object[] = []
  for (let m = 0; m < 473; m++) {
    for (const city of CITIES.slice(0, m < 189 ? 8 : 7)) rows.push({ path: [`Merchant ${m} Grocers`, city, "USD"], value: Math.round(9_000 / (1 + rows.length * 0.01)) })
  }
  const groups = Array.from({ length: 7 }, (_, i) => ({ key: `Merchant ${i} Grocers`, label: `Merchant ${i} Grocers`, color: `series-${i + 1}` }))
  const settings = { type, description: "", levels: ["Merchant", "City", "Currency"], value: { label: "Sum of card_transactions", format: "integer" }, groups, partial: true }
  return [settings as ChartSettings, rows]
}

/** A treemap of the 17 categories by country and city: 102 groups of three to six cities. */
function places(): [ChartSettings, object[]] {
  const rows: object[] = []
  CATEGORIES.forEach((c, i) =>
    ["US", "GB", "DE", "FR", "NL", "ES"].forEach((country, j) =>
      CITIES.slice(0, 3 + ((i + j) % 4)).forEach((city, k) => rows.push({ path: [c, country, city], value: Math.round(50_000 / (1 + i) / (1 + j * 0.7) / (1 + k)) })),
    ),
  )
  const settings = { type: "treemap", description: "", levels: ["Category", "Country", "City"], value: { label: "Count of rows", format: "integer" }, groups: [] }
  return [settings as ChartSettings, rows]
}

/** A treemap or a sunburst of one level: 17 categories, one of them 2 of 60,433 rows. */
function categories(type: "treemap" | "sunburst"): [ChartSettings, object[]] {
  const counts = [9_800, 8_400, 7_200, 6_100, 5_300, 2, 4_200, 3_700, 3_300, 2_900, 2_500, 2_100, 1_700, 1_300, 900, 600, 431]
  const groups = CATEGORIES.slice(0, 7).map((c, i) => ({ key: c, label: c, color: `series-${i + 1}` }))
  const settings = { type, description: "", levels: ["Category"], value: { label: "Count of rows", format: "integer" }, groups }
  return [settings as ChartSettings, CATEGORIES.map((c, i) => ({ path: [c], value: counts[i] }))]
}

/** Each slice of a sunburst: how wide it is along the middle of its ring, and the line drawn half over each of its sides. */
function slices(svg: string): { across: number; line: number }[] {
  const n = String.raw`([-\d.e]+)`
  const arc = String.raw`A${n} ${n} 0 [01] [01] ${n} ${n}`
  const sector = new RegExp(String.raw`<path d="M${n} ${n}${arc}L${n} ${n}(?:${arc})?Z"([^>]*ecmeta_series_index[^>]*)>`, "g")
  return [...svg.matchAll(sector)].map((m) => {
    const [x0, y0, , , x1, y1, x2, y2, , , x3, y3] = m.slice(1, 13).map(Number)
    const chord = (ax: number, ay: number, bx: number, by: number) => Math.hypot(bx - ax, by - ay)
    // The innermost ring meets at the middle, where a slice is no width at all.
    const inner = Number.isNaN(x3) ? 0 : chord(x2, y2, x3, y3)
    const stroke = /\sstroke="([^"]+)"/.exec(m[13])?.[1]
    return { across: (chord(x0, y0, x1, y1) + inner) / 2, line: stroke && stroke !== "none" ? Number(/stroke-width="([\d.]+)"/.exec(m[13])?.[1] ?? 1) : 0 }
  })
}

/** Each part a treemap fills with its colour, by its index in the tree: its width and height. */
function tiles(svg: string): Map<number, { width: number; height: number }> {
  const found = new Map<number, { width: number; height: number }>()
  for (const [, w, h, attrs] of svg.matchAll(/<path d="M[-\d.e]+ [-\d.e]+l([-\d.e]+) 0l0 ([-\d.e]+)l[^"]*Z"([^>]*)>/g)) {
    const fill = /\sfill="([^"]+)"/.exec(attrs)?.[1]
    const index = /ecmeta_data_index="(\d+)"/.exec(attrs)?.[1]
    // The card's colour is the part's border, under its colour.
    if (index === undefined || !fill || /^#f+$/i.test(fill)) continue
    found.set(Number(index), { width: Number(w), height: Number(h) })
  }
  return found
}

const PARTS_SIZES: [number, number][] = [...PHONES.map((w): [number, number] => [w, HEIGHT]), [568, HEIGHT], [568, 180], [1300, 360]]

/**
 * Letters as wide as the page's font draws them where 0.6 em is under it: a capital
 * (0.72 em) and the ellipsis (a full em). Named apart, as zrender keeps a text's width
 * per font for the life of the process.
 */
const pageMeasure: MeasureText = (text, size, bold) =>
  [...text].reduce((w, ch) => w + size * (/[\u2e80-\u9fff…]/.test(ch) ? 1 : /[A-Z]/.test(ch) ? 0.72 : bold ? 0.65 : 0.55), 0)

/** Each word that is neither a whole name nor the start of one and its ellipsis: a lone "…", or a name cut to a letter or two with no sign it is cut. */
function fragments(svg: string, names: string[]): string[] {
  return words(svg)
    .map((w) => w.text)
    .filter((t) => !names.includes(t) && !(t.endsWith("…") && t.length > 1 && names.some((n) => n.startsWith(t.slice(0, -1)))))
}

/** A treemap or a sunburst of 40 categories, each in one country and city, from 0.5% to 5.5% of the whole: a country's slice at each width in between. */
function shares(type: "treemap" | "sunburst"): [ChartSettings, object[]] {
  const settings = { type, description: "", levels: ["Category", "Country", "City"], value: { label: "Count of rows", format: "integer" }, groups: [] }
  const rows = Array.from({ length: 40 }, (_, i) => ({ path: [CATEGORIES[i % 17], ["US", "GB", "DE"][i % 3], CITIES[i % 8]], value: 50 + 4 * (40 - i) }))
  return [settings as ChartSettings, rows]
}

describe("a treemap's and a sunburst's names, whole or cut short", () => {
  // A part too narrow for its name shows the start of it and an ellipsis, or nothing: a
  // sunburst at a phone's width wrote a lone "…", its room under a letter and the
  // ellipsis in the page's font, and a treemap's narrow parts read "Ph" and "S", cut to
  // what fitted, the ellipsis dropped.
  const names = [...CATEGORIES, ...CITIES, "US", "GB", "DE", "FR", "NL", "ES"]
  for (const type of ["treemap", "sunburst"] as const) {
    for (const [width, height] of PARTS_SIZES) {
      for (const [font, measured] of [["Inter", measure], ["Page", pageMeasure]] as const) {
        it(`a ${type} of 17 categories by country and city at ${width}x${height} px, measured in ${font}, writes no name cut to a letter or two without its ellipsis`, () => {
          const [settings, rows] = places()
          const theme = { ...printChartTheme(), font }
          for (const [s, r] of [[{ ...settings, type }, rows], categories(type), shares(type)] as [ChartSettings, object[]][]) {
            const svg = chartSvg(s as ChartSettings, r, theme, { width, height }, measured)
            expect(fragments(svg, names)).toEqual([])
          }
        })
      }
    }
  }
})

describe("a treemap's and a sunburst's parts under a pixel", () => {
  // One merchant of a million rows and 199 of one row each: each of the 199 is laid out
  // under a pixel at any size, too small to see or point at, and nothing said so.
  const steep = (type: "treemap" | "sunburst"): [ChartSettings, object[]] => [
    { type, description: "", levels: ["Merchant"], value: { label: "Count of rows", format: "integer" }, groups: [] } as ChartSettings,
    Array.from({ length: 200 }, (_, i) => ({ path: [`Merchant ${i}`], value: i === 0 ? 1_000_000 : 1 })),
  ]
  const NOTE = "199 parts are under a pixel at this size, too small to see."
  for (const [width, height] of PARTS_SIZES) {
    it(`a treemap at ${width}x${height} px says how many parts it draws under a pixel`, () => {
      const [settings, rows] = steep("treemap")
      expect([...tiles(draw(settings, rows, width, height)).values()].filter((t) => t.width * t.height < 1)).toHaveLength(199)
      expect(sizeNote(settings, rows, printChartTheme(), width, height)).toBe(NOTE)
    })

    it(`a sunburst at ${width}x${height} px says how many slices it draws under a pixel`, () => {
      const [settings, rows] = steep("sunburst")
      // A slice under a pixel is drawn as a line from the middle out, with no arc and no area.
      const drawn = draw(settings, rows, width, height).match(/<path d="[^"A]*"[^>]*ecmeta_series_index[^>]*>/g)
      expect(drawn).toHaveLength(199)
      expect(sizeNote(settings, rows, printChartTheme(), width, height)).toBe(NOTE)
    })

    it(`a treemap or a sunburst at ${width}x${height} px whose parts can all be seen says nothing`, () => {
      expect(sizeNote(...places(), printChartTheme(), width, height)).toBeNull()
      expect(sizeNote(...shares("sunburst"), printChartTheme(), width, height)).toBeNull()
    })
  }

  it("says it of one part, and of no type but a treemap or a sunburst", () => {
    const [settings, rows] = steep("treemap")
    expect(sizeNote(settings, rows.slice(0, 2), printChartTheme(), 375, HEIGHT)).toBe("1 part is under a pixel at this size, too small to see.")
    expect(sizeNote({ ...settings, type: "pie" } as unknown as ChartSettings, rows, printChartTheme(), 375, HEIGHT)).toBeNull()
  })
})

describe("a treemap and a sunburst of many parts", () => {
  for (const [width, height] of PARTS_SIZES) {
    it(`a sunburst of 3,500 parts at ${width}x${height} px shows its colour over most of every slice`, () => {
      const drawn = slices(draw(...merchants("sunburst"), width, height))
      expect(drawn.length).toBeGreaterThan(3_500)
      expect(drawn.filter((s) => s.line > s.across / 2).length).toBe(0)
    })

    it(`a treemap of 3,500 parts at ${width}x${height} px draws no word over another`, () => {
      expect(clashes(draw(...merchants("treemap"), width, height), width, height)).toEqual([])
    })

    // Each group's line and name strip are taken from inside it, its parts drawn in what is
    // left: a group's 2 px line on every side of parts a few px across left most of the
    // picture the card's colour, and most parts no area at all.
    it(`a treemap of 3,500 parts at ${width}x${height} px shows its parts' colour over most of the picture`, () => {
      const drawn = [...tiles(draw(...merchants("treemap"), width, height)).values()]
      const area = drawn.reduce((a, t) => a + t.width * t.height, 0)
      expect(area / (width * height)).toBeGreaterThan(0.5)
      expect(drawn.filter((t) => t.width * t.height < 1).length).toBe(0)
    })

    it(`a treemap of 17 categories by country and city at ${width}x${height} px draws no word over another`, () => {
      expect(clashes(draw(...places(), width, height), width, height)).toEqual([])
    })

    it(`a sunburst of 17 categories by country and city at ${width}x${height} px names each part inside its ring, no word over another`, () => {
      const [settings, rows] = places()
      const svg = draw({ ...settings, type: "sunburst" } as ChartSettings, rows, width, height)
      // Three rings in 90% of the picture's half; a name is written along its part's radius.
      const ring = (0.45 * Math.min(width, height)) / 3
      const drawn = words(svg)
      // Parts are named; the largest category, whole or cut short, where the middle ring has room.
      expect(drawn.length).toBeGreaterThan(0)
      if (Math.min(width, height) >= HEIGHT) expect(labelled(svg, ["Groceries"])).toEqual([0])
      const along = (w: Word) => Math.hypot(w.corners[1][0] - w.corners[0][0], w.corners[1][1] - w.corners[0][1])
      expect(drawn.filter((w) => along(w) > ring).map((w) => `${w.text} ${Math.round(along(w))} px in ${Math.round(ring)}`)).toEqual([])
      expect(clashes(svg, width, height)).toEqual([])
    })

    it(`a treemap of 17 categories at ${width}x${height} px draws the one of 2 rows in 60,433`, () => {
      const drawn = tiles(draw(...categories("treemap"), width, height))
      // The root is 0, then each category in the tree's order, Digital Goods last.
      expect([...drawn.keys()].sort((a, b) => a - b)).toEqual(Array.from({ length: 17 }, (_, i) => i + 1))
      expect([...drawn.values()].filter((t) => !(t.width > 0 && t.height > 0))).toEqual([])
    })
  }

  for (const width of [...PHONES, 568]) {
    it(`a sunburst and a treemap of 17 categories at ${width} px keep a line of the card's colour between their large parts`, () => {
      const wide = slices(draw(...categories("sunburst"), width)).filter((s) => s.across >= 20)
      expect(wide.length).toBeGreaterThan(5)
      expect(wide.every((s) => s.line === 2)).toBe(true)
      // A part's colour inside its border: the card's colour around it.
      const svg = draw(...categories("treemap"), width)
      const large = [...svg.matchAll(/<path d="M1 1l([\d.]+) 0l0 ([\d.]+)l[^"]*Z"/g)].filter(([, w, h]) => Number(w) >= 20 && Number(h) >= 20)
      expect(large.length).toBeGreaterThan(5)
    })

    it(`a treemap of a few groups at ${width} px names each group along its top`, () => {
      const settings = { type: "treemap", description: "", levels: ["Category", "Country"], value: { label: "Count of rows", format: "integer" }, groups: [] }
      const rows = ["Groceries", "Electronics", "Travel"].flatMap((c, i) => ["US", "GB", "DE"].map((country, j) => ({ path: [c, country], value: (3 - i) * 1_000 + (3 - j) * 100 })))
      const texts = words(draw(settings as ChartSettings, rows, width)).map((w) => w.text)
      expect(texts).toEqual(expect.arrayContaining(["Groceries", "Electronics", "Travel"]))
    })
  }
})

/** The 17 categories' counts, three of them under one in 400: 150, 60 and 30 of 60,580. */
const FEW = [9_800, 8_400, 7_200, 6_100, 5_300, 60, 4_200, 3_700, 3_300, 2_900, 2_500, 2_100, 1_700, 150, 900, 600, 30]

function slicesOf(type: "pie" | "donut"): [ChartSettings, object[]] {
  const values = CATEGORIES.map((c, i) => ({ key: c, label: c, color: `series-${(i % 8) + 1}` }))
  const settings = { type, description: "", slices: { field: "s", label: "Category", values }, value: { field: "v", label: "Count of rows", format: "integer" } }
  return [settings as ChartSettings, CATEGORIES.map((c, i) => ({ s: c, v: FEW[i] }))]
}

/** Each stage of a funnel: its top, how tall and how wide halfway down it is, and the line drawn half over each of its sides. */
function stages(svg: string): { top: number; tall: number; across: number; line: number }[] {
  return [...svg.matchAll(/<polygon points="([^"]+)"([^>]*ecmeta_series_index[^>]*)>/g)].map(([, points, attrs]) => {
    const [x0, y0, x1, , x2, y2, x3] = points.split(" ").map(Number)
    const stroke = /\sstroke="([^"]+)"/.exec(attrs)?.[1]
    const line = stroke && stroke !== "none" ? Number(/stroke-width="([\d.]+)"/.exec(attrs)?.[1] ?? 1) : 0
    return { top: y0, tall: y2 - y0, across: (x1 - x0 + (x2 - x3)) / 2, line }
  })
}

describe("a pie, a donut and a funnel with a share of a few rows in many", () => {
  for (const width of [...PHONES, 568]) {
    for (const type of ["pie", "donut"] as const) {
      it(`a ${type} at ${width} px shows its colour over most of every slice, and parts its large ones`, () => {
        const drawn = slices(draw(...slicesOf(type), width))
        expect(drawn).toHaveLength(17)
        expect(drawn.filter((s) => s.line > s.across / 2)).toEqual([])
        const wide = drawn.filter((s) => s.across >= 20)
        expect(wide.length).toBeGreaterThan(5)
        expect(wide.every((s) => s.line === 1)).toBe(true)
      })
    }

    it(`a funnel at ${width} px shows its colour over most of every stage`, () => {
      const falling = [...FEW].sort((a, b) => b - a)
      const drawn = stages(draw(...xy("funnel", CATEGORIES, falling, "Stage", "Count of rows"), width))
      expect(drawn).toHaveLength(17)
      expect(drawn.filter((s) => s.line > s.across / 2)).toEqual([])
    })

    // On prod a phone's funnel wrote "Home Improvement · 1,700 · 17%" past the right edge.
    it(`a funnel at ${width} px writes its stages' names inside the picture, each with its numbers`, () => {
      const falling = [...FEW].sort((a, b) => b - a)
      const svg = draw(...xy("funnel", CATEGORIES, falling, "Stage", "Count of rows"), width)
      expect(clashes(svg, width, HEIGHT).filter((c) => c.endsWith("past the edge"))).toEqual([])
      const named = words(svg).map((w) => w.text).filter((t) => t.includes(" · "))
      expect(named.length).toBeGreaterThan(8)
      // A name too long for the room is what is cut short, with "…"; its numbers are kept.
      const cut = (t: string) => t.split(" · ")[0]
      expect(named.filter((t) => !/ · [\d,]+ · [\d.]+%$/.test(t))).toEqual([])
      expect(named.filter((t) => !CATEGORIES.includes(cut(t)) && !(cut(t).endsWith("…") && CATEGORIES.some((c) => c.startsWith(cut(t).slice(0, -1)))))).toEqual([])
    })
  }
})

// A funnel in its stages' own order (by name, say) joined each band to the next stage's
// width, so a stage of 100 before one of 900 was drawn widening to 900: its band read as
// neither. A funnel whose stages do not fall in its order draws each its own width.
describe("a funnel whose stages do not fall in its order", () => {
  // Each band drawn in a colour (the joins between are drawn with none): how wide at its
  // head and its foot, where it starts and how tall it is.
  const edges = (svg: string) =>
    [...svg.matchAll(/<polygon points="([^"]+)"[^>]*fill="(?!none")[^>]*ecmeta_series_index[^>]*>/g)].map(([, points]) => {
      const [x0, y0, x1, , x2, y2, x3] = points.split(" ").map(Number)
      return { top: x1 - x0, bottom: x2 - x3, start: y0, tall: y2 - y0 }
    })
  for (const width of [...PHONES, 568, 1440]) {
    it(`at ${width} px draws each stage its own width, named and in its order`, () => {
      const [settings, rows] = xy("funnel", CATEGORIES, FEW, "Stage", "Count of rows")
      const svg = draw(settings, rows, width)
      const drawn = edges(svg).filter((e) => e.tall >= 1)
      expect(drawn).toHaveLength(CATEGORIES.length)
      // A band is as wide at its foot as at its head, and as wide as its value makes it.
      expect(drawn.filter((e) => Math.abs(e.top - e.bottom) > 0.5)).toEqual([])
      const widest = Math.max(...drawn.map((e) => e.top))
      const most = Math.max(...FEW)
      expect(drawn.map((e, i) => Math.abs(e.top / widest - FEW[i] / most) < 0.01)).toEqual(FEW.map(() => true))
      // The stages stand in their order, top to bottom, with a gap between each two.
      expect(drawn.slice(1).filter((e, i) => e.start - (drawn[i].start + drawn[i].tall) < 0.5)).toEqual([])
      // Every name written is a stage's, with its own numbers; nothing is written for the joins.
      const option = chartOption(settings, rows, printChartTheme(), width, HEIGHT)
      const [series] = option.series as { label: { formatter: (p: { dataIndex: number }) => string }; data: { name: string; label?: { show?: boolean } }[] }[]
      const named = series.data.flatMap((d, i) => (d.label?.show === false ? [] : [[d.name, series.label.formatter({ dataIndex: i })]]))
      expect(named.map(([name]) => name)).toEqual(CATEGORIES)
      expect(named.filter(([name, text], i) => !text.startsWith(name.slice(0, 3)) || !text.includes(` · ${FEW[i].toLocaleString("en-US")} · `))).toEqual([])
      expect(clashes(svg, width, HEIGHT).filter((c) => c.endsWith("past the edge"))).toEqual([])
    })
  }

  it("a funnel whose stages fall keeps each band tapering to the next stage's width", () => {
    const falling = [...FEW].sort((a, b) => b - a)
    const drawn = edges(draw(...xy("funnel", CATEGORIES, falling, "Stage", "Count of rows"), 568))
    expect(drawn).toHaveLength(CATEGORIES.length)
    expect(drawn.slice(0, -1).filter((e, i) => Math.abs(e.bottom - drawn[i + 1].top) > 0.5)).toEqual([])
    expect(drawn.at(-1)!.bottom).toBe(0)
  })
})

// The page's font draws a Latin letter near 0.5 em, narrower than the estimate's 0.6: a
// funnel fitted by the estimate cut "Subscriptions · 2,372 · 38.6%" on prod where the
// whole line had room. The names are measured as the page draws them.
describe("a funnel's names, measured as the page draws them", () => {
  const page: TextMeasure = (text, size) => [...text].length * size * 0.5
  const fallingFew = [...FEW].sort((a, b) => b - a)
  // One name too long for a phone's room, so a cut is still made there.
  const names = CATEGORIES.map((c) => (c === "Home Improvement" ? "Home Improvement, Garden and Outdoor Living" : c))
  for (const width of [...PHONES, 568, 1440]) {
    it(`at ${width} px cuts only a name whose whole line has no room`, () => {
      const [settings, rows] = xy("funnel", names, fallingFew, "Stage", "Count of rows")
      const option = chartOption(settings, rows, printChartTheme(), width, HEIGHT, page)
      const [series] = option.series as { label: { formatter: (p: { dataIndex: number }) => string }; labelLayout: { x: number } }[]
      const room = width - series.labelLayout.x
      const texts = names.map((_, i) => series.label.formatter({ dataIndex: i }))
      const cut = texts.flatMap((t, i) => (t.split(" · ")[0] === names[i] ? [] : [i]))
      expect(cut).toEqual(width < 568 ? [names.indexOf("Home Improvement, Garden and Outdoor Living")] : [])
      // Every name cut would not fit whole, and every line drawn fits.
      const whole = (i: number) => texts[i].replace(/^[^·]+…(?= · )/, names[i])
      expect(cut.filter((i) => page(whole(i), 11) <= room)).toEqual([])
      expect(texts.filter((t) => page(t, 11) > room + 1)).toEqual([])
    })
  }
})

/** A sankey from each of `from` to each of `to`, `value` the flow's size. */
function flows(from: string[], to: string[], value: (i: number, j: number) => number): [ChartSettings, object[]] {
  const nodes = [...from, ...to].map((key, i) => ({ key, label: key, color: `series-${(i % 8) + 1}` }))
  const settings = { type: "sankey", description: "", source: { label: "From" }, target: { label: "To" }, value: { label: "Count of rows", format: "integer" }, nodes }
  return [settings as ChartSettings, from.flatMap((source, i) => to.map((target, j) => ({ source, target, value: value(i, j) })).filter((r) => r.value > 0))]
}

/** Each node a sankey draws: its top and how tall it is. */
function nodes(svg: string): { top: number; tall: number }[] {
  return [...svg.matchAll(/<path d="M[-\d.e]+ ([-\d.e]+)l10 0l0 ([-\d.e]+)l-10 0Z"[^>]*ecmeta_series_index/g)].map(([, y, h]) => ({ top: Number(y), tall: Number(h) }))
}

describe("a sankey of many names in a column", () => {
  const SIZES: [number, number][] = [...PHONES.map((w): [number, number] => [w, 360]), [568, 360], [568, 180]]
  // 75 names into five, a few large and most a row or two, as a count by an id is.
  const MANY = Array.from({ length: 75 }, (_, i) => String(i))
  const many = flows(MANY, ["Leeds", "York", "Hull", "Bath", "Ely"], (i, j) => (j === i % 5 ? Math.max(1, Math.round(900 / (1 + i * i))) : 0))
  const counts = [9_800, 8_400, 7_200, 6_100, 5_300, 2, 4_200, 3_700, 3_300, 2_900, 2_500, 2_100, 1_700, 40, 900, 600, 431]
  const few = flows(CATEGORIES, ["US", "GB", "DE", "FR", "NL", "ES"], (i, j) => Math.max(1, Math.round((counts[i] * [50, 15, 12, 10, 8, 5][j]) / 100)))
  for (const [width, height] of SIZES) {
    it(`75 names in a column at ${width}x${height} px are drawn inside the picture, the largest named`, () => {
      const svg = draw(...many, width, height)
      const drawn = nodes(svg)
      expect(drawn).toHaveLength(80)
      expect(drawn.filter((n) => !(n.tall >= 0 && n.top >= 0 && n.top + n.tall <= height))).toEqual([])
      expect(words(svg).map((w) => w.text)).toEqual(expect.arrayContaining(["0", "1", "Leeds", "York"]))
      expect(clashes(svg, width, height)).toEqual([])
    })

    it(`17 categories into 6 countries at ${width}x${height} px draws no word over another`, () => {
      const svg = draw(...few, width, height)
      expect(nodes(svg)).toHaveLength(23)
      expect(words(svg).map((w) => w.text)).toEqual(expect.arrayContaining(["Groceries", "US", "GB"]))
      expect(clashes(svg, width, height)).toEqual([])
    })
  }
})

/** A calendar of the latest 366 days, as a source with more is drawn. */
function year(): [ChartSettings, object[]] {
  const settings = { type: "calendar", description: "", range: ["2025-10-08", "2026-10-08"], value: { label: "Sum of card_spend", format: "integer" }, color: "series-1" }
  const rows = Array.from({ length: 366 }, (_, i) => ({ day: new Date(Date.UTC(2025, 9, 8 + i)).toISOString().slice(0, 10), value: 1000 + ((i * 37) % 500) }))
  return [settings as ChartSettings, rows]
}

/** The category of each point a line draws: a circle, a path of its own. */
const points = (svg: string) => [...svg.matchAll(/<path d="M1 0A1 1 [^"]*"[^>]*ecmeta_data_index="(\d+)"/g)].map((m) => Number(m[1]))

describe("a line of a year's days with a value on few of them", () => {
  // A day with no rows is put back with no value (saved-chart.ts), so a value with none
  // either side has no line to it, and its point is all that draws it.
  const alone = YEAR.map((_, i) => (i % 9 === 0 ? 100 + i : null))
  // The first 120 days each have a value, joined by the line; the rest are alone.
  const mixed = YEAR.map((_, i) => (i < 120 || i % 9 === 0 ? 100 + (i % 40) : null))
  for (const type of ["line", "area"]) {
    for (const width of [...PHONES, 568, 1300]) {
      it(`a ${type} at ${width} px draws the point of each value alone`, () => {
        const [settings] = xy(type, YEAR, [])
        const at = (values: (number | null)[]) => values.flatMap((v, i) => (v === null ? [] : [i]))
        expect(points(draw(settings, YEAR.map((x, i) => ({ x, v: alone[i] })), width))).toEqual(at(alone))
        const drawn = points(draw(settings, YEAR.map((x, i) => ({ x, v: mixed[i] })), width))
        expect(at(mixed).filter((i) => i > 120).filter((i) => !drawn.includes(i))).toEqual([])
        // The joined days' points are not drawn one over another where the days are close.
        const band = (width - 60) / YEAR.length
        if (band < 7) expect(drawn.filter((i) => i < 120).length).toBeLessThan(120 / 2)
      })
    }
  }
  // A range's band with none either side is its two points, its low's and its high's.
  for (const width of [...PHONES, 568, 1300]) {
    it(`a range at ${width} px draws both points of each band alone`, () => {
      const rows = YEAR.map((x, i) => ({ x, low: alone[i] === null ? null : 16 + i, high: 9000 - i }))
      const drawn = points(draw(range(YEAR)[0], rows, width))
      expect(alone.flatMap((v, i) => (v === null ? [] : [i])).filter((i) => drawn.filter((d) => d === i).length !== 2)).toEqual([])
    })
  }
})

describe("a calendar of a year", () => {
  for (const width of [...PHONES, 568]) {
    it(`at ${width} px draws its months' names apart and inside the picture, and says each year`, () => {
      const svg = draw(...year(), width)
      expect(clashes(svg, width, HEIGHT)).toEqual([])
      const months = words(svg).map((w) => w.text).filter((t) => /^[A-Z][a-z]{2}( \d{4})?$/.test(t))
      expect(months.length).toBeGreaterThanOrEqual(width < 568 ? 6 : 12)
      expect(months.filter((t) => / \d{4}$/.test(t)).map((t) => t.slice(-4))).toEqual(["2025", "2026"])
    })
  }
})

/**
 * A chart drawn at one size, then at another as the page draws it again on a resize:
 * the same settings merge into the chart it has (EChart.tsx), so a key the second draw
 * leaves out keeps the first draw's value.
 */
function redrawn(settings: ChartSettings, rows: object[], [w0, h0]: [number, number], [w1, h1]: [number, number]): string {
  setPlatformAPI({ measureText: (text, font) => ({ width: measure(text ?? "", parseFont(font).size, parseFont(font).bold) }) })
  const chart = init(null, undefined, { renderer: "svg", ssr: true, width: w0, height: h0 })
  try {
    chart.setOption(stillOption(chartOption(settings, rows, printChartTheme(), w0, h0)))
    chart.resize({ width: w1, height: h1 })
    chart.setOption(stillOption(chartOption(settings, rows, printChartTheme(), w1, h1)), { notMerge: false })
    return printableSvg(chart.renderToSVGString())
  } finally {
    chart.dispose()
  }
}

/** What an SVG draws, without the names zrender gives its styles, which count up for the life of the process. */
const marks = (svg: string) => svg.replace(/<style[^]*?<\/style>/g, "").replace(/\s*class="[^"]*"/g, "").replace(/zr\d+-/g, "")

describe("a chart drawn again at a new size", () => {
  const MERCHANTS = Array.from({ length: 120 }, (_, i) => `Merchant ${i + 1}`)
  const DAYS_120 = Array.from({ length: 120 }, (_, i) => new Date(Date.UTC(2026, 0, 1 + i)).toISOString().slice(0, 10))
  const WEEK = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
  const [hbar] = xy("hbar", MERCHANTS, [])
  const stackedHbar: [ChartSettings, object[]] = [
    { ...hbar, stacked: true, series: { values: [{ key: "a", label: "Settled", color: "series-1", value: "a" }, { key: "b", label: "Declined", color: "series-2", value: "b" }] } } as unknown as ChartSettings,
    MERCHANTS.map((x, i) => ({ x, a: 900 + (i % 40) * 20, b: 300 + (i % 9) * 30 })),
  ]
  // 60 days, the first 20 joined and the rest a value alone every 5th: at 1300 px each
  // point is drawn, and on a phone the joined days' are left out.
  const DAYS_60 = DAYS_120.slice(0, 60)
  const sparse: [ChartSettings, object[]] = [xy("line", DAYS_60, [])[0], DAYS_60.map((x, i) => ({ x, v: i < 20 || i % 5 === 0 ? 100 + i : null }))]
  const cases: [string, [ChartSettings, object[]], [number, number], [number, number]][] = [
    ["a stacked bar of 367 days, from 1300 px to a phone's", stackedDays(), [1300, HEIGHT], [375, HEIGHT]],
    ["a histogram of 120 bins, from 1300 px to a phone's", bins(120, 50), [1300, HEIGHT], [375, HEIGHT]],
    ["a heatmap of 120 days by weekday, from 1300 px to a phone's", heatmap(DAYS_120, WEEK), [1300, HEIGHT], [375, HEIGHT]],
    ["a heatmap of 120 days by weekday, from a phone's to 1300 px", heatmap(DAYS_120, WEEK), [375, HEIGHT], [1300, HEIGHT]],
    ["a stacked hbar of 120 merchants, from 1300 px tall to 180", stackedHbar, [568, 1300], [568, 180]],
    ["a heatmap of a week by 120 merchants, from 1300 px tall to 180", heatmap(WEEK, MERCHANTS), [568, 1300], [568, 180]],
    ["a bar of 17 categories, from 1300 px to a phone's", xy("bar", CATEGORIES, falling(17, 9_000), "category"), [1300, HEIGHT], [317, HEIGHT]],
    ["a bar of 17 categories, from a phone's to 1300 px", xy("bar", CATEGORIES, falling(17, 9_000), "category"), [317, HEIGHT], [1300, HEIGHT]],
    ["a treemap at the cap, from 1300x360 to a phone's", merchants("treemap"), [1300, 360], [375, HEIGHT]],
    ["a sunburst at the cap, from 1300x360 to a phone's", merchants("sunburst"), [1300, 360], [375, HEIGHT]],
    ["a pie of 17 categories, from 1300 px to a phone's", slicesOf("pie"), [1300, HEIGHT], [317, HEIGHT]],
    ["a funnel of 120 merchants, from 1300 px tall to 180", xy("funnel", MERCHANTS, MERCHANTS.map((_, i) => Math.round(90_000 / (1 + i))), "Stage", "Count of rows"), [568, 1300], [568, 180]],
    ["a calendar of a year, from 1300 px to a phone's", year(), [1300, HEIGHT], [317, HEIGHT]],
    ["a box plot of 17 categories, from 1300 px to a phone's", boxplot(CATEGORIES), [1300, HEIGHT], [317, HEIGHT]],
    ["a line of 60 days with values alone, from 1300 px to a phone's", sparse, [1300, HEIGHT], [317, HEIGHT]],
  ]
  for (const [name, [settings, rows], from, to] of cases) {
    it(`${name} is drawn as it is drawn there from nothing`, () => {
      expect(marks(redrawn(settings, rows, from, to))).toBe(marks(draw(settings, rows, ...to)))
    })
  }

  // The points the phone's draw kept are listed before those the wider one adds: the same
  // points, of one colour and apart, so only their order in the SVG differs.
  it("a line of 60 days with values alone, from a phone's to 1300 px, draws what it draws there from nothing", () => {
    const each = (svg: string) => marks(svg).split(/(?=<)/).sort()
    const fresh = draw(...sparse, 1300)
    expect(points(fresh)).toEqual(DAYS_60.flatMap((_, i) => (i < 20 || i % 5 === 0 ? [i] : [])))
    expect(each(redrawn(...sparse, [317, HEIGHT], [1300, HEIGHT]))).toEqual(each(fresh))
  })
})
