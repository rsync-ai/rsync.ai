import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, render, screen } from "@testing-library/react"

import ScheduledQueriesPage from "@/app/(dashboard)/models/page"
import { authFetch } from "@/lib/api/auth-fetch"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// The Models list across a workspace switch. Pinned here:
//   - a switch empties the list and asks again, because this is a client page and
//     nothing else re-reads it;
//   - a response from the workspace switched away from is dropped even when it
//     lands last, so one workspace's schedules never show under another's;
//   - while the new workspace's list is still out, the page keeps loading rather
//     than saying the workspace has nothing scheduled.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/models",
}))
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))
// The layer list is its own request (useWorkspaceLayers); these tests are not about layers.
vi.mock("@/lib/hooks/useWorkspaceLayers", () => ({
  useWorkspaceLayers: () => ({ layers: null, canManage: false, error: null, reload: async () => {} }),
}))
vi.mock("@/contexts/WorkspaceContext", () => ({
  useWorkspaceRole: () => ({ role: "admin", can: () => true }),
}))
vi.mock("@/contexts/CurrentUserContext", () => ({
  useCurrentUser: () => ({ user: { id: "u-1" } }),
}))
vi.mock("@/components/explorer/SavedQueryEditDialog", () => ({ SavedQueryEditDialog: () => null }))
vi.mock("@/components/explorer/SavedQueryModelDialog", () => ({ SavedQueryModelDialog: () => null }))

const mockFetch = authFetch as unknown as Mock
const LIST = "/api/v1/explorer/schedules"

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response
}

function schedule(id: string, name: string) {
  return {
    schedule_id: `sch-${id}`,
    saved_query_id: id,
    name,
    connection_id: "c-1",
    schedule_type: "cron",
    schedule_spec: { cron: "0 2 * * *", timezone: "UTC" },
    status: "active",
    materialization: "table",
    target_table: `analytics.${name}`,
    statement_class: "select",
    supports_materialization: true,
    created_by: "u-1",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
  }
}

const A = res(200, { schedules: [schedule("a-1", "workspace_a_rollup")] })
const B = res(200, { schedules: [schedule("b-1", "workspace_b_rollup")] })

// The list answers come from `answers`, in order; the live-state polls get empty answers.
let answers: Array<() => Promise<Response>>

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
  setActiveWorkspaceId("ws-a")
  answers = []
  mockFetch.mockImplementation((url: string) => {
    if (url === LIST) {
      const next = answers.shift()
      if (!next) return Promise.reject(new Error("no answer left for the list"))
      return next()
    }
    if (url === "/api/v1/explorer/running") {
      return Promise.resolve(res(200, { models: [], count: 0, limit: 50, temporal_available: true }))
    }
    if (url === "/api/v1/explorer/freshness") return Promise.resolve(res(200, { breaches: [], count: 0 }))
    return Promise.reject(new Error(`unexpected request ${url}`))
  })
})

afterEach(() => setActiveWorkspaceId(null))

function deferred() {
  let resolve!: (r: Response) => void
  const promise = new Promise<Response>((r) => (resolve = r))
  return { answer: () => promise, resolve }
}

function listCalls() {
  return mockFetch.mock.calls.filter(([url]) => url === LIST).length
}

describe("Models — workspace switch", () => {
  it("asks again for the new workspace's schedules on a switch", async () => {
    answers.push(() => Promise.resolve(A), () => Promise.resolve(B))
    render(<ScheduledQueriesPage />)
    await screen.findByText("workspace_a_rollup")

    act(() => setActiveWorkspaceId("ws-b"))
    expect(await screen.findByText("workspace_b_rollup")).toBeInTheDocument()
    expect(screen.queryByText("workspace_a_rollup")).toBeNull()
    expect(listCalls()).toBe(2)
  })

  it("drops the old workspace's answer when it lands after the switch", async () => {
    const first = deferred()
    answers.push(first.answer, () => Promise.resolve(B))
    render(<ScheduledQueriesPage />)

    act(() => setActiveWorkspaceId("ws-b"))
    await screen.findByText("workspace_b_rollup")
    await act(async () => first.resolve(A))
    expect(screen.queryByText("workspace_a_rollup")).toBeNull()
    expect(screen.getByText("workspace_b_rollup")).toBeInTheDocument()
  })

  it("keeps loading, not 'nothing scheduled', while the new workspace's list is out", async () => {
    const first = deferred()
    const second = deferred()
    answers.push(first.answer, second.answer)
    render(<ScheduledQueriesPage />)

    act(() => setActiveWorkspaceId("ws-b"))
    await act(async () => first.resolve(A))
    expect(screen.getByText("Loading models…")).toBeInTheDocument()
    expect(screen.queryByText("workspace_a_rollup")).toBeNull()

    await act(async () => second.resolve(B))
    expect(await screen.findByText("workspace_b_rollup")).toBeInTheDocument()
    expect(screen.queryByText("Loading models…")).toBeNull()
  })
})
