"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { usePathname, useRouter } from "next/navigation"
import { AlertTriangle, Boxes, GitBranch, History, Loader2, Plus, type LucideIcon } from "lucide-react"

import { WorkflowNodes } from "@/components/icons/WorkflowNodes"
import { EmptyState } from "@/components/kit/EmptyState"
import { RowsSkeleton } from "@/components/kit/PageSkeleton"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { FilterButton } from "@/components/runs/FilterButton"
import { RunStatusBadge } from "@/components/runs/RunStatusBadge"
import { RunsKpis } from "@/components/runs/RunsKpis"
import {
  ACTIVITY_KIND_LABELS,
  ACTIVITY_KIND_PLURAL_LABELS,
  ACTIVITY_STATUS_LABELS,
  activityHref,
  activityScheduleHref,
  activityTriggerLabel,
  isActivityStatus,
  listActivity,
  type ActivityKind,
  type ActivityRun,
  type ActivityStatus,
} from "@/lib/api/activity"
import { formatDurationBetween } from "@/lib/duration"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

export const RUNS_PAGE_SIZE = 50

const KINDS = Object.keys(ACTIVITY_KIND_LABELS) as ActivityKind[]
const STATUSES = Object.keys(ACTIVITY_STATUS_LABELS) as ActivityStatus[]
/** The status Select's value for "no status filter"; Radix keeps "" for its placeholder. */
const ANY_STATUS = "any"

/** Each kind's icon, the same one the sidebar shows beside its page. */
const KIND_ICONS: Record<ActivityKind, LucideIcon> = {
  pipeline: GitBranch,
  scheduled_query: Boxes,
  workflow: WorkflowNodes,
}

/** "4m 12s" from start to finish, in the shared words; empty while the run is going or unreadable. */
export function runDuration(run: Pick<ActivityRun, "started_at" | "finished_at">): string {
  if (!run.finished_at) return ""
  const span = formatDurationBetween(run.started_at, run.finished_at)
  return span === "—" ? "" : span
}

/**
 * Every run in the workspace, newest first: pipeline runs, scheduled-query runs
 * and workflow runs in one list (GET /api/v1/activity). The filters live in the
 * URL, so Home can link to "/runs?status=failed" and a reload keeps them.
 */
export function RunsList({
  initialKind,
  initialStatus,
}: {
  initialKind?: ActivityKind
  initialStatus?: ActivityStatus
}) {
  const router = useRouter()
  const pathname = usePathname()
  const [kind, setKind] = useState<ActivityKind | undefined>(initialKind)
  const [status, setStatus] = useState<ActivityStatus | undefined>(initialStatus)
  const [runs, setRuns] = useState<ActivityRun[] | null>(null)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [version, setVersion] = useState(0)
  // Bumped by every first-page load, so a "Load more" that a filter change or a
  // workspace switch overtook cannot append its page to the new list.
  const listSeq = useRef(0)

  // Back to "Loading…" before the first page of a new list is asked for. Done by
  // whoever changes the list (a filter, a switch, Retry), not by the effect.
  const resetList = useCallback(() => {
    setRuns(null)
    setCursor(undefined)
    setError(null)
    setLoadingMore(false)
  }, [])

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        resetList()
        setVersion((v) => v + 1)
      }),
    [resetList],
  )

  useEffect(() => {
    const seq = ++listSeq.current
    const isStale = captureWorkspace()
    listActivity({ kind, status, limit: RUNS_PAGE_SIZE }).then(
      (page) => {
        if (seq !== listSeq.current || isStale()) return
        setRuns(page.runs)
        setCursor(page.next_cursor)
      },
      (err: unknown) => {
        if (seq !== listSeq.current || isStale()) return
        setError(err instanceof Error ? err.message : "Could not load runs.")
      },
    )
  }, [kind, status, version])

  const loadMore = useCallback(async () => {
    if (!cursor || loadingMore) return
    const seq = listSeq.current
    const isStale = captureWorkspace()
    setLoadingMore(true)
    try {
      const page = await listActivity({ kind, status, limit: RUNS_PAGE_SIZE, cursor })
      if (seq !== listSeq.current || isStale()) return
      setRuns((prev) => {
        const seen = new Set((prev ?? []).map((r) => `${r.kind}:${r.id}`))
        return [...(prev ?? []), ...page.runs.filter((r) => !seen.has(`${r.kind}:${r.id}`))]
      })
      setCursor(page.next_cursor)
    } catch (err) {
      if (seq === listSeq.current && !isStale()) setError(err instanceof Error ? err.message : "Could not load more runs.")
    } finally {
      if (seq === listSeq.current) setLoadingMore(false)
    }
  }, [cursor, kind, loadingMore, status])

  const applyFilters = (nextKind: ActivityKind | undefined, nextStatus: ActivityStatus | undefined) => {
    if (nextKind === kind && nextStatus === status) return
    resetList()
    setKind(nextKind)
    setStatus(nextStatus)
    const params = new URLSearchParams()
    if (nextKind) params.set("kind", nextKind)
    if (nextStatus) params.set("status", nextStatus)
    const qs = params.toString()
    router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false })
  }

  return (
    <div className="space-y-4">
      {/* Workflow runs have no per-day series yet, so their filter has no numbers to show. */}
      {kind !== "workflow" && <RunsKpis />}
      <div className="flex flex-wrap items-center gap-3">
        <div role="group" aria-label="Kind" className="flex flex-wrap gap-1">
          <FilterButton pressed={!kind} onClick={() => applyFilters(undefined, status)}>
            All
          </FilterButton>
          {KINDS.map((k) => (
            <FilterButton key={k} pressed={kind === k} onClick={() => applyFilters(k, status)}>
              {ACTIVITY_KIND_PLURAL_LABELS[k]}
            </FilterButton>
          ))}
        </div>
        <Select
          value={status ?? ANY_STATUS}
          onValueChange={(v) => applyFilters(kind, isActivityStatus(v) ? v : undefined)}
        >
          <SelectTrigger aria-label="Status" className="h-8 w-40">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={ANY_STATUS}>Any status</SelectItem>
            {STATUSES.map((s) => (
              <SelectItem key={s} value={s}>
                {ACTIVITY_STATUS_LABELS[s]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {error && (
        <p
          role="alert"
          className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300"
        >
          <AlertTriangle className="h-4 w-4 shrink-0" aria-hidden />
          {error}{" "}
          <button
            type="button"
            className="underline"
            onClick={() => {
              resetList()
              setVersion((v) => v + 1)
            }}
          >
            Retry
          </button>
        </p>
      )}
      {runs === null && !error && (
        <RowsSkeleton
          rows={6}
          label="Loading runs…"
          className="rounded-xl border border-zinc-200 bg-white px-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950"
        />
      )}
      {runs !== null && runs.length === 0 && (
        <div className="rounded-xl border border-zinc-200 bg-white shadow-sm dark:border-zinc-800 dark:bg-zinc-950">
          {kind || status ? (
            <EmptyState icon={History} title="No runs match these filters." description="Try another kind or status." />
          ) : (
            <EmptyState
              icon={History}
              title="No runs yet"
              description="A pipeline, model or workflow lists its runs here once it runs."
              action={
                <Button asChild size="sm" className="gap-1.5 bg-violet-600 text-white hover:bg-violet-700">
                  <Link href="/chat">
                    <Plus className="h-4 w-4" aria-hidden />
                    Create pipeline
                  </Link>
                </Button>
              }
            />
          )}
        </div>
      )}
      {runs !== null && runs.length > 0 && (
        <div className="overflow-x-auto rounded-xl border border-zinc-200 bg-white shadow-sm dark:border-zinc-800 dark:bg-zinc-950">
          <table className="w-full text-left text-sm">
            <thead className="bg-zinc-50 text-xs text-zinc-500 dark:bg-zinc-900/50 dark:text-zinc-400">
              <tr>
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium">Kind</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 font-medium">Trigger</th>
                <th className="px-3 py-2 font-medium">Started</th>
                <th className="px-3 py-2 font-medium">Duration</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr key={`${r.kind}:${r.id}`} className="border-t border-zinc-100 align-top dark:border-zinc-800">
                  <td className="px-3 py-2">
                    <Link href={activityHref(r)} className="font-medium text-zinc-900 hover:underline dark:text-white">
                      {r.subject_name || "Unnamed"}
                    </Link>
                    {r.error && (
                      // A skip's reason ("waiting on 1 upstream…") is not a failure: only a failed run's is red.
                      <p
                        className={`mt-0.5 line-clamp-2 max-w-xl text-xs ${
                          r.status === "failed" ? "text-red-700 dark:text-red-300" : "text-zinc-500 dark:text-zinc-400"
                        }`}
                        title={r.error}
                      >
                        {r.error}
                      </p>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                    <RunKind kind={r.kind} />
                  </td>
                  <td className="px-3 py-2">
                    <RunStatusBadge run={r} />
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                    <RunTrigger run={r} />
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                    <LocalDateTime value={r.started_at} zone={false} />
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-xs text-zinc-600 dark:text-zinc-300">
                    {runDuration(r) || (r.status === "running" ? "Running" : "—")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {runs !== null && cursor && (
        <div className="flex justify-center">
          <Button variant="outline" size="sm" onClick={() => void loadMore()} disabled={loadingMore}>
            {loadingMore && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
            Load more
          </Button>
        </div>
      )}
    </div>
  )
}

/** The kind in words, after the icon its page has in the sidebar. */
function RunKind({ kind }: { kind: ActivityKind }) {
  const Icon = KIND_ICONS[kind]
  return (
    <span className="inline-flex items-center gap-1.5">
      {Icon && <Icon className="h-3.5 w-3.5 shrink-0 text-zinc-400 dark:text-zinc-500" aria-hidden />}
      {ACTIVITY_KIND_LABELS[kind] ?? kind}
    </span>
  )
}

/** The trigger; a scheduled pipeline run's links to the schedule that started it. */
function RunTrigger({ run }: { run: ActivityRun }) {
  const label = activityTriggerLabel(run.trigger)
  const href = activityScheduleHref(run)
  if (!href) return <>{label}</>
  return (
    <Link href={href} className="underline underline-offset-2 hover:text-zinc-900 dark:hover:text-white" title="Open this schedule">
      {label}
    </Link>
  )
}
