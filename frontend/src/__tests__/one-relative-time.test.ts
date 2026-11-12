import { readdirSync, readFileSync, statSync } from "node:fs"
import { join, relative } from "node:path"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ageLabel } from "@/components/admin/ComponentHealthTable"
import { formatAgeMs, formatRelativeTime } from "@/lib/utils"

// Eleven components each wrote their own "how long ago", and the same moment read
// differently from panel to panel: "Just now" beside "just now", "36h ago" on the
// health table beside "1d ago" on the pipeline page, "" beside "—" for a missing
// time, "last change 1h 5m ago" on the Steps graph beside "last event 1h ago" in
// the header. There is now one formatter, with options for the surfaces that need
// seconds or a fixed "now". Pinned here: its boundaries, and that no other file
// under src/ builds an "… ago" string by hand.

const NOW = Date.parse("2026-10-03T12:00:00Z")
const ago = (ms: number) => new Date(NOW - ms).toISOString()

describe("formatRelativeTime", () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(NOW)
  })
  afterEach(() => vi.useRealTimers())

  it("floors at every unit and says just now under 45 s", () => {
    expect(formatRelativeTime(ago(10_000))).toBe("just now")
    expect(formatRelativeTime(ago(50_000))).toBe("1m ago")
    expect(formatRelativeTime(ago(59 * 60_000 + 59_000))).toBe("59m ago")
    expect(formatRelativeTime(ago(23 * 3_600_000 + 59 * 60_000))).toBe("23h ago")
    expect(formatRelativeTime(ago(36 * 3_600_000))).toBe("1d ago")
  })

  it("counts seconds under a minute when asked", () => {
    expect(formatRelativeTime(ago(4_000), { seconds: true })).toBe("4s ago")
    expect(formatRelativeTime(ago(59_900), { seconds: true })).toBe("59s ago")
    expect(formatRelativeTime(ago(60_000), { seconds: true })).toBe("1m ago")
    expect(formatAgeMs(12_000, { seconds: true })).toBe("12s ago")
  })

  it("measures against a given now, and clamps a future time", () => {
    expect(formatRelativeTime("2026-10-03T11:48:00Z", { now: NOW })).toBe("12m ago")
    expect(formatRelativeTime(ago(-60_000))).toBe("just now")
    expect(formatRelativeTime(NOW - 3 * 3_600_000)).toBe("3h ago")
  })

  it("is a dash, or the caller's empty, for a missing or unreadable time", () => {
    expect(formatRelativeTime(undefined)).toBe("—")
    expect(formatRelativeTime(null)).toBe("—")
    expect(formatRelativeTime("")).toBe("—")
    expect(formatRelativeTime("not a time")).toBe("—")
    expect(formatRelativeTime("not a time", { empty: "" })).toBe("")
  })

  it("is what the health table says too, past the old 48 h cut-over", () => {
    expect(ageLabel(ago(36 * 3_600_000), NOW)).toBe(formatRelativeTime(ago(36 * 3_600_000)))
  })
})

describe("no file builds its own '… ago' string", () => {
  const SRC = join(__dirname, "..")
  const files: string[] = []
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const p = join(dir, name)
      if (statSync(p).isDirectory()) {
        if (name !== "__tests__" && name !== "node_modules") walk(p)
      } else if (/\.(ts|tsx)$/.test(name) && !/\.test\.tsx?$/.test(name)) {
        files.push(p)
      }
    }
  }
  walk(SRC)

  it("finds the source files it scans", () => {
    expect(files.length).toBeGreaterThan(200)
  })

  // Every one of the eleven built it as a template: `${mins}m ago`, `${…} ago`.
  it("keeps every '… ago' template in lib/utils.ts", () => {
    const offenders = files
      .filter((p) => relative(SRC, p) !== join("lib", "utils.ts"))
      .filter((p) => /\$\{[^`\n]*\}\w* ago`/.test(readFileSync(p, "utf8")))
      .map((p) => relative(SRC, p))
    expect(offenders).toEqual([])
  })
})
