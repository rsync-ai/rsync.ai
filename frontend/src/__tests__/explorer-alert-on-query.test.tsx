/**
 * THE EXPLORER'S "ALERT ON THIS QUERY" (workflows-design.md § 0.8).
 *
 * It opens the workflow builder with the one statement Run would execute and
 * its connection as the query, handed over in sessionStorage, never the URL.
 * Only a statement that reads can be a workflow's query, so anything else
 * hands nothing over. Building a workflow is for workspace owners and admins,
 * so a member sees no Alert on this query.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import ExplorerPage from "@/app/(dashboard)/explorer/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { takeWorkflowQuery } from "@/lib/explorer/queryTabs"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

const push = vi.hoisted(() => vi.fn())
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), message: vi.fn() },
}))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, refresh: vi.fn(), replace: vi.fn() }),
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
const ORDERS = "SELECT day, count(*) AS orders FROM shop.orders GROUP BY day"

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

function serve() {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) {
      return res(200, { connection_id: CONN_ID, table_count: 0, tables: [], foreign_keys: [] })
    }
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    if (url.includes("/explorer/saved?")) return res(200, { saved_queries: [], count: 0 })
    return res(200, {})
  })
}

/** One tab on the warehouse connection, holding sql. */
function tabWith(sql: string) {
  localStorage.setItem(
    "rsync_explorer_tabs_v1:u-1:w-1",
    JSON.stringify({ active: "t0", tabs: [{ id: "t0", name: "Query 1", sql, connectionId: CONN_ID }] }),
  )
}

const alertButton = () => screen.queryByRole("button", { name: /Alert on this query/ })

async function openExplorer() {
  serve()
  render(<ExplorerPage />)
  await screen.findByTestId("compose-pane")
  // Control: the page has drawn its toolbar.
  expect(await screen.findByRole("button", { name: /^Saved$/ })).toBeInTheDocument()
}

beforeAll(() => {
  // CodeMirror measures text through Range rects, which jsdom does not implement.
  const empty = () => Object.assign([], { item: () => null }) as unknown as DOMRectList
  Range.prototype.getClientRects ||= empty
  Range.prototype.getBoundingClientRect ||= () => new DOMRect(0, 0, 0, 0)
})

beforeEach(() => {
  mockFetch.mockReset()
  push.mockReset()
  localStorage.clear()
  sessionStorage.clear()
  localStorage.setItem(ACTIVE_WORKSPACE_KEY, "w-1")
  admin = true
})

describe("Alert on this query", () => {
  it("opens the workflow builder with the query and its connection, not in the URL", async () => {
    tabWith(ORDERS)
    const user = userEvent.setup()
    await openExplorer()
    await waitFor(() => expect(alertButton()).toBeEnabled())

    await user.click(alertButton()!)
    expect(push).toHaveBeenCalledWith("/workflows/new?from=explorer")
    expect(push.mock.calls[0][0]).not.toContain("SELECT")
    expect(takeWorkflowQuery()).toEqual({ sql: ORDERS, connectionId: CONN_ID })
    // Taken once: a second visit opens on a query to fill in.
    expect(takeWorkflowQuery()).toBeNull()
  })

  it("hands over the one statement Run would execute, not the whole editor", async () => {
    tabWith(`${ORDERS};\nDELETE FROM shop.orders`)
    const user = userEvent.setup()
    await openExplorer()
    await waitFor(() => expect(alertButton()).toBeEnabled())

    // The caret starts in the first statement, so Run would execute the SELECT.
    await user.click(alertButton()!)
    expect(takeWorkflowQuery()).toEqual({ sql: ORDERS, connectionId: CONN_ID })
  })

  it("hands nothing over for a statement that writes", async () => {
    tabWith("DELETE FROM shop.orders WHERE day < now()")
    const user = userEvent.setup()
    await openExplorer()
    await waitFor(() => expect(alertButton()).toBeDisabled())

    await user.click(alertButton()!)
    expect(push).not.toHaveBeenCalled()
    expect(takeWorkflowQuery()).toBeNull()
  })

  it("hands nothing over for an empty editor", async () => {
    tabWith("")
    await openExplorer()
    await waitFor(() => expect(alertButton()).toBeDisabled())
  })

  it("is not shown to a member, who cannot build a workflow", async () => {
    admin = false
    tabWith(ORDERS)
    await openExplorer()
    expect(alertButton()).toBeNull()
  })
})
