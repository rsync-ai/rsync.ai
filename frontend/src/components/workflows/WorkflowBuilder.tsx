"use client"

import { useEffect, useRef, useState, useSyncExternalStore } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { ChevronLeft, Loader2 } from "lucide-react"

import { readHomeAskMode } from "@/components/home/HomeAskBox"
import { Button } from "@/components/ui/button"
import { workflowSteps } from "@/components/workflows/WorkflowCanvas"
import { WorkflowStepsGrid, currentStep, refusalNode } from "@/components/workflows/WorkflowEditor"
import { WorkflowSummary } from "@/components/workflows/WorkflowSummary"
import { previewWorkflow, useWorkflowEdit } from "@/components/workflows/useWorkflowEdit"
import { featureFlagsManager } from "@/config/features"
import { newWorkflowDefinition, withQuery, type Workflow } from "@/lib/api/workflows"
import { takeWorkflowQuery } from "@/lib/explorer/queryTabs"
import { onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

const subscribeFlags = (onChange: () => void) => featureFlagsManager.subscribe(() => onChange())
const noop = () => {}

/** The workflow a person starts from: daily at 9:00 UTC, a query to fill in, one condition that always sends. */
function newWorkflow(): Workflow {
  return {
    id: "",
    name: "",
    status: "draft",
    definition: newWorkflowDefinition(),
    trigger_description: "",
    condition_description: "",
    branch_descriptions: [],
    connection: null,
    channels: [],
    proposed_by: "none",
    created_at: "",
    updated_at: "",
    url: "",
  }
}

/**
 * A workflow built by hand (/workflows/new), with or without an LLM: the
 * editor's canvas and step settings on an unsaved workflow, for an owner or
 * admin. It is a query, its conditions in order and a message and channels
 * for each; Save checks it as the server checks any draft (running the query
 * once, sending nothing), saves it off, and opens it in the editor to Test and
 * Turn on. Pipeline, model, wait, chart and dashboard steps are Ask rsync.ai's.
 * fromExplorer: opened by the Explorer's "Alert on this query", whose statement
 * and connection fill in the query step.
 */
export function WorkflowBuilder({ fromExplorer = false }: { fromExplorer?: boolean } = {}) {
  const router = useRouter()
  const [seed] = useState(newWorkflow)
  const [selected, setSelected] = useState("query")
  const nameRef = useRef<HTMLInputElement>(null)
  const canAsk = useSyncExternalStore(subscribeFlags, readHomeAskMode, () => "loading" as const) === "assistant"
  const edit = useWorkflowEdit(seed, {
    replace: noop,
    onSaved: noop,
    create: (saved, checked) => router.replace(`/workflows/${saved.id}?created=${checked ? "checked" : "unchecked"}`),
  })
  const { start, setDraft } = edit
  const started = edit.draft !== null

  useEffect(() => {
    start()
  }, [start])

  // The Explorer's query fills in the query step once, as soon as there is a
  // draft to fill; the schedule, the condition and the send stay a new workflow's.
  const tookQuery = useRef(false)
  useEffect(() => {
    if (!fromExplorer || tookQuery.current || !started) return
    tookQuery.current = true
    const q = takeWorkflowQuery()
    if (!q) return
    setDraft((d) => d && { ...d, definition: withQuery(d.definition, { connection_id: q.connectionId, sql: q.sql }) })
  }, [fromExplorer, started, setDraft])

  // The pickers list the workspace it was opened in; another one's
  // connections and channels are not this one's.
  useEffect(() => onActiveWorkspaceChange(() => router.replace("/workflows")), [router])

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
  if (edit.canManage === false) {
    return (
      <div className="space-y-3">
        {back}
        <p className="text-sm text-zinc-600 dark:text-zinc-300">Only workspace owners and admins can build a workflow.</p>
      </div>
    )
  }
  if (!edit.draft) {
    return (
      <div className="space-y-3">
        {back}
        <div role="status" className="flex items-center gap-2 text-sm text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-4 w-4 animate-spin" /> Loading…
        </div>
      </div>
    )
  }

  const draft = edit.draft
  const shown = previewWorkflow(seed, draft, edit.options)
  const steps = workflowSteps(shown)
  const current = currentStep(steps, selected)
  return (
    <div className="space-y-3">
      <header className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0 space-y-0.5">
          {back}
          <input
            ref={nameRef}
            aria-label="Workflow name"
            placeholder="Name it, such as Weekly revenue digest"
            maxLength={200}
            value={draft.name}
            onChange={(e) => edit.setDraft({ ...draft, name: e.target.value })}
            className="min-w-[16rem] rounded-md border border-zinc-300 bg-white px-2 py-1 text-lg font-semibold text-zinc-900 focus:outline-none focus:ring-2 focus:ring-violet-400 dark:border-zinc-700 dark:bg-zinc-950 dark:text-white"
          />
          <p className="text-xs text-zinc-500 dark:text-zinc-400">New workflow. It stays off until you turn it on.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {edit.saving ? (
            <Button size="sm" variant="outline" disabled>
              Cancel
            </Button>
          ) : (
            <Button asChild size="sm" variant="outline">
              <Link href="/workflows">Cancel</Link>
            </Button>
          )}
          <Button size="sm" disabled={edit.saving} onClick={onSave}>
            {edit.saving && <Loader2 className="mr-1 h-3.5 w-3.5 animate-spin" />}
            {edit.saving ? "Checking the query…" : "Save"}
          </Button>
        </div>
      </header>
      <p className="text-xs text-zinc-500 dark:text-zinc-400">
        Pick when it runs, then a connection and a query, the conditions to check in order, and a message and channels
        for each. Save runs the query once to check it, sends nothing, and saves it as a draft.
        {canAsk && " To run a pipeline, rebuild a model, wait, or send a chart or dashboard, ask rsync.ai."}
        {edit.options.error && ` ${edit.options.error}`}
      </p>
      {edit.error && (
        <p role="alert" className="text-xs text-red-700 dark:text-red-300">
          {edit.error.message}
        </p>
      )}
      <WorkflowSummary workflow={shown} />
      <WorkflowStepsGrid
        workflow={shown}
        steps={steps}
        selectedId={current}
        onSelect={setSelected}
        edit={{ draft, onChange: edit.setDraft, options: edit.options }}
      />
    </div>
  )
}
