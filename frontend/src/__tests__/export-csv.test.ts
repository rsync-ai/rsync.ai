import { describe, expect, it } from "vitest"

import { tableExport } from "@/components/charts/saved-chart"
import type { ChartResult, ServerChartSettings } from "@/lib/api/charts"
import { toCsv } from "@/lib/export/csv"

// A table tile as CSV (dashboards-design.md § 8, D3): the rows the tile shows, the
// measure whole, in a file Excel opens as UTF-8 and cannot be tricked into running.

describe("toCsv", () => {
  it("starts with a byte-order mark and ends every row with CRLF", () => {
    const csv = toCsv(["a", "b"], [["1", "2"]])
    expect(csv.charCodeAt(0)).toBe(0xfeff)
    expect(csv.slice(1)).toBe("a,b\r\n1,2\r\n")
  })

  it("quotes a cell with a comma, a quote or a line break, and doubles its quotes", () => {
    const csv = toCsv(["name"], [["Smith, J"], ['say "hi"'], ["two\nlines"], ["plain"]])
    expect(csv.slice(1).split("\r\n")).toEqual(["name", '"Smith, J"', '"say ""hi"""', '"two\nlines"', "plain", ""])
  })

  it("defuses a text cell a spreadsheet would run as a formula", () => {
    const csv = toCsv(["v"], [["=HYPERLINK(\"x\")"], ["+1+2"], ["@SUM(A1)"], ["-cmd"], ["\tTAB"]])
    expect(csv.slice(1).split("\r\n").slice(1, -1)).toEqual(["\"'=HYPERLINK(\"\"x\"\")\"", "'+1+2", "'@SUM(A1)", "'-cmd", "'\tTAB"])
  })

  it("leaves a plain number alone, in a text column and in a number column", () => {
    const csv = toCsv(["label", "amount"], [["-12.5", "-3"], ["+4", "1e-3"]], [false, true])
    expect(csv.slice(1).split("\r\n").slice(1, -1)).toEqual(["-12.5,-3", "+4,1e-3"])
  })
})

describe("tableExport", () => {
  const result = (rows: unknown[][], masked: string[] = []): ChartResult => ({
    columns: [
      { alias: "region", role: "table column region" },
      { alias: "day", role: "table column day" },
      { alias: "amount", role: "the measure" },
    ],
    rows,
    truncated: false,
    masked,
    unfiltered: [],
    sql: "",
    read_at: "",
    execution_time_ms: 0,
  })
  const table: ServerChartSettings = {
    version: 2,
    type: "table",
    roles: { columns: [{ column: "region" }, { column: "day" }], measure: [{ aggregate: "sum", column: "amount", format: "compact" }] },
  }

  it("takes the table's columns and rows with the measure whole, not as the screen rounds it", () => {
    expect(tableExport(table, result([["EU", "2026-10-01T00:00:00Z", "12500.25"], [null, "2026-10-02", null]]))).toEqual({
      head: ["region", "day", "Sum of amount"],
      body: [
        ["EU", "2026-10-01", "12500.25"],
        ["", "2026-10-02", ""],
      ],
      numeric: [false, false, true],
    })
  })

  it("leaves a masked measure as the server sent it", () => {
    expect(tableExport(table, result([["EU", "2026-10-01", "****"]], ["amount"])).body[0][2]).toBe("****")
  })
})
