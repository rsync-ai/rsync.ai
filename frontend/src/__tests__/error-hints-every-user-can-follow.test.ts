/**
 * A failed request's hint is read by everyone who uses the app, most of them on
 * rsync.ai Cloud, where nobody runs Docker. A network failure on Connections read
 * "Make sure Docker Desktop is running and the API Gateway is up: `docker compose
 * up -d`": a step only whoever installed rsync.ai can take, and a self-hosted
 * install starts with install.sh, not a bare compose. Cloud and self-hosted run
 * the same frontend, so a hint leads with what any user can do and gives the
 * server step one sentence, addressed to whoever hosts it.
 */

import { readdirSync, readFileSync, statSync } from "node:fs"
import { join, relative, resolve } from "node:path"
import { describe, expect, expectTypeOf, it } from "vitest"

import { classifyError } from "@/lib/utils/error-handling"

type Context = NonNullable<Parameters<typeof classifyError>[1]>

const CONTEXTS = [
  "connections.load",
  "connections.test",
  "connections.create",
  "connections.update",
  "connections.delete",
  "connectors.load",
  "connector.save",
  "connector.generate",
  "connector.validate",
  "pipeline.load",
  "pipeline.run",
  "pipeline.pause",
  "pipeline.resume",
  "pipeline.stop",
  "pipeline.delete",
  "pipeline.export",
  "general",
] as const satisfies readonly Context[]

/** What the browser throws when the request never reached the server. */
const offline = () => new TypeError("Failed to fetch")
/** A 503 the gateway answered for a backend it could not reach. */
const unavailable = () => Object.assign(new Error(), { statusCode: 503, message: "Service Unavailable" })

const SELF_HOSTED = "If you host rsync.ai yourself, check that its services are running."

describe("classifyError hints name no host tool", () => {
  it("covers every context", () => {
    expectTypeOf<Exclude<Context, (typeof CONTEXTS)[number]>>().toBeNever()
  })

  it.each(CONTEXTS)("%s", (ctx) => {
    for (const err of [offline(), unavailable()]) {
      const { hint } = classifyError(err, ctx)
      expect(hint).toBeTruthy()
      expect(hint).not.toMatch(/docker|compose|api gateway/i)
    }
  })

  it("a network failure on Connections says what anyone can check, then what a self-hoster can", () => {
    const e = classifyError(offline(), "connections.load")
    expect(e.title).toBe("Connection failed")
    expect(e.hint).toBe(`Check your network connection and try again. ${SELF_HOSTED}`)
  })

  it("an unavailable service says to try again, then what a self-hoster can check", () => {
    expect(classifyError(unavailable(), "connections.load").hint).toBe(
      `A backend service is temporarily unavailable. Try again in a moment. ${SELF_HOSTED}`,
    )
  })
})

// Telling the reader to start Docker or run compose, anywhere the app shows text.
const HOST_STEP = /docker desktop|docker[ -]compose|docker\b.*\b(is|are) running/i

/** Every quoted string on a line of code: comments are for the people who run it. */
function quoted(line: string): string[] {
  const code = line.trim()
  if (/^(\/\/|\/\*|\*|\{\/\*)/.test(code)) return []
  return [...code.matchAll(/(["'`])((?:\\.|(?!\1)[^\\])*)\1/g)].map((m) => m[2])
}

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return name === "__tests__" ? [] : sources(path)
    return /\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) ? [path] : []
  })
}

describe("app text names no host step", () => {
  it("no string in the app tells the reader to start Docker or run compose", () => {
    const src = resolve(__dirname, "..")
    const found = sources(src).flatMap((path) =>
      readFileSync(path, "utf8")
        .split("\n")
        .flatMap((line, i) =>
          quoted(line)
            .filter((s) => HOST_STEP.test(s))
            .map((s) => `${relative(src, path)}:${i + 1}: ${s}`),
        ),
    )
    expect(found).toEqual([])
  })

  // Armed: the scan finds the wording it guards against, in each quote style.
  it.each([
    [`hint: "Make sure Docker Desktop is running"`],
    [`hint: 'run docker compose up -d'`],
    ["hint: `check that the database Docker container is running`"],
  ])("finds %s", (line) => {
    expect(quoted(line).some((s) => HOST_STEP.test(s))).toBe(true)
  })
})
