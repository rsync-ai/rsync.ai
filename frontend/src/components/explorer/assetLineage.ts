/**
 * The workspace asset graph (GET /api/v1/explorer/asset-graph, api-gateway
 * asset_graph.go) and the pure logic the Lineage page draws it with.
 *
 * The graph is derived, not configured. Every edge carries how the backend knows it:
 * `observed` (a run recorded it), `declared` (someone configured it) or `inferred`
 * (parsed out of a model's SQL). The page keeps those apart, because an inferred edge is
 * a suggestion and drawing it like an observed one would overstate what is known.
 */

import { chartSpec } from "@/components/charts/chart-registry"
import { modelHref } from "@/components/explorer/modelLineage"
import type { ScheduleUpstream } from "@/components/explorer/SavedQueryModelDialog"
import { layoutGraph, type GraphLayout } from "@/lib/elk-layout"
import type { SchemaTableLike } from "@/lib/explorer/schemaTree"

export interface AssetNode {
  id: string
  kind: string
  name: string
  ref_id?: string
  connection_id?: string
  /**
   * The name of a model's workspace layer, on the model and on the table it builds.
   * Absent when the model is in none, and on a table a pipeline writes.
   */
  layer?: string
  /** A table a pipeline writes. Not a layer: models are built on these. */
  pipeline_table?: boolean
  /** A chart's type, as its settings name it ("line", "scorecard"). */
  chart_type?: string
  /** A dashboard's charts, drawn or not: some may be on models this graph does not have. */
  charts?: number
}

/** Whether a model runs when what a link carries changes. */
export type EdgeRefresh = "wakes" | "stale"

export interface AssetEdge {
  from: string
  to: string
  kind: string
  evidence: string
  /**
   * Set by drawnGraph only. A read is "wakes" when every producer of the table wakes the
   * model, "stale" when one does not or the model's schedule is paused; a trigger that
   * fires is "wakes". Absent when the graph has no refresh facts to tell by.
   */
  refresh?: EdgeRefresh
}

export interface ModelRefresh {
  model_id: string
  node_id: string
  name: string
  trigger: string
  trigger_upstreams: string[]
  trigger_paused: boolean
  upstreams: string[]
  uncovered_upstreams: string[]
  unresolved: string[]
  ambiguous: boolean
}

export interface AssetGraphStats {
  pipelines: number
  tables: number
  models: number
  charts: number
  dashboards: number
  edges: Record<string, number>
  models_with_uncovered_upstreams: number
  models_with_unresolved_references: number
  models_with_multiple_upstreams: number
  truncated: { pipelines: boolean; produced_tables: boolean; models: boolean; charts: boolean; dashboards: boolean }
}

export interface AssetGraph {
  nodes: AssetNode[]
  edges: AssetEdge[]
  models: ModelRefresh[]
  stats: AssetGraphStats
  /**
   * A warehouse's external tables are being read for the first time, so the lake
   * pipelines that feed it are not linked yet; asking again shortly draws them.
   */
  external_tables_pending: boolean
  /**
   * Which column of a table each model column is computed from (design § 3.4,
   * asset_column_lineage.go). Null when no model has been read yet.
   */
  column_lineage: ColumnLineage | null
}

/** One model's column lineage answer. */
export interface ColumnLineageModel {
  node_id: string
  /** ok, partial (some columns could not be followed), failed or unsupported. */
  status: string
  columns: string[]
  reason: string
  truncated: boolean
}

/**
 * One column link, in OpenLineage's terms. A DIRECT link computes the model's
 * `to_column` from the table's column; an INDIRECT one (`to_column` "") filters, joins,
 * groups, sorts or windows the model's rows by it. `from` is the table's node, or "" when
 * the graph does not have that table; `from_table` names it either way.
 */
export interface ColumnLineageEdge {
  from: string
  from_table: string
  from_column: string
  to: string
  to_column: string
  type: string
  subtype: string
}

export interface ColumnLineage {
  /** A model's columns are being read for the first time; asking again shortly shows them. */
  pending: boolean
  /** The graph's column links stop at a limit. */
  truncated: boolean
  models: ColumnLineageModel[]
  edges: ColumnLineageEdge[]
}

/** More than this and a drawing stops being readable, so the page draws one asset's neighbourhood. */
export const MAX_DRAWN_ASSETS = 80

function arr<T>(v: unknown): T[] {
  return Array.isArray(v) ? (v as T[]) : []
}

function num(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0
}

/**
 * The response, with every list defaulted: a Go nil slice arrives as null, and a
 * missing list must read as empty rather than crash the page.
 */
export function parseAssetGraph(body: unknown): AssetGraph {
  const b = (body ?? {}) as Record<string, unknown>
  const s = (b.stats ?? {}) as Record<string, unknown>
  const t = (s.truncated ?? {}) as Record<string, unknown>
  return {
    nodes: arr<AssetNode>(b.nodes),
    edges: arr<AssetEdge>(b.edges),
    models: arr<Record<string, unknown>>(b.models).map((m) => ({
      model_id: String(m.model_id ?? ""),
      node_id: String(m.node_id ?? ""),
      name: String(m.name ?? ""),
      trigger: String(m.trigger ?? "manual"),
      trigger_upstreams: arr<string>(m.trigger_upstreams),
      trigger_paused: m.trigger_paused === true,
      upstreams: arr<string>(m.upstreams),
      uncovered_upstreams: arr<string>(m.uncovered_upstreams),
      unresolved: arr<string>(m.unresolved),
      ambiguous: m.ambiguous === true,
    })),
    stats: {
      pipelines: num(s.pipelines),
      tables: num(s.tables),
      models: num(s.models),
      charts: num(s.charts),
      dashboards: num(s.dashboards),
      edges: (s.edges && typeof s.edges === "object" ? s.edges : {}) as Record<string, number>,
      models_with_uncovered_upstreams: num(s.models_with_uncovered_upstreams),
      models_with_unresolved_references: num(s.models_with_unresolved_references),
      models_with_multiple_upstreams: num(s.models_with_multiple_upstreams),
      truncated: {
        pipelines: t.pipelines === true,
        produced_tables: t.produced_tables === true,
        models: t.models === true,
        charts: t.charts === true,
        dashboards: t.dashboards === true,
      },
    },
    external_tables_pending: b.external_tables_pending === true,
    column_lineage: parseColumnLineage(b.column_lineage),
  }
}

function parseColumnLineage(v: unknown): ColumnLineage | null {
  if (!v || typeof v !== "object") return null
  const c = v as Record<string, unknown>
  const str = (x: unknown) => (typeof x === "string" ? x : "")
  return {
    pending: c.pending === true,
    truncated: c.truncated === true,
    models: arr<Record<string, unknown>>(c.models).map((m) => ({
      node_id: str(m.node_id),
      status: str(m.status),
      columns: arr<unknown>(m.columns).filter((x): x is string => typeof x === "string"),
      reason: str(m.reason),
      truncated: m.truncated === true,
    })),
    edges: arr<Record<string, unknown>>(c.edges).map((e) => ({
      from: str(e.from),
      from_table: str(e.from_table),
      from_column: str(e.from_column),
      to: str(e.to),
      to_column: str(e.to_column),
      type: str(e.type),
      subtype: str(e.subtype),
    })),
  }
}

/** Whether the backend is still reading something the graph will have once it is done. */
export function stillReading(graph: AssetGraph): boolean {
  return graph.external_tables_pending || graph.column_lineage?.pending === true
}

/** Whether a graph asked for again has finished a read the one drawn was waiting on. */
export function finishedReading(drawn: AssetGraph, again: AssetGraph): boolean {
  return (
    (drawn.external_tables_pending && !again.external_tables_pending) ||
    (drawn.column_lineage?.pending === true && again.column_lineage?.pending !== true)
  )
}

const KIND_LABELS: Record<string, string> = {
  pipeline: "Pipeline",
  table: "Table",
  model: "Model",
  chart: "Chart",
  dashboard: "Dashboard",
}

/** What a model is drawn on: a chart, and the dashboard that shows it. Neither feeds anything. */
export function isDisplay(kind: string): boolean {
  return kind === "chart" || kind === "dashboard"
}

/**
 * The graph without its charts and dashboards, for the page's "Charts & dashboards"
 * switch. Its stats are the workspace's, so the summary still counts them.
 */
export function withoutDisplays(graph: AssetGraph): AssetGraph {
  if (!graph.nodes.some((n) => isDisplay(n.kind))) return graph
  return {
    ...graph,
    nodes: graph.nodes.filter((n) => !isDisplay(n.kind)),
    edges: graph.edges.filter((e) => e.kind !== "draws" && e.kind !== "shows"),
  }
}

/**
 * The charts and dashboards downstream of an asset, however far: what would look
 * different if it changed. Charts first, then dashboards, each by name.
 */
export function displaysDownstream(graph: AssetGraph, id: string): AssetNode[] {
  const out = new Map<string, string[]>()
  for (const e of graph.edges) out.set(e.from, [...(out.get(e.from) ?? []), e.to])
  const byId = new Map(graph.nodes.map((n) => [n.id, n]))
  return [...walk(id, out).keys()]
    .map((d) => byId.get(d))
    .filter((n): n is AssetNode => n !== undefined && isDisplay(n.kind))
    .sort((a, b) => Number(a.kind === "dashboard") - Number(b.kind === "dashboard") || a.name.localeCompare(b.name))
}

export function kindLabel(kind: string): string {
  return KIND_LABELS[kind] ?? kind
}

const TRIGGER_LABELS: Record<string, string> = {
  manual: "Refreshed by hand",
  cron: "Runs on a cron schedule",
  interval: "Runs on an interval",
  after_upstream: "Runs after its upstreams",
}

export function triggerLabel(trigger: string): string {
  return TRIGGER_LABELS[trigger] ?? trigger
}

/**
 * The warehouses the graph's assets live in, as connection ids. A pipeline counts under
 * the warehouse it writes to, which is also what `?connection_id=` filters it by.
 */
export function warehouseIds(graph: AssetGraph): string[] {
  return [...new Set(graph.nodes.flatMap((n) => (n.connection_id ? [n.connection_id] : [])))].sort()
}

/** A warehouse's connection name, or a short id when the connection list has no name for it. */
export function warehouseLabel(id: string, names: ReadonlyMap<string, string>): string {
  return names.get(id) ?? `Warehouse ${id.slice(0, 8)}`
}

/** Where a node's own page is. Tables have none: a table is not a row the product owns. */
export function assetHref(node: AssetNode): string | null {
  if (!node.ref_id) return null
  if (node.kind === "model") return modelHref(node.ref_id)
  if (node.kind === "pipeline") return `/pipelines/${encodeURIComponent(node.ref_id)}`
  if (node.kind === "chart") return `/charts/${encodeURIComponent(node.ref_id)}`
  if (node.kind === "dashboard") return `/dashboards/${encodeURIComponent(node.ref_id)}`
  return null
}

export type AssetRole = "focus" | "upstream" | "downstream"

export interface AssetView {
  /** Drawn nodes: the focus first, then nearest first. */
  nodeIds: string[]
  /** Edges whose ends are both drawn. */
  edges: AssetEdge[]
  focus: string | null
  role: Map<string, AssetRole>
  distance: Map<string, number>
  /** Everything upstream of the focus, however far. */
  upstreamFound: number
  /** Everything downstream of the focus, however far. */
  downstreamFound: number
  upstreamShown: number
  downstreamShown: number
  /** Per drawn node, its next hop away from the focus that is not drawn. */
  hiddenBeyond: Map<string, number>
  /** Within the depth but not drawn, because of the cap. */
  omitted: number
}

export interface NeighbourhoodOptions {
  /** Hops drawn on each side of the focus; Infinity draws them all. */
  depth?: number
  /** Nodes whose next hop is drawn whatever the depth. */
  expanded?: ReadonlySet<string>
  cap?: number
}

/** Hops from `start`, following a node's `next` only while it is within `depth` or expanded. */
function walk(
  start: string,
  next: Map<string, string[]>,
  depth = Infinity,
  expanded: ReadonlySet<string> = new Set(),
): Map<string, number> {
  const dist = new Map<string, number>([[start, 0]])
  let frontier = [start]
  for (let d = 1; frontier.length > 0; d++) {
    const following: string[] = []
    for (const id of frontier) {
      if (d > depth && !expanded.has(id)) continue
      for (const n of next.get(id) ?? []) {
        if (dist.has(n)) continue
        dist.set(n, d)
        following.push(n)
      }
    }
    frontier = following
  }
  dist.delete(start)
  return dist
}

/**
 * What feeds the focus and what it feeds, `depth` hops out on each side plus the next
 * hop of every expanded node, capped at `cap` nodes with the nearest kept. The totals
 * count every hop, drawn or not. A node that is both (a cycle) counts as upstream.
 */
export function neighbourhood(
  graph: AssetGraph,
  focus: string,
  { depth = Infinity, expanded, cap = MAX_DRAWN_ASSETS }: NeighbourhoodOptions = {},
): AssetView {
  const out = new Map<string, string[]>()
  const into = new Map<string, string[]>()
  for (const e of graph.edges) {
    out.set(e.from, [...(out.get(e.from) ?? []), e.to])
    into.set(e.to, [...(into.get(e.to) ?? []), e.from])
  }
  const allUps = walk(focus, into)
  const allDowns = walk(focus, out)
  for (const id of allUps.keys()) allDowns.delete(id)

  const ups = walk(focus, into, depth, expanded)
  const downs = walk(focus, out, depth, expanded)
  for (const id of ups.keys()) downs.delete(id)

  const found = [
    ...[...ups].map(([id, d]) => ({ id, d, role: "upstream" as const })),
    ...[...downs].map(([id, d]) => ({ id, d, role: "downstream" as const })),
  ].sort((a, b) => a.d - b.d || a.id.localeCompare(b.id))
  const kept = found.slice(0, Math.max(0, cap - 1))

  const role = new Map<string, AssetRole>([[focus, "focus"]])
  const distance = new Map<string, number>([[focus, 0]])
  for (const k of kept) {
    role.set(k.id, k.role)
    distance.set(k.id, k.d)
  }
  const nodeIds = [focus, ...kept.map((k) => k.id)]
  const drawn = new Set(nodeIds)
  const hiddenBeyond = new Map<string, number>()
  for (const k of kept) {
    const onward = (k.role === "upstream" ? into : out).get(k.id) ?? []
    const hidden = new Set(onward.filter((id) => !drawn.has(id))).size
    if (hidden > 0) hiddenBeyond.set(k.id, hidden)
  }
  return {
    nodeIds,
    edges: graph.edges.filter((e) => drawn.has(e.from) && drawn.has(e.to)),
    focus,
    role,
    distance,
    upstreamFound: allUps.size,
    downstreamFound: allDowns.size,
    upstreamShown: kept.filter((k) => k.role === "upstream").length,
    downstreamShown: kept.filter((k) => k.role === "downstream").length,
    hiddenBeyond,
    omitted: found.length - kept.length,
  }
}

export function wholeGraph(graph: AssetGraph): AssetView {
  return {
    nodeIds: graph.nodes.map((n) => n.id),
    edges: graph.edges,
    focus: null,
    role: new Map(),
    distance: new Map(),
    upstreamFound: 0,
    downstreamFound: 0,
    upstreamShown: 0,
    downstreamShown: 0,
    hiddenBeyond: new Map(),
    omitted: 0,
  }
}

/** Fewer loose assets than this stay drawn: a box would hide almost nothing. */
export const FOLD_MIN = 3

/** What a box folds: a pipeline's tables nothing drawn reads, or a model's charts on no dashboard. */
export type FoldKind = "table" | "chart"

/** One pipeline's unread tables, or one model's loose charts, drawn as one box. */
export interface FoldGroup {
  id: string
  kind: FoldKind
  /** The pipeline that writes the tables, or the model the charts are drawn from. */
  from: string
  members: string[]
}

export interface FoldedView {
  nodeIds: string[]
  edges: AssetEdge[]
  /** Boxes still folded, keyed by their node id. */
  groups: Map<string, FoldGroup>
  /** Boxes that would fold but are drawn open. */
  opened: number
  /** What the boxes that would fold hold, folded or drawn open. */
  kinds: Set<FoldKind>
}

/** The node id of a pipeline's box of unread tables, or a model's box of loose charts. */
export function foldId(from: string): string {
  return `fold:${from}`
}

const EVIDENCE_STRENGTH = ["inferred", "declared", "observed"]

/** The link into each kind of box: the pipeline writes the tables, the model draws the charts. */
const FOLD_EDGE: Record<FoldKind, string> = { table: "writes", chart: "draws" }

/**
 * A pipeline that writes many tables no model reads stacks them in one column as tall as
 * the page, and the drawing can then only be read by zooming until the words vanish. So
 * the tables only one pipeline writes, and nothing drawn reads, fold into one box per
 * pipeline. A model's charts fold the same way, one box per model, when no dashboard shows
 * them: a chart on a dashboard keeps its own card, drawn or not, as the link to it is
 * what the reader came for. The focused asset is never folded away, nor is the focused
 * pipeline's or model's output: that is what was asked to see. `open` names the boxes
 * drawn open, or "all".
 */
export function foldLooseAssets(
  view: AssetView,
  graph: AssetGraph,
  open: ReadonlySet<string> | "all" = new Set(),
): FoldedView {
  const kinds = new Map(graph.nodes.map((n) => [n.id, n.kind]))
  const into = new Map<string, AssetEdge[]>()
  const readers = new Set<string>()
  for (const e of view.edges) {
    into.set(e.to, [...(into.get(e.to) ?? []), e])
    readers.add(e.from)
  }
  const shown = new Set(graph.edges.filter((e) => e.kind === "shows").map((e) => e.from))
  const loose = new Map<string, { kind: FoldKind; members: string[] }>()
  for (const id of view.nodeIds) {
    const kind = kinds.get(id)
    if (id === view.focus || (kind !== "table" && kind !== "chart") || readers.has(id)) continue
    if (kind === "chart" && shown.has(id)) continue
    const links = into.get(id) ?? []
    const from = links[0]?.from
    if (!from || from === view.focus || kinds.get(from) !== (kind === "table" ? "pipeline" : "model")) continue
    if (!links.every((e) => e.from === from && e.kind === FOLD_EDGE[kind])) continue
    const had = loose.get(from)
    if (had) had.members.push(id)
    else loose.set(from, { kind, members: [id] })
  }

  const groups = new Map<string, FoldGroup>()
  const folded = new Map<string, string>()
  const foldKinds = new Set<FoldKind>()
  let opened = 0
  for (const [from, { kind, members }] of loose) {
    if (members.length < FOLD_MIN) continue
    foldKinds.add(kind)
    const id = foldId(from)
    if (open === "all" || open.has(id)) {
      opened++
      continue
    }
    groups.set(id, { id, kind, from, members })
    for (const m of members) folded.set(m, id)
  }
  if (groups.size === 0) return { nodeIds: view.nodeIds, edges: view.edges, groups, opened, kinds: foldKinds }

  const nodeIds: string[] = []
  for (const id of view.nodeIds) {
    const group = folded.get(id)
    if (!group) nodeIds.push(id)
    else if (groups.get(group)!.members[0] === id) nodeIds.push(group)
  }
  const edges = view.edges.filter((e) => !folded.has(e.to))
  for (const g of groups.values()) {
    // The box's link is only as sure as the least sure link in it.
    const evidence = EVIDENCE_STRENGTH.find((grade) =>
      g.members.some((m) => into.get(m)!.some((e) => e.evidence === grade)),
    )
    edges.push({ from: g.from, to: g.id, kind: FOLD_EDGE[g.kind], evidence: evidence ?? "observed" })
  }
  return { nodeIds, edges, groups, opened, kinds: foldKinds }
}

/** An asset's card on the canvas. */
export const ASSET_NODE_WIDTH = 256
export const ASSET_NODE_HEIGHT = 88

/** Space between assets linked to nothing, and between them and the drawing above. */
const LOOSE_GAP = 20
const LOOSE_TOP_GAP = 56

/**
 * Where each asset goes: linked ones in columns, upstreams on the left. Assets linked to
 * nothing are set in rows under that drawing, as wide as it is; left to the layout they
 * would share its first column and make it as tall as there are of them.
 *
 * With `bands` (stageColumns), every asset sits in its band's columns instead, linked or
 * not, and a link pointing back to an earlier band does not move it.
 */
export async function layoutAssets(
  nodeIds: string[],
  edges: { from: string; to: string }[],
  sizeOf: (id: string) => { width: number; height: number } = () => ({
    width: ASSET_NODE_WIDTH,
    height: ASSET_NODE_HEIGHT,
  }),
  bands?: Map<string, number>,
): Promise<GraphLayout> {
  if (bands) return layoutGraph(nodeIds.map((id) => ({ id, ...sizeOf(id) })), edges, { partitions: bands })
  const linked = new Set(edges.flatMap((e) => [e.from, e.to]))
  const { positions, bounds } = await layoutGraph(
    nodeIds.filter((id) => linked.has(id)).map((id) => ({ id, ...sizeOf(id) })),
    edges,
  )
  const loose = nodeIds.filter((id) => !linked.has(id))
  if (loose.length === 0) return { positions, bounds }

  const sizes = loose.map(sizeOf)
  const step = Math.max(...sizes.map((s) => s.width)) + LOOSE_GAP
  const columns = Math.max(Math.floor((bounds.width + LOOSE_GAP) / step), Math.min(loose.length, 4))
  const drewLinked = positions.size > 0
  let top = drewLinked ? bounds.y + bounds.height + LOOSE_TOP_GAP : 0
  const left = drewLinked ? bounds.x : 0
  for (let row = 0; row * columns < loose.length; row++) {
    const inRow = loose.slice(row * columns, (row + 1) * columns)
    inRow.forEach((id, i) => positions.set(id, { x: left + i * step, y: top }))
    top += Math.max(...sizes.slice(row * columns, (row + 1) * columns).map((s) => s.height)) + LOOSE_GAP
  }
  const looseWidth = Math.min(loose.length, columns) * step - LOOSE_GAP
  const y = drewLinked ? bounds.y : 0
  return {
    positions,
    bounds: {
      x: left,
      y,
      width: Math.max(drewLinked ? bounds.width : 0, looseWidth),
      height: top - LOOSE_GAP - y,
    },
  }
}

export type ColumnsLookup =
  | { found: "one"; table: SchemaTableLike }
  | { found: "none" }
  | { found: "many"; count: number }

/**
 * The warehouse table an asset table node names. The graph names a table
 * `schema.table`, or bare when its producer gave no schema (asset_graph.go
 * splitModelTarget), so a bare name matches in any schema, and can match more than one.
 */
export function findWarehouseTable(assetName: string, tables: SchemaTableLike[]): ColumnsLookup {
  const dot = assetName.lastIndexOf(".")
  const table = assetName.slice(dot + 1).toLowerCase()
  const schema = dot > 0 ? assetName.slice(0, dot).toLowerCase() : null
  const hits = tables.filter(
    (t) => t.name.toLowerCase() === table && (schema === null || (t.schema ?? "").toLowerCase() === schema),
  )
  if (hits.length === 1) return { found: "one", table: hits[0] }
  return hits.length === 0 ? { found: "none" } : { found: "many", count: hits.length }
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}

function nameList(ids: string[], byId: Map<string, AssetNode>): string {
  const names = ids.map((id) => byId.get(id)?.name || id)
  if (names.length <= 3) return names.join(", ")
  return `${names.slice(0, 3).join(", ")} and ${names.length - 3} more`
}

export interface AttentionItem {
  model: ModelRefresh
  node: AssetNode | undefined
  reasons: string[]
}

/**
 * Models whose freshness depends on someone remembering: an upstream that does not
 * refresh them, a table reference nothing produces, or a reference that could be more
 * than one table. The reasons are in words, one per problem.
 */
export function needsAttention(graph: AssetGraph): AttentionItem[] {
  const byId = new Map(graph.nodes.map((n) => [n.id, n]))
  const items: AttentionItem[] = []
  for (const m of graph.models) {
    const reasons: string[] = []
    const uncovered = m.uncovered_upstreams
    if (uncovered.length > 0) {
      const names = nameList(uncovered, byId)
      const verb = uncovered.length === 1 ? "runs" : "run"
      if (m.trigger === "manual") reasons.push(`Reads from ${names}, but is only refreshed by hand.`)
      else if (m.trigger === "after_upstream")
        reasons.push(`Reads from ${names}, but does not refresh when ${uncovered.length === 1 ? "it" : "they"} ${verb}.`)
      else reasons.push(`Reads from ${names}, but runs on a clock rather than when ${uncovered.length === 1 ? "it" : "they"} ${verb}.`)
    }
    if (m.trigger_paused && (uncovered.length > 0 || m.trigger_upstreams.length > 0))
      reasons.push("Its schedule is paused.")
    if (m.unresolved.length > 0)
      reasons.push(
        `Its SQL reads ${m.unresolved.join(", ")}, which no pipeline or model here writes, so where ${m.unresolved.length === 1 ? "that table gets" : "those tables get"} data is unknown.`,
      )
    if (m.ambiguous) reasons.push("A table name in its SQL could be more than one table, so an upstream shown may be the wrong one.")
    if (reasons.length > 0) items.push({ model: m, node: byId.get(m.node_id), reasons })
  }
  return items.sort(
    (a, b) =>
      b.model.uncovered_upstreams.length - a.model.uncovered_upstreams.length ||
      b.model.unresolved.length - a.model.unresolved.length ||
      a.model.name.localeCompare(b.model.name),
  )
}

/**
 * A short line under a node's name. A graph `narrowed` to one warehouse lacks the models in
 * the others, so a table none of its models reads is only unread in this warehouse.
 */
export function describeAsset(
  node: AssetNode,
  graph: AssetGraph,
  model: ModelRefresh | undefined,
  narrowed = false,
): string {
  if (node.kind === "model") {
    const trigger = model ? triggerLabel(model.trigger) : "Model"
    return model?.trigger_paused ? `${trigger} · paused` : trigger
  }
  const outgoing = graph.edges.filter((e) => e.from === node.id)
  if (node.kind === "chart") {
    const type = (node.chart_type && chartSpec(node.chart_type)?.label) || "Chart"
    const on = outgoing.filter((e) => e.kind === "shows").length
    return `${type} · ${on === 0 ? "on no dashboard" : `on ${plural(on, "dashboard", "dashboards")}`}`
  }
  if (node.kind === "dashboard") return `Shows ${plural(node.charts ?? 0, "chart", "charts")}`
  if (node.kind === "pipeline") {
    const writes = outgoing.filter((e) => e.kind === "writes").length
    const triggers = outgoing.filter((e) => e.kind === "triggers").length
    const parts = [`Writes ${plural(writes, "table", "tables")}`]
    if (triggers > 0) parts.push(`wakes ${plural(triggers, "model", "models")}`)
    return parts.join(", ")
  }
  const readers = outgoing.filter((e) => e.kind === "reads").length
  const external = outgoing.filter((e) => e.kind === "external").length
  if (external > 0 && readers === 0) return `A warehouse reads it as ${plural(external, "external table", "external tables")}`
  if (external > 0) return `Read by ${plural(readers, "model", "models")} and ${plural(external, "external table", "external tables")}`
  if (readers > 0) return `Read by ${plural(readers, "model", "models")}`
  return narrowed ? "Nothing in this warehouse reads it" : "No model reads it"
}

/** Names that start with the query first; then models, pipelines, tables, charts and dashboards; then by name. */
export function searchAssets(graph: AssetGraph, query: string, limit = 8): AssetNode[] {
  const q = query.trim().toLowerCase()
  if (!q) return []
  const rank: Record<string, number> = { model: 0, pipeline: 1, table: 2, chart: 3, dashboard: 4 }
  return graph.nodes
    .filter((n) => n.name.toLowerCase().includes(q))
    .sort(
      (a, b) =>
        Number(!a.name.toLowerCase().startsWith(q)) - Number(!b.name.toLowerCase().startsWith(q)) ||
        (rank[a.kind] ?? 5) - (rank[b.kind] ?? 5) ||
        a.name.localeCompare(b.name),
    )
    .slice(0, limit)
}

/**
 * What produces each table: the pipelines that write it and the models that build it.
 * An external table produces nothing itself; its files are another table's (an
 * `external` link from the lake folder a pipeline writes), so that table's producers
 * are its own. The backend gives a model reading it the same pipeline as its upstream
 * (asset_graph.go), which is what lets a refresh check compare the two by node id.
 */
export function tableProducers(edges: readonly AssetEdge[]): Map<string, string[]> {
  const producers = new Map<string, string[]>()
  const add = (table: string, ids: readonly string[]) => {
    const had = producers.get(table) ?? []
    producers.set(table, [...had, ...ids.filter((id) => !had.includes(id))])
  }
  for (const e of edges) if (e.kind === "writes" || e.kind === "materializes") add(e.to, [e.from])
  for (const e of edges) if (e.kind === "external") add(e.to, producers.get(e.from) ?? [])
  return producers
}

/** A table one model reads, and what produces it: the pipelines that write it, the models that build it. */
export interface ModelInput {
  table: AssetNode
  producers: AssetNode[]
}

/** One model, one hop each way: what it reads, what it builds and which models read that. */
export interface ModelSurroundings {
  model: AssetNode
  inputs: ModelInput[]
  builds: AssetNode[]
  readers: AssetNode[]
}

/**
 * The Schedule dialog's "Where it sits" and the Lineage panel's Reads from / Read by, from
 * the graph's own edges. Null when the graph does not have the model (another warehouse's
 * graph, or a list cut short).
 *
 * A producer's node id is `pipeline:<id>` or `model:<id>`, which is what upstreamKey makes
 * of a schedule's upstream, so a caller can ask whether an input wakes the model by id.
 */
export function modelSurroundings(graph: AssetGraph, modelId: string): ModelSurroundings | null {
  const byId = new Map(graph.nodes.map((n) => [n.id, n]))
  const model = byId.get(modelId)
  if (!model || model.kind !== "model") return null
  const nodesOf = (ids: string[]) =>
    [...new Set(ids)].map((id) => byId.get(id)).filter((n): n is AssetNode => n !== undefined)
  const producers = tableProducers(graph.edges)
  const producersOf = (table: string) => nodesOf((producers.get(table) ?? []).filter((id) => id !== modelId))
  const inputs = nodesOf(graph.edges.filter((e) => e.kind === "reads" && e.to === modelId).map((e) => e.from)).map(
    (table) => ({ table, producers: producersOf(table.id) }),
  )
  const builds = nodesOf(graph.edges.filter((e) => e.kind === "materializes" && e.from === modelId).map((e) => e.to))
  const built = new Set(builds.map((b) => b.id))
  const readers = nodesOf(
    graph.edges.filter((e) => e.kind === "reads" && built.has(e.from) && e.to !== modelId).map((e) => e.to),
  )
  return { model, inputs, builds, readers }
}

/** Whether a model's input wakes it: one of the table's producers is among the upstreams that do. */
export function inputWakes(input: ModelInput, waking: ReadonlySet<string>): boolean {
  return input.producers.some((p) => waking.has(p.id))
}

/** The graph as the Lineage page draws it. */
export interface DrawnGraph {
  graph: AssetGraph
  /** A merged model's node id → the node id of the table it builds. */
  builds: Map<string, string>
  /** A merged table's node id → the model it is drawn as. */
  builtBy: Map<string, string>
}

/**
 * The graph the Lineage page draws. A model and the one table it is configured to build
 * are one thing to whoever reads the page, so they are one card, and the table's links
 * become the model's. A table keeps its own card when anything else writes it, when its
 * model builds more than one table, or when the build is only inferred (a statement
 * model's SQL names it): the two are then not the same thing.
 *
 * Each read is marked with whether it keeps the model fresh, from the backend's refresh
 * facts (asset_graph.go): "stale" when a producer of the table is not among what wakes
 * the model, or the model's schedule is paused. A trigger from something the model reads
 * says nothing its read does not, so it is left out; any other trigger "wakes".
 */
export function drawnGraph(graph: AssetGraph): DrawnGraph {
  const kinds = new Map(graph.nodes.map((n) => [n.id, n.kind]))
  const producers = new Map<string, string[]>()
  const made = new Map<string, AssetEdge[]>()
  for (const e of graph.edges) {
    if (e.kind !== "writes" && e.kind !== "materializes") continue
    producers.set(e.to, [...(producers.get(e.to) ?? []), e.from])
    if (e.kind === "materializes") made.set(e.from, [...(made.get(e.from) ?? []), e])
  }
  const builds = new Map<string, string>()
  const builtBy = new Map<string, string>()
  for (const [model, [e, ...more]] of made) {
    if (more.length > 0 || kinds.get(model) !== "model" || e.evidence !== "declared") continue
    if (kinds.get(e.to) !== "table" || producers.get(e.to)!.length !== 1) continue
    builds.set(model, e.to)
    builtBy.set(e.to, model)
  }

  const refreshOf = new Map(graph.models.map((m) => [m.node_id, m]))
  const feeders = tableProducers(graph.edges)
  const seen = new Set<string>()
  const edges: AssetEdge[] = []
  for (const e of graph.edges) {
    if (e.kind === "materializes" && builds.get(e.from) === e.to) continue
    const from = builtBy.get(e.from) ?? e.from
    const to = builtBy.get(e.to) ?? e.to
    if (from === to) continue
    const m = refreshOf.get(e.to)
    let refresh: EdgeRefresh | undefined
    if (m && e.kind === "reads") {
      const by = (feeders.get(e.from) ?? []).filter((p) => p !== e.to)
      if (by.length > 0 && (m.trigger_paused || by.some((p) => m.uncovered_upstreams.includes(p)))) refresh = "stale"
      else if (by.length > 0 && by.every((p) => m.trigger_upstreams.includes(p))) refresh = "wakes"
    } else if (m && e.kind === "triggers") {
      if (m.upstreams.includes(e.from)) continue
      if (!m.trigger_paused) refresh = "wakes"
    }
    const key = `${from}\u0000${to}\u0000${e.kind}`
    if (seen.has(key)) continue
    seen.add(key)
    edges.push(refresh ? { ...e, from, to, refresh } : { ...e, from, to })
  }
  return { graph: { ...graph, nodes: graph.nodes.filter((n) => !builtBy.has(n.id)), edges }, builds, builtBy }
}

/** The asset and everything upstream of it, along the links given. */
export function upstreamPath(edges: readonly AssetEdge[], id: string): Set<string> {
  const into = new Map<string, string[]>()
  for (const e of edges) into.set(e.to, [...(into.get(e.to) ?? []), e.from])
  return new Set([id, ...walk(id, into).keys()])
}

export const PIPELINE_BAND = 0
export const TABLE_BAND = 1

/** One column of the Layers layout. */
export interface StageColumn {
  band: number
  label: string
  /** Assets in it: a folded box counts what it holds. */
  count: number
}

/** A card in the Layers layout: its node, or none for a folded box. */
export interface StageCard {
  id: string
  node: AssetNode | undefined
  /** Assets the card stands for. Defaults to 1. */
  count?: number
  /** A folded box's kind: what it holds. A box without one holds tables. */
  kind?: FoldKind
}

/**
 * The Layers layout: each card's band, left to right. Pipelines, then the tables they
 * write, then one band per workspace layer in the workspace's own order, then models in
 * no layer. A layer the workspace no longer lists (renamed or removed since a model was
 * put in it) is placed after the ones it does, by name. A table with a layer is one a
 * model builds and keeps its own card (drawnGraph says when): it sits with its model.
 * Charts come after every model, and the dashboards that show them last.
 * Only bands with a card in them get a column.
 */
export function stageColumns(
  cards: readonly StageCard[],
  layers: readonly { name: string }[] | null,
): { bands: Map<string, number>; columns: StageColumn[] } {
  const known = new Map((layers ?? []).map((l, i) => [l.name, i]))
  const unknown = [
    ...new Set(
      cards.flatMap((c) =>
        c.node?.layer && c.node.kind !== "pipeline" && !isDisplay(c.node.kind) && !known.has(c.node.layer) ? [c.node.layer] : [],
      ),
    ),
  ].sort((a, b) => a.localeCompare(b))
  const noLayer = 2 + known.size + unknown.length
  const bandOf = (c: StageCard) => {
    const kind = c.node?.kind ?? c.kind
    if (kind === "chart") return noLayer + 1
    if (kind === "dashboard") return noLayer + 2
    const node = c.node
    if (!node) return TABLE_BAND
    if (node.kind === "pipeline") return PIPELINE_BAND
    if (node.layer) return 2 + (known.get(node.layer) ?? known.size + unknown.indexOf(node.layer))
    return node.kind === "table" ? TABLE_BAND : noLayer
  }

  const bands = new Map<string, number>()
  const members = new Map<number, StageCard[]>()
  for (const c of cards) {
    const band = bandOf(c)
    bands.set(c.id, band)
    members.set(band, [...(members.get(band) ?? []), c])
  }
  const names = [...known.keys(), ...unknown]
  const labelOf = (band: number, inBand: StageCard[]) => {
    if (band === PIPELINE_BAND) return "Pipelines"
    if (band === TABLE_BAND) return inBand.every((c) => !c.node || c.node.pipeline_table) ? "Pipeline tables" : "Tables"
    if (band < noLayer) return names[band - 2]
    if (band === noLayer + 1) return "Charts"
    if (band === noLayer + 2) return "Dashboards"
    return names.length === 0 ? "Models" : "No layer"
  }
  const columns = [...members]
    .sort(([a], [b]) => a - b)
    .map(([band, inBand]) => ({
      band,
      label: labelOf(band, inBand),
      count: inBand.reduce((sum, c) => sum + (c.count ?? 1), 0),
    }))
  return { bands, columns }
}

export type RefreshTone = "ok" | "warn" | "plain"

/** What refreshes a model, in a couple of words for its card. */
export function refreshChip(m: ModelRefresh): { label: string; tone: RefreshTone } {
  const behind = m.uncovered_upstreams.length > 0
  if (m.trigger_paused) return { label: "Paused", tone: "warn" }
  if (m.trigger === "after_upstream") {
    if (m.upstreams.length === 0) return { label: "After its upstreams", tone: "plain" }
    if (!behind) return { label: "After its inputs", tone: "ok" }
    const followed = m.upstreams.length - m.uncovered_upstreams.length
    return { label: `After ${followed} of ${m.upstreams.length} inputs`, tone: "warn" }
  }
  const label = { manual: "By hand", cron: "On a cron", interval: "On an interval" }[m.trigger] ?? triggerLabel(m.trigger)
  return { label, tone: behind ? "warn" : "plain" }
}

/**
 * The Needs-attention list split the way the summary counts it, so the two callouts add
 * up to the list: models not refreshed by what they read (paused ones that follow
 * something included), and the rest, which read a table that can't be traced to one
 * producer (unresolved or ambiguous).
 */
export function attentionSplit(items: readonly AttentionItem[]): { notRefreshed: AttentionItem[]; untraced: AttentionItem[] } {
  const behind = (i: AttentionItem) =>
    i.model.uncovered_upstreams.length > 0 || (i.model.trigger_paused && i.model.trigger_upstreams.length > 0)
  return { notRefreshed: items.filter(behind), untraced: items.filter((i) => !behind(i)) }
}

/**
 * What produces the tables a model reads, as the Schedule dialog's upstreams: what
 * "Rebuild after its inputs" picks. A node id is `pipeline:<id>` or `model:<id>`.
 */
export function inputUpstreams(m: ModelRefresh, byId: ReadonlyMap<string, AssetNode>): ScheduleUpstream[] {
  return m.upstreams.flatMap((nodeId) => {
    const at = nodeId.indexOf(":")
    const kind = nodeId.slice(0, at)
    if (kind !== "pipeline" && kind !== "model") return []
    const node = byId.get(nodeId)
    return [{ kind, id: node?.ref_id || nodeId.slice(at + 1), ...(node ? { name: node.name } : {}) }]
  })
}

/** Whether a model already runs after everything it reads, so there is nothing to rebuild after. */
export function followsItsInputs(m: ModelRefresh): boolean {
  return m.trigger === "after_upstream" && !m.trigger_paused && m.upstreams.length > 0 && m.uncovered_upstreams.length === 0
}
