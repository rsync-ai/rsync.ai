/**
 * The ECharts modules a type needs past the bars, lines and heatmap every chart
 * registers (dashboards-design.md § 14.6). They load in groups, the first time a chart
 * of one of a group's types is drawn, so a page of bars never downloads them.
 */

import type { ChartType } from "@/lib/api/charts"

// A pie's, a donut's and a funnel's series: shapes drawn without a grid.
const shapes = () => import("@/components/charts/echart-shapes")

// A scatter's and a bubble's points, and a bullet chart's target mark.
const points = () => import("@/components/charts/echart-points")

// A treemap's rectangles and a sunburst's rings.
const parts = () => import("@/components/charts/echart-parts")

// A sankey's nodes and flows.
const flows = () => import("@/components/charts/echart-flows")

// A box plot's boxes and a candlestick's candles.
const stats = () => import("@/components/charts/echart-stats")

// A calendar heatmap's calendar.
const calendar = () => import("@/components/charts/echart-calendar")

// A map's series and its world and US-state shapes.
const maps = () => import("@/components/charts/echart-maps")

const GROUP: Partial<Record<ChartType, () => Promise<unknown>>> = {
  pie: shapes,
  donut: shapes,
  funnel: shapes,
  scatter: points,
  bubble: points,
  bullet: points,
  treemap: parts,
  sunburst: parts,
  sankey: flows,
  boxplot: stats,
  candlestick: stats,
  calendar,
  map: maps,
}

const registered = new Set<() => Promise<unknown>>()

/** Resolves once the type's group is registered; null when every chart's modules draw it. */
export function chartModules(type: ChartType): Promise<unknown> | null {
  const group = GROUP[type]
  if (!group) return null
  return group().then(() => {
    registered.add(group)
  })
}

/**
 * How many groups have registered. ECharts copies the layouts and processors registered
 * so far into a chart when it makes one, so a chart made before a group registered never
 * lays out that group's types: a pie throws, a scatter draws no points. EChart makes such
 * a chart again before drawing.
 */
export function registeredGroups(): number {
  return registered.size
}
