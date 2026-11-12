import { notFound, unstable_rethrow } from "next/navigation"
import Link from "next/link"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { ArrowLeft, Clock, Timer, AlertTriangle, Calendar, Play, Layers, Rows3 } from "lucide-react"
import { cn, formatAgeMs } from "@/lib/utils"
import { formatDurationBetween } from "@/lib/duration"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { ExecutionDetailClient } from "@/components/executions/ExecutionDetailClient"
import { TransformExecutionLogsPanel, type TransformExecutionLog } from "@/components/transforms/TransformExecutionLogsPanel"
import { RecentRunsCard } from "@/components/executions/RecentRunsCard"
import { durationChange } from "@/components/executions/run-history"
import type { ChartRun } from "@/components/runs/run-chart-specs"
import { RunLogPanel } from "@/components/executions/RunLogPanel"
import { DiagnosisCard, type DiagnosisData } from "@/components/chat/DiagnosisCard"
import { CopyId } from "@/components/kit/CopyId"
import { StatCard } from "@/components/kit/StatCard"
import { StatusPill } from "@/components/kit/StatusPill"
import { API_ENDPOINTS } from "@/lib/config/api"
import {
  pickExecutionFreshness,
  type FreshnessLabel,
  type RuntimeLivenessLike,
} from "@/lib/pipeline/executionFreshness"
import { cookies } from "next/headers"
import { activeWorkspaceCookieHeader } from "@/lib/workspace/server-workspace"
import {
  EXECUTION_STATUS_TONES,
  executionStatusConfig as statusConfig,
  normalizeExecutionStatus,
  type ExecutionStatus,
  type ExecutionStatusConfig,
} from "@/lib/execution-status"

export const dynamic = "force-dynamic"

interface Props {
  params: Promise<{ id: string }>
}

export default async function ExecutionDetailPage({ params }: Props) {
  const { id } = await params
  
  const cookieStore = await cookies()
  const authToken = cookieStore.get("auth_token")?.value
  // API Gateway expects the raw token value (no Bearer prefix) in local dev.
  // Forward the active-workspace selection (cookie mirror) so all three fetches
  // below scope to the chosen workspace instead of the caller's personal one.
  const authHeaders: Record<string, string> = {
    ...(authToken ? { Authorization: authToken } : {}),
    ...activeWorkspaceCookieHeader(cookieStore),
  }

  let execution: any = null
  let duration = "0s"
  let nodeResults: any[] = []
  let transformLogs: TransformExecutionLog[] = []
  let diagnosis: DiagnosisData | null = null
  let config: ExecutionStatusConfig = statusConfig.pending
  let freshnessAge = "—"
  // ISO string, rendered by <LocalDateTime> so it shows in the viewer's zone.
  let freshnessAbs: string | null = null
  let freshnessLabel: FreshnessLabel = "Last activity"
  // The pipeline's last 10 runs, newest first, for the chart and the Duration
  // card's change. Null when they could not be read: the page then shows neither.
  let recentRuns: ChartRun[] | null = null
  let rowsWritten: number | undefined

  try {
    const res = await fetch(API_ENDPOINTS.EXECUTIONS.GET_INTERNAL(id), {
      cache: "no-store",
      headers: {
        ...authHeaders,
      },
    })

    if (!res.ok) {
      if (res.status === 404) notFound()
      throw new Error(`Failed to fetch execution: ${res.status}`)
    }

    const rawExecution: any = await res.json()
    
    // Safely transform data
    execution = {
      id: rawExecution.id || id,
      pipelineId: rawExecution.pipeline_id || "",
      status: normalizeExecutionStatus(rawExecution.status, rawExecution.error_message),
      startedAt: rawExecution.start_time ? new Date(rawExecution.start_time) : new Date(),
      finishedAt: rawExecution.end_time ? new Date(rawExecution.end_time) : null,
      liveStream: rawExecution.live_stream === true,
      // The gateway sends a count only when it is above zero (GetExecution).
      recordsProcessed: typeof rawExecution.metrics?.records_processed === "number" ? rawExecution.metrics.records_processed : undefined,
      error: rawExecution.error_message || null,
      nodeResults: Array.isArray(rawExecution.node_results) ? rawExecution.node_results : [],
      pipeline: rawExecution.pipeline || { 
        id: rawExecution.pipeline_id || "", 
        name: rawExecution.pipeline_name || "Unknown Pipeline", 
        status: "draft", 
        nodes: [], 
        description: "" 
      },
    }

    config = statusConfig[execution.status as keyof typeof statusConfig] || statusConfig.pending

    duration = formatDurationBetween(execution.startedAt, execution.finishedAt)
    nodeResults = execution.nodeResults

    try {
      const logsRes = await fetch(API_ENDPOINTS.EXECUTIONS.TRANSFORMS_INTERNAL(id), {
        cache: "no-store",
        headers: {
          ...authHeaders,
        },
      })
      if (logsRes.ok) {
        const logsJson: any = await logsRes.json()
        transformLogs = Array.isArray(logsJson?.logs) ? logsJson.logs : []
      }
    } catch {
      transformLogs = []
    }

    // Freshness KPI. A live stream has no end time, so it reads the pipeline's
    // runtime liveness — the same "last event" the pipeline page shows. Best-effort:
    // an unreadable runtime falls back to the landed time.
    let liveness: RuntimeLivenessLike | null = null
    if (execution.liveStream && execution.pipelineId) {
      try {
        const rtRes = await fetch(API_ENDPOINTS.PIPELINES.RUNTIME_INTERNAL(execution.pipelineId), {
          cache: "no-store",
          headers: { ...authHeaders },
        })
        if (rtRes.ok) {
          const rt = (await rtRes.json()) as { liveness?: RuntimeLivenessLike | null } | null
          liveness = rt && typeof rt.liveness === "object" ? rt.liveness : null
        }
      } catch {
        liveness = null
      }
    }
    const now = Date.now()
    const freshness = pickExecutionFreshness({
      status: execution.status,
      liveStream: execution.liveStream,
      finishedAt: execution.finishedAt,
      transformTimes: transformLogs.map((l) => l.updated_at || l.created_at),
      liveness,
      now,
    })
    freshnessLabel = freshness.label
    if (freshness.at !== null) {
      freshnessAbs = new Date(freshness.at).toISOString()
      freshnessAge = formatAgeMs(now - freshness.at)
    }

    // The pipeline's recent runs (the same trends call its Overview makes).
    // Best-effort: without them the page has no chart and no change, nothing else.
    if (execution.pipelineId) {
      try {
        const trRes = await fetch(API_ENDPOINTS.PIPELINES.TRENDS_INTERNAL(execution.pipelineId, 10), {
          cache: "no-store",
          headers: { ...authHeaders },
        })
        if (trRes.ok) {
          const tr = (await trRes.json()) as { recent_executions?: unknown } | null
          recentRuns = Array.isArray(tr?.recent_executions) ? (tr.recent_executions as ChartRun[]) : null
        }
      } catch {
        recentRuns = null
      }
    }
    rowsWritten = execution.recordsProcessed

    // Fetch the Phase 3 diagnosis for failed/silent-drop executions. Best-effort:
    // a missing endpoint or empty response leaves the section hidden rather than
    // breaking the page.
    if (FAILURE_STATUSES.has(execution.status)) {
      try {
        const diagRes = await fetch(API_ENDPOINTS.EXECUTIONS.DIAGNOSE_INTERNAL(id), {
          cache: "no-store",
          headers: { ...authHeaders },
        })
        if (diagRes.ok) {
          const diagJson: any = await diagRes.json()
          // chat_diagnose response wraps the payload in `data`
          if (diagJson && diagJson.data && typeof diagJson.data === "object") {
            const d = diagJson.data
            diagnosis = {
              execution_id: String(d.execution_id || id),
              pipeline_id: String(d.pipeline_id || ""),
              pipeline_name: String(d.pipeline_name || execution.pipeline?.name || ""),
              status: String(d.status || execution.status),
              category: String(d.category || ""),
              action: String(d.action || ""),
              action_label: String(d.action_label || ""),
              deep_link: String(d.deep_link || ""),
              confidence: typeof d.confidence === "number" ? d.confidence : 0,
              rationale: String(d.rationale || ""),
              source_rows: typeof d.source_rows === "number" ? d.source_rows : undefined,
              landed_rows: typeof d.landed_rows === "number" ? d.landed_rows : undefined,
            }
          }
        }
      } catch {
        // ignore
      }
    }
  } catch (error: any) {
    // notFound()/redirect() signal themselves by throwing. Next tags those errors
    // with a `digest` (`NEXT_HTTP_ERROR_FALLBACK;404`) — the old `message ===
    // 'NEXT_NOT_FOUND'` check never matched, so a 404 fell through to the generic
    // error card below and the not-found boundary was unreachable. unstable_rethrow
    // is Next's own guard: it re-throws framework control-flow errors and returns
    // for everything else.
    unstable_rethrow(error)
    console.error("Error loading execution details:", error)
    // Fallback error state or rethrow if critical
    // For now, render error state
    return (
      <div className="flex flex-col items-center justify-center min-h-[400px] gap-4">
        <AlertTriangle className="h-10 w-10 text-red-500" />
        <h2 className="text-xl font-semibold">Failed to load execution details</h2>
        <p className="text-zinc-500 dark:text-zinc-400">{String(error)}</p>
        <Link href="/runs">
          <Button variant="outline">Back to Runs</Button>
        </Link>
      </div>
    )
  }

  const status = execution.status as ExecutionStatus
  const failed = FAILURE_STATUSES.has(status)
  const running = status === "running" || status === "pending"
  const change = recentRuns ? durationChange(recentRuns, execution.id, execution.pipelineId) : undefined

  return (
    <div className="space-y-5">
      {/* Header: the pipeline this run belongs to, its status, and what can be done. */}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-2">
          <Button asChild variant="ghost" size="icon" className="-ml-2 shrink-0">
            <Link href="/runs" aria-label="Back to runs">
              <ArrowLeft className="h-5 w-5" />
            </Link>
          </Button>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="truncate text-xl font-semibold text-zinc-900 dark:text-white">
                {execution.pipeline.name}
              </h1>
              <StatusPill tone={EXECUTION_STATUS_TONES[status] ?? "skip"}>{config.label}</StatusPill>
            </div>
            <p className="mt-1 flex items-center gap-1.5 text-sm text-zinc-500 dark:text-zinc-400">
              Run <CopyId value={execution.id} label="run ID" />
            </p>
          </div>
        </div>
        <ExecutionDetailClient
          execution={{
            id: execution.id,
            status: execution.status,
            pipelineId: execution.pipelineId || "",
            liveStream: execution.liveStream === true,
          }}
        />
      </div>

      {/* Always four cards: a node-count card used to make a fifth, but batch and
          CDC runs record no node results, so most runs drew four cards in a
          five-column row. The count now reads in the timeline's header. */}
      <div className="grid grid-cols-1 gap-4 @xl:grid-cols-2 @3xl:grid-cols-4">
        <StatCard
          title="Started"
          icon={Calendar}
          value={formatAgeMs(Date.now() - execution.startedAt.getTime())}
          hint={<LocalDateTime value={execution.startedAt} />}
        />
        <StatCard
          title="Duration"
          icon={Timer}
          value={duration}
          hint={
            running ? (
              "So far"
            ) : execution.finishedAt ? (
              <>
                {change && <span className="block">{change}</span>}
                Ended <LocalDateTime value={execution.finishedAt} />
              </>
            ) : (
              change
            )
          }
        />
        <StatCard
          title="Rows written"
          icon={Rows3}
          href={execution.pipelineId ? `/pipelines/${execution.pipelineId}?tab=table-stats` : undefined}
          value={rowsWritten ?? "—"}
          hint={
            rowsWritten !== undefined
              ? "Per table on the pipeline's Table statistics"
              : running
                ? "Counted as each table lands"
                : "No rows recorded for this run"
          }
        />
        <StatCard
          title="Data freshness"
          icon={Clock}
          value={freshnessAge}
          hint={
            freshnessAbs ? (
              <>
                {freshnessLabel} <LocalDateTime value={freshnessAbs} />
              </>
            ) : undefined
          }
        />
      </div>

      {/* Only for a run that did not succeed: a succeeded run can keep the error
          message of an attempt it recovered from. */}
      {failed && execution.error && (
        <div
          role="alert"
          className="flex items-start gap-3 rounded-xl border border-red-200 bg-red-50 p-4 dark:border-red-900/60 dark:bg-red-950/30"
        >
          <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-red-600 dark:text-red-400" aria-hidden />
          <div className="min-w-0">
            <p className="text-sm font-semibold text-red-700 dark:text-red-300">
              {status === "failed" ? "This run failed" : config.label}
            </p>
            <p className="mt-1 break-words text-sm text-red-700 dark:text-red-300">{execution.error}</p>
          </div>
        </div>
      )}

      {/* Phase 3/4 Healer Diagnosis — auto-renders for failure statuses */}
      {diagnosis && (
        <div>
          <h2 className="mb-2 flex items-center gap-2 text-sm font-semibold text-zinc-700 dark:text-zinc-300">
            <AlertTriangle className="h-4 w-4 text-amber-500" aria-hidden />
            Healer diagnosis
          </h2>
          <DiagnosisCard data={diagnosis} />
        </div>
      )}

      {recentRuns && execution.pipelineId && (
        <RecentRunsCard runs={recentRuns} executionId={execution.id} pipelineId={execution.pipelineId} />
      )}

      {/* The run's own log — where "View Logs" in a pipeline's ⋯ menu lands (#logs). */}
      {execution.pipelineId && (
        <RunLogPanel pipelineId={execution.pipelineId} executionId={execution.id} />
      )}

      {/* Most runs run no transforms; an always-empty card read as missing data. */}
      {transformLogs.length > 0 && (
        <Card>
          <CardHeader className="pb-4">
            <CardTitle className="flex items-center gap-2 text-base">
              <Layers className="h-4 w-4" aria-hidden />
              Transform execution logs
            </CardTitle>
          </CardHeader>
          <CardContent>
            <TransformExecutionLogsPanel logs={transformLogs} emptyLabel="No transform execution logs for this execution." />
          </CardContent>
        </Card>
      )}

      {/* Only batch runs record per-node results. A CDC run never has any, and an
          empty "No node results available" card read as missing data. */}
      {nodeResults.length > 0 && (
        <Card>
          <CardHeader className="pb-4">
            <CardTitle className="flex items-center gap-2 text-base">
              <Play className="h-4 w-4" aria-hidden />
              Execution timeline
              <span className="text-sm font-normal text-zinc-500 dark:text-zinc-400">
                {nodeResults.length} {nodeResults.length === 1 ? "node" : "nodes"}
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <div className="space-y-4">
              {nodeResults.map((node: any, index: number) => {
                const nodeStatus = normalizeExecutionStatus(node?.status)
                const nodeConfig = statusConfig[nodeStatus]
                const NodeIcon = nodeConfig.icon

                return (
                  <div key={node?.node_id || index} className="relative">
                    {index < nodeResults.length - 1 && (
                      <div className="absolute bottom-0 left-[21px] top-12 w-0.5 bg-zinc-200 dark:bg-zinc-700" />
                    )}

                    <div className="flex items-start gap-4">
                      <div className={cn("flex h-11 w-11 shrink-0 items-center justify-center rounded-full", nodeConfig.bg)}>
                        <NodeIcon className={cn("h-5 w-5", nodeConfig.color, nodeConfig.animate && "animate-spin")} />
                      </div>

                      <div className="min-w-0 flex-1 pb-6">
                        <div className="flex items-center justify-between gap-2">
                          <div className="min-w-0">
                            <p className="font-semibold text-zinc-900 dark:text-white">
                              {node?.node_name || `Node ${index + 1}`}
                            </p>
                            <p className="mt-0.5 truncate text-xs text-zinc-500 dark:text-zinc-400">{node?.node_id}</p>
                          </div>
                          <StatusPill tone={EXECUTION_STATUS_TONES[nodeStatus]}>{nodeConfig.label}</StatusPill>
                        </div>

                        {node?.duration_ms !== undefined && (
                          <p className="mt-2 text-sm text-zinc-500 dark:text-zinc-400">
                            Duration: {node.duration_ms}ms
                            {node.row_count !== undefined && ` • ${node.row_count} rows processed`}
                          </p>
                        )}

                        {node?.error && (
                          <p className="mt-2 text-sm text-red-600 dark:text-red-400">Error: {node.error}</p>
                        )}
                      </div>
                    </div>
                  </div>
                )
              })}
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  )
}

/** Statuses that are not a success: the error card and the diagnosis show for these. */
const FAILURE_STATUSES = new Set<ExecutionStatus>([
  "failed",
  "silent_drop_detected",
  "silent_partial_drop_detected",
  "waiting_for_credential_reauth",
  "waiting_for_user",
])
