import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ConnectionsPage from "@/app/(dashboard)/connections/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// Launch review, PR5: the Connections list is a table, one row per connection
// (each was a tall card, so ten took three screens). It says which pipelines
// use each connection, counts sources and destinations on its chips, and
// puts a pill on the page for a failing test or an expired token.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/connections",
}))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: "admin", isLoading: false, error: false, activeWorkspace: null, can: () => true, meets: () => true }),
}))
vi.mock("@/components/connectors/ConnectionLogo", () => ({ ConnectionLogo: () => <span data-testid="logo" /> }))
vi.mock("@/components/oauth/OAuthConnectButton", () => ({
  OAuthConnectButton: ({ label }: { label: string }) => <button type="button">{label}</button>,
}))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const mockFetch = authFetch as unknown as Mock
const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString()

const conn = (id: string, over: Record<string, unknown> = {}) => ({
  id,
  name: id,
  description: "",
  type: "source",
  connector_type: "postgresql",
  is_connected: true,
  last_test_status: "success",
  last_tested_at: hoursAgo(2),
  config: {},
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
  ...over,
})

const CONNECTIONS = [
  conn("orders-db", { sync_mode: "cdc", cdc_mode: "initial" }),
  conn("events-mongo", { connector_type: "mongodb", sync_mode: "cdc", cdc_mode: "streaming_only", is_connected: false, last_test_status: "failed", last_test_error: "auth failed" }),
  conn("stripe", { connector_type: "stripe", sync_mode: "batch", is_connected: false, last_test_status: "success", is_expired: true, token_expires_at: hoursAgo(1), last_tested_at: hoursAgo(5) }),
  conn("warehouse", { type: "destination", connector_type: "bigquery" }),
]

const pipe = (id: string, source: string, dest: string) => ({
  id,
  name: id,
  source_connection: { id: source },
  destination_connection: { id: dest },
})

const PIPELINES = [
  pipe("p1", "orders-db", "warehouse"),
  pipe("p2", "orders-db", "warehouse"),
  pipe("p3", "events-mongo", "warehouse"),
  // One connection at both ends counts once.
  pipe("p4", "orders-db", "orders-db"),
]

function serve({ pipelines = PIPELINES, hasNext = false, pipelinesFail = false, connections = CONNECTIONS } = {}) {
  mockFetch.mockImplementation(async (url: string) => {
    const ok = (body: unknown, status = 200) => ({ ok: status < 400, status, headers: { get: () => null }, json: async () => body })
    if (url === API_ENDPOINTS.OAUTH.TOKENS) return ok({ tokens: [] })
    if (url === API_ENDPOINTS.CONNECTIONS.LIST) return ok({ connections })
    if (String(url).startsWith("/api/v1/pipelines?")) {
      if (pipelinesFail) return ok({}, 500)
      // A server that keeps saying there is more, with nothing new, ends the paging as truncated.
      return ok({ pipelines, pagination: { has_next: hasNext } })
    }
    return ok({})
  })
}

const row = (name: string) => screen.getByRole("link", { name }).closest("tr") as HTMLElement

beforeEach(() => {
  vi.clearAllMocks()
})

describe("Connections table", () => {
  it("shows one row per connection with its role, mode, last test and the pipelines that use it", async () => {
    serve()
    render(<ConnectionsPage />)
    const table = await screen.findByRole("table", { name: "Connections" })
    expect(within(table).getAllByRole("row")).toHaveLength(1 + CONNECTIONS.length)

    expect(await within(row("orders-db")).findByText("3 pipelines")).toBeInTheDocument()
    expect(within(row("orders-db")).getByText("Source")).toBeInTheDocument()
    expect(within(row("orders-db")).getByText("CDC")).toBeInTheDocument()
    expect(within(row("orders-db")).getByText(/^Connected · 2h ago/)).toBeInTheDocument()

    expect(within(row("events-mongo")).getByText("1 pipeline")).toBeInTheDocument()
    expect(within(row("events-mongo")).getByText("CDC, new changes only")).toBeInTheDocument()
    expect(within(row("events-mongo")).getByText("Test failed")).toBeInTheDocument()

    expect(within(row("stripe")).getByText("No pipelines")).toBeInTheDocument()
    expect(within(row("stripe")).getByText("Batch")).toBeInTheDocument()
    expect(within(row("stripe")).getByText("Token expired")).toBeInTheDocument()

    expect(within(row("warehouse")).getByText("Destination")).toBeInTheDocument()
    expect(within(row("warehouse")).getByText("3 pipelines")).toBeInTheDocument()
    expect(within(row("warehouse")).getByText("—")).toBeInTheDocument()
  })

  it("marks a count a floor when the pipeline list stopped at its cap", async () => {
    serve({ hasNext: true })
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    const usedBy = await within(row("orders-db")).findByText("3+ pipelines")
    expect(usedBy).toHaveAttribute("title", "Counted from the first 5,000 pipelines")
  })

  it("shows a dash, not zero, when the pipeline list did not load", async () => {
    serve({ pipelinesFail: true })
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    expect(await within(row("orders-db")).findByTitle("The pipeline list did not load")).toHaveTextContent("—")
    expect(screen.queryByText("No pipelines")).toBeNull()
  })

  it("shows a dash, not the last workspace's counts, until a switched workspace's pipelines load", async () => {
    serve()
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    expect(await within(row("orders-db")).findByText("3 pipelines")).toBeInTheDocument()

    // The new workspace's pipeline list has not answered yet.
    const pending = new Promise(() => {})
    const served = mockFetch.getMockImplementation() as (url: string) => Promise<unknown>
    mockFetch.mockImplementation((url: string) => (String(url).startsWith("/api/v1/pipelines?") ? pending : served(url)))
    act(() => setActiveWorkspaceId("ws-other"))

    await screen.findByRole("table", { name: "Connections" })
    expect(within(row("orders-db")).getByText("—")).toBeInTheDocument()
    expect(screen.queryByText("3 pipelines")).toBeNull()
    expect(screen.queryByText("No pipelines")).toBeNull()
    act(() => setActiveWorkspaceId(null))
  })
})

describe("Connections filters and problems", () => {
  it("counts each role on its chip and filters by it", async () => {
    serve()
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    const chips = screen.getByRole("group", { name: "Connection role" })
    expect(within(chips).getAllByRole("button").map((b) => b.textContent)).toEqual(["All4", "Sources3", "Destinations1"])

    await userEvent.click(within(chips).getByRole("button", { name: /Destinations/ }))
    expect(screen.getByRole("link", { name: "warehouse" })).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "orders-db" })).toBeNull()
  })

  it("puts a pill on the page for each kind of problem", async () => {
    serve()
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    expect(screen.getByText("1 test failing")).toBeInTheDocument()
    expect(screen.getByText("1 needs reconnect")).toBeInTheDocument()
  })

  it("shows no problem pill when every test passes", async () => {
    serve({ connections: [CONNECTIONS[0], CONNECTIONS[3]] })
    render(<ConnectionsPage />)
    await screen.findByRole("table", { name: "Connections" })
    expect(screen.queryByText(/failing/)).toBeNull()
    expect(screen.queryByText(/reconnect/)).toBeNull()
  })

  it("says so when a search matches nothing", async () => {
    serve()
    render(<ConnectionsPage />)
    await userEvent.type(await screen.findByLabelText("Search connections"), "zzz")
    expect(screen.getByText("No connection matches")).toBeInTheDocument()
    expect(screen.queryByRole("table", { name: "Connections" })).toBeNull()
  })
})

describe("Connections load and empty", () => {
  it("shows grey rows while the list loads, then an empty state with the next step", async () => {
    let answer: () => void = () => {}
    mockFetch.mockImplementation((url: string) =>
      url === API_ENDPOINTS.CONNECTIONS.LIST
        ? new Promise((resolve) => {
            answer = () => resolve({ ok: true, status: 200, json: async () => ({ connections: [] }) })
          })
        : Promise.resolve({ ok: true, status: 200, json: async () => ({}) }),
    )
    render(<ConnectionsPage />)
    expect(screen.getByText("Loading connections…")).toBeInTheDocument()

    answer()
    expect(await screen.findByText("No connections yet")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: /Browse connectors/ })).toHaveAttribute("href", "/connectors")
  })

  // "All 0" beside grey rows or an error reads as an empty workspace.
  const roleChips = () =>
    within(screen.getByRole("group", { name: "Connection role" }))
      .getAllByRole("button")
      .map((b) => b.textContent)

  it("leaves the counts off the role chips until the list loads", async () => {
    serve()
    const served = mockFetch.getMockImplementation() as (url: string) => Promise<unknown>
    let answer: () => void = () => {}
    mockFetch.mockImplementation((url: string) =>
      url === API_ENDPOINTS.CONNECTIONS.LIST
        ? new Promise((resolve) => {
            answer = () => resolve(served(url))
          })
        : served(url),
    )
    render(<ConnectionsPage />)
    expect(screen.getByText("Loading connections…")).toBeInTheDocument()
    expect(roleChips()).toEqual(["All", "Sources", "Destinations"])

    await act(async () => answer())
    await screen.findByRole("table", { name: "Connections" })
    expect(roleChips()).toEqual(["All4", "Sources3", "Destinations1"])
  })

  it("leaves the counts off the role chips when the list did not load", async () => {
    mockFetch.mockImplementation(async (url: string) => {
      const failed = url === API_ENDPOINTS.CONNECTIONS.LIST
      return { ok: !failed, status: failed ? 500 : 200, headers: { get: () => null }, json: async () => ({}) }
    })
    render(<ConnectionsPage />)
    expect(await screen.findByText("Could not load connections")).toBeInTheDocument()
    expect(roleChips()).toEqual(["All", "Sources", "Destinations"])
    expect(screen.queryByText("No connections yet")).toBeNull()
  })
})
