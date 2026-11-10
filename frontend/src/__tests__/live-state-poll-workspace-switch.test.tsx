import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, renderHook, waitFor } from "@testing-library/react"

import { useOpenFreshnessBreaches, useRunningModels } from "@/components/explorer/ModelLiveState"
import { authFetch } from "@/lib/api/auth-fetch"
import { setActiveWorkspaceId } from "@/lib/workspace/active-workspace"

// The Scheduled Queries live-state polls (useVisiblePoll) across a workspace
// switch. Bug class: a polled answer asked in one workspace that lands after a
// switch to another, and is shown there — Rebuilding or Overdue from a tenant
// the page no longer shows. The guard lives in useVisiblePoll, so every poll
// built on it drops such an answer, and the next ask is for the new workspace
// rather than a wait on the old one.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as unknown as Mock

function res(body: unknown) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body } as unknown as Response
}

const RUNNING_ANSWER = {
  models: [{ saved_query_id: "m-a", name: "a_model", workflow_id: "model-refresh-m-a", state: "idle", detail_available: false }],
  count: 1,
  limit: 50,
  temporal_available: true,
}
const FRESHNESS_ANSWER = {
  breaches: [
    {
      breach_id: "b-m-a",
      saved_query_id: "m-a",
      name: "a_model",
      target_table: "analytics.a_model",
      deadline_seconds: 3600,
      cause: "overdue",
      reference_at: "2026-09-15T10:00:00Z",
      never_succeeded: false,
      stale_seconds: 600,
      stale_seconds_now: 7200,
      detected_at: "2026-09-15T11:10:00Z",
    },
  ],
  count: 1,
}

/** What a test reads of either poll. */
interface Poll {
  name: string
  answer: unknown
  use: () => { state: { status: string }; refresh: () => Promise<void> }
}

const polls: Poll[] = [
  {
    name: "live state",
    answer: RUNNING_ANSWER,
    use: () => {
      const { running, refresh } = useRunningModels(true)
      return { state: running, refresh }
    },
  },
  {
    name: "freshness",
    answer: FRESHNESS_ANSWER,
    use: () => {
      const { freshness, refresh } = useOpenFreshnessBreaches(true)
      return { state: freshness, refresh }
    },
  },
]

/** Each ask gets the next reply; an unanswered one waits until its resolver is called. */
function asks() {
  const pending: Array<(r: Response) => void> = []
  mockFetch.mockImplementation(() => new Promise<Response>((resolve) => pending.push(resolve)))
  return pending
}

beforeAll(() => {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" })
})

afterAll(() => {
  delete (document as unknown as Record<string, unknown>).visibilityState
})

beforeEach(() => {
  mockFetch.mockReset()
  window.localStorage.clear()
  setActiveWorkspaceId("ws-a")
})

describe.each(polls)("$name poll across a workspace switch", ({ answer, use }) => {
  it("drops an answer asked in the workspace the page has left", async () => {
    const pending = asks()
    const { result } = renderHook(use)
    await waitFor(() => expect(pending).toHaveLength(1))

    act(() => setActiveWorkspaceId("ws-b"))
    await act(async () => pending[0](res(answer)))

    expect(result.current.state.status).toBe("loading")
  })

  // Control: the same answer, with no switch, is shown.
  it("shows the answer when the workspace has not changed", async () => {
    const pending = asks()
    const { result } = renderHook(use)
    await waitFor(() => expect(pending).toHaveLength(1))

    await act(async () => pending[0](res(answer)))

    expect(result.current.state.status).toBe("ok")
  })

  it("asks again for the new workspace instead of waiting on the old ask", async () => {
    const pending = asks()
    const { result } = renderHook(use)
    await waitFor(() => expect(pending).toHaveLength(1))

    act(() => setActiveWorkspaceId("ws-b"))
    let refreshed!: Promise<void>
    act(() => {
      refreshed = result.current.refresh()
    })
    expect(pending).toHaveLength(2)

    await act(async () => {
      pending[1](res(answer))
      await refreshed
    })
    const shown = result.current.state
    expect(shown.status).toBe("ok")

    // The old ask landing last changes nothing.
    await act(async () => pending[0](res(answer)))
    expect(result.current.state).toBe(shown)
  })
})
