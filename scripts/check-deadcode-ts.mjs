#!/usr/bin/env node
// No frontend file, export or dependency may be unused by the production build
// unless scripts/deadcode-ts-allowlist.txt names it with a reason.
//
// The TypeScript twin of scripts/check-deadcode.sh; its header says why unused
// code is not free here. The first sweep deleted two components nothing
// rendered (HITLTablePicker, NodeInspector), 27 types from lib/api/types.ts and
// 25 other exports, and several misled rather than merely sat there: an
// ORCHESTRATOR_URL, LLM_SERVICE_URL and orchestrator WebSocket endpoint the
// browser never called, plumbed through three compose files, the Dockerfile and
// the self-hosting doc as if it did; a second `Pipeline` interface in types.ts
// beside the one lib/api/pipelines.ts getPipeline() actually returns; and an
// updatePipeline client nothing called, while the one rename that PUTs to that
// endpoint calls it directly.
//
// How it works:
// - knip, pinned, in frontend/ with --production: the entry points are the ones
//   its Next.js plugin derives (app/ routes, layouts, middleware, next.config)
//   and the project is frontend/knip.json's list minus tests. A function only a
//   test calls is dead in production, which is exactly what this reports -- the
//   same rule as the Go check's run without -test.
// - Every issue type knip reports counts: unused files, exports, types, enum and
//   namespace members, duplicate exports, and unused, unlisted or unresolved
//   dependencies.
// - Blind spots. knip.json sets ignoreExportsUsedInFile, because the shadcn ui/
//   files export every part they compose; so an export whose only use is in its
//   own file -- a `X.displayName =` line, or in-file code that is itself dead --
//   is never reported. The first sweep ran once with it off and found 4 dead
//   shadcn parts that way, by hand. knip also never looks inside an exported
//   object: a dead key of API_ENDPOINTS, WS_ENDPOINTS or a store's state hides
//   in the live object (WS_ENDPOINTS.ORCHESTRATOR was found by grep).
// - Unused imports, variables and props are the other half of the guard, and
//   ESLint's @typescript-eslint/no-unused-vars at "error" catches them
//   (frontend/eslint.config.mjs, run by the frontend job's lint step).
// - Entries match on path + name, never line number, so moving code does not
//   churn the allowlist.
// - The comparison runs both ways. A new finding fails, and so does an allowlist
//   line that no longer matches one. That second direction is the control: the
//   allowlisted entries are known findings, so a run that silently found nothing
//   (knip's JSON shape changed, a config change emptied the project) fails as
//   that many stale lines instead of passing.
//
// Exit 0 = clean · 1 = a finding (each printed as a FAIL line) · 2 = setup error
// (no frontend/node_modules, knip did not install or run, or its output did not
// parse).
import { spawnSync } from "node:child_process"
import { existsSync, readFileSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const KNIP = "knip@6.40.0"
const ALLOWLIST = "scripts/deadcode-ts-allowlist.txt"
const FRONTEND = "frontend"

const root = join(dirname(fileURLToPath(import.meta.url)), "..")
process.chdir(root)

function setupError(msg) {
  console.error(`setup error: ${msg}`)
  process.exit(2)
}

// knip resolves imports and loads the Next.js, ESLint and Vitest plugins from
// the project's own node_modules; without them every import is "unresolved".
if (!existsSync(join(FRONTEND, "node_modules"))) {
  setupError(`${FRONTEND}/node_modules is missing -- run \`npm ci\` in ${FRONTEND}/ first`)
}

const res = spawnSync(
  "npx",
  ["--yes", KNIP, "--no-progress", "--production", "--reporter", "json"],
  { cwd: FRONTEND, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 },
)
// knip: 0 = nothing found, 1 = issues found; anything else did not run. npx
// also exits 1 when the install fails, which the JSON parse below catches.
if (res.error || (res.status !== 0 && res.status !== 1)) {
  setupError(`npx ${KNIP} exited ${res.status}:\n${res.error ?? ""}${res.stderr}${res.stdout}`)
}
let report
try {
  report = JSON.parse(res.stdout)
} catch (e) {
  setupError(`npx ${KNIP} exited ${res.status} without a JSON report (${e.message}):\n` +
    `${res.stderr}${res.stdout.slice(0, 2000)}`)
}
if (!Array.isArray(report?.issues)) setupError(`${KNIP} JSON has no "issues" array`)

const found = new Map() // allowlist key -> what knip called it, for the FAIL line
for (const issue of report.issues) {
  const path = `${FRONTEND}/${issue.file}`
  for (const [kind, items] of Object.entries(issue)) {
    if (kind === "file" || !Array.isArray(items)) continue
    for (const item of items) {
      if (kind === "files") {
        found.set(path, "unused file")
        continue
      }
      // A duplicate is a list of the names one value is exported under.
      const name = Array.isArray(item) ? item.map((d) => d.name).join("|") : item.name
      const line = Array.isArray(item) ? item[0]?.line : item.line
      found.set(`${path} ${name}`, `unused ${kind}${line ? `, line ${line}` : ""}`)
    }
  }
}

const allowed = new Set(
  readFileSync(ALLOWLIST, "utf8")
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith("#"))
    .map((l) => l.split(/\s+/).join(" ")),
)

console.log(`${KNIP} --production over ${FRONTEND}/; ${found.size} findings, ${allowed.size} allowlisted`)
const fails = []
for (const key of [...found.keys()].filter((k) => !allowed.has(k)).sort()) {
  fails.push(`unused and not allowlisted: ${key} (${found.get(key)})`)
}
for (const key of [...allowed].filter((k) => !found.has(k)).sort()) {
  fails.push(`allowlisted but no longer unused -- delete the line from ${ALLOWLIST}: ${key}`)
}
for (const f of fails) console.log(`  FAIL  ${f}`)
if (fails.length) {
  console.log(`
Code the production build never uses. In order of preference:
  1. delete it (and its test, if only the test called it);
  2. if only tests need it, move it into the test or a test helper;
  3. if it is kept on purpose, add "<path>" (a whole file) or "<path> <name>"
     to ${ALLOWLIST} under a comment saying why and what would retire it.
A stale allowlist line just needs deleting.`)
  process.exit(1)
}
console.log(`  ok    all ${found.size} findings allowlisted; nothing else unused`)
