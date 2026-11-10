import { beforeEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"

import { adminListUsers, adminRead, adminUpdateUserRole, validateInvite } from "@/lib/api/admin"
import { authFetch, redirectToLogout } from "@/lib/api/auth-fetch"

// adminRead is the one GET every admin page makes; each page draws a state from
// the way it failed. Pinned here: which failure each status becomes, that a 401
// signs out once, and that a body which is not JSON is a failure, not data.

vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn(), authPost: vi.fn(), redirectToLogout: vi.fn() }))

const mockFetch = authFetch as unknown as Mock

function res(status: number, body: string, headers: Record<string, string> = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: { get: (k: string) => headers[k] ?? null },
    json: async () => JSON.parse(body),
    text: async () => body,
  } as unknown as Response
}

beforeEach(() => {
  mockFetch.mockReset()
  ;(redirectToLogout as Mock).mockReset()
})

describe("adminRead", () => {
  it("returns the body of a good read", async () => {
    mockFetch.mockResolvedValue(res(200, '{"total":3}'))
    expect(await adminRead("/api/v1/admin/x")).toEqual({ ok: true, data: { total: 3 } })
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/admin/x", { method: "GET" })
  })

  it("carries the server's reason on a 500", async () => {
    mockFetch.mockResolvedValue(res(500, '{"error":"admin store unavailable"}'))
    expect(await adminRead("/x")).toEqual({ ok: false, status: 500, retryAfter: null, error: "admin store unavailable" })
  })

  it("names the status when the server gave no reason", async () => {
    mockFetch.mockResolvedValue(res(502, ""))
    expect(await adminRead("/x")).toMatchObject({ ok: false, status: 502, error: "Request failed (HTTP 502)" })
  })

  it("reads Retry-After on a 429, and only there", async () => {
    mockFetch.mockResolvedValueOnce(res(429, "", { "Retry-After": "12" }))
    expect(await adminRead("/x")).toMatchObject({ status: 429, retryAfter: 12 })
    mockFetch.mockResolvedValueOnce(res(503, "", { "Retry-After": "12" }))
    expect(await adminRead("/x")).toMatchObject({ status: 503, retryAfter: null })
    mockFetch.mockResolvedValueOnce(res(429, ""))
    expect(await adminRead("/x")).toMatchObject({ status: 429, retryAfter: null })
  })

  it("signs out on a 401, and says 403 as a status", async () => {
    mockFetch.mockResolvedValueOnce(res(401, ""))
    expect(await adminRead("/x")).toMatchObject({ ok: false, status: 401 })
    expect(redirectToLogout).toHaveBeenCalledTimes(1)

    mockFetch.mockResolvedValueOnce(res(403, '{"error":"forbidden"}'))
    expect(await adminRead("/x")).toMatchObject({ ok: false, status: 403 })
    expect(redirectToLogout).toHaveBeenCalledTimes(1)
  })

  it("is status 0 when the request never got an answer", async () => {
    mockFetch.mockRejectedValue(new TypeError("Failed to fetch"))
    expect(await adminRead("/x")).toEqual({ ok: false, status: 0, retryAfter: null, error: "Network error" })
  })

  it("does not take a 200 whose body is not JSON as data", async () => {
    mockFetch.mockResolvedValue(res(200, "<html>proxy login</html>"))
    expect(await adminRead("/x")).toMatchObject({ ok: false, status: 200, error: "The response was not JSON" })
  })

  it("passes a POST through with its body", async () => {
    mockFetch.mockResolvedValue(res(200, "{}"))
    await adminRead("/x", { method: "POST", body: '{"a":1}' })
    expect(mockFetch).toHaveBeenCalledWith("/x", { method: "POST", body: '{"a":1}' })
  })
})

describe("the admin helpers around it", () => {
  it("leave empty filters out of the query string", async () => {
    mockFetch.mockResolvedValue(res(200, '{"data":[],"total":0}'))
    await adminListUsers({ limit: 50, offset: 0, q: "ann lee", role: "", status: "" })
    expect(mockFetch.mock.calls[0][0]).toBe("/api/v1/admin/users?limit=50&q=ann+lee")
  })

  it("throw a mutation's server reason as the error message", async () => {
    mockFetch.mockResolvedValue(res(409, '{"error":"cannot demote the last admin"}'))
    await expect(adminUpdateUserRole("u1", "user")).rejects.toThrow("cannot demote the last admin")
  })

  it("validate an invite token without a session, and escape it", async () => {
    mockFetch.mockResolvedValue(res(200, '{"valid":false,"reason":"expired"}'))
    expect(await validateInvite("a/b?c")).toEqual({ valid: false, reason: "expired" })
    expect(mockFetch).toHaveBeenCalledWith("/api/v1/auth/invite/a%2Fb%3Fc", { method: "GET", skipAuth: true })
  })
})
