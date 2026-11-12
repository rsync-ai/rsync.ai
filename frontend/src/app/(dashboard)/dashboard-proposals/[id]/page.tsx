"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { useParams, useRouter } from "next/navigation"
import { Loader2, Save } from "lucide-react"
import { toast } from "sonner"

import { TYPE_LABEL } from "@/components/charts/chart-form"
import { ChartInfo } from "@/components/charts/ChartInfo"
import { SavedChart } from "@/components/charts/SavedChart"
import { GRID_GAP, ROW_HEIGHT, readingOrder } from "@/components/dashboards/dashboard-layout"
import { PageHeader } from "@/components/layout/PageHeader"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { ChartError, previewChart, type ChartDraw, type ChartVisibility } from "@/lib/api/charts"
import { getDashboardProposal, saveDashboardProposal, type DashboardProposal } from "@/lib/api/dashboards"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/** Charts previewed at once, as a dashboard draws its tiles. */
const PREVIEW_CONCURRENCY = 4

interface Preview {
  draw?: ChartDraw
  error?: string
  loading: boolean
}

/**
 * A dashboard Ask rsync.ai drafted (dashboards-design.md § 7.3): its charts, each
 * drawn from its source as it would be saved, laid out as the dashboard will be.
 * Nothing is saved until an owner or admin saves it here; the server then checks
 * every chart and the dashboard as it checks a person's.
 */
export default function DashboardProposalPage() {
  const { id } = useParams<{ id: string }>()
  const router = useRouter()
  const [proposal, setProposal] = useState<DashboardProposal | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const [previews, setPreviews] = useState<Record<number, Preview>>({})
  const [visibility, setVisibility] = useState<ChartVisibility>("private")
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [savedId, setSavedId] = useState<string | null>(null)

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setProposal(null)
        setPreviews({})
        setVersion((v) => v + 1)
      }),
    [],
  )

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    setError(null)
    getDashboardProposal(id).then(
      (got) => {
        if (!alive || isStale()) return
        setProposal(got.proposal)
        setCanManage(got.can_manage)
      },
      (err: unknown) => {
        if (alive && !isStale()) setError(err instanceof Error ? err.message : "Could not load the dashboard draft")
      },
    )
    return () => {
      alive = false
    }
  }, [id, version])

  const open = proposal !== null && proposal.status === "proposed" && !proposal.expired

  // Each chart, drawn from its source as it would be saved; a few at a time.
  useEffect(() => {
    if (!proposal || !open || !canManage) return
    let alive = true
    const isStale = captureWorkspace()
    const queue = proposal.charts.map((c, i) => ({ c, i }))
    setPreviews(Object.fromEntries(queue.map(({ i }) => [i, { loading: true }])))
    const worker = async () => {
      for (let next = queue.shift(); next; next = queue.shift()) {
        const { c, i } = next
        try {
          const draw = await previewChart(c.source_id, c.settings)
          if (alive && !isStale()) setPreviews((p) => ({ ...p, [i]: { draw, loading: false } }))
        } catch (err) {
          if (alive && !isStale())
            setPreviews((p) => ({
              ...p,
              [i]: { error: err instanceof Error ? err.message : "Could not draw the chart", loading: false },
            }))
        }
      }
    }
    for (let n = 0; n < Math.min(PREVIEW_CONCURRENCY, queue.length); n++) void worker()
    return () => {
      alive = false
    }
  }, [proposal, open, canManage])

  const save = async () => {
    if (!proposal) return
    setSaving(true)
    setSaveError(null)
    try {
      const saved = await saveDashboardProposal(proposal.id, visibility)
      toast.success("Dashboard saved")
      router.push(`/dashboards/${saved.dashboard_id}`)
    } catch (err) {
      if (err instanceof ChartError && err.code === "already_saved") {
        setSavedId(err.dashboardId ?? null)
        setSaveError(err.message)
      } else if (err instanceof ChartError && err.code === "expired") {
        setProposal((p) => (p ? { ...p, expired: true } : p))
        setSaveError(err.message)
      } else {
        setSaveError(err instanceof Error ? err.message : "Could not save the dashboard")
      }
    } finally {
      setSaving(false)
    }
  }

  if (error) {
    return (
      <div className="space-y-4">
        <PageHeader heading="Dashboard draft" />
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {error}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
      </div>
    )
  }
  if (!proposal) {
    return (
      <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-4 w-4 animate-spin" /> Loading the dashboard draft…
      </div>
    )
  }

  const savedDashboard = proposal.saved_dashboard_id ?? savedId
  const tiles = readingOrder(proposal.tiles)
  return (
    <div className="space-y-5">
      <PageHeader
        heading={proposal.name}
        description={proposal.description || "A dashboard drafted by Ask rsync.ai. Nothing is saved until you save it."}
      >
        <Badge variant="outline">Drafted by {proposal.proposed_by === "agent" ? "an agent" : "Ask rsync.ai"}</Badge>
      </PageHeader>

      {proposal.status !== "proposed" ? (
        <p role="status" className="text-sm text-zinc-700 dark:text-zinc-300">
          This draft is saved.{" "}
          {savedDashboard && (
            <Link href={`/dashboards/${savedDashboard}`} className="underline">
              Open the dashboard
            </Link>
          )}
        </p>
      ) : proposal.expired ? (
        <p role="status" className="text-sm text-amber-800 dark:text-amber-200">
          This draft expired: an unsaved draft is kept for 24 hours. Ask rsync.ai to draft it again.
        </p>
      ) : !canManage ? (
        <p role="status" className="text-sm text-zinc-600 dark:text-zinc-400">
          Workspace owners and admins save dashboards.
        </p>
      ) : (
        <section
          aria-label="Save the dashboard"
          className="flex flex-wrap items-end gap-3 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800"
        >
          <div className="space-y-1">
            <label htmlFor="proposal-visibility" className="text-xs font-medium text-zinc-600 dark:text-zinc-400">
              Who sees it
            </label>
            <select
              id="proposal-visibility"
              className="h-9 rounded-md border border-zinc-200 bg-white px-2 text-sm dark:border-zinc-800 dark:bg-zinc-950"
              value={visibility}
              onChange={(e) => setVisibility(e.target.value as ChartVisibility)}
            >
              <option value="private">Only me</option>
              <option value="workspace">The workspace</option>
            </select>
          </div>
          <Button onClick={save} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-4 w-4 animate-spin" /> : <Save className="mr-1.5 h-4 w-4" />}
            Save dashboard and its {proposal.charts.length} {proposal.charts.length === 1 ? "chart" : "charts"}
          </Button>
          <p className="basis-full text-[11px] text-zinc-500 dark:text-zinc-400">
            Saving makes each chart and the dashboard, with the same checks as when you make them yourself. The
            charts get the same &quot;Who sees it&quot; as the dashboard.
          </p>
        </section>
      )}
      {saveError && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {saveError}{" "}
          {savedId && (
            <Link href={`/dashboards/${savedId}`} className="underline">
              Open the dashboard
            </Link>
          )}
        </p>
      )}

      {proposal.filters.length > 0 && (
        <section aria-label="Filters" className="text-sm text-zinc-700 dark:text-zinc-300">
          <h2 className="mb-1 text-sm font-medium">Filters</h2>
          <ul className="list-inside list-disc text-xs">
            {proposal.filters.map((f) => (
              <li key={f.id}>
                {f.label}: {f.kind === "date_range" ? "a date range" : "a list of values"} on {f.column}
              </li>
            ))}
          </ul>
        </section>
      )}

      <div
        className="grid grid-cols-1 @xl:grid-cols-12"
        style={{ gridAutoRows: `${ROW_HEIGHT}px`, gap: GRID_GAP }}
        data-testid="proposal-grid"
      >
        {tiles.map((t, n) => {
          const chart = proposal.charts[t.chart]
          const preview = previews[t.chart]
          if (!chart) return null
          return (
            <section
              key={n}
              aria-label={chart.name}
              className="flex min-w-0 flex-col overflow-hidden rounded-lg border border-zinc-200 bg-white p-3 [grid-row:var(--tile-rows)] @xl:[grid-column:var(--tile-col)] @xl:[grid-row:var(--tile-row)] dark:border-zinc-800 dark:bg-zinc-950"
              style={
                {
                  "--tile-rows": `span ${t.h}`,
                  "--tile-col": `${t.x + 1} / span ${t.w}`,
                  "--tile-row": `${t.y + 1} / span ${t.h}`,
                } as React.CSSProperties
              }
            >
              <header className="mb-2 flex min-w-0 items-start gap-2">
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-medium text-zinc-900 dark:text-zinc-50" title={chart.name}>
                    {chart.name}
                  </div>
                  <div className="truncate text-[11px] text-zinc-500 dark:text-zinc-400">
                    {TYPE_LABEL[chart.type] ?? chart.type} of {chart.source_name}{" "}
                    {chart.source_kind === "model" ? "(model)" : "(saved query)"}
                  </div>
                </div>
                {open && canManage && !preview?.error && <ChartInfo draw={preview?.draw ?? null} type={chart.type} />}
              </header>
              <div className="flex min-h-0 flex-1 flex-col overflow-auto">
                {!open || !canManage ? (
                  <p className="text-xs text-zinc-500 dark:text-zinc-400">Not drawn.</p>
                ) : preview?.error ? (
                  <p role="alert" className="text-sm text-red-700 dark:text-red-300">
                    {preview.error}
                  </p>
                ) : (
                  <SavedChart
                    settings={chart.settings}
                    draw={preview?.draw ?? null}
                    loading={preview?.loading ?? true}
                    fill
                    title={chart.name}
                  />
                )}
              </div>
            </section>
          )
        })}
      </div>
    </div>
  )
}
