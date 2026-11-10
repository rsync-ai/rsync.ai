import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react"

import LogoutPage from "@/app/(auth)/logout/page"
import { AssistantLauncher } from "@/components/assistant/AssistantLauncher"
import { authFetch } from "@/lib/api/auth-fetch"
import { logout } from "@/lib/auth"
import { API_ENDPOINTS } from "@/lib/config/api"
import { useAssistantStore } from "@/lib/store/useAssistantStore"

// A hard refresh shows Ask rsync.ai as it was: the panel open or closed,
// Expand, the chats and the one on screen (lib/assistant/session.ts). A
// question on its way cannot come back, so it shows as failed with Try again
// and the server is told to stop it. A load that is not a reload (a copy of the
// tab) may still have it running in the first tab, so it says where the answer
// is, with no Try again to ask it twice. The copy is this tab's and this user's:
// another user never sees it, and both sign-out paths remove it.
//
// A reload is the launcher unmounted and the store back to its first state,
// which is what a page load gives; sessionStorage is what survives it. The new
// page's navigation timing says how it was loaded and when the load began.

const KEY = "rsync-assistant-session"
const who = vi.hoisted(() => ({ id: "user-1" as string | null }))

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/",
  useSearchParams: () => new URLSearchParams(),
}))
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: who.id ? { id: who.id } : null, role: "", isLoading: false }),
}))

const mockFetch = authFetch as unknown as Mock

function json(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) } as Response
}

const ANSWER = {
  conversation_id: "conv-1",
  answer: "Two pipelines failed: **orders** and **refunds**.",
  model: { entry: "claude", label: "Claude", model: "claude-sonnet-5-5" },
  looked_at: [],
  cost_cents: 1,
}

const RELOADED = "The page reloaded before the answer came."
const DROPPED = "The connection to the server dropped before the answer came."
const ELSEWHERE = "Asked before this page opened. If that page is still open, the answer shows there."

// GET models, POST messages (`messages` stands in for the answer), POST cancel.
function route(opts: { messages?: () => Promise<Response> } = {}) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === API_ENDPOINTS.ASSISTANT.MODELS) {
      return Promise.resolve(json(200, { entries: [ANSWER.model], default: "claude" }))
    }
    if (url === API_ENDPOINTS.ASSISTANT.MESSAGES && init?.method === "POST") {
      return opts.messages ? opts.messages() : Promise.resolve(json(200, ANSWER))
    }
    if (url.startsWith(`${API_ENDPOINTS.ASSISTANT.MESSAGES}/`) && init?.method === "POST") {
      return Promise.resolve(json(204, {}))
    }
    return Promise.resolve(json(404, { error: "not_found" }))
  })
}

async function ask(text: string) {
  const box = await screen.findByRole("textbox", { name: "Your question" })
  fireEvent.change(box, { target: { value: text } })
  fireEvent.click(screen.getByRole("button", { name: "Send" }))
}

const panel = () => screen.queryByRole("complementary", { name: "Ask rsync.ai" })
const thread = () => within(screen.getByRole("log", { name: "Chat" }))

const hang = () => new Promise<Response>(() => {})

/** An answer the test ends: `drop` is the browser failing the request. */
function pending() {
  let reject: ((err: Error) => void) | undefined
  const messages = () => new Promise<Response>((_, fail) => (reject = fail))
  return { messages, drop: (err: Error) => reject?.(err) }
}

const cancelled = () =>
  mockFetch.mock.calls.filter(([url]) => String(url).startsWith(`${API_ENDPOINTS.ASSISTANT.MESSAGES}/`))

/**
 * The page loads again. `type` is its navigation timing type: "reload", or
 * "back_forward" for a copy of the tab (Duplicate, reopen a closed tab), which
 * comes with this tab's sessionStorage. `startedAt` is when that load began.
 */
function reload(unmount: () => void, opts: { type?: NavigationTimingType; startedAt?: number } = {}) {
  const startedAt = opts.startedAt ?? Date.now()
  unmount()
  useAssistantStore.setState(useAssistantStore.getInitialState(), true)
  vi.spyOn(performance, "now").mockImplementation(() => Date.now() - startedAt)
  Object.defineProperty(performance, "getEntriesByType", {
    configurable: true,
    value: (kind: string) => (kind === "navigation" ? [{ type: opts.type ?? "reload" }] : []),
  })
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
  who.id = "user-1"
  useAssistantStore.setState(useAssistantStore.getInitialState(), true)
  sessionStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  Reflect.deleteProperty(performance, "getEntriesByType")
})

describe("Ask rsync.ai after a hard refresh", () => {
  it("shows the panel open, expanded, on the chat it showed", async () => {
    route()
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    expect(await screen.findByText("orders")).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Expand assistant" }))

    reload(first.unmount)
    render(<AssistantLauncher />)

    await waitFor(() => expect(panel()).toBeTruthy())
    expect(screen.getByRole("button", { name: "Collapse assistant" })).toBeTruthy()
    expect(thread().getByText("which pipelines failed?")).toBeTruthy()
    expect(thread().getByText("orders")).toBeTruthy()
  })

  it("keeps a closed panel closed, with its chat there when it opens", async () => {
    route()
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    expect(await screen.findByText("orders")).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Close assistant" }))

    reload(first.unmount)
    render(<AssistantLauncher />)

    await waitFor(() => expect(useAssistantStore.getState().chats).toHaveLength(1))
    expect(panel()).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    expect(thread().getByText("which pipelines failed?")).toBeTruthy()
  })

  it("shows a question the reload cut off as failed, tells the server to stop it, and Try again asks it", async () => {
    route({ messages: hang })
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    await waitFor(() => expect(useAssistantStore.getState().chats[0]?.requestId).toBeTruthy())
    const requestId = useAssistantStore.getState().chats[0].requestId!

    reload(first.unmount)
    route()
    render(<AssistantLauncher />)

    const alert = await screen.findByRole("alert")
    expect(within(alert).getByText(RELOADED)).toBeTruthy()
    await waitFor(() => expect(mockFetch).toHaveBeenCalledWith(API_ENDPOINTS.ASSISTANT.CANCEL(requestId), { method: "POST" }))
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }))
    expect(await screen.findByText("orders")).toBeTruthy()
    expect(thread().getAllByText("which pipelines failed?")).toHaveLength(1)
  })

  // What a browser does (KI-ASK-RELOAD-CUT-OFF-READS-FAILED-TO-FETCH): as the
  // page unloads, Chrome fails the request with "Failed to fetch", and the chat
  // is saved with that error and nothing on its way, after the new load began.
  it("shows a question the browser failed as the page reloaded as cut off, and tells the server to stop it", async () => {
    const answer = pending()
    route({ messages: answer.messages })
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    await waitFor(() => expect(useAssistantStore.getState().chats[0]?.requestId).toBeTruthy())
    const requestId = useAssistantStore.getState().chats[0].requestId!

    const startedAt = Date.now()
    await act(async () => answer.drop(new TypeError("Failed to fetch")))
    expect(useAssistantStore.getState().chats[0].pendingSince).toBeUndefined()
    reload(first.unmount, { startedAt })
    route()
    render(<AssistantLauncher />)

    const alert = await screen.findByRole("alert")
    expect(within(alert).getByText(RELOADED)).toBeTruthy()
    await waitFor(() => expect(mockFetch).toHaveBeenCalledWith(API_ENDPOINTS.ASSISTANT.CANCEL(requestId), { method: "POST" }))
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }))
    expect(await screen.findByText("orders")).toBeTruthy()
  })

  it("keeps a connection that dropped before the reload as it was, in words, and stops nothing", async () => {
    route({ messages: () => Promise.reject(new TypeError("Failed to fetch")) })
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    expect(within(await screen.findByRole("alert")).getByText(DROPPED)).toBeTruthy()
    expect(screen.queryByText("Failed to fetch")).toBeNull()

    await new Promise((r) => setTimeout(r, 5))
    reload(first.unmount)
    route()
    render(<AssistantLauncher />)

    expect(within(await screen.findByRole("alert")).getByText(DROPPED)).toBeTruthy()
    await act(async () => {})
    expect(cancelled()).toEqual([])
  })

  // A copy of the tab (Duplicate, reopen a closed tab) loads as "back_forward",
  // a link back to the app as "navigate": neither is known to have cut it off.
  it.each(["back_forward", "navigate"] as const)(
    "never stops, nor offers to ask again, a question a %s load may still have running in the first tab",
    async (type) => {
      route({ messages: hang })
      const first = render(<AssistantLauncher />)
      fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
      await ask("which pipelines failed?")
      await waitFor(() => expect(useAssistantStore.getState().chats[0]?.requestId).toBeTruthy())

      reload(first.unmount, { type })
      route()
      render(<AssistantLauncher />)

      expect(await screen.findByText(ELSEWHERE)).toBeTruthy()
      expect(thread().getByText("which pipelines failed?")).toBeTruthy()
      expect(screen.queryByText(RELOADED)).toBeNull()
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull()
      await act(async () => {})
      expect(cancelled()).toEqual([])
    },
  )

  it("never shows another user's chats, and drops them", async () => {
    route()
    const first = render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await ask("which pipelines failed?")
    expect(await screen.findByText("orders")).toBeTruthy()

    reload(first.unmount)
    who.id = "user-2"
    render(<AssistantLauncher />)

    await waitFor(() => expect(sessionStorage.getItem(KEY)).not.toContain("which pipelines failed?"))
    expect(panel()).toBeNull()
    expect(useAssistantStore.getState().chats).toEqual([])
  })

  it("keeps nothing until the signed-in user is known", async () => {
    route()
    who.id = null
    render(<AssistantLauncher />)
    fireEvent.click(screen.getByRole("button", { name: "Ask rsync.ai" }))
    await act(async () => {})
    expect(sessionStorage.getItem(KEY)).toBeNull()
  })

  it("is removed by both sign-out paths", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(json(200, {}))))
    sessionStorage.setItem(KEY, "{}")
    logout()
    expect(sessionStorage.getItem(KEY)).toBeNull()

    sessionStorage.setItem(KEY, "{}")
    render(<LogoutPage />)
    await waitFor(() => expect(sessionStorage.getItem(KEY)).toBeNull())
  })
})
