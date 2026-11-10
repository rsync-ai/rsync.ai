/**
 * Workflows: a trigger, then steps that each run after others (definition
 * version 3): process steps (run a pipeline, refresh a model, wait), at most
 * one read-only SQL query on a connection with a condition step on its result
 * (any number of conditions; the API calls each a branch; the last may be an
 * "otherwise"; the first that is true sends), and send steps, each a message
 * to its own delivery channels (Slack channels and email lists). Ask rsync.ai
 * or an agent drafts one (plan_workflow), or an owner or admin builds one in
 * the editor (POST /workflows: a query, its conditions and a message for each,
 * with or without an LLM); a draft runs and sends nothing until the person
 * tests it and turns it on here, with their own session. An owner or admin
 * edits it here too, adding and removing conditions, but only Ask rsync.ai
 * adds or removes any other step (plan_workflow, plan_workflow_change); the
 * screens show process steps read-only.
 * handlers/workflows.go, handlers/workflow_graph_runs.go, handlers/workflow_changes.go,
 * handlers/delivery_channels.go
 *   GET    /workflows                 -> { workflows, can_manage }
 *   POST   /workflows                 { name, definition, node_channel_ids }
 *                                     -> 201 { workflow, can_manage, query_checked }; a draft
 *   GET    /workflows/:id             -> { workflow, can_manage }
 *   PUT    /workflows/:id             { name, definition, node_channel_ids, updated_at }
 *                                     -> { workflow, query_checked }; 409 when it changed since it was loaded
 *   GET    /workflows/:id/runs        -> { runs }
 *   POST   /workflows/:id/runs/:run/stop -> 202 { status: "stopping" }; 409 when it already stopped or finished
 *   POST   /workflows/:id/test        -> WorkflowTestResult (nothing is sent, no step runs)
 *   POST   /workflows/:id/activate    -> { workflow }
 *   POST   /workflows/:id/pause
 *   POST   /workflows/:id/run         -> { run_id }
 *   DELETE /workflows/:id
 *   GET    /workflow-changes/:id      -> { change, can_manage }
 *   POST   /workflow-changes/:id/apply -> { workflow, query_checked }
 *   GET    /delivery-channels         -> { channels, can_manage }
 *   POST   /delivery-channels         { name, kind, webhook_url | recipients }
 *   PUT    /delivery-channels/:id
 *   DELETE /delivery-channels/:id
 *   POST   /delivery-channels/:id/test
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"

export type WorkflowStatus = "draft" | "active" | "paused"
export type TriggerType = "schedule" | "after_pipeline" | "manual"
export type ConditionType = "always" | "rows_exist" | "no_rows" | "value"
export type StepType = "query" | "condition" | "send" | "run_pipeline" | "refresh_model" | "wait" | "chart" | "dashboard"
/** When a step runs after its parents: every one passed (the default), every one finished, or one failed. */
export type StepRule = "all_success" | "all_done" | "any_failed"

export interface WorkflowCondition {
  type: ConditionType
  column?: string
  op?: string
  threshold: number
}

export interface WorkflowMessage {
  subject: string
  body: string
}

/** One step of a workflow (shared/go/workflowdef Step): what it does and which steps it runs after. */
export interface WorkflowStepDef {
  id: string
  type: StepType
  /** The steps it runs after; none means it starts the run. */
  after?: string[]
  rule?: StepRule | ""
  /** query */
  connection_id?: string
  sql?: string
  /** condition: the first that is true sends its case. */
  conditions?: WorkflowCondition[]
  /** send: the 0-based condition it sends for; absent on a send that sends when its steps finish. */
  case?: number
  message?: WorkflowMessage
  /** send: its failure fails the run. */
  required?: boolean
  /** run_pipeline */
  pipeline_id?: string
  /** refresh_model */
  model_id?: string
  /** run_pipeline and refresh_model: how long it may take, retries included (12 hours when absent). */
  timeout_minutes?: number
  /** run_pipeline: how many times a failed run is started again (0 to 3). */
  retries?: number
  /** wait */
  minutes?: number
  /** chart: the saved chart it draws into the message after it. */
  chart_id?: string
  /** dashboard: the dashboard whose tiles it draws into the message after it, with the board as a PDF. */
  dashboard_id?: string
}

export interface WorkflowDefinition {
  version: number
  trigger: { type: TriggerType; cron?: string; timezone?: string; pipeline_id?: string }
  steps: WorkflowStepDef[]
  /** Per condition: a condition that sent within this many minutes does not send again. */
  quiet_minutes?: number
}

/** A step's timeout when it names none: 12 hours, retries included. */
export const DEFAULT_STEP_TIMEOUT_MINUTES = 720

export function isProcessStep(s: WorkflowStepDef): boolean {
  return s.type === "run_pipeline" || s.type === "refresh_model" || s.type === "wait"
}

/** The workflow's query step; undefined when it has none. */
export function queryStep(d: WorkflowDefinition): WorkflowStepDef | undefined {
  return d.steps.find((s) => s.type === "query")
}

/** The workflow's condition step; undefined when it has none. */
export function conditionStep(d: WorkflowDefinition): WorkflowStepDef | undefined {
  return d.steps.find((s) => s.type === "condition")
}

/** The send for condition i (0-based). */
export function caseSend(d: WorkflowDefinition, i: number): WorkflowStepDef | undefined {
  return d.steps.find((s) => s.type === "send" && s.case === i)
}

/** A step's rule as it runs: a step with steps before it and no rule waits for them all to pass. */
export function stepRule(s: WorkflowStepDef): StepRule | "" {
  if (!s.after?.length) return ""
  return s.rule || "all_success"
}

/**
 * Each step's column, left to right: a step that starts the run is in column
 * 1 (the trigger is column 0), any other one column right of its furthest
 * step before it. A step whose steps before it are missing, or that sits on a
 * loop the server would have refused, counts as starting the run.
 */
export function stepColumns(d: WorkflowDefinition): Map<string, number> {
  const byId = new Map(d.steps.map((s) => [s.id, s]))
  const col = new Map<string, number>()
  const visiting = new Set<string>()
  const of = (id: string): number => {
    const known = col.get(id)
    if (known !== undefined) return known
    const s = byId.get(id)
    if (!s || visiting.has(id)) return 0
    visiting.add(id)
    let c = 1
    for (const p of s.after ?? []) if (byId.has(p)) c = Math.max(c, of(p) + 1)
    visiting.delete(id)
    col.set(id, c)
    return c
  }
  for (const s of d.steps) of(s.id)
  return col
}

/** The steps left to right, by column, then in definition order within a column. */
export function stepsInOrder(d: WorkflowDefinition): WorkflowStepDef[] {
  const col = stepColumns(d)
  return d.steps
    .map((s, i) => ({ s, i, c: col.get(s.id) ?? 1 }))
    .sort((a, b) => a.c - b.c || a.i - b.i)
    .map((x) => x.s)
}

/** The query's connection and SQL; null when the workflow has no query. */
export function definitionQuery(d: WorkflowDefinition): { connection_id: string; sql: string } | null {
  const q = queryStep(d)
  return q ? { connection_id: q.connection_id ?? "", sql: q.sql ?? "" } : null
}

/**
 * One send as the screens list it (the API's branch, for a condition's send):
 * its message goes to the channels whose node_id is `send`.
 */
export interface WorkflowBranch {
  /** The send step's id. */
  send: string
  /** The 0-based condition it sends for; absent on a send that sends when its steps finish. */
  case?: number
  condition?: WorkflowCondition
  message: WorkflowMessage
  /** That send step. */
  step: WorkflowStepDef
}

/**
 * Every send of the workflow: each condition with its send, top to bottom,
 * then any send that follows no condition, in definition order.
 */
export function workflowBranches(d: WorkflowDefinition): WorkflowBranch[] {
  const out: WorkflowBranch[] = []
  const conds = conditionStep(d)?.conditions ?? []
  conds.forEach((condition, i) => {
    const s = caseSend(d, i)
    if (s) out.push({ send: s.id, case: i, condition, message: s.message ?? { subject: "", body: "" }, step: s })
  })
  for (const s of d.steps) {
    if (s.type === "send" && s.case === undefined) {
      out.push({ send: s.id, message: s.message ?? { subject: "", body: "" }, step: s })
    }
  }
  return out
}

/** d with the query's connection and SQL changed; d itself when it has no query. */
export function withQuery(d: WorkflowDefinition, q: { connection_id: string; sql: string }): WorkflowDefinition {
  return {
    ...d,
    steps: d.steps.map((s) => (s.type === "query" ? { ...s, connection_id: q.connection_id, sql: q.sql } : s)),
  }
}

/**
 * d with one send's message and, for a condition's send, its condition
 * changed. Ids, the order of the steps and every other step stay as they are.
 */
export function withBranch(
  d: WorkflowDefinition,
  send: string,
  patch: { condition?: WorkflowCondition; message?: WorkflowMessage },
): WorkflowDefinition {
  const target = d.steps.find((s) => s.id === send)
  const k = target?.case
  return {
    ...d,
    steps: d.steps.map((s) => {
      if (s.id === send && patch.message) return { ...s, message: patch.message }
      if (s.type === "condition" && patch.condition && k !== undefined && s.conditions) {
        return { ...s, conditions: s.conditions.map((c, i) => (i === k ? patch.condition! : c)) }
      }
      return s
    }),
  }
}

/** The message a condition the person adds starts with: only slots every query fills. */
export const NEW_CONDITION_BODY = "{{workflow_name}}: the query returned {{row_count}} rows at {{run_time}}.\n\n{{table}}"

/** The first send_<k> id d does not use. */
function freeSendId(d: WorkflowDefinition): string {
  const used = new Set(d.steps.map((s) => s.id))
  let k = 1
  while (used.has(`send_${k}`)) k++
  return `send_${k}`
}

/**
 * d with a new condition and its send, as the server's ReplaceBranches adds
 * one: the send gets the next free send_<k> id and fails the run when it cannot
 * deliver, as every condition's send does. It goes above an otherwise (a last
 * condition that always sends), since only the last condition may always send;
 * the sends below it move down one. Answers the new condition's index and its
 * send's id. Only for a workflow with a condition step (handlers/workflows.go
 * workflowConditionsEditRefusal keeps every other step as it is).
 */
export function addCondition(d: WorkflowDefinition): { definition: WorkflowDefinition; at: number; send: string } {
  const conds = conditionStep(d)?.conditions ?? []
  const n = conds.length
  const at = n > 0 && conds[n - 1].type === "always" ? n - 1 : n
  const send = freeSendId(d)
  const condition: WorkflowCondition = { type: "rows_exist", threshold: 0 }
  const steps = d.steps.map((s) => {
    if (s.type === "condition") return { ...s, conditions: [...conds.slice(0, at), condition, ...conds.slice(at)] }
    if (s.type === "send" && s.case !== undefined && s.case >= at) return { ...s, case: s.case + 1 }
    return s
  })
  steps.push({
    id: send,
    type: "send",
    after: ["condition"],
    rule: "all_success",
    case: at,
    message: { subject: "", body: NEW_CONDITION_BODY },
    required: true,
  })
  return { definition: { ...d, steps }, at, send }
}

/**
 * d without condition i and its send; the sends below it move up one. Answers
 * the removed send's id, so its channels can go too, or null when i is the
 * only condition (a workflow sends at least one message) or not there.
 */
export function removeCondition(d: WorkflowDefinition, i: number): { definition: WorkflowDefinition; send: string } | null {
  const conds = conditionStep(d)?.conditions ?? []
  const send = caseSend(d, i)
  if (conds.length < 2 || i < 0 || i >= conds.length || !send) return null
  const steps = d.steps
    .filter((s) => s.id !== send.id)
    .map((s) => {
      if (s.type === "condition") return { ...s, conditions: conds.filter((_, j) => j !== i) }
      if (s.type === "send" && s.case !== undefined && s.case > i) return { ...s, case: s.case - 1 }
      return s
    })
  return { definition: { ...d, steps }, send: send.id }
}

/**
 * d with condition i moved to j (one place up or down), its send with it: the
 * first condition that is true sends, so the order is the priority. Each send
 * keeps its id and so its channels. Null when the move would put an otherwise
 * (a condition that always sends) anywhere but last, or i or j is not there.
 */
export function moveCondition(d: WorkflowDefinition, i: number, j: number): WorkflowDefinition | null {
  const conds = conditionStep(d)?.conditions ?? []
  const n = conds.length
  if (i < 0 || j < 0 || i >= n || j >= n || Math.abs(i - j) !== 1) return null
  const moved = [...conds]
  ;[moved[i], moved[j]] = [moved[j], moved[i]]
  if (moved.slice(0, -1).some((c) => c.type === "always")) return null
  return {
    ...d,
    steps: d.steps.map((s) => {
      if (s.type === "condition") return { ...s, conditions: moved }
      if (s.type === "send" && s.case === i) return { ...s, case: j }
      if (s.type === "send" && s.case === j) return { ...s, case: i }
      return s
    }),
  }
}

/**
 * True for a workflow of only a query, a condition step after it and one send
 * per condition after that: the shape the editor builds, whose conditions an
 * edit may add, remove and reorder (the server's Definition.IsLegacyShape). A
 * workflow with a pipeline, model, wait, chart or dashboard step is not.
 */
export function isBuiltShape(d: WorkflowDefinition): boolean {
  const q = queryStep(d)
  const c = conditionStep(d)
  if (!q || !c || q.after?.length || c.after?.length !== 1 || c.after[0] !== q.id) return false
  const conds = c.conditions ?? []
  if (d.steps.length !== 2 + conds.length) return false
  return conds.every((_, i) => {
    const s = caseSend(d, i)
    return !!s && s.after?.length === 1 && s.after[0] === c.id && (!s.rule || s.rule === "all_success")
  })
}

/**
 * Where a workflow built in the editor starts: every morning at 09:00 UTC, a
 * query to fill in, one condition that sends every time, and its message (the
 * steps the server's FromBranches makes).
 */
export function newWorkflowDefinition(): WorkflowDefinition {
  return {
    version: 3,
    trigger: { type: "schedule", cron: "0 9 * * *", timezone: "UTC" },
    steps: [
      { id: "query", type: "query", connection_id: "", sql: "" },
      { id: "condition", type: "condition", after: ["query"], rule: "all_success", conditions: [{ type: "always", threshold: 0 }] },
      {
        id: "send_1",
        type: "send",
        after: ["condition"],
        rule: "all_success",
        case: 0,
        message: { subject: "", body: NEW_CONDITION_BODY },
        required: true,
      },
    ],
    quiet_minutes: 0,
  }
}

export interface WorkflowRef {
  id: string
  name: string
  connector_type?: string
}

export interface WorkflowChannel {
  id: string
  name: string
  kind: "slack" | "email"
  /** The send step this channel belongs to. */
  node_id: string
}

export interface DeliveryOutcome {
  /** The send step that sent it (absent on a run from before steps). */
  step_id?: string
  channel_id: string
  name: string
  kind: string
  status: "sent" | "failed" | "skipped"
  sent?: number
  error?: string
}

export type StepRunStatus = "running" | "waiting" | "succeeded" | "warned" | "failed" | "skipped" | "stopped"

/** How one step of a run went: its last attempt, with links. Never a row value. */
export interface WorkflowStepRun {
  step_id: string
  type: StepType
  attempt: number
  /** "waiting": its pipeline or model was busy with another run. "warned": a warn check failed, but the step passed. */
  status: StepRunStatus
  /** run_pipeline: the pipeline run (/executions/<id>). */
  execution_id?: string
  pipeline_id?: string
  /** refresh_model: the model run and the model (/models/<id>). */
  model_run_id?: string
  model_id?: string
  row_count?: number
  /** condition: the 0-based condition it took, -1 for none. */
  case?: number
  started_at: string
  finished_at?: string
  error?: string
  note?: string
}

export interface WorkflowRun {
  id: string
  trigger: TriggerType
  status: "running" | "succeeded" | "failed" | "stopped"
  condition_met?: boolean
  /** The 0-based condition that sent; absent when none was true. */
  branch?: number
  row_count?: number
  deliveries: DeliveryOutcome[]
  notes: string[]
  error?: string
  started_at: string
  finished_at?: string
  /** Set when this run ran a definition the workflow has since been edited away from. */
  earlier_version?: boolean
  /** That earlier definition. */
  definition?: WorkflowDefinition
  /** Someone pressed Stop: the run ends at its next step. */
  stop_requested?: boolean
  /** Each step's last attempt; a step not in it has not started. Absent on a run from before steps. */
  steps?: WorkflowStepRun[]
}

export interface Workflow {
  id: string
  name: string
  status: WorkflowStatus
  paused_reason?: string
  definition: WorkflowDefinition
  trigger_description: string
  /** Every branch's condition, joined ("send when …; else send when …"). */
  condition_description: string
  /** Each condition, as the UI says it ("send when …", "otherwise send"); empty without a condition step. */
  branch_descriptions: string[]
  /** The query's connection; null when it was deleted or there is no query. */
  connection: WorkflowRef | null
  pipeline?: WorkflowRef
  channels: WorkflowChannel[]
  /** Each run_pipeline, refresh_model, chart and dashboard step's pipeline, model, chart or dashboard, by step id; missing when it was deleted. */
  step_refs?: Record<string, WorkflowRef>
  next_run_at?: string
  /** Who made it: Ask rsync.ai, an agent, or "none" for a person in the editor. */
  proposed_by: "assistant" | "agent" | "none"
  created_at: string
  updated_at: string
  last_run?: WorkflowRun
  url: string
}

export interface WorkflowTestMessage {
  channel: string
  kind: string
  subject?: string
  body: string
  /** The send step whose message this is. */
  step: string
  /** The 0-based condition whose message this is; absent on a send that follows no condition. */
  branch?: number
}

export interface WorkflowTestResult {
  /** The query's result; absent (columns and rows empty) when the workflow has no query. */
  columns: string[]
  rows: Record<string, unknown>[]
  row_count?: number
  truncated?: boolean
  execution_time_ms?: number
  condition_met: boolean
  condition_error?: string
  /** The 0-based condition a run now would send; absent when none is true. */
  branch?: number
  value?: number
  /** Every send's message, to each of that send's channels. */
  messages: WorkflowTestMessage[]
  notes: string[]
}

export interface DeliveryChannel {
  id: string
  name: string
  kind: "slack" | "email"
  recipient_count: number
  recipients?: string[]
  used_by: string[]
  created_at: string
  updated_at: string
}

export class WorkflowError extends Error {
  constructor(
    message: string,
    public status: number,
    /** The refusal's code ("invalid_sql", "channel_not_found"…): which field is at fault. */
    public code?: string,
    /** The 1-based branch the refusal is about, when it is about one. */
    public branch?: number,
    /** The id of the step the refusal is about, when it is about one. */
    public step?: string,
  ) {
    super(message)
    this.name = "WorkflowError"
  }
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as {
      error?: unknown
      message?: unknown
      code?: unknown
      branch?: unknown
      step?: unknown
    } | null
    // A refusal says it in error, with its code apart; a few (Stop) give the
    // code in error and say it in message.
    const said = typeof body?.message === "string" && body.message ? body.message : undefined
    const error = typeof body?.error === "string" && body.error ? body.error : undefined
    const message = said ?? error ?? fallback
    const code = typeof body?.code === "string" && body.code ? body.code : said ? error : undefined
    const branch = typeof body?.branch === "number" ? body.branch : undefined
    const step = typeof body?.step === "string" && body.step ? body.step : undefined
    throw new WorkflowError(
      res.status === 404 && !body?.error ? "Not found in this workspace." : message,
      res.status,
      code,
      branch,
      step,
    )
  }
  return res.json() as Promise<T>
}

function post(url: string, body?: unknown, timeoutMs = 30_000): Promise<Response> {
  return authFetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body ?? {}),
    timeoutMs,
  })
}

export async function listWorkflows(): Promise<{ workflows: Workflow[]; can_manage: boolean }> {
  const data = await readJSON<{ workflows?: Workflow[]; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.WORKFLOWS.LIST),
    "Could not load workflows",
  )
  return { workflows: Array.isArray(data.workflows) ? data.workflows : [], can_manage: !!data.can_manage }
}

export async function getWorkflow(id: string): Promise<{ workflow: Workflow; can_manage: boolean }> {
  const data = await readJSON<{ workflow?: Workflow; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.WORKFLOWS.GET(id)),
    "Could not load the workflow",
  )
  if (!data.workflow || typeof data.workflow.id !== "string") {
    throw new WorkflowError("Unexpected workflow response", 500)
  }
  return { workflow: data.workflow, can_manage: !!data.can_manage }
}

export async function listWorkflowRuns(id: string): Promise<WorkflowRun[]> {
  const data = await readJSON<{ runs?: WorkflowRun[] }>(
    await authFetch(API_ENDPOINTS.WORKFLOWS.RUNS(id)),
    "Could not load the runs",
  )
  return Array.isArray(data.runs) ? data.runs : []
}

/**
 * Asks a running run to stop: no new step starts, a pipeline run a step started is
 * stopped too, and a model run in flight is left to finish.
 */
export async function stopWorkflowRun(id: string, runId: string): Promise<void> {
  await readJSON(await post(API_ENDPOINTS.WORKFLOWS.STOP(id, runId)), "Could not stop the run")
}

/** Runs the query and renders each message for the person to see. Nothing is sent and no step runs. */
export async function testWorkflow(id: string): Promise<WorkflowTestResult> {
  const data = await readJSON<WorkflowTestResult>(
    await post(API_ENDPOINTS.WORKFLOWS.TEST(id), undefined, 120_000),
    "Could not test the workflow",
  )
  return {
    ...data,
    columns: Array.isArray(data.columns) ? data.columns : [],
    rows: Array.isArray(data.rows) ? data.rows : [],
    messages: Array.isArray(data.messages) ? data.messages : [],
    notes: Array.isArray(data.notes) ? data.notes : [],
  }
}

export interface WorkflowCreate {
  name: string
  definition: WorkflowDefinition
  /** Each send step's channel ids, by step id. */
  node_channel_ids: Record<string, string[]>
}

/**
 * Saves a workflow a person built in the editor, as a draft: a query, its
 * conditions and a message for each. The server checks it as it checks a
 * draft Ask rsync.ai made, running the query once; query_checked is false
 * when the query was still running at the check's deadline. Nothing is sent.
 */
export async function createWorkflow(input: WorkflowCreate): Promise<{ workflow: Workflow; query_checked: boolean }> {
  const data = await readJSON<{ workflow?: Workflow; query_checked?: boolean }>(
    await post(API_ENDPOINTS.WORKFLOWS.LIST, input, 120_000),
    "Could not save the workflow",
  )
  if (!data.workflow || typeof data.workflow.id !== "string") {
    throw new WorkflowError("Unexpected workflow response", 500)
  }
  return { workflow: data.workflow, query_checked: data.query_checked === true }
}

export interface WorkflowUpdate {
  name: string
  definition: WorkflowDefinition
  /** Each send step's channel ids, by step id. */
  node_channel_ids: Record<string, string[]>
  /** The updated_at the edit started from: the save is refused (409) if the workflow changed since. */
  updated_at: string
}

/**
 * Saves an edit. The server checks it as it checks a draft, running the query
 * once (when it has one); query_checked is false when the query was still running at the check's
 * deadline, so only Test has run it in full. Nothing is sent.
 */
export async function updateWorkflow(
  id: string,
  input: WorkflowUpdate,
): Promise<{ workflow: Workflow; query_checked: boolean }> {
  const data = await readJSON<{ workflow?: Workflow; query_checked?: boolean }>(
    await authFetch(API_ENDPOINTS.WORKFLOWS.GET(id), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
      timeoutMs: 120_000,
    }),
    "Could not save the workflow",
  )
  if (!data.workflow || typeof data.workflow.id !== "string") {
    throw new WorkflowError("Unexpected workflow response", 500)
  }
  return { workflow: data.workflow, query_checked: data.query_checked === true }
}

export type WorkflowChangeStatus = "proposed" | "applied" | "expired" | "stale"

/**
 * A change Ask rsync.ai proposed to a saved workflow (plan_workflow_change):
 * the workflow as it is and as it would be. Only the person it was proposed
 * for reads it; an owner or admin applies it.
 */
export interface WorkflowChange {
  id: string
  workflow_id: string
  workflow_name: string
  summary: string
  status: WorkflowChangeStatus
  proposed_by: "assistant" | "agent"
  current: Workflow
  proposed: Workflow
  created_at: string
  expires_at: string
  url: string
}

export async function getWorkflowChange(id: string): Promise<{ change: WorkflowChange; can_manage: boolean }> {
  const data = await readJSON<{ change?: WorkflowChange; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.WORKFLOW_CHANGES.GET(id)),
    "Could not load the proposed change",
  )
  if (!data.change || typeof data.change.id !== "string") {
    throw new WorkflowError("Unexpected workflow change response", 500)
  }
  return { change: data.change, can_manage: !!data.can_manage }
}

/**
 * Applies a proposed change: saved as an edit, checked as an edit is, and
 * refused when the workflow was saved since the change was proposed.
 */
export async function applyWorkflowChange(id: string): Promise<{ workflow: Workflow; query_checked: boolean }> {
  const data = await readJSON<{ workflow?: Workflow; query_checked?: boolean }>(
    await post(API_ENDPOINTS.WORKFLOW_CHANGES.APPLY(id), undefined, 120_000),
    "Could not apply the change",
  )
  if (!data.workflow || typeof data.workflow.id !== "string") {
    throw new WorkflowError("Unexpected workflow response", 500)
  }
  return { workflow: data.workflow, query_checked: data.query_checked === true }
}

export async function activateWorkflow(id: string): Promise<Workflow> {
  const data = await readJSON<{ workflow: Workflow }>(
    await post(API_ENDPOINTS.WORKFLOWS.ACTIVATE(id)),
    "Could not turn the workflow on",
  )
  return data.workflow
}

export async function pauseWorkflow(id: string): Promise<void> {
  await readJSON(await post(API_ENDPOINTS.WORKFLOWS.PAUSE(id)), "Could not pause the workflow")
}

export async function runWorkflowNow(id: string): Promise<string> {
  const data = await readJSON<{ run_id?: string }>(await post(API_ENDPOINTS.WORKFLOWS.RUN(id)), "Could not start a run")
  return data.run_id ?? ""
}

export async function deleteWorkflow(id: string): Promise<void> {
  await readJSON(await authFetch(API_ENDPOINTS.WORKFLOWS.GET(id), { method: "DELETE" }), "Could not delete the workflow")
}

export async function listDeliveryChannels(): Promise<{ channels: DeliveryChannel[]; can_manage: boolean }> {
  const data = await readJSON<{ channels?: DeliveryChannel[]; can_manage?: boolean }>(
    await authFetch(API_ENDPOINTS.DELIVERY_CHANNELS.LIST),
    "Could not load delivery channels",
  )
  return { channels: Array.isArray(data.channels) ? data.channels : [], can_manage: !!data.can_manage }
}

export interface DeliveryChannelInput {
  name: string
  kind: "slack" | "email"
  webhook_url?: string
  recipients?: string[]
}

export async function createDeliveryChannel(input: DeliveryChannelInput): Promise<string> {
  const data = await readJSON<{ id?: string }>(
    await post(API_ENDPOINTS.DELIVERY_CHANNELS.LIST, input),
    "Could not add the channel",
  )
  return data.id ?? ""
}

export async function deleteDeliveryChannel(id: string): Promise<void> {
  await readJSON(
    await authFetch(API_ENDPOINTS.DELIVERY_CHANNELS.ITEM(id), { method: "DELETE" }),
    "Could not delete the channel",
  )
}

export async function testDeliveryChannel(id: string): Promise<void> {
  await readJSON(await post(API_ENDPOINTS.DELIVERY_CHANNELS.TEST(id), undefined, 60_000), "The test message failed")
}

/** Splits a typed list of addresses on commas, semicolons, spaces and new lines. */
export function parseRecipients(text: string): string[] {
  return text
    .split(/[\s,;]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

/** A workflow status as the UI says it. */
export function statusLabel(status: WorkflowStatus): string {
  return status === "active" ? "On" : status === "paused" ? "Paused" : "Draft"
}
