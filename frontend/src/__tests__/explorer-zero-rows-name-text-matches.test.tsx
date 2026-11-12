/**
 * A QUERY THAT MATCHED NOTHING NAMES ITS TEXT MATCHES.
 *
 * Found in the 2026-10-11 Ask rsync.ai eval: the SQL writer sees names and
 * types, never values, and wrote status = 'DECLINED' for a stored 'declined'.
 * The query ran, matched nothing, and the Explorer said only "Loosen your
 * WHERE clause". It now names each text match in the SQL that ran, and says a
 * text value matches only as stored, case included.
 *
 * Bug class: a 0-row result that does not say which guessed value to check.
 */

import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, within } from "@testing-library/react"
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


function serve(rows: Record<string, unknown>[]) {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) return res(200, { connection_id: CONN_ID, table_count: 0, tables: [], foreign_keys: [] })
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    if (url.includes("/explorer/query"))
      return res(200, { columns: ["merchant_name", "declined"], rows, row_count: rows.length, execution_time_ms: 40 })
    return res(200, {})
  })
}

async function run(sql: string) {
  localStorage.setItem(
    `rsync_explorer_tabs_v1:u-1:w-1`,
    JSON.stringify({ active: "t1", tabs: [{ id: "t1", name: "Declines", sql, connectionId: CONN_ID }] }),
  )
  const user = userEvent.setup()
  render(<ExplorerPage />)
  await user.click(await screen.findByRole("button", { name: /Run Query/i }))
  await screen.findByText(/no rows matched/i)
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
})

const GUESSED =
  "select m.merchant_name, count(*) as declined from fintech_silver.transactions t " +
  "join fintech_silver.merchants m on m.merchant_id = t.merchant_id " +
  "where t.status = 'DECLINED' and t.txn_type = 'card' group by 1 order by 2 desc limit 10"

describe("a query that matched nothing", () => {
  it("names each text match in the SQL that ran", async () => {
    serve([])
    await run(GUESSED)
    const hint = screen.getByTestId("zero-row-text-filters")
    expect(within(hint).getByText("t.status = 'DECLINED'")).toBeInTheDocument()
    expect(within(hint).getByText("t.txn_type = 'card'")).toBeInTheDocument()
    expect(hint).toHaveTextContent(/stored exactly as written, case included/)
  })

  it("keeps the general advice when it made no text match", async () => {
    serve([])
    await run("select merchant_name, count(*) as declined from t where amount > 100 group by 1")
    expect(screen.queryByTestId("zero-row-text-filters")).toBeNull()
    expect(screen.getByText(/Loosen your WHERE clause/)).toBeInTheDocument()
  })
})
