import { Suspense } from "react"
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ConnectionDetailPage from "@/app/(dashboard)/connections/[id]/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// ---------------------------------------------------------------------------
// Launch review, PR5: the connection page says what uses the connection.
//
// Which pipelines use a connection only showed up in the error a refused
// delete printed, and the page was a narrow column of settings. These render
// the real page against the gateway's list and usage shapes and read what a
// user sees: the pipelines at either end, linked; four stat cards counted from
// those pipelines only; and the two-column layout.
// ---------------------------------------------------------------------------

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
// One router object for every render, as Next gives: the page's load effect
// depends on `router`, so a fresh object per render would refetch forever.
const router = vi.hoisted(() => ({ push: vi.fn(), refresh: vi.fn(), replace: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => router,
  useSearchParams: () => new URLSearchParams(),
  notFound: vi.fn(),
}))
vi.mock("@/components/connectors/ConnectionLogo", () => ({
  ConnectionLogo: () => <span data-testid="logo" />,
}))
vi.mock("@/components/connectors/GenericConnectorForm", () => ({
  GenericConnectorForm: () => <div data-testid="form" />,
}))
vi.mock("@/components/oauth/OAuthConnectButton", () => ({
  OAuthConnectButton: () => <button type="button">Connect</button>,
}))

const mockFetch = authFetch as unknown as Mock

const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString()

const SOURCE = {
  id: "conn-1",
  name: "orders-db",
  type: "source",
  connector_type: "postgresql",
  sync_mode: "cdc",
  cdc_mode: "initial",
  config: { host: "pg.internal" },
  status: "active",
  last_test_status: "success",
  last_tested_at: ago(120),
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-01T00:00:00Z",
}

const DESTINATION = {
  ...SOURCE,
  id: "conn-9",
  name: "warehouse",
  type: "destination",
  connector_type: "bigquery",
  sync_mode: undefined,
  cdc_mode: undefined,
}

const end = (id: string, name: string) => ({ id, name, connector_type: "x" })

const PIPELINES = [
  { id: "p1", name: "orders_sync", source_connection: end("conn-1", "orders-db"), destination_connection: end("conn-9", "warehouse"), derived_status: "passed" },
  { id: "p2", name: "customers_cdc", source_connection: end("conn-1", "orders-db"), destination_connection: end("conn-7", "lake"), derived_status: "running" },
  { id: "p3", name: "payments_backfill", source_connection: end("conn-1", "orders-db"), destination_connection: end("conn-9", "warehouse"), derived_status: "paused" },
  // Another source into the same warehouse: not the source's, but the warehouse's.
  { id: "p4", name: "crm_copy", source_connection: end("conn-2", "crm"), destination_connection: end("conn-9", "warehouse"), derived_status: "failed" },
]

const usageRow = (id: string, rows_read: number, rows_written: number, last_activity: string | null) => ({
  id,
  name: id,
  status: "passed",
  rows_read,
  rows_written,
  records_processed: 0,
  cdc_inserts: 0,
  cdc_updates: 0,
  cdc_deletes: 0,
  cdc_applied: 0,
  last_activity,
})

const USAGE = {
  retention_enabled: false,
  retention_days: 0,
  pipelines: [
    usageRow("p1", 1000, 900, ago(300)),
    usageRow("p2", 2000, 1900, ago(12)),
    usageRow("p3", 500, 400, null),
    usageRow("p4", 99_999, 70_000, ago(1)),
  ],
}

function res(body: unknown, status = 200) {
  return { ok: status < 400, status, headers: { get: () => null }, json: async () => body } as unknown as Response
}

const gw = { pipelinesFail: false, usageFail: false, hasNext: false, pipelines: PIPELINES as unknown[], usage: USAGE as unknown }

function serve(conn: { id: string; name: string }) {
  mockFetch.mockImplementation(async (url: string) => {
    if (url.startsWith("/api/v1/pipelines?")) {
      return gw.pipelinesFail ? res({}, 500) : res({ pipelines: gw.pipelines, pagination: { has_next: gw.hasNext } })
    }
    if (url === "/api/v1/usage") return gw.usageFail ? res({}, 500) : res(gw.usage)
    if (url === API_ENDPOINTS.CONNECTIONS.GET(conn.id)) return res(conn)
    return res({})
  })
}

async function open(conn: { id: string; name: string } = SOURCE) {
  serve(conn)
  await act(async () => {
    render(
      <Suspense fallback={<div>loading</div>}>
        <ConnectionDetailPage params={Promise.resolve({ id: conn.id })} />
      </Suspense>,
    )
  })
  await screen.findByRole("heading", { level: 1, name: conn.name })
}

/** A stat card's number and its line under it. */
function stat(title: string) {
  const card = screen.getByRole("region", { name: title })
  const [value, ...rest] = Array.from(card.querySelectorAll("p"))
  return { card, value: value?.textContent ?? "", hint: rest.at(-1)?.textContent ?? "" }
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})
beforeEach(() => {
  mockFetch.mockReset()
  Object.assign(gw, { pipelinesFail: false, usageFail: false, hasNext: false, pipelines: PIPELINES, usage: USAGE })
})
afterEach(() => cleanup())

describe("connection page — pipelines using this connection", () => {
  it("lists every pipeline that reads from it, each linked, in the list's status words", async () => {
    await open()
    const list = await screen.findByRole("list", { name: "Pipelines using this connection" })
    const links = within(list).getAllByRole("link")
    expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
      ["orders_sync", "/pipelines/p1"],
      ["customers_cdc", "/pipelines/p2"],
      ["payments_backfill", "/pipelines/p3"],
    ])
    expect(within(list).queryByText("crm_copy")).toBeNull()
    const rows = within(list).getAllByRole("listitem")
    expect(within(rows[0]).getByText("orders-db → warehouse")).toBeTruthy()
    expect(rows.map((r) => r.lastElementChild?.textContent)).toEqual(["Succeeded", "Running", "Paused"])
  })

  it("on a destination, lists the pipelines that write to it", async () => {
    await open(DESTINATION)
    const list = await screen.findByRole("list", { name: "Pipelines using this connection" })
    expect(within(list).getAllByRole("link").map((a) => a.textContent)).toEqual(["orders_sync", "payments_backfill", "crm_copy"])
  })

  it("says when nothing uses it", async () => {
    gw.pipelines = [PIPELINES[3]]
    await open()
    expect(await screen.findByText("No pipeline uses this connection yet")).toBeTruthy()
    await waitFor(() => expect(stat("Pipelines").value).toBe("0"))
    expect(stat("Pipelines").hint).toBe("None yet")
    await waitFor(() => expect(stat("Last activity").value).toBe("Never"))
  })

  it("says the count is partial when the pipeline list stopped at its cap", async () => {
    gw.hasNext = true
    await open()
    expect(await screen.findByText("Counted from the first 5,000 pipelines.")).toBeTruthy()
    expect(stat("Pipelines").value).toBe("3+")
  })

  it("says the list did not load rather than showing none, and Try again reloads it", async () => {
    gw.pipelinesFail = true
    await open()
    expect(await screen.findByText("The pipeline list did not load.")).toBeTruthy()
    expect(screen.queryByText("No pipeline uses this connection yet")).toBeNull()
    await waitFor(() => expect(stat("Pipelines").value).toBe("—"))
    expect(stat("Pipelines").hint).toBe("The pipeline list did not load")

    gw.pipelinesFail = false
    await userEvent.click(screen.getByRole("button", { name: "Try again" }))
    const list = await screen.findByRole("list", { name: "Pipelines using this connection" })
    expect(within(list).getAllByRole("link")).toHaveLength(3)
  })
})

describe("connection page — stat cards", () => {
  it("status with its last test, pipelines by status, rows read over its own pipelines, last activity", async () => {
    await open()
    expect(stat("Status").value).toBe("Connected")
    expect(stat("Status").hint).toBe("Last test: Succeeded · 2h ago")

    await waitFor(() => expect(stat("Rows read").value).toBe("3,500"))
    expect(stat("Rows read").hint).toBe("From this source, to date")

    const pipelines = stat("Pipelines")
    expect(pipelines.value).toBe("3")
    expect(within(pipelines.card).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "1 succeeded",
      "1 running",
      "1 paused",
    ])

    // p2 ran 12 minutes ago; p4 ran a minute ago but does not use this connection.
    expect(stat("Last activity").value).toBe("12m ago")
  })

  it("a destination counts rows written by the pipelines that write to it", async () => {
    await open(DESTINATION)
    await waitFor(() => expect(stat("Rows written").value).toBe("71,300"))
    expect(stat("Rows written").hint).toBe("To this destination, to date")
    expect(screen.queryByRole("region", { name: "Rows read" })).toBeNull()
    // A failing pipeline at its end is counted in red.
    const failed = within(stat("Pipelines").card).getByText("1 failed").closest("li") as Element
    expect(failed.className).toContain("text-red-700")
    // A destination has no sync mode of its own.
    expect(screen.queryByRole("heading", { name: "Sync Mode" })).toBeNull()
  })

  it("names the window when run-stat retention is on", async () => {
    gw.usage = { ...USAGE, retention_enabled: true, retention_days: 30 }
    await open()
    await waitFor(() => expect(stat("Rows read").hint).toBe("From this source, last 30 days"))
  })

  it("an unreadable usage shows — with the reason, never 0", async () => {
    gw.usageFail = true
    await open()
    await waitFor(() => expect(stat("Rows read").value).toBe("—"))
    expect(stat("Rows read").hint).toBe("Usage did not load")
    expect(stat("Last activity").value).toBe("—")
    expect(stat("Pipelines").value).toBe("3")
  })
})

describe("connection page — layout", () => {
  it("uses the full width: what uses it on the left, its details on the right", async () => {
    await open()
    const list = await screen.findByRole("list", { name: "Pipelines using this connection" })
    const details = screen.getByRole("heading", { level: 2, name: "Details" })
    const grid = Array.from(document.querySelectorAll("div")).find((d) =>
      d.className.split(/\s+/).includes("@5xl:grid-cols-[3fr_2fr]"),
    ) as HTMLElement
    expect(grid).toBeTruthy()
    const [left, right] = Array.from(grid.children)
    expect(left.contains(list)).toBe(true)
    expect(right.contains(details)).toBe(true)
    expect(document.querySelector(".max-w-4xl")).toBeNull()
  })
})
