import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { DashboardHistoryDialog, savedHow } from "@/components/dashboards/DashboardHistoryDialog"
import { authFetch } from "@/lib/api/auth-fetch"
import type { Dashboard, DashboardVersion } from "@/lib/api/dashboards"
import { API_ENDPOINTS } from "@/lib/config/api"

// A dashboard's History (dashboards-design.md § 7.7). The server keeps a version
// before every save and refuses a restore over a newer save. What is pinned here
// is what only the dialog can get wrong:
//   - it lists the versions, newest first, and says who saved and how;
//   - Restore asks first, then is one PUT with the version's name, filters and
//     tiles, the dashboard's visibility and updated_at as they are now, and
//     restored_from;
//   - a refused restore over a newer save says so and reloads;
//   - a person who cannot manage gets no Restore.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock
const DASH = "dddddddd-0000-4000-8000-0000000000a1"
const UPDATED_AT = "2026-10-06T12:00:00.123456Z"

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

const dashboard: Dashboard = {
  id: DASH,
  name: "Sales",
  description: "",
  filters: [],
  tiles: [{ id: "t1", kind: "chart", chart_id: "c1", x: 0, y: 0, w: 12, h: 7, chart: { name: "Orders by day" } as never }],
  tile_count: 1,
  visibility: "workspace",
  proposed_by: "",
  mine: true,
  created_at: "2026-10-01T10:00:00Z",
  updated_at: UPDATED_AT,
}

function version(n: number, overrides: Partial<DashboardVersion> = {}): DashboardVersion {
  return {
    version: n,
    name: "Sales",
    description: "",
    filters: [],
    visibility: "private",
    tiles: [{ kind: "chart", chart_id: "c1", x: 0, y: 0, w: 6, h: 7 }],
    saved_by: "pat@example.com",
    how: "edit",
    created_at: "2026-10-05T09:00:00Z",
    ...overrides,
  }
}

const VERSIONS = [
  version(2, {
    name: "Sales by region",
    how: "ask",
    filters: [{ id: "region", kind: "values", label: "Region", column: "region" }],
    tiles: [
      { kind: "chart", chart_id: "c1", x: 0, y: 0, w: 6, h: 7 },
      { kind: "chart", chart_id: "gone", x: 6, y: 0, w: 6, h: 7 },
      { kind: "text", body: "Notes", x: 0, y: 7, w: 12, h: 2 },
    ],
  }),
  version(1),
]

function route(opts: { lists?: Response[]; put?: Response; canManage?: boolean } = {}) {
  const lists = [...(opts.lists ?? [])]
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET"
    if (url === API_ENDPOINTS.DASHBOARDS.VERSIONS(DASH) && method === "GET") {
      return Promise.resolve(
        lists.shift() ??
          json(200, {
            versions: VERSIONS,
            count: VERSIONS.length,
            current: { name: "Sales", visibility: "workspace", updated_at: UPDATED_AT },
            can_manage: opts.canManage ?? true,
          }),
      )
    }
    if (url === API_ENDPOINTS.DASHBOARDS.ITEM(DASH) && method === "PUT") {
      return Promise.resolve(opts.put ?? json(200, { dashboard: { ...dashboard, name: "Sales by region" } }))
    }
    return Promise.resolve(json(500, { error: `unexpected ${method} ${url}` }))
  })
}

const puts = () => mockFetch.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === "PUT")
const lists = () => mockFetch.mock.calls.filter(([, init]) => !(init as RequestInit | undefined)?.method)

function open(props: { onRestored?: (d: Dashboard) => void; onOpenChange?: (o: boolean) => void } = {}) {
  render(
    <DashboardHistoryDialog
      dashboard={dashboard}
      open
      onOpenChange={props.onOpenChange ?? vi.fn()}
      onRestored={props.onRestored ?? vi.fn()}
    />,
  )
}

beforeEach(() => {
  mockFetch.mockReset()
})

describe("savedHow", () => {
  it("says how each save came about", () => {
    expect(savedHow({ how: "edit" })).toBe("saved an edit")
    expect(savedHow({ how: "ask" })).toBe("applied a change from Ask rsync.ai")
    expect(savedHow({ how: "restore", restored_from: 3 })).toBe("restored version 3")
  })
})

describe("DashboardHistoryDialog", () => {
  it("lists the versions newest first and shows the newest", async () => {
    route()
    open()
    const list = await screen.findByRole("list", { name: "Versions" })
    const buttons = within(list).getAllByRole("button")
    expect(buttons.map((b) => b.textContent)).toEqual([
      expect.stringContaining("Version 2Before an Ask rsync.ai change by pat@example.com"),
      expect.stringContaining("Version 1Before an edit by pat@example.com"),
    ])
    expect(buttons[0]).toHaveAttribute("aria-pressed", "true")
    const shown = screen.getByRole("region", { name: "Version 2" })
    expect(shown).toHaveTextContent("before pat@example.com applied a change from Ask rsync.ai")
    expect(shown).toHaveTextContent("Sales by region (now Sales)")
    expect(shown).toHaveTextContent("Region")
    const tiles = within(shown).getByRole("list", { name: "Tiles in this version" })
    expect(within(tiles).getAllByRole("listitem").map((li) => li.firstChild?.textContent)).toEqual([
      "Orders by day",
      "A chart that is not on the dashboard now",
      "Text",
    ])
    expect(puts()).toHaveLength(0)
  })

  it("restores after a confirm, with one PUT that keeps the visibility and names the version", async () => {
    route()
    const onRestored = vi.fn()
    const onOpenChange = vi.fn()
    open({ onRestored, onOpenChange })
    fireEvent.click(await screen.findByRole("button", { name: "Restore this version" }))
    expect(puts()).toHaveLength(0)
    const confirm = screen.getByRole("group", { name: "Confirm restore" })
    expect(confirm).toHaveTextContent("The dashboard as it is now is kept as the next version")
    fireEvent.click(within(confirm).getByRole("button", { name: "Restore version 2" }))
    await waitFor(() => expect(onRestored).toHaveBeenCalledTimes(1))
    expect(puts()).toHaveLength(1)
    expect(JSON.parse((puts()[0][1] as RequestInit).body as string)).toEqual({
      name: "Sales by region",
      description: "",
      filters: VERSIONS[0].filters,
      tiles: VERSIONS[0].tiles,
      visibility: "workspace",
      updated_at: UPDATED_AT,
      restored_from: 2,
    })
    expect(onRestored.mock.calls[0][0]).toMatchObject({ id: DASH, name: "Sales by region" })
    expect(onOpenChange).toHaveBeenCalledWith(false)
  })

  it("restores the version the person picked, never one confirmed before they picked it", async () => {
    route()
    open()
    const list = await screen.findByRole("list", { name: "Versions" })
    fireEvent.click(screen.getByRole("button", { name: "Restore this version" }))
    fireEvent.click(within(list).getAllByRole("button")[1])
    expect(screen.queryByRole("group", { name: "Confirm restore" })).toBeNull()
    expect(screen.getByRole("region", { name: "Version 1" })).toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "Restore this version" }))
    fireEvent.click(screen.getByRole("button", { name: "Restore version 1" }))
    await waitFor(() => expect(puts()).toHaveLength(1))
    expect(JSON.parse((puts()[0][1] as RequestInit).body as string)).toMatchObject({ name: "Sales", restored_from: 1 })
  })

  it("says a restore over a newer save restored nothing, and reloads", async () => {
    route({ put: json(409, { error: "The dashboard was saved since.", code: "stale" }) })
    const onRestored = vi.fn()
    open({ onRestored })
    fireEvent.click(await screen.findByRole("button", { name: "Restore this version" }))
    fireEvent.click(screen.getByRole("button", { name: "Restore version 2" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("Someone saved this dashboard since the History was loaded")
    await waitFor(() => expect(lists()).toHaveLength(2))
    expect(onRestored).not.toHaveBeenCalled()
  })

  it("gives a person who cannot manage no Restore", async () => {
    route({ canManage: false })
    open()
    expect(await screen.findByText("Only workspace owners and admins can restore a version.")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /Restore/ })).toBeNull()
  })

  it("says a dashboard never saved since it was made has no versions", async () => {
    route({ lists: [json(200, { versions: [], count: 0, current: null, can_manage: true })] })
    open()
    expect(await screen.findByText(/has not been saved since it was made/)).toBeInTheDocument()
  })
})
