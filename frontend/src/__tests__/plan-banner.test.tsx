import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { render, screen, fireEvent, waitFor } from "@testing-library/react"
import type { Mock } from "vitest"

import { PlanBanner, bannerFromPlan } from "@/components/layout/PlanBanner"
import { authFetch } from "@/lib/api/auth-fetch"
import { notifyPipelinesChanged } from "@/lib/plan/plan-events"

// The banner's "Upgrade to Pro" used to be a bare mailto anchor — a dead click on
// machines with no mail client. It now opens the in-app UpgradeModal. These tests
// pin: (1) the free-plan banner renders, (2) clicking the CTA opens the contact
// dialog (not a silent mailto), (3) a Pro user sees no banner.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false
  if (!Element.prototype.scrollIntoView) Element.prototype.scrollIntoView = () => {}
})

beforeEach(() => vi.clearAllMocks())
afterEach(() => vi.restoreAllMocks())

function mockPlan(plan: Record<string, unknown>) {
  ;(authFetch as Mock).mockImplementation(async (url: string) => {
    if (String(url).endsWith("/usage/plan")) {
      return { ok: true, status: 200, json: async () => plan }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
}

describe("PlanBanner — Upgrade to Pro", () => {
  it("shows the free-plan banner and opens the contact dialog on click", async () => {
    mockPlan({ plan: "free", pipelines_limit: 2, pipelines_used: 1 })
    render(<PlanBanner />)

    expect(await screen.findByText(/Free plan — 1\/2 pipelines used/)).toBeInTheDocument()

    // The CTA is a real button (not a bare mailto anchor).
    fireEvent.click(screen.getByRole("button", { name: /upgrade to pro/i }))

    // Clicking surfaces the in-app dialog with a clear contact-the-team path.
    expect(await screen.findByText(/contact the rsync.ai team/i)).toBeInTheDocument()
    expect(screen.getByText("sales@rsync.ai")).toBeInTheDocument()
  })

  it("renders nothing for a pro plan", async () => {
    mockPlan({ plan: "pro" })
    const { container } = render(<PlanBanner />)
    await waitFor(() => expect(authFetch).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()
  })
})

// Issues #7/#21: the banner undercounted (0/2 with one pipeline running) because
// it read /auth/me once when the persistent layout mounted. It now reads the
// active-workspace meter (/api/v1/usage/plan, the create gate's own count) and
// re-reads when a pipeline is created or deleted, or the tab regains focus.
describe("PlanBanner — pipeline meter stays current", () => {
  it("reads the active-workspace meter, not /auth/me", async () => {
    mockPlan({ plan: "free", pipelines_limit: 2, pipelines_used: 1 })
    render(<PlanBanner />)
    expect(await screen.findByText(/Free plan — 1\/2 pipelines used/)).toBeInTheDocument()
    const urls = (authFetch as Mock).mock.calls.map((c) => String(c[0]))
    expect(urls).toContain("/api/v1/usage/plan")
    expect(urls).not.toContain("/api/v1/auth/me")
  })

  it("refetches after a pipeline is created and on focus", async () => {
    let used = 0
    ;(authFetch as Mock).mockImplementation(async (url: string) => {
      if (String(url).endsWith("/usage/plan")) {
        return { ok: true, status: 200, json: async () => ({ plan: "free", pipelines_limit: 2, pipelines_used: used }) }
      }
      return { ok: true, status: 200, json: async () => ({}) }
    })
    const { container } = render(<PlanBanner />)
    // Two left: the Usage page has the meter, the banner stays out of the way.
    await waitFor(() => expect(authFetch).toHaveBeenCalled())
    expect(container).toBeEmptyDOMElement()

    used = 1
    notifyPipelinesChanged()
    expect(await screen.findByText(/Free plan — 1\/2 pipelines used/)).toBeInTheDocument()

    used = 2
    window.dispatchEvent(new Event("focus"))
    expect(await screen.findByText(/Free plan — 2\/2 pipelines used/)).toBeInTheDocument()
  })

  // Coming back to a tab fires both "visibilitychange" and "focus"; each used to
  // start its own read of the same meter.
  it("reads once when the tab comes back, not once per event", async () => {
    mockPlan({ plan: "free", pipelines_limit: 2, pipelines_used: 1 })
    render(<PlanBanner />)
    await screen.findByText(/Free plan — 1\/2 pipelines used/)
    const reads = () => (authFetch as Mock).mock.calls.filter((c) => String(c[0]).endsWith("/usage/plan")).length
    const before = reads()

    document.dispatchEvent(new Event("visibilitychange"))
    window.dispatchEvent(new Event("focus"))
    await waitFor(() => expect(reads()).toBe(before + 1))
    await new Promise((r) => setTimeout(r, 20))
    expect(reads()).toBe(before + 1)
  })

  it("falls back to /auth/me when the gateway has no /usage/plan route", async () => {
    ;(authFetch as Mock).mockImplementation(async (url: string) => {
      if (String(url).endsWith("/usage/plan")) return { ok: false, status: 404, json: async () => ({}) }
      if (String(url).endsWith("/auth/me")) {
        return { ok: true, status: 200, json: async () => ({ plan: "free", pipelines_limit: 2, pipelines_used: 2 }) }
      }
      return { ok: true, status: 200, json: async () => ({}) }
    })
    render(<PlanBanner />)
    expect(await screen.findByText(/Free plan — 2\/2 pipelines used/)).toBeInTheDocument()
  })
})

// The Usage page owns the "x/y pipelines used" meter; the banner on every page
// repeated it even with room to spare. It now shows only near the cap.
describe("PlanBanner — the meter shows only near the cap", () => {
  it.each([
    ["free", 0, 2, false],
    ["free", 1, 2, true],
    ["free", 2, 2, true],
    ["free", 3, 5, false],
    ["free", 4, 5, true],
    ["starter", 4, 5, false],
    ["starter", 5, 5, true],
  ])("%s plan, %i of %i used → banner %s", (plan, used, limit, shown) => {
    const banner = bannerFromPlan({ plan, pipelines_used: used, pipelines_limit: limit })
    expect(banner !== null).toBe(shown)
    if (shown) expect(banner).toMatchObject({ variant: "plan_limit", used, limit })
  })

  it("control: a trial still shows its days left, and an unlimited plan shows nothing", () => {
    const ends = new Date(Date.now() + 3 * 86_400_000).toISOString()
    expect(bannerFromPlan({ plan: "trial", trial_ends_at: ends })).toMatchObject({ variant: "trial", daysLeft: 3 })
    expect(bannerFromPlan({ plan: "free", pipelines_used: 9, pipelines_limit: null })).toBeNull()
  })
})
