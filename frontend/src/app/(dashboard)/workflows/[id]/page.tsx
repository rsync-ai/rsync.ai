"use client"

import { Suspense } from "react"
import { useParams, useSearchParams } from "next/navigation"
import { Loader2 } from "lucide-react"

import { WorkflowEditor } from "@/components/workflows/WorkflowEditor"

/**
 * One workflow in the editor: its steps on a canvas, the selected step's
 * settings, Test / Turn on / Pause / Run now / Delete, and its runs. Opened from
 * the Workflows list, from Ask rsync.ai's card, or from the link plan_workflow
 * gives an agent. ?change=<id> (the link plan_workflow_change gives) shows that
 * proposed change above the editor, to apply. ?created=checked|unchecked
 * (WorkflowBuilder's redirect) says the workflow was just built and saved.
 */
// useSearchParams needs a Suspense boundary above it; without one, Radix Tabs render
// every trigger with tabIndex=-1 (pipelines/[id]/page.tsx says the same of its tabs).
export default function WorkflowPage() {
  return (
    <Suspense
      fallback={
        <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading the workflow…
        </div>
      }
    >
      <WorkflowPageBody />
    </Suspense>
  )
}

function WorkflowPageBody() {
  const { id } = useParams<{ id: string }>()
  const params = useSearchParams()
  return <WorkflowEditor workflowId={id} changeId={params?.get("change")} created={params?.get("created")} />
}
