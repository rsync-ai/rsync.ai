/**
 * A model's data checks: the shapes the Checks API returns, the words the Checks tab and
 * the Runs views use for them, and the add/edit form's state.
 *
 * The rules here mirror the gateway's (saved_query_checks.go normalizeModelCheck), so the
 * form can say what is wrong before a round trip; the gateway still decides. The one rule
 * the form cannot mirror is custom_sql's "one read-only SELECT", which needs the parser.
 */

import { displayConnectorName } from "@/lib/connector-display"

export type CheckType = "row_count" | "row_count_change" | "not_null" | "unique" | "custom_sql"
export type CheckSeverity = "block" | "warn"
export type CheckStatus = "pass" | "fail" | "error"

export interface CheckParams {
  min?: number
  max?: number
  max_drop_pct?: number
  max_growth_pct?: number
}

/** What one check found on one run, newest first in a check's history. */
export interface CheckHistoryEntry {
  run_id: string
  finished_at: string
  status: CheckStatus
  severity: CheckSeverity
  observed?: number
  threshold?: string
  message?: string
}

/** One check as GET /explorer/saved/:id/checks returns it. */
export interface ModelCheck {
  check_id: string
  name: string
  type: CheckType
  columns: string[]
  params: CheckParams
  sql_text?: string
  severity: CheckSeverity
  enabled: boolean
  history: CheckHistoryEntry[]
}

/** A custom_sql check waiting for an admin, on a scheduled model. */
export interface CheckProposal {
  id: string
  /** Set when the proposal edits an existing check; absent for a new one. */
  check_id?: string
  sql_text: string
  base_sql_text: string
  check: {
    name: string
    type: CheckType
    columns: string[]
    params: CheckParams
    severity: CheckSeverity
    enabled: boolean
  }
  note?: string
  proposed_by: string
  proposed_at: string
}

export interface ModelChecksResponse {
  checks: ModelCheck[]
  pending: CheckProposal[]
  max_checks: number
  can_block: boolean
  /** Why this model cannot have checks at all (a statement model, a document database…). */
  unsupported_reason?: string
  /** Why its checks can only warn (an engine that replaces the table in one statement). */
  cannot_block_reason?: string
  /** With cannot_block_reason: the connection's type, so the tab can name the engine. */
  connector_type?: string
}

/** One check's result: a run's (GET …/runs/:runId/checks) or Test now's. */
export interface CheckResult {
  check_id?: string
  name: string
  type: CheckType
  /** What the check did: a blocking check on an engine that cannot block ran as a warning. */
  severity: CheckSeverity
  status: CheckStatus
  observed?: number
  threshold?: string
  message?: string
}

/**
 * The whole of a blocked run's error (modelCheckBlockedError, saved_query_checks.go). A
 * blocked run is stored as failed; this sentence is how every view tells it from a
 * broken one. A test reads the Go constant so the two cannot drift.
 */
export const MODEL_CHECK_BLOCKED_ERROR = "Blocked by a data check — see the Checks tab"

export function isCheckBlocked(run: { status?: string; error?: string | null }): boolean {
  return run.status === "failed" && run.error === MODEL_CHECK_BLOCKED_ERROR
}

export const CHECK_TYPES: { value: CheckType; label: string; hint: string }[] = [
  { value: "row_count", label: "Row count", hint: "The table has at least a number of rows, and at most another." },
  {
    value: "row_count_change",
    label: "Row count change",
    hint: "The row count did not fall, or grow, by more than a percentage since the last published run.",
  },
  { value: "not_null", label: "Not null", hint: "A column has no NULL." },
  { value: "unique", label: "Unique", hint: "A column, or a combination of columns, has no value twice. NULLs are ignored." },
  {
    value: "custom_sql",
    label: "Custom SQL",
    hint: "A query that returns the rows breaking a rule. The check fails when it returns any row.",
  },
]

/** "a check needs a name" → "A check needs a name": the gateway's words, shown as a sentence. */
export function asSentence(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}

export function checkTypeLabel(type: string): string {
  return CHECK_TYPES.find((t) => t.value === type)?.label ?? type
}

/** The gateway's default (defaultCheckSeverity): invariants block, a count's movement warns. */
export function defaultSeverity(type: CheckType): CheckSeverity {
  return type === "row_count_change" ? "warn" : "block"
}

function plural(n: number, one: string, many: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`
}

/** "At least 1 row", "order_id has no NULLs", "Query returns no rows". */
export function describeCheckSettings(c: { type: string; columns?: string[]; params?: CheckParams }): string {
  const p = c.params ?? {}
  const cols = c.columns ?? []
  switch (c.type) {
    case "row_count": {
      const s = `At least ${plural(p.min ?? 1, "row", "rows")}`
      return p.max != null ? `${s}, at most ${p.max.toLocaleString()}` : s
    }
    case "row_count_change": {
      const parts: string[] = []
      if (p.max_drop_pct != null) parts.push(`falls at most ${p.max_drop_pct}%`)
      if (p.max_growth_pct != null) parts.push(`grows at most ${p.max_growth_pct}%`)
      return parts.length ? `Count ${parts.join(", ")}` : "Count change"
    }
    case "not_null":
      return `${cols[0] ?? "column"} has no NULLs`
    case "unique":
      return cols.length > 1 ? `${cols.join(", ")} unique together` : `${cols[0] ?? "column"} is unique`
    case "custom_sql":
      return "Query returns no rows"
    default:
      return c.type
  }
}

/**
 * What a check counted, in words: observed means a different thing per type
 * (judgeCheckCount) — the table's rows for the two count checks, the rows breaking
 * the rule for the rest.
 */
export function describeObserved(r: { type: string; observed?: number }): string {
  if (r.observed == null) return ""
  const n = r.observed
  switch (r.type) {
    case "row_count":
    case "row_count_change":
      return plural(n, "row", "rows")
    case "not_null":
      return plural(n, "NULL row", "NULL rows")
    case "unique":
      return plural(n, "duplicate row", "duplicate rows")
    case "custom_sql":
      return plural(n, "row returned", "rows returned")
    default:
      return n.toLocaleString()
  }
}

export const CHECK_STATUS_LABEL: Record<CheckStatus, string> = {
  pass: "Passed",
  fail: "Failed",
  error: "Could not run",
}

// ---------------------------------------------------------------------------
// The add/edit form
// ---------------------------------------------------------------------------

/** The form's state. Numbers stay text until saved, so "no max" differs from a max of 0. */
export interface CheckDraft {
  name: string
  type: CheckType
  columns: string[]
  min: string
  max: string
  max_drop_pct: string
  max_growth_pct: string
  sql_text: string
  severity: CheckSeverity
  enabled: boolean
  note: string
}

/** What POST and PUT …/checks take. */
export interface CheckRequestBody {
  name: string
  type: CheckType
  columns: string[]
  params: CheckParams
  sql_text?: string
  severity: CheckSeverity
  enabled: boolean
  note?: string
}

export const CHECK_THIS_PLACEHOLDER = /\{\{\s*this\s*\}\}/

const MAX_CHECK_NAME = 100
const MAX_CHECK_COLUMN = 128
const MAX_UNIQUE_COLUMNS = 10
const MAX_CHECK_SQL_BYTES = 20000
// The gateway refuses a block it cannot hold back with this sentence; the form shows it too.
export const CANNOT_BLOCK_MESSAGE = "this engine can't hold a table back yet — add it as a warning"

/**
 * Why a model's checks can only warn, naming the engine when the gateway says which
 * (modelCheckCanBlock: BigQuery, Snowflake and the other warehouses, and an owned
 * ClickHouse rebuild, replace the table in one statement, so there is no point to stop at).
 */
export function cannotBlockNotice(connectorType?: string): string {
  const engine = displayConnectorName(connectorType)
  return (
    `${engine ? `On ${engine}, checks` : "Checks on this model"} can only warn: a run replaces the table in one step, ` +
    "so a failing check can't keep the old one. The new table goes live and the check raises an alert."
  )
}

export function emptyDraft(canBlock: boolean, type: CheckType = "row_count"): CheckDraft {
  return {
    name: "",
    type,
    columns: [],
    min: "1",
    max: "",
    max_drop_pct: type === "row_count_change" ? "50" : "",
    max_growth_pct: "",
    sql_text: type === "custom_sql" ? "SELECT *\nFROM {{this}}\nWHERE " : "",
    severity: canBlock ? defaultSeverity(type) : "warn",
    enabled: true,
    note: "",
  }
}

/**
 * The draft after the person picks another type: the type's own defaults, keeping the
 * name and, where the new type takes columns, the columns.
 */
export function changeDraftType(d: CheckDraft, type: CheckType, canBlock: boolean): CheckDraft {
  const next = emptyDraft(canBlock, type)
  next.name = d.name
  next.enabled = d.enabled
  next.note = d.note
  if (type === "unique") next.columns = d.columns.slice(0, MAX_UNIQUE_COLUMNS)
  if (type === "not_null") next.columns = d.columns.slice(0, 1)
  return next
}

function numText(n: number | undefined): string {
  return n == null ? "" : String(n)
}

export function draftFromCheck(c: {
  name: string
  type: CheckType
  columns: string[]
  params: CheckParams
  sql_text?: string
  severity: CheckSeverity
  enabled: boolean
}): CheckDraft {
  return {
    name: c.name,
    type: c.type,
    columns: [...(c.columns ?? [])],
    min: numText(c.params?.min),
    max: numText(c.params?.max),
    max_drop_pct: numText(c.params?.max_drop_pct),
    max_growth_pct: numText(c.params?.max_growth_pct),
    sql_text: c.sql_text ?? "",
    severity: c.severity,
    enabled: c.enabled,
    note: "",
  }
}

function hasControlChar(s: string): boolean {
  // Go's unicode.IsControl: C0, DEL and C1.
  return /[\u0000-\u001f\u007f-\u009f]/.test(s)
}

function wholeNumber(s: string): number | null {
  return /^-?\d+$/.test(s) ? Number(s) : null
}

function percent(s: string): number | null {
  const t = s.trim().replace(/%$/, "").trim()
  if (t === "" || !/^-?(\d+\.?\d*|\.\d+)$/.test(t)) return null
  return Number(t)
}

export type DraftVerdict = { ok: true; body: CheckRequestBody } | { ok: false; error: string }

/**
 * Checks a draft as the gateway would and builds the request body. An error names a
 * field as the form labels it (“At least”), not by its API parameter (min).
 */
export function validateDraft(d: CheckDraft, canBlock: boolean): DraftVerdict {
  const fail = (error: string): DraftVerdict => ({ ok: false, error })
  const name = d.name.trim()
  if (!name) return fail("a check needs a name")
  if (Array.from(name).length > MAX_CHECK_NAME) return fail(`a check's name can be at most ${MAX_CHECK_NAME} characters`)
  if (hasControlChar(name)) return fail("a check's name cannot contain control characters")

  const params: CheckParams = {}
  let wantCols: "none" | "one" | "some" = "none"
  let sqlText: string | undefined
  switch (d.type) {
    case "row_count": {
      const minText = d.min.trim()
      const min = minText === "" ? 1 : wholeNumber(minText)
      if (min == null) return fail("“At least” must be a whole number, such as 1")
      if (min < 0) return fail("“At least” cannot be negative")
      params.min = min
      const maxText = d.max.trim()
      if (maxText !== "") {
        const max = wholeNumber(maxText)
        if (max == null) return fail("“At most” must be a whole number")
        if (max < min) return fail("“At most” cannot be below “At least”")
        params.max = max
      }
      break
    }
    case "row_count_change": {
      if (d.max_drop_pct.trim() === "") return fail("a row count change check needs the largest drop")
      const drop = percent(d.max_drop_pct)
      if (drop == null) return fail("the largest drop must be a number, such as 50")
      if (drop < 0 || drop > 100) return fail("the largest drop must be between 0 and 100")
      params.max_drop_pct = drop
      if (d.max_growth_pct.trim() !== "") {
        const growth = percent(d.max_growth_pct)
        if (growth == null) return fail("the largest growth must be a number, such as 200")
        if (growth < 0) return fail("the largest growth cannot be negative")
        params.max_growth_pct = growth
      }
      break
    }
    case "not_null":
      wantCols = "one"
      break
    case "unique":
      wantCols = "some"
      break
    case "custom_sql":
      sqlText = d.sql_text.trim()
      if (!sqlText) return fail("a custom SQL check needs its query")
      if (new TextEncoder().encode(sqlText).length > MAX_CHECK_SQL_BYTES)
        return fail(`a custom SQL check's query can be at most ${MAX_CHECK_SQL_BYTES} bytes`)
      if (!CHECK_THIS_PLACEHOLDER.test(sqlText)) return fail("the query must name the model's table as {{this}}")
      break
    default:
      return fail(`unknown check type "${d.type as string}"`)
  }

  const columns = wantCols === "none" ? [] : d.columns
  if (wantCols === "one" && columns.length !== 1) return fail("a not null check takes exactly one column")
  if (wantCols === "some" && (columns.length < 1 || columns.length > MAX_UNIQUE_COLUMNS))
    return fail(`a unique check takes 1 to ${MAX_UNIQUE_COLUMNS} columns`)
  const seen = new Set<string>()
  for (const col of columns) {
    if (col === "" || col.trim() !== col) return fail("a column name cannot be blank or start or end with a space")
    if (Array.from(col).length > MAX_CHECK_COLUMN) return fail(`a column name can be at most ${MAX_CHECK_COLUMN} characters`)
    if (hasControlChar(col)) return fail("a column name cannot contain control characters")
    if (seen.has(col)) return fail(`column "${col}" is listed twice`)
    seen.add(col)
  }

  if (d.severity !== "block" && d.severity !== "warn") return fail("“When it fails” must be Block or Warn")
  if (d.severity === "block" && !canBlock) return fail(CANNOT_BLOCK_MESSAGE)

  const body: CheckRequestBody = {
    name,
    type: d.type,
    columns: [...columns],
    params,
    severity: d.severity,
    enabled: d.enabled,
  }
  if (sqlText !== undefined) body.sql_text = sqlText
  if (d.note.trim()) body.note = d.note.trim()
  return { ok: true, body }
}

// ---------------------------------------------------------------------------
// A draft from Ask rsync.ai (plan_model_checks)
// ---------------------------------------------------------------------------

// The agent gateway's synonyms (agentgw/model_checks.go): a looked-at call keeps the
// model's own arguments, so its words arrive here unmapped.
const TYPE_SYNONYMS: Record<string, CheckType> = {
  count: "row_count",
  rows: "row_count",
  min_rows: "row_count",
  not_empty: "row_count",
  row_change: "row_count_change",
  volume: "row_count_change",
  count_change: "row_count_change",
  row_count_drop: "row_count_change",
  notnull: "not_null",
  required: "not_null",
  no_nulls: "not_null",
  non_null: "not_null",
  distinct: "unique",
  uniqueness: "unique",
  no_duplicates: "unique",
  sql: "custom_sql",
  custom: "custom_sql",
  query: "custom_sql",
  assertion: "custom_sql",
}
const SEVERITY_SYNONYMS: Record<string, CheckSeverity> = {
  blocking: "block",
  error: "block",
  fail: "block",
  hard: "block",
  warning: "warn",
  soft: "warn",
  alert: "warn",
}

// enumKey: lower case, spaces, hyphens and underscores folded to one underscore.
function enumKey(s: string): string {
  return s
    .toLowerCase()
    .split(/[ \-_]+/)
    .filter(Boolean)
    .join("_")
}

function argText(v: unknown): string {
  if (typeof v === "string") return v.trim()
  if (typeof v === "number" && Number.isFinite(v)) return String(v)
  return ""
}

function checkTypeOf(v: unknown): CheckType | null {
  const k = enumKey(argText(v))
  if (CHECK_TYPES.some((t) => t.value === k)) return k as CheckType
  return TYPE_SYNONYMS[k] ?? null
}

function severityOf(v: unknown): CheckSeverity | null {
  const k = enumKey(argText(v))
  if (k === "block" || k === "warn") return k
  return SEVERITY_SYNONYMS[k] ?? null
}

export interface AssistantChecksDraft {
  modelId: string
  drafts: CheckDraft[]
}

/**
 * The checks a successful plan_model_checks call drafted, as the form's state. null when
 * the arguments hold none. The gateway accepted these, so a check it could not read is
 * skipped rather than guessed at.
 */
export function draftsFromAssistant(
  args: Record<string, unknown> | null | undefined,
  canBlock = true,
): AssistantChecksDraft | null {
  if (!args) return null
  const modelId = argText(args.model_id)
  let items: unknown = args.checks
  if (typeof items === "string") {
    try {
      items = JSON.parse(items)
    } catch {
      return null
    }
  }
  if (!modelId || !Array.isArray(items)) return null
  const drafts: CheckDraft[] = []
  for (const raw of items) {
    if (!raw || typeof raw !== "object") continue
    const it = raw as Record<string, unknown>
    const type = checkTypeOf(it.type)
    if (!type) continue
    const d = emptyDraft(canBlock, type)
    d.name = argText(it.name)
    d.columns = argText(it.columns)
      .split(",")
      .map((c) => c.trim())
      .filter(Boolean)
    d.min = argText(it.min) || d.min
    d.max = argText(it.max)
    d.max_drop_pct = argText(it.max_drop_pct) || d.max_drop_pct
    d.max_growth_pct = argText(it.max_growth_pct)
    if (type === "custom_sql") d.sql_text = typeof it.sql === "string" ? it.sql.trim() : ""
    const sev = severityOf(it.severity)
    if (sev) d.severity = canBlock ? sev : "warn"
    drafts.push(d)
  }
  return drafts.length ? { modelId, drafts } : null
}

// The card hands the draft to the model page through sessionStorage, keyed by model, so a
// reload of the page does not lose it and another model's page never picks it up.
const DRAFT_KEY_PREFIX = "rsync.checks-draft."

/** Fired with the model id once a draft is stored, for a Checks tab already open on that model. */
export const ASSISTANT_DRAFT_EVENT = "rsync:checks-draft"

export function storeAssistantDraft(draft: AssistantChecksDraft): void {
  try {
    sessionStorage.setItem(DRAFT_KEY_PREFIX + draft.modelId, JSON.stringify(draft.drafts))
  } catch {
    // Storage blocked: the person opens the tab without the draft filled in.
    return
  }
  window.dispatchEvent(new CustomEvent<string>(ASSISTANT_DRAFT_EVENT, { detail: draft.modelId }))
}

/** The draft waiting for this model, removed as it is read. */
export function takeAssistantDraft(modelId: string): CheckDraft[] | null {
  try {
    const key = DRAFT_KEY_PREFIX + modelId
    const raw = sessionStorage.getItem(key)
    if (raw == null) return null
    sessionStorage.removeItem(key)
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) && parsed.length ? (parsed as CheckDraft[]) : null
  } catch {
    return null
  }
}
