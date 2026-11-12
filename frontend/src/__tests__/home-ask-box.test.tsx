import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"

import { ASSISTANT_EXAMPLES, BUILDER_SUGGESTION, HomeAskBox } from "@/components/home/HomeAskBox"
import { featureFlagsManager } from "@/config/features"
import { NO_MODEL, sendQuestion } from "@/lib/assistant/ask"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// Home's one box is Ask rsync.ai only with the assistant switched on AND a model
// set up; otherwise it is the pipeline chat's first message. These pin which box
// shows, where each one sends the text, and that Enter, Shift+Enter, an input
// method's Enter, Tab and the example chips each do only their own job.

const { push } = vi.hoisted(() => ({ push: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, replace: vi.fn(), refresh: vi.fn() }),
}))
vi.mock("@/lib/assistant/ask", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/assistant/ask")>()),
  sendQuestion: vi.fn(async () => {}),
}))

const WS = "0df2f291-0000-0000-0000-000000000001"

function flags(f: { assistant: boolean; llmConfigured: boolean }) {
  featureFlagsManager.updateFlags(f)
  featureFlagsManager.markResolved()
}

const box = () => screen.getByRole("textbox") as HTMLTextAreaElement

beforeEach(() => {
  vi.clearAllMocks()
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS)
  useAssistantStore.setState({ open: false, page: null })
})

afterEach(() => {
  featureFlagsManager.resetFlags()
  window.localStorage.clear()
})

describe("HomeAskBox — which box", () => {
  it("shows neither box until the flags have answered, then the right one without a remount", () => {
    render(<HomeAskBox />)
    expect(screen.queryByRole("textbox")).toBeNull()
    expect(document.querySelector('[aria-busy="true"]')).not.toBeNull()

    act(() => flags({ assistant: true, llmConfigured: true }))
    expect(screen.getByRole("heading", { name: "Ask rsync.ai" })).toBeTruthy()
    expect(box().getAttribute("aria-label")).toBe("Ask rsync.ai")
    expect(box().placeholder).toBe(ASSISTANT_EXAMPLES[0].text)
  })

  it("is the pipeline chat when the assistant is on but no model is set up, and says what a model adds", () => {
    flags({ assistant: true, llmConfigured: false })
    render(<HomeAskBox />)
    expect(screen.getByRole("heading", { name: "Create a pipeline" })).toBeTruthy()
    expect(box().getAttribute("aria-label")).toBe("Describe the pipeline you want")
    expect(box().placeholder).toBe(BUILDER_SUGGESTION)
    expect(screen.getByText(`${NO_MODEL} With one, this box also builds models and workflows.`)).toBeTruthy()
    expect(screen.getByRole("link", { name: "Explorer" }).getAttribute("href")).toBe("/explorer")
    expect(screen.getByRole("link", { name: "Models" }).getAttribute("href")).toBe("/models")
    // The chips name things only the assistant builds.
    expect(screen.queryByRole("button", { name: "Workflow" })).toBeNull()
  })

  it("does not promise more of the box where the assistant is off", () => {
    flags({ assistant: false, llmConfigured: false })
    render(<HomeAskBox />)
    expect(screen.getByText(NO_MODEL)).toBeTruthy()
    expect(screen.queryByText(/also builds models/)).toBeNull()
  })

  it("is the pipeline chat, with no note, when a model is set up but the assistant is off", () => {
    flags({ assistant: false, llmConfigured: true })
    render(<HomeAskBox />)
    expect(screen.getByRole("heading", { name: "Create a pipeline" })).toBeTruthy()
    expect(screen.queryByText(NO_MODEL, { exact: false })).toBeNull()
  })
})

describe("HomeAskBox — sending", () => {
  it("asks the assistant in a new docked chat on Enter, and clears the box", () => {
    flags({ assistant: true, llmConfigured: true })
    render(<HomeAskBox />)
    fireEvent.change(box(), { target: { value: "  how many orders yesterday?  " } })
    fireEvent.keyDown(box(), { key: "Enter" })

    expect(useAssistantStore.getState()).toMatchObject({ open: true, page: null })
    expect(sendQuestion).toHaveBeenCalledWith({
      chatId: null,
      question: "how many orders yesterday?",
      workspaceId: WS,
      page: null,
    })
    expect(push).not.toHaveBeenCalled()
    expect(box().value).toBe("")
  })

  it("starts the pipeline chat with the text when there is no assistant", () => {
    flags({ assistant: true, llmConfigured: false })
    render(<HomeAskBox />)
    fireEvent.change(box(), { target: { value: "sync mysql & s3" } })
    fireEvent.click(screen.getByRole("button", { name: "Start pipeline chat" }))

    expect(push).toHaveBeenCalledWith("/chat?prompt=sync%20mysql%20%26%20s3&autosend=1")
    expect(sendQuestion).not.toHaveBeenCalled()
    expect(useAssistantStore.getState().open).toBe(false)
  })

  it("does not send on Shift+Enter, on an input method's Enter, or with only spaces", () => {
    flags({ assistant: true, llmConfigured: true })
    render(<HomeAskBox />)
    fireEvent.change(box(), { target: { value: "first line" } })
    fireEvent.keyDown(box(), { key: "Enter", shiftKey: true })
    fireEvent.keyDown(box(), { key: "Enter", isComposing: true })
    expect(sendQuestion).not.toHaveBeenCalled()

    fireEvent.change(box(), { target: { value: "   " } })
    fireEvent.keyDown(box(), { key: "Enter" })
    expect(sendQuestion).not.toHaveBeenCalled()
    expect((screen.getByRole("button", { name: "Ask" }) as HTMLButtonElement).disabled).toBe(true)
  })
})

describe("HomeAskBox — Tab and the example chips", () => {
  it("writes the suggestion on Tab into an empty box, without sending it", () => {
    flags({ assistant: true, llmConfigured: false })
    render(<HomeAskBox />)
    expect(screen.getByText("to use the suggestion")).toBeTruthy()

    const handled = !fireEvent.keyDown(box(), { key: "Tab" })
    expect(handled).toBe(true)
    expect(box().value).toBe(BUILDER_SUGGESTION)
    expect(push).not.toHaveBeenCalled()
    expect(screen.queryByText("to use the suggestion")).toBeNull()
  })

  it("fills the box from a chip without sending it", () => {
    flags({ assistant: true, llmConfigured: true })
    render(<HomeAskBox />)
    const workflow = ASSISTANT_EXAMPLES.find((e) => e.label === "Workflow")!
    fireEvent.click(screen.getByRole("button", { name: "Workflow" }))

    expect(box().value).toBe(workflow.text)
    expect(document.activeElement).toBe(box())
    expect(sendQuestion).not.toHaveBeenCalled()
  })
})
