"use client"

import { ChevronRight, type LucideIcon } from "lucide-react"

import { stepColumns, type Workflow } from "@/lib/api/workflows"
import type { StepKind } from "@/components/workflows/WorkflowCanvas"
import { STEP_KIND_STYLE, TRIGGER_ICON, workflowStepIcons } from "@/components/workflows/stepIcons"
import { workflowSummary } from "@/components/workflows/workflowWords"
import { cn } from "@/lib/utils"

type Chip = { icon: LucideIcon; label: string; kind: StepKind }

/**
 * The steps as stops, left to right as the canvas draws them: the trigger, then
 * one stop per column. A column's steps run side by side ("and"); a condition's
 * sends are alternatives, only one of which runs ("or"). A kind shows once per stop.
 */
function stepStops(w: Workflow): { chips: Chip[]; join: "and" | "or" }[] {
  const col = stepColumns(w.definition)
  const trigger = TRIGGER_ICON[w.definition.trigger.type] ?? TRIGGER_ICON.manual
  const stops: { c: number; chips: Chip[]; join: "and" | "or" }[] = [
    { c: 0, chips: [{ ...trigger, kind: "trigger" }], join: "and" },
  ]
  for (const { step, ...chip } of workflowStepIcons(w)) {
    const c = col.get(step.id) ?? 1
    let stop = stops[stops.length - 1]
    if (stop.c !== c) stops.push((stop = { c, chips: [], join: "and" }))
    if (step.type === "send" && step.case !== undefined) stop.join = "or"
    if (!stop.chips.some((x) => x.label === chip.label)) stop.chips.push(chip)
  }
  return stops
}

/**
 * The whole workflow at a glance, above the canvas: its steps in run order as
 * icons, then in plain words when it runs, what it reads, and each condition in
 * order with where it sends. It follows an edit as it is made (the editor passes
 * the previewed workflow).
 */
export function WorkflowSummary({ workflow }: { workflow: Workflow }) {
  const s = workflowSummary(workflow)
  return (
    <section
      aria-label="What this workflow does"
      className="rounded-lg border border-zinc-200 bg-white p-3 text-xs text-zinc-700 dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-300"
    >
      <ol aria-label="Steps in order" className="flex flex-wrap items-center gap-x-1 gap-y-1.5">
        {stepStops(workflow).map(({ chips, join }, i) => (
          <li key={i} className="flex flex-wrap items-center gap-1">
            {i > 0 && <ChevronRight className="h-3.5 w-3.5 text-zinc-400 dark:text-zinc-500" aria-hidden />}
            {chips.map(({ icon: Icon, label, kind }, j) => (
              <span key={label} className="flex items-center gap-1">
                {j > 0 && <span className="text-zinc-500 dark:text-zinc-400"> {join} </span>}
                <span className="flex items-center gap-1.5 rounded-md border border-zinc-200 py-0.5 pl-0.5 pr-2 dark:border-zinc-700">
                  <span className={cn("flex h-6 w-6 items-center justify-center rounded", STEP_KIND_STYLE[kind].tile)}>
                    <Icon className={cn("h-3.5 w-3.5", STEP_KIND_STYLE[kind].icon)} aria-hidden />
                  </span>
                  <span className="font-medium text-zinc-800 dark:text-zinc-200">{label}</span>
                </span>
              </span>
            ))}
          </li>
        ))}
      </ol>
      <p className="mt-2 text-sm text-zinc-800 dark:text-zinc-200">{s.lead}</p>
      {s.conditions.length > 0 && (
        <ol aria-label="Conditions in order" className="mt-1.5 max-h-40 list-decimal overflow-y-auto pl-5">
          {s.conditions.map((c, i) => (
            <li key={i}>{c}</li>
          ))}
        </ol>
      )}
    </section>
  )
}
