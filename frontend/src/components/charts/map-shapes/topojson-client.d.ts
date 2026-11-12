// topojson-client ships no types; this is the one call the map chart makes.
declare module "topojson-client" {
  type Polygon = { type: "Polygon"; coordinates: number[][][] }
  type MultiPolygon = { type: "MultiPolygon"; coordinates: number[][][][] }
  interface TopoFeature {
    type: "Feature"
    id?: string | number
    properties: Record<string, unknown>
    geometry: Polygon | MultiPolygon | null
  }
  /** A TopoJSON object as GeoJSON: a GeometryCollection is a FeatureCollection. */
  export function feature(topology: object, object: object): { type: "FeatureCollection"; features: TopoFeature[] }
}
