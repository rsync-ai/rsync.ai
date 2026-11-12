import {
  Boxes,
  ChartColumn,
  Clock,
  Database,
  GitBranch,
  Hand,
  Hourglass,
  LayoutDashboard,
  Mail,
  MessageSquare,
  Send,
  Split,
  Zap,
  type LucideIcon,
} from "lucide-react"

import type { StepKind } from "@/components/workflows/WorkflowCanvas"
import {
  stepsInOrder,
  type StepType,
  type TriggerType,
  type Workflow,
  type WorkflowChannel,
  type WorkflowStepDef,
} from "@/lib/api/workflows"

// One icon and one tint per kind of step, so the canvas, the Workflows list and the
// summary sentence draw a query, a chart or a Slack message the same way.

/** A step kind's tile and icon colours, as n8n gives each node kind its own. */
export const STEP_KIND_STYLE: Record<StepKind, { tile: string; icon: string }> = {
  trigger: { tile: "bg-orange-50 dark:bg-orange-950/40", icon: "text-orange-600 dark:text-orange-400" },
  query: { tile: "bg-sky-50 dark:bg-sky-950/40", icon: "text-sky-600 dark:text-sky-400" },
  condition: { tile: "bg-emerald-50 dark:bg-emerald-950/40", icon: "text-emerald-600 dark:text-emerald-400" },
  channel: { tile: "bg-violet-50 dark:bg-violet-950/40", icon: "text-violet-600 dark:text-violet-400" },
  run_pipeline: { tile: "bg-indigo-50 dark:bg-indigo-950/40", icon: "text-indigo-600 dark:text-indigo-400" },
  refresh_model: { tile: "bg-teal-50 dark:bg-teal-950/40", icon: "text-teal-600 dark:text-teal-400" },
  wait: { tile: "bg-zinc-100 dark:bg-zinc-800/60", icon: "text-zinc-600 dark:text-zinc-300" },
  chart: { tile: "bg-fuchsia-50 dark:bg-fuchsia-950/40", icon: "text-fuchsia-600 dark:text-fuchsia-400" },
  dashboard: { tile: "bg-fuchsia-50 dark:bg-fuchsia-950/40", icon: "text-fuchsia-600 dark:text-fuchsia-400" },
}

/** What starts a run: a clock, a pipeline landing, or someone clicking Run now. */
export const TRIGGER_ICON: Record<TriggerType, { icon: LucideIcon; label: string }> = {
  schedule: { icon: Clock, label: "Schedule" },
  after_pipeline: { icon: Zap, label: "After a pipeline" },
  manual: { icon: Hand, label: "Run now" },
}

/** Each step type's icon and name. A send is drawn by its channels' kind where it has one. */
export const STEP_TYPE_ICON: Record<StepType, { icon: LucideIcon; label: string }> = {
  query: { icon: Database, label: "Query" },
  condition: { icon: Split, label: "Condition" },
  send: { icon: Send, label: "Send" },
  run_pipeline: { icon: GitBranch, label: "Pipeline run" },
  refresh_model: { icon: Boxes, label: "Model refresh" },
  wait: { icon: Hourglass, label: "Wait" },
  chart: { icon: ChartColumn, label: "Chart" },
  dashboard: { icon: LayoutDashboard, label: "Dashboard" },
}

export const CHANNEL_KIND_ICON: Record<WorkflowChannel["kind"], { icon: LucideIcon; label: string }> = {
  slack: { icon: MessageSquare, label: "Slack message" },
  email: { icon: Mail, label: "Email" },
}

/**
 * A send step's icon: its channels' kind when they are all one kind, the plain send
 * icon when it has none or both.
 */
export function sendIcon(channels: Pick<WorkflowChannel, "kind">[]): { icon: LucideIcon; label: string } {
  const kinds = new Set(channels.map((c) => c.kind))
  if (kinds.size !== 1) return STEP_TYPE_ICON.send
  return CHANNEL_KIND_ICON[[...kinds][0]]
}

/**
 * A workflow's steps in run order, each as its icon, name and tint: a send by its
 * channels' kind. The trigger is not one of them.
 */
export function workflowStepIcons(
  w: Pick<Workflow, "definition" | "channels">,
): { step: WorkflowStepDef; icon: LucideIcon; label: string; kind: StepKind }[] {
  return stepsInOrder(w.definition).map((s) =>
    s.type === "send"
      ? { step: s, ...sendIcon(w.channels.filter((c) => c.node_id === s.id)), kind: "channel" }
      : { step: s, ...STEP_TYPE_ICON[s.type], kind: s.type },
  )
}
