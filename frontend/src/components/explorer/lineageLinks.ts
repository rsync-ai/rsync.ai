// What the Lineage row's popup says about each pipeline and model chained to a model: its
// name, how far away it is, and the tables it writes. A name is shown under the graph's
// rule (describeLineageNode): only once a route that checks access has vouched for it.

import { lookupFromResponse, modelHref, type LineageKind, type LineageNode } from "@/components/explorer/modelLineage"

/** What the two words mean, in the same words wherever a list of either is shown. */
export const UPSTREAM_MEANING = "Pipelines and models this one runs after"
export const DOWNSTREAM_MEANING = "Models that run after this one"

/** A pipeline's tables named before the rest are only counted. */
export const PIPELINE_TABLES_SHOWN = 5

export type LinkedLookup =
  | { status: "pipeline"; name: string; tables: string[]; destination: string | null }
  | { status: "model"; name: string; table: string | null }
  /** 403 or 404: one the caller may not open, or one that is gone. */
  | { status: "denied" }
  /** Anything else: its name is unknown, not hidden. */
  | { status: "unavailable" }

export interface LinkedNodeView {
  key: string
  kindLabel: "Model" | "Pipeline"
  title: string
  /** False when `title` is a placeholder ("A model you can't open") rather than a name. */
  named: boolean
  href: string | null
  distance: string
  /** The tables it writes, in a sentence, or null when there is nothing to say. */
  detail: string | null
}

/**
 * A node a list route has not already vouched for. A model's own schedule row is the list
 * route vouching for it, name and table both; a pipeline has no row there.
 */
export function needsLinkedLookup(node: LineageNode): boolean {
  return node.role !== "root" && (node.kind === "pipeline" || !node.schedule)
}

/** The URL whose answer says what the popup may show about a node. */
export function linkedLookupUrl(node: LineageNode): string {
  const id = encodeURIComponent(node.id)
  return node.kind === "pipeline" ? `/api/v1/pipelines/${id}` : `/api/v1/explorer/saved/${id}`
}

/**
 * Turns GET /pipelines/:id or GET /explorer/saved/:id into what the popup may say. Fails
 * closed, as lookupFromResponse does: only a 2xx with an object body names anything.
 */
export function linkedLookupFromResponse(kind: LineageKind, httpStatus: number, body: unknown): LinkedLookup {
  if (kind === "pipeline") {
    const found = lookupFromResponse("pipeline", httpStatus, body)
    if (found.status === "denied" || found.status === "unavailable") return found
    const p = body as { name?: unknown; selected_tables?: unknown; destination_connection?: { name?: unknown } | null }
    return {
      status: "pipeline",
      name: trimmed(p.name),
      tables: Array.isArray(p.selected_tables) ? p.selected_tables.map(trimmed).filter(Boolean) : [],
      destination: trimmed(p.destination_connection?.name) || null,
    }
  }
  // The query route answers 404 for a query that does not exist and for another member's
  // private one alike (GET /explorer/saved/:id).
  if (httpStatus === 403 || httpStatus === 404) return { status: "denied" }
  if (httpStatus < 200 || httpStatus >= 300 || !body || typeof body !== "object") return { status: "unavailable" }
  const q = body as { name?: unknown; target_table?: unknown }
  return { status: "model", name: trimmed(q.name), table: trimmed(q.target_table) || null }
}

function trimmed(v: unknown): string {
  return typeof v === "string" ? v.trim() : ""
}

export function describeLinkedNode(node: LineageNode, lookup: LinkedLookup | undefined): LinkedNodeView {
  const distance = node.distance === 1 ? "Direct" : `${node.distance} hops away`
  const base = { key: node.key, distance }

  if (node.kind === "pipeline") {
    if (lookup?.status === "denied") return { ...base, ...hidden("A pipeline you can't open", "Pipeline") }
    if (lookup?.status !== "pipeline") return { ...base, ...unknown("A pipeline", "Pipeline") }
    return {
      ...base,
      kindLabel: "Pipeline",
      title: lookup.name || "Unnamed pipeline",
      named: !!lookup.name,
      href: `/pipelines/${encodeURIComponent(node.id)}`,
      detail: pipelineDetail(lookup.tables, lookup.destination),
    }
  }

  let name: string
  let table: string | null
  if (node.schedule) {
    name = node.schedule.name?.trim() ?? ""
    table = node.schedule.target_table?.trim() || null
  } else if (lookup?.status === "model") {
    name = lookup.name
    table = lookup.table
  } else if (lookup?.status === "denied") {
    return { ...base, ...hidden("A model you can't open", "Model") }
  } else {
    return { ...base, ...unknown("A model", "Model") }
  }
  return {
    ...base,
    kindLabel: "Model",
    title: name || "Unnamed model",
    named: !!name,
    href: modelHref(node.id),
    detail: table ? `Writes ${table}` : null,
  }
}

function pipelineDetail(tables: string[], destination: string | null): string | null {
  const into = destination ? ` into ${destination}` : ""
  if (tables.length === 0) return destination ? `Loads into ${destination}` : null
  const shown = tables.slice(0, PIPELINE_TABLES_SHOWN).join(", ")
  const rest = tables.length - PIPELINE_TABLES_SHOWN
  return rest > 0 ? `Copies ${shown} and ${rest} more${into}` : `Copies ${shown}${into}`
}

function hidden(title: string, kindLabel: "Model" | "Pipeline") {
  return { kindLabel, title, named: false, href: null, detail: null }
}

function unknown(title: string, kindLabel: "Model" | "Pipeline") {
  return { kindLabel, title, named: false, href: null, detail: "Could not load its name or tables." }
}
