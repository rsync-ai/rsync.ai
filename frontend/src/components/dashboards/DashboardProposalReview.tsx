"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { LayoutDashboard, Loader2, Save } from "lucide-react"
import { toast } from "sonner"

import { TYPE_LABEL } from "@/components/charts/chart-form"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { ChartError, type ChartVisibility } from "@/lib/api/charts"
import { getDashboardProposal, saveDashboardProposal, type DashboardProposal } from "@/lib/api/dashboards"
import { captureWorkspace } from "@/lib/workspace/active-workspace"

/**
 * A dashboard Ask rsync.ai or an agent drafted (plan_dashboard), shown as a card
 * under the answer (dashboards-design.md § 7.3): its charts and filters, Save with
 * who sees it, and Preview, which draws each chart before anything is saved. A
 * draft saves nothing and expires after 24 hours.
 */
export function DashboardProposalReview({ proposalId }: { proposalId: string }) {
  const router = useRouter()
  const [proposal, setProposal] = useState<DashboardProposal | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const [visibility, setVisibility] = useState<ChartVisibility>("private")
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [savedId, setSavedId] = useState<string | null>(null)

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    getDashboardProposal(proposalId).then(
      (got) => {
        if (!alive || isStale()) return
        setProposal(got.proposal)
        setCanManage(got.can_manage)
        setError(null)
      },
      (err: unknown) => {
        if (!alive || isStale()) return
        setError(
          err instanceof ChartError && err.status === 404
            ? "This dashboard draft was not found in this workspace."
            : err instanceof Error
              ? err.message
              : "Could not load the dashboard draft",
        )
      },
    )
    return () => {
      alive = false
    }
  }, [proposalId, version])

  if (error) {
    return (
      <div
        role="alert"
        className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
      >
        <span>{error}</span>
        <button
          type="button"
          onClick={() => setVersion((v) => v + 1)}
          className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
        >
          Retry
        </button>
      </div>
    )
  }
  if (!proposal) {
    return (
      <div role="status" className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading the dashboard draft…
      </div>
    )
  }

  const saved = proposal.status !== "proposed" ? (proposal.saved_dashboard_id ?? savedId) : savedId
  const save = async () => {
    setSaving(true)
    setSaveError(null)
    try {
      const done = await saveDashboardProposal(proposal.id, visibility)
      toast.success("Dashboard saved")
      setSavedId(done.dashboard_id)
      router.push(`/dashboards/${done.dashboard_id}`)
    } catch (err) {
      if (err instanceof ChartError && err.code === "already_saved") {
        setSavedId(err.dashboardId ?? null)
        setProposal({ ...proposal, status: "saved" })
      } else if (err instanceof ChartError && err.code === "expired") {
        setProposal({ ...proposal, expired: true })
      }
      setSaveError(err instanceof Error ? err.message : "Could not save the dashboard")
    } finally {
      setSaving(false)
    }
  }

  return (
    <section
      aria-label={`Dashboard draft: ${proposal.name}`}
      className="space-y-2 rounded-lg border border-zinc-200 bg-white p-3 text-xs dark:border-zinc-800 dark:bg-zinc-950"
    >
      <header className="flex items-start gap-2">
        <LayoutDashboard className="mt-0.5 h-4 w-4 shrink-0 text-zinc-500 dark:text-zinc-400" aria-hidden />
        <div className="min-w-0">
          <div className="truncate text-sm font-medium text-zinc-900 dark:text-zinc-50">{proposal.name}</div>
          {proposal.description && <p className="text-zinc-600 dark:text-zinc-400">{proposal.description}</p>}
        </div>
      </header>
      <ul className="space-y-0.5 text-zinc-700 dark:text-zinc-300">
        {proposal.charts.map((c, i) => (
          <li key={i} className="truncate">
            {c.name} <span className="text-zinc-500 dark:text-zinc-400">· {TYPE_LABEL[c.type] ?? c.type} of {c.source_name}</span>
          </li>
        ))}
      </ul>
      {proposal.filters.length > 0 && (
        <p className="text-zinc-600 dark:text-zinc-400">
          Filters: {proposal.filters.map((f) => `${f.label} (${f.column})`).join(", ")}
        </p>
      )}

      {proposal.status !== "proposed" ? (
        <p role="status" className="text-zinc-700 dark:text-zinc-300">
          Saved.{" "}
          {saved && (
            <Link href={`/dashboards/${saved}`} className="underline">
              Open the dashboard
            </Link>
          )}
        </p>
      ) : proposal.expired ? (
        <p role="status" className="text-amber-800 dark:text-amber-200">
          This draft expired: an unsaved draft is kept for 24 hours. Ask again to draft it anew.
        </p>
      ) : (
        <>
          <p className="text-zinc-500 dark:text-zinc-400">
            Drafted by {proposal.proposed_by === "agent" ? "an AI agent" : "Ask rsync.ai"}. Nothing is saved until you
            save it; the draft expires <LocalDateTime value={proposal.expires_at} />.
          </p>
          <div className="flex flex-wrap items-center gap-2">
            <Button asChild size="sm" variant="outline">
              <Link href={`/dashboard-proposals/${proposal.id}`}>Preview</Link>
            </Button>
            {canManage ? (
              <>
                <label htmlFor={`proposal-${proposal.id}-visibility`} className="sr-only">
                  Who sees it
                </label>
                <select
                  id={`proposal-${proposal.id}-visibility`}
                  className="h-8 rounded-md border border-zinc-200 bg-white px-2 text-xs dark:border-zinc-800 dark:bg-zinc-950"
                  value={visibility}
                  onChange={(e) => setVisibility(e.target.value as ChartVisibility)}
                >
                  <option value="private">Only me</option>
                  <option value="workspace">The workspace</option>
                </select>
                <Button size="sm" onClick={save} disabled={saving}>
                  {saving ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : <Save className="mr-1.5 h-3.5 w-3.5" />}
                  Save dashboard
                </Button>
              </>
            ) : (
              <span className="text-zinc-500 dark:text-zinc-400">Workspace owners and admins save dashboards.</span>
            )}
          </div>
        </>
      )}
      {saveError && (
        <p role="alert" className="text-red-700 dark:text-red-300">
          {saveError}
        </p>
      )}
    </section>
  )
}
