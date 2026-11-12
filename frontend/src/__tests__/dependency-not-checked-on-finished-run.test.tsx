/**
 * A dependency nobody checked must not read "Checking…" on a run that is over.
 *
 * The orchestrator's prober skips a pipeline that is completed, failed,
 * cancelled, paused, stopped or archived (dependency_probe.go:102). A batch run
 * registers its dependencies when the sink starts; one that finished before the
 * first 15 s probe kept "Checking…" on every row, for good.
 */

import { describe, it, expect, vi, afterEach } from "vitest"
import { render, screen, cleanup, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"
import type { PipelineRuntime, RuntimeDep } from "@/lib/hooks/usePipelineRuntime"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))

let runtime: PipelineRuntime | null = null
vi.mock("@/lib/hooks/usePipelineRuntime", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/hooks/usePipelineRuntime")>()),
  usePipelineRuntime: () => ({ runtime, loading: false, error: null }),
}))

// /state answers with whatever the test sets; every other read is an empty 200.
let stateStatus = "running"
vi.mock("@/lib/api/auth-fetch", () => {
  const reply = async (url: unknown) => {
    const body = String(url).endsWith("/state") ? { status: stateStatus } : {}
    return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) }
  }
  return { authFetch: vi.fn(reply), authFetchOrThrow: vi.fn(reply) }
})

import { PipelineAccordionView, type PipelineState } from "@/components/chat/PipelineAccordionView"
import { MonitorTab } from "@/components/pipeline/MonitorTab"

const NEVER_CHECKED: RuntimeDep = { kind: "mcp_dest", identifier: "gcs@v1.0.0", status: "unknown" }

function runtimeFor(pipelineId: string, phase: PipelineRuntime["phase"]): PipelineRuntime {
  return {
    pipeline_id: pipelineId,
    execution_id: "e1",
    mode: "batch",
    phase,
    health: "unknown",
    dependencies: [NEVER_CHECKED],
    updated_at: new Date().toISOString(),
  }
}

function stateFor(pipelineId: string, status: string): PipelineState {
  return {
    pipeline_id: pipelineId,
    status,
    current_stage: "executor",
    execution_plan: { stages: [{ id: "executor", display_name: "Executor", status: "complete" }] },
    progress: { percent: 100 },
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  }
}

afterEach(() => {
  cleanup()
  runtime = null
  stateStatus = "running"
})

describe("Runtime dependencies panel (chat card)", () => {
  it("a completed run reads 'Not checked', not 'Checking…'", () => {
    runtime = runtimeFor("dep-chat-1", "completed")
    render(<PipelineAccordionView state={stateFor("dep-chat-1", "completed")} />)

    expect(screen.getByText("Not checked")).toBeInTheDocument()
    expect(screen.queryByText("Checking…")).not.toBeInTheDocument()
  })

  it("control: a running pipeline still reads 'Checking…'", () => {
    runtime = runtimeFor("dep-chat-2", "syncing")
    render(<PipelineAccordionView state={stateFor("dep-chat-2", "running")} />)

    expect(screen.getByText("Checking…")).toBeInTheDocument()
  })
})

describe("Dependencies card (Monitor tab)", () => {
  it("a completed run reads 'Not checked' once /state says so", async () => {
    runtime = runtimeFor("dep-monitor-1", "completed")
    stateStatus = "completed"
    render(<MonitorTab pipelineId="dep-monitor-1" />)

    await waitFor(() => expect(screen.getByText("Not checked")).toBeInTheDocument())
    expect(screen.queryByText("Checking…")).not.toBeInTheDocument()
  })

  it("control: a running pipeline still reads 'Checking…'", async () => {
    runtime = runtimeFor("dep-monitor-2", "syncing")
    stateStatus = "running"
    render(<MonitorTab pipelineId="dep-monitor-2" />)

    expect(await screen.findByText("Checking…")).toBeInTheDocument()
  })
})

// A stopped pipeline's row was checked once, before Stop, and the prober has
// skipped it since. The gateway withdraws its frozen "healthy" (pipeline_runtime.go
// markUnprobedDeps): the Debezium row read a green "healthy" for a STOPPED
// connector (10-22).
describe("a stopped pipeline's dependency checked before the stop", () => {
  const WITHDRAWN: RuntimeDep = {
    kind: "debezium_task",
    identifier: "rsync-cdc-orders",
    status: "unknown",
    last_checked_at: "2026-10-22T10:00:00Z",
    last_healthy_at: "2026-10-22T10:00:00Z",
    details: { not_checked_while: "stopped", last_result: "healthy" },
  }
  const stoppedRuntime = (pipelineId: string): PipelineRuntime => ({
    ...runtimeFor(pipelineId, "stopped"),
    mode: "cdc",
    health: "degraded",
    dependencies: [WITHDRAWN],
  })

  it("reads 'Not checked' on the chat card, not 'healthy' or 'unknown'", () => {
    runtime = stoppedRuntime("dep-chat-stopped")
    render(<PipelineAccordionView state={stateFor("dep-chat-stopped", "stopped")} />)

    expect(screen.getByText("Not checked")).toBeInTheDocument()
    expect(screen.queryByText("healthy")).not.toBeInTheDocument()
    expect(screen.queryByText("unknown")).not.toBeInTheDocument()
  })

  it("reads 'Not checked' on the Monitor tab", async () => {
    runtime = stoppedRuntime("dep-monitor-stopped")
    stateStatus = "stopped"
    render(<MonitorTab pipelineId="dep-monitor-stopped" />)

    await waitFor(() => expect(screen.getByText("Not checked")).toBeInTheDocument())
    expect(screen.queryByText("healthy")).not.toBeInTheDocument()
  })
})
