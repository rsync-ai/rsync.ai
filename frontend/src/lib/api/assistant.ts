/**
 * The in-app assistant ("Ask rsync.ai"). api-gateway internal/assistant:
 *   POST /assistant/messages  { question, conversation_id?, page?, model?, request_id? } -> AssistantAnswer
 *   POST /assistant/messages/:request_id/cancel  -> 204 (Stop)
 *   GET  /assistant/models    -> { entries: AssistantModel[], default }
 * and, for platform admins (Admin > Settings > AI models):
 *   GET    /admin/assistant/models               -> { entries: AdminModelEntry[], env_known }
 *   PUT    /admin/assistant/models/:entry         { api_key?, model? } -> the same list
 *   DELETE /admin/assistant/models/:entry         -> the same list
 *   POST   /admin/assistant/models/:entry/test    -> AdminModelTest
 *   PUT    /admin/assistant/models/:entry/sample-rows { allowed } -> the same list
 * and, for the active workspace (any member reads, the owner sets):
 *   GET/PUT /workspace/llm-data  { mode } -> WorkspaceLLMData
 *
 * No route ever returns a model key. The conversation lives on the server: the
 * browser sends only the new question and the conversation id.
 */

import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { isNetworkFailure } from "@/lib/errors/network"

/** The picker's entries, in the server's order. */
export type AssistantEntry = "claude" | "chatgpt" | "gemini"

export interface AssistantModel {
  entry: AssistantEntry
  /** The vendor name the picker shows: "Claude", "ChatGPT", "Gemini". */
  label: string
  /** The model id that answers, such as "claude-sonnet-5-5". */
  model: string
}

/** What a question is about: the pipeline, connection, run or dashboard page it was asked on. */
export interface AssistantPage {
  kind: "pipeline" | "connection" | "run" | "dashboard"
  id: string
}

/** One tool the assistant ran to answer. */
export interface LookedAt {
  tool: string
  arguments: Record<string, unknown> | null
  ok: boolean
  /** What a write made, e.g. the plan plan_pipeline proposed. */
  resource_id?: string
  /** A sample_rows call: how many rows it read and how many columns were withheld. Never a value. */
  sample?: { rows: number; withheld_columns: number; not_sent?: boolean }
  /** A generate_sql call's SQL, for the person only: the model is not shown it. */
  sql?: SqlDraft
}

/** SQL Ask rsync.ai wrote; nothing has run. */
export interface SqlDraft {
  sql: string
  connection_id: string
  tables: string[]
}

export interface AssistantAnswer {
  conversation_id: string
  answer: string
  model: { entry: AssistantEntry; label: string; model: string }
  looked_at: LookedAt[]
  /** "limit" when the question hit a limit before the model finished. */
  stopped?: string
  cost_cents: number
}

/** The drafts an answer's plan_workflow calls stored; a refused call stores none. */
export function workflowIds(answer: AssistantAnswer): string[] {
  return answer.looked_at
    .filter((l) => l.tool === "plan_workflow" && l.ok && typeof l.resource_id === "string" && l.resource_id)
    .map((l) => l.resource_id as string)
}

/** A failed call that keeps the server's status and error code. */
export class AssistantError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code: string,
  ) {
    super(message)
    this.name = "AssistantError"
  }
}

async function readJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: unknown; message?: unknown } | null
    const code = typeof body?.error === "string" ? body.error : "http_" + res.status
    const message = typeof body?.message === "string" && body.message ? body.message : fallback
    throw new AssistantError(message, res.status, code)
  }
  return res.json() as Promise<T>
}

export async function listAssistantModels(): Promise<{ entries: AssistantModel[]; default: string }> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.MODELS)
  const data = await readJSON<{ entries?: AssistantModel[]; default?: string }>(
    res,
    "Could not load the assistant's models",
  )
  if (!Array.isArray(data?.entries)) {
    throw new AssistantError("Unexpected models response", res.status, "bad_response")
  }
  return { entries: data.entries, default: data.default ?? "" }
}

export async function askAssistant(input: {
  question: string
  conversationId?: string
  page?: AssistantPage | null
  model?: string
  /** Names the question for cancelAssistant. */
  requestId?: string
  /** Aborting it lets go of the request in the browser; cancelAssistant ends it on the server. */
  signal?: AbortSignal
}): Promise<AssistantAnswer> {
  const body: Record<string, unknown> = { question: input.question }
  if (input.conversationId) body.conversation_id = input.conversationId
  if (input.page) body.page = input.page
  if (input.model) body.model = input.model
  if (input.requestId) body.request_id = input.requestId
  let res: Response
  let data: AssistantAnswer
  try {
    res = await authFetch(API_ENDPOINTS.ASSISTANT.MESSAGES, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: input.signal,
      // The server's own deadline is 120 s; past that the socket is not coming back.
      timeoutMs: 130_000,
    })
    data = await readJSON<AssistantAnswer>(res, "The assistant could not answer")
  } catch (err) {
    // A dropped connection gets a code of its own: the chat says so in words,
    // and a reload can tell whether it was the cause (lib/assistant/session.ts).
    if (isNetworkFailure(err)) throw new AssistantError("The connection to the server dropped", 0, "network")
    throw err
  }
  if (typeof data?.answer !== "string") {
    throw new AssistantError("Unexpected assistant response", res.status, "bad_response")
  }
  return { ...data, looked_at: Array.isArray(data.looked_at) ? data.looked_at : [] }
}

/**
 * Stop on the server: ends the question askAssistant sent with this requestId,
 * on whichever api-gateway replica runs it. A browser abort alone is not
 * enough, since a proxy on the way can keep the request open.
 */
export async function cancelAssistant(requestId: string): Promise<void> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.CANCEL(requestId), { method: "POST" })
  if (!res.ok) await readJSON<unknown>(res, "Could not stop the question")
}

// ---------------------------------------------------------------------------
// Admin > Settings > AI models
// ---------------------------------------------------------------------------

export interface AdminModelEntry {
  entry: AssistantEntry
  label: string
  /** A key is saved in the panel. Never the key itself. */
  panel_key_set: boolean
  /** The panel key's model id; "" means default_model. */
  model: string
  default_model: string
  updated_by?: string
  updated_at?: string
  /** The environment file sets a key for this entry. */
  env_key_set: boolean
  env_model?: string
  /** Where a question's key comes from: the panel wins over the environment. */
  source: "panel" | "env" | "none"
  /** An admin approved sending sample rows to this model. */
  sample_rows?: boolean
}

export interface AdminModelList {
  entries: AdminModelEntry[]
  /** False when llm-service did not answer, so the env columns are unknown. */
  env_known: boolean
  /** False when the sample-rows approvals could not be read: the card shows no switch. */
  sample_rows_known?: boolean
}

export interface AdminModelTest {
  ok: boolean
  entry: string
  label?: string
  model?: string
  source?: string
  error?: string
  message?: string
}

function checkList(data: AdminModelList, status: number): AdminModelList {
  if (!Array.isArray(data?.entries)) {
    throw new AssistantError("Unexpected AI models response", status, "bad_response")
  }
  return data
}

export async function adminListModelKeys(): Promise<AdminModelList> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.ADMIN_MODELS)
  return checkList(await readJSON<AdminModelList>(res, "Could not load the AI models"), res.status)
}

/** Saves a key (and model id), or with no apiKey only the saved key's model id. */
export async function adminSetModelKey(
  entry: AssistantEntry,
  input: { apiKey?: string; model: string },
): Promise<AdminModelList> {
  const body: Record<string, string> = { model: input.model }
  if (input.apiKey !== undefined) body.api_key = input.apiKey
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL(entry), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  })
  return checkList(await readJSON<AdminModelList>(res, "Could not save the key"), res.status)
}

export async function adminRemoveModelKey(entry: AssistantEntry): Promise<AdminModelList> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL(entry), { method: "DELETE" })
  return checkList(await readJSON<AdminModelList>(res, "Could not remove the key"), res.status)
}

export async function adminTestModelKey(entry: AssistantEntry): Promise<AdminModelTest> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL_TEST(entry), {
    method: "POST",
    timeoutMs: 40_000,
  })
  return readJSON<AdminModelTest>(res, "Could not test the key")
}

/** Approves (or withdraws) sending sample rows to this model's provider. */
export async function adminSetSampleRows(entry: AssistantEntry, allowed: boolean): Promise<AdminModelList> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.ADMIN_MODEL_SAMPLE_ROWS(entry), {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ allowed }),
  })
  return checkList(await readJSON<AdminModelList>(res, "Could not save the setting"), res.status)
}

// ---------------------------------------------------------------------------
// Sample rows in the active workspace
// ---------------------------------------------------------------------------

export type LLMDataMode = "names_only" | "sample_rows"

export interface WorkspaceLLMData {
  mode: LLMDataMode
  /** The models an admin approved for sample rows, in the picker's order. */
  approved_models: AssistantEntry[]
  /** The caller owns the workspace, so may change mode. */
  can_edit: boolean
}

function checkLLMData(data: WorkspaceLLMData, status: number): WorkspaceLLMData {
  if (data?.mode !== "names_only" && data?.mode !== "sample_rows") {
    throw new AssistantError("Unexpected setting response", status, "bad_response")
  }
  return { ...data, approved_models: Array.isArray(data.approved_models) ? data.approved_models : [] }
}

/**
 * The workspace routes answer a refusal as {"error": "<sentence>"}, not the
 * assistant's {error: code, message}, so the sentence is in `error`.
 */
async function readSettingJSON<T>(res: Response, fallback: string): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: unknown } | null
    const message = typeof body?.error === "string" && body.error ? body.error : fallback
    throw new AssistantError(message, res.status, "http_" + res.status)
  }
  return res.json() as Promise<T>
}

/** The active workspace's setting, or null when the assistant is off (no route). */
export async function getWorkspaceLLMData(): Promise<WorkspaceLLMData | null> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.WORKSPACE_LLM_DATA, { cache: "no-store" })
  if (res.status === 404) return null
  return checkLLMData(await readSettingJSON<WorkspaceLLMData>(res, "Could not load the setting"), res.status)
}

export async function setWorkspaceLLMData(mode: LLMDataMode): Promise<WorkspaceLLMData> {
  const res = await authFetch(API_ENDPOINTS.ASSISTANT.WORKSPACE_LLM_DATA, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode }),
  })
  return checkLLMData(await readSettingJSON<WorkspaceLLMData>(res, "Could not save the setting"), res.status)
}

/** "Claude, ChatGPT" for the models the picker calls them. */
export function modelNames(entries: readonly string[]): string {
  const names: Record<string, string> = { claude: "Claude", chatgpt: "ChatGPT", gemini: "Gemini" }
  return entries.map((e) => names[e] ?? e).join(", ")
}
