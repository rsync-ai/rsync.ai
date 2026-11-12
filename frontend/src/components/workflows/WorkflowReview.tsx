"use client"

import { useCallback } from "react"
import Link from "next/link"
import { Loader2 } from "lucide-react"

import { StatusPill, type PillTone } from "@/components/kit/StatusPill"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { WorkflowActionsBar } from "@/components/workflows/WorkflowActionsBar"
import { WorkflowCanvas, workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowTestResult } from "@/components/workflows/WorkflowTestResult"
import { afterWords, branchTitle, conditionLabel, isOtherwiseIn } from "@/components/workflows/workflowWords"
import { useWorkflow, useWorkflowActions } from "@/components/workflows/useWorkflow"
import { queryStep, statusLabel, workflowBranches, WorkflowError, type Workflow } from "@/lib/api/workflows"
import { notifyWorkflowChanged } from "@/lib/events/workflowChanged"

/** How many test rows the card shows; the editor shows every row the test returned. */
const CARD_ROWS = 5

const STATUS_TONE: Record<Workflow["status"], PillTone> = { active: "ok", paused: "wait", draft: "vio" }

/** A workflow's status in the app's one pill: On green, Paused amber, Draft violet. */
export function WorkflowStatusPill({ workflow: w }: { workflow: Pick<Workflow, "status" | "paused_reason"> }) {
  return (
    <StatusPill tone={STATUS_TONE[w.status]} title={w.status === "paused" ? w.paused_reason : undefined}>
      {statusLabel(w.status)}
    </StatusPill>
  )
}

/**
 * What the status means, in one sentence, as the card and the editor header say
 * it. Both show the status pill beside it, so the line does not say the status
 * again: "On" in the pill and "On." in the line read twice.
 */
export function WorkflowStatusLine({ workflow: w }: { workflow: Workflow }) {
  if (w.status === "draft") {
    return (
      <>
        {w.proposed_by === "none" ? "Built in the editor" : `Drafted by ${w.proposed_by === "agent" ? "an AI agent" : "Ask rsync.ai"}`}{" "}
        and off: it won&apos;t run on its own until you turn it on. To go live, Test it, then Turn on. Run now sends for real, even on a draft.
      </>
    )
  }
  if (w.status === "active") {
    return w.next_run_at ? (
      <>
        Next run <LocalDateTime value={w.next_run_at} />.
      </>
    ) : (
      <>Runs {w.trigger_description}.</>
    )
  }
  const reason = w.paused_reason?.trim()
  if (reason) return <>{/[.!?]$/.test(reason) ? reason : `${reason}.`}</>
  return <>It won&apos;t run on its own until you turn it on.</>
}

export function WorkflowLoadError({ error, onRetry }: { error: Error; onRetry: () => void }) {
  const notFound = error instanceof WorkflowError && error.status === 404
  return (
    <div
      role="alert"
      className="flex items-start justify-between gap-3 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
    >
      <span>{notFound ? "This workflow was not found in this workspace. It may have been deleted." : error.message}</span>
      {!notFound && (
        <button
          type="button"
          onClick={onRetry}
          className="shrink-0 rounded-md border border-red-300 px-2 py-0.5 font-medium hover:bg-red-100 dark:border-red-800 dark:hover:bg-red-900/40"
        >
          Retry
        </button>
      )}
    </div>
  )
}

/**
 * A workflow Ask rsync.ai or an agent drafted (plan_workflow), shown as a card
 * under the answer: its steps as a small canvas, Test, Turn on and Run now. A
 * draft never runs on its own (its query ran once, to check it); Test shows the result and
 * the message to this person only, and Run now runs it for real, sends included
 * (workflows.go RunWorkflowNow takes any status). Open workflow goes to the full editor.
 */
export function WorkflowReview({ workflowId }: { workflowId: string }) {
  const { workflow, canManage, error, reload, replace } = useWorkflow(workflowId)
  // The card is in Ask rsync.ai's panel, beside the Workflows list it changes.
  const onChanged = useCallback(() => notifyWorkflowChanged(workflowId), [workflowId])
  const actions = useWorkflowActions(workflow, { replace, reload, onChanged })

  if (error) return <WorkflowLoadError error={error} onRetry={reload} />
  if (!workflow) {
    return (
      <div role="status" className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading the workflow…
      </div>
    )
  }
  const w = workflow
  const steps = workflowSteps(w)
  const branches = workflowBranches(w.definition)
  const conditions = branches.filter((b) => b.case !== undefined).length
  const several = branches.length > 1
  const sql = queryStep(w.definition)?.sql
  return (
    <section
      aria-label={w.status === "draft" ? "Drafted workflow" : "Workflow"}
      className="space-y-3 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0">
          <h3 className="truncate font-semibold text-zinc-900 dark:text-white">{w.name}</h3>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            <WorkflowStatusLine workflow={w} />
          </p>
        </div>
        <WorkflowStatusPill workflow={w} />
      </header>

      <WorkflowCanvas workflow={w} compact />
      <ol aria-label="Steps" className="sr-only">
        {steps.map((s) => (
          <li key={s.id}>
            {s.title}: {s.subtitle}
          </li>
        ))}
      </ol>

      <details className="text-xs">
        <summary className="cursor-pointer text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200">
          {sql !== undefined ? (several ? "SQL and messages" : "SQL and message") : several ? "Messages" : "Message"}
        </summary>
        <div className="mt-2 space-y-2">
          {sql !== undefined && (
            <pre
              aria-label="SQL"
              className="max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-zinc-50 p-2 font-mono text-[11px] text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200"
            >
              {sql}
            </pre>
          )}
          {branches.map((b) => (
            <div
              key={b.send}
              aria-label={
                b.case !== undefined && conditions > 1
                  ? `${conditionLabel(w.definition, b.case)} message`
                  : several
                    ? `Message step ${b.send}`
                    : "Message"
              }
              className="rounded-md bg-zinc-50 p-2 dark:bg-zinc-950"
            >
              {several && b.case !== undefined && b.condition && (
                <p className="mb-1 text-zinc-500 dark:text-zinc-400">
                  <span className="font-medium">{branchTitle(b.case, conditions, b.condition.type)}</span>:{" "}
                  {isOtherwiseIn(w.definition, b.case)
                    ? "send when no condition above is true"
                    : (w.branch_descriptions?.[b.case] ?? "")}
                </p>
              )}
              {several && b.case === undefined && (
                <p className="mb-1 text-zinc-500 dark:text-zinc-400">Sent {afterWords(w, b.step)}</p>
              )}
              {b.message.subject && <p className="font-medium text-zinc-800 dark:text-zinc-100">{b.message.subject}</p>}
              <pre className="whitespace-pre-wrap font-sans text-zinc-700 dark:text-zinc-300">{b.message.body}</pre>
            </div>
          ))}
        </div>
      </details>

      {canManage ? (
        <div className="flex flex-wrap items-center gap-2">
          <WorkflowActionsBar
            workflow={w}
            busy={actions.busy}
            onTest={actions.onTest}
            onActivate={actions.onActivate}
            onPause={actions.onPause}
            onRun={actions.onRun}
          />
          <Link
            href={`/workflows/${w.id}`}
            className="ml-auto text-xs font-medium text-violet-700 hover:underline dark:text-violet-300"
          >
            Open workflow
          </Link>
        </div>
      ) : (
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          Only workspace owners and admins can test, turn on or run a workflow.{" "}
          <Link href={`/workflows/${w.id}`} className="font-medium text-violet-700 hover:underline dark:text-violet-300">
            Open workflow
          </Link>
        </p>
      )}

      {actions.notice && (
        <p role="status" className="text-xs text-emerald-700 dark:text-emerald-300">
          {actions.notice}{" "}
          <Link href={`/workflows/${w.id}`} className="font-medium underline">
            See its runs
          </Link>
        </p>
      )}
      {actions.error && (
        <p role="alert" className="text-xs text-red-700 dark:text-red-300">
          {actions.error}
        </p>
      )}
      {actions.test && (
        <WorkflowTestResult
          test={actions.test}
          workflow={w}
          maxRows={CARD_ROWS}
          onClose={actions.clearTest}
        />
      )}
    </section>
  )
}
