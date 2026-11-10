// @vitest-environment node
import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { isDeepStrictEqual } from "node:util"
import { describe, expect, it } from "vitest"

import {
  BOX_ROLES,
  PRICE_ROLES,
  ROLE_BIN_WIDTH,
  ROLE_OTHER,
  ROLE_POINT_ROWS,
  ROLE_SECOND_MEASURE,
  ROLE_SERIES_COUNT,
  ROLE_SIZE,
  ROLE_THIRD_MEASURE,
  levelRole,
  pivotRole,
  pivotTotalRole,
  savedChartView,
} from "@/components/charts/saved-chart"
import { CHART_REGISTRY } from "@/components/charts/chart-registry"
import type { ChartTheme } from "@/components/charts/chart-theme"
import type { ChartResult, ServerChartSettings } from "@/lib/api/charts"
import { chartImage } from "@/lib/export/chart-image"
import { chartSvg, type MeasureText } from "@/lib/export/chart-svg"
import { printChartTheme } from "@/lib/export/print-theme"

// Each chart type as it is drawn, pinned as golden files (dashboards-design.md § 14.9, D4a
// to D4e): a saved chart's settings and the rows the gateway read go in; the view the
// page draws and the SVG the server draws for a PDF or an email come out. The theme, the
// size and the text widths are fixed, so a change to how a chart is drawn shows up as a
// diff to review. `npx vitest run src/__tests__/chart-golden.test.ts -u` rewrites them.
//
// The files were written from version 1 settings before settings by role (§ 14.2). Each
// chart is now drawn from the version 2 that migration 147 and the gateway's reader make
// of its version 1 (shared/chart_settings_v1_v2.json), and a series chart's rows carry the
// gateway's Other flag and series count, so the files not changing is the proof that the
// seven types draw as they did. The types D4b to D4e added have only version 2 settings.

const UPGRADES: { v1: object; v2: ServerChartSettings }[] = JSON.parse(
  readFileSync(resolve(__dirname, "../../../shared/chart_settings_v1_v2.json"), "utf8"),
).cases

/** The version 2 of a version 1 chart, from the shared cases; a v1 not there fails the test. */
function v2Of(v1: object): ServerChartSettings {
  const c = UPGRADES.find((u) => isDeepStrictEqual(u.v1, v1))
  if (!c) throw new Error(`no case in shared/chart_settings_v1_v2.json has the v1 ${JSON.stringify(v1)}`)
  return c.v2
}

const result = (columns: [alias: string, role: string][], rows: unknown[][], masked: string[] = []): ChartResult => ({
  columns: columns.map(([alias, role]) => ({ alias, role })),
  rows,
  truncated: false,
  masked,
  unfiltered: [],
  sql: "",
  read_at: "",
  execution_time_ms: 0,
})

const DAYS = ["2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z"]
const REGIONS = ["EU", "US", "APAC"]
const bySeries = (value: (d: number, r: number) => number) =>
  DAYS.flatMap((d, i) => REGIONS.map((r, j) => [d, r, String(value(i, j))]))
/** A series chart's rows as the gateway reads them: none folded into Other, three series. */
const withOther = (rows: unknown[][]) => rows.map((row) => [...row, 0, REGIONS.length])
const OTHER_COLUMNS: [string, string][] = [["rsync_other", ROLE_OTHER], ["rsync_series", ROLE_SERIES_COUNT]]
/** A scatter's rows at each point, its last column. */
const POINT_ROWS: [string, string] = ["rsync_rows", ROLE_POINT_ROWS]

/** The pivot's rows from nine seeded orders, as Postgres returns them (the totals equal a hand count). */
const PIVOT_ROWS: unknown[][] = [
  [null, null, null, "1070", "118.88888888888889", 9, 1, 1, 1],
  [null, null, "store", "450", "90.0", 5, 1, 1, 0],
  [null, null, "web", "620", "155.0", 4, 1, 1, 0],
  ["EU", null, null, "500", "100.0", 5, 0, 1, 1],
  ["US", null, null, "570", "142.5", 4, 0, 1, 1],
  ["EU", null, "store", "180", "90.0", 2, 0, 1, 0],
  ["EU", null, "web", "320", "106.66666666666667", 3, 0, 1, 0],
  ["US", null, "store", "270", "90.0", 3, 0, 1, 0],
  ["US", null, "web", "300", "300.0", 1, 0, 1, 0],
  ["EU", "2026-09-01T00:00:00Z", null, "240", "80.0", 3, 0, 0, 1],
  ["EU", "2026-10-01T00:00:00Z", null, "260", "130.0", 2, 0, 0, 1],
  ["US", "2026-09-01T00:00:00Z", null, "540", "180.0", 3, 0, 0, 1],
  ["US", "2026-10-01T00:00:00Z", null, "30", "30.0", 1, 0, 0, 1],
  ["EU", "2026-09-01T00:00:00Z", "store", "120", "120.0", 1, 0, 0, 0],
  ["EU", "2026-09-01T00:00:00Z", "web", "120", "60.0", 2, 0, 0, 0],
  ["EU", "2026-10-01T00:00:00Z", "store", "60", "60.0", 1, 0, 0, 0],
  ["EU", "2026-10-01T00:00:00Z", "web", "200", "200.0", 1, 0, 0, 0],
  ["US", "2026-09-01T00:00:00Z", "store", "240", "120.0", 2, 0, 0, 0],
  ["US", "2026-09-01T00:00:00Z", "web", "300", "300.0", 1, 0, 0, 0],
  ["US", "2026-10-01T00:00:00Z", "store", "30", "30.0", 1, 0, 0, 0],
]

/** The nine orders by region and channel, largest first, as the gateway sorts a treemap's parts (§ 14.5). */
const PART_ROWS: unknown[][] = [
  ["EU", "web", "320"],
  ["US", "web", "300"],
  ["US", "store", "270"],
  ["EU", "store", "180"],
]

/** 34 days from 2026-09-01, one a row, each day's count a fixed pattern; a weekend is quieter. */
const CALENDAR_ROWS: unknown[][] = Array.from({ length: 34 }, (_, i) => {
  const day = new Date(Date.UTC(2026, 8, 1 + i))
  const weekend = day.getUTCDay() === 0 || day.getUTCDay() === 6
  return [day.toISOString(), (weekend ? 2 : 8) + ((i * 7) % 11)]
})

/** `zeroMarks`: the marks the chart draws with no height on purpose, such as a histogram's empty bins. */
const CASES: ({ name: string; result: ChartResult; zeroMarks?: number } & ({ v1: object } | { v2: ServerChartSettings }))[] = [
  {
    name: "bar_sum",
    v1: { type: "bar", x: { column: "day" }, measure: { aggregate: "sum", column: "amount" } },
    result: result(
      [["day", "the x"], ["amount", "the measure"]],
      DAYS.map((d, i) => [d, String(1200 + i * 350)]),
    ),
  },
  {
    name: "bar_series_stacked",
    v1: {
      type: "bar",
      x: { column: "day", title: "Day" },
      measure: { aggregate: "count" },
      series: { column: "region", colors: [{ value: "EU", color: "series-2" }] },
      stacked: true,
    },
    result: result(
      [["day", "the x"], ["region", "the series"], ["count", "the measure"], ...OTHER_COLUMNS],
      withOther(bySeries((d, r) => 10 + d * 3 + r * 7)),
    ),
  },
  {
    name: "hbar_avg_sorted",
    v1: { type: "hbar", x: { column: "region" }, measure: { aggregate: "avg", column: "amount", format: "decimal" }, sort: "value_asc" },
    result: result(
      [["region", "the x"], ["amount", "the measure"]],
      [["APAC", "41.25"], ["US", "57.5"], ["EU", "63.125"], [null, "12"]],
    ),
  },
  {
    name: "line_count_distinct",
    v1: { type: "line", x: { column: "day" }, measure: { aggregate: "count_distinct", column: "customer_id", title: "Customers" } },
    result: result(
      [["day", "the x"], ["count_distinct", "the measure"]],
      DAYS.map((d, i) => [d, 30 + ((i * 17) % 11)]),
    ),
  },
  {
    name: "area_series",
    v1: { type: "area", x: { column: "day" }, measure: { aggregate: "max", column: "amount" }, series: { column: "region" }, sort: "x_desc" },
    result: result(
      [["day", "the x"], ["region", "the series"], ["amount", "the measure"], ...OTHER_COLUMNS],
      withOther(bySeries((d, r) => 500 - d * 40 + r * 90)),
    ),
  },
  {
    name: "heatmap",
    v1: { type: "heatmap", x: { column: "day" }, y: { column: "region" }, measure: { aggregate: "min", column: "amount", color: "series-4" } },
    result: result([["day", "the x"], ["region", "the y"], ["amount", "the measure"]], bySeries((d, r) => (d + 1) * (r + 2))),
  },
  {
    name: "scorecard_plain",
    v1: { type: "scorecard", measure: { aggregate: "sum", column: "amount", format: "compact" } },
    result: result([["amount", "the measure"]], [["1234567.5"]]),
  },
  {
    name: "scorecard_compare",
    v1: { type: "scorecard", measure: { aggregate: "count" }, compare: { column: "day", days: 7 } },
    result: result([["count", "the measure"], ["previous", "the earlier period"]], [["420", "350"]]),
  },
  {
    name: "table_rows",
    v1: { type: "table", columns: ["day", "region", "amount"], sort: "x_desc" },
    result: result(
      [["day", "table column day"], ["region", "table column region"], ["amount", "table column amount"]],
      [[DAYS[1], "EU", "12.5"], [DAYS[0], null, "7"]],
    ),
  },
  {
    name: "table_grouped",
    v1: { type: "table", columns: ["region", "day"], measure: { aggregate: "sum", column: "amount" }, sort: "value_desc" },
    result: result(
      [["region", "table column region"], ["day", "table column day"], ["amount", "the measure"]],
      [["US", DAYS[0], "900"], ["EU", DAYS[0], "450.75"]],
    ),
  },
  {
    name: "bar_masked",
    v1: { type: "bar", x: { column: "region" }, measure: { aggregate: "sum", column: "salary" } },
    result: result([["region", "the x"], ["salary", "the measure"]], [["EU", null]], ["salary"]),
  },
  // D4b: a combo, 100% stacks, pie, donut, waterfall, funnel, histogram and a sparkline.
  {
    name: "combo_two_axes",
    v2: {
      version: 2,
      type: "bar",
      roles: {
        x: [{ column: "day" }],
        measure: [
          { aggregate: "sum", column: "amount", title: "Revenue", format: "compact" },
          { aggregate: "count", title: "Orders", mark: "line", axis: "right" },
        ],
      },
    },
    result: result(
      [["day", "the x"], ["amount", "the measure"], ["count", ROLE_SECOND_MEASURE]],
      DAYS.map((d, i) => [d, String(12000 + i * 3500), 40 + i * 9]),
    ),
  },
  {
    name: "bar_percent",
    v2: {
      version: 2,
      type: "bar",
      roles: { x: [{ column: "day" }], series: [{ column: "region" }], measure: [{ aggregate: "count" }] },
      stacked: true,
      percent: true,
    },
    result: result(
      [["day", "the x"], ["region", "the series"], ["count", "the measure"], ...OTHER_COLUMNS],
      withOther(bySeries((d, r) => 10 + d * 3 + r * 7)),
    ),
  },
  {
    name: "area_percent",
    v2: {
      version: 2,
      type: "area",
      roles: { x: [{ column: "day" }], series: [{ column: "region" }], measure: [{ aggregate: "sum", column: "amount" }] },
      stacked: true,
      percent: true,
    },
    result: result(
      [["day", "the x"], ["region", "the series"], ["amount", "the measure"], ...OTHER_COLUMNS],
      withOther(bySeries((d, r) => 500 - d * 40 + r * 90)),
    ),
  },
  {
    name: "pie_other",
    v2: {
      version: 2,
      type: "pie",
      roles: {
        series: [{ column: "country", colors: [{ value: "Germany", color: "series-5" }] }],
        measure: [{ aggregate: "sum", column: "amount", format: "compact" }],
      },
    },
    result: result(
      [["country", "the series"], ["amount", "the measure"], ...OTHER_COLUMNS],
      [
        ["United States", "52000", 0, 10],
        ["Germany", "31000", 0, 10],
        ["India", "24000", 0, 10],
        ["Brazil", "18000", 0, 10],
        ["Japan", "15000", 0, 10],
        ["France", "9000", 0, 10],
        ["Kenya", "0", 0, 10],
        [null, "11000", 1, 10],
      ],
    ),
  },
  {
    name: "donut",
    v2: {
      version: 2,
      type: "donut",
      roles: { series: [{ column: "plan" }], measure: [{ aggregate: "count", title: "Customers" }] },
    },
    result: result(
      [["plan", "the series"], ["count", "the measure"], ...OTHER_COLUMNS],
      [
        ["Team", 410, 0, 3],
        ["Free", 260, 0, 3],
        ["Enterprise", 85, 0, 3],
      ],
    ),
  },
  {
    name: "waterfall",
    v2: {
      version: 2,
      type: "waterfall",
      roles: { x: [{ column: "step", title: "Step" }], measure: [{ aggregate: "sum", column: "amount", title: "Cash" }] },
      sort: "x_asc",
    },
    result: result(
      [["step", "the x"], ["amount", "the measure"]],
      [
        ["1 Opening", "1000"],
        ["2 Sales", "650"],
        ["3 Refunds", "-180"],
        ["4 Payroll", "-720"],
        ["5 Interest", "45"],
      ],
    ),
  },
  {
    name: "funnel",
    v2: {
      version: 2,
      type: "funnel",
      roles: { x: [{ column: "stage", title: "Stage" }], measure: [{ aggregate: "count_distinct", column: "user_id", title: "Users" }] },
      sort: "value_desc",
    },
    result: result(
      [["stage", "the x"], ["count_distinct", "the measure"]],
      [
        ["Visited", 1000],
        ["Signed up", 420],
        ["Activated", 180],
        ["Paid", 60],
      ],
    ),
  },
  {
    // Ten bins from 0 to 500, rows in five of them: the empty ones are drawn as gaps.
    name: "histogram_bins",
    // 150–200 and 250–450 hold no row: five bins drawn at a count of 0.
    zeroMarks: 5,
    v2: { version: 2, type: "histogram", roles: { x: [{ column: "amount", title: "Order amount", bins: 10 }] } },
    result: result(
      [["amount", "the x"], ["rsync_bin_width", ROLE_BIN_WIDTH], ["count", "the measure"]],
      [
        [0, 50, 12],
        [50, 50, 30],
        [100, 50, 41],
        [200, 50, 9],
        [450, 50, 2],
      ],
    ),
  },
  {
    // A width of 0.1 from zero: the edges read 0.3, not 0.30000000000000004.
    name: "histogram_width",
    v2: { version: 2, type: "histogram", roles: { x: [{ column: "rate", width: 0.1 }] } },
    result: result(
      [["rate", "the x"], ["rsync_bin_width", ROLE_BIN_WIDTH], ["count", "the measure"]],
      [
        ["0.1", "0.1", "7"],
        ["0.2", "0.1", "19"],
        ["0.30000000000000004", "0.1", "11"],
      ],
    ),
  },
  {
    name: "scorecard_sparkline",
    v2: {
      version: 2,
      type: "scorecard",
      roles: { measure: [{ aggregate: "sum", column: "amount", format: "compact" }], date: [{ column: "day" }] },
      compare: { days: 7 },
      sparkline: true,
    },
    result: {
      ...result([["amount", "the measure"], ["previous", "the earlier period"]], [["48200", "41000"]]),
      spark: DAYS.map((d, i) => [d.slice(0, 10), String(6000 + i * 900)]),
    },
  },
  // D4c: scatter, bubble and the pivot table.
  {
    // The rows as the gateway reads them: a point a spot with its rows, no x or y missing,
    // the most rows first, then by x and y.
    name: "scatter",
    v2: {
      version: 2,
      type: "scatter",
      roles: { x: [{ column: "amount", title: "Order amount" }], y: [{ column: "discount", title: "Discount" }] },
    },
    result: result(
      [["amount", "the x"], ["discount", "the y"], POINT_ROWS],
      [
        ["40", "4", 5],
        ["12.5", "0", 2],
        ["40", "2.5", 1],
        ["75.25", "6", 1],
        ["120", "9.5", 1],
        ["180", "12", 1],
        ["260", "20", 1],
      ],
    ),
  },
  {
    // Coloured by region, EU's colour pinned; a size of 0 has no bubble. Other, past seven
    // colours, is in saved-chart.test.ts.
    name: "bubble",
    v2: {
      version: 2,
      type: "bubble",
      roles: {
        x: [{ column: "amount" }],
        y: [{ column: "quantity" }],
        size: [{ column: "discount", title: "Discount" }],
        series: [{ column: "region", colors: [{ value: "EU", color: "series-3" }] }],
      },
    },
    result: result(
      [["amount", "the x"], ["quantity", "the y"], ["discount", ROLE_SIZE], ["region", "the series"], ...OTHER_COLUMNS, POINT_ROWS],
      [
        ["90", "6", "12", "APAC", 0, 3, 4],
        ["20", "1", "2", "EU", 0, 3, 2],
        ["35", "4", "0", "US", 0, 3, 1],
        ["60", "2", "5", "US", 0, 3, 1],
        ["110", "3", "8", "APAC", 0, 3, 1],
        ["150", "8", "20", "EU", 0, 3, 1],
        ["210", "5", "3", "US", 0, 3, 1],
      ],
    ),
  },
  {
    // One row field and no column field: a row a region and the total last. The total's
    // average is the source's, 1070 / 9, not the average of the two regions' averages.
    name: "pivot_rows",
    v2: {
      version: 2,
      type: "pivot",
      roles: {
        rows: [{ column: "region", title: "Region" }],
        measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }, { aggregate: "avg", column: "amount", format: "decimal" }],
      },
    },
    result: result(
      [["region", pivotRole("row", 1)], ["amount", "the measure"], ["amount_2", ROLE_SECOND_MEASURE], ["rsync_total_r1", pivotTotalRole("row", 1)]],
      [
        [null, "1070", "118.88888888888889", 1],
        ["EU", "500", "100.0", 0],
        ["US", "570", "142.5", 0],
      ],
    ),
  },
  {
    // Two row fields and a column field, as GROUPING SETS returns them, totals first: each
    // region's subtotal after its months, a column a channel then the channels' total.
    name: "pivot_rows_columns",
    v2: {
      version: 2,
      type: "pivot",
      roles: {
        rows: [{ column: "region" }, { column: "day", grain: "month" }],
        columns: [{ column: "channel" }],
        measure: [
          { aggregate: "sum", column: "amount" },
          { aggregate: "avg", column: "amount", format: "decimal" },
          { aggregate: "count" },
        ],
      },
    },
    result: result(
      [
        ["region", pivotRole("row", 1)],
        ["day", pivotRole("row", 2)],
        ["channel", pivotRole("column", 1)],
        ["amount", "the measure"],
        ["amount_2", ROLE_SECOND_MEASURE],
        ["count", ROLE_THIRD_MEASURE],
        ["rsync_total_r1", pivotTotalRole("row", 1)],
        ["rsync_total_r2", pivotTotalRole("row", 2)],
        ["rsync_total_c1", pivotTotalRole("column", 1)],
      ],
      PIVOT_ROWS,
    ),
  },
  {
    name: "treemap_two_levels",
    v2: {
      version: 2,
      type: "treemap",
      roles: { rows: [{ column: "region", title: "Region" }, { column: "channel" }], measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }] },
    },
    result: result([["region", levelRole(1)], ["channel", levelRole(2)], ["amount", "the measure"]], PART_ROWS),
  },
  {
    name: "sunburst_two_levels",
    v2: {
      version: 2,
      type: "sunburst",
      roles: { rows: [{ column: "region", title: "Region" }, { column: "channel" }], measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }] },
    },
    result: result([["region", levelRole(1)], ["channel", levelRole(2)], ["amount", "the measure"]], PART_ROWS),
  },
  {
    // Largest flow first, as the gateway sorts them: the nodes are placed in the order the
    // flows first name them, never moved.
    name: "sankey_flows",
    v2: {
      version: 2,
      type: "sankey",
      roles: { x: [{ column: "from", title: "From page" }], y: [{ column: "to", title: "To page" }], measure: [{ aggregate: "count" }] },
    },
    result: result(
      [["from", "the x"], ["to", "the y"], ["count", "the measure"]],
      [
        ["visit", "cart", 50],
        ["visit", "left", 30],
        ["cart", "left", 25],
        ["cart", "paid", 20],
        ["paid", "refund", 2],
      ],
    ),
  },
  {
    name: "calendar_days",
    v2: { version: 2, type: "calendar", roles: { x: [{ column: "day" }], measure: [{ aggregate: "count", title: "Orders" }] } },
    result: result([["day", "the x"], ["count", "the measure"]], CALENDAR_ROWS),
  },
  {
    name: "bullet_target_number",
    v2: { version: 2, type: "bullet", target: 1000, roles: { measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }] } },
    result: result([["amount", "the measure"]], [["800"]]),
  },
  {
    name: "bullet_target_measure",
    v2: {
      version: 2,
      type: "bullet",
      roles: { measure: [{ aggregate: "sum", column: "amount", title: "Revenue" }, { aggregate: "max", column: "goal", title: "Goal" }] },
    },
    result: result([["amount", "the measure"], ["goal", ROLE_SECOND_MEASURE]], [["1070", "950"]]),
  },
  {
    // The quartiles as the gateway reads them, PERCENTILE_CONT per plan.
    name: "boxplot_by_plan",
    v2: { version: 2, type: "boxplot", roles: { x: [{ column: "plan" }], y: [{ column: "amount", title: "Order value", format: "integer" }] } },
    result: result(
      [["plan", "the x"], ...BOX_ROLES.map((r, i): [string, string] => [`rsync_box_${i}`, r])],
      [
        ["free", "10", "25", "40", "60", "150"],
        ["pro", "60", "90", "120", "180", "300"],
        ["team", "200", "240", "260", "320", "500"],
      ],
    ),
  },
  {
    // A month that rose, one that fell and one that closed where it opened.
    name: "candlestick_by_month",
    v2: {
      version: 2,
      type: "candlestick",
      roles: { x: [{ column: "day", grain: "month" }], y: ["open", "high", "low", "close"].map((column) => ({ column })) },
    },
    result: result(
      [["day", "the x"], ...PRICE_ROLES.map((r, i): [string, string] => [`rsync_${["open", "high", "low", "close"][i]}`, r])],
      [
        ["2026-07-01T00:00:00Z", "100", "130", "95", "120"],
        ["2026-08-01T00:00:00Z", "120", "125", "80", "90"],
        ["2026-09-01T00:00:00Z", "90", "110", "85", "90"],
        ["2026-10-01T00:00:00Z", "90", "140", "88", "135"],
      ],
    ),
  },
  {
    // The third day has no low, so the band has a gap there.
    name: "range_by_day",
    v2: {
      version: 2,
      type: "range",
      roles: {
        x: [{ column: "day" }],
        measure: [
          { aggregate: "min", column: "amount", title: "Low" },
          { aggregate: "max", column: "amount", title: "High" },
        ],
      },
    },
    result: result(
      [["day", "the x"], ["amount", "the measure"], ["amount_2", ROLE_SECOND_MEASURE]],
      [
        [DAYS[0], "40", "120"],
        [DAYS[1], "55", "160"],
        [DAYS[2], null, "140"],
        [DAYS[3], "30", "90"],
      ],
    ),
  },
  {
    // Largest first, as the gateway sorts a map: codes of each kind, a name (never matched)
    // and France twice (FR after FRA is left out). 276 is Germany's numeric code.
    name: "map_world",
    v2: {
      version: 2,
      type: "map",
      map: "world",
      roles: { x: [{ column: "country" }], measure: [{ aggregate: "sum", column: "amount", title: "Revenue", format: "integer" }] },
    },
    result: result(
      [["country", "the x"], ["amount", "the measure"]],
      [
        ["US", "1200"],
        ["FRA", "800"],
        [276, "650"],
        ["in", "500"],
        ["BR", "300"],
        ["FR", "200"],
        ["JP", "150"],
        ["United Kingdom", "90"],
        ["AU", "40"],
      ],
    ),
  },
  {
    // A state by "US-CA" or "CA", in any case; Alaska and Hawaii drawn under the lower 48.
    name: "map_us_states",
    v2: {
      version: 2,
      type: "map",
      map: "us_states",
      roles: { x: [{ column: "state", title: "State" }], measure: [{ aggregate: "count", title: "Customers" }] },
    },
    result: result(
      [["state", "the x"], ["count", "the measure"]],
      [["US-CA", 900], ["TX", 700], ["ny", 500], ["FL", 350], ["AK", 200], ["HI", 100], ["WA", 60]],
    ),
  },
]

const settingsOf = (c: (typeof CASES)[number]): ServerChartSettings => ("v2" in c ? c.v2 : v2Of(c.v1))

/** Every character a fixed share of its size: the same widths on any machine. */
const measure: MeasureText = (text, size, bold) => text.length * size * (bold ? 0.6 : 0.55)

const LIGHT = printChartTheme()
const DARK: ChartTheme = { ...LIGHT, text: "#f2f2f2", muted: "#a1a1aa", border: "#3f3f46", surface: "#18181b", popover: "#27272a", popoverText: "#fafafa" }
const SIZE = { width: 568, height: 300 }

/**
 * The SVG with zrender's ids and classes ("zr11-cls-42") numbered from 0 in the order they
 * first appear: zrender numbers them across every chart the process drew, so the same
 * chart would otherwise differ by which tests ran before it.
 */
function svg(view: { settings: Parameters<typeof chartSvg>[0]; rows: readonly object[] }, theme: ChartTheme): string {
  const seen = new Map<string, string>()
  const count = new Map<string, number>()
  return chartSvg(view.settings, view.rows, theme, SIZE, measure).replace(/\bzr\d+-([a-z]+)-?\d+/g, (token, kind: string) => {
    if (!seen.has(token)) {
      const n = count.get(kind) ?? 0
      count.set(kind, n + 1)
      seen.set(token, `zr-${kind}-${n}`)
    }
    return seen.get(token)!
  })
}

/** The box a path's points span, from its absolute and relative commands. */
function box(d: string): { width: number; height: number } {
  const ARGS: Record<string, number> = { M: 2, L: 2, H: 1, V: 1, C: 6, S: 4, Q: 4, T: 2, A: 7, Z: 0 }
  const xs: number[] = []
  const ys: number[] = []
  let x = 0
  let y = 0
  for (const [, cmd, args] of d.matchAll(/([a-zA-Z])([^a-zA-Z]*)/g)) {
    const n = args.trim() ? args.trim().split(/[\s,]+/).map(Number) : []
    const step = ARGS[cmd.toUpperCase()]
    const rel = cmd !== cmd.toUpperCase()
    for (let i = 0; step && i + step <= n.length; i += step) {
      const p = n.slice(i, i + step)
      if (cmd.toUpperCase() === "H") x = rel ? x + p[0] : p[0]
      else if (cmd.toUpperCase() === "V") y = rel ? y + p[0] : p[0]
      else {
        x = rel ? x + p[step - 2] : p[step - 2]
        y = rel ? y + p[step - 1] : p[step - 1]
      }
      xs.push(x)
      ys.push(y)
    }
  }
  return { width: Math.max(...xs) - Math.min(...xs), height: Math.max(...ys) - Math.min(...ys) }
}

/**
 * Each shape a series fills: a bar, a slice, a funnel's stage, a heatmap's cell, a part, a
 * sankey's node or flow (moved down by a translate), a box or a candle. A waterfall's
 * floating base is a bar with no fill, and a line's point is a symbol scaled into place by
 * a matrix, so neither is one.
 */
function marks(svg: string): { width: number; height: number }[] {
  return [...svg.matchAll(/<(path|polygon) ([^>]*)>/g)]
    .filter(([, , attrs]) => attrs.includes("ecmeta_series_index=") && /\bfill="(?!none")/.test(attrs) && !attrs.includes('transform="matrix('))
    .map(([, tag, attrs]) =>
      tag === "polygon" ? box(`M${/\bpoints="([^"]*)"/.exec(attrs)![1]}`) : box(/\bd="([^"]*)"/.exec(attrs)![1]),
    )
}

/**
 * Each point a scatter, a bubble chart or a range's edge draws: a circle of radius 1 that a
 * matrix scales to its size and moves into place, so its width and height are twice the scale.
 */
function points(svg: string): { width: number; height: number }[] {
  return [...svg.matchAll(/<path ([^>]*)>/g)]
    .filter(([, attrs]) => attrs.includes("ecmeta_series_index=") && attrs.includes('transform="matrix('))
    .map(([, attrs]) => {
      const [a, , , d] = /\btransform="matrix\(([^)]*)\)"/.exec(attrs)![1].split(",").map(Number)
      return { width: 2 * a, height: 2 * d }
    })
}

describe("each chart type, drawn", () => {
  for (const c of CASES) {
    it(`${c.name}: the view and its SVG are the golden files`, async () => {
      const view = savedChartView(settingsOf(c), c.result)
      await expect(JSON.stringify(view, null, 2) + "\n").toMatchFileSnapshot(`./__goldens__/charts/${c.name}.view.json`)
      if (view.kind !== "echart") return
      await expect(svg(view, LIGHT)).toMatchFileSnapshot(`./__goldens__/charts/${c.name}.light.svg`)
      await expect(svg(view, DARK)).toMatchFileSnapshot(`./__goldens__/charts/${c.name}.dark.svg`)
    })

    it(`${c.name}: the image the server draws has its title and a bar of a height above zero for each value`, async () => {
      const view = savedChartView(settingsOf(c), c.result)
      if (view.kind !== "echart") return
      const image = await chartImage(c.name, view.settings, view.rows, SIZE, measure)
      expect(image.svg).toContain(`<title>${c.name}</title>`)
      // A line or an area is drawn as a line through points: it has no bar to measure.
      if (view.settings.type === "line" || view.settings.type === "area") return
      // A scatter's or a bubble's mark is a point: one for each row the view keeps.
      if (view.settings.type === "scatter" || view.settings.type === "bubble") {
        const drawn = points(image.svg)
        expect(drawn).toHaveLength(view.rows.length)
        expect(drawn.length).toBeGreaterThan(0)
        expect(drawn.filter((m) => m.width > 0 && m.height > 0)).toHaveLength(drawn.length)
        return
      }
      // A range's band is an area under its high edge: a point at each x's low and high.
      if (view.settings.type === "range") {
        const drawn = points(image.svg)
        const bands = (view.rows as { low: number | null; high: number | null }[]).filter((b) => b.low !== null && b.high !== null)
        expect(bands.length).toBeGreaterThan(0)
        expect(drawn).toHaveLength(2 * bands.length)
        expect(drawn.filter((m) => m.width > 0 && m.height > 0)).toHaveLength(drawn.length)
        return
      }
      const drawn = marks(image.svg)
      expect(drawn.length).toBeGreaterThan(0)
      expect(drawn.filter((m) => m.width > 0 && m.height > 0)).toHaveLength(drawn.length - (c.zeroMarks ?? 0))
    })
  }

  it("covers every type the server takes", () => {
    const types = new Set(CASES.map((c) => settingsOf(c).type))
    expect([...types].sort()).toEqual(CHART_REGISTRY.map((t) => t.type).sort())
  })
})
