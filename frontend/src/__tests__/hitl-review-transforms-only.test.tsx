/**
 * Tables named in the chat message skip the table picker, and the transform
 * step opened only from the picker, so it was never shown. The orchestrator now
 * pauses once with `review_transforms_only` and the named tables; the chat must
 * go straight to the transform step with them, and an ordinary table wait must
 * still open the picker.
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

import { transformsReviewTables, useHITLState } from "@/lib/hooks/useHITLState"
import { resumePipelineNodeInput, resumePipelineTables } from "@/lib/api/pipelines"
import type { PipelineUIState } from "@/lib/pipeline/pipelineStateReducer"

const PIPELINE = "3c9f1d2e-0000-4000-8000-000000000043"
const NAMED = ["public.users", "public.orders"]

function parked(details: Record<string, unknown>): PipelineUIState {
  return {
    hitl: { blockingReason: { type: "table_selection", details } },
    metadata: { executionId: "exec-1" },
  } as unknown as PipelineUIState
}

function mount(details: Record<string, unknown>) {
  return renderHook(() =>
    useHITLState({
      pipelineId: PIPELINE,
      pipelineState: parked(details),
      authoritativePipelineMetadata: null,
      optimisticAction: {} as never,
      resolveHITL: vi.fn(),
      flashResumingState: vi.fn(),
    })
  )
}

const offered = NAMED.map((name) => ({ name, schema: "", row_count: 0, columns: 0 }))

beforeEach(() => {
  ;(resumePipelineNodeInput as Mock).mockClear()
  ;(resumePipelineTables as Mock).mockClear()
})

describe("the transform step for tables named in chat", () => {
  it("opens the transform step with the named tables, not the picker", () => {
    const hook = mount({
      source_connection_id: "conn-1",
      available_tables: offered,
      review_transforms_only: true,
      preselected_tables: NAMED,
    })
    act(() => hook.result.current.handleOpenTableModal())

    expect(hook.result.current.suggestionsReviewOpen).toBe(true)
    expect(hook.result.current.selectedTablesForSuggestions).toEqual(NAMED)
    expect(hook.result.current.sourceConnectionIdForSuggestions).toBe("conn-1")
    expect(hook.result.current.tableSelectorOpen).toBe(false)
    // Nothing resumes until the user applies or skips the step.
    expect(resumePipelineTables).not.toHaveBeenCalled()
    expect(resumePipelineNodeInput).not.toHaveBeenCalled()
  })

  it("an ordinary table wait still opens the picker", () => {
    const hook = mount({
      source_connection_id: "conn-1",
      available_tables: offered,
      // Always written by the worker, so a flag from an earlier wait never survives.
      review_transforms_only: false,
      preselected_tables: [],
    })
    act(() => hook.result.current.handleOpenTableModal())

    expect(hook.result.current.tableSelectorOpen).toBe(true)
    expect(hook.result.current.suggestionsReviewOpen).toBe(false)
  })

  it("without the source connection it shows the picker holding the named tables, and never resumes on its own", () => {
    const hook = mount({ available_tables: offered, review_transforms_only: true, preselected_tables: NAMED })
    act(() => hook.result.current.handleOpenTableModal())

    expect(hook.result.current.tableSelectorOpen).toBe(true)
    expect(hook.result.current.availableTables.map((t) => t.name)).toEqual(NAMED)
    expect(hook.result.current.suggestionsReviewOpen).toBe(false)
    expect(resumePipelineTables).not.toHaveBeenCalled()
  })
})

describe("transformsReviewTables", () => {
  it.each([
    [{ review_transforms_only: true, preselected_tables: [" public.users ", "", 7] }, ["public.users"]],
    [{ review_transforms_only: true, preselected_tables: [] }, null],
    [{ review_transforms_only: "true", preselected_tables: NAMED }, null],
    [{ review_transforms_only: false, preselected_tables: NAMED }, null],
    [{ preselected_tables: NAMED }, null],
  ])("%j → %j", (details, want) => {
    expect(transformsReviewTables(details as Record<string, unknown>)).toEqual(want)
  })
})
