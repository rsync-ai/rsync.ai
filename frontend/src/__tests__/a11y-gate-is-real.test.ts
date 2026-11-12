import { existsSync, readFileSync, readdirSync } from "node:fs"
import { join, resolve } from "node:path"
import { describe, expect, it } from "vitest"

// The nightly axe job (`frontend-a11y` in ci.yml) reported green for months
// while measuring two public pages: it started no backend, so the login in
// e2e/fixtures/auth.setup.ts skipped, the authenticated specs skipped with it,
// and both the step and the job carried `continue-on-error`. A skip read as a
// pass and a failure read as a pass.
//
// The class is a check that cannot fail. It now has a backend
// (e2e/fixtures/a11y-api-stub.mjs, started by playwright.a11y.config.ts), its
// setup fails on an unreachable backend, and the job is blocking. This holds
// each of those, so none can quietly come back.

// vitest's root is the frontend package (vitest.config.ts), so the repo is one up.
const FRONTEND = process.cwd()
const CI_YML = resolve(FRONTEND, "..", ".github/workflows/ci.yml")

const read = (p: string) => readFileSync(p, "utf8")

/** The job's lines, from `  frontend-a11y:` to the next job; comments dropped. */
function a11yJob(): string[] {
  const lines = read(CI_YML).split("\n")
  const start = lines.findIndex((l) => /^ {2}frontend-a11y:\s*$/.test(l))
  expect(start, "no frontend-a11y job in ci.yml; this guard went blind").toBeGreaterThan(-1)
  const rest = lines.slice(start + 1)
  const end = rest.findIndex((l) => /^ {2}[\w-]+:\s*$/.test(l))
  return (end === -1 ? rest : rest.slice(0, end)).filter((l) => !/^\s*#/.test(l))
}

/** Source with line and block comments removed, so prose about skips doesn't count. */
const code = (p: string) => read(p).replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "")

const SUITE_FILES = [
  ...readdirSync(join(FRONTEND, "e2e/a11y")).map((f) => join("e2e/a11y", f)),
  "e2e/fixtures/auth.setup.ts",
].filter((f) => /\.tsx?$/.test(f))

/**
 * The job's header and each step, as text. A step starts at `      - `; the
 * header is everything before the first one.
 */
function a11yJobParts(): string[] {
  const parts: string[][] = [[]]
  for (const l of a11yJob()) {
    if (/^ {6}- /.test(l)) parts.push([])
    parts[parts.length - 1].push(l)
  }
  return parts.map((p) => p.join("\n"))
}

describe("the axe suite can fail", () => {
  it("neither the frontend-a11y job nor any step but an artifact upload carries continue-on-error", () => {
    // The one allowed: an upload must not go red on a full artifact quota
    // (test_no_job_depends_on_the_artifact_quota.py), and it runs only after
    // the suite has already failed the job.
    const flagged = a11yJobParts().filter(
      (p) => /^\s*continue-on-error:/m.test(p) && !/^\s*uses: actions\/upload-artifact@/m.test(p),
    )
    expect(flagged, "continue-on-error turns every axe failure into a green check").toEqual([])
  })

  it("the job runs the suite, and uploads the report when it fails", () => {
    const job = a11yJob().join("\n")
    expect(job).toMatch(/run: npm run test:a11y/)
    // Without failure() the implicit success() skips the upload on exactly the
    // run whose report is wanted.
    const upload = job.match(/- name: Upload report on failure\n(?:.*\n)*?\s+if: (.*)/)
    expect(upload?.[1], "the report upload's if: was not found").toMatch(/failure\(\)/)
  })

  it.each(SUITE_FILES)("%s never skips", (file) => {
    const skips = code(join(FRONTEND, file)).match(/\.(?:skip|fixme)\s*\(/g) ?? []
    expect(skips, "a skipped axe test reads as a pass in the report").toEqual([])
  })

  it("the config starts a backend for the login and loads its session unconditionally", () => {
    const config = code(join(FRONTEND, "playwright.a11y.config.ts"))
    expect(config).toMatch(/node e2e\/fixtures\/a11y-api-stub\.mjs/)
    expect(existsSync(join(FRONTEND, "e2e/fixtures/a11y-api-stub.mjs"))).toBe(true)
    // A storageState that falls back to undefined when the file is missing is
    // how the authenticated project ran signed out (and redirected) unnoticed.
    expect(config).not.toMatch(/existsSync/)
    expect(config).toMatch(/storageState: AUTH_STATE\b/)
  })
})
