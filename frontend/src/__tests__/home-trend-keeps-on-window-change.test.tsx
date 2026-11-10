/**
 * Home's card trend lines stay put while the charts' window changes.
 *
 * The cards draw the last 7 days, and every window (7 or 30 days) ends today,
 * so either answer draws the same line. The provider cleared its metrics on a
 * window change, so both lines dropped to a placeholder and came back on every
 * 7d/30d click (launch review PR1, reviewer M4). A workspace switch still
 * clears them: the old workspace's line must not show for the new one.
 *
 * Bug class: a value that does not depend on the change being loaded is
 * cleared with the rest while it loads.
 */

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/",
}))
vi.mock("@/lib/api/read-stat", () => ({
  readStatOrUnknown: async (path: string) => {
    if (path.startsWith("/api/v1/pipelines")) return { total: 2, pipelines: [] }
    if (path.startsWith("/api/v1/explorer/schedules")) return { schedules: [] }
    if (path.startsWith("/api/v1/explorer/freshness")) return { breaches: [] }
    if (path.startsWith("/api/v1/workflows")) return { workflows: [] }
    return undefined
  },
}))

const pending: Array<{ days: number; resolve: (m: unknown) => void }> = []
vi.mock("@/lib/api/activity-metrics", async (orig) => ({
  ...(await orig<typeof import("@/lib/api/activity-metrics")>()),
  getActivityMetrics: (days: number) => new Promise((resolve) => pending.push({ days, resolve })),
}))

import { ActivityMetricsProvider, useActivityMetrics } from "@/components/runs/ActivityMetricsProvider"
import { HomeOverview } from "@/components/home/HomeOverview"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

function metrics(days: number) {
  const list = Array.from({ length: days }, (_, i) => `2026-10-${String(i + 1).padStart(2, "0")}`)
  return {
    days: list,
    time_zone: "UTC",
    pipeline_runs: list.map((day) => ({ day, status: "succeeded", count: 1 })),
    pipeline_run_time: [],
    rows_written: [],
    model_runs: list.map((day) => ({ day, status: "succeeded", count: 2 })),
    model_run_time: [],
    checks: [],
  }
}

function ThirtyDays() {
  const shared = useActivityMetrics()!
  return (
    <button type="button" onClick={() => shared.setDays(30)}>
      30d
    </button>
  )
}

const lines = () => screen.queryAllByTestId("sparkline").length

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})
beforeEach(() => {
  pending.length = 0
  window.localStorage.clear()
  setActiveWorkspaceId("ws-a")
})
afterEach(() => window.localStorage.clear())

describe("Home card trend lines", () => {
  it("stay drawn while another window loads, and clear on a workspace switch", async () => {
    render(
      <ActivityMetricsProvider>
        <HomeOverview />
        <ThirtyDays />
      </ActivityMetricsProvider>,
    )
    await screen.findByRole("region", { name: "Pipelines" })
    await waitFor(() => expect(pending).toHaveLength(1))
    await act(async () => pending[0].resolve(metrics(7)))
    await waitFor(() => expect(lines()).toBe(2))

    fireEvent.click(screen.getByRole("button", { name: "30d" }))
    await waitFor(() => expect(pending).toHaveLength(2))
    expect(pending[1].days).toBe(30)
    expect(lines()).toBe(2)

    await act(async () => pending[1].resolve(metrics(30)))
    expect(lines()).toBe(2)

    act(() => setActiveWorkspaceId("ws-b"))
    await screen.findByRole("region", { name: "Pipelines" })
    await waitFor(() => expect(pending).toHaveLength(3))
    expect(lines()).toBe(0)
  })
})
