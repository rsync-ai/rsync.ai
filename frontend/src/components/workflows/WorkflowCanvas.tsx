"use client"

import { useMemo } from "react"
import {
  Background,
  BackgroundVariant,
  Handle,
  MarkerType,
  Panel,
  Position,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Edge,
  type Node,
  type NodeProps,
} from "@xyflow/react"
import {
  AlertTriangle,
  Check,
  Loader2,
  Maximize,
  Minus,
  Square,
  X,
  ZoomIn,
  ZoomOut,
  type LucideIcon,
} from "lucide-react"

import { FitViewOnResize } from "@/components/ui/fit-view-on-resize"
import { cn } from "@/lib/utils"
import { CHANNEL_KIND_ICON, STEP_KIND_STYLE, STEP_TYPE_ICON, TRIGGER_ICON } from "@/components/workflows/stepIcons"
import { branchTitle, draftRunNote, minutesWords } from "@/components/workflows/workflowWords"
import {
  stepColumns,
  stepRule,
  stepsInOrder,
  type DeliveryOutcome,
  type StepRunStatus,
  type TriggerType,
  type Workflow,
  type WorkflowRun,
  type WorkflowStepDef,
} from "@/lib/api/workflows"

export type StepKind =
  | "trigger"
  | "query"
  | "condition"
  | "channel"
  | "run_pipeline"
  | "refresh_model"
  | "wait"
  | "chart"
  | "dashboard"
/** "untrue": a condition the run checked and found not true; it has its own grey mark, not a green tick. */
export type StepState = "ok" | "untrue" | "warned" | "failed" | "skipped" | "running" | "stopped"

/** One step of a workflow: a node on the canvas and a tab in the step panel. */
export interface WorkflowStep {
  /**
   * "trigger", "query", "branch:<i>" (i is the 0-based condition),
   * "channel:<send step id>:<channel id>" (or ":none" for a send with no
   * channel), or "step:<step id>" for a pipeline, model, wait, chart or dashboard step.
   */
  id: string
  kind: StepKind
  /** The node's label, as n8n puts it under the icon: "Schedule", "Query", "If". */
  title: string
  /** One line under the title: "Every day at 09:00 (UTC)". */
  subtitle: string
  /** The 0-based condition a condition node, or a channel of that condition's send, belongs to (the API's branch). */
  branch?: number
  /** The definition step this node draws: the query, condition, send, process, chart or dashboard step's id. */
  stepId?: string
  channelId?: string
  channelKind?: "slack" | "email"
  state?: StepState
}

function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s
}

/** A step run's status as a node shows it. */
const RUN_STATE: Record<StepRunStatus, StepState> = {
  running: "running",
  waiting: "running",
  succeeded: "ok",
  warned: "warned",
  failed: "failed",
  skipped: "skipped",
  stopped: "stopped",
}

/** A pipeline, model, wait, chart or dashboard step's node. */
function processNode(w: Workflow, s: WorkflowStepDef): WorkflowStep {
  const ref = w.step_refs?.[s.id]
  const node = { id: `step:${s.id}`, stepId: s.id }
  if (s.type === "run_pipeline") {
    const r = s.retries ?? 0
    const retries = r > 0 ? `, ${r} ${r === 1 ? "retry" : "retries"}` : ""
    return { ...node, kind: "run_pipeline", title: ref?.name ?? "Deleted pipeline", subtitle: `Pipeline run${retries}` }
  }
  if (s.type === "refresh_model") {
    return { ...node, kind: "refresh_model", title: ref?.name ?? "Deleted model", subtitle: "Model refresh" }
  }
  if (s.type === "chart") {
    return { ...node, kind: "chart", title: ref?.name ?? "Deleted chart", subtitle: "Chart in the message" }
  }
  if (s.type === "dashboard") {
    return { ...node, kind: "dashboard", title: ref?.name ?? "Deleted dashboard", subtitle: "Dashboard in the message" }
  }
  return { ...node, kind: "wait", title: "Wait", subtitle: capitalize(minutesWords(s.minutes ?? 0)) }
}

/** A send step's channel nodes; one "No channel" node when it has none. */
function channelNodes(w: Workflow, send: WorkflowStepDef): WorkflowStep[] {
  const branch = send.case
  const channels = w.channels.filter((c) => c.node_id === send.id)
  if (channels.length === 0) {
    return [
      {
        id: `channel:${send.id}:none`,
        kind: "channel",
        title: "No channel",
        subtitle: "Sends nothing",
        branch,
        stepId: send.id,
        channelKind: "email",
      },
    ]
  }
  return channels.map((ch) => ({
    id: `channel:${send.id}:${ch.id}`,
    kind: "channel",
    title: ch.name,
    subtitle: ch.kind === "slack" ? "Slack" : "Email",
    branch,
    stepId: send.id,
    channelId: ch.id,
    channelKind: ch.kind,
  }))
}

/**
 * A workflow's steps, left to right as the canvas draws them: its trigger,
 * then each step after the steps it runs after. A condition is one node per
 * condition, each followed by the delivery channels of its send; any other
 * send is a node per channel. The last run marks how far it got: each step's
 * own outcome when the run recorded its steps, else (a run from before steps)
 * every condition up to the one that sent was checked, and only that
 * condition's channels were sent to.
 */
export function workflowSteps(w: Workflow): WorkflowStep[] {
  const d = w.definition
  const trigger = d.trigger.type
  const steps: WorkflowStep[] = [
    {
      id: "trigger",
      kind: "trigger",
      title: trigger === "schedule" ? "Schedule" : trigger === "after_pipeline" ? "After pipeline" : "Run now",
      subtitle: capitalize(w.trigger_description),
    },
  ]
  const descriptions = w.branch_descriptions ?? []
  for (const s of stepsInOrder(d)) {
    switch (s.type) {
      case "query":
        steps.push({
          id: "query",
          kind: "query",
          title: "Query",
          subtitle: w.connection ? w.connection.name : s.connection_id ? "Connection deleted" : "Pick a connection",
          stepId: s.id,
        })
        break
      case "condition": {
        const conds = s.conditions ?? []
        conds.forEach((c, i) => {
          steps.push({
            id: `branch:${i}`,
            kind: "condition",
            title: branchTitle(i, conds.length, c.type),
            subtitle: capitalize(descriptions[i] ?? ""),
            branch: i,
            stepId: s.id,
          })
          const send = d.steps.find((x) => x.type === "send" && x.case === i)
          if (send) steps.push(...channelNodes(w, send))
        })
        break
      }
      case "send":
        // A condition's send is drawn with its condition.
        if (s.case === undefined) steps.push(...channelNodes(w, s))
        break
      default:
        steps.push(processNode(w, s))
    }
  }
  markLastRun(w, steps)
  return steps
}

/**
 * A channel node's delivery in a run. A run with steps names the send step of each
 * delivery; a run from before steps names only the channel, so the delivery belongs
 * to the one condition that sent, not to every node with that channel.
 */
export function lastRunDelivery(run: WorkflowRun, step: WorkflowStep): DeliveryOutcome | undefined {
  if (step.kind !== "channel") return undefined
  if (run.steps) return run.deliveries.find((x) => x.step_id === step.stepId && x.channel_id === step.channelId)
  const sent = run.condition_met ? (run.branch ?? 0) : -1
  return step.branch === sent ? run.deliveries.find((x) => x.channel_id === step.channelId) : undefined
}

/**
 * A checked condition's state, given the one that was true (-1 for none): the true one
 * is "ok", the ones above it were checked and are "untrue", the ones below it were not
 * checked. A green tick on a condition that was not true read as "condition met".
 */
function conditionState(branch: number, taken: number): StepState {
  if (branch === taken) return "ok"
  return taken < 0 || branch < taken ? "untrue" : "skipped"
}

/** Marks each node with how the workflow's last run went there. */
function markLastRun(w: Workflow, steps: WorkflowStep[]) {
  const run = w.last_run
  if (!run) return
  if (run.steps) {
    const byStep = new Map(run.steps.map((r) => [r.step_id, r]))
    steps[0].state = "ok"
    for (const s of steps.slice(1)) {
      const r = s.stepId ? byStep.get(s.stepId) : undefined
      if (!r) continue
      const state = RUN_STATE[r.status]
      if (s.kind === "condition") {
        const taken = r.case ?? -1
        s.state = r.status !== "succeeded" ? state : conditionState(s.branch ?? 0, taken)
        continue
      }
      if (s.kind === "channel") {
        const d = lastRunDelivery(run, s)
        if (d) s.state = d.status === "sent" ? "ok" : d.status === "failed" ? "failed" : "skipped"
        else if (r.status !== "succeeded") s.state = state
        continue
      }
      s.state = state
    }
    return
  }
  // A run from before steps: a query, then its conditions, then one condition's send.
  if (run.status === "running") return
  const queried = run.row_count !== undefined && run.row_count !== null
  steps[0].state = "ok"
  const query = steps.find((s) => s.kind === "query")
  if (query) query.state = queried ? "ok" : "failed"
  if (!queried) return
  // The condition that sent; -1 when none was true, so every one was checked.
  const sent = run.condition_met ? (run.branch ?? 0) : -1
  for (const s of steps) {
    if (s.branch === undefined) continue
    if (s.kind === "condition") {
      s.state = run.condition_met === undefined ? "failed" : conditionState(s.branch, sent)
      continue
    }
    const d = lastRunDelivery(run, s)
    s.state = !d ? "skipped" : d.status === "sent" ? "ok" : "failed"
  }
}

/** The step's icon, as n8n gives each node kind its own. */
function StepIcon({ step, triggerType, className }: { step: WorkflowStep; triggerType: string; className: string }) {
  const { icon: Icon } =
    step.kind === "trigger"
      ? (TRIGGER_ICON[triggerType as TriggerType] ?? TRIGGER_ICON.manual)
      : step.kind === "channel"
        ? CHANNEL_KIND_ICON[step.channelKind ?? "email"]
        : STEP_TYPE_ICON[step.kind]
  return <Icon className={className} aria-hidden />
}

/** The mark on a node's corner for how the last run went there. */
const STATE_BADGE: Partial<Record<StepState, { className: string; icon: LucideIcon; label: string }>> = {
  ok: { className: "bg-emerald-700", icon: Check, label: "passed" },
  untrue: { className: "bg-zinc-500", icon: Minus, label: "not true" },
  failed: { className: "bg-red-600", icon: X, label: "failed" },
  warned: { className: "bg-amber-600", icon: AlertTriangle, label: "warned" },
  running: { className: "bg-sky-600", icon: Loader2, label: "running" },
  stopped: { className: "bg-zinc-500", icon: Square, label: "stopped" },
}
const STATE_BORDER: Partial<Record<StepState, string>> = {
  ok: "border-emerald-400 dark:border-emerald-700",
  failed: "border-red-400 dark:border-red-700",
  warned: "border-amber-400 dark:border-amber-700",
  running: "border-sky-400 dark:border-sky-700",
}

/** Which of a node's handles a line joins: in and out on its sides, prev and else (both on its left) between conditions. */
type Handles = { in: boolean; out: boolean; prev: boolean; else: boolean }

type StepNodeData = {
  step: WorkflowStep
  triggerType: string
  selected: boolean
  compact: boolean
  /** Set when the workflow is a draft: what its last run did, which the mark's label says. Its colours are the run's own. */
  draftNote?: string
  handles: Handles
}
export type StepNode = Node<StepNodeData, "step">

const handleClass = "!h-2 !w-2 !border-0 !bg-zinc-400"
/** A node's width, its label's (w-[112px] / w-[180px] below), and its icon tile's (h-12 w-12 / h-20 w-20). */
const NODE_WIDTH = { compact: 112, full: 180 }
const TILE = { compact: 48, full: 80 }

/** A node as n8n draws one: an icon tile, its label and one line under it. */
function StepNodeView({ data }: NodeProps<StepNode>) {
  const { step, triggerType, selected, compact, draftNote, handles } = data
  const style = STEP_KIND_STYLE[step.kind]
  const badge = step.state ? STATE_BADGE[step.state] : undefined
  const BadgeIcon = badge?.icon
  const border = step.state ? STATE_BORDER[step.state] : undefined
  return (
    <div className={cn("flex flex-col items-center text-center", compact ? "w-[112px]" : "w-[180px]")}>
      <div
        className={cn(
          "relative flex items-center justify-center rounded-xl border-2 shadow-sm transition-colors",
          compact ? "h-12 w-12" : "h-20 w-20",
          style.tile,
          step.kind === "trigger" && (compact ? "rounded-l-[28px]" : "rounded-l-[40px]"),
          selected
            ? "border-violet-500 ring-2 ring-violet-200 dark:ring-violet-900"
            : (border ?? "border-zinc-300 dark:border-zinc-600"),
        )}
      >
        {handles.in && <Handle id="in" type="target" position={Position.Left} className={handleClass} />}
        {/* Between conditions, on the left: a line out of the bottom ran down through the node's own label. */}
        {handles.prev && (
          <Handle id="prev" type="target" position={Position.Left} style={{ top: "30%" }} className={handleClass} />
        )}
        {handles.else && (
          <Handle id="else" type="source" position={Position.Left} style={{ top: "70%" }} className={handleClass} />
        )}
        <StepIcon step={step} triggerType={triggerType} className={cn(compact ? "h-5 w-5" : "h-9 w-9", style.icon)} />
        {badge && BadgeIcon && (
          <span
            role="img"
            aria-label={draftNote ? `Last run, as a draft: ${badge.label}` : `Last run: ${badge.label}`}
            title={draftNote ? `Last run: ${badge.label}. ${draftNote}` : undefined}
            className={cn(
              "absolute -right-2 -top-2 flex items-center justify-center rounded-full text-white",
              compact ? "h-5 w-5" : "h-6 w-6",
              badge.className,
            )}
          >
            <BadgeIcon
              className={cn(
                step.state === "stopped" ? "h-2.5 w-2.5" : compact ? "h-3 w-3" : "h-3.5 w-3.5",
                step.state === "running" && "animate-spin",
              )}
              aria-hidden
            />
          </span>
        )}
        {handles.out && <Handle id="out" type="source" position={Position.Right} className={handleClass} />}
      </div>
      <div className={cn("mt-2 font-medium text-zinc-900 dark:text-zinc-100", compact ? "text-[11px]" : "text-sm")}>
        {step.title}
      </div>
      <div
        className={cn(
          "line-clamp-2 leading-snug text-zinc-500 dark:text-zinc-400",
          compact ? "text-[10px]" : "text-xs",
        )}
      >
        {step.subtitle}
      </div>
    </div>
  )
}

const nodeTypes = { step: StepNodeView }

/**
 * How the canvas fits the whole workflow: a short one fills the box, and none is
 * zoomed in past a readable size.
 */
const FIT = { padding: 0.1, maxZoom: 1.5 }
/**
 * The smallest the canvas opens a workflow at, its labels still readable. A
 * longer one starts at its trigger and is dragged for the rest: fitted whole
 * into the editor beside the step settings, a five-step workflow was drawn at
 * half size, its labels too small to read and the box empty below it.
 */
const READABLE_ZOOM = 0.7
/** The compact canvas in a chat card. */
const FIT_COMPACT = { padding: 0.15, maxZoom: 1 }

const zoomButton =
  "flex h-7 w-7 items-center justify-center text-zinc-600 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-300 dark:hover:bg-zinc-800 dark:hover:text-white"

/**
 * Zoom in, zoom out and fit, drawn as magnifiers rather than + and −: on a
 * canvas a plus reads as "add a step", and steps are only added by asking
 * rsync.ai. Rendered inside a ReactFlowProvider. Fit frames the steps with the
 * canvas's own margin, not at React Flow's default (twice the size), and frames
 * all of them: on a long workflow, smaller than the readable size it opens at.
 */
export function CanvasZoomControls() {
  const { zoomIn, zoomOut, fitView } = useReactFlow()
  return (
    <Panel
      position="bottom-left"
      className="flex flex-col overflow-hidden rounded-md border border-zinc-200 bg-white shadow-sm dark:border-zinc-700 dark:bg-zinc-900"
    >
      <button type="button" aria-label="Zoom in" title="Zoom in" className={zoomButton} onClick={() => zoomIn()}>
        <ZoomIn className="h-4 w-4" aria-hidden />
      </button>
      <button type="button" aria-label="Zoom out" title="Zoom out" className={zoomButton} onClick={() => zoomOut()}>
        <ZoomOut className="h-4 w-4" aria-hidden />
      </button>
      <button type="button" aria-label="Fit to view" title="Fit to view" className={zoomButton} onClick={() => void fitView(FIT)}>
        <Maximize className="h-4 w-4" aria-hidden />
      </button>
    </Panel>
  )
}

/** A line's look from the rule of the step it leads to: red dashes for "when one fails", grey for "even if one failed". */
const RULE_STYLE: Record<string, Edge["style"]> = {
  any_failed: { stroke: "#ef4444", strokeDasharray: "5 4" },
  all_done: { stroke: "#a1a1aa", strokeDasharray: "5 4" },
}

/**
 * The canvas's nodes and the lines between them, left to right: the trigger,
 * then each step a column right of the furthest step it runs after. A
 * condition's conditions sit one under another, each joined to the one below
 * it and to its send's channels on its right; a step with several nodes (a
 * send's channels) is a stack of them. A line into a step that runs when one
 * before it fails is dashed red, and into one that runs even if a step before it failed,
 * dashed grey. The lines carry no words; each node's settings say what it does.
 */
export function workflowGraph(
  workflow: Workflow,
  { compact, selectedId, interactive }: { compact: boolean; selectedId?: string | null; interactive: boolean },
): { nodes: StepNode[]; edges: Edge[]; shape: string } {
  const d = workflow.definition
  const triggerType = d.trigger.type
  const steps = workflowSteps(workflow)
  const gap = compact ? 130 : 210
  const rowGap = compact ? 110 : 170
  const ordered = stepsInOrder(d)
  // A condition's send has the condition alone before it, so it is the column right of it.
  const col = stepColumns(d)
  const nodesOf = (stepId: string) => steps.filter((s) => s.stepId === stepId && s.kind !== "condition")
  const conditionNodes = steps.filter((s) => s.kind === "condition")

  // Each column's next free row, and the row each step's line leaves from.
  const free = new Map<number, number>()
  const freeAt = (c: number) => free.get(c) ?? -Infinity
  const anchor = new Map<string, number>()
  const position = new Map<string, { x: number; y: number }>([["trigger", { x: 0, y: 0 }]])
  const rowOf = (s: WorkflowStepDef) => {
    const first = s.after?.find((p) => anchor.has(p))
    return first ? (anchor.get(first) ?? 0) : 0
  }
  const stack = (ids: string[], c: number, want: number): number => {
    const top = Math.max(want - ((ids.length - 1) / 2) * rowGap, freeAt(c))
    ids.forEach((id, i) => position.set(id, { x: c * gap, y: top + i * rowGap }))
    free.set(c, top + ids.length * rowGap)
    return top + ((ids.length - 1) / 2) * rowGap
  }
  for (const s of ordered) {
    const c = col.get(s.id) ?? 1
    if (s.type === "send" && s.case !== undefined && anchor.has(s.id)) continue
    if (s.type === "condition") {
      // A block of rows per condition, one per channel of its send: the
      // condition at the block's middle, its channels down the block.
      const blocks = (s.conditions ?? []).map((_, k) => {
        const send = d.steps.find((x) => x.type === "send" && x.case === k)
        return { k, send, chans: send ? nodesOf(send.id) : [] }
      })
      const rows0 = Math.max(1, blocks[0]?.chans.length ?? 1)
      let top = Math.max(rowOf(s) - ((rows0 - 1) / 2) * rowGap, freeAt(c), freeAt(c + 1))
      for (const b of blocks) {
        const rows = Math.max(1, b.chans.length)
        const y = top + ((rows - 1) / 2) * rowGap
        position.set(`branch:${b.k}`, { x: c * gap, y })
        if (b.k === 0) anchor.set(s.id, y)
        b.chans.forEach((ch, ci) => position.set(ch.id, { x: (c + 1) * gap, y: top + ci * rowGap }))
        if (b.send) anchor.set(b.send.id, y)
        top += rows * rowGap
      }
      free.set(c, top)
      free.set(c + 1, top)
      continue
    }
    const ids = s.type === "query" ? ["query"] : s.type === "send" ? nodesOf(s.id).map((n) => n.id) : [`step:${s.id}`]
    anchor.set(s.id, stack(ids, c, rowOf(s)))
  }

  // Where a line into a step lands and where a line out of it leaves.
  const entries = (s: WorkflowStepDef): string[] => {
    if (s.type === "query") return ["query"]
    if (s.type === "condition") return conditionNodes.length ? ["branch:0"] : []
    if (s.type === "send") return nodesOf(s.id).map((n) => n.id)
    return [`step:${s.id}`]
  }
  const exits = (s: WorkflowStepDef, child: WorkflowStepDef): string[] => {
    if (s.type === "condition") {
      const k = child.type === "send" && child.case !== undefined ? child.case : 0
      return conditionNodes.some((n) => n.branch === k) ? [`branch:${k}`] : []
    }
    return entries(s)
  }
  const edges: Edge[] = []
  const edge = (
    source: string,
    target: string,
    ruleStyle?: Edge["style"],
    handles = { from: "out", to: "in" },
    offset?: number,
  ) =>
    edges.push({
      id: `${source}->${target}`,
      source,
      target,
      sourceHandle: handles.from,
      targetHandle: handles.to,
      type: "smoothstep",
      markerEnd: { type: MarkerType.ArrowClosed, width: 14, height: 14 },
      style: { strokeWidth: 1.5, ...ruleStyle },
      ...(offset !== undefined && { pathOptions: { offset } }),
    })
  const byId = new Map(d.steps.map((s) => [s.id, s]))
  for (const s of ordered) {
    const style = RULE_STYLE[stepRule(s)]
    const parents = (s.after ?? []).map((p) => byId.get(p)).filter((p): p is WorkflowStepDef => !!p)
    if (parents.length === 0) {
      for (const to of entries(s)) edge("trigger", to, style)
      continue
    }
    for (const p of parents) for (const from of exits(p, s)) for (const to of entries(s)) edge(from, to, style)
  }
  // From one condition to the next, out past the edge of its label on the left
  // and down: a line straight down ran through the condition's own label.
  const elseOffset = (compact ? NODE_WIDTH.compact - TILE.compact : NODE_WIDTH.full - TILE.full) / 2 + 12
  for (let k = 1; k < conditionNodes.length; k++) {
    edge(`branch:${k - 1}`, `branch:${k}`, undefined, { from: "else", to: "prev" }, elseOffset)
  }

  const handles = new Map<string, Handles>()
  const flags = (id: string) => {
    let h = handles.get(id)
    if (!h) handles.set(id, (h = { in: false, out: false, prev: false, else: false }))
    return h
  }
  for (const e of edges) {
    flags(e.source)[e.sourceHandle === "else" ? "else" : "out"] = true
    flags(e.target)[e.targetHandle === "prev" ? "prev" : "in"] = true
  }

  const nodes: StepNode[] = steps.map((step) => ({
    id: step.id,
    type: "step",
    position: position.get(step.id) ?? { x: 0, y: 0 },
    data: {
      step,
      triggerType,
      selected: step.id === selectedId,
      compact,
      draftNote: workflow.status === "draft" ? draftRunNote(workflow) : undefined,
      handles: flags(step.id),
    },
    draggable: false,
    connectable: false,
    selectable: interactive,
    focusable: interactive,
    ariaLabel: `${step.title}: ${step.subtitle}`,
  }))
  // The steps and where they sit, not which is selected: the canvas fits
  // again when these change, and a click on a step leaves the view alone.
  const shape = nodes.map((n) => `${n.id}@${n.position.x},${n.position.y}`).join("|")
  return { nodes, edges, shape }
}

/**
 * The workflow drawn as n8n draws one (workflowGraph), on a dotted canvas with
 * zoom controls. The canvas itself is not edited: a step's settings are
 * changed in the panel beside it (WorkflowEditor's Edit), and a step or a
 * condition is added or removed only by asking rsync.ai. Clicking a node (or Enter on a
 * focused one) selects it; the editor shows that step's settings beside the
 * canvas.
 */
export function WorkflowCanvas({
  workflow,
  compact = false,
  selectedId,
  onSelect,
  className,
}: {
  workflow: Workflow
  compact?: boolean
  selectedId?: string | null
  onSelect?: (stepId: string) => void
  className?: string
}) {
  const fit = compact ? FIT_COMPACT : FIT
  const interactive = !!onSelect
  const { nodes, edges, shape } = useMemo(
    () => workflowGraph(workflow, { compact, selectedId, interactive }),
    [workflow, compact, selectedId, interactive],
  )

  return (
    <div
      role="group"
      aria-label="Workflow canvas"
      className={cn(
        "relative w-full overflow-hidden rounded-lg border border-zinc-200 bg-zinc-50/60 dark:border-zinc-800 dark:bg-zinc-950/40",
        compact ? "h-[190px]" : "h-full min-h-[420px]",
        className,
      )}
    >
      <ReactFlowProvider>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          nodeTypes={nodeTypes}
          onNodeClick={onSelect ? (_, node) => onSelect(node.id) : undefined}
          onSelectionChange={onSelect ? ({ nodes }) => nodes[0] && onSelect(nodes[0].id) : undefined}
          fitView
          fitViewOptions={fit}
          minZoom={0.3}
          maxZoom={2}
          nodesDraggable={false}
          nodesConnectable={false}
          elementsSelectable={!!onSelect}
          nodesFocusable={!!onSelect}
          edgesFocusable={false}
          panOnDrag={!compact}
          zoomOnScroll={!compact}
          zoomOnPinch={!compact}
          zoomOnDoubleClick={false}
          preventScrolling={!compact}
          proOptions={{ hideAttribution: true }}
        >
          <Background variant={BackgroundVariant.Dots} gap={18} size={1} />
          {/* fitView above fits once; this fits again when the step settings, the
              window or the docked Ask rsync.ai panel make the canvas narrower. */}
          {/* The chat card's canvas cannot be dragged, so it always shows the whole workflow. */}
          <FitViewOnResize
            padding={fit.padding}
            maxZoom={fit.maxZoom}
            minZoom={compact ? undefined : READABLE_ZOOM}
            shape={shape}
          />
          {!compact && <CanvasZoomControls />}
        </ReactFlow>
      </ReactFlowProvider>
    </div>
  )
}
