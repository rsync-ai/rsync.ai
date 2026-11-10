/**
 * The chart type registry (dashboards-design.md § 14.2), as api-gateway's
 * internal/charts/registry.go holds it: one entry per type, with the roles its settings
 * fill, how its rows are read, its caps, whether the server draws it for an email or a
 * PDF, and the sorts it takes. The drawing and the builder read it here;
 * shared/chart_types_golden.json pins it to the gateway's (chart-registry.test.ts), so a
 * type, role or cap added on one side alone fails a test. No ECharts import.
 */

import type { ChartAggregate, ChartRole, ChartSort, ChartType } from "@/lib/api/charts"

type ColumnKind = "any" | "number" | "time" | "text"
type FieldAttr =
  | "column"
  | "aggregate"
  | "grain"
  | "title"
  | "format"
  | "color"
  | "colors"
  | "mark"
  | "axis"
  | "bins"
  | "width"

/** One role of a type: the fewest and most fields it takes, their column kinds and attributes. */
export interface ChartRoleSpec {
  name: ChartRole
  min: number
  max: number
  kinds: ColumnKind[]
  attrs: FieldAttr[]
}

export interface ChartTypeSpec {
  type: ChartType
  label: string
  roles: ChartRoleSpec[]
  /**
   * "aggregate": grouped and measured in SQL; "statistics": a histogram's bins, a box
   * plot's quartiles or a candlestick's prices worked out in SQL; "rows": the rows as they are.
   */
  path: "aggregate" | "statistics" | "rows"
  /** The most series drawn before the rest are one Other (0: no series), and the most rows. */
  cap: { series: number; rows: number }
  /** The server can draw it for an email or a PDF. */
  static: boolean
  /** Still drawn, never offered for a new chart. */
  deprecated: boolean
  version: number
  stackable: boolean
  sorts: ChartSort[]
  /** A scorecard's comparison with an earlier period. */
  compare: boolean
  /** The aggregates its measure takes: a pie's slices and a waterfall's steps add up, so a count or a sum. */
  aggregates: ChartAggregate[]
}

const ALL_SORTS: ChartSort[] = ["x_asc", "x_desc", "value_asc", "value_desc"]
const ALL_AGGREGATES: ChartAggregate[] = ["count", "count_distinct", "sum", "avg", "min", "max"]
const ADDS_UP: ChartAggregate[] = ["count", "sum"]
const AXIS: FieldAttr[] = ["column", "title", "grain"]
const MEASURE: ChartRoleSpec = {
  name: "measure",
  min: 1,
  max: 1,
  kinds: ["any"],
  attrs: ["column", "aggregate", "title", "format", "color"],
}
/** A bar, line or area chart's one or two measures: two make a combo, each with a mark and an axis. */
const COMBO_MEASURE: ChartRoleSpec = { ...MEASURE, max: 2, attrs: [...MEASURE.attrs, "mark", "axis"] }
/** A measure drawn in one colour per slice or step, not its own. */
const PLAIN_MEASURE: ChartRoleSpec = { ...MEASURE, attrs: ["column", "aggregate", "title", "format"] }
const SERIES: ChartRoleSpec = { name: "series", min: 0, max: 1, kinds: ["any"], attrs: ["column", "colors"] }
const X: ChartRoleSpec = { name: "x", min: 1, max: 1, kinds: ["any"], attrs: AXIS }
/** A scatter's or a bubble's x, y and size, number columns, a point a row; its colour a text column. */
const POINT = (name: ChartRole, min = 1): ChartRoleSpec => ({ name, min, max: 1, kinds: ["number"], attrs: ["column", "title"] })
const POINT_COLOUR: ChartRoleSpec = { name: "series", min: 0, max: 1, kinds: ["text"], attrs: ["column", "colors"] }
/** A pivot table's row and column fields, each grouped by its values or a date grain (§ 14.8). */
const PIVOT_FIELD: FieldAttr[] = ["column", "title", "grain"]
const AGG_CAP = { series: 7, rows: 10_000 }
const ROW_CAP = { series: 0, rows: 10_000 }

/** A type's entry with the defaults most share; each type names what it changes. */
const spec = (type: ChartType, label: string, roles: ChartRoleSpec[], rest: Partial<ChartTypeSpec>): ChartTypeSpec => ({
  type,
  label,
  roles,
  path: "aggregate",
  cap: ROW_CAP,
  static: true,
  deprecated: false,
  version: 2,
  stackable: false,
  sorts: ALL_SORTS,
  compare: false,
  aggregates: ALL_AGGREGATES,
  ...rest,
})

const xy = (type: ChartType, label: string, stackable: boolean, measure = COMBO_MEASURE): ChartTypeSpec =>
  spec(type, label, [X, SERIES, measure], { cap: AGG_CAP, stackable })

/** A scatter or a bubble: the rows as they are, a colour past 7 folded into Other. */
const points = (type: ChartType, label: string, sizeMin: number): ChartTypeSpec =>
  spec(type, label, [POINT("x"), POINT("y"), POINT("size", sizeMin), POINT_COLOUR], {
    path: "rows",
    cap: AGG_CAP,
    sorts: [],
    aggregates: [],
  })

/** A treemap's or a sunburst's levels, outermost first, and what each part measures; the largest 3,500 parts drawn. */
const parts = (type: ChartType, label: string): ChartTypeSpec =>
  spec(type, label, [{ name: "rows", min: 1, max: 3, kinds: ["any"], attrs: ["column", "title"] }, PLAIN_MEASURE], {
    cap: { series: 0, rows: 3500 },
    sorts: [],
    aggregates: ADDS_UP,
  })

/** A pie's or a donut's slices are its series, folded past 7 into Other; no x. */
const slices = (type: ChartType, label: string): ChartTypeSpec =>
  spec(type, label, [{ ...SERIES, min: 1 }, PLAIN_MEASURE], { cap: AGG_CAP, sorts: [], aggregates: ADDS_UP })

/** Every chart type, in the order the chart page offers them. */
export const CHART_REGISTRY: readonly ChartTypeSpec[] = [
  xy("bar", "Bar", true),
  xy("hbar", "Horizontal bar", true, MEASURE),
  xy("line", "Line", false),
  xy("area", "Area", true),
  slices("pie", "Pie"),
  slices("donut", "Donut"),
  spec("waterfall", "Waterfall", [X, PLAIN_MEASURE], { aggregates: ADDS_UP }),
  spec("funnel", "Funnel", [{ ...X, attrs: ["column", "title"] }, MEASURE], {}),
  spec(
    "histogram",
    "Histogram",
    [{ name: "x", min: 1, max: 1, kinds: ["number"], attrs: ["column", "title", "bins", "width"] }],
    { path: "statistics", sorts: [], aggregates: [] },
  ),
  spec("heatmap", "Heatmap", [X, { name: "y", min: 1, max: 1, kinds: ["any"], attrs: AXIS }, MEASURE], {
    sorts: ["x_asc", "x_desc"],
  }),
  points("scatter", "Scatter", 0),
  points("bubble", "Bubble", 1),
  spec("scorecard", "Scorecard", [MEASURE, { name: "date", min: 0, max: 1, kinds: ["time"], attrs: ["column"] }], {
    sorts: [],
    compare: true,
  }),
  spec("table", "Table", [{ name: "columns", min: 1, max: 20, kinds: ["any"], attrs: ["column"] }, { ...MEASURE, min: 0 }], {}),
  spec(
    "pivot",
    "Pivot table",
    [
      { name: "rows", min: 1, max: 3, kinds: ["any"], attrs: PIVOT_FIELD },
      { name: "columns", min: 0, max: 2, kinds: ["any"], attrs: PIVOT_FIELD },
      { ...PLAIN_MEASURE, max: 3 },
    ],
    { sorts: [] },
  ),
  parts("treemap", "Treemap"),
  parts("sunburst", "Sunburst"),
  spec(
    "sankey",
    "Sankey",
    [
      { ...X, attrs: ["column", "title"] },
      { name: "y", min: 1, max: 1, kinds: ["any"], attrs: ["column", "title"] },
      PLAIN_MEASURE,
    ],
    { sorts: [], aggregates: ADDS_UP },
  ),
  spec("calendar", "Calendar heatmap", [{ ...X, kinds: ["time"], attrs: ["column", "title"] }, PLAIN_MEASURE], {
    cap: { series: 0, rows: 366 },
    sorts: [],
  }),
  // A bullet's target is its second measure or a typed number, exactly one of the two.
  spec("bullet", "Bullet", [{ ...MEASURE, max: 2 }], { sorts: [] }),
  spec("boxplot", "Box plot", [X, { name: "y", min: 1, max: 1, kinds: ["number"], attrs: ["column", "title", "format"] }], {
    path: "statistics",
    sorts: ["x_asc", "x_desc"],
    aggregates: [],
  }),
  // A candlestick's y is its open, high, low and close columns, in that order.
  spec("candlestick", "Candlestick", [{ ...X, kinds: ["time"] }, { name: "y", min: 4, max: 4, kinds: ["number"], attrs: ["column"] }], {
    path: "statistics",
    sorts: [],
    aggregates: [],
  }),
  spec("range", "Range bands", [X, { ...PLAIN_MEASURE, min: 2, max: 2 }], { sorts: ["x_asc", "x_desc"] }),
  // A map's x is a country's or a US state's ISO code; which of the two is the settings' map.
  spec("map", "Choropleth map", [{ ...X, kinds: ["text", "number"], attrs: ["column", "title"] }, MEASURE], { sorts: [] }),
]

/**
 * What an email or a PDF says in place of a chart whose type is not static
 * (dashboards-design.md § 14.5), as the gateway's charts.NotStatic says it.
 */
export const NOT_STATIC = "This kind of chart is drawn only in rsync.ai, not in an email or a PDF."

/** A type's registry entry; undefined for a type rsync.ai does not draw. */
export function chartSpec(type: string): ChartTypeSpec | undefined {
  return CHART_REGISTRY.find((t) => t.type === type)
}

/** The server draws it for an email or a PDF: a type in the registry marked static. */
export const isStatic = (type: string): boolean => chartSpec(type)?.static ?? false

/** The most series a type draws before the rest are one Other; 0 when it has no series. */
export const seriesCap = (type: string): number => chartSpec(type)?.cap.series ?? 0
