import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import { AssetLineageView, edgeLabel } from "@/components/explorer/AssetLineageView"
import {
  ASSET_NODE_HEIGHT,
  ASSET_NODE_WIDTH,
  assetHref,
  attentionSplit,
  describeAsset,
  displaysDownstream,
  drawnGraph,
  findWarehouseTable,
  foldLooseAssets,
  followsItsInputs,
  inputUpstreams,
  inputWakes,
  isDisplay,
  layoutAssets,
  modelSurroundings,
  needsAttention,
  neighbourhood,
  parseAssetGraph,
  refreshChip,
  searchAssets,
  stageColumns,
  tableProducers,
  upstreamPath,
  warehouseIds,
  warehouseLabel,
  wholeGraph,
  withoutDisplays,
  type AssetGraph,
  type ModelRefresh,
} from "@/components/explorer/assetLineage"
import { revealShift } from "@/components/explorer/LineageFlowCanvas"
import { MIN_READABLE_FIT_ZOOM } from "@/components/explorer/modelLineage"
import { authFetch } from "@/lib/api/auth-fetch"
import { listConnections } from "@/lib/api/connections"
import { getConnectorLogoUrl } from "@/lib/api/mcp-connectors"
import { formatRelativeTime } from "@/lib/utils"
import type { SavedModel } from "@/lib/hooks/useSavedModels"

// GET /api/v1/explorer/asset-graph (asset_graph.go) had no reader. The Lineage page draws it,
// says which models are refreshed out of step with what they read, and keeps the backend's
// three grades of evidence apart: an edge read out of SQL is not drawn like one seen in a run.

const media = vi.hoisted(() => ({ canvas: false }))

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/lib/api/connections", () => ({ listConnections: vi.fn() }))
vi.mock("next-themes", () => ({ useTheme: () => ({ resolvedTheme: "light" }) }))
// The workspace's layer list is its own request (useWorkspaceLayers): a knob here, so
// the fetch mocks below answer only the graph's requests, in their order.
const mockLayers: { value: { name: string; position: number }[] | null } = { value: null }
vi.mock("@/lib/hooks/useWorkspaceLayers", () => ({
  useWorkspaceLayers: () => ({ layers: mockLayers.value, canManage: false, error: null, reload: async () => {} }),
}))
// So are the workspace's saved queries (useSavedModels), each model's last run and schedule.
const mockSaved: { value: ReadonlyMap<string, SavedModel> } = { value: new Map() }
const mockReloadSaved = vi.fn()
vi.mock("@/lib/hooks/useSavedModels", () => ({
  useSavedModels: () => ({ saved: mockSaved.value, reload: mockReloadSaved }),
}))
// The Schedule dialog has its own tests (SavedQueryModelDialog.test.tsx): here, only what the page opens it with.
vi.mock("@/components/explorer/SavedQueryModelDialog", () => ({
  SavedQueryModelDialog: (props: {
    savedQueryId: string
    savedQueryName: string
    preset?: unknown
    onOpenChange: (open: boolean) => void
    onChanged?: () => void
  }) => (
    <div
      role="dialog"
      aria-label={`Schedule ${props.savedQueryName}`}
      data-saved-query={props.savedQueryId}
      data-preset={JSON.stringify(props.preset ?? null)}
    >
      <button type="button" onClick={() => props.onChanged?.()}>
        Save schedule
      </button>
      <button type="button" onClick={() => props.onOpenChange(false)}>
        Close schedule
      </button>
    </div>
  ),
}))

// The History section asks for its runs itself (lineage-history.test.tsx tests it): here, only
// which card the panel mounts it for, so the fetch mocks below answer the graph's requests.
vi.mock("@/components/explorer/LineageHistorySection", () => ({
  LineageHistorySection: (props: { node: { id: string }; reloadTick: number }) => (
    <div data-history-node={props.node.id} data-history-reload={props.reloadTick} />
  ),
}))

const mockFetch = authFetch as unknown as Mock
const mockConnections = listConnections as unknown as Mock
/** The lineage canvas on a laptop: 65% of a 920px-tall window. */
const CANVAS = { width: 1250, height: 600 }
const URL = "/api/v1/explorer/asset-graph"

beforeAll(() => {
  // What React Flow needs from a browser that jsdom does not have (as in model-lineage-graph.test.tsx).
  class ResizeObserverStub {
    constructor(private cb: ResizeObserverCallback) {}
    observe(target: Element) {
      const contentRect = { width: 800, height: 480 } as DOMRectReadOnly
      this.cb([{ target, contentRect } as ResizeObserverEntry], this as unknown as ResizeObserver)
    }
    unobserve() {}
    disconnect() {}
  }
  class DOMMatrixReadOnlyStub {
    m22: number
    constructor(transform?: string) {
      const scale = transform?.match(/scale\(([\d.]+)\)/)?.[1]
      this.m22 = scale !== undefined ? Number(scale) : 1
    }
  }
  Object.assign(globalThis, { ResizeObserver: ResizeObserverStub, DOMMatrixReadOnly: DOMMatrixReadOnlyStub })
  Object.defineProperties(HTMLElement.prototype, {
    offsetHeight: { configurable: true, get() { return parseFloat(this.style.height) || 1 } },
    offsetWidth: { configurable: true, get() { return parseFloat(this.style.width) || 1 } },
    // The canvas reads its own size to pick the first view: 65% of a laptop window's height.
    clientWidth: { configurable: true, get() { return this.hasAttribute("data-lineage-canvas") ? CANVAS.width : 0 } },
    clientHeight: { configurable: true, get() { return this.hasAttribute("data-lineage-canvas") ? CANVAS.height : 0 } },
  })
  ;(SVGElement.prototype as unknown as { getBBox: () => DOMRect }).getBBox = () =>
    ({ x: 0, y: 0, width: 0, height: 0 }) as DOMRect
  window.matchMedia = ((query: string) => ({
    matches: media.canvas,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
})

beforeEach(() => {
  mockFetch.mockReset()
  mockConnections.mockReset()
  mockConnections.mockResolvedValue({ connections: [], total: 0 })
  media.canvas = false
  mockLayers.value = null
  mockSaved.value = new Map()
  mockReloadSaved.mockReset()
})

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response
}

const translateOf = (transform: string) =>
  transform.match(/translate\((-?[\d.]+)px, ?(-?[\d.]+)px\)/)!.slice(1, 3).map(Number) as [number, number]

/** Where a card is on the canvas's screen, in pixels from the canvas's top left. */
function screenBox(container: HTMLElement, card: HTMLElement) {
  const view = container.querySelector<HTMLElement>(".react-flow__viewport")!.style.transform
  const [tx, ty] = translateOf(view)
  const zoom = Number(view.match(/scale\(([\d.]+)\)/)![1])
  const [x, y] = translateOf(card.style.transform)
  const [width, height] = [parseFloat(card.style.width), parseFloat(card.style.height)]
  return { left: tx + x * zoom, top: ty + y * zoom, right: tx + (x + width) * zoom, bottom: ty + (y + height) * zoom }
}

// orders_sync writes orders and customers and wakes weekly_rollup. daily_revenue reads both
// tables on a cron and materializes analytics.daily_revenue, which weekly_rollup reads. So
// weekly_rollup is woken by the pipeline but not by the model it actually reads.
const P1 = "pipeline:p-1"
const P2 = "pipeline:p-2"
const T_ORDERS = "table:c-1:public.orders"
const T_CUST = "table:c-1:public.customers"
const T_DAILY = "table:c-1:analytics.daily_revenue"
const T_EVENTS = "table:c-2:public.events"
const M1 = "model:m-1"
const M2 = "model:m-2"
const M3 = "model:m-3"

const BODY = {
  nodes: [
    { id: P1, kind: "pipeline", name: "orders_sync", ref_id: "p-1" },
    { id: P2, kind: "pipeline", name: "events_sync", ref_id: "p-2" },
    { id: T_ORDERS, kind: "table", name: "public.orders", connection_id: "c-1" },
    { id: T_CUST, kind: "table", name: "public.customers", connection_id: "c-1" },
    { id: T_DAILY, kind: "table", name: "analytics.daily_revenue", connection_id: "c-1" },
    { id: T_EVENTS, kind: "table", name: "public.events", connection_id: "c-2" },
    { id: M1, kind: "model", name: "daily_revenue", ref_id: "m-1" },
    { id: M2, kind: "model", name: "weekly_rollup", ref_id: "m-2" },
    { id: M3, kind: "model", name: "scratch_report", ref_id: "m-3" },
  ],
  edges: [
    { from: P1, to: T_ORDERS, kind: "writes", evidence: "observed" },
    { from: P1, to: T_CUST, kind: "writes", evidence: "observed" },
    { from: P1, to: M2, kind: "triggers", evidence: "declared" },
    { from: T_ORDERS, to: M1, kind: "reads", evidence: "inferred" },
    { from: T_CUST, to: M1, kind: "reads", evidence: "inferred" },
    { from: M1, to: T_DAILY, kind: "materializes", evidence: "declared" },
    { from: T_DAILY, to: M2, kind: "reads", evidence: "inferred" },
    { from: P2, to: T_EVENTS, kind: "writes", evidence: "observed" },
  ],
  models: [
    {
      model_id: "m-1",
      node_id: M1,
      name: "daily_revenue",
      trigger: "cron",
      trigger_upstreams: [],
      trigger_paused: false,
      upstreams: [P1],
      uncovered_upstreams: [P1],
      unresolved: ["public.refunds"],
      ambiguous: false,
    },
    {
      model_id: "m-2",
      node_id: M2,
      name: "weekly_rollup",
      trigger: "after_upstream",
      trigger_upstreams: [P1],
      trigger_paused: false,
      upstreams: [M1],
      uncovered_upstreams: [M1],
      unresolved: null,
      ambiguous: true,
    },
    {
      model_id: "m-3",
      node_id: M3,
      name: "scratch_report",
      trigger: "manual",
      trigger_upstreams: null,
      trigger_paused: false,
      upstreams: null,
      uncovered_upstreams: null,
      unresolved: null,
      ambiguous: false,
    },
  ],
  stats: {
    pipelines: 2,
    tables: 4,
    models: 3,
    edges: { writes: 3, triggers: 1, reads: 3, materializes: 1 },
    models_with_uncovered_upstreams: 2,
    models_with_unresolved_references: 1,
    models_with_multiple_upstreams: 0,
    truncated: { pipelines: false, produced_tables: false, models: false },
  },
}

const graph: AssetGraph = parseAssetGraph(BODY)

// The workspace in the bug report (25 assets, 19 links): four pipelines writing 9, 4, 1 and 2
// tables, two models whose SQL names a table nothing writes, and one chain
// pipeline → table → model → table → model. Drawn as-is it is a 1304 x 1756 column of tables.
const PA = "pipeline:p-a"
const PB = "pipeline:p-b"
const WIDE = (() => {
  const nodes: { id: string; kind: string; name: string; ref_id?: string; connection_id?: string }[] = []
  const edges: { from: string; to: string; kind: string; evidence: string }[] = []
  const pipeline = (key: string, tables: string[]) => {
    nodes.push({ id: `pipeline:p-${key}`, kind: "pipeline", name: `${key}_sync`, ref_id: `p-${key}`, connection_id: "wh-1" })
    for (const t of tables) {
      nodes.push({ id: `table:wh-1:${t}`, kind: "table", name: t, connection_id: "wh-1" })
      edges.push({ from: `pipeline:p-${key}`, to: `table:wh-1:${t}`, kind: "writes", evidence: "observed" })
    }
  }
  pipeline("a", Array.from({ length: 9 }, (_, i) => `raw.a${i}`))
  pipeline("b", Array.from({ length: 4 }, (_, i) => `raw.b${i}`))
  pipeline("c", ["raw.c0"])
  pipeline("d", ["raw.d0", "raw.d1"])
  for (const m of ["m-1", "m-2", "m-3", "m-4"]) nodes.push({ id: `model:${m}`, kind: "model", name: `model_${m}`, ref_id: m })
  nodes.push({ id: "table:wh-1:gold.g", kind: "table", name: "gold.g", connection_id: "wh-1" })
  edges.push(
    { from: "table:wh-1:raw.d1", to: "model:m-3", kind: "reads", evidence: "inferred" },
    { from: "model:m-3", to: "table:wh-1:gold.g", kind: "materializes", evidence: "declared" },
    { from: "table:wh-1:gold.g", to: "model:m-4", kind: "reads", evidence: "inferred" },
  )
  return { ...BODY, nodes, edges, models: [] }
})()

describe("assetLineage helpers", () => {
  it("lists each warehouse once, and names one the connection list does not know by a short id", () => {
    expect(warehouseIds(graph)).toEqual(["c-1", "c-2"])
    expect(warehouseIds(parseAssetGraph(null))).toEqual([])
    expect(warehouseLabel("c-1", new Map([["c-1", "analytics_wh"]]))).toBe("analytics_wh")
    expect(warehouseLabel("0f3c9a1e-77aa-4b1d-9c1e-2f7d6e5a4b3c", new Map())).toBe("Warehouse 0f3c9a1e")
  })

  it("says a table nothing drawn reads is unread only in its warehouse when the graph is narrowed to one", () => {
    const events = graph.nodes.find((n) => n.id === T_EVENTS)!
    expect(describeAsset(events, graph, undefined)).toBe("No model reads it")
    // A narrowed graph lacks the models in the other warehouses, and one of them may read it.
    expect(describeAsset(events, graph, undefined, true)).toBe("Nothing in this warehouse reads it")
    // What a narrowed graph does draw it can still say.
    expect(describeAsset(graph.nodes.find((n) => n.id === T_ORDERS)!, graph, undefined, true)).toBe("Read by 1 model")
  })

  it("reads a Go nil slice as an empty list, and a missing body as an empty graph", () => {
    const m3 = graph.models.find((m) => m.model_id === "m-3")!
    expect(m3.upstreams).toEqual([])
    expect(m3.trigger_upstreams).toEqual([])
    const empty = parseAssetGraph(null)
    expect(empty.nodes).toEqual([])
    expect(empty.stats.truncated).toEqual({
      pipelines: false,
      produced_tables: false,
      models: false,
      charts: false,
      dashboards: false,
    })
  })

  it("walks everything upstream and downstream of the focus, nearest first", () => {
    const v = neighbourhood(graph, M1)
    expect(v.nodeIds[0]).toBe(M1)
    expect(new Set(v.nodeIds)).toEqual(new Set([M1, T_ORDERS, T_CUST, P1, T_DAILY, M2]))
    expect(v.role.get(P1)).toBe("upstream")
    expect(v.distance.get(P1)).toBe(2)
    expect(v.role.get(M2)).toBe("downstream")
    expect(v.distance.get(M2)).toBe(2)
    expect([v.upstreamFound, v.downstreamFound, v.omitted]).toEqual([3, 2, 0])
    // An edge is drawn when both ends are: the pipeline waking weekly_rollup is, the other pipeline is not.
    expect(v.edges).toHaveLength(7)
    expect(v.edges.some((e) => e.from === P2)).toBe(false)
  })

  it("keeps the nearest when capped and counts the rest", () => {
    const v = neighbourhood(graph, M1, { cap: 3 })
    expect(v.nodeIds).toEqual([M1, T_DAILY, T_CUST])
    expect(v.omitted).toBe(3)
    expect([v.upstreamShown, v.downstreamShown]).toEqual([1, 1])
  })

  it("draws as many hops as asked, and says what waits beyond each edge card", () => {
    const one = neighbourhood(graph, M1, { depth: 1 })
    expect(new Set(one.nodeIds)).toEqual(new Set([M1, T_ORDERS, T_CUST, T_DAILY]))
    // The totals still count every hop: the header says "2 of 3 upstream".
    expect([one.upstreamShown, one.upstreamFound, one.downstreamShown, one.downstreamFound]).toEqual([2, 3, 1, 2])
    expect(one.omitted).toBe(0)
    // Both tables are written by orders_sync; weekly_rollup reads the model's table.
    expect(Object.fromEntries(one.hiddenBeyond)).toEqual({ [T_ORDERS]: 1, [T_CUST]: 1, [T_DAILY]: 1 })
    expect(one.edges).toHaveLength(3)

    const two = neighbourhood(graph, M1, { depth: 2 })
    expect(two.nodeIds).toHaveLength(6)
    expect(two.hiddenBeyond.size).toBe(0)
  })

  it("draws the next hop of an expanded asset only, whatever the depth", () => {
    const v = neighbourhood(graph, M1, { depth: 1, expanded: new Set([T_ORDERS]) })
    expect(new Set(v.nodeIds)).toEqual(new Set([M1, T_ORDERS, T_CUST, T_DAILY, P1]))
    expect(v.distance.get(P1)).toBe(2)
    // orders_sync is now drawn, so nothing waits behind customers either.
    expect(Object.fromEntries(v.hiddenBeyond)).toEqual({ [T_DAILY]: 1 })
    // An expanded asset that is not drawn opens nothing.
    expect(neighbourhood(graph, M1, { depth: 1, expanded: new Set([P1]) }).nodeIds).toHaveLength(4)
  })

  it("counts a node on a cycle once, as upstream", () => {
    const loop = parseAssetGraph({
      nodes: [
        { id: "a", kind: "model", name: "a" },
        { id: "b", kind: "table", name: "b" },
      ],
      edges: [
        { from: "a", to: "b", kind: "materializes", evidence: "declared" },
        { from: "b", to: "a", kind: "reads", evidence: "inferred" },
      ],
    })
    const v = neighbourhood(loop, "a")
    expect([v.upstreamFound, v.downstreamFound]).toEqual([1, 0])
    expect(v.nodeIds).toEqual(["a", "b"])
  })

  it("says in words why a model needs attention, and leaves a clean one out", () => {
    const items = needsAttention(graph)
    expect(items.map((i) => i.model.model_id)).toEqual(["m-1", "m-2"])
    expect(items[0].reasons).toEqual([
      "Reads from orders_sync, but runs on a clock rather than when it runs.",
      "Its SQL reads public.refunds, which no pipeline or model here writes, so where that table gets data is unknown.",
    ])
    expect(items[1].reasons).toEqual([
      "Reads from daily_revenue, but does not refresh when it runs.",
      "A table name in its SQL could be more than one table, so an upstream shown may be the wrong one.",
    ])
  })

  it("links pipelines and models to their pages, and tables nowhere", () => {
    expect(assetHref(graph.nodes.find((n) => n.id === P1)!)).toBe("/pipelines/p-1")
    expect(assetHref(graph.nodes.find((n) => n.id === M1)!)).toBe("/models/m-1?tab=graph")
    expect(assetHref(graph.nodes.find((n) => n.id === T_ORDERS)!)).toBeNull()
  })

  it("finds by name, prefix matches and models first", () => {
    expect(searchAssets(graph, "daily").map((n) => n.id)).toEqual([M1, T_DAILY])
    expect(searchAssets(graph, "  ")).toEqual([])
  })
})

describe("folding tables no model reads", () => {
  const wide = parseAssetGraph(WIDE)

  it("starts from a graph the size of the reported one: 25 assets, 19 links", () => {
    expect(wide.nodes).toHaveLength(25)
    expect(wide.edges).toHaveLength(19)
  })

  it("folds each pipeline's unread tables into one box, and leaves read ones and a lone one drawn", () => {
    const f = foldLooseAssets(wholeGraph(wide), wide)
    expect([...f.groups.values()].map((g) => [g.kind, g.from, g.members.length])).toEqual([
      ["table", PA, 9],
      ["table", PB, 4],
    ])
    expect(f.nodeIds).toHaveLength(25 - 13 + 2)
    // raw.d1 is read by a model; raw.d0 is its pipeline's only unread table, so a box would hide nothing.
    expect(f.nodeIds).toEqual(expect.arrayContaining(["table:wh-1:raw.d0", "table:wh-1:raw.d1", "table:wh-1:raw.c0"]))
    expect(f.nodeIds.some((id) => id.startsWith("table:wh-1:raw.a"))).toBe(false)
    // One write per box, and no edge left pointing at a folded table.
    const drawn = new Set(f.nodeIds)
    expect(f.edges.every((e) => drawn.has(e.from) && drawn.has(e.to))).toBe(true)
    const intoA = f.edges.filter((e) => e.from === PA)
    expect(intoA).toEqual([{ from: PA, to: f.groups.keys().next().value, kind: "writes", evidence: "observed" }])
  })

  it("draws a box open when asked, and never folds the tables of the focused pipeline", () => {
    const [groupA] = [...foldLooseAssets(wholeGraph(wide), wide).groups.keys()]
    const opened = foldLooseAssets(wholeGraph(wide), wide, new Set([groupA]))
    expect([...opened.groups.values()].map((g) => g.from)).toEqual([PB])
    expect(opened.nodeIds).toHaveLength(25 - 4 + 1)
    expect(foldLooseAssets(wholeGraph(wide), wide, "all").nodeIds).toHaveLength(25)
    expect(foldLooseAssets(neighbourhood(wide, PA), wide).groups.size).toBe(0)
  })

  it("lays the folded workspace out so it can be read whole, with unlinked assets in rows, not a column", async () => {
    const f = foldLooseAssets(wholeGraph(wide), wide)
    const { positions, bounds } = await layoutAssets(f.nodeIds, f.edges)
    expect(positions.size).toBe(f.nodeIds.length)
    const fit = Math.min((CANVAS.width - 48) / bounds.width, (CANVAS.height - 48) / bounds.height)
    expect(fit).toBeGreaterThanOrEqual(MIN_READABLE_FIT_ZOOM)
    // The two models linked to nothing sit side by side, under everything else.
    const m1 = positions.get("model:m-1")!
    const m2 = positions.get("model:m-2")!
    expect(m1.y).toBe(m2.y)
    for (const [id, p] of positions) if (!id.startsWith("model:m-1") && !id.startsWith("model:m-2")) expect(p.y).toBeLessThan(m1.y)
  })

  it("makes room under a table whose columns are open", async () => {
    const ids = ["p", "a", "b"]
    const links = [
      { from: "p", to: "a" },
      { from: "p", to: "b" },
    ]
    // A column list as tall as one with five rows.
    const panel = 167
    const tall = (open: string) => (id: string) => ({
      width: ASSET_NODE_WIDTH,
      height: ASSET_NODE_HEIGHT + (id === open ? panel : 0),
    })
    const shut = await layoutAssets(ids, links)
    const opened = await layoutAssets(ids, links, tall("a"))
    expect(opened.bounds.height - shut.bounds.height).toBe(panel)
    const [top, below] = [opened.positions.get("a")!, opened.positions.get("b")!].sort((x, y) => x.y - y.y)
    expect(below.y - top.y).toBeGreaterThanOrEqual(ASSET_NODE_HEIGHT + (top === opened.positions.get("a") ? panel : 0))
  })
})

describe("revealShift", () => {
  // A 1000px canvas; the card the reader clicked spans 400..656, the added one is given.
  const shift = (lo: number, hi: number, keep: [number, number] = [400, 656]) => revealShift(lo, hi, keep[0], keep[1], 1000)

  it("leaves the view alone when the added cards are on the canvas", () => {
    expect(shift(700, 956)).toBe(0)
    expect(shift(24, 280)).toBe(0)
  })

  it("pans only as far as brings them in, with a margin", () => {
    expect(shift(900, 1156)).toBe(-180)
    expect(shift(-300, -44, [400, 600])).toBe(324)
  })

  it("shows where a span wider than the canvas starts", () => {
    expect(shift(700, 2000, [700, 956])).toBe(-676)
  })

  it("stops where the clicked card would leave the canvas", () => {
    expect(shift(900, 1500)).toBe(-376)
    expect(shift(-600, -44, [400, 900])).toBe(76)
  })

  it("does not push a clicked card that is already at the edge any further", () => {
    expect(shift(900, 1156, [10, 266])).toBe(0)
    expect(shift(-300, -44, [800, 990])).toBe(0)
  })
})

describe("findWarehouseTable", () => {
  const tables = [
    { name: "orders", schema: "public", columns: [] },
    { name: "orders", schema: "staging", columns: [] },
    { name: "Customers", schema: "public", columns: [] },
  ]

  it("finds a schema-qualified name in its schema, whatever the case", () => {
    expect(findWarehouseTable("public.orders", tables)).toEqual({ found: "one", table: tables[0] })
    expect(findWarehouseTable("PUBLIC.customers", tables)).toEqual({ found: "one", table: tables[2] })
  })

  it("matches a bare name in any schema, and says when that is more than one", () => {
    expect(findWarehouseTable("customers", tables)).toEqual({ found: "one", table: tables[2] })
    expect(findWarehouseTable("orders", tables)).toEqual({ found: "many", count: 2 })
  })

  it("says when the warehouse does not list it", () => {
    expect(findWarehouseTable("analytics.orders", tables)).toEqual({ found: "none" })
    expect(findWarehouseTable("refunds", [])).toEqual({ found: "none" })
  })
})

describe("AssetLineageView", () => {
  it("summarises the workspace and lists the models that need attention", async () => {
    mockFetch.mockResolvedValue(res(200, BODY))
    render(<AssetLineageView reloadTick={0} />)

    const list = await screen.findByRole("list", { name: "Models that need attention" })
    expect(mockFetch).toHaveBeenCalledWith(URL, { method: "GET" })
    const rows = [...list.querySelectorAll("[data-attention]")].map((r) => r.getAttribute("data-attention"))
    expect(rows).toEqual(["m-1", "m-2"])
    expect(within(list).getByRole("link", { name: "daily_revenue" })).toHaveAttribute("href", "/models/m-1?tab=graph")
    expect(within(list).getByText("Runs on a cron schedule")).toBeInTheDocument()

    const summary = document.querySelector<HTMLElement>("[data-lineage-summary]")!
    expect(summary.querySelector("p")).toHaveTextContent(/^2 pipelines · 4 tables · 3 models$/)
    expect(within(summary).getByRole("link", { name: "2 aren't refreshed by what they read" })).toHaveAttribute(
      "href",
      "#lineage-attention",
    )
    // Both read tables something here writes: no "can't trace" callout.
    expect(within(summary).getAllByRole("link")).toHaveLength(1)
    expect(screen.getByRole("heading", { name: /Needs attention \(2\)/ })).toBeInTheDocument()
  })

  it("focuses an asset from the attention list and lists what feeds it and what it feeds", async () => {
    mockFetch.mockResolvedValue(res(200, BODY))
    render(<AssetLineageView reloadTick={0} />)

    // The whole workspace fits, so it is listed by kind before anything is picked.
    expect(await screen.findByText("Whole workspace")).toBeInTheDocument()
    expect(screen.getByText("Pipelines (2)")).toBeInTheDocument()

    await userEvent.click(screen.getByRole("button", { name: "Show daily_revenue in the graph" }))
    expect(screen.getByRole("heading", { name: /What feeds daily_revenue, and what it feeds/ })).toBeInTheDocument()

    // One hop each side to start, and the header says how much more there is.
    expect(screen.getByText("2 of 3 upstream · 1 downstream")).toBeInTheDocument()
    const section = (heading: RegExp) => screen.getByRole("heading", { name: heading }).closest("section")!
    const names = (s: HTMLElement) =>
      [...s.querySelectorAll("[data-asset-row]")].map((row) => row.querySelector(".font-mono")!.textContent)
    expect(names(section(/^Upstream \(2 of 3\)/))).toEqual(["public.customers", "public.orders"])
    // daily_revenue and the table it builds are one row, which says what it builds.
    expect(section(/^Focus/).querySelector("[data-asset-row]")).toHaveTextContent("Builds analytics.daily_revenue")
    expect(names(section(/^Downstream \(1\)/))).toEqual(["weekly_rollup"])
    // Picked from the list, it is also the one whose details are open, with its run history.
    expect(screen.getByRole("region", { name: "Details of daily_revenue" })).toBeInTheDocument()
    const history = screen.getByRole("region", { name: "Details of daily_revenue" }).querySelector("[data-history-node]")
    expect(history).toHaveAttribute("data-history-node", M1)
    expect(history).toHaveAttribute("data-history-reload", "0")

    // One asset's next hop opens on its own.
    await userEvent.click(screen.getByRole("button", { name: "Show 1 more upstream of public.orders" }))
    expect(screen.getByText("3 upstream · 1 downstream")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /more upstream of public\.customers/ })).toBeNull()

    // Every hop.
    const hops = screen.getByRole("group", { name: "Hops drawn on each side" })
    await userEvent.click(within(hops).getByRole("button", { name: "Every hop" }))
    expect(within(hops).getByRole("button", { name: "Every hop" })).toHaveAttribute("aria-pressed", "true")
    expect(screen.getByText("3 upstream · 1 downstream")).toBeInTheDocument()
    expect(names(section(/^Upstream \(3\)/))).toEqual(["public.customers", "public.orders", "orders_sync"])
    expect(names(section(/^Downstream \(1\)/))).toEqual(["weekly_rollup"])
    expect(within(section(/^Upstream/)).getByText("Upstream, 2 hops away")).toBeInTheDocument()
    expect(within(section(/^Upstream/)).getByRole("link", { name: "orders_sync" })).toHaveAttribute("href", "/pipelines/p-1")

    // Focus moves along the chain, and back out to the whole workspace.
    await userEvent.click(screen.getByRole("button", { name: "Show what feeds weekly_rollup and what it feeds" }))
    expect(screen.getByRole("heading", { name: /What feeds weekly_rollup/ })).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Show the whole workspace" }))
    expect(screen.getByText("Whole workspace")).toBeInTheDocument()
  })

  // A chart's lineage badge links /explorer/lineage?focus=model:<id> (chart_badges.go).
  it("opens focused on the asset it is given, and on the whole workspace for one the graph has not", async () => {
    mockFetch.mockResolvedValue(res(200, BODY))
    const { unmount } = render(<AssetLineageView reloadTick={0} initialFocus={M1} />)
    expect(await screen.findByRole("heading", { name: /What feeds daily_revenue, and what it feeds/ })).toBeInTheDocument()
    unmount()

    render(<AssetLineageView reloadTick={0} initialFocus="model:gone" />)
    expect(await screen.findByText("Whole workspace")).toBeInTheDocument()
  })

  // A model's layer (migration 135) rides on it and on the table it builds; a table a
  // pipeline writes is marked a pipeline table instead. A row with neither gets no
  // badge, so a pipeline is never labelled, and the List orders each section by the
  // workspace's own layer order, not by name.
  it("marks pipeline tables, badges each model's layer, and orders the list by the workspace's layers", async () => {
    mockLayers.value = [
      { name: "Silver", position: 0 },
      { name: "Gold", position: 1 },
    ]
    const layer: Record<string, string> = { [M1]: "Gold", [T_DAILY]: "Gold", [M2]: "Silver", [M3]: "Archive" }
    const written = new Set([T_ORDERS, T_CUST])
    mockFetch.mockResolvedValue(
      res(200, {
        ...BODY,
        nodes: BODY.nodes.map((n) => ({
          ...n,
          ...(layer[n.id] ? { layer: layer[n.id] } : {}),
          ...(written.has(n.id) ? { pipeline_table: true } : {}),
        })),
      })
    )
    render(<AssetLineageView reloadTick={0} />)

    const rows = (heading: RegExp) =>
      [...screen.getByRole("heading", { name: heading }).closest("section")!.querySelectorAll("[data-asset-row]")].map((row) => [
        row.querySelector(".font-mono")!.textContent,
        row.querySelector("[data-pipeline-table]") ? "pipeline table" : row.querySelector("[data-layer]")?.getAttribute("data-layer") ?? "",
      ])
    await screen.findByRole("heading", { name: /^Models \(/ })
    // Silver before Gold because the workspace orders them so; Archive is not on its
    // list, so it follows them; a table in no layer comes last.
    expect(rows(/^Models \(/)).toEqual([
      ["weekly_rollup", "Silver"],
      ["daily_revenue", "Gold"],
      ["scratch_report", "Archive"],
    ])
    expect(rows(/^Tables \(/)).toEqual([
      ["public.customers", "pipeline table"],
      ["public.orders", "pipeline table"],
      ["public.events", ""],
    ])
    expect(rows(/^Pipelines \(/)).toEqual([
      ["events_sync", ""],
      ["orders_sync", ""],
    ])

    await userEvent.click(await screen.findByRole("button", { name: "Show daily_revenue in the graph" }))
    expect(rows(/^Upstream/)).toEqual([
      ["public.customers", "pipeline table"],
      ["public.orders", "pipeline table"],
    ])
    expect(rows(/^Downstream/)).toEqual([["weekly_rollup", "Silver"]])
    await userEvent.click(within(screen.getByRole("group", { name: "Hops drawn on each side" })).getByRole("button", { name: "Every hop" }))
    expect(rows(/^Upstream/)).toContainEqual(["orders_sync", ""])
    expect(rows(/^Downstream/)).toContainEqual(["weekly_rollup", "Silver"])
  })

  it("opens a table's columns under its row in the list, one request per warehouse", async () => {
    const orders = [{ name: "order_id", type: "uuid", is_primary_key: true }]
    mockFetch.mockImplementation(async (url: string) =>
      url.includes("/schema-index")
        ? res(200, {
            tables: [
              { name: "orders", schema: "public", columns: orders },
              { name: "customers", schema: "public", columns: [{ name: "email", type: "text" }] },
            ],
          })
        : res(200, BODY),
    )
    render(<AssetLineageView reloadTick={0} />)
    await screen.findByText("Whole workspace")
    // Pipelines and models have no columns to show.
    expect(screen.getAllByRole("button", { name: /^Columns of / })).toHaveLength(4)

    const button = screen.getByRole("button", { name: "Columns of public.orders" })
    expect(button).toHaveAttribute("aria-expanded", "false")
    expect(button).not.toHaveAttribute("aria-controls")
    await userEvent.click(button)
    expect(button).toHaveAttribute("aria-expanded", "true")
    const panel = document.getElementById(button.getAttribute("aria-controls")!)!
    expect(await within(panel).findByRole("list", { name: "Columns of public.orders" })).toHaveTextContent("order_id")
    expect(panel.closest("[data-asset-row]")).toHaveTextContent("public.orders")

    await userEvent.click(screen.getByRole("button", { name: "Columns of public.customers" }))
    expect(await screen.findByRole("list", { name: "Columns of public.customers" })).toHaveTextContent("email")
    await userEvent.click(screen.getByRole("button", { name: "Columns of public.events" }))
    const schemaCalls = () => mockFetch.mock.calls.map(([u]) => String(u)).filter((u) => u.includes("/schema-index"))
    expect(schemaCalls()).toEqual([
      "/api/v1/explorer/connections/c-1/schema-index",
      "/api/v1/explorer/connections/c-2/schema-index",
    ])

    await userEvent.click(button)
    expect(button).toHaveAttribute("aria-expanded", "false")
    expect(screen.queryByRole("list", { name: "Columns of public.orders" })).toBeNull()
  })

  it("names an edge by what it does and how the backend knows it", () => {
    expect(edgeLabel({ kind: "writes", evidence: "observed" })).toBe("Writes · seen in a run")
    expect(edgeLabel({ kind: "materializes", evidence: "declared" })).toBe("Builds · configured")
    expect(edgeLabel({ kind: "reads", evidence: "inferred" })).toBe("Read by · read from a model's SQL")
    expect(edgeLabel({ kind: "triggers", evidence: "declared" })).toBe("Wakes when it finishes · configured")
  })

  it("draws each read by whether it refreshes its model, and each link by how the backend knows it on asking", async () => {
    media.canvas = true
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)

    // daily_revenue and the table it builds are one card, so its materialization is not a link.
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    expect(container.querySelectorAll(".react-flow__edge")).toHaveLength(7)
    const dashes = () =>
      [...container.querySelectorAll<SVGPathElement>(".react-flow__edge-path")].map((p) => p.style.strokeDasharray || "solid")
    // Refresh: 3 writes solid; 3 reads whose writer does not wake the model dashed; the wake dotted.
    expect(dashes().filter((d) => d === "solid")).toHaveLength(3)
    expect(dashes().filter((d) => d === "6 4")).toHaveLength(3)
    expect(dashes().filter((d) => d === "2 4")).toHaveLength(1)
    expect(screen.getByText("read, but doesn't refresh it")).toBeInTheDocument()
    expect(screen.getByText("wakes it when it finishes")).toBeInTheDocument()

    // Each edge names itself when the pointer rests on its middle.
    const canvas = container.querySelector(".react-flow")!.parentElement!
    expect(within(canvas).getAllByText("Read by · read from a model's SQL · does not refresh the model")).toHaveLength(3)
    expect(within(canvas).getByText("Wakes when it finishes · configured")).toBeInTheDocument()

    // Each picker is captioned with what it changes, and no option shares a name with another control:
    // the page has its own Refresh button, and each table card its own Columns button.
    const links = within(screen.getByRole("group", { name: "Colour links by" }))
    expect(links.getByRole("button", { name: "Freshness" })).toHaveAttribute("aria-pressed", "true")
    expect(links.queryByRole("button", { name: "Refresh" })).toBeNull()
    expect(screen.getByText("Colour links by:")).toBeInTheDocument()
    expect(screen.getByText("Arrange by:")).toBeInTheDocument()
    expect(screen.queryByRole("group", { name: "Columns" })).toBeNull()
    expect(screen.queryByRole("group", { name: "Links" })).toBeNull()

    // How we know: 3 writes seen in a run solid, the configured trigger dashed, 3 reads parsed out of SQL dotted.
    await userEvent.click(links.getByRole("button", { name: "How we know" }))
    await waitFor(() => expect(dashes().filter((d) => d === "2 4")).toHaveLength(3))
    expect(dashes().filter((d) => d === "solid")).toHaveLength(3)
    expect(dashes().filter((d) => d === "6 4")).toHaveLength(1)
    expect(screen.getByText("read from a model's SQL")).toBeInTheDocument()

    // A click on a card selects it: everything stays drawn, what is not upstream of it dimmed.
    fireEvent.click(within(canvas).getByTitle("daily_revenue"))
    await waitFor(() => expect(within(canvas).getByText("Selected")).toBeInTheDocument())
    expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8)
    const dimmed = [...container.querySelectorAll(".react-flow__node [data-dimmed]")].map(
      (c) => c.querySelector("[title]:not([title='Show its details'])")?.getAttribute("title") ?? c.textContent,
    )
    expect(dimmed).toHaveLength(4)
    for (const name of ["events_sync", "public.events", "weekly_rollup", "scratch_report"])
      expect(within(canvas).getByTitle(name).closest("[data-dimmed]")).not.toBeNull()
    for (const name of ["daily_revenue", "public.orders", "public.customers", "orders_sync"])
      expect(within(canvas).getByTitle(name).closest("[data-dimmed]")).toBeNull()
    const edgeOpacity = [...container.querySelectorAll<SVGPathElement>(".react-flow__edge-path")].map((p) => p.style.opacity)
    expect(edgeOpacity.filter((o) => o === "")).toHaveLength(4)
    expect(edgeOpacity.filter((o) => o !== "")).toHaveLength(3)
    expect(screen.getByRole("region", { name: "Details of daily_revenue" })).toBeInTheDocument()

    // Its neighbours only: one hop each side.
    await userEvent.click(screen.getByRole("button", { name: "Show only its neighbours" }))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(4))
    expect(container.querySelectorAll(".react-flow__edge")).toHaveLength(3)

    // +1 on a table it reads draws the pipeline that writes it, and nothing else.
    const more = within(canvas).getAllByTitle(/^Show 1 more (upstream|downstream)$/)
    expect(more.map((b) => b.title)).toEqual(["Show 1 more upstream", "Show 1 more upstream"])
    fireEvent.click(more[0])
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(5))
    expect(within(canvas).getByTitle("orders_sync")).toBeInTheDocument()
    // Asking for a hop is not asking to select the card it is on.
    expect(within(canvas).getByText("Selected").closest(".react-flow__node")).toHaveTextContent("daily_revenue")
  })

  it("puts a moved card back when a new layout lands, and keeps the card the reader acted on where they left it", async () => {
    media.canvas = true
    const tables = [{ name: "daily_revenue", schema: "analytics", columns: [{ name: "day", type: "date", is_primary_key: true }] }]

    // Focus the model, and open its table's columns: the same drawing twice, once untouched.
    async function open(moveFirst: boolean) {
      mockFetch.mockImplementation(async (url: string) =>
        url.includes("/schema-index") ? res(200, { tables, foreign_keys: [] }) : res(200, BODY),
      )
      const { container, unmount } = render(<AssetLineageView reloadTick={0} />)
      await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
      const canvas = container.querySelector(".react-flow")!.parentElement!
      fireEvent.click(within(canvas).getByTitle("daily_revenue"))
      fireEvent.click(await screen.findByRole("button", { name: "Show only its neighbours" }))
      await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(4))

      const card = (name: string) => within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!
      const flowAt = (text: string) => translateOf(card(text).style.transform)
      const screenAt = (text: string) => {
        const { left, top } = screenBox(container, card(text))
        return [left, top]
      }
      // A drag as d3-drag reads one: pressed on the card, moved and released on the window.
      // d3-drag follows the drag on event.view, which jsdom will not take in the init.
      const mouse = (target: Element | Window, type: string, x: number, y: number) => {
        const event = new MouseEvent(type, { bubbles: true, cancelable: true, button: 0, clientX: x, clientY: y })
        Object.defineProperty(event, "view", { value: window })
        fireEvent(target, event)
      }
      const drag = (text: string, dx: number, dy: number) => {
        mouse(card(text), "mousedown", 600, 300)
        mouse(window, "mousemove", 600 + dx / 2, 300 + dy / 2)
        mouse(window, "mousemove", 600 + dx, 300 + dy)
        mouse(window, "mouseup", 600 + dx, 300 + dy)
      }

      let tableBefore: number[] | null = null
      if (moveFirst) {
        await userEvent.click(screen.getByRole("button", { name: "Unlock to move cards" }))
        const ordersFrom = flowAt("public.orders")
        drag("public.orders", 240, 160)
        await waitFor(() => expect(flowAt("public.orders")).not.toEqual(ordersFrom))
        const tableFrom = flowAt("daily_revenue")
        drag("daily_revenue", -480, 200)
        await waitFor(() => expect(flowAt("daily_revenue")).not.toEqual(tableFrom))
        tableBefore = screenAt("daily_revenue")
      }
      // A new layout, with the card as tall as its column list, that adds no card. The
      // model's card opens the columns of the table it builds.
      fireEvent.click(within(card("daily_revenue")).getByTitle("Show the columns of analytics.daily_revenue"))
      await waitFor(() => expect(within(card("daily_revenue")).getByText("day")).toBeInTheDocument())
      await waitFor(() => expect(parseFloat(card("daily_revenue").style.height)).toBeGreaterThan(ASSET_NODE_HEIGHT))
      expect(container.querySelectorAll(".react-flow__node")).toHaveLength(4)
      const after = { orders: flowAt("public.orders"), table: flowAt("daily_revenue"), tableOnScreen: screenAt("daily_revenue") }
      unmount()
      return { ...after, tableBefore }
    }

    // jsdom gives every element an empty box, so each point of a drag reads as the canvas's
    // edge and React Flow pans the view under it. Give the canvas its size.
    const box = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
      const { width, height } = this.classList.contains("react-flow") ? CANVAS : { width: 0, height: 0 }
      return { x: 0, y: 0, left: 0, top: 0, right: width, bottom: height, width, height, toJSON: () => ({}) }
    })
    try {
      const untouched = await open(false)
      const moved = await open(true)
      // A card moved in the old drawing takes its place in the new one, like every other card.
      expect(moved.orders).toEqual(untouched.orders)
      expect(moved.table).toEqual(untouched.table)
      // The card whose columns were opened stays where the reader had dragged it on screen.
      expect(moved.tableOnScreen.map(Math.round)).toEqual(moved.tableBefore!.map(Math.round))
    } finally {
      box.mockRestore()
    }
  })

  // A model whose table `n` models read: one card, the model with the table it builds. Focused
  // on the table the model reads, the card's +n draws a column of n cards.
  function fanOut(n: number) {
    const [src, hub] = ["table:c-1:raw.src", "table:c-1:gold.hub"]
    return {
      ...BODY,
      nodes: [
        { id: "pipeline:p-1", kind: "pipeline", name: "src_sync", ref_id: "p-1" },
        { id: src, kind: "table", name: "raw.src", connection_id: "c-1" },
        { id: "model:m-0", kind: "model", name: "hub_model", ref_id: "m-0" },
        { id: hub, kind: "table", name: "gold.hub", connection_id: "c-1" },
        ...Array.from({ length: n }, (_, i) => ({ id: `model:r-${i}`, kind: "model", name: `reader_${i}`, ref_id: `r-${i}` })),
      ],
      edges: [
        { from: "pipeline:p-1", to: src, kind: "writes", evidence: "observed" },
        { from: src, to: "model:m-0", kind: "reads", evidence: "inferred" },
        { from: "model:m-0", to: hub, kind: "materializes", evidence: "declared" },
        ...Array.from({ length: n }, (_, i) => ({ from: hub, to: `model:r-${i}`, kind: "reads", evidence: "inferred" })),
      ],
      models: [],
    }
  }

  /** Focuses the table the fan-out's model reads, clicks the model's +n, and returns the canvas once the n cards are drawn. */
  async function openFan(n: number) {
    media.canvas = true
    mockFetch.mockResolvedValue(res(200, fanOut(n)))
    const { container } = render(<AssetLineageView reloadTick={0} initialFocus="table:c-1:raw.src" />)
    const canvas = await waitFor(() => {
      const el = container.querySelector(".react-flow")!.parentElement!
      expect(within(el).getByText("hub_model")).toBeInTheDocument()
      return el
    })
    const hub = () => within(canvas).getByTitle("gold.hub").closest<HTMLElement>(".react-flow__node")!
    await waitFor(() => expect(within(hub()).getByTitle(`Show ${n} more downstream`)).toBeInTheDocument())
    const before = container.querySelectorAll(".react-flow__node").length
    fireEvent.click(within(hub()).getByTitle(`Show ${n} more downstream`))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(before + n))
    const cards = [...container.querySelectorAll<HTMLElement>(".react-flow__node")]
    const zoom = Number(container.querySelector<HTMLElement>(".react-flow__viewport")!.style.transform.match(/scale\(([\d.]+)\)/)![1])
    return { container, cards, zoom, hub: screenBox(container, hub()) }
  }

  const inCanvas = (box: ReturnType<typeof screenBox>) =>
    box.left >= 0 && box.top >= 0 && box.right <= CANVAS.width && box.bottom <= CANVAS.height

  it("zooms out after a +N to fit every card, the one clicked included", async () => {
    // Six cards stacked are too tall for the canvas at full size, but readable fitted.
    const { container, cards, zoom } = await openFan(6)
    const boxes = cards.map((c) => screenBox(container, c))
    expect(boxes.filter((b) => !inCanvas(b))).toEqual([])

    // Fitted, not panned: the zoom that fits the whole drawing, and the drawing centred.
    const [left, top] = [Math.min(...boxes.map((b) => b.left)), Math.min(...boxes.map((b) => b.top))]
    const [right, bottom] = [Math.max(...boxes.map((b) => b.right)), Math.max(...boxes.map((b) => b.bottom))]
    const fit = Math.min((CANVAS.width - 48) / ((right - left) / zoom), (CANVAS.height - 48) / ((bottom - top) / zoom), 1)
    expect(zoom).toBeLessThan(1)
    expect(zoom).toBeGreaterThanOrEqual(MIN_READABLE_FIT_ZOOM)
    expect(zoom).toBeCloseTo(fit, 3)
    expect(left).toBeCloseTo(CANVAS.width - right, 0)
    expect(top).toBeCloseTo(CANVAS.height - bottom, 0)
  })

  it("after a +N too big to read whole, zooms out only to the readable size and keeps the card clicked in view", async () => {
    const { container, cards, zoom, hub } = await openFan(14)
    expect(zoom).toBe(MIN_READABLE_FIT_ZOOM)
    expect(inCanvas(hub)).toBe(true)
    // The column it added sits beside the card, half of it above. The view moves toward the
    // column's start as far as keeps the card on the canvas; the rest is a pan away.
    expect(hub.bottom).toBeCloseTo(CANVAS.height - 24, 0)
    const added = cards.filter((c) => /reader_/.test(c.textContent ?? "")).map((c) => screenBox(container, c))
    expect(added.filter(inCanvas).length).toBeGreaterThan(0)
    expect(added.some((b) => !inCanvas(b))).toBe(true)
    // Across, the card clicked stays put through the zoom and moves only as far as the column
    // needs to fit: the column's edge lands 24px inside the canvas.
    expect(Math.max(...added.map((b) => b.right))).toBeCloseTo(CANVAS.width - 24, 0)
  })

  it("shows a table's columns on its card, from the warehouse's schema", async () => {
    media.canvas = true
    const columns = Array.from({ length: 12 }, (_, i) => ({ name: `col_${i}`, type: i === 0 ? "uuid" : "text", is_primary_key: i === 0 }))
    const tables = [
      { name: "orders", schema: "public", columns },
      { name: "customers", schema: "public", columns: [{ name: "customer_id", type: "bigint", is_primary_key: true }] },
    ]
    mockFetch.mockImplementation(async (url: string) =>
      url.includes("/schema-index") ? res(200, { tables, foreign_keys: [] }) : res(200, BODY),
    )
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const card = () => within(canvas).getByTitle("public.orders").closest<HTMLElement>(".react-flow__node")!

    fireEvent.click(within(card()).getByTitle("Show the columns of public.orders"))
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/explorer/connections/c-1/schema-index")
    await waitFor(() => expect(within(card()).getByText("col_0")).toBeInTheDocument())
    expect(within(card()).getByText("key")).toBeInTheDocument()
    // Ten at a time; the pager reaches the rest.
    expect(within(card()).getByText("1–10 of 12")).toBeInTheDocument()
    expect(within(card()).queryByText("col_11")).toBeNull()
    // The card is laid out as tall as its ten rows, search and pager, and reading it does not select the table.
    const height = (el: HTMLElement) => parseFloat(el.style.height)
    await waitFor(() => expect(height(card())).toBeGreaterThanOrEqual(ASSET_NODE_HEIGHT + 10 * 20))
    fireEvent.click(within(card()).getByText("col_0"))
    expect(within(canvas).queryByText("Selected")).toBeNull()

    // The next table in the same warehouse needs no second request, and one column gets a short card.
    const customers = () => within(canvas).getByTitle("public.customers").closest<HTMLElement>(".react-flow__node")!
    fireEvent.click(within(customers()).getByTitle("Show the columns of public.customers"))
    await waitFor(() => expect(within(customers()).getByText("customer_id")).toBeInTheDocument())
    expect(mockFetch.mock.calls.filter(([u]) => String(u).includes("/schema-index"))).toHaveLength(1)
    await waitFor(() => expect(height(customers())).toBeGreaterThan(ASSET_NODE_HEIGHT))
    expect(height(customers())).toBeLessThanOrEqual(ASSET_NODE_HEIGHT + 80)
    expect(height(card())).toBeGreaterThan(height(customers()))
  })

  it("draws a workspace whose pipelines write many tables so every card is readable in the first view", async () => {
    media.canvas = true
    mockFetch.mockResolvedValue(res(200, WIDE))
    const { container } = render(<AssetLineageView reloadTick={0} />)

    // 13 cards: model_m-3 and the table it builds are one.
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(13))
    const inView = () => {
      const vp = container.querySelector<HTMLElement>(".react-flow__viewport")!.style.transform
      const [, tx, ty, z] = vp.match(/translate\((-?[\d.]+)px, ?(-?[\d.]+)px\) scale\(([\d.]+)\)/)!.map(Number)
      const cards = [...container.querySelectorAll<HTMLElement>(".react-flow__node")].map((n) => {
        const [, x, y] = n.style.transform.match(/translate\((-?[\d.]+)px, ?(-?[\d.]+)px\)/)!.map(Number)
        return { x: tx + x * z, y: ty + y * z, right: tx + (x + ASSET_NODE_WIDTH) * z, bottom: ty + (y + ASSET_NODE_HEIGHT) * z }
      })
      return { zoom: z, outside: cards.filter((c) => c.x < 0 || c.y < 0 || c.right > CANVAS.width || c.bottom > CANVAS.height).length }
    }
    await waitFor(() => expect(inView().zoom).not.toBe(1))
    expect(inView().zoom).toBeGreaterThanOrEqual(MIN_READABLE_FIT_ZOOM)
    expect(inView().outside).toBe(0)

    // The nine tables no model reads are one box, which opens in place.
    const canvas = container.querySelector(".react-flow")!.parentElement!
    fireEvent.click(within(canvas).getByText("9 tables no model reads"))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(13 - 1 + 9))
    expect(within(canvas).getByTitle("raw.a0")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Fold unread tables again" }))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(13))

    // Resetting the view never lands below the readable size, as "fit everything" used to.
    await userEvent.click(screen.getByRole("button", { name: "Reset view" }))
    expect(inView().zoom).toBeGreaterThanOrEqual(MIN_READABLE_FIT_ZOOM)
  })

  it("opens a drawing too big to read at its start, with an overview map, and in full screen", async () => {
    media.canvas = true
    mockFetch.mockResolvedValue(res(200, WIDE))
    const { container } = render(<AssetLineageView reloadTick={0} />)

    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(13))
    expect(container.querySelector(".react-flow__minimap")).toBeNull()
    expect(screen.queryByRole("button", { name: "Overview map" })).toBeNull()

    // Every table drawn is too tall to fit at a readable size. Opening them leaves the view
    // where the reader had it; a map shows where the rest is, and Reset view starts from
    // the cards at a readable size.
    const zoom = () =>
      Number(container.querySelector<HTMLElement>(".react-flow__viewport")!.style.transform.match(/scale\(([\d.]+)\)/)![1])
    await waitFor(() => expect(zoom()).not.toBe(1))
    const before = zoom()
    await userEvent.click(screen.getByRole("button", { name: "Show every table" }))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(24))
    await waitFor(() => expect(container.querySelector(".react-flow__minimap")).not.toBeNull())
    // The map colours each box by its kind from the theme, as the cards are, so it holds in dark mode.
    const fills = [...container.querySelectorAll<SVGRectElement>(".react-flow__minimap-node")].map((r) => r.style.fill)
    expect(fills).toHaveLength(24)
    expect(new Set(fills)).toEqual(new Set([KIND.pipeline, KIND.table, KIND.model]))
    // The map is framed and captioned, so it reads as a map of the drawing; the reader can put it away and back.
    expect(within(container.querySelector<HTMLElement>("[data-overview-map]")!).getByText("Overview")).toBeInTheDocument()
    const mapToggle = screen.getByRole("button", { name: "Overview map" })
    expect(mapToggle).toHaveAttribute("aria-pressed", "true")
    await userEvent.click(mapToggle)
    expect(container.querySelector(".react-flow__minimap")).toBeNull()
    expect(container.querySelector("[data-overview-map]")).toBeNull()
    expect(mapToggle).toHaveAttribute("aria-pressed", "false")
    await userEvent.click(mapToggle)
    await waitFor(() => expect(container.querySelector(".react-flow__minimap")).not.toBeNull())
    expect(mapToggle).toHaveAttribute("aria-pressed", "true")
    expect(zoom()).toBe(before)
    expect(screen.queryByRole("button", { name: "Show every table" })).toBeNull()
    await userEvent.click(screen.getByRole("button", { name: "Reset view" }))
    expect(zoom()).toBe(MIN_READABLE_FIT_ZOOM)

    const opener = screen.getByRole("button", { name: "Full screen" })
    await userEvent.click(opener)
    const dialog = screen.getByRole("dialog", { name: "Whole workspace" })
    await waitFor(() => expect(dialog.querySelectorAll(".react-flow__node")).toHaveLength(24))
    expect(within(dialog).queryByRole("button", { name: "Full screen" })).toBeNull()
    await userEvent.keyboard("{Escape}")
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(opener).toHaveFocus()
  })

  it("asks for a pick when the workspace is too big to draw, and finds by search", async () => {
    const many = {
      ...BODY,
      nodes: [
        ...BODY.nodes,
        ...Array.from({ length: 80 }, (_, i) => ({ id: `table:c-9:t${i}`, kind: "table", name: `filler_${i}` })),
      ],
    }
    mockFetch.mockResolvedValue(res(200, many))
    render(<AssetLineageView reloadTick={0} />)

    // 89 assets, 88 cards: daily_revenue and the table it builds are one.
    expect(await screen.findByText(/has 88 assets, too many to draw at once/)).toBeInTheDocument()
    expect(document.querySelectorAll("[data-asset-row]")).toHaveLength(0)

    const search = screen.getByRole("combobox", { name: "Find a pipeline, table, model, chart or dashboard" })
    await userEvent.type(search, "weekly")
    const matches = screen.getByRole("listbox", { name: "Matches" })
    expect(within(matches).getAllByRole("option")).toHaveLength(1)
    expect(screen.getByRole("status")).toHaveTextContent("1 match")
    await userEvent.type(search, "{Enter}")
    expect(screen.getByRole("heading", { name: /What feeds weekly_rollup/ })).toBeInTheDocument()
    expect(search).toHaveValue("")

    // The arrow keys move through the matches; Enter picks the highlighted one.
    await userEvent.type(search, "daily")
    const options = within(screen.getByRole("listbox", { name: "Matches" })).getAllByRole("option")
    expect(options.map((o) => o.textContent)).toEqual(["Model daily_revenue", "Table analytics.daily_revenue Warehouse c-1"])
    expect(search).toHaveAttribute("aria-activedescendant", options[0].id)
    await userEvent.keyboard("{ArrowDown}")
    expect(search).toHaveAttribute("aria-activedescendant", options[1].id)
    expect(options[1]).toHaveAttribute("aria-selected", "true")
    await userEvent.keyboard("{ArrowDown}")
    expect(search).toHaveAttribute("aria-activedescendant", options[0].id)
    // The table is found by its own name, and drawn as the model that builds it.
    await userEvent.keyboard("{ArrowUp}{Enter}")
    expect(screen.getByRole("heading", { name: /What feeds daily_revenue, and what it feeds/ })).toBeInTheDocument()

    // Leaving the box closes the list; a click on a match still picks it.
    await userEvent.type(search, "orders")
    expect(screen.getByRole("listbox", { name: "Matches" })).toBeInTheDocument()
    await userEvent.tab()
    expect(screen.queryByRole("listbox")).toBeNull()
    expect(search).toHaveAttribute("aria-expanded", "false")
    await userEvent.click(search)
    await userEvent.click(within(screen.getByRole("listbox", { name: "Matches" })).getByText("orders_sync"))
    expect(screen.getByRole("heading", { name: /What feeds orders_sync/ })).toBeInTheDocument()

    // A search that finds nothing says so without listing kinds, so it holds for charts and dashboards too.
    await userEvent.type(search, "zzz")
    expect(screen.getByText("Nothing matches.")).toBeInTheDocument()
    expect(screen.queryByRole("listbox")).toBeNull()
  })

  it("keeps a wide box, like the search, inside the card on a phone", async () => {
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await screen.findByRole("combobox", { name: "Find a pipeline, table, model, chart or dashboard" })

    // jsdom lays nothing out, so this reads the classes that decide it. A box 16rem or wider
    // fits a 375px phone only if it is capped, and a cap of max-w-full only bites if every
    // flex item between it and the card can shrink: a flex item in a row is held at least as
    // wide as its widest child unless it is min-w-0 (or is itself capped).
    const classes = (e: Element) => (e.getAttribute("class") ?? "").split(/\s+/)
    const wide = /^w-(6[4-9]|[7-9]\d|\d{3})$|^w-\[(1[6-9]|[2-9]\d)(\.\d+)?rem\]$/
    const boxes = [...container.querySelectorAll("*")].filter(
      (e) => classes(e).some((c) => wide.test(c)) && !classes(e).some((c) => c === "absolute" || c === "fixed"),
    )
    expect(boxes.length).toBeGreaterThan(0)
    const held: string[] = []
    for (const box of boxes) {
      if (!classes(box).some((c) => c.startsWith("max-w-"))) held.push(`uncapped: ${box.getAttribute("class")}`)
      for (let e: Element | null = box; e && e !== container; e = e.parentElement) {
        const row = e.parentElement && classes(e.parentElement).some((c) => c === "flex" || c === "inline-flex")
        const column = e.parentElement && classes(e.parentElement).includes("flex-col")
        if (row && !column && !classes(e).some((c) => c === "min-w-0" || c === "max-w-full")) {
          held.push(`held open: ${e.getAttribute("class")}`)
        }
      }
    }
    expect(held).toEqual([])
  })


  it("filters the graph to one warehouse, and names each warehouse from its connection", async () => {
    mockConnections.mockResolvedValue({
      connections: [
        { id: "c-1", name: "analytics_wh" },
        { id: "c-2", name: "events_wh" },
      ],
      total: 2,
    })
    const EVENTS_ONLY = {
      ...BODY,
      nodes: BODY.nodes.filter((n) => n.id === P2 || n.id === T_EVENTS),
      edges: BODY.edges.filter((e) => e.from === P2),
      models: [],
    }
    mockFetch.mockImplementation(async (url: string) => res(200, url.includes("connection_id=") ? EVENTS_ONLY : BODY))
    render(<AssetLineageView reloadTick={0} />)

    const filter = await screen.findByRole("combobox", { name: "Warehouse" })
    await waitFor(() =>
      expect(within(filter).getAllByRole("option").map((o) => o.textContent)).toEqual(["All warehouses", "analytics_wh", "events_wh"]),
    )
    // With more than one warehouse drawn, each table says which one it is in.
    const row = (name: string) => screen.getByText(name, { selector: ".font-mono" }).closest("[data-asset-row]") as HTMLElement
    expect(within(row("public.events")).getByText("events_wh")).toBeInTheDocument()
    expect(within(row("public.orders")).getByText("analytics_wh")).toBeInTheDocument()
    expect(within(row("public.events")).getByText("No model reads it")).toBeInTheDocument()

    // The backend filters: pipelines by where they write, models by where they run.
    await userEvent.selectOptions(filter, "events_wh")
    expect(mockFetch).toHaveBeenLastCalledWith(`${URL}?connection_id=c-2`, { method: "GET" })
    expect(await screen.findByText("Everything in events_wh")).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText("Pipelines (1)")).toBeInTheDocument())
    // One warehouse drawn: labelling every card with it says nothing.
    expect(within(row("public.events")).queryByText("events_wh")).toBeNull()
    // The models in the other warehouses are not drawn, so all it can say is that none here reads it.
    expect(within(row("public.events")).getByText("Nothing in this warehouse reads it")).toBeInTheDocument()
    expect(screen.queryByText("No model reads it")).toBeNull()
    // The other warehouses stay on offer.
    expect(within(filter).getAllByRole("option")).toHaveLength(3)

    await userEvent.selectOptions(filter, "All warehouses")
    expect(mockFetch).toHaveBeenLastCalledWith(URL, { method: "GET" })
    expect(await screen.findByText("Whole workspace")).toBeInTheDocument()
  })

  it("says what nothing in a graph narrowed to one warehouse reads is unread there, not unread by every model", async () => {
    media.canvas = true
    // A second warehouse puts the filter on offer; narrowed, the graph hit the pipeline cap.
    const TWO = {
      ...WIDE,
      nodes: [...WIDE.nodes, { id: "table:wh-2:other.t", kind: "table", name: "other.t", connection_id: "wh-2" }],
    }
    const NARROWED = { ...WIDE, stats: { ...WIDE.stats, truncated: { pipelines: true, produced_tables: false, models: false } } }
    mockFetch.mockImplementation(async (url: string) => res(200, url.includes("connection_id=") ? NARROWED : TWO))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    const canvas = () => container.querySelector(".react-flow")!.parentElement!

    // The whole workspace: what no model reads, no model anywhere reads.
    await waitFor(() => expect(within(canvas()).getByText("9 tables no model reads")).toBeInTheDocument())
    expect(screen.getByText(/^13 tables no model reads are folded into one box per pipeline\./)).toBeInTheDocument()
    expect(within(canvas()).getAllByText("No model reads it").length).toBeGreaterThan(0)

    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Warehouse" }), "wh-1")
    expect(mockFetch).toHaveBeenLastCalledWith(`${URL}?connection_id=wh-1`, { method: "GET" })
    await waitFor(() => expect(within(canvas()).getByText("9 tables unread here")).toBeInTheDocument())
    expect(screen.getByText(/^13 tables unread in this warehouse are folded into one box per pipeline\./)).toBeInTheDocument()
    // raw.c0 and raw.d0: their own cards, as a pipeline's one or two unread tables are not folded.
    expect(within(canvas()).getAllByText("Nothing in this warehouse reads it")).toHaveLength(2)
    expect(within(canvas()).queryByText("No model reads it")).toBeNull()
    expect(within(canvas()).queryByText(/tables no model reads/)).toBeNull()
    // So do a card's details.
    fireEvent.click(within(canvas()).getByTitle("raw.c0"))
    expect(
      within(await screen.findByRole("region", { name: "Details of raw.c0" })).getByText("Nothing in this warehouse reads it"),
    ).toBeInTheDocument()
    // The cap was hit in this warehouse's graph, not the workspace's.
    expect(screen.getByText(/^This warehouse has more than 500 pipelines\./)).toBeInTheDocument()
    expect(screen.queryByText(/This workspace has more than/)).toBeNull()
  })

  it("says a lone asset has nothing feeding or reading it in the workspace, or in the warehouse it is narrowed to", async () => {
    const LONE = {
      ...BODY,
      nodes: [{ id: "table:c-2:public.lone", kind: "table", name: "public.lone", connection_id: "c-2" }],
      edges: [],
      models: [],
    }
    mockFetch.mockResolvedValue(res(200, LONE))
    const { unmount } = render(<AssetLineageView reloadTick={0} />)
    expect(await screen.findByText("Nothing in this workspace feeds it or reads from it.")).toBeInTheDocument()
    unmount()

    mockFetch.mockImplementation(async (url: string) => res(200, url.includes("connection_id=") ? LONE : BODY))
    render(<AssetLineageView reloadTick={0} />)
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Warehouse" }), "c-2")
    expect(await screen.findByText("Nothing in this warehouse feeds it or reads from it.")).toBeInTheDocument()
    expect(screen.queryByText(/Nothing in this workspace/)).toBeNull()
  })

  it("keeps the warehouse filter out of a one-warehouse workspace, and names a warehouse it cannot look up by id", async () => {
    const ONE = { ...BODY, nodes: BODY.nodes.filter((n) => n.id !== T_EVENTS), edges: BODY.edges.filter((e) => e.to !== T_EVENTS) }
    mockFetch.mockResolvedValue(res(200, ONE))
    const { unmount } = render(<AssetLineageView reloadTick={0} />)
    await screen.findByText("Whole workspace")
    expect(screen.queryByRole("combobox", { name: "Warehouse" })).toBeNull()
    // The connection list is still read, for each card's logo.
    expect(mockConnections).toHaveBeenCalled()
    unmount()

    // The connection list did not load: the filter still works, by a short id.
    mockConnections.mockRejectedValue(new Error("network"))
    mockFetch.mockResolvedValue(res(200, BODY))
    render(<AssetLineageView reloadTick={0} />)
    const filter = await screen.findByRole("combobox", { name: "Warehouse" })
    await waitFor(() => expect(mockConnections).toHaveBeenCalled())
    expect(within(filter).getAllByRole("option").map((o) => o.textContent)).toEqual(["All warehouses", "Warehouse c-1", "Warehouse c-2"])
  })

  it("says when the graph was cut short", async () => {
    mockFetch.mockResolvedValue(
      res(200, { ...BODY, stats: { ...BODY.stats, truncated: { pipelines: true, produced_tables: false, models: false } } }),
    )
    render(<AssetLineageView reloadTick={0} />)
    expect(await screen.findByText(/more than 500 pipelines\. The graph stops there/)).toBeInTheDocument()
  })

  it("says so when there is nothing to draw", async () => {
    mockFetch.mockResolvedValue(res(200, { nodes: null, edges: null, models: null, stats: {} }))
    render(<AssetLineageView reloadTick={0} />)
    expect(await screen.findByText("Nothing to draw yet.")).toBeInTheDocument()
  })

  it("offers a retry after a failed load, and keeps the graph when a reload fails", async () => {
    mockFetch.mockResolvedValueOnce(res(500, { error: "boom" }))
    mockFetch.mockResolvedValueOnce(res(200, BODY))
    mockFetch.mockRejectedValueOnce(new Error("network"))
    const { rerender } = render(<AssetLineageView reloadTick={0} />)

    expect(await screen.findByRole("alert")).toHaveTextContent("HTTP 500")
    await userEvent.click(screen.getByRole("button", { name: "Retry" }))
    await screen.findByText("Whole workspace")

    rerender(<AssetLineageView reloadTick={1} />)
    expect(await screen.findByRole("alert")).toHaveTextContent("Showing the graph from the last load.")
    expect(screen.getByText("Whole workspace")).toBeInTheDocument()
  })
})

// The launch review (PR4): the summary is the app's stat cards, each with its kind's icon,
// and "Not refreshed" links to the list that names them. A card says its kind with an
// icon, a table's schema apart from its name, its warehouse's logo, and a chip when a
// model needs attention. One colour per kind, from the theme, on the card, the legend's
// square swatch (bug 14: a 4px sliver, the table's invisible) and the overview map.
const KIND = {
  pipeline: "hsl(var(--chart-series-6))",
  table: "hsl(var(--chart-other))",
  model: "hsl(var(--chart-series-3))",
}

describe("the Lineage page's launch review", () => {
  it("says the workspace in one line, each callout linking to the models it counts", async () => {
    mockFetch.mockResolvedValue(res(200, BODY))
    const { unmount } = render(<AssetLineageView reloadTick={0} />)
    await screen.findByText("Whole workspace")

    const summary = () => document.querySelector<HTMLElement>("[data-lineage-summary]")!
    expect(summary().querySelector("p")).toHaveTextContent(/^2 pipelines · 4 tables · 3 models$/)
    const behind = within(summary()).getByRole("link", { name: "2 aren't refreshed by what they read" })
    expect(behind).toHaveAttribute("href", "#lineage-attention")
    // The link lands on the list of them.
    expect(document.getElementById("lineage-attention")).toContainElement(
      screen.getByRole("list", { name: "Models that need attention" }),
    )
    unmount()

    // A model refreshed by what it reads, whose SQL names a table nothing writes: the other callout.
    const traced = BODY.models.map((m) => ({ ...m, uncovered_upstreams: [], trigger_paused: false, ambiguous: false }))
    mockFetch.mockResolvedValue(res(200, { ...BODY, models: traced }))
    const second = render(<AssetLineageView reloadTick={0} />)
    await screen.findByText("Whole workspace")
    expect(within(summary()).getAllByRole("link").map((a) => a.textContent)).toEqual(["1 reads a table we can't trace"])
    second.unmount()

    // Nothing out of step: no callout, and the panel says so.
    const clean = traced.map((m) => ({ ...m, unresolved: [] }))
    mockFetch.mockResolvedValue(res(200, { ...BODY, models: clean }))
    render(<AssetLineageView reloadTick={0} />)
    await screen.findByText("Whole workspace")
    expect(within(summary()).queryByRole("link")).toBeNull()
    expect(screen.getByText("Nothing needs attention")).toBeInTheDocument()
    expect(screen.queryByRole("list", { name: "Models that need attention" })).toBeNull()
  })

  it("gives each card its kind's icon and colour, a table's schema apart from its name, its warehouse's logo, and a chip when it needs attention", async () => {
    media.canvas = true
    mockConnections.mockResolvedValue({
      connections: [{ id: "c-1", name: "analytics_wh", connector_type: "bigquery" }],
      total: 1,
    })
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const card = (name: string) => within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!
    const face = (name: string) => card(name).firstElementChild as HTMLElement

    // A table: the schema small, the name bold, the logo of the warehouse it is in.
    const orders = within(canvas).getByTitle("public.orders")
    expect([...orders.children].map((c) => c.textContent)).toEqual(["public.", "orders"])
    expect(orders.children[1].className).toMatch(/font-semibold/)
    expect(orders.className).not.toMatch(/font-mono/)
    // (The canvas is hidden from screen readers, so its images are found by their alt text.)
    const logo = await waitFor(() => within(card("public.orders")).getByAltText("bigquery logo"))
    expect(logo).toHaveAttribute("src", getConnectorLogoUrl("bigquery"))
    expect(card("public.orders").querySelector("[data-kind-icon=table] svg")).not.toBeNull()
    expect(face("public.orders").style.borderLeftColor).toBe(KIND.table)
    // A warehouse the connection list does not have: no logo, nothing guessed.
    expect(card("public.events").querySelector("img")).toBeNull()

    // A model that needs attention says so in words; one that does not, does not. A card that
    // also carries the table it builds has no room for the words, so its chip names itself.
    expect(card("daily_revenue").querySelector("[data-kind-icon=model]")).not.toBeNull()
    expect(face("daily_revenue").style.borderLeftColor).toBe(KIND.model)
    expect(within(card("daily_revenue")).getByTitle("Needs attention")).toBeInTheDocument()
    expect(within(card("weekly_rollup")).getByText("Needs attention")).toBeInTheDocument()
    expect(within(card("scratch_report")).queryByTitle("Needs attention")).toBeNull()
    // A model's name is not split at a dot it does not have.
    expect(within(canvas).getByTitle("daily_revenue").textContent).toBe("daily_revenue")

    // A pipeline: its icon, in its colour, and no uppercase kind label any more.
    expect(card("orders_sync").querySelector("[data-kind-icon=pipeline]")).not.toBeNull()
    expect(face("orders_sync").style.borderLeftColor).toBe(KIND.pipeline)
    expect(card("orders_sync").querySelector(".uppercase")).toBeNull()

    // The legend's swatches are squares of the same colours.
    const swatches = [...container.querySelectorAll<HTMLElement>("[data-swatch]")]
    expect(swatches.map((s) => [s.dataset.swatch, s.style.backgroundColor])).toEqual([
      ["pipeline", KIND.pipeline],
      ["table", KIND.table],
      ["model", KIND.model],
    ])
    for (const s of swatches) expect(s.className.split(/\s+/)).toEqual(expect.arrayContaining(["h-2.5", "w-2.5"]))
  })
})

// The Lineage page as a map of the workspace: a model and the table it builds are one card,
// the columns are the workspace's own layers, each read says whether it refreshes the model,
// and a model's schedule opens from its card.
describe("the Lineage page's layers and refresh", () => {
  const modelOf = (g: AssetGraph, id: string) => g.models.find((m) => m.model_id === id)!
  const byIdOf = (g: AssetGraph) => new Map(g.nodes.map((n) => [n.id, n]))
  const withEdges = (edges: object[]) => parseAssetGraph({ ...BODY, edges: [...BODY.edges, ...edges] })
  const withModel = (id: string, patch: object) =>
    parseAssetGraph({ ...BODY, models: BODY.models.map((m) => (m.model_id === id ? { ...m, ...patch } : m)) })

  // Silver before Gold, as the workspace orders them; scratch_report is in none. The tables
  // orders_sync and events_sync write are pipeline tables.
  const LAYERS = [
    { name: "Silver", position: 0 },
    { name: "Gold", position: 1 },
  ]
  const LAYERED = {
    ...BODY,
    nodes: BODY.nodes.map((n) => ({
      ...n,
      ...({ [M1]: { layer: "Gold" }, [T_DAILY]: { layer: "Gold" }, [M2]: { layer: "Silver" } } as Record<string, object>)[n.id],
      ...([T_ORDERS, T_CUST, T_EVENTS].includes(n.id) ? { pipeline_table: true } : {}),
    })),
  }

  it("draws a model and the one table it builds as one card, each read marked by whether it refreshes the model", () => {
    const drawn = drawnGraph(graph)
    expect([...drawn.builds]).toEqual([[M1, T_DAILY]])
    expect([...drawn.builtBy]).toEqual([[T_DAILY, M1]])
    expect(drawn.graph.nodes.map((n) => n.id)).not.toContain(T_DAILY)
    expect(drawn.graph.nodes).toHaveLength(8)
    // The build is not a link any more; what read the table reads the model.
    expect(drawn.graph.edges.map((e) => [e.from, e.to, e.kind, e.refresh ?? ""])).toEqual([
      [P1, T_ORDERS, "writes", ""],
      [P1, T_CUST, "writes", ""],
      [P1, M2, "triggers", "wakes"],
      [T_ORDERS, M1, "reads", "stale"],
      [T_CUST, M1, "reads", "stale"],
      [M1, M2, "reads", "stale"],
      [P2, T_EVENTS, "writes", ""],
    ])
    // The asset graph it came from is left alone.
    expect(graph.nodes).toHaveLength(9)

    // Not one thing: a table something else writes too, a build only inferred, a model that builds two.
    expect(drawnGraph(withEdges([{ from: P2, to: T_DAILY, kind: "writes", evidence: "observed" }])).builds.size).toBe(0)
    const inferred = parseAssetGraph({
      ...BODY,
      edges: BODY.edges.map((e) => (e.kind === "materializes" ? { ...e, evidence: "inferred" } : e)),
    })
    expect(drawnGraph(inferred).builds.size).toBe(0)
    const twice = parseAssetGraph({
      ...BODY,
      nodes: [...BODY.nodes, { id: "table:c-1:analytics.daily_copy", kind: "table", name: "analytics.daily_copy", connection_id: "c-1" }],
      edges: [...BODY.edges, { from: M1, to: "table:c-1:analytics.daily_copy", kind: "materializes", evidence: "declared" }],
    })
    expect(drawnGraph(twice).builds.size).toBe(0)
  })

  it("leaves out a trigger from what the model reads, and marks a read that wakes it and a paused one", () => {
    // weekly_rollup also follows daily_revenue, which it reads: the read says so, the trigger adds nothing.
    const follows = parseAssetGraph({
      ...BODY,
      edges: [...BODY.edges, { from: M1, to: M2, kind: "triggers", evidence: "declared" }],
      models: BODY.models.map((m) => (m.model_id === "m-2" ? { ...m, trigger_upstreams: [P1, M1], uncovered_upstreams: [] } : m)),
    })
    const edges = drawnGraph(follows).graph.edges
    expect(edges.filter((e) => e.from === M1 && e.to === M2).map((e) => [e.kind, e.refresh])).toEqual([["reads", "wakes"]])
    expect(edges.find((e) => e.from === P1 && e.to === M2)!.refresh).toBe("wakes")

    // Paused: no read keeps it fresh, and no trigger wakes it.
    const paused = drawnGraph(withModel("m-2", { trigger_paused: true, uncovered_upstreams: [] })).graph.edges
    expect(paused.find((e) => e.from === M1 && e.to === M2)!.refresh).toBe("stale")
    expect(paused.find((e) => e.from === P1 && e.to === M2)!.refresh).toBeUndefined()
  })

  it("walks everything upstream of a card", () => {
    const { edges } = drawnGraph(graph).graph
    expect(upstreamPath(edges, M2)).toEqual(new Set([M2, P1, M1, T_ORDERS, T_CUST]))
    expect(upstreamPath(edges, M1)).toEqual(new Set([M1, T_ORDERS, T_CUST, P1]))
    expect(upstreamPath(edges, P2)).toEqual(new Set([P2]))
  })

  it("puts each card in its stage: pipelines, their tables, each layer in the workspace's order, then no layer", () => {
    const cardsOf = (g: AssetGraph) => drawnGraph(g).graph.nodes.map((node) => ({ id: node.id, node }))
    const layered = parseAssetGraph(LAYERED)
    const { bands, columns } = stageColumns(cardsOf(layered), LAYERS)
    expect(columns).toEqual([
      { band: 0, label: "Pipelines", count: 2 },
      { band: 1, label: "Pipeline tables", count: 3 },
      { band: 2, label: "Silver", count: 1 },
      { band: 3, label: "Gold", count: 1 },
      { band: 4, label: "No layer", count: 1 },
    ])
    expect([M2, M1, M3].map((id) => bands.get(id))).toEqual([2, 3, 4])

    // A layer the workspace no longer lists follows the ones it does; a folded box counts its tables;
    // a table nothing marks a pipeline table makes the column plain "Tables".
    const moved = cardsOf(parseAssetGraph({ ...LAYERED, nodes: LAYERED.nodes.map((n) => (n.id === M3 ? { ...n, layer: "Archive" } : n)) }))
    const box = { id: "fold:p-9", node: undefined, count: 9 }
    expect(stageColumns([...moved, box], LAYERS).columns.map((c) => [c.label, c.count])).toEqual([
      ["Pipelines", 2],
      ["Pipeline tables", 12],
      ["Silver", 1],
      ["Gold", 1],
      ["Archive", 1],
    ])
    expect(stageColumns(cardsOf(graph), null).columns.map((c) => [c.label, c.count])).toEqual([
      ["Pipelines", 2],
      ["Tables", 3],
      ["Models", 3],
    ])

    // A built table that keeps its own card sits with its model.
    const unmerged = parseAssetGraph({
      ...LAYERED,
      edges: LAYERED.edges.map((e) => (e.kind === "materializes" ? { ...e, evidence: "inferred" } : e)),
    })
    const own = stageColumns(cardsOf(unmerged), LAYERS)
    expect(own.bands.get(T_DAILY)).toBe(own.bands.get(M1))
    expect(own.columns.find((c) => c.label === "Gold")!.count).toBe(2)
  })

  it("says in a couple of words what refreshes a model, and what to schedule it after", () => {
    expect(refreshChip(modelOf(graph, "m-1"))).toEqual({ label: "On a cron", tone: "warn" })
    expect(refreshChip(modelOf(graph, "m-2"))).toEqual({ label: "After 0 of 1 inputs", tone: "warn" })
    expect(refreshChip(modelOf(graph, "m-3"))).toEqual({ label: "By hand", tone: "plain" })
    const m2 = modelOf(graph, "m-2")
    expect(refreshChip({ ...m2, uncovered_upstreams: [] })).toEqual({ label: "After its inputs", tone: "ok" })
    expect(refreshChip({ ...m2, upstreams: [], uncovered_upstreams: [] })).toEqual({ label: "After its upstreams", tone: "plain" })
    expect(refreshChip({ ...m2, trigger_paused: true })).toEqual({ label: "Paused", tone: "warn" })
    expect(refreshChip({ ...modelOf(graph, "m-3"), trigger: "interval" })).toEqual({ label: "On an interval", tone: "plain" })

    const byId = byIdOf(graph)
    expect(inputUpstreams(modelOf(graph, "m-2"), byId)).toEqual([{ kind: "model", id: "m-1", name: "daily_revenue" }])
    expect(inputUpstreams(modelOf(graph, "m-1"), byId)).toEqual([{ kind: "pipeline", id: "p-1", name: "orders_sync" }])
    expect(inputUpstreams(modelOf(graph, "m-3"), byId)).toEqual([])
    // An upstream the graph does not draw keeps its id; a table is not something to wait for.
    expect(inputUpstreams({ ...m2, upstreams: ["model:gone", T_ORDERS] }, byId)).toEqual([{ kind: "model", id: "gone" }])

    expect(followsItsInputs(m2)).toBe(false)
    expect(followsItsInputs({ ...m2, uncovered_upstreams: [] })).toBe(true)
    expect(followsItsInputs({ ...m2, uncovered_upstreams: [], trigger_paused: true })).toBe(false)
    expect(followsItsInputs({ ...m2, uncovered_upstreams: [], upstreams: [] })).toBe(false)
    expect(followsItsInputs({ ...modelOf(graph, "m-1"), uncovered_upstreams: [] })).toBe(false)
  })

  it("splits the attention list the way the summary counts it", () => {
    const ids = (items: { model: ModelRefresh }[]) => items.map((i) => i.model.model_id)
    const split = attentionSplit(needsAttention(graph))
    expect([ids(split.notRefreshed), ids(split.untraced)]).toEqual([["m-1", "m-2"], []])
    // daily_revenue followed by what it reads still names a table nothing writes; scratch_report is paused while following a pipeline.
    const g = parseAssetGraph({
      ...BODY,
      models: BODY.models.map((m) =>
        m.model_id === "m-1"
          ? { ...m, uncovered_upstreams: [] }
          : m.model_id === "m-3"
            ? { ...m, trigger: "after_upstream", trigger_upstreams: [P2], trigger_paused: true }
            : m,
      ),
    })
    const both = attentionSplit(needsAttention(g))
    expect([ids(both.notRefreshed).sort(), ids(both.untraced)]).toEqual([["m-2", "m-3"], ["m-1"]])
  })

  it("lays the canvas out in the workspace's layers, and says where a read goes back a layer", async () => {
    media.canvas = true
    mockLayers.value = LAYERS
    mockFetch.mockResolvedValue(res(200, LAYERED))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const x = (name: string) => translateOf(within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!.style.transform)[0]
    const headers = () =>
      [...container.querySelectorAll<HTMLElement>("[data-column-header]")].map((h) => ({
        text: [...h.children].map((c) => c.textContent),
        x: translateOf(h.style.transform)[0],
      }))
    const columns = within(screen.getByRole("group", { name: "Arrange by" }))

    // A workspace with layers starts in them.
    expect(columns.getByRole("button", { name: "Layers" })).toHaveAttribute("aria-pressed", "true")
    await waitFor(() => expect(headers()).toHaveLength(5))
    expect(headers().map((h) => h.text)).toEqual([
      ["Pipelines", "2"],
      ["Pipeline tables", "3"],
      ["Silver", "1"],
      ["Gold", "1"],
      ["No layer", "1"],
    ])
    const xs = headers().map((h) => h.x)
    expect(xs).toEqual([...xs].sort((a, b) => a - b))
    expect(new Set(xs).size).toBe(5)
    // Each card under its column: weekly_rollup in Silver, left of daily_revenue in Gold, though it reads it.
    expect(x("orders_sync")).toBe(xs[0])
    expect(x("public.orders")).toBe(xs[1])
    expect(x("weekly_rollup")).toBe(xs[2])
    expect(x("daily_revenue")).toBe(xs[3])
    expect(x("scratch_report")).toBe(xs[4])
    // The read that goes back a layer says so on the link, and the legend explains it.
    expect([...container.querySelectorAll("[data-edge-note]")].map((n) => n.textContent)).toEqual(["Reads a later layer"])

    // Run order: what runs first on the left, no headers, nothing goes back.
    await userEvent.click(columns.getByRole("button", { name: "Run order" }))
    await waitFor(() => expect(container.querySelectorAll("[data-column-header]")).toHaveLength(0))
    await waitFor(() => expect(x("daily_revenue")).toBeLessThan(x("weekly_rollup")))
    expect(container.querySelectorAll("[data-edge-note]")).toHaveLength(0)
    expect(columns.getByRole("button", { name: "Run order" })).toHaveAttribute("aria-pressed", "true")
  })

  it("starts in run order when the workspace has no layers", async () => {
    media.canvas = true
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    expect(within(screen.getByRole("group", { name: "Arrange by" })).getByRole("button", { name: "Run order" })).toHaveAttribute(
      "aria-pressed",
      "true",
    )
    expect(container.querySelectorAll("[data-column-header]")).toHaveLength(0)
  })

  it("shows on each model's card what refreshes it and when it last ran", async () => {
    media.canvas = true
    const ranAt = new Date(Date.now() - 2 * 3600_000).toISOString()
    const failedAt = new Date(Date.now() - 5 * 60_000).toISOString()
    mockSaved.value = new Map([
      ["m-1", { id: "m-1", name: "daily_revenue", materialization: "table", last_run_at: ranAt, last_run_status: "success" }],
      ["m-2", { id: "m-2", name: "weekly_rollup", materialization: "table", last_run_at: failedAt, last_run_status: "failed" }],
    ])
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const card = (name: string) => within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!
    const chip = (name: string) => {
      const c = card(name).querySelector<HTMLElement>("[data-refresh-chip]")
      return c && [c.textContent, c.dataset.refreshChip]
    }

    expect(chip("daily_revenue")).toEqual(["On a cron", "warn"])
    expect(chip("weekly_rollup")).toEqual(["After 0 of 1 inputs", "warn"])
    expect(chip("scratch_report")).toEqual(["By hand", "plain"])
    expect(chip("public.orders")).toBeNull()
    expect(chip("orders_sync")).toBeNull()
    expect(card("daily_revenue")).toHaveTextContent(`ran ${formatRelativeTime(ranAt)}`)
    expect(card("weekly_rollup")).toHaveTextContent(`failed ${formatRelativeTime(failedAt)}`)
    // Not run, or not in the list: no run to show.
    expect(card("scratch_report")).not.toHaveTextContent(/ran |failed /)
  })

  it("opens a model's details from its card, and its schedule with what it reads picked", async () => {
    media.canvas = true
    mockSaved.value = new Map([
      ["m-1", { id: "m-1", name: "daily_revenue", materialization: "table" }],
      [
        "m-2",
        {
          id: "m-2",
          name: "weekly_rollup",
          materialization: "table",
          last_run_at: new Date(Date.now() - 60_000).toISOString(),
          last_run_status: "failed",
          last_run_error: 'relation "analytics.daily_revenue" does not exist',
        },
      ],
    ])
    mockFetch.mockResolvedValue(res(200, BODY))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!

    fireEvent.click(within(canvas).getByTitle("weekly_rollup"))
    const details = await screen.findByRole("region", { name: "Details of weekly_rollup" })
    expect(within(details).getByText("After 0 of 1 inputs")).toBeInTheDocument()
    expect(within(details).getByText(/^Last run failed /)).toBeInTheDocument()
    expect(within(details).getByText('relation "analytics.daily_revenue" does not exist')).toBeInTheDocument()
    expect(within(details).getByRole("list", { name: "Why it needs attention" })).toBeInTheDocument()
    // What feeds it: the model it reads, which does not refresh it, and the pipeline that wakes it.
    const feeds = within(details).getByRole("list", { name: "What feeds it" })
    // (Each starts with its kind icon's hidden word.)
    expect([...feeds.querySelectorAll("li")].map((li) => li.textContent)).toEqual([
      "Pipelineorders_sync wakes it when it finishes",
      "Modeldaily_revenue it reads this · doesn't refresh it",
    ])
    expect(within(details).getByText("Nothing drawn.")).toBeInTheDocument()

    // Rebuild after its inputs: the Schedule dialog, with daily_revenue picked.
    await userEvent.click(within(details).getByRole("button", { name: "Rebuild after its inputs" }))
    let dialog = screen.getByRole("dialog", { name: "Schedule weekly_rollup" })
    expect(dialog).toHaveAttribute("data-saved-query", "m-2")
    expect(JSON.parse(dialog.getAttribute("data-preset")!)).toEqual({
      upstreams: [{ kind: "model", id: "m-1", name: "daily_revenue" }],
    })
    await userEvent.click(within(dialog).getByRole("button", { name: "Close schedule" }))
    expect(screen.queryByRole("dialog")).toBeNull()

    // Edit schedule: the dialog as it is, nothing picked. Saving refetches the graph and the runs.
    await userEvent.click(within(details).getByRole("button", { name: "Edit schedule" }))
    dialog = screen.getByRole("dialog", { name: "Schedule weekly_rollup" })
    expect(JSON.parse(dialog.getAttribute("data-preset")!)).toBeNull()
    const fetches = mockFetch.mock.calls.length
    await userEvent.click(within(dialog).getByRole("button", { name: "Save schedule" }))
    expect(mockReloadSaved).toHaveBeenCalledTimes(1)
    await waitFor(() => expect(mockFetch.mock.calls.length).toBeGreaterThan(fetches))
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Close schedule" }))

    // Its upstream, from the panel: the merged card, with the table it builds and the pipeline it waits for.
    await userEvent.click(within(details).getByRole("button", { name: "Select daily_revenue" }))
    const upstream = await screen.findByRole("region", { name: "Details of daily_revenue" })
    expect(upstream).toHaveTextContent("Builds analytics.daily_revenue")
    await userEvent.click(within(upstream).getByRole("button", { name: "Rebuild after its inputs" }))
    expect(JSON.parse(screen.getByRole("dialog", { name: "Schedule daily_revenue" }).getAttribute("data-preset")!)).toEqual({
      upstreams: [{ kind: "pipeline", id: "p-1", name: "orders_sync" }],
    })
    await userEvent.click(screen.getByRole("button", { name: "Close schedule" }))

    // No saved query listed for it: nothing to schedule from here.
    fireEvent.click(within(canvas).getByTitle("scratch_report"))
    const scratch = await screen.findByRole("region", { name: "Details of scratch_report" })
    expect(within(scratch).queryByRole("button", { name: "Edit schedule" })).toBeNull()
    expect(within(scratch).queryByRole("button", { name: "Rebuild after its inputs" })).toBeNull()

    // A click on the empty canvas puts the attention list back.
    fireEvent.click(container.querySelector(".react-flow__pane")!)
    await waitFor(() => expect(screen.queryByRole("region", { name: /^Details of / })).toBeNull())
    expect(screen.getByRole("heading", { name: /Needs attention \(2\)/ })).toBeInTheDocument()
  })

  it("says a model already rebuilds after its inputs instead of offering to", async () => {
    mockSaved.value = new Map([["m-2", { id: "m-2", name: "weekly_rollup", materialization: "table" }]])
    mockFetch.mockResolvedValue(
      res(200, {
        ...BODY,
        models: BODY.models.map((m) => (m.model_id === "m-2" ? { ...m, trigger_upstreams: [P1, M1], uncovered_upstreams: [] } : m)),
      }),
    )
    render(<AssetLineageView reloadTick={0} />)
    await userEvent.click(await screen.findByRole("button", { name: "Show the details of weekly_rollup" }))
    const details = screen.getByRole("region", { name: "Details of weekly_rollup" })
    expect(within(details).getByText("Already rebuilds after its inputs")).toBeInTheDocument()
    expect(within(details).queryByRole("button", { name: "Rebuild after its inputs" })).toBeNull()
    expect(within(details).getByRole("button", { name: "Edit schedule" })).toBeInTheDocument()
    await userEvent.click(within(details).getByRole("button", { name: "Close details" }))
    expect(screen.queryByRole("region", { name: /^Details of / })).toBeNull()
  })
})

// A lake pipeline writes Parquet files to a bucket; a BigQuery external table reads them;
// a Silver model reads the external table. The backend links the folder's table to the
// external table (an "external" link, asset_graph.go) and gives the model the pipeline as
// its upstream, so the page has to see the pipeline behind the external table too: what
// refreshes the read, what the Schedule dialog lists as the input's producer.
describe("a lake pipeline linked to the external table a warehouse reads its files through", () => {
  const P_LAKE = "pipeline:p-lake"
  const T_FOLDER = "table:gcs-1\u0000lake\u0000fintech/public/transactions"
  const T_EXT = "table:bq-1:fintech_bronze.transactions"
  const M_SILVER = "model:m-silver"
  const silver = {
    model_id: "m-silver",
    node_id: M_SILVER,
    name: "silver_transactions",
    trigger: "after_upstream",
    trigger_upstreams: [P_LAKE],
    trigger_paused: false,
    upstreams: [P_LAKE],
    uncovered_upstreams: [],
    unresolved: [],
    ambiguous: false,
  }
  const LAKE = {
    nodes: [
      { id: P_LAKE, kind: "pipeline", name: "card_transactions", ref_id: "p-lake", connection_id: "gcs-1" },
      { id: T_FOLDER, kind: "table", name: "public.transactions", connection_id: "gcs-1", pipeline_table: true },
      { id: T_EXT, kind: "table", name: "fintech_bronze.transactions", connection_id: "bq-1", pipeline_table: true },
      { id: M_SILVER, kind: "model", name: "silver_transactions", ref_id: "m-silver", connection_id: "bq-1", layer: "Silver" },
    ],
    edges: [
      { from: P_LAKE, to: T_FOLDER, kind: "writes", evidence: "observed" },
      { from: T_FOLDER, to: T_EXT, kind: "external", evidence: "observed" },
      { from: T_EXT, to: M_SILVER, kind: "reads", evidence: "inferred" },
    ],
    models: [silver],
    stats: {
      pipelines: 1,
      tables: 2,
      models: 1,
      edges: { writes: 1, external: 1, reads: 1 },
      models_with_uncovered_upstreams: 0,
      models_with_unresolved_references: 0,
      models_with_multiple_upstreams: 0,
      truncated: { pipelines: false, produced_tables: false, models: false },
    },
  }
  const lake = parseAssetGraph(LAKE)
  const withSilver = (patch: object) => parseAssetGraph({ ...LAKE, models: [{ ...silver, ...patch }] })

  afterEach(() => {
    vi.useRealTimers()
  })

  it("finds the pipeline behind an external table, and marks the model's read by whether it follows it", () => {
    const producers = tableProducers(lake.edges)
    expect(producers.get(T_FOLDER)).toEqual([P_LAKE])
    expect(producers.get(T_EXT)).toEqual([P_LAKE])
    expect(producers.has(M_SILVER)).toBe(false)

    const readOf = (g: AssetGraph) => drawnGraph(g).graph.edges.find((e) => e.from === T_EXT && e.to === M_SILVER)!
    expect(readOf(lake).refresh).toBe("wakes")
    expect(readOf(withSilver({ trigger: "cron", trigger_upstreams: [], uncovered_upstreams: [P_LAKE] })).refresh).toBe("stale")
    expect(readOf(withSilver({ trigger_paused: true })).refresh).toBe("stale")
    // The link itself is kept as it is: nothing merges a folder into the table that reads it.
    expect(drawnGraph(lake).graph.edges.map((e) => [e.from, e.to, e.kind])).toEqual([
      [P_LAKE, T_FOLDER, "writes"],
      [T_FOLDER, T_EXT, "external"],
      [T_EXT, M_SILVER, "reads"],
    ])
    expect(upstreamPath(drawnGraph(lake).graph.edges, M_SILVER)).toEqual(new Set([M_SILVER, T_EXT, T_FOLDER, P_LAKE]))
  })

  it("gives the Schedule dialog the pipeline as the producer of what the model reads", () => {
    const around = modelSurroundings(lake, M_SILVER)!
    expect(around.inputs.map((i) => [i.table.id, i.producers.map((p) => p.id)])).toEqual([[T_EXT, [P_LAKE]]])
    expect(inputWakes(around.inputs[0], new Set([P_LAKE]))).toBe(true)
    expect(inputWakes(around.inputs[0], new Set())).toBe(false)
    expect(inputUpstreams(lake.models[0], new Map(lake.nodes.map((n) => [n.id, n])))).toEqual([
      { kind: "pipeline", id: "p-lake", name: "card_transactions" },
    ])
  })

  it("says a folder is read as an external table, and names the link", () => {
    const folder = lake.nodes.find((n) => n.id === T_FOLDER)!
    expect(describeAsset(folder, lake, undefined)).toBe("A warehouse reads it as 1 external table")
    const both = parseAssetGraph({ ...LAKE, edges: [...LAKE.edges, { from: T_FOLDER, to: M_SILVER, kind: "reads", evidence: "inferred" }] })
    expect(describeAsset(folder, both, undefined)).toBe("Read by 1 model and 1 external table")
    expect(describeAsset(lake.nodes.find((n) => n.id === T_EXT)!, lake, undefined)).toBe("Read by 1 model")
    expect(edgeLabel({ kind: "external", evidence: "observed" })).toBe("Read as an external table · seen in a run")
    // A folder the warehouse reads is not folded away with the tables nothing reads.
    expect(foldLooseAssets(wholeGraph(lake), lake).groups.size).toBe(0)
  })

  it("reads whether a warehouse's external tables are still being read", () => {
    expect(lake.external_tables_pending).toBe(false)
    expect(parseAssetGraph({ ...LAKE, external_tables_pending: true }).external_tables_pending).toBe(true)
    expect(parseAssetGraph(null).external_tables_pending).toBe(false)
  })

  it("lays the folder and its external table out in the pipeline tables' column, in that order, before Silver", async () => {
    media.canvas = true
    mockLayers.value = [{ name: "Silver", position: 0 }]
    mockFetch.mockResolvedValue(res(200, LAKE))
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(4))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const x = (name: string) => translateOf(within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!.style.transform)[0]
    await waitFor(() => expect(container.querySelectorAll("[data-column-header]")).toHaveLength(3))
    const headers = [...container.querySelectorAll<HTMLElement>("[data-column-header]")].map((h) => ({
      text: [...h.children].map((c) => c.textContent),
      x: translateOf(h.style.transform)[0],
    }))
    expect(headers.map((h) => h.text)).toEqual([
      ["Pipelines", "1"],
      ["Pipeline tables", "2"],
      ["Silver", "1"],
    ])
    expect(x("card_transactions")).toBe(headers[0].x)
    expect(x("public.transactions")).toBe(headers[1].x)
    expect(x("fintech_bronze.transactions")).toBeGreaterThan(x("public.transactions"))
    expect(x("silver_transactions")).toBeGreaterThan(x("fintech_bronze.transactions"))
    expect(x("silver_transactions")).toBe(headers[2].x)
    // Nothing reads back a layer.
    expect(container.querySelectorAll("[data-edge-note]")).toHaveLength(0)
  })

  it("asks again, quietly, while a warehouse's external tables are first being read, and draws the link once they are", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    // Before the read: the model names a table the graph cannot place yet.
    const before = {
      ...LAKE,
      nodes: LAKE.nodes.filter((n) => n.id !== T_EXT),
      edges: LAKE.edges.slice(0, 1),
      models: [{ ...silver, trigger: "manual", trigger_upstreams: [], upstreams: [], unresolved: ["fintech_bronze.transactions"] }],
      external_tables_pending: true,
    }
    mockFetch
      .mockResolvedValueOnce(res(200, before))
      .mockResolvedValueOnce(res(500, { error: "boom" }))
      .mockResolvedValueOnce(res(200, before))
      .mockResolvedValueOnce(res(200, LAKE))
    render(<AssetLineageView reloadTick={0} />)
    const tableRows = () =>
      [...screen.getByRole("heading", { name: /^Tables \(/ }).closest("section")!.querySelectorAll("[data-asset-row]")].map(
        (row) => row.querySelector(".font-mono")!.textContent,
      )

    expect(await screen.findByText(/Reading the external tables of a warehouse for the first time/)).toBeInTheDocument()
    expect(tableRows()).toEqual(["public.transactions"])
    expect(mockFetch).toHaveBeenCalledTimes(1)

    // A failed ask says nothing; one still reading leaves the graph as it is.
    await vi.advanceTimersByTimeAsync(15_000)
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2))
    expect(screen.queryByRole("alert")).toBeNull()
    await vi.advanceTimersByTimeAsync(15_000)
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(3))
    expect(tableRows()).toEqual(["public.transactions"])

    // Read: the link is drawn, the notice goes, and nothing more is asked.
    await vi.advanceTimersByTimeAsync(15_000)
    await waitFor(() => expect(tableRows()).toEqual(["fintech_bronze.transactions", "public.transactions"]))
    expect(screen.queryByText(/Reading the external tables of a warehouse/)).toBeNull()
    expect(screen.getByText("A warehouse reads it as 1 external table")).toBeInTheDocument()
    expect(mockFetch.mock.calls.every(([url]) => url === URL)).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(mockFetch).toHaveBeenCalledTimes(4)
  })

  it("stops asking after as long as the backend gives the read", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    mockFetch.mockResolvedValue(res(200, { ...LAKE, external_tables_pending: true }))
    render(<AssetLineageView reloadTick={0} />)
    await screen.findByText(/Reading the external tables of a warehouse for the first time/)
    // Twelve asks, 15 seconds apart: the three minutes the backend gives the read. Far
    // longer on the clock than that asks no more.
    for (let i = 0; i < 40; i++) await vi.advanceTimersByTimeAsync(15_000)
    expect(mockFetch).toHaveBeenCalledTimes(13)
  })
})

// The charts drawn from a model, and the dashboards that show them, after the last model
// layer. daily_revenue draws four charts, one on a dashboard; weekly_rollup draws one,
// on the same dashboard. So three of daily_revenue's charts are on no dashboard.
describe("charts and dashboards on Lineage", () => {
  const C_ON = "chart:c-1"
  const C_LOOSE = ["chart:c-2", "chart:c-3", "chart:c-4"]
  const C_WEEKLY = "chart:c-5"
  const D = "dashboard:d-1"
  const CHARTS = {
    ...BODY,
    nodes: [
      ...BODY.nodes,
      { id: C_ON, kind: "chart", name: "revenue_by_day", ref_id: "c-1", chart_type: "bar" },
      { id: C_LOOSE[0], kind: "chart", name: "revenue_trend", ref_id: "c-2", chart_type: "line" },
      { id: C_LOOSE[1], kind: "chart", name: "revenue_split", ref_id: "c-3", chart_type: "pie" },
      { id: C_LOOSE[2], kind: "chart", name: "top_customers", ref_id: "c-4", chart_type: "a-type-from-later" },
      { id: C_WEEKLY, kind: "chart", name: "weekly_total", ref_id: "c-5", chart_type: "scorecard" },
      { id: D, kind: "dashboard", name: "Revenue overview", ref_id: "d-1", charts: 2 },
    ],
    edges: [
      ...BODY.edges,
      ...[C_ON, ...C_LOOSE].map((to) => ({ from: M1, to, kind: "draws", evidence: "declared" })),
      { from: M2, to: C_WEEKLY, kind: "draws", evidence: "declared" },
      { from: C_ON, to: D, kind: "shows", evidence: "declared" },
      { from: C_WEEKLY, to: D, kind: "shows", evidence: "declared" },
    ],
    stats: {
      ...BODY.stats,
      charts: 5,
      dashboards: 1,
      edges: { ...BODY.stats.edges, draws: 5, shows: 2 },
      truncated: { ...BODY.stats.truncated, charts: true },
    },
  }
  const charted = parseAssetGraph(CHARTS)
  const node = (id: string) => charted.nodes.find((n) => n.id === id)!

  it("reads charts and dashboards, says what each is, and links each to its own page", () => {
    expect(charted.stats).toMatchObject({ charts: 5, dashboards: 1 })
    expect(charted.stats.truncated).toMatchObject({ charts: true, dashboards: false })
    expect(node(C_ON).chart_type).toBe("bar")
    expect(node(D).charts).toBe(2)
    expect(charted.nodes.filter((n) => isDisplay(n.kind)).map((n) => n.id)).toEqual([C_ON, ...C_LOOSE, C_WEEKLY, D])

    expect(describeAsset(node(C_ON), charted, undefined)).toBe("Bar · on 1 dashboard")
    expect(describeAsset(node(C_LOOSE[0]), charted, undefined)).toBe("Line · on no dashboard")
    // A type this page does not know yet is still a chart.
    expect(describeAsset(node(C_LOOSE[2]), charted, undefined)).toBe("Chart · on no dashboard")
    expect(describeAsset(node(D), charted, undefined)).toBe("Shows 2 charts")

    expect(assetHref(node(C_ON))).toBe("/charts/c-1")
    expect(assetHref(node(D))).toBe("/dashboards/d-1")
    expect(edgeLabel({ kind: "draws", evidence: "declared" })).toBe("Drawn as a chart · configured")
    expect(edgeLabel({ kind: "shows", evidence: "declared" })).toBe("Shown on a dashboard · configured")

    // Named alike, a model comes before a chart and a chart before a dashboard.
    expect(searchAssets(charted, "revenue").map((n) => n.name)).toEqual([
      "revenue_by_day",
      "revenue_split",
      "revenue_trend",
      "Revenue overview",
      "daily_revenue",
      "analytics.daily_revenue",
    ])
  })

  it("lists what a model changes, however far downstream: its charts, then the dashboards, by name", () => {
    expect(displaysDownstream(charted, M1).map((n) => n.name)).toEqual([
      "revenue_by_day",
      "revenue_split",
      "revenue_trend",
      "top_customers",
      // Through the table daily_revenue builds, which weekly_rollup reads.
      "weekly_total",
      "Revenue overview",
    ])
    expect(displaysDownstream(charted, M2).map((n) => n.name)).toEqual(["weekly_total", "Revenue overview"])
    expect(displaysDownstream(charted, M3)).toEqual([])
  })

  it("leaves charts and dashboards out when asked, but keeps the workspace's counts", () => {
    const plain = withoutDisplays(charted)
    expect(plain.nodes).toHaveLength(BODY.nodes.length)
    expect(plain.edges.map((e) => e.kind)).toEqual(BODY.edges.map((e) => e.kind))
    expect(plain.stats).toBe(charted.stats)
    // Nothing to leave out: the graph itself, so nothing redraws.
    expect(withoutDisplays(graph)).toBe(graph)
  })

  it("folds a model's charts on no dashboard into one box, and keeps a chart a dashboard shows", () => {
    const f = foldLooseAssets(wholeGraph(charted), charted)
    expect([...f.groups.values()].map((g) => [g.id, g.kind, g.from, g.members])).toEqual([
      ["fold:model:m-1", "chart", M1, C_LOOSE],
    ])
    expect(f.kinds).toEqual(new Set(["chart"]))
    expect(f.nodeIds).toEqual(expect.arrayContaining([C_ON, C_WEEKLY, D, "fold:model:m-1"]))
    expect(f.nodeIds.some((id) => C_LOOSE.includes(id))).toBe(false)
    expect(f.edges.filter((e) => e.to === "fold:model:m-1")).toEqual([
      { from: M1, to: "fold:model:m-1", kind: "draws", evidence: "declared" },
    ])

    // Two hops from public.orders reach the charts but not the dashboard: the chart it
    // shows still keeps its own card.
    const near = neighbourhood(charted, T_ORDERS, { depth: 2 })
    expect(near.nodeIds).not.toContain(D)
    expect([...foldLooseAssets(near, charted).groups.values()].map((g) => g.members)).toEqual([C_LOOSE])
    // The focused model's charts are what was asked to see.
    expect(foldLooseAssets(neighbourhood(charted, M1), charted).groups.size).toBe(0)
    // Opened, the box is still counted as one that would fold.
    const opened = foldLooseAssets(wholeGraph(charted), charted, "all")
    expect(opened.groups.size).toBe(0)
    expect(opened.opened).toBe(1)
    expect(opened.kinds).toEqual(new Set(["chart"]))
  })

  it("puts charts after every model layer, and dashboards last", () => {
    const g = parseAssetGraph({
      ...CHARTS,
      nodes: CHARTS.nodes.map((n) => (n.id === M1 ? { ...n, layer: "Gold" } : n)),
    })
    const byId = new Map(g.nodes.map((n) => [n.id, n]))
    const cards = [P1, T_ORDERS, M1, M3, C_ON, D].map((id) => ({ id, node: byId.get(id) }))
    const box = { id: "fold:model:m-1", node: undefined, count: 3, kind: "chart" as const }
    const { bands, columns } = stageColumns([...cards, box], [{ name: "Gold" }])
    expect(columns.map((c) => [c.label, c.count])).toEqual([
      ["Pipelines", 1],
      ["Tables", 1],
      ["Gold", 1],
      ["No layer", 1],
      ["Charts", 4],
      ["Dashboards", 1],
    ])
    expect(bands.get("fold:model:m-1")).toBe(bands.get(C_ON))
    // A box of tables, which has no kind, stays with the tables.
    expect(stageColumns([{ id: "fold:pipeline:p-1", node: undefined, count: 3 }], null).columns[0].label).toBe("Pipeline tables")
  })

  it("counts them in the summary, lists them, and a switch leaves them out", async () => {
    mockFetch.mockResolvedValue(res(200, CHARTS))
    render(<AssetLineageView reloadTick={0} />)

    expect(await screen.findByText("Charts (5)")).toBeInTheDocument()
    const summary = () => document.querySelector<HTMLElement>("[data-lineage-summary]")!.querySelector("p")
    expect(summary()).toHaveTextContent(/^2 pipelines · 4 tables · 3 models · 5 charts · 1 dashboard$/)
    expect(screen.getByText("Dashboards (1)")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "revenue_by_day" })).toHaveAttribute("href", "/charts/c-1")
    expect(screen.getByRole("link", { name: "Revenue overview" })).toHaveAttribute("href", "/dashboards/d-1")
    expect(screen.getByText("Bar · on 1 dashboard")).toBeInTheDocument()

    const toggle = screen.getByRole("switch", { name: "Charts & dashboards" })
    expect(toggle).toBeChecked()
    await userEvent.click(toggle)
    expect(toggle).not.toBeChecked()
    expect(screen.queryByText("Charts (5)")).toBeNull()
    expect(screen.queryByText("Dashboards (1)")).toBeNull()
    expect(screen.queryByRole("link", { name: "revenue_by_day" })).toBeNull()
    // The workspace still has them.
    expect(summary()).toHaveTextContent(/· 5 charts · 1 dashboard$/)
    await userEvent.click(toggle)
    expect(screen.getByText("Charts (5)")).toBeInTheDocument()
  })

  it("names in a model's details the charts and dashboards it changes, even with them left out of the drawing", async () => {
    mockFetch.mockResolvedValue(res(200, CHARTS))
    render(<AssetLineageView reloadTick={0} />)
    await userEvent.click(await screen.findByRole("button", { name: "Show the details of weekly_rollup" }))
    const details = () => screen.getByRole("region", { name: "Details of weekly_rollup" })
    const impact = () => within(details()).getByRole("list", { name: "Charts and dashboards it changes" })
    expect(within(details()).getByText("Charts and dashboards it changes (2)")).toBeInTheDocument()
    const links = () => within(impact()).getAllByRole("link")
    expect(links().map((a) => [a.closest("li")!.getAttribute("data-impact"), a.getAttribute("href")])).toEqual([
      ["chart", "/charts/c-5"],
      ["dashboard", "/dashboards/d-1"],
    ])
    // Each says what it is, then its name, as two words.
    expect(links().map((a) => a.textContent)).toEqual(["Chart weekly_total", "Dashboard Revenue overview"])
    expect(links()[1]).toHaveAccessibleName("Dashboard Revenue overview")

    await userEvent.click(screen.getByRole("switch", { name: "Charts & dashboards" }))
    expect(links()).toHaveLength(2)

    // A model nothing is drawn from has no such list, and nor has a chart.
    await userEvent.click(screen.getByRole("button", { name: "Show the details of scratch_report" }))
    expect(within(screen.getByRole("region", { name: "Details of scratch_report" })).queryByRole("list", {
      name: "Charts and dashboards it changes",
    })).toBeNull()
  })

  it("draws charts and dashboards as the last two columns, a model's loose charts as one box that opens", async () => {
    media.canvas = true
    // A workspace with layers is drawn in columns, one per layer.
    mockLayers.value = [{ name: "Gold", position: 0 }]
    mockFetch.mockResolvedValue(res(200, CHARTS))
    const { container } = render(<AssetLineageView reloadTick={0} />)

    // 15 assets: daily_revenue and the table it builds are one card, its three loose charts one box.
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(15 - 1 - 2))
    await waitFor(() => expect(container.querySelectorAll("[data-column-header]").length).toBeGreaterThan(0))
    const headers = () =>
      [...container.querySelectorAll<HTMLElement>("[data-column-header]")].map((h) => [...h.children].map((c) => c.textContent))
    expect(headers().slice(-2)).toEqual([
      ["Charts", "5"],
      ["Dashboards", "1"],
    ])
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const x = (name: string) => translateOf(within(canvas).getByTitle(name).closest<HTMLElement>(".react-flow__node")!.style.transform)[0]
    expect(x("revenue_by_day")).toBeGreaterThan(x("weekly_rollup"))
    expect(x("Revenue overview")).toBeGreaterThan(x("revenue_by_day"))
    expect(within(canvas).getByTitle("Show these charts")).toHaveTextContent("3 charts on no dashboard")
    expect(screen.getByText(/3 charts on no dashboard are folded into one box per model/)).toBeInTheDocument()
    // The legend has a swatch for each kind drawn.
    expect([...container.querySelectorAll<HTMLElement>("[data-swatch]")].map((s) => s.dataset.swatch)).toEqual([
      "pipeline",
      "table",
      "model",
      "chart",
      "dashboard",
    ])

    fireEvent.click(within(canvas).getByText("3 charts on no dashboard"))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(15 - 1))
    expect(within(canvas).getByTitle("top_customers")).toBeInTheDocument()
    await userEvent.click(screen.getByRole("button", { name: "Fold loose charts again" }))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(15 - 1 - 2))

    // Left out, the drawing is the workspace's pipelines, tables and models again.
    await userEvent.click(screen.getByRole("switch", { name: "Charts & dashboards" }))
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(9 - 1))
    expect(headers().map((h) => h[0])).not.toContain("Charts")
    expect([...container.querySelectorAll<HTMLElement>("[data-swatch]")].map((s) => s.dataset.swatch)).toEqual([
      "pipeline",
      "table",
      "model",
    ])
  })
})

// Column lineage (design § 3.5): each model's output columns and what each is computed from,
// read out of its SQL by sqlglot. daily_revenue's revenue sums orders.amount and a refunds
// table the graph does not have, and its rows are filtered and joined; weekly_rollup's
// revenue sums daily_revenue's, in the warehouse's case. scratch_report's SQL did not parse.
describe("column lineage on Lineage", () => {
  const direct = (from: string, from_table: string, from_column: string, to: string, to_column: string, subtype: string) => ({
    from,
    from_table,
    from_column,
    to,
    to_column,
    type: "DIRECT",
    subtype,
  })
  const COLUMNS = {
    pending: false,
    truncated: false,
    models: [
      { node_id: M1, status: "partial", columns: ["day", "revenue", "customer_count"], reason: "public.refunds is not in the warehouse's schema" },
      { node_id: M2, status: "ok", columns: ["week", "revenue"] },
      { node_id: M3, status: "failed", columns: [], reason: "the SQL did not parse" },
    ],
    edges: [
      direct(T_ORDERS, "public.orders", "created_at", M1, "day", "TRANSFORMATION"),
      direct(T_ORDERS, "public.orders", "amount", M1, "revenue", "AGGREGATION"),
      direct("", "public.refunds", "refund_amount", M1, "revenue", "TRANSFORMATION"),
      direct(T_CUST, "public.customers", "customer_id", M1, "customer_count", "AGGREGATION"),
      { from: T_ORDERS, from_table: "public.orders", from_column: "status", to: M1, to_column: "", type: "INDIRECT", subtype: "FILTER" },
      { from: T_CUST, from_table: "public.customers", from_column: "customer_id", to: M1, to_column: "", type: "INDIRECT", subtype: "JOIN" },
      direct(T_DAILY, "analytics.daily_revenue", "day", M2, "week", "TRANSFORMATION"),
      direct(T_DAILY, "analytics.daily_revenue", "REVENUE", M2, "revenue", "AGGREGATION"),
    ],
  }
  const WITH_COLUMNS = { ...BODY, column_lineage: COLUMNS }
  const items = (list: HTMLElement) => [...list.querySelectorAll("li")].map((li) => li.textContent)

  afterEach(() => {
    vi.useRealTimers()
  })

  it("lists a model's columns in its details, and says where the chosen one comes from and what reads it", async () => {
    mockFetch.mockResolvedValue(res(200, WITH_COLUMNS))
    render(<AssetLineageView reloadTick={0} />)
    await userEvent.click(await screen.findByRole("button", { name: "Show the details of daily_revenue" }))
    const details = (name: string) => screen.getByRole("region", { name: `Details of ${name}` })
    const lineage = (name: string) => within(details(name)).getByRole("group", { name: "Column lineage" })
    const column = (name: string, c: string) => within(lineage(name)).getByRole("button", { name: c })

    expect(within(lineage("daily_revenue")).getByText("Some of its columns could not be traced: public.refunds is not in the warehouse's schema")).toBeInTheDocument()
    const chips = within(lineage("daily_revenue")).getByRole("list", { name: "Columns of daily_revenue" })
    expect(within(chips).getAllByRole("button").map((b) => b.textContent)).toEqual(["day", "revenue", "customer_count"])
    // What filters or joins its rows is listed apart from where any one column comes from.
    expect(items(within(lineage("daily_revenue")).getByRole("list", { name: "Its rows are filtered, joined or grouped by" }))).toEqual([
      "Tablepublic.customers.customer_id in a join",
      "Tablepublic.orders.status in a filter",
    ])
    expect(within(lineage("daily_revenue")).queryByRole("list", { name: "Comes from" })).toBeNull()

    await userEvent.click(column("daily_revenue", "revenue"))
    expect(column("daily_revenue", "revenue")).toHaveAttribute("aria-pressed", "true")
    expect(column("daily_revenue", "day")).toHaveAttribute("aria-pressed", "false")
    expect(items(within(lineage("daily_revenue")).getByRole("list", { name: "Comes from" }))).toEqual([
      "Tablepublic.orders.amount aggregated",
      "public.refunds.refund_amount transformed · not on this graph",
    ])
    expect(items(within(lineage("daily_revenue")).getByRole("list", { name: "Read by" }))).toEqual([
      "Modelweekly_rollup.revenue aggregated",
    ])

    // Following a link chooses that column on its asset.
    await userEvent.click(within(lineage("daily_revenue")).getByRole("button", { name: "Trace weekly_rollup.revenue" }))
    expect(column("weekly_rollup", "revenue")).toHaveAttribute("aria-pressed", "true")
    expect(items(within(lineage("weekly_rollup")).getByRole("list", { name: "Comes from" }))).toEqual([
      "Tableanalytics.daily_revenue.REVENUE aggregated",
    ])
    expect(within(lineage("weekly_rollup")).getByText("No model on this graph reads it.")).toBeInTheDocument()

    // The table daily_revenue builds is its card: back there, with the same column chosen.
    await userEvent.click(within(lineage("weekly_rollup")).getByRole("button", { name: "Trace analytics.daily_revenue.REVENUE" }))
    expect(column("daily_revenue", "revenue")).toHaveAttribute("aria-pressed", "true")

    // A source table: the columns models read from it, filled in by the pipeline that writes it.
    await userEvent.click(within(lineage("daily_revenue")).getByRole("button", { name: "Trace public.orders.amount" }))
    expect(within(within(lineage("public.orders")).getByRole("list", { name: "Columns of public.orders" })).getAllByRole("button").map((b) => b.textContent)).toEqual(
      ["created_at", "amount", "status"],
    )
    expect(column("public.orders", "amount")).toHaveAttribute("aria-pressed", "true")
    expect(within(lineage("public.orders")).getByText("Copied in by orders_sync.")).toBeInTheDocument()
    expect(items(within(lineage("public.orders")).getByRole("list", { name: "Read by" }))).toEqual(["Modeldaily_revenue.revenue aggregated"])

    // Choosing it again stops tracing it.
    await userEvent.click(column("public.orders", "amount"))
    expect(column("public.orders", "amount")).toHaveAttribute("aria-pressed", "false")
    expect(within(lineage("public.orders")).queryByRole("list", { name: "Read by" })).toBeNull()

    // A model whose SQL could not be read says why, and offers nothing to choose.
    await userEvent.click(screen.getByRole("button", { name: "Show the details of scratch_report" }))
    expect(within(lineage("scratch_report")).getByText("Its columns could not be read: the SQL did not parse")).toBeInTheDocument()
    expect(within(lineage("scratch_report")).queryByRole("list")).toBeNull()
  })

  it("has no column section without column lineage, and says when its links were cut short", async () => {
    mockFetch.mockResolvedValueOnce(res(200, BODY))
    const { unmount } = render(<AssetLineageView reloadTick={0} />)
    await userEvent.click(await screen.findByRole("button", { name: "Show the details of daily_revenue" }))
    expect(within(screen.getByRole("region", { name: "Details of daily_revenue" })).queryByRole("group", { name: "Column lineage" })).toBeNull()
    expect(screen.queryByText(/column links/)).toBeNull()
    unmount()

    mockFetch.mockResolvedValueOnce(res(200, { ...WITH_COLUMNS, column_lineage: { ...COLUMNS, truncated: true } }))
    render(<AssetLineageView reloadTick={0} />)
    expect(await screen.findByText(/more than 20,000 column links\. Column lineage stops there/)).toBeInTheDocument()
  })

  it("lights a chosen column's path on the canvas, and the card's own path again once it is put down", async () => {
    media.canvas = true
    const columns = ["created_at", "amount", "status"].map((name) => ({ name, type: "text", is_primary_key: false }))
    mockFetch.mockImplementation(async (url: string) =>
      url.includes("/schema-index")
        ? res(200, { tables: [{ name: "orders", schema: "public", columns }], foreign_keys: [] })
        : res(200, WITH_COLUMNS),
    )
    const { container } = render(<AssetLineageView reloadTick={0} />)
    await waitFor(() => expect(container.querySelectorAll(".react-flow__node")).toHaveLength(8))
    const canvas = container.querySelector(".react-flow")!.parentElement!
    const card = () => within(canvas).getByTitle("public.orders").closest<HTMLElement>(".react-flow__node")!
    const dimmed = (name: string) => within(canvas).getByTitle(name).closest("[data-dimmed]") !== null
    const names = ["orders_sync", "public.orders", "public.customers", "daily_revenue", "weekly_rollup", "events_sync", "public.events", "scratch_report"]

    // (React Flow's cards are not in jsdom's accessibility tree: found by their text.)
    const amount = () => within(card()).getByText("amount").closest("button")!
    fireEvent.click(within(card()).getByTitle("Show the columns of public.orders"))
    await waitFor(() => expect(within(card()).getByText("amount")).toBeInTheDocument())
    expect(amount()).toHaveAttribute("title", "Trace where amount comes from and what reads it")
    fireEvent.click(amount())

    // The table is selected, and its amount column's path lit: the model that sums it, the
    // table that model builds (its card), the model that reads that, and the pipeline.
    await waitFor(() => expect(within(card()).getByText("Selected")).toBeInTheDocument())
    expect(amount()).toHaveAttribute("aria-pressed", "true")
    expect(amount()).toHaveAttribute("title", "Stop tracing amount")
    expect(names.filter(dimmed)).toEqual(["public.customers", "events_sync", "public.events", "scratch_report"])
    expect(
      within(screen.getByRole("region", { name: "Details of public.orders" })).getByRole("button", { name: "amount" }),
    ).toHaveAttribute("aria-pressed", "true")

    // Put down, the selected table lights only what is upstream of it.
    fireEvent.click(amount())
    await waitFor(() => expect(amount()).toHaveAttribute("aria-pressed", "false"))
    expect(names.filter(dimmed)).toEqual(["public.customers", "daily_revenue", "weekly_rollup", "events_sync", "public.events", "scratch_report"])
  })

  it("says while a model's columns are being read, and asks again until they are", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const reading = { ...WITH_COLUMNS, column_lineage: { ...COLUMNS, pending: true, models: COLUMNS.models.slice(1), edges: [] } }
    mockFetch.mockResolvedValueOnce(res(200, reading)).mockResolvedValueOnce(res(200, WITH_COLUMNS))
    render(<AssetLineageView reloadTick={0} />)
    await userEvent.click(await screen.findByRole("button", { name: "Show the details of daily_revenue" }))
    const lineage = () =>
      within(screen.getByRole("region", { name: "Details of daily_revenue" })).getByRole("group", { name: "Column lineage" })
    expect(within(lineage()).getByText("Reading the model's columns…")).toBeInTheDocument()
    expect(mockFetch).toHaveBeenCalledTimes(1)

    await vi.advanceTimersByTimeAsync(15_000)
    await waitFor(() => expect(within(lineage()).getByRole("button", { name: "revenue" })).toBeInTheDocument())
    expect(within(lineage()).queryByText("Reading the model's columns…")).toBeNull()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(mockFetch).toHaveBeenCalledTimes(2)
  })
})
