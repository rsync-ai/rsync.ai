/**
 * The pipeline chat keeps the pipeline it started when the user leaves and comes back.
 *
 * The chat saved its messages and the user's request, not the pipeline: back on
 * /chat, the transcript came back but the pipeline's card, its HITL prompts and its
 * state did not, and the only way to it was the pipelines list.
 *
 * Pinned here:
 *  - after leaving and coming back, the chat reads the pipeline's state again and
 *    links to its page;
 *  - a save that does not name the pipeline keeps it (the chat saves its messages
 *    on every change, and once as it opens, before it has restored anything);
 *  - control: Start new forgets it, so the next visit opens on the empty chat.
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"
import type { Mock } from "vitest"

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/lib/api/chat", () => ({
  sendChatMessage: vi.fn(),
  resetSessionId: vi.fn(() => "session-reset"),
}))
vi.mock("@/lib/api/pipelines", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api/pipelines")>()
  return { ...actual, getPipeline: vi.fn(async () => ({})) }
})
vi.mock("@/contexts/WebSocketContext", () => ({
  useWebSocket: () => ({ subscribe: () => () => {} }),
}))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/chat",
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock("@/components/chat/AgenticPipelineHome", () => ({
  AgenticPipelineHome: ({ onSubmit }: { onSubmit: (p: string) => void }) => (
    <button type="button" onClick={() => onSubmit("Move my orders table to cloud storage")}>
      Start from home
    </button>
  ),
}))
vi.mock("@/components/chat/ChatMessageItem", () => ({ ChatMessageItem: () => null }))
vi.mock("@/components/chat/PipelineAccordionView", () => ({ PipelineAccordionView: () => null }))
vi.mock("@/components/chat/ActivePipelinesList", () => ({ ActivePipelinesList: () => null }))
vi.mock("@/components/chat/SuggestionsReviewDialog", () => ({ SuggestionsReviewDialog: () => null }))
vi.mock("@/components/pipeline/PipelineConnectionSelector", () => ({ PipelineConnectionSelector: () => null }))
vi.mock("@/components/pipeline/PipelineTableSelector", () => ({ PipelineTableSelector: () => null }))

import { AgenticChatInterfaceV2 } from "@/components/chat/AgenticChatInterfaceV2"
import { authFetch } from "@/lib/api/auth-fetch"
import { sendChatMessage } from "@/lib/api/chat"

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

const PIPELINE_ID = "5d1e2f3a-0000-4000-8000-000000001364"

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })
}

function stateReads() {
  return (authFetch as Mock).mock.calls.filter(([u]) => String(u).endsWith(`/pipelines/${PIPELINE_ID}/state`)).length
}

async function tick(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

async function startPipeline() {
  ;(sendChatMessage as Mock).mockResolvedValue({
    type: "pipeline_started",
    message: "Setting up your pipeline.",
    data: { pipeline_id: PIPELINE_ID, execution_id: "exec-1" },
    metadata: {},
  })
  const view = render(<AgenticChatInterfaceV2 />)
  fireEvent.click(screen.getByRole("button", { name: "Start from home" }))
  await tick(0)
  await tick(100)
  return view
}

beforeEach(() => {
  vi.clearAllMocks()
  try {
    window.localStorage.clear()
  } catch {
    // storage unavailable
  }
  vi.useFakeTimers()
  ;(authFetch as Mock).mockImplementation(async (url: string) => {
    if (String(url).endsWith(`/pipelines/${PIPELINE_ID}/state`)) {
      return json(200, { pipeline_id: PIPELINE_ID, execution_id: "exec-1", status: "processing", current_stage: "executor" })
    }
    return json(404, { error: "not found" })
  })
})
afterEach(() => {
  vi.useRealTimers()
})

describe("pipeline chat — coming back to it", () => {
  it("shows the pipeline it started, and links to its page", async () => {
    const first = await startPipeline()
    expect(stateReads()).toBeGreaterThan(0)
    first.unmount()
    ;(authFetch as Mock).mockClear()

    render(<AgenticChatInterfaceV2 />)
    await tick(0)
    await tick(100)

    expect(stateReads()).toBeGreaterThan(0)
    expect(screen.queryByRole("button", { name: "Start from home" })).toBeNull()
    expect(screen.getByRole("link", { name: /open pipeline/i }).getAttribute("href")).toBe(
      `/pipelines/${PIPELINE_ID}?from=chat`,
    )
  })

  it("control: Start new forgets it", async () => {
    const first = await startPipeline()
    fireEvent.click(screen.getByRole("button", { name: "Start new" }))
    await tick(0)
    first.unmount()
    ;(authFetch as Mock).mockClear()

    render(<AgenticChatInterfaceV2 />)
    await tick(0)
    await tick(100)

    expect(stateReads()).toBe(0)
    expect(screen.getByRole("button", { name: "Start from home" })).toBeTruthy()
  })
})
