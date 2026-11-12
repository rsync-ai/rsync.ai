import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { describe, expect, it } from "vitest"

import { CHART_REGISTRY, chartSpec, isStatic, seriesCap } from "@/components/charts/chart-registry"
import { CHART_TYPES } from "@/lib/api/charts"

/**
 * The frontend's chart registry is the gateway's (dashboards-design.md § 14.2):
 * shared/chart_types_golden.json is written from internal/charts/registry.go, whose
 * registry_test.go compares the two. A type, role, cap or sort added here alone, or there
 * alone, fails one side or the other.
 */
const golden = JSON.parse(
  readFileSync(resolve(__dirname, "../../../shared/chart_types_golden.json"), "utf8"),
) as { types: unknown[] }

describe("chart registry", () => {
  it("is the shared golden, type by type and in order", () => {
    // Control: the golden is read, so an empty one cannot pass.
    expect(golden.types.length).toBeGreaterThanOrEqual(7)
    expect(JSON.parse(JSON.stringify(CHART_REGISTRY))).toEqual(golden.types)
  })

  it("offers every type the API lists, and no other", () => {
    expect(CHART_REGISTRY.map((t) => t.type)).toEqual([...CHART_TYPES])
  })

  it("names an unknown type as undrawn", () => {
    expect(chartSpec("gauge")).toBeUndefined()
    expect(seriesCap("gauge")).toBe(0)
    expect(seriesCap("bar")).toBe(7)
    expect(seriesCap("table")).toBe(0)
  })

  it("draws a type on the server only when the registry marks it static", () => {
    expect(CHART_REGISTRY.filter((t) => isStatic(t.type)).map((t) => t.type)).toEqual(CHART_REGISTRY.map((t) => t.type))
    expect(isStatic("gauge")).toBe(false)
    const bar = chartSpec("bar")!
    bar.static = false
    try {
      expect(isStatic("bar")).toBe(false)
    } finally {
      bar.static = true
    }
  })
})
