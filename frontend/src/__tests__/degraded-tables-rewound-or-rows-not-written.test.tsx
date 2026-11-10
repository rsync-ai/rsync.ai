/**
 * A table whose rows fell short of the destination says what happened to it.
 *
 * Every such table read "Degraded": a batch table whose run lost rows and was
 * rewound (the next run reads them again) looked the same as one whose rows
 * were simply not written. The gateway now says which tables a run rewound
 * (`rewound`, `tables_rewound` — table_stats.go, migration 141), so the page
 * says "Rewound" only for those and "Rows not written" for the rest, with the
 * written, read and dead-letter counts on hover.
 */
import { beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import "@testing-library/jest-dom"
import { cardFact } from "./statCards"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { TableStatisticsPanel } from "@/components/pipeline/TableStatisticsPanel"
import { degradedTableLabel, tableStatusBreakdown } from "@/components/pipeline/executionSummary"
import { tablesNeedingAttention } from "@/components/pipeline/MonitoringOverviewTab"

function ok(body: unknown) {
  return { ok: true, status: 200, json: async () => body } as unknown as Response
}

const batchRow = (name: string, extra: Record<string, unknown> = {}) => ({
  qualified_name: `app.${name}`,
  table_name: name,
  schema_name: "app",
  mode: "batch",
  status: "degraded",
  updated_at: "2026-10-05T10:00:00Z",
  ...extra,
})

// t-clickup on prod: list_task lost a batch and was rewound; list_comment's
// rows were dead-lettered and were not.
const REWOUND = batchRow("list_task", { read_rows: 1250, inserted_rows: 1000, dlq_rows: 0, rewound: true })
const NOT_WRITTEN = batchRow("list_comment", { read_rows: 50, inserted_rows: 0, dlq_rows: 5 })

function serve(tables: unknown[], summary: Record<string, unknown>) {
  authFetch.mockResolvedValue(
    ok({
      summary: { mode: "batch", total_tables: tables.length, tables_running: 0, tables_completed: 0, tables_failed: 0, ...summary },
      tables,
      total: tables.length,
    })
  )
}

function row(name: string): HTMLElement {
  const tr = Array.from(document.querySelectorAll<HTMLElement>("table tbody tr")).find((r) =>
    (r.textContent || "").includes(name)
  )
  if (!tr) throw new Error(`no row for ${name}`)
  return tr
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe("degradedTableLabel", () => {
  it("says Rewound only for a table the run rewound", () => {
    const d = degradedTableLabel(REWOUND)
    expect(d.label).toBe("Rewound")
    expect(d.title).toContain("1,000 of 1,250")
    expect(d.title).toMatch(/next run reads/i)
  })

  it("says Rows not written otherwise, with written vs read and the dead-letter count", () => {
    const d = degradedTableLabel(NOT_WRITTEN)
    expect(d.label).toBe("Rows not written")
    expect(d.title).toBe("Wrote 0 of 50 rows read · 5 rows in the dead-letter queue")
  })

  it("gives a CDC table its dead-letter count, which is what makes it short", () => {
    const d = degradedTableLabel({ mode: "cdc", dlq_rows: 1 })
    expect(d.label).toBe("Rows not written")
    expect(d.title).toBe("1 row in the dead-letter queue")
  })
})

describe("the table statistics page", () => {
  it("reads Rewound for the rewound table and Rows not written for the other — never Degraded", async () => {
    serve([REWOUND, NOT_WRITTEN], { tables_degraded: 2, tables_rewound: 1, total_dlq_rows: 5, tables_with_dlq: 1 })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="failed" />)
    await waitFor(() => expect(screen.getByText("list_task")).toBeInTheDocument())

    const rewound = within(row("list_task")).getByText("Rewound")
    expect(rewound.closest("[title]")?.getAttribute("title")).toContain("1,000 of 1,250")
    const notWritten = within(row("list_comment")).getByText("Rows not written")
    expect(notWritten.closest("[title]")?.getAttribute("title")).toBe(
      "Wrote 0 of 50 rows read · 5 rows in the dead-letter queue"
    )

    expect(cardFact("Tables", "rewound")).toBe("1")
    expect(cardFact("Tables", "rows not written")).toBe("1")
    expect(cardFact("Tables", "degraded")).toBeNull()
    expect(screen.getByText("1 table rewound")).toBeInTheDocument()
    expect(screen.getByText("1 table with rows not written")).toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(/degraded/i)
  })

  it("control: a gateway with no rewound flag shows Rows not written and no Rewound count", async () => {
    serve([batchRow("list_task", { read_rows: 1250, inserted_rows: 1000, dlq_rows: 0 })], { tables_degraded: 1 })
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="failed" />)
    await waitFor(() => expect(screen.getByText("list_task")).toBeInTheDocument())

    expect(within(row("list_task")).getByText("Rows not written")).toBeInTheDocument()
    expect(within(row("list_task")).queryByText("Rewound")).toBeNull()
    expect(cardFact("Tables", "rows not written")).toBe("1")
    expect(cardFact("Tables", "rewound")).toBeNull()
    expect(screen.queryByText(/table rewound/)).toBeNull()
  })
})

describe("the Monitor tab's table counts", () => {
  it("split degraded tables into rewound and rows not written, and still add up", () => {
    const b = tableStatusBreakdown({ total_tables: 5, tables_completed: 1, tables_degraded: 3, tables_rewound: 2, tables_running: 1 })
    expect(b.rows).toEqual([
      { key: "rewound", label: "Tables rewound", count: 2 },
      { key: "degraded", label: "Tables with rows not written", count: 1 },
      { key: "running", label: "Tables running", count: 1 },
    ])
    expect(b.completed + b.rows.reduce((sum, r) => sum + r.count, 0)).toBe(b.total)
  })

  it("never counts more tables rewound than degraded", () => {
    const b = tableStatusBreakdown({ total_tables: 1, tables_degraded: 1, tables_rewound: 4 })
    expect(b.rows).toEqual([{ key: "rewound", label: "Tables rewound", count: 1 }])
  })
})

describe("the Overview's tables needing attention", () => {
  it("say Rewound or Rows not written with the written count", () => {
    const items = tablesNeedingAttention(
      [
        { qualified_name: "app.list_task", mode: "batch", status: "degraded", read_rows: 1250, inserted_rows: 1000, rewound: true },
        { qualified_name: "app.list_folder", mode: "batch", status: "degraded", read_rows: 50, inserted_rows: 0 },
      ],
      false
    )
    expect(items.map((i) => [i.name, i.reason])).toEqual([
      ["app.list_folder", "Rows not written · destination wrote 0 rows"],
      ["app.list_task", "Rewound · 1,000 of 1,250 rows written"],
    ])
  })
})
