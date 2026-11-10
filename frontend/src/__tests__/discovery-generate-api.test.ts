import { afterEach, describe, expect, it, vi } from "vitest"
import type { Mock } from "vitest"

import { authFetch } from "@/lib/api/auth-fetch"
import { DiscoveryError, generateFromSession, moveConnectionsToConnectorVersion } from "@/lib/api/discovery"

// Generating a connector the deterministic paths can't build needs an LLM. On an
// install without one, api-gateway (connector_generator.go) answers 503 with
// error="llm_not_configured" and the sentence in error_message. The wizard shows
// the thrown message as is, so it must be the sentence, never the code.
vi.mock("@/lib/api/auth-fetch", () => ({ authFetch: vi.fn() }))

const mockFetch = authFetch as Mock

afterEach(() => vi.clearAllMocks())

function failure(body: unknown, status: number) {
  return { ok: false, status, statusText: "Service Unavailable", json: async () => body }
}

async function thrown(): Promise<DiscoveryError> {
  try {
    await generateFromSession("acme", "session-1")
  } catch (e) {
    return e as DiscoveryError
  }
  throw new Error("generateFromSession did not throw")
}

const SENTENCE = "Set up an LLM first: add OPENAI_API_KEY to .env, then restart rsync.ai."

describe("generateFromSession errors", () => {
  it("shows the sentence, not the code, when no LLM is set up", async () => {
    mockFetch.mockResolvedValue(
      failure(
        { success: false, error: "llm_not_configured", error_message: SENTENCE, error_stage: "llm_not_configured" },
        503,
      ),
    )
    const e = await thrown()
    expect(e).toBeInstanceOf(DiscoveryError)
    expect(e.status).toBe(503)
    expect(e.message).toBe(SENTENCE)
  })

  it("still falls back to error, then detail, when there is no error_message", async () => {
    mockFetch.mockResolvedValue(failure({ error: "generator exploded" }, 500))
    expect((await thrown()).message).toBe("generator exploded")

    mockFetch.mockResolvedValue(failure({ detail: "bad request" }, 400))
    expect((await thrown()).message).toBe("bad request")
  })

  it("uses the status text when the body is not JSON", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 502,
      statusText: "Bad Gateway",
      json: async () => {
        throw new SyntaxError("not json")
      },
    })
    expect((await thrown()).message).toBe("Bad Gateway")
  })
})

// An overwrite rewrites the connector's current version in place; the gateway
// refuses it with 409 while a run uses that version (connector_overwrite.go).
describe("generateFromSession overwrite", () => {
  const ok = { ok: true, status: 200, json: async () => ({ success: true, version: "v1.0.0" }) }
  const sent = () => JSON.parse(String(mockFetch.mock.calls.at(-1)![1].body))

  it("sends overwrite only when asked", async () => {
    mockFetch.mockResolvedValue(ok)
    await generateFromSession("acme", "session-1", { forceRegenerate: true })
    expect(sent()).toMatchObject({ force_regenerate: true, overwrite: false })
    await generateFromSession("acme", "session-1", { forceRegenerate: true, overwrite: true })
    expect(sent()).toMatchObject({ force_regenerate: true, overwrite: true })
  })

  it("throws the gateway's sentence when a run uses the version", async () => {
    const msg = "acme v1.0.0 is in use by a run of Orders sync, so it was not overwritten."
    mockFetch.mockResolvedValue(failure({ success: false, code: "connector_in_use", error: msg, error_message: msg }, 409))
    const e = await thrown()
    expect(e.status).toBe(409)
    expect(e.message).toBe(msg)
  })
})

describe("moveConnectionsToConnectorVersion", () => {
  it("posts the ids and version to the connector's bulk endpoint", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 200, json: async () => ({ success: true, connector_version: "v1.0.1" }) })
    const res = await moveConnectionsToConnectorVersion("acme crm", "v1.0.1", ["c1", "c2"])
    expect(res.connector_version).toBe("v1.0.1")
    const [url, init] = mockFetch.mock.calls[0]
    expect(String(url)).toMatch(/\/api\/v1\/connectors\/acme%20crm\/connections\/version$/)
    expect(init.method).toBe("POST")
    expect(JSON.parse(String(init.body))).toEqual({ version: "v1.0.1", connection_ids: ["c1", "c2"] })
  })

  it("throws the gateway's error, so nothing looks moved", async () => {
    mockFetch.mockResolvedValue(failure({ error: "Nothing was changed." }, 404))
    await expect(moveConnectionsToConnectorVersion("acme", "v1.0.1", ["c1"])).rejects.toMatchObject({
      status: 404,
      message: "Nothing was changed.",
    })
  })
})
