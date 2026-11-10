import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import type { ChartSettings } from "@/components/charts/chart-settings"
import type { PipelineUsage } from "@/lib/api/usage"

// Launch review, PR5: the Usage page on the kit. A limited meter turns amber from
// 80% and red at the limit, with the share in words (every meter was one violet
// gradient); the transfer tiles are StatCards; rows written per day and the top
// 10 pipelines are charts; and the table's names link to their pipelines, with
// the status as the Pipelines list says it.

vi.mock("@/config/features", () => ({ useUsagePanelState: () => "on" }))
const push = vi.fn()
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }))
const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: (...a: unknown[]) => authFetch(...a) }))
// A chart as its bars, one button per category, so a test can click one.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings, onCategoryClick }: { settings: ChartSettings; onCategoryClick?: (i: number) => void }) => (
    <div role="img" aria-label={settings.description}>
      {("x" in settings ? settings.x.categories : []).map((c, i) => (
        <button key={`${c}-${i}`} type="button" onClick={() => onCategoryClick?.(i)}>
          {`bar ${c}`}
        </button>
      ))}
    </div>
  ),
}))

import UsagePage from "@/app/(dashboard)/usage/page"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const pipeline = (id: string, over: Partial<PipelineUsage> = {}): PipelineUsage => ({
  id,
  name: id,
  status: "passed",
  rows_read: 0,
  rows_written: 0,
  records_processed: 0,
  cdc_inserts: 0,
  cdc_updates: 0,
  cdc_deletes: 0,
  cdc_applied: 0,
  last_activity: null,
  ...over,
})

function usage(over: Record<string, unknown> = {}) {
  return {
    workspace_id: "w-1",
    plan: "starter",
    pipeline_limit: null,
    pipelines_used: 3,
    can_run: true,
    blocked: false,
    plan_expires_at: null,
    queries_used: 0,
    queries_limit: null,
    transfer: {
      rows_read: 1_234_567,
      rows_written: 1_200_000,
      records_processed: 1_234_000,
      cdc_events_applied: 0,
      gb: 0,
      gb_metered: true,
      gb_limit: null,
    },
    pipelines: [],
    retention_enabled: false,
    retention_days: 0,
    ...over,
  }
}

const DAYS = ["2026-10-01", "2026-10-02", "2026-10-03"]

function serve(body: unknown, rowsWritten: { day: string; rows: number }[] = []) {
  authFetch.mockImplementation(async (url: string) => {
    const json = String(url).includes("/activity/metrics")
      ? { days: DAYS, time_zone: "UTC", rows_written: rowsWritten }
      : body
    return { ok: true, status: 200, json: async () => json, text: async () => "" }
  })
}

const card = (title: string) => screen.getByRole("region", { name: title })

beforeEach(() => {
  authFetch.mockReset()
  push.mockReset()
})

describe("Usage meters", () => {
  it("turn amber from 80% and red at the limit, and say the share in words", async () => {
    serve(
      usage({
        pipeline_limit: 10,
        pipelines_used: 8,
        queries_used: 30,
        queries_limit: 120,
        transfer: { ...usage().transfer, gb: 10, gb_limit: 10 },
      }),
    )
    render(<UsagePage />)

    const pipelines = await screen.findByRole("progressbar", { name: "Pipelines" })
    expect(screen.getByText("8 of 10 used · 80%")).toHaveClass("text-amber-700")
    expect(pipelines.firstElementChild).toHaveClass("bg-amber-500")

    const gb = screen.getByRole("progressbar", { name: "Data transfer (this month)" })
    expect(screen.getByText("10.00 of 10 GB · 100%, limit reached")).toHaveClass("text-red-700")
    expect(gb.firstElementChild).toHaveClass("bg-red-600")
    expect(gb).toHaveAttribute("aria-valuetext", "10.00 of 10 GB, 100%, limit reached")

    const queries = screen.getByRole("progressbar", { name: "NL→SQL queries (this month)" })
    expect(screen.getByText("30 of 120 queries · 25%")).not.toHaveClass("text-amber-700")
    expect(queries.firstElementChild).not.toHaveClass("bg-amber-500")
    expect(queries.firstElementChild).not.toHaveClass("bg-red-600")
  })
})

describe("Usage totals", () => {
  it("shows rows read, rows written and records processed on three cards", async () => {
    serve(usage())
    render(<UsagePage />)
    await screen.findByRole("region", { name: "Rows read" })
    expect(card("Rows read").querySelector("p")).toHaveTextContent("1,234,567")
    expect(within(card("Rows read")).getByText("From sources, to date")).toBeInTheDocument()
    expect(card("Rows written").querySelector("p")).toHaveTextContent("1,200,000")
    expect(card("Records processed").querySelector("p")).toHaveTextContent("1,234,000")
  })

  it("says the window when run-stat retention cuts the totals short", async () => {
    serve(usage({ retention_enabled: true, retention_days: 30 }))
    render(<UsagePage />)
    expect(await screen.findByText("To destinations, last 30 days")).toBeInTheDocument()
    // The top pipelines are summed from the same run stats, so they say the window too.
    expect(screen.getByText("By rows written, last 30 days. Click a bar to open its pipeline.")).toBeInTheDocument()
    expect(screen.queryByText(/to date/)).toBeNull()
  })
})

describe("Usage charts", () => {
  it("draws rows written per day, and reads the 30-day window when asked", async () => {
    serve(usage(), [{ day: "2026-10-02", rows: 500 }])
    render(<UsagePage />)

    expect(await screen.findByRole("img", { name: "Rows written by all pipeline runs per day" })).toBeInTheDocument()
    expect(authFetch.mock.calls.some(([u]) => String(u).includes("/activity/metrics?days=7"))).toBe(true)

    await userEvent.click(screen.getByRole("button", { name: /^30d/ }))
    await waitFor(() =>
      expect(authFetch.mock.calls.some(([u]) => String(u).includes("/activity/metrics?days=30"))).toBe(true),
    )
  })

  it("names the day's time zone once the metrics say it, not UTC until then", async () => {
    let answer: (tz: string) => void = () => {}
    authFetch.mockImplementation((url: string) => {
      if (!String(url).includes("/activity/metrics")) {
        return Promise.resolve({ ok: true, status: 200, json: async () => usage(), text: async () => "" })
      }
      return new Promise((resolve) => {
        answer = (time_zone) =>
          resolve({ ok: true, status: 200, json: async () => ({ days: DAYS, time_zone, rows_written: [] }), text: async () => "" })
      })
    })
    render(<UsagePage />)
    expect(await screen.findByText("All pipelines, per day")).toBeInTheDocument()
    expect(screen.queryByText(/per UTC day/)).toBeNull()

    act(() => answer("Europe/Berlin"))
    expect(await screen.findByText("All pipelines, per Europe/Berlin day")).toBeInTheDocument()
  })

  it("says so when no pipeline wrote rows in the window", async () => {
    serve(usage())
    render(<UsagePage />)
    expect(await screen.findByText("No pipeline wrote rows in the last 7 days.")).toBeInTheDocument()
    expect(screen.getByText("No pipeline has written rows yet.")).toBeInTheDocument()
  })

  it("ranks the 10 pipelines that wrote the most rows, and a bar opens its pipeline", async () => {
    const many = Array.from({ length: 12 }, (_, i) => pipeline(`p${i + 1}`, { rows_written: (i + 1) * 100 }))
    serve(
      usage({
        pipelines: [
          ...many,
          pipeline("idle-one", { rows_written: 0 }),
          // Two pipelines with one name: the second gets a number, so each keeps its bar.
          pipeline("dup-a", { name: "orders", rows_written: 5_000 }),
          pipeline("dup-b", { name: "orders", rows_written: 4_000 }),
        ],
      }),
    )
    render(<UsagePage />)

    const chart = await screen.findByRole("img", { name: /wrote the most rows/ })
    const bars = within(chart).getAllByRole("button").map((b) => b.textContent)
    expect(bars).toEqual([
      "bar orders",
      "bar orders (2)",
      "bar p12",
      "bar p11",
      "bar p10",
      "bar p9",
      "bar p8",
      "bar p7",
      "bar p6",
      "bar p5",
    ])
    await userEvent.click(within(chart).getByRole("button", { name: "bar orders (2)" }))
    expect(push).toHaveBeenCalledWith("/pipelines/dup-b")
  })
})

describe("Usage table", () => {
  it("links each pipeline and says its status as the Pipelines list does", async () => {
    serve(
      usage({
        pipelines: [
          pipeline("p-1", { name: "orders_sync", status: "passed" }),
          pipeline("p-2", { name: "payments_backfill", status: "paused" }),
          pipeline("p-3", { name: "fresh", status: "draft" }),
        ],
      }),
    )
    render(<UsagePage />)

    const table = await screen.findByRole("table", { name: "Per-pipeline transfer" })
    expect(within(table).getByRole("link", { name: "orders_sync" })).toHaveAttribute("href", "/pipelines/p-1")
    expect(within(table).getByText("Succeeded")).toBeInTheDocument()
    expect(within(table).getByText("Paused")).toBeInTheDocument()
    // A raw status the backend did not reconcile is spelled as sent.
    expect(within(table).getByText("Draft")).toBeInTheDocument()
  })
})
