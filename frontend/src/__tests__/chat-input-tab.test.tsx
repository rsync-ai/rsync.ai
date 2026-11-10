import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { fireEvent, render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { useState } from "react"
import type { Mock } from "vitest"

import { AgenticChatInterfaceV2 } from "@/components/chat/AgenticChatInterfaceV2"
import { ChatInputBar, replySuggestion } from "@/components/chat/ChatInputBar"
import type { ChatMessage } from "@/components/chat/ChatMessageItem"
import { authFetch } from "@/lib/api/auth-fetch"
import { sendChatMessage } from "@/lib/api/chat"

// The chat page itself (last describe): only the network, the socket and the
// panels that play no part are stubbed.
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("@/lib/api/chat", () => ({
  sendChatMessage: vi.fn(),
  resetSessionId: vi.fn(() => "session-reset"),
}))
vi.mock("@/contexts/WebSocketContext", () => ({
  useWebSocket: () => ({ subscribe: () => () => {} }),
}))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/chat",
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock("@/components/chat/AgenticPipelineHome", async () => {
  const { useEffect } = await import("react")
  const example = "Stream Postgres changes to GCS"
  return {
    AgenticPipelineHome: ({
      onSubmit,
      onPrefill,
      onCustom,
      onSuggestion,
    }: {
      onSubmit: (p: string) => void
      onPrefill: (text: string) => void
      onCustom: () => void
      onSuggestion?: (s: string | undefined) => void
    }) => {
      useEffect(() => onSuggestion?.(example), [onSuggestion])
      return (
        <>
          <button type="button" onClick={() => onSubmit("Move my orders table to cloud storage")}>
            Start from home
          </button>
          <button type="button" onClick={() => onPrefill(example)}>
            {example}
          </button>
          <button type="button" onClick={onCustom}>
            Custom pipeline
          </button>
        </>
      )
    },
  }
})
const HOME_EXAMPLE = "Stream Postgres changes to GCS"
vi.mock("@/components/chat/ChatMessageItem", () => ({
  ChatMessageItem: ({ message }: { message: { role: string; content: string } }) => (
    <div data-testid={`msg-${message.role}`}>{message.content}</div>
  ),
}))
vi.mock("@/components/chat/PipelineAccordionView", () => ({ PipelineAccordionView: () => null }))
vi.mock("@/components/chat/ActivePipelinesList", () => ({ ActivePipelinesList: () => null }))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

// The pipeline chat's box: Tab writes in the reply the last answer's first chip
// offers, the same text a click on that chip writes. Only chips that write text
// count: a confirmation shows a card instead of chips, and a connector_missing
// "generate X" chip opens the generator rather than writing anything.

const assistant = (over: Partial<ChatMessage> = {}): ChatMessage => ({
  id: "a1",
  role: "assistant",
  content: "Which source?",
  suggestions: ["Use PostgreSQL", "Use MySQL"],
  ...over,
})

describe("replySuggestion", () => {
  it("is the last answer's first chip", () => {
    expect(replySuggestion([assistant()])).toBe("Use PostgreSQL")
  })

  it("is nothing before any answer, after the user's own message, or for an answer without chips", () => {
    expect(replySuggestion([])).toBeUndefined()
    expect(replySuggestion([assistant(), { id: "u1", role: "user", content: "postgres" }])).toBeUndefined()
    expect(replySuggestion([assistant({ suggestions: undefined })])).toBeUndefined()
    expect(replySuggestion([assistant({ suggestions: [] })])).toBeUndefined()
  })

  it("is nothing for a confirmation, whose card has the buttons", () => {
    expect(replySuggestion([assistant({ responseType: "confirmation", suggestions: ["Yes", "No"] })])).toBeUndefined()
  })

  it("skips a connector_missing 'generate X' chip, which opens the generator", () => {
    const m = assistant({ responseType: "connector_missing", suggestions: ["Generate Zendesk", "Use a CSV upload"] })
    expect(replySuggestion([m])).toBe("Use a CSV upload")
    expect(replySuggestion([{ ...m, suggestions: ["generate zendesk"] }])).toBeUndefined()
    // Control: the same chip on any other answer writes text, so it counts.
    expect(replySuggestion([assistant({ suggestions: ["Generate Zendesk"] })])).toBe("Generate Zendesk")
  })

  it("skips blank chips and looks only at the six chips the answer shows", () => {
    expect(replySuggestion([assistant({ suggestions: ["  ", "Use MySQL"] })])).toBe("Use MySQL")
    const seven = ["", "", "", "", "", "", "Use MySQL"]
    expect(replySuggestion([assistant({ suggestions: seven })])).toBeUndefined()
  })
})

function Bar({ suggestion, sending = false }: { suggestion?: string; sending?: boolean }) {
  const [input, setInput] = useState("")
  return (
    <ChatInputBar
      input={input}
      onInputChange={setInput}
      isSendingMessage={sending}
      pipelineId={undefined}
      isPipelineDone={false}
      executionFailed={false}
      onStartNew={vi.fn()}
      onViewPipeline={vi.fn()}
      onRetry={vi.fn()}
      suggestion={suggestion}
    />
  )
}

const box = () => screen.getByRole("textbox", { name: "Message" })
/** True when the box took the key. */
const tab = () => !fireEvent.keyDown(box(), { key: "Tab" })

describe("ChatInputBar Tab", () => {
  it("shows the suggestion as the placeholder and Tab writes it in", () => {
    render(<Bar suggestion="Use PostgreSQL" />)
    expect(box()).toHaveAttribute("placeholder", "Use PostgreSQL")
    expect(screen.getByText("to use the suggestion")).toBeTruthy()

    expect(tab()).toBe(true)
    expect(box()).toHaveValue("Use PostgreSQL")
    expect(screen.queryByText("to use the suggestion")).toBeNull()
    expect(tab()).toBe(false)
  })

  it("with no suggestion keeps the old placeholder and plain Tab", () => {
    render(<Bar />)
    expect(box()).toHaveAttribute("placeholder", "Describe what data you want to move…")
    expect(screen.queryByText("to use the suggestion")).toBeNull()
    expect(tab()).toBe(false)
    expect(box()).toHaveValue("")
  })

  it("hides the hint while a message is sending", () => {
    render(<Bar suggestion="Use PostgreSQL" sending />)
    expect(box()).toBeDisabled()
    expect(screen.queryByText("to use the suggestion")).toBeNull()
  })
})

describe("the chat page hands the box the last answer's first chip", () => {
  it("shows it once the answer lands, and Tab writes it", async () => {
    ;(authFetch as Mock).mockResolvedValue(new Response("{}", { status: 404 }))
    ;(sendChatMessage as Mock).mockResolvedValue({
      type: "clarification",
      message: "Which source should I read from?",
      suggestions: ["Use PostgreSQL", "Use MySQL"],
      data: {},
      metadata: {},
    })
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    render(<AgenticChatInterfaceV2 />)
    await user.click(screen.getByRole("button", { name: "Start from home" }))

    await screen.findByText("Which source should I read from?")
    expect(box()).toHaveAttribute("placeholder", "Use PostgreSQL")
    expect(tab()).toBe(true)
    expect(box()).toHaveValue("Use PostgreSQL")
  })
})

describe("the empty chat has one box, and the home's examples go into it", () => {
  // The chat keeps its transcript across visits: start each test on an empty one.
  beforeEach(() => {
    vi.clearAllMocks()
    window.localStorage.clear()
  })

  it("shows the home's first example, and Tab writes it", async () => {
    ;(authFetch as Mock).mockResolvedValue(new Response("{}", { status: 404 }))
    render(<AgenticChatInterfaceV2 />)

    expect(screen.getAllByRole("textbox")).toHaveLength(1)
    expect(box()).toHaveAttribute("placeholder", HOME_EXAMPLE)
    expect(tab()).toBe(true)
    expect(box()).toHaveValue(HOME_EXAMPLE)
    expect(sendChatMessage).not.toHaveBeenCalled()
  })

  it("an example chip writes into the box and focuses it; Custom pipeline focuses it", async () => {
    ;(authFetch as Mock).mockResolvedValue(new Response("{}", { status: 404 }))
    const user = userEvent.setup({ pointerEventsCheck: 0 })
    render(<AgenticChatInterfaceV2 />)

    await user.click(screen.getByRole("button", { name: "Custom pipeline" }))
    expect(box()).toHaveFocus()
    expect(box()).toHaveValue("")

    box().blur()
    await user.click(screen.getByRole("button", { name: HOME_EXAMPLE }))
    expect(box()).toHaveValue(HOME_EXAMPLE)
    expect(box()).toHaveFocus()
    expect(sendChatMessage).not.toHaveBeenCalled()
  })
})
