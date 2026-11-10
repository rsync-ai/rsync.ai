"use client"

import { useCallback, useEffect, useState } from "react"
import Link from "next/link"
import { Loader2 } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { afterWords, branchTitle, stepName } from "@/components/workflows/workflowWords"
import {
  applyWorkflowChange,
  getWorkflowChange,
  isProcessStep,
  stepsInOrder,
  workflowBranches,
  WorkflowError,
  type Workflow,
  type WorkflowChange,
} from "@/lib/api/workflows"
import { notifyWorkflowChanged } from "@/lib/events/workflowChanged"

/** Refusals after which the change is no longer open: show what it is now. */
const CLOSED_CODES = ["already_applied", "expired", "workflow_changed", "change_changed"]

/**
 * A change Ask rsync.ai or an agent proposed to a saved workflow
 * (plan_workflow_change): its branches as they are and as they would be. Nothing
 * changes until an owner or admin clicks Apply, which saves it as an edit with
 * their session (handlers/workflow_changes.go). Shown as a card under an Ask
 * rsync.ai answer and above the editor when its link (?change=) is opened.
 */
export function WorkflowChangeReview({
  changeId,
  onApplied,
  hideOpenLink = false,
}: {
  changeId: string
  onApplied?: (w: Workflow) => void
  hideOpenLink?: boolean
}) {
  const [data, setData] = useState<{ change: WorkflowChange; can_manage: boolean } | null>(null)
  const [loadError, setLoadError] = useState<Error | null>(null)
  const [applying, setApplying] = useState(false)
  const [applied, setApplied] = useState<{ workflow: Workflow; query_checked: boolean } | null>(null)
  const [applyError, setApplyError] = useState<string | null>(null)

  const asError = (err: unknown) => (err instanceof Error ? err : new Error("Could not load the proposed change"))
  const load = useCallback(async () => {
    setLoadError(null)
    try {
      setData(await getWorkflowChange(changeId))
    } catch (err) {
      setLoadError(asError(err))
    }
  }, [changeId])

  useEffect(() => {
    // A late answer for an earlier changeId is dropped.
    let alive = true
    getWorkflowChange(changeId).then(
      (d) => {
        if (!alive) return
        setData(d)
        setLoadError(null)
      },
      (err: unknown) => {
        if (alive) setLoadError(asError(err))
      },
    )
    return () => {
      alive = false
    }
  }, [changeId])

  const apply = async () => {
    if (!data || applying || applied) return
    setApplying(true)
    setApplyError(null)
    try {
      const res = await applyWorkflowChange(data.change.id)
      setApplied(res)
      onApplied?.(res.workflow)
      notifyWorkflowChanged(res.workflow.id)
    } catch (err) {
      setApplyError(err instanceof Error ? err.message : "Could not apply the change")
      if (err instanceof WorkflowError && err.code && CLOSED_CODES.includes(err.code)) await load()
    } finally {
      setApplying(false)
    }
  }

  if (loadError) {
    const notFound = loadError instanceof WorkflowError && loadError.status === 404
    return (
      <div
        role="alert"
        className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
      >
        <span>{notFound ? "This proposed change was not found. It may be for someone else." : loadError.message}</span>
        {!notFound && (
          <button
            type="button"
            onClick={() => void load()}
            className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
          >
            Retry
          </button>
        )}
      </div>
    )
  }
  if (!data) {
    return (
      <div role="status" className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading the proposed change…
      </div>
    )
  }

  const { change, can_manage: canManage } = data
  const status = applied ? "applied" : change.status
  const open = status === "proposed"
  return (
    <section
      aria-label="Proposed workflow change"
      className="space-y-3 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="font-semibold text-zinc-900 dark:text-white">
            Proposed change to <span className="break-all">{change.workflow_name}</span>
          </h3>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Proposed by {change.proposed_by === "agent" ? "an AI agent" : "Ask rsync.ai"}. Nothing changes until you
            click Apply.
          </p>
        </div>
        <ChangeStatusBadge status={status} />
      </header>

      {change.summary && <p className="text-zinc-800 dark:text-zinc-200">{change.summary}</p>}

      <div className="grid gap-3 @xl:grid-cols-2">
        <BranchList label="Now" workflow={change.current} />
        <BranchList label="After the change" workflow={change.proposed} />
      </div>
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        A change only edits conditions. The name, schedule and query stay as they are.
      </p>

      {applyError && (
        <p
          role="alert"
          className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
        >
          {applyError}
        </p>
      )}

      <footer className="flex flex-wrap items-center gap-3">
        {open && canManage && (
          <>
            <Button size="sm" onClick={() => void apply()} loading={applying}>
              Apply change
            </Button>
            <span className="text-xs text-zinc-500 dark:text-zinc-400">
              Open until <LocalDateTime value={change.expires_at} />.
            </span>
          </>
        )}
        {open && !canManage && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            Only workspace owners and admins can apply a change to a workflow.
          </span>
        )}
        {applied && (
          <span role="status" className="text-xs text-emerald-700 dark:text-emerald-300">
            Applied.{" "}
            {applied.query_checked
              ? "rsync.ai ran the query once to check it; nothing was sent."
              : "The query was still running when the check stopped, so click Test to run it in full."}
          </span>
        )}
        {!applied && status === "applied" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">This change was applied.</span>
        )}
        {status === "expired" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            This change expired 24 hours after it was proposed. Ask again for a new one.
          </span>
        )}
        {status === "stale" && (
          <span className="text-xs text-zinc-500 dark:text-zinc-400">
            The workflow was saved after this change was proposed. Ask again so the change starts from what it is now.
          </span>
        )}
        {!hideOpenLink && (
          <Link
            href={`/workflows/${change.workflow_id}`}
            className="ml-auto text-xs font-medium text-violet-700 hover:underline dark:text-violet-300"
          >
            Open workflow
          </Link>
        )}
      </footer>
    </section>
  )
}

function ChangeStatusBadge({ status }: { status: WorkflowChange["status"] }) {
  if (status === "applied") return <Badge variant="success">Applied</Badge>
  if (status === "expired") return <Badge variant="secondary">Expired</Badge>
  if (status === "stale") return <Badge variant="warning">Out of date</Badge>
  return <Badge variant="info">Waiting for you</Badge>
}

/**
 * A workflow's pipeline, model and wait steps, each with when it runs, then
 * its sends: each condition's, then any that follows no condition, with when
 * each sends, to which channels, and what.
 */
function BranchList({ label, workflow: w }: { label: string; workflow: Workflow }) {
  const branches = workflowBranches(w.definition)
  const conditions = branches.filter((b) => b.case !== undefined).length
  const process = stepsInOrder(w.definition).filter(isProcessStep)
  const upper = (t: string) => t.charAt(0).toUpperCase() + t.slice(1)
  return (
    <div role="group" aria-label={label} className="space-y-2">
      <h4 className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">{label}</h4>
      {process.length > 0 && (
        <ol aria-label="Steps" className="space-y-1 rounded-md bg-zinc-50 p-2 text-xs text-zinc-800 dark:bg-zinc-950 dark:text-zinc-100">
          {process.map((s) => (
            <li key={s.id}>{`${upper(stepName(w, s))} runs ${afterWords(w, s)}.`}</li>
          ))}
        </ol>
      )}
      <ol className="space-y-2">
        {branches.map((b) => {
          const channels = w.channels.filter((c) => c.node_id === b.send)
          return (
            <li key={b.send} className="space-y-1 rounded-md bg-zinc-50 p-2 text-xs dark:bg-zinc-950">
              <p className="text-zinc-800 dark:text-zinc-100">
                {b.case !== undefined && b.condition ? (
                  <>
                    <span className="font-medium">{branchTitle(b.case, conditions, b.condition.type)}</span>:{" "}
                    {w.branch_descriptions?.[b.case] ?? ""}
                  </>
                ) : (
                  <>
                    <span className="font-medium">Message</span>: sent {afterWords(w, b.step)}
                  </>
                )}
              </p>
              <p className="text-zinc-600 dark:text-zinc-300">
                To: {channels.length > 0 ? channels.map((c) => c.name).join(", ") : "no channel"}
              </p>
              {b.message.subject && <p className="font-medium text-zinc-800 dark:text-zinc-100">{b.message.subject}</p>}
              <pre className="whitespace-pre-wrap font-sans text-zinc-700 dark:text-zinc-300">{b.message.body}</pre>
            </li>
          )
        })}
      </ol>
    </div>
  )
}
