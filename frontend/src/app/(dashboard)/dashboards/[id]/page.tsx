"use client"

import { Suspense } from "react"
import { useParams, useSearchParams } from "next/navigation"
import { Loader2 } from "lucide-react"

import { DashboardPage } from "@/components/dashboards/DashboardPage"

/**
 * One dashboard (dashboards-design.md § 4, § 7.2): its tiles read live, with the
 * viewer's rights. ?change=<id> (the link plan_dashboard_change gives) shows that
 * proposed change to one tile above the grid, to apply (§ 7.6).
 */
// useSearchParams needs a Suspense boundary above it (workflows/[id]/page.tsx says why).
export default function DashboardByIdPage() {
  return (
    <Suspense
      fallback={
        <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" />
          Loading the dashboard…
        </div>
      }
    >
      <DashboardByIdBody />
    </Suspense>
  )
}

function DashboardByIdBody() {
  const { id } = useParams<{ id: string }>()
  const changeId = useSearchParams()?.get("change")
  return <DashboardPage key={id} dashboardId={id} changeId={changeId} />
}
