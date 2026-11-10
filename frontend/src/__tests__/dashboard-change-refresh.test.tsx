import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen, waitFor } from "@testing-library/react"

import type { Dashboard, DashboardChange, DashboardChangeSide } from "@/lib/api/dashboards"

// A change applied from Ask rsync.ai's panel (dashboards-design.md § 7.6). The panel
// sits beside the page, so the card in it has no handle on the dashboard the person
// has open. On prod (2026-10-06) Apply from the panel saved the change, and the board
// behind it kept drawing the tile as it was until the page was reloaded by hand.
//
// What is pinned, for every card that applies a change, wherever it is shown:
//   - the open dashboard the change is for reads itself again and redraws its tiles;
//   - so does each change applied after it;
//   - a dashboard the change is not for does nothing;
//   - a dashboard being edited keeps the person's draft (its save is refused as out
//     of date by the server); it reads itself again when the editing ends;
//   - the card on the dashboard itself (?change=) reloads it once, not twice.

vi.mock("@/components/dashboards/DashboardGridEditorNoSSR", () => ({
  DashboardGridEditorNoSSR: () => <div data-testid="grid-editor-stub" />,
}))

const api = vi.hoisted(() => ({
  getDashboard: vi.fn(),
  drawTile: vi.fn(),
  listCharts: vi.fn(),
  getDashboardChange: vi.fn(),
  applyDashboardChange: vi.fn(),
}))
vi.mock("@/lib/api/dashboards", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/dashboards")>()),
  getDashboard: api.getDashboard,
  drawTile: api.drawTile,
  getDashboardChange: api.getDashboardChange,
  applyDashboardChange: api.applyDashboardChange,
}))
vi.mock("@/lib/api/charts", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/charts")>()),
  listCharts: api.listCharts,
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspaceRole: () => ({ meets: () => true, isLoading: false }) }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { DashboardChangeReview } from "@/components/dashboards/DashboardChangeReview"
import { DashboardPage } from "@/components/dashboards/DashboardPage"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const dashboard: Dashboard = {
  id: "d1",
  name: "Swipes",
  description: "",
  filters: [],
  tiles: [{ id: "t1", kind: "text", body: "Notes", x: 0, y: 0, w: 12, h: 2 }],
  tile_count: 1,
  visibility: "workspace",
  proposed_by: "none",
  mine: true,
  created_at: "2026-10-06T00:00:00Z",
  updated_at: "2026-10-06T00:00:00Z",
}

const side = (w: number): DashboardChangeSide => ({
  name: "Notes",
  type: "bar",
  settings: { version: 2, type: "bar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count" }] } },
  x: 0,
  y: 0,
  w,
  h: 7,
})

/** A change's id is "<dashboard id>/<n>", so the stubs know which dashboard it is for. */
const dashboardOf = (changeId: string) => changeId.split("/")[0]

const change = (id: string): DashboardChange => ({
  id,
  dashboard_id: dashboardOf(id),
  dashboard_name: "Swipes",
  tile_id: "t1",
  chart_id: "c1",
  before: side(6),
  after: side(12),
  moves: [],
  summary: "Full width.",
  chart_changed: false,
  size_changed: true,
  shared_elsewhere: 0,
  copy: false,
  source_id: "s1",
  source_name: "swipes",
  status: "proposed",
  expired: false,
  stale: false,
  copied: false,
  proposed_by: "assistant",
  created_at: new Date().toISOString(),
  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
  url: "",
})

beforeEach(() => {
  vi.clearAllMocks()
  api.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
  api.drawTile.mockReturnValue(new Promise(() => {}))
  api.listCharts.mockResolvedValue({ charts: [], can_manage: true })
  api.getDashboardChange.mockImplementation((id: string) => Promise.resolve({ change: change(id), can_manage: true }))
  api.applyDashboardChange.mockImplementation((id: string) =>
    Promise.resolve({ dashboard_id: dashboardOf(id), chart_id: "c1", copied: false }),
  )
})

/** The page with the dashboard open, and the panel's card beside it (no onApplied: the panel has no handle on the page). */
async function openWithPanelCard(dashboardId: string) {
  render(
    <>
      <DashboardPage dashboardId="d1" />
      <aside aria-label="Ask rsync.ai">
        <DashboardChangeReview changeId={`${dashboardId}/1`} />
      </aside>
    </>,
  )
  await screen.findByRole("heading", { name: "Swipes" })
  await screen.findByRole("button", { name: "Apply change" })
  expect(api.getDashboard).toHaveBeenCalledTimes(1)
}

const apply = async () => {
  fireEvent.click(screen.getByRole("button", { name: "Apply change" }))
  await screen.findByText(/^Applied\./)
}

describe("a change applied from Ask rsync.ai's panel", () => {
  it("reloads the dashboard it is for, open beside the panel", async () => {
    await openWithPanelCard("d1")
    await apply()
    await waitFor(() => expect(api.getDashboard).toHaveBeenCalledTimes(2))
    expect(api.getDashboard).toHaveBeenLastCalledWith("d1")
  })

  it("reloads it again for each change applied after the first", async () => {
    render(
      <>
        <DashboardPage dashboardId="d1" />
        <aside aria-label="Ask rsync.ai">
          <DashboardChangeReview changeId="d1/1" />
          <DashboardChangeReview changeId="d1/2" />
        </aside>
      </>,
    )
    await screen.findByRole("heading", { name: "Swipes" })
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Apply change" })).toHaveLength(2))

    fireEvent.click(screen.getAllByRole("button", { name: "Apply change" })[0])
    await waitFor(() => expect(api.getDashboard).toHaveBeenCalledTimes(2))
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Apply change" })).toHaveLength(1))
    fireEvent.click(screen.getByRole("button", { name: "Apply change" }))
    await waitFor(() => expect(api.getDashboard).toHaveBeenCalledTimes(3))
  })

  it("leaves a dashboard it is not for as it is", async () => {
    await openWithPanelCard("d2")
    await apply()
    // Give a reload the same turns it takes in the test above.
    await new Promise((r) => setTimeout(r, 20))
    expect(api.getDashboard).toHaveBeenCalledTimes(1)
  })

  it("keeps an open editor's draft, and reloads when the editing ends", async () => {
    await openWithPanelCard("d1")
    fireEvent.click(screen.getByRole("button", { name: /Edit/ }))
    await screen.findByTestId("grid-editor-stub")

    await apply()
    await new Promise((r) => setTimeout(r, 20))
    expect(api.getDashboard).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId("grid-editor-stub")).toBeTruthy()

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    await waitFor(() => expect(api.getDashboard).toHaveBeenCalledTimes(2))
  })
})

describe("a change applied on the dashboard itself (?change=)", () => {
  it("reloads it once", async () => {
    render(<DashboardPage dashboardId="d1" changeId="d1/1" />)
    await screen.findByRole("heading", { name: "Swipes" })
    await screen.findByRole("button", { name: "Apply change" })
    await apply()
    await waitFor(() => expect(api.getDashboard).toHaveBeenCalledTimes(2))
    await new Promise((r) => setTimeout(r, 20))
    expect(api.getDashboard).toHaveBeenCalledTimes(2)
  })
})
