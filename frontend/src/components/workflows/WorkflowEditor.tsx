"use client"

import { useCallback, useEffect, useId, useRef, useState } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { ChevronLeft, Loader2, Pencil } from "lucide-react"

import { Button } from "@/components/ui/button"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { WorkflowActionsBar } from "@/components/workflows/WorkflowActionsBar"
import { WorkflowCanvas, workflowSteps, type WorkflowStep } from "@/components/workflows/WorkflowCanvas"
import { WorkflowChangeReview } from "@/components/workflows/WorkflowChangeReview"
import { WorkflowDetails } from "@/components/workflows/WorkflowDetails"
import {
  WorkflowPanelResizeHandle,
  maxPanelWidth,
  panelGridStyle,
  readStoredPanelWidth,
} from "@/components/workflows/WorkflowPanelResizeHandle"
import { WorkflowLoadError, WorkflowStatusLine, WorkflowStatusPill } from "@/components/workflows/WorkflowReview"
import { WorkflowRuns } from "@/components/workflows/WorkflowRuns"
import { WorkflowStepPanel } from "@/components/workflows/WorkflowStepPanel"
import { WorkflowSummary } from "@/components/workflows/WorkflowSummary"
import { WorkflowTestResult } from "@/components/workflows/WorkflowTestResult"
import { useWorkflow, useWorkflowActions } from "@/components/workflows/useWorkflow"
import {
  previewWorkflow,
  useWorkflowEdit,
  type WorkflowDraft,
  type WorkflowEditOptions,
  type WorkflowSaveError,
} from "@/components/workflows/useWorkflowEdit"
import { isBuiltShape, queryStep, type Workflow, type WorkflowDefinition, type WorkflowStepDef } from "@/lib/api/workflows"

/**
 * One workflow, laid out the way n8n lays out its editor: the name, status and
 * actions across the top; an Editor tab with the workflow in plain words, the
 * steps on a canvas under it and the selected step's settings beside it, as
 * wide as the user drags them; a Runs tab with every run beside the
 * workflow's details. Test results open under the canvas.
 *
 * An owner or admin edits it in place (Edit: the name and every step's
 * settings, then Save), with or without an LLM set up. The server checks an
 * edit as it checks a draft, and refuses one of a workflow changed since it
 * was loaded. On a workflow of only a query, its conditions and their
 * messages, Edit also adds, removes and reorders conditions; it never adds or
 * removes any other step, and shows pipeline, model and wait steps read-only:
 * Ask rsync.ai proposes those changes, and changeId (the page's ?change=)
 * shows such a proposal above the editor to apply. created (the page's
 * ?created=, set by WorkflowBuilder) says the workflow was just built here.
 */
export function WorkflowEditor({
  workflowId,
  changeId,
  created,
}: {
  workflowId: string
  changeId?: string | null
  created?: string | null
}) {
  const router = useRouter()
  const { workflow, canManage, error, reload, replace } = useWorkflow(workflowId)
  const [tab, setTab] = useState("editor")
  const [selected, setSelected] = useState("trigger")
  const [runsKey, setRunsKey] = useState(0)
  const onChanged = useCallback(() => {
    setRunsKey((k) => k + 1)
    reload()
  }, [reload])
  const onDeleted = useCallback(() => router.push("/workflows"), [router])
  const [saveNotice, setSaveNotice] = useState<string | null>(() => createdNotice(created))
  // One status line at a time: an action drops what a save said, an edit drops what an action said.
  const onStart = useCallback(() => setSaveNotice(null), [])
  const actions = useWorkflowActions(workflow, { replace, reload, onChanged, onDeleted, onStart })
  // The notice is said once: a reload of the page does not say it again.
  useEffect(() => {
    if (created) router.replace(`/workflows/${workflowId}`)
  }, [created, router, workflowId])
  const onSaved = useCallback(
    (notice: string) => {
      setSaveNotice(notice)
      setRunsKey((k) => k + 1)
    },
    [],
  )
  const edit = useWorkflowEdit(workflow, { replace, onSaved })
  const nameRef = useRef<HTMLInputElement>(null)
  // A refusal is about the definition that was sent: the draft's, whose added
  // conditions the saved one does not have.
  const onSave = async () => {
    const d = edit.draft?.definition
    const refused = await edit.save()
    if (!refused || !d) return
    const node = refusalNode(d, refused, selected)
    if (node === "name") nameRef.current?.focus()
    else if (node) setSelected(node)
  }

  const back = (
    <Link
      href="/workflows"
      className="inline-flex items-center gap-1 text-sm text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200"
    >
      <ChevronLeft className="h-4 w-4" /> Workflows
    </Link>
  )
  if (error) {
    return (
      <div className="space-y-3">
        {back}
        <WorkflowLoadError error={error} onRetry={reload} />
      </div>
    )
  }
  if (!workflow) {
    return (
      <div className="space-y-3">
        {back}
        <div role="status" className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" /> Loading the workflow…
        </div>
      </div>
    )
  }

  const w = workflow
  const editing = edit.draft !== null
  // While editing, the canvas and the panel show the draft as it would save.
  const shown = edit.draft ? previewWorkflow(w, edit.draft, edit.options) : w
  const steps = workflowSteps(shown)
  const current = currentStep(steps, selected)
  const hasQuery = !!queryStep(shown.definition)
  const startEdit = () => {
    setSaveNotice(null)
    actions.clearStatus()
    actions.clearTest()
    setTab("editor")
    edit.start()
  }
  return (
    <div className="space-y-3">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 space-y-0.5">
          {back}
          <div className="flex flex-wrap items-center gap-2">
            {edit.draft ? (
              <input
                ref={nameRef}
                aria-label="Workflow name"
                maxLength={200}
                value={edit.draft.name}
                onChange={(e) => edit.setDraft({ ...edit.draft!, name: e.target.value })}
                className="min-w-[16rem] rounded-md border border-zinc-300 bg-white px-2 py-1 text-lg font-semibold text-zinc-900 focus:outline-none focus:ring-2 focus:ring-violet-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-white"
              />
            ) : (
              <h1 className="truncate text-xl font-semibold text-zinc-900 dark:text-white">{w.name}</h1>
            )}
            <WorkflowStatusPill workflow={w} />
          </div>
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            <WorkflowStatusLine workflow={w} /> Created <LocalDateTime value={w.created_at} />.
          </p>
        </div>
        {editing ? (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" disabled={edit.saving} onClick={edit.cancel}>
              Cancel
            </Button>
            <Button size="sm" disabled={edit.saving} onClick={onSave}>
              {edit.saving && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
              {edit.saving ? (hasQuery ? "Checking the query…" : "Saving…") : "Save"}
            </Button>
          </div>
        ) : canManage ? (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" disabled={!!actions.busy} onClick={startEdit}>
              <Pencil className="mr-1 h-3.5 w-3.5" />
              Edit
            </Button>
            <WorkflowActionsBar
              workflow={w}
              busy={actions.busy}
              onTest={actions.onTest}
              onActivate={actions.onActivate}
              onPause={actions.onPause}
              onRun={actions.onRun}
              onDelete={actions.onDelete}
            />
          </div>
        ) : (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Only workspace owners and admins can test, turn on or run a workflow.
          </p>
        )}
      </header>

      {changeId && !editing && (
        <WorkflowChangeReview changeId={changeId} onApplied={replace} hideOpenLink />
      )}
      {editing && (
        <p className="text-xs text-zinc-500 dark:text-zinc-400">
          {hasQuery ? "Save runs the query once to check it and sends nothing." : "Save sends nothing."}{" "}
          {isBuiltShape(shown.definition)
            ? "Add, remove or move a condition in its settings."
            : "Only Ask rsync.ai adds or removes a step or a condition, or changes a pipeline, model or wait step."}
          {w.status === "active" && " The workflow stays on and runs as you from now on."}
          {edit.options.error && ` ${edit.options.error}`}
        </p>
      )}
      {edit.error && (
        <div role="alert" className="flex flex-wrap items-center gap-2 text-xs text-red-700 dark:text-red-300">
          <span>{edit.error.message}</span>
          {edit.error.stale && (
            <button
              type="button"
              className="font-medium underline"
              onClick={() => {
                edit.cancel()
                reload()
              }}
            >
              Reload it (drops your edit)
            </button>
          )}
        </div>
      )}
      {saveNotice && !editing && (
        <p role="status" className="text-xs text-emerald-700 dark:text-emerald-300">
          {saveNotice}
        </p>
      )}
      {actions.notice && (
        <p role="status" className="text-xs text-emerald-700 dark:text-emerald-300">
          {actions.notice}{" "}
          <button type="button" className="font-medium underline" onClick={() => setTab("runs")}>
            See its runs
          </button>
        </p>
      )}
      {actions.error && (
        <p role="alert" className="text-xs text-red-700 dark:text-red-300">
          {actions.error}
        </p>
      )}

      <Tabs value={tab} onValueChange={setTab} className="space-y-3">
        <TabsList>
          <TabsTrigger value="editor">Editor</TabsTrigger>
          <TabsTrigger value="runs">Runs</TabsTrigger>
        </TabsList>

        <TabsContent value="editor" className="space-y-3">
          <WorkflowSummary workflow={shown} />
          <WorkflowStepsGrid
            workflow={shown}
            steps={steps}
            selectedId={current}
            onSelect={setSelected}
            edit={edit.draft ? { draft: edit.draft, onChange: edit.setDraft, options: edit.options } : undefined}
          />
          {actions.test && (
            <WorkflowTestResult test={actions.test} workflow={w} onClose={actions.clearTest} />
          )}
        </TabsContent>

        <TabsContent value="runs">
          <div className="grid gap-6 @3xl:grid-cols-[minmax(0,1fr)_320px]">
            <div className="min-w-0">
              <WorkflowRuns workflow={w} canManage={canManage} refreshKey={runsKey} onChanged={reload} />
            </div>
            <WorkflowDetails workflow={w} />
          </div>
        </TabsContent>
      </Tabs>
    </div>
  )
}

/**
 * The canvas and, beside it, the selected step's settings, as wide as the
 * user drags them: the editor's and the builder's (WorkflowBuilder) middle.
 */
export function WorkflowStepsGrid({
  workflow,
  steps,
  selectedId,
  onSelect,
  edit,
}: {
  workflow: Workflow
  steps: WorkflowStep[]
  selectedId: string
  onSelect: (id: string) => void
  edit?: { draft: WorkflowDraft; onChange: (next: WorkflowDraft) => void; options: WorkflowEditOptions }
}) {
  // Read once on mount. The server never renders the grid (the workflow loads
  // in the browser), so the width it cannot read is never in its HTML.
  const [panelWidth, setPanelWidth] = useState(readStoredPanelWidth)
  const gridRef = useRef<HTMLDivElement>(null)
  const panelId = useId()
  const measurePanelMax = useCallback(() => maxPanelWidth(gridRef.current?.clientWidth ?? 0), [])
  return (
    // The panel is as wide as the user chose, and never squeezes the canvas under CANVAS_MIN_WIDTH.
    <div
      ref={gridRef}
      data-testid="workflow-editor-grid"
      style={panelGridStyle(panelWidth)}
      className="grid gap-3 @3xl:h-[calc(100dvh-17rem)] @3xl:min-h-[460px] @3xl:grid-cols-[minmax(0,1fr)_min(var(--workflow-panel-width),calc(100%_-_var(--workflow-canvas-reserve)))]"
    >
      <WorkflowCanvas workflow={workflow} selectedId={selectedId} onSelect={onSelect} className="min-h-[420px]" />
      <div className="relative min-h-0">
        <WorkflowPanelResizeHandle
          width={panelWidth}
          onWidth={setPanelWidth}
          measureMax={measurePanelMax}
          controls={panelId}
        />
        <div
          id={panelId}
          className="h-full min-h-0 overflow-hidden rounded-lg border border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900"
        >
          <WorkflowStepPanel workflow={workflow} steps={steps} selectedId={selectedId} onSelect={onSelect} edit={edit} />
        </div>
      </div>
    </div>
  )
}

/** What the editor says of a workflow WorkflowBuilder just saved (?created=checked or unchecked). */
function createdNotice(created: string | null | undefined): string | null {
  if (created === "checked") {
    return "Saved as a draft. rsync.ai ran the query once to check it; nothing was sent. Test it, then Turn on."
  }
  if (created === "unchecked") {
    return "Saved as a draft. The query was still running when the check stopped, so click Test to run it in full."
  }
  return null
}

/**
 * The node a save refusal opens: "name" for the name box, else the node the
 * refusal's step, condition (the API's 1-based branch) or field is on, or null
 * for nothing to open. d is the definition that was sent.
 */
export function refusalNode(d: WorkflowDefinition, refused: WorkflowSaveError, selected: string): string | null {
  const step = refused.step
  const b = refused.branch ? refused.branch - 1 : null
  const named = refused.stepId ? d.steps.find((s) => s.id === refused.stepId) : undefined
  if (step === "name") return "name"
  // A condition's refusal names the send of its case (workflowdef.CheckResult): open the condition.
  if (step === "condition") return `branch:${b ?? named?.case ?? selectedBranch(d, selected) ?? 0}`
  if (named) return refusedNode(named, b ?? selectedBranch(d, selected))
  if (step === "channel") {
    // "channel:<send>:first" falls to that send's first channel (currentStep).
    const send = b !== null ? d.steps.find((s) => s.type === "send" && s.case === b) : undefined
    return send ? `channel:${send.id}:first` : selected.startsWith("channel:") ? selected : "channel:first"
  }
  return step
}

/** The 0-based condition a "branch:<i>" node, or a channel of condition i's send, belongs to. */
function selectedBranch(d: WorkflowDefinition, id: string): number | null {
  const m = /^branch:(\d+)$/.exec(id)
  if (m) return Number(m[1])
  const send = /^channel:([^:]+):/.exec(id)?.[1]
  return d.steps.find((s) => s.id === send)?.case ?? null
}

/** The node a refusal about step s opens: the query, condition b, the send's first channel, or the step. */
function refusedNode(s: WorkflowStepDef, b: number | null): string {
  switch (s.type) {
    case "query":
      return "query"
    case "condition":
      return `branch:${b ?? 0}`
    case "send":
      return `channel:${s.id}:first`
    default:
      return `step:${s.id}`
  }
}

/**
 * The step to show for the selected id: itself while it exists; else, for a
 * channel the edit removed (or "channel:<send>:first"), that send's first
 * channel, then its condition, then the first channel; else the trigger.
 */
export function currentStep(steps: WorkflowStep[], selected: string): string {
  if (steps.some((s) => s.id === selected)) return selected
  if (selected.startsWith("channel:")) {
    const send = /^channel:([^:]+):/.exec(selected)?.[1]
    const own = steps.find((s) => s.kind === "channel" && s.stepId === send)
    const b = steps.find((s) => s.stepId === send)?.branch
    return (
      own?.id ??
      (b !== undefined ? steps.find((s) => s.kind === "condition" && s.branch === b)?.id : undefined) ??
      steps.find((s) => s.kind === "channel")?.id ??
      "trigger"
    )
  }
  if (selected.startsWith("branch:")) return steps.find((s) => s.kind === "condition")?.id ?? "trigger"
  return "trigger"
}
