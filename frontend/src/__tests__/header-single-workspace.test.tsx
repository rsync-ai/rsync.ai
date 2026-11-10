import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import type { Mock } from "vitest"

import { Header } from "@/components/layout/Header"
import { authFetch } from "@/lib/api/auth-fetch"
import { featureFlagsManager } from "@/config/features"
import { ACTIVE_WORKSPACE_KEY } from "@/lib/workspace/active-workspace"

// Single-workspace mode (RSYNC_SINGLE_WORKSPACE=true, the self-host compose and
// Helm; /api/v1/features says single_workspace): the gateway refuses to create a
// second workspace, so the header stops offering one. With exactly one workspace
// there is nothing to switch to, so the switcher becomes a label. An upgraded
// install whose users already hold several keeps the switcher, minus "New
// workspace". The 403 is the real gate; this only stops the UI offering it.

const pushMock = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: pushMock, replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/",
}))
vi.mock("next-themes", () => ({
  useTheme: () => ({ theme: "light", setTheme: vi.fn(), resolvedTheme: "light" }),
}))
vi.mock("@/lib/auth", () => ({ logout: vi.fn(), getUserEmail: () => null }))
vi.mock("@/lib/store/useUIStore", () => ({
  useUIStore: (selector: (s: Record<string, unknown>) => unknown) =>
    selector({ sidebarCollapsed: false, setSidebarOpen: () => {}, toggleCommandPalette: () => {} }),
}))

const mockFetch = authFetch as unknown as Mock

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.setPointerCapture) Element.prototype.setPointerCapture = () => {}
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

function installMemoryStorage() {
  const store: Record<string, string> = {}
  const ls: Storage = {
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
  Object.defineProperty(window, "localStorage", { value: ls, configurable: true, writable: true })
}

type Ws = { id: string; name: string; slug: string; role: string }
const ACME: Ws = { id: "ws-acme", name: "Acme", slug: "acme", role: "owner" }
const OLD_TEAM: Ws = { id: "ws-old", name: "Old Team", slug: "old-team", role: "member" }

function wireWorkspaces(workspaces: Ws[]) {
  mockFetch.mockImplementation(async (url: string, opts?: RequestInit) => {
    const method = (opts?.method ?? "GET").toUpperCase()
    if (/\/workspaces$/.test(String(url)) && method === "GET") {
      return { ok: true, status: 200, json: async () => ({ workspaces }) }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
}

function setSingleWorkspace(on: boolean) {
  featureFlagsManager.updateFlags({ singleWorkspace: on })
  featureFlagsManager.markResolved()
}

beforeEach(() => {
  vi.clearAllMocks()
  pushMock.mockReset()
  installMemoryStorage()
  document.cookie = `${ACTIVE_WORKSPACE_KEY}=; path=/; max-age=0; SameSite=Lax`
})

afterEach(() => {
  featureFlagsManager.resetFlags()
  vi.restoreAllMocks()
})

describe("Header — single-workspace mode", () => {
  it("shows the one workspace as a label, with nothing to switch or create", async () => {
    setSingleWorkspace(true)
    wireWorkspaces([ACME])

    render(<Header />)

    expect(await screen.findByText("Acme")).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: /switch workspace/i })).toBeNull()
    expect(screen.queryByText(/new workspace/i)).toBeNull()
  })

  it("keeps the switcher for an upgraded user with several, without New workspace", async () => {
    const user = userEvent.setup()
    setSingleWorkspace(true)
    wireWorkspaces([ACME, OLD_TEAM])

    render(<Header />)
    await user.click(await screen.findByRole("button", { name: /switch workspace/i }))

    expect(await screen.findByText("Old Team")).toBeInTheDocument()
    expect(screen.getByText(/workspace settings/i)).toBeInTheDocument()
    expect(screen.queryByText(/new workspace/i)).toBeNull()
  })

  // Control: the same one-workspace user on cloud keeps the switcher and its
  // "New workspace" item, so the label above is the flag's doing.
  it("cloud keeps the switcher and New workspace for one workspace", async () => {
    const user = userEvent.setup()
    setSingleWorkspace(false)
    wireWorkspaces([ACME])

    render(<Header />)
    await user.click(await screen.findByRole("button", { name: /switch workspace/i }))

    await waitFor(() => expect(screen.getByText(/new workspace/i)).toBeInTheDocument())
  })
})
