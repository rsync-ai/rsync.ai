"use client"

import { Suspense } from "react"
import { useParams } from "next/navigation"
import { Loader2 } from "lucide-react"

import { ChartBuilder } from "@/components/charts/ChartBuilder"

/**
 * One saved chart: owners and admins change it on the chart page (§ 7.2); a
 * member sees it drawn, with its badges.
 */
// The builder's Radix Tabs need a Suspense boundary above them (workflows/[id]/page.tsx says why).
export default function ChartPage() {
  return (
    <Suspense
      fallback={
        <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading the chart…
        </div>
      }
    >
      <ChartPageBody />
    </Suspense>
  )
}

function ChartPageBody() {
  const { id } = useParams<{ id: string }>()
  return <ChartBuilder key={id} chartId={id} />
}
