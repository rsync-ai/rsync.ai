/**
 * Pins the page half of KI-STOP-RACING-COMPLETION-OVERWRITTEN.
 *
 * Stop ends the run, but a table's stats row keeps whatever status it last
 * wrote — for a transfer cut off mid-table, "running". Under a stopped pipeline
 * that row drew a spinner and "Running", and the Tables card counted it as running:
 * the page said the transfer was still going after the user stopped it. It
 * reads "Stopped", the way a paused pipeline's tables read "Paused" (#14).
 *
 * Every "reads X" test has the control that does not.
 */

import { beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"
import { cardFact } from "./statCards"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { TableStatisticsPanel } from "@/components/pipeline/TableStatisticsPanel"

function ok(body: unknown) {
  return { ok: true, status: 200, json: async () => body } as unknown as Response
}

const batchRow = (name: string, extra: Record<string, unknown> = {}) => ({
  qualified_name: `app.${name}`,
  table_name: name,
  schema_name: "app",
  mode: "batch",
  status: "running",
  read_rows: 20000,
  inserted_rows: 12000,
  updated_at: "2026-09-30T10:00:00Z",
  ...extra,
})

function serve(tables: unknown[], running: number, completed = 0) {
  authFetch.mockResolvedValue(
    ok({
      summary: { mode: "batch", total_tables: tables.length, tables_running: running, tables_completed: completed, tables_failed: 0 },
      tables,
      total: tables.length,
    })
  )
}

const headings = () =>
  Array.from(document.querySelectorAll("table thead th")).map((h) => (h.textContent || "").trim())

function statusCell(rowIndex: number): string {
  const col = headings().indexOf("Status")
  if (col < 0) throw new Error(`no Status column; headings are ${JSON.stringify(headings())}`)
  const row = document.querySelectorAll("table tbody tr")[rowIndex]
  return (row.querySelectorAll("td")[col]?.textContent || "").trim()
}

const spinners = () => document.querySelectorAll("table tbody .animate-spin").length

beforeEach(() => {
  vi.clearAllMocks()
})

describe("a stopped pipeline's tables", () => {
  // The API reports a stopped pipeline as "stopped"; older rows and the run
  // history spell it "cancelled" or "canceled".
  for (const pipelineStatus of ["stopped", "cancelled", "canceled"]) {
    it(`read Stopped, not Running, when the pipeline is ${pipelineStatus}`, async () => {
      serve([batchRow("events")], 1)
      render(<TableStatisticsPanel pipelineId="p1" pipelineStatus={pipelineStatus} />)
      await waitFor(() => expect(screen.getByText("events")).toBeInTheDocument())

      expect(statusCell(0)).toBe("Stopped")
      expect(spinners()).toBe(0)
      expect(cardFact("Tables", "stopped")).toBe("1")
      expect(cardFact("Tables", "running")).toBeNull()
    })
  }

  it("a table that finished before the Stop still reads Completed", async () => {
    serve([batchRow("users", { status: "completed" }), batchRow("events")], 1, 1)
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="stopped" />)
    await waitFor(() => expect(screen.getByText("events")).toBeInTheDocument())

    expect(statusCell(0)).toBe("Completed")
    expect(statusCell(1)).toBe("Stopped")
  })

  it("control: a running pipeline's table reads Running", async () => {
    serve([batchRow("events")], 1)
    render(<TableStatisticsPanel pipelineId="p1" pipelineStatus="running" />)
    await waitFor(() => expect(screen.getByText("events")).toBeInTheDocument())

    expect(statusCell(0)).toBe("Running")
    expect(cardFact("Tables", "running")).toBe("1")
    expect(cardFact("Tables", "stopped")).toBeNull()
  })
})
