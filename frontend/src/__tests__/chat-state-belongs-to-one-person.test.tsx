import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"

import LoginPage from "@/app/(auth)/login/page"
import SignupClient from "@/app/(auth)/signup/signup-client"
import { logout } from "@/lib/auth"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// /chat keeps its conversation, its session id and the pipelines it started in
// localStorage, per workspace. Signing out removes them, but a session that runs
// out reaches /login without /logout (middleware.ts sends a request with no
// cookie straight there), so the next person to sign in on that browser was
// shown the last one's /chat, in the same workspace (inbox 2026-10-11).
//
// Bug class: browser state written for one signed-in person, read by the next.
// A session only starts at a sign-in or a sign-up, so that is where the state is
// claimed for the person; pinned here:
//   - someone else signing in or up never sees it;
//   - state no sign-in claimed (an older build's) is dropped too;
//   - the same person signing back in keeps it (control);
//   - signing out still removes it (control).

const pushMock = vi.fn()
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: pushMock, replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(""),
}))
vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

// jsdom's web storage is not fully functional under this harness (see
// scoped-storage.test.ts); key()/length must be honest for the clear to scan.
function memoryStorage(): Storage {
  const store: Record<string, string> = {}
  return {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => {
      store[k] = String(v)
    },
    removeItem: (k) => {
      delete store[k]
    },
    clear: () => {
      for (const k of Object.keys(store)) delete store[k]
    },
    key: (i) => Object.keys(store)[i] ?? null,
    get length() {
      return Object.keys(store).length
    },
  }
}

const WS = "ws-shared"
const CHAT_KEYS = ["rsync_chat_ui_v2_state", "rsync_chat_session_id", "rsync_active_pipelines"]

/** What /chat leaves behind for whoever is signed in, in the shared workspace. */
function writeChatState(who: string) {
  window.localStorage.setItem(`rsync_chat_ui_v2_state:${WS}`, JSON.stringify({ v: 1, messages: [`${who}: orders by region`] }))
  window.localStorage.setItem(`rsync_chat_session_id:${WS}`, `session-${who}`)
  window.localStorage.setItem(`rsync_active_pipelines:${WS}`, JSON.stringify([{ id: `p-${who}` }]))
}

function chatState(): Record<string, string | null> {
  return Object.fromEntries(CHAT_KEYS.map((k) => [k, window.localStorage.getItem(`${k}:${WS}`)]))
}

const nothing = Object.fromEntries(CHAT_KEYS.map((k) => [k, null]))

function answerWith(userId: string, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok: true,
      status,
      json: async () => ({ user_id: userId, email: `${userId}@acme.com`, email_verified: true }),
    })),
  )
}

async function signIn(userId: string) {
  answerWith(userId)
  pushMock.mockClear()
  render(<LoginPage />)
  fireEvent.change(await screen.findByLabelText("Email"), { target: { value: `${userId}@acme.com` } })
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "password123" } })
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }))
  await waitFor(() => expect(pushMock).toHaveBeenCalled())
  cleanup()
}

async function signUp(userId: string) {
  answerWith(userId, 201)
  pushMock.mockClear()
  render(<SignupClient />)
  fireEvent.change(screen.getByLabelText(/work email/i), { target: { value: `${userId}@acme.com` } })
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "password123" } })
  fireEvent.change(screen.getByLabelText(/confirm password/i), { target: { value: "password123" } })
  fireEvent.click(screen.getByRole("button", { name: /create account/i }))
  await waitFor(() => expect(pushMock).toHaveBeenCalled())
  cleanup()
}

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { value: memoryStorage(), configurable: true, writable: true })
  Object.defineProperty(window, "sessionStorage", { value: memoryStorage(), configurable: true, writable: true })
  window.localStorage.setItem(ACTIVE_WORKSPACE_KEY, WS)
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe("/chat's browser state belongs to the person who signed in", () => {
  it("is not there for the next person who signs in after a session ran out", async () => {
    await signIn("user-a")
    writeChatState("user-a")

    await signIn("user-b")
    expect(chatState()).toEqual(nothing)
  })

  it("is not there for someone who signs up on the same browser", async () => {
    await signIn("user-a")
    writeChatState("user-a")

    await signUp("user-c")
    expect(chatState()).toEqual(nothing)
  })

  it("is dropped when no sign-in claimed it (an older build's)", async () => {
    writeChatState("someone")

    await signIn("user-b")
    expect(chatState()).toEqual(nothing)
  })

  // Controls: the clear is for another person, not for every sign-in.
  it("is kept when the same person signs back in", async () => {
    await signIn("user-a")
    writeChatState("user-a")
    const before = chatState()
    expect(before["rsync_chat_session_id"]).toBe("session-user-a")

    await signIn("user-a")
    expect(chatState()).toEqual(before)
  })

  it("is removed on sign-out", async () => {
    await signIn("user-a")
    writeChatState("user-a")

    logout()
    expect(chatState()).toEqual(nothing)
  })
})
