"use client"

import { useMemo } from "react"
import { ChartColumn, History } from "lucide-react"

import type { HBarChartSettings } from "@/components/charts/chart-settings"
import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import { hbarHeight } from "@/components/pipeline/table-stats-charts"
import type { QueryHistoryEntry } from "@/lib/explorer/queryTabs"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"

const TOP = 10
const RECENT = 5

interface StartHereTable {
  name: string
  schema?: string
  row_count?: number
}

interface LargeTable {
  /** As the bar names it: the bare name, or the qualified one when two share it. */
  table: string
  /** As a query names it, the way the schema browser's Insert table does. */
  qualified: string
  rows: number
}

const qualify = (t: StartHereTable) => (t.schema ? `${t.schema}.${t.name}` : t.name)

/** The tables the schema index has a row count for, most rows first, at most ten. */
function largestTables(tables: readonly StartHereTable[]): LargeTable[] {
  const top = tables
    .filter((t): t is StartHereTable & { row_count: number } => typeof t.row_count === "number" && t.row_count > 0)
    .sort((a, b) => b.row_count - a.row_count)
    .slice(0, TOP)
  const counts = new Map<string, number>()
  for (const t of top) counts.set(t.name, (counts.get(t.name) ?? 0) + 1)
  return top.map((t) => ({ table: counts.get(t.name)! > 1 ? qualify(t) : t.name, qualified: qualify(t), rows: t.row_count }))
}

function largestTablesChart(rows: readonly LargeTable[]): HBarChartSettings {
  return {
    type: "hbar",
    stacked: false,
    description: `The ${rows.length} largest tables on this connection, most rows first.`,
    x: { field: "table", label: "Table", categories: rows.map((r) => r.table) },
    y: { field: "rows", format: "integer", axisFormat: "compact", title: "Rows" },
    series: { values: [{ key: "rows", label: "Rows", color: "series-1" }] },
  }
}

/** When it ran, exactly: "" for a timestamp storage held badly. */
function ranAt(entry: QueryHistoryEntry): string {
  const ms = entry.timestamp.getTime()
  return Number.isNaN(ms) ? "" : formatAbsoluteTime(new Date(ms).toISOString())
}

const rowsText = (n: number) => `${new Intl.NumberFormat("en-US").format(n)} ${n === 1 ? "row" : "rows"}`

/**
 * What the Explorer's result pane shows before a query has run in this tab
 * (launch review, PR4): the connection's ten largest tables as bars, one click
 * from a query on any of them, and the last queries run on it in this browser.
 * The bars come from the row counts the schema index already read; with none,
 * there are no bars.
 */
export function ExplorerStartHere({
  tables,
  history,
  onQueryTable,
  onOpenQuery,
}: {
  tables: readonly StartHereTable[]
  history: readonly QueryHistoryEntry[]
  /** Puts a query on the table, named as `qualified`, in the editor. */
  onQueryTable: (qualified: string) => void
  onOpenQuery: (entry: QueryHistoryEntry) => void
}) {
  const largest = useMemo(() => largestTables(tables), [tables])
  const settings = useMemo(() => largestTablesChart(largest), [largest])
  const recent = history.slice(0, RECENT)

  return (
    <div data-testid="results-empty" className="grid min-h-0 flex-1 content-start gap-3 overflow-auto py-2 @3xl:grid-cols-2">
      {largest.length > 0 && (
        <section aria-labelledby="start-largest" className="min-w-0 rounded-xl border border-zinc-200 p-3 dark:border-zinc-800">
          <h2 id="start-largest" className="flex items-center gap-1.5 text-sm font-semibold text-zinc-900 dark:text-white">
            <ChartColumn className="h-4 w-4 text-zinc-500 dark:text-zinc-400" aria-hidden />
            Start here: largest tables
          </h2>
          <p className="mt-0.5 text-xs text-zinc-500 dark:text-zinc-400">Click a bar to start a query on that table.</p>
          <div className="mt-2">
            <EChartNoSSR
              settings={settings}
              rows={largest}
              height={hbarHeight(largest.length)}
              onCategoryClick={(i) => largest[i] && onQueryTable(largest[i].qualified)}
            />
          </div>
          {/* The bars as buttons, for a keyboard: shown when one has focus. */}
          <ul aria-label="Query a table" className="flex flex-wrap gap-1">
            {largest.map((t) => (
              <li key={t.qualified}>
                <button
                  type="button"
                  onClick={() => onQueryTable(t.qualified)}
                  className="sr-only rounded-md border border-zinc-200 px-2 py-1 text-xs focus:not-sr-only dark:border-zinc-700"
                >
                  Query {t.qualified}
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}

      <section aria-labelledby="start-recent" className="min-w-0 rounded-xl border border-zinc-200 p-3 dark:border-zinc-800">
        <h2 id="start-recent" className="flex items-center gap-1.5 text-sm font-semibold text-zinc-900 dark:text-white">
          <History className="h-4 w-4 text-zinc-500 dark:text-zinc-400" aria-hidden />
          Recent queries
        </h2>
        {recent.length === 0 ? (
          <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">
            Queries you run on this connection show here. Insert a table from the schema browser to start one.
          </p>
        ) : (
          <ul className="mt-2 space-y-1">
            {recent.map((entry) => (
              <li key={entry.id}>
                <button
                  type="button"
                  onClick={() => onOpenQuery(entry)}
                  className="w-full rounded-md px-2 py-1.5 text-left hover:bg-zinc-50 dark:hover:bg-zinc-900"
                >
                  <span className="block truncate font-mono text-xs text-zinc-700 dark:text-zinc-300">{entry.sql}</span>
                  <span className="mt-0.5 block text-[11px] text-zinc-500 dark:text-zinc-400">
                    {typeof entry.rowCount === "number" ? `${rowsText(entry.rowCount)} · ` : ""}
                    <span title={ranAt(entry)}>{formatRelativeTime(entry.timestamp)}</span>
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}
