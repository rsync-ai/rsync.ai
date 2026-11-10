import { describe, expect, it } from "vitest"

import { finishedReading, parseAssetGraph, stillReading } from "@/components/explorer/assetLineage"
import {
  columnKey,
  columnLinks,
  columnPath,
  modelsOf,
  pickableColumns,
  rowsShapedBy,
  subtypeWords,
} from "@/components/explorer/columnLineage"

// A column's path through the asset graph (columnLineage.ts, design § 3.5). orders_sync writes
// orders and customers; daily_revenue reads both and builds analytics.daily_revenue, which
// weekly_rollup reads and draws a chart from, on a dashboard. A lake pipeline's folder is read
// by a warehouse as an external table, which silver_transactions reads.
const P1 = "pipeline:p-1"
const T_ORDERS = "table:c-1:public.orders"
const T_CUST = "table:c-1:public.customers"
const T_DAILY = "table:c-1:analytics.daily_revenue"
const M1 = "model:m-1"
const M2 = "model:m-2"
const CHART = "chart:c-5"
const DASH = "dashboard:d-1"
const P_LAKE = "pipeline:p-lake"
const T_FOLDER = "table:gcs-1\u0000lake\u0000fintech/public/transactions"
const T_EXT = "table:bq-1:fintech_bronze.transactions"
const M_SILVER = "model:m-silver"

const direct = (from: string, from_table: string, from_column: string, to: string, to_column: string, subtype: string) => ({
  from,
  from_table,
  from_column,
  to,
  to_column,
  type: "DIRECT",
  subtype,
})
const indirect = (from: string, from_table: string, from_column: string, to: string, subtype: string) => ({
  from,
  from_table,
  from_column,
  to,
  to_column: "",
  type: "INDIRECT",
  subtype,
})

const BODY = {
  nodes: [
    { id: P1, kind: "pipeline", name: "orders_sync" },
    { id: T_ORDERS, kind: "table", name: "public.orders" },
    { id: T_CUST, kind: "table", name: "public.customers" },
    { id: T_DAILY, kind: "table", name: "analytics.daily_revenue" },
    { id: M1, kind: "model", name: "daily_revenue" },
    { id: M2, kind: "model", name: "weekly_rollup" },
    { id: CHART, kind: "chart", name: "weekly_total" },
    { id: DASH, kind: "dashboard", name: "Revenue overview" },
    { id: P_LAKE, kind: "pipeline", name: "card_transactions" },
    { id: T_FOLDER, kind: "table", name: "public.transactions" },
    { id: T_EXT, kind: "table", name: "fintech_bronze.transactions" },
    { id: M_SILVER, kind: "model", name: "silver_transactions" },
  ],
  edges: [
    { from: P1, to: T_ORDERS, kind: "writes", evidence: "observed" },
    { from: P1, to: T_CUST, kind: "writes", evidence: "observed" },
    { from: T_ORDERS, to: M1, kind: "reads", evidence: "inferred" },
    { from: T_CUST, to: M1, kind: "reads", evidence: "inferred" },
    { from: M1, to: T_DAILY, kind: "materializes", evidence: "declared" },
    { from: T_DAILY, to: M2, kind: "reads", evidence: "inferred" },
    { from: M2, to: CHART, kind: "draws", evidence: "declared" },
    { from: CHART, to: DASH, kind: "shows", evidence: "declared" },
    { from: P_LAKE, to: T_FOLDER, kind: "writes", evidence: "observed" },
    { from: T_FOLDER, to: T_EXT, kind: "external", evidence: "observed" },
    { from: T_EXT, to: M_SILVER, kind: "reads", evidence: "inferred" },
  ],
  column_lineage: {
    pending: false,
    truncated: false,
    models: [
      { node_id: M1, status: "partial", columns: ["day", "revenue", "customer_count"], reason: "public.refunds is not in the warehouse's schema", truncated: false },
      { node_id: M2, status: "ok", columns: ["week", "revenue"], reason: "", truncated: false },
      { node_id: M_SILVER, status: "ok", columns: ["amount"], reason: "", truncated: false },
    ],
    edges: [
      direct(T_ORDERS, "public.orders", "created_at", M1, "day", "TRANSFORMATION"),
      direct(T_ORDERS, "public.orders", "amount", M1, "revenue", "AGGREGATION"),
      // A table the graph does not have: the SQL's name, and no node.
      direct("", "public.refunds", "refund_amount", M1, "revenue", "TRANSFORMATION"),
      direct(T_CUST, "public.customers", "customer_id", M1, "customer_count", "AGGREGATION"),
      indirect(T_ORDERS, "public.orders", "status", M1, "FILTER"),
      indirect(T_ORDERS, "public.orders", "customer_id", M1, "JOIN"),
      indirect(T_CUST, "public.customers", "customer_id", M1, "JOIN"),
      direct(T_DAILY, "analytics.daily_revenue", "day", M2, "week", "TRANSFORMATION"),
      // The warehouse's case, not the model's: still the same column.
      direct(T_DAILY, "analytics.daily_revenue", "REVENUE", M2, "revenue", "AGGREGATION"),
      direct(T_EXT, "fintech_bronze.transactions", "amount", M_SILVER, "amount", "IDENTITY"),
    ],
  },
}
const graph = parseAssetGraph(BODY)
const keys = (pairs: [string, string][]) => new Set(pairs.map(([n, c]) => columnKey(n, c)))

describe("reading column lineage", () => {
  it("reads it off the graph, a missing one as none, and fills what a model's answer leaves out", () => {
    expect(graph.column_lineage?.models.map((m) => m.node_id)).toEqual([M1, M2, M_SILVER])
    expect(graph.column_lineage?.edges).toHaveLength(10)
    expect(parseAssetGraph({ nodes: [], edges: [] }).column_lineage).toBeNull()
    expect(parseAssetGraph(null).column_lineage).toBeNull()
    expect(parseAssetGraph({ column_lineage: { models: [{ node_id: M1, columns: ["a", 3, null] }], edges: null } }).column_lineage).toEqual({
      pending: false,
      truncated: false,
      models: [{ node_id: M1, status: "", columns: ["a"], reason: "", truncated: false }],
      edges: [],
    })
  })

  it("asks again while either read is pending, and redraws only when one finishes", () => {
    const pending = (patch: object) => parseAssetGraph({ ...BODY, ...patch })
    const columnsPending = pending({ column_lineage: { ...BODY.column_lineage, pending: true } })
    const tablesPending = pending({ external_tables_pending: true })
    expect(stillReading(graph)).toBe(false)
    expect(stillReading(columnsPending)).toBe(true)
    expect(stillReading(tablesPending)).toBe(true)

    expect(finishedReading(columnsPending, graph)).toBe(true)
    expect(finishedReading(columnsPending, columnsPending)).toBe(false)
    expect(finishedReading(tablesPending, graph)).toBe(true)
    // The other read finishing is not the one this graph waited on.
    expect(finishedReading(columnsPending, pending({ column_lineage: { ...BODY.column_lineage, pending: true }, external_tables_pending: false }))).toBe(false)
    expect(finishedReading(graph, graph)).toBe(false)
  })
})

describe("a column's path", () => {
  it("follows a model column back through the table another model builds, to the pipeline that writes the source", () => {
    const path = columnPath(graph, { node: M2, column: "revenue" })
    expect(path.columns).toEqual(
      keys([
        [M2, "revenue"],
        [T_DAILY, "revenue"],
        [M1, "revenue"],
        [T_ORDERS, "amount"],
      ]),
    )
    // The pipeline, and the chart and dashboard drawn from weekly_rollup, are context; the
    // table the graph does not have ends the path; what only joins the rows (customers) is not on it.
    expect(path.nodes).toEqual(new Set([M2, T_DAILY, M1, T_ORDERS, P1, CHART, DASH]))
    expect(path.chosen).toEqual(keys([[M2, "revenue"]]))
  })

  it("follows a source column down, model to model, without case, to the charts and dashboards drawn from it", () => {
    const path = columnPath(graph, { node: T_ORDERS, column: "Amount" })
    expect(path.columns).toEqual(
      keys([
        [T_ORDERS, "amount"],
        [M1, "revenue"],
        [T_DAILY, "revenue"],
        [M2, "revenue"],
      ]),
    )
    expect(path.nodes).toEqual(new Set([T_ORDERS, P1, M1, T_DAILY, M2, CHART, DASH]))
  })

  it("lights the model a column only filters or joins, but follows no column into it", () => {
    const path = columnPath(graph, { node: T_ORDERS, column: "status" })
    expect(path.columns).toEqual(keys([[T_ORDERS, "status"]]))
    expect(path.nodes).toEqual(new Set([T_ORDERS, P1, M1]))
  })

  it("treats a model's column and the one on the table it builds as one", () => {
    const fromTable = columnPath(graph, { node: T_DAILY, column: "revenue" })
    const fromModel = columnPath(graph, { node: M1, column: "Revenue" })
    expect(fromTable.chosen).toEqual(keys([[T_DAILY, "revenue"], [M1, "revenue"]]))
    expect(fromModel.chosen).toEqual(fromTable.chosen)
    expect(fromModel.columns).toEqual(fromTable.columns)
    expect(fromModel.nodes).toEqual(fromTable.nodes)
  })

  it("goes through an external table to the lake folder and the pipeline that writes it", () => {
    const path = columnPath(graph, { node: M_SILVER, column: "amount" })
    expect(path.columns).toEqual(
      keys([
        [M_SILVER, "amount"],
        [T_EXT, "amount"],
        [T_FOLDER, "amount"],
      ]),
    )
    expect(path.nodes).toEqual(new Set([M_SILVER, T_EXT, T_FOLDER, P_LAKE]))
    // And down from the folder to the model.
    expect(columnPath(graph, { node: T_FOLDER, column: "amount" }).columns).toEqual(path.columns)
  })

  it("is the chosen column alone when nothing links it", () => {
    const path = columnPath(graph, { node: T_CUST, column: "email" })
    expect(path.columns).toEqual(keys([[T_CUST, "email"]]))
    expect(path.nodes).toEqual(new Set([T_CUST, P1]))
  })
})

describe("a column's links, for the details panel", () => {
  it("lists what a model column comes from, the table the graph does not have by its SQL name", () => {
    const fromTable = columnLinks(graph, { node: T_DAILY, column: "revenue" })
    expect(fromTable.from).toEqual([
      { node: T_ORDERS, name: "public.orders", column: "amount", type: "DIRECT", subtype: "AGGREGATION" },
      { node: "", name: "public.refunds", column: "refund_amount", type: "DIRECT", subtype: "TRANSFORMATION" },
    ])
    expect(fromTable.readBy).toEqual([{ node: M2, name: "", column: "revenue", type: "DIRECT", subtype: "AGGREGATION" }])
    // The model's own column says the same.
    expect(columnLinks(graph, { node: M1, column: "revenue" })).toEqual(fromTable)
  })

  it("lists what reads a source column, a join apart from a computed column", () => {
    expect(columnLinks(graph, { node: T_CUST, column: "customer_id" })).toEqual({
      from: [],
      readBy: [
        { node: M1, name: "", column: "", type: "INDIRECT", subtype: "JOIN" },
        { node: M1, name: "", column: "customer_count", type: "DIRECT", subtype: "AGGREGATION" },
      ],
    })
  })

  it("lists the columns that shape a model's rows, for the model and the table it builds", () => {
    const shaped = rowsShapedBy(graph, T_DAILY)
    expect(shaped.map((l) => [l.name, l.column, l.subtype])).toEqual([
      ["public.customers", "customer_id", "JOIN"],
      ["public.orders", "customer_id", "JOIN"],
      ["public.orders", "status", "FILTER"],
    ])
    expect(rowsShapedBy(graph, M1)).toEqual(shaped)
    expect(rowsShapedBy(graph, M2)).toEqual([])
    expect(rowsShapedBy(graph, T_ORDERS)).toEqual([])
  })

  it("offers a model's columns, and on a source table the columns models read from it, each once", () => {
    expect(modelsOf(graph, T_DAILY)).toEqual([M1])
    expect(modelsOf(graph, M2)).toEqual([M2])
    expect(modelsOf(graph, T_ORDERS)).toEqual([])
    expect(pickableColumns(graph, T_DAILY)).toEqual(["day", "revenue", "customer_count"])
    expect(pickableColumns(graph, M2)).toEqual(["week", "revenue"])
    expect(pickableColumns(graph, T_ORDERS)).toEqual(["created_at", "amount", "status", "customer_id"])
    expect(pickableColumns(graph, P1)).toEqual([])
  })

  it("says how a column is used in a few words, and a type from later in plain words", () => {
    expect(["IDENTITY", "TRANSFORMATION", "AGGREGATION", "JOIN", "GROUP_BY"].map(subtypeWords)).toEqual([
      "copied",
      "transformed",
      "aggregated",
      "in a join",
      "in a group by",
    ])
    expect(subtypeWords("CONDITIONAL_MASK")).toBe("conditional mask")
  })
})
