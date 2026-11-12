import { describe, expect, it } from "vitest"
import {
  describeLinkedNode,
  linkedLookupFromResponse,
  linkedLookupUrl,
  needsLinkedLookup,
} from "@/components/explorer/lineageLinks"
import type { LineageNode } from "@/components/explorer/modelLineage"
import type { ScheduledQuery } from "@/components/explorer/scheduledModel"

// The Lineage row's popup names what a model is chained to and the tables each writes.
// What matters is the graph's rule: a name, or a table, shows only once a route that
// checks access has vouched for it.

function node(overrides: Partial<LineageNode> & Pick<LineageNode, "kind" | "id">): LineageNode {
  return { key: `${overrides.kind}:${overrides.id}`, role: "upstream", distance: 1, upstreamsNotDrawn: 0, ...overrides }
}

function row(id: string, overrides: Partial<ScheduledQuery> = {}): ScheduledQuery {
  return {
    schedule_id: `s-${id}`,
    saved_query_id: id,
    name: id,
    connection_id: "c-1",
    schedule_type: "cron",
    schedule_spec: { cron: "0 3 * * *" },
    status: "active",
    materialization: "table",
    created_by: "u-1",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    ...overrides,
  }
}

describe("which nodes the popup asks about", () => {
  it("asks about every pipeline and every model without its own schedule row, never the model itself", () => {
    expect(needsLinkedLookup(node({ kind: "pipeline", id: "p-1" }))).toBe(true)
    expect(needsLinkedLookup(node({ kind: "model", id: "m-1" }))).toBe(true)
    expect(needsLinkedLookup(node({ kind: "model", id: "m-1", schedule: row("m-1") }))).toBe(false)
    expect(needsLinkedLookup(node({ kind: "model", id: "q-1", role: "root", distance: 0 }))).toBe(false)
  })

  it("asks the routes that check access", () => {
    expect(linkedLookupUrl(node({ kind: "pipeline", id: "p 1" }))).toBe("/api/v1/pipelines/p%201")
    expect(linkedLookupUrl(node({ kind: "model", id: "m-1" }))).toBe("/api/v1/explorer/saved/m-1")
  })
})

describe("linkedLookupFromResponse", () => {
  it("reads a pipeline's name, its tables and where it loads them", () => {
    const body = {
      name: " orders_sync ",
      selected_tables: ["public.orders", " ", " public.customers", 7],
      destination_connection: { name: "BigQuery prod" },
    }
    expect(linkedLookupFromResponse("pipeline", 200, body)).toEqual({
      status: "pipeline",
      name: "orders_sync",
      tables: ["public.orders", "public.customers"],
      destination: "BigQuery prod",
    })
    expect(linkedLookupFromResponse("pipeline", 200, { name: "x" })).toEqual({
      status: "pipeline",
      name: "x",
      tables: [],
      destination: null,
    })
  })

  it("fails closed for a pipeline as the graph does", () => {
    expect(linkedLookupFromResponse("pipeline", 403, {})).toEqual({ status: "denied" })
    expect(linkedLookupFromResponse("pipeline", 404, { error: "not found" })).toEqual({ status: "denied" })
    // Past the access gate: a row deleted since, which says nothing about access.
    expect(linkedLookupFromResponse("pipeline", 404, { error: "pipeline_not_found" })).toEqual({ status: "unavailable" })
    expect(linkedLookupFromResponse("pipeline", 500, { name: "secret" })).toEqual({ status: "unavailable" })
    expect(linkedLookupFromResponse("pipeline", 200, null)).toEqual({ status: "unavailable" })
  })

  it("reads a model's name and table from its own row, and fails closed", () => {
    expect(linkedLookupFromResponse("model", 200, { name: "stg_orders", target_table: "analytics.stg_orders" })).toEqual({
      status: "model",
      name: "stg_orders",
      table: "analytics.stg_orders",
    })
    expect(linkedLookupFromResponse("model", 200, { name: "dml_only" })).toEqual({ status: "model", name: "dml_only", table: null })
    expect(linkedLookupFromResponse("model", 404, {})).toEqual({ status: "denied" })
    expect(linkedLookupFromResponse("model", 403, {})).toEqual({ status: "denied" })
    expect(linkedLookupFromResponse("model", 502, { name: "secret" })).toEqual({ status: "unavailable" })
    expect(linkedLookupFromResponse("model", 200, "oops")).toEqual({ status: "unavailable" })
  })
})

describe("describeLinkedNode", () => {
  it("names a model from its own schedule row, with the table it writes and a link", () => {
    const v = describeLinkedNode(
      node({ kind: "model", id: "d-1", role: "downstream", distance: 2, schedule: row("d-1", { target_table: "analytics.rollup" }) }),
      undefined,
    )
    expect(v).toMatchObject({
      kindLabel: "Model",
      title: "d-1",
      named: true,
      href: "/models/d-1?tab=graph",
      distance: "2 hops away",
      detail: "Writes analytics.rollup",
    })
  })

  it("names a model with no row only from its own route, never from a downstream's claim", () => {
    const claimed = node({ kind: "model", id: "m-1", claimedName: "what_a_downstream_called_it" })
    expect(describeLinkedNode(claimed, { status: "model", name: "stg_orders", table: "analytics.stg_orders" })).toMatchObject({
      title: "stg_orders",
      named: true,
      href: "/models/m-1?tab=graph",
      distance: "Direct",
      detail: "Writes analytics.stg_orders",
    })
    const denied = describeLinkedNode(claimed, { status: "denied" })
    expect(denied).toMatchObject({ title: "A model you can't open", named: false, href: null, detail: null })
    expect(JSON.stringify(denied)).not.toContain("what_a_downstream_called_it")
    expect(describeLinkedNode(claimed, undefined)).toMatchObject({
      title: "A model",
      named: false,
      href: null,
      detail: "Could not load its name or tables.",
    })
  })

  it("says what a pipeline copies and where, naming five tables and counting the rest", () => {
    const p = node({ kind: "pipeline", id: "p-1" })
    const tables = ["t1", "t2", "t3", "t4", "t5", "t6", "t7"]
    expect(describeLinkedNode(p, { status: "pipeline", name: "orders_sync", tables, destination: "BigQuery prod" })).toMatchObject({
      kindLabel: "Pipeline",
      title: "orders_sync",
      href: "/pipelines/p-1",
      detail: "Copies t1, t2, t3, t4, t5 and 2 more into BigQuery prod",
    })
    expect(describeLinkedNode(p, { status: "pipeline", name: "a", tables: ["t1"], destination: null }).detail).toBe("Copies t1")
    expect(describeLinkedNode(p, { status: "pipeline", name: "a", tables: [], destination: "BigQuery prod" }).detail).toBe(
      "Loads into BigQuery prod",
    )
    expect(describeLinkedNode(p, { status: "pipeline", name: "", tables: [], destination: null })).toMatchObject({
      title: "Unnamed pipeline",
      named: false,
      detail: null,
    })
    expect(describeLinkedNode(p, { status: "denied" })).toMatchObject({ title: "A pipeline you can't open", href: null, detail: null })
    expect(describeLinkedNode(p, { status: "unavailable" })).toMatchObject({ title: "A pipeline", href: null })
  })
})
