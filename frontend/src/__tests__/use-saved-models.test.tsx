import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, renderHook, waitFor } from "@testing-library/react"

import { authFetch } from "@/lib/api/auth-fetch"
import { useSavedModels } from "@/lib/hooks/useSavedModels"

// The Lineage page's saved queries (GET /explorer/saved): each model's last run on its
// card, and the row its Schedule dialog opens with. Context for a page that works
// without it, so a failed read says nothing, and never another workspace's rows.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))
const workspace = vi.hoisted(() => ({ id: "w-1" }))
vi.mock("@/contexts/WorkspaceContext", () => ({ useWorkspace: () => ({ activeWorkspace: { id: workspace.id } }) }))

const mockFetch = authFetch as unknown as Mock

function res(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, headers: { get: () => null }, json: async () => body } as unknown as Response
}

const ROWS = [
  { id: "m-1", name: "daily_revenue", materialization: "table", last_run_status: "success" },
  { id: "m-2", name: "weekly_rollup", materialization: "view" },
]

beforeEach(() => {
  mockFetch.mockReset()
  workspace.id = "w-1"
})

describe("useSavedModels", () => {
  it("keys the workspace's saved queries by id, and reads them again on asking", async () => {
    mockFetch.mockResolvedValue(res(200, { saved_queries: ROWS }))
    const { result } = renderHook(() => useSavedModels(0))
    await waitFor(() => expect([...result.current.saved.keys()]).toEqual(["m-1", "m-2"]))
    expect(result.current.saved.get("m-1")!.last_run_status).toBe("success")
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/explorer/saved", { cache: "no-store" })

    mockFetch.mockResolvedValue(res(200, { saved_queries: [ROWS[1]] }))
    act(() => result.current.reload())
    await waitFor(() => expect([...result.current.saved.keys()]).toEqual(["m-2"]))
    expect(mockFetch).toHaveBeenCalledTimes(2)
  })

  it("keeps what it had when a read fails, and says nothing", async () => {
    mockFetch.mockResolvedValue(res(200, { saved_queries: ROWS }))
    const { result } = renderHook(() => useSavedModels(0))
    await waitFor(() => expect(result.current.saved.size).toBe(2))

    mockFetch.mockResolvedValueOnce(res(500, { error: "boom" }))
    act(() => result.current.reload())
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2))
    mockFetch.mockRejectedValueOnce(new Error("offline"))
    act(() => result.current.reload())
    await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(3))
    expect(result.current.saved.size).toBe(2)
  })

  it("never shows one workspace's rows under another's", async () => {
    mockFetch.mockResolvedValue(res(200, { saved_queries: ROWS }))
    const { result, rerender } = renderHook(() => useSavedModels(0))
    await waitFor(() => expect(result.current.saved.size).toBe(2))

    // The switch: the old rows go at once, before the new workspace's answer lands.
    let answer: (r: Response) => void = () => {}
    mockFetch.mockReturnValueOnce(new Promise<Response>((resolve) => (answer = resolve)))
    workspace.id = "w-2"
    rerender()
    expect(result.current.saved.size).toBe(0)
    await act(async () => answer(res(200, { saved_queries: [ROWS[0]] })))
    await waitFor(() => expect([...result.current.saved.keys()]).toEqual(["m-1"]))
  })
})
