import { readFileSync, readdirSync } from "node:fs"
import { join } from "node:path"

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

// instrumentation.ts re-exports Sentry's request hook and register() loads the
// Sentry server config; neither matters to the settings check, so stub both.
vi.mock("@sentry/nextjs", () => ({ captureRequestError: vi.fn() }))
vi.mock("../../sentry.server.config", () => ({}))
vi.mock("../../sentry.edge.config", () => ({}))

import { frontendStartupProblems, register } from "@/instrumentation"
import { GET as healthGET } from "@/app/api/health/route"

const GATEWAY_URL_VALUE = "http://unit-test-gateway.internal:8080"
const SECRET_VALUE = "unit-test-internal-secret-frontend-startup"

const SETTINGS = ["API_GATEWAY_INTERNAL_URL", "INTERNAL_SERVICE_SECRET"] as const

function productionEnv(overrides: Record<string, string | undefined> = {}) {
  return {
    NODE_ENV: "production",
    API_GATEWAY_INTERNAL_URL: GATEWAY_URL_VALUE,
    INTERNAL_SERVICE_SECRET: SECRET_VALUE,
    ...overrides,
  }
}

function expectNoValueLeak(text: string) {
  for (const value of [GATEWAY_URL_VALUE, SECRET_VALUE]) {
    expect(text).not.toContain(value)
  }
}

describe("frontendStartupProblems", () => {
  it("names exactly the one missing setting, says what will not work and what to set", () => {
    const expected: Record<(typeof SETTINGS)[number], { consequence: string[]; remediation: string[] }> = {
      API_GATEWAY_INTERNAL_URL: {
        consequence: ["http://localhost:5001", "explorer queries", "only works when the gateway runs on the same host"],
        remediation: ["Set it to the gateway's in-network address, for example http://api-gateway:8080."],
      },
      INTERNAL_SERVICE_SECRET: {
        consequence: ["refuses api-gateway's requests", "chart step fails", "goes without its PDF"],
        remediation: [
          "same INTERNAL_SERVICE_SECRET as api-gateway, orchestrator and temporal-adapter",
          "openssl rand -hex 32",
        ],
      },
    }
    let checked = 0
    for (const setting of SETTINGS) {
      for (const missing of [undefined, "", "   "]) {
        const problems = frontendStartupProblems(productionEnv({ [setting]: missing }))
        expect(problems.map((p) => p.setting)).toEqual([setting])
        expect(problems[0].message.startsWith(`${setting} `)).toBe(true)
        for (const text of [...expected[setting].consequence, ...expected[setting].remediation]) {
          expect(problems[0].message).toContain(text)
          checked++
        }
        expectNoValueLeak(problems[0].message)
      }
    }
    expect(checked).toBeGreaterThan(0)
  })

  it("reports every missing setting, in a stable order", () => {
    const problems = frontendStartupProblems({ NODE_ENV: "production" })
    expect(problems.map((p) => p.setting)).toEqual([...SETTINGS])
  })

  it("marks only the secret as sensitive", () => {
    const problems = frontendStartupProblems({ NODE_ENV: "production" })
    expect(problems.length).toBeGreaterThan(0)
    expect(problems.filter((p) => p.sensitive).map((p) => p.setting)).toEqual(["INTERNAL_SERVICE_SECRET"])
  })

  it("is quiet when a production server has everything", () => {
    expect(frontendStartupProblems(productionEnv())).toEqual([])
  })

  it("is quiet outside production, where the localhost defaults are intended (control)", () => {
    // Same missing settings that produce two problems under production.
    expect(frontendStartupProblems({ NODE_ENV: "production" })).toHaveLength(2)
    expect(frontendStartupProblems({ NODE_ENV: "development" })).toEqual([])
    expect(frontendStartupProblems({ NODE_ENV: "test" })).toEqual([])
    expect(frontendStartupProblems({})).toEqual([])
  })
})

describe("register() startup log", () => {
  let errorSpy: ReturnType<typeof vi.spyOn>

  beforeEach(() => {
    errorSpy = vi.spyOn(console, "error").mockImplementation(() => {})
  })

  afterEach(() => {
    errorSpy.mockRestore()
    vi.unstubAllEnvs()
  })

  function stubEnv(env: Record<string, string>) {
    for (const [key, value] of Object.entries(env)) vi.stubEnv(key, value)
  }

  function startupLines(): string[] {
    return errorSpy.mock.calls
      .map((args: unknown[]) => String(args[0]))
      .filter((line: string) => line.startsWith("Startup check: "))
  }

  it("logs one ERROR line per missing setting on the Node.js server", async () => {
    stubEnv(productionEnv({ NEXT_RUNTIME: "nodejs", INTERNAL_SERVICE_SECRET: "" }) as Record<string, string>)
    await register()
    const lines = startupLines()
    expect(lines).toHaveLength(1)
    expect(lines[0]).toContain("INTERNAL_SERVICE_SECRET")
    expectNoValueLeak(lines.join("\n"))
  })

  it("logs each problem's full message, so the log says what breaks and what to do", async () => {
    const env = productionEnv({ NEXT_RUNTIME: "nodejs", API_GATEWAY_INTERNAL_URL: "", INTERNAL_SERVICE_SECRET: "" })
    const expected = frontendStartupProblems(env).map((p) => `Startup check: ${p.message}`)
    expect(expected).toHaveLength(2)
    stubEnv(env as Record<string, string>)
    await register()
    // Every console.error call, not only the prefixed ones, so a dropped prefix fails too.
    const lines = errorSpy.mock.calls.map((args: unknown[]) => String(args[0]))
    expect(lines).toEqual(expected)
    expectNoValueLeak(lines.join("\n"))
  })

  it("logs nothing when every setting is present (control)", async () => {
    stubEnv(productionEnv({ NEXT_RUNTIME: "nodejs" }) as Record<string, string>)
    await register()
    expect(startupLines()).toEqual([])
  })

  it("does not run the check in the edge runtime, which has no server settings", async () => {
    stubEnv({ NODE_ENV: "production", NEXT_RUNTIME: "edge", API_GATEWAY_INTERNAL_URL: "", INTERNAL_SERVICE_SECRET: "" })
    await register()
    expect(startupLines()).toEqual([])
  })
})

describe("/api/health missingSettings", () => {
  beforeEach(() => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ status: "ok" }), { status: 200 })),
    )
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.unstubAllEnvs()
  })

  async function environmentCheck() {
    const res = await healthGET()
    const body = await res.json()
    return { status: res.status, body, text: JSON.stringify(body) }
  }

  it("lists missing setting names but never the secret or any value", async () => {
    vi.stubEnv("NODE_ENV", "production")
    vi.stubEnv("API_GATEWAY_INTERNAL_URL", "")
    vi.stubEnv("INTERNAL_SERVICE_SECRET", "")
    const { status, body, text } = await environmentCheck()
    expect(body.checks.environment.missingSettings).toEqual(["API_GATEWAY_INTERNAL_URL"])
    expect(text).not.toContain("INTERNAL_SERVICE_SECRET")
    expectNoValueLeak(text)
    // Missing settings are reported, not turned into a failing health check.
    expect(status).toBe(200)
    expect(body.status).toBe("healthy")
  })

  it("lists nothing when every setting is present (control)", async () => {
    vi.stubEnv("NODE_ENV", "production")
    vi.stubEnv("API_GATEWAY_INTERNAL_URL", GATEWAY_URL_VALUE)
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET_VALUE)
    const { body } = await environmentCheck()
    expect(body.checks.environment.missingSettings).toEqual([])
  })

  function stubAllMissing(nodeEnv: string) {
    vi.stubEnv("NODE_ENV", nodeEnv)
    vi.stubEnv("API_GATEWAY_INTERNAL_URL", "")
    vi.stubEnv("INTERNAL_SERVICE_SECRET", "")
  }

  it("does not ask for ORCHESTRATOR_INTERNAL_URL, which the frontend no longer reads", async () => {
    vi.stubEnv("NODE_ENV", "production")
    vi.stubEnv("API_GATEWAY_INTERNAL_URL", GATEWAY_URL_VALUE)
    vi.stubEnv("ORCHESTRATOR_INTERNAL_URL", "")
    vi.stubEnv("INTERNAL_SERVICE_SECRET", SECRET_VALUE)
    const { body, text } = await environmentCheck()
    expect(body.checks.environment.missingSettings).toEqual([])
    expect(text).not.toContain("ORCHESTRATOR_INTERNAL_URL")
  })

  it("lists nothing outside production, where the localhost defaults are intended", async () => {
    // Control: the same missing settings are listed on a production server.
    stubAllMissing("production")
    expect((await environmentCheck()).body.checks.environment.missingSettings).toEqual(["API_GATEWAY_INTERNAL_URL"])
    for (const nodeEnv of ["development", "test"]) {
      vi.unstubAllEnvs()
      stubAllMissing(nodeEnv)
      expect((await environmentCheck()).body.checks.environment.missingSettings).toEqual([])
    }
  })
})

// A setting the check names must still be read by the frontend: otherwise the
// startup log and /api/health ask the operator for something that changes
// nothing. ORCHESTRATOR_INTERNAL_URL stayed checked after its only reader went
// (#1210, 2026-09-25), so every compose file kept setting it.
describe("every setting the check names", () => {
  const FRONTEND = process.cwd()
  const CHECK = "src/instrumentation.ts"

  function sources(dir: string): string[] {
    return readdirSync(join(FRONTEND, dir), { withFileTypes: true }).flatMap((entry) => {
      const path = `${dir}/${entry.name}`
      if (entry.isDirectory()) return entry.name === "__tests__" ? [] : sources(path)
      return /\.(ts|tsx|mjs|js)$/.test(entry.name) && path !== CHECK ? [path] : []
    })
  }

  it("is read by the frontend outside the check", () => {
    const checked = [...readFileSync(join(FRONTEND, CHECK), "utf8").matchAll(/\bunset\("([A-Z0-9_]+)"\)/g)].map((m) => m[1])
    expect(checked).toEqual([...SETTINGS])
    const files = [...sources("src"), ...readdirSync(FRONTEND).filter((f) => /^(next|sentry\.[a-z]+)\.config\.(ts|mjs|js)$/.test(f))]
    expect(files.length).toBeGreaterThan(100)
    const code = files.map((f) => readFileSync(join(FRONTEND, f), "utf8"))
    const unread = checked.filter(
      (name) => !code.some((text) => new RegExp(`process\\.env(\\.${name}\\b|\\[["']${name}["']\\])`).test(text)),
    )
    expect(unread, "checked at startup, but nothing in the frontend reads it").toEqual([])
  })
})
