/**
 * THE EXPLORER BEFORE A QUERY HAS RUN (launch review, PR4).
 *
 * The result pane shows a "Start here" panel instead of an icon and two lines:
 * the connection's ten largest tables as bars, from the row counts the schema
 * index already read, a click on one putting a query on it in the editor (never
 * running it, and never over SQL already typed); and the last queries run on
 * the connection in this browser, a click opening one. No counts, no bars.
 * Pinned beside it: the toolbar says "Explorer", Run is the shared button, and
 * the shortcut hint names Ctrl off a Mac (bug 15) and ⌘ on one.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import ExplorerPage from "@/app/(dashboard)/explorer/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"
import { HISTORY_KEY_PREFIX } from "@/lib/explorer/queryTabs"
import type { ChartProps } from "@/components/charts/chart-settings"

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
// ECharts draws nothing in jsdom: the stub names each bar, in order, and clicks it as ECharts would.
vi.mock("@/components/charts/EChartNoSSR", () => ({
  EChartNoSSR: ({ settings, onCategoryClick }: ChartProps) => (
    <div data-testid="echart" aria-label={settings.description}>
      {("x" in settings ? settings.x.categories : []).map((c, i) => (
        <button key={c} type="button" data-testid="bar" onClick={() => onCategoryClick?.(i)}>
          {c}
        </button>
      ))}
    </div>
  ),
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
  connector_type: "postgresql",
  type: "source",
  status: "active",
  is_connected: true,
  supports_explorer: true,
  explorer_mode: "sql",
  sql_dialect: "postgresql",
  config: { database: "analytics" },
}

// Twelve counted tables, one counted 0, one with no count; `orders` in two schemas.
const COUNTED = [
  ["shop", "orders", 9_000],
  ["archive", "orders", 8_000],
  ["shop", "payments", 12_000],
  ["shop", "customers", 700],
  ["shop", "t1", 600],
  ["shop", "t2", 500],
  ["shop", "t3", 400],
  ["shop", "t4", 300],
  ["shop", "t5", 200],
  ["shop", "t6", 100],
  ["shop", "t7", 50],
  ["shop", "t8", 10],
] as const
const TABLES = [
  ...COUNTED.map(([schema, name, row_count]) => ({ schema, name, row_count })),
  { schema: "shop", name: "empty", row_count: 0 },
  { schema: "shop", name: "uncounted" },
]

function serve(tables: object[] = TABLES) {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.includes("/schema-index")) return res(200, { connection_id: CONN_ID, tables, foreign_keys: [] })
    if (/\/api\/v1\/connections$/.test(url)) return res(200, { connections: [CONNECTION] })
    return res(200, {})
  })
}

const editorText = () => document.querySelector(".cm-content")?.textContent ?? ""
const tabs = () => screen.getAllByTestId("query-tab")

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

afterEach(() => vi.restoreAllMocks())

describe("Start here: the largest tables", () => {
  it("draws the ten largest counted tables, most rows first, naming a shared name by its schema", async () => {
    serve()
    render(<ExplorerPage />)
    const bars = await screen.findAllByTestId("bar")
    expect(bars.map((b) => b.textContent)).toEqual([
      "payments",
      "shop.orders",
      "archive.orders",
      "customers",
      "t1",
      "t2",
      "t3",
      "t4",
      "t5",
      "t6",
    ])
    expect(screen.getByRole("heading", { name: "Start here: largest tables" })).toBeInTheDocument()
  })

  it("puts a query on a clicked table in the empty editor, without running it", async () => {
    serve()
    const user = userEvent.setup()
    render(<ExplorerPage />)
    await user.click((await screen.findAllByTestId("bar"))[1])
    await waitFor(() => expect(editorText()).toBe("SELECT * FROM shop.orders LIMIT 100"))
    expect(tabs()).toHaveLength(1)
    expect(mockFetch.mock.calls.some(([url]) => String(url).includes("/explorer/query"))).toBe(false)
  })

  it("opens a new tab for it when this one already has SQL, so nothing typed is lost", async () => {
    serve()
    localStorage.setItem(
      "rsync_explorer_tabs_v1:u-1:w-1",
      JSON.stringify({ active: "t1", tabs: [{ id: "t1", name: "Mine", sql: "select 1", connectionId: CONN_ID }] }),
    )
    const user = userEvent.setup()
    render(<ExplorerPage />)
    await user.click((await screen.findAllByTestId("bar"))[0])
    await waitFor(() => expect(tabs()).toHaveLength(2))
    expect(editorText()).toBe("SELECT * FROM shop.payments LIMIT 100")
    await user.click(within(tabs()[0]).getAllByRole("button")[0])
    await waitFor(() => expect(editorText()).toBe("select 1"))
  })

  it("is a keyboard's too: each bar is a button, shown when it has focus", async () => {
    serve()
    const user = userEvent.setup()
    render(<ExplorerPage />)
    const list = await screen.findByRole("list", { name: "Query a table" })
    const first = within(list).getAllByRole("button")[0]
    expect(first).toHaveTextContent("Query shop.payments")
    expect(first.className.split(/\s+/)).toEqual(expect.arrayContaining(["sr-only", "focus:not-sr-only"]))
    await user.click(first)
    await waitFor(() => expect(editorText()).toBe("SELECT * FROM shop.payments LIMIT 100"))
  })

  it("draws no bars when the schema index counted no rows", async () => {
    serve([{ schema: "shop", name: "orders" }, { schema: "shop", name: "empty", row_count: 0 }])
    render(<ExplorerPage />)
    expect(await screen.findByRole("heading", { name: "Recent queries" })).toBeInTheDocument()
    expect(screen.queryByTestId("echart")).toBeNull()
    expect(screen.queryByRole("heading", { name: "Start here: largest tables" })).toBeNull()
  })
})

describe("Start here: recent queries", () => {
  it("lists the last five run on this connection, newest first, and a click opens one", async () => {
    serve()
    const now = Date.now()
    localStorage.setItem(
      `${HISTORY_KEY_PREFIX}:${CONN_ID}`,
      JSON.stringify(
        Array.from({ length: 7 }, (_, i) => ({
          id: `h${i}`,
          sql: `select ${i}`,
          timestamp: new Date(now - i * 60_000).toISOString(),
          rowCount: i === 0 ? 1 : 1200,
        })),
      ),
    )
    const user = userEvent.setup()
    render(<ExplorerPage />)
    const section = (await screen.findByRole("heading", { name: "Recent queries" })).closest("section") as HTMLElement
    const items = within(section).getAllByRole("button")
    expect(items.map((b) => b.querySelector(".font-mono")?.textContent)).toEqual(["select 0", "select 1", "select 2", "select 3", "select 4"])
    expect(items[0]).toHaveTextContent(/^select 01 row · /)
    expect(items[1]).toHaveTextContent(/^select 11,200 rows · /)

    await user.click(items[2])
    await waitFor(() => expect(editorText()).toBe("select 2"))
  })

  it("opens one in a new tab when this one already has SQL, so nothing typed is lost", async () => {
    serve()
    localStorage.setItem(
      "rsync_explorer_tabs_v1:u-1:w-1",
      JSON.stringify({ active: "t1", tabs: [{ id: "t1", name: "Mine", sql: "select 1", connectionId: CONN_ID }] }),
    )
    localStorage.setItem(
      `${HISTORY_KEY_PREFIX}:${CONN_ID}`,
      JSON.stringify([{ id: "h0", sql: "select 42", timestamp: new Date().toISOString() }]),
    )
    const user = userEvent.setup()
    render(<ExplorerPage />)
    const section = (await screen.findByRole("heading", { name: "Recent queries" })).closest("section") as HTMLElement
    await user.click(within(section).getByRole("button"))
    await waitFor(() => expect(tabs()).toHaveLength(2))
    expect(editorText()).toBe("select 42")
    await user.click(within(tabs()[0]).getAllByRole("button")[0])
    await waitFor(() => expect(editorText()).toBe("select 1"))
  })

  it("says where they come from when there are none yet", async () => {
    serve()
    render(<ExplorerPage />)
    expect(await screen.findByText(/Queries you run on this connection show here\./)).toBeInTheDocument()
  })
})

describe("the toolbar and Run", () => {
  it("says Explorer, and Run is the shared button with no gradient of its own", async () => {
    serve()
    render(<ExplorerPage />)
    expect(await screen.findByRole("heading", { name: "Explorer", level: 1 })).toBeInTheDocument()
    const run = await screen.findByRole("button", { name: /Run Query/ })
    expect(run.className).not.toMatch(/gradient|emerald|teal/)
    expect(run.className.split(/\s+/)).toContain("bg-zinc-900")
  })

  it("names Ctrl in the shortcut hint off a Mac, and ⌘ on one (bug 15)", async () => {
    serve()
    const { unmount } = render(<ExplorerPage />)
    const hint = (await screen.findByText("to run")).parentElement as HTMLElement
    expect(hint).toHaveTextContent(/^Ctrl\+Enterto run/)
    unmount()

    vi.spyOn(navigator, "platform", "get").mockReturnValue("MacIntel")
    render(<ExplorerPage />)
    expect((await screen.findByText("to run")).parentElement).toHaveTextContent(/^⌘\+Enterto run/)
  })
})
