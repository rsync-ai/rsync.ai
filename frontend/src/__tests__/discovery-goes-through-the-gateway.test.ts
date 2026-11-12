import { afterEach, describe, expect, it, vi } from "vitest"

// The browser reaches the tool-generator only through the api-gateway's
// /api/v1/tool-generator proxy, which checks the caller's workspace role and
// stamps the membership-checked X-Workspace-ID (ToolGeneratorProxy). Pointed at
// the service directly, a browser could send any workspace id and the
// tool-generator's egress log would name it. NEXT_PUBLIC_TOOL_GENERATOR_URL
// used to allow exactly that, so it must change nothing.
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

afterEach(() => {
  vi.unstubAllEnvs()
  vi.resetModules()
  vi.clearAllMocks()
})

describe("tool-generator base URL", () => {
  it("is the gateway proxy even when NEXT_PUBLIC_TOOL_GENERATOR_URL is set", async () => {
    vi.stubEnv("NEXT_PUBLIC_TOOL_GENERATOR_URL", "http://localhost:5010")
    vi.resetModules()
    const { TOOLGEN_BASE } = await import("@/lib/api/discovery")
    const { API_GATEWAY_URL } = await import("@/lib/config/api")
    expect(TOOLGEN_BASE).toBe(`${API_GATEWAY_URL}/api/v1/tool-generator`)
  })

  it("sends a discovery call to the gateway proxy", async () => {
    vi.stubEnv("NEXT_PUBLIC_TOOL_GENERATOR_URL", "http://localhost:5010")
    vi.resetModules()
    const { authFetch } = await import("@/lib/api/auth-fetch")
    const mockFetch = authFetch as unknown as ReturnType<typeof vi.fn>
    mockFetch.mockResolvedValue({ ok: true, status: 200, json: async () => ({ session_id: "s-1" }) })
    const { confirmDiscovery } = await import("@/lib/api/discovery")
    const { API_GATEWAY_URL } = await import("@/lib/config/api")
    await confirmDiscovery("session-1", {})
    expect(mockFetch).toHaveBeenCalled()
    const url = String(mockFetch.mock.calls[0][0])
    expect(url.startsWith(`${API_GATEWAY_URL}/api/v1/tool-generator/`)).toBe(true)
    expect(url).not.toContain("5010")
  })
})
