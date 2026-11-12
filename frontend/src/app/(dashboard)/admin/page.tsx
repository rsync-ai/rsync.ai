"use client"

import { useEffect, useState } from "react"
import { PageHeader } from "@/components/layout/PageHeader"
import { AdminNav } from "@/components/admin/AdminNav"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { adminRead, type AdminReadFailure } from "@/lib/api/admin"
import { AdminReadFailureState, LoadingState } from "@/components/admin/AdminStates"

type OverviewResponse = {
  counts: {
    users: number
    pipelines: number
    executions: number
    connections: number
  }
  recent_failed_executions: Array<{
    id: string
    pipeline_id: string
    pipeline_name: string
    created_by_email: string
    status: string
    start_time: string
    end_time?: string | null
    error_message?: string | null
  }>
}

export default function AdminOverviewPage() {
  const [data, setData] = useState<OverviewResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [failure, setFailure] = useState<AdminReadFailure | null>(null)

  async function load() {
    setLoading(true)
    setFailure(null)
    const r = await adminRead<OverviewResponse>("/api/v1/admin/overview")
    if (r.ok) setData(r.data)
    else setFailure(r)
    setLoading(false)
  }

  useEffect(() => {
    load()
  }, [])

  return (
    <div className="space-y-5">
      <PageHeader heading="Admin" description="Operator-wide overview (beta)" />
      <AdminNav />

      {/* These counts come from /admin/overview, which aggregates the WHOLE platform
          — all users and all workspaces. They are deliberately NOT scoped to the
          workspace selected in the header switcher, so say so to avoid confusion. */}
      <div className="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:border-amber-900/50 dark:bg-amber-950/20 dark:text-amber-200">
        These metrics are platform-wide across all users and workspaces — they are not scoped to the
        workspace selected in the header.
      </div>

      {loading ? (
        <LoadingState />
      ) : failure ? (
        <AdminReadFailureState failure={failure} onRetry={load} />
      ) : !data ? (
        <LoadingState label="No data" />
      ) : (
        <div className="space-y-6">
          <div className="grid gap-4 @xl:grid-cols-4">
            <Card className="p-4">
              <div className="text-sm text-zinc-500 dark:text-zinc-400">Users</div>
              <div className="text-2xl font-semibold">{data.counts.users}</div>
            </Card>
            <Card className="p-4">
              <div className="text-sm text-zinc-500 dark:text-zinc-400">Pipelines</div>
              <div className="text-2xl font-semibold">{data.counts.pipelines}</div>
            </Card>
            <Card className="p-4">
              <div className="text-sm text-zinc-500 dark:text-zinc-400">Executions</div>
              <div className="text-2xl font-semibold">{data.counts.executions}</div>
            </Card>
            <Card className="p-4">
              <div className="text-sm text-zinc-500 dark:text-zinc-400">Connections</div>
              <div className="text-2xl font-semibold">{data.counts.connections}</div>
            </Card>
          </div>

          <Card className="p-4">
            <div className="flex items-center justify-between gap-4">
              <div>
                <div className="font-semibold text-zinc-900 dark:text-white">Recent failures (24h)</div>
                <div className="text-sm text-zinc-500 dark:text-zinc-400">Last 20 failed/error executions</div>
              </div>
              <Button variant="outline" onClick={load}>
                Refresh
              </Button>
            </div>

            <div className="mt-4 space-y-2">
              {data.recent_failed_executions.length === 0 ? (
                <div className="text-sm text-zinc-500 dark:text-zinc-400">No recent failures</div>
              ) : (
                data.recent_failed_executions.map((e) => (
                  <div
                    key={e.id}
                    className="rounded-md border border-zinc-200 bg-white p-3 text-sm dark:border-zinc-800 dark:bg-zinc-900"
                  >
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <div className="font-mono text-xs text-zinc-500 dark:text-zinc-400">{e.id}</div>
                      <Badge variant="outline">{e.status}</Badge>
                    </div>
                    <div className="mt-1">
                      <span className="font-medium">{e.pipeline_name || e.pipeline_id}</span>
                      {e.created_by_email ? (
                        <span className="ml-2 text-xs text-zinc-500 dark:text-zinc-400">({e.created_by_email})</span>
                      ) : null}
                    </div>
                    {e.error_message ? (
                      <div className="mt-1 text-xs text-red-700 dark:text-red-300">{e.error_message}</div>
                    ) : null}
                  </div>
                ))
              )}
            </div>
          </Card>
        </div>
      )}
    </div>
  )
}

