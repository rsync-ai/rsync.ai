/**
 * The resume that ends the transform step must not carry the transform list.
 *
 * The batch executor reads `metadata.transforms` from the task before it reads the
 * pipeline's stored rules (executor.go applyTransformsToData), and the resume's
 * metadata becomes the task's. So a list sent here was the one the initial load
 * ran, whatever the Transform Builder saved afterwards — two copies of the rules,
 * one of them frozen. The step saves to transform_definitions; the resume says
 * only how many rules it added.
 */
import { describe, it, expect, vi, beforeEach } from "vitest"
import { act, renderHook } from "@testing-library/react"
import type { Mock } from "vitest"

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(async () => new Response("{}", { status: 200 })),
}))
vi.mock("@/lib/api/pipelines", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/pipelines")>()
  return { ...actual, resumePipelineNodeInput: vi.fn(async () => ({})), resumePipelineTables: vi.fn(async () => ({})) }
})

import { useHITLState } from "@/lib/hooks/useHITLState"
import { resumePipelineNodeInput, resumePipelineTables } from "@/lib/api/pipelines"
import type { PipelineUIState } from "@/lib/pipeline/pipelineStateReducer"

const PIPELINE = "3c9f1d2e-0000-4000-8000-000000000042"

function parkedOnTables(): PipelineUIState {
  return {
    hitl: { blockingReason: { details: { source_connection_id: "conn-1" } } },
    metadata: { executionId: "exec-1" },
  } as unknown as PipelineUIState
}

function mount() {
  return renderHook(() =>
    useHITLState({
      pipelineId: PIPELINE,
      pipelineState: parkedOnTables(),
      authoritativePipelineMetadata: null,
      optimisticAction: {} as never,
      resolveHITL: vi.fn(),
      flashResumingState: vi.fn(),
    })
  )
}

function metadataOf(mock: Mock, viaNode: boolean): Record<string, unknown> {
  const body = mock.mock.calls[0][1] as {
    metadata?: Record<string, unknown>
    config_patch?: { metadata?: Record<string, unknown> }
  }
  return (viaNode ? body.config_patch?.metadata : body.metadata) ?? {}
}

beforeEach(() => {
  ;(resumePipelineNodeInput as Mock).mockClear()
  ;(resumePipelineTables as Mock).mockClear()
})

describe.each([
  { path: "the table resume", viaNode: false },
  { path: "the node-input resume", viaNode: true },
])("$path after the transform step", ({ viaNode }) => {
  const resume = () => (viaNode ? resumePipelineNodeInput : resumePipelineTables) as Mock

  async function pickTables(hook: ReturnType<typeof mount>) {
    await act(async () => {
      await hook.result.current.handleTablesSelected(["public.users"])
    })
    if (viaNode) act(() => hook.result.current.setTableHitlNodeId("node-tables"))
  }

  it("Save & continue sends the count, not the rules", async () => {
    const hook = mount()
    await pickTables(hook)
    await act(async () => {
      await hook.result.current.handleApplySuggestions({ count: 2 })
    })

    expect(resume()).toHaveBeenCalledTimes(1)
    const metadata = metadataOf(resume(), viaNode)
    expect(metadata).not.toHaveProperty("transforms")
    expect(metadata.transforms_saved).toBe(2)
  })

  it("Skip sends no transform list either, so the stored rules still apply", async () => {
    const hook = mount()
    await pickTables(hook)
    await act(async () => {
      await hook.result.current.handleSkipSuggestions()
    })

    expect(resume()).toHaveBeenCalledTimes(1)
    const metadata = metadataOf(resume(), viaNode)
    expect(metadata).not.toHaveProperty("transforms")
    expect(metadata.skipped_suggestions).toBe(true)
  })
})
