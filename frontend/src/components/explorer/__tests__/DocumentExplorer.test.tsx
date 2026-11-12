import { describe, it, expect, vi, beforeEach } from "vitest"
import { useState } from "react"
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: (...args: unknown[]) => authFetch(...args) }))

import { DocumentExplorer } from "../DocumentExplorer"

const COLLECTIONS = [
  { name: "customers", schema: "shop", columns: [] },
  { name: "orders", schema: "shop", columns: [] },
]

function reply(status: number, body: unknown) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body) }
}

function sentBodies(): Record<string, unknown>[] {
  return authFetch.mock.calls.map((c) => JSON.parse((c[1] as { body: string }).body))
}

function Harness({ initial = "orders" }: { initial?: string }) {
  return (
    <DocumentExplorer
      connectionId="conn-1"
      collections={COLLECTIONS}
      collection={initial}
      onCollectionChange={() => {}}
    />
  )
}

describe("DocumentExplorer", () => {
  beforeEach(() => authFetch.mockReset())

  it("browses the picked collection and renders one column per field", async () => {
    authFetch.mockResolvedValueOnce(
      reply(200, {
        collection: "orders",
        columns: ["_id", "status", "total"],
        documents: [
          { _id: { $oid: "65f000000000000000000001" }, status: "paid", total: 12.5 },
          { _id: { $oid: "65f000000000000000000002" }, status: null },
        ],
        returned: 2,
        has_more: false,
        paging_mode: "keyset",
        execution_time_ms: 4,
      }),
    )
    render(<Harness />)

    await waitFor(() => expect(screen.getAllByTestId("doc-row")).toHaveLength(2))
    expect(authFetch.mock.calls[0][0]).toBe("/api/v1/explorer/documents/find")
    expect(sentBodies()[0]).toEqual({ connection_id: "conn-1", collection: "orders", limit: 50 })
    expect(screen.getByText(`ObjectId("65f000000000000000000001")`)).toBeInTheDocument()
    expect(screen.getByText("12.5")).toBeInTheDocument()
    expect(screen.getByText("null")).toBeInTheDocument()
    // total is absent from the second document
    expect(screen.getByTitle("Field not present in this document")).toBeInTheDocument()
    expect(screen.getByTestId("doc-summary")).toHaveTextContent("2 documents · 4 ms")
    expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument()
  })

  it("pages with the returned cursor and appends the next page", async () => {
    authFetch
      .mockResolvedValueOnce(
        reply(200, {
          collection: "orders",
          columns: ["_id", "a"],
          documents: [{ _id: 1, a: "first" }],
          has_more: true,
          paging_mode: "keyset",
          next_cursor: "CURSOR-1",
        }),
      )
      .mockResolvedValueOnce(
        reply(200, {
          collection: "orders",
          columns: ["_id", "b"],
          documents: [{ _id: 2, b: "second" }],
          has_more: false,
          paging_mode: "keyset",
        }),
      )
    render(<Harness />)

    fireEvent.click(await screen.findByRole("button", { name: /load more/i }))
    await waitFor(() => expect(screen.getAllByTestId("doc-row")).toHaveLength(2))
    expect(sentBodies()[1]).toMatchObject({ collection: "orders", cursor: "CURSOR-1" })
    expect(screen.getByText("first")).toBeInTheDocument()
    expect(screen.getByText("second")).toBeInTheDocument()
    expect(screen.getByRole("columnheader", { name: "b" })).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument()
  })

  it("shows the gateway's refusal with the path it points at", async () => {
    authFetch.mockResolvedValueOnce(reply(200, { collection: "orders", documents: [], has_more: false }))
    authFetch.mockResolvedValueOnce(
      reply(400, { error: "operator $where is not allowed", error_code: "operator_not_allowed", path: "filter.$where" }),
    )
    render(<Harness />)
    await screen.findByText("No documents match this filter")

    fireEvent.change(screen.getByLabelText("Filter"), { target: { value: `{"$where":"1"}` } })
    fireEvent.click(screen.getByRole("button", { name: "Find" }))

    const alert = await screen.findByRole("alert")
    expect(alert).toHaveTextContent("operator $where is not allowed")
    expect(alert).toHaveTextContent("at filter.$where")
  })

  describe("when the collection list could not be loaded", () => {
    const FAILURE = {
      title: "Failed to load schema",
      message: "Failed to build schema index: could not list collections: bad auth : authentication failed",
    }

    it("shows the reason and what to check instead of an empty database", () => {
      const onRetry = vi.fn()
      render(
        <DocumentExplorer
          connectionId="conn-1"
          collections={[]}
          collection=""
          onCollectionChange={() => {}}
          collectionsError={FAILURE}
          onRetryCollections={onRetry}
        />,
      )

      const alert = screen.getByRole("alert")
      expect(alert).toHaveTextContent("Could not list the collections on this connection")
      expect(alert).toHaveTextContent("bad auth : authentication failed")
      expect(alert).toHaveTextContent(/username and password/)
      expect(alert).toHaveTextContent(/IP allowlist/)
      expect(screen.queryByRole("option", { name: "No collections" })).not.toBeInTheDocument()
      expect(screen.getByRole("option", { name: "Collections could not be loaded" })).toBeInTheDocument()
      expect(screen.getByTestId("doc-summary")).not.toHaveTextContent("Pick a collection")
      expect(authFetch).not.toHaveBeenCalled()

      fireEvent.click(screen.getByRole("button", { name: "Retry" }))
      expect(onRetry).toHaveBeenCalledTimes(1)
    })

    it("uses the page's own hint when it has a more specific one", () => {
      render(
        <DocumentExplorer
          connectionId="conn-1"
          collections={[]}
          collection=""
          onCollectionChange={() => {}}
          collectionsError={{ ...FAILURE, hint: "The database rejected the login." }}
        />,
      )
      expect(screen.getByRole("alert")).toHaveTextContent("The database rejected the login.")
      expect(screen.getByRole("alert")).not.toHaveTextContent(/IP allowlist/)
    })

    it("waits for a reload in flight instead of showing the old failure", () => {
      render(
        <DocumentExplorer
          connectionId="conn-1"
          collections={[]}
          collection=""
          onCollectionChange={() => {}}
          collectionsError={FAILURE}
          loadingCollections
        />,
      )
      expect(screen.queryByRole("alert")).not.toBeInTheDocument()
    })
  })

  // Controls: a successful load renders its collections, and a database that really
  // has none still says so, with no error in either case.
  it("renders loaded collections with no error", async () => {
    authFetch.mockResolvedValueOnce(reply(200, { collection: "orders", documents: [], has_more: false }))
    render(<Harness />)
    await screen.findByText("No documents match this filter")
    const names = screen.getAllByRole("option").map((o) => o.textContent)
    expect(names.length).toBeGreaterThan(0)
    expect(names).toEqual(["customers", "orders"])
    expect(screen.queryByRole("alert")).not.toBeInTheDocument()
  })

  describe("on a server-level connection (collections from several databases)", () => {
    const SPREAD = [
      { name: "orders", schema: "eu", columns: [] },
      { name: "orders", schema: "us", columns: [] },
      { name: "users", schema: "us", columns: [] },
    ]
    const EMPTY = { collection: "orders", documents: [], has_more: false }

    function Picker({ onPick }: { onPick: (name: string, database?: string) => void }) {
      const [pick, setPick] = useState<{ name: string; database?: string }>({ name: "orders", database: "us" })
      return (
        <DocumentExplorer
          connectionId="conn-1"
          collections={SPREAD}
          collection={pick.name}
          database={pick.database}
          onCollectionChange={(name, database) => {
            onPick(name, database)
            setPick({ name, database })
          }}
        />
      )
    }

    it("groups collections by database and finds in the picked one", async () => {
      authFetch.mockResolvedValue(reply(200, EMPTY))
      const onPick = vi.fn()
      render(<Picker onPick={onPick} />)
      await waitFor(() => expect(sentBodies()[0]).toMatchObject({ collection: "orders", database: "us" }))
      expect(onPick).not.toHaveBeenCalled()

      const groups = within(screen.getByLabelText("Collection"))
        .getAllByRole("group")
        .map((g) => g.getAttribute("label"))
      expect(groups).toEqual(["eu", "us"])

      // The same collection name in another database is its own pick.
      const eu = screen.getAllByRole("option", { name: "orders" })[0] as HTMLOptionElement
      fireEvent.change(screen.getByLabelText("Collection"), { target: { value: eu.value } })
      expect(onPick).toHaveBeenLastCalledWith("orders", "eu")
      await waitFor(() => expect(sentBodies().at(-1)).toMatchObject({ collection: "orders", database: "eu" }))
    })
  })

  it("still says No collections for a reachable database that has none", () => {
    render(<DocumentExplorer connectionId="conn-1" collections={[]} collection="" onCollectionChange={() => {}} />)
    expect(screen.getByRole("option", { name: "No collections" })).toBeInTheDocument()
    expect(screen.queryByRole("alert")).not.toBeInTheDocument()
    expect(screen.getByTestId("doc-summary")).toHaveTextContent("Pick a collection to browse its documents")
  })

  it("catches malformed JSON before sending anything", async () => {
    authFetch.mockResolvedValueOnce(reply(200, { collection: "orders", documents: [], has_more: false }))
    render(<Harness />)
    await screen.findByText("No documents match this filter")

    fireEvent.change(screen.getByLabelText("Sort"), { target: { value: `{"total":` } })
    fireEvent.click(screen.getByRole("button", { name: "Find" }))

    expect(await screen.findByRole("alert")).toHaveTextContent(/Sort is not valid JSON/)
    expect(screen.getByLabelText("Sort")).toHaveAttribute("aria-invalid", "true")
    expect(authFetch).toHaveBeenCalledTimes(1)
  })

  // The Pipeline tab previews an aggregation. It is never run on its own — a pipeline
  // can be expensive, and half-typed it is not one — and its result has no paging
  // cursor, so it shows no Load more.
  describe("in Pipeline mode", () => {
    const FIND_PAGE = {
      collection: "orders",
      columns: ["_id", "status"],
      documents: [{ _id: 1, status: "paid" }],
      has_more: true,
      paging_mode: "keyset",
      next_cursor: "CURSOR-1",
    }
    const PIPELINE = `[{"$match":{"n":{"$gte":9007199254740993}}},{"$group":{"_id":"$status","n":{"$sum":1}}}]`

    function PipelineHarness({ mode = "pipeline" as const }: { mode?: "find" | "pipeline" }) {
      return (
        <DocumentExplorer
          connectionId="conn-1"
          collections={COLLECTIONS}
          collection="orders"
          database="shop"
          onCollectionChange={() => {}}
          mode={mode}
        />
      )
    }

    it("runs nothing until asked, then posts the pipeline as typed to /aggregate", async () => {
      authFetch.mockResolvedValueOnce(
        reply(200, {
          collection: "orders",
          columns: ["_id", "n"],
          documents: [{ _id: "paid", n: 3 }],
          returned: 1,
          has_more: true,
          execution_time_ms: 7,
          // The gateway sends no cursor for a pipeline. Were one ever to appear, it
          // would be a find cursor, and paging with it would leave the pipeline behind.
          paging_mode: "keyset",
          next_cursor: "CURSOR-1",
        }),
      )
      render(<PipelineHarness />)

      expect(screen.getByTestId("doc-summary")).toHaveTextContent("Write a pipeline and run it to preview its results")
      expect(authFetch).not.toHaveBeenCalled()

      fireEvent.change(screen.getByLabelText("Pipeline"), { target: { value: PIPELINE } })
      fireEvent.click(screen.getByRole("button", { name: "Run pipeline" }))

      await waitFor(() => expect(screen.getAllByTestId("doc-row")).toHaveLength(1))
      expect(authFetch.mock.calls[0][0]).toBe("/api/v1/explorer/documents/aggregate")
      // The 64-bit integer reaches the gateway unrounded.
      expect((authFetch.mock.calls[0][1] as { body: string }).body).toBe(
        `{"connection_id":"conn-1","collection":"orders","database":"shop","pipeline":${PIPELINE},"limit":50}`,
      )
      expect(screen.getByTestId("doc-summary")).toHaveTextContent("1 document (more available) · 7 ms")
      // has_more, but a pipeline preview is one page.
      expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument()
    })

    it("refuses a pipeline that is not an array of stages without sending it", async () => {
      render(<PipelineHarness />)

      fireEvent.change(screen.getByLabelText("Pipeline"), { target: { value: `{"$match":{}}` } })
      fireEvent.click(screen.getByRole("button", { name: "Run pipeline" }))

      expect(await screen.findByRole("alert")).toHaveTextContent("A pipeline must be a JSON array of stages")
      expect(screen.getByLabelText("Pipeline")).toHaveAttribute("aria-invalid", "true")
      expect(authFetch).not.toHaveBeenCalled()
    })

    it("shows the gateway's refusal with the stage it points at", async () => {
      authFetch.mockResolvedValueOnce(
        reply(400, { error: "stage $out is not allowed", error_code: "stage_not_allowed", path: "pipeline[1]" }),
      )
      render(<PipelineHarness />)

      fireEvent.change(screen.getByLabelText("Pipeline"), { target: { value: `[{"$match":{}},{"$out":"x"}]` } })
      fireEvent.click(screen.getByRole("button", { name: "Run pipeline" }))

      const alert = await screen.findByRole("alert")
      expect(alert).toHaveTextContent("stage $out is not allowed")
      expect(alert).toHaveTextContent("at pipeline[1]")
    })

    it("drops the other mode's result on a switch, so a find page is never shown as a pipeline's", async () => {
      authFetch.mockResolvedValueOnce(reply(200, FIND_PAGE))
      render(<Harness />)
      await waitFor(() => expect(screen.getAllByTestId("doc-row")).toHaveLength(1))
      expect(screen.getByRole("button", { name: /load more/i })).toBeInTheDocument()

      fireEvent.click(screen.getByRole("button", { name: "Pipeline mode" }))

      expect(screen.getByRole("button", { name: "Pipeline mode" })).toHaveAttribute("aria-pressed", "true")
      expect(screen.queryAllByTestId("doc-row")).toHaveLength(0)
      expect(screen.queryByRole("button", { name: /load more/i })).not.toBeInTheDocument()
      expect(screen.getByTestId("doc-summary")).toHaveTextContent("Write a pipeline and run it to preview its results")
      // Switching does not run anything.
      expect(authFetch).toHaveBeenCalledTimes(1)
    })
  })
})
