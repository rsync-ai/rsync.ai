/**
 * The Table statistics tab's two charts as chart settings (dashboards-design.md
 * § 6): how many tables are in each state, and which tables wrote the most.
 * Bars across, never a donut: the counts read off one axis.
 *
 * No ECharts import here: settings are plain objects, and ECharts loads only with
 * EChartNoSSR.
 */

import type { HBarChartSettings } from "@/components/charts/chart-settings"
import type { ChartColor } from "@/components/charts/chart-theme"

export interface TableStatusCounts {
  completed: number
  /** Tables the API calls running; they read Paused or Stopped when the pipeline is. */
  running: number
  runningLabel: "Running" | "Paused" | "Stopped"
  notWritten: number
  rewound: number
  failed: number
  waitingForData: number
  removed: number
}

interface StatusBar {
  label: string
  count: number
  color: ChartColor
}

export interface StatusRow {
  status: string
  tables: number
}

/** The states with at least one table, in the order the tab names them. */
function statusBars(c: TableStatusCounts): StatusBar[] {
  const bars: StatusBar[] = [
    { label: "Completed", count: c.completed, color: "succeeded" },
    { label: c.runningLabel, count: c.running, color: c.runningLabel === "Running" ? "running" : "cancelled" },
    { label: "Rows not written", count: c.notWritten, color: "error" },
    { label: "Rewound", count: c.rewound, color: "waiting" },
    { label: "Failed", count: c.failed, color: "failed" },
    { label: "No data yet", count: c.waitingForData, color: "skipped" },
    { label: "Removed", count: c.removed, color: "skipped" },
  ]
  return bars.filter((b) => b.count > 0)
}

export function tableStatusRows(c: TableStatusCounts): StatusRow[] {
  return statusBars(c).map((b) => ({ status: b.label, tables: b.count }))
}

/** One bar per state, in that state's colour. */
export function tableStatusChart(c: TableStatusCounts): HBarChartSettings {
  const bars = statusBars(c)
  return {
    type: "hbar",
    // One row per state, each in its own series, so each bar takes its state's colour.
    stacked: true,
    description: `Tables by status: ${bars.map((b) => `${b.count} ${b.label.toLowerCase()}`).join(", ") || "none"}.`,
    x: { field: "status", label: "Status", categories: bars.map((b) => b.label) },
    y: { field: "tables", format: "integer", title: "Tables" },
    series: { field: "status", values: bars.map((b) => ({ key: b.label, label: b.label, color: b.color })) },
  }
}

/** The fields of a table-stats row the top-tables chart reads. */
export interface WrittenTable {
  qualified_name: string
  table_name?: string
  mode: "batch" | "cdc"
  inserted_rows?: number
  applied_snapshot_rows?: number
  applied_total_events?: number
}

/**
 * What a table wrote: rows for a batch table; for a CDC one, the rows its
 * snapshot loaded plus the changes the sink applied, since most of a newly
 * streamed table's rows arrive by the snapshot.
 */
function writtenByTable(t: WrittenTable): number {
  if (t.mode === "cdc") return (t.applied_snapshot_rows ?? 0) + (t.applied_total_events ?? 0)
  return t.inserted_rows ?? 0
}

export interface TopTableRow {
  table: string
  written: number
}

const TOP = 10

/**
 * The ten tables that wrote the most, most first; a table that wrote nothing is
 * left off. Each is named by its table name, or its qualified name when two share one.
 */
export function topTablesRows(tables: readonly WrittenTable[]): TopTableRow[] {
  const top = tables
    .filter((t) => writtenByTable(t) > 0)
    .sort((a, b) => writtenByTable(b) - writtenByTable(a))
    .slice(0, TOP)
  const short = (t: WrittenTable) => t.table_name || t.qualified_name.split(".").pop() || t.qualified_name
  const counts = new Map<string, number>()
  for (const t of top) counts.set(short(t), (counts.get(short(t)) ?? 0) + 1)
  return top.map((t) => ({
    table: counts.get(short(t))! > 1 ? t.qualified_name : short(t),
    written: writtenByTable(t),
  }))
}

/** Tall enough for each bar to read beside its label. */
export function hbarHeight(bars: number): number {
  return Math.max(160, bars * 28 + 48)
}

export function topTablesChart(rows: readonly TopTableRow[], measure: string): HBarChartSettings {
  return {
    type: "hbar",
    stacked: false,
    description: `The ${rows.length} tables with the most ${measure.toLowerCase()}, most first.`,
    x: { field: "table", label: "Table", categories: rows.map((r) => r.table) },
    y: { field: "written", format: "integer", axisFormat: "compact", title: measure },
    series: { values: [{ key: "written", label: measure, color: "series-1" }] },
  }
}
