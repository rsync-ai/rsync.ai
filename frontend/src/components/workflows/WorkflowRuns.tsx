"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { Loader2, RefreshCw, Square } from "lucide-react"

import { RunDurationChart, usePointedRun, type ChartRun } from "@/components/explorer/RunDurationChart"
import { RunHealthKpis, type HealthRun } from "@/components/runs/RunHealthKpis"
import { formatAbsoluteTime } from "@/lib/utils"
import { formatDurationOrDash } from "@/lib/duration"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import {
  RunOutcomeBadge,
  WORKFLOW_RUN_KINDS,
  runDurationMs,
  runOutcome,
  type RunOutcome,
} from "@/components/workflows/runOutcome"
import { usePollWhileRunning } from "@/components/workflows/usePollWhileRunning"
import { conditionLabel, stepName } from "@/components/workflows/workflowWords"
import {
  conditionStep,
  definitionQuery,
  listWorkflowRuns,
  stopWorkflowRun,
  type StepRunStatus,
  type Workflow,
  type WorkflowDefinition,
  type WorkflowRun,
  type WorkflowStepRun,
} from "@/lib/api/workflows"

const TRIGGER_LABEL: Record<string, string> = {
  schedule: "Schedule",
  after_pipeline: "After pipeline",
  manual: "Run now",
}

type RunFilter = "" | "failed" | "sent" | "not_sent"
const RUN_FILTERS: { value: RunFilter; label: string; empty: string }[] = [
  { value: "", label: "All", empty: "No runs yet. Test it, then turn it on or click Run now." },
  { value: "failed", label: "Failed", empty: "No failed runs." },
  { value: "sent", label: "Sent", empty: "No run has sent a message." },
  { value: "not_sent", label: "Not sent", empty: "No run has ended without sending." },
]

// Not sent: the condition was not true, or the quiet period held the message back.
const FILTER_OUTCOMES: Record<Exclude<RunFilter, "">, RunOutcome[]> = {
  failed: ["failed"],
  sent: ["sent"],
  not_sent: ["nothing_to_send", "held_back", "succeeded"],
}

const STEP_STATUS_WORDS: Record<StepRunStatus, string> = {
  running: "running",
  waiting: "waiting for a run already going",
  succeeded: "succeeded",
  warned: "succeeded with a warning",
  failed: "failed",
  skipped: "skipped",
  stopped: "stopped",
}

const link = "text-violet-700 hover:underline dark:text-violet-300"

/**
 * A workflow's last 50 runs, laid out as a scheduled query's are: how they
 * have been doing as cards, their durations as bars, a filter by what they did, and when, how long, why, what
 * and the detail of each. Outcomes only: a run stores no row values. Polls
 * while a run is in progress. A run that started before the workflow was last
 * edited says so, with the query it ran (workflow_runs.definition). With
 * several conditions, a run that sent names the condition that was true. A
 * run of the steps lists how each step went, with a link to the pipeline run
 * or the model it started; an owner or admin can stop a run still going.
 */
export function WorkflowRuns({
  workflow: w,
  canManage = false,
  refreshKey = 0,
  onChanged,
}: {
  workflow: Workflow
  /** An owner or admin: may stop a run. */
  canManage?: boolean
  refreshKey?: number
  /** After a Stop, so the page reloads the workflow's last run. */
  onChanged?: () => void
}) {
  const workflowId = w.id
  const [runs, setRuns] = useState<WorkflowRun[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [filter, setFilter] = useState<RunFilter>("")
  const [stopping, setStopping] = useState<string | null>(null)
  const [stopError, setStopError] = useState<string | null>(null)

  const [version, setVersion] = useState(0)
  const load = useCallback(() => setVersion((v) => v + 1), [])

  // Every read takes the next ticket, and its answer is applied only while
  // that ticket is the latest: an answer a later read overtook is dropped.
  const ticket = useRef(0)
  const read = useCallback(
    (alive: () => boolean = () => true) => {
      const mine = ++ticket.current
      const current = () => alive() && mine === ticket.current
      return listWorkflowRuns(workflowId).then(
        (r) => {
          if (!current()) return
          setRuns(r)
          setError(null)
        },
        (err: unknown) => {
          if (current()) setError(err instanceof Error ? err.message : "Could not load the runs")
        },
      )
    },
    [workflowId],
  )

  // A response that lands after unmount, or after the workflow changed, is dropped.
  useEffect(() => {
    let alive = true
    void read(() => alive)
    return () => {
      alive = false
    }
  }, [read, refreshKey, version])

  const stop = async (r: WorkflowRun) => {
    setStopping(r.id)
    setStopError(null)
    try {
      await stopWorkflowRun(workflowId, r.id)
      onChanged?.()
    } catch (err) {
      setStopError(err instanceof Error ? err.message : "Could not stop the run")
    } finally {
      setStopping(null)
      load()
    }
  }

  const running = runs?.some((r) => r.status === "running") ?? false
  usePollWhileRunning(running, read)

  // The server sends the last 50 runs, so the filter is this page's, as the chart is.
  const shown = (runs ?? []).filter((r) => filter === "" || FILTER_OUTCOMES[filter].includes(runOutcome(r)))
  // A run still going has no duration yet: it is in the table, not the chart.
  const bars: ChartRun[] = shown
    .filter((r) => r.status !== "running")
    .map((r) => ({ run_id: r.id, status: runOutcome(r), duration_ms: runDurationMs(r), started_at: r.started_at }))
  const empty = RUN_FILTERS.find((f) => f.value === filter)?.empty
  const { point, rowProps } = usePointedRun()

  return (
    <div className="space-y-3">
      {runs && runs.length > 0 && <RunHealthKpis runs={runs.map(healthRun)} />}
      <Card className="flex h-full flex-col overflow-hidden p-0" data-testid="workflow-runs-card">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b px-4 py-3 dark:border-zinc-800">
          <h2 className="text-sm font-semibold text-zinc-900 dark:text-white">Runs</h2>
          <div className="flex flex-wrap items-center gap-1">
            <div role="group" aria-label="Filter runs by outcome" className="flex flex-wrap gap-1">
              {RUN_FILTERS.map((f) => (
                <Button
                  key={f.value || "all"}
                  size="sm"
                  variant={filter === f.value ? "secondary" : "ghost"}
                  className="h-7 px-2 text-xs"
                  aria-pressed={filter === f.value}
                  onClick={() => setFilter(f.value)}
                >
                  {f.label}
                </Button>
              ))}
            </div>
            <Button size="sm" variant="ghost" className="h-7 px-2 text-xs" onClick={load}>
              <RefreshCw className="mr-1 h-3.5 w-3.5" /> Refresh
            </Button>
          </div>
        </div>

        {stopError && (
          <p role="alert" className="border-b px-4 py-2 text-xs text-red-600 dark:border-zinc-800 dark:text-red-400">
            {stopError}
          </p>
        )}

        {error ? (
          <div role="alert" className="flex flex-col items-center gap-2 px-4 py-10 text-center">
            <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
            <Button size="sm" variant="outline" onClick={load}>
              Retry
            </Button>
          </div>
        ) : runs === null ? (
          <div role="status" className="flex items-center justify-center gap-2 py-10 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 animate-spin" />
            Loading runs…
          </div>
        ) : shown.length === 0 ? (
          <p className="px-4 py-10 text-center text-sm text-zinc-500 dark:text-zinc-400">{empty}</p>
        ) : (
          <>
            <RunDurationChart runs={bars} kinds={WORKFLOW_RUN_KINDS} onRunClick={point} />
            <div className="overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="whitespace-nowrap">Started</TableHead>
                    <TableHead>Duration</TableHead>
                    <TableHead>Trigger</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Result</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {shown.map((r) => (
                    <TableRow key={r.id} {...rowProps(r.id)}>
                      <TableCell className="whitespace-nowrap align-top text-xs text-zinc-600 dark:text-zinc-400">
                        {formatAbsoluteTime(r.started_at)}
                      </TableCell>
                      <TableCell className="whitespace-nowrap align-top text-xs tabular-nums">
                        {formatDurationOrDash(runDurationMs(r))}
                      </TableCell>
                      <TableCell className="whitespace-nowrap align-top text-xs text-zinc-600 dark:text-zinc-400">
                        {TRIGGER_LABEL[r.trigger] ?? r.trigger}
                      </TableCell>
                      <TableCell className="align-top">
                        <div className="flex flex-col items-start gap-1">
                          <RunOutcomeBadge outcome={runOutcome(r)} />
                          {r.status === "running" && r.stop_requested && (
                            <span className="text-xs text-zinc-500 dark:text-zinc-400">Stopping…</span>
                          )}
                          {r.status === "running" && !r.stop_requested && canManage && (
                            <Button
                              size="sm"
                              variant="outline"
                              className="h-6 px-2 text-xs"
                              disabled={stopping === r.id}
                              onClick={() => void stop(r)}
                            >
                              <Square className="mr-1 h-3 w-3" aria-hidden />
                              {stopping === r.id ? "Stopping…" : "Stop"}
                            </Button>
                          )}
                        </div>
                      </TableCell>
                      <TableCell className="min-w-[200px] max-w-[420px] align-top text-xs">
                        <RunResult run={r} workflow={w} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          </>
        )}
      </Card>
    </div>
  )
}

/**
 * A run as the health cards count it: a run that finished without failing did its
 * work whether or not it sent; one still going or stopped is not counted.
 */
function healthRun(r: WorkflowRun): HealthRun {
  const outcome = runOutcome(r)
  const status = outcome === "failed" || outcome === "running" || outcome === "stopped" ? outcome : "succeeded"
  return { status, duration_ms: runDurationMs(r), started_at: r.started_at }
}

function RunResult({ run: r, workflow: w }: { run: WorkflowRun; workflow: Workflow }) {
  const branch = runBranchLabel(r, w.definition)
  const sql = r.definition ? definitionQuery(r.definition)?.sql : undefined
  return (
    <div className="space-y-0.5">
      {branch && <div className="font-medium text-zinc-900 dark:text-zinc-100">{branch}</div>}
      {r.error && (
        <div className="line-clamp-3 break-words text-red-600 dark:text-red-400" title={r.error}>
          {r.error}
        </div>
      )}
      {r.steps && r.steps.length > 0 && <RunSteps run={r} workflow={w} />}
      {r.deliveries.map((d) => (
        <div key={`${d.step_id ?? ""}:${d.channel_id}`} className={d.status === "failed" ? "text-red-600 dark:text-red-400" : "text-zinc-700 dark:text-zinc-300"}>
          {`${d.name}: ${d.status}${d.error ? ` (${d.error})` : ""}`}
        </div>
      ))}
      {/* A step's note is already under its step: a condition's "nothing was sent" read twice in one row. */}
      {r.notes
        .filter((n) => !r.steps?.some((s) => s.note === n))
        .map((n) => (
          <div key={n} className="text-zinc-500 dark:text-zinc-400">
            {n}
          </div>
        ))}
      {typeof r.row_count === "number" && (
        <div className="text-zinc-500 dark:text-zinc-400">
          {r.row_count} row{r.row_count === 1 ? "" : "s"}
        </div>
      )}
      {r.earlier_version && (
        <details>
          <summary className="cursor-pointer">
            <Badge variant="secondary">Ran an earlier version</Badge>
          </summary>
          {sql !== undefined && (
            <pre
              aria-label="SQL it ran"
              className="mt-1 max-h-40 overflow-auto whitespace-pre-wrap rounded-md bg-zinc-50 p-2 font-mono text-[11px] text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200"
            >
              {sql}
            </pre>
          )}
        </details>
      )}
    </div>
  )
}

/**
 * How each step of a run went, in the order the run's definition lists them:
 * its status, its attempt when it was retried, a link to the pipeline run or
 * the model it started, and why it failed or was skipped. A step that has not
 * started is not listed.
 */
function RunSteps({ run: r, workflow: w }: { run: WorkflowRun; workflow: Workflow }) {
  const def = r.definition ?? w.definition
  const byId = new Map((r.steps ?? []).map((s) => [s.step_id, s]))
  const ordered: WorkflowStepRun[] = [
    ...def.steps.map((s) => byId.get(s.id)).filter((s): s is WorkflowStepRun => !!s),
    ...(r.steps ?? []).filter((s) => !def.steps.some((d) => d.id === s.step_id)),
  ]
  const name = (sr: WorkflowStepRun) => {
    const s = def.steps.find((d) => d.id === sr.step_id)
    const n = s ? stepName({ ...w, definition: def }, s) : sr.step_id
    return n.charAt(0).toUpperCase() + n.slice(1)
  }
  // A condition's message skipped because another case was taken: the run's result
  // already names the case that sent, and the condition's note says when none did,
  // so its "Condition 2 was true, not this one." said it again. A skip for any other
  // reason (the condition failed) keeps its note.
  const decided = (r.steps ?? []).some((sr) => sr.status === "succeeded" && conditionStep(def)?.id === sr.step_id)
  const note = (sr: WorkflowStepRun) => {
    const s = def.steps.find((d) => d.id === sr.step_id)
    return sr.status === "skipped" && s?.type === "send" && s.case !== undefined && decided ? null : sr.note
  }
  return (
    <ul aria-label="Steps" className="space-y-0.5">
      {ordered.map((sr) => (
        <li
          key={sr.step_id}
          className={sr.status === "failed" ? "text-red-600 dark:text-red-400" : "text-zinc-700 dark:text-zinc-300"}
        >
          {`${name(sr)}: ${STEP_STATUS_WORDS[sr.status] ?? sr.status}`}
          {sr.attempt > 1 && ` (attempt ${sr.attempt})`}
          {sr.execution_id && (
            <>
              {" · "}
              <Link href={`/executions/${sr.execution_id}`} className={link}>
                Open the run
              </Link>
            </>
          )}
          {sr.model_id && (
            <>
              {" · "}
              <Link href={`/models/${sr.model_id}`} className={link}>
                Open the model
              </Link>
            </>
          )}
          {sr.error && <div className="line-clamp-3 break-words pl-3" title={sr.error}>{sr.error}</div>}
          {note(sr) && <div className="pl-3 text-zinc-500 dark:text-zinc-400">{note(sr)}</div>}
        </li>
      ))}
    </ul>
  )
}

/**
 * "Condition n", or "Otherwise", for a run of a workflow with several conditions
 * that sent; null otherwise. A run from before conditions were numbered (no
 * branch) sent condition 1. current is the workflow's definition now; a run of
 * an earlier version reads its own.
 */
export function runBranchLabel(r: WorkflowRun, current: WorkflowDefinition): string | null {
  const def = r.definition ?? current
  const n = conditionStep(def)?.conditions?.length ?? 0
  if (n <= 1 || !r.condition_met) return null
  return conditionLabel(def, r.branch ?? 0)
}
