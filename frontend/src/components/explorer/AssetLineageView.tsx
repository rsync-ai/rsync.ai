"use client"

// The workspace Lineage page: which pipelines write which tables, which models read them,
// and which models are refreshed out of step with what they read. The graph comes whole
// from GET /api/v1/explorer/asset-graph; assetLineage.ts holds the logic, this file draws.
//
// What is drawn is drawnGraph's graph: a model and the one table it builds are one card.
// Clicking a card selects it: its upstream path stays lit and the panel beside the
// drawing says what feeds it, what refreshes it, and opens its Schedule dialog.
// Choosing one of its columns lights that column's path instead (columnLineage.ts).

import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type MouseEvent } from "react"
import Link from "next/link"
import {
  BaseEdge,
  EdgeLabelRenderer,
  MarkerType,
  getBezierPath,
  type Edge,
  type EdgeProps,
  type Node,
  type NodeProps,
} from "@xyflow/react"
import {
  AlertTriangle,
  BarChart3,
  Boxes,
  Columns3,
  GitBranch,
  LayoutDashboard,
  Loader2,
  Search,
  Table2,
  X,
  type LucideIcon,
} from "lucide-react"

import { cn, formatRelativeTime } from "@/lib/utils"
import { authFetch } from "@/lib/api/auth-fetch"
import { listConnections } from "@/lib/api/connections"
import { getConnectorLogoUrl } from "@/lib/api/mcp-connectors"
import { useGraphLayout } from "@/lib/hooks/useGraphLayout"
import { useSavedModels, type SavedModel } from "@/lib/hooks/useSavedModels"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Switch } from "@/components/ui/switch"
import { ConnectorLogo } from "@/components/connectors/ConnectorLogo"
import { modelHref } from "@/components/explorer/modelLineage"
import { SavedQueryModelDialog, type ScheduleUpstream } from "@/components/explorer/SavedQueryModelDialog"
import { LineageHistorySection } from "@/components/explorer/LineageHistorySection"
import {
  ColumnButton,
  ColumnsPanel,
  columnsPanelHeight,
  tablesFor,
  useWarehouseTables,
  type WarehouseTablesCache,
} from "@/components/explorer/AssetColumns"
import {
  CanvasStatus,
  CardHandles,
  GraphListToggle,
  LegendLine,
  LineageFlowCanvas,
  nodeHandles,
  useCanvasCapable,
  useEdgeColors,
} from "@/components/explorer/LineageFlowCanvas"
import {
  ASSET_NODE_HEIGHT,
  ASSET_NODE_WIDTH,
  MAX_DRAWN_ASSETS,
  assetHref,
  attentionSplit,
  describeAsset,
  displaysDownstream,
  drawnGraph,
  foldLooseAssets,
  followsItsInputs,
  inputUpstreams,
  isDisplay,
  kindLabel,
  layoutAssets,
  needsAttention,
  neighbourhood,
  finishedReading,
  parseAssetGraph,
  refreshChip,
  searchAssets,
  stageColumns,
  stillReading,
  triggerLabel,
  upstreamPath,
  warehouseIds,
  warehouseLabel,
  wholeGraph,
  withoutDisplays,
  type AssetEdge,
  type AssetGraph,
  type AssetNode,
  type AssetView,
  type AttentionItem,
  type ColumnLineageModel,
  type DrawnGraph,
  type FoldedView,
  type FoldKind,
  type ModelRefresh,
  type RefreshTone,
  type StageColumn,
} from "@/components/explorer/assetLineage"
import {
  columnKey,
  columnLineageOf,
  columnLinks,
  columnPath,
  modelsOf,
  pickableColumns,
  rowsShapedBy,
  subtypeWords,
  type ColumnLink,
  type ColumnPath,
  type ColumnRef,
} from "@/components/explorer/columnLineage"
import { LayerBadge, PipelineTableBadge, layerPosition } from "@/components/explorer/modelLayer"
import { useWorkspaceLayers } from "@/lib/hooks/useWorkspaceLayers"
import type { WorkspaceLayer } from "@/lib/api/modelLayers"

const ASSET_GRAPH_URL = "/api/v1/explorer/asset-graph"

function assetGraphUrl(warehouse: string | null): string {
  return warehouse ? `${ASSET_GRAPH_URL}?connection_id=${encodeURIComponent(warehouse)}` : ASSET_GRAPH_URL
}

/**
 * While a warehouse's external tables or a model's columns are first being read, the
 * graph is asked for again this often, this many times: as long as the backend gives
 * either read (lake_external_tables.go externalTableRefreshTimeout and
 * asset_column_lineage.go columnLineageTimeout, 3 minutes each).
 */
const PENDING_POLL_MS = 15_000
const PENDING_POLLS = 12

/** Attention items shown before "Show all". */
const ATTENTION_PREVIEW = 5

/**
 * Each kind's colour, from the chart palette so it holds in dark mode: a card's left
 * edge, the legend's swatch and the overview map's box are all the same one.
 */
const KIND_COLOR: Record<string, string> = {
  pipeline: "hsl(var(--chart-series-6))",
  table: "hsl(var(--chart-other))",
  model: "hsl(var(--chart-series-3))",
  chart: "hsl(var(--chart-series-7))",
  dashboard: "hsl(var(--chart-series-1))",
}

const kindColor = (kind: string) => KIND_COLOR[kind] ?? KIND_COLOR.table

const KIND_ICON: Record<string, LucideIcon> = {
  pipeline: GitBranch,
  table: Table2,
  model: Boxes,
  chart: BarChart3,
  dashboard: LayoutDashboard,
}

/** The id of the attention list, which the "Not refreshed" tile links to. */
const ATTENTION_ID = "lineage-attention"

type LoadResult = { key: string; warehouse: string | null; graph: AssetGraph } | { key: string; error: string }

const NO_NAMES: ReadonlyMap<string, string> = new Map()

/** Each connection's name and connector type, by id: who the warehouses are, and their logos. */
interface ConnectionsById {
  names: ReadonlyMap<string, string>
  types: ReadonlyMap<string, string>
}

const NO_CONNECTIONS: ConnectionsById = { names: NO_NAMES, types: NO_NAMES }

const NOTHING_OPEN: ReadonlySet<string> = new Set()

/** Hops the reader can draw on each side of a focused asset. */
const DEPTHS = [1, 2, 3, Infinity]

/** How the drawing's columns are made: one per stage and workspace layer, or by what runs first. */
type ColumnsMode = "layers" | "order"

/** What a link's colour says: whether it keeps a model fresh, or how the backend knows it. */
type LinkMode = "refresh" | "evidence"

/** The Schedule dialog, open on one model. `preset` is what "Rebuild after its inputs" picks. */
interface ScheduleOpen {
  modelId: string
  preset?: { upstreams: ScheduleUpstream[] }
}

/**
 * initialFocus is an asset id to open focused on (?focus= on the Lineage page, such as
 * "model:<id>" from a chart's lineage badge); an id the graph has not falls back to the
 * workspace view.
 */
export function AssetLineageView({ reloadTick, initialFocus }: { reloadTick: number; initialFocus?: string | null }) {
  const [retryTick, setRetryTick] = useState(0)
  const [result, setResult] = useState<LoadResult | null>(null)
  const [focusId, setFocusId] = useState<string | null>(initialFocus || null)
  // The asset whose details the panel shows and whose upstream path stays lit.
  const [selectedId, setSelectedAsset] = useState<string | null>(initialFocus || null)
  // A column of the selected asset, whose path is lit instead. Selecting anything drops it.
  const [pickedColumn, setPickedColumn] = useState<ColumnRef | null>(null)
  const setSelectedId = useCallback((id: string | null) => {
    setSelectedAsset(id)
    setPickedColumn(null)
  }, [])
  const [query, setQuery] = useState("")
  const [view, setView] = useState<"graph" | "list">("graph")
  // The warehouse (connection id) the backend narrows the graph to; null is all of them.
  const [warehouse, setWarehouse] = useState<string | null>(null)
  const [depth, setDepth] = useState(1)
  // The assets whose next hop the reader opened, for one focus at one depth.
  const [expansion, setExpansion] = useState<{ key: string; ids: ReadonlySet<string> }>({ key: "", ids: NOTHING_OPEN })
  // null until the reader picks: then Layers when the workspace has layers, Run order when not.
  const [columnsPick, setColumnsPick] = useState<ColumnsMode | null>(null)
  const [linkMode, setLinkMode] = useState<LinkMode>("refresh")
  // Charts and dashboards are drawn after the models they show, until the reader hides them.
  const [showDisplays, setShowDisplays] = useState(true)
  const [schedule, setSchedule] = useState<ScheduleOpen | null>(null)
  const tables = useWarehouseTables()
  const { layers } = useWorkspaceLayers()
  const { saved, reload: reloadSaved } = useSavedModels(reloadTick)
  const graphRef = useRef<HTMLDivElement>(null)
  const canvasCapable = useCanvasCapable()

  const loadKey = `${reloadTick}|${retryTick}|${warehouse ?? ""}`
  const loading = result?.key !== loadKey

  useEffect(() => {
    let cancelled = false
    const url = assetGraphUrl(warehouse)
    ;(async () => {
      try {
        const res = await authFetch(url, { method: "GET" })
        if (cancelled) return
        if (!res.ok) {
          setResult({ key: loadKey, error: `Could not load the lineage graph (HTTP ${res.status}).` })
          return
        }
        const graph = parseAssetGraph(await res.json())
        if (!cancelled) setResult({ key: loadKey, warehouse, graph })
      } catch {
        if (!cancelled) setResult({ key: loadKey, error: "Could not reach the server to load the lineage graph." })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [loadKey, warehouse])

  // A warehouse whose external tables are being read for the first time has no lake
  // links yet. The graph is asked for again, quietly, until the read is done: nothing
  // dims, a failed ask is let go, and the graph is only replaced once it has them.
  const [polls, setPolls] = useState({ key: "", n: 0 })
  const pollCount = polls.key === loadKey ? polls.n : 0
  const pending =
    result !== null && result.key === loadKey && "graph" in result && stillReading(result.graph) ? result.graph : null
  useEffect(() => {
    if (!pending || pollCount >= PENDING_POLLS) return
    let cancelled = false
    const timer = setTimeout(async () => {
      try {
        const res = await authFetch(assetGraphUrl(warehouse), { method: "GET" })
        if (cancelled || !res.ok) return
        const graph = parseAssetGraph(await res.json())
        if (!cancelled && finishedReading(pending, graph)) setResult({ key: loadKey, warehouse, graph })
      } catch {
        // The next ask, or the reader's own reload, tries again.
      } finally {
        if (!cancelled) setPolls({ key: loadKey, n: pollCount + 1 })
      }
    }, PENDING_POLL_MS)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [pending, pollCount, loadKey, warehouse])

  // A failed reload keeps the graph it had: the error is said above it, not instead of it.
  const [lastGraph, setLastGraph] = useState<AssetGraph | null>(null)
  if (result && "graph" in result && result.graph !== lastGraph) setLastGraph(result.graph)
  const loaded = result && "graph" in result ? result.graph : lastGraph
  const hasDisplays = loaded?.nodes.some((n) => isDisplay(n.kind)) ?? false
  // What the page draws, lists and searches. Hiding charts keeps the summary's counts:
  // withoutDisplays leaves the stats as they were.
  const graph = useMemo(() => (loaded && !showDisplays ? withoutDisplays(loaded) : loaded), [loaded, showDisplays])
  const error = !loading && result && "error" in result ? result.error : null

  // The filter offers every warehouse the last unfiltered load had, so picking one does not
  // take the others off the list.
  const [unfiltered, setUnfiltered] = useState<AssetGraph | null>(null)
  if (result && "graph" in result && result.warehouse === null && result.graph !== unfiltered) setUnfiltered(result.graph)
  const warehouses = useMemo(() => (unfiltered ? warehouseIds(unfiltered) : []), [unfiltered])
  const multiWarehouse = warehouses.length >= 2

  // Warehouse names and logos come from the connection list. If it fails, each warehouse is
  // shown by a short id and a card has no logo: the filter still works.
  const [connections, setConnections] = useState<ConnectionsById>(NO_CONNECTIONS)
  useEffect(() => {
    let cancelled = false
    listConnections()
      .then(({ connections }) => {
        if (cancelled) return
        const all = connections ?? []
        setConnections({
          names: new Map(all.map((c) => [c.id, c.name])),
          types: new Map(all.filter((c) => c.connector_type).map((c) => [c.id, c.connector_type])),
        })
      })
      .catch(() => {})
    return () => {
      cancelled = true
    }
  }, [reloadTick])
  const names = connections.names

  // Each asset is labelled with its warehouse only while more than one is drawn.
  const warehouseNames = useMemo(
    () => (multiWarehouse && warehouse === null ? new Map(warehouses.map((id) => [id, warehouseLabel(id, names)])) : null),
    [multiWarehouse, warehouse, warehouses, names],
  )
  const scopeName = warehouse ? `Everything in ${warehouseLabel(warehouse, names)}` : "Whole workspace"
  const warehouseFilter = (multiWarehouse || warehouse !== null) && (
    <WarehouseFilter warehouses={warehouses} names={names} value={warehouse} onChange={setWarehouse} />
  )

  // Every asset by id, the tables merged into their model's card included: names are looked up here.
  const byId = useMemo(() => new Map((graph?.nodes ?? []).map((n) => [n.id, n])), [graph])
  const drawn = useMemo(() => (graph ? drawnGraph(graph) : null), [graph])
  const cards = useMemo(() => new Set((drawn?.graph.nodes ?? []).map((n) => n.id)), [drawn])
  const modelByNode = useMemo(() => new Map((graph?.models ?? []).map((m) => [m.node_id, m])), [graph])
  const attention = useMemo(() => (graph ? needsAttention(graph) : []), [graph])
  const attentionIds = useMemo(() => new Set(attention.map((a) => a.model.node_id)), [attention])

  // An asset's card: a table drawn as the model that builds it is that model's. An id the
  // reloaded graph no longer has falls back to none.
  const cardOf = (id: string | null) => {
    const card = id ? (drawn?.builtBy.get(id) ?? id) : null
    return card && cards.has(card) ? card : null
  }
  const focus = cardOf(focusId)
  const selected = cardOf(selectedId)
  // What is drawn: a new one starts from its own first view.
  const signature = `${warehouse ?? ""}|${focus ?? ""}|${focus ? depth : ""}`
  const expanded = expansion.key === signature ? expansion.ids : NOTHING_OPEN
  const expand = useCallback(
    (id: string) =>
      setExpansion((prev) => ({ key: signature, ids: new Set([...(prev.key === signature ? prev.ids : NOTHING_OPEN), id]) })),
    [signature],
  )
  const assetView: AssetView | null = useMemo(() => {
    if (!drawn) return null
    if (focus) return neighbourhood(drawn.graph, focus, { depth, expanded })
    return drawn.graph.nodes.length <= MAX_DRAWN_ASSETS ? wholeGraph(drawn.graph) : null
  }, [drawn, focus, depth, expanded])

  const showInGraph = (id: string) => {
    setFocusId(id)
    setSelectedId(id)
    setQuery("")
    graphRef.current?.scrollIntoView?.({ behavior: "smooth", block: "start" })
  }

  // The summary's callouts point at the attention list, which the panel shows when nothing is selected.
  const showAttention = (e: MouseEvent) => {
    if (!selected) return
    e.preventDefault()
    setSelectedId(null)
    requestAnimationFrame(() =>
      document.getElementById(ATTENTION_ID)?.scrollIntoView?.({ behavior: "smooth", block: "start" }),
    )
  }

  // A card named in the details panel may not be drawn: the drawing then moves to it.
  const selectCard = (id: string) => {
    const card = cardOf(id)
    setSelectedId(card ?? id)
    if (card && !assetView?.nodeIds.includes(card)) setFocusId(card)
  }

  // The chosen column, while the asset it is on is the selected one.
  const chosenColumn = pickedColumn && selected && cardOf(pickedColumn.node) === selected ? pickedColumn : null
  const columnTrace = useMemo(() => (graph && chosenColumn ? columnPath(graph, chosenColumn) : null), [graph, chosenColumn])
  // The cards on its path: a table drawn as the model that builds it is that model's.
  const columnLit = useMemo(
    () => (columnTrace && drawn ? new Set([...columnTrace.nodes].map((id) => drawn.builtBy.get(id) ?? id)) : null),
    [columnTrace, drawn],
  )
  // Choosing a column selects its asset, moving the drawing to it as selectCard does;
  // choosing the chosen one again stops tracing it.
  const pickColumn = (ref: ColumnRef) => {
    if (columnTrace?.chosen.has(columnKey(ref.node, ref.column))) {
      setPickedColumn(null)
      return
    }
    const card = cardOf(ref.node)
    setSelectedAsset(card ?? ref.node)
    setPickedColumn(ref)
    if (card && !assetView?.nodeIds.includes(card)) setFocusId(card)
  }
  // A graph without column lineage (a backend from before it) leaves a column's name as text.
  const columnPick: ColumnPick | null = graph?.column_lineage
    ? { chosen: chosenColumn, trace: columnTrace, pick: pickColumn }
    : null

  const retry = () => setRetryTick((t) => t + 1)

  if (!graph || !drawn) {
    return (
      <Card className="p-0">
        {error ? (
          <div className="flex flex-col items-center gap-2 px-4 py-10 text-center">
            <p role="alert" className="text-sm text-red-600 dark:text-red-400">
              {error}
            </p>
            <Button size="sm" variant="outline" onClick={retry}>
              Retry
            </Button>
          </div>
        ) : (
          <div className="flex items-center justify-center gap-2 py-10 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            Loading the lineage graph…
          </div>
        )}
      </Card>
    )
  }

  if (graph.nodes.length === 0) {
    return (
      <div className="space-y-4">
        {warehouseFilter}
        <Card className="px-4 py-10 text-center">
          <p className="text-sm text-zinc-600 dark:text-zinc-300">
            {warehouse ? "Nothing in this warehouse to draw." : "Nothing to draw yet."}
          </p>
          <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            Lineage appears once a pipeline has written a table, or a saved query is scheduled as a model.
          </p>
        </Card>
      </div>
    )
  }

  const effectiveView = canvasCapable ? view : "list"
  const focusNode = focus ? byId.get(focus) : undefined
  const focusHref = focusNode ? assetHref(focusNode) : null
  const selectedNode = selected ? byId.get(selected) : undefined
  const columnsMode: ColumnsMode = columnsPick ?? (layers && layers.length > 0 ? "layers" : "order")
  const drawsCanvas = effectiveView === "graph" && assetView !== null && assetView.nodeIds.length > 1
  const scheduleRow = schedule ? saved.get(schedule.modelId) : undefined
  const drawnCount = drawn.graph.nodes.length
  const linkCount = drawn.graph.edges.length

  return (
    <div className="space-y-4">
      {error && (
        <div className="flex flex-wrap items-center gap-2 text-sm text-red-600 dark:text-red-400">
          <p role="alert">{error} Showing the graph from the last load.</p>
          <Button size="sm" variant="outline" className="h-7" onClick={retry}>
            Retry
          </Button>
        </div>
      )}

      {warehouseFilter}

      <LineageSummary graph={graph} attention={attention} onShowAttention={showAttention} />

      <Card ref={graphRef} className="scroll-mt-4 p-0">
        <div className="flex flex-wrap items-start justify-between gap-3 border-b px-4 py-3 dark:border-zinc-800">
          <div className="min-w-0">
            <h2 className="text-sm font-semibold text-zinc-900 dark:text-white">
              {focusNode ? (
                <>
                  What feeds <span className="font-mono">{focusNode.name}</span>, and what it feeds
                </>
              ) : (
                scopeName
              )}
            </h2>
            <p className="text-xs text-zinc-500 dark:text-zinc-400">
              {assetView && focusNode
                ? `${shownOf(assetView.upstreamShown, assetView.upstreamFound)} upstream · ${shownOf(assetView.downstreamShown, assetView.downstreamFound)} downstream`
                : `${drawnCount} ${drawnCount === 1 ? "asset" : "assets"} · ${linkCount} ${linkCount === 1 ? "link" : "links"}`}
            </p>
            {focusNode && (
              <div className="mt-1 flex flex-wrap gap-2">
                {focusHref && (
                  <Link href={focusHref} className="text-xs font-medium text-blue-600 hover:underline dark:text-blue-400">
                    Open {kindLabel(focusNode.kind).toLowerCase()}
                  </Link>
                )}
                <button
                  type="button"
                  className="text-xs font-medium text-zinc-600 hover:underline dark:text-zinc-300"
                  onClick={() => setFocusId(null)}
                >
                  {drawnCount <= MAX_DRAWN_ASSETS ? (warehouse ? "Show the whole warehouse" : "Show the whole workspace") : "Clear"}
                </button>
              </div>
            )}
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <AssetSearch graph={graph} warehouseNames={warehouseNames} query={query} onQuery={setQuery} onPick={showInGraph} />
            {focusNode && assetView && assetView.upstreamFound + assetView.downstreamFound > 0 && (
              <DepthPicker value={depth} onChange={setDepth} />
            )}
            {canvasCapable && assetView && assetView.nodeIds.length > 1 && (
              <GraphListToggle label="Show lineage as" value={view} onChange={setView} />
            )}
          </div>
        </div>

        {(drawsCanvas || hasDisplays) && (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b px-4 py-2 dark:border-zinc-800">
            {drawsCanvas && (
              <>
                <Choice
                  label="Arrange by"
                  value={columnsMode}
                  onChange={setColumnsPick}
                  options={[
                    { value: "layers", text: "Layers", hint: "A column per stage: pipelines, their tables, then each layer in your workspace's order" },
                    { value: "order", text: "Run order", hint: "Columns by what runs before what" },
                  ]}
                />
                <Choice
                  label="Colour links by"
                  value={linkMode}
                  onChange={setLinkMode}
                  options={[
                    { value: "refresh", text: "Freshness", hint: "Colours each read by whether it refreshes the model" },
                    { value: "evidence", text: "How we know", hint: "Draws each link by how rsync.ai knows it: seen in a run, configured, or read from SQL" },
                  ]}
                />
              </>
            )}
            {hasDisplays && <DisplaysSwitch checked={showDisplays} onChange={setShowDisplays} />}
          </div>
        )}

        <LayerOrderContext.Provider value={layers}>
          <NarrowedContext.Provider value={warehouse !== null}>
          <ColumnPickContext.Provider value={columnPick}>
            <div className="flex flex-col @4xl:flex-row">
              <div className={cn("min-w-0 flex-1", loading && "opacity-60")} aria-busy={loading}>
                <GraphNotices graph={graph} view={assetView} />
                {!assetView ? (
                  <PickPrompt graph={drawn.graph} attention={attention} onPick={showInGraph} />
                ) : assetView.nodeIds.length === 1 ? (
                  <p className="px-4 py-10 text-center text-sm text-zinc-600 dark:text-zinc-300">
                    Nothing in this {warehouse ? "warehouse" : "workspace"} feeds it or reads from it.
                  </p>
                ) : effectiveView === "graph" ? (
                  <AssetCanvas
                    graph={graph}
                    drawn={drawn}
                    view={assetView}
                    byId={byId}
                    modelByNode={modelByNode}
                    attentionIds={attentionIds}
                    warehouseNames={warehouseNames}
                    connectorTypes={connections.types}
                    saved={saved}
                    scopeName={scopeName}
                    signature={signature}
                    tables={tables}
                    columnsMode={columnsMode}
                    linkMode={linkMode}
                    layers={layers}
                    selected={selected}
                    columnLit={columnLit}
                    onSelect={setSelectedId}
                    onExpand={expand}
                  />
                ) : (
                  <AssetList
                    graph={graph}
                    drawn={drawn}
                    view={assetView}
                    byId={byId}
                    modelByNode={modelByNode}
                    attentionIds={attentionIds}
                    warehouseNames={warehouseNames}
                    tables={tables}
                    selected={selected}
                    onFocus={showInGraph}
                    onSelect={setSelectedId}
                    onExpand={expand}
                  />
                )}
              </div>
              {/* Beside the drawing on a wide screen, as tall as it and scrolling on its own; under it on a narrow one. */}
              <aside className="relative border-t @4xl:w-80 @4xl:shrink-0 @4xl:border-l @4xl:border-t-0 dark:border-zinc-800">
                <div className="@4xl:absolute @4xl:inset-0 @4xl:overflow-y-auto">
                  {selectedNode ? (
                    <AssetDetails
                      key={selectedNode.id}
                      node={selectedNode}
                      graph={graph}
                      drawn={drawn}
                      byId={byId}
                      model={modelByNode.get(selectedNode.id)}
                      savedRow={savedRowOf(modelByNode.get(selectedNode.id), saved)}
                      attention={attention.find((a) => a.model.node_id === selectedNode.id)}
                      warehouse={warehouseOf(selectedNode, warehouseNames)}
                      isFocus={selectedNode.id === focus}
                      impact={selectedNode.kind === "model" ? displaysDownstream(loaded ?? graph, selectedNode.id) : []}
                      reloadTick={reloadTick}
                      onSelect={selectCard}
                      onFocus={(id) => {
                        setFocusId(id)
                        setSelectedId(id)
                      }}
                      onSchedule={setSchedule}
                      onClose={() => setSelectedId(null)}
                    />
                  ) : (
                    <AttentionPanel items={attention} cards={cards} onShow={showInGraph} />
                  )}
                </div>
              </aside>
            </div>
          </ColumnPickContext.Provider>
          </NarrowedContext.Provider>
        </LayerOrderContext.Provider>
      </Card>

      {schedule && scheduleRow && (
        <SavedQueryModelDialog
          key={`edit-schedule-${scheduleRow.id}`}
          savedQueryId={scheduleRow.id}
          savedQueryName={scheduleRow.name}
          materialization={scheduleRow.materialization}
          targetTable={scheduleRow.target_table}
          layer={scheduleRow.layer}
          statementClass={scheduleRow.statement_class}
          supportsMaterialization={scheduleRow.supports_materialization}
          connectorType={scheduleRow.connector_type}
          connectionId={scheduleRow.connection_id}
          queryLanguage={scheduleRow.query_language}
          lastRunStatus={scheduleRow.last_run_status}
          lastRunError={scheduleRow.last_run_error}
          // Held in state, so the dialog sees one preset however often this renders.
          preset={schedule.preset}
          open
          onOpenChange={(next) => {
            if (!next) setSchedule(null)
          }}
          onChanged={() => {
            // A new schedule changes what wakes the model: its links and the attention list.
            retry()
            reloadSaved()
          }}
        />
      )}
    </div>
  )
}

/** A model's saved query, for its last run and its Schedule dialog; none for anything else. */
function savedRowOf(model: ModelRefresh | undefined, saved: ReadonlyMap<string, SavedModel>): SavedModel | undefined {
  return model ? saved.get(model.model_id) : undefined
}

/** "3" when every one is drawn, "2 of 3" when some are not. */
function shownOf(shown: number, found: number): string {
  return shown === found ? `${found}` : `${shown} of ${found}`
}

function DepthPicker({ value, onChange }: { value: number; onChange: (depth: number) => void }) {
  return (
    <div role="group" aria-label="Hops drawn on each side" className="flex items-center gap-1">
      <span className="text-xs text-zinc-500 dark:text-zinc-400" aria-hidden>
        Hops
      </span>
      {DEPTHS.map((d) => (
        <Button
          key={d}
          size="sm"
          variant={value === d ? "secondary" : "ghost"}
          className="h-7 min-w-7 px-2 text-xs"
          aria-pressed={value === d}
          aria-label={d === Infinity ? "Every hop" : `${d} ${d === 1 ? "hop" : "hops"}`}
          onClick={() => onChange(d)}
        >
          {d === Infinity ? "All" : d}
        </Button>
      ))}
    </div>
  )
}

function WarehouseFilter({
  warehouses,
  names,
  value,
  onChange,
}: {
  warehouses: string[]
  names: ReadonlyMap<string, string>
  value: string | null
  onChange: (id: string | null) => void
}) {
  const id = useId()
  const options = warehouses
    .map((w) => ({ id: w, label: warehouseLabel(w, names) }))
    .sort((a, b) => a.label.localeCompare(b.label))
  return (
    <div className="flex items-center gap-2">
      <label htmlFor={id} className="text-xs font-medium text-zinc-600 dark:text-zinc-300">
        Warehouse
      </label>
      <select
        id={id}
        className="h-7 max-w-64 rounded-md border border-zinc-300 bg-white px-2 text-xs focus:outline-none focus:ring-2 focus:ring-blue-500 dark:border-zinc-700 dark:bg-zinc-900"
        value={value ?? ""}
        onChange={(e) => onChange(e.target.value || null)}
      >
        <option value="">All warehouses</option>
        {options.map((o) => (
          <option key={o.id} value={o.id}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  )
}

/** The warehouse an asset is in, when more than one is drawn; null otherwise. */
function warehouseOf(node: AssetNode, names: ReadonlyMap<string, string> | null): string | null {
  return (names && node.connection_id && names.get(node.connection_id)) || null
}

/**
 * A captioned row of buttons that picks one of a few: how the drawing is arranged, and how
 * its links are coloured. The buttons sit in one bordered pill and the picked one is filled,
 * so the row reads as a control and its caption as a label.
 */
function Choice<T extends string>({
  label,
  value,
  onChange,
  options,
}: {
  label: string
  value: T
  onChange: (value: T) => void
  options: { value: T; text: string; hint: string }[]
}) {
  return (
    <div role="group" aria-label={label} className="flex items-center gap-2">
      <span className="select-none text-xs text-zinc-500 dark:text-zinc-400" aria-hidden>
        {label}:
      </span>
      <div className="inline-flex rounded-md border border-zinc-200 bg-zinc-100 p-0.5 dark:border-zinc-700 dark:bg-zinc-800">
        {options.map((o) => (
          <Button
            key={o.value}
            size="sm"
            variant="ghost"
            className={cn(
              "h-6 rounded px-2.5 text-xs",
              value === o.value
                ? "bg-white text-zinc-900 shadow-sm hover:bg-white dark:bg-zinc-950 dark:text-white dark:hover:bg-zinc-950"
                : "text-zinc-600 hover:bg-zinc-200 dark:text-zinc-400 dark:hover:bg-zinc-700",
            )}
            aria-pressed={value === o.value}
            title={o.hint}
            onClick={() => onChange(o.value)}
          >
            {o.text}
          </Button>
        ))}
      </div>
    </div>
  )
}

/** Draws or hides the charts and dashboards after the models they show. */
function DisplaysSwitch({ checked, onChange }: { checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <label
      className="flex cursor-pointer items-center gap-2 text-xs text-zinc-600 dark:text-zinc-300"
      title="Draws each chart after the model it shows, and each dashboard after its charts"
    >
      <Switch checked={checked} onCheckedChange={onChange} />
      Charts &amp; dashboards
    </label>
  )
}

function countOf(n: number, one: string, many: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`
}

/** What the workspace has, on one line, and what needs a look, each linking to the list that names them. */
function LineageSummary({
  graph,
  attention,
  onShowAttention,
}: {
  graph: AssetGraph
  attention: AttentionItem[]
  onShowAttention: (e: MouseEvent) => void
}) {
  const { stats } = graph
  const { notRefreshed, untraced } = attentionSplit(attention)
  const callout =
    "inline-flex items-center gap-1.5 rounded-full bg-amber-50 px-2.5 py-0.5 text-xs font-medium text-amber-800 hover:bg-amber-100 dark:bg-amber-950 dark:text-amber-300 dark:hover:bg-amber-900"
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2" data-lineage-summary>
      <p className="text-sm text-zinc-600 dark:text-zinc-300">
        {[
          countOf(stats.pipelines, "pipeline", "pipelines"),
          countOf(stats.tables, "table", "tables"),
          countOf(stats.models, "model", "models"),
          stats.charts > 0 && countOf(stats.charts, "chart", "charts"),
          stats.dashboards > 0 && countOf(stats.dashboards, "dashboard", "dashboards"),
        ]
          .filter(Boolean)
          .join(" · ")}
      </p>
      {notRefreshed.length > 0 && (
        <a href={`#${ATTENTION_ID}`} className={callout} onClick={onShowAttention}>
          <AlertTriangle className="h-3.5 w-3.5" aria-hidden />
          {notRefreshed.length === 1
            ? "1 isn't refreshed by what it reads"
            : `${notRefreshed.length} aren't refreshed by what they read`}
        </a>
      )}
      {untraced.length > 0 && (
        <a href={`#${ATTENTION_ID}`} className={callout} onClick={onShowAttention}>
          <AlertTriangle className="h-3.5 w-3.5" aria-hidden />
          {`${untraced.length}${notRefreshed.length > 0 ? " more" : ""} ${untraced.length === 1 ? "reads" : "read"} a table we can't trace`}
        </a>
      )}
    </div>
  )
}

/** The panel with nothing selected: the models that need a look. */
function AttentionPanel({
  items,
  cards,
  onShow,
}: {
  items: AttentionItem[]
  cards: ReadonlySet<string>
  onShow: (nodeId: string) => void
}) {
  const [expanded, setExpanded] = useState(false)
  if (items.length === 0) {
    return (
      <div id={ATTENTION_ID} className="px-4 py-6 text-center">
        <p className="text-sm text-zinc-600 dark:text-zinc-300">Nothing needs attention</p>
        <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
          Click a card to see what feeds it and what refreshes it.
        </p>
      </div>
    )
  }
  const shown = expanded ? items : items.slice(0, ATTENTION_PREVIEW)
  return (
    <section id={ATTENTION_ID} className="scroll-mt-20">
      <div className="border-b px-4 py-3 dark:border-zinc-800">
        <h2 className="flex items-center gap-1.5 text-sm font-semibold text-zinc-900 dark:text-white">
          <AlertTriangle className="h-4 w-4 text-amber-500" aria-hidden />
          Needs attention{" "}
          <span className="font-normal text-zinc-500 dark:text-zinc-400">({items.length})</span>
        </h2>
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          Models whose refresh does not follow the data they read, or whose SQL reads a table nothing here writes. Click
          a card to see what feeds it.
        </p>
      </div>
      <ul aria-label="Models that need attention" className="divide-y dark:divide-zinc-800">
        {shown.map(({ model, node, reasons }) => (
          <li key={model.model_id} data-attention={model.model_id} className="space-y-1.5 px-4 py-3">
            <div className="flex flex-wrap items-center gap-2">
              <Link href={modelHref(model.model_id)} className="text-sm font-medium text-zinc-900 hover:underline dark:text-white">
                {model.name || node?.name || model.model_id}
              </Link>
              <Badge variant="outline" className="text-[11px] font-normal">
                {triggerLabel(model.trigger)}
              </Badge>
              {model.trigger_paused && (
                <Badge variant="warning" className="text-[11px] font-normal">
                  Paused
                </Badge>
              )}
            </div>
            <ul className="list-disc space-y-0.5 pl-4 text-xs text-zinc-600 dark:text-zinc-300">
              {reasons.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
            {cards.has(model.node_id) && (
              <Button
                size="sm"
                variant="outline"
                className="h-7 text-xs"
                aria-label={`Show ${model.name || model.model_id} in the graph`}
                onClick={() => onShow(model.node_id)}
              >
                Show in graph
              </Button>
            )}
          </li>
        ))}
      </ul>
      {items.length > ATTENTION_PREVIEW && (
        <div className="border-t px-4 py-2 dark:border-zinc-800">
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => setExpanded((e) => !e)}>
            {expanded ? "Show fewer" : `Show all ${items.length}`}
          </Button>
        </div>
      )}
    </section>
  )
}

const CHIP_TONE: Record<RefreshTone, string> = {
  ok: "bg-violet-50 text-violet-800 dark:bg-violet-950 dark:text-violet-300",
  warn: "bg-amber-50 text-amber-800 dark:bg-amber-950 dark:text-amber-300",
  plain: "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300",
}

/** What refreshes a model, as a chip: violet when it follows what it reads, amber when it does not. */
function RefreshChip({ label, tone }: { label: string; tone: RefreshTone }) {
  return (
    <span
      className={cn("inline-flex shrink-0 items-center rounded-full px-1.5 text-[10px] font-medium leading-4", CHIP_TONE[tone])}
      data-refresh-chip={tone}
    >
      {label}
    </span>
  )
}

/** A model's last run, as its saved query has it; null when it has not run or its saved query is not listed. */
function lastRunOf(row: SavedModel | undefined): { ago: string; failed: boolean } | null {
  const ago = formatRelativeTime(row?.last_run_at, { empty: "" })
  return ago ? { ago, failed: row?.last_run_status === "failed" } : null
}

/** How a link reads from the selected asset's side. */
const INTO_TEXT: Record<string, string> = {
  writes: "writes it",
  materializes: "builds it",
  reads: "it reads this",
  triggers: "wakes it when it finishes",
  external: "holds the files it reads",
  draws: "it is drawn from this",
  shows: "it shows this",
}

const OUT_TEXT: Record<string, string> = {
  writes: "it writes this",
  materializes: "it builds this",
  reads: "reads it",
  triggers: "it wakes this when it finishes",
  external: "reads its files",
  draws: "is drawn from it",
  shows: "shows it",
}

/** Whether a read keeps its model fresh, from either end of it. */
function refreshTag(e: AssetEdge, into: boolean): { text: string; stale: boolean } | null {
  if (e.kind !== "reads" || !e.refresh) return null
  if (e.refresh === "wakes") return { text: into ? "keeps it fresh" : "kept fresh by it", stale: false }
  return { text: into ? "doesn't refresh it" : "not refreshed by it", stale: true }
}

function Relations({
  title,
  edges,
  into,
  byId,
  onSelect,
}: {
  title: string
  edges: AssetEdge[]
  into: boolean
  byId: ReadonlyMap<string, AssetNode>
  onSelect: (id: string) => void
}) {
  return (
    <div>
      <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
        {title} ({edges.length})
      </h3>
      {edges.length === 0 ? (
        <p className="text-xs text-zinc-500 dark:text-zinc-400">Nothing drawn.</p>
      ) : (
        <ul aria-label={title} className="space-y-1">
          {edges.map((e) => {
            const other = byId.get(into ? e.from : e.to)
            if (!other) return null
            const tag = refreshTag(e, into)
            return (
              <li key={`${e.from}>${e.to}:${e.kind}`} className="text-xs" data-relation={e.kind}>
                <button
                  type="button"
                  className="inline-flex max-w-full items-center gap-1 font-mono text-zinc-900 hover:underline dark:text-white"
                  aria-label={`Select ${other.name}`}
                  onClick={() => onSelect(other.id)}
                >
                  <KindIcon kind={other.kind} />
                  <span className="truncate">{other.name}</span>
                </button>{" "}
                <span className="text-zinc-500 dark:text-zinc-400">{(into ? INTO_TEXT : OUT_TEXT)[e.kind] ?? e.kind}</span>
                {tag && (
                  <>
                    {" · "}
                    <span className={tag.stale ? "text-amber-700 dark:text-amber-400" : "text-violet-700 dark:text-violet-300"}>
                      {tag.text}
                    </span>
                  </>
                )}
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

/** The panel with a card selected: what it is, what refreshes it, what it is linked to, and what to do about it. */
function AssetDetails({
  node,
  graph,
  drawn,
  byId,
  model,
  savedRow,
  attention,
  warehouse,
  isFocus,
  impact,
  reloadTick,
  onSelect,
  onFocus,
  onSchedule,
  onClose,
}: {
  node: AssetNode
  graph: AssetGraph
  drawn: DrawnGraph
  byId: ReadonlyMap<string, AssetNode>
  model: ModelRefresh | undefined
  savedRow: SavedModel | undefined
  attention: AttentionItem | undefined
  warehouse: string | null
  isFocus: boolean
  /** The charts and dashboards that change when it does, drawn or not. */
  impact: AssetNode[]
  reloadTick: number
  onSelect: (id: string) => void
  onFocus: (id: string) => void
  onSchedule: (open: ScheduleOpen) => void
  onClose: () => void
}) {
  const layers = useContext(LayerOrderContext)
  const narrowed = useContext(NarrowedContext)
  const builtId = drawn.builds.get(node.id)
  const built = builtId ? byId.get(builtId) : undefined
  const href = assetHref(node)
  const chip = model ? refreshChip(model) : null
  const run = lastRunOf(savedRow)
  const upstreams = model ? inputUpstreams(model, byId) : []
  const into = drawn.graph.edges.filter((e) => e.to === node.id)
  const out = drawn.graph.edges.filter((e) => e.from === node.id)
  return (
    <section aria-label={`Details of ${node.name}`} className="space-y-4 px-4 py-3">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <KindAndLayer node={node} layers={layers} />
            {warehouse && <span className="text-[11px] text-zinc-500 dark:text-zinc-400">{warehouse}</span>}
          </div>
          <h2 className="break-all font-mono text-sm font-semibold text-zinc-900 dark:text-white">{node.name}</h2>
          {built && (
            <p className="break-all text-xs text-zinc-600 dark:text-zinc-300">
              Builds <span className="font-mono">{built.name}</span>
            </p>
          )}
          <p className="text-xs text-zinc-500 dark:text-zinc-400">{describeAsset(node, graph, model, narrowed)}</p>
        </div>
        <Button size="icon-sm" variant="ghost" className="h-7 w-7 shrink-0" aria-label="Close details" onClick={onClose}>
          <X className="h-4 w-4" aria-hidden />
        </Button>
      </div>

      {model && (chip || run || attention) && (
        <div className="space-y-1.5">
          <div className="flex flex-wrap items-center gap-2">
            {chip && <RefreshChip {...chip} />}
            {run && (
              <span className={cn("text-xs", run.failed ? "text-red-600 dark:text-red-400" : "text-zinc-500 dark:text-zinc-400")}>
                {run.failed ? `Last run failed ${run.ago}` : `Last ran ${run.ago}`}
              </span>
            )}
          </div>
          {run?.failed && savedRow?.last_run_error && (
            <p className="break-words text-xs text-red-600 dark:text-red-400">{savedRow.last_run_error}</p>
          )}
          {attention && (
            <ul aria-label="Why it needs attention" className="list-disc space-y-0.5 pl-4 text-xs text-amber-800 dark:text-amber-300">
              {attention.reasons.map((r) => (
                <li key={r}>{r}</li>
              ))}
            </ul>
          )}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        {model &&
          savedRow &&
          upstreams.length > 0 &&
          (followsItsInputs(model) ? (
            <span className="text-xs text-violet-700 dark:text-violet-300">Already rebuilds after its inputs</span>
          ) : (
            <Button
              size="sm"
              className="h-7 text-xs"
              title="Opens its schedule with what it reads picked, so it runs when they do"
              onClick={() => onSchedule({ modelId: model.model_id, preset: { upstreams } })}
            >
              Rebuild after its inputs
            </Button>
          ))}
        {model && savedRow && (
          <Button size="sm" variant="outline" className="h-7 text-xs" onClick={() => onSchedule({ modelId: model.model_id })}>
            Edit schedule
          </Button>
        )}
        {href && (
          <Button asChild size="sm" variant="outline" className="h-7 text-xs">
            <Link href={href}>Open {kindLabel(node.kind).toLowerCase()}</Link>
          </Button>
        )}
        {!isFocus && (
          <Button size="sm" variant="ghost" className="h-7 text-xs" onClick={() => onFocus(node.id)}>
            Show only its neighbours
          </Button>
        )}
      </div>

      <Relations title="What feeds it" edges={into} into byId={byId} onSelect={onSelect} />
      <Relations title="What it feeds" edges={out} into={false} byId={byId} onSelect={onSelect} />
      <ColumnLineageSection node={node} graph={graph} byId={byId} onSelect={onSelect} />
      <LineageHistorySection node={node} byId={byId} reloadTick={reloadTick} onSelect={onSelect} />
      {impact.length > 0 && <Impact displays={impact} />}
    </section>
  )
}

/** Column names shown before "Show all". */
const COLUMN_CHIPS = 24

/** What a model's answer says about itself, when it is not simply ok. */
function columnLineageNote(answer: ColumnLineageModel): string | null {
  const why = answer.reason ? `: ${answer.reason}` : "."
  switch (answer.status) {
    case "ok":
      return answer.truncated ? "It has more column links than are kept, so some are missing." : null
    case "partial":
      return `Some of its columns could not be traced${why}`
    case "unsupported":
      return `Its columns are not traced${why}`
    default:
      return `Its columns could not be read${why}`
  }
}

/**
 * The selected asset's columns, to choose one; for the chosen one, where its value comes
 * from and what reads it; and the columns that only shape the model's rows, listed apart.
 */
function ColumnLineageSection({
  node,
  graph,
  byId,
  onSelect,
}: {
  node: AssetNode
  graph: AssetGraph
  byId: ReadonlyMap<string, AssetNode>
  onSelect: (id: string) => void
}) {
  const pick = useContext(ColumnPickContext)
  const [showAll, setShowAll] = useState(false)
  if (!graph.column_lineage || !pick || (node.kind !== "model" && node.kind !== "table")) return null
  const models = modelsOf(graph, node.id)
  const answers = models.flatMap((m) => columnLineageOf(graph, m) ?? [])
  const reading = graph.column_lineage.pending && models.some((m) => !columnLineageOf(graph, m))
  const notes = answers.flatMap((a) => {
    const note = columnLineageNote(a)
    if (!note) return []
    const who = models.length > 1 ? `${byId.get(a.node_id)?.name ?? "A model"}: ` : ""
    return [{ id: a.node_id, text: who + note }]
  })
  const columns = pickableColumns(graph, node.id)
  const shapedBy = rowsShapedBy(graph, node.id)
  const chosen = pick.chosen
  if (columns.length === 0 && !reading && notes.length === 0) return null
  const shown = showAll ? columns : columns.slice(0, COLUMN_CHIPS)
  const title = "Column lineage"
  return (
    <div aria-label={title} role="group">
      <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">{title}</h3>
      <div className="space-y-2 text-xs">
        {reading && (
          <p className="flex items-center gap-1.5 text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            Reading the model&apos;s columns…
          </p>
        )}
        {notes.map((n) => (
          <p key={n.id} className="break-words text-amber-800 dark:text-amber-300">
            {n.text}
          </p>
        ))}
        {columns.length > 0 && (
          <>
            <p className="text-zinc-500 dark:text-zinc-400">Choose a column to light where it comes from and what reads it.</p>
            <ul aria-label={`Columns of ${node.name}`} className="flex flex-wrap gap-1">
              {shown.map((c) => (
                <li key={c} className="flex min-w-0 max-w-full">
                  <ColumnButton
                    name={c}
                    chosen={pick.trace?.chosen.has(columnKey(node.id, c)) ?? false}
                    onPath={false}
                    onPick={(column) => pick.pick({ node: node.id, column })}
                    className="border py-0.5 text-[11px] dark:border-zinc-700"
                  />
                </li>
              ))}
            </ul>
            {columns.length > shown.length && (
              <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setShowAll(true)}>
                Show all {columns.length}
              </Button>
            )}
          </>
        )}
        {chosen && <ChosenColumn chosen={chosen} graph={graph} byId={byId} onPick={pick.pick} onSelect={onSelect} />}
        {shapedBy.length > 0 && (
          <ColumnLinkList
            title="Its rows are filtered, joined or grouped by"
            links={shapedBy}
            byId={byId}
            onPick={pick.pick}
            onSelect={onSelect}
          />
        )}
      </div>
    </div>
  )
}

/** The chosen column's links, one step each way. */
function ChosenColumn({
  chosen,
  graph,
  byId,
  onPick,
  onSelect,
}: {
  chosen: ColumnRef
  graph: AssetGraph
  byId: ReadonlyMap<string, AssetNode>
  onPick: (ref: ColumnRef) => void
  onSelect: (id: string) => void
}) {
  const links = columnLinks(graph, chosen)
  // A table no model builds gets its columns from what writes it, which copies them.
  const copiers = graph.edges
    .filter((e) => e.to === chosen.node && (e.kind === "writes" || e.kind === "external"))
    .flatMap((e) => byId.get(e.from)?.name ?? [])
  const noSource =
    copiers.length > 0 ? `Copied in by ${copiers.join(", ")}.` : "No column of a table on this graph computes it."
  return (
    <div className="space-y-2 rounded-md border p-2 dark:border-zinc-800">
      <p className="break-all font-mono font-semibold text-zinc-900 dark:text-white">{chosen.column}</p>
      <ColumnLinkList title="Comes from" links={links.from} empty={noSource} byId={byId} onPick={onPick} onSelect={onSelect} />
      <ColumnLinkList
        title="Read by"
        links={links.readBy}
        empty="No model on this graph reads it."
        byId={byId}
        onPick={onPick}
        onSelect={onSelect}
      />
    </div>
  )
}

function ColumnLinkList({
  title,
  links,
  empty,
  byId,
  onPick,
  onSelect,
}: {
  title: string
  links: ColumnLink[]
  empty?: string
  byId: ReadonlyMap<string, AssetNode>
  onPick: (ref: ColumnRef) => void
  onSelect: (id: string) => void
}) {
  return (
    <div>
      <h4 className="mb-0.5 text-[11px] font-medium text-zinc-500 dark:text-zinc-400">{title}</h4>
      {links.length === 0 ? (
        <p className="text-zinc-500 dark:text-zinc-400">{empty}</p>
      ) : (
        <ul aria-label={title} className="space-y-0.5">
          {links.map((l) => {
            const other = l.node ? byId.get(l.node) : undefined
            const label = `${other?.name ?? l.name}${l.column ? `.${l.column}` : ""}`
            return (
              <li key={`${l.node}|${l.name}|${l.column}|${l.subtype}`} data-column-link={l.type}>
                {other ? (
                  <button
                    type="button"
                    className="inline-flex max-w-full items-center gap-1 font-mono text-zinc-900 hover:underline dark:text-white"
                    aria-label={l.column ? `Trace ${label}` : `Select ${other.name}`}
                    onClick={() => (l.column ? onPick({ node: other.id, column: l.column }) : onSelect(other.id))}
                  >
                    <KindIcon kind={other.kind} />
                    <span className="truncate">{label}</span>
                  </button>
                ) : (
                  <span className="break-all font-mono text-zinc-900 dark:text-white">{label}</span>
                )}{" "}
                <span className="text-zinc-500 dark:text-zinc-400">
                  {subtypeWords(l.subtype)}
                  {!other && " · not on this graph"}
                </span>
              </li>
            )
          })}
        </ul>
      )}
    </div>
  )
}

/** A model's charts, and the dashboards they are on, each linking to its page. */
function Impact({ displays }: { displays: AssetNode[] }) {
  const title = "Charts and dashboards it changes"
  return (
    <div>
      <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
        {title} ({displays.length})
      </h3>
      <ul aria-label={title} className="space-y-1">
        {displays.map((d) => (
          <li key={d.id} className="text-xs" data-impact={d.kind}>
            <Link
              href={assetHref(d) ?? "#"}
              className="inline-flex max-w-full items-center gap-1 text-zinc-900 hover:underline dark:text-white"
            >
              <KindIcon kind={d.kind} /> <span className="truncate">{d.name}</span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  )
}

function AssetSearch({
  graph,
  warehouseNames,
  query,
  onQuery,
  onPick,
}: {
  graph: AssetGraph
  warehouseNames: ReadonlyMap<string, string> | null
  query: string
  onQuery: (q: string) => void
  onPick: (id: string) => void
}) {
  const matches = useMemo(() => searchAssets(graph, query), [graph, query])
  const listId = useId()
  // The highlighted match belongs to the query it was chosen for: typing starts at the top.
  const [active, setActive] = useState({ query, index: 0 })
  const index = active.query === query ? Math.min(active.index, matches.length - 1) : 0
  const [open, setOpen] = useState(true)
  const shown = open && query.trim() !== ""
  const optionId = (i: number) => `${listId}-${i}`

  const move = (by: number) => {
    if (matches.length === 0) return
    setOpen(true)
    setActive({ query, index: (index + by + matches.length) % matches.length })
  }

  // As wide as its placeholder needs, at 12px in the system font, with room to spare.
  return (
    <div className="relative w-[21rem] max-w-full">
      <Search className="pointer-events-none absolute left-2 top-2 h-3.5 w-3.5 text-zinc-400" aria-hidden />
      <Input
        type="search"
        role="combobox"
        aria-expanded={shown}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={shown && matches.length > 0 ? optionId(index) : undefined}
        value={query}
        onChange={(e) => {
          setOpen(true)
          onQuery(e.target.value)
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault()
            move(e.key === "ArrowDown" ? 1 : -1)
          } else if (e.key === "Escape") {
            onQuery("")
          } else if (e.key === "Enter" && shown && matches.length > 0) {
            e.preventDefault()
            onPick(matches[index].id)
          }
        }}
        placeholder="Find a pipeline, table, model, chart or dashboard"
        aria-label="Find a pipeline, table, model, chart or dashboard"
        className="h-7 pl-7 text-xs"
      />
      <p role="status" className="sr-only">
        {shown ? `${matches.length} ${matches.length === 1 ? "match" : "matches"}` : ""}
      </p>
      {shown && (
        <div className="absolute right-0 top-8 z-20 w-72 max-w-[calc(100vw-2rem)] rounded-md border bg-white p-1 shadow-lg dark:border-zinc-700 dark:bg-zinc-900">
          {matches.length === 0 ? (
            <p className="px-2 py-1.5 text-xs text-zinc-500 dark:text-zinc-400">Nothing matches.</p>
          ) : (
            <ul id={listId} role="listbox" aria-label="Matches">
              {matches.map((n, i) => (
                <li
                  key={n.id}
                  id={optionId(i)}
                  role="option"
                  aria-selected={i === index}
                  className={cn(
                    "flex cursor-pointer items-baseline gap-2 rounded px-2 py-1.5",
                    i === index ? "bg-zinc-100 dark:bg-zinc-800" : "hover:bg-zinc-50 dark:hover:bg-zinc-800/60",
                  )}
                  // Keeps focus in the box, so the list is not closed by the blur before the click lands.
                  onMouseDown={(e) => e.preventDefault()}
                  onMouseMove={() => i !== index && setActive({ query, index: i })}
                  onClick={() => onPick(n.id)}
                >
                  {/* The spaces are for screen readers, which read the option as one string. */}
                  <span className="shrink-0 text-[10px] uppercase tracking-wide text-zinc-400">{kindLabel(n.kind)}</span>{" "}
                  <span className="truncate font-mono text-xs text-zinc-900 dark:text-white">{n.name}</span>
                  {warehouseOf(n, warehouseNames) && (
                    <>
                      {" "}
                      <span className="ml-auto shrink-0 truncate text-[11px] text-zinc-500 dark:text-zinc-400">
                        {warehouseOf(n, warehouseNames)}
                      </span>
                    </>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  )
}

function GraphNotices({ graph, view }: { graph: AssetGraph; view: AssetView | null }) {
  const narrowed = useContext(NarrowedContext)
  const t = graph.stats.truncated
  const cut = [
    t.pipelines && "500 pipelines",
    t.models && "500 models",
    t.produced_tables && "5,000 written tables",
    t.charts && "1,000 charts",
    t.dashboards && "2,000 charts on dashboards",
  ].filter(Boolean) as string[]
  const omitted = view?.omitted ?? 0
  const reading = graph.external_tables_pending
  const columnsCut = graph.column_lineage?.truncated === true
  if (cut.length === 0 && omitted === 0 && !reading && !columnsCut) return null
  const shown = view ? view.upstreamShown + view.downstreamShown : 0
  return (
    <div className="space-y-2 border-b px-4 py-3 text-xs text-amber-800 dark:border-zinc-800 dark:text-amber-300">
      {cut.length > 0 && (
        <p className="flex items-start gap-1.5">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          This {narrowed ? "warehouse" : "workspace"} has more than {cut.join(", ")}. The graph stops there, so some links
          are missing and a model can look as if nothing feeds it.
        </p>
      )}
      {omitted > 0 && (
        <p className="flex items-start gap-1.5">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          Drawing the nearest {shown} of the {shown + omitted} assets in reach, to keep it readable. {omitted} more{" "}
          {omitted === 1 ? "is" : "are"} not drawn.
        </p>
      )}
      {columnsCut && (
        <p className="flex items-start gap-1.5">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          This {narrowed ? "warehouse" : "workspace"} has more than 20,000 column links. Column lineage stops there, so a
          column&apos;s path can end early.
        </p>
      )}
      {reading && (
        <p className="flex items-start gap-1.5 text-zinc-600 dark:text-zinc-400">
          <Loader2 className="mt-0.5 h-3.5 w-3.5 shrink-0 animate-spin" aria-hidden />
          Reading the external tables of a warehouse for the first time, so the lake pipelines that feed them are not linked
          yet. They are drawn here as soon as the read is done.
        </p>
      )}
    </div>
  )
}

function PickPrompt({
  graph,
  attention,
  onPick,
}: {
  graph: AssetGraph
  attention: AttentionItem[]
  onPick: (id: string) => void
}) {
  // Models first: they are where the lineage questions are. Attention ones lead.
  const flagged = new Set(attention.map((a) => a.model.node_id))
  const picks = graph.nodes
    .filter((n) => n.kind === "model")
    .sort((a, b) => Number(flagged.has(b.id)) - Number(flagged.has(a.id)) || a.name.localeCompare(b.name))
    .slice(0, 6)
  return (
    <div className="px-4 py-8 text-center">
      <p className="text-sm text-zinc-600 dark:text-zinc-300">
        This workspace has {graph.nodes.length.toLocaleString()} assets, too many to draw at once.
      </p>
      <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">Find one above to see what feeds it and what it feeds.</p>
      {picks.length > 0 && (
        <div className="mt-3 flex flex-wrap justify-center gap-2">
          {picks.map((n) => (
            <Button key={n.id} size="sm" variant="outline" className="h-7 font-mono text-xs" onClick={() => onPick(n.id)}>
              {n.name}
            </Button>
          ))}
        </div>
      )}
    </div>
  )
}

/** What a card on the canvas can ask of the page: the drawing is out of reach of its props. */
interface CardActions {
  /** Draws the next hop beyond an asset. `flowId` is its card, which the view keeps still. */
  expand: (id: string, flowId: string) => void
  /** Opens or shuts the columns of `table`: the card's own table, or the one its model builds. */
  toggleColumns: (cardId: string, table: AssetNode, flowId: string) => void
  tables: WarehouseTablesCache
}

const CardActionsContext = createContext<CardActions | null>(null)

/** The chosen column and how to choose one, for every list of columns the page shows. */
interface ColumnPick {
  /** The chosen column, on the selected asset. */
  chosen: ColumnRef | null
  /** Its path, for marking the columns on it. */
  trace: ColumnPath | null
  pick: (ref: ColumnRef) => void
}

const ColumnPickContext = createContext<ColumnPick | null>(null)

/** What makes a table's ColumnsPanel choose columns. */
function pickableColumnsPanel(pick: ColumnPick | null, table: AssetNode) {
  if (!pick) return {}
  return {
    onPickColumn: (column: string) => pick.pick({ node: table.id, column }),
    isChosen: (column: string) => pick.trace?.chosen.has(columnKey(table.id, column)) ?? false,
    isOnPath: (column: string) => pick.trace?.columns.has(columnKey(table.id, column)) ?? false,
  }
}

/** The workspace's layers, so a card and a list row colour a layer by its place in the order. */
const LayerOrderContext = createContext<WorkspaceLayer[] | null>(null)

/**
 * Whether the graph is narrowed to one warehouse. It then lacks the models in the others,
 * so what nothing drawn reads is only unread in this warehouse, not by every model.
 */
const NarrowedContext = createContext(false)

/** Its kind as an icon in the kind's colour, named on hover and to a screen reader. */
function KindIcon({ kind }: { kind: string }) {
  const Icon = KIND_ICON[kind] ?? Table2
  return (
    <span className="inline-flex shrink-0" title={kindLabel(kind)} data-kind-icon={kind}>
      <Icon className="h-3.5 w-3.5" style={{ color: kindColor(kind) }} aria-hidden />
      <span className="sr-only">{kindLabel(kind)}</span>
    </span>
  )
}

/** Its kind's icon, then: a pipeline table names itself; anything else says its layer. */
function KindAndLayer({ node, layers }: { node: AssetNode; layers: WorkspaceLayer[] | null }) {
  return (
    <>
      <KindIcon kind={node.kind} />
      {node.pipeline_table ? (
        <PipelineTableBadge />
      ) : (
        <LayerBadge layer={node.layer} position={layerPosition(layers, node.layer)} />
      )}
    </>
  )
}

/** A table's name as schema, small, then the table, bold; anything else as it is. */
function AssetName({ node }: { node: AssetNode }) {
  const dot = node.kind === "table" ? node.name.lastIndexOf(".") : -1
  return (
    <span className="min-w-0 truncate text-sm text-zinc-900 dark:text-white" title={node.name}>
      {dot > 0 && <span className="text-[11px] text-zinc-500 dark:text-zinc-400">{node.name.slice(0, dot + 1)}</span>}
      <span className="font-semibold">{dot > 0 ? node.name.slice(dot + 1) : node.name}</span>
    </span>
  )
}

/**
 * The List's order inside Tables and Models: what pipelines write first, then each
 * layer in the workspace's order, then a layer the list does not have, then none.
 */
function layerRank(node: AssetNode, layers: WorkspaceLayer[] | null): number {
  if (node.pipeline_table) return -1
  if (!node.layer) return Number.MAX_SAFE_INTEGER
  return layerPosition(layers, node.layer) ?? Number.MAX_SAFE_INTEGER - 1
}

interface AssetNodeData extends Record<string, unknown> {
  node: AssetNode
  /** The table its model builds, when the two are one card; null otherwise. */
  builds: AssetNode | null
  line: string
  warehouse: string | null
  /** The connector type of the connection it is in, for its logo; null when not known. */
  connector: string | null
  selected: boolean
  /** Something is selected and this is not on its upstream path. */
  dimmed: boolean
  flagged: boolean
  /** What refreshes a model; null for anything else. */
  refresh: { label: string; tone: RefreshTone } | null
  /** When a model last ran, as "3h ago"; null when that is not known. */
  lastRun: string | null
  lastRunFailed: boolean
  /** The side of the focus it is on; null for the focus, or with no focus. */
  side: "upstream" | "downstream" | null
  /** Its next hop away from the focus that is not drawn. */
  hidden: number
  /** The height of its open column list; 0 when the list is shut. */
  panel: number
}

type AssetFlowNode = Node<AssetNodeData, "asset">

/** The border-t and py-2 around a card's column list. */
const COLUMNS_FRAME_HEIGHT = 17

/** Card controls: out of the tab order (the canvas is hidden from screen readers; the List has them) and not a drag. */
const IN_CARD = { tabIndex: -1 }

/** How faint a card or link off the selected card's upstream path is. */
const DIMMED_OPACITY = 0.35

function AssetNodeCard({ id, data }: NodeProps<AssetFlowNode>) {
  const { node, builds, line, warehouse, connector, selected, dimmed, flagged, refresh, lastRun, lastRunFailed, side, hidden, panel } =
    data
  const columnsOpen = panel > 0
  const actions = useContext(CardActionsContext)
  const columnPick = useContext(ColumnPickContext)
  const layers = useContext(LayerOrderContext)
  const table = builds ?? (node.kind === "table" ? node : null)
  const stop = (act: () => void) => (e: { stopPropagation: () => void }) => {
    e.stopPropagation()
    act()
  }
  return (
    <div
      title={selected ? undefined : "Show its details"}
      className={cn(
        "relative flex flex-col rounded-md border border-l-4 bg-white shadow-sm transition-opacity dark:border-zinc-700 dark:bg-zinc-900",
        selected ? "cursor-default ring-2 ring-zinc-900/70 dark:ring-white/70" : "cursor-pointer hover:shadow-md",
      )}
      style={{
        width: ASSET_NODE_WIDTH,
        height: ASSET_NODE_HEIGHT + panel,
        borderLeftColor: kindColor(node.kind),
        opacity: dimmed ? DIMMED_OPACITY : 1,
      }}
      data-dimmed={dimmed || undefined}
    >
      <CardHandles top={ASSET_NODE_HEIGHT / 2} />
      {selected && (
        <span className="absolute -top-2 left-2 rounded bg-zinc-900 px-1 text-[10px] font-medium leading-4 text-white dark:bg-white dark:text-zinc-900">
          Selected
        </span>
      )}
      <div
        className={cn("flex shrink-0 flex-col justify-center px-3 py-1.5", !selected && "hover:bg-zinc-50 dark:hover:bg-zinc-800")}
        style={{ height: ASSET_NODE_HEIGHT - 2 }}
      >
        <div className="flex min-w-0 items-center gap-1.5">
          <KindAndLayer node={node} layers={layers} />
          {warehouse && (
            <span className="truncate text-[10px] text-zinc-500 dark:text-zinc-400" title={warehouse}>
              · {warehouse}
            </span>
          )}
          {flagged && (
            <span
              className="inline-flex shrink-0 items-center gap-0.5 rounded-full bg-amber-50 px-1.5 text-[10px] font-medium leading-4 text-amber-800 dark:bg-amber-950 dark:text-amber-300"
              title="Needs attention"
            >
              <AlertTriangle className="h-2.5 w-2.5" aria-hidden />
              {builds ? null : "Needs attention"}
            </span>
          )}
          {table && actions && (
            <button
              type="button"
              {...IN_CARD}
              aria-expanded={columnsOpen}
              title={columnsOpen ? `Hide the columns of ${table.name}` : `Show the columns of ${table.name}`}
              className="nodrag nopan ml-auto inline-flex shrink-0 items-center gap-1 rounded px-1 text-[10px] text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-white"
              onClick={stop(() => actions.toggleColumns(node.id, table, id))}
            >
              <Columns3 className="h-3 w-3" aria-hidden />
              {columnsOpen ? "Hide columns" : "Columns"}
            </button>
          )}
        </div>
        <span className="flex min-w-0 items-center gap-1.5">
          {connector && (
            <ConnectorLogo connectorType={connector} logoUrl={getConnectorLogoUrl(connector)} size="xs" className="shrink-0" />
          )}
          <AssetName node={node} />
        </span>
        {builds && (
          <span className="flex min-w-0 items-center gap-1 text-[11px] leading-4 text-zinc-500 dark:text-zinc-400" title={builds.name}>
            <Table2 className="h-3 w-3 shrink-0" style={{ color: kindColor("table") }} aria-hidden />
            <span className="truncate">
              Builds <span className="font-mono">{builds.name}</span>
            </span>
          </span>
        )}
        {refresh ? (
          <span className="flex min-w-0 items-center gap-1.5 text-[11px] leading-4 text-zinc-500 dark:text-zinc-400">
            <RefreshChip label={refresh.label} tone={refresh.tone} />
            {lastRun && (
              <span className={cn("truncate", lastRunFailed && "text-red-600 dark:text-red-400")}>
                {lastRunFailed ? `failed ${lastRun}` : `ran ${lastRun}`}
              </span>
            )}
          </span>
        ) : (
          <span className="truncate text-[11px] leading-4 text-zinc-500 dark:text-zinc-400" title={line}>
            {line}
          </span>
        )}
      </div>
      {columnsOpen && actions && table && (
        // Reading columns is not asking for the card's details.
        <div
          className="nodrag nopan min-h-0 flex-1 cursor-default overflow-hidden border-t px-3 py-2 dark:border-zinc-800"
          onClick={(e) => e.stopPropagation()}
        >
          <ColumnsPanel
            node={table}
            tables={tablesFor(table, actions.tables)}
            onRetry={() => table.connection_id && actions.tables.load(table.connection_id)}
            inCanvas
            className="h-full"
            {...pickableColumnsPanel(columnPick, table)}
          />
        </div>
      )}
      {hidden > 0 && side && actions && (
        <button
          type="button"
          {...IN_CARD}
          title={`Show ${hidden} more ${side}`}
          className={cn(
            "nodrag nopan absolute z-10 -translate-y-1/2 rounded-full border bg-white px-1.5 text-[10px] font-medium leading-4 text-zinc-700 shadow-sm hover:bg-zinc-100 dark:border-zinc-600 dark:bg-zinc-900 dark:text-zinc-200 dark:hover:bg-zinc-800",
            side === "upstream" ? "-left-3 -translate-x-1/2" : "-right-3 translate-x-1/2",
          )}
          style={{ top: ASSET_NODE_HEIGHT / 2 }}
          onClick={stop(() => actions.expand(node.id, id))}
        >
          +{hidden}
        </button>
      )}
    </div>
  )
}

interface FoldNodeData extends Record<string, unknown> {
  kind: FoldKind
  count: number
  dimmed: boolean
}

type FoldFlowNode = Node<FoldNodeData, "fold">

type DrawnNode = AssetFlowNode | FoldFlowNode

/**
 * What a folded box says it holds, and in a graph narrowed to one warehouse. A narrowed
 * graph keeps the dashboards of the charts it draws, so a chart's box says the same.
 */
const FOLD_TEXT: Record<FoldKind, { title: string; heading: string; holds: string; holdsHere: string }> = {
  table: { title: "Show these tables", heading: "Tables", holds: "tables no model reads", holdsHere: "tables unread here" },
  chart: { title: "Show these charts", heading: "Charts", holds: "charts on no dashboard", holdsHere: "charts on no dashboard" },
}

/** A pipeline's tables that nothing drawn reads, or a model's charts on no dashboard, as one card that opens in place. */
function FoldCard({ data }: NodeProps<FoldFlowNode>) {
  const text = FOLD_TEXT[data.kind]
  const narrowed = useContext(NarrowedContext)
  return (
    <div
      title={text.title}
      className="relative flex cursor-pointer flex-col justify-center rounded-md border border-l-4 border-dashed bg-white px-3 py-2 shadow-sm transition-opacity hover:bg-zinc-50 hover:shadow-md dark:border-zinc-700 dark:bg-zinc-900 dark:hover:bg-zinc-800"
      style={{
        width: ASSET_NODE_WIDTH,
        height: ASSET_NODE_HEIGHT,
        borderLeftColor: kindColor(data.kind),
        opacity: data.dimmed ? DIMMED_OPACITY : 1,
      }}
    >
      <CardHandles top={ASSET_NODE_HEIGHT / 2} />
      <span className="flex items-center gap-1.5 text-[11px] text-zinc-500 dark:text-zinc-400">
        <KindIcon kind={data.kind} />
        {text.heading}
      </span>
      <span className="truncate text-sm font-medium text-zinc-900 dark:text-white">
        {data.count} {narrowed ? text.holdsHere : text.holds}
      </span>
      <span className="truncate text-[11px] text-zinc-500 dark:text-zinc-400">Click to show them</span>
    </div>
  )
}

const nodeTypes = { asset: AssetNodeCard, fold: FoldCard }

const ASSET_HANDLES = nodeHandles(ASSET_NODE_WIDTH, ASSET_NODE_HEIGHT / 2)

/** How an edge is drawn says how the backend knows it. */
const EVIDENCE_DASH: Record<string, string | undefined> = {
  observed: undefined,
  declared: "6 4",
  inferred: "2 4",
}

/** A read that does not refresh its model. */
const STALE_DASH = "6 4"

/** A wake: dotted, so it never reads as data moving. */
const WAKE_DASH = "2 4"

const EDGE_KIND_TEXT: Record<string, string> = {
  writes: "Writes",
  materializes: "Builds",
  reads: "Read by",
  triggers: "Wakes when it finishes",
  external: "Read as an external table",
  draws: "Drawn as a chart",
  shows: "Shown on a dashboard",
}

const EVIDENCE_TEXT: Record<string, string> = {
  observed: "seen in a run",
  declared: "configured",
  inferred: "read from a model's SQL",
}

/** What an edge says when the pointer rests on it. */
export function edgeLabel(e: Pick<AssetEdge, "kind" | "evidence">): string {
  const kind = EDGE_KIND_TEXT[e.kind] ?? e.kind
  const evidence = EVIDENCE_TEXT[e.evidence]
  return evidence ? `${kind} · ${evidence}` : kind
}

/** What an edge says on the canvas: how it is known, then whether it refreshes the model. */
function linkLabel(e: AssetEdge): string {
  const known = edgeLabel(e)
  if (e.kind === "reads" && e.refresh === "wakes") return `${known} · refreshes the model`
  if (e.kind === "reads" && e.refresh === "stale") return `${known} · does not refresh the model`
  if (e.kind === "triggers" && e.refresh !== "wakes") return `${known} · paused`
  return known
}

/**
 * How a link is drawn. "How we know" draws it by its evidence. "Freshness" draws what moves
 * data grey, and colours each read by whether it refreshes the model: violet when what
 * writes the table wakes it, amber and dashed when it does not. A wake is dotted.
 */
function edgeLook(
  e: AssetEdge,
  mode: LinkMode,
  colors: { stroke: string; accent: string; wake: string; stale: string },
): { color: string; dash: string | undefined } {
  if (mode === "evidence") return { color: e.kind === "triggers" ? colors.accent : colors.stroke, dash: EVIDENCE_DASH[e.evidence] }
  if (e.kind === "reads") {
    if (e.refresh === "wakes") return { color: colors.wake, dash: undefined }
    if (e.refresh === "stale") return { color: colors.stale, dash: STALE_DASH }
    return { color: colors.stroke, dash: undefined }
  }
  if (e.kind === "triggers") return { color: e.refresh === "wakes" ? colors.wake : colors.stale, dash: WAKE_DASH }
  return { color: colors.stroke, dash: undefined }
}

/** What the Layers layout says on a read from a model's own or an earlier column to a later one. */
const BACKWARD_NOTE = "Reads a later layer"

interface EvidenceEdgeData extends Record<string, unknown> {
  label: string
  color: string
  /** Shown on the link, always: something the reader should see without hovering. */
  note?: string
  dimmed: boolean
}

type EvidenceFlowEdge = Edge<EvidenceEdgeData, "evidence">

/** A curved edge with a dot at its middle that names the link and how it is known. */
function EvidenceEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
  data,
}: EdgeProps<EvidenceFlowEdge>) {
  const [path, labelX, labelY] = getBezierPath({ sourceX, sourceY, sourcePosition, targetX, targetY, targetPosition })
  return (
    <>
      <BaseEdge id={id} path={path} style={style} markerEnd={markerEnd} />
      {data && (
        <EdgeLabelRenderer>
          <div
            className="nodrag nopan group absolute flex h-4 w-4 items-center justify-center"
            style={{
              transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
              pointerEvents: "all",
              opacity: data.dimmed ? DIMMED_OPACITY : 1,
            }}
          >
            <span
              className="block h-2 w-2 rounded-full border-[1.5px] bg-zinc-50 dark:bg-zinc-950"
              style={{ borderColor: data.color }}
            />
            {data.note && (
              <span
                className="pointer-events-none absolute left-1/2 top-3.5 -translate-x-1/2 whitespace-nowrap rounded-full bg-amber-50 px-1.5 text-[10px] font-medium leading-4 text-amber-800 group-hover:hidden dark:bg-amber-950 dark:text-amber-300"
                data-edge-note
              >
                {data.note}
              </span>
            )}
            <span className="pointer-events-none absolute left-1/2 top-4 z-10 hidden -translate-x-1/2 whitespace-nowrap rounded border bg-white px-1.5 py-0.5 text-[10px] text-zinc-700 shadow-sm group-hover:block dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-200">
              {data.label}
            </span>
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  )
}

const edgeTypes = { evidence: EvidenceEdge }

/** Room above the drawing for the Layers layout's column headings. */
const COLUMN_HEADER_ROOM = 40

/** What one layout is of; the drawing is made from it, so cards and positions always agree. */
interface AssetDrawingInput {
  /** The canvas this drawing belongs to: the page's signature, and how its columns are made. */
  signature: string
  view: AssetView
  folded: FoldedView
  /** Cards whose columns are open → the height their list adds under the card. */
  panels: ReadonlyMap<string, number>
  /** Asset id → card id. Card ids are graph positions, so no asset id is written into the DOM. */
  flowId: Map<string, string>
  /** Each card's column in the Layers layout; null in Run order. */
  bands: Map<string, number> | null
  columns: StageColumn[]
}

function layoutDrawing(input: AssetDrawingInput) {
  return layoutAssets(
    input.folded.nodeIds,
    input.folded.edges,
    (id) => ({ width: ASSET_NODE_WIDTH, height: ASSET_NODE_HEIGHT + (input.panels.get(id) ?? 0) }),
    input.bands ?? undefined,
  )
}

/** What the line over the canvas says about its boxes, and its two buttons, by what the boxes hold. */
function foldText(folded: FoldedView, narrowed: boolean): { text: string; showAll: string; foldAgain: string } {
  const count = (kind: FoldKind) =>
    [...folded.groups.values()].reduce((n, g) => n + (g.kind === kind ? g.members.length : 0), 0)
  const tables = count("table")
  const charts = count("chart")
  const both = folded.kinds.size > 1
  const text =
    folded.groups.size === 0
      ? both
        ? "Every table and chart is drawn."
        : folded.kinds.has("chart")
          ? "Every chart is drawn."
          : "Every table is drawn."
      : [
          tables > 0 &&
            `${tables} tables ${narrowed ? "unread in this warehouse" : "no model reads"} are folded into one box per pipeline.`,
          charts > 0 && `${charts} charts on no dashboard are folded into one box per model.`,
          "Click a box to open it.",
        ]
          .filter(Boolean)
          .join(" ")
  if (both) return { text, showAll: "Show everything", foldAgain: "Fold them again" }
  if (folded.kinds.has("chart")) return { text, showAll: "Show every chart", foldAgain: "Fold loose charts again" }
  return { text, showAll: "Show every table", foldAgain: "Fold unread tables again" }
}

function AssetCanvas({
  graph,
  drawn,
  view,
  byId,
  modelByNode,
  attentionIds,
  warehouseNames,
  connectorTypes,
  saved,
  scopeName,
  signature,
  tables,
  columnsMode,
  linkMode,
  layers,
  selected,
  columnLit,
  onSelect,
  onExpand,
}: {
  graph: AssetGraph
  drawn: DrawnGraph
  view: AssetView
  /** Every asset by id, the tables merged into their model's card included. */
  byId: Map<string, AssetNode>
  modelByNode: Map<string, ModelRefresh>
  attentionIds: Set<string>
  warehouseNames: ReadonlyMap<string, string> | null
  /** Each connection's connector type, for the logo on a card in it. */
  connectorTypes: ReadonlyMap<string, string>
  /** Each model's saved query, for when it last ran. */
  saved: ReadonlyMap<string, SavedModel>
  /** What the unfocused drawing is of: the workspace, or one warehouse of it. */
  scopeName: string
  /** Names what is drawn; the reader's opened boxes and columns last as long as it does. */
  signature: string
  tables: WarehouseTablesCache
  columnsMode: ColumnsMode
  linkMode: LinkMode
  layers: WorkspaceLayer[] | null
  /** The card whose details are open; its upstream path stays lit. */
  selected: string | null
  /** The cards on its chosen column's path, lit instead when it has one. */
  columnLit: ReadonlySet<string> | null
  onSelect: (id: string | null) => void
  onExpand: (id: string) => void
}) {
  const colors = useEdgeColors()
  const narrowed = useContext(NarrowedContext)
  // The boxes of unread tables and loose charts the reader opened, or all of them.
  const [opened, setOpened] = useState<{ signature: string; open: ReadonlySet<string> | "all" }>({
    signature,
    open: NOTHING_OPEN,
  })
  const open = opened.signature === signature ? opened.open : NOTHING_OPEN
  const setOpen = (next: ReadonlySet<string> | "all") => setOpened({ signature, open: next })
  // The cards whose columns are shown.
  const [columns, setColumns] = useState<{ signature: string; open: ReadonlySet<string> }>({ signature, open: NOTHING_OPEN })
  const columnsOpen = columns.signature === signature ? columns.open : NOTHING_OPEN
  // Switching columns draws a new picture, which starts from its own first view.
  const canvasSignature = `${signature}|${columnsMode}`
  // The card the reader last acted on, kept still while the drawing moves around it.
  const [anchor, setAnchor] = useState<{ signature: string; flowId: string } | null>(null)

  const folded = useMemo(() => foldLooseAssets(view, drawn.graph, open), [view, drawn, open])
  // Each open column list as tall as what it shows, so one column does not get ten rows' room.
  const panels = useMemo(() => {
    const heights = new Map<string, number>()
    for (const id of columnsOpen) {
      const card = byId.get(id)
      const table = card && byId.get(drawn.builds.get(id) ?? id)
      if (card && table) heights.set(id, COLUMNS_FRAME_HEIGHT + columnsPanelHeight(table, tablesFor(table, tables)))
    }
    return heights
  }, [columnsOpen, byId, drawn, tables])
  const indexOf = useMemo(() => new Map(graph.nodes.map((n, i) => [n.id, i])), [graph])

  const input: AssetDrawingInput = useMemo(() => {
    const flowId = new Map<string, string>()
    for (const id of folded.nodeIds) {
      const group = folded.groups.get(id)
      const at = indexOf.get(group ? group.from : id)
      if (at !== undefined) flowId.set(id, `${group ? "f" : "n"}${at}`)
    }
    const stage =
      columnsMode === "layers"
        ? stageColumns(
            folded.nodeIds.map((id) => {
              const group = folded.groups.get(id)
              return { id, node: group ? undefined : byId.get(id), count: group?.members.length, kind: group?.kind }
            }),
            layers,
          )
        : null
    return {
      signature: canvasSignature,
      view,
      folded,
      panels,
      flowId,
      bands: stage?.bands ?? null,
      columns: stage?.columns ?? [],
    }
  }, [canvasSignature, columnsMode, view, folded, panels, indexOf, byId, layers])

  const layoutKey = [
    canvasSignature,
    folded.nodeIds.join(","),
    folded.edges.map((e) => `${e.from}>${e.to}:${e.kind}:${e.evidence}`).join(","),
    [...panels].map(([id, height]) => `${id}:${height}`).join(","),
    [...(input.bands ?? [])].map(([id, band]) => `${id}:${band}`).join(","),
  ].join("|")
  const { settled, pending, error } = useGraphLayout(layoutKey, input, layoutDrawing)

  const drawing = useMemo(() => {
    if (!settled) return null
    const { input: laidOut, layout } = settled
    const idByFlowId = new Map([...laidOut.flowId].map(([id, f]) => [f, id]))
    const focusAt = laidOut.view.focus ? layout.positions.get(laidOut.view.focus) : undefined
    const root = focusAt
      ? {
          ...focusAt,
          width: ASSET_NODE_WIDTH,
          height: ASSET_NODE_HEIGHT + (laidOut.panels.get(laidOut.view.focus!) ?? 0),
        }
      : null
    // Each column's heading over its leftmost card, above the drawing.
    const headers = laidOut.bands
      ? laidOut.columns.flatMap((c) => {
          const xs = laidOut.folded.nodeIds
            .filter((id) => laidOut.bands!.get(id) === c.band)
            .flatMap((id) => layout.positions.get(id)?.x ?? [])
          return xs.length > 0 ? [{ ...c, x: Math.min(...xs), y: layout.bounds.y - COLUMN_HEADER_ROOM + 8 }] : []
        })
      : []
    const bounds =
      headers.length > 0
        ? {
            ...layout.bounds,
            y: layout.bounds.y - COLUMN_HEADER_ROOM,
            height: layout.bounds.height + COLUMN_HEADER_ROOM,
          }
        : layout.bounds
    return { ...laidOut, layoutKey: settled.key, idByFlowId, positions: layout.positions, bounds, root, headers }
  }, [settled])

  // The selected card and everything upstream of it, when it is drawn; or, with a column
  // chosen, its column's path, a folded box lit when it holds a card on it.
  const lit = useMemo(() => {
    if (!selected || !drawing?.folded.nodeIds.includes(selected)) return null
    if (!columnLit) return upstreamPath(drawn.graph.edges, selected)
    const out = new Set(columnLit)
    for (const [id, group] of drawing.folded.groups) if (group.members.some((m) => columnLit.has(m))) out.add(id)
    return out
  }, [selected, drawing, drawn, columnLit])

  const nodes: DrawnNode[] = useMemo(
    () =>
      (drawing?.folded.nodeIds ?? []).flatMap((id): DrawnNode[] => {
        const flowId = drawing!.flowId.get(id)
        const position = drawing!.positions.get(id)
        if (!flowId || !position) return []
        const panel = drawing!.panels.get(id) ?? 0
        const dimmed = lit !== null && !lit.has(id)
        const card = {
          id: flowId,
          position,
          width: ASSET_NODE_WIDTH,
          height: ASSET_NODE_HEIGHT + panel,
          selectable: false,
          connectable: false,
          focusable: false,
          handles: ASSET_HANDLES,
        }
        const group = drawing!.folded.groups.get(id)
        if (group) return [{ ...card, type: "fold", data: { kind: group.kind, count: group.members.length, dimmed } }]
        const node = byId.get(id)
        if (!node) return []
        const role = drawing!.view.role.get(id)
        const model = modelByNode.get(id)
        const builtTable = drawn.builds.get(id)
        const lastRun = lastRunOf(savedRowOf(model, saved))
        return [
          {
            ...card,
            type: "asset",
            data: {
              node,
              builds: (builtTable && byId.get(builtTable)) || null,
              line: describeAsset(node, graph, model, narrowed),
              warehouse: warehouseOf(node, warehouseNames),
              connector: (node.connection_id && connectorTypes.get(node.connection_id)) || null,
              selected: id === selected,
              dimmed,
              flagged: attentionIds.has(id),
              refresh: model ? refreshChip(model) : null,
              lastRun: lastRun?.ago ?? null,
              lastRunFailed: lastRun?.failed ?? false,
              side: role === "upstream" || role === "downstream" ? role : null,
              hidden: drawing!.view.hiddenBeyond.get(id) ?? 0,
              panel,
            },
          },
        ]
      }),
    [drawing, lit, byId, drawn, graph, modelByNode, saved, attentionIds, warehouseNames, connectorTypes, selected, narrowed],
  )

  const edges: EvidenceFlowEdge[] = useMemo(
    () =>
      (drawing?.folded.edges ?? []).flatMap((e, i): EvidenceFlowEdge[] => {
        const source = drawing!.flowId.get(e.from)
        const target = drawing!.flowId.get(e.to)
        if (!source || !target) return []
        const { color, dash } = edgeLook(e, linkMode, colors)
        const bands = drawing!.bands
        const backward = bands !== null && e.kind === "reads" && (bands.get(e.from) ?? 0) > (bands.get(e.to) ?? 0)
        const dimmed = lit !== null && !(lit.has(e.from) && lit.has(e.to))
        return [
          {
            id: `e${i}`,
            source,
            target,
            type: "evidence",
            focusable: false,
            selectable: false,
            // null stops React Flow writing "Edge from n0 to n1" as a label; the type says string.
            ariaLabel: null as unknown as string,
            style: {
              stroke: color,
              strokeWidth: 1.5,
              ...(dash ? { strokeDasharray: dash } : {}),
              ...(dimmed ? { opacity: DIMMED_OPACITY } : {}),
            },
            markerEnd: { type: MarkerType.ArrowClosed, color },
            data: { label: linkLabel(e), color, dimmed, ...(backward ? { note: BACKWARD_NOTE } : {}) },
          },
        ]
      }),
    [drawing, linkMode, colors, lit],
  )

  const actions: CardActions = useMemo(
    () => ({
      expand: (id, flowId) => {
        setAnchor({ signature: canvasSignature, flowId })
        onExpand(id)
      },
      toggleColumns: (cardId, table, flowId) => {
        const next = new Set(columnsOpen)
        if (next.has(cardId)) next.delete(cardId)
        else {
          next.add(cardId)
          if (table.connection_id) tables.load(table.connection_id)
        }
        setAnchor({ signature: canvasSignature, flowId })
        setColumns({ signature, open: next })
      },
      tables,
    }),
    [signature, canvasSignature, columnsOpen, tables, onExpand],
  )

  if (!drawing) {
    return (
      <CanvasStatus>
        {error ? (
          <span role="alert">Could not lay out the graph. The List view has the same pipelines, tables and models.</span>
        ) : (
          <>
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            Laying out the graph…
          </>
        )}
      </CanvasStatus>
    )
  }

  const onNodeClick = (flowId: string) => {
    const id = drawing.idByFlowId.get(flowId)
    if (!id) return
    if (drawing.folded.groups.has(id)) {
      setAnchor({ signature: canvasSignature, flowId })
      setOpen(open === "all" ? open : new Set([...open, id]))
    } else onSelect(id)
  }

  const shownEdges = drawing.folded.edges
  const foldNotice = foldText(folded, narrowed)
  const drawnKinds = new Set(nodes.map((n) => (n.type === "asset" ? n.data.node.kind : n.data.kind)))
  const focusName = view.focus ? byId.get(view.focus)?.name : undefined
  const focusFlowId = view.focus ? input.flowId.get(view.focus) : undefined
  const keepFocusStill = () => setAnchor(focusFlowId ? { signature: canvasSignature, flowId: focusFlowId } : null)

  return (
    <>
      {(folded.groups.size > 0 || folded.opened > 0) && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-2 text-xs text-zinc-600 dark:border-zinc-800 dark:text-zinc-300">
          <p>{foldNotice.text}</p>
          {folded.groups.size > 0 && (
            <Button
              size="sm"
              variant="outline"
              className="h-6 px-2 text-xs"
              onClick={() => {
                keepFocusStill()
                setOpen("all")
              }}
            >
              {foldNotice.showAll}
            </Button>
          )}
          {folded.opened > 0 && (
            <Button
              size="sm"
              variant="outline"
              className="h-6 px-2 text-xs"
              onClick={() => {
                keepFocusStill()
                setOpen(NOTHING_OPEN)
              }}
            >
              {foldNotice.foldAgain}
            </Button>
          )}
        </div>
      )}
      <CardActionsContext.Provider value={actions}>
        <LineageFlowCanvas<DrawnNode>
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          edgeTypes={edgeTypes}
          bounds={drawing.bounds}
          root={drawing.root}
          signature={drawing.signature}
          layoutKey={drawing.layoutKey}
          onNodeClick={onNodeClick}
          onPaneClick={() => onSelect(null)}
          description="A drawing of the lineage. The List view has the same pipelines, tables and models, with links, their columns, and each one's details."
          title={focusName ? `What feeds ${focusName}, and what it feeds` : scopeName}
          legend={
            linkMode === "refresh" ? (
              <RefreshLegend
                kinds={drawnKinds}
                edges={shownEdges}
                colors={colors}
                expandable={view.hiddenBeyond.size > 0}
                backward={edges.some((e) => e.data?.note)}
              />
            ) : (
              <AssetLegend
                kinds={drawnKinds}
                evidence={new Set(shownEdges.map((e) => e.evidence))}
                wakeColor={shownEdges.some((e) => e.kind === "triggers") ? colors.accent : null}
                expandable={view.hiddenBeyond.size > 0}
              />
            )
          }
          minimapColor={(n) => kindColor(n.type === "asset" ? n.data.node.kind : n.data.kind)}
          pending={pending}
          anchor={anchor?.signature === canvasSignature ? anchor.flowId : null}
          overlay={
            drawing.headers.length > 0 ? (
              <>
                {drawing.headers.map((h) => (
                  <div
                    key={h.band}
                    data-column-header
                    className="pointer-events-none absolute left-0 top-0 flex items-baseline gap-1.5 text-xs"
                    style={{ transform: `translate(${h.x}px, ${h.y}px)`, width: ASSET_NODE_WIDTH }}
                  >
                    <span className="truncate font-semibold uppercase tracking-wide text-zinc-700 dark:text-zinc-200">
                      {h.label}
                    </span>
                    <span className="shrink-0 tabular-nums text-zinc-500 dark:text-zinc-400">{h.count}</span>
                  </div>
                ))}
              </>
            ) : undefined
          }
        />
      </CardActionsContext.Provider>
    </>
  )
}

/** The kinds of card, as swatches: charts and dashboards only when some are drawn. */
function KindSwatches({ kinds }: { kinds: ReadonlySet<string> }) {
  return (
    <>
      {(["pipeline", "table", "model", "chart", "dashboard"] as const)
        .filter((k) => !isDisplay(k) || kinds.has(k))
        .map((k) => (
          <span key={k} className="inline-flex items-center gap-1.5">
            <span className="h-2.5 w-2.5 rounded-sm" style={{ backgroundColor: kindColor(k) }} data-swatch={k} aria-hidden />
            {kindLabel(k)}
          </span>
        ))}
    </>
  )
}

const LEGEND_CLASS =
  "flex flex-wrap items-center gap-x-4 gap-y-1 border-t px-4 py-2 text-xs text-zinc-500 dark:text-zinc-400 dark:border-zinc-800"

/** The Freshness links' key: only the lines the drawing has. */
function RefreshLegend({
  kinds,
  edges,
  colors,
  expandable,
  backward,
}: {
  kinds: ReadonlySet<string>
  edges: readonly AssetEdge[]
  colors: { stroke: string; wake: string; stale: string }
  expandable: boolean
  backward: boolean
}) {
  const has = (kind: string, refresh?: AssetEdge["refresh"] | "none") =>
    edges.some((e) => e.kind === kind && (refresh === undefined || (refresh === "none" ? !e.refresh : e.refresh === refresh)))
  const moves = has("writes") || has("materializes")
  // A warehouse reading a lake folder's files is a read like any other on this key.
  const plainRead = has("reads", "none") || has("external")
  const charted = has("draws") || has("shows")
  return (
    <div className={LEGEND_CLASS}>
      <KindSwatches kinds={kinds} />
      {(moves || plainRead) && (
        <LegendLine color={colors.stroke} label={plainRead ? (moves ? "writes, builds or reads" : "reads") : "writes or builds"} />
      )}
      {charted && <LegendLine color={colors.stroke} label="drawn as a chart, or shown on a dashboard" />}
      {has("reads", "wakes") && <LegendLine color={colors.wake} label="read, and refreshes the model" />}
      {has("reads", "stale") && <LegendLine color={colors.stale} dash={STALE_DASH} label="read, but doesn't refresh it" />}
      {has("triggers", "wakes") && <LegendLine color={colors.wake} dash={WAKE_DASH} label="wakes it when it finishes" />}
      {has("triggers", "none") && <LegendLine color={colors.stale} dash={WAKE_DASH} label="would wake it, but it's paused" />}
      {backward && (
        <span className="inline-flex items-center gap-1.5">
          <span className="rounded-full bg-amber-50 px-1.5 text-[10px] font-medium leading-4 text-amber-800 dark:bg-amber-950 dark:text-amber-300">
            {BACKWARD_NOTE}
          </span>
          a model reads from a column after its own
        </span>
      )}
      {expandable && <span>+N on a card draws its next hop</span>}
    </div>
  )
}

/** The How-we-know links' key. */
function AssetLegend({
  kinds,
  evidence,
  wakeColor,
  expandable,
}: {
  kinds: ReadonlySet<string>
  evidence: Set<string>
  wakeColor: string | null
  expandable: boolean
}) {
  return (
    <div className={LEGEND_CLASS}>
      <KindSwatches kinds={kinds} />
      {evidence.has("observed") && <LegendLine label={EVIDENCE_TEXT.observed} />}
      {evidence.has("declared") && <LegendLine dash={EVIDENCE_DASH.declared} label={EVIDENCE_TEXT.declared} />}
      {evidence.has("inferred") && <LegendLine dash={EVIDENCE_DASH.inferred} label={EVIDENCE_TEXT.inferred} />}
      {wakeColor && <LegendLine color={wakeColor} label="wakes the model when it finishes" />}
      {expandable && <span>+N on a card draws its next hop</span>}
    </div>
  )
}

function relationText(view: AssetView, id: string): string {
  const role = view.role.get(id)
  const d = view.distance.get(id) ?? 0
  if (role === "focus") return "Focus"
  const side = role === "upstream" ? "upstream" : "downstream"
  return d === 1 ? `Direct ${side}` : `${side.charAt(0).toUpperCase()}${side.slice(1)}, ${d} hops away`
}

function AssetList({
  graph,
  drawn,
  view,
  byId,
  modelByNode,
  attentionIds,
  warehouseNames,
  tables,
  selected,
  onFocus,
  onSelect,
  onExpand,
}: {
  graph: AssetGraph
  drawn: DrawnGraph
  view: AssetView
  byId: Map<string, AssetNode>
  modelByNode: Map<string, ModelRefresh>
  attentionIds: Set<string>
  warehouseNames: ReadonlyMap<string, string> | null
  tables: WarehouseTablesCache
  /** The row whose details are open. */
  selected: string | null
  onFocus: (id: string) => void
  onSelect: (id: string) => void
  onExpand: (id: string) => void
}) {
  // Rows whose columns are shown: a table's own, or the table its model builds.
  const [columnsOpen, setColumnsOpen] = useState<ReadonlySet<string>>(NOTHING_OPEN)
  const panelIds = useId()
  const columnPick = useContext(ColumnPickContext)
  const layers = useContext(LayerOrderContext)
  const narrowed = useContext(NarrowedContext)
  const toggleColumns = (rowId: string, table: AssetNode) => {
    const next = new Set(columnsOpen)
    if (next.has(rowId)) next.delete(rowId)
    else {
      next.add(rowId)
      if (table.connection_id) tables.load(table.connection_id)
    }
    setColumnsOpen(next)
  }

  const rowsOf = view.nodeIds.map((id) => byId.get(id)).filter((n): n is AssetNode => !!n)
  let sections: [string, AssetNode[]][]
  if (view.focus) {
    const byDistance = (a: AssetNode, b: AssetNode) => (view.distance.get(a.id) ?? 0) - (view.distance.get(b.id) ?? 0)
    const ups = rowsOf.filter((n) => view.role.get(n.id) === "upstream").sort(byDistance)
    const downs = rowsOf.filter((n) => view.role.get(n.id) === "downstream").sort(byDistance)
    sections = [
      [`Upstream (${shownOf(ups.length, view.upstreamFound)})`, ups],
      ["Focus", rowsOf.filter((n) => n.id === view.focus)],
      [`Downstream (${shownOf(downs.length, view.downstreamFound)})`, downs],
    ]
  } else {
    const ofKind = (k: string) =>
      rowsOf
        .filter((n) => n.kind === k)
        .sort((a, b) => layerRank(a, layers) - layerRank(b, layers) || a.name.localeCompare(b.name))
    sections = [
      [`Pipelines (${ofKind("pipeline").length})`, ofKind("pipeline")],
      [`Tables (${ofKind("table").length})`, ofKind("table")],
      [`Models (${ofKind("model").length})`, ofKind("model")],
    ]
    // Charts and dashboards only get a section when the workspace has some.
    const charts = ofKind("chart")
    const dashboards = ofKind("dashboard")
    if (charts.length > 0) sections.push([`Charts (${charts.length})`, charts])
    if (dashboards.length > 0) sections.push([`Dashboards (${dashboards.length})`, dashboards])
  }
  return (
    <div className="divide-y dark:divide-zinc-800">
      {sections.map(([heading, rows]) => (
        <section key={heading} className="px-4 py-3">
          <h3 className="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">{heading}</h3>
          {rows.length === 0 ? (
            <p className="text-xs text-zinc-500 dark:text-zinc-400">None.</p>
          ) : (
            <ul className="space-y-2">
              {rows.map((n) => {
                const href = assetHref(n)
                const warehouse = warehouseOf(n, warehouseNames)
                const hidden = view.hiddenBeyond.get(n.id) ?? 0
                const side = view.role.get(n.id)
                const builtTable = drawn.builds.get(n.id)
                const builds = builtTable ? byId.get(builtTable) : undefined
                const table = builds ?? (n.kind === "table" ? n : undefined)
                const showsColumns = columnsOpen.has(n.id) && !!table
                const panelId = `${panelIds}-${view.nodeIds.indexOf(n.id)}`
                const isSelected = n.id === selected
                return (
                  <li
                    key={n.id}
                    data-asset-row
                    data-selected={isSelected || undefined}
                    aria-current={n.id === view.focus ? "true" : undefined}
                    className={cn("border-l-4 py-0.5 pl-3", isSelected && "rounded-r-md bg-zinc-100 dark:bg-zinc-800/70")}
                    style={{ borderLeftColor: kindColor(n.kind) }}
                  >
                    <div className="flex min-w-0 flex-wrap items-center gap-x-2">
                      <KindAndLayer node={n} layers={layers} />
                      {warehouse && <span className="text-[11px] text-zinc-500 dark:text-zinc-400">{warehouse}</span>}
                      {href ? (
                        <Link href={href} className="font-mono text-sm font-medium text-zinc-900 hover:underline dark:text-white">
                          {n.name}
                        </Link>
                      ) : (
                        <span className="font-mono text-sm font-medium text-zinc-900 dark:text-white">{n.name}</span>
                      )}
                      {view.focus && n.id !== view.focus && <span className="text-xs text-zinc-500 dark:text-zinc-400">{relationText(view, n.id)}</span>}
                      {attentionIds.has(n.id) && (
                        <span className="inline-flex items-center gap-1 text-xs text-amber-700 dark:text-amber-400">
                          <AlertTriangle className="h-3 w-3" aria-hidden />
                          Needs attention
                        </span>
                      )}
                      {!isSelected && (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-6 px-2 text-xs"
                          aria-label={`Show the details of ${n.name}`}
                          onClick={() => onSelect(n.id)}
                        >
                          Details
                        </Button>
                      )}
                      {n.id !== view.focus && (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-6 px-2 text-xs"
                          aria-label={`Show what feeds ${n.name} and what it feeds`}
                          onClick={() => onFocus(n.id)}
                        >
                          Focus
                        </Button>
                      )}
                      {hidden > 0 && (side === "upstream" || side === "downstream") && (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-6 px-2 text-xs"
                          aria-label={`Show ${hidden} more ${side} of ${n.name}`}
                          onClick={() => onExpand(n.id)}
                        >
                          +{hidden} {side}
                        </Button>
                      )}
                      {table && (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-6 px-2 text-xs"
                          aria-expanded={showsColumns}
                          aria-controls={showsColumns ? panelId : undefined}
                          aria-label={`Columns of ${table.name}`}
                          onClick={() => toggleColumns(n.id, table)}
                        >
                          <Columns3 className="mr-1 h-3 w-3" aria-hidden />
                          Columns
                        </Button>
                      )}
                    </div>
                    <div className="text-[11px] text-zinc-500 dark:text-zinc-400">
                      {describeAsset(n, graph, modelByNode.get(n.id), narrowed)}
                      {builds && (
                        <>
                          {" · Builds "}
                          <span className="font-mono">{builds.name}</span>
                        </>
                      )}
                    </div>
                    {showsColumns && table && (
                      <div id={panelId} className="mt-2 max-w-md rounded-md border p-2 dark:border-zinc-800">
                        <ColumnsPanel
                          node={table}
                          tables={tablesFor(table, tables)}
                          onRetry={() => table.connection_id && tables.load(table.connection_id)}
                          {...pickableColumnsPanel(columnPick, table)}
                        />
                      </div>
                    )}
                  </li>
                )
              })}
            </ul>
          )}
        </section>
      ))}
    </div>
  )
}
