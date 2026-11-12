/**
 * Prod retest 2026-09-18: a CDC pipeline read "Failed" on /pipelines and stayed
 * Failed after it recovered — the list polled only while some row was running, so
 * with the one stream failed nothing ever re-fetched. The page now always polls
 * (fast while a run is in flight, slow otherwise).
 *
 * The page's four count cards are gone: Home has the counts, and "Failed runs"
 * here counted runs where Home's "failed" counts pipelines.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest"
import { act, render, screen, within } from "@testing-library/react"
import "@testing-library/jest-dom"

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: "admin", can: () => true }),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() } }))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

import { authFetch } from "@/lib/api/auth-fetch"
import { PipelinesPageClient, pipelinesPollInterval } from "@/app/(dashboard)/pipelines/PipelinesPageClient"
import type { PipelineListItem } from "@/components/pipeline/PipelinesTable"

const mockAuthFetch = vi.mocked(authFetch)

function row(derived_status: string): PipelineListItem {
  return {
    id: "p1",
    name: "orders-sync",
    pipeline_status: "active",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    derived_status,
    sync_mode: "cdc",
  }
}

function json(body: unknown): Response {
  return { ok: true, status: 200, json: async () => body } as unknown as Response
}

let listStatus = "failed"

function calls(fragment: string): number {
  return mockAuthFetch.mock.calls.filter(([url]) => String(url).includes(fragment)).length
}

describe("pipelinesPollInterval", () => {
  it("polls fast while a run is in flight and slowly otherwise — never off", () => {
    expect(pipelinesPollInterval([row("running")])).toBe(5_000)
    expect(pipelinesPollInterval([row("waiting_for_user")])).toBe(5_000)
    expect(pipelinesPollInterval([row("failed")])).toBe(30_000)
    expect(pipelinesPollInterval([])).toBe(30_000)
  })
})

describe("/pipelines — a Failed row is not frozen", () => {
  beforeEach(() => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    listStatus = "failed"
    mockAuthFetch.mockReset()
    mockAuthFetch.mockImplementation(async () => json({ pipelines: [row(listStatus)], total: 1 }))
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it("re-fetches the list with nothing running, so recovery shows", async () => {
    render(<PipelinesPageClient />)
    const table = await screen.findByRole("table")
    expect(await within(table).findByText(/^failed$/i)).toBeInTheDocument()
    const listBefore = calls("/api/v1/pipelines?")

    // The stream recovers out of band (healer / Reload / another tab).
    listStatus = "running"
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })

    expect(calls("/api/v1/pipelines?")).toBeGreaterThan(listBefore)
    expect(await within(table).findByText(/^running$/i)).toBeInTheDocument()
    expect(within(table).queryByText(/^failed$/i)).not.toBeInTheDocument()
  })

  it("shows no count cards and reads no stats; Home has the counts", async () => {
    render(<PipelinesPageClient />)
    await screen.findByRole("table")
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })

    expect(calls("/api/v1/pipelines?")).toBeGreaterThan(0)
    expect(calls("/pipelines/stats")).toBe(0)
    for (const label of ["Running", "Successful runs", "Failed runs"]) {
      expect(screen.queryByText(label)).not.toBeInTheDocument()
    }
  })
})
