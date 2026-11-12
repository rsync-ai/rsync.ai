/**
 * Data flow and Table statistics draw nothing batch-shaped on a CDC pipeline
 * while the pipeline's mode is still loading.
 *
 * Bug class (#1572): a component draws mode-specific UI from a placeholder mode
 * before the authoritative mode loads. Steps/DAG flashed its batch-only
 * Graph/Timeline switch on a CDC pipeline; these are the two siblings the sweep
 * found:
 *
 *  - MonitorTab (Data flow) read the mode from /runtime only, so until that
 *    answered a CDC pipeline got the batch Throughput card, reading "No execution
 *    yet", and then swapped it for the Flow card.
 *  - PipelineMonitoringPanel (Table statistics) read the mode from the pipeline
 *    GET only, so until that answered a CDC pipeline got the batch copy, "Edit
 *    tables for future runs", and every pipeline got "(Available after the first
 *    successful run.)" — a verdict made before the answer it depends on.
 *
 * Both now take the mode the server page already resolved (`pipelineType`) for
 * their first paint; the fetched mode still wins once it arrives.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, cleanup, render, screen } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/pipelines/p1",
}))
vi.mock("@/components/pipeline/MonitoringOverviewTab", () => ({ MonitoringOverviewTab: () => null }))
vi.mock("@/components/pipeline/DiagnosePanel", () => ({ DiagnosePanel: () => null }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
  authFetchOrThrow: (...a: unknown[]) => authFetch(...a),
}))

import { MonitorTab } from "@/components/pipeline/MonitorTab"
import { PipelineMonitoringPanel } from "@/components/pipeline/PipelineMonitoringPanel"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

// One request held open until the test lets it answer.
function gate() {
  let open!: (r: Response) => void
  const promise = new Promise<Response>((r) => {
    open = r
  })
  return { promise, open }
}

const runtimeBody = (id: string, mode: "cdc" | "batch", executionId?: string) => ({
  pipeline_id: id,
  ...(executionId ? { execution_id: executionId } : {}),
  mode,
  phase: mode === "cdc" ? "streaming" : "idle",
  health: "healthy",
  dependencies: [],
  updated_at: "2026-10-23T10:00:00Z",
})

const statsBody = (mode: "cdc" | "batch") => ({
  summary: { mode, total_tables: 0, tables_completed: 0, tables_running: 0, tables_failed: 0, tables_degraded: 0 },
  tables: [],
  total: 0,
})

// The usePipelineRuntime poller is module-level and keyed by pipeline id, so
// every test takes its own id.
let seq = 0
const nextId = (tag: string) => `${tag}-${++seq}`

async function settle() {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve()
  })
}

beforeEach(() => {
  vi.clearAllMocks()
})
afterEach(() => {
  cleanup()
})

describe("Data flow before /runtime answers", () => {
  function route(id: string, runtime: Promise<Response>) {
    authFetch.mockImplementation(async (url: string) => {
      const u = String(url)
      if (u.includes(`/pipelines/${id}/runtime`)) return runtime
      if (u.includes("/table-stats")) return res(200, statsBody(u.includes("mode=cdc") ? "cdc" : "batch"))
      if (u.includes("/consumers")) return res(200, { consumers: [], measured: true })
      return res(200, {})
    })
  }

  it("a CDC pipeline never shows the batch Throughput card", async () => {
    const id = nextId("cdc")
    const runtime = gate()
    route(id, runtime.promise)
    render(<MonitorTab pipelineId={id} pipelineType="cdc" />)
    await settle()

    expect(screen.queryByText("Throughput")).toBeNull()
    expect(screen.queryByText(/No execution yet/)).toBeNull()
    expect(screen.getByTestId("flow-card")).toBeInTheDocument()

    runtime.open(res(200, runtimeBody(id, "cdc", id)))
    await settle()
    expect(screen.getByTestId("flow-card")).toBeInTheDocument()
    expect(screen.queryByText("Throughput")).toBeNull()
  })

  it("a batch pipeline says it is loading, not that it has never run", async () => {
    const id = nextId("batch")
    const runtime = gate()
    route(id, runtime.promise)
    render(<MonitorTab pipelineId={id} pipelineType="etl" />)
    await settle()

    expect(screen.getByText("Throughput")).toBeInTheDocument()
    expect(screen.getByText("Loading throughput…")).toBeInTheDocument()
    expect(screen.queryByText(/No execution yet/)).toBeNull()

    // Control: the gate opens. A batch pipeline that has never run still says so.
    runtime.open(res(200, runtimeBody(id, "batch")))
    expect(await screen.findByText(/No execution yet/)).toBeInTheDocument()
  })

  it("control: a failed /runtime still settles to the batch card's empty state", async () => {
    const id = nextId("fail")
    route(id, Promise.resolve(res(500, {})))
    render(<MonitorTab pipelineId={id} pipelineType="etl" />)
    expect(await screen.findByText(/No execution yet/)).toBeInTheDocument()
  })

  it("control: the mode /runtime reports wins over the page's", async () => {
    const id = nextId("wins")
    route(id, Promise.resolve(res(200, runtimeBody(id, "batch", "e1"))))
    render(<MonitorTab pipelineId={id} pipelineType="cdc" />)
    expect(await screen.findByText("Throughput")).toBeInTheDocument()
    expect(screen.queryByTestId("flow-card")).toBeNull()
  })
})

describe("Table statistics before the pipeline GET answers", () => {
  const BATCH_COPY = /Edit tables for future runs/
  const CDC_COPY = /Choose which tables this pipeline streams/
  const AFTER_FIRST_RUN = /Available after the first successful run/

  function route(id: string, pipeline: Promise<Response>) {
    authFetch.mockImplementation(async (url: string) => {
      const u = String(url)
      if (u.endsWith(`/pipelines/${id}`)) return pipeline
      if (u.endsWith("/state")) return res(200, { schema_version: 1, pipeline_id: id, status: "running" })
      if (u.includes("/runtime")) return res(200, runtimeBody(id, "cdc", id))
      if (u.includes("/table-stats")) return res(200, statsBody("cdc"))
      if (u.includes("/events")) return res(200, { events: [] })
      return res(200, {})
    })
  }

  it("a CDC pipeline gets the CDC copy and no first-run verdict", async () => {
    const id = nextId("ts-cdc")
    const pipeline = gate()
    route(id, pipeline.promise)
    render(<PipelineMonitoringPanel pipelineId={id} variant="table_stats" pipelineType="cdc" />)
    await settle()

    expect(screen.queryByText(BATCH_COPY)).toBeNull()
    expect(screen.queryByText(AFTER_FIRST_RUN)).toBeNull()
    expect(screen.getByText(CDC_COPY)).toBeInTheDocument()

    pipeline.open(res(200, { id, sync_mode: "cdc", source_connection_id: "src-1" }))
    await settle()
    expect(screen.getByText(CDC_COPY)).toBeInTheDocument()
    expect(screen.queryByText(AFTER_FIRST_RUN)).toBeNull()
  })

  it("a batch pipeline keeps its copy, and gets the verdict only once the answer is in", async () => {
    const id = nextId("ts-batch")
    const pipeline = gate()
    route(id, pipeline.promise)
    render(<PipelineMonitoringPanel pipelineId={id} variant="table_stats" pipelineType="etl" />)
    await settle()

    expect(screen.getByText(BATCH_COPY)).toBeInTheDocument()
    expect(screen.queryByText(AFTER_FIRST_RUN)).toBeNull()

    // Control: the gate opens. No source connection on the record → the verdict.
    pipeline.open(res(200, { id, sync_mode: "batch" }))
    expect(await screen.findByText(AFTER_FIRST_RUN)).toBeInTheDocument()
  })

  it("control: a failed pipeline GET still settles the verdict", async () => {
    const id = nextId("ts-fail")
    route(id, Promise.resolve(res(500, {})))
    render(<PipelineMonitoringPanel pipelineId={id} variant="table_stats" pipelineType="etl" />)
    expect(await screen.findByText(AFTER_FIRST_RUN)).toBeInTheDocument()
  })
})
