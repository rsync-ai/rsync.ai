import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { Mock } from "vitest"

import PIIDashboardPage from "@/app/(dashboard)/pii/page"
import type { ChartSettings } from "@/components/charts/chart-settings"
import { authFetch } from "@/lib/api/auth-fetch"
import type { PIIMaskedColumn } from "@/lib/pii/masked-columns"

// Launch review, PR5: the PII page on the kit. Its five reads run side by side
// behind a skeleton, the KPIs are StatCards, two charts count what the page
// already reads, the type filter offers every type the scans found (it listed
// six fixed types, so bank_account and password could not be picked), both
// tables say when they are empty, and Approve and Deny each confirm their own
// decision (both opened one dialog that asked for either).

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
// A chart as its bars, one button per category, so a test can click one.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings, onCategoryClick }: { settings: ChartSettings; onCategoryClick?: (i: number) => void }) => (
    <div role="img" aria-label={settings.description}>
      {("x" in settings ? settings.x.categories : []).map((c, i) => (
        <button key={c} type="button" onClick={() => onCategoryClick?.(i)}>
          {`bar ${c}`}
        </button>
      ))}
    </div>
  ),
}))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

const scanRow = (table: string, column: string, piiType: string, over: Record<string, unknown> = {}) => ({
  id: `${table}.${column}`,
  pipeline_id: "",
  table_name: table,
  column_name: column,
  pii_type: piiType,
  confidence: 0.9,
  detection_method: "column_name",
  suggested_masking: "hash",
  created_at: "2026-09-26T00:00:00Z",
  ...over,
})

const masked = (over: Partial<PIIMaskedColumn>): PIIMaskedColumn => ({
  pipeline_id: "p-1",
  pipeline_name: "orders to lake",
  column: "email",
  action: "hash",
  ...over,
})

const REQUEST = {
  id: "req-1",
  pipeline_id: "p-9",
  requested_by: "analyst",
  justification: "Fraud review",
  status: "pending",
  columns: [{ table_name: "customers", column_name: "ssn", pii_type: "ssn", requested_action: "plain" }],
  created_at: "2026-10-01T00:00:00Z",
}

interface Served {
  scan?: unknown[]
  maskedColumns?: PIIMaskedColumn[]
  requests?: unknown[]
  policies?: unknown[]
  failing?: string
}

function serve(opts: Served) {
  ;(authFetch as Mock).mockImplementation(async (url: string, init?: RequestInit) => {
    const u = String(url)
    if (opts.failing && u.includes(opts.failing)) return { ok: false, status: 500, json: async () => ({}) }
    const ok = (body: unknown) => ({ ok: true, status: 200, json: async () => body })
    if (init?.method === "POST") return ok({})
    if (u.includes("/pii/masked-columns")) return ok({ columns: opts.maskedColumns ?? [], truncated: false })
    if (u.includes("/pii/scan/results")) return ok({ results: opts.scan ?? [] })
    if (u.includes("/pii/approvals")) return ok({ requests: opts.requests ?? [] })
    if (u.includes("/pii/policies")) return ok({ policies: opts.policies ?? [] })
    return ok({ functions: [] })
  })
}

const card = (title: string) => screen.getByRole("region", { name: title })
const statValue = (title: string) => card(title).querySelector("p")

beforeEach(() => {
  vi.clearAllMocks()
})

describe("PII page: load", () => {
  it("asks for all five reads at once and shows grey cards, not zeros, until they are back", async () => {
    const answers: (() => void)[] = []
    ;(authFetch as Mock).mockImplementation(
      () =>
        new Promise((resolve) =>
          answers.push(() => resolve({ ok: true, status: 200, json: async () => ({}) })),
        ),
    )
    render(<PIIDashboardPage />)

    await waitFor(() => expect(authFetch).toHaveBeenCalledTimes(5))
    expect(screen.getByText("Loading PII findings…")).toBeInTheDocument()
    expect(screen.queryByRole("region", { name: "Columns found" })).toBeNull()

    answers.forEach((answer) => answer())
    expect(await screen.findByRole("region", { name: "Columns found" })).toBeInTheDocument()
    expect(screen.queryByText("Loading PII findings…")).toBeNull()
  })

  it("shows a KPI it could not read as a dash, beside the error, never as 0", async () => {
    serve({ failing: "/pii/scan/results" })
    render(<PIIDashboardPage />)
    expect(await screen.findByText(/Scan results: HTTP 500/)).toBeInTheDocument()
    expect(statValue("Columns found")).toHaveTextContent("—")
    expect(statValue("Masked")).toHaveTextContent("0")
  })

  it("says a section did not load where its hint and empty states would claim there is nothing", async () => {
    serve({ failing: "/pii/scan/results" })
    const { unmount } = render(<PIIDashboardPage />)
    expect(await screen.findByText("Scan results did not load")).toBeInTheDocument()
    expect(within(card("Columns found")).getByText("Did not load")).toBeInTheDocument()
    expect(screen.getByText("Scan results did not load. Use Refresh to try again.")).toBeInTheDocument()
    expect(screen.queryByText("No scan results yet")).toBeNull()
    expect(screen.queryByText("No scan has flagged a column yet.")).toBeNull()
    unmount()

    serve({ failing: "/pii/approvals" })
    render(<PIIDashboardPage />)
    await screen.findByText(/Approval requests: HTTP 500/)
    expect(statValue("Pending approvals")).toHaveTextContent("—")
    expect(within(card("Pending approvals")).getByText("Did not load")).toBeInTheDocument()
    expect(screen.queryByText("Nothing waiting")).toBeNull()
  })
})

describe("PII page: KPIs and charts", () => {
  it("counts columns, masks, approvals and policies on four cards", async () => {
    serve({
      scan: [scanRow("customers", "email", "email"), scanRow("customers", "phone", "phone")],
      maskedColumns: [masked({}), masked({ column: "ssn", action: "redact" }), masked({ pipeline_id: "p-2", column: "iban" })],
      requests: [REQUEST],
      policies: [
        { id: "a", policy_type: "always_mask", pii_type: "ssn", action: "hash", priority: 1, enabled: true },
        { id: "b", policy_type: "audit_required", pii_type: "*", action: "log", priority: 2, enabled: false },
      ],
    })
    render(<PIIDashboardPage />)

    await screen.findByRole("region", { name: "Columns found" })
    // email is scanned and masked, so it counts once: 2 scanned + ssn + iban.
    expect(statValue("Columns found")).toHaveTextContent("4")
    expect(within(card("Columns found")).getByText("2 from scans · 2 masked by pipelines")).toBeInTheDocument()
    expect(statValue("Masked")).toHaveTextContent("3")
    expect(within(card("Masked")).getByText("in 2 pipelines")).toBeInTheDocument()
    expect(statValue("Pending approvals")).toHaveTextContent("1")
    expect(statValue("Pending approvals")).toHaveClass("text-amber-700")
    expect(statValue("Policies")).toHaveTextContent("1")
    expect(within(card("Policies")).getByText("of 2 on · stored, not enforced")).toBeInTheDocument()
  })

  it("draws scanned columns by type and masked columns by mask type, most first", async () => {
    serve({
      scan: [scanRow("a", "e1", "email"), scanRow("b", "e2", "email"), scanRow("c", "acct", "bank_account")],
      maskedColumns: [masked({}), masked({ column: "x", action: "redact" }), masked({ column: "y", action: "redact" })],
    })
    render(<PIIDashboardPage />)

    const types = await screen.findByRole("img", { name: /by PII type/ })
    expect(within(types).getAllByRole("button").map((b) => b.textContent)).toEqual(["bar Email", "bar Bank account"])
    const masks = screen.getByRole("img", { name: /by how they are masked/ })
    expect(within(masks).getAllByRole("button").map((b) => b.textContent)).toEqual(["bar Redacted", "bar Hashed"])
    expect(screen.getByText("3 columns found by scans. Click a bar to list its columns.")).toBeInTheDocument()
    expect(screen.getByText("3 columns pipelines mask, by mask type")).toBeInTheDocument()
  })

  it("says so in the mask chart when no pipeline masks a column", async () => {
    serve({ scan: [scanRow("a", "e1", "email")] })
    render(<PIIDashboardPage />)
    expect(await screen.findByText("No pipeline masks a column yet")).toBeInTheDocument()
  })
})

describe("PII page: detected columns", () => {
  it("offers every type the scans found, bank account and password included, and filters by it", async () => {
    serve({
      scan: [
        scanRow("customers", "email", "email"),
        scanRow("accounts", "iban", "bank_account"),
        scanRow("users", "pw", "password"),
      ],
    })
    render(<PIIDashboardPage />)

    const chips = await screen.findByRole("group", { name: "PII type" })
    expect(within(chips).getAllByRole("button").map((b) => b.textContent)).toEqual([
      "All types3",
      "Bank account1",
      "Email1",
      "Password1",
    ])
    fireEvent.click(within(chips).getByRole("button", { name: /Password/ }))
    const table = screen.getByRole("table", { name: "Detected PII columns" })
    expect(within(table).getByText("users.pw")).toBeInTheDocument()
    expect(within(table).queryByText("customers.email")).toBeNull()

    // A bar in the type chart picks the same filter.
    fireEvent.click(screen.getByRole("button", { name: "bar Bank account" }))
    expect(within(table).getByText("accounts.iban")).toBeInTheDocument()
    expect(within(table).queryByText("users.pw")).toBeNull()
    expect(within(chips).getByRole("button", { name: /Bank account/ })).toHaveAttribute("aria-pressed", "true")
  })

  it("lists every column once a picked type is gone after a refresh, as the chips show", async () => {
    serve({ scan: [scanRow("customers", "email", "email"), scanRow("users", "pw", "password")] })
    render(<PIIDashboardPage />)
    const chips = await screen.findByRole("group", { name: "PII type" })
    fireEvent.click(within(chips).getByRole("button", { name: /Password/ }))

    serve({ scan: [scanRow("customers", "email", "email"), scanRow("accounts", "iban", "bank_account")] })
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }))
    const table = screen.getByRole("table", { name: "Detected PII columns" })
    expect(await within(table).findByText("accounts.iban")).toBeInTheDocument()
    expect(within(table).getByText("customers.email")).toBeInTheDocument()
    expect(within(chips).getByRole("button", { name: /All types/ })).toHaveAttribute("aria-pressed", "true")
  })

  it("says why the table is empty: no scan yet, or nothing matches", async () => {
    serve({})
    const { unmount } = render(<PIIDashboardPage />)
    expect(await screen.findByText("No scan has flagged a column yet.")).toBeInTheDocument()
    unmount()

    serve({ scan: [scanRow("customers", "email", "email")] })
    render(<PIIDashboardPage />)
    fireEvent.change(await screen.findByLabelText("Search columns"), { target: { value: "nothing-like-it" } })
    expect(screen.getByText("No column matches this search and type.")).toBeInTheDocument()
  })

  it("marks a reviewed column Approved and the rest Not reviewed", async () => {
    serve({
      scan: [
        scanRow("customers", "email", "email", { approved_action: "hash" }),
        scanRow("customers", "phone", "phone", { approved_action: "redact" }),
        scanRow("customers", "name", "name"),
      ],
    })
    render(<PIIDashboardPage />)
    const table = await screen.findByRole("table", { name: "Detected PII columns" })
    expect(within(table).getByText("Approved")).toBeInTheDocument()
    expect(within(table).getByText("Approved: redact")).toBeInTheDocument()
    expect(within(table).getByText("Not reviewed")).toBeInTheDocument()
  })
})

describe("PII page: approvals", () => {
  async function openApprovals() {
    serve({ requests: [REQUEST] })
    render(<PIIDashboardPage />)
    await screen.findByRole("region", { name: "Columns found" })
    await userEvent.click(screen.getByRole("tab", { name: /Approvals/ }))
  }

  it("Deny asks to confirm a denial, and sends one", async () => {
    await openApprovals()
    await userEvent.click(screen.getByRole("button", { name: "Deny" }))
    const dialog = screen.getByRole("dialog")
    expect(within(dialog).getByText("Deny this request?")).toBeInTheDocument()
    expect(within(dialog).queryByRole("button", { name: "Approve request" })).toBeNull()

    await userEvent.click(within(dialog).getByRole("button", { name: "Deny request" }))
    const post = (authFetch as Mock).mock.calls.find((c) => c[1]?.method === "POST")
    expect(String(post?.[0])).toContain("/api/v1/pii/approvals/req-1/decide")
    expect(JSON.parse(post?.[1].body).decision).toBe("denied")
  })

  it("Approve asks to confirm an approval, and sends one", async () => {
    await openApprovals()
    await userEvent.click(screen.getByRole("button", { name: "Approve" }))
    const dialog = screen.getByRole("dialog")
    expect(within(dialog).getByText("Approve this request?")).toBeInTheDocument()
    expect(within(dialog).queryByRole("button", { name: "Deny request" })).toBeNull()

    await userEvent.click(within(dialog).getByRole("button", { name: "Approve request" }))
    const post = (authFetch as Mock).mock.calls.find((c) => c[1]?.method === "POST")
    expect(JSON.parse(post?.[1].body).decision).toBe("approved")
  })
})
