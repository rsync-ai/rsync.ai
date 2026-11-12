"use client"

import { useCallback, useRef, useState } from "react"
import { isStaleHITLAnswer, type DestinationConfig } from "@/lib/api/pipelines"
import { notifyStaleHITLAnswer } from "@/lib/pipeline/staleHitlAnswer"
import { fetchTransformStepSchema, resumeAfterTransformStep } from "@/lib/pipeline/transformStep"
import type { TableMetadata } from "@/components/chat/SuggestionsReviewDialog"

/**
 * The transform step on the pipeline pages: a table-selection pause there goes
 * picker → SuggestionsReviewDialog → resume, as it does in chat (useHITLState).
 * Without it the pages resumed straight from the picker, so a pipeline opened
 * from an Ask rsync.ai link never offered the AI transform suggestions.
 */
export interface PipelineTransformStepInput {
  pipelineId: string
  executionId?: string | null
  // Set on a DAG node-input pause; its answer goes to that node.
  nodeId?: string | null
  // The pipeline's source connection. The pause details do not carry it
  // (pipeline_state.go), so the page reads it from the pipeline.
  sourceConnectionId?: string | null
  // Called after the run is resumed, so the page can refresh its state.
  onResumed?: () => void
}

interface StepTarget {
  executionId?: string
  nodeId?: string
  sourceConnectionId: string
  tables: string[]
  destinationConfig: DestinationConfig | null
}

export interface PipelineTransformStepDialogProps {
  isOpen: true
  pipelineId: string
  sourceConnectionId: string
  selectedTables: string[]
  prefetchedSchema: TableMetadata[]
  onApplyContinue: (saved: { count: number }) => Promise<void>
  onClose: () => Promise<void>
}

export function usePipelineTransformStep(input: PipelineTransformStepInput) {
  const { pipelineId, executionId, nodeId, sourceConnectionId, onResumed } = input
  const [target, setTarget] = useState<StepTarget | null>(null)
  const [schema, setSchema] = useState<TableMetadata[]>([])
  // Only the latest open may set the prefetched schema.
  const openSeq = useRef(0)

  /**
   * Opens the step for the chosen tables. Returns false when there is no step to
   * show (no source connection known, or no tables), so the caller resumes the
   * run itself as before.
   */
  const start = useCallback(
    (tables: string[], destinationConfig?: DestinationConfig | null): boolean => {
      const source = String(sourceConnectionId || "").trim()
      if (!pipelineId || !source || tables.length === 0) return false
      // The step answers the pause that was showing when it opened, even if the
      // page's state moves on while the dialog is up.
      setTarget({
        executionId: executionId || undefined,
        nodeId: String(nodeId || "").trim() || undefined,
        sourceConnectionId: source,
        tables,
        destinationConfig: destinationConfig ?? null,
      })
      setSchema([])
      const seq = ++openSeq.current
      // Best effort: the dialog loads the columns itself when this comes back empty.
      void fetchTransformStepSchema(source, tables).then((s) => {
        if (openSeq.current === seq) setSchema(s)
      })
      return true
    },
    [executionId, nodeId, pipelineId, sourceConnectionId]
  )

  const close = useCallback(() => {
    openSeq.current++
    setTarget(null)
    setSchema([])
  }, [])

  // On failure the error reaches the dialog, which stays open so the user can
  // retry; a stale answer (pause answered elsewhere, or the run ended) closes it.
  const answer = useCallback(
    async (outcome: { saved: number } | { skipped: true }) => {
      if (!target) return
      try {
        await resumeAfterTransformStep(pipelineId, {
          executionId: target.executionId,
          nodeId: target.nodeId,
          tables: target.tables,
          destinationConfig: target.destinationConfig,
          outcome,
        })
      } catch (err) {
        if (!isStaleHITLAnswer(err)) throw err
        notifyStaleHITLAnswer(err)
        close()
        return
      }
      close()
      onResumed?.()
    },
    [close, onResumed, pipelineId, target]
  )

  const dialogProps: PipelineTransformStepDialogProps | null = target
    ? {
        isOpen: true,
        pipelineId,
        sourceConnectionId: target.sourceConnectionId,
        selectedTables: target.tables,
        prefetchedSchema: schema,
        onApplyContinue: (saved) => answer({ saved: saved.count }),
        onClose: () => answer({ skipped: true }),
      }
    : null

  return { start, dialogProps, isOpen: target !== null }
}
