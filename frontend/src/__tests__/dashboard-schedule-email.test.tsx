import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen } from "@testing-library/react"

import type { Dashboard } from "@/lib/api/dashboards"

// Schedule email on a dashboard (dashboards-design.md § 8, D3): it opens Ask rsync.ai
// about this dashboard in a new chat, offering to Tab in a request that drafts a
// workflow with a dashboard step. Only owners and admins may draft a workflow, and
// the panel exists only where Ask rsync.ai is on, so no one else sees the item in
// the dashboard's ⋯ menu.

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
const role = vi.hoisted(() => ({ admin: true }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ meets: (min: string) => min !== "admin" || role.admin, isLoading: false }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

import { DashboardPage, SCHEDULE_EMAIL_ASK } from "@/components/dashboards/DashboardPage"
import { featureFlagsManager } from "@/config/features"
import { useAssistantStore } from "@/lib/store/useAssistantStore"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const dashboard: Dashboard = {
  id: "d1",
  name: "Ops review",
  description: "",
  filters: [],
  tiles: [{ id: "t1", kind: "text", body: "Notes", x: 0, y: 0, w: 12, h: 2 }],
  tile_count: 1,
  visibility: "workspace",
  proposed_by: "none",
  mine: false,
  created_at: "2026-10-07T00:00:00Z",
  updated_at: "2026-10-07T00:00:00Z",
}

beforeEach(() => {
  role.admin = true
  featureFlagsManager.updateFlags({ assistant: true })
  api.getDashboard.mockResolvedValue({ dashboard, can_manage: false })
  useAssistantStore.setState({ open: false, page: null, pageCleared: false, suggestion: null, chats: [], activeChatId: "old-chat" })
})

afterEach(() => {
  featureFlagsManager.resetFlags()
  vi.clearAllMocks()
})

/** Opens the dashboard's ⋯ menu, where Schedule email sits with History and Export PDF. */
async function openMenu(item: string) {
  fireEvent.keyDown(await screen.findByRole("button", { name: "Dashboard actions" }), { key: "Enter" })
  return screen.findByRole("menuitem", { name: item })
}

describe("Schedule email on a dashboard", () => {
  it("opens Ask rsync.ai about this dashboard in a new chat, offering the request", async () => {
    render(<DashboardPage dashboardId="d1" />)
    fireEvent.click(await openMenu("Schedule email"))
    expect(useAssistantStore.getState()).toMatchObject({
      open: true,
      page: { kind: "dashboard", id: "d1", name: "Ops review" },
      suggestion: "Email this dashboard to me every Monday at 9:00",
      activeChatId: null,
    })
    expect(SCHEDULE_EMAIL_ASK).toBe("Email this dashboard to me every Monday at 9:00")
  })

  it("sits beside History and Export PDF for an admin who cannot edit this dashboard", async () => {
    render(<DashboardPage dashboardId="d1" />)
    await openMenu("Schedule email")
    expect(screen.getAllByRole("menuitem").map((i) => i.textContent?.trim())).toEqual(["History", "Export PDF", "Schedule email"])
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull()
  })

  it("is not shown to a member, who may not draft a workflow", async () => {
    role.admin = false
    render(<DashboardPage dashboardId="d1" />)
    await openMenu("Export PDF")
    expect(screen.queryByRole("menuitem", { name: "Schedule email" })).toBeNull()
  })

  it("is not shown where Ask rsync.ai is off", async () => {
    featureFlagsManager.updateFlags({ assistant: false })
    render(<DashboardPage dashboardId="d1" />)
    await openMenu("Export PDF")
    expect(screen.queryByRole("menuitem", { name: "Schedule email" })).toBeNull()
  })
})
