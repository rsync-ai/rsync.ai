import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen } from "@testing-library/react"
import type { Mock } from "vitest"

import { Header } from "@/components/layout/Header"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { featureFlagsManager } from "@/config/features"
import { useAssistantStore } from "@/lib/store/useAssistantStore"

// The header is `fixed … backdrop-blur-sm`. A backdrop-filter (like filter or
// transform) makes an element the containing block of every `position: fixed`
// descendant, so an overlay rendered inside the header is laid out inside the
// header's 64 px box instead of the viewport. That is how the Ask rsync.ai panel
// shipped 2–7 px tall and see-through (2026-10-01). Overlays opened from the
// header must render outside it: a portal (the panel) or a sibling (the New
// Workspace dialog). This holds the whole class, not just the panel: nothing
// inside <header> may be `fixed`.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
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
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

beforeEach(() => {
  mockFetch.mockReset()
  mockFetch.mockImplementation(async (url: string) => {
    if (url === API_ENDPOINTS.ASSISTANT.MODELS) {
      return { ok: true, status: 200, json: async () => ({ entries: [{ entry: "claude", label: "Claude", model: "m" }], default: "claude" }) }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
  useAssistantStore.setState({ open: false, page: null, pageCleared: false })
  act(() => featureFlagsManager.updateFlags({ assistant: true }))
})

afterEach(() => {
  act(() => useAssistantStore.setState({ open: false }))
  featureFlagsManager.resetFlags()
})

// A `fixed` class token, bare or behind any variant (`sm:fixed`, `md:hover:fixed`).
const FIXED = /(^|\s)([\w-]+:)*fixed(\s|$)/

describe("header overlays", () => {
  it("nothing inside the blurred header is position: fixed, with the assistant panel open", async () => {
    render(<Header />)
    const header = document.querySelector("header")!
    // Controls: the trap exists, and the panel is a fixed overlay, so a pass
    // below means it was rendered elsewhere, not that the probe can't see it.
    expect(header.className).toMatch(/backdrop-blur/)

    fireEvent.click(await screen.findByRole("button", { name: "Ask rsync.ai" }))
    const panel = await screen.findByRole("complementary", { name: "Ask rsync.ai" })
    expect(panel.getAttribute("class")).toMatch(FIXED)

    expect(header.contains(panel)).toBe(false)
    const fixedInside = Array.from(header.querySelectorAll("*"))
      .filter((el) => FIXED.test(el.getAttribute("class") ?? ""))
      .map((el) => el.outerHTML.slice(0, 120))
    expect(fixedInside).toEqual([])
  })
})
