// @vitest-environment node
import { getMap } from "echarts/core"
import { describe, expect, it } from "vitest"
import countries from "world-atlas/countries-110m.json"

import "@/components/charts/echart-maps"
import { ECHARTS_MAPS, regionName, regionOf } from "@/components/charts/map-regions"
import { US_STATES, WORLD_CODES } from "@/components/charts/map-shapes/region-codes"
import usStates from "@/components/charts/map-shapes/us-states.json"

// A map chart fills a shape a row names by its ISO code, never by its name, from shapes
// shipped in the image (dashboards-design.md § 14.8, open question 11).

type Geometry = { type: "Polygon"; coordinates: number[][][] } | { type: "MultiPolygon"; coordinates: number[][][][] }
type Shapes = { features: { properties: { name: string }; geometry: Geometry }[] }

/** world-atlas's shapes with no ISO code, as it names them: drawn, never filled (build-map-shapes.py checks the same three). */
const NO_CODE = ["N. Cyprus", "Somaliland", "Kosovo"]

/** The map ECharts holds under the name, as echart-maps.ts registered it. */
const registered = (map: keyof typeof ECHARTS_MAPS): Shapes => (getMap(ECHARTS_MAPS[map]) as unknown as { geoJSON: Shapes }).geoJSON

const rings = (g: Geometry) => (g.type === "Polygon" ? g.coordinates : g.coordinates.flat())

describe("the shapes' codes", () => {
  it("names every world-atlas shape by its ISO code, but the three that have none", () => {
    const atlas = countries.objects.countries.geometries as { id?: string; properties: { name: string } }[]
    const coded = atlas.filter((g) => g.id !== undefined)
    expect(coded.map((g) => g.id).sort()).toEqual(Object.keys(WORLD_CODES).sort())
    expect(atlas.filter((g) => g.id === undefined).map((g) => g.properties.name)).toEqual(NO_CODE)
    // Each alpha-2 and alpha-3 code names one shape.
    for (const i of [0, 1]) {
      const codes = Object.values(WORLD_CODES).map((c) => c[i])
      expect(new Set(codes).size).toBe(codes.length)
    }
  })

  it("names the 50 states and DC each by its ISO 3166-2 code, once", () => {
    const names = usStates.features.map((f) => f.properties.name)
    expect(names).toHaveLength(51)
    expect([...names].sort()).toEqual(Object.keys(US_STATES).sort())
    expect(names.every((n) => /^US-[A-Z]{2}$/.test(n))).toBe(true)
  })

  it("registers each world shape under its alpha-3 code and each state under its code", () => {
    const world = registered("world").features.map((f) => f.properties.name)
    expect(world).toHaveLength(177)
    expect(world.filter((n) => !NO_CODE.includes(n)).sort()).toEqual(Object.values(WORLD_CODES).map((c) => c[1]).sort())
    expect(registered("us_states").features.map((f) => f.properties.name).sort()).toEqual(Object.keys(US_STATES).sort())
  })

  it("draws no world shape as a band across the map: a ring that crossed 180° is cut there", () => {
    // ECharts draws longitude flat, so a step of more than half the world is a line across it.
    const jumps = registered("world").features.flatMap((f) =>
      rings(f.geometry)
        .filter((ring) => ring.some((p, i) => i > 0 && Math.abs(p[0] - ring[i - 1][0]) > 180))
        .map(() => f.properties.name),
    )
    // Antarctica's ring goes round the pole: its one step from 180° to -180° is along the bottom edge.
    expect(jumps).toEqual(["ATA"])
    // world-atlas joins Fiji and Russia across 180°: each is cut into a west and an east part.
    for (const name of ["FJI", "RUS"]) {
      const lons = rings(registered("world").features.find((f) => f.properties.name === name)!.geometry).flat().map((p) => p[0])
      expect(Math.max(...lons)).toBeCloseTo(180)
      expect(Math.min(...lons)).toBeCloseTo(-180)
    }
  })
})

describe("regionOf", () => {
  it("reads a country by its alpha-2, alpha-3 or numeric code, in any case and with spaces around it", () => {
    for (const code of ["FR", "fra", " Fr ", "250", 250]) expect(regionOf("world", code)).toEqual({ region: "FRA", name: "France" })
    // A numeric code is three digits, so 4 is 004.
    expect(regionOf("world", 4)?.region).toBe("AFG")
    expect(regionOf("world", "4")?.region).toBe("AFG")
    expect(regionOf("world", "CA")?.region).toBe("CAN")
  })

  it("reads a US state by US-CA or CA", () => {
    for (const code of ["US-CA", "us-ca", "CA", " ca "]) expect(regionOf("us_states", code)).toEqual({ region: "US-CA", name: "California" })
    expect(regionOf("us_states", "DC")?.name).toBe("District of Columbia")
  })

  it("never reads a name, a code of the other map or anything that is not a code", () => {
    for (const v of ["France", "United Kingdom", "", "1000", 4.5, -4, null, undefined, true, {}]) expect(regionOf("world", v)).toBeNull()
    for (const v of ["California", "CAN", "US-XX", "USCA", 6]) expect(regionOf("us_states", v)).toBeNull()
  })

  it("names a shape by the name ECharts knows it by; a shape with no code keeps world-atlas's name", () => {
    expect(regionName("world", "DEU")).toBe("Germany")
    expect(regionName("us_states", "US-NY")).toBe("New York")
    expect(regionName("world", "Kosovo")).toBe("Kosovo")
  })
})
