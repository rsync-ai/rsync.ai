import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"

import SettingsPage from "@/app/(dashboard)/settings/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { featureFlagsManager } from "@/config/features"

// /settings (your own settings). Pinned here:
//   - the theme picker marks the CHOICE, so "System" shows as picked. It read
//     `resolvedTheme`, which is only ever light or dark, so System never did;
//   - a profile read that fails for any reason but a 401 shows Retry. The page
//     logged the user out on every failure, a 500 or a dropped connection too;
//   - the open tab is in ?tab=, both ways, and the AI apps tab exists only with
//     the agent gateway flag.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const router = { push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }
let searchParams = new URLSearchParams()
vi.mock("next/navigation", () => ({
  useRouter: () => router,
  useSearchParams: () => searchParams,
  usePathname: () => "/settings",
}))

const setTheme = vi.fn()
let themeState: { theme?: string; resolvedTheme?: string } = { theme: "light", resolvedTheme: "light" }
vi.mock("next-themes", () => ({ useTheme: () => ({ ...themeState, setTheme }) }))

const mockFetch = authFetch as unknown as Mock

function res(body: unknown, status = 200) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as unknown as Response
}

const PROFILE = { name: "Ada Lovelace", email: "ada@example.com", role: "member" }

function route(me: () => Promise<Response> = async () => res(PROFILE)) {
  mockFetch.mockImplementation((url: string) => {
    if (url === API_ENDPOINTS.AUTH.ME) return me()
    if (url === API_ENDPOINTS.NOTIFICATIONS.PREFERENCES) {
      return Promise.resolve(
        res({
          email_enabled: true,
          email_categories: { run_status: true },
          categories: [{ id: "run_status", label: "Run failed or stopped", description: "A run ended with an error." }],
          channels: { email: true },
        }),
      )
    }
    if (url === API_ENDPOINTS.AGENT_TOKENS.LIST) return Promise.resolve(res({ tokens: [] }))
    return Promise.resolve(res({}))
  })
}

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  router.push.mockReset()
  setTheme.mockReset()
  searchParams = new URLSearchParams()
  themeState = { theme: "light", resolvedTheme: "light" }
  window.history.replaceState(null, "", "/settings")
})

afterEach(() => {
  act(() => featureFlagsManager.resetFlags())
})

describe("theme picker", () => {
  it("marks System as picked when the theme follows the system, whatever it resolved to", async () => {
    route()
    themeState = { theme: "system", resolvedTheme: "dark" }
    searchParams = new URLSearchParams("tab=appearance")
    render(<SettingsPage />)

    const system = await screen.findByRole("button", { name: "System" })
    expect(system).toHaveAttribute("aria-pressed", "true")
    expect(screen.getByRole("button", { name: "Dark" })).toHaveAttribute("aria-pressed", "false")
    expect(screen.getByRole("button", { name: "Light" })).toHaveAttribute("aria-pressed", "false")

    await userEvent.click(screen.getByRole("button", { name: "Light" }))
    expect(setTheme).toHaveBeenCalledWith("light")
  })
})

describe("profile read", () => {
  it("a 500 shows Retry and keeps you signed in; Retry reads again", async () => {
    let calls = 0
    route(async () => (++calls === 1 ? res({ error: "boom" }, 500) : res(PROFILE)))
    render(<SettingsPage />)

    await screen.findByText("Could not load your profile.")
    expect(router.push).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole("button", { name: "Retry" }))
    await waitFor(() => expect(screen.getByLabelText("Full name")).toHaveValue("Ada Lovelace"))
    expect(screen.getByLabelText("Email")).toHaveValue("ada@example.com")
  })

  it("a dropped connection shows Retry too", async () => {
    route(async () => {
      throw new TypeError("Failed to fetch")
    })
    render(<SettingsPage />)
    await screen.findByText("Could not load your profile.")
    expect(router.push).not.toHaveBeenCalled()
  })

  it("a 401 is the session gone, and logs out (control: the logout path still works)", async () => {
    route(async () => res({ error: "unauthorized" }, 401))
    render(<SettingsPage />)
    await waitFor(() => expect(router.push).toHaveBeenCalledWith("/logout?next=%2Fsettings"))
  })
})

describe("tabs", () => {
  it("opens the tab ?tab= names, and puts the tab you pick into the URL", async () => {
    route()
    searchParams = new URLSearchParams("tab=alerts")
    render(<SettingsPage />)

    expect(screen.getByRole("tab", { name: "Email alerts" })).toHaveAttribute("aria-selected", "true")
    expect(await screen.findByRole("switch", { name: "Email: Run failed or stopped" })).toBeInTheDocument()

    await userEvent.click(screen.getByRole("tab", { name: "Appearance" }))
    expect(screen.getByRole("tab", { name: "Appearance" })).toHaveAttribute("aria-selected", "true")
    expect(window.location.search).toBe("?tab=appearance")

    await userEvent.click(screen.getByRole("tab", { name: "Account" }))
    expect(window.location.pathname + window.location.search).toBe("/settings")
  })

  it("has no AI apps tab without the agent gateway, and a link to it opens Account", async () => {
    route()
    searchParams = new URLSearchParams("tab=ai-apps")
    render(<SettingsPage />)

    expect(screen.queryByRole("tab", { name: "AI apps" })).toBeNull()
    expect(screen.getByRole("tab", { name: "Account" })).toHaveAttribute("aria-selected", "true")

    // The flags arrive after the first render: the link then lands where it pointed.
    act(() => featureFlagsManager.updateFlags({ agentGateway: true }))
    expect(screen.getByRole("tab", { name: "AI apps" })).toHaveAttribute("aria-selected", "true")
    expect(await screen.findByRole("tablist", { name: "AI app" })).toBeInTheDocument()
  })

  it("an unknown ?tab= opens Account", () => {
    route()
    searchParams = new URLSearchParams("tab=nope")
    render(<SettingsPage />)
    expect(screen.getByRole("tablist", { name: "Settings" })).toBeInTheDocument()
    expect(screen.getByRole("tab", { name: "Account" })).toHaveAttribute("aria-selected", "true")
  })
})
