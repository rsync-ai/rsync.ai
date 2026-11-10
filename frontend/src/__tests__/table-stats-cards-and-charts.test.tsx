/**
 * The Table statistics tab as cards and charts (GUI launch review, PR 2).
 *
 * The summary was a grid of bare label/number tiles under a "Summary" title,
 * inside a card inside the tab's own card. It is now KPI cards (Tables, Rows
 * read/written or Changes captured/applied, Dropped) and two bar charts: tables
 * by status, and the ten tables that wrote the most.
 *
 * Pinned here: which cards a batch, CDC and mixed pipeline get; that a paused or
 * stopped pipeline's tables never read "running"; that the charts are drawn from
 * the summary and the rows; that the top ten is the whole run's (one extra
 * ten-row read for a batch run past one page) or says it is the page's; and that
 * the tab draws no frame round its cards.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, render, screen, waitFor, within } from "@testing-library/react"
import "@testing-library/jest-dom"
import { cardFact, cardValue } from "./statCards"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/pipelines/p1",
}))
vi.mock("@/lib/hooks/usePipelineRuntime", () => ({
  usePipelineRuntime: () => ({ runtime: null, loading: false, error: null }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
  authFetchOrThrow: (...a: unknown[]) => authFetch(...a),
}))

type DrawnChart = { settings: { description: string; x: { categories: string[] }; series: { values: { key: string; color: string }[] } }; rows: Record<string, unknown>[] }
const charts: DrawnChart[] = []
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: (props: DrawnChart) => {
    charts.push(props)
    return <div data-testid="chart" data-description={props.settings.description} />
  },
}))

import { TableStatisticsPanel } from "@/components/pipeline/TableStatisticsPanel"
import { PipelineMonitoringPanel } from "@/components/pipeline/PipelineMonitoringPanel"
import { tableStatusChart, tableStatusRows, topTablesRows } from "@/components/pipeline/table-stats-charts"

function ok(body: unknown) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) } as unknown as Response
}

const batch = (name: string, inserted: number, extra: Record<string, unknown> = {}) => ({
  qualified_name: `app.${name}`,
  table_name: name,
  schema_name: "app",
  mode: "batch",
  status: "completed",
  read_rows: inserted,
  inserted_rows: inserted,
  dlq_rows: 0,
  ...extra,
})

const cdc = (name: string, snapshot: number, applied: number) => ({
  qualified_name: `app.${name}`,
  table_name: name,
  schema_name: "app",
  mode: "cdc",
  status: "running",
  inserts: applied,
  total_events: applied,
  applied_inserts: applied,
  applied_total_events: applied,
  snapshot_rows: snapshot,
  applied_snapshot_rows: snapshot,
  dlq_rows: 0,
})

const statsCalls = () => authFetch.mock.calls.map(([u]) => new URL(String(u), "http://localhost")).filter((u) => u.pathname.endsWith("/table-stats"))

/** A ChartCard, found by its caption. */
function figure(title: string): HTMLElement {
  const fig = screen.getByText(title, { selector: "figcaption" }).closest("figure")
  if (!fig) throw new Error(`no figure "${title}"`)
  return fig
}
const findFigure = async (title: string) => (await screen.findByText(title, { selector: "figcaption" })).closest("figure")!

/** The chart a ChartCard draws, found by the card's title. */
function chartIn(title: string): DrawnChart {
  const fig = figure(title)
  const description = within(fig).getByTestId("chart").getAttribute("data-description")
  const drawn = [...charts].reverse().find((c) => c.settings.description === description)
  if (!drawn) throw new Error(`no chart drawn in "${title}"`)
  return drawn
}

function serve(body: { summary: Record<string, unknown>; tables: unknown[]; total?: number }, top?: unknown[]) {
  authFetch.mockImplementation(async (url: string) => {
    const u = new URL(String(url), "http://localhost")
    if (!u.pathname.endsWith("/table-stats")) return ok({})
    if (u.searchParams.get("sort") === "inserted_rows" && u.searchParams.get("limit") === "10") return ok({ ...body, tables: top ?? [] })
    return ok({ total: body.tables.length, ...body })
  })
}

const BATCH_SUMMARY = {
  mode: "batch",
  total_tables: 3,
  tables_completed: 2,
  tables_running: 0,
  tables_failed: 1,
  tables_degraded: 0,
  total_read_rows: 1_500,
  total_inserted_rows: 1_200,
  total_dlq_rows: 0,
}

beforeEach(() => {
  authFetch.mockReset()
  charts.length = 0
  localStorage.clear()
})
afterEach(() => cleanup())

describe("the summary cards", () => {
  it("batch: Tables, Rows read, Rows written and Dropped, four to a row", async () => {
    serve({ summary: BATCH_SUMMARY, tables: [batch("orders", 1_000), batch("users", 200), batch("empty", 0)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="completed" />)
    await screen.findByRole("region", { name: "Tables" })

    expect(cardValue("Tables")).toBe("3")
    expect(cardFact("Tables", "done")).toBe("2")
    expect(cardFact("Tables", "running")).toBe("0")
    expect(cardFact("Tables", "failed")).toBe("1")
    expect(cardValue("Rows read")).toBe("1,500")
    expect(cardValue("Rows written")).toBe("1,200")
    expect(cardValue("Dropped")).toBe("0")
    expect(cardValue("Changes captured")).toBeNull()
    expect(screen.getByRole("region", { name: "Tables" }).parentElement?.className).toContain("@3xl:grid-cols-4")
    // The old "Summary" heading and its tiles are gone.
    expect(screen.queryByText("Summary")).toBeNull()
  })

  it("CDC: Changes captured and applied with their counts, and no Rows read", async () => {
    serve({
      summary: {
        mode: "cdc",
        total_tables: 1,
        tables_completed: 0,
        tables_running: 1,
        tables_failed: 0,
        total_cdc_events: 46,
        total_inserts: 40,
        total_updates: 5,
        total_deletes: 1,
        total_applied_cdc_events: 45,
        total_applied_inserts: 40,
        total_applied_updates: 5,
        total_applied_deletes: 0,
        total_dlq_rows: 0,
      },
      tables: [cdc("events", 0, 45)],
    })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="running" />)
    await screen.findByRole("region", { name: "Tables" })

    expect(cardValue("Changes captured")).toBe("46")
    expect(cardFact("Changes captured", "updates")).toBe("5")
    expect(cardValue("Changes applied")).toBe("45")
    expect(cardFact("Changes applied", "deletes")).toBe("0")
    // No snapshot counts from this backend: no snapshot fact, rather than a 0.
    expect(cardFact("Changes applied", "snapshot rows")).toBeNull()
    expect(cardValue("Rows read")).toBeNull()
  })

  it("mixed: six cards, three to a row", async () => {
    serve({
      summary: { ...BATCH_SUMMARY, mode: "mixed", total_cdc_events: 10, total_applied_cdc_events: 10 },
      tables: [batch("orders", 1_000), cdc("events", 0, 10)],
    })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="running" />)
    await screen.findByRole("region", { name: "Tables" })

    const grid = screen.getByRole("region", { name: "Tables" }).parentElement!
    expect(within(grid).getAllByRole("region")).toHaveLength(6)
    expect(grid.className).toContain("@3xl:grid-cols-3")
  })

  it("a paused pipeline's tables read paused, never running", async () => {
    serve({ summary: { ...BATCH_SUMMARY, tables_running: 2 }, tables: [batch("orders", 10)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="paused" />)
    await screen.findByRole("region", { name: "Tables" })

    expect(cardFact("Tables", "paused")).toBe("2")
    expect(cardFact("Tables", "running")).toBeNull()
    expect(chartIn("Tables by status").settings.x.categories).toContain("Paused")
    expect(chartIn("Tables by status").settings.x.categories).not.toContain("Running")
  })

  it("Dropped turns red with the tables it came from; a clean run keeps it plain", async () => {
    serve({ summary: { ...BATCH_SUMMARY, total_dlq_rows: 7, tables_with_dlq: 2 }, tables: [batch("orders", 10)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="completed" />)
    await screen.findByRole("region", { name: "Dropped" })

    expect(cardValue("Dropped")).toBe("7")
    expect(cardFact("Dropped", "tables")).toBe("2")
    expect(screen.getByRole("region", { name: "Dropped" }).querySelector("p")?.className).toMatch(/text-red/)

    cleanup()
    serve({ summary: BATCH_SUMMARY, tables: [batch("orders", 10)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="completed" />)
    await screen.findByRole("region", { name: "Dropped" })
    expect(cardFact("Dropped", "tables")).toBeNull()
    expect(screen.getByRole("region", { name: "Dropped" }).querySelector("p")?.className).not.toMatch(/text-red/)
  })
})

describe("the charts", () => {
  it("Tables by status: one bar per state with tables, each in its state's colour", async () => {
    serve({ summary: { ...BATCH_SUMMARY, tables_degraded: 1, tables_rewound: 1, tables_removed: 1 }, tables: [batch("orders", 10)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="failed" />)
    await findFigure("Tables by status")

    const chart = chartIn("Tables by status")
    expect(chart.rows).toEqual([
      { status: "Completed", tables: 2 },
      { status: "Rewound", tables: 1 },
      { status: "Failed", tables: 1 },
      { status: "Removed", tables: 1 },
    ])
    expect(chart.settings.series.values.map((v) => v.color)).toEqual(["succeeded", "waiting", "failed", "skipped"])
    expect(screen.getByText(/Removed tables are no longer selected/)).toBeInTheDocument()
  })

  it("Top 10 from the page when the page holds every table, with no extra read", async () => {
    serve({ summary: BATCH_SUMMARY, tables: [batch("users", 200), batch("orders", 1_000), batch("empty", 0)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="completed" />)
    await findFigure("Top 10 tables by rows written")

    expect(chartIn("Top 10 tables by rows written").rows).toEqual([
      { table: "orders", written: 1_000 },
      { table: "users", written: 200 },
    ])
    expect(screen.getByText("The tables that wrote the most rows in this run.")).toBeInTheDocument()
    expect(statsCalls()).toHaveLength(1)
  })

  it("a batch run past one page reads its top ten once (sorted by rows written) and charts those", async () => {
    const page = Array.from({ length: 25 }, (_, i) => batch(`t${String(i).padStart(2, "0")}`, i + 1))
    serve(
      { summary: { ...BATCH_SUMMARY, total_tables: 30 }, tables: page, total: 30 },
      [batch("big", 9_000), batch("bigger", 12_000)],
    )
    render(<TableStatisticsPanel pipelineId="p1" executionId="run-1" pipelineStatus="completed" />)

    await waitFor(() => expect(chartIn("Top 10 tables by rows written").rows[0]).toEqual({ table: "bigger", written: 12_000 }))
    const top = statsCalls().filter((u) => u.searchParams.get("limit") === "10")
    expect(top).toHaveLength(1)
    expect(top[0].searchParams.get("sort")).toBe("inserted_rows")
    expect(top[0].searchParams.get("offset")).toBe("0")
    expect(top[0].searchParams.get("execution_id")).toBe("run-1")
    expect(screen.queryByText(/Of the tables on this page/)).toBeNull()
  })

  it("control: a CDC run past one page charts the page and says so, with no extra read", async () => {
    const page = Array.from({ length: 25 }, (_, i) => cdc(`t${String(i).padStart(2, "0")}`, 100, i))
    serve({ summary: { mode: "cdc", total_tables: 30, tables_completed: 0, tables_running: 30, tables_failed: 0 }, tables: page, total: 30 })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="running" />)
    await findFigure("Top 10 tables by rows applied")

    // A CDC table counts its snapshot rows and what the sink applied.
    expect(chartIn("Top 10 tables by rows applied").rows[0]).toEqual({ table: "t24", written: 124 })
    expect(screen.getByText(/Of the tables on this page\./)).toBeInTheDocument()
    expect(statsCalls().filter((u) => u.searchParams.get("limit") === "10")).toHaveLength(0)
  })

  it("nothing written yet says so instead of an empty chart", async () => {
    serve({ summary: { ...BATCH_SUMMARY, total_inserted_rows: 0 }, tables: [batch("orders", 0)] })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="running" />)
    const fig = await findFigure("Top 10 tables by rows written")
    expect(within(fig).getByText("Nothing written yet")).toBeInTheDocument()
    expect(within(fig).queryByTestId("chart")).toBeNull()
  })
})

describe("loading and frame", () => {
  it("loads as card and chart skeletons, named for a screen reader", async () => {
    authFetch.mockImplementation(() => new Promise(() => {}))
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="completed" />)
    expect(screen.getAllByRole("status").map((s) => s.textContent)).toEqual(["Loading table statistics…", "Loading table charts…"])
  })

  it("the Table statistics panel draws no frame round its cards; the Activity panel keeps its frame", async () => {
    authFetch.mockImplementation(async () => ok({}))
    render(<PipelineMonitoringPanel pipelineId="p1" variant="table_stats" />)
    // The Card's border, white fill and shadow are each overridden (cn merges them away).
    const frameless = screen.getByTestId("pipeline-monitoring-panel").className.split(" ")
    expect(frameless).toEqual(expect.arrayContaining(["border-0", "bg-transparent", "shadow-none"]))
    expect(frameless).not.toEqual(expect.arrayContaining(["shadow-sm"]))
    expect(frameless).not.toContain("bg-white")

    cleanup()
    render(<PipelineMonitoringPanel pipelineId="p1" variant="activity" />)
    const framed = screen.getByTestId("pipeline-monitoring-panel").className.split(" ")
    expect(framed).toEqual(expect.arrayContaining(["border", "bg-white", "shadow-sm"]))
    expect(framed).not.toContain("border-0")
  })
})

describe("table-stats-charts", () => {
  it("names a table by its qualified name only when two share a table name", () => {
    const rows = topTablesRows([
      { qualified_name: "a.users", table_name: "users", mode: "batch", inserted_rows: 5 },
      { qualified_name: "b.users", table_name: "users", mode: "batch", inserted_rows: 9 },
      { qualified_name: "a.orders", mode: "batch", inserted_rows: 3 },
      { qualified_name: "a.none", mode: "batch", inserted_rows: 0 },
    ])
    expect(rows).toEqual([
      { table: "b.users", written: 9 },
      { table: "a.users", written: 5 },
      { table: "orders", written: 3 },
    ])
  })

  it("keeps ten, most first", () => {
    const rows = topTablesRows(Array.from({ length: 14 }, (_, i) => ({ qualified_name: `s.t${i}`, mode: "batch" as const, inserted_rows: i + 1 })))
    expect(rows).toHaveLength(10)
    expect(rows[0]).toEqual({ table: "t13", written: 14 })
    expect(rows[9]).toEqual({ table: "t4", written: 5 })
  })

  it("leaves states with no tables off the status chart, and says the counts in its description", () => {
    const counts = { completed: 3, running: 0, runningLabel: "Stopped" as const, notWritten: 0, rewound: 0, failed: 0, waitingForData: 2, removed: 0 }
    expect(tableStatusRows(counts)).toEqual([
      { status: "Completed", tables: 3 },
      { status: "No data yet", tables: 2 },
    ])
    expect(tableStatusChart(counts).description).toBe("Tables by status: 3 completed, 2 no data yet.")
  })
})
