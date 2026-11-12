import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react"

import AdminAuditPage from "@/app/(dashboard)/admin/audit/page"
import AdminExecutionsPage from "@/app/(dashboard)/admin/executions/page"
import AdminHealthPage from "@/app/(dashboard)/admin/health/page"
import AdminInvitationsPage from "@/app/(dashboard)/admin/invitations/page"
import AdminPipelinesPage from "@/app/(dashboard)/admin/pipelines/page"
import AdminUsersPage from "@/app/(dashboard)/admin/users/page"
import { authFetch } from "@/lib/api/auth-fetch"

// An admin page whose read fails says so; it does not draw an empty list.
// Four pages handled 401, 403 and 429 by name and let every other status fall
// through to `res.json()`, which succeeds on the API's `{"error": …}` body: a
// 500 on Users read as "No users found", on Audit as "No audit logs found", on
// Invitations as "No invitations yet", on Health as no services at all.
// Pinned here on the real pages over a stubbed authFetch:
//   - a 500 shows "Failed to load" with the server's reason and a Retry;
//   - Retry reads again and shows the rows once the read succeeds;
//   - typing in a search box keeps it mounted and focused through the reload
//     (it used to be swapped out for a loading card per keystroke);
//   - a slower answer to an earlier keystroke does not replace a newer one's.

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/admin",
}))
vi.mock("@/lib/api/auth-fetch", async (orig) => ({
  ...(await orig<typeof import("@/lib/api/auth-fetch")>()),
  authFetch: vi.fn(),
  redirectToLogout: vi.fn(),
}))
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const mockFetch = authFetch as unknown as Mock

function res(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: { get: () => null },
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response
}

const DOWN = { error: "admin store unavailable" }

const user = (id: string, email: string) => ({
  id,
  email,
  name: email.split("@")[0],
  role: "user",
  status: "active",
  created_at: "2026-10-01T08:00:00Z",
})

beforeAll(() => {
  window.ResizeObserver ||= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
})

beforeEach(() => {
  mockFetch.mockReset()
})

describe("an admin page whose read fails says so", () => {
  it.each([
    ["Users", AdminUsersPage, /no users found/i],
    ["Audit", AdminAuditPage, /no audit logs found/i],
    ["Invitations", AdminInvitationsPage, /no invitations yet/i],
  ])("%s: a 500 shows the reason and a Retry, not an empty list", async (_name, Page, empty) => {
    mockFetch.mockResolvedValue(res(500, DOWN))
    render(<Page />)

    await waitFor(() => expect(screen.getByText("Failed to load")).toBeInTheDocument())
    expect(screen.getByText(DOWN.error)).toBeInTheDocument()
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument()
    expect(screen.queryByText(empty)).not.toBeInTheDocument()
  })

  it("Health: a 500 shows the reason, not a page with no services", async () => {
    mockFetch.mockResolvedValue(res(500, DOWN))
    render(<AdminHealthPage />)

    await waitFor(() => expect(screen.getByText("Failed to load")).toBeInTheDocument())
    expect(screen.getByText(DOWN.error)).toBeInTheDocument()
  })

  it("Retry reads again and shows the rows once the read succeeds", async () => {
    mockFetch
      .mockResolvedValueOnce(res(500, DOWN))
      .mockResolvedValueOnce(res(200, { data: [user("u1", "ann@example.com")], total: 1 }))
    render(<AdminUsersPage />)

    await waitFor(() => expect(screen.getByText("Failed to load")).toBeInTheDocument())
    fireEvent.click(screen.getByRole("button", { name: "Retry" }))

    await waitFor(() => expect(screen.getByText("ann@example.com")).toBeInTheDocument())
    expect(screen.queryByText("Failed to load")).not.toBeInTheDocument()
  })
})

describe("an admin search box survives its own reload", () => {
  const SEARCH = "Search by email or name..."

  it("keeps the same box, focused, while the search reloads", async () => {
    mockFetch.mockImplementation(async (url: string) =>
      res(200, url.includes("q=ann")
        ? { data: [user("u1", "ann@example.com")], total: 1 }
        : { data: [user("u1", "ann@example.com"), user("u2", "bea@example.com")], total: 2 }),
    )
    render(<AdminUsersPage />)
    await waitFor(() => expect(screen.getByText("bea@example.com")).toBeInTheDocument())

    const box = screen.getByPlaceholderText(SEARCH)
    box.focus()
    fireEvent.change(box, { target: { value: "ann" } })

    await waitFor(() => expect(screen.queryByText("bea@example.com")).not.toBeInTheDocument())
    expect(screen.getByPlaceholderText(SEARCH)).toBe(box)
    expect(document.activeElement).toBe(box)
  })

  it("shows the newest search's rows when an older one answers last", async () => {
    const pending: Record<string, (r: Response) => void> = {}
    mockFetch.mockImplementation((url: string) => {
      const q = new URL(url, "http://x").searchParams.get("q") ?? ""
      if (!q) return Promise.resolve(res(200, { data: [user("u0", "zed@example.com")], total: 1 }))
      return new Promise<Response>((resolve) => { pending[q] = resolve })
    })
    render(<AdminUsersPage />)
    await waitFor(() => expect(screen.getByText("zed@example.com")).toBeInTheDocument())

    fireEvent.change(screen.getByPlaceholderText(SEARCH), { target: { value: "a" } })
    await waitFor(() => expect(pending.a).toBeDefined())
    fireEvent.change(screen.getByPlaceholderText(SEARCH), { target: { value: "ab" } })
    await waitFor(() => expect(pending.ab).toBeDefined())

    await act(async () => pending.ab(res(200, { data: [user("u2", "abe@example.com")], total: 1 })))
    await act(async () => pending.a(res(200, { data: [user("u1", "ann@example.com")], total: 1 })))

    expect(screen.getByText("abe@example.com")).toBeInTheDocument()
    expect(screen.queryByText("ann@example.com")).not.toBeInTheDocument()
  })
})

// Executions and Pipelines read once per keystroke too (the query is in their
// `queryString`), so they carry the same guard. Their rows show the pipeline's
// name, which is what these fixtures vary.
describe("an admin search answers with the newest keystroke's rows", () => {
  const row = (id: string, name: string) => ({
    id,
    pipeline_id: id,
    pipeline_name: name,
    name,
    status: "active",
    created_by_email: "owner@example.com",
    created_at: "2026-10-01T08:00:00Z",
    updated_at: "2026-10-01T08:00:00Z",
    start_time: "2026-10-01T08:00:00Z",
  })

  it.each([
    ["Executions", AdminExecutionsPage, "Search id, pipeline or email…"],
    ["Pipelines", AdminPipelinesPage, "Search name, id or email…"],
  ])("%s: an older search answering last does not replace the newer one's rows", async (_name, Page, box) => {
    const pending: Record<string, (r: Response) => void> = {}
    mockFetch.mockImplementation((url: string) => {
      const q = new URL(url, "http://x").searchParams.get("q") ?? ""
      if (!q) return Promise.resolve(res(200, { data: [row("p0", "zed-sync")], total: 1, limit: 50, offset: 0 }))
      return new Promise<Response>((resolve) => { pending[q] = resolve })
    })
    render(<Page />)
    await waitFor(() => expect(screen.getByText("zed-sync")).toBeInTheDocument())

    fireEvent.change(screen.getByPlaceholderText(box), { target: { value: "a" } })
    await waitFor(() => expect(pending.a).toBeDefined())
    fireEvent.change(screen.getByPlaceholderText(box), { target: { value: "ab" } })
    await waitFor(() => expect(pending.ab).toBeDefined())

    await act(async () => pending.ab(res(200, { data: [row("p2", "abe-sync")], total: 1, limit: 50, offset: 0 })))
    await act(async () => pending.a(res(200, { data: [row("p1", "ann-sync")], total: 1, limit: 50, offset: 0 })))

    expect(screen.getByText("abe-sync")).toBeInTheDocument()
    expect(screen.queryByText("ann-sync")).not.toBeInTheDocument()
  })
})
