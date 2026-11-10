/**
 * The pipeline page's live-state panel offers Resume for a run the user paused.
 *
 * A pause parks the workflow on the "resume" signal with blocking_reason_type
 * paused_by_user, and the state endpoint keeps the run waiting_for_user. The
 * panel's classifier had no arm for it, so it fell to "generic" and told the user
 * this build could not answer the request, when POST /pipelines/:id/resume does.
 * The chat card had the same gap (chat-hitl-card-every-blocking-type.test.tsx);
 * both now render the one ResumePausedRun.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))

vi.mock("@/lib/hooks/usePipelineRuntime", () => ({
  usePipelineRuntime: () => ({ runtime: null, loading: false, error: null }),
}))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...args: unknown[]) => authFetch(...args),
  authFetchOrThrow: (...args: unknown[]) => authFetch(...args),
}))

import { PipelineLiveStatePanel } from "@/components/pipeline/PipelineLiveStatePanel"
import { UNSUPPORTED_HITL_TITLE } from "@/components/chat/GenericHitlForm"

function jsonOk(body: unknown) {
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) }
}

function parkedOn(type: string) {
  return {
    schema_version: 1,
    pipeline_id: "p1",
    execution_id: "e1",
    status: "waiting_for_user",
    message: "Waiting",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    current_stage: "executor",
    blocking_reason: { type, description: "Paused by user" },
  }
}

function mountWith(state: Record<string, unknown>) {
  authFetch.mockImplementation(async (url: string) => {
    const u = String(url)
    if (u.includes("/state")) return jsonOk(state)
    if (u.includes("/events")) return jsonOk({ events: [] })
    return jsonOk({})
  })
  return render(<PipelineLiveStatePanel pipelineId="p1" />)
}

beforeEach(() => authFetch.mockReset())
afterEach(() => cleanup())

describe("the live-state panel on a run the user paused", () => {
  it("offers Resume, which posts the resume request, instead of the not-supported notice", async () => {
    mountWith(parkedOn("paused_by_user"))

    fireEvent.click(await screen.findByRole("button", { name: "Resume" }))
    expect(screen.queryByText(UNSUPPORTED_HITL_TITLE)).not.toBeInTheDocument()
    await waitFor(() =>
      expect(authFetch).toHaveBeenCalledWith(
        expect.stringMatching(/\/api\/v1\/pipelines\/p1\/resume$/),
        expect.objectContaining({ method: "POST" }),
      ),
    )
  })

  it("control: a request this build cannot answer still says so", async () => {
    mountWith(parkedOn("needs_export_sign_off"))

    expect(await screen.findByText(UNSUPPORTED_HITL_TITLE)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Resume" })).not.toBeInTheDocument()
  })
})
