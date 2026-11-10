import { Suspense } from "react"
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import "@testing-library/jest-dom"

import ConnectionsPage from "@/app/(dashboard)/connections/page"
import ConnectionDetailPage from "@/app/(dashboard)/connections/[id]/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// ---------------------------------------------------------------------------
// app.rsync.ai, 2026-09-28: a connection's ⋯ → Edit opened the detail page and
// stopped there. The menu item linked to the same URL as View Details, and the
// detail page only opened its edit form from its own button. These follow the
// real link from the list into the real detail page.
//
// The second half guards the layout of the same two pages. On an 800px window
// the list's View Details button was pushed off the card, and the detail
// header squeezed the title onto two lines. jsdom does no layout, so these
// check the flex properties that let a row give way. A class check is the
// cheapest guard CI can run for it; the PR carries the browser measurements.
// ---------------------------------------------------------------------------

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
// One router object for every render, as Next gives: the detail page's load
// effect depends on `router`, so a fresh object per render would refetch forever.
const nav = vi.hoisted(() => ({
  params: new URLSearchParams(),
  router: { push: vi.fn(), refresh: vi.fn(), replace: vi.fn() },
}))
vi.mock("next/navigation", () => ({
  useRouter: () => nav.router,
  useSearchParams: () => nav.params,
  usePathname: () => "/connections",
  notFound: vi.fn(),
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
vi.mock("@/components/connectors/ConnectionLogo", () => ({
  ConnectionLogo: () => <span data-testid="logo" />,
}))
vi.mock("@/components/connectors/GenericConnectorForm", () => ({
  GenericConnectorForm: () => <div data-testid="edit-form" />,
}))
vi.mock("@/components/oauth/OAuthConnectButton", () => ({
  OAuthConnectButton: () => <button type="button">Connect</button>,
}))

const mockFetch = authFetch as unknown as Mock

const CONN = {
  id: "conn-1",
  name: "Orders Postgres",
  description: "",
  type: "source",
  connector_type: "postgresql",
  sync_mode: "cdc",
  cdc_mode: "initial",
  config: {},
  status: "active",
  is_connected: true,
  last_test_status: "success",
  last_tested_at: "2026-09-27T00:00:00Z",
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
}

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, headers: { get: () => null }, json: async () => body } as unknown as Response
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})
beforeEach(() => {
  vi.clearAllMocks()
  nav.params = new URLSearchParams()
  mockFetch.mockImplementation(async (url: string) => {
    if (url === API_ENDPOINTS.OAUTH.TOKENS) return res({ tokens: [] })
    if (url === API_ENDPOINTS.CONNECTORS.GET("postgresql")) return res({ name: "postgresql" })
    if (url === API_ENDPOINTS.CONNECTIONS.LIST) return res({ connections: [CONN] })
    if (url === API_ENDPOINTS.CONNECTIONS.GET("conn-1")) return res(CONN)
    return res({})
  })
})
afterEach(() => cleanup())

/** Renders the detail page as the browser would at `href`. */
async function openDetail(href: string) {
  const url = new URL(href, "http://app.test")
  nav.params = url.searchParams
  const id = url.pathname.split("/").pop() as string
  await act(async () => {
    render(
      <Suspense fallback={<div>loading</div>}>
        <ConnectionDetailPage params={Promise.resolve({ id })} />
      </Suspense>,
    )
  })
  await screen.findByRole("heading", { name: "Orders Postgres" })
}

/** The href of the list's ⋯ → Edit item for Orders Postgres. */
async function listEditHref() {
  const user = userEvent.setup()
  render(<ConnectionsPage />)
  await user.click(await screen.findByRole("button", { name: "Actions for Orders Postgres" }))
  const edit = await screen.findByRole("menuitem", { name: "Edit" })
  const href = edit.getAttribute("href")
  expect(href).toBeTruthy()
  cleanup()
  return href as string
}

const editDialog = () => screen.queryByRole("dialog", { name: /Edit Orders Postgres/ })

describe("connections list ⋯ → Edit", () => {
  it("opens the connection's edit form, not just its detail page", async () => {
    await openDetail(await listEditHref())

    const dialog = editDialog()
    expect(dialog).not.toBeNull()
    expect(within(dialog as HTMLElement).getByTestId("edit-form")).toBeInTheDocument()
  })

  it("drops the edit hint from the address bar, so a refresh or Back does not reopen the form", async () => {
    await openDetail("/connections/conn-1?type=source&edit=1")

    expect(editDialog()).not.toBeNull()
    expect(nav.router.replace).toHaveBeenCalledWith("/connections/conn-1?type=source")
  })

  it("control: the name link opens the page with the form closed, and its Edit button still opens it", async () => {
    render(<ConnectionsPage />)
    const details = await screen.findByRole("link", { name: "Orders Postgres" })
    const href = details.getAttribute("href") as string
    cleanup()

    await openDetail(href)
    expect(editDialog()).toBeNull()
    expect(nav.router.replace).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole("button", { name: "Edit Connection" }))
    expect(editDialog()).not.toBeNull()
  })
})

describe("connection detail page — narrow screens", () => {
  it("folds Test, Edit and Delete into one menu, and its Edit opens the form", async () => {
    const user = userEvent.setup()
    await openDetail("/connections/conn-1?type=source")

    await user.click(screen.getByRole("button", { name: "More actions for Orders Postgres" }))
    const items = (await screen.findAllByRole("menuitem")).map((i) => i.textContent?.trim())
    expect(items).toEqual(["Test Connection", "Edit Connection", "Delete"])

    await user.click(screen.getByRole("menuitem", { name: "Edit Connection" }))
    expect(editDialog()).not.toBeNull()
  })
})

// ---------------------------------------------------------------------------
// Layout. A flex item will not shrink below its content unless it says so
// (`min-w-0`, or an overflow like `truncate`); a flex row never wraps unless it
// says so (`flex-wrap`). The View Details bug was a row with neither.
// ---------------------------------------------------------------------------

const cls = (el: Element | null) => (el?.getAttribute("class") ?? "").split(/\s+/)
const canShrink = (el: Element | null) => cls(el).some((c) => c === "min-w-0" || c === "truncate")

/**
 * Every flex item between `from` and `row` (exclusive) must be allowed to
 * shrink, or the widest of them sets a minimum width for the whole row.
 */
function assertShrinksUpTo(from: Element, row: Element) {
  let el: Element | null = from
  while (el && el !== row) {
    const parentIsFlex = cls(el.parentElement).includes("flex")
    if (parentIsFlex) expect(canShrink(el), `${el.tagName}.${cls(el).join(".")} can shrink`).toBe(true)
    el = el.parentElement
  }
  expect(el).toBe(row)
}

// The list is a table now (launch review, PR5). The same bug in a table is a
// name cell that will not give way and a row whose ⋯ scrolls out of reach.
describe("connections list — a row keeps its actions in reach", () => {
  it("the name gives way, the table scrolls inside its card, and the actions stay pinned right", async () => {
    render(<ConnectionsPage />)
    const name = await screen.findByRole("link", { name: "Orders Postgres" })
    const actions = screen.getByRole("button", { name: "Actions for Orders Postgres" }).closest("td") as Element

    expect(cls(name)).toContain("truncate")
    expect(name.getAttribute("title")).toBe("Orders Postgres")
    assertShrinksUpTo(name, name.closest("td") as Element)
    expect(cls(actions)).toEqual(expect.arrayContaining(["sticky", "right-0"]))
    expect(cls(screen.getByRole("table", { name: "Connections" }).parentElement)).toContain("overflow-x-auto")
  })
})

describe("connection detail page — header", () => {
  it("keeps the title on its own line: badges sit under it, and the buttons wrap", async () => {
    await openDetail("/connections/conn-1?type=source")
    const title = screen.getByRole("heading", { level: 1, name: "Orders Postgres" })
    const test = screen.getByRole("button", { name: "Test Connection" })

    let row = test.parentElement as Element
    while (!row.contains(title)) row = row.parentElement as Element

    expect(cls(title)).toContain("truncate")
    expect(title.getAttribute("title")).toBe("Orders Postgres")
    // No badge shares the title's line; the badges get a wrapping row of their own.
    const badges = screen.getByText("Real-time CDC").parentElement as Element
    expect(badges.contains(title)).toBe(false)
    expect(cls(badges)).toContain("flex-wrap")
    assertShrinksUpTo(title, row)
    expect(cls(row)).toContain("flex-wrap")
    // A wrapping row only wraps an item whose basis does not fit. `flex-1` has a
    // zero basis, so on an 800px window the row never wrapped and the buttons
    // squeezed the title to 0px. The title group asks for a width of its own.
    const group = Array.from(row.children).find((c) => c.contains(title)) as Element
    expect(cls(group)).not.toContain("flex-1")
    expect(cls(group).some((c) => c.startsWith("basis-"))).toBe(true)
  })
})
