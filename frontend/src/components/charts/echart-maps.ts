// Loaded by chart-modules.ts the first time a map is drawn, and by the server's
// chart-svg.ts: the map series and the world and US-state shapes, so a page of bars never
// downloads them (§ 14.6). The series goes in first: ECharts' registerMap does nothing
// until a map or geo module is.
//
// Each shape is given to ECharts under the name map-regions.ts reads a row's code into: a
// world-atlas country by its ISO alpha-3 code, a US state by its ISO 3166-2 code. A
// world-atlas shape with no ISO code keeps world-atlas's name, which no code matches, so
// it is drawn and never filled.
import { MapChart } from "echarts/charts"
import { registerMap, use as registerModules } from "echarts/core"
import { feature } from "topojson-client"
import countries from "world-atlas/countries-110m.json"

import { ECHARTS_MAPS } from "@/components/charts/map-regions"
import { WORLD_CODES } from "@/components/charts/map-shapes/region-codes"
import usStates from "@/components/charts/map-shapes/us-states.json"

registerModules([MapChart])

type GeoJSON = Parameters<typeof registerMap>[1]
type Ring = number[][]

/**
 * The ring with a longitude moved by 360 wherever it jumps across the 180° meridian, so it
 * runs on past 180° instead; null for a ring round a pole, which ends 360° from where it
 * started (Antarctica's, which ECharts draws as it is).
 */
function unwrap(ring: Ring): Ring | null {
  const out = [ring[0]]
  for (const [lon, lat] of ring.slice(1)) {
    const prev = out[out.length - 1][0]
    out.push([lon + 360 * Math.round((prev - lon) / 360), lat])
  }
  return Math.abs(out[out.length - 1][0] - out[0][0]) < 1e-9 ? out : null
}

/** The part of a closed ring west or east of 180°, closed, with a point on 180° where it crosses; null when nothing is left. */
function clip(ring: Ring, side: "west" | "east"): Ring | null {
  const inside = (p: number[]) => (side === "west" ? p[0] <= 180 : p[0] >= 180)
  const out: Ring = []
  for (let i = 0; i + 1 < ring.length; i++) {
    const [p, q] = [ring[i], ring[i + 1]]
    if (inside(p)) out.push(p)
    if (inside(p) !== inside(q)) out.push([180, p[1] + ((180 - p[0]) / (q[0] - p[0])) * (q[1] - p[1])])
  }
  if (out.length > 0) out.push(out[0])
  return out.length >= 4 ? out : null
}

/**
 * A polygon cut in two at 180°. world-atlas joins the shapes Natural Earth cuts there
 * (Fiji's, Russia's) for a globe, but ECharts draws longitude flat, so a joined ring would
 * be a band across the whole map. Each ring is unwrapped to run past 180° and cut: the
 * west part stays, the east part moves back by 360°. A polygon with no ring that crosses
 * is returned as it is.
 */
function cutAt180(polygon: Ring[]): Ring[][] {
  if (!polygon.some((ring) => ring.some((p, i) => i > 0 && Math.abs(p[0] - ring[i - 1][0]) > 180))) return [polygon]
  const rings = polygon.map(unwrap)
  if (rings.some((r) => r === null)) return [polygon]
  // Each ring on the side of 0° to 360° where it meets the others.
  const east = (rings as Ring[]).map((r) => (Math.min(...r.map((p) => p[0])) < 0 ? r.map(([lon, lat]) => [lon + 360, lat]) : r))
  return (["west", "east"] as const).flatMap((side) => {
    const [outer, ...holes] = east.map((r) => clip(r, side))
    if (!outer) return []
    const part = [outer, ...holes.filter((h): h is Ring => h !== null)]
    return [side === "west" ? part : part.map((r) => r.map(([lon, lat]) => [lon - 360, lat]))]
  })
}

const world = feature(countries, countries.objects.countries)
for (const f of world.features) {
  const codes = f.id === undefined ? undefined : WORLD_CODES[String(f.id)]
  if (codes) f.properties = { name: codes[1] }
  const g = f.geometry
  if (!g) continue
  const polygons = (g.type === "Polygon" ? [g.coordinates] : g.coordinates).flatMap(cutAt180)
  f.geometry = polygons.length === 1 ? { type: "Polygon", coordinates: polygons[0] } : { type: "MultiPolygon", coordinates: polygons }
}
registerMap(ECHARTS_MAPS.world, world as GeoJSON)

// Alaska and Hawaii drawn under the lower 48, where ECharts' own US example puts them.
registerMap(ECHARTS_MAPS.us_states, usStates as GeoJSON, {
  "US-AK": { left: -131, top: 25, width: 15 },
  "US-HI": { left: -110, top: 28, width: 5 },
})
