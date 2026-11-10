import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor, fireEvent } from "@testing-library/react"
import type { Mock } from "vitest"

import SignupClient from "@/app/(auth)/signup/signup-client"
import { POST_VERIFY_NEXT_KEY } from "@/lib/auth/safe-next"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// Single-workspace mode: after the first user, sign-up is invite-only, and a
// workspace invite is one of the ways in. The invite landing sends a logged-out
// invitee to /signup?workspace_invite=<token>&next=/invite/<token>; signup must
// forward the token to /auth/register (the gateway claims it in the sign-up
// transaction) and, once the response names the joined workspace, land there
// rather than return to an invite that has just been used.

const pushMock = vi.fn()
let searchParamsStr = ""
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: pushMock }),
  useSearchParams: () => new URLSearchParams(searchParamsStr),
}))
vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => (
    <a href={typeof href === "string" ? href : "#"} {...rest}>
      {children}
    </a>
  ),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("@/lib/workspace/active-workspace", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/workspace/active-workspace")>()),
  setActiveWorkspaceId: vi.fn(),
}))
// The instance-invite path validates its token on mount; these tests are about
// the workspace invite, so that lookup answers valid with no email hint.
vi.mock("@/lib/api/admin", () => ({ validateInvite: vi.fn(async () => ({ valid: true })) }))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

function installMemorySessionStorage() {
  const store: Record<string, string> = {}
  const ss: Storage = {
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
  Object.defineProperty(window, "sessionStorage", { value: ss, configurable: true, writable: true })
}

function fillAndSubmit() {
  fireEvent.change(screen.getByLabelText(/work email/i), { target: { value: "bob@acme.com" } })
  fireEvent.change(screen.getByLabelText("Password"), { target: { value: "password123" } })
  fireEvent.change(screen.getByLabelText(/confirm password/i), { target: { value: "password123" } })
  fireEvent.click(screen.getByRole("button", { name: /create account/i }))
}

function mockRegister(body: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({ ok: true, status: 201, json: async () => ({ email: "bob@acme.com", ...body }) })),
  )
}

function registerURL(): URL {
  const reg = (global.fetch as Mock).mock.calls.find((c) => String(c[0]).includes("/api/v1/auth/register"))
  expect(reg).toBeTruthy()
  return new URL(String(reg![0]), "http://localhost")
}

const invitePath = "/invite/ws-tok-1"

beforeEach(() => {
  vi.clearAllMocks()
  searchParamsStr = ""
  installMemorySessionStorage()
})

describe("SignupClient workspace invite", () => {
  it("forwards workspace_invite to /auth/register", async () => {
    searchParamsStr = `workspace_invite=ws-tok-1&next=${encodeURIComponent(invitePath)}`
    mockRegister({ email_verified: true, joined_workspace_id: "ws-acme" })

    render(<SignupClient />)
    fillAndSubmit()

    await waitFor(() => expect(pushMock).toHaveBeenCalled())
    const url = registerURL()
    expect(url.searchParams.get("workspace_invite")).toBe("ws-tok-1")
    expect(url.searchParams.has("invite")).toBe(false)
  })

  it("forwards both tokens when the link carries an instance invite too", async () => {
    searchParamsStr = `invite=inst-tok&workspace_invite=ws-tok-1`
    mockRegister({ email_verified: true })

    render(<SignupClient />)
    await waitFor(() => expect(screen.getByRole("button", { name: /create account/i })).toBeEnabled())
    fillAndSubmit()

    await waitFor(() => expect(pushMock).toHaveBeenCalled())
    const url = registerURL()
    expect(url.searchParams.get("invite")).toBe("inst-tok")
    expect(url.searchParams.get("workspace_invite")).toBe("ws-tok-1")
  })

  it("lands in the joined workspace instead of returning to the used invite", async () => {
    searchParamsStr = `workspace_invite=ws-tok-1&next=${encodeURIComponent(invitePath)}`
    mockRegister({ email_verified: true, joined_workspace_id: "ws-acme" })

    render(<SignupClient />)
    fillAndSubmit()

    await waitFor(() => expect(pushMock).toHaveBeenCalledWith("/"))
    expect(pushMock).not.toHaveBeenCalledWith(invitePath)
    expect(setActiveWorkspaceId).toHaveBeenCalledWith("ws-acme")
  })

  it("does not stash the used invite for after verification", async () => {
    searchParamsStr = `workspace_invite=ws-tok-1&next=${encodeURIComponent(invitePath)}`
    mockRegister({ email_verified: false, joined_workspace_id: "ws-acme" })

    render(<SignupClient />)
    fillAndSubmit()

    expect(await screen.findByText(/check your email/i)).toBeInTheDocument()
    expect(window.sessionStorage.getItem(POST_VERIFY_NEXT_KEY)).toBeNull()
    expect(setActiveWorkspaceId).toHaveBeenCalledWith("ws-acme")
  })

  // Control: no joined workspace in the response (the invite was not claimed at
  // sign-up) keeps today's return trip to the invite page.
  it("returns to next when the response names no joined workspace", async () => {
    searchParamsStr = `workspace_invite=ws-tok-1&next=${encodeURIComponent(invitePath)}`
    mockRegister({ email_verified: true })

    render(<SignupClient />)
    fillAndSubmit()

    await waitFor(() => expect(pushMock).toHaveBeenCalledWith(invitePath))
    expect(setActiveWorkspaceId).not.toHaveBeenCalled()
  })
})
