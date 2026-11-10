import { Loader2 } from "lucide-react"

import type { RunBarKind } from "@/components/explorer/RunDurationChart"
import { Badge } from "@/components/ui/badge"
import type { WorkflowRun } from "@/lib/api/workflows"

/**
 * What a workflow run did, read off what the server stored (workflow_runtime.go,
 * workflow_graph_runs.go): a failed required step fails the run; Stop stops
 * it; a run that delivered a message sent; one whose condition was not true
 * sends nothing; one whose condition was true but sent nothing was held back
 * by the quiet period. A run with no condition that sent nothing succeeded:
 * its steps ran.
 */
export type RunOutcome = "running" | "failed" | "stopped" | "sent" | "succeeded" | "nothing_to_send" | "held_back"

export const RUN_OUTCOME_LABEL: Record<RunOutcome, string> = {
  running: "running",
  failed: "failed",
  stopped: "stopped",
  sent: "sent",
  succeeded: "succeeded",
  nothing_to_send: "nothing to send",
  held_back: "not sent (quiet period)",
}

export function runOutcome(r: Pick<WorkflowRun, "status" | "condition_met" | "deliveries">): RunOutcome {
  if (r.status === "running") return "running"
  if (r.status === "failed") return "failed"
  if (r.status === "stopped") return "stopped"
  if (r.deliveries.some((d) => d.status === "sent")) return "sent"
  if (r.condition_met === false) return "nothing_to_send"
  return r.condition_met === true ? "held_back" : "succeeded"
}

/**
 * The chart's bars for finished runs, in the badges' hues: a good outcome (sent,
 * or every step ran) green, a failure red, held back amber, stopped grey; sky for
 * nothing to send, which no run status has a colour for.
 */
export const WORKFLOW_RUN_KINDS: RunBarKind[] = [
  { status: "sent", label: RUN_OUTCOME_LABEL.sent, color: "succeeded" },
  { status: "failed", label: RUN_OUTCOME_LABEL.failed, color: "failed" },
  { status: "nothing_to_send", label: RUN_OUTCOME_LABEL.nothing_to_send, color: "series-3" },
  { status: "held_back", label: RUN_OUTCOME_LABEL.held_back, color: "waiting" },
  { status: "succeeded", label: RUN_OUTCOME_LABEL.succeeded, color: "succeeded" },
  { status: "stopped", label: RUN_OUTCOME_LABEL.stopped, color: "cancelled" },
]

const BADGE_CLASS: Record<RunOutcome, string> = {
  running: "text-zinc-500 dark:text-zinc-400",
  failed: "border-red-500 text-red-600",
  stopped: "border-zinc-400 text-zinc-600 dark:text-zinc-300",
  succeeded: "border-emerald-500 text-emerald-600",
  sent: "border-emerald-500 text-emerald-600",
  nothing_to_send: "border-sky-400 text-sky-600",
  held_back: "border-amber-500 text-amber-600",
}

export function RunOutcomeBadge({ outcome }: { outcome: RunOutcome }) {
  return (
    <Badge variant="outline" className={BADGE_CLASS[outcome]}>
      {outcome === "running" && <Loader2 className="mr-1 h-3 w-3 animate-spin" aria-hidden />}
      {RUN_OUTCOME_LABEL[outcome]}
    </Badge>
  )
}

/** How long a finished run took, or NaN while it runs or when a time is unreadable. */
export function runDurationMs(r: Pick<WorkflowRun, "started_at" | "finished_at">): number {
  if (!r.finished_at) return Number.NaN
  const ms = new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()
  return ms >= 0 ? ms : Number.NaN
}
