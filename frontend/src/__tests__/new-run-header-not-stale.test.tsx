/**
 * KI-BATCH-UI-STALE-AFTER-RUN (header half): after "Run pipeline" a batch page
 * kept "Running · step 7/7" for over a minute although the run finished in
 * 1.2 s (prod pipeline 9a094389, 2026-09-26); a hard reload showed Completed.
 *
 * Two client paths hold the header on something other than this run:
 *
 *  - the stage timeline reads the pipeline's last 200 events, every run's, and
 *    when none belonged to the run in state yet it fell back to all of them, so
 *    a run that had just started showed the previous run's finished stages as
 *    "step 7/7";
 *  - the shared /state read had no timeout and schedules the next read only
 *    when it answers, so one hung GET froze the status pill for good;
 *  - the run page's card for a blocked run (which connection failed, which
 *    tables to pick) took the newest blocking_reason from every run's events,
 *    so a new run showed an earlier run's reason (inbox 2026-10-05).
 *
 * Bug class: a live view that shows an answer about another run (or a read that
 * never answers) as the current one.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, cleanup, render, screen } from "@testing-library/react"
import "@testing-library/jest-dom"

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
}))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))
vi.mock("@/components/pipeline/DiagnosePanel", () => ({ DiagnosePanel: () => null }))

import {
  buildAgenticStagesFromEvents,
  deriveStepInfo,
  latestBlockingReason,
} from "@/components/pipeline/PipelineLiveStatePanel"
import { PipelineHealthHeader } from "@/components/pipeline/PipelineHealthHeader"
import { ACTIVE_POLL_MS, usePipelineStatePoll } from "@/lib/hooks/usePipelineStatePoll"

type Ev = Parameters<typeof buildAgenticStagesFromEvents>[0][number]
type State = Parameters<typeof buildAgenticStagesFromEvents>[1]

const BATCH_STAGES = [
  "intent",
  "capability_resolver",
  "connector_check",
  "connection_validation",
  "planner",
  "validator",
  "executor",
]

let seq = 0
function ev(execution: string | undefined, stage: string, type: string, second: number): Ev {
  seq += 1
  return {
    pipeline_id: "p1",
    execution_id: execution,
    event_id: `ev-${seq}`,
    event_type: type,
    stage_id: stage,
    occurred_at: `2026-09-26T15:03:${String(second).padStart(2, "0")}Z`,
  } as Ev
}

/** A batch run whose every stage finished. */
function finishedRun(execution: string | undefined): Ev[] {
  return BATCH_STAGES.flatMap((stage, i) => [
    ev(execution, stage, "STAGE_STARTED", i * 2),
    ev(execution, stage, "STAGE_COMPLETED", i * 2 + 1),
  ])
}

const running = (execution: string): State =>
  ({ pipeline_id: "p1", execution_id: execution, status: "running", created_at: "2026-09-26T15:04:00Z" }) as unknown as State

describe("the stage timeline shows the run in state, never an earlier one", () => {
  it("a run with no events yet does not show the previous run's stages as step 7/7", () => {
    const previous = finishedRun("run-1")

    const stages = buildAgenticStagesFromEvents(previous, running("run-2"))
    expect(stages).toEqual([])
    expect(deriveStepInfo(stages, undefined, "running")).toBeNull()
  })

  it("once the new run reports, only its own stages count", () => {
    const events = [...finishedRun("run-1"), ev("run-2", "intent", "STAGE_STARTED", 40)]

    const stages = buildAgenticStagesFromEvents(events, running("run-2"))
    expect(stages.find((s) => s.stage === "intent")?.status).toBe("running")
    expect(stages.filter((s) => s.status === "completed")).toEqual([])
  })

  // Control: a stream that carries no run ids at all is still read whole.
  it("events that name no run are still read", () => {
    const stages = buildAgenticStagesFromEvents(finishedRun(undefined), running("run-2"))
    expect(stages.filter((s) => s.status === "completed").map((s) => s.stage)).toEqual(
      expect.arrayContaining(BATCH_STAGES),
    )
  })
})

/** An event that says the run is waiting on the person, and why. */
function blocked(execution: string | undefined, connection: string, second: number): Ev {
  return {
    ...ev(execution, "connection_validation", "PIPELINE_WAITING_FOR_USER", second),
    payload: { blocking_reason: { source_connection_id: connection } },
  } as Ev
}

describe("the blocked-run card reads the run in state, never an earlier one", () => {
  it("a run that has not blocked shows no reason, not the previous run's", () => {
    const events = [...finishedRun("run-1"), blocked("run-1", "conn-old", 30), ev("run-2", "intent", "STAGE_STARTED", 40)]
    expect(latestBlockingReason(events, "run-2")).toBeNull()
  })

  it("a run that blocked shows its own reason, whatever an earlier run's timestamps", () => {
    // The earlier run's event carries the later clock: newest-wins alone picks it.
    const events = [blocked("run-1", "conn-old", 59), blocked("run-2", "conn-new", 45)]
    expect(latestBlockingReason(events, "run-2")?.source_connection_id).toBe("conn-new")
  })

  // Controls: the newest of the run's own reasons wins, and a stream that names
  // no run at all is still read whole.
  it("the newest of the run's own reasons wins", () => {
    const events = [blocked("run-2", "conn-a", 41), blocked("run-2", "conn-b", 48)]
    expect(latestBlockingReason(events, "run-2")?.source_connection_id).toBe("conn-b")
  })

  it("events that name no run are still read", () => {
    expect(latestBlockingReason([blocked(undefined, "conn-x", 30)], "run-2")?.source_connection_id).toBe("conn-x")
  })
})

const ok = (body: unknown) => ({ ok: true, status: 200, json: async () => body }) as unknown as Response

function Pill() {
  const { status } = usePipelineStatePoll("p1")
  return <span data-testid="status">{status ?? "none"}</span>
}

async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

describe("a hung /state read cannot freeze the status pill", () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" })
  })
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it("a read that never answers times out, and the next read shows the finished run", async () => {
    let call = 0
    // authFetch's own contract: `timeoutMs` aborts the request with a
    // TimeoutError; without it a hung request never settles.
    authFetch.mockImplementation((_url: string, init: { signal?: AbortSignal; timeoutMs?: number }) => {
      call += 1
      if (call === 1) return Promise.resolve(ok({ status: "running" }))
      if (call > 2) return Promise.resolve(ok({ status: "completed" }))
      return new Promise((_resolve, reject) => {
        if (init?.timeoutMs) {
          setTimeout(
            () => reject(new DOMException("The request timed out. Please try again.", "TimeoutError")),
            init.timeoutMs,
          )
        }
        init?.signal?.addEventListener("abort", () =>
          reject(Object.assign(new Error("aborted"), { name: "AbortError" })),
        )
      })
    })

    render(<Pill />)
    await tick(0)
    expect(screen.getByTestId("status")).toHaveTextContent("running")

    // The second read hangs. Well within a minute the pill must have moved on.
    await tick(ACTIVE_POLL_MS + 30_000)
    expect(screen.getByTestId("status")).toHaveTextContent("completed")
  })
})

// /runtime's phase comes from pipelines.status and /state's from
// pipeline_progress.status. The two can disagree after a batch run ends: the page
// said "Completed" in its status pill while the health header kept "Syncing 99%"
// from /runtime. Bug class: one page, two status sources, the header trusting the
// one that has not caught up.
describe("the health header agrees with a finished /state", () => {
  const runtimeBody = (over: Record<string, unknown> = {}) => ({
    pipeline_id: "p1",
    execution_id: "e1",
    mode: "batch",
    phase: "syncing",
    health: "healthy",
    message: "",
    progress: { percent: 99 },
    dependencies: [],
    updated_at: "2026-10-01T10:00:00Z",
    ...over,
  })
  let stateStatus = "completed"
  let runtimeOver: Record<string, unknown> = {}

  beforeEach(() => {
    vi.clearAllMocks()
    vi.useFakeTimers()
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" })
    stateStatus = "completed"
    runtimeOver = {}
    authFetch.mockImplementation(async (url: string) =>
      String(url).includes("/state") ? ok({ status: stateStatus }) : ok(runtimeBody(runtimeOver)),
    )
  })
  afterEach(() => {
    cleanup()
    vi.useRealTimers()
  })

  it("a completed /state reads Completed and 100%, not Syncing 99%", async () => {
    render(<PipelineHealthHeader pipelineId="p1" />)
    await tick(0)
    expect(screen.getByText("Completed")).toBeInTheDocument()
    expect(screen.getByText("100%")).toBeInTheDocument()
    expect(screen.queryByText("Syncing")).toBeNull()
    expect(screen.queryByText("99%")).toBeNull()
  })

  it("a failed /state reads Failed", async () => {
    stateStatus = "failed"
    render(<PipelineHealthHeader pipelineId="p1" />)
    await tick(0)
    expect(screen.getByText("Failed")).toBeInTheDocument()
    expect(screen.queryByText("Syncing")).toBeNull()
  })

  it("control: a running /state keeps the runtime phase and percent", async () => {
    stateStatus = "running"
    render(<PipelineHealthHeader pipelineId="p1" />)
    await tick(0)
    expect(screen.getByText("Syncing")).toBeInTheDocument()
    expect(screen.getByText("99%")).toBeInTheDocument()
  })

  it("control: a CDC stream keeps its runtime phase — /state is not its verdict", async () => {
    runtimeOver = { mode: "cdc", phase: "streaming", progress: undefined, message: "Streaming pipeline active" }
    render(<PipelineHealthHeader pipelineId="p1" />)
    await tick(0)
    expect(screen.getByText("Streaming")).toBeInTheDocument()
    expect(screen.queryByText("Completed")).toBeNull()
  })
})
