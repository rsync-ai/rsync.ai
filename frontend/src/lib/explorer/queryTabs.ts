/**
 * The Explorer's query tabs, the hand-off that opens SQL in a new one, and the
 * per-connection History list.
 *
 * dashboards-design.md § 7.5: up to 10 tabs per person, each with its own
 * connection and SQL. Kept in the browser, per user and workspace: a tab's
 * name, connection and SQL, never its results. They do not follow a person to
 * another browser. Everything here is pure or touches only Web Storage, so the
 * rules are tested without rendering the page.
 */

export const MAX_QUERY_TABS = 10

export interface QueryTab {
  id: string
  name: string
  connectionId: string
  sql: string
}

export interface QueryTabsState {
  active: string
  tabs: QueryTab[]
}

const TABS_KEY_PREFIX = "rsync_explorer_tabs_v1"

/** The storage key for one person in one workspace, or null until both are known. */
export function queryTabsKey(userId: string | undefined | null, workspaceId: string | undefined | null): string | null {
  if (!userId || !workspaceId) return null
  return `${TABS_KEY_PREFIX}:${userId}:${workspaceId}`
}

function newTabId(): string {
  const uuid = (globalThis.crypto as { randomUUID?: () => string } | undefined)?.randomUUID?.()
  return uuid || `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
}

/** "Query N" with the lowest N no open tab already uses. */
export function newTabName(tabs: QueryTab[]): string {
  const used = new Set(tabs.map((t) => t.name))
  let n = 1
  while (used.has(`Query ${n}`)) n++
  return `Query ${n}`
}

export function createTab(tabs: QueryTab[], init?: Partial<Omit<QueryTab, "id">>): QueryTab {
  return {
    id: newTabId(),
    name: init?.name?.trim() || newTabName(tabs),
    connectionId: init?.connectionId ?? "",
    sql: init?.sql ?? "",
  }
}

export function initialTabsState(connectionId = ""): QueryTabsState {
  const tab = createTab([], { connectionId })
  return { active: tab.id, tabs: [tab] }
}

/** Opens a tab and makes it active, or returns null when MAX_QUERY_TABS are already open. */
export function addTab(state: QueryTabsState, init?: Partial<Omit<QueryTab, "id">>): QueryTabsState | null {
  if (state.tabs.length >= MAX_QUERY_TABS) return null
  const tab = createTab(state.tabs, init)
  return { active: tab.id, tabs: [...state.tabs, tab] }
}

/**
 * Closes a tab. The neighbour to its left takes focus if it was active (the
 * right one when it was first). Closing the last tab leaves one empty tab on
 * the same connection, never an Explorer with nowhere to type.
 */
export function closeTab(state: QueryTabsState, id: string): QueryTabsState {
  const idx = state.tabs.findIndex((t) => t.id === id)
  if (idx === -1) return state
  if (state.tabs.length === 1) return initialTabsState(state.tabs[0].connectionId)
  const tabs = state.tabs.filter((t) => t.id !== id)
  if (state.active !== id) return { ...state, tabs }
  const next = tabs[Math.max(0, idx - 1)]
  return { active: next.id, tabs }
}

export function updateTab(
  state: QueryTabsState,
  id: string,
  patch: Partial<Omit<QueryTab, "id">>,
): QueryTabsState {
  let changed = false
  const tabs = state.tabs.map((t) => {
    if (t.id !== id) return t
    const next = { ...t, ...patch }
    if (next.name === t.name && next.connectionId === t.connectionId && next.sql === t.sql) return t
    changed = true
    return next
  })
  return changed ? { ...state, tabs } : state
}

/** A blank or whitespace name keeps the old one. */
export function renameTab(state: QueryTabsState, id: string, name: string): QueryTabsState {
  const trimmed = name.trim().slice(0, 60)
  if (!trimmed) return state
  return updateTab(state, id, { name: trimmed })
}

function storage(kind: "localStorage" | "sessionStorage"): Storage | null {
  try {
    return typeof window === "undefined" ? null : window[kind]
  } catch {
    return null // Storage blocked (private window, sandboxed frame).
  }
}

function isTab(v: unknown): v is QueryTab {
  if (!v || typeof v !== "object") return false
  const t = v as Record<string, unknown>
  return (
    typeof t.id === "string" && t.id !== "" &&
    typeof t.name === "string" &&
    typeof t.connectionId === "string" &&
    typeof t.sql === "string"
  )
}

/** The tabs stored under `key`, or null when there are none (or they do not parse). */
export function loadTabs(key: string): QueryTabsState | null {
  const raw = storage("localStorage")?.getItem(key)
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as { active?: unknown; tabs?: unknown }
    if (!Array.isArray(parsed.tabs)) return null
    const tabs = parsed.tabs
      .filter(isTab)
      .slice(0, MAX_QUERY_TABS)
      .map((t) => ({ id: t.id, name: t.name || "Query", connectionId: t.connectionId, sql: t.sql }))
    if (tabs.length === 0) return null
    const active = tabs.some((t) => t.id === parsed.active) ? (parsed.active as string) : tabs[0].id
    return { active, tabs }
  } catch {
    return null
  }
}

/**
 * Writes the tabs under `key`. Only a tab's id, name, connection and SQL are
 * written, whatever else the caller's objects carry: a result is a copy of
 * customer data and has no business in the browser's storage.
 */
export function saveTabs(key: string, state: QueryTabsState): void {
  const tabs = state.tabs.map(({ id, name, connectionId, sql }) => ({ id, name, connectionId, sql }))
  try {
    storage("localStorage")?.setItem(key, JSON.stringify({ active: state.active, tabs }))
  } catch {
    // Quota or storage blocked: the tabs still work for this page load.
  }
}

// ── Open in Explorer ──────────────────────────────────────────────────────────
// Ask rsync.ai hands SQL to the Explorer through sessionStorage (it survives the
// navigation) plus an event (for an Explorer that is already on screen). The
// Explorer takes the draft once, so the two never open it twice.

export const EXPLORER_OPEN_SQL_EVENT = "rsync:explorer-open-sql"
const OPEN_SQL_KEY = "rsync_explorer_open_sql_v1"

export interface ExplorerSqlDraft {
  sql: string
  connectionId?: string
  name?: string
}

export function openSqlInExplorer(draft: ExplorerSqlDraft): void {
  try {
    storage("sessionStorage")?.setItem(OPEN_SQL_KEY, JSON.stringify(draft))
  } catch {
    return
  }
  if (typeof window !== "undefined") window.dispatchEvent(new Event(EXPLORER_OPEN_SQL_EVENT))
}

/** The pending draft, removed as it is read. */
export function takeExplorerSql(): ExplorerSqlDraft | null {
  const store = storage("sessionStorage")
  const raw = store?.getItem(OPEN_SQL_KEY)
  if (!raw) return null
  try {
    store?.removeItem(OPEN_SQL_KEY)
  } catch {
    // Read-only storage: the draft stays and opens once more on the next visit.
  }
  try {
    const d = JSON.parse(raw) as Record<string, unknown>
    if (typeof d.sql !== "string" || !d.sql.trim()) return null
    return {
      sql: d.sql,
      connectionId: typeof d.connectionId === "string" ? d.connectionId : undefined,
      name: typeof d.name === "string" ? d.name : undefined,
    }
  } catch {
    return null
  }
}

// ── Alert on this query ───────────────────────────────────────────────────────
// The Explorer hands one statement and its connection to the workflow builder
// the same way, never in the URL, so the SQL stays out of the address bar and
// the history. The builder takes it once, and only when opened from here.

export const WORKFLOW_FROM_EXPLORER_HREF = "/workflows/new?from=explorer"
const WORKFLOW_QUERY_KEY = "rsync_workflow_new_query_v1"

export interface WorkflowQueryDraft {
  sql: string
  connectionId: string
}

export function stashWorkflowQuery(draft: WorkflowQueryDraft): void {
  try {
    storage("sessionStorage")?.setItem(WORKFLOW_QUERY_KEY, JSON.stringify(draft))
  } catch {
    // Storage blocked: the builder opens on a query to fill in.
  }
}

/** The query the Explorer handed over, removed as it is read. */
export function takeWorkflowQuery(): WorkflowQueryDraft | null {
  const store = storage("sessionStorage")
  const raw = store?.getItem(WORKFLOW_QUERY_KEY)
  if (!raw) return null
  try {
    store?.removeItem(WORKFLOW_QUERY_KEY)
  } catch {
    // Read-only storage: the query fills in once more on the next visit from the Explorer.
  }
  try {
    const d = JSON.parse(raw) as Record<string, unknown>
    if (typeof d.sql !== "string" || !d.sql.trim() || typeof d.connectionId !== "string" || !d.connectionId) {
      return null
    }
    return { sql: d.sql, connectionId: d.connectionId }
  } catch {
    return null
  }
}

// ── History ───────────────────────────────────────────────────────────────────
// Unchanged by the tabs (§ 7.5): the last 20 successful runs per connection, in
// this browser. Read and written by connection id rather than through page
// state, because a run in one tab can finish while another tab, on another
// connection, is on screen.

export const HISTORY_KEY_PREFIX = "rsync_explorer_history_v1"
export const MAX_HISTORY = 20

export interface QueryHistoryEntry {
  id: string
  /** Set on runs from before the Explorer stopped writing SQL from questions. */
  question?: string
  sql: string
  timestamp: Date
  executionTimeMs?: number
  rowCount?: number
}

export function readHistory(connectionId: string): QueryHistoryEntry[] {
  if (!connectionId) return []
  try {
    const raw = storage("localStorage")?.getItem(`${HISTORY_KEY_PREFIX}:${connectionId}`)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    return parsed
      .map((e: Record<string, unknown>) => ({
        id: String(e.id || ""),
        question: e.question ? String(e.question) : undefined,
        sql: String(e.sql || ""),
        timestamp: new Date((e.timestamp as string | number | undefined) || Date.now()),
        executionTimeMs: typeof e.executionTimeMs === "number" ? e.executionTimeMs : undefined,
        rowCount: typeof e.rowCount === "number" ? e.rowCount : undefined,
      }))
      .filter((e) => e.id && e.sql)
  } catch {
    return []
  }
}

/** Puts `entry` first in the connection's history, keeps the newest 20, and returns the list. */
export function appendHistory(connectionId: string, entry: QueryHistoryEntry): QueryHistoryEntry[] {
  const next = [entry, ...readHistory(connectionId)].slice(0, MAX_HISTORY)
  try {
    storage("localStorage")?.setItem(
      `${HISTORY_KEY_PREFIX}:${connectionId}`,
      JSON.stringify(next.map((e) => ({ ...e, timestamp: e.timestamp.toISOString() }))),
    )
  } catch {
    // Storage blocked: the list on screen still updates.
  }
  return next
}
