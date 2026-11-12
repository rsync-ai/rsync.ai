"use client"

import { Suspense } from "react"
import { useSearchParams } from "next/navigation"
import { Loader2 } from "lucide-react"

import { WorkflowBuilder } from "@/components/workflows/WorkflowBuilder"

/**
 * Build a workflow by hand: a query, its conditions and a message for each,
 * saved as a draft. Opened from the Workflows list's "Build it yourself", or
 * from the Explorer's "Alert on this query" (?from=explorer), which fills in
 * the query.
 */
// useSearchParams needs a Suspense boundary above it (workflows/[id]/page.tsx says why).
export default function NewWorkflowPage() {
  return (
    <Suspense
      fallback={
        <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading…
        </div>
      }
    >
      <NewWorkflowBody />
    </Suspense>
  )
}

function NewWorkflowBody() {
  const params = useSearchParams()
  return <WorkflowBuilder fromExplorer={params?.get("from") === "explorer"} />
}
