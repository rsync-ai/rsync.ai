import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { toast } from "sonner"

import type { AssetNode } from "@/components/explorer/assetLineage"
import { LineageHistorySection } from "@/components/explorer/LineageHistorySection"
import {
  columnChangeText,
  datasetNode,
  hasLineageHistory,
  isCDCWindow,
  lineageHistoryUrl,
  parseLineageHistory,
  runDuration,
  runOutcome,
} from "@/components/explorer/lineageHistory"
import {
  OPENLINEAGE_TARGET_URL,
  OpenLineageButton,
  OpenLineageDialog,
  deliveryText,
  parseOpenLineageTarget,
  targetPayload,
  validateTargetUrl,
} from "@/components/explorer/OpenLineageSettings"
import { authFetch } from "@/lib/api/auth-fetch"

// GET /api/v1/lineage/history (lineage_history.go) and /lineage/openlineage/target
// (openlineage_export.go) had no reader: the Lineage page's History section says what ran
// against a card, and an admin sets where each run is sent as an OpenLineage event.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
const role = vi.hoisted(() => ({ value: "admin", loading: false }))
vi.mock("@/contexts/WorkspaceContext", async () => {
  const { meetsRole } = await import("@/lib/workspace/roles")
  return {
    useWorkspaceRole: () => ({
      role: role.value,
      isLoading: role.loading,
      error: false,
      activeWorkspace: null,
      can: () => false,
      meets: (min: "owner" | "admin" | "member" | "viewer") => meetsRole(role.value, min),
    }),
  }
})

const mockFetch = authFetch as unknown as Mock
const res = (status: number, body: unknown) =>
  ({ ok: status >= 200 && status < 300, status, json: async () => body }) as unknown as Response

const C = "c0000000-0000-4000-8000-000000000001"
const W = "c0000000-0000-4000-8000-000000000002"
const MODEL: AssetNode = { id: "model:m-1", kind: "model", name: "orders_daily", ref_id: "m-1" }
const PIPE: AssetNode = { id: "pipeline:p-1", kind: "pipeline", name: "shop_sync", ref_id: "p-1" }
const ORDERS: AssetNode = { id: `table:${C}\x00public.orders`, kind: "table", name: "public.orders", connection_id: C }
const DAILY: AssetNode = { id: `table:${W}\x00analytics.orders_daily`, kind: "table", name: "analytics.orders_daily", connection_id: W }
const CAPTURED: AssetNode = { id: `table:${C}\x00captured\x00public.customers`, kind: "table", name: "public.customers", connection_id: C }
const LAKE: AssetNode = { id: `table:${W}\x00lake\x00gs://b/k`, kind: "table", name: "k", connection_id: W }
const CHART: AssetNode = { id: "chart:x", kind: "chart", name: "Revenue" }
const byId = new Map([MODEL, PIPE, ORDERS, DAILY, CAPTURED].map((n) => [n.id, n]))

const run = (over: Record<string, unknown>) => ({
  id: "e-1",
  job_kind: "model",
  job_id: "m-1",
  job_name: "orders_daily",
  run_ref: "r-1",
  event_type: "COMPLETE",
  started_at: "2026-10-15T08:00:00Z",
  ended_at: "2026-10-15T08:02:05Z",
  inputs: [{ connection_id: W, name: "public.orders" }],
  outputs: [{ connection_id: W, name: "analytics.orders_daily" }],
  ...over,
})

beforeEach(() => {
  mockFetch.mockReset()
  vi.mocked(toast.success).mockReset()
  vi.mocked(toast.error).mockReset()
  role.value = "admin"
  role.loading = false
})

describe("lineage history helpers", () => {
  it("reads the answer defensively", () => {
    expect(parseLineageHistory(null)).toEqual({
      node: "",
      supported: false,
      retention_days: 0,
      total_runs: 0,
      runs: [],
      skipped_runs: 0,
      last_skipped_at: null,
      column_changes: null,
    })
    const h = parseLineageHistory({
      node: MODEL.id,
      supported: true,
      retention_days: 365,
      runs: [run({ started_at: null, inputs: [{ connection_id: C, name: "" }, { connection_id: C, name: "a", captured: true }] })],
      column_changes: [{ at: "2026-10-15T08:00:00Z", columns_known: true, added: ["status", 4], dropped: null }],
    })
    expect(h.runs[0].started_at).toBeNull()
    // A table with no name is dropped, not drawn as a blank.
    expect(h.runs[0].inputs).toEqual([{ connection_id: C, name: "a", captured: true }])
    expect(h.column_changes).toEqual([{ at: "2026-10-15T08:00:00Z", columns_known: true, added: ["status"], dropped: [] }])
    // An answer without the total (an older server) counts what it sent; a total below that is not believed.
    expect(h.total_runs).toBe(1)
    expect(parseLineageHistory({ runs: [run({}), run({ id: "e-2" })], total_runs: 54 }).total_runs).toBe(54)
    expect(parseLineageHistory({ runs: [run({}), run({ id: "e-2" })], total_runs: 1 }).total_runs).toBe(2)
    expect(parseLineageHistory({ runs: [], total_runs: "54" }).total_runs).toBe(0)
  })

  it("reads how many runs the list left out as skipped", () => {
    const h = parseLineageHistory({ runs: [], skipped_runs: 53, last_skipped_at: "2026-10-15T09:00:00Z" })
    expect(h.skipped_runs).toBe(53)
    expect(h.last_skipped_at).toBe("2026-10-15T09:00:00Z")
    // An older server sends neither; nothing believable is a count of none.
    expect(parseLineageHistory({ runs: [] })).toMatchObject({ skipped_runs: 0, last_skipped_at: null })
    expect(parseLineageHistory({ runs: [], skipped_runs: -3, last_skipped_at: 5 })).toMatchObject({ skipped_runs: 0, last_skipped_at: null })
    expect(parseLineageHistory({ runs: [], skipped_runs: "53", last_skipped_at: "" })).toMatchObject({ skipped_runs: 0, last_skipped_at: null })
  })

  it("asks only for the cards the backend keeps runs for", () => {
    expect([MODEL, PIPE, ORDERS, CAPTURED].every(hasLineageHistory)).toBe(true)
    expect(hasLineageHistory(LAKE)).toBe(false)
    expect(hasLineageHistory(CHART)).toBe(false)
    expect(hasLineageHistory({ id: "dashboard:d", kind: "dashboard" })).toBe(false)
    // The node id's NUL separator is sent, escaped.
    expect(lineageHistoryUrl(ORDERS.id)).toBe(`/api/v1/lineage/history?node=table%3A${C}%00public.orders`)
    expect(lineageHistoryUrl(MODEL.id, 20)).toBe("/api/v1/lineage/history?node=model%3Am-1&limit=20")
    // Skipped runs are left out unless asked for.
    expect(lineageHistoryUrl(MODEL.id, undefined, true)).toBe("/api/v1/lineage/history?node=model%3Am-1&skipped=1")
    expect(lineageHistoryUrl(MODEL.id, 20, true)).toBe("/api/v1/lineage/history?node=model%3Am-1&limit=20&skipped=1")
    expect(lineageHistoryUrl(MODEL.id, 20, false)).toBe("/api/v1/lineage/history?node=model%3Am-1&limit=20")
  })

  it("names how a run ended and how long it took", () => {
    expect(runOutcome("COMPLETE", "model")).toEqual({ text: "Completed", tone: "ok" })
    expect(runOutcome("FAIL", "pipeline")).toEqual({ text: "Failed", tone: "failed" })
    // One event type, two meanings: a model run that was skipped, a pipeline run that was cancelled.
    expect(runOutcome("ABORT", "model")).toEqual({ text: "Skipped", tone: "neutral" })
    expect(runOutcome("ABORT", "pipeline")).toEqual({ text: "Cancelled", tone: "neutral" })
    expect(runOutcome("RUNNING", "model")).toEqual({ text: "running", tone: "neutral" })
    expect(runOutcome("", "model")).toEqual({ text: "Unknown", tone: "neutral" })
    expect(runDuration({ started_at: "2026-10-15T08:00:00Z", ended_at: "2026-10-15T08:02:05Z" })).toBe("2m 5s")
    expect(runDuration({ started_at: "2026-10-15T08:00:00Z", ended_at: "2026-10-15T08:00:09Z" })).toBe("9s")
    expect(runDuration({ started_at: "2026-10-15T06:00:00Z", ended_at: "2026-10-15T08:00:00Z" })).toBe("2h")
    expect(runDuration({ started_at: null, ended_at: "2026-10-15T08:00:00Z" })).toBe("")
    expect(runDuration({ started_at: "2026-10-15T09:00:00Z", ended_at: "2026-10-15T08:00:00Z" })).toBe("")
    expect(runDuration({ started_at: "2026-10-15T08:00:00Z", ended_at: "" })).toBe("")
    // A run that began and ended in the same instant took no time to speak of: no "0ms" is claimed.
    expect(runDuration({ started_at: "2026-10-15T08:00:00Z", ended_at: "2026-10-15T08:00:00Z" })).toBe("")
    expect(isCDCWindow({ job_kind: "pipeline", run_ref: "cdc-trail-5m-123" })).toBe(true)
    expect(isCDCWindow({ job_kind: "pipeline", run_ref: "exec-1" })).toBe(false)
  })

  it("finds a run's table on the graph by id, without case, or by its one bare name", () => {
    expect(datasetNode({ connection_id: C, name: "public.orders", captured: false }, byId)).toBe(ORDERS)
    expect(datasetNode({ connection_id: C, name: "PUBLIC.Orders", captured: false }, byId)).toBe(ORDERS)
    expect(datasetNode({ connection_id: C, name: "orders", captured: false }, byId)).toBe(ORDERS)
    expect(datasetNode({ connection_id: C, name: "public.customers", captured: true }, byId)).toBe(CAPTURED)
    // A captured name is never matched to a placed card, nor a placed one to a captured card.
    expect(datasetNode({ connection_id: C, name: "public.orders", captured: true }, byId)).toBeUndefined()
    expect(datasetNode({ connection_id: C, name: "customers", captured: false }, byId)).toBeUndefined()
    // Another connection's table of the same name is not it.
    expect(datasetNode({ connection_id: W, name: "public.orders", captured: false }, byId)).toBeUndefined()
    // Two tables end in the same bare name: neither is guessed.
    const two = new Map(byId)
    const other: AssetNode = { id: `table:${C}\x00sales.orders`, kind: "table", name: "sales.orders", connection_id: C }
    two.set(other.id, other)
    expect(datasetNode({ connection_id: C, name: "orders", captured: false }, two)).toBeUndefined()
  })

  it("says what a column change was", () => {
    const at = "2026-10-15T08:00:00Z"
    expect(columnChangeText({ at, columns_known: true, added: ["status"], dropped: [] })).toBe("Added status.")
    expect(columnChangeText({ at, columns_known: true, added: ["a", "b"], dropped: ["c"] })).toBe("Added a, b; dropped c.")
    expect(columnChangeText({ at, columns_known: true, added: [], dropped: [] })).toBe("Its SQL changed; its columns stayed the same.")
    expect(columnChangeText({ at, columns_known: false, added: [], dropped: [] })).toBe(
      "Its SQL changed; its columns were not read for both versions.",
    )
  })
})

describe("the History section", () => {
  const show = (node: AssetNode, onSelect = vi.fn(), reloadTick = 0) =>
    render(<LineageHistorySection node={node} byId={byId} reloadTick={reloadTick} onSelect={onSelect} />)

  it("lists a model's runs, links the tables the graph draws, and its column changes newest first", async () => {
    mockFetch.mockResolvedValue(
      res(200, {
        node: MODEL.id,
        supported: true,
        retention_days: 365,
        runs: [
          run({ id: "e-3", event_type: "FAIL", inputs: [{ connection_id: C, name: "public.orders" }, { connection_id: C, name: "public.gone" }] }),
          run({ id: "e-2", event_type: "ABORT", started_at: null }),
          run({ id: "e-1" }),
        ],
        column_changes: [
          { at: "2026-10-01T00:00:00Z", columns_known: false, added: [], dropped: [] },
          { at: "2026-10-10T00:00:00Z", columns_known: true, added: ["status"], dropped: [] },
        ],
      }),
    )
    const onSelect = vi.fn()
    show(MODEL, onSelect)
    expect(screen.getByText("Reading its runs…")).toBeInTheDocument()
    const runs = await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/lineage/history?node=model%3Am-1", { method: "GET" })
    expect(screen.getByRole("heading", { name: "History (3)" })).toBeInTheDocument()

    const items = within(runs).getAllByRole("listitem")
    expect(items.map((li) => li.getAttribute("data-outcome"))).toEqual(["FAIL", "ABORT", "COMPLETE"])
    expect(within(items[0]).getByText("Failed")).toBeInTheDocument()
    expect(within(items[1]).getByText("Skipped")).toBeInTheDocument()
    expect(within(items[2]).getByText("Completed")).toBeInTheDocument()
    expect(items[2]).toHaveTextContent("took 2m 5s")
    // No start recorded: no duration is made up.
    expect(items[1]).not.toHaveTextContent("took")
    expect(items[0].querySelector("time")).toHaveAttribute("dateTime", "2026-10-15T08:02:05Z")
    // On its own card, the run's job is not named again.
    expect(items[0]).not.toHaveTextContent("By ")

    // A drawn table is a button to its card; one the graph no longer draws is only named.
    await userEvent.click(within(items[0]).getByRole("button", { name: "Select public.orders" }))
    expect(onSelect).toHaveBeenCalledWith(ORDERS.id)
    expect(within(items[0]).queryByRole("button", { name: /public\.gone/ })).toBeNull()
    expect(within(items[0]).getByTitle("Not drawn on the graph now")).toHaveTextContent("public.gone")

    const changes = screen.getByRole("list", { name: "Column changes of orders_daily" })
    expect(within(changes).getAllByRole("listitem").map((li) => li.textContent?.split(" · ")[1])).toEqual([
      "Added status.",
      "Its SQL changed; its columns were not read for both versions.",
    ])
    expect(screen.getByText("Runs are kept 365 days.")).toBeInTheDocument()
  })

  it("names the job on a table's runs, and says a CDC window's changes landed", async () => {
    mockFetch.mockResolvedValue(
      res(200, {
        node: CAPTURED.id,
        supported: true,
        retention_days: 30,
        runs: [
          run({
            id: "e-9",
            job_kind: "pipeline",
            job_id: "p-1",
            job_name: "shop_sync",
            run_ref: "cdc-trail-5m-1760000000",
            inputs: [],
            outputs: [{ connection_id: C, name: "public.customers", captured: true }],
          }),
          run({ id: "e-8", job_kind: "pipeline", job_id: "p-gone", job_name: "old_sync", run_ref: "x-1", inputs: [] }),
        ],
      }),
    )
    const onSelect = vi.fn()
    show(CAPTURED, onSelect)
    const items = within(await screen.findByRole("list", { name: "Runs of public.customers" })).getAllByRole("listitem")
    expect(items[0]).toHaveTextContent("changes landed")
    expect(items[0]).not.toHaveTextContent("Read")
    await userEvent.click(within(items[0]).getByRole("button", { name: "Select shop_sync" }))
    expect(onSelect).toHaveBeenCalledWith(PIPE.id)
    // A pipeline the graph no longer has is named from the run.
    expect(items[1]).toHaveTextContent("By old_sync")
    expect(items[1]).not.toHaveTextContent("changes landed")
    expect(screen.getByText("Runs are kept 30 days.")).toBeInTheDocument()
  })

  it("shows five runs, then all of them", async () => {
    mockFetch.mockResolvedValue(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, runs: [1, 2, 3, 4, 5, 6, 7].map((i) => run({ id: `e-${i}` })) }),
    )
    show(MODEL)
    const runs = await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(within(runs).getAllByRole("listitem")).toHaveLength(5)
    await userEvent.click(screen.getByRole("button", { name: "Show all 7" }))
    expect(within(runs).getAllByRole("listitem")).toHaveLength(7)
    expect(screen.queryByRole("button", { name: /Show all/ })).toBeNull()
  })

  it("counts every run the server holds, not the page it loaded", async () => {
    // 7 of 54 loaded: the heading must not read "History (7)" as if that were all of it.
    mockFetch.mockResolvedValue(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 54, runs: [1, 2, 3, 4, 5, 6, 7].map((i) => run({ id: `e-${i}` })) }),
    )
    const { unmount } = show(MODEL)
    const runs = await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(screen.getByRole("heading", { name: "History (7 of 54)" })).toBeInTheDocument()
    // The button cannot promise "all" of 54 when it holds 7.
    expect(screen.queryByRole("button", { name: /Show all/ })).toBeNull()
    await userEvent.click(screen.getByRole("button", { name: "Show latest 7" }))
    expect(within(runs).getAllByRole("listitem")).toHaveLength(7)
    expect(screen.getByRole("heading", { name: "History (7 of 54)" })).toBeInTheDocument()
    unmount()

    // Every run loaded: the count is plain.
    mockFetch.mockResolvedValue(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 7, runs: [1, 2, 3, 4, 5, 6, 7].map((i) => run({ id: `e-${i}` })) }),
    )
    show(MODEL)
    await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(screen.getByRole("heading", { name: "History (7)" })).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Show all 7" })).toBeInTheDocument()
  })

  it("calls a skipped model run skipped, with no tables and no time it did not take", async () => {
    mockFetch.mockResolvedValue(
      res(200, {
        node: MODEL.id,
        supported: true,
        retention_days: 365,
        total_runs: 2,
        runs: [
          // As the server sends a skip: it ran nothing, so it read and wrote nothing, and ended as it began.
          run({ id: "e-2", event_type: "ABORT", started_at: "2026-10-15T08:00:00Z", ended_at: "2026-10-15T08:00:00Z", inputs: [], outputs: [] }),
          run({ id: "e-1" }),
        ],
      }),
    )
    show(MODEL)
    const items = within(await screen.findByRole("list", { name: "Runs of orders_daily" })).getAllByRole("listitem")
    expect(within(items[0]).getByText("Skipped")).toBeInTheDocument()
    expect(items[0]).not.toHaveTextContent("took")
    expect(items[0]).not.toHaveTextContent("Read")
    expect(items[0]).not.toHaveTextContent("Wrote")
    expect(items[1]).toHaveTextContent("took 2m 5s")
  })

  it("calls a cancelled pipeline run cancelled, and keeps the tables it had moved", async () => {
    mockFetch.mockResolvedValue(
      res(200, {
        node: PIPE.id,
        supported: true,
        retention_days: 365,
        total_runs: 1,
        runs: [
          run({
            id: "e-5",
            job_kind: "pipeline",
            job_id: "p-1",
            job_name: "shop_sync",
            run_ref: "exec-9",
            event_type: "ABORT",
            inputs: [],
            outputs: [{ connection_id: W, name: "analytics.orders_daily" }],
          }),
        ],
      }),
    )
    show(PIPE)
    const [item] = within(await screen.findByRole("list", { name: "Runs of shop_sync" })).getAllByRole("listitem")
    expect(within(item).getByText("Cancelled")).toBeInTheDocument()
    expect(within(item).queryByText("Skipped")).toBeNull()
    expect(item).toHaveTextContent("Wrote")
    expect(item).toHaveTextContent("analytics.orders_daily")
  })

  it("leaves skipped runs out, says how many, and lists them when asked", async () => {
    // A model skipped every hour has few runs that ran: the server lists those, and counts the rest.
    mockFetch.mockResolvedValueOnce(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 53, last_skipped_at: "2026-10-15T09:00:00Z", runs: [run({})] }),
    )
    show(MODEL)
    const runs = await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(within(runs).getAllByRole("listitem")).toHaveLength(1)
    expect(screen.getByRole("heading", { name: "History (1)" })).toBeInTheDocument()
    expect(screen.getByText(/53 runs skipped/)).toBeInTheDocument()
    expect(screen.getByText(/53 runs skipped/).querySelector("time")).toHaveAttribute("dateTime", "2026-10-15T09:00:00Z")
    expect(mockFetch).toHaveBeenLastCalledWith("/api/v1/lineage/history?node=model%3Am-1", { method: "GET" })

    // Asked for, they are listed, as skipped; the count stays, for the way back.
    mockFetch.mockResolvedValueOnce(
      res(200, {
        node: MODEL.id,
        supported: true,
        retention_days: 365,
        total_runs: 2,
        skipped_runs: 53,
        last_skipped_at: "2026-10-15T09:00:00Z",
        runs: [run({ id: "e-2", event_type: "ABORT", ended_at: "2026-10-15T09:00:00Z", inputs: [], outputs: [] }), run({})],
      }),
    )
    await userEvent.click(screen.getByRole("button", { name: "Show skipped" }))
    await waitFor(() =>
      expect(mockFetch).toHaveBeenLastCalledWith("/api/v1/lineage/history?node=model%3Am-1&skipped=1", { method: "GET" }),
    )
    const all = within(await screen.findByRole("list", { name: "Runs of orders_daily" })).getAllByRole("listitem")
    expect(all.map((li) => li.getAttribute("data-outcome"))).toEqual(["ABORT", "COMPLETE"])
    expect(within(all[0]).getByText("Skipped")).toBeInTheDocument()

    mockFetch.mockResolvedValueOnce(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 53, last_skipped_at: "2026-10-15T09:00:00Z", runs: [run({})] }),
    )
    await userEvent.click(screen.getByRole("button", { name: "Hide skipped" }))
    await waitFor(() => expect(mockFetch).toHaveBeenLastCalledWith("/api/v1/lineage/history?node=model%3Am-1", { method: "GET" }))
    expect(await screen.findByRole("button", { name: "Show skipped" })).toBeInTheDocument()
  })

  it("does not show the list it had while the one asked for is read", async () => {
    mockFetch.mockResolvedValueOnce(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 3, last_skipped_at: "2026-10-15T09:00:00Z", runs: [run({})] }),
    )
    // The second read stays out: the list of the first is not shown as if it were the answer.
    mockFetch.mockReturnValueOnce(new Promise(() => {}))
    show(MODEL)
    await userEvent.click(await screen.findByRole("button", { name: "Show skipped" }))
    expect(await screen.findByText("Reading its runs…")).toBeInTheDocument()
    expect(screen.queryByRole("list", { name: "Runs of orders_daily" })).toBeNull()
    expect(screen.queryByRole("button", { name: /skipped/ })).toBeNull()
  })

  it("says one run skipped, and nothing of skipped runs when there are none", async () => {
    mockFetch.mockResolvedValueOnce(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 1, last_skipped_at: null, runs: [run({})] }),
    )
    const { unmount } = show(MODEL)
    expect(await screen.findByText(/1 run skipped/)).toBeInTheDocument()
    expect(screen.queryByText(/runs skipped/)).toBeNull()
    unmount()

    mockFetch.mockResolvedValueOnce(res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 0, runs: [run({})] }))
    show(MODEL)
    await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(screen.queryByText(/skipped/)).toBeNull()
    expect(screen.queryByRole("button", { name: /skipped/ })).toBeNull()
  })

  it("says a model that was only ever skipped has not run", async () => {
    mockFetch.mockResolvedValue(
      res(200, { node: MODEL.id, supported: true, retention_days: 365, total_runs: 0, skipped_runs: 4, last_skipped_at: "2026-10-15T09:00:00Z", runs: [] }),
    )
    show(MODEL)
    expect(await screen.findByText("Nothing has run yet: every recorded run was skipped.")).toBeInTheDocument()
    expect(screen.queryByText("No runs recorded yet.")).toBeNull()
    expect(screen.getByText(/4 runs skipped/)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Show skipped" })).toBeInTheDocument()
  })

  it("asks for skipped runs of the card selected, not of the one before it", async () => {
    const OTHER: AssetNode = { id: "model:m-2", kind: "model", name: "orders_weekly", ref_id: "m-2" }
    mockFetch.mockImplementation(async (url: string) =>
      res(200, { node: url, supported: true, retention_days: 365, total_runs: 1, skipped_runs: 3, last_skipped_at: "2026-10-15T09:00:00Z", runs: [run({})] }),
    )
    const { rerender } = show(MODEL)
    await userEvent.click(await screen.findByRole("button", { name: "Show skipped" }))
    expect(await screen.findByRole("button", { name: "Hide skipped" })).toBeInTheDocument()

    rerender(<LineageHistorySection node={OTHER} byId={byId} reloadTick={0} onSelect={vi.fn()} />)
    expect(await screen.findByRole("button", { name: "Show skipped" })).toBeInTheDocument()
    expect(mockFetch).toHaveBeenLastCalledWith("/api/v1/lineage/history?node=model%3Am-2", { method: "GET" })
  })

  it("says when nothing ran, and that a table has no runs yet", async () => {
    mockFetch.mockResolvedValue(res(200, { node: MODEL.id, supported: true, retention_days: 365, runs: [], column_changes: [] }))
    const { unmount } = show(MODEL)
    expect(await screen.findByText("No runs recorded yet.")).toBeInTheDocument()
    expect(screen.queryByRole("heading", { name: "Column changes" })).toBeNull()
    unmount()
    mockFetch.mockResolvedValue(res(200, { node: ORDERS.id, supported: true, retention_days: 365, runs: [] }))
    show(ORDERS)
    expect(await screen.findByText("No recorded run has read or written it yet.")).toBeInTheDocument()
  })

  it("says a failed read and asks again", async () => {
    mockFetch.mockResolvedValueOnce(res(500, { error: "failed to load lineage history" }))
    show(MODEL)
    expect(await screen.findByText("Its runs could not be read (HTTP 500).")).toBeInTheDocument()
    mockFetch.mockResolvedValueOnce(res(200, { node: MODEL.id, supported: true, retention_days: 365, runs: [run({})] }))
    await userEvent.click(screen.getByRole("button", { name: "Try again" }))
    expect(await screen.findByRole("list", { name: "Runs of orders_daily" })).toBeInTheDocument()
    expect(mockFetch).toHaveBeenCalledTimes(2)
  })

  it("says an unreachable server", async () => {
    mockFetch.mockRejectedValueOnce(new Error("offline"))
    show(MODEL)
    expect(await screen.findByText("Could not reach the server to read its runs.")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument()
  })

  it("is asked again on the page's Refresh", async () => {
    mockFetch.mockResolvedValue(res(200, { node: MODEL.id, supported: true, retention_days: 365, runs: [run({})] }))
    const { rerender } = show(MODEL)
    await screen.findByRole("list", { name: "Runs of orders_daily" })
    expect(mockFetch).toHaveBeenCalledTimes(1)
    rerender(<LineageHistorySection node={MODEL} byId={byId} reloadTick={1} onSelect={vi.fn()} />)
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2))
  })

  it("is not shown, nor asked for, on a lake table or a chart", () => {
    const { container: lake } = show(LAKE)
    expect(lake).toBeEmptyDOMElement()
    const { container: chart } = show(CHART)
    expect(chart).toBeEmptyDOMElement()
    expect(mockFetch).not.toHaveBeenCalled()
  })
})

describe("OpenLineage settings helpers", () => {
  it("reads the target defensively", () => {
    expect(parseOpenLineageTarget(null)).toEqual({
      configured: false,
      url: "",
      has_auth_header: false,
      enabled: false,
      last_status: "",
      last_status_code: null,
      last_delivered_at: null,
      updated_at: null,
    })
  })

  it("checks the URL as the server does", () => {
    expect(validateTargetUrl("")).toBe("Enter the URL to send events to.")
    expect(validateTargetUrl("ftp://x.example.com/")).toBe("The URL must be an http or https URL with a host.")
    expect(validateTargetUrl("not a url")).toBe("The URL must be an http or https URL with a host.")
    expect(validateTargetUrl("https://u:p@x.example.com/")).toBe("Put a token in the auth header, not in the URL.")
    expect(validateTargetUrl(`https://x.example.com/${"a".repeat(2048)}`)).toMatch(/at most 2048/)
    expect(validateTargetUrl(" https://marquez.example.com/api/v1/lineage ")).toBeNull()
  })

  it("keeps the stored auth header unless one is typed or it is cleared", () => {
    const base = { url: " https://x.example.com/ ", enabled: true, authHeader: "", clearAuth: false }
    expect(targetPayload(base)).toEqual({ url: "https://x.example.com/", enabled: true })
    expect(targetPayload({ ...base, clearAuth: true })).toEqual({ url: "https://x.example.com/", enabled: true, auth_header: "" })
    expect(targetPayload({ ...base, authHeader: " Bearer t ", clearAuth: true })).toEqual({
      url: "https://x.example.com/",
      enabled: true,
      auth_header: "Bearer t",
    })
  })

  it("says how the last delivery went", () => {
    const t = parseOpenLineageTarget({ configured: true, url: "https://x.example.com/", enabled: true })
    expect(deliveryText(parseOpenLineageTarget({}))).toBe("No target is set: events can only be pulled.")
    expect(deliveryText(t)).toBe("Nothing sent yet: the next run that ends is.")
    expect(deliveryText({ ...t, enabled: false })).toBe("Nothing sent yet. Sending is off.")
    expect(deliveryText({ ...t, last_status: "failed", last_status_code: 503 })).toBe("The last event failed (HTTP 503).")
    expect(deliveryText({ ...t, last_status: "failed" })).toBe("The last event failed (no response).")
    expect(deliveryText({ ...t, last_status: "refused" })).toBe("The last event was not sent: the address is not a public one.")
    expect(deliveryText({ ...t, last_status: "delivered", last_status_code: 201 })).toBe("The last event was delivered (HTTP 201).")
  })
})

describe("the OpenLineage dialog", () => {
  const STORED = {
    configured: true,
    url: "https://marquez.example.com/api/v1/lineage",
    has_auth_header: true,
    enabled: true,
    last_status: "failed",
    last_status_code: 503,
    last_delivered_at: null,
    updated_at: "2026-10-15T08:00:00Z",
  }
  const sent = (method: string) =>
    mockFetch.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === method)

  it("is offered to an admin and to no one else", async () => {
    role.value = "member"
    const { rerender } = render(<OpenLineageButton />)
    expect(screen.queryByRole("button", { name: "OpenLineage" })).toBeNull()
    role.value = "admin"
    role.loading = true
    rerender(<OpenLineageButton />)
    expect(screen.queryByRole("button", { name: "OpenLineage" })).toBeNull()
    role.loading = false
    rerender(<OpenLineageButton />)
    mockFetch.mockResolvedValue(res(200, STORED))
    await userEvent.click(screen.getByRole("button", { name: "OpenLineage" }))
    expect(await screen.findByRole("dialog")).toBeInTheDocument()
    expect(mockFetch).toHaveBeenCalledWith(OPENLINEAGE_TARGET_URL, { method: "GET", cache: "no-store" })
  })

  it("shows the target without its auth header, and saves without resending it", async () => {
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    const url = await screen.findByLabelText("Endpoint URL")
    expect(url).toHaveValue(STORED.url)
    const auth = screen.getByLabelText("Authorization header")
    // Write-only: the API never gives the value, and the field starts empty.
    expect(auth).toHaveValue("")
    expect(auth).toHaveAttribute("type", "password")
    expect(auth).toHaveAttribute("placeholder", "Stored. Type a new value to replace it")
    expect(screen.getByText("The last event failed (HTTP 503).")).toBeInTheDocument()
    expect(screen.getByText(/GET \/api\/v1\/lineage\/openlineage\?since=/)).toBeInTheDocument()

    mockFetch.mockResolvedValueOnce(res(200, { ...STORED, enabled: false, last_status: "failed" }))
    await userEvent.click(screen.getByRole("switch"))
    await userEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(sent("PUT")).toHaveLength(1))
    expect(JSON.parse(sent("PUT")[0][1].body)).toEqual({ url: STORED.url, enabled: false })
    expect(toast.success).toHaveBeenCalledWith("Saved. Sending is off.")
  })

  it("says what a pull holds and what a push does", async () => {
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    await screen.findByLabelText("Endpoint URL")
    const dialog = screen.getByRole("dialog")
    // What an event holds: not only table names. Each is a thing a reader of the export gets.
    expect(dialog).toHaveTextContent(/host and port/)
    expect(dialog).toHaveTextContent(/Snowflake account/)
    expect(dialog).toHaveTextContent(/connection's id/)
    // A BigQuery table is named by its project, in the table's name, not in a namespace.
    expect(dialog).toHaveTextContent(/BigQuery table's project is in its name/)
    // The job is `name (kind id)`; columns are on a model's output tables only.
    expect(dialog).toHaveTextContent(/job's name with its kind and id, under your workspace's id/)
    expect(dialog).toHaveTextContent(/A model's output tables also carry their columns and the column mapping/)
    expect(dialog).toHaveTextContent(/Never the SQL, row counts, row values or error text/)
    expect(dialog).toHaveTextContent(/Private models are left out/)

    const pull = within(screen.getByRole("region", { name: "Pull" }))
    expect(pull.getByText(/GET .*\?since=&until=&cursor=&limit=/)).toBeInTheDocument()
    expect(pull.getByText(/500 events a page/)).toBeInTheDocument()
    expect(pull.getByText(/up to 1000 with limit/)).toBeInTheDocument()
    expect(pull.getByText(/since is included, until is not/)).toBeInTheDocument()

    const push = within(screen.getByRole("region", { name: "Push" }))
    expect(push.getByText(/tried up to 3 times/)).toBeInTheDocument()
    expect(push.getByText(/15 seconds/)).toBeInTheDocument()
    expect(push.getByText(/redirect is not followed/)).toBeInTheDocument()
    // Held in memory: a full queue, or a restart, loses a push but not the pull.
    expect(push.getByText(/256/)).toBeInTheDocument()
    expect(push.getByText(/stay in the pull/)).toBeInTheDocument()
  })

  it("sends a typed header, or \"\" to clear the stored one", async () => {
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    await screen.findByLabelText("Endpoint URL")

    await userEvent.click(screen.getByRole("button", { name: "Clear" }))
    expect(screen.getByText("The stored header is removed when you save.")).toBeInTheDocument()
    mockFetch.mockResolvedValueOnce(res(200, { ...STORED, has_auth_header: false }))
    await userEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(sent("PUT")).toHaveLength(1))
    expect(JSON.parse(sent("PUT")[0][1].body)).toEqual({ url: STORED.url, enabled: true, auth_header: "" })
    // Nothing stored now: no Clear, and the plain placeholder.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Clear" })).toBeNull())

    await userEvent.type(screen.getByLabelText("Authorization header"), "Bearer abc")
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    await userEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(sent("PUT")).toHaveLength(2))
    expect(JSON.parse(sent("PUT")[1][1].body)).toEqual({ url: STORED.url, enabled: true, auth_header: "Bearer abc" })
    // The typed value does not stay on screen once saved.
    await waitFor(() => expect(screen.getByLabelText("Authorization header")).toHaveValue(""))
  })

  it("refuses a URL the server would, before sending", async () => {
    mockFetch.mockResolvedValueOnce(res(200, parseOpenLineageTarget({})))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    const url = await screen.findByLabelText("Endpoint URL")
    expect(screen.getByText("No target is set: events can only be pulled.")).toBeInTheDocument()
    // No target: nothing to remove.
    expect(screen.queryByRole("button", { name: /Remove/ })).toBeNull()
    await userEvent.type(url, "https://user:pw@x.example.com/")
    await userEvent.click(screen.getByRole("button", { name: "Save" }))
    expect(toast.error).toHaveBeenCalledWith("Put a token in the auth header, not in the URL.")
    expect(sent("PUT")).toHaveLength(0)
  })

  it("says the server's refusal", async () => {
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    await screen.findByLabelText("Endpoint URL")
    mockFetch.mockResolvedValueOnce(res(403, { error: "requires workspace role admin" }))
    await userEvent.click(screen.getByRole("button", { name: "Save" }))
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("requires workspace role admin"))
  })

  it("removes the target on a second click", async () => {
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    await screen.findByLabelText("Endpoint URL")
    await userEvent.click(screen.getByRole("button", { name: "Remove…" }))
    expect(sent("DELETE")).toHaveLength(0)
    mockFetch.mockResolvedValueOnce(res(200, parseOpenLineageTarget({})))
    await userEvent.click(screen.getByRole("button", { name: "Remove the target" }))
    await waitFor(() => expect(sent("DELETE")).toHaveLength(1))
    expect(await screen.findByText("No target is set: events can only be pulled.")).toBeInTheDocument()
    expect(screen.getByLabelText("Endpoint URL")).toHaveValue("")
  })

  it("says a failed read and asks again", async () => {
    mockFetch.mockResolvedValueOnce(res(500, { error: "failed to read the OpenLineage target" }))
    render(<OpenLineageDialog onOpenChange={vi.fn()} />)
    expect(await screen.findByText("failed to read the OpenLineage target")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull()
    mockFetch.mockResolvedValueOnce(res(200, STORED))
    await userEvent.click(screen.getByRole("button", { name: "Try again" }))
    expect(await screen.findByLabelText("Endpoint URL")).toHaveValue(STORED.url)
  })
})
