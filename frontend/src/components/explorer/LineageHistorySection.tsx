"use client"

/**
 * The History section of the Lineage page's details panel: the runs recorded against the
 * selected card (lineageHistory.ts), newest first, each with how it ended, when, and the
 * tables it read and wrote; and, for a model, when its SQL changed and which columns came
 * and went. The graph says what is linked now; this says what ran.
 */

import { useEffect, useState } from "react"
import { Loader2, Table2 } from "lucide-react"

import { authFetch } from "@/lib/api/auth-fetch"
import { cn, formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import type { AssetNode } from "@/components/explorer/assetLineage"
import {
  columnChangeText,
  datasetNode,
  hasLineageHistory,
  isCDCWindow,
  lineageHistoryUrl,
  parseLineageHistory,
  runDuration,
  runOutcome,
  type LineageDataset,
  type LineageHistory,
  type LineageHistoryRun,
} from "@/components/explorer/lineageHistory"

/** Runs shown before "Show all". */
const RUNS_PREVIEW = 5

const OUTCOME_CLASS = {
  ok: "border-emerald-200 bg-emerald-50 text-emerald-800 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300",
  failed: "border-red-200 bg-red-50 text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300",
  neutral: "border-zinc-200 bg-zinc-50 text-zinc-600 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300",
} as const

type Loaded = { key: string; history: LineageHistory } | { key: string; error: string }

type SectionProps = {
  node: AssetNode
  byId: ReadonlyMap<string, AssetNode>
  /** The page's Refresh: the runs are asked for again with the graph. */
  reloadTick: number
  onSelect: (id: string) => void
}

/** Keyed by the card, so what was chosen on one (all runs, skipped runs) is not carried to the next. */
export function LineageHistorySection(props: SectionProps) {
  return <HistorySection key={props.node.id} {...props} />
}

function HistorySection({ node, byId, reloadTick, onSelect }: SectionProps) {
  const supported = hasLineageHistory(node)
  const [retryTick, setRetryTick] = useState(0)
  const [result, setResult] = useState<Loaded | null>(null)
  const [showAll, setShowAll] = useState(false)
  const [withSkipped, setWithSkipped] = useState(false)
  const loadKey = `${node.id}|${reloadTick}|${retryTick}|${withSkipped ? "skipped" : ""}`
  const loading = result?.key !== loadKey

  useEffect(() => {
    if (!supported) return
    let cancelled = false
    ;(async () => {
      try {
        const res = await authFetch(lineageHistoryUrl(node.id, undefined, withSkipped), { method: "GET" })
        if (cancelled) return
        if (!res.ok) {
          setResult({ key: loadKey, error: `Its runs could not be read (HTTP ${res.status}).` })
          return
        }
        const history = parseLineageHistory(await res.json())
        if (!cancelled) setResult({ key: loadKey, history })
      } catch {
        if (!cancelled) setResult({ key: loadKey, error: "Could not reach the server to read its runs." })
      }
    })()
    return () => {
      cancelled = true
    }
  }, [loadKey, node.id, supported, withSkipped])

  if (!supported) return null
  const title = "History"
  const history = !loading && result && "history" in result ? result.history : null
  const error = !loading && result && "error" in result ? result.error : null
  const runs = history?.runs ?? []
  // The server sends only the newest runs: the count it holds is what the heading says, and
  // a button over the page it sent cannot offer "all".
  const capped = history !== null && history.total_runs > runs.length
  const shown = showAll ? runs : runs.slice(0, RUNS_PREVIEW)
  const changes = history?.column_changes ?? []
  const skipped = history?.skipped_runs ?? 0
  return (
    <div aria-label={title} role="group">
      <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
        {title}
        {history && runs.length > 0 && (capped ? ` (${runs.length} of ${history.total_runs})` : ` (${runs.length})`)}
      </h3>
      <div className="space-y-2 text-xs">
        {loading && (
          <p className="flex items-center gap-1.5 text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            Reading its runs…
          </p>
        )}
        {error && (
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-red-600 dark:text-red-400">{error}</p>
            <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setRetryTick((t) => t + 1)}>
              Try again
            </Button>
          </div>
        )}
        {history && runs.length === 0 && (
          <p className="text-zinc-500 dark:text-zinc-400">{emptyText(node.kind, skipped > 0)}</p>
        )}
        {history && skipped > 0 && (
          <div className="flex flex-wrap items-center gap-x-1 text-zinc-500 dark:text-zinc-400">
            <p>
              {skipped === 1 ? "1 run" : `${skipped} runs`} skipped
              {history.last_skipped_at && (
                <>
                  {" · last "}
                  <time dateTime={history.last_skipped_at} title={formatAbsoluteTime(history.last_skipped_at, { zone: true })}>
                    {formatRelativeTime(history.last_skipped_at)}
                  </time>
                </>
              )}
            </p>
            <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setWithSkipped((w) => !w)}>
              {withSkipped ? "Hide skipped" : "Show skipped"}
            </Button>
          </div>
        )}
        {runs.length > 0 && (
          <ol aria-label={`Runs of ${node.name}`} className="space-y-2">
            {shown.map((r) => (
              <RunItem key={r.id} run={r} node={node} byId={byId} onSelect={onSelect} />
            ))}
          </ol>
        )}
        {runs.length > shown.length && (
          <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setShowAll(true)}>
            {capped ? "Show latest" : "Show all"} {runs.length}
          </Button>
        )}
        {changes.length > 0 && (
          <div>
            <h4 className="mb-1 font-medium text-zinc-700 dark:text-zinc-200">Column changes</h4>
            <ol aria-label={`Column changes of ${node.name}`} className="space-y-1">
              {[...changes].reverse().map((c) => (
                <li key={c.at} className="text-zinc-600 dark:text-zinc-300">
                  <time dateTime={c.at} title={formatAbsoluteTime(c.at, { zone: true })} className="text-zinc-500 dark:text-zinc-400">
                    {formatRelativeTime(c.at)}
                  </time>
                  {" · "}
                  {columnChangeText(c)}
                </li>
              ))}
            </ol>
          </div>
        )}
        {history && history.retention_days > 0 && (
          <p className="text-[11px] text-zinc-500 dark:text-zinc-400">Runs are kept {history.retention_days} days.</p>
        )}
      </div>
    </div>
  )
}

function emptyText(kind: string, onlySkipped: boolean): string {
  if (kind === "table") return "No recorded run has read or written it yet."
  // A model skipped on every tick has runs on record, none of which ran.
  if (onlySkipped) return "Nothing has run yet: every recorded run was skipped."
  return "No runs recorded yet."
}

function RunItem({
  run,
  node,
  byId,
  onSelect,
}: {
  run: LineageHistoryRun
  node: AssetNode
  byId: ReadonlyMap<string, AssetNode>
  onSelect: (id: string) => void
}) {
  const outcome = runOutcome(run.event_type, run.job_kind)
  const took = runDuration(run)
  // On a table's history, which pipeline or model ran is the point; on its own, it is the card.
  const job = node.kind === "table" ? byId.get(`${run.job_kind}:${run.job_id}`) : undefined
  return (
    <li className="space-y-0.5" data-outcome={run.event_type}>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className={cn("rounded border px-1.5 py-px text-[11px] font-medium", OUTCOME_CLASS[outcome.tone])}>
          {outcome.text}
        </span>
        <time dateTime={run.ended_at} title={formatAbsoluteTime(run.ended_at, { zone: true })} className="text-zinc-600 dark:text-zinc-300">
          {formatRelativeTime(run.ended_at)}
        </time>
        {took && <span className="text-zinc-500 dark:text-zinc-400">· took {took}</span>}
        {isCDCWindow(run) && <span className="text-zinc-500 dark:text-zinc-400">· changes landed</span>}
      </div>
      {node.kind === "table" && (
        <p className="text-zinc-500 dark:text-zinc-400">
          By{" "}
          {job ? (
            <button
              type="button"
              className="font-mono text-zinc-900 hover:underline dark:text-white"
              aria-label={`Select ${job.name}`}
              onClick={() => onSelect(job.id)}
            >
              {job.name}
            </button>
          ) : (
            <span className="font-mono text-zinc-700 dark:text-zinc-200">{run.job_name || run.job_kind}</span>
          )}
        </p>
      )}
      <DatasetLine label="Read" datasets={run.inputs} byId={byId} onSelect={onSelect} />
      <DatasetLine label="Wrote" datasets={run.outputs} byId={byId} onSelect={onSelect} />
    </li>
  )
}

/** A run's tables: each one the graph draws is a button to it; the rest are named. */
function DatasetLine({
  label,
  datasets,
  byId,
  onSelect,
}: {
  label: string
  datasets: LineageDataset[]
  byId: ReadonlyMap<string, AssetNode>
  onSelect: (id: string) => void
}) {
  if (datasets.length === 0) return null
  return (
    <p className="flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5 text-zinc-500 dark:text-zinc-400">
      <span>{label}</span>
      {datasets.map((d) => {
        const n = datasetNode(d, byId)
        const key = `${d.connection_id}:${d.captured ? "c:" : ""}${d.name}`
        return n ? (
          <button
            key={key}
            type="button"
            className="inline-flex min-w-0 max-w-full items-center gap-1 font-mono text-zinc-900 hover:underline dark:text-white"
            aria-label={`Select ${n.name}`}
            onClick={() => onSelect(n.id)}
          >
            <Table2 className="h-3 w-3 shrink-0" aria-hidden />
            <span className="truncate">{d.name}</span>
          </button>
        ) : (
          <span
            key={key}
            className="inline-flex min-w-0 max-w-full items-center gap-1 font-mono text-zinc-700 dark:text-zinc-200"
            title="Not drawn on the graph now"
          >
            <Table2 className="h-3 w-3 shrink-0" aria-hidden />
            <span className="truncate">{d.name}</span>
          </span>
        )
      })}
    </p>
  )
}
