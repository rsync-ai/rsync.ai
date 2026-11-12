"use client"

import { useEffect, useState, useCallback, useId, useMemo, useRef } from "react"
import { useRouter } from "next/navigation"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { ChartColumn, History, RefreshCw, Eye, RotateCcw } from "lucide-react"
import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import { ChartCard, WindowToggle } from "@/components/kit/ChartCard"
import { EmptyState } from "@/components/kit/EmptyState"
import { ChartCardsSkeleton, RowsSkeleton } from "@/components/kit/PageSkeleton"
import { pipelineRunMetrics } from "@/components/pipeline/pipeline-run-metrics"
import {
  PIPELINE_OUTCOMES,
  countChart,
  countRows,
  dayLabel,
  rowsWrittenChart,
  rowsWrittenRows,
  runTimeChart,
  runTimeRows,
} from "@/components/runs/run-chart-specs"
import { METRICS_WINDOWS, type MetricsWindow } from "@/lib/api/activity-metrics"
import { formatRelativeTime } from "@/lib/utils"
// Was a local copy that floored to whole seconds, so this tab and the pipeline
// list disagreed about the same execution ("0s" vs "500ms").
import { formatDurationBetween as formatDuration } from "@/lib/duration"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { listExecutionsResponse, type ExecutionListResponse } from "@/lib/api/executions"
import { ACTIVITY_STATUS_LABELS } from "@/lib/api/activity"
import {
  executePipelineWithRunMode,
  AssessmentRequiredError,
  type AssessmentReport,
} from "@/lib/api/pipelines"
import { PreMigrationAssessmentModal } from "@/components/pipeline/PreMigrationAssessmentModal"
import { usePipelineReloadSupported } from "@/lib/hooks/usePipelineReloadSupported"
import {
  ExecutionDetailDialog,
  statusPresentation,
  type ExecutionRow as Execution,
} from "@/components/pipeline/ExecutionDetailDialog"
import { toast } from "sonner"

interface ExecutionHistoryTabProps {
  pipelineId: string
}

const LIMIT = 250

/**
 * The stored statuses one filter option reads. The gateway matches one status
 * exactly (`pipelines.go:4285`), and a succeeded run is stored as "success" or
 * "completed", so "Succeeded" asks for both.
 */
function filterStatuses(filter: string): (string | undefined)[] {
  if (filter === "all") return [undefined]
  if (filter === "succeeded") return ["success", "completed"]
  return [filter]
}

/**
 * Several newest-first lists as one, newest first, each run once, at most
 * `limit` long. One list is kept in the order the server sent it.
 */
export function mergeNewestFirst<T extends { id: string; start_time: string }>(lists: T[][], limit: number): T[] {
  if (lists.length === 1) return lists[0].slice(0, limit)
  const seen = new Set<string>()
  return lists
    .flat()
    .sort((a, b) => (Date.parse(b.start_time) || 0) - (Date.parse(a.start_time) || 0))
    .filter((e) => (seen.has(e.id) ? false : (seen.add(e.id), true)))
    .slice(0, limit)
}

/**
 * The tab's three charts, counted from the runs it loaded (no other request):
 * runs per day by outcome, run time, and rows written, over the last `days` days.
 */
function pipelineRunCharts(runs: readonly Execution[], days: number) {
  const m = pipelineRunMetrics(runs, days, { truncated: runs.length >= LIMIT })
  return {
    since: m.since,
    list: [
      {
        title: "Runs",
        caption: "Per day, by outcome",
        settings: countChart("This pipeline's runs per day, by outcome", m.days, PIPELINE_OUTCOMES),
        rows: countRows(m.pipeline_runs, PIPELINE_OUTCOMES),
        empty: m.pipeline_runs.length > 0 ? undefined : "No runs in this window.",
      },
      {
        title: "Run time",
        caption: "Median and 95th percentile, in seconds",
        settings: runTimeChart("Median and 95th-percentile run time per day of this pipeline, in seconds", m.days),
        rows: runTimeRows(m.days, m.pipeline_run_time),
        empty: m.pipeline_run_time.length > 0 ? undefined : "No run succeeded or failed in this window.",
      },
      {
        title: "Rows written",
        caption: "Per day",
        settings: rowsWrittenChart("Rows this pipeline's runs wrote per day", m.days),
        rows: rowsWrittenRows(m.rows_written),
        empty: m.rows_written.length > 0 ? undefined : "No runs in this window.",
      },
    ],
  }
}

export function ExecutionHistoryTab({ pipelineId }: ExecutionHistoryTabProps) {
  const router = useRouter()
  const chartsHeadingId = useId()
  const [executions, setExecutions] = useState<Execution[]>([])
  // The charts count every run, so they read the unfiltered list: a status filter
  // narrows the table, not the charts. Null until the first unfiltered list lands.
  const [allRuns, setAllRuns] = useState<Execution[] | null>(null)
  const allRunsLoaded = useRef(false)
  const [chartDays, setChartDays] = useState<MetricsWindow>(METRICS_WINDOWS[0])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [statusFilter, setStatusFilter] = useState("all")
  const [selectedExecution, setSelectedExecution] = useState<Execution | null>(null)
  const [currentPage, setCurrentPage] = useState(1)
  const perPage = 25
  // No "Reload everything" when the destination is an app's API (reverse ETL).
  const reloadSupported = usePipelineReloadSupported(pipelineId)

  // Pre-migration assessment modal state. The first POST /run returns
  // 422 with an assessment payload when warnings need acking. We hold
  // the report + the run mode the user originally clicked so the
  // Proceed button can re-issue with ack_warnings: true.
  const [assessmentReport, setAssessmentReport] = useState<AssessmentReport | null>(null)
  const [pendingRunMode, setPendingRunMode] = useState<"resume" | "reload" | null>(null)
  const [submittingProceed, setSubmittingProceed] = useState(false)

  // Try a run; if the gateway returns the assessment envelope, surface
  // the modal instead of toasting an error. ackWarnings=false on first
  // attempt; true on the Proceed retry.
  const tryRun = useCallback(
    async (
      runMode: "resume" | "reload",
      ackWarnings: boolean,
      nominatedKeys?: Record<string, string[]>,
    ) => {
      try {
        const res = await executePipelineWithRunMode(pipelineId, runMode, { ackWarnings, nominatedKeys })
        toast.success(`Pipeline re-run started (${runMode})`)
        router.push(`/executions/${res.execution_id}`)
        return { ok: true as const }
      } catch (e: any) {
        if (e instanceof AssessmentRequiredError) {
          setAssessmentReport(e.report)
          setPendingRunMode(runMode)
          return { ok: false as const, awaitingAck: true }
        }
        toast.error(e?.message || `Failed to retry (${runMode})`)
        return { ok: false as const, awaitingAck: false }
      }
    },
    [pipelineId, router],
  )

  // Succeeded asks twice, so an older filter's answer can land after a newer
  // one; only the latest request may write the list.
  const listSeq = useRef(0)

  const fetchExecutions = useCallback(async () => {
    const seq = ++listSeq.current
    setLoading(true)
    setError(null)

    try {
      // A filter picked before the unfiltered list came, or after it failed,
      // loads that list as well, for the charts; if only it fails, they stay away.
      const withAll = statusFilter !== "all" && !allRunsLoaded.current
      const [resps, everyResp] = await Promise.all([
        Promise.all(
          filterStatuses(statusFilter).map((status) =>
            // Keep a wide window for accurate pagination/search within the tab.
            listExecutionsResponse({ pipelineId, status, limit: LIMIT }),
          ),
        ),
        withAll ? listExecutionsResponse({ pipelineId, limit: LIMIT }).catch(() => null) : null,
      ])

      const toRow = (e: any): Execution => ({
        id: String(e.id || ""),
        pipeline_id: String(e.pipeline_id || e.pipelineId || pipelineId),
        status: String(e.status || "pending"),
        start_time: String(e.start_time || e.startedAt || ""),
        end_time: e.end_time ?? e.finishedAt ?? null,
        error_message: e.error_message ?? e.error ?? null,
        metrics: (e.metrics ?? null) as Execution['metrics'],
        // The handler emits these four (`pipelines.go:3602-3621`); the mapper used
        // to drop them on the floor, so the dialog could not have shown how a run
        // was triggered even though the answer was already in the response.
        trigger_source: e.trigger_source ?? null,
        schedule_id: e.schedule_id ?? null,
        scheduled_time: e.scheduled_time ?? null,
        pipeline_name: e.pipeline_name ?? null,
      })
      const rowsOf = (r: ExecutionListResponse) => (r.executions || []).map(toRow).filter((e) => Boolean(e.id))
      const lists = resps.map(rowsOf)

      if (seq !== listSeq.current) return
      const merged = mergeNewestFirst(lists, LIMIT)
      setExecutions(merged)
      const every = statusFilter === "all" ? merged : everyResp ? rowsOf(everyResp).slice(0, LIMIT) : null
      if (every) {
        setAllRuns(every)
        allRunsLoaded.current = true
      }
    } catch (err: any) {
      if (seq !== listSeq.current) return
      setError(err?.message || "Failed to load execution history")
    } finally {
      if (seq === listSeq.current) setLoading(false)
    }
  }, [pipelineId, statusFilter])

  useEffect(() => {
    fetchExecutions()
  }, [fetchExecutions])

  const filteredExecutions = executions.slice((currentPage - 1) * perPage, currentPage * perPage)
  const totalPages = Math.ceil(executions.length / perPage)

  // The next-older run, for the dialog's delta. The list is newest-first, so it
  // is simply the following index — and it is already in state, so comparing two
  // runs costs no extra request. Undefined for the oldest run in the window,
  // which is the honest answer: we have nothing to compare it against.
  const selectedIndex = selectedExecution
    ? executions.findIndex((e) => e.id === selectedExecution.id)
    : -1
  const previousExecution =
    selectedIndex >= 0 ? executions[selectedIndex + 1] ?? null : null

  const charts = useMemo(() => (allRuns ? pipelineRunCharts(allRuns, chartDays) : null), [allRuns, chartDays])

  return (
    <div className="space-y-6">
      {/* Hidden only while no unfiltered list has loaded: when it failed, the run list below says so, with Retry. */}
      {(charts || loading) && (
        <section aria-labelledby={chartsHeadingId} className="space-y-4">
          <div className="flex flex-wrap items-end justify-between gap-x-4 gap-y-2">
            <div className="min-w-0 space-y-0.5">
              <h3 id={chartsHeadingId} className="text-sm font-semibold text-zinc-900 dark:text-zinc-100">
                Runs per day
              </h3>
              <p className="text-xs text-zinc-500 dark:text-zinc-400" data-testid="run-charts-caption">
                Days are UTC days, by when each run started.
                {charts?.since &&
                  ` Counted from the ${LIMIT} most recent runs, which go back to ${dayLabel(charts.since)}.`}
              </p>
            </div>
            <WindowToggle value={chartDays} options={METRICS_WINDOWS} onChange={setChartDays} />
          </div>
          {charts ? (
            <div className="grid gap-4 @3xl:grid-cols-2 @4xl:grid-cols-3">
              {charts.list.map((c) => (
                <ChartCard key={c.title} title={c.title} description={c.caption} empty={c.empty} emptyIcon={ChartColumn}>
                  <EChartNoSSR settings={c.settings} rows={c.rows} />
                </ChartCard>
              ))}
            </div>
          ) : (
            <ChartCardsSkeleton count={3} label="Loading run charts…" className="@4xl:grid-cols-3" />
          )}
        </section>
      )}

      <section
        aria-label="Run list"
        className="space-y-4 rounded-xl border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950"
      >
      {/* Filters */}
      <div className="flex items-center gap-4">
        {/*
          Reset to page 1 with the filter. The fetch callback depends on the
          filter but the page did not, and the pager only renders when
          totalPages > 1 — so filtering to a sub-page-sized result while on page
          3 left an empty slice rendering "No execution history yet", with no
          control on screen to get back. The rows existed; the UI denied it.
        */}
        <Select
          value={statusFilter}
          onValueChange={(value) => {
            setStatusFilter(value)
            setCurrentPage(1)
          }}
        >
          <SelectTrigger className="w-[180px]" aria-label="Status">
            <SelectValue placeholder="All Statuses" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All Statuses</SelectItem>
            <SelectItem value="succeeded">{ACTIVITY_STATUS_LABELS.succeeded}</SelectItem>
            <SelectItem value="failed">{ACTIVITY_STATUS_LABELS.failed}</SelectItem>
            <SelectItem value="running">{ACTIVITY_STATUS_LABELS.running}</SelectItem>
            <SelectItem value="cancelled">{ACTIVITY_STATUS_LABELS.cancelled}</SelectItem>
          </SelectContent>
        </Select>

        <Button variant="outline" size="sm" onClick={fetchExecutions}>
          <RefreshCw className="h-4 w-4 mr-2" />
          Refresh
        </Button>
      </div>

      {/* Table */}
      {loading ? (
        <RowsSkeleton rows={5} label="Loading runs…" />
      ) : error ? (
        <div className="flex flex-col items-center justify-center py-12">
          <div className="text-red-600 mb-4">{error}</div>
          <Button onClick={fetchExecutions}>Retry</Button>
        </div>
      ) : filteredExecutions.length === 0 ? (
        statusFilter === "all" ? (
          <EmptyState icon={History} title="No runs yet" description="Run this pipeline and its runs appear here." />
        ) : (
          <EmptyState
            icon={History}
            title="No runs with this status"
            description="Pick All Statuses to see every run."
          />
        )
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Execution ID</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Started</TableHead>
                <TableHead>Duration</TableHead>
                <TableHead>Records</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filteredExecutions.map((execution) => {
                const statusConfig = statusPresentation(execution.status)
                const StatusIcon = statusConfig.icon

                return (
                  <TableRow key={execution.id}>
                    <TableCell className="font-mono text-xs">
                      {execution.id.slice(0, 8)}...
                    </TableCell>

                    <TableCell>
                      <Badge className={`gap-1 ${statusConfig.color}`} variant="outline">
                        <StatusIcon className={`h-3 w-3 ${execution.status === "running" ? "animate-spin" : ""}`} />
                        {statusConfig.label}
                      </Badge>
                    </TableCell>

                    <TableCell>
                      <div className="text-sm">
                        {formatRelativeTime(new Date(execution.start_time))}
                      </div>
                      <div className="text-xs text-zinc-500 dark:text-zinc-400">
                        {/* Names its zone (#56 retest: the bare "18 Sep, 04:30" gave none). */}
                        <LocalDateTime value={execution.start_time} fallback="—" />
                      </div>
                    </TableCell>

                    <TableCell>
                      {execution.end_time ? (
                        formatDuration(execution.start_time, execution.end_time)
                      ) : execution.status === "running" ? (
                        <span className="text-zinc-400">In progress</span>
                      ) : (
                        <span className="text-zinc-400">—</span>
                      )}
                    </TableCell>

                    <TableCell>
                      {execution.metrics?.records_processed !== undefined
                        ? execution.metrics.records_processed.toLocaleString()
                        : "—"}
                    </TableCell>

                    <TableCell className="text-right">
                      <div className="flex items-center justify-end gap-2">
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => setSelectedExecution(execution)}
                        >
                          <Eye className="h-4 w-4 mr-1" />
                          View
                        </Button>
                        {execution.status === "failed" && (
                          <DropdownMenu>
                            <DropdownMenuTrigger asChild>
                              {/*
                                Named for what it does. This control starts a new
                                run of the PIPELINE — `tryRun` closes over
                                pipelineId and the selected execution is never an
                                input — and there is no execution-scoped retry
                                endpoint to call. Labelled "Retry" inside a row it
                                cannot reach, it promised to re-run that run.
                              */}
                              <Button variant="ghost" size="sm">
                                <RotateCcw className="h-4 w-4 mr-1" />
                                Re-run pipeline
                              </Button>
                            </DropdownMenuTrigger>
                            <DropdownMenuContent align="end">
                              <DropdownMenuItem
                                onClick={() => void tryRun("resume", false)}
                              >
                                Resume from the pipeline&apos;s checkpoints
                              </DropdownMenuItem>
                              {reloadSupported && (
                                <DropdownMenuItem
                                  onClick={() => void tryRun("reload", false)}
                                >
                                  Reload everything from scratch
                                </DropdownMenuItem>
                              )}
                            </DropdownMenuContent>
                          </DropdownMenu>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>

          {/* Pagination */}
          {totalPages > 1 && (
            <div className="flex items-center justify-between pt-4">
              <div className="text-sm text-zinc-600 dark:text-zinc-400">
                Page {currentPage} of {totalPages}
              </div>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={currentPage === 1}
                  onClick={() => setCurrentPage((p) => Math.max(1, p - 1))}
                >
                  Previous
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={currentPage === totalPages}
                  onClick={() => setCurrentPage((p) => p + 1)}
                >
                  Next
                </Button>
              </div>
            </div>
          )}
        </>
      )}
      </section>

      {/*
        The detail dialog is its own component because it now asks the API
        questions the list row cannot answer. Previously it re-rendered the row
        object it was handed — so no amount of layout work could have made it
        show more than the row already showed.
      */}
      <ExecutionDetailDialog
        execution={selectedExecution}
        pipelineId={pipelineId}
        previousExecution={previousExecution}
        onClose={() => setSelectedExecution(null)}
        onRerun={(mode) => {
          // Resume the PIPELINE from the PIPELINE's checkpoints (the safe
          // default). Not this execution's — `tryRun` never sees the selected
          // execution, and for a week-old failure the pipeline's checkpoints are
          // somewhere else entirely.
          setSelectedExecution(null)
          void tryRun(mode, false)
        }}
        onOpenFullDetail={(executionId) => {
          // Capture the id before closing — clearing the selection unmounts the
          // dialog and with it the value we are routing to.
          setSelectedExecution(null)
          router.push(`/executions/${executionId}`)
        }}
      />

      <PreMigrationAssessmentModal
        open={!!assessmentReport}
        onOpenChange={(open) => {
          if (!open) {
            setAssessmentReport(null)
            setPendingRunMode(null)
            setSubmittingProceed(false)
          }
        }}
        report={assessmentReport}
        assessmentTabHref={`/pipelines/${pipelineId}?tab=assessment`}
        submitting={submittingProceed}
        onProceed={async (nominatedKeys) => {
          if (!pendingRunMode) return
          setSubmittingProceed(true)
          const result = await tryRun(pendingRunMode, true, nominatedKeys)
          // On success or non-assessment error, close the modal.
          // (tryRun already cleared/changed state in the error path.)
          if (!result.awaitingAck) {
            setAssessmentReport(null)
            setPendingRunMode(null)
          }
          setSubmittingProceed(false)
        }}
      />
    </div>
  )
}
