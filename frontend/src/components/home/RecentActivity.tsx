"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { AlertTriangle, ArrowRight, History } from "lucide-react"

import { EmptyState } from "@/components/kit/EmptyState"
import { RowsSkeleton } from "@/components/kit/PageSkeleton"
import { RunStatusBadge } from "@/components/runs/RunStatusBadge"
import { ACTIVITY_KIND_LABELS, activityHref, listActivity, type ActivityRun } from "@/lib/api/activity"
import { formatRelativeTime } from "@/lib/utils"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

export const RECENT_RUNS = 8

/**
 * The newest runs of every kind, the top of the Runs page. Home shows only
 * these few; "All runs" opens the rest.
 */
export function RecentActivity() {
  const [runs, setRuns] = useState<ActivityRun[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const seq = useRef(0)

  const reload = useCallback(() => {
    setRuns(null)
    setError(null)
    setVersion((v) => v + 1)
  }, [])

  useEffect(() => onActiveWorkspaceChange(reload), [reload])

  useEffect(() => {
    const mine = ++seq.current
    const isStale = captureWorkspace()
    listActivity({ limit: RECENT_RUNS }).then(
      (page) => {
        if (mine !== seq.current || isStale()) return
        setRuns(page.runs)
      },
      (err: unknown) => {
        if (mine !== seq.current || isStale()) return
        setError(err instanceof Error ? err.message : "Could not load runs.")
      },
    )
  }, [version])

  return (
    <section
      aria-labelledby="home-runs-heading"
      className="rounded-xl border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950"
    >
      <div className="flex items-center justify-between gap-2">
        <h2 id="home-runs-heading" className="text-sm font-semibold text-zinc-900 dark:text-white">
          Recent runs
        </h2>
        <Link
          href="/runs"
          className="inline-flex items-center gap-1 text-sm font-medium text-violet-700 hover:underline dark:text-violet-300"
        >
          All runs <ArrowRight className="h-3.5 w-3.5" aria-hidden />
        </Link>
      </div>

      {error && (
        <p role="alert" className="mt-2 flex flex-wrap items-center gap-2 text-sm text-red-700 dark:text-red-300">
          <AlertTriangle className="h-4 w-4 shrink-0" aria-hidden />
          {error}
          <button type="button" onClick={reload} className="font-medium underline">
            Retry
          </button>
        </p>
      )}
      {runs === null && !error && <RowsSkeleton rows={4} label="Loading runs…" className="mt-1" />}
      {runs !== null && runs.length === 0 && (
        <EmptyState
          size="sm"
          icon={History}
          title="No runs yet"
          description="A pipeline, model or workflow lists its runs here once it runs."
        />
      )}
      {runs !== null && runs.length > 0 && (
        <ul className="mt-2 divide-y divide-zinc-100 dark:divide-zinc-800">
          {runs.map((r) => (
            <li key={`${r.kind}:${r.id}`} className="flex items-center gap-3 py-2 text-sm">
              <RunStatusBadge run={r} />
              <div className="min-w-0 flex-1">
                <Link href={activityHref(r)} className="block truncate font-medium text-zinc-900 hover:underline dark:text-white">
                  {r.subject_name || "Unnamed"}
                </Link>
                <span className="text-xs text-zinc-500 dark:text-zinc-400">{ACTIVITY_KIND_LABELS[r.kind] ?? r.kind}</span>
              </div>
              <time dateTime={r.started_at} className="shrink-0 text-xs text-zinc-500 dark:text-zinc-400">
                {formatRelativeTime(r.started_at)}
              </time>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
