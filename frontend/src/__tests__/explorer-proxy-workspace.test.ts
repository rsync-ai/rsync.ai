import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

// P5b review gap: the /proxy/explorer/* server routes proxy to WORKSPACE-SCOPED
// api-gateway endpoints (explorer.go: connections WHERE id=$1 AND workspace_id=$2,
// scoped by activeWorkspaceID(c)). They forward auth via a `Cookie: auth_token`
// header but DROPPED the rsync_active_workspace_id selection, so an export
// 404'd ("Connection not found") for any connection living in a shared
// (non-personal) workspace. These reproducers pin that the upstream Cookie header
// now carries BOTH auth_token AND the active-workspace selection, merged into
// a single header (a naive spread of activeWorkspaceCookieHeader's `cookie` key
// would clobber auth_token).
//
// The route read and forwarded `session_token`, a name the gateway never sets
// (it sets `auth_token`), and sent no CSRF token to a POST the gateway guards, so
// every export answered 401 or 403; these tests used the same invented name.
// proxy-cookie-names.test.ts now checks every name against the gateway's source.

// Mutable cookie jar shared with the next/headers mock (hoisted above imports).
const hoisted = vi.hoisted(() => ({ jar: {} as Record<string, string> }))

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) =>
      name in hoisted.jar ? { name, value: hoisted.jar[name] } : undefined,
  }),
}))

// The route handler reads request.json() and the CSRF header; a minimal stub avoids
// depending on a global Request implementation in the test environment.
function makeReq(body: unknown, headers: Record<string, string> = { "X-CSRF-Token": "csrf-1" }) {
  return { json: async () => body, headers: new Headers(headers) } as unknown as Request
}

function upstreamCookieHeader(fetchMock: ReturnType<typeof vi.fn>): string {
  const init = fetchMock.mock.calls[0]?.[1] as RequestInit | undefined
  const headers = (init?.headers ?? {}) as Record<string, string>
  return headers.Cookie ?? ""
}

beforeEach(() => {
  for (const k of Object.keys(hoisted.jar)) delete hoisted.jar[k]
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe("/proxy/explorer/export forwards the active-workspace selection", () => {
  function stubFetch() {
    const f = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      text: async () => JSON.stringify({ columns: ["a"], rows: [{ a: 1 }] }),
    })
    vi.stubGlobal("fetch", f)
    return f
  }

  const query = { connection_id: "c1", sql: "SELECT 1", format: "csv" }

  it("forwards auth_token, the CSRF pair and rsync_active_workspace_id in one upstream Cookie", async () => {
    hoisted.jar.auth_token = "sess-1"
    hoisted.jar.csrf_token = "csrf-1"
    hoisted.jar.rsync_active_workspace_id = "ws-1"
    const f = stubFetch()

    const { POST } = await import("@/app/proxy/explorer/export/route")
    const res = await POST(makeReq(query))

    expect(res.status).toBe(200)
    expect(upstreamCookieHeader(f)).toBe("auth_token=sess-1; csrf_token=csrf-1; rsync_active_workspace_id=ws-1")
    const headers = (f.mock.calls[0]?.[1] as RequestInit).headers as Record<string, string>
    expect(headers["X-CSRF-Token"]).toBe("csrf-1")
  })

  it("still forwards auth_token when no workspace is selected", async () => {
    hoisted.jar.auth_token = "sess-1"
    hoisted.jar.csrf_token = "csrf-1"
    const f = stubFetch()

    const { POST } = await import("@/app/proxy/explorer/export/route")
    await POST(makeReq(query))

    const cookie = upstreamCookieHeader(f)
    expect(cookie).toBe("auth_token=sess-1; csrf_token=csrf-1")
    expect(cookie).not.toContain("rsync_active_workspace_id")
  })

  it("refuses a viewer who is not signed in, before asking the gateway", async () => {
    hoisted.jar.csrf_token = "csrf-1"
    const f = stubFetch()

    const { POST } = await import("@/app/proxy/explorer/export/route")
    expect((await POST(makeReq(query))).status).toBe(401)
    expect(f).not.toHaveBeenCalled()
  })

  it("refuses a request without the CSRF header, or with the wrong one, before asking the gateway", async () => {
    hoisted.jar.auth_token = "sess-1"
    hoisted.jar.csrf_token = "csrf-1"
    const f = stubFetch()

    const { POST } = await import("@/app/proxy/explorer/export/route")
    expect((await POST(makeReq(query, {}))).status).toBe(403)
    expect((await POST(makeReq(query, { "X-CSRF-Token": "someone-elses" }))).status).toBe(403)
    delete hoisted.jar.csrf_token
    expect((await POST(makeReq(query))).status).toBe(403)
    expect(f).not.toHaveBeenCalled()
  })
})
