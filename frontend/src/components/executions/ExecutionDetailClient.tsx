"use client"

import { useState } from "react"
import { useRouter } from "next/navigation"
import { Button } from "@/components/ui/button"
import { StopCircle, RotateCcw, GitBranch, Loader2 } from "lucide-react"
import { cancelExecution } from "@/lib/api/executions"
import {
  AssessmentRequiredError,
  executePipelineWithRunMode,
  type AssessmentReport,
} from "@/lib/api/pipelines"
import { PreMigrationAssessmentModal } from "@/components/pipeline/PreMigrationAssessmentModal"
import { usePipelineReloadSupported } from "@/lib/hooks/usePipelineReloadSupported"
import Link from "next/link"
import { toast } from "sonner"
import { classifyError } from "@/lib/utils/error-handling"

interface Props {
  execution: {
    id: string
    status: string
    pipelineId: string
    // A CDC run past its backfill that is still streaming. The API reports it as
    // running, but there is no run to cancel (the stream belongs to the pipeline)
    // and re-running would start a second stream beside the live one.
    liveStream?: boolean
  }
}

export function ExecutionDetailClient({ execution }: Props) {
  const router = useRouter()
  const [isCancelling, setIsCancelling] = useState(false)
  const [isRerunning, setIsRerunning] = useState(false)
  const [rerunMode, setRerunMode] = useState<"resume" | "reload" | null>(null)
  // No Re-run (Reload) when the destination is an app's API (reverse ETL).
  const reloadSupported = usePipelineReloadSupported(execution.pipelineId || undefined)

  // Pre-migration assessment modal state. The re-run hits the same 422
  // assessment gate as PipelineActions; without this it silently fails.
  const [assessmentReport, setAssessmentReport] = useState<AssessmentReport | null>(null)
  const [pendingRunMode, setPendingRunMode] = useState<"resume" | "reload" | null>(null)
  const [submittingProceed, setSubmittingProceed] = useState(false)

  const handleCancel = async () => {
    if (isCancelling) return
    
    setIsCancelling(true)
    try {
      await cancelExecution(execution.id)
      router.refresh()
    } catch (error) {
      // A cancel that failed used to log to the console and stop. The spinner
      // cleared, the page refreshed, and the run carried on -- which reads
      // exactly like a cancel that worked and has not propagated yet. The one
      // thing the user must know is that the run is STILL RUNNING.
      console.error("Failed to cancel execution:", error)
      toast.error("Could not cancel this execution", {
        description: `${error instanceof Error ? error.message : "The request never reached the server."} The run is still going.`,
      })
    } finally {
      setIsCancelling(false)
    }
  }

  // Re-run, surfacing the pre-migration assessment modal when the server
  // gates on warnings (422). Mirrors PipelineActions.runViaWizard:
  //   1. First attempt with ackWarnings: false (the default).
  //   2. On AssessmentRequiredError, render the modal; its Proceed callback
  //      re-invokes this with ackWarnings: true.
  const handleRerun = async (
    mode: "resume" | "reload" = "resume",
    ackWarnings = false,
    nominatedKeys?: Record<string, string[]>,
  ) => {
    if (isRerunning) return { awaitingAck: false }

    setIsRerunning(true)
    setRerunMode(mode)
    try {
      const result = await executePipelineWithRunMode(execution.pipelineId, mode, {
        ackWarnings,
        nominatedKeys,
      })
      // Navigate to the new execution
      router.push(`/executions/${result.execution_id}`)
      return { awaitingAck: false }
    } catch (error) {
      if (error instanceof AssessmentRequiredError) {
        setAssessmentReport(error.report)
        setPendingRunMode(mode)
        return { awaitingAck: true }
      }
      // A refused re-run (another run is already in progress, a plan limit) used
      // to reach only the console: the button stopped spinning and nothing said why.
      console.error("Failed to re-run pipeline:", error)
      const e = classifyError(error, "pipeline.run")
      toast.error(e.title, { description: e.hint ?? e.message })
      return { awaitingAck: false }
    } finally {
      // If navigation happens, this component will unmount; this is still safe.
      setIsRerunning(false)
      setRerunMode(null)
    }
  }

  const liveStream = execution.liveStream === true
  const isRunning = !liveStream && (execution.status === "running" || execution.status === "pending")
  const isComplete = !liveStream && (execution.status === "success" || execution.status === "failed" || execution.status === "cancelled")

  // The run's actions, in the page header: the pipeline, then what can be done to
  // this run. They used to sit in a card at the bottom with a second Back button.
  return (
    <>
    <div className="flex flex-wrap items-center gap-2">
      {liveStream && (
        <p className="text-sm text-zinc-600 dark:text-zinc-300">
          Streaming changes. Stop or pause the stream from the pipeline page.
        </p>
      )}

      <Button asChild variant="outline" size="sm">
        <Link href={`/pipelines/${execution.pipelineId}`}>
          <GitBranch className="h-4 w-4 mr-2" />
          View pipeline
        </Link>
      </Button>

      {isRunning && (
        <Button variant="destructive" size="sm" onClick={handleCancel} disabled={isCancelling}>
          {isCancelling ? (
            <Loader2 className="h-4 w-4 mr-2 animate-spin" />
          ) : (
            <StopCircle className="h-4 w-4 mr-2" />
          )}
          {isCancelling ? "Cancelling..." : "Cancel run"}
        </Button>
      )}

      {isComplete && reloadSupported && (
        <Button variant="outline" size="sm" onClick={() => handleRerun("reload")} disabled={isRerunning}>
          {isRerunning && rerunMode === "reload" ? (
            <>
              <Loader2 className="h-4 w-4 mr-2 animate-spin" />
              Reloading...
            </>
          ) : (
            "Re-run (Reload)"
          )}
        </Button>
      )}

      {isComplete && (
        <Button size="sm" onClick={() => handleRerun("resume")} disabled={isRerunning}>
          {isRerunning && rerunMode === "resume" ? (
            <Loader2 className="h-4 w-4 mr-2 animate-spin" />
          ) : (
            <RotateCcw className="h-4 w-4 mr-2" />
          )}
          {isRerunning && rerunMode === "resume" ? "Starting..." : "Re-run (Resume)"}
        </Button>
      )}
    </div>

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
      assessmentTabHref={`/pipelines/${execution.pipelineId}?tab=assessment`}
      submitting={submittingProceed}
      onProceed={async (nominatedKeys) => {
        if (!pendingRunMode) return
        setSubmittingProceed(true)
        const result = await handleRerun(pendingRunMode, true, nominatedKeys)
        // Close on success or non-ack errors; keep open on a fresh
        // 422 (the modal report will have been replaced in state).
        if (!result.awaitingAck) {
          setAssessmentReport(null)
          setPendingRunMode(null)
        }
        setSubmittingProceed(false)
      }}
    />
    </>
  )
}

