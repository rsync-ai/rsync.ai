"use client"

import { useEffect, useMemo, useState } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { AlertTriangle, ChartColumn, Gauge, InfinityIcon, RefreshCw } from "lucide-react"
import { PageHeader } from "@/components/layout/PageHeader"
import { Card } from "@/components/ui/card"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Progress } from "@/components/ui/progress"
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@/components/ui/table"
import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import type { HBarChartSettings } from "@/components/charts/chart-settings"
import { ChartCard, WindowToggle } from "@/components/kit/ChartCard"
import { StatCardsSkeleton } from "@/components/kit/PageSkeleton"
import { StatCard } from "@/components/kit/StatCard"
import { StatusPill, type PillTone } from "@/components/kit/StatusPill"
import { derivedStatusConfig } from "@/components/pipeline/PipelinesTable"
import { hbarHeight } from "@/components/pipeline/table-stats-charts"
import { ActivityMetricsProvider, useActivityMetrics } from "@/components/runs/ActivityMetricsProvider"
import { rowsWrittenChart, rowsWrittenRows } from "@/components/runs/run-chart-specs"
import { METRICS_WINDOWS } from "@/lib/api/activity-metrics"
import { authFetch } from "@/lib/api/auth-fetch"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import type { PipelineUsage, WorkspaceUsage } from "@/lib/api/usage"
import { UsagePanelGate } from "@/components/usage/UsagePanelGate"
import { cn, formatAbsoluteTime, formatDate, formatRelativeTime } from "@/lib/utils"

const nf = new Intl.NumberFormat()
const fmt = (n: number) => nf.format(n)

function daysLeft(iso?: string | null): number | null {
  if (!iso) return null
  const ms = new Date(iso).getTime() - Date.now()
  if (Number.isNaN(ms)) return null
  return Math.max(0, Math.ceil(ms / 86_400_000))
}

// A meter with no limit. Drawing it as a full Progress bar read as "you are
// out"; an empty track with "Unlimited" beside the count reads as no ceiling.
function UnlimitedMeter() {
  return <div role="img" aria-label="No limit" className="h-2 w-full rounded-full bg-zinc-100 dark:bg-zinc-800" />
}

function unlimitedLabel(used: string) {
  return (
    <span className="inline-flex items-center gap-1">
      {used} ·{" "}
      <InfinityIcon className="h-3.5 w-3.5" aria-hidden />{" "}
      Unlimited
    </span>
  )
}

// How full a limited meter is: violet below 80%, amber from 80%, red at the
// limit, and the share in words beside the count, so the colour is never the
// only sign.
function meterTone(pct: number): { bar?: string; text: string; words?: string } {
  if (pct >= 100) {
    return { bar: "bg-none bg-red-600 dark:bg-red-500", text: "text-red-700 dark:text-red-300", words: "limit reached" }
  }
  if (pct >= 80) return { bar: "bg-none bg-amber-500 dark:bg-amber-400", text: "text-amber-700 dark:text-amber-300" }
  return { text: "text-zinc-500 dark:text-zinc-400" }
}

function Meter({
  label,
  used,
  limit,
  usedText,
  ofText,
}: {
  label: string
  used: number
  /** null = the plan has no limit. */
  limit: number | null
  /** What is used, on a plan with no limit: "14 used". */
  usedText: string
  /** What is used of the limit: "14 of 28 used". */
  ofText: string
}) {
  if (limit == null) {
    return (
      <div>
        <div className="mb-1.5 flex items-center justify-between gap-3 text-sm">
          <span className="font-medium text-zinc-700 dark:text-zinc-300">{label}</span>
          <span className="text-zinc-500 dark:text-zinc-400">{unlimitedLabel(usedText)}</span>
        </div>
        <UnlimitedMeter />
      </div>
    )
  }
  const pct = limit > 0 ? Math.floor((used / limit) * 100) : used > 0 ? 100 : 0
  const tone = meterTone(pct)
  const share = `${pct}%${tone.words ? `, ${tone.words}` : ""}`
  return (
    <div>
      <div className="mb-1.5 flex items-center justify-between gap-3 text-sm">
        <span className="font-medium text-zinc-700 dark:text-zinc-300">{label}</span>
        <span className={cn("tabular-nums", tone.text)}>
          {ofText} · {share}
        </span>
      </div>
      <Progress
        value={used}
        max={Math.max(limit, 0.001)}
        aria-label={label}
        aria-valuetext={`${ofText}, ${share}`}
        indicatorClassName={tone.bar}
      />
    </div>
  )
}

function planBadgeVariant(plan: string): "default" | "secondary" | "outline" | "success" | "warning" | "info" {
  switch (plan) {
    case "pro":
      return "info"
    case "starter":
      return "default"
    case "trial":
      return "warning"
    case "free":
      return "outline"
    default:
      return "secondary"
  }
}

// The reconciled run-state tokens the /usage API returns are the pipelines list's
// derived_status, so the same pipeline reads the same here and on Pipelines. A raw
// pipeline status (never-run pipelines fall through the backend reconcile) is
// title-cased as sent. (R2)
const RECONCILED_STATUSES = new Set([
  "running",
  "passed",
  "failed",
  "stopped",
  "paused",
  "scheduled",
  "idle",
  "waiting_for_user",
])

function usageStatus(s: string): { tone: PillTone; label: string } {
  if (RECONCILED_STATUSES.has(s)) return derivedStatusConfig(s)
  return { tone: "skip", label: s ? s.charAt(0).toUpperCase() + s.slice(1) : "—" }
}

const TOP_PIPELINES = 10

interface TopPipeline {
  id: string
  label: string
  rows: number
}

/** The pipelines that wrote the most rows, most first; a name two pipelines share gets a number. */
function topPipelines(pipelines: readonly PipelineUsage[]): TopPipeline[] {
  const seen = new Map<string, number>()
  return pipelines
    .filter((p) => p.rows_written > 0)
    .sort((a, b) => b.rows_written - a.rows_written)
    .slice(0, TOP_PIPELINES)
    .map((p) => {
      const name = p.name || p.id
      const n = (seen.get(name) ?? 0) + 1
      seen.set(name, n)
      return { id: p.id, label: n > 1 ? `${name} (${n})` : name, rows: p.rows_written }
    })
}

function topPipelinesChart(rows: readonly TopPipeline[], window: string): HBarChartSettings {
  return {
    type: "hbar",
    stacked: false,
    description: `The pipelines that wrote the most rows, ${window}, most first.`,
    x: { field: "label", label: "Pipeline", categories: rows.map((r) => r.label) },
    y: { field: "rows", format: "integer", axisFormat: "compact", title: "Rows written" },
    series: { values: [{ key: "rows", label: "Rows written", color: "series-1" }] },
  }
}

// The gate wraps the page rather than living inside it, so a deployment with
// the panel off never mounts the content and never fires its /api/v1/usage
// request. Hiding the sidebar entry alone would still leave this URL typeable.
export default function UsagePage() {
  return (
    <UsagePanelGate>
      <UsagePageContent />
    </UsagePanelGate>
  )
}

function UsagePageContent() {
  const [data, setData] = useState<WorkspaceUsage | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [noWorkspace, setNoWorkspace] = useState(false)

  async function load() {
    setLoading(true)
    setError(null)
    setNoWorkspace(false)

    // A switch mid-flight makes these the previous workspace's meters.
    const isStale = captureWorkspace()

    const res = await authFetch("/api/v1/usage", { method: "GET" }).catch(() => null)
    if (isStale()) return
    if (!res) {
      setError("Network error")
      setLoading(false)
      return
    }
    if (res.status === 401) {
      window.location.href = `/logout?next=${encodeURIComponent(`${window.location.pathname}${window.location.search}`)}`
      return
    }
    if (res.status === 400) {
      // No active workspace selected.
      setNoWorkspace(true)
      setLoading(false)
      return
    }
    if (!res.ok) {
      const txt = await res.text().catch(() => "")
      setError(txt || `HTTP ${res.status}`)
      setLoading(false)
      return
    }
    const json = (await res.json().catch(() => null)) as WorkspaceUsage | null
    if (isStale()) return
    setData(json)
    setLoading(false)
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Re-fetch when the active workspace changes (header switcher). This is a client
  // page, so router.refresh() alone won't re-run load() — subscribe explicitly so
  // the meters never show the previous workspace's usage after a switch.
  useEffect(() => {
    return onActiveWorkspaceChange(() => {
      load()
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const used = data?.pipelines_used ?? 0
  const expiryDays = daysLeft(data?.plan_expires_at)
  const gbUsed = data?.transfer?.gb ?? 0
  const queriesUsed = data?.queries_used ?? 0
  // With run-stat retention on, the to-date totals only cover its window.
  const toDate = data?.retention_enabled ? `last ${data.retention_days} days` : "to date"

  return (
    <div className="space-y-6">
      <PageHeader
        heading="Usage"
        description="This workspace's consumption against your plan limits."
      >
        <Button variant="outline" size="sm" onClick={load} disabled={loading}>
          <RefreshCw className={cn("mr-2 h-4 w-4", loading && "motion-safe:animate-spin")} aria-hidden />
          Refresh
        </Button>
      </PageHeader>

      {loading ? (
        <StatCardsSkeleton count={3} label="Loading usage…" />
      ) : noWorkspace ? (
        <Card className="p-6 text-sm text-zinc-500 dark:text-zinc-400">
          No workspace selected. Pick a workspace from the switcher in the header to see its usage.
        </Card>
      ) : error ? (
        <Card className="border border-red-200 bg-red-50 p-4 dark:border-red-900/40 dark:bg-red-950/20">
          <div className="font-semibold text-red-800 dark:text-red-200">Failed to load usage</div>
          <div className="mt-1 text-sm text-red-700 dark:text-red-300">{error}</div>
          <div className="mt-3">
            <Button variant="outline" onClick={load}>
              Retry
            </Button>
          </div>
        </Card>
      ) : !data ? (
        <Card className="p-6 text-sm text-zinc-500 dark:text-zinc-400">No usage data for this workspace.</Card>
      ) : (
        <div className="space-y-6">
          {/* Plan + meters */}
          <Card className="p-5">
            <div className="flex flex-wrap items-start justify-between gap-4">
              <div className="flex items-center gap-2">
                <Gauge className="h-5 w-5 text-violet-600 dark:text-violet-400" aria-hidden />
                <div>
                  <div className="text-sm text-zinc-500 dark:text-zinc-400">Current plan</div>
                  <div className="mt-0.5 flex items-center gap-2">
                    <Badge variant={planBadgeVariant(data.plan)} className="capitalize">
                      {data.plan}
                    </Badge>
                    {data.blocked ? (
                      <span className="text-xs font-medium text-red-600 dark:text-red-400">Expired</span>
                    ) : !data.can_run ? (
                      <span className="text-xs font-medium text-amber-600 dark:text-amber-400">Runs paused</span>
                    ) : null}
                  </div>
                </div>
              </div>
              <div className="text-right text-sm">
                <div className="text-zinc-500 dark:text-zinc-400">Plan renews / expires</div>
                <div className="font-medium text-zinc-900 dark:text-white">
                  {data.plan_expires_at
                    ? `${formatDate(data.plan_expires_at)}${expiryDays != null ? ` · ${expiryDays} day${expiryDays === 1 ? "" : "s"} left` : ""}`
                    : "Never expires"}
                </div>
              </div>
            </div>

            <div className="mt-5 space-y-4">
              <Meter
                label="Pipelines"
                used={used}
                limit={data.pipeline_limit ?? null}
                usedText={`${fmt(used)} used`}
                ofText={`${fmt(used)} of ${fmt(data.pipeline_limit ?? 0)} used`}
              />
              <Meter
                label="Data transfer (this month)"
                used={gbUsed}
                limit={data.transfer?.gb_limit ?? null}
                usedText={`${gbUsed.toFixed(2)} GB used`}
                ofText={`${gbUsed.toFixed(2)} of ${fmt(data.transfer?.gb_limit ?? 0)} GB`}
              />
              <Meter
                label="NL→SQL queries (this month)"
                used={queriesUsed}
                limit={data.queries_limit ?? null}
                usedText={`${fmt(queriesUsed)} used`}
                ofText={`${fmt(queriesUsed)} of ${fmt(data.queries_limit ?? 0)} queries`}
              />
            </div>
          </Card>

          {/* Rows moved, to date */}
          <div className="grid gap-4 @xl:grid-cols-3">
            <StatCard title="Rows read" value={data.transfer.rows_read} hint={`From sources, ${toDate}`} />
            <StatCard title="Rows written" value={data.transfer.rows_written} hint={`To destinations, ${toDate}`} />
            <StatCard title="Records processed" value={data.transfer.records_processed} hint={`By pipelines, ${toDate}`} />
          </div>
          {data.retention_enabled ? (
            <p className="-mt-3 text-xs text-zinc-500 dark:text-zinc-400">
              Totals cover the last {data.retention_days} days (run-stat retention is enabled).
            </p>
          ) : null}

          <ActivityMetricsProvider>
            <div className="grid gap-4 @3xl:grid-cols-2">
              <RowsWrittenCard />
              <TopPipelinesCard pipelines={data.pipelines} window={toDate} />
            </div>
          </ActivityMetricsProvider>

          {/* Per-pipeline breakdown */}
          <Card className="p-0">
            <div className="border-b border-zinc-200 p-4 dark:border-zinc-800">
              <h2 className="font-semibold text-zinc-900 dark:text-white">Per-pipeline transfer</h2>
              <p className="text-sm text-zinc-500 dark:text-zinc-400">Cumulative rows moved by each pipeline in this workspace.</p>
            </div>
            <div className="overflow-x-auto">
              <Table aria-label="Per-pipeline transfer">
                <TableHeader>
                  <TableRow>
                    <TableHead className="text-zinc-600 dark:text-zinc-400">Pipeline</TableHead>
                    <TableHead className="text-zinc-600 dark:text-zinc-400">Status</TableHead>
                    <TableHead className="text-right text-zinc-600 dark:text-zinc-400">Records</TableHead>
                    <TableHead className="text-right text-zinc-600 dark:text-zinc-400">Rows read</TableHead>
                    <TableHead className="text-right text-zinc-600 dark:text-zinc-400">Rows written</TableHead>
                    <TableHead className="text-right text-zinc-600 dark:text-zinc-400">CDC (I/U/D)</TableHead>
                    <TableHead className="text-zinc-600 dark:text-zinc-400">Last activity</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.pipelines.length === 0 ? (
                    <TableRow>
                      <TableCell colSpan={7} className="py-8 text-center text-sm text-zinc-500 dark:text-zinc-400">
                        No pipelines in this workspace yet.
                      </TableCell>
                    </TableRow>
                  ) : (
                    data.pipelines.map((p) => {
                      const status = usageStatus(p.status)
                      return (
                        <TableRow key={p.id}>
                          <TableCell>
                            <Link
                              href={`/pipelines/${p.id}`}
                              className="font-medium text-zinc-900 hover:underline dark:text-white"
                            >
                              {p.name || p.id}
                            </Link>
                          </TableCell>
                          <TableCell>
                            <StatusPill tone={status.tone}>{status.label}</StatusPill>
                          </TableCell>
                          <TableCell className="text-right tabular-nums">{fmt(p.records_processed)}</TableCell>
                          <TableCell className="text-right tabular-nums">{fmt(p.rows_read)}</TableCell>
                          <TableCell className="text-right tabular-nums">{fmt(p.rows_written)}</TableCell>
                          <TableCell className="text-right tabular-nums text-zinc-500 dark:text-zinc-400">
                            {fmt(p.cdc_inserts)}/{fmt(p.cdc_updates)}/{fmt(p.cdc_deletes)}
                          </TableCell>
                          <TableCell className="whitespace-nowrap text-zinc-500 dark:text-zinc-400">
                            <span title={formatAbsoluteTime(p.last_activity)}>{formatRelativeTime(p.last_activity)}</span>
                          </TableCell>
                        </TableRow>
                      )
                    })
                  )}
                </TableBody>
              </Table>
            </div>
          </Card>
        </div>
      )}
    </div>
  )
}

/** Rows written per day by every pipeline run: the series Home and Runs draw. */
function RowsWrittenCard() {
  const { days, setDays, metrics, error, reload } = useActivityMetrics()!
  const settings = useMemo(
    () => (metrics ? rowsWrittenChart("Rows written by all pipeline runs per day", metrics.days) : null),
    [metrics],
  )
  const rows = useMemo(() => (metrics ? rowsWrittenRows(metrics.rows_written) : []), [metrics])

  return (
    <ChartCard
      title="Rows written"
      // Says the time zone once it is known, rather than "UTC" until then.
      description={metrics ? `All pipelines, per ${metrics.time_zone} day` : "All pipelines, per day"}
      action={<WindowToggle value={days} options={METRICS_WINDOWS} onChange={setDays} />}
      loading={!metrics && !error}
      empty={metrics && metrics.rows_written.length === 0 ? `No pipeline wrote rows in the last ${days} days.` : undefined}
      emptyIcon={ChartColumn}
    >
      {error ? (
        <div
          role="alert"
          className="flex h-[200px] flex-col items-center justify-center gap-2 text-center text-sm text-red-700 dark:text-red-300"
        >
          <AlertTriangle className="h-4 w-4" aria-hidden />
          {error}
          <button type="button" className="font-medium underline" onClick={reload}>
            Retry
          </button>
        </div>
      ) : (
        settings && <EChartNoSSR settings={settings} rows={rows} />
      )}
    </ChartCard>
  )
}

/**
 * The 10 pipelines that wrote the most rows; a bar opens its pipeline. Their
 * rows come from the same run stats as the totals, so `window` is the same
 * "to date" or retention window the totals say.
 */
function TopPipelinesCard({ pipelines, window }: { pipelines: readonly PipelineUsage[]; window: string }) {
  const router = useRouter()
  const rows = useMemo(() => topPipelines(pipelines), [pipelines])
  const settings = useMemo(() => topPipelinesChart(rows, window), [rows, window])

  return (
    <ChartCard
      title={`Top ${TOP_PIPELINES} pipelines`}
      description={`By rows written, ${window}. Click a bar to open its pipeline.`}
      empty={rows.length === 0 ? "No pipeline has written rows yet." : undefined}
      emptyIcon={ChartColumn}
    >
      <EChartNoSSR
        settings={settings}
        rows={rows}
        height={hbarHeight(rows.length)}
        onCategoryClick={(i) => rows[i] && router.push(`/pipelines/${rows[i].id}`)}
      />
    </ChartCard>
  )
}
