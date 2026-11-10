import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import ScheduledQueriesPage from "@/app/(dashboard)/models/page"
import { LayerBadge, layerFilterFor, layerPosition, matchesLayer } from "@/components/explorer/modelLayer"
import { authFetch } from "@/lib/api/auth-fetch"

// The workspace's own layers (migration 135) on the Models list: a badge beside
// the name and a filter. What must hold is that the filter offers the workspace's
// layers in their order, shows exactly the rows of the chosen layer, "No layer"
// means a model in none, and a layer the list does not have still shows and filters
// by its name rather than vanishing.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/models",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: "admin", can: () => true }),
  useWorkspace: () => ({ activeWorkspace: { id: "ws-1" } }),
}))
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: { id: "u-1" } }),
}))
vi.mock("@/components/explorer/SavedQueryEditDialog", () => ({ SavedQueryEditDialog: () => null }))
vi.mock("@/components/explorer/SavedQueryModelDialog", () => ({ SavedQueryModelDialog: () => null }))

const mockFetch = authFetch as unknown as Mock

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response
}

function schedule(id: string, name: string, layer?: string) {
  return {
    schedule_id: `sch-${id}`,
    saved_query_id: id,
    name,
    connection_id: "c-1",
    schedule_type: "cron",
    schedule_spec: { cron: "0 2 * * *", timezone: "UTC" },
    status: "active",
    materialization: "table",
    target_table: `analytics.${name}`,
    statement_class: "select",
    supports_materialization: true,
    created_by: "u-1",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    ...(layer === undefined ? {} : { layer }),
  }
}

let schedules: unknown[]
let layersResponse: () => Response

const WORKSPACE_LAYERS = [
  { id: "l-g", name: "Gold", description: "", position: 1, model_count: 1 },
  { id: "l-s", name: "Silver", description: "", position: 0, model_count: 1 },
  { id: "l-m", name: "Marts", description: "", position: 2, model_count: 0 },
]

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  schedules = [
    schedule("m-s", "orders_clean", "Silver"),
    schedule("m-g", "daily_revenue", "Gold"),
    schedule("m-n", "scratch_rollup"),
    // Its layer was renamed after this list was read: it still shows by name.
    schedule("m-x", "legacy_model", "Archive"),
  ]
  // Served out of order: the client orders by position.
  layersResponse = () => res(200, { layers: WORKSPACE_LAYERS, can_manage: true })
  mockFetch.mockImplementation((url: string) => {
    if (url === "/api/v1/explorer/schedules") return Promise.resolve(res(200, { schedules }))
    if (url.endsWith("/api/v1/model-layers")) return Promise.resolve(layersResponse())
    if (url === "/api/v1/explorer/running") {
      return Promise.resolve(res(200, { models: [], count: 0, limit: 50, temporal_available: true }))
    }
    if (url === "/api/v1/explorer/freshness") return Promise.resolve(res(200, { breaches: [], count: 0 }))
    if (url.endsWith("/runs")) return Promise.resolve(res(200, { runs: [] }))
    return Promise.reject(new Error(`unexpected request ${url}`))
  })
})

function names(): string[] {
  return ["orders_clean", "daily_revenue", "scratch_rollup", "legacy_model"].filter((n) => screen.queryByText(n))
}

async function pick(user: ReturnType<typeof userEvent.setup>, option: string) {
  await user.click(screen.getByRole("combobox", { name: "Layer" }))
  await user.click(await screen.findByRole("option", { name: option }))
}

describe("Models — layer", () => {
  it("badges each row with its own layer, coloured by the layer's place in the order", async () => {
    render(<ScheduledQueriesPage />)
    const silver = (await screen.findByText("orders_clean")).closest("tr")!
    expect(within(silver).getByText("Silver")).toHaveAttribute("data-layer", "Silver")
    const gold = within(screen.getByText("daily_revenue").closest("tr")!).getByText("Gold")
    // Gold is second in the workspace's order, so it keeps gold's colour.
    await vi.waitFor(() => expect(gold.className).toContain("amber"))
    expect(within(silver).getByText("Silver").className).toContain("zinc-100")
    // A layer the list does not have still names itself, in a neutral colour.
    const archive = within(screen.getByText("legacy_model").closest("tr")!).getByText("Archive")
    expect(archive).toHaveAttribute("data-layer", "Archive")
    expect(archive.className).not.toContain("amber")
    expect(screen.getByText("scratch_rollup").closest("tr")!.querySelector("[data-layer]")).toBeNull()
  })

  it("offers the workspace's layers in their order, then a layer only a model names", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ScheduledQueriesPage />)
    await screen.findByText("orders_clean")
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledWith(expect.stringMatching(/\/api\/v1\/model-layers$/)))
    await user.click(screen.getByRole("combobox", { name: "Layer" }))
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent)
    expect(options).toEqual(["All layers", "Silver", "Gold", "Marts", "Archive", "No layer"])
  })

  it("filters to one layer, to models with none, and back to all", async () => {
    const user = userEvent.setup({ delay: null })
    render(<ScheduledQueriesPage />)
    await screen.findByText("orders_clean")
    await screen.findByText("Silver")
    expect(names()).toEqual(["orders_clean", "daily_revenue", "scratch_rollup", "legacy_model"])

    await pick(user, "Gold")
    expect(names()).toEqual(["daily_revenue"])
    await pick(user, "Silver")
    expect(names()).toEqual(["orders_clean"])
    await pick(user, "Archive")
    expect(names()).toEqual(["legacy_model"])
    await pick(user, "No layer")
    expect(names()).toEqual(["scratch_rollup"])
    await pick(user, "All layers")
    expect(names()).toHaveLength(4)
  })

  it("says so when no row is in the chosen layer", async () => {
    schedules = [schedule("m-n", "scratch_rollup")]
    const user = userEvent.setup({ delay: null })
    render(<ScheduledQueriesPage />)
    await screen.findByText("scratch_rollup")
    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledWith(expect.stringMatching(/\/api\/v1\/model-layers$/)))
    await pick(user, "Marts")
    const empty = screen.getByText("No model is in this layer.").closest("td")!
    // The note spans the whole table: one column more and it would stop short.
    expect(empty.colSpan).toBe(screen.getAllByRole("columnheader").length)
    expect(screen.queryByText("scratch_rollup")).toBeNull()
  })

  it("still names and filters each model's layer when the layer list fails to load", async () => {
    layersResponse = () => res(500, { error: "failed to list layers" })
    const user = userEvent.setup({ delay: null })
    render(<ScheduledQueriesPage />)
    await screen.findByText("orders_clean")
    expect(within(screen.getByText("orders_clean").closest("tr")!).getByText("Silver")).toBeInTheDocument()
    await pick(user, "Gold")
    expect(names()).toEqual(["daily_revenue"])
  })
})

describe("modelLayer", () => {
  it("finds a layer's place by its name, ignoring case", () => {
    expect(layerPosition(WORKSPACE_LAYERS, "gold")).toBe(1)
    expect(layerPosition(WORKSPACE_LAYERS, " Marts ")).toBe(2)
    expect(layerPosition(WORKSPACE_LAYERS, "Archive")).toBeUndefined()
    expect(layerPosition(null, "Gold")).toBeUndefined()
    expect(layerPosition(WORKSPACE_LAYERS, "")).toBeUndefined()
  })

  it("matches a filter by layer name, ignoring case", () => {
    expect(matchesLayer("Gold", layerFilterFor("gold"))).toBe(true)
    expect(matchesLayer("Gold", layerFilterFor("Silver"))).toBe(false)
    expect(matchesLayer("", "none")).toBe(true)
    expect(matchesLayer(undefined, "none")).toBe(true)
    expect(matchesLayer("Gold", "none")).toBe(false)
    expect(matchesLayer(undefined, "all")).toBe(true)
  })

  it("never reads a layer named none as no layer", () => {
    expect(matchesLayer("none", "none")).toBe(false)
    expect(matchesLayer("none", layerFilterFor("none"))).toBe(true)
    expect(matchesLayer("", layerFilterFor("none"))).toBe(false)
  })

  it("renders no badge for no layer", () => {
    const { container } = render(<LayerBadge layer="" />)
    expect(container).toBeEmptyDOMElement()
  })
})
