import { afterEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import {
  MIN_BAR_PERCENT,
  RunDurationChart,
  type RunBarKind,
  SKIPPED_BAR_PERCENT,
  barHeightPercent,
  longestRunMs,
  usePointedRun,
} from "@/components/explorer/RunDurationChart"
import type { BarChartSettings } from "@/components/charts/chart-settings"
import { uniqueRunLabels } from "@/components/runs/run-chart-specs"
import type { SavedQueryRun } from "@/components/explorer/scheduledModel"

// ECharts draws on a canvas jsdom does not have; the stand-in keeps what it was asked
// to draw, a bar per category, so a test can read the settings and click a bar.
let drawn: { settings: BarChartSettings; rows: { run: string; kind: string; value: number }[] } | null = null
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({
    settings,
    rows,
    onCategoryClick,
  }: {
    settings: BarChartSettings
    rows: { run: string; kind: string; value: number }[]
    onCategoryClick?: (i: number) => void
  }) => {
    drawn = { settings, rows }
    return (
      <div data-testid="run-time-chart">
        {settings.x.categories.map((c, i) => (
          <button key={c} type="button" data-kind={rows[i].kind} onClick={() => onCategoryClick?.(i)}>
            {c}
          </button>
        ))}
      </div>
    )
  },
}))

function run(id: string, startedAt: string, overrides: Partial<SavedQueryRun> = {}): SavedQueryRun {
  return {
    run_id: id,
    saved_query_id: "q-1",
    trigger_source: "scheduled",
    status: "succeeded",
    started_at: startedAt,
    finished_at: startedAt,
    duration_ms: 60_000,
    ...overrides,
  }
}

// Newest first, the order GET /explorer/saved/:id/runs sends them in.
const RUNS = [
  run("r-newest", "2026-09-16T03:00:00Z", { status: "skipped", duration_ms: -1, skip_reason: "waiting_on_upstreams" }),
  run("r-failed", "2026-09-15T03:00:00Z", { status: "failed", duration_ms: 30_000 }),
  run("r-tiny", "2026-09-14T03:00:00Z", { duration_ms: 5 }),
  run("r-oldest", "2026-09-13T03:00:00Z", { duration_ms: 120_000 }),
]

function chart() {
  if (!drawn) throw new Error("nothing was drawn")
  return drawn
}

describe("RunDurationChart", () => {
  afterEach(() => {
    cleanup()
    drawn = null
  })

  it("draws one bar per run, oldest on the left, on a shared axis", () => {
    render(<RunDurationChart runs={RUNS} />)

    const { settings, rows } = chart()
    expect(settings.x.categories).toEqual(
      uniqueRunLabels(["2026-09-13T03:00:00Z", "2026-09-14T03:00:00Z", "2026-09-15T03:00:00Z", "2026-09-16T03:00:00Z"]),
    )
    expect(rows.map((r) => r.kind)).toEqual(["succeeded", "succeeded", "failed", "skipped"])
    // The longest run is 2 minutes, so the axis counts minutes.
    expect(settings.y).toMatchObject({ title: "Minutes", format: "decimal" })
    expect(rows.map((r) => r.value)).toEqual([2, 5 / 60_000, 0.5, 0])
    expect(settings.stacked).toBe(true)
    expect(screen.getByText(/longest 2m/)).toBeInTheDocument()
  })

  // Skipped is the grey skipped token on every page; it was amber here alone (launch review bug 10).
  it("colours succeeded green, failed red and skipped grey", () => {
    render(<RunDurationChart runs={[...RUNS, run("r-future", "2026-09-12T03:00:00Z", { status: "cancelled" })]} />)

    expect(chart().settings.series.values).toEqual([
      { key: "succeeded", label: "Succeeded", color: "succeeded" },
      { key: "failed", label: "Failed", color: "failed" },
      { key: "skipped", label: "Skipped", color: "skipped" },
      // A status this client does not know is still drawn, in grey, not dropped.
      { key: "__other__", label: "Other", color: "other" },
    ])
    expect(chart().rows[0].kind).toBe("__other__")
  })

  it("keeps a skip and a 5 ms run visible beside a run of minutes", () => {
    render(<RunDurationChart runs={RUNS} />)
    // A skip did no work: it is drawn at 0, which the least bar height still shows,
    // whatever its recorded duration (here -1 ms of clock jitter).
    expect(chart().settings.minBarHeight).toBeGreaterThan(0)
    expect(chart().rows[3]).toMatchObject({ kind: "skipped", value: 0 })
  })

  it("does not let a skip set the scale", () => {
    // A skipped row with a large recorded duration must not shrink the runs that did work.
    const runs = [
      run("r-skip", "2026-09-16T03:00:00Z", { status: "skipped", duration_ms: 999_999 }),
      run("r-ok", "2026-09-15T03:00:00Z", { duration_ms: 1_000 }),
    ]
    expect(longestRunMs(runs)).toBe(1_000)
    render(<RunDurationChart runs={runs} />)
    expect(chart().settings.y.title).toBe("Seconds")
    expect(chart().rows.map((r) => r.value)).toEqual([1, 0])
  })

  // The run grid's cells still size their bars by percent of the page's longest run.
  it("sizes a grid cell's bar by percent, with a floor and a stub for a skip", () => {
    const longest = longestRunMs(RUNS)
    expect(barHeightPercent(RUNS[3], longest)).toBe(100)
    expect(barHeightPercent(RUNS[1], longest)).toBe(25)
    expect(barHeightPercent(RUNS[2], longest)).toBe(MIN_BAR_PERCENT)
    expect(barHeightPercent(RUNS[0], longest)).toBe(SKIPPED_BAR_PERCENT)
  })

  // Seen in a browser on 2026-09-16: stg_orders ran in 16 to 181 ms, and every bar and
  // the caption's "longest 0s" said 0 seconds.
  it("counts sub-second runs in milliseconds", () => {
    const runs = [
      run("r-slow", "2026-09-16T03:00:00Z", { duration_ms: 181 }),
      run("r-fast", "2026-09-15T03:00:00Z", { status: "failed", duration_ms: 16 }),
    ]
    render(<RunDurationChart runs={runs} />)

    expect(chart().settings.y).toMatchObject({ title: "Milliseconds", format: "integer" })
    expect(chart().rows.map((r) => r.value)).toEqual([16, 181])
    expect(screen.getByText(/longest 181ms/)).toBeInTheDocument()
  })

  it("gives no longest when nothing on the page did work", () => {
    const skips = [
      run("r-2", "2026-09-16T03:00:00Z", { status: "skipped", duration_ms: 0 }),
      run("r-1", "2026-09-15T03:00:00Z", { status: "skipped", duration_ms: 0 }),
    ]
    render(<RunDurationChart runs={skips} />)

    expect(within(screen.getByTestId("run-time-chart")).getAllByRole("button")).toHaveLength(2)
    expect(screen.queryByText(/longest/)).not.toBeInTheDocument()
  })

  it("hands a clicked bar's run to onRunClick, and says so only when it can", async () => {
    const user = userEvent.setup()
    const onRunClick = vi.fn()
    const { rerender } = render(<RunDurationChart runs={RUNS} />)
    expect(screen.queryByText(/Click a bar/)).not.toBeInTheDocument()

    rerender(<RunDurationChart runs={RUNS} onRunClick={onRunClick} />)
    expect(screen.getByText("Click a bar to find its run below.")).toBeInTheDocument()
    await user.click(within(screen.getByTestId("run-time-chart")).getAllByRole("button")[2])
    expect(onRunClick).toHaveBeenCalledWith("r-failed")
  })

  // A workflow's runs are charted by what they did, not by a saved query's three statuses.
  it("draws, names and keys the kinds it is given", () => {
    const kinds: RunBarKind[] = [
      { status: "sent", label: "sent", color: "succeeded" },
      { status: "quiet", label: "not sent (quiet period)", color: "waiting" },
    ]
    const runs = [
      // A run that did work in 0 ms is measured, not a no-work stub.
      run("r-quiet", "2026-09-16T03:00:00Z", { status: "quiet", duration_ms: 0 }),
      run("r-sent", "2026-09-15T03:00:00Z", { status: "sent", duration_ms: 4_000 }),
    ]
    render(<RunDurationChart runs={runs} kinds={kinds} />)

    expect(chart().settings.series.values).toEqual([
      { key: "sent", label: "Sent", color: "succeeded" },
      { key: "quiet", label: "Not sent (quiet period)", color: "waiting" },
    ])
    expect(chart().rows).toMatchObject([
      { kind: "sent", value: 4 },
      { kind: "quiet", value: 0 },
    ])
  })

  it("draws nothing for no runs", () => {
    const { container } = render(<RunDurationChart runs={[]} />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe("usePointedRun", () => {
  afterEach(cleanup)

  function Table() {
    const { point, rowProps } = usePointedRun()
    return (
      <>
        <button type="button" onClick={() => point("r-2")}>
          point
        </button>
        <table>
          <tbody>
            {["r-1", "r-2"].map((id) => (
              <tr key={id} {...rowProps(id)}>
                <td>{id}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </>
    )
  }

  it("scrolls to the run a bar was clicked for and marks its row current", async () => {
    const user = userEvent.setup()
    const scrollIntoView = vi.fn()
    Element.prototype.scrollIntoView = scrollIntoView
    render(<Table />)
    const row = (id: string) => screen.getByText(id).closest("tr")!
    expect(row("r-2")).not.toHaveAttribute("aria-current")

    await user.click(screen.getByRole("button", { name: "point" }))

    expect(row("r-2")).toHaveAttribute("aria-current", "true")
    expect(row("r-2").className).toMatch(/bg-violet-50/)
    expect(row("r-1")).not.toHaveAttribute("aria-current")
    expect(scrollIntoView).toHaveBeenCalledTimes(1)
    expect(scrollIntoView.mock.instances[0]).toBe(row("r-2"))
  })
})
