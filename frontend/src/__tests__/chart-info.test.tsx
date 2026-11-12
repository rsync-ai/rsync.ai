import { beforeEach, describe, expect, it } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { ChartInfo } from "@/components/charts/ChartInfo"
import { formatAbsoluteTime } from "@/lib/utils"
import type { ChartBadges, ChartDraw, ChartResult } from "@/lib/api/charts"

// A dashboard tile's badges and warnings, behind one button in its header
// (dashboards-design.md § 5). The button's icons say on time or late, a failed check
// and how many warnings by shape as well as colour, and its name says all three to a
// screen reader; the popover has what SavedChart draws above a chart on its own page.

beforeEach(() => {
  // The popover measures its trigger.
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

const result: ChartResult = {
  columns: [{ alias: "count", role: "the measure" }],
  rows: [[40005]],
  truncated: false,
  masked: [],
  unfiltered: [],
  sql: "",
  read_at: "2026-10-06T04:41:00Z",
  execution_time_ms: 3,
}

const draw = (b: Partial<ChartBadges> | null, r: Partial<ChartResult> = {}): ChartDraw => ({
  result: { ...result, ...r },
  break: null,
  badges: b === null ? null : { ...badges, ...b },
})

const icons = (el: Element) => [...el.querySelectorAll("svg")].map((s) => s.getAttribute("class") ?? "")
const amber = (cls: string) => cls.includes("text-amber-600")

describe("a tile's ChartInfo", () => {
  it("names on time, the checks and the warnings, and counts the warnings, the ignored filter among them", () => {
    render(<ChartInfo draw={draw({})} ignored={["Swipe date"]} />)
    const button = screen.getByRole("button", { name: "About this chart: on time, 2 of 2 checks passed, 2 warnings" })
    expect(button.textContent).toBe("2")
    const [clock, checks, warning] = icons(button)
    expect(clock).toContain("lucide-clock")
    expect(amber(clock)).toBe(false)
    expect(checks).toContain("lucide-circle-check")
    expect(amber(checks)).toBe(false)
    // The triangle and its count, both in amber.
    expect(warning).toContain("lucide-triangle-alert")
    expect(amber(button.querySelector("span")!.className)).toBe(true)
  })

  it("shows overdue and a failed check in amber, each with its own icon", () => {
    const failed = [badges.checks[0], { name: "no_null_ids", severity: "warn", status: "fail" }]
    render(<ChartInfo draw={draw({ late: true, checks: failed, warnings: [] })} />)
    const button = screen.getByRole("button", { name: "About this chart: overdue, 1 of 2 checks passed" })
    const [clock, checks] = icons(button)
    expect(clock).toContain("lucide-clock-alert")
    expect(amber(clock)).toBe(true)
    expect(checks).toContain("lucide-circle-x")
    expect(amber(checks)).toBe(true)
    expect(icons(button)).toHaveLength(2)
  })

  it("is a plain info button for a saved query with nothing to warn of", () => {
    render(<ChartInfo draw={draw({ source_kind: "saved_query", deadline_seconds: undefined, checks: [], warnings: [] })} />)
    const button = screen.getByRole("button", { name: "About this chart" })
    expect(icons(button)).toHaveLength(1)
    expect(icons(button)[0]).toContain("lucide-info")
  })

  it("is not drawn when there is nothing to say", () => {
    const { container, rerender } = render(<ChartInfo draw={null} />)
    expect(container.innerHTML).toBe("")
    rerender(<ChartInfo draw={draw(null)} />)
    expect(container.innerHTML).toBe("")
  })

  it("opens the badges, each check and the warnings, and closes on Escape back to its button", async () => {
    const failed = [badges.checks[0], { name: "no_null_ids", severity: "warn", status: "fail" }]
    render(<ChartInfo draw={draw({ checks: failed })} ignored={["Swipe date"]} />)
    const button = screen.getByRole("button", { name: /^About this chart/ })
    button.focus()
    fireEvent.click(button)

    const dialog = await screen.findByRole("dialog", { name: "About this chart" })
    expect(within(dialog).getByText("Data as of")).toBeTruthy()
    expect(within(dialog).getByText("Deadline")).toBeTruthy()
    expect(within(dialog).getByText("On time")).toBeTruthy()
    expect(within(dialog).getByText("Read")).toBeTruthy()
    expect(within(dialog).getByText("Checks 1 of 2 passed")).toBeTruthy()
    const checks = within(within(dialog).getByRole("list", { name: "Checks" })).getAllByRole("listitem")
    expect(checks.map((li) => li.textContent)).toEqual(["not_emptypass", "no_null_idsfail"])
    const warnings = within(within(dialog).getByRole("list", { name: "Warnings" })).getAllByRole("listitem")
    // The gateway's UTC time, in the viewer's zone as "Data as of" is.
    expect(warnings.map((li) => li.textContent)).toEqual([
      `The model's last run failed; these rows are from ${formatAbsoluteTime("2026-10-05T11:30:00Z")}.`,
      "Not filtered by Swipe date: this chart's source has no such column.",
    ])
    expect(within(dialog).getByRole("link", { name: "Lineage" }).getAttribute("href")).toBe(badges.lineage_url)

    fireEvent.keyDown(dialog, { key: "Escape" })
    expect(screen.queryByRole("dialog")).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(button))
  })

  it("says Overdue, the models pages' word, for a missed deadline", async () => {
    render(<ChartInfo draw={draw({ late: true, warnings: [] })} />)
    fireEvent.click(screen.getByRole("button", { name: /^About this chart/ }))
    const dialog = await screen.findByRole("dialog", { name: "About this chart" })
    expect(within(dialog).getByText("Overdue")).toBeTruthy()
    expect(within(dialog).queryByText("Late")).toBeNull()
  })

  it("says only when a saved query was read, and has no lineage", async () => {
    render(<ChartInfo draw={draw({ source_kind: "saved_query", deadline_seconds: undefined, checks: [], warnings: [] })} />)
    fireEvent.click(screen.getByRole("button", { name: "About this chart" }))
    const dialog = await screen.findByRole("dialog", { name: "About this chart" })
    expect(within(dialog).getByText("Read")).toBeTruthy()
    expect(within(dialog).queryByText("Data as of")).toBeNull()
    expect(within(dialog).queryByRole("link")).toBeNull()
  })
})
