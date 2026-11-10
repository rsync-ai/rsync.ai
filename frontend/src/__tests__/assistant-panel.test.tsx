import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import { AssistantLauncher } from "@/components/assistant/AssistantLauncher"
import { DashboardShell } from "@/components/layout/DashboardShell"
import { PipelineCopilotDock } from "@/components/pipeline/PipelineCopilotDock"
import type { ExecutionPlanStage } from "@/components/pipeline/dagTypes"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { featureFlagsManager, fetchFeatureFlags } from "@/config/features"
import { SERVER_MEMORY_MS } from "@/lib/assistant/ask"
import { isUUID, pageFromPath, pageLabel } from "@/lib/assistant/page-context"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// The in-app assistant ("Ask rsync.ai"). api-gateway owns the loop, the tools,
// the limits and the key; what is pinned here is what only the browser can get
// wrong:
//   - the page a question is about is a {kind, id} read from a uuid route, or
//     the pipeline the dock was opened from, and the user can drop it;
//   - the conversation id from one answer goes with the next question;
//   - a chat outlives the panel closing, History reopens it, and an answer
//     lands in the chat it was asked in; Stop and Try again act on that chat;
//   - a chat is shown only in the workspace it was started in;
//   - the picker lists only what /assistant/models returns, and hides itself
//     for one model;
//   - an answer says what it looked at and which model answered;
//   - with no model set up, the panel says where an admin adds a key;
//   - nothing renders unless /api/v1/features says assistant.

const nav = vi.hoisted(() => ({ pathname: "/" }))

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => nav.pathname,
}))

const mockFetch = authFetch as unknown as Mock
const PIPELINE = "3f2b8c1e-5a6d-4e7f-9a0b-1c2d3e4f5a6b"

const claude = { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" }
const chatgpt = { entry: "chatgpt", label: "ChatGPT", model: "gpt-5" }

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

function answer(overrides: Record<string, unknown> = {}) {
  return {
    conversation_id: "conv-1",
    answer: "Two pipelines failed: **orders** and **refunds**.",
    model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
    looked_at: [
      { tool: "list_pipelines", arguments: {}, ok: true },
      { tool: "get_run_status", arguments: { run_id: "r1" }, ok: true },
      { tool: "get_run_status", arguments: { run_id: "r2" }, ok: true },
    ],
    cost_cents: 1,
    ...overrides,
  }
}

// Routes authFetch: GET models, POST messages. `asks` collects each question's body.
function route(opts: { models?: unknown[]; def?: string; answers?: Response[] } = {}) {
  const asks: Record<string, unknown>[] = []
  const answers = [...(opts.answers ?? [json(200, answer())])]
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === API_ENDPOINTS.ASSISTANT.MODELS) {
      return Promise.resolve(json(200, { entries: opts.models ?? [claude], default: opts.def ?? "claude" }))
    }
    if (url === API_ENDPOINTS.ASSISTANT.MESSAGES && init?.method === "POST") {
      asks.push(JSON.parse(String(init.body)))
      return Promise.resolve(answers.shift() ?? json(200, answer()))
    }
    return Promise.resolve(json(404, { error: "not_found" }))
  })
  return asks
}

async function askVia(text: string) {
  const box = await screen.findByRole("textbox", { name: "Your question" })
  fireEvent.change(box, { target: { value: text } })
  fireEvent.click(screen.getByRole("button", { name: "Send" }))
}

function openHistory() {
  const trigger = screen.getByRole("button", { name: "Chat history" })
  trigger.focus()
  fireEvent.keyDown(trigger, { key: "Enter" })
}

/** The chat's messages, not the header's title or History's list. */
function thread() {
  return within(screen.getByRole("log", { name: "Chat" }))
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

beforeEach(() => {
  mockFetch.mockReset()
  nav.pathname = "/"
  useAssistantStore.setState({
    open: false,
    expanded: false,
    page: null,
    pageCleared: false,
    suggestion: null,
    chats: [],
    activeChatId: null,
  })
  try {
    localStorage.clear()
  } catch {}
})

afterEach(() => setActiveWorkspaceId(null))

describe("pageFromPath", () => {
  it("reads a pipeline, connection, run or dashboard from a uuid route, lowercased", () => {
    expect(pageFromPath(`/pipelines/${PIPELINE.toUpperCase()}`)).toEqual({ kind: "pipeline", id: PIPELINE })
    expect(pageFromPath(`/pipelines/${PIPELINE}/edit`)).toEqual({ kind: "pipeline", id: PIPELINE })
    expect(pageFromPath(`/connections/${PIPELINE}`)).toEqual({ kind: "connection", id: PIPELINE })
    expect(pageFromPath(`/executions/${PIPELINE}`)).toEqual({ kind: "run", id: PIPELINE })
    expect(pageFromPath(`/dashboards/${PIPELINE}`)).toEqual({ kind: "dashboard", id: PIPELINE })
    expect(pageLabel({ kind: "dashboard" })).toBe("This dashboard")
  })

  it("is no page for anything that is not a uuid route", () => {
    for (const p of ["/", "/pipelines", "/pipelines/new", "/connections/browse", "/dashboards/new", `/pipelines/${PIPELINE}x`, `/admin/pipelines/${PIPELINE}`, null]) {
      expect(pageFromPath(p)).toBeNull()
    }
    expect(isUUID("new")).toBe(false)
    expect(isUUID(PIPELINE)).toBe(true)
  })
})

describe("Ask rsync.ai panel", () => {
  it("opens from the header button and from ⌘/ and Ctrl+/, and Escape closes it", async () => {
    route()
    render(<AssistantLauncher />)
    expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull()

    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    expect(await screen.findByRole("complementary", { name: "Ask rsync.ai" })).toBeTruthy()
    fireEvent.keyDown(window, { key: "Escape" })
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull())

    fireEvent.keyDown(window, { key: "/", metaKey: true })
    expect(await screen.findByRole("complementary", { name: "Ask rsync.ai" })).toBeTruthy()
    fireEvent.keyDown(window, { key: "/", ctrlKey: true })
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull())

    // A plain "/" is typing, not the shortcut.
    fireEvent.keyDown(window, { key: "/" })
    expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull()
  })

  it("renders the panel at the body, not inside the launcher's parent", async () => {
    // The launcher lives in the header, whose backdrop blur would otherwise be
    // the panel's containing block (header-no-fixed-descendant.test.tsx).
    route()
    render(
      <header className="fixed backdrop-blur-sm">
        <AssistantLauncher />
      </header>,
    )
    const button = screen.getByRole("button", { name: "Ask rsync.ai" })
    expect(button.closest("header")).not.toBeNull() // control: the launcher is inside
    fireEvent.click(button)
    const panel = await screen.findByRole("complementary", { name: "Ask rsync.ai" })
    expect(panel.closest("header")).toBeNull()
    expect(panel.parentElement).toBe(document.body)
    expect(button).toHaveAttribute("aria-controls", panel.id)
  })

  it("leaves a key another widget already handled alone: Escape for a menu, Ctrl+/ for an editor", async () => {
    route()
    render(<AssistantLauncher />)
    // A handler that runs first and claims the key, as Radix menus (Escape)
    // and CodeMirror (Mod-/ toggles a comment) do.
    const claim = (e: KeyboardEvent) => e.preventDefault()
    document.addEventListener("keydown", claim, { capture: true })
    try {
      fireEvent.keyDown(document, { key: "/", ctrlKey: true })
      expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull()
      act(() => useAssistantStore.getState().toggle())
      await screen.findByRole("complementary", { name: "Ask rsync.ai" })
      fireEvent.keyDown(document, { key: "Escape" })
      expect(screen.getByRole("complementary", { name: "Ask rsync.ai" })).toBeTruthy()
    } finally {
      document.removeEventListener("keydown", claim, { capture: true })
    }
    // Control: the same keys, unclaimed, do act.
    fireEvent.keyDown(document, { key: "Escape" })
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull())
  })

  it("gives focus back to the launcher when the panel closes", async () => {
    route()
    render(<AssistantLauncher />)
    const button = screen.getByRole("button", { name: "Ask rsync.ai" })
    button.focus()
    fireEvent.click(button)
    const box = await screen.findByRole("textbox", { name: "Your question" })
    await waitFor(() => expect(document.activeElement).toBe(box))
    fireEvent.click(screen.getByRole("button", { name: "Close assistant" }))
    await waitFor(() => expect(document.activeElement).toBe(button))
  })

  it("puts an answer that lands after New chat into the chat it was asked in, and the new chat starts fresh", async () => {
    let release: (r: Response) => void = () => {}
    const late = new Promise<Response>((r) => (release = r))
    const asks: Record<string, unknown>[] = []
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === API_ENDPOINTS.ASSISTANT.MODELS) {
        return Promise.resolve(json(200, { entries: [claude], default: "claude" }))
      }
      asks.push(JSON.parse(String(init?.body)))
      return asks.length === 1 ? late : Promise.resolve(json(200, answer({ conversation_id: "conv-2", answer: "fresh" })))
    })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("first")
    fireEvent.click(await screen.findByRole("button", { name: "New chat" }))
    await askVia("second")
    expect(await screen.findByText("fresh")).toBeTruthy()
    expect(asks[1].conversation_id).toBeUndefined()

    // The first answer lands while the second chat is on screen: not there.
    await act(async () => release(json(200, answer({ conversation_id: "conv-old", answer: "late answer" }))))
    expect(screen.queryByText("late answer")).toBeNull()

    // It is in its own chat, which History opens.
    openHistory()
    const chats = await screen.findAllByRole("menuitem")
    expect(chats.map((c) => c.textContent)).toEqual([expect.stringContaining("second"), expect.stringContaining("first")])
    fireEvent.click(chats[1])
    expect(await screen.findByText("late answer")).toBeTruthy()
    expect(screen.queryByText("fresh")).toBeNull()
  })

  it("sends the page it was asked on as {kind, id} only, and drops it when the user clears it", async () => {
    nav.pathname = `/pipelines/${PIPELINE}`
    const asks = route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))

    expect(await screen.findByText("About: This pipeline")).toBeTruthy()
    await askVia("why is it slow?")
    await screen.findByText(/Answered by/)
    expect(asks[0]).toEqual({
      question: "why is it slow?",
      page: { kind: "pipeline", id: PIPELINE },
      model: "claude",
      request_id: expect.any(String),
    })

    fireEvent.click(screen.getByRole("button", { name: "Ask without this page" }))
    expect(screen.queryByText(/About:/)).toBeNull()
    await askVia("and in general?")
    await waitFor(() => expect(asks).toHaveLength(2))
    expect(asks[1].page).toBeUndefined()
  })

  it("carries the conversation id to the next question, and New chat drops it", async () => {
    const asks = route({ answers: [json(200, answer({ conversation_id: "conv-42" })), json(200, answer({ conversation_id: "conv-42" })), json(200, answer({ conversation_id: "conv-43" }))] })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))

    await askVia("which pipelines failed?")
    await waitFor(() => expect(screen.getAllByText(/Answered by/)).toHaveLength(1))
    expect(asks[0].conversation_id).toBeUndefined()

    await askVia("and yesterday?")
    await waitFor(() => expect(screen.getAllByText(/Answered by/)).toHaveLength(2))
    expect(asks[1].conversation_id).toBe("conv-42")

    fireEvent.click(screen.getByRole("button", { name: "New chat" }))
    await askVia("something else")
    await waitFor(() => expect(asks).toHaveLength(3))
    expect(asks[2].conversation_id).toBeUndefined()
  })

  it("shows the answer with what it looked at (once per tool) and which model answered", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    fireEvent.click(await screen.findByRole("button", { name: "Which pipelines are failing right now?" }))

    expect(await screen.findByText("Looked at your pipelines, run status")).toBeTruthy()
    expect(screen.getByText("Answered by Claude · claude-sonnet-5-5")).toBeTruthy()
    // Model output goes through the markdown renderer.
    expect(screen.getByText("orders").tagName).toBe("STRONG")
  })

  it("says so when a question stopped at the limit", async () => {
    route({ answers: [json(200, answer({ stopped: "limit", answer: "I looked at 8 things." }))] })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("everything about everything")
    expect(await screen.findByText(/Stopped at the limit for one question/)).toBeTruthy()
  })

  it("hides the picker for one model and lists exactly the returned models for more", async () => {
    // On a pipeline page, so the composer's bottom row has the chip as well.
    nav.pathname = `/pipelines/${PIPELINE}`
    route({ models: [claude] })
    const { unmount } = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await screen.findByText("About: This pipeline")
    await waitFor(() => expect(mockFetch).toHaveBeenCalledWith(API_ENDPOINTS.ASSISTANT.MODELS))
    expect(screen.queryByRole("combobox", { name: "Model" })).toBeNull()
    unmount()
    useAssistantStore.setState({ open: false })

    const asks = route({ models: [claude, chatgpt], def: "claude" })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    const picker = (await screen.findByRole("combobox", { name: "Model" })) as HTMLSelectElement
    expect(within(picker).getAllByRole("option").map((o) => o.textContent)).toEqual(["Claude", "ChatGPT"])
    expect(picker.value).toBe("claude")

    fireEvent.change(picker, { target: { value: "chatgpt" } })
    await askVia("hi")
    await waitFor(() => expect(asks).toHaveLength(1))
    expect(asks[0].model).toBe("chatgpt")
  })

  it("with no model set up, says where an admin adds a key", async () => {
    route({ models: [], def: "" })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    expect(await screen.findByText(/An admin can add a key in Admin → Settings → AI models/)).toBeTruthy()
  })

  it("turns model_not_configured into the same message instead of the raw error", async () => {
    route({ answers: [json(409, { error: "model_not_configured", message: "no key for claude" })] })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("hi")
    const alert = await screen.findByRole("alert")
    expect(alert.textContent).toMatch(/An admin can add a key in Admin → Settings → AI models/)
    expect(alert.textContent).not.toMatch(/no key for claude/)
  })

  it("sends with Enter, not with Shift+Enter", async () => {
    const asks = route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    const box = await screen.findByRole("textbox", { name: "Your question" })
    fireEvent.change(box, { target: { value: "line one" } })
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true })
    expect(asks).toHaveLength(0)
    fireEvent.keyDown(box, { key: "Enter" })
    await waitFor(() => expect(asks).toHaveLength(1))
  })
})

describe("Ask rsync.ai chats", () => {
  it("keeps the chat when the panel closes and opens again", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("which pipelines failed?")
    await screen.findByText(/Answered by/)

    fireEvent.click(screen.getByRole("button", { name: "Close assistant" }))
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Ask rsync.ai" })).toBeNull())
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await screen.findByRole("complementary", { name: "Ask rsync.ai" })
    expect(thread().getByText("which pipelines failed?")).toBeTruthy()
    expect(thread().getByText(/Answered by/)).toBeTruthy()
  })

  it("lists the chats in History, newest first, and opens the one picked", async () => {
    route({ answers: [json(200, answer({ answer: "first answer" })), json(200, answer({ conversation_id: "conv-2", answer: "second answer" }))] })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("first question")
    await screen.findByText("first answer")
    fireEvent.click(screen.getByRole("button", { name: "New chat" }))
    expect(screen.queryByText("first answer")).toBeNull()
    await askVia("second question")
    await screen.findByText("second answer")

    openHistory()
    const chats = await screen.findAllByRole("menuitem")
    expect(chats.map((c) => c.textContent)).toEqual([
      expect.stringContaining("second question"),
      expect.stringContaining("first question"),
    ])
    expect(chats[0]).toHaveAttribute("aria-current", "true")
    fireEvent.click(chats[1])
    expect(await screen.findByText("first answer")).toBeTruthy()
    expect(screen.queryByText("second answer")).toBeNull()
  })

  // On prod the browser's abort never reached the gateway, so the server went
  // on asking the model and running tools after Stop. Stop also tells the
  // server, by the question's request_id.
  it("Stop ends the request on its way, tells the server which question, and says it stopped", async () => {
    let signal: AbortSignal | null | undefined
    const asks: Record<string, unknown>[] = []
    const cancels: string[] = []
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === API_ENDPOINTS.ASSISTANT.MODELS) {
        return Promise.resolve(json(200, { entries: [claude], default: "claude" }))
      }
      if (url === API_ENDPOINTS.ASSISTANT.MESSAGES && init?.method === "POST") {
        asks.push(JSON.parse(String(init.body)))
        signal = init?.signal
        return new Promise<Response>((_, reject) =>
          init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError"))),
        )
      }
      if (init?.method === "POST" && url.endsWith("/cancel")) {
        cancels.push(url)
        return Promise.resolve({ ok: true, status: 204, json: () => Promise.resolve(null) } as Response)
      }
      return Promise.resolve(json(404, { error: "not_found" }))
    })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("a slow one")
    fireEvent.click(await screen.findByRole("button", { name: "Stop" }))

    expect(await screen.findByText("Stopped before it answered.")).toBeTruthy()
    expect(signal?.aborted).toBe(true)
    const requestId = asks[0]?.request_id
    expect(typeof requestId).toBe("string")
    expect(String(requestId)).toMatch(/^[A-Za-z0-9_-]{1,64}$/)
    expect(cancels).toEqual([`${API_ENDPOINTS.ASSISTANT.MESSAGES}/${requestId}/cancel`])
    expect(screen.queryByRole("alert")).toBeNull()
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy()
  })

  it("names each question with a new request_id, even where crypto.randomUUID is missing (plain http)", async () => {
    // A self-hosted server on http://<ip> is not a secure context: no randomUUID.
    const randomUUID = crypto.randomUUID
    Object.defineProperty(crypto, "randomUUID", { value: undefined, configurable: true })
    try {
      const asks = route({ answers: [json(200, answer()), json(200, answer({ answer: "second" }))] })
      render(<AssistantLauncher />)
      fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
      await askVia("first")
      await thread().findByText(/Two pipelines failed/)
      await askVia("second")
      await thread().findByText("second")
      const ids = asks.map((a) => a.request_id)
      expect(ids).toHaveLength(2)
      for (const id of ids) expect(String(id)).toMatch(/^[A-Za-z0-9_-]{16,64}$/)
      expect(ids[0]).not.toBe(ids[1])
    } finally {
      Object.defineProperty(crypto, "randomUUID", { value: randomUUID, configurable: true })
    }
  })

  it("Try again asks the failed question once more, without a second copy of it", async () => {
    const asks = route({
      answers: [json(502, { error: "provider_error", message: "overloaded" }), json(200, answer({ answer: "second time lucky" }))],
    })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("flaky")
    fireEvent.click(within(await screen.findByRole("alert")).getByRole("button", { name: "Try again" }))

    expect(await screen.findByText("second time lucky")).toBeTruthy()
    expect(screen.queryByRole("alert")).toBeNull()
    expect(thread().getAllByText("flaky")).toHaveLength(1)
    expect(asks.map((a) => a.question)).toEqual(["flaky", "flaky"])
  })

  it("past 30 minutes says the assistant forgot, and the next question starts a new conversation", async () => {
    const asks = route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("first")
    await screen.findByText(/Answered by/)
    // The last answer came 30 minutes and a second ago.
    act(() =>
      useAssistantStore.setState((s) => ({
        chats: s.chats.map((c) => ({ ...c, updatedAt: c.updatedAt - SERVER_MEMORY_MS - 1000 })),
      })),
    )
    await askVia("follow-up")
    await waitFor(() => expect(asks).toHaveLength(2))
    expect(asks[1].conversation_id).toBeUndefined()
    expect(screen.getByText(/no longer remembers the questions above/)).toBeTruthy()
  })

  it("shows a chat only in the workspace it was started in", async () => {
    act(() => setActiveWorkspaceId("ws-a"))
    route({ answers: [json(200, answer({ answer: "answer in A" }))] })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("in A")
    await screen.findByText("answer in A")

    act(() => setActiveWorkspaceId("ws-b"))
    expect(screen.queryByText("answer in A")).toBeNull()
    act(() => setActiveWorkspaceId("ws-a"))
    expect(screen.getByText("answer in A")).toBeTruthy()

    act(() => setActiveWorkspaceId("ws-b"))
    openHistory()
    expect(await screen.findByText("No chats yet.")).toBeTruthy()
    expect(screen.queryByRole("menuitem")).toBeNull()
  })

  it("↑ in an empty box brings back this chat's last question, ↓ goes forward again", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("first q")
    await waitFor(() => expect(screen.getAllByText(/Answered by/)).toHaveLength(1))
    await askVia("second q")
    await waitFor(() => expect(screen.getAllByText(/Answered by/)).toHaveLength(2))

    const box = screen.getByRole("textbox", { name: "Your question" })
    fireEvent.keyDown(box, { key: "ArrowUp" })
    expect(box).toHaveValue("second q")
    fireEvent.keyDown(box, { key: "ArrowUp" })
    expect(box).toHaveValue("first q")
    fireEvent.keyDown(box, { key: "ArrowDown" })
    expect(box).toHaveValue("second q")
    fireEvent.keyDown(box, { key: "ArrowDown" })
    expect(box).toHaveValue("")
    // Text the user typed is never replaced.
    fireEvent.change(box, { target: { value: "draft" } })
    fireEvent.keyDown(box, { key: "ArrowUp" })
    expect(box).toHaveValue("draft")
  })

  it("Expand fills the page and Collapse docks it beside the page again", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    const panel = await screen.findByRole("complementary", { name: "Ask rsync.ai" })
    expect(panel).toHaveAttribute("data-expanded", "false")
    fireEvent.click(screen.getByRole("button", { name: "Expand assistant" }))
    expect(panel).toHaveAttribute("data-expanded", "true")
    fireEvent.click(screen.getByRole("button", { name: "Collapse assistant" }))
    expect(panel).toHaveAttribute("data-expanded", "false")
  })

  it("the page gives up the panel's width only while the panel is open and docked", () => {
    render(<DashboardShell>page</DashboardShell>)
    const main = screen.getByRole("main")
    expect(main.className).not.toContain("xl:pr-(--assistant-dock-width)")
    act(() => useAssistantStore.getState().openWith())
    expect(main.className).toContain("xl:pr-(--assistant-dock-width)")
    act(() => useAssistantStore.getState().toggleExpanded())
    expect(main.className).not.toContain("xl:pr-(--assistant-dock-width)")
  })

  it("the docked panel has a resize edge; the expanded one does not", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    const panel = await screen.findByRole("complementary", { name: "Ask rsync.ai" })
    const edge = within(panel).getByRole("separator", { name: "Resize Ask rsync.ai" })
    expect(edge).toHaveAttribute("aria-controls", panel.id)
    expect(panel.className).toContain("xl:w-(--assistant-dock-width)")
    fireEvent.click(screen.getByRole("button", { name: "Expand assistant" }))
    expect(within(panel).queryByRole("separator")).toBeNull()
  })
})

describe("pipeline dock with the assistant on", () => {
  beforeEach(() => featureFlagsManager.updateFlags({ assistant: true }))
  afterEach(() => featureFlagsManager.resetFlags())

  it("opens the assistant about this pipeline, by id and name", async () => {
    route()
    render(<PipelineCopilotDock pipelineId={PIPELINE.toUpperCase()} pipelineName="orders-sync" stages={[]} />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai about this pipeline" }))
    expect(useAssistantStore.getState()).toMatchObject({
      open: true,
      page: { kind: "pipeline", id: PIPELINE, name: "orders-sync" },
    })
    // The old chat copilot (which pasted stage text into a prompt) is not rendered.
    expect(screen.queryByRole("button", { name: "Open Pipeline Copilot" })).toBeNull()
  })

  it("asks about that pipeline by {kind, id}: the name it shows never goes to the server", async () => {
    const asks = route()
    render(
      <>
        <PipelineCopilotDock pipelineId={PIPELINE} pipelineName="orders-sync" stages={[]} />
        <AssistantLauncher />
      </>,
    )
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai about this pipeline" }))
    expect(await screen.findByText("About: This pipeline: orders-sync")).toBeTruthy()
    await askVia("why did it fail?")
    await waitFor(() => expect(asks).toHaveLength(1))
    expect(asks[0].page).toEqual({ kind: "pipeline", id: PIPELINE })
  })

  it("opens with no page for an id that is not a uuid", () => {
    render(<PipelineCopilotDock pipelineId="draft" stages={[]} />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai about this pipeline" }))
    expect(useAssistantStore.getState()).toMatchObject({ open: true, page: null })
  })
})

describe("assistant feature flag", () => {
  beforeEach(() => featureFlagsManager.resetFlags())
  afterEach(() => featureFlagsManager.resetFlags())

  it("is off by default and until the API says true", async () => {
    expect(featureFlagsManager.getFlags().assistant).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { agent_gateway: true }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().assistant).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { assistant: "true" }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().assistant).toBe(false)

    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(json(200, { assistant: true }))
    await fetchFeatureFlags("http://gw")
    expect(featureFlagsManager.getFlags().assistant).toBe(true)
  })

  it("keeps the dock on the old copilot while off", () => {
    act(() => featureFlagsManager.updateFlags({ assistant: false }))
    render(<PipelineCopilotDock pipelineId={PIPELINE} stages={[]} />)
    expect(screen.getByRole("button", { name: "Open Pipeline Copilot" })).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Ask rsync.ai about this pipeline" })).toBeNull()
  })
})

describe("Tab writes the suggestion", () => {
  // A new chat offers the welcome's first question as the placeholder, and Tab
  // writes it in; a chat under way has its own thread, so ↑ is the hint there.
  // `!fireEvent.keyDown(...)` is true when the box took the key.
  it("a new chat: the first starter is the placeholder and Tab writes it in", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    const box = await screen.findByRole("textbox", { name: "Your question" })
    expect(box).toHaveAttribute("placeholder", "Which pipelines are failing right now?")
    expect(screen.getByText(/Tab for the suggestion/)).toBeTruthy()

    expect(!fireEvent.keyDown(box, { key: "Tab" })).toBe(true)
    expect(box).toHaveValue("Which pipelines are failing right now?")
    expect(screen.getByText(/↑ for your last question/)).toBeTruthy()
  })

  it("a new chat about a pipeline offers that pipeline's question", async () => {
    featureFlagsManager.updateFlags({ assistant: true })
    try {
      route()
      render(
        <>
          <PipelineCopilotDock pipelineId={PIPELINE} pipelineName="orders-sync" stages={[]} />
          <AssistantLauncher />
        </>,
      )
      fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai about this pipeline" }))
      const box = await screen.findByRole("textbox", { name: "Your question" })
      expect(box).toHaveAttribute("placeholder", "Why did this pipeline's last run fail?")
    } finally {
      featureFlagsManager.resetFlags()
    }
  })

  it("an opener's suggestion is offered instead, and goes with its page", async () => {
    // A dashboard's Schedule email button opens the panel with its own request.
    route()
    render(<AssistantLauncher />)
    act(() => useAssistantStore.getState().openWith({ kind: "dashboard", id: PIPELINE, name: "Ops" }, "Email this dashboard"))
    const box = await screen.findByRole("textbox", { name: "Your question" })
    expect(box).toHaveAttribute("placeholder", "Email this dashboard")
    expect(screen.getByText("About: This dashboard: Ops")).toBeTruthy()

    // Without its page the request has no subject, so the page's starter is gone too.
    fireEvent.click(screen.getByRole("button", { name: "Ask without this page" }))
    expect(box).toHaveAttribute("placeholder", "Which pipelines are failing right now?")

    // Opened again with no suggestion, the page's own first question is back.
    act(() => useAssistantStore.getState().openWith({ kind: "dashboard", id: PIPELINE, name: "Ops" }))
    expect(box).toHaveAttribute("placeholder", "What charts are on this dashboard?")
  })

  it("a chat under way has no suggestion: Tab leaves the box and the hint is ↑", async () => {
    route()
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await askVia("first q")
    await waitFor(() => expect(screen.getAllByText(/Answered by/)).toHaveLength(1))

    const box = screen.getByRole("textbox", { name: "Your question" })
    expect(box).toHaveAttribute("placeholder", "Ask about your pipelines, connections or runs…")
    expect(!fireEvent.keyDown(box, { key: "Tab" })).toBe(false)
    expect(box).toHaveValue("")
    expect(screen.getByText(/↑ for your last question/)).toBeTruthy()
    expect(screen.queryByText(/Tab for the suggestion/)).toBeNull()
  })

  it("with no model set up there is nothing to suggest", async () => {
    route({ models: [], def: "" })
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await screen.findByText(/An admin can add a key/)
    const box = screen.getByRole("textbox", { name: "Your question" })
    await waitFor(() => expect(box).toHaveAttribute("placeholder", "No AI model is set up yet"))
    expect(!fireEvent.keyDown(box, { key: "Tab" })).toBe(false)
    expect(box).toHaveValue("")
    expect(screen.queryByText(/Tab for the suggestion/)).toBeNull()
  })

  it("the old pipeline copilot: Tab writes the first suggested question", () => {
    featureFlagsManager.updateFlags({ assistant: false })
    try {
      const failed = { id: "s1", display_name: "load", status: "failed" } as ExecutionPlanStage
      render(<PipelineCopilotDock pipelineId={PIPELINE} stages={[failed]} />)
      fireEvent.click(screen.getByRole("button", { name: "Open Pipeline Copilot" }))
      const box = screen.getByRole("textbox", { name: "Ask about this pipeline" })
      expect(box).toHaveAttribute("placeholder", 'Diagnose "load" failure')
      expect(screen.getByText(/Tab for the suggestion/)).toBeTruthy()

      expect(!fireEvent.keyDown(box, { key: "Tab" })).toBe(true)
      expect(box).toHaveValue('Diagnose "load" failure')
      expect(screen.queryByText(/Tab for the suggestion/)).toBeNull()
      // Text in the box: Tab moves on.
      expect(!fireEvent.keyDown(box, { key: "Tab" })).toBe(false)
    } finally {
      featureFlagsManager.resetFlags()
    }
  })
})
