"use client"

import { useCallback, useState } from "react"

import { describeCron } from "@/components/explorer/cronSentence"
import { conditionClause } from "@/components/workflows/workflowWords"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { listAllPipelines, MAX_PIPELINE_PAGES, PIPELINE_PAGE_SIZE } from "@/lib/pipeline/listAllPipelines"
import {
  conditionStep,
  createWorkflow,
  listDeliveryChannels,
  queryStep,
  updateWorkflow,
  WorkflowError,
  type DeliveryChannel,
  type Workflow,
  type WorkflowChannel,
  type WorkflowCondition,
  type WorkflowDefinition,
  type WorkflowRef,
} from "@/lib/api/workflows"

/** The edit in progress: what Save sends. */
export interface WorkflowDraft {
  name: string
  definition: WorkflowDefinition
  /** Each send step's channel ids, by the step's id. */
  nodeChannelIds: Record<string, string[]>
}

/** What the pickers offer: the workspace's delivery channels, SQL connections and pipelines. */
export interface WorkflowEditOptions {
  channels: DeliveryChannel[]
  connections: WorkflowRef[]
  pipelines: WorkflowRef[]
  /** The lists have loaded (or failed to): until then an empty one is not "none to pick". */
  loaded: boolean
  /**
   * Set when a list could not be loaded (the pickers then offer only the saved choice),
   * or when the pipeline list stopped at its cap.
   */
  error: string | null
}

export interface WorkflowSaveError {
  message: string
  /** The step the refusal names, so the editor can open it. */
  step: RefusalStep | null
  /** The 1-based branch the refusal is about, when it is about one. */
  branch?: number
  /** The id of the step the refusal is about, when it names one. */
  stepId?: string
  /** The workflow changed since it was loaded (409): the edit can only be dropped. */
  stale: boolean
}

export type RefusalStep = "name" | "trigger" | "query" | "condition" | "channel"

/**
 * The step a save refusal's code names (handlers/workflows.go: refusePlan and
 * workflow.Normalize's "invalid_<field>"). An unknown code opens nothing.
 */
export function refusalStep(code: string | undefined): RefusalStep | null {
  switch (code) {
    case "invalid_name":
      return "name"
    case "invalid_trigger":
    case "invalid_cron":
    case "invalid_timezone":
    case "invalid_pipeline_id":
    case "invalid_pipeline":
    case "pipeline_not_found":
      return "trigger"
    case "invalid_connection_id":
    case "invalid_connection":
    case "connection_not_found":
    case "connection_not_sql":
    case "invalid_sql":
    case "query_failed":
      return "query"
    case "invalid_branches":
    case "invalid_condition":
    case "invalid_condition_op":
    case "invalid_condition_column":
    case "invalid_threshold":
    case "invalid_quiet_minutes":
      return "condition"
    case "invalid_message":
    case "invalid_subject":
    case "invalid_channels":
    case "no_channels":
    case "too_many_channels":
    case "channel_not_found":
      return "channel"
    default:
      return null
  }
}

function draftOf(w: Workflow): WorkflowDraft {
  const nodeChannelIds: Record<string, string[]> = {}
  for (const s of w.definition.steps) {
    if (s.type === "send") nodeChannelIds[s.id] = w.channels.filter((c) => c.node_id === s.id).map((c) => c.id)
  }
  return {
    name: w.name,
    definition: JSON.parse(JSON.stringify(w.definition)) as WorkflowDefinition,
    nodeChannelIds,
  }
}

// The server's wording (internal/workflow/definition.go: DescribeCondition,
// DescribeBranch, DescribeBranches), so a draft reads as it will once saved.
export function describeCondition(c: WorkflowCondition): string {
  const clause = conditionClause(c)
  return clause ? `send when ${clause}` : "always send"
}

export function describeBranch(c: WorkflowCondition, i: number, n: number): string {
  if (n > 1 && i === n - 1 && (c.type === "always" || !c.type)) return "otherwise send"
  return describeCondition(c)
}

export function describeBranches(cs: WorkflowCondition[]): string {
  return cs
    .map((c, i) => {
      const part = describeBranch(c, i, cs.length)
      return i > 0 && !part.startsWith("otherwise") ? `else ${part}` : part
    })
    .join("; ")
}

/** Keeps the saved choice in a list that came back without it (or did not come back). */
function withSaved<T extends { id: string }>(list: T[], saved: T | null | undefined): T[] {
  return saved && !list.some((x) => x.id === saved.id) ? [saved, ...list] : list
}

async function loadSQLConnections(): Promise<WorkflowRef[]> {
  const res = await authFetch(API_ENDPOINTS.CONNECTIONS.LIST)
  if (!res.ok) throw new Error(`The server answered ${res.status} for your connections`)
  const data = (await res.json()) as {
    connections?: { id: string; name: string; connector_type: string; supports_explorer?: boolean; explorer_mode?: string }[]
  }
  // The server's own test (workflowConnectionRefusal): a connection the
  // explorer queries with SQL. MongoDB is explorable but not with SQL.
  return (data.connections ?? [])
    .filter((c) => c.supports_explorer === true && (c.explorer_mode ?? "sql") === "sql")
    .map((c) => ({ id: c.id, name: c.name, connector_type: c.connector_type }))
    .sort((a, b) => a.name.localeCompare(b.name))
}

// Every pipeline, not one page of them: a single `limit=200` read silently dropped
// every older pipeline from the trigger picker.
async function loadPipelines(): Promise<{ refs: WorkflowRef[]; truncated: boolean }> {
  const { pipelines, truncated } = await listAllPipelines().catch((e: unknown) => {
    throw new Error(`Your pipelines did not load (${e instanceof Error ? e.message : String(e)})`)
  })
  return {
    refs: pipelines.map((p) => ({ id: p.id, name: p.name })).sort((a, b) => a.name.localeCompare(b.name)),
    truncated,
  }
}

/**
 * The workflow as the draft would save it, for the canvas and the step panel:
 * the draft's definition, channels and connection, described the way the
 * server describes a saved one (workflow.DescribeTrigger, DescribeBranches).
 */
export function previewWorkflow(w: Workflow, draft: WorkflowDraft, options: WorkflowEditOptions): Workflow {
  const d = draft.definition
  const q = queryStep(d)
  const connection = q
    ? (options.connections.find((c) => c.id === q.connection_id) ??
      (w.connection?.id === q.connection_id ? w.connection : null))
    : null
  const pipeline =
    options.pipelines.find((p) => p.id === d.trigger.pipeline_id) ??
    (w.pipeline?.id === d.trigger.pipeline_id ? w.pipeline : undefined)
  const known = new Map<string, Omit<WorkflowChannel, "node_id">>(w.channels.map((c) => [c.id, c]))
  for (const c of options.channels) known.set(c.id, { id: c.id, name: c.name, kind: c.kind })
  // In step order, as the server lists a saved workflow's channels.
  const channels: WorkflowChannel[] = d.steps.flatMap((s) =>
    (draft.nodeChannelIds[s.id] ?? []).flatMap((id) => {
      const c = known.get(id)
      return c ? [{ id: c.id, name: c.name, kind: c.kind, node_id: s.id }] : []
    }),
  )
  const conds = conditionStep(d)?.conditions ?? []

  let trigger: string
  if (d.trigger.type === "schedule") {
    const words = describeCron(d.trigger.cron)
    trigger = `${words ? words[0].toLowerCase() + words.slice(1) : `on cron ${d.trigger.cron ?? ""}`} (${d.trigger.timezone || "UTC"})`
  } else if (d.trigger.type === "after_pipeline") {
    trigger = pipeline ? `after ${pipeline.name} finishes` : "after a pipeline you pick finishes"
  } else {
    trigger = "only when someone clicks Run now"
  }

  return {
    ...w,
    name: draft.name,
    definition: d,
    connection,
    pipeline,
    channels,
    trigger_description: trigger,
    condition_description: conds.length ? describeBranches(conds) : w.condition_description,
    branch_descriptions: conds.map((c, i) => describeBranch(c, i, conds.length)),
    // The last run belongs to the saved version; its marks would mislead here.
    last_run: undefined,
  }
}

/**
 * Edit, Save and Cancel for one workflow. Save sends the whole draft with the
 * updated_at it started from; the server checks it as it checks a draft
 * (running the query once, when it has one) and refuses an edit of a workflow changed since.
 *
 * With create set, workflow is the unsaved one a person is building
 * (WorkflowBuilder): Save creates it as a draft (POST /workflows) and hands
 * the saved workflow to create; the draft stays until the page moves on.
 */
export function useWorkflowEdit(
  workflow: Workflow | null,
  opts: {
    replace: (w: Workflow) => void
    onSaved: (notice: string) => void
    create?: (saved: Workflow, queryChecked: boolean) => void
  },
) {
  const [draft, setDraft] = useState<WorkflowDraft | null>(null)
  const [options, setOptions] = useState<WorkflowEditOptions>({
    channels: [],
    connections: [],
    pipelines: [],
    loaded: false,
    error: null,
  })
  // Whether this person may save (owner or admin), as the channel list says; null until it loads.
  const [canManage, setCanManage] = useState<boolean | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<WorkflowSaveError | null>(null)
  const { replace, onSaved, create } = opts

  const start = useCallback(() => {
    if (!workflow) return
    setDraft(draftOf(workflow))
    setError(null)
    const saved = { connection: workflow.connection, pipeline: workflow.pipeline }
    Promise.allSettled([listDeliveryChannels(), loadSQLConnections(), loadPipelines()]).then(([ch, conns, pipes]) => {
      const failed = [ch, conns, pipes].find((r): r is PromiseRejectedResult => r.status === "rejected")
      setCanManage(ch.status === "fulfilled" ? ch.value.can_manage : null)
      setOptions({
        channels: ch.status === "fulfilled" ? ch.value.channels : [],
        connections: withSaved(conns.status === "fulfilled" ? conns.value : [], saved.connection),
        pipelines: withSaved(pipes.status === "fulfilled" ? pipes.value.refs : [], saved.pipeline),
        loaded: true,
        error: failed
          ? `${failed.reason instanceof Error ? failed.reason.message : "A list did not load"}; the pickers offer only what is saved.`
          : pipes.status === "fulfilled" && pipes.value.truncated
            ? `Only the newest ${(MAX_PIPELINE_PAGES * PIPELINE_PAGE_SIZE).toLocaleString()} pipelines are offered.`
            : null,
      })
    })
  }, [workflow])

  const cancel = useCallback(() => {
    setDraft(null)
    setError(null)
  }, [])

  /** Saves the draft; on a refusal, answers it so the editor can open the step at fault. */
  const save = useCallback(async (): Promise<WorkflowSaveError | null> => {
    if (!workflow || !draft || saving) return null
    setSaving(true)
    setError(null)
    try {
      if (create) {
        const made = await createWorkflow({
          name: draft.name,
          definition: draft.definition,
          node_channel_ids: draft.nodeChannelIds,
        })
        create(made.workflow, made.query_checked)
        return null
      }
      const { workflow: saved, query_checked } = await updateWorkflow(workflow.id, {
        name: draft.name,
        definition: draft.definition,
        node_channel_ids: draft.nodeChannelIds,
        updated_at: workflow.updated_at,
      })
      replace(saved)
      setDraft(null)
      onSaved(
        !queryStep(draft.definition)
          ? "Saved. Nothing was sent."
          : query_checked
            ? "Saved. rsync.ai ran the query once to check it; nothing was sent."
            : "Saved. The query was still running when the check stopped, so click Test to run it in full.",
      )
      return null
    } catch (err) {
      const e = err instanceof WorkflowError ? err : null
      const refused: WorkflowSaveError = {
        message: err instanceof Error ? err.message : "Could not save the workflow",
        step: refusalStep(e?.code),
        branch: e?.branch,
        stepId: e?.step,
        stale: e?.status === 409,
      }
      setError(refused)
      return refused
    } finally {
      setSaving(false)
    }
  }, [workflow, draft, saving, replace, onSaved, create])

  return { draft, setDraft, options, canManage, saving, error, start, cancel, save, editing: draft !== null }
}
