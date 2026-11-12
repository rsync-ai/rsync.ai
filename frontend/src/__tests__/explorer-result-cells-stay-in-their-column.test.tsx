/**
 * A RESULT CELL STAYS IN ITS COLUMN, AND A CUT-OFF VALUE CAN STILL BE READ.
 *
 * "data explorer output is not showing correctly": a JSON value has no space
 * to break at, and the result cell had a max-width but no clipping, so the
 * text ran 600px past its 520px cell across the columns beside it. Every cell
 * is now one line, cut with an ellipsis at its width, and a click on any cut
 * value opens it in full. A long object is marked as one to click, as a long
 * string always was; one the cell cuts short of 200 characters used to open
 * nothing at all.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
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
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: { id: "u-1" }, role: null, isLoading: false }),
}))

const mockFetch = authFetch as unknown as Mock
const CONN_ID = "11111111-1111-1111-1111-111111111111"

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
  connector_type: "bigquery",
  type: "destination",
  status: "active",
  is_connected: true,
  supports_explorer: true,
  explorer_mode: "sql",
  sql_dialect: "bigquery",
  config: { project_id: "p" },
}

// A value as BigQuery returns a JSON column: an object, long once printed.
const PAYLOAD = { merchant: { name: "Corner Store", category: "grocery", city: "Austin" }, items: Array(30).fill("sku-0001") }
const SHORT = "card_present"
const NOTE = "x".repeat(260)

function serve() {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) return res(200, { connection_id: CONN_ID, table_count: 0, tables: [], foreign_keys: [] })
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    if (url.includes("/explorer/query"))
      return res(200, {
        columns: ["payload", "channel", "note"],
        rows: [{ payload: PAYLOAD, channel: SHORT, note: NOTE }],
        row_count: 1,
        execution_time_ms: 812,
      })
    return res(200, {})
  })
}

async function runQuery() {
  localStorage.setItem(
    `rsync_explorer_tabs_v1:u-1:w-1`,
    JSON.stringify({ active: "t1", tabs: [{ id: "t1", name: "Payments", sql: "select * from t", connectionId: CONN_ID }] }),
  )
  const user = userEvent.setup()
  render(<ExplorerPage />)
  await user.click(await screen.findByRole("button", { name: /Run Query/i }))
  const grid = await screen.findByTestId("results-grid-scroller")
  return { user, cells: within(grid).getAllByRole("cell") }
}

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
  serve()
})

describe("a result cell stays in its column", () => {
  it("cuts every value to one line at the cell's width", async () => {
    const { cells } = await runQuery()
    expect(cells).toHaveLength(3)
    for (const cell of cells) {
      expect(cell).toHaveClass("truncate")
      expect(cell.className).toMatch(/max-w-\[/)
    }
  })

  it("marks a long object value as one to click, and opens it in full", async () => {
    expect(JSON.stringify(PAYLOAD).length).toBeGreaterThan(200)
    const { user, cells } = await runQuery()
    expect(cells[0]).toHaveAttribute("title", "Click to view full value")
    await user.click(cells[0])
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("textbox")).toHaveValue(JSON.stringify(PAYLOAD))
  })

  it("opens a short value the cell's width cut off", async () => {
    const { user, cells } = await runQuery()
    // jsdom lays nothing out: say the cell is narrower than its text.
    Object.defineProperty(cells[1], "scrollWidth", { configurable: true, value: 300 })
    Object.defineProperty(cells[1], "clientWidth", { configurable: true, value: 120 })
    await user.click(cells[1])
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByRole("textbox")).toHaveValue(SHORT)
  })

  it("leaves a value that fits where it is", async () => {
    const { user, cells } = await runQuery()
    await user.click(cells[1])
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull())
  })
})
