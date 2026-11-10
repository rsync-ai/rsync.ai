"use client"

import { useEffect, useMemo, useRef, useState } from "react"
import { useRouter } from "next/navigation"
import { PageHeader } from "@/components/layout/PageHeader"
import { AdminNav } from "@/components/admin/AdminNav"
import { Card } from "@/components/ui/card"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Badge } from "@/components/ui/badge"
import { adminRead, type AdminReadFailure } from "@/lib/api/admin"
import { AdminReadFailureState, LoadingState } from "@/components/admin/AdminStates"
import { configForExecution } from "@/lib/execution-status"

type ExecutionsResponse = {
  data: Array<{
    id: string
    pipeline_id: string
    pipeline_name: string
    created_by_email: string
    status: string
    start_time: string
    end_time?: string | null
    error_message?: string | null
  }>
  total: number
  limit: number
  offset: number
}

const ADMIN_EXECUTION_STATUSES = [
  { value: "running", label: "Running" },
  { value: "pending", label: "Pending" },
  { value: "waiting_for_user", label: "Waiting for user" },
  { value: "success", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "cancelled", label: "Cancelled" },
]

export default function AdminExecutionsPage() {
  const router = useRouter()
  const [q, setQ] = useState("")
  const [statusFilter, setStatusFilter] = useState("")
  const [offset, setOffset] = useState(0)
  const [limit] = useState(50)

  const [data, setData] = useState<ExecutionsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [failure, setFailure] = useState<AdminReadFailure | null>(null)
  const latest = useRef(0)

  const queryString = useMemo(() => {
    const params = new URLSearchParams()
    params.set("limit", String(limit))
    params.set("offset", String(offset))
    if (q.trim()) params.set("q", q.trim())
    if (statusFilter.trim()) params.set("status", statusFilter.trim())
    return params.toString()
  }, [limit, offset, q, statusFilter])

  async function load() {
    const request = ++latest.current
    setLoading(true)
    setFailure(null)
    const r = await adminRead<ExecutionsResponse>(`/api/v1/admin/executions?${queryString}`)
    // Typing sends a read per keystroke; an older one answering late must not
    // replace the newer one's rows.
    if (request !== latest.current) return
    if (r.ok) setData(r.data)
    else setFailure(r)
    setLoading(false)
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryString])

  const canPrev = offset > 0
  const canNext = !!data && offset + limit < (data.total || 0)

  return (
    <div className="space-y-5">
      <PageHeader heading="Admin" description="Executions across all users" />
      <AdminNav />

      <Card className="p-4">
        <div className="flex flex-col gap-3 @md:flex-row @md:items-center @md:justify-between">
          <form
            className="flex w-full max-w-2xl flex-col gap-2 @md:flex-row @md:items-center"
            onSubmit={(e) => {
              e.preventDefault()
              setOffset(0)
              load()
            }}
          >
            {/* The long hint was cut off in the box (#53); it stays in the title. */}
            <Input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Search id, pipeline or email…"
              title="Search by execution id, pipeline id, pipeline name, or email"
              aria-label="Search executions"
            />
            {/* A fixed list, not free text: a typed "success" missed every row stored
                as "completed" (#53). The gateway maps each value onto its spellings. */}
            <Select
              value={statusFilter || "all"}
              onValueChange={(v) => {
                setStatusFilter(v === "all" ? "" : v)
                setOffset(0)
              }}
            >
              <SelectTrigger className="@md:w-[180px]" aria-label="Status">
                <SelectValue placeholder="All statuses" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">All statuses</SelectItem>
                {ADMIN_EXECUTION_STATUSES.map((s) => (
                  <SelectItem key={s.value} value={s.value}>
                    {s.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button type="submit" variant="outline">
              Apply
            </Button>
          </form>

          <div className="flex items-center gap-2">
            <Button variant="outline" onClick={load}>
              Refresh
            </Button>
          </div>
        </div>
      </Card>

      {loading ? (
        <LoadingState />
      ) : failure ? (
        <AdminReadFailureState failure={failure} onRetry={load} />
      ) : !data ? (
        <LoadingState label="No data" />
      ) : (
        <Card className="p-4">
          <div className="flex items-center justify-between text-sm text-zinc-600 dark:text-zinc-400">
            <div>
              Showing <span className="font-medium">{data.data.length}</span> of{" "}
              <span className="font-medium">{data.total}</span>
            </div>
            <div className="flex items-center gap-2">
              <Button variant="outline" disabled={!canPrev} onClick={() => setOffset((v) => Math.max(0, v - limit))}>
                Prev
              </Button>
              <Button variant="outline" disabled={!canNext} onClick={() => setOffset((v) => v + limit)}>
                Next
              </Button>
            </div>
          </div>

          <div className="mt-4 space-y-2">
            {data.data.length === 0 ? (
              <div className="text-sm text-zinc-500 dark:text-zinc-400">No executions found</div>
            ) : (
              data.data.map((e) => (
                <div
                  key={e.id}
                  className="rounded-md border border-zinc-200 bg-white p-3 text-sm hover:bg-zinc-50 cursor-pointer dark:border-zinc-800 dark:bg-zinc-900 dark:hover:bg-zinc-900/60"
                  role="button"
                  tabIndex={0}
                  onClick={() => {
                    router.push(`/pipelines/${e.pipeline_id}`)
                  }}
                  onKeyDown={(evt) => {
                    if (evt.key === "Enter" || evt.key === " ") {
                      router.push(`/pipelines/${e.pipeline_id}`)
                    }
                  }}
                >
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <div className="min-w-0">
                      <div className="font-mono text-xs text-zinc-500 dark:text-zinc-400">{e.id}</div>
                      <div className="mt-1 font-medium text-zinc-900 dark:text-white truncate">
                        {e.pipeline_name || e.pipeline_id}
                      </div>
                      {e.created_by_email ? (
                        <div className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">Owner: {e.created_by_email}</div>
                      ) : null}
                      {e.error_message ? (
                        <div className="mt-1 text-xs text-red-700 dark:text-red-300">{e.error_message}</div>
                      ) : null}
                    </div>
                    <div className="flex items-center gap-2">
                      {(() => {
                        const cfg = configForExecution(e.status, e.error_message)
                        const Icon = cfg.icon
                        return (
                          <Badge variant={cfg.variant} className="gap-1">
                            <Icon className={`h-3 w-3 ${cfg.animate ? "animate-spin" : ""}`} />
                            {cfg.label}
                          </Badge>
                        )
                      })()}
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={(evt) => {
                          evt.stopPropagation()
                          router.push(`/pipelines/${e.pipeline_id}`)
                        }}
                      >
                        Open pipeline
                      </Button>
                    </div>
                  </div>
                </div>
              ))
            )}
          </div>
        </Card>
      )}
    </div>
  )
}

