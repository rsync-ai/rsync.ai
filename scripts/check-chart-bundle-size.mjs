#!/usr/bin/env node
// The chart code a browser downloads stays within a gzip budget
// (dashboards-design.md § 14.6). Run in frontend/ after `npm run build`.
//
// Why: ECharts is the largest thing a page loads, and the ways it grows are
// silent. A bare `import ... from "echarts"` registers every chart and
// component: measured on 2026-10-11, one such import in EChart.tsx took the
// chart chunks from 189,926 to 395,943 bytes gzipped, and the custom-series
// packages import `echarts` that way. next.config.ts aliases `echarts` to
// `echarts/core`; this check is what notices the alias gone, or a new type
// registered on every page instead of in its own group.
//
// How it works:
// - A chart chunk is a file under .next/static/chunks holding ECharts code:
//   core's `__ec_` keys, the `ecModel` every component and series is handed,
//   a series model's `series.<type>` name, so a group of series loaded on
//   its own still counts, or a module's install calls (`registerSeriesModel`,
//   `registerComponentModel`…), whose names are properties and survive
//   minifying. `ecModel` came with D4c: the tooltip's chunk, and a chunk of
//   the bars' and lines' modules, held neither of the other two. The install
//   calls came with D4d: the calendar's group is a component loaded on its
//   own, and its chunk held none of the other three.
// - The budget is the gzipped total (zlib level 9, as `gzip -9`) of those
//   chunks. A PR that adds a chart type raises BUDGET by what the type
//   measures, so the cost is in its diff.
// - No chart chunk at all fails too: a build that stopped carrying the markers
//   would otherwise pass with a total of 0.

import { readdirSync, readFileSync, statSync } from "node:fs"
import { join } from "node:path"
import { gzipSync } from "node:zlib"

// 189,926 bytes on 2026-10-11 (bar, line, area, heatmap and their components).
// 215,866 later that day with D4b's types: 9,060 for the pie and funnel series
// in a group of their own, loaded by the first chart that draws one, and
// 16,880 for the combo, 100%, waterfall, histogram and scorecard option code in
// the chunks every chart loads (no ECharts module there is new). Plus about 5%
// so a patch release of ECharts does not fail it.
// Those totals left out the tooltip's chunk, 9,512 bytes: counted with `ecModel`,
// D4b is 225,378. 238,751 with D4c's scatter and bubble: 2,808 for the scatter
// series in a group of its own, loaded by the first chart that draws one; 4,827
// for the image export's chunk, which now holds a copy of that series (Turbopack
// copies a module that small into the chunk that needs it) and so counts; and
// 5,738 in the chunks every chart loads, the scatter and bubble option code and
// the bars' and lines' modules a scatter shares, now a chunk of their own. Plus
// about 5%.
// 284,652 with D4d's eight types (D4c measures the same 238,751 with the install
// calls counted). 36,183 for four groups of their own, each loaded by the first
// chart that draws one: the treemap and sunburst 13,850, the sankey 11,514, the
// box plot and candlestick 6,162, the calendar 4,657; the bullet and the range
// bands are bars, lines and the scatter's group, no module of their own. The
// image export no longer holds a copy of a series: it loads the chart's group,
// so its 4,827-byte chunk is gone. The graphic and data modules the new groups
// share left the core chunk (127,474, from 133,798) for chunks of their own:
// 6,759 for the data modules, and the graphic modules twice, 7,404 and 7,226,
// one copy for each set of pages (no page loads both; this check counts both).
// The other chunks moved by 520 less in all. Plus about 5%.
// 359,035 with D4e's map. 70,280 for its group, two chunks loaded together by
// the first map drawn: 65,378 for the map series, the geo modules it draws
// with, topojson-client and the world and US-state shapes (on their own the
// shapes gzip to 38,995 and 9,629), and 4,902 for the pan-and-zoom helpers the
// series is built with. 3,363 for the ISO code table a saved map's rows are
// read with: the page reads a row's code before any map loads, so the table is
// in the page's code, and Turbopack put one copy in a graphic modules' chunk
// (7,404 to 10,767); the other pages' copies are in chunks with no ECharts code,
// which this check does not count. The other chunks moved by 740 in all. Plus
// about 5%.
const BUDGET = 377_000
const CHUNKS = ".next/static/chunks"
const ECHARTS = /__ec_|\becModel\b|["']series\.[a-z]|\bregister(SeriesModel|ChartView|ComponentModel|ComponentView|CoordinateSystem)\b/

function files(dir) {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    return statSync(path).isDirectory() ? files(path) : path.endsWith(".js") ? [path] : []
  })
}

let all
try {
  all = files(CHUNKS)
} catch (err) {
  console.error(`check-chart-bundle-size: no ${CHUNKS} (${err.code}); run it in frontend/ after npm run build.`)
  process.exit(1)
}

const charts = all
  .map((path) => ({ path, src: readFileSync(path) }))
  .filter(({ src }) => ECHARTS.test(src.toString("utf8")))
  .map(({ path, src }) => ({ path, raw: src.length, gzip: gzipSync(src, { level: 9 }).length }))

if (charts.length === 0) {
  console.error(`check-chart-bundle-size: none of the ${all.length} chunks holds ECharts code; the markers need updating.`)
  process.exit(1)
}

const total = charts.reduce((sum, c) => sum + c.gzip, 0)
for (const c of charts) console.log(`${c.path}  ${c.raw} bytes, ${c.gzip} gzipped`)
console.log(`chart chunks: ${total} bytes gzipped, budget ${BUDGET}`)
if (total > BUDGET) {
  console.error(
    `check-chart-bundle-size: the chart chunks are ${total - BUDGET} bytes over budget. ` +
      `A bare "echarts" import (next.config.ts aliases it), or a type registered on every page? ` +
      `If a new chart type is the cost, raise BUDGET in this script by what it measures.`,
  )
  process.exit(1)
}
