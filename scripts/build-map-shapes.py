#!/usr/bin/env python3
"""Builds the map chart's shape files (dashboards-design.md § 14.9, D4e).

A map chart fills world countries and US states by ISO code, from shapes shipped in the
image (open question 11): no tiles and no outside geocoding. The world's shapes are
world-atlas 2.0.2's countries-110m.json as it is in node_modules; this script writes the
two files beside it in frontend/src/components/charts/map-shapes/:

- region-codes.ts: each world-atlas shape's ISO 3166-1 numeric id with its alpha-2 and
  alpha-3 codes and its name, from Natural Earth's admin-0 countries; the shapes with no
  ISO code (Northern Cyprus, Somaliland, Kosovo), by world-atlas's name; and each US
  state's ISO 3166-2 code (US-CA) with its name. world-atlas keys a shape by its numeric
  code only. Natural Earth's `_EH` fields are used, since its plain ISO fields are -99
  for France and Norway and CN-TW for Taiwan; a country's name is its ADMIN field, which
  has no abbreviations ("Bosnia and Herzegovina", not "Bosnia and Herz.").
- us-states.json: the 50 states and DC from Natural Earth's admin-1 states, each named
  by its ISO 3166-2 code, coordinates rounded to 0.01 degrees.

Natural Earth is public domain; world-atlas is ISC. Run with the 110m files of the
Natural Earth 5.1.2 release, which are not kept in the repo:

  https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_110m_admin_0_countries.geojson
    sha256 6866c877d39cba9c357620878839b336d569f8c662d3cfab4cb1dbe2d39c977f
  https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_110m_admin_1_states_provinces.geojson
    sha256 0067dec6a2c4f9c7a644bf7a9a46163d8d881595953326d0c67d89934190aefe

  python3 -I scripts/build-map-shapes.py ne_110m_admin_0_countries.geojson \\
      ne_110m_admin_1_states_provinces.geojson \\
      frontend/node_modules/world-atlas/countries-110m.json frontend/src/components/charts/map-shapes
"""

import hashlib
import json
import sys
from pathlib import Path

COUNTRIES_SHA256 = "6866c877d39cba9c357620878839b336d569f8c662d3cfab4cb1dbe2d39c977f"
STATES_SHA256 = "0067dec6a2c4f9c7a644bf7a9a46163d8d881595953326d0c67d89934190aefe"
# world-atlas's shapes that have no ISO code, as it names them.
NO_CODE = ["N. Cyprus", "Somaliland", "Kosovo"]
STATES = 51  # 50 states and DC
DIGITS = 2


def load(path, sha256):
    raw = Path(path).read_bytes()
    got = hashlib.sha256(raw).hexdigest()
    if sha256 and got != sha256:
        sys.exit(f"{path}: sha256 {got}, want {sha256} (Natural Earth 5.1.2)")
    return json.loads(raw)


def world_codes(countries, atlas):
    by_numeric = {}
    for f in countries["features"]:
        p = f["properties"]
        if p["ISO_N3_EH"] != "-99":
            if p["ISO_N3_EH"] in by_numeric:
                sys.exit(f"two Natural Earth countries have the numeric code {p['ISO_N3_EH']}")
            by_numeric[p["ISO_N3_EH"]] = p
    codes, no_code = {}, []
    for g in atlas["objects"]["countries"]["geometries"]:
        numeric = g.get("id")
        if numeric is None:
            no_code.append(g["properties"]["name"])
            continue
        p = by_numeric.get(numeric)
        if p is None or len(p["ISO_A2_EH"]) != 2 or len(p["ISO_A3_EH"]) != 3:
            sys.exit(f"world-atlas shape {numeric} ({g['properties']['name']}) has no ISO code in Natural Earth")
        if numeric in codes:
            sys.exit(f"two world-atlas shapes have the id {numeric}")
        codes[numeric] = (p["ISO_A2_EH"], p["ISO_A3_EH"], p["ADMIN"])
    if no_code != NO_CODE:
        sys.exit(f"world-atlas's shapes with no id are {no_code}, want {NO_CODE}")
    for i, a in ((0, "alpha-2"), (1, "alpha-3")):
        seen = [c[i] for c in codes.values()]
        if len(seen) != len(set(seen)):
            sys.exit(f"two shapes share an {a} code")
    return codes


def ring_of(ring):
    out = []
    for x, y in ring:
        p = [round(x, DIGITS), round(y, DIGITS)]
        if not out or out[-1] != p:
            out.append(p)
    if out[0] != out[-1]:
        out.append(out[0])
    return out if len(out) >= 4 else None


def us_states(states):
    out, names, dropped = [], {}, 0
    for f in states["features"]:
        p, g = f["properties"], f["geometry"]
        if p["adm0_a3"] != "USA" or p["iso_3166_2"] != "US-" + p["postal"]:
            sys.exit(f"{p['name']}: not a US state with an ISO 3166-2 code")
        polygons = g["coordinates"] if g["type"] == "MultiPolygon" else [g["coordinates"]]
        kept = []
        for polygon in polygons:
            rings = [ring_of(r) for r in polygon]
            if rings[0] is None:
                dropped += 1
                continue
            kept.append([r for r in rings if r is not None])
        if not kept:
            sys.exit(f"{p['name']}: no shape left at {DIGITS} decimals")
        geometry = {"type": "Polygon", "coordinates": kept[0]} if len(kept) == 1 else {"type": "MultiPolygon", "coordinates": kept}
        out.append({"type": "Feature", "properties": {"name": p["iso_3166_2"]}, "geometry": geometry})
        names[p["iso_3166_2"]] = p["name"]
    if len(out) != STATES or len(names) != STATES:
        sys.exit(f"{len(out)} states, {len(names)} codes, want {STATES}")
    out.sort(key=lambda f: f["properties"]["name"])
    return {"type": "FeatureCollection", "features": out}, names, dropped


def main():
    if len(sys.argv) != 5:
        sys.exit(__doc__)
    countries = load(sys.argv[1], COUNTRIES_SHA256)
    states = load(sys.argv[2], STATES_SHA256)
    atlas = load(sys.argv[3], None)
    dest = Path(sys.argv[4])

    codes = world_codes(countries, atlas)
    shapes, state_names, dropped = us_states(states)
    lines = [
        "// Generated by scripts/build-map-shapes.py; do not edit. The codes and names of the map",
        "// chart's shapes, from Natural Earth 5.1.2 (public domain).",
        "",
        "/** Each world-atlas 2.0.2 shape's ISO 3166-1 numeric id, with its alpha-2 and alpha-3 codes and its name. */",
        "export const WORLD_CODES: Readonly<Record<string, readonly [alpha2: string, alpha3: string, name: string]>> = {",
    ]
    for numeric in sorted(codes):
        a2, a3, name = codes[numeric]
        lines.append(f"  {json.dumps(numeric)}: [{json.dumps(a2)}, {json.dumps(a3)}, {json.dumps(name, ensure_ascii=False)}],")
    lines += [
        "}",
        "",
        "/** Each US state's and DC's ISO 3166-2 code, as us-states.json names its shape, with its name. */",
        "export const US_STATES: Readonly<Record<string, string>> = {",
    ]
    for code in sorted(state_names):
        lines.append(f"  {json.dumps(code)}: {json.dumps(state_names[code], ensure_ascii=False)},")
    lines += ["}", ""]
    (dest / "region-codes.ts").write_text("\n".join(lines))

    (dest / "us-states.json").write_text(json.dumps(shapes, separators=(",", ":"), ensure_ascii=False) + "\n")
    points = sum(len(r) for f in shapes["features"] for poly in (
        [f["geometry"]["coordinates"]] if f["geometry"]["type"] == "Polygon" else f["geometry"]["coordinates"]) for r in poly)
    print(f"region-codes.ts: {len(codes)} world shapes with a code, {len(NO_CODE)} without ({', '.join(NO_CODE)})")
    print(f"us-states.json: {len(shapes['features'])} states, {points} points, {dropped} islands under 0.01 degrees dropped")


if __name__ == "__main__":
    main()
