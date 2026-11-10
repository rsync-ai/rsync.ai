/**
 * A column's path through the asset graph (design § 3.5): pure functions over the graph's
 * `column_lineage` and its edges, so the Lineage page only draws what they return.
 *
 * A column is a node and a column name. A model's output column is the same column as the
 * one of that name on each table the model materializes, so a path goes model to model
 * through those tables. Pipelines have no column links of their own (they copy columns),
 * so the pipeline that writes a table on the path is context, as are the charts and
 * dashboards drawn from a model the column reaches. Names compare without case, as the
 * warehouses that fold case do.
 */

import type { AssetEdge, AssetGraph, ColumnLineageEdge, ColumnLineageModel } from "@/components/explorer/assetLineage"

/** A chosen column: a table's or model's node id (the graph's, not a drawn card's) and its name. */
export interface ColumnRef {
  node: string
  column: string
}

/** The column's path, upstream and downstream. */
export interface ColumnPath {
  /** Every node the path touches, context included: the ids the canvas lights. */
  nodes: Set<string>
  /** The columns on the path, as columnKey gives them. */
  columns: Set<string>
  /** The chosen column and the same column on its model or the tables the model builds. */
  chosen: Set<string>
}

/** A column's key: its node and its name without case. */
export function columnKey(node: string, column: string): string {
  return `${node}\u0000${column.toLowerCase()}`
}

/** What a column is computed from or feeds, one link away, for the details panel. */
export interface ColumnLink {
  /** The other end's node, or "" for a table the graph does not have. */
  node: string
  /** A table's name as the model's SQL wrote it; "" for a model, which the panel names from its node. */
  name: string
  /** The other end's column; "" when a link only filters, joins or groups a model's rows. */
  column: string
  type: string
  subtype: string
}

/** The links one chosen column has, one step each way. */
export interface ColumnLinks {
  /** The columns its value is computed from. */
  from: ColumnLink[]
  /** The model columns computed from it, and the models it only filters, joins or groups. */
  readBy: ColumnLink[]
}

const SUBTYPE_WORDS: Record<string, string> = {
  IDENTITY: "copied",
  TRANSFORMATION: "transformed",
  AGGREGATION: "aggregated",
  JOIN: "in a join",
  FILTER: "in a filter",
  GROUP_BY: "in a group by",
  SORT: "in a sort",
  WINDOW: "in a window",
}

/** A link's type in words: how a column is computed, or where in the SQL it shapes the rows. */
export function subtypeWords(subtype: string): string {
  return SUBTYPE_WORDS[subtype] ?? subtype.toLowerCase().replace(/_/g, " ")
}

/** A model's answer, if it has one. */
export function columnLineageOf(graph: AssetGraph, node: string): ColumnLineageModel | undefined {
  return graph.column_lineage?.models.find((m) => m.node_id === node)
}

function push<K, V>(map: Map<K, V[]>, key: K, value: V) {
  const list = map.get(key)
  if (list) list.push(value)
  else map.set(key, [value])
}

interface Index {
  kind: Map<string, string>
  /** Graph edges by where they end, and by where they start. */
  into: Map<string, AssetEdge[]>
  out: Map<string, AssetEdge[]>
  /** Column links by the model column they compute, the model they shape, and the table column they read. */
  computes: Map<string, ColumnLineageEdge[]>
  shapes: Map<string, ColumnLineageEdge[]>
  reads: Map<string, ColumnLineageEdge[]>
}

function index(graph: AssetGraph): Index {
  const idx: Index = {
    kind: new Map(graph.nodes.map((n) => [n.id, n.kind])),
    into: new Map(),
    out: new Map(),
    computes: new Map(),
    shapes: new Map(),
    reads: new Map(),
  }
  for (const e of graph.edges) {
    push(idx.into, e.to, e)
    push(idx.out, e.from, e)
  }
  for (const e of graph.column_lineage?.edges ?? []) {
    if (e.type === "DIRECT") push(idx.computes, columnKey(e.to, e.to_column), e)
    else push(idx.shapes, e.to, e)
    if (e.from) push(idx.reads, columnKey(e.from, e.from_column), e)
  }
  return idx
}

/**
 * The same column under another node: a model's column on each table it materializes,
 * and a table's on each model that materializes it.
 */
function twins(idx: Index, ref: ColumnRef): ColumnRef[] {
  if (idx.kind.get(ref.node) === "model") {
    return (idx.out.get(ref.node) ?? []).filter((e) => e.kind === "materializes").map((e) => ({ node: e.to, column: ref.column }))
  }
  return (idx.into.get(ref.node) ?? []).filter((e) => e.kind === "materializes").map((e) => ({ node: e.from, column: ref.column }))
}

/** One step upstream: the columns this one is computed from, and the pipelines that write it. */
function upstreamOf(idx: Index, ref: ColumnRef, context: Set<string>): ColumnRef[] {
  if (idx.kind.get(ref.node) === "model") {
    return (idx.computes.get(columnKey(ref.node, ref.column)) ?? [])
      .filter((e) => e.from)
      .map((e) => ({ node: e.from, column: e.from_column }))
  }
  const next: ColumnRef[] = []
  for (const e of idx.into.get(ref.node) ?? []) {
    // A lake table's files are what an external table reads: the same columns.
    if (e.kind === "materializes" || e.kind === "external") next.push({ node: e.from, column: ref.column })
    else if (e.kind === "writes") context.add(e.from)
  }
  return next
}

/** One step downstream: the columns computed from this one; the models it only shapes, and the charts it reaches, as context. */
function downstreamOf(idx: Index, ref: ColumnRef, context: Set<string>): ColumnRef[] {
  const next: ColumnRef[] = []
  if (idx.kind.get(ref.node) === "model") {
    for (const e of idx.out.get(ref.node) ?? []) {
      if (e.kind === "materializes") next.push({ node: e.to, column: ref.column })
      else if (e.kind === "draws") {
        context.add(e.to)
        for (const s of idx.out.get(e.to) ?? []) if (s.kind === "shows") context.add(s.to)
      }
    }
    return next
  }
  for (const e of idx.reads.get(columnKey(ref.node, ref.column)) ?? []) {
    if (e.type === "DIRECT") next.push({ node: e.to, column: e.to_column })
    else context.add(e.to)
  }
  for (const e of idx.out.get(ref.node) ?? []) if (e.kind === "external") next.push({ node: e.to, column: ref.column })
  return next
}

function follow(
  starts: ColumnRef[],
  step: (ref: ColumnRef) => ColumnRef[],
  columns: Set<string>,
  nodes: Set<string>,
) {
  const seen = new Set(starts.map((r) => columnKey(r.node, r.column)))
  let frontier = starts
  while (frontier.length > 0) {
    const following: ColumnRef[] = []
    for (const ref of frontier) {
      columns.add(columnKey(ref.node, ref.column))
      nodes.add(ref.node)
      for (const n of step(ref)) {
        const key = columnKey(n.node, n.column)
        if (seen.has(key)) continue
        seen.add(key)
        following.push(n)
      }
    }
    frontier = following
  }
}

/**
 * The chosen column's path: every column its value comes from and every column computed
 * from it, however far, with the pipelines, models, charts and dashboards they touch.
 */
export function columnPath(graph: AssetGraph, ref: ColumnRef): ColumnPath {
  const idx = index(graph)
  const starts = [ref, ...twins(idx, ref)]
  const columns = new Set<string>()
  const nodes = new Set<string>()
  const context = new Set<string>()
  follow(starts, (r) => upstreamOf(idx, r, context), columns, nodes)
  follow(starts, (r) => downstreamOf(idx, r, context), columns, nodes)
  for (const id of context) nodes.add(id)
  return { nodes, columns, chosen: new Set(starts.map((r) => columnKey(r.node, r.column))) }
}

function byName(a: ColumnLink, b: ColumnLink): number {
  return a.name.localeCompare(b.name) || a.node.localeCompare(b.node) || a.column.localeCompare(b.column) || a.subtype.localeCompare(b.subtype)
}

/**
 * The chosen column's links one step each way, for the details panel. A table built by a
 * model answers with the model's links, and a model's column is read through the tables it
 * materializes.
 */
export function columnLinks(graph: AssetGraph, ref: ColumnRef): ColumnLinks {
  const idx = index(graph)
  const starts = [ref, ...twins(idx, ref)]
  const models = starts.filter((r) => idx.kind.get(r.node) === "model")
  const tables = starts.filter((r) => idx.kind.get(r.node) !== "model")
  const from = models.flatMap((m) =>
    (idx.computes.get(columnKey(m.node, m.column)) ?? []).map((e) => ({
      node: e.from,
      name: e.from_table,
      column: e.from_column,
      type: e.type,
      subtype: e.subtype,
    })),
  )
  const readBy = tables.flatMap((t) =>
    (idx.reads.get(columnKey(t.node, t.column)) ?? []).map((e) => ({
      node: e.to,
      name: "",
      column: e.to_column,
      type: e.type,
      subtype: e.subtype,
    })),
  )
  return { from: from.sort(byName), readBy: readBy.sort(byName) }
}

/**
 * The models whose columns a node's are: a model itself, or each model that materializes
 * a table.
 */
export function modelsOf(graph: AssetGraph, node: string): string[] {
  const idx = index(graph)
  return idx.kind.get(node) === "model" ? [node] : twins(idx, { node, column: "" }).map((r) => r.node)
}

/**
 * The columns that filter, join, group, sort or window a model's rows, and so shape every
 * column it has: listed apart from where each column's value comes from.
 */
export function rowsShapedBy(graph: AssetGraph, node: string): ColumnLink[] {
  const idx = index(graph)
  return modelsOf(graph, node)
    .flatMap((m) =>
      (idx.shapes.get(m) ?? []).map((e) => ({
        node: e.from,
        name: e.from_table,
        column: e.from_column,
        type: e.type,
        subtype: e.subtype,
      })),
    )
    .sort(byName)
}

/**
 * The columns the details panel offers to choose on a node: a model's output columns (its
 * own, or those of the model that builds a table), then for a table the columns models
 * read from it. Each name once, without case, in the order first given.
 */
export function pickableColumns(graph: AssetGraph, node: string): string[] {
  const idx = index(graph)
  const names: string[] = []
  const seen = new Set<string>()
  const add = (c: string) => {
    const key = c.toLowerCase()
    if (!c || seen.has(key)) return
    seen.add(key)
    names.push(c)
  }
  for (const m of modelsOf(graph, node)) for (const c of columnLineageOf(graph, m)?.columns ?? []) add(c)
  if (idx.kind.get(node) !== "model") {
    for (const e of graph.column_lineage?.edges ?? []) if (e.from === node) add(e.from_column)
  }
  return names
}
