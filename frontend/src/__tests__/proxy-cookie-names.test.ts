import { readdirSync, readFileSync, statSync } from "node:fs"
import { join, relative, resolve } from "node:path"

import { describe, expect, it } from "vitest"

import { SRC, sourceFiles } from "./class-literals"

// Bug class: a cookie name the gateway never sets or reads. A server route that reads
// `session_token` finds no session for a signed-in viewer (the gateway sets
// `auth_token`) and answers 401 — the dashboard PDF export did, live on prod
// 2026-10-06, and the Explorer export had since #344. Their tests used the same
// invented name, so they were green. This guard reads the gateway's own Go source
// for the names it sets (http.Cookie{Name: …}), reads (c.Cookie(…)) or names in a
// cookie constant, and fails when the frontend reads or forwards any other name.

const GATEWAY = resolve(SRC, "../../api-gateway")

function goFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (name === "vendor" || name === "testdata") return []
    if (statSync(path).isDirectory()) return goFiles(path)
    return name.endsWith(".go") && !name.endsWith("_test.go") ? [path] : []
  })
}

function gatewayCookieNames(): Set<string> {
  const names = new Set<string>()
  for (const file of goFiles(GATEWAY)) {
    const src = readFileSync(file, "utf8")
    for (const m of src.matchAll(/http\.Cookie\{\s*Name:\s*"([\w-]+)"/g)) names.add(m[1])
    for (const m of src.matchAll(/\.Cookie\(\s*"([\w-]+)"\s*\)/g)) names.add(m[1])
    for (const m of src.matchAll(/\b\w*[cC]ookie\w*\s*=\s*"([\w-]+)"/g)) names.add(m[1])
  }
  return names
}

interface CookieUse {
  where: string
  name: string
}

// A cookie read on the server (`cookieStore.get("x")`, `(await cookies()).get(X)`,
// `request.cookies.get(…)`), in the browser (`getCookie("x")`), or a pair written into
// a forwarded Cookie header (`` `x=${value}` `` in a file that sends a Cookie header).
const READ = /(?:\bcookieStore|\bstore|\bcookies(?:\(\)\))?|\brequest\.cookies|\breq\.cookies)\.get\(\s*(?:["']([\w-]+)["']|([A-Z][A-Z0-9_]*))\s*\)/g
const CLIENT_READ = /\bgetCookie\(\s*["']([\w-]+)["']\s*\)/g
const FORWARDED_PAIR = /`([a-z][\w-]*)=\$\{/g

function frontendCookieUses(): CookieUse[] {
  const files = sourceFiles(SRC).map((f) => ({ path: relative(SRC, f), src: readFileSync(f, "utf8") }))
  const constants = new Map<string, string>()
  for (const { src } of files) {
    for (const m of src.matchAll(/\bconst\s+([A-Z][A-Z0-9_]*)\s*=\s*["']([\w-]+)["']/g)) constants.set(m[1], m[2])
  }
  const uses: CookieUse[] = []
  for (const { path, src } of files) {
    const at = (index: number | undefined) => `${path}:${src.slice(0, index).split("\n").length}`
    for (const m of src.matchAll(READ)) {
      const name = m[1] ?? constants.get(m[2])
      if (name) uses.push({ where: at(m.index), name })
    }
    for (const m of src.matchAll(CLIENT_READ)) uses.push({ where: at(m.index), name: m[1] })
    if (/\bCookie\b\s*[:=]|cookieHeader/.test(src)) {
      for (const m of src.matchAll(FORWARDED_PAIR)) uses.push({ where: at(m.index), name: m[1] })
    }
  }
  return uses
}

describe("every cookie the frontend reads or forwards is one the gateway knows", () => {
  const gateway = gatewayCookieNames()
  const uses = frontendCookieUses()

  it("finds the gateway's session, CSRF and workspace cookies (the scan is armed)", () => {
    expect(gateway).toContain("auth_token")
    expect(gateway).toContain("csrf_token")
    expect(gateway).toContain("rsync_active_workspace_id")
  })

  it("finds the reads it guards: middleware, server fetch, both export routes, the workspace pair", () => {
    const where = uses.map((u) => u.where.replace(/:\d+$/, ""))
    for (const file of [
      "middleware.ts",
      "lib/api/auth-fetch.ts",
      "lib/workspace/server-workspace.ts",
      "app/proxy/dashboards/[id]/pdf/route.ts",
      "app/proxy/explorer/export/route.ts",
    ]) {
      expect(where, file).toContain(file)
    }
    expect(uses.length).toBeGreaterThanOrEqual(12)
  })

  it("reads and forwards no cookie name the gateway never sets or reads", () => {
    const unknown = uses.filter((u) => !gateway.has(u.name)).map((u) => `${u.where} ${u.name}`)
    expect(unknown).toEqual([])
  })
})
