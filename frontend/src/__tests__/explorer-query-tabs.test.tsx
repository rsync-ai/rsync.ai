/**
 * THE EXPLORER'S QUERY TABS (dashboards-design.md § 7.5).
 *
 * Up to 10 tabs per person, each with its own connection, SQL, Run and Cancel.
 * They are kept in this browser per person and workspace — a tab's name,
 * connection and SQL, never its results — and Ask rsync.ai's "Open in Explorer"
 * opens its SQL in a new one. The rules themselves are unit-tested in
 * lib/explorer/__tests__/queryTabs.test.ts; this file pins that the page keeps
 * them: the limit is enforced where a person clicks, the storage key follows
 * the signed-in person and the workspace switcher, a result never reaches
 * storage, a run lands in the tab it started in, and a hand-off survives the
 * stored tabs loading over it.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"
import { toast } from "sonner"

import ExplorerPage from "@/app/(dashboard)/explorer/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { ACTIVE_WORKSPACE_KEY, setActiveWorkspaceId } from "@/lib/workspace/active-workspace"
import { openSqlInExplorer, takeExplorerSql } from "@/lib/explorer/queryTabs"

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
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({
    role: "admin",
    isLoading: false,
    error: false,
    activeWorkspace: null,
    can: () => true,
    meets: () => true,
  }),
}))
let currentUser: { id: string } | null = { id: "u-1" }
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: currentUser, role: null, isLoading: false }),
}))

const mockFetch = authFetch as unknown as Mock
const CONN_ID = "11111111-1111-1111-1111-111111111111"
const LIMIT_MESSAGE = "10 query tabs are open. Close one to open another."
const keyFor = (user: string, workspace: string) => `rsync_explorer_tabs_v1:${user}:${workspace}`

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

const TABLES = {
  connection_id: CONN_ID,
  table_count: 1,
  tables: [{ name: "customers", schema: "shop", columns: [{ name: "email", type: "text" }] }],
  foreign_keys: [],
}

// `query` answers /explorer/query; everything else is the connection and its schema.
function serve(query?: () => Promise<Response>) {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) return res(200, TABLES)
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    if (url.includes("/explorer/query") && query) return query()
    return res(200, {})
  })
}

function storeTabs(key: string, tabs: { id: string; name: string; sql: string }[], active = tabs[0].id) {
  localStorage.setItem(
    key,
    JSON.stringify({ active, tabs: tabs.map((t) => ({ ...t, connectionId: CONN_ID })) }),
  )
}

function storedTabs(key: string): { name: string; sql: string }[] {
  const raw = localStorage.getItem(key)
  return raw ? JSON.parse(raw).tabs : []
}

const tabs = () => screen.getAllByTestId("query-tab")
const tabNames = () => tabs().map((t) => t.textContent)
// The tab's own button: its accessible name gains "Running" while a run is in
// flight, so it is found by the tab's text rather than by role name.
function tabButton(name: string): HTMLElement {
  const tab = tabs().find((t) => t.textContent === name)
  expect(tab, `tab "${name}" in ${JSON.stringify(tabNames())}`).toBeDefined()
  return tab!.querySelector("button") as HTMLElement
}

beforeAll(() => {
  // CodeMirror measures text through Range rects, which jsdom does not implement.
  const empty = () => Object.assign([], { item: () => null }) as unknown as DOMRectList
  Range.prototype.getClientRects ||= empty
  Range.prototype.getBoundingClientRect ||= () => new DOMRect(0, 0, 0, 0)
})

beforeEach(() => {
  mockFetch.mockReset()
  vi.mocked(toast.error).mockClear()
  localStorage.clear()
  sessionStorage.clear()
  // localStorage wins over the cookie a previous test's switch may have left.
  localStorage.setItem(ACTIVE_WORKSPACE_KEY, "w-1")
  currentUser = { id: "u-1" }
})

describe("opening tabs", () => {
  it("opens 10 and refuses the 11th, saying why", async () => {
    serve()
    const user = userEvent.setup()
    render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")

    const add = screen.getByRole("button", { name: "New query tab" })
    for (let i = 0; i < 9; i++) await user.click(add)
    expect(tabs()).toHaveLength(10)
    // Control: the nine that opened said nothing, and the button was not yet marked.
    expect(toast.error).not.toHaveBeenCalled()
    expect(add).toHaveAttribute("aria-disabled", "true")

    await user.click(add)
    expect(tabs()).toHaveLength(10)
    expect(toast.error).toHaveBeenCalledWith(LIMIT_MESSAGE)
    expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(10)
  })

  it("refuses an Open in Explorer hand-off at 10 tabs too, and leaves the 10 alone", async () => {
    serve()
    const ten = Array.from({ length: 10 }, (_, i) => ({ id: `t${i}`, name: `Q${i}`, sql: `select ${i}` }))
    storeTabs(keyFor("u-1", "w-1"), ten)
    openSqlInExplorer({ sql: "select 42", name: "From Ask" })
    render(<ExplorerPage />)

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(LIMIT_MESSAGE))
    expect(tabNames()).toEqual(ten.map((t) => t.name))
    expect(storedTabs(keyFor("u-1", "w-1")).map((t) => t.sql)).not.toContain("select 42")
  })
})

describe("where the tabs are kept", () => {
  it("stores them per person and per workspace, and a switch loads the other workspace's", async () => {
    serve()
    const user = userEvent.setup()
    const { unmount } = render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")
    await user.click(screen.getByRole("button", { name: "New query tab" }))
    await waitFor(() => expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(2))

    act(() => setActiveWorkspaceId("w-2"))
    await waitFor(() => expect(tabs()).toHaveLength(1))
    await waitFor(() => expect(storedTabs(keyFor("u-1", "w-2"))).toHaveLength(1))
    // The switch did not write workspace 2's single tab over workspace 1's two.
    expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(2)

    act(() => setActiveWorkspaceId("w-1"))
    await waitFor(() => expect(tabs()).toHaveLength(2))
    unmount()

    // Another person on the same browser and workspace starts with their own.
    currentUser = { id: "u-2" }
    render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")
    await waitFor(() => expect(storedTabs(keyFor("u-2", "w-1"))).toHaveLength(1))
    expect(tabs()).toHaveLength(1)
    expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(2)
  })
})

describe("a run belongs to its tab", () => {
  it("lands in the tab it started in, and its rows never reach storage", async () => {
    let answer!: (r: Response) => void
    serve(() => new Promise<Response>((resolve) => (answer = resolve)))
    storeTabs(keyFor("u-1", "w-1"), [
      { id: "t1", name: "Customers", sql: "select email from shop.customers" },
      { id: "t2", name: "Scratch", sql: "" },
    ])
    const user = userEvent.setup()
    render(<ExplorerPage />)

    await waitFor(() => expect(tabNames()).toEqual(["Customers", "Scratch"]))
    await user.click(await screen.findByRole("button", { name: /Run Query/i }))
    await waitFor(() => expect(answer).toBeTypeOf("function"))

    // Switch away while the run is in flight, then let it finish.
    await user.click(tabButton("Scratch"))
    await act(async () =>
      answer(
        res(200, {
          columns: ["email"],
          rows: [{ email: "ada@customer.example" }],
          row_count: 1,
          execution_time_ms: 4,
        }),
      ),
    )
    // Scratch never ran: no grid, and its own Run is not a Cancel.
    expect(screen.queryByTestId("results-grid-scroller")).toBeNull()
    expect(screen.getByRole("button", { name: /Run Query/i })).toBeInTheDocument()

    await user.click(tabButton("Customers"))
    expect(await screen.findByTestId("results-grid-scroller")).toHaveTextContent("ada@customer.example")

    // The run went to History and the tab kept its SQL, but the row went nowhere.
    await waitFor(() =>
      expect(localStorage.getItem(`rsync_explorer_history_v1:${CONN_ID}`)).toContain("select email from shop.customers"),
    )
    expect(storedTabs(keyFor("u-1", "w-1"))[0].sql).toBe("select email from shop.customers")
    for (let i = 0; i < localStorage.length; i++) {
      const k = localStorage.key(i)!
      expect({ key: k, leaks: localStorage.getItem(k)!.includes("ada@customer.example") }).toEqual({
        key: k,
        leaks: false,
      })
    }
  })
})

describe("Open in Explorer", () => {
  it("opens the SQL in a new tab beside the stored ones, not under them", async () => {
    serve()
    storeTabs(keyFor("u-1", "w-1"), [
      { id: "t1", name: "Revenue", sql: "select 1" },
      { id: "t2", name: "Churn", sql: "select 2" },
    ])
    openSqlInExplorer({ sql: "select count(*) from shop.customers", name: "From Ask", connectionId: CONN_ID })
    render(<ExplorerPage />)

    await waitFor(() => expect(tabNames()).toEqual(["Revenue", "Churn", "From Ask"]))
    expect(tabButton("From Ask")).toHaveAttribute("aria-current", "true")
    await waitFor(() =>
      expect(document.querySelector(".cm-content")?.textContent).toBe("select count(*) from shop.customers"),
    )
    await waitFor(() => expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(3))
    // Taken once: a reload does not open it again.
    expect(takeExplorerSql()).toBeNull()
  })

  it("opens a hand-off sent while the Explorer is already on screen", async () => {
    serve()
    render(<ExplorerPage />)
    await screen.findByTestId("compose-pane")
    await waitFor(() => expect(storedTabs(keyFor("u-1", "w-1"))).toHaveLength(1))

    act(() => openSqlInExplorer({ sql: "select 7" }))
    await waitFor(() => expect(tabs()).toHaveLength(2))
    expect(tabButton("Query 2")).toHaveAttribute("aria-current", "true")
    // With no connection named, it opens on the one on screen.
    await waitFor(() => expect(storedTabs(keyFor("u-1", "w-1"))[1]).toMatchObject({ sql: "select 7", connectionId: CONN_ID }))
  })
})
