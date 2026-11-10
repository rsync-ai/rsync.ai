import { readFileSync } from "node:fs"
import { relative } from "node:path"

import { afterEach, describe, expect, it, vi } from "vitest"

import { timeZoneChoices } from "@/lib/timezones"

import { SRC, sourceFiles } from "./class-literals"

// Bug class: a timezone picker whose list lacks the zone it shows. Chrome's
// Intl.supportedValuesOf("timeZone") has no "UTC" (nor has Node's), so the pipeline
// schedule dialog opened blank in a UTC browser and on every chat-made schedule
// (stored in UTC), and no picker could offer UTC — seen recording the launch demo,
// 2026-10-09. Three pickers built the list by hand; they now share timeZoneChoices,
// and the guard below fails when a new one reads the browser's list itself.

const supported = (zones: string[] | undefined) =>
  vi.spyOn(Intl as unknown as { supportedValuesOf: (k: string) => string[] }, "supportedValuesOf")
    .mockImplementation(() => {
      if (!zones) throw new RangeError("unsupported")
      return zones
    })

afterEach(() => {
  vi.restoreAllMocks()
})

describe("timeZoneChoices", () => {
  it("adds UTC to a list that lacks it, sorted", () => {
    supported(["Europe/Brussels", "America/New_York"])
    expect(timeZoneChoices()).toEqual(["America/New_York", "Europe/Brussels", "UTC"])
  })

  it("adds the zone in use when the list spells it differently", () => {
    supported(["Asia/Kolkata", "UTC"])
    expect(timeZoneChoices("Asia/Calcutta")).toEqual(["Asia/Calcutta", "Asia/Kolkata", "UTC"])
  })

  it("lists each zone once, and ignores an empty value", () => {
    supported(["UTC", "Europe/London"])
    expect(timeZoneChoices("UTC", "", null, undefined, "Europe/London")).toEqual(["Europe/London", "UTC"])
  })

  it("falls back to a short list, with UTC, where the engine has none", () => {
    supported(undefined)
    const zones = timeZoneChoices("Europe/Brussels")
    expect(zones).toContain("UTC")
    expect(zones).toContain("Europe/Brussels")
    expect(zones.length).toBeGreaterThan(2)
  })
})

describe("timezone pickers", () => {
  it("take their list from timeZoneChoices, never from Intl.supportedValuesOf directly", () => {
    const offenders = new Set<string>()
    for (const file of sourceFiles(SRC)) {
      const where = relative(SRC, file)
      if (where === "lib/timezones.ts" || /\.test\.tsx?$/.test(where)) continue
      const src = readFileSync(file, "utf8")
      for (const m of src.matchAll(/supportedValuesOf/g)) {
        offenders.add(`${where}:${src.slice(0, m.index).split("\n").length}`)
      }
    }
    expect([...offenders]).toEqual([])
  })
})
