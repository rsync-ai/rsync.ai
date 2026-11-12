/**
 * THE EXPLORER'S "CREATE CHART" (dashboards-design.md § 7.5).
 *
 * A chart's source is a model or a saved query (§ 7.2), so Create chart charts
 * the saved query last opened from Saved while the editor still holds its SQL,
 * and otherwise opens the chart page to pick a source. Making charts is for
 * workspace owners and admins, so a member sees no Create chart.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import ExplorerPage from "@/app/(dashboard)/explorer/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), message: vi.fn() },
}))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn(), replace: vi.fn() }),
  usePathname: () => "/explorer",
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock("next-themes", () => ({
  useTheme: () => ({ resolvedTheme: "light", setTheme: vi.fn() }),
}))
vi.mock("@/lib/hooks/useWorkspaceLayers", () => ({
  useWorkspaceLayers: () => ({ layers: null, canManage: false, error: null, reload: async () => {} }),
}))
let admin = true
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: admin ? "admin" : "member",
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => admin,
    meets: (min: string) => admin || min === "viewer" || min === "member",
  }),
}))
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: { id: "u-1" }, role: null, isLoading: false }),
}))

const mockFetch = authFetch as unknown as Mock
const CONN_ID = "11111111-1111-1111-1111-111111111111"
const SAVED_ID = "33333333-3333-3333-3333-333333333333"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

const CONNECTION = {
  id: CONN_ID,
  name: "warehouse",
  connector_type: "postgresql",
  type: "source",
  status: "active",
  is_connected: true,
  supports_explorer: true,
  explorer_mode: "sql",
  sql_dialect: "postgresql",
  config: { database: "analytics" },
}

const SAVED = {
  id: SAVED_ID,
  connection_id: CONN_ID,
  name: "Daily orders",
  sql_text: "SELECT day, count(*) AS orders FROM shop.orders GROUP BY day",
  statement_class: "read",
  visibility: "workspace",
  created_by: "u-1",
  created_at: "2026-10-05T00:00:00Z",
  updated_at: "2026-10-05T00:00:00Z",
}

function serve() {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) {
      return res(200, { connection_id: CONN_ID, table_count: 0, tables: [], foreign_keys: [] })
    }
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    if (url.includes("/explorer/saved?")) return res(200, { saved_queries: [SAVED], count: 1 })
    return res(200, {})
  })
}

const chartLinks = () => screen.queryAllByRole("link", { name: /Create chart/ })

beforeAll(() => {
  // CodeMirror measures text through Range rects, which jsdom does not implement.
  const empty = () => Object.assign([], { item: () => null }) as unknown as DOMRectList
  Range.prototype.getClientRects ||= empty
  Range.prototype.getBoundingClientRect ||= () => new DOMRect(0, 0, 0, 0)
})

beforeEach(() => {
  mockFetch.mockReset()
  localStorage.clear()
  sessionStorage.clear()
  localStorage.setItem(ACTIVE_WORKSPACE_KEY, "w-1")
  // One tab, on the warehouse connection.
  localStorage.setItem(
    "rsync_explorer_tabs_v1:u-1:w-1",
    JSON.stringify({ active: "t0", tabs: [{ id: "t0", name: "Query 1", sql: "", connectionId: CONN_ID }] }),
  )
  admin = true
})

describe("Create chart", () => {
  it("charts the saved query opened from Saved, and picks a source once the editor holds other SQL", async () => {
    serve()
    const user = userEvent.setup()
    render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")

    // Nothing opened yet: the chart page asks for a source.
    await waitFor(() => expect(chartLinks().length).toBeGreaterThan(0))
    for (const link of chartLinks()) expect(link).toHaveAttribute("href", "/charts/new")

    await user.click(screen.getByRole("button", { name: /^Saved$/ }))
    // The row itself, not its Schedule, Edit, History or Delete buttons.
    await user.click(await screen.findByRole("button", { name: /^Daily orders/ }))
    await waitFor(() =>
      expect(chartLinks().map((l) => l.getAttribute("href"))).toEqual(
        chartLinks().map(() => `/charts/new?source=${SAVED_ID}`),
      ),
    )
    expect(chartLinks().length).toBeGreaterThan(0)

    // A new tab holds other SQL: that is not the saved query, so it is not charted as one.
    await user.click(screen.getByRole("button", { name: "New query tab" }))
    await waitFor(() => {
      for (const link of chartLinks()) expect(link).toHaveAttribute("href", "/charts/new")
    })
  })

  it("is not shown to a member, who cannot make charts", async () => {
    admin = false
    serve()
    render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")
    // Control: the page has drawn its toolbar.
    expect(await screen.findByRole("button", { name: /^Saved$/ })).toBeInTheDocument()
    expect(chartLinks()).toHaveLength(0)
  })
})
