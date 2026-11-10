/**
 * Usage meters on an unlimited plan (PR1 of the launch review).
 *
 * A limit of null means the plan has none, but each meter drew it as
 * `<Progress value={1} max={1}>`: a full bar, which reads as "you are out". An
 * unlimited meter is now an empty track that says "Unlimited"; a limited one
 * still fills to its share.
 */

import { describe, it, expect, vi, beforeEach } from "vitest"
import { render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("@/config/features", () => ({ useUsagePanelState: () => "on" }))
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }))

const authFetch = vi.fn()
vi.mock("@/lib/api/auth-fetch", () => ({
  authFetch: (...a: unknown[]) => authFetch(...a),
}))

import UsagePage from "@/app/(dashboard)/usage/page"

function usage(limits: { pipelines: number | null; gb: number | null; queries: number | null }) {
  return {
    workspace_id: "w-1",
    plan: limits.pipelines == null ? "pro" : "starter",
    pipeline_limit: limits.pipelines,
    pipelines_used: 14,
    can_run: true,
    blocked: false,
    plan_expires_at: null,
    queries_used: 30,
    queries_limit: limits.queries,
    transfer: {
      rows_read: 0,
      rows_written: 0,
      records_processed: 0,
      cdc_events_applied: 0,
      gb: 2.5,
      gb_metered: true,
      gb_limit: limits.gb,
    },
    pipelines: [],
    retention_enabled: false,
    retention_days: 0,
  }
}

function serve(body: unknown) {
  authFetch.mockResolvedValue({ ok: true, status: 200, json: async () => body, text: async () => "" })
}

/** The meter under a label: its label row's parent holds the bar. */
function meter(label: string): HTMLElement {
  return screen.getByText(label).parentElement!.parentElement as HTMLElement
}

beforeEach(() => {
  authFetch.mockReset()
})

describe("Usage meters", () => {
  it("draws an unlimited limit as an empty track that says Unlimited", async () => {
    serve(usage({ pipelines: null, gb: null, queries: null }))
    render(<UsagePage />)
    await screen.findByText("Pipelines")

    for (const [label, used] of [
      ["Pipelines", "14 used"],
      ["Data transfer (this month)", "2.50 GB used"],
      ["NL→SQL queries (this month)", "30 used"],
    ]) {
      const m = meter(label)
      expect(m).toHaveTextContent(`${used} · Unlimited`)
      expect(within(m).queryByRole("progressbar")).not.toBeInTheDocument()
      const track = within(m).getByRole("img", { name: "No limit" })
      expect(track.children).toHaveLength(0)
    }
    expect(screen.queryByText(/· unlimited/)).not.toBeInTheDocument()
  })

  it("still fills a limited meter to its share", async () => {
    serve(usage({ pipelines: 28, gb: 10, queries: 120 }))
    render(<UsagePage />)
    await screen.findByText("Pipelines")

    for (const [label, pct] of [
      ["Pipelines", "50%"],
      ["Data transfer (this month)", "25%"],
      ["NL→SQL queries (this month)", "25%"],
    ]) {
      const bar = within(meter(label)).getByRole("progressbar")
      expect((bar.firstElementChild as HTMLElement).style.width).toBe(pct)
    }
    expect(screen.queryByRole("img", { name: "No limit" })).not.toBeInTheDocument()
    expect(screen.queryByText(/Unlimited/)).not.toBeInTheDocument()
  })
})
