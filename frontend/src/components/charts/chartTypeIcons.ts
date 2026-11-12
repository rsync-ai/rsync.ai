import {
  AlignHorizontalSpaceAround,
  CalendarDays,
  ChartArea,
  ChartBar,
  ChartCandlestick,
  ChartColumn,
  ChartColumnBig,
  ChartGantt,
  ChartLine,
  ChartPie,
  ChartScatter,
  ChartSpline,
  CircleDot,
  Donut,
  Earth,
  Funnel,
  Grid3x3,
  Hash,
  LayoutPanelLeft,
  Sheet,
  Split,
  Sun,
  Table2,
  Target,
  type LucideIcon,
} from "lucide-react"

import { SERIES_COLORS } from "@/components/charts/chart-theme"
import { one, type ChartType, type ServerChartSettings } from "@/lib/api/charts"

/** Each chart type's icon: the Charts list, the type picker and a chart's card draw it the same way. */
export const CHART_TYPE_ICON: Record<ChartType, LucideIcon> = {
  line: ChartLine,
  area: ChartArea,
  bar: ChartColumn,
  hbar: ChartBar,
  pie: ChartPie,
  donut: Donut,
  // Steps that float, each from where the last ended.
  waterfall: ChartGantt,
  funnel: Funnel,
  histogram: ChartColumnBig,
  heatmap: Grid3x3,
  scatter: ChartScatter,
  // A point of a size: a bubble.
  bubble: CircleDot,
  scorecard: Hash,
  table: Table2,
  // Rows by columns, with a total row and column.
  pivot: Sheet,
  // A large part beside smaller ones.
  treemap: LayoutPanelLeft,
  sunburst: Sun,
  // One flow splitting in two.
  sankey: Split,
  calendar: CalendarDays,
  bullet: Target,
  // A box between two whiskers.
  boxplot: AlignHorizontalSpaceAround,
  candlestick: ChartCandlestick,
  // A band between two curves.
  range: ChartSpline,
  map: Earth,
}

/**
 * The CSS colour of a chart's own series token ("series-3" → hsl(var(--chart-series-3))),
 * with an alpha for a wash. A chart with no colour set, or one the list does not know,
 * takes the first series colour, as the chart itself does.
 */
export function chartSeriesColor(settings: ServerChartSettings, alpha?: number): string {
  const token = one(settings, "measure")?.color
  const name = token && (SERIES_COLORS as readonly string[]).includes(token) ? token : SERIES_COLORS[0]
  return alpha === undefined ? `hsl(var(--chart-${name}))` : `hsl(var(--chart-${name}) / ${alpha})`
}
