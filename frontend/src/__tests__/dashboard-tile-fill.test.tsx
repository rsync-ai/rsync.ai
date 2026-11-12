import { readFileSync } from "node:fs"
import path from "node:path"

import { beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, within } from "@testing-library/react"

import { SavedChart } from "@/components/charts/SavedChart"
import type { ChartBadges, ChartDraw, ChartResult, ServerChartSettings } from "@/lib/api/charts"

// A chart in a dashboard tile (dashboards-design.md § 3.3, § 5). A tile's height is
// set by its rows, but what sits above the chart is not: the badges wrap to more
// lines on a narrow tile, and each warning adds a line or two. The tile used to
// size the plot as its height less a fixed 92 px, so on prod a 3-row scorecard
// with three lines of badges had its number pushed under the tile's scrollbar.
//
// Then the badges and warnings went in a box above the chart that shrank to one
// line and scrolled: every tile on prod had a second scrollbar showing one line of
// badges at a time (2026-10-06).
//
// What is pinned: in a tile nothing sits above the chart; its badges and warnings
// are behind the tile header's ChartInfo. A scorecard keeps its own height, a plot
// or table takes the tile's room, and every tile renderer asks for both. jsdom has
// no layout, so these pin the boxes and their flex rules; the sizes were measured
// in a browser (PR body).

const heights = vi.hoisted(() => [] as unknown[])
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ height }: { height?: unknown }) => {
    heights.push(height)
    return <div data-testid="echart-stub" />
  },
}))

beforeEach(() => {
  heights.length = 0
  // The change's tooltip measures its trigger.
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const badges: ChartBadges = {
  source_kind: "model",
  source_id: "m1",
  source_name: "swipes_daily",
  data_as_of: "2026-10-06T04:12:00Z",
  data_as_of_from: "run_finished",
  deadline_seconds: 86400,
  late: false,
  checks: [
    { name: "not_empty", severity: "error", status: "pass" },
    { name: "no_null_ids", severity: "warn", status: "pass" },
  ],
  warnings: ["The model's last run failed; these rows are from 2026-10-05 11:30 UTC."],
  lineage_url: "/explorer/lineage?focus=model%3Am1",
}

const result = (columns: [string, string][], rows: unknown[][]): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked: [],
  unfiltered: [],
  sql: "",
  read_at: "2026-10-06T04:41:00Z",
  execution_time_ms: 3,
})

const draw = (r: ChartResult): ChartDraw => ({ result: r, break: null, badges })

const CARD: ServerChartSettings = {
  version: 2,
  type: "scorecard",
  roles: { measure: [{ aggregate: "count" }], date: [{ column: "day" }] },
  compare: { days: 7 },
}
const CARD_DRAW = draw(result([["count", "the measure"], ["previous", "the earlier period"]], [[40005, 39000]]))
const BAR: ServerChartSettings = { version: 2, type: "bar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } }
const BAR_DRAW = draw(result([["x", "the x"], ["v", "the measure"]], [["2026-10-01T00:00:00Z", 10]]))
const TABLE: ServerChartSettings = { version: 2, type: "table", roles: { columns: [{ column: "region" }], measure: [{ aggregate: "count" }] } }
const TABLE_DRAW = draw(result([["region", "table column region"], ["count", "the measure"]], [["EU", 5]]))

const classes = (el: Element) => el.className.split(/\s+/)

describe("a chart in a dashboard tile", () => {
  it("draws nothing above a scorecard's number: its badges and warnings are the tile header's, so nothing scrolls", () => {
    render(<SavedChart settings={CARD} draw={CARD_DRAW} ignored={["Swipe date"]} fill />)
    // It has badges and two warnings, and none of them is drawn here.
    expect(screen.queryByTestId("saved-chart-about")).toBeNull()
    expect(screen.queryByLabelText("About these rows")).toBeNull()
    expect(screen.queryByLabelText("Warnings")).toBeNull()
    const chart = screen.getByTestId("saved-chart")
    expect(chart.querySelector(".overflow-y-auto")).toBeNull()
    expect(chart.firstElementChild).toBe(screen.getByTestId("saved-chart-body"))

    const number = screen.getByTestId("scorecard-value")
    expect(number.textContent).toBe("40,005")
    // The number's box never gives up its height.
    const body = screen.getByTestId("saved-chart-body")
    expect(body.contains(number)).toBe(true)
    expect(classes(body)).toContain("shrink-0")
    expect(classes(body)).not.toContain("min-h-0")
    // And has no room above its label or below its number, so a 3-row tile fits the label and the number.
    expect(classes(body.firstElementChild!)).not.toContain("py-2")
    expect(classes(body.firstElementChild!)).not.toContain("pb-2")

    // The chart fills its tile: a flex column the tile's body sizes.
    expect(classes(screen.getByTestId("saved-chart"))).toEqual(expect.arrayContaining(["flex-1", "min-h-0", "flex-col"]))
  })

  it("puts a scorecard's change beside its number, in short, and keeps the sentence for hover and screen readers", async () => {
    // On prod a 3-row tile's body has 90 px; the change on its own line, under a line
    // of badges, took it to 116. Beside the number, with the badges in the header, it
    // is 56: label 16, gap 4, number 36.
    render(<SavedChart settings={CARD} draw={CARD_DRAW} fill />)
    const number = screen.getByTestId("scorecard-value")
    const change = screen.getByTestId("scorecard-change")
    const row = number.parentElement!
    expect(row.contains(change)).toBe(true)
    // One row; on a tile too narrow for both, the change wraps under the number rather than squeezing it.
    expect(classes(row)).toEqual(expect.arrayContaining(["flex", "flex-wrap"]))
    expect(classes(row)).not.toContain("flex-col")
    // A 30 px line (leading-none) left the font's 36 px text box 3 px past it, and the tile
    // scrolled by those 3 px: the number keeps its line, and what its text box reaches past it is clipped.
    expect(classes(number)).toContain("overflow-y-clip")
    expect(classes(number)).not.toContain("leading-none")

    const sentence = "+2.6% on the 7 days before (39,000)"
    expect(within(change).getByText("+2.6%").getAttribute("aria-hidden")).toBe("true")
    expect(classes(within(change).getByText(sentence))).toContain("sr-only")
    // A keyboard reaches it, and hover or focus shows the sentence.
    expect(change.tabIndex).toBe(0)
    fireEvent.focus(change)
    expect((await screen.findByRole("tooltip")).textContent).toBe(sentence)
  })

  // The number sat top-left of a tile that is often 350 px tall, the rest blank
  // (launch review, PR1). It is centred in the room the tile has and scales with the
  // tile's width, from text-3xl to 48 px: label 16 + gap 4 + a 58 px line still fits a
  // 3-row tile's 90 px body.
  it("centres a scorecard's number in its tile and scales it to the tile's width", () => {
    render(<SavedChart settings={CARD} draw={CARD_DRAW} fill />)
    const body = screen.getByTestId("saved-chart-body")
    // It takes the tile's room, but never less than its own label and number.
    expect(classes(body)).toEqual(expect.arrayContaining(["grow", "shrink-0", "flex-col"]))
    expect(classes(body)).not.toContain("basis-0")
    const card = body.firstElementChild!
    expect(classes(card)).toEqual(expect.arrayContaining(["@container", "flex-1", "flex-col"]))
    const number = screen.getByTestId("scorecard-value")
    expect(classes(number.parentElement!)).toEqual(expect.arrayContaining(["flex-1", "items-center", "justify-center"]))
    expect(classes(number)).toEqual(expect.arrayContaining(["text-[clamp(1.875rem,8cqi,3rem)]", "leading-[1.2]"]))
  })

  it("colours a scorecard's change by its direction", () => {
    const tone = (current: number | null, previous: number | null) => {
      const { unmount } = render(
        <SavedChart
          settings={CARD}
          draw={draw(result([["count", "the measure"], ["previous", "the earlier period"]], [[current, previous]]))}
          fill
        />,
      )
      const change = screen.getByTestId("scorecard-change")
      const out = { pill: classes(change).includes("rounded-full"), bg: classes(change).find((c) => c.startsWith("bg-")) }
      unmount()
      return out
    }
    expect(tone(40005, 39000)).toEqual({ pill: true, bg: "bg-emerald-50" })
    expect(tone(30000, 39000)).toEqual({ pill: true, bg: "bg-red-50" })
    expect(tone(39000, 39000)).toEqual({ pill: true, bg: "bg-zinc-100" })
    expect(tone(39000, null)).toEqual({ pill: true, bg: "bg-zinc-100" })
  })

  // Launch review bug 17: a tile titled "Count" said "Count" again over its number.
  it("leaves out a scorecard's label when the tile's title says the same, and keeps the period", () => {
    const label = (title: string, settings: ServerChartSettings = CARD) => {
      const { unmount } = render(<SavedChart settings={settings} draw={CARD_DRAW} fill title={title} />)
      // The card's first line is its label, unless the number's row comes first.
      const first = screen.getByTestId("saved-chart-body").firstElementChild!.firstElementChild!
      const out = first.contains(screen.getByTestId("scorecard-value")) ? null : first.textContent
      unmount()
      return out
    }
    expect(label("Swipes")).toBe("Count · last 7 days")
    expect(label(" count ")).toBe("Last 7 days")
    // With no period to compare, a label that says the title again is left out whole.
    expect(label("Count", { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } })).toBeNull()
    expect(label("Swipes", { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count" }] } })).toBe("Count")
  })

  // GUI review D-1 (2026-10-10): a tile titled "Card transactions · last 7 days" said
  // "Card transactions · last 7 days" again over its number, and "Loans, all statuses"
  // said "Loans". Only a title that was the measure's name, word for word, counted.
  it("leaves out whatever the tile's title already says: the measure's name, its period, or both", () => {
    const label = (title: string, settings: ServerChartSettings) => {
      const { unmount } = render(<SavedChart settings={settings} draw={CARD_DRAW} fill title={title} />)
      const first = screen.getByTestId("saved-chart-body").firstElementChild!.firstElementChild!
      const out = first.contains(screen.getByTestId("scorecard-value")) ? null : first.textContent
      unmount()
      return out
    }
    const card: ServerChartSettings = { ...CARD, roles: { ...CARD.roles, measure: [{ aggregate: "count", title: "Card transactions" }] } }
    const loans: ServerChartSettings = { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "count", title: "Loans" }] } }
    expect(label("Card transactions · last 7 days", card)).toBeNull()
    expect(label("Card transactions, last 7 days", card)).toBeNull()
    expect(label("Card transactions", card)).toBe("Last 7 days")
    expect(label("Swipes · last 7 days", card)).toBe("Card transactions")
    expect(label("Loans, all statuses", loans)).toBeNull()
    // A title that only starts the same way names something else: the label stays.
    expect(label("Card", card)).toBe("Card transactions · last 7 days")
    expect(label("Loansharks", loans)).toBe("Loans")
  })

  // Launch review bug 18: on a tile with room, the change was a pill whose meaning
  // ("on the 7 days before") was only in a tooltip a touch screen never shows.
  it("writes a scorecard's change out under its number in a tile of 4 rows or more", () => {
    const sentence = "+2.6% on the 7 days before (39,000)"
    const { unmount } = render(<SavedChart settings={CARD} draw={CARD_DRAW} fill rows={4} />)
    expect(screen.getByTestId("scorecard-sentence").textContent).toBe(sentence)
    const pill = screen.getByTestId("scorecard-change")
    expect(pill.textContent).toBe("+2.6%")
    expect(classes(pill)).toEqual(expect.arrayContaining(["rounded-full", "bg-emerald-50"]))
    // The sentence is read once: the pill is its short form, hidden from a screen reader, and not a tooltip trigger.
    expect(pill.getAttribute("aria-hidden")).toBe("true")
    expect(pill.tabIndex).toBe(-1)
    unmount()

    // A 3-row tile keeps the short pill and the sentence for hover and screen readers.
    render(<SavedChart settings={CARD} draw={CARD_DRAW} fill rows={3} />)
    expect(screen.queryByTestId("scorecard-sentence")).toBeNull()
    expect(classes(within(screen.getByTestId("scorecard-change")).getByText(sentence))).toContain("sr-only")
  })

  // D4b (dashboards-design.md § 14.9): a scorecard's sparkline is its current period.
  it("draws a scorecard's sparkline under its number on a page and in a roomy tile, never in a 3-row tile", () => {
    const settings: ServerChartSettings = { ...CARD, sparkline: true }
    const spark = { ...CARD_DRAW, result: { ...CARD_DRAW.result!, spark: [["2026-10-01", 5], ["2026-10-02", 9], ["2026-10-03", -2], ["2026-10-04", -6]] } }
    const { unmount } = render(<SavedChart settings={settings} draw={spark} />)
    const line = screen.getByTestId("sparkline").querySelector("polyline")!
    const ys = line.getAttribute("points")!.split(" ").map((p) => Number(p.split(",")[1]))
    expect(ys).toHaveLength(4)
    // Values below zero stay inside the box, apart: the line runs from the lowest value up.
    for (const y of ys) {
      expect(y).toBeGreaterThanOrEqual(0)
      expect(y).toBeLessThanOrEqual(24)
    }
    expect(ys[3]).toBe(Math.max(...ys))
    expect(ys[3]).toBeGreaterThan(ys[2])
    unmount()

    const roomy = render(<SavedChart settings={settings} draw={spark} fill rows={4} />)
    expect(screen.getByTestId("sparkline")).toBeInTheDocument()
    roomy.unmount()
    render(<SavedChart settings={settings} draw={spark} fill rows={3} />)
    expect(screen.queryByTestId("sparkline")).toBeNull()
  })

  // Spaced a point apart, a day with no rows drew the days either side of it a day apart.
  it("breaks a scorecard's sparkline at a day with no rows, each day in its place", () => {
    const settings: ServerChartSettings = { ...CARD, sparkline: true }
    const gappy = [["2026-10-01", 5], ["2026-10-02", 9], ["2026-10-04", -6], ["2026-10-05", 2]]
    render(<SavedChart settings={settings} draw={{ ...CARD_DRAW, result: { ...CARD_DRAW.result!, spark: gappy } }} />)
    const lines = [...screen.getByTestId("sparkline").querySelectorAll("polyline")]
    const xs = lines.map((l) => l.getAttribute("points")!.split(" ").map((p) => Number(p.split(",")[0])))
    // Five days, the 3rd with no rows: two lines, a quarter of the width a day.
    expect(xs).toEqual([[0, 25], [75, 100]])
  })

  it("lines a table's column of numbers up on the right, as it does the measure", () => {
    const settings: ServerChartSettings = { version: 2, type: "table", roles: { columns: [{ column: "region" }, { column: "orders" }] } }
    render(
      <SavedChart
        settings={settings}
        draw={draw(result([["region", "table column region"], ["orders", "table column orders"]], [["EU", 12], ["US", 7]]))}
        fill
      />,
    )
    const [region, orders] = screen.getAllByRole("columnheader")
    expect(classes(region)).toContain("text-left")
    expect(classes(orders)).toContain("text-right")
    expect(classes(screen.getByRole("cell", { name: "12" }))).toEqual(expect.arrayContaining(["text-right", "tabular-nums"]))
  })

  it("keeps a scorecard's change as a sentence on its own line outside a tile", () => {
    render(<SavedChart settings={CARD} draw={CARD_DRAW} />)
    const change = screen.getByTestId("scorecard-change")
    expect(change.textContent).toBe("+2.6% on the 7 days before (39,000)")
    expect(classes(screen.getByTestId("scorecard-value").parentElement!)).toContain("flex-col")
  })

  it("draws a plot into the room left, never a pixel height worked out from the tile's rows", () => {
    render(<SavedChart settings={BAR} draw={BAR_DRAW} height={260} fill />)
    expect(heights).toEqual(["fill"])
    const body = screen.getByTestId("saved-chart-body")
    expect(within(body).getByTestId("echart-stub")).toBeTruthy()
    // It grows into the tile's room, keeps at least 80 px, and never shrinks below that:
    // under a 3-row tile's 90 px body, so a 3-row plot or table does not scroll its tile.
    expect(classes(body)).toEqual(expect.arrayContaining(["grow", "basis-0", "shrink-0", "min-h-20", "flex-col"]))
    expect(screen.queryByTestId("saved-chart-about")).toBeNull()
  })

  it("lets a table shrink to the room left and scroll, with no height of its own", () => {
    render(<SavedChart settings={TABLE} draw={TABLE_DRAW} height={260} fill />)
    const scroller = screen.getByRole("table").parentElement!
    expect(scroller.style.maxHeight).toBe("")
    expect(classes(scroller)).toEqual(expect.arrayContaining(["min-h-0", "overflow-auto"]))
    expect(classes(screen.getByTestId("saved-chart-body"))).toEqual(expect.arrayContaining(["grow", "min-h-20"]))
  })

  it("keeps the badges and warnings above the chart outside a tile, as on a chart's own page", () => {
    render(<SavedChart settings={CARD} draw={CARD_DRAW} ignored={["Swipe date"]} />)
    expect(screen.getByLabelText("About these rows")).toBeTruthy()
    expect(screen.getByLabelText("Warnings").querySelectorAll("li")).toHaveLength(2)
  })

  it("keeps the height it is given outside a tile, as on a chart's own page", () => {
    render(<SavedChart settings={BAR} draw={BAR_DRAW} height={260} />)
    expect(heights).toEqual([260])
    render(<SavedChart settings={TABLE} draw={TABLE_DRAW} height={200} />)
    expect(screen.getByRole("table").parentElement!.style.maxHeight).toBe("200px")
  })
})

describe("a chart's description", () => {
  // The builder offers "Description (read to a screen reader)" for every type; a plot
  // reads it as its label and its table's caption (EChart.tsx).
  const DESC = "Card swipes over the last 7 days, all channels"

  it.each([true, false])("reads a scorecard's and a table's to a screen reader, in a tile (%s) and on a page", (fill) => {
    const card = render(<SavedChart settings={{ ...CARD, description: DESC }} draw={CARD_DRAW} fill={fill} rows={3} />)
    expect(classes(screen.getByText(DESC))).toContain("sr-only")
    card.unmount()
    render(<SavedChart settings={{ ...TABLE, description: DESC }} draw={TABLE_DRAW} fill={fill} />)
    expect(screen.getByRole("table", { name: DESC })).toBeTruthy()
  })

  it("adds nothing when the author wrote none", () => {
    render(<SavedChart settings={TABLE} draw={TABLE_DRAW} fill />)
    expect(screen.getByRole("table").querySelector("caption")).toBeNull()
  })
})

describe("every tile renderer", () => {
  // The dashboard, its editor and a draft's preview each draw a chart in a tile
  // whose height is set: each must fill it rather than pass a height, and each
  // must put the chart's badges and warnings in the tile's header.
  const renderers = [
    "src/components/dashboards/DashboardPage.tsx",
    "src/app/(dashboard)/dashboard-proposals/[id]/page.tsx",
  ]

  it.each(renderers)("%s draws each chart filling its tile", (file) => {
    const source = readFileSync(path.join(process.cwd(), file), "utf8")
    // Every use, each up to the end of its tag (a prop may hold a "=>").
    const uses = source.split(/<SavedChart\b/).slice(1).map((rest) => rest.slice(0, rest.indexOf("/>")))
    expect(uses.length).toBeGreaterThan(0)
    for (const use of uses) {
      expect(use).toMatch(/\sfill\b/)
      expect(use).not.toMatch(/\bheight=/)
    }
    // A tile's chart without a ChartInfo would show no badges and no warnings at all.
    expect(source.split(/<ChartInfo\b/).length - 1).toBe(uses.length)
  })

  it.each(renderers)("%s shows a tile's whole title on hover where it is cut to fit", (file) => {
    const source = readFileSync(path.join(process.cwd(), file), "utf8")
    // Each element cut with an ellipsis whose text is the tile's title, up to the end of its tag.
    const cut = [...source.matchAll(/<\w+\b([^<>]*\btruncate\b[^<>]*)>\s*\{(?:chart\.name|title)\}\s*</g)].map((m) => m[1])
    expect(cut.length).toBeGreaterThan(0)
    for (const attrs of cut) expect(attrs).toMatch(/\stitle=\{(?:chart\.name|title)\}/)
  })
})
