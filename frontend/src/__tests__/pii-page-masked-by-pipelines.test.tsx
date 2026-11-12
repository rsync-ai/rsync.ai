import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, within } from "@testing-library/react"
import type { Mock } from "vitest"

import PIIDashboardPage from "@/app/(dashboard)/pii/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { maskedColumnsNotScanned, type PIIMaskedColumn } from "@/lib/pii/masked-columns"

// Issue 8: a workspace whose pipelines hash `email` showed "0 PII Columns
// Detected", because the page only counted pii_scan_results and nothing writes
// a pipeline's own masks there. The page now reads them from
// GET /api/v1/pii/masked-columns, lists them, and counts them in the headline.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("@/components/charts/EChartNoSSR", () => ({ EChartNoSSR: () => <div data-testid="chart" /> }))

// The number on a KPI card: the first paragraph of the card the title names.
const statValue = (title: string) => screen.getByRole("region", { name: title }).querySelector("p")

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

function installMemoryStorage() {
  const store: Record<string, string> = {}
  const ls: Storage = {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => {
      store[k] = String(v)
    },
    removeItem: (k) => {
      delete store[k]
    },
    clear: () => {
      for (const k of Object.keys(store)) delete store[k]
    },
    key: (i) => Object.keys(store)[i] ?? null,
    get length() {
      return Object.keys(store).length
    },
  }
  Object.defineProperty(window, "localStorage", { value: ls, configurable: true, writable: true })
}

const scanRow = (table: string, column: string) => ({
  id: `${table}.${column}`,
  pipeline_id: "",
  table_name: table,
  column_name: column,
  pii_type: "email",
  confidence: 0.6,
  detection_method: "column_name",
  suggested_masking: "hash",
  created_at: "2026-09-26T00:00:00Z",
})

const masked = (over: Partial<PIIMaskedColumn>): PIIMaskedColumn => ({
  pipeline_id: "p-1",
  pipeline_name: "orders to lake",
  column: "email",
  action: "hash",
  ...over,
})

function installPageFetch(opts: {
  scan?: ReturnType<typeof scanRow>[]
  maskedColumns?: PIIMaskedColumn[]
  truncated?: boolean
  maskedStatus?: number
}) {
  ;(authFetch as Mock).mockImplementation(async (url: string) => {
    const u = String(url)
    if (u.includes("/pii/masked-columns")) {
      const status = opts.maskedStatus ?? 200
      return {
        ok: status < 400,
        status,
        json: async () => ({ columns: opts.maskedColumns ?? [], truncated: opts.truncated ?? false }),
      }
    }
    if (u.includes("/pii/scan/results")) {
      return { ok: true, status: 200, json: async () => ({ results: opts.scan ?? [] }) }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
}

beforeEach(() => {
  vi.clearAllMocks()
  installMemoryStorage()
})

describe("PII page: masked by pipelines", () => {
  it("counts a pipeline-masked column in the headline when no scan found it", async () => {
    installPageFetch({ maskedColumns: [masked({})] })

    render(<PIIDashboardPage />)

    await screen.findByText("Masked by pipelines")
    expect(statValue("Columns found")).toHaveTextContent("1")
    expect(screen.getByText("0 from scans · 1 masked by pipelines")).toBeInTheDocument()

    const table = screen.getByRole("table", { name: "Columns masked by pipelines" })
    const link = within(table).getByRole("link", { name: "orders to lake" })
    expect(link).toHaveAttribute("href", "/pipelines/p-1")
    expect(within(table).getByText("Every table")).toBeInTheDocument()
    expect(within(table).getByText("hash")).toBeInTheDocument()
  })

  it("does not count a column twice when a scan already found it", async () => {
    installPageFetch({
      scan: [scanRow("public.users", "email")],
      maskedColumns: [masked({ table: "users" })],
    })

    render(<PIIDashboardPage />)

    await screen.findByRole("table", { name: "Columns masked by pipelines" })
    expect(statValue("Columns found")).toHaveTextContent("1")
    expect(screen.getByText("1 from scans · 0 masked by pipelines")).toBeInTheDocument()
  })

  it("says so when no pipeline masks anything", async () => {
    installPageFetch({})

    render(<PIIDashboardPage />)

    expect(await screen.findByText("No pipeline in this workspace masks or hashes a column.")).toBeInTheDocument()
    expect(statValue("Columns found")).toHaveTextContent("0")
  })

  it("reports a failed read instead of showing nothing masked as fact", async () => {
    installPageFetch({ maskedStatus: 500 })

    render(<PIIDashboardPage />)

    expect(await screen.findByText(/Masked columns: HTTP 500/)).toBeInTheDocument()
  })

  it("says the list is incomplete when the server truncated it", async () => {
    installPageFetch({ maskedColumns: [masked({})], truncated: true })

    render(<PIIDashboardPage />)

    expect(await screen.findByText(/this list is incomplete/)).toBeInTheDocument()
  })
})

describe("maskedColumnsNotScanned", () => {
  const scan = [scanRow("public.users", "email"), scanRow("public.orders", "phone")]

  it.each<[string, PIIMaskedColumn[], number]>([
    ["nothing masked", [], 0],
    ["a scanned column, table matched by its last segment", [masked({ table: "users" })], 0],
    ["a scanned column, full table name and other case", [masked({ table: "PUBLIC.USERS", column: "Email" })], 0],
    ["an unscoped rule on a column some table's scan found", [masked({ column: "phone" })], 0],
    ["the same column name in a table the scan did not report", [masked({ table: "audit.users_archive" })], 1],
    ["a column no scan found", [masked({ column: "ssn" })], 1],
    [
      "two pipelines masking the same column count once",
      [masked({ column: "ssn" }), masked({ pipeline_id: "p-2", column: "SSN", action: "redact" })],
      1,
    ],
    ["a scoped and an unscoped rule are different columns", [masked({ column: "ssn" }), masked({ table: "t", column: "ssn" })], 2],
    ["a blank column is ignored", [masked({ column: "  " })], 0],
  ])("%s", (_name, rules, want) => {
    expect(maskedColumnsNotScanned(scan, rules)).toBe(want)
  })
})

// Launch review, PR1: the type card was blank when no scan had run, and an "Audit
// Log" tab said only "Audit log will be displayed here". Nothing feeds that tab yet,
// so it is gone until something does, and the card says why it is empty.
describe("PII page: nothing that reads as unfinished", () => {
  it("says the type card is empty because no scan has results", async () => {
    installPageFetch({})
    render(<PIIDashboardPage />)
    // "Masked by pipelines" draws before the reads land; the type card only after.
    expect(await screen.findByText("No scan results yet")).toBeInTheDocument()
    expect(screen.getByText("Columns a PII scan flags are counted here by type.")).toBeInTheDocument()
  })

  it("draws the types, not the empty message, once a scan has results", async () => {
    installPageFetch({ scan: [scanRow("public.users", "email")] })
    render(<PIIDashboardPage />)
    await screen.findByText(/1 column found by scans/)
    expect(screen.queryByText("No scan results yet")).not.toBeInTheDocument()
  })

  it("has no Audit Log tab", async () => {
    installPageFetch({})
    render(<PIIDashboardPage />)
    await screen.findByText("Masked by pipelines")
    const tabs = screen.getAllByRole("tab").map((t) => t.textContent)
    expect(tabs).toEqual(["Overview", "Approvals", "Policies", "Hash functions"])
    expect(screen.queryByText("Audit log will be displayed here")).not.toBeInTheDocument()
    expect(screen.getByRole("tablist")).toHaveClass("grid-cols-4")
  })
})
