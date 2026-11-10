import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ModelsPage from "@/app/(dashboard)/models/page"
import { authFetch } from "@/lib/api/auth-fetch"

// The Models list shows every model, not only the scheduled ones: a model nobody has
// scheduled is a row saying "Manual". What must hold is that a row is Manual only when
// the model really has no schedule, that a plain saved query is not a model, and that
// the unscheduled half failing to load is said, never shown as "no models".

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/models",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
// Who is looking: an admin who made the queries, unless a test says otherwise.
const who = vi.hoisted(() => ({ role: "admin", userId: "u-1" }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: who.role, can: () => who.role === "admin" }),
  useWorkspace: () => ({ activeWorkspace: { id: "ws-1" } }),
}))
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: { id: who.userId } }),
}))
vi.mock("@/components/explorer/SavedQueryEditDialog", () => ({ SavedQueryEditDialog: () => null }))
vi.mock("@/components/explorer/SavedQueryModelDialog", () => ({
  SavedQueryModelDialog: (p: { savedQueryId: string; savedQueryName: string; connectionId?: string }) => (
    <div role="dialog" aria-label={`Model dialog for ${p.savedQueryName}`} data-connection={p.connectionId}>
      {p.savedQueryId}
    </div>
  ),
}))

const mockFetch = authFetch as unknown as Mock

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response
}

const SCHEDULED = {
  schedule_id: "sch-1",
  saved_query_id: "m-sched",
  name: "daily_revenue",
  connection_id: "c-1",
  schedule_type: "cron",
  schedule_spec: { cron: "0 2 * * *", timezone: "UTC" },
  status: "active",
  materialization: "table",
  target_table: "analytics.daily_revenue",
  statement_class: "select",
  supports_materialization: true,
  created_by: "u-1",
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
}

function saved(id: string, name: string, extra: Record<string, unknown> = {}) {
  return {
    id,
    name,
    connection_id: "c-2",
    materialization: "table",
    target_table: `analytics.${name}`,
    statement_class: "select",
    supports_materialization: true,
    created_by: "u-1",
    created_at: "2026-09-02T00:00:00Z",
    schedule_status: "",
    ...extra,
  }
}

let schedules: unknown[]
let savedReply: () => Promise<Response>

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  who.role = "admin"
  who.userId = "u-1"
  schedules = [SCHEDULED]
  savedReply = () =>
    Promise.resolve(
      res(200, {
        saved_queries: [
          // The scheduled one, as the saved list also returns it.
          saved("m-sched", "daily_revenue", { connection_id: "c-1", schedule_status: "active" }),
          saved("m-manual", "customer_dim", { layer: "Silver" }),
          saved("m-stmt", "merge_refunds", { materialization: "statement", target_table: undefined }),
          // A plain saved query builds nothing: not a model.
          saved("q-plain", "adhoc_lookup", { materialization: "none", target_table: undefined }),
          // Scheduled, but past the end of the schedules list (it stops at 500). The
          // saved list still says so, and that is what keeps it from reading Manual.
          saved("m-far", "far_model", { schedule_status: "paused" }),
        ],
        count: 5,
      })
    )
  mockFetch.mockImplementation((url: string) => {
    if (url === "/api/v1/explorer/schedules") return Promise.resolve(res(200, { schedules }))
    if (url === "/api/v1/explorer/saved") return savedReply()
    if (url === "/api/v1/explorer/freshness") return Promise.resolve(res(200, { breaches: [], count: 0 }))
    if (url.endsWith("/api/v1/model-layers")) {
      return Promise.resolve(
        res(200, {
          layers: [
            { id: "l-s", name: "Silver", description: "", position: 0, model_count: 1 },
            { id: "l-g", name: "Gold", description: "", position: 1, model_count: 0 },
          ],
          can_manage: true,
        })
      )
    }
    if (url === "/api/v1/explorer/running") {
      return Promise.resolve(res(200, { models: [], count: 0, limit: 50, temporal_available: true }))
    }
    return Promise.reject(new Error(`unexpected request ${url}`))
  })
})

/** A count card's number: the first paragraph in its region, on every StatCard. */
function countOf(title: string): string | null {
  return within(screen.getByRole("region", { name: title })).getAllByRole("paragraph")[0].textContent
}

function rowOf(name: string): HTMLElement {
  return screen.getByRole("link", { name }).closest("tr") as HTMLElement
}

describe("Models — models with no schedule", () => {
  it("lists them as Manual, beside the scheduled ones", async () => {
    render(<ModelsPage />)
    await screen.findByRole("link", { name: "customer_dim" })

    expect(screen.getByRole("heading", { name: "Models" })).toBeInTheDocument()
    expect(rowOf("daily_revenue")).not.toHaveTextContent("Manual")

    const manual = rowOf("customer_dim")
    expect(manual).toHaveTextContent("Manual")
    expect(manual).toHaveTextContent("analytics.customer_dim")
    expect(manual).toHaveTextContent("never run")
    expect(within(manual).getByRole("link", { name: "customer_dim" })).toHaveAttribute("href", "/models/m-manual")

    expect(rowOf("merge_refunds")).toHaveTextContent("runs its SQL as written")
    expect(screen.queryByText("adhoc_lookup")).not.toBeInTheDocument()
    expect(screen.queryByText("far_model")).not.toBeInTheDocument()
    // Each model once, the scheduled one included, though both lists return it.
    expect(screen.getAllByRole("link", { name: "daily_revenue" })).toHaveLength(1)
  })

  it("schedules one from its row, on its own connection", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    render(<ModelsPage />)
    const row = rowOf(await screen.findByRole("link", { name: "customer_dim" }).then((l) => l.textContent!))
    await user.click(within(row).getByRole("button", { name: "Actions for customer_dim" }))
    await user.click(await screen.findByRole("menuitem", { name: /^Schedule/ }))
    const dialog = screen.getByRole("dialog", { name: "Model dialog for customer_dim" })
    expect(dialog).toHaveTextContent("m-manual")
    expect(dialog).toHaveAttribute("data-connection", "c-2")
  })

  it("lists a workspace whose only models are unscheduled", async () => {
    schedules = []
    render(<ModelsPage />)
    expect(await screen.findByRole("link", { name: "customer_dim" })).toBeInTheDocument()
    expect(screen.queryByText("No models in this workspace yet.")).not.toBeInTheDocument()
  })

  it("says so when there is nothing at all", async () => {
    schedules = []
    savedReply = () => Promise.resolve(res(200, { saved_queries: [], count: 0 }))
    render(<ModelsPage />)
    expect(await screen.findByText("No models in this workspace yet.")).toBeInTheDocument()
  })

  // The scheduled rows are still true when the second list fails, so they stay — but
  // the page must not go on to claim it shows every model.
  it("keeps the scheduled rows and says the rest could not be listed", async () => {
    savedReply = () => Promise.resolve(res(500, { error: "failed to list saved queries" }))
    render(<ModelsPage />)
    expect(await screen.findByRole("link", { name: "daily_revenue" })).toBeInTheDocument()
    expect(screen.getByText("Models without a schedule could not be listed (HTTP 500).")).toBeInTheDocument()
    expect(within(screen.getByRole("table")).queryByText("Manual")).not.toBeInTheDocument()
    // Their number is unknown, so the count says so, and the total is a floor.
    expect(countOf("Manual")).toBe("—")
    expect(countOf("Total")).toBe("1+")
  })

  it("never reads an empty schedules list as no models when the rest failed", async () => {
    schedules = []
    savedReply = () => Promise.reject(new Error("network down"))
    render(<ModelsPage />)
    expect(await screen.findByText("Nothing is scheduled in this workspace yet.")).toBeInTheDocument()
    expect(screen.getByText("Models without a schedule could not be listed.")).toBeInTheDocument()
    expect(screen.queryByText("No models in this workspace yet.")).not.toBeInTheDocument()
  })

  it("filters Manual rows by layer like the rest", async () => {
    const user = userEvent.setup()
    render(<ModelsPage />)
    await screen.findByRole("link", { name: "customer_dim" })
    await user.click(screen.getByRole("combobox", { name: "Layer" }))
    await user.click(await screen.findByRole("option", { name: "Gold" }))
    expect(screen.queryByRole("link", { name: "customer_dim" })).not.toBeInTheDocument()
    expect(screen.getByText("No model is in this layer.")).toBeInTheDocument()

    await user.click(screen.getByRole("combobox", { name: "Layer" }))
    await user.click(await screen.findByRole("option", { name: "Silver" }))
    expect(screen.getByRole("link", { name: "customer_dim" })).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "merge_refunds" })).not.toBeInTheDocument()
  })

  // The table scrolls sideways when the page is narrow, and macOS hides the scrollbar
  // until you scroll, so the Edit buttons read as cut off (launch review, PR1). The
  // actions column stays pinned to the right edge instead, one ⋮ menu wide (PR3).
  it("keeps the actions column pinned while the table scrolls sideways", async () => {
    render(<ModelsPage />)
    await screen.findByRole("link", { name: "customer_dim" })
    expect(screen.getByRole("columnheader", { name: "Actions" })).toHaveClass("sticky", "right-0")
    for (const name of ["daily_revenue", "customer_dim", "merge_refunds"]) {
      const row = rowOf(name)
      expect(row).toHaveClass("group")
      const cells = within(row).getAllByRole("cell")
      expect(cells[cells.length - 1]).toHaveClass("sticky", "right-0")
      expect(within(cells[cells.length - 1]).getByRole("button", { name: `Actions for ${name}` })).toBeInTheDocument()
    }
  })

  // Status and Created were two more columns pushing the actions off the card (launch
  // review, PR3): a schedule's status now sits under its cadence, Created is on the
  // model's own page.
  it("says each row's status under its cadence, in the app's pills", async () => {
    render(<ModelsPage />)
    await screen.findByRole("link", { name: "customer_dim" })
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Model",
      "Cadence",
      "Now",
      "Next run",
      "Last run",
      "Actions",
    ])
    const cadence = (name: string) => within(rowOf(name)).getAllByRole("cell")[1]
    expect(cadence("daily_revenue")).toHaveTextContent("Active")
    expect(within(cadence("customer_dim")).getByText("Not scheduled")).toBeInTheDocument()
  })

  it("counts the models the table lists, following the layer filter", async () => {
    const user = userEvent.setup()
    schedules = [
      { ...SCHEDULED, last_run_status: "failed", last_run_at: "2026-09-16T02:00:00Z" },
      { ...SCHEDULED, schedule_id: "sch-2", saved_query_id: "m-p", name: "paused_dim", status: "paused" },
    ]
    render(<ModelsPage />)
    await screen.findByRole("link", { name: "customer_dim" })
    expect(countOf("Total")).toBe("4")
    expect(countOf("Active")).toBe("1")
    expect(countOf("Paused")).toBe("1")
    expect(countOf("Last run failed")).toBe("1")
    expect(countOf("Manual")).toBe("2")

    await user.click(screen.getByRole("combobox", { name: "Layer" }))
    await user.click(await screen.findByRole("option", { name: "Silver" }))
    expect(countOf("Total")).toBe("1")
    expect(countOf("Manual")).toBe("1")
    expect(countOf("Active")).toBe("0")
    expect(within(screen.getByRole("region", { name: "Total" })).getByText("In this layer")).toBeInTheDocument()
  })

  it("greys a menu item the role cannot use and says why", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    render(<ModelsPage />)
    await user.click(await screen.findByRole("button", { name: "Actions for daily_revenue" }))
    expect(await screen.findByRole("menuitem", { name: /Edit query/ })).not.toHaveAttribute("data-disabled")
    expect(screen.getByRole("menuitem", { name: /Edit schedule/ })).toHaveTextContent("Cadence, pause, resume or remove the schedule")
  })

  it("greys both items for a member who did not make the query, naming the role", async () => {
    who.role = "member"
    who.userId = "u-9"
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    render(<ModelsPage />)
    await user.click(await screen.findByRole("button", { name: "Actions for daily_revenue" }))
    const edit = await screen.findByRole("menuitem", { name: /Edit query/ })
    expect(edit).toHaveAttribute("data-disabled")
    expect(edit).toHaveTextContent("Only the query's creator or a workspace admin can change it. Your role is member.")
    const schedule = screen.getByRole("menuitem", { name: /Edit schedule/ })
    expect(schedule).toHaveAttribute("data-disabled")
    expect(schedule).toHaveTextContent("Scheduling needs the admin role or higher. Your role is member.")
  })

  // The saved list stops at 500, most recently edited first: an older model with no
  // schedule is not on it, and the page says so rather than claiming every model.
  it("says when the saved list was cut off at its limit", async () => {
    savedReply = () =>
      Promise.resolve(
        res(200, {
          saved_queries: Array.from({ length: 500 }, (_, i) =>
            saved(`q-${i}`, `adhoc_${i}`, { materialization: "none", target_table: undefined }),
          ),
          count: 500,
        }),
      )
    render(<ModelsPage />)
    expect(await screen.findByRole("link", { name: "daily_revenue" })).toBeInTheDocument()
    expect(
      screen.getByText(
        "Only the 500 most recently edited saved queries were checked, so an older model with no schedule is not listed.",
      ),
    ).toBeInTheDocument()
    // A list cut off at its cap gives a floor, not an exact count.
    expect(countOf("Manual")).toBe("0+")
    expect(countOf("Total")).toBe("1+")
    // A floor of 0 failures is not a promise that none failed.
    const failed = screen.getByRole("region", { name: "Last run failed" })
    expect(within(failed).getByText("0+")).toBeInTheDocument()
    expect(within(failed).queryByText("No model's last run failed")).not.toBeInTheDocument()
    expect(within(failed).getByText("None of the listed models' last run failed")).toBeInTheDocument()
  })
})
