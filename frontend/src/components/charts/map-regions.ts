/**
 * A map chart's shapes by code (dashboards-design.md § 14.8, open question 11): world
 * countries by their ISO 3166-1 code and US states by their ISO 3166-2 code. A name is
 * never matched and nothing is looked up outside the install. No ECharts import and no
 * shapes: the page reads a row's code here, and the shapes load with the first map it
 * draws (echart-maps.ts).
 */

import { US_STATES, WORLD_CODES } from "@/components/charts/map-shapes/region-codes"
import type { ChartMap } from "@/lib/api/charts"

/** The name ECharts knows each map's shapes by (echart-maps.ts). */
export const ECHARTS_MAPS: Record<ChartMap, string> = { world: "rsync-world", us_states: "rsync-us-states" }

/** A shape: the name ECharts knows it by (a country's alpha-3 code, a state's "US-CA") and the name it shows. */
export interface Region {
  region: string
  name: string
}

const WORLD = new Map<string, Region>()
for (const [numeric, [alpha2, alpha3, name]] of Object.entries(WORLD_CODES)) {
  const region = { region: alpha3, name }
  for (const key of [numeric, alpha2, alpha3]) WORLD.set(key, region)
}

const STATES = new Map<string, Region>()
for (const [code, name] of Object.entries(US_STATES)) {
  const region = { region: code, name }
  for (const key of [code, code.slice("US-".length)]) STATES.set(key, region)
}

/** A shape's name, by the name ECharts knows it by; a world-atlas shape with no ISO code is named as world-atlas names it. */
export function regionName(map: ChartMap, region: string): string {
  return (map === "us_states" ? STATES : WORLD).get(region)?.name ?? region
}

/**
 * The shape a row's code names, or null: a country by its alpha-2 (FR), alpha-3 (FRA) or
 * numeric code (250, or 4 for 004); a state by "US-CA" or "CA". Case and the spaces
 * around it do not matter.
 */
export function regionOf(map: ChartMap, value: unknown): Region | null {
  let key: string
  if (typeof value === "number") {
    if (!Number.isInteger(value) || value < 0) return null
    key = String(value)
  } else if (typeof value === "string") {
    key = value.trim().toUpperCase()
  } else {
    return null
  }
  if (map === "us_states") return STATES.get(key) ?? null
  return WORLD.get(/^\d{1,3}$/.test(key) ? key.padStart(3, "0") : key) ?? null
}
