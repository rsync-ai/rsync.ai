"use client"

import { Suspense } from "react"
import { useSearchParams } from "next/navigation"
import { Loader2 } from "lucide-react"

import { ChartBuilder } from "@/components/charts/ChartBuilder"

/**
 * A new chart (dashboards-design.md § 7.2). ?source=<id> picks its source: the
 * Explorer's Create chart passes the saved query it has open.
 */
// useSearchParams needs a Suspense boundary above it (workflows/[id]/page.tsx says why).
export default function NewChartPage() {
  return (
    <Suspense
      fallback={
        <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading…
        </div>
      }
    >
      <NewChartBody />
    </Suspense>
  )
}

function NewChartBody() {
  const source = useSearchParams()?.get("source")
  return <ChartBuilder initialSource={source} />
}
