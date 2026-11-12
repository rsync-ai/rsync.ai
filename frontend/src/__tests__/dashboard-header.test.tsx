import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import type { Dashboard } from "@/lib/api/dashboards"
import { formatAbsoluteTime } from "@/lib/utils"

// A dashboard's header (launch review bug 19): a way back to the list, the name,
// one line saying who sees it, how many tiles it has and when it changed, Edit as
// the one main button for those who may edit, and the rest (History, Export PDF,
// Schedule email) in a ⋯ menu rather than a row of equal outline buttons.

vi.mock("@/components/dashboards/DashboardGridEditorNoSSR", () => ({
  DashboardGridEditorNoSSR: () => <div data-testid="grid-editor-stub" />,
}))

const api = vi.hoisted(() => ({ getDashboard: vi.fn(), drawTile: vi.fn() }))
vi.mock("@/lib/api/dashboards", async (actual) => ({
  ...(await actual<typeof import("@/lib/api/dashboards")>()),
  getDashboard: api.getDashboard,
  drawTile: api.drawTile,
}))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn(), replace: vi.fn() }) }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ meets: () => true, isLoading: false }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { DashboardPage } from "@/components/dashboards/DashboardPage"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  // Radix menus open on pointer events jsdom only half implements.
  Element.prototype.hasPointerCapture ||= () => false
  Element.prototype.scrollIntoView ||= () => {}
})

const text = (id: string, y: number) => ({ id, kind: "text" as const, body: `Note ${id}`, x: 0, y, w: 12, h: 2 })

const dashboard: Dashboard = {
  id: "d1",
  name: "Ops review",
  description: "What the ops team reads on Monday",
  filters: [],
  tiles: [text("t1", 0), text("t2", 2)],
  tile_count: 2,
  visibility: "workspace",
  proposed_by: "none",
  mine: true,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-06T09:30:00Z",
}

beforeEach(() => {
  api.getDashboard.mockResolvedValue({ dashboard, can_manage: true })
})

afterEach(() => vi.clearAllMocks())

/** The line under the name: who sees it · how many tiles · when it changed. */
const meta = () => screen.getByText(/^Edited /).parentElement as HTMLElement

describe("a dashboard's header", () => {
  it("links back to the list, and says who sees it, how many tiles it has and when it changed", async () => {
    render(<DashboardPage dashboardId="d1" />)
    expect(await screen.findByRole("heading", { name: "Ops review" })).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Dashboards" })).toHaveAttribute("href", "/dashboards")
    expect(screen.getByText("What the ops team reads on Monday")).toBeInTheDocument()

    expect(meta()).toHaveTextContent(/^Workspace·2 tiles·Edited /)
    // The exact time is on hover.
    expect(within(meta()).getByText(/^Edited /)).toHaveAttribute("title", formatAbsoluteTime(dashboard.updated_at))
    expect(within(meta()).queryByText(/Drafted/)).toBeNull()
  })

  it("makes Edit the one main button, with the other actions behind ⋯", async () => {
    render(<DashboardPage dashboardId="d1" />)
    const edit = await screen.findByRole("button", { name: "Edit" })
    const more = screen.getByRole("button", { name: "Dashboard actions" })
    // Edit is the filled button, ⋯ the outline one; History and Export PDF are no longer buttons of their own.
    expect(edit.className.split(/\s+/)).toContain("bg-zinc-900")
    expect(more.className.split(/\s+/)).toEqual(expect.arrayContaining(["border", "bg-white"]))
    expect(screen.queryByRole("button", { name: "History" })).toBeNull()
    expect(screen.queryByRole("button", { name: /Export PDF/ })).toBeNull()

    await userEvent.setup().click(more)
    const menu = await screen.findByRole("menu")
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent?.trim())).toEqual(["History", "Export PDF"])
  })

  it("says one person sees it, the one tile, and who drafted it", async () => {
    api.getDashboard.mockResolvedValue({
      dashboard: { ...dashboard, visibility: "private", proposed_by: "assistant", tiles: [text("t1", 0)], tile_count: 1 },
      can_manage: false,
    })
    render(<DashboardPage dashboardId="d1" />)
    await screen.findByRole("heading", { name: "Ops review" })
    expect(meta()).toHaveTextContent(/^Only me·1 tile·Edited .*Drafted by Ask rsync\.ai$/)
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull()
  })
})
