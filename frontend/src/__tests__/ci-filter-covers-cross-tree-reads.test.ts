import { existsSync, readFileSync, readdirSync, statSync } from "node:fs"
import { join, relative, resolve } from "node:path"
import { describe, expect, it } from "vitest"

// The frontend job runs only when the `frontend` paths filter matches
// (.github/paths-filters.yml). A test here that reads a file outside frontend/
// pins the UI to it — a Go constant list, a shared golden, a doc — and a PR
// that changes only that file skips the job, so the pin goes red on somebody
// else's PR later. run-chart-specs.test.ts read activity_metrics.go and
// activity.go that way. This finds every such read and checks the filter
// matches it.

// vitest's root is the frontend package (vitest.config.ts), so the repo is one up.
const FRONTEND = process.cwd()
const REPO = resolve(FRONTEND, "..")
const SELF = "src/__tests__/ci-filter-covers-cross-tree-reads.test.ts"

// The top-level trees a frontend test reads from.
const TREES = [
  "api-gateway",
  "backend-orchestrator",
  "backend-temporal-adapter",
  "connector-deployer",
  "deploy",
  "docs",
  "kafka-mcp-sink",
  "llm-service",
  "scripts",
  "shared",
]

// A test that walks a whole directory instead of naming its files: the filter
// globs that cover what it reads there, or why the filter does not.
const WALKED: Record<string, string[] | string> = {
  "shared/mcp-connectors/public": [
    "shared/mcp-connectors/**/latest.json",
    "shared/mcp-connectors/**/versions/*/metadata.json",
  ],
  "api-gateway":
    "proxy-cookie-names.test.ts reads every gateway .go file for cookie names; " +
    "matching api-gateway/**/*.go would run this job on every gateway change",
}

function globToRegExp(glob: string): RegExp {
  let out = ""
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i]
    if (glob.startsWith("**/", i)) {
      out += "(?:.*/)?"
      i += 2
    } else if (glob.startsWith("**", i)) {
      out += ".*"
      i += 1
    } else if (c === "*") out += "[^/]*"
    else if (c === "?") out += "[^/]"
    else out += c.replace(/[.+^${}()|[\]\\]/g, "\\$&")
  }
  return new RegExp(`^${out}$`)
}

function frontendFilter(): string[] {
  const lines = readFileSync(join(REPO, ".github/paths-filters.yml"), "utf8").split("\n")
  const start = lines.findIndex((l) => /^frontend:\s*$/.test(l))
  const globs: string[] = []
  for (const line of lines.slice(start + 1)) {
    if (/^[\w-]+:\s*$/.test(line)) break
    const m = line.match(/^\s*-\s*['"]?([^'"#]+?)['"]?\s*$/)
    if (m) globs.push(m[1])
  }
  return globs
}

function testFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (name === "node_modules") return []
    if (statSync(path).isDirectory()) return testFiles(path)
    return /\.(test|spec)\.tsx?$/.test(name) ? [path] : []
  })
}

const isFile = (p: string) => existsSync(p) && statSync(p).isFile()
const isDir = (p: string) => existsSync(p) && statSync(p).isDirectory()

/**
 * The repo files a test source reads outside frontend/, and the directories it
 * walks. A path is written through one of TREES; when its tail is dynamic
 * (`…/handlers/${f}`, `resolve(WORKFLOWS, f)`), the file names are the string
 * literals in the same source that exist under that directory.
 */
function crossTreeReads(source: string): { files: string[]; walked: string[] } {
  if (!/\b(readFileSync|readdirSync|existsSync)\b/.test(source)) return { files: [], walked: [] }
  const code = source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|\s)\/\/.*$/gm, "$1")
  const literals = [...code.matchAll(/["'`]([\w./-]+\.[A-Za-z]+)["'`]/g)].map((m) => m[1])
  const files = new Set<string>()
  const walked = new Set<string>()
  const pattern = new RegExp(`(?<![\\w-])((?:\\.\\./)*)(${TREES.join("|")})((?:/[\\w.@-]+)*)(/?)`, "g")
  for (const m of code.matchAll(pattern)) {
    const [, up, tree, tail, slash] = m
    // A bare word such as "shared" is not a path; `${root}/backend-orchestrator/${rel}` is.
    if (!up && !tail && !slash && code[m.index - 1] !== "/") continue
    const rel = tree + tail
    const abs = join(REPO, rel)
    if (isFile(abs)) files.add(rel)
    else if (isDir(abs)) {
      const named = literals.map((l) => join(rel, l)).filter((p) => isFile(join(REPO, p)))
      if (named.length) named.forEach((p) => files.add(p))
      else walked.add(rel)
    }
  }
  return { files: [...files], walked: [...walked] }
}

describe("the frontend CI filter covers every file a frontend test reads outside frontend/", () => {
  const globs = frontendFilter().map(globToRegExp)
  const covered = (rel: string) => globs.some((g) => g.test(rel))
  const reads = testFiles(join(FRONTEND, "src"))
    .map((abs) => ({ test: relative(FRONTEND, abs), ...crossTreeReads(readFileSync(abs, "utf8")) }))
    .filter((r) => r.test !== SELF)

  it("matches each file read by name", () => {
    const all = reads.flatMap((r) => r.files.map((f) => ({ test: r.test, file: f })))
    // Armed: the scan finds the reads this file was written for.
    expect(all.length).toBeGreaterThan(15)
    expect(all).toContainEqual({
      test: "src/__tests__/run-chart-specs.test.ts",
      file: "api-gateway/internal/handlers/activity_metrics.go",
    })
    const missing = all.filter((r) => !covered(r.file)).map((r) => `${r.file} (${r.test})`)
    expect(missing, "add these to the frontend filter in .github/paths-filters.yml").toEqual([])
  })

  it("accounts for each directory walked whole", () => {
    const walked = [...new Set(reads.flatMap((r) => r.walked))].sort()
    expect(walked).toEqual(Object.keys(WALKED).sort())
    for (const [dir, cover] of Object.entries(WALKED)) {
      if (typeof cover === "string") continue
      for (const glob of cover) expect(frontendFilter(), dir).toContain(glob)
    }
  })

  it("reads a dynamic tail from the file names beside it", () => {
    const src = [
      "const go = (f: string) => readFileSync(`${SRC}/../../api-gateway/internal/handlers/${f}`)",
      'go("activity.go")',
    ].join("\n")
    expect(crossTreeReads(src)).toEqual({ files: ["api-gateway/internal/handlers/activity.go"], walked: [] })
    expect(globToRegExp("shared/mcp-connectors/**/latest.json").test("shared/mcp-connectors/public/x/latest.json")).toBe(true)
    expect(globToRegExp("frontend/**").test("frontend/src/a.ts")).toBe(true)
    expect(globToRegExp("shared/*.json").test("shared/a/b.json")).toBe(false)
  })
})
