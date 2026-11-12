"use client"

import { useEffect, useState, useCallback } from "react"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { useRouter } from "next/navigation"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Card, CardContent } from "@/components/ui/card"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Search, Filter, GitBranch, SearchX, Sparkles } from "lucide-react"
import { EmptyState } from "@/components/kit/EmptyState"
import { RowsSkeleton } from "@/components/kit/PageSkeleton"
import {
  ANY_TIME,
  CreatedRangeFilter,
  createdRangeParams,
  isAnyTime,
  type CreatedRange,
} from "@/components/pipeline/CreatedRangeFilter"
import { PipelinesTable } from "@/components/pipeline/PipelinesTable"
import type { PipelineListItem } from "@/components/pipeline/PipelinesTable"
import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import { toast } from "sonner"
import { deleteWarningToast, readDeleteWarnings } from "@/lib/utils/delete-warnings"
import { classifyError, type AppError } from "@/lib/utils/error-handling"
import { ErrorPage } from "@/components/ui/error-banner"
import type { ApiErrorBody } from "@/lib/api/types"
import { UpgradeModal } from "@/components/plan/UpgradeModal"
import {
  AssessmentRequiredError,
  PlanLimitError,
  executePipelineWithRunMode,
  type AssessmentReport,
  type PlanLimitPayload,
} from "@/lib/api/pipelines"
import { PreMigrationAssessmentModal } from "@/components/pipeline/PreMigrationAssessmentModal"
import { exportPipelineConfig } from "@/lib/pipeline/pipelineMenuActions"
import { pipelineIsCDC } from "@/lib/pipeline/syncMode"

interface PipelinesPageState {
  pipelines: PipelineListItem[]
  pagination: {
    page: number
    per_page: number
    total: number
    total_pages: number
    has_next: boolean
    has_prev: boolean
  }
  loading: boolean
  error: AppError | null
}

const ACTIVE_POLL_MS = 5_000
const IDLE_POLL_MS = 30_000

/** 5s while a run is in flight on this page, 30s otherwise — never off. */
export function pipelinesPollInterval(pipelines: PipelineListItem[]): number {
  const active = pipelines.some(
    (p) => p.derived_status === "running" || p.derived_status === "waiting_for_user",
  )
  return active ? ACTIVE_POLL_MS : IDLE_POLL_MS
}

export function PipelinesPageClient() {
  const router = useRouter()

  // State
  // Role gate: creating pipelines is member+ (mirrors the backend
  // requireWorkspaceRole on CreatePipeline). Viewers see the list read-only.
  const { can } = useWorkspaceRole()
  const canCreate = can("create_pipeline")
  const [isAnyMenuOpen, setIsAnyMenuOpen] = useState(false)
  const [deleteDialogOpen, setDeleteDialogOpen] = useState(false)
  const [deleteTargetId, setDeleteTargetId] = useState<string | null>(null)
  const [planLimitPayload, setPlanLimitPayload] = useState<PlanLimitPayload | null>(null)

  // Pre-migration assessment modal state. handleRunPipeline goes through
  // executePipelineWithRunMode, which gates on a 422 assessment the same
  // way PipelineActions does; without this the run silently fails.
  const [assessmentReport, setAssessmentReport] = useState<AssessmentReport | null>(null)
  const [pendingRunId, setPendingRunId] = useState<string | null>(null)
  const [submittingProceed, setSubmittingProceed] = useState(false)
  const [state, setState] = useState<PipelinesPageState>({
    pipelines: [],
    pagination: {
      page: 1,
      per_page: 25,
      total: 0,
      total_pages: 0,
      has_next: false,
      has_prev: false,
    },
    loading: true,
    error: null,
  })

  // Filters
  const [searchQuery, setSearchQuery] = useState("")
  const [statusFilter, setStatusFilter] = useState("all")
  const [typeFilter, setTypeFilter] = useState("all")
  const [createdRange, setCreatedRange] = useState<CreatedRange>(ANY_TIME)
  const [currentPage, setCurrentPage] = useState(1)

  // Panel state removed - redirecting to /chat instead

  // Fetch pipelines
  const fetchPipelines = useCallback(async () => {
    // Don't blank the table during background refresh/polling.
    setState((prev) => ({ ...prev, loading: prev.pipelines.length === 0, error: null }))

    // A switch mid-flight makes this response the previous workspace's rows.
    const isStale = captureWorkspace()

    try {
      const params = new URLSearchParams()
      params.set("page", String(currentPage))
      params.set("per_page", "25")

      if (searchQuery.trim()) params.set("q", searchQuery.trim())
      if (statusFilter !== "all") params.set("status", statusFilter)
      if (typeFilter !== "all") params.set("type", typeFilter)
      // A preset is relative to now, so each poll re-reads it.
      const created = createdRangeParams(createdRange)
      if (created.from) params.set("created_from", created.from)
      if (created.to) params.set("created_to", created.to)

      const res = await authFetch(`${API_ENDPOINTS.PIPELINES.LIST}?${params}`, {
        cache: "no-store",
      })

      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        throw Object.assign(new Error(), { statusCode: res.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || `HTTP ${res.status}` })
      }

      const data = await res.json()
      if (isStale()) return

      setState({
        pipelines: data.pipelines || [],
        pagination: data.pagination || {
          page: 1,
          per_page: 25,
          total: data.total || 0,
          total_pages: Math.ceil((data.total || 0) / 25),
          has_next: false,
          has_prev: false,
        },
        loading: false,
        error: null,
      })
    } catch (err) {
      if (isStale()) return
      setState((prev) => ({
        ...prev,
        loading: false,
        error: classifyError(err, "pipeline.load"),
      }))
    }
  }, [currentPage, searchQuery, statusFilter, typeFilter, createdRange])

  // Initial fetch and polling for running pipelines
  useEffect(() => {
    fetchPipelines()
  }, [fetchPipelines])

  // Poll the list. Fast while any pipeline is running or parked on user input
  // (a HITL prompt answered elsewhere resumes the run); slow otherwise, because a
  // row's status also changes with nothing running here — a Failed CDC stream the
  // healer, a Reload or another tab brings back stayed "Failed" until a manual
  // reload (prod retest 2026-09-18).
  const pollMs = pipelinesPollInterval(state.pipelines)
  useEffect(() => {
    if (isAnyMenuOpen) return

    const interval = setInterval(() => {
      if (typeof document !== "undefined" && document.visibilityState === "hidden") return
      void fetchPipelines()
    }, pollMs)

    return () => {
      clearInterval(interval)
    }
  }, [pollMs, fetchPipelines, isAnyMenuOpen])

  // Re-fetch the list when the active workspace changes (header switcher or
  // another tab). router.refresh() only re-runs server components; this is a
  // client page, so without this the list keeps showing the previous
  // workspace's pipelines until a manual reload. (R4)
  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        void fetchPipelines()
      }),
    [fetchPipelines],
  )

  const handleCreateNew = () => {
    router.push("/chat")
  }

  // Pipeline actions. Runs via executePipelineWithRunMode so the 422
  // pre-migration assessment gate surfaces the modal (same as PipelineActions)
  // and 402 plan limits surface the UpgradeModal — a raw run POST silently
  // failed on both. Returns whether we're awaiting an ack so the modal's
  // Proceed callback can keep itself open on a fresh 422.
  const handleRunPipeline = async (
    id: string,
    ackWarnings = false,
    nominatedKeys?: Record<string, string[]>,
  ) => {
    try {
      await executePipelineWithRunMode(id, "resume", { ackWarnings, nominatedKeys })
      toast.success("Pipeline started")
      fetchPipelines()
      return { awaitingAck: false }
    } catch (err) {
      if (err instanceof AssessmentRequiredError) {
        setAssessmentReport(err.report)
        setPendingRunId(id)
        return { awaitingAck: true }
      }
      if (err instanceof PlanLimitError) {
        setPlanLimitPayload(err.payload)
        return { awaitingAck: false }
      }
      const e = classifyError(err, "pipeline.run")
      toast.error(e.title, { description: e.hint ?? e.message })
      return { awaitingAck: false }
    }
  }

  const handlePausePipeline = async (id: string) => {
    try {
      const res = await authFetch(API_ENDPOINTS.PIPELINES.PAUSE(id), { method: "POST" })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        const e = classifyError(Object.assign(new Error(), { statusCode: res.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || res.statusText }), "pipeline.pause")
        toast.error(e.title, { description: e.hint ?? e.message })
        return
      }
      toast.success("Pipeline paused")
      fetchPipelines()
    } catch (err) {
      const e = classifyError(err, "pipeline.pause")
      toast.error(e.title, { description: e.hint ?? e.message })
    }
  }

  const handleResumePipeline = async (id: string) => {
    try {
      const res = await authFetch(API_ENDPOINTS.PIPELINES.RESUME(id), { method: "POST" })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        const e = classifyError(Object.assign(new Error(), { statusCode: res.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || res.statusText }), "pipeline.resume")
        toast.error(e.title, { description: e.hint ?? e.message })
        return
      }
      toast.success("Pipeline resumed")
      fetchPipelines()
    } catch (err) {
      const e = classifyError(err, "pipeline.resume")
      toast.error(e.title, { description: e.hint ?? e.message })
    }
  }

  const handleStopPipeline = async (id: string) => {
    try {
      const res = await authFetch(API_ENDPOINTS.PIPELINES.STOP(id), { method: "POST" })
      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        const e = classifyError(Object.assign(new Error(), { statusCode: res.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || res.statusText }), "pipeline.stop")
        toast.error(e.title, { description: e.hint ?? e.message })
        return
      }
      toast.success("Pipeline stopped")
      fetchPipelines()
    } catch (err) {
      const e = classifyError(err, "pipeline.stop")
      toast.error(e.title, { description: e.hint ?? e.message })
    }
  }

  const deleteTarget = deleteTargetId ? state.pipelines.find((p) => p.id === deleteTargetId) : undefined
  const deleteTargetRunning =
    deleteTarget?.derived_status === "running" || deleteTarget?.derived_status === "waiting_for_user"
  const deleteTargetIsCDC = pipelineIsCDC(deleteTarget) === true

  const handleDeletePipeline = async (id: string) => {
    setDeleteTargetId(id)
    setDeleteDialogOpen(true)
  }

  const confirmDeletePipeline = async () => {
    const id = deleteTargetId
    if (!id) return

    try {
      const res = await authFetch(API_ENDPOINTS.PIPELINES.DELETE(id), {
        method: "DELETE",
      })

      if (!res.ok) {
        const body = await res.json().catch(() => ({}))
        const e = classifyError(Object.assign(new Error(), { statusCode: res.status, message: (body as ApiErrorBody)?.error || (body as ApiErrorBody)?.message || res.statusText }), "pipeline.delete")
        toast.error(e.title, { description: e.hint ?? e.message })
        return
      }

      // A 200 can still carry teardown warnings (a slot that would not drop,
      // Kafka topics left behind). Answering those with a flat success toast hid
      // every leak behind "Pipeline deleted".
      const warnings = await readDeleteWarnings(res)
      if (warnings.length > 0) {
        const { title, description } = deleteWarningToast(warnings)
        toast.warning(title, { description, duration: 12000 })
      } else {
        toast.success("Pipeline deleted")
      }
      setDeleteDialogOpen(false)
      setDeleteTargetId(null)
      fetchPipelines()
    } catch (err) {
      const e = classifyError(err, "pipeline.delete")
      toast.error(e.title, { description: e.hint ?? e.message })
    }
  }

  const clearFilters = () => {
    setSearchQuery("")
    setStatusFilter("all")
    setTypeFilter("all")
    setCreatedRange(ANY_TIME)
    setCurrentPage(1)
  }

  const hasActiveFilters =
    searchQuery || statusFilter !== "all" || typeFilter !== "all" || !isAnyTime(createdRange)

  return (
    <div className="space-y-5">
      <PageHeader
        heading="Pipelines"
        description="Create and manage your data pipelines with AI assistance"
      />

      {/* No count cards: Home has them. These counted runs, Home's count
          pipelines, and the same word read differently on the two pages. */}
      {/* Filters & Actions Bar */}
      <Card>
        <CardContent className="p-4">
          <div className="flex flex-col gap-4 @3xl:flex-row @3xl:items-center @3xl:justify-between">
            <div className="flex flex-col gap-3 @xl:flex-row @xl:flex-wrap @xl:items-center">
              {/* Search */}
              <div className="relative w-full @xl:w-64">
                <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-zinc-400" />
                <Input
                  placeholder="Search pipelines..."
                  value={searchQuery}
                  onChange={(e) => setSearchQuery(e.target.value)}
                  className="pl-10"
                />
              </div>

              {/* Status + Type filters side by side on mobile. Each trigger
                  shows only the chosen value, so it names its filter itself
                  (axe button-name, critical). */}
              <div className="flex gap-2">
                <Select value={statusFilter} onValueChange={setStatusFilter}>
                  <SelectTrigger className="w-full @md:w-[160px]" aria-label="Status">
                    <SelectValue placeholder="All Statuses" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All Statuses</SelectItem>
                    <SelectItem value="passed">Succeeded</SelectItem>
                    <SelectItem value="failed">Failed</SelectItem>
                    <SelectItem value="running">Running</SelectItem>
                    <SelectItem value="paused">Paused</SelectItem>
                    <SelectItem value="stopped">Stopped</SelectItem>
                    <SelectItem value="scheduled">Scheduled</SelectItem>
                    <SelectItem value="idle">Idle</SelectItem>
                  </SelectContent>
                </Select>

                <Select value={typeFilter} onValueChange={setTypeFilter}>
                  <SelectTrigger className="w-full @md:w-[120px]" aria-label="Type">
                    <SelectValue placeholder="All Types" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">All Types</SelectItem>
                    <SelectItem value="etl">ETL</SelectItem>
                    <SelectItem value="cdc">CDC</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              {/* When the pipeline was created: one button, presets and a custom range. */}
              <CreatedRangeFilter
                value={createdRange}
                onChange={(next) => {
                  setCreatedRange(next)
                  setCurrentPage(1)
                }}
              />

              {hasActiveFilters && (
                <Button variant="outline" size="sm" onClick={clearFilters}>
                  <Filter className="h-4 w-4 mr-2" />
                  Clear
                </Button>
              )}
            </div>

            {/* Primary action */}
            {canCreate && (
              <div className="flex justify-end">
                <Button onClick={handleCreateNew} className="gap-2">
                  <Sparkles className="h-4 w-4" />
                  Create New
                </Button>
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      {/* Pipelines Table */}
      <Card>
        <CardContent className="p-0">
          {state.loading ? (
            <RowsSkeleton rows={6} label="Loading pipelines…" className="p-4" />
          ) : state.error ? (
            <ErrorPage error={state.error} onRetry={fetchPipelines} />
          ) : state.pipelines.length === 0 && !hasActiveFilters ? (
            <EmptyState
              icon={GitBranch}
              title="No pipelines yet"
              description={
                canCreate
                  ? "Describe what to sync in chat, and rsync.ai builds the pipeline from your sources to your destinations."
                  : "You have view-only access to this workspace."
              }
              action={
                canCreate ? (
                  <Button onClick={handleCreateNew} className="mt-2 gap-2">
                    <Sparkles className="h-4 w-4" />
                    Create with AI
                  </Button>
                ) : undefined
              }
            />
          ) : state.pipelines.length === 0 && hasActiveFilters ? (
            <EmptyState
              icon={SearchX}
              title="No pipelines match your filters"
              description="Try a different search or status, or widen the date range."
              action={
                <Button variant="outline" size="sm" onClick={clearFilters} className="mt-2">
                  Clear filters
                </Button>
              }
            />
          ) : (
            <>
              <PipelinesTable
                pipelines={state.pipelines}
                onMenuOpenChange={setIsAnyMenuOpen}
                onRunPipeline={handleRunPipeline}
                onPausePipeline={handlePausePipeline}
                onResumePipeline={handleResumePipeline}
                onDeletePipeline={handleDeletePipeline}
                onStopPipeline={handleStopPipeline}
                onExportConfig={exportPipelineConfig}
              />

              {/* Pagination */}
              {state.pagination.total_pages > 1 && (
                <div className="flex items-center justify-between border-t p-4">
                  <div className="text-sm text-zinc-600 dark:text-zinc-400">
                    Page {state.pagination.page} of {state.pagination.total_pages} (
                    {state.pagination.total} total)
                  </div>
                  <div className="flex gap-2">
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={!state.pagination.has_prev}
                      onClick={() => setCurrentPage((p) => Math.max(1, p - 1))}
                    >
                      Previous
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={!state.pagination.has_next}
                      onClick={() => setCurrentPage((p) => p + 1)}
                    >
                      Next
                    </Button>
                  </div>
                </div>
              )}

              <AlertDialog
                open={deleteDialogOpen}
                onOpenChange={(open) => {
                  setDeleteDialogOpen(open)
                  if (!open) setDeleteTargetId(null)
                }}
              >
                <AlertDialogContent>
                  <AlertDialogHeader>
                    <AlertDialogTitle>Delete pipeline?</AlertDialogTitle>
                    <AlertDialogDescription>
                      This will cancel any running execution, delete all history, and remove associated schedules. This
                      cannot be undone.
                    </AlertDialogDescription>
                    {/* The row menu offers Delete on a running pipeline too, as the
                        detail menu does (#52) — so name what stops, up front. */}
                    {deleteTargetRunning && (
                      <p className="text-sm font-medium text-amber-700 dark:text-amber-400">
                        {deleteTargetIsCDC
                          ? "This pipeline is streaming now — its live stream stops."
                          : "This pipeline is running now — the run is cancelled, not allowed to finish."}
                      </p>
                    )}
                  </AlertDialogHeader>
                  <AlertDialogFooter>
                    <AlertDialogCancel>Cancel</AlertDialogCancel>
                    <AlertDialogAction
                      className="bg-red-600 hover:bg-red-700 focus:ring-red-600"
                      onClick={() => void confirmDeletePipeline()}
                    >
                      Delete
                    </AlertDialogAction>
                  </AlertDialogFooter>
                </AlertDialogContent>
              </AlertDialog>
            </>
          )}
        </CardContent>
      </Card>

      <UpgradeModal
        payload={planLimitPayload}
        onClose={() => setPlanLimitPayload(null)}
      />

      <PreMigrationAssessmentModal
        open={!!assessmentReport}
        onOpenChange={(open) => {
          if (!open) {
            setAssessmentReport(null)
            setPendingRunId(null)
            setSubmittingProceed(false)
          }
        }}
        report={assessmentReport}
        assessmentTabHref={pendingRunId ? `/pipelines/${pendingRunId}?tab=assessment` : undefined}
        submitting={submittingProceed}
        onProceed={async (nominatedKeys) => {
          if (!pendingRunId) return
          setSubmittingProceed(true)
          const result = await handleRunPipeline(pendingRunId, true, nominatedKeys)
          // Close on success or non-ack errors; keep open on a fresh
          // 422 (the modal report will have been replaced in state).
          if (!result.awaitingAck) {
            setAssessmentReport(null)
            setPendingRunId(null)
          }
          setSubmittingProceed(false)
        }}
      />
    </div>
  )
}

