/**
 * A HITL dialog that outlived its pause closes; it does not offer a retry.
 *
 * Two tabs can show the same pause (the chat and the pipeline page). Pipeline
 * a3b07ece (2026-10-01): the step was answered on the pipeline page and the run
 * finished, while the chat's transform dialog stayed open. Its Apply then
 * signalled the finished run, and the gateway marked that run failed. The
 * gateway now asks the run first and answers 409 with a reason
 * (pipeline_hitl_gate.go); these tests pin what each dialog does with it.
 */
import { describe, it, expect, vi, beforeEach, type Mock } from "vitest"
import { act, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: vi.fn(),
}))
vi.mock("sonner", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }))

import { toast } from "sonner"
import { authFetch } from "@/lib/api/auth-fetch"
import {
  HITLAnswerRefusedError,
  hitlAnswerRefusalFrom,
  isStaleHITLAnswer,
  resumePipelineConnections,
  resumePipelineConnectors,
  resumePipelineNodeInput,
  resumePipelineTables,
  type HITLRefusalCode,
} from "@/lib/api/pipelines"
import { useHITLState } from "@/lib/hooks/useHITLState"
import { useOptimisticAction } from "@/lib/pipeline/usePipelineState"
import type { PipelineUIState } from "@/lib/pipeline/pipelineStateReducer"
import { PipelineTableSelector, type AvailableTable } from "@/components/pipeline/PipelineTableSelector"
import { PipelineConnectionSelector } from "@/components/pipeline/PipelineConnectionSelector"

const PIPELINE = "a3b07ece-80da-4574-b6e1-41437645d3fa"
const EXEC = "exec-1"

// pipeline_hitl_gate.go, as the gateway sends them.
const REFUSALS: Record<HITLRefusalCode, Record<string, unknown>> = {
  run_finished: {
    error: "run_finished",
    message: "This run has already finished, so it is no longer waiting for this answer.",
    execution_id: EXEC,
    how_ended: "completed",
  },
  pause_already_answered: {
    error: "pause_already_answered",
    message: "This step was already answered, in another tab or by someone else, and the run has moved on.",
    execution_id: EXEC,
    waiting_for: "",
  },
  pause_busy: {
    error: "pause_busy",
    message: "The run is still working on the last answer to this step. Try again in a moment.",
    execution_id: EXEC,
    retryable: true,
  },
  hitl_answer_in_flight: {
    error: "hitl_answer_in_flight",
    message: "This step is already being answered. Wait for it to finish.",
    execution_id: EXEC,
  },
  table_selection_in_flight: {
    error: "table_selection_in_flight",
    message: "A table selection is already being applied for this pipeline. Wait for it to complete before submitting a new one.",
    execution_id: EXEC,
    selected_tables: ["public.orders"],
  },
}

const respond = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })

function refusal(code: HITLRefusalCode): HITLAnswerRefusedError {
  const r = REFUSALS[code]
  return new HITLAnswerRefusedError(code, String(r.message), { executionId: EXEC, howEnded: r.how_ended as string | undefined })
}

beforeEach(() => {
  ;(authFetch as Mock).mockReset()
  ;(toast.info as Mock).mockClear()
})

describe("the resume calls report a refusal as HITLAnswerRefusedError", () => {
  const calls = {
    tables: () => resumePipelineTables(PIPELINE, { execution_id: EXEC, selected_tables: ["public.orders"] }),
    connections: () => resumePipelineConnections(PIPELINE, { execution_id: EXEC, source_connection_id: "c1" }),
    connectors: () => resumePipelineConnectors(PIPELINE, { execution_id: EXEC, connectors: ["pg"] }),
    "node input": () => resumePipelineNodeInput(PIPELINE, { execution_id: EXEC, node_id: "n1" }),
  }

  for (const [name, call] of Object.entries(calls)) {
    for (const code of Object.keys(REFUSALS) as HITLRefusalCode[]) {
      it(`${name}: ${code}`, async () => {
        ;(authFetch as Mock).mockImplementation(async () => respond(409, REFUSALS[code]))
        const err = await call().then(
          () => null,
          (e: unknown) => e
        )
        expect(err).toBeInstanceOf(HITLAnswerRefusedError)
        const refused = err as HITLAnswerRefusedError
        expect(refused.code).toBe(code)
        expect(refused.message).toBe(REFUSALS[code].message)
        expect(refused.executionId).toBe(EXEC)
        // Only a busy pause can still take this dialog's answer.
        expect(refused.retryable).toBe(code === "pause_busy")
        expect(isStaleHITLAnswer(refused)).toBe(code !== "pause_busy")
      })
    }
  }

  it("carries how the run ended", () => {
    expect(hitlAnswerRefusalFrom(409, JSON.stringify(REFUSALS.run_finished))?.howEnded).toBe("completed")
  })

  it("control: other errors stay plain errors", async () => {
    expect(hitlAnswerRefusalFrom(409, JSON.stringify({ error: "invalid_tables", message: "x" }))).toBeNull()
    // The codes mean a refusal only on a 409.
    expect(hitlAnswerRefusalFrom(500, JSON.stringify(REFUSALS.run_finished))).toBeNull()
    expect(hitlAnswerRefusalFrom(409, "not json")).toBeNull()
    expect(hitlAnswerRefusalFrom(409, "")).toBeNull()
    expect(isStaleHITLAnswer(new Error("pause_already_answered"))).toBe(false)

    ;(authFetch as Mock).mockImplementation(async () => respond(500, { error: "failed_to_signal_workflow", message: "Could not reach the run. Try again." }))
    const err = await resumePipelineTables(PIPELINE, { execution_id: EXEC, selected_tables: ["t"] }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(Error)
    expect(err).not.toBeInstanceOf(HITLAnswerRefusedError)
  })
})

// ── The chat: transform dialog and connector generation (useHITLState) ─────────

vi.mock("@/lib/api/pipelines", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/pipelines")>()
  return {
    ...actual,
    resumePipelineTables: vi.fn(actual.resumePipelineTables),
    resumePipelineConnectors: vi.fn(actual.resumePipelineConnectors),
  }
})

function parkedOnTables(): PipelineUIState {
  return {
    hitl: {
      stage: "table_selection",
      blockingReason: { type: "table_selection", description: "Pick tables", details: { source_connection_id: "conn-1" } },
    },
    metadata: { executionId: EXEC },
  } as unknown as PipelineUIState
}

function mountChatHITL(opts?: { dispatch?: Mock }) {
  const resolveHITL = vi.fn()
  const dispatch = opts?.dispatch ?? vi.fn()
  const hook = renderHook(() => {
    const optimisticAction = useOptimisticAction(dispatch)
    return useHITLState({
      pipelineId: PIPELINE,
      pipelineState: parkedOnTables(),
      authoritativePipelineMetadata: null,
      optimisticAction,
      resolveHITL,
      flashResumingState: vi.fn(),
    })
  })
  return { hook, resolveHITL, dispatch }
}

async function openTransformDialog(hook: ReturnType<typeof mountChatHITL>["hook"]) {
  ;(authFetch as Mock).mockImplementation(async () => respond(200, {}))
  await act(async () => {
    await hook.result.current.handleTablesSelected(["public.orders"])
  })
  expect(hook.result.current.suggestionsReviewOpen).toBe(true)
}

describe("the chat's transform dialog", () => {
  for (const action of ["Apply", "Skip"] as const) {
    const run = (hook: ReturnType<typeof mountChatHITL>["hook"]) =>
      action === "Apply"
        ? hook.result.current.handleApplySuggestions({ count: 2 })
        : hook.result.current.handleSkipSuggestions()

    for (const code of ["run_finished", "pause_already_answered", "hitl_answer_in_flight"] as const) {
      it(`${action} on a step it no longer owns (${code}) closes the dialog and says why`, async () => {
        const { hook, resolveHITL } = mountChatHITL()
        await openTransformDialog(hook)
        ;(resumePipelineTables as Mock).mockRejectedValueOnce(refusal(code))

        await act(async () => {
          await run(hook)
        })

        expect(resumePipelineTables).toHaveBeenCalled()
        expect(hook.result.current.suggestionsReviewOpen).toBe(false)
        expect(toast.info).toHaveBeenCalledWith(REFUSALS[code].message, expect.anything())
        // Nothing was resumed: the pause stays until /state stops reporting it,
        // so the chat's auto-open does not reopen the dialog in the meantime.
        expect(resolveHITL).not.toHaveBeenCalled()
        expect(hook.result.current.suppressHitlUntilRef.current).toBe(0)
      })
    }

    it(`${action} on a busy pause keeps the dialog open for a retry`, async () => {
      const { hook, resolveHITL } = mountChatHITL()
      await openTransformDialog(hook)
      ;(resumePipelineTables as Mock).mockRejectedValueOnce(refusal("pause_busy"))

      let thrown: unknown
      await act(async () => {
        thrown = await run(hook).catch((e: unknown) => e)
      })

      expect(thrown).toBeInstanceOf(HITLAnswerRefusedError)
      expect(hook.result.current.suggestionsReviewOpen).toBe(true)
      expect(toast.info).not.toHaveBeenCalled()
      expect(resolveHITL).not.toHaveBeenCalled()
    })
  }
})

describe("the chat's connector-generation resume", () => {
  it("a stale answer is not rolled back into a pause the auto-open would answer again", async () => {
    const { hook, dispatch } = mountChatHITL()
    ;(resumePipelineConnectors as Mock).mockRejectedValueOnce(refusal("pause_already_answered"))

    await act(async () => {
      await hook.result.current.handleConnectorGeneration()
    })

    const types = dispatch.mock.calls.map((c) => (c[0] as { type: string }).type)
    expect(types).toEqual(["HITL_RESOLVED"])
    expect(toast.info).toHaveBeenCalledTimes(1)
  })

  it("control: any other failure still rolls back to the pause", async () => {
    const { hook, dispatch } = mountChatHITL()
    ;(resumePipelineConnectors as Mock).mockRejectedValueOnce(new Error("Could not reach the run. Try again."))

    await act(async () => {
      await hook.result.current.handleConnectorGeneration()
    })

    const types = dispatch.mock.calls.map((c) => (c[0] as { type: string }).type)
    expect(types).toEqual(["HITL_RESOLVED", "HITL_REQUIRED"])
    expect(toast.info).not.toHaveBeenCalled()
  })
})

// ── The modals every page shares ───────────────────────────────────────────────

const tables: AvailableTable[] = [{ name: "orders", schema: "public", row_count: 10, columns: 3 }]

function renderTableSelector(onTablesSelected: (t: string[]) => Promise<void>) {
  const onClose = vi.fn()
  render(
    <PipelineTableSelector
      isOpen
      onClose={onClose}
      pipelineId={PIPELINE}
      availableTables={tables}
      suggestedTables={[]}
      title="Select Tables to Sync"
      initialSelectedTables={["public.orders"]}
      onTablesSelected={onTablesSelected}
    />
  )
  return onClose
}

describe("the table selector", () => {
  it("closes on a step it no longer owns, and says why", async () => {
    const onClose = renderTableSelector(async () => {
      throw refusal("run_finished")
    })

    fireEvent.click(screen.getByRole("button", { name: "Sync 1 table" }))

    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(toast.info).toHaveBeenCalledWith(REFUSALS.run_finished.message, expect.anything())
    expect(screen.queryByRole("alert")).not.toBeInTheDocument()
  })

  it("control: a busy pause stays open with the reason", async () => {
    const onClose = renderTableSelector(async () => {
      throw refusal("pause_busy")
    })

    fireEvent.click(screen.getByRole("button", { name: "Sync 1 table" }))

    const alert = await screen.findByRole("alert")
    expect(alert).toHaveTextContent("still working on the last answer")
    expect(onClose).not.toHaveBeenCalled()
    expect(toast.info).not.toHaveBeenCalled()
  })
})

describe("the connection selector", () => {
  function renderConnectionSelector() {
    ;(authFetch as Mock).mockImplementation(async (url: string, init?: { method?: string }) => {
      if (init?.method === "POST") return respond(409, REFUSALS.pause_already_answered)
      return respond(200, [{ id: "c1", name: "orders db", connector_type: "postgresql", type: "source", status: "active" }])
    })
    const onClose = vi.fn()
    const onSuccess = vi.fn()
    render(
      <PipelineConnectionSelector
        isOpen
        onClose={onClose}
        onSuccess={onSuccess}
        pipelineId={PIPELINE}
        executionId={EXEC}
        requirements={{ source: { connector_type: "postgresql" }, destination: { connector_type: "" }, missing: ["source"] }}
      />
    )
    return { onClose, onSuccess }
  }

  it("closes on a step answered elsewhere, without reporting success", async () => {
    const { onClose, onSuccess } = renderConnectionSelector()

    const start = await screen.findByRole("button", { name: /Start Pipeline/ })
    await waitFor(() => expect(start).toBeEnabled())
    fireEvent.click(start)

    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(onSuccess).not.toHaveBeenCalled()
    expect(toast.info).toHaveBeenCalledWith(REFUSALS.pause_already_answered.message, expect.anything())
  })
})
