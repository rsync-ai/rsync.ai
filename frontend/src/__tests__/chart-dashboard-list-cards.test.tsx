import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ChartsPage from "@/app/(dashboard)/charts/page"
import DashboardsPage from "@/app/(dashboard)/dashboards/page"
import { listCharts, type ChartRecord } from "@/lib/api/charts"
import { listDashboards, type Dashboard } from "@/lib/api/dashboards"

// The Dashboards and Charts lists as card grids. Pinned here:
//   - a dashboard's card says how many tiles and filters it has ("no filters",
//     never "None"), who sees it, and "Drafted" only when Ask rsync.ai or an
//     agent made it;
//   - All / Mine / Shared carry exact counts and narrow the grid in the browser;
//   - a chart's card names its type and its source, and says in amber when its
//     source is gone or not shared, rather than naming a source it cannot read;
//   - the chart type chips count each type the workspace has, and only those;
//   - an empty workspace gets an empty state with the New button in it.

vi.mock("@/lib/api/dashboards", async (orig) => ({ ...(await orig()), listDashboards: vi.fn() }))
vi.mock("@/lib/api/charts", async (orig) => ({ ...(await orig()), listCharts: vi.fn() }))

const mockDashboards = listDashboards as unknown as Mock
const mockCharts = listCharts as unknown as Mock

function dashboard(id: string, name: string, extra: Partial<Dashboard> = {}): Dashboard {
  return {
    id,
    name,
    description: "",
    filters: [],
    tile_count: 3,
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...extra,
  }
}

function chart(id: string, name: string, extra: Partial<ChartRecord> = {}): ChartRecord {
  return {
    id,
    name,
    source_id: "sq-1",
    source_name: "daily_revenue",
    source_kind: "model",
    source_state: "ok",
    settings: { version: 2, type: "line", roles: { measure: [{ aggregate: "sum", column: "amount", color: "series-3" }] } },
    settings_version: 2,
    source_columns: [],
    visibility: "workspace",
    proposed_by: "none",
    mine: true,
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...extra,
  }
}

const card = (name: string) => screen.getByRole("heading", { name }).closest("a") as HTMLElement
const chip = (group: string, name: RegExp) =>
  within(screen.getByRole("group", { name: group })).getByRole("button", { name })

beforeEach(() => {
  mockDashboards.mockReset()
  mockCharts.mockReset()
})

describe("Dashboards list as cards", () => {
  beforeEach(() => {
    mockDashboards.mockResolvedValue({
      can_manage: true,
      dashboards: [
        dashboard("d1", "Revenue", {
          description: "Daily revenue by region",
          tile_count: 7,
          filters: [
            { id: "f1", label: "Date", kind: "date", column: "day" },
            { id: "f2", label: "Region", kind: "values", column: "region" },
          ] as Dashboard["filters"],
        }),
        dashboard("d2", "Signups", { tile_count: 1, mine: false, proposed_by: "assistant", visibility: "private" }),
        dashboard("d3", "Churn", { mine: false, proposed_by: "agent" }),
      ],
    })
  })

  it("says what is on each card, and Drafted only for one Ask rsync.ai or an agent made", async () => {
    render(<DashboardsPage />)
    await screen.findByRole("heading", { name: "Revenue" })
    const revenue = card("Revenue")
    expect(revenue).toHaveAttribute("href", "/dashboards/d1")
    expect(revenue).toHaveTextContent("Daily revenue by region")
    expect(revenue).toHaveTextContent("7 tiles · 2 filters")
    expect(within(revenue).getByText("2 filters")).toHaveAttribute("title", "Date, Region")
    expect(within(revenue).getByText("Workspace")).toBeInTheDocument()
    expect(within(revenue).queryByText("Drafted")).not.toBeInTheDocument()

    const signups = card("Signups")
    expect(signups).toHaveTextContent("1 tile · no filters")
    expect(signups).not.toHaveTextContent("None")
    expect(within(signups).getByText("Only me")).toBeInTheDocument()
    expect(within(signups).getByText("Drafted").closest("[title]")).toHaveAttribute("title", "Drafted by Ask rsync.ai")
    expect(within(card("Churn")).getByText("Drafted").closest("[title]")).toHaveAttribute("title", "Drafted by an agent")
  })

  it("counts All, Mine and Shared exactly and narrows the grid to the one picked", async () => {
    const user = userEvent.setup()
    render(<DashboardsPage />)
    await screen.findByRole("heading", { name: "Revenue" })
    expect(chip("Show dashboards", /^All/)).toHaveTextContent("All3")
    expect(chip("Show dashboards", /^Mine/)).toHaveTextContent("Mine1")
    expect(chip("Show dashboards", /^Shared/)).toHaveTextContent("Shared2")
    expect(chip("Show dashboards", /^All/)).toHaveAttribute("aria-pressed", "true")

    await user.click(chip("Show dashboards", /^Shared/))
    expect(screen.queryByRole("heading", { name: "Revenue" })).not.toBeInTheDocument()
    expect(screen.getByRole("heading", { name: "Signups" })).toBeInTheDocument()
    expect(screen.getByRole("heading", { name: "Churn" })).toBeInTheDocument()

    await user.click(chip("Show dashboards", /^Mine/))
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Revenue"])
  })

  it("puts New dashboard in an empty state when there are none", async () => {
    mockDashboards.mockResolvedValue({ can_manage: true, dashboards: [] })
    render(<DashboardsPage />)
    expect(await screen.findByText("No dashboards yet")).toBeInTheDocument()
    expect(screen.getAllByRole("link", { name: /New dashboard/ })).toHaveLength(1)
    expect(screen.queryByRole("group", { name: "Show dashboards" })).not.toBeInTheDocument()
  })
})

describe("Charts list as cards", () => {
  beforeEach(() => {
    mockCharts.mockResolvedValue({
      can_manage: true,
      charts: [
        chart("c1", "Revenue trend"),
        chart("c2", "Orders by region", { settings: { version: 2, type: "bar", roles: { measure: [{ aggregate: "count" }] } }, source_kind: "saved_query", source_name: "orders_q" }),
        chart("c3", "Total revenue", { settings: { version: 2, type: "scorecard", roles: { measure: [{ aggregate: "sum", column: "amount" }] } }, source_state: "deleted", source_name: undefined }),
        chart("c4", "Signups", { settings: { version: 2, type: "line", roles: { measure: [{ aggregate: "count" }] } }, source_state: "hidden", mine: false, proposed_by: "assistant" }),
      ],
    })
  })

  it("names each card's type and source, and says in amber when the source cannot be read", async () => {
    render(<ChartsPage />)
    await screen.findByRole("heading", { name: "Revenue trend" })
    const trend = card("Revenue trend")
    expect(trend).toHaveAttribute("href", "/charts/c1")
    expect(trend).toHaveTextContent("Line")
    expect(trend).toHaveTextContent("daily_revenue model")
    expect(card("Orders by region")).toHaveTextContent("orders_q saved query")

    expect(card("Total revenue")).toHaveTextContent("Its source was deleted, so it draws nothing.")
    const hidden = card("Signups")
    expect(hidden).toHaveTextContent("Its source is not shared with you, so it draws nothing for you.")
    expect(hidden).not.toHaveTextContent("daily_revenue")
    expect(within(hidden).getByText("Drafted")).toBeInTheDocument()
  })

  it("tints the type icon with the chart's own series colour", async () => {
    render(<ChartsPage />)
    await screen.findByRole("heading", { name: "Revenue trend" })
    const tile = within(card("Revenue trend")).getByTitle("Line")
    expect(tile.style.color).toBe("hsl(var(--chart-series-3))")
    // No colour set: the first series colour, as the chart itself draws.
    expect(within(card("Orders by region")).getByTitle("Bar").style.color).toBe("hsl(var(--chart-series-1))")
  })

  it("counts each type the workspace has, only those, and narrows to the one picked", async () => {
    const user = userEvent.setup()
    render(<ChartsPage />)
    await screen.findByRole("heading", { name: "Revenue trend" })
    const group = screen.getByRole("group", { name: "Show chart types" })
    expect(within(group).getAllByRole("button").map((b) => b.textContent)).toEqual(["All4", "Line2", "Bar1", "Scorecard1"])

    await user.click(chip("Show chart types", /^Line/))
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual(["Revenue trend", "Signups"])
  })

  it("puts New chart in an empty state when there are none, and says who makes them otherwise", async () => {
    mockCharts.mockResolvedValue({ can_manage: true, charts: [] })
    const { unmount } = render(<ChartsPage />)
    expect(await screen.findByText("No charts yet")).toBeInTheDocument()
    expect(screen.getAllByRole("link", { name: /New chart/ })).toHaveLength(1)
    unmount()

    mockCharts.mockResolvedValue({ can_manage: false, charts: [] })
    render(<ChartsPage />)
    expect(await screen.findByText("Workspace owners and admins make them.")).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: /New chart/ })).not.toBeInTheDocument()
  })
})
