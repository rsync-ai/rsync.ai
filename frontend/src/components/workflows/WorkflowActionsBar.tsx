"use client"

import { useState } from "react"
import { FlaskConical, Loader2, Pause, Play, Power, Trash2 } from "lucide-react"

import { Button } from "@/components/ui/button"
import type { Workflow } from "@/lib/api/workflows"
import type { WorkflowAction } from "@/components/workflows/useWorkflow"

/**
 * Test, Turn on or Pause, Run now, and (on the editor) Delete. Delete asks
 * twice; the second click is the delete. Each says what it does on hover: Test
 * sends nothing, and Run now sends for real whatever the status
 * (workflows.go TestWorkflow, RunWorkflowNow).
 */
export function WorkflowActionsBar({
  workflow,
  busy,
  onTest,
  onActivate,
  onPause,
  onRun,
  onDelete,
}: {
  workflow: Workflow
  busy: WorkflowAction
  onTest: () => void
  onActivate: () => void
  onPause: () => void
  onRun: () => void
  onDelete?: () => void
}) {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const spin = (kind: WorkflowAction) => busy === kind && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />
  return (
    <div className="flex flex-wrap items-center gap-2">
      <Button
        size="sm"
        variant="outline"
        disabled={!!busy}
        title="Runs the query and shows you the result and the message. Nothing is sent and no step runs."
        onClick={onTest}
      >
        {spin("test") || <FlaskConical className="mr-1 h-3.5 w-3.5" />}
        Test
      </Button>
      {workflow.status === "active" ? (
        <Button
          size="sm"
          variant="outline"
          disabled={!!busy}
          title="Stops it running on its own. Run now still works."
          onClick={onPause}
        >
          {spin("pause") || <Pause className="mr-1 h-3.5 w-3.5" />}
          Pause
        </Button>
      ) : (
        <Button
          size="sm"
          disabled={!!busy}
          title={
            workflow.definition.trigger.type === "manual"
              ? "Turns it on, as you. It has no schedule or pipeline to run after, so it still runs only on Run now."
              : `From now on it runs on its own, ${workflow.trigger_description}, as you, and sends its messages.`
          }
          onClick={onActivate}
        >
          {spin("activate") || <Power className="mr-1 h-3.5 w-3.5" />}
          Turn on
        </Button>
      )}
      <Button
        size="sm"
        variant="outline"
        disabled={!!busy}
        title="Runs every step once now, for real: the message is sent when a condition is true, even on a draft."
        onClick={onRun}
      >
        {spin("run") || <Play className="mr-1 h-3.5 w-3.5" />}
        Run now
      </Button>
      {onDelete && (
        <Button
          size="sm"
          variant={confirmDelete ? "destructive" : "ghost"}
          disabled={!!busy}
          onBlur={() => setConfirmDelete(false)}
          onClick={() => {
            if (confirmDelete) onDelete()
            else setConfirmDelete(true)
          }}
        >
          <Trash2 className="mr-1 h-3.5 w-3.5" /> {confirmDelete ? "Click again to delete" : "Delete"}
        </Button>
      )}
    </div>
  )
}
