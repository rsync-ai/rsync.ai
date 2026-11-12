/**
 * The chat card's "Action required" box offers a real action for every parked
 * run, or says plainly that this build cannot answer it.
 *
 * It knew three requests. A user's own pause (paused_by_user) and a schema
 * approval (approval) carry no input_schema, so the box rendered with nothing in
 * it. A type it did not know that did carry a schema got a form whose Submit
 * handed the answer to onSelectTables, so it opened the table picker for a
 * request about something else. The classifier is lib/pipeline/chatHitlAction.ts;
 * its census holds every type the adapter writes to a decided action.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))

vi.mock("@/lib/hooks/usePipelineRuntime", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/hooks/usePipelineRuntime")>()),
  usePipelineRuntime: () => ({ runtime: null, loading: false, error: null }),
}))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
  authFetchOrThrow: (...args: unknown[]) => authFetch(...args),
}))

import { PipelineAccordionView, type PipelineState } from "@/components/chat/PipelineAccordionView"
import { UNSUPPORTED_HITL_TITLE } from "@/components/chat/GenericHitlForm"

function parkedOn(pipelineId: string, type: string, details?: Record<string, unknown>): PipelineState {
  return {
    pipeline_id: pipelineId,
    status: "waiting_for_user",
    current_stage: "executor",
    execution_plan: { stages: [{ id: "executor", display_name: "Executor", status: "waiting" }] },
    progress: { percent: 40 },
    blocking_reason: { type, description: "The run is waiting", ...(details ? { details } : {}) },
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  }
}

function handlers() {
  return { onSelectTables: vi.fn(), onConfigureConnections: vi.fn(), onGenerateConnectors: vi.fn() }
}

beforeEach(() => {
  authFetch.mockReset()
  authFetch.mockImplementation(async () => ({ ok: true, status: 200, json: async () => ({}), text: async () => "{}" }))
})
afterEach(() => cleanup())

describe("the chat card's Action required box", () => {
  it("a schema approval says it cannot be answered here, instead of an empty box", () => {
    render(<PipelineAccordionView state={parkedOn("p-approval", "approval")} {...handlers()} />)

    expect(screen.getByText("Action required")).toBeInTheDocument()
    expect(screen.getByText(UNSUPPORTED_HITL_TITLE)).toBeInTheDocument()
    expect(screen.getByText("approval")).toBeInTheDocument()
  })

  it("an unknown request with a schema is shown read-only and opens no picker", () => {
    const h = handlers()
    const { container } = render(
      <PipelineAccordionView
        state={parkedOn("p-unknown", "needs_export_sign_off", {
          input_schema: { type: "object", properties: { note: { type: "string", description: "Note" } } },
        })}
        {...h}
      />,
    )

    expect(screen.getByText(UNSUPPORTED_HITL_TITLE)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: /submit/i })).toBeDisabled()
    fireEvent.submit(container.querySelector("form")!)
    expect(h.onSelectTables).not.toHaveBeenCalled()
    expect(h.onConfigureConnections).not.toHaveBeenCalled()
    expect(h.onGenerateConnectors).not.toHaveBeenCalled()
  })

  it("a run the user paused offers Resume, which posts the resume request", async () => {
    render(<PipelineAccordionView state={parkedOn("p-paused", "paused_by_user")} {...handlers()} />)

    expect(screen.queryByText(UNSUPPORTED_HITL_TITLE)).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole("button", { name: "Resume" }))

    await waitFor(() =>
      expect(authFetch).toHaveBeenCalledWith(
        expect.stringMatching(/\/api\/v1\/pipelines\/p-paused\/resume$/),
        expect.objectContaining({ method: "POST" }),
      ),
    )
  })

  it("a failed resume says so in the card", async () => {
    authFetch.mockImplementation(async (url: unknown) =>
      String(url).endsWith("/resume")
        ? { ok: false, status: 400, json: async () => ({ error: "Pipeline is not paused" }), text: async () => "" }
        : { ok: true, status: 200, json: async () => ({}), text: async () => "{}" },
    )
    render(<PipelineAccordionView state={parkedOn("p-paused-2", "paused_by_user")} {...handlers()} />)

    fireEvent.click(screen.getByRole("button", { name: "Resume" }))
    expect(await screen.findByText(/Pipeline is not paused/)).toBeInTheDocument()
  })

  it("control: a table selection still opens the table picker", () => {
    const h = handlers()
    render(<PipelineAccordionView state={parkedOn("p-tables", "table_selection", { reason: "pick" })} {...h} />)

    fireEvent.click(screen.getByRole("button", { name: "Select Tables" }))
    expect(h.onSelectTables).toHaveBeenCalledTimes(1)
    expect(screen.queryByText(UNSUPPORTED_HITL_TITLE)).not.toBeInTheDocument()
  })

  it("control: the connection check's request still opens connection setup", () => {
    const h = handlers()
    render(
      <PipelineAccordionView
        state={parkedOn("p-conn", "user_input_required", { request_type: "connection_fix" })}
        {...h}
      />,
    )

    fireEvent.click(screen.getByRole("button", { name: "Configure Connections" }))
    expect(h.onConfigureConnections).toHaveBeenCalledTimes(1)
  })
})
