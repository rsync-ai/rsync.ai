import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, renderHook, screen, waitFor, within } from "@testing-library/react"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

import { authFetch } from "@/lib/api/auth-fetch"
import { ModelPlacement, useModelSurroundings } from "@/components/explorer/ModelPlacement"
import {
  inputWakes,
  modelSurroundings,
  parseAssetGraph,
  type AssetEdge,
  type AssetNode,
} from "@/components/explorer/assetLineage"

// "Where it sits" in the Schedule dialog: one model's inputs, the table it builds and the
// models that read that, from the asset graph's own edges. Its job is to show which inputs
// wake the model, so the one thing it must not do is call an input "waking" that is not.

const mockFetch = authFetch as unknown as Mock
const MODEL = "model:m-mrr"

function res(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as unknown as Response
}

const node = (id: string, name: string): AssetNode => ({ id, kind: id.split(":")[0], name })
const edge = (from: string, to: string, kind: string): AssetEdge => ({ from, to, kind, evidence: "sql" })

/**
 * orders ← pipeline Orders load; customers ← pipeline CRM sync AND model Customer dim;
 * MRR reads both and builds analytics.mrr, which Churn and MRR itself (a self-read) read.
 */
function graphBody() {
  return {
    nodes: [
      node("pipeline:p-orders", "Orders load"),
      node("pipeline:p-crm", "CRM sync"),
      node("model:m-cust", "Customer dim"),
      node(MODEL, "MRR"),
      node("model:m-churn", "Churn"),
      node("table:c1:raw.orders", "raw.orders"),
      node("table:c1:raw.customers", "raw.customers"),
      node("table:c1:analytics.mrr", "analytics.mrr"),
    ],
    edges: [
      edge("pipeline:p-orders", "table:c1:raw.orders", "writes"),
      edge("pipeline:p-crm", "table:c1:raw.customers", "writes"),
      edge("model:m-cust", "table:c1:raw.customers", "materializes"),
      edge("table:c1:raw.orders", MODEL, "reads"),
      edge("table:c1:raw.customers", MODEL, "reads"),
      edge(MODEL, "table:c1:analytics.mrr", "materializes"),
      edge("table:c1:analytics.mrr", "model:m-churn", "reads"),
      edge("table:c1:analytics.mrr", MODEL, "reads"),
    ],
  }
}

beforeEach(() => {
  mockFetch.mockReset()
})

describe("modelSurroundings", () => {
  const found = modelSurroundings(parseAssetGraph(graphBody()), MODEL)!

  it("lists each table the model reads with everything that produces it", () => {
    const byTable = Object.fromEntries(found.inputs.map((i) => [i.table.id, i.producers.map((p) => p.id)]))
    expect(byTable["table:c1:raw.orders"]).toEqual(["pipeline:p-orders"])
    // A pipeline and a model both producing one table are both producers: either run can
    // change what the model reads.
    expect(byTable["table:c1:raw.customers"]).toEqual(["pipeline:p-crm", "model:m-cust"])
  })

  it("names the table it builds and the models that read it, never itself", () => {
    expect(found.builds.map((b) => b.id)).toEqual(["table:c1:analytics.mrr"])
    expect(found.readers.map((r) => r.id)).toEqual(["model:m-churn"])
    // A model reading its own table is not its own producer either.
    const own = found.inputs.find((i) => i.table.id === "table:c1:analytics.mrr")
    expect(own?.producers ?? []).toEqual([])
  })

  it("has nothing to say about a node that is not a model, or is not in the graph", () => {
    const graph = parseAssetGraph(graphBody())
    expect(modelSurroundings(graph, "pipeline:p-orders")).toBeNull()
    expect(modelSurroundings(graph, "model:not-here")).toBeNull()
  })

  it("counts an input as waking the model when any one of its producers does", () => {
    const customers = found.inputs.find((i) => i.table.id === "table:c1:raw.customers")!
    expect(inputWakes(customers, new Set(["model:m-cust"]))).toBe(true)
    expect(inputWakes(customers, new Set(["pipeline:p-orders"]))).toBe(false)
    expect(inputWakes(customers, new Set())).toBe(false)
  })
})

describe("useModelSurroundings", () => {
  it("reads this connection's graph and finds the model in it", async () => {
    mockFetch.mockResolvedValue(res(200, graphBody()))
    const { result } = renderHook(() => useModelSurroundings("m-mrr", "c 1", true))

    await waitFor(() => expect(result.current?.model.name).toBe("MRR"))
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/explorer/asset-graph?connection_id=c%201", expect.anything())
  })

  it("asks for nothing while the dialog is closed", () => {
    renderHook(() => useModelSurroundings("m-mrr", undefined, false))
    expect(mockFetch).not.toHaveBeenCalled()
  })

  // Context for a form that works without it: a failed read leaves the strip out, no error.
  it.each([
    ["an error answer", () => mockFetch.mockResolvedValue(res(500, { error: "boom" }))],
    ["a network failure", () => mockFetch.mockRejectedValue(new Error("offline"))],
  ])("stays empty on %s", async (_case, arrange) => {
    arrange()
    const { result } = renderHook(() => useModelSurroundings("m-mrr", undefined, true))
    await waitFor(() => expect(mockFetch).toHaveBeenCalled())
    expect(result.current).toBeNull()
  })
})

describe("ModelPlacement", () => {
  const surroundings = modelSurroundings(parseAssetGraph(graphBody()), MODEL)!
  const inputsOf = () => within(screen.getByRole("list", { name: "Reads from" })).getAllByRole("listitem")

  it("draws an input as waking only when the model follows its inputs and one of them is chosen", () => {
    const { rerender } = render(
      <ModelPlacement surroundings={surroundings} waking={new Set(["pipeline:p-orders"])} when="inputs" savedQueryId="m-mrr" />
    )
    // An input with one producer is named by it; one with two by its table.
    expect(inputsOf().map((li) => [li.textContent, li.dataset.wakes])).toEqual([
      ["Orders load", "true"],
      ["raw.customers", "false"],
      ["analytics.mrr", "false"],
    ])
    expect(screen.getByText(/1 of 3 inputs wake this model/i)).toBeInTheDocument()

    // The same set on a clock wakes nothing: a clock does not read who ran.
    rerender(
      <ModelPlacement surroundings={surroundings} waking={new Set(["pipeline:p-orders"])} when="clock" savedQueryId="m-mrr" />
    )
    expect(inputsOf().every((li) => li.dataset.wakes === "false")).toBe(true)
    expect(screen.getByText(/runs on a clock/i)).toBeInTheDocument()
  })

  it("names what reads the table it builds, and links to the model on the Lineage page", () => {
    render(<ModelPlacement surroundings={surroundings} waking={new Set()} when="hand" savedQueryId="m-mrr" />)
    expect(screen.getByTitle("The table it builds")).toHaveTextContent("analytics.mrr")
    expect(screen.getByText("Read by Churn")).toBeInTheDocument()
    expect(screen.getByText(/only when someone presses Run now/i)).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Open in Lineage" })).toHaveAttribute(
      "href",
      "/explorer/lineage?focus=model%3Am-mrr"
    )
  })

  it("shows the table a model will build before the graph has it, and that nothing reads it yet", () => {
    const fresh = { ...surroundings, builds: [], readers: [] }
    render(
      <ModelPlacement surroundings={fresh} waking={new Set()} when="clock" plannedTable="analytics.new" savedQueryId="m-mrr" />
    )
    expect(screen.getByTitle("The table it will build once saved")).toHaveTextContent("analytics.new")
    expect(screen.getByText("Read by nothing yet")).toBeInTheDocument()
  })

  it("keeps a long list to a line, and says how many more there are", () => {
    const table = (i: number) => node(`table:c1:t${i}`, `t${i}`)
    const many = {
      ...surroundings,
      inputs: Array.from({ length: 6 }, (_, i) => ({ table: table(i), producers: [] })),
      readers: Array.from({ length: 5 }, (_, i) => node(`model:r${i}`, `R${i}`)),
    }
    render(<ModelPlacement surroundings={many} waking={new Set()} when="inputs" savedQueryId="m-mrr" />)
    expect(inputsOf().map((li) => li.textContent)).toEqual(["t0", "t1", "t2", "t3", "+2 more"])
    expect(screen.getByText("Read by R0, R1, R2 +2 more")).toBeInTheDocument()
    expect(screen.getByText(/0 of 6 inputs wake this model/i)).toBeInTheDocument()
  })

  it("says so when the SQL reads nothing produced here", () => {
    render(
      <ModelPlacement surroundings={{ ...surroundings, inputs: [] }} waking={new Set()} when="inputs" savedQueryId="m-mrr" />
    )
    expect(screen.queryByRole("list", { name: "Reads from" })).not.toBeInTheDocument()
    expect(screen.getByText(/names no table a pipeline or model here produces/i)).toBeInTheDocument()
  })
})
