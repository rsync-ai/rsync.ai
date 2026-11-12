/**
 * Run detail (launch review PR1 and PR2).
 *
 * PR1: the overview row was five columns wide, but its "Nodes Executed" card shows
 * only when a run recorded node results, which batch and CDC runs never do: most
 * runs drew four cards and a hole where the fifth belonged. The row is now always
 * four cards, and the node count reads in the timeline's own header.
 *
 * PR2: the page is titled by its pipeline, not "Execution Details" over a raw id;
 * the cards are the kit's StatCards; Duration says how it changed against the
 * previous run; the error card shows only for a run that did not succeed (a
 * succeeded run can keep an old error message); and the transform log card shows
 * only when there are logs, since most runs have none.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/headers", () => ({
  cookies: async () => ({ get: () => undefined }),
}))
vi.mock("next/navigation", () => ({
  notFound: () => {
    throw new Error("NEXT_NOT_FOUND")
  },
  unstable_rethrow: () => {},
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))
vi.mock("@/components/executions/ExecutionDetailClient", () => ({ ExecutionDetailClient: () => null }))
vi.mock("@/components/executions/RecentRunsCard", () => ({
  RecentRunsCard: ({ runs }: { runs: unknown[] }) => <div data-testid="recent-runs">{runs.length}</div>,
}))
vi.mock("@/components/executions/RunLogPanel", () => ({ RunLogPanel: () => null }))
vi.mock("@/components/transforms/TransformExecutionLogsPanel", () => ({ TransformExecutionLogsPanel: () => null }))
vi.mock("@/components/chat/DiagnosisCard", () => ({ DiagnosisCard: () => null }))

import ExecutionDetailPage from "@/app/(dashboard)/executions/[id]/page"

function run(nodeResults: unknown[], over: Record<string, unknown> = {}) {
  return {
    id: "exec-1",
    pipeline_id: "p-1",
    pipeline_name: "orders_to_warehouse",
    status: "success",
    start_time: "2026-10-07T08:00:00Z",
    end_time: "2026-10-07T08:02:30Z",
    node_results: nodeResults,
    ...over,
  }
}

// exec-1 took the 2 min 30 s its start and end say; exec-0 took 2 min 43 s.
const TRENDS = {
  recent_executions: [
    { execution_id: "exec-1", status: "completed", start_time: "2026-10-07T08:00:00Z", duration_ms: 150_000 },
    { execution_id: "exec-0", status: "completed", start_time: "2026-10-07T07:00:00Z", duration_ms: 163_000 },
  ],
}

function serve(body: unknown, { logs = [] as unknown[], trends = TRENDS as unknown } = {}) {
  const fetch = vi.fn(async (url: string) => {
    if (url.includes("/transforms")) return { ok: true, status: 200, json: async () => ({ logs }) }
    if (url.includes("/trends")) return { ok: true, status: 200, json: async () => trends }
    return { ok: true, status: 200, json: async () => body }
  })
  vi.stubGlobal("fetch", fetch)
  return fetch
}

async function renderPage() {
  render(await ExecutionDetailPage({ params: Promise.resolve({ id: "exec-1" }) }))
}

const card = (name: string) => screen.getByRole("region", { name })

beforeEach(() => {
  vi.restoreAllMocks()
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe("Run detail overview row", () => {
  it("is four cards that fill their row, with no node-count card", async () => {
    serve(run([{ node_id: "n1", status: "success" }, { node_id: "n2", status: "success" }]))
    await renderPage()
    const row = card("Duration").parentElement as HTMLElement
    expect(row).toHaveClass("grid", "@xl:grid-cols-2", "@3xl:grid-cols-4")
    expect(row.className).not.toMatch(/grid-cols-5/)
    expect(row.children).toHaveLength(4)
    expect([...row.children].map((c) => c.getAttribute("aria-label"))).toEqual([
      "Started",
      "Duration",
      "Rows written",
      "Data freshness",
    ])
    expect(screen.queryByText("Nodes Executed")).not.toBeInTheDocument()
  })

  it("counts the nodes in the timeline header", async () => {
    serve(run([{ node_id: "n1", status: "success" }, { node_id: "n2", status: "success" }]))
    await renderPage()
    expect(screen.getByRole("heading", { name: /Execution timeline/i })).toHaveTextContent("2 nodes")
  })

  it("says one node, not one nodes", async () => {
    serve(run([{ node_id: "n1", status: "success" }]))
    await renderPage()
    expect(screen.getByRole("heading", { name: /Execution timeline/i })).toHaveTextContent(/1 node$/)
  })

  it("keeps four cards for a run with no node results", async () => {
    serve(run([]))
    await renderPage()
    expect((card("Duration").parentElement as HTMLElement).children).toHaveLength(4)
    expect(screen.queryByRole("heading", { name: /Execution timeline/i })).not.toBeInTheDocument()
  })
})

describe("Run detail header", () => {
  it("is titled by the pipeline, with the run's status and a short run id", async () => {
    serve(run([]))
    await renderPage()
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("orders_to_warehouse")
    expect(screen.queryByText("Execution Details")).not.toBeInTheDocument()
    expect(screen.getByText("Succeeded")).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /copy run id exec-1/i })).toHaveTextContent("exec-1")
  })
})

describe("Run detail cards", () => {
  it("says how the run's duration changed against the previous run", async () => {
    const fetch = serve(run([]))
    await renderPage()
    expect(fetch).toHaveBeenCalledWith(expect.stringContaining("/api/v1/pipelines/p-1/trends?limit=10"), expect.anything())
    expect(within(card("Duration")).getByText("8% faster than the previous run")).toBeInTheDocument()
    expect(screen.getByTestId("recent-runs")).toHaveTextContent("2")
  })

  it("draws no change and no chart when the runs could not be read", async () => {
    serve(run([]), { trends: null })
    await renderPage()
    expect(within(card("Duration")).queryByText(/previous run/)).not.toBeInTheDocument()
    expect(screen.queryByTestId("recent-runs")).not.toBeInTheDocument()
  })

  it("shows the rows written, linking to the table statistics", async () => {
    serve(run([], { metrics: { records_processed: 12480 } }))
    await renderPage()
    expect(within(card("Rows written")).getByText("12,480")).toBeInTheDocument()
    expect(within(card("Rows written")).getByRole("link", { name: "Rows written" })).toHaveAttribute(
      "href",
      "/pipelines/p-1?tab=table-stats",
    )
  })

  it("control: a run with no count says so rather than claiming it could not be read", async () => {
    serve(run([]))
    await renderPage()
    expect(within(card("Rows written")).getByText("No rows recorded for this run")).toBeInTheDocument()
  })
})

describe("Run detail error and logs", () => {
  it("hides an old error message on a run that succeeded", async () => {
    serve(run([], { error_message: "connection reset" }))
    await renderPage()
    expect(screen.queryByText("connection reset")).not.toBeInTheDocument()
  })

  it("control: shows the error on a run that failed", async () => {
    serve(run([], { status: "failed", error_message: "connection reset" }))
    await renderPage()
    expect(screen.getByRole("alert")).toHaveTextContent("This run failed")
    expect(screen.getByRole("alert")).toHaveTextContent("connection reset")
  })

  it("shows the transform log card only when there are logs", async () => {
    serve(run([]))
    await renderPage()
    expect(screen.queryByRole("heading", { name: /Transform execution logs/i })).not.toBeInTheDocument()
  })

  it("control: a run with transform logs has the card", async () => {
    serve(run([]), { logs: [{ id: "l1", created_at: "2026-10-07T08:01:00Z" }] })
    await renderPage()
    expect(screen.getByRole("heading", { name: /Transform execution logs/i })).toBeInTheDocument()
  })
})
