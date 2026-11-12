"use client"

import { useState, useEffect, useCallback, useMemo, useRef, type CSSProperties } from "react"
import { isNameResolutionFailure } from "@/lib/errors/name-resolution"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Badge } from "@/components/ui/badge"
import { Textarea } from "@/components/ui/textarea"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import {
  Database,
  Play,
  History,
  Bookmark,
  Code,
  Loader2,
  Plus,
  AlertCircle,
  CheckCircle2,
  RefreshCw,
  Clock,
  Copy,
  Download,
  X,
  Server,
  PanelLeftClose,
  PanelLeftOpen,
  Table2,
  EyeOff,
  GripHorizontal,
  ChartColumn,
  BellRing,
} from "lucide-react"
import { Switch } from "@/components/ui/switch"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { toast } from "sonner"
import { copyTextToClipboard } from "@/lib/clipboard"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"
import { cn } from "@/lib/utils"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { useCurrentUser } from "@/contexts/CurrentUserContext"
import { captureWorkspace, getActiveWorkspaceId, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import {
  canRunExplorerStatement,
  classifyExplorerStatement,
  destructiveLabel,
  firstSqlVerb,
  minRoleForStmtClass,
} from "@/lib/explorer/statementClass"
import { resolveRunTarget, type RunTarget } from "@/lib/explorer/sqlStatements"
import {
  SqlEditor,
  type SqlEditorHandle,
  SchemaBrowser,
  SavedQueries,
} from "@/components/explorer"
import { DEFAULT_DATABASE, groupTablesByDatabase } from "@/lib/explorer/schemaTree"
import { explorerSqlDialect, SQL_DIALECT_LABELS, type ExplorerSqlDialect } from "@/lib/explorer/sqlDialect"
import { handleExplorerRunShortcut } from "@/lib/explorer/runShortcut"
import {
  EXPLORER_OPEN_SQL_EVENT,
  MAX_QUERY_TABS,
  addTab,
  appendHistory,
  closeTab,
  initialTabsState,
  loadTabs,
  queryTabsKey,
  readHistory,
  renameTab,
  saveTabs,
  stashWorkflowQuery,
  takeExplorerSql,
  updateTab,
  WORKFLOW_FROM_EXPLORER_HREF,
  type QueryHistoryEntry,
  type QueryTabsState,
} from "@/lib/explorer/queryTabs"
import { DocumentExplorer, type DocumentMode } from "@/components/explorer/DocumentExplorer"
import { buildDocumentQueryText, parseDocumentQueryText } from "@/lib/explorer/documentSpec"
import { isInternalExplorerTable } from "@/lib/explorer/internalTables"
import { extractRawMessage } from "@/lib/explorer/errorMessage"
import { maskedColumns } from "@/lib/explorer/redactedColumns"
import { textFilters } from "@/lib/explorer/textFilters"
import { ExplorerStartHere } from "@/components/explorer/ExplorerStartHere"
import { useModKey } from "@/lib/hooks/useModKey"

// Types
interface Connection {
  id: string
  name: string
  connector_type: string
  type: string
  status: string
  is_connected?: boolean
  // Backend-computed Data Explorer capability (ResolveExplorerCapability). The
  // dropdown filters on supports_explorer instead of a hardcoded connector allowlist,
  // so a new warehouse becomes explorable server-side with no frontend change.
  supports_explorer?: boolean
  explorer_mode?: string // "sql" | "document" (MongoDB: DocumentExplorer instead of the SQL editor)
  sql_dialect?: string
  // Decrypted, secret-masked connection config — already in the /connections
  // payload (ListConnections runs it through maskSensitiveFields). Read here for
  // `config.database`: the ONE database discovery is pinned to, which is the
  // whole explanation when a connection browses empty.
  config?: Record<string, unknown>
}

interface TableMetadata {
  name: string
  schema?: string
  row_count?: number
  columns?: ColumnMetadata[]
}

interface ColumnMetadata {
  name: string
  type: string
  nullable?: boolean
  is_primary_key?: boolean
}

interface ForeignKeyMetadata {
  from_schema?: string
  from_table: string
  from_column: string
  to_schema?: string
  to_table: string
  to_column: string
  confidence?: number
}

interface ExplorerError {
  code: string   // "auth" | "service_unavailable" | "db_error" | "network" | "validation" | "schema_failed" | "unknown"
  title: string  // shown as the error heading
  message: string // raw server message or fallback
  hint?: string  // actionable suggestion shown below
}

// Carry a structured ExplorerError through throw so the catch block
// can set it directly rather than re-parsing a plain string.
class ExplorerApiError extends Error {
  constructor(public readonly explorerError: ExplorerError) {
    super(explorerError.message)
    this.name = "ExplorerApiError"
  }
}

interface QueryResult {
  columns: string[]
  rows: Record<string, unknown>[]
  row_count: number
  execution_time_ms: number
  truncated: boolean
  warnings?: string[]
  // Set for write statements: the number of rows the statement changed (undefined for
  // reads and for engines that don't report a count).
  rows_affected?: number
  // The classified statement (SELECT, INSERT, DROP, …) — drives write-vs-grid rendering.
  statement_type?: string
}

// What the centre column shows: the active query tab's editor and results, or
// the Saved or History list in their place. Saved and History are tabs at the
// top (dashboards-design.md § 7.5): "history" is the automatic, per-browser
// localStorage scratchpad; "saved" is the workspace-scoped server resource
// (migration 084). Those two coexist on purpose — one is disposable, one shared.
type ExplorerView = "query" | "saved" | "history"

// One tab's last run. Held in memory only, by tab id: a result is a copy of
// customer data and is never written to the browser's storage.
interface TabRun {
  result: QueryResult | null
  // The SQL the result came from, for the 0-row state's text matches.
  sql: string | null
  error: ExplorerError | null
  running: boolean
}

const EMPTY_RUN: TabRun = { result: null, sql: null, error: null, running: false }

const TAB_LIMIT_MESSAGE = `${MAX_QUERY_TABS} query tabs are open. Close one to open another.`

type ExportFormat = "csv" | "tsv" | "json" | "xlsx"

// Compose/results split: the editor pane's share of the workspace column,
// dragged by the separator between them and persisted per browser. The bounds
// keep both panes usable — the editor never gets too short to read SQL in, the
// grid never gets squeezed to one row.
const COMPOSE_RATIO_KEY = "explorer.composeRatio"
const DEFAULT_COMPOSE_RATIO = 0.45
const MIN_COMPOSE_RATIO = 0.25
const MAX_COMPOSE_RATIO = 0.75

function clampComposeRatio(v: number): number {
  if (!Number.isFinite(v)) return DEFAULT_COMPOSE_RATIO
  return Math.min(MAX_COMPOSE_RATIO, Math.max(MIN_COMPOSE_RATIO, v))
}

// The one database discovery is pinned to. A connector whose tables live in
// databases and whose config names one ignores the connection's Scope entirely
// (connectionScope, backend-orchestrator .../discovery_scope.go), so this single
// value decides what the tree can possibly contain.
function pinnedDatabaseOf(config: Record<string, unknown> | undefined): string {
  if (!config) return ""
  for (const key of ["database", "db", "dbname", "database_name"]) {
    const v = config[key]
    if (typeof v === "string" && v.trim() !== "") return v.trim()
  }
  return ""
}

// Tiny English-only pluralization helper. Centralized so every label
// across the page reads "1 row" vs "12 rows" the same way; locale-aware
// formatting (commas, narrow non-breaking space) is bundled in.
function pluralizeRows(n: number): string {
  const count = new Intl.NumberFormat("en-US").format(n)
  return `${count} ${n === 1 ? "row" : "rows"}`
}

function normalizeTableName(input: unknown): string {
  const raw = String(input ?? "").trim()
  if (!raw) return ""
  // Handle schema.table or db.schema.table by taking the last segment.
  const last = raw.split(".").pop() || ""
  // Strip common quoting.
  return last.replace(/^["'`]+/, "").replace(/["'`]+$/, "").trim()
}

// True when the connection is a MySQL/MariaDB family source. These are
// per-server connections where each "namespace" is a sibling database, so the
// Explorer can offer a "show all databases" scope. Postgres namespaces are
// schemas under one database and are always fully browsable.
function isMySQLFamily(conn?: { connector_type?: string; type?: string }): boolean {
  if (!conn) return false
  return /mysql|mariadb/i.test(`${conn.connector_type || ""} ${conn.type || ""}`)
}

function tableKeyFromMeta(t: { name: string; schema?: string }): string {
  const table = normalizeTableName(t.name)
  const schema = String(t.schema || "").trim()
  return schema ? `${schema}.${table}` : table
}

export default function ExplorerPage() {
  const modKey = useModKey()
  const DEFAULT_ROW_LIMIT = 100
  const MAX_ROW_LIMIT = 1000
  const SCHEMA_CACHE_TTL_MS = 15 * 60 * 1000

  // Connection state
  const [connections, setConnections] = useState<Connection[]>([])

  // ── Query tabs (dashboards-design.md § 7.5) ──
  // Up to 10, each with its own connection and SQL, kept in this browser per
  // person and workspace (lib/explorer/queryTabs). Until the person is known
  // there is no key and the tabs live in memory; the stored ones replace them
  // the moment it is. A person with no workspace picked uses the default one.
  const { user, isLoading: userLoading } = useCurrentUser()
  const [workspaceId, setWorkspaceId] = useState<string | null>(() => getActiveWorkspaceId())
  const tabsKey = queryTabsKey(user?.id, workspaceId || "default")
  // The tabs carry the key they were loaded under, so a workspace switch can
  // never save one workspace's tabs under the other's key.
  const [tabsBox, setTabsBox] = useState<{ key: string | null; state: QueryTabsState }>(() => ({
    key: null,
    state: initialTabsState(),
  }))
  const tabsState = tabsBox.state
  const setTabsState = useCallback((update: (s: QueryTabsState) => QueryTabsState) => {
    setTabsBox((box) => {
      const next = update(box.state)
      return next === box.state ? box : { ...box, state: next }
    })
  }, [])
  useEffect(() => {
    if (!tabsKey) return
    const stored = loadTabs(tabsKey)
    // Storage is only readable on the client and the key needs the signed-in
    // person, so this cannot be a useState initializer; it runs once per key.
    // With nothing stored, the first load keeps the tab already on screen and
    // a switch to another workspace starts that one fresh.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setTabsBox((box) => ({ key: tabsKey, state: stored ?? (box.key === null ? box.state : initialTabsState()) }))
  }, [tabsKey])
  useEffect(() => {
    if (tabsBox.key && tabsBox.key === tabsKey) saveTabs(tabsBox.key, tabsBox.state)
  }, [tabsBox, tabsKey])
  const activeTab = tabsState.tabs.find((t) => t.id === tabsState.active) ?? tabsState.tabs[0]
  const activeTabId = activeTab.id
  // A tab whose connection is gone (deleted, or not in this workspace) reads the
  // first usable one until it is pointed at another. Derived, not stored, so it
  // can never disagree with the connection list it was picked from.
  const selectedConnection = connections.some((c) => c.id === activeTab.connectionId)
    ? activeTab.connectionId
    : (connections[0]?.id ?? "")
  // Document connections (explorer_mode "document") browse one collection at a time.
  const [documentCollection, setDocumentCollection] = useState<string>("")
  // The picked collection's database: a server-level connection lists several.
  const [documentDatabase, setDocumentDatabase] = useState<string | undefined>(undefined)
  const pickDocumentCollection = useCallback((name: string, database?: string) => {
    setDocumentCollection(name)
    setDocumentDatabase(database)
  }, [])
  // Find or Pipeline, and the pipeline as typed. Held here because a pipeline is what a
  // MongoDB saved query (and so a MongoDB model) stores.
  const [documentMode, setDocumentMode] = useState<DocumentMode>("find")
  const [documentPipeline, setDocumentPipeline] = useState("")
  const loadSavedPipeline = useCallback(
    (text: string) => {
      const saved = parseDocumentQueryText(text)
      if (!saved) {
        toast.error("This saved query is not a pipeline the Document Browser can open")
        return
      }
      pickDocumentCollection(saved.collection, saved.database)
      setDocumentPipeline(saved.pipeline)
      setDocumentMode("pipeline")
    },
    [pickDocumentCollection],
  )
  const [loadingConnections, setLoadingConnections] = useState(true)

  // Schema state
  const [tables, setTables] = useState<TableMetadata[]>([])
  const [foreignKeys, setForeignKeys] = useState<ForeignKeyMetadata[]>([])

  // Internal `_rsync_*` bookkeeping tables are noise in the explorer UI; hide
  // them from the schema tree, NL example chips, and SQL autocomplete. The raw
  // `tables` list stays available for selection / key logic. (I2b)
  const visibleTables = useMemo(
    () => tables.filter((t) => !isInternalExplorerTable(t.name)),
    [tables],
  )
  const [loadingSchema, setLoadingSchema] = useState(false)
  const [schemaError, setSchemaError] = useState<ExplorerError | null>(null)
  // CachedLabel: true when the visible schema came from the in-memory
  // schemaCacheRef rather than a fresh fetch. Shown as a small "Cached"
  // pill near the Tables count so the user knows whether to hit the
  // refresh button before composing queries against potentially-stale
  // schema. Reset to false on any fresh load + on connection change.
  const [schemaFromCache, setSchemaFromCache] = useState(false)
  // Age of the cached schema being displayed, so the badge can read "Cached ·
  // 7m" instead of a bare "Cached". "Cached" alone doesn't tell you whether to
  // distrust the row counts; the age does.
  const [schemaCachedAt, setSchemaCachedAt] = useState<number | null>(null)
  // Ticks only while a cached schema is on screen, so the age label doesn't
  // freeze at whenever the page last happened to re-render.
  const [schemaCacheAgeTick, setSchemaCacheAgeTick] = useState(0)
  useEffect(() => {
    if (schemaCachedAt === null) return
    const t = window.setInterval(() => setSchemaCacheAgeTick((n) => n + 1), 30_000)
    return () => window.clearInterval(t)
  }, [schemaCachedAt])
  const schemaCacheAgeLabel = useMemo(() => {
    void schemaCacheAgeTick
    if (schemaCachedAt === null) return ""
    const mins = Math.floor((Date.now() - schemaCachedAt) / 60_000)
    return mins < 1 ? "" : `${mins}m`
  }, [schemaCachedAt, schemaCacheAgeTick])
  // allDatabases (MySQL only) switches the schema-index fetch to server scope
  // (every database, not just the configured one). Tree expansion + search now
  // live inside <SchemaBrowser>.
  const [allDatabases, setAllDatabases] = useState(false)

  // Cache schema + analysis per connection (in-memory, per session).
  const schemaCacheRef = useRef(
    new Map<
      string,
      {
        tables: TableMetadata[]
        foreignKeys: ForeignKeyMetadata[]
        cachedAt: number
      }
    >()
  )

  // Query state: the active tab's SQL, and its last run.
  const sqlQuery = activeTab.sql
  // The saved query last opened from Saved, while the editor still holds its SQL: Create
  // chart charts that query (a chart's source is a model or a saved query, § 7.2).
  const [openedSaved, setOpenedSaved] = useState<{ id: string; sql: string } | null>(null)
  const chartHref =
    openedSaved && openedSaved.sql === sqlQuery ? `/charts/new?source=${encodeURIComponent(openedSaved.id)}` : "/charts/new"
  const setSqlQuery = useCallback(
    (v: string | ((prev: string) => string)) =>
      setTabsState((s) => {
        const t = s.tabs.find((x) => x.id === s.active)
        if (!t) return s
        return updateTab(s, t.id, { sql: typeof v === "function" ? v(t.sql) : v })
      }),
    [setTabsState],
  )
  const [runs, setRuns] = useState<Record<string, TabRun>>({})
  const patchRun = useCallback((tabId: string, patch: Partial<TabRun>) => {
    setRuns((prev) => ({ ...prev, [tabId]: { ...(prev[tabId] ?? EMPTY_RUN), ...patch } }))
  }, [])
  const activeRun = runs[activeTabId] ?? EMPTY_RUN
  const queryResult = activeRun.result
  // A query that matched nothing names its text matches: a value it guessed
  // ('DECLINED' for a stored 'declined') is the usual reason.
  const zeroRowFilters = queryResult?.rows.length === 0 ? textFilters(activeRun.sql) : []
  const queryError = activeRun.error
  const executingQuery = activeRun.running
  const setQueryError = useCallback(
    (e: ExplorerError | null) => patchRun(activeTabId, { error: e }),
    [patchRun, activeTabId],
  )
  // Destructive statements (DROP/TRUNCATE) require an owner AND a typed confirmation.
  const [showDestructiveConfirm, setShowDestructiveConfirm] = useState(false)
  const [destructiveConfirmText, setDestructiveConfirmText] = useState("")
  // The run target the destructive dialog is holding. Captured when the dialog opens so
  // the typed-confirmation and the eventual execute both refer to the statement the user
  // was looking at, even if the caret moves behind the modal.
  const [pendingRunTarget, setPendingRunTarget] = useState<RunTarget | null>(null)
  // The editor's caret/selection, mirrored so the Run button, the statement badge and the
  // role warning can all describe the statement that will actually run.
  const [editorSelection, setEditorSelection] = useState<{ from: number; to: number }>({
    from: 0,
    to: 0,
  })
  // The single statement a Run action would execute: the selection, else the statement
  // under the caret, else the whole buffer. EVERY gate below reads runTarget.sql — never
  // sqlQuery — because with several queries in the buffer those are different strings and
  // gating on the wrong one is how a DROP under the caret slips past a confirm dialog that
  // only ever saw the leading SELECT.
  const runTarget = useMemo(
    () => resolveRunTarget(sqlQuery, editorSelection.from, editorSelection.to),
    [sqlQuery, editorSelection.from, editorSelection.to]
  )
  // The caller's workspace role gates which statement classes can run (advisory; the
  // api-gateway independently enforces via validators.ValidateExplorerStatement).
  const { role: workspaceRole, meets: meetsRole } = useWorkspaceRole()
  const canMakeCharts = meetsRole("admin")
  // Building a workflow is an owner's or admin's, as on /workflows/new; the server checks it again.
  const canBuildWorkflows = meetsRole("admin")
  const router = useRouter()
  const currentStmtClass = classifyExplorerStatement(runTarget.sql)
  const canRunCurrentStatement = canRunExplorerStatement(workspaceRole, runTarget.sql)
  // Stable identity: SqlEditor holds this in a memoized extension array, so a new
  // function every render would rebuild the CodeMirror extensions on every keystroke.
  const handleEditorSelectionChange = useCallback(
    (sel: { from: number; to: number }) => setEditorSelection(sel),
    []
  )
  // Name what Run will actually do. With one query this stays "Run Query"; with several
  // it has to say which, or the button is a coin flip.
  const runButtonLabel = useMemo(() => {
    if (runTarget.multi) return "Run Query"
    if (runTarget.source === "selection") {
      return currentStmtClass === "destructive" ? "Run Selection…" : "Run Selection"
    }
    if (runTarget.total > 1) {
      return currentStmtClass === "destructive"
        ? `Run Statement ${runTarget.index}…`
        : `Run Statement ${runTarget.index}`
    }
    return currentStmtClass === "destructive" ? "Run Query…" : "Run Query"
  }, [runTarget.multi, runTarget.source, runTarget.total, runTarget.index, currentStmtClass])
  const [exportingFormat, setExportingFormat] = useState<ExportFormat | null>(null)

  // History: the last 20 successful runs per connection in this browser, read
  // from storage and re-read after each run (a run in a background tab can add
  // to a connection that is not on screen).
  const [historyVersion, setHistoryVersion] = useState(0)
  const queryHistory = useMemo(() => {
    void historyVersion
    return readHistory(selectedConnection)
  }, [selectedConnection, historyVersion])

  // Cell viewer (for long values so UI truncation isn't confused with masking)
  const [cellViewerOpen, setCellViewerOpen] = useState(false)
  const [cellViewerTitle, setCellViewerTitle] = useState<string>("")
  const [cellViewerValue, setCellViewerValue] = useState<string>("")

  const [view, setView] = useState<ExplorerView>("query")
  const [renamingTabId, setRenamingTabId] = useState<string | null>(null)

  // Height split between the compose pane and the results pane below it.
  const [composeRatio, setComposeRatio] = useState(DEFAULT_COMPOSE_RATIO)
  const splitColumnRef = useRef<HTMLDivElement>(null)

  // The schema browser (left rail) collapses to a thin strip so power users
  // writing raw SQL can reclaim the full editor width. Open by default.
  const [leftRailOpen, setLeftRailOpen] = useState(true)

  // Imperative handle on the CodeMirror SQL editor — used by the
  // sidebar's "insert table/column at cursor" buttons. Without this
  // the click-to-insert flow would require replicating CodeMirror's
  // selection API at the page level.
  const sqlEditorRef = useRef<SqlEditorHandle>(null)
  // One AbortController per tab with a run in flight. .abort() cancels both the
  // fetch AND the server-side query via the request context (the Go driver
  // honors ctx.Done() on connections). Per tab, so a run started in one tab
  // never cancels another's.
  const abortsRef = useRef(new Map<string, AbortController>())

  // Persist the dragged split so a chosen layout survives a reload.
  const persistComposeRatio = useCallback((next: number) => {
    const clamped = clampComposeRatio(next)
    setComposeRatio(clamped)
    try {
      window.localStorage.setItem(COMPOSE_RATIO_KEY, String(clamped))
    } catch {
      // Storage disabled (private window): the split still works this session.
    }
  }, [])

  // Restore it. Deliberately an effect rather than a useState initializer so the
  // server render and the first client render agree.
  useEffect(() => {
    try {
      const raw = window.localStorage.getItem(COMPOSE_RATIO_KEY)
      // localStorage is only readable on the client, and a useState initializer
      // that read it would make the server and client renders disagree. Runs
      // once, no deps, so there is no cascading-render loop to worry about.
      // eslint-disable-next-line react-hooks/set-state-in-effect
      if (raw !== null) setComposeRatio(clampComposeRatio(Number(raw)))
    } catch {
      // Same as above — fall back to the default ratio.
    }
  }, [])

  // Pointer drag on the separator. The ratio moves by how far the pointer
  // travelled, NOT to where it landed. Both panes have minimum heights, so as
  // soon as one of those floors binds, the separator stops sitting at
  // `composeRatio` of the column — and mapping the pointer's absolute position
  // onto the ratio then teleports the split the moment it is grabbed. Measured
  // on the deployed page: grabbing the handle at rest moved the stored ratio
  // from 43% to 75%, and because the drag persists, the jump outlived it.
  // Measured against the workspace column, not the pane, so a given delta means
  // the same thing whatever the panes currently are.
  const startSplitDrag = useCallback(
    (e: React.PointerEvent<HTMLDivElement>) => {
      const column = splitColumnRef.current
      if (!column) return
      e.preventDefault()
      const box = column.getBoundingClientRect()
      if (box.height <= 0) return
      const startY = e.clientY
      const startRatio = composeRatio
      const ratioAt = (clientY: number) => startRatio + (clientY - startY) / box.height
      const move = (ev: PointerEvent) => setComposeRatio(clampComposeRatio(ratioAt(ev.clientY)))
      const up = (ev: PointerEvent) => {
        window.removeEventListener("pointermove", move)
        window.removeEventListener("pointerup", up)
        persistComposeRatio(ratioAt(ev.clientY))
      }
      window.addEventListener("pointermove", move)
      window.addEventListener("pointerup", up)
    },
    [composeRatio, persistComposeRatio]
  )

  // Keyboard equivalent — a drag handle that only works with a mouse is not one.
  const handleSplitKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const step = e.shiftKey ? 0.1 : 0.02
      if (e.key === "ArrowUp") {
        e.preventDefault()
        persistComposeRatio(composeRatio - step)
      } else if (e.key === "ArrowDown") {
        e.preventDefault()
        persistComposeRatio(composeRatio + step)
      } else if (e.key === "Home") {
        e.preventDefault()
        persistComposeRatio(MIN_COMPOSE_RATIO)
      } else if (e.key === "End") {
        e.preventDefault()
        persistComposeRatio(MAX_COMPOSE_RATIO)
      }
    },
    [composeRatio, persistComposeRatio]
  )

  const loadConnections = useCallback(async () => {
    // A switch mid-flight makes this response the previous workspace's list.
    const isStale = captureWorkspace()
    try {
      setLoadingConnections(true)
      const res = await authFetch(API_ENDPOINTS.CONNECTIONS.LIST)
      const data = await res.json()
      if (isStale()) return
      // The backend computes supports_explorer per connection (ResolveExplorerCapability)
      // and is the single source of truth — a new warehouse becomes explorable with a
      // resolver entry and no frontend edit. Fall back to substring matching only for
      // older gateways that don't yet emit the flag. Non-SQL connectors (mongodb,
      // object stores, SaaS) resolve supports_explorer=false and are excluded — they
      // 400 on query.
      const sqlConnectors = (data.connections || []).filter((c: Connection) => {
        if (typeof c.supports_explorer === "boolean") return c.supports_explorer
        const t = (c.connector_type || "").toLowerCase()
        return t.includes("postgres") || t.includes("mysql") || t.includes("mariadb")
          || t.includes("redshift") || t.includes("databricks")
          || t.includes("sqlserver") || t.includes("mssql") || t.includes("bigquery")
      })

      const usable = sqlConnectors.sort((a: Connection, b: Connection) => {
        // Sources before destinations; within each type, tested-success connections first, then stable by name.
        if (a.type !== b.type) return a.type === "source" ? -1 : 1
        const aOk = a.is_connected ? 0 : 1
        const bOk = b.is_connected ? 0 : 1
        if (aOk !== bOk) return aOk - bOk
        return String(a.name || "").localeCompare(String(b.name || ""))
      })

      // No auto-select: a tab with no usable connection reads the first one
      // (selectedConnection above).
      setConnections(usable)

      if (usable.length === 0) {
        toast.message("No queryable connections found", {
          description: "Create a PostgreSQL, MySQL, SQL Server, Redshift, Databricks, BigQuery, Snowflake, ClickHouse, Oracle, or MongoDB connection (source or destination) to use Explorer.",
        })
      }
    } catch (err) {
      const e = classifyApiError(
        (err as { statusCode?: number })?.statusCode,
        { error: err instanceof Error ? err.message : String(err) },
        "connections"
      )
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      setLoadingConnections(false)
    }
  }, [])

  const loadSchema = useCallback(
    async (opts?: { force?: boolean }) => {
    if (!selectedConnection) return
    // Cache + request key are scoped: the server-scope (all databases) index is
    // a different result set than the default database-scope one, so they must
    // not share an in-memory cache entry.
    const scopeSuffix = allDatabases ? "::server" : "::db"
    const cacheKey = `${selectedConnection}${scopeSuffix}`
    try {
      const cached = schemaCacheRef.current.get(cacheKey)
      const isFresh = cached && Date.now() - cached.cachedAt < SCHEMA_CACHE_TTL_MS
      if (!opts?.force && cached && isFresh) {
        // A previous connection's failure must not stay on screen over this one's tables.
        setSchemaError(null)
        setTables(cached.tables || [])
        setForeignKeys(cached.foreignKeys || [])
        setSchemaFromCache(true)
        setSchemaCachedAt(cached.cachedAt)
        return
      }

      setLoadingSchema(true)
      setSchemaError(null)
      setSchemaFromCache(false)
      setSchemaCachedAt(null)
      setTables([])
      // Use Explorer schema-index (cached + includes types + FK relationships).
      //
      // Through authFetch, like the four other api-gateway calls in this file.
      // The /api/explorer/* Next.js proxy routes are intercepted by Traefik
      // before reaching Next.js, so this must address the api-gateway itself —
      // and a bare relative URL does not do that. It resolves against the page
      // origin, which is the frontend, so on any deployment where the two are
      // separate origins the schema load 404'd at Next.js and the Explorer
      // showed a schema error for a perfectly healthy connection. authFetch
      // resolves the base through @/lib/config/api and carries the session and
      // workspace headers the endpoint needs.
      const params = new URLSearchParams()
      if (opts?.force) params.set("refresh", "true")
      if (allDatabases) params.set("scope", "server")
      const qs = params.toString()
      const res = await authFetch(
        `/api/v1/explorer/connections/${selectedConnection}/schema-index${qs ? `?${qs}` : ""}`
      )
      if (!res.ok) {
        const raw = await res.text().catch(() => "")
        let parsed: any = {}
        try { parsed = raw ? JSON.parse(raw) : {} } catch { parsed = { error: raw } }
        throw classifyApiError(res.status, parsed, "schema")
      }
      const data = await res.json()
      const nextTables = Array.isArray(data?.tables) ? data.tables : []
      const nextFks = Array.isArray(data?.foreign_keys) ? data.foreign_keys : []
      setTables(nextTables)
      setForeignKeys(nextFks)
      schemaCacheRef.current.set(cacheKey, {
        tables: nextTables,
        foreignKeys: nextFks,
        cachedAt: Date.now(),
      })
    } catch (err) {
      const e: ExplorerError =
        err && typeof err === "object" && "code" in err && "title" in err
          ? (err as ExplorerError)
          : classifyApiError(undefined, { error: err instanceof Error ? err.message : "Unknown error" }, "schema")
      setSchemaError(e)
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      setLoadingSchema(false)
    }
    },
    [selectedConnection, allDatabases]
  )

  // A run can finish after its tab, or its connection, has left the screen.
  const selectedConnectionRef = useRef(selectedConnection)
  const loadSchemaRef = useRef(loadSchema)
  useEffect(() => {
    selectedConnectionRef.current = selectedConnection
    loadSchemaRef.current = loadSchema
  }, [selectedConnection, loadSchema])

  // Load connections on mount
  useEffect(() => {
    loadConnections()
  }, [loadConnections])

  // Reload connections when the active workspace changes (header switcher). This is a
  // client page, so router.refresh() won't re-run the fetch. The tabs move to the new
  // workspace's key, and the old list goes at once so the effects keyed on
  // selectedConnection clear the schema. Runs in flight belong to the previous
  // workspace: they are cancelled and every result is dropped.
  useEffect(() => {
    return onActiveWorkspaceChange(() => {
      setWorkspaceId(getActiveWorkspaceId())
      setConnections([])
      for (const ctl of abortsRef.current.values()) ctl.abort()
      abortsRef.current.clear()
      setRuns({})
      void loadConnections()
    })
  }, [loadConnections])

  // Load schema when connection changes
  useEffect(() => {
    if (selectedConnection) {
      loadSchema()
    } else {
      setTables([])
      setForeignKeys([])
      setSchemaFromCache(false)
      setSchemaError(null)
    }
  }, [selectedConnection, loadSchema])

  // Reset server-scope browsing when the connection changes — scope is a
  // MySQL-only, per-connection choice and shouldn't leak across connections.
  useEffect(() => {
    setAllDatabases(false)
    pickDocumentCollection("")
    setDocumentMode("find")
    setDocumentPipeline("")
  }, [selectedConnection, pickDocumentCollection])

  // Map the selected connection's connector_type onto a CodeMirror SQL
  // dialect so the editor highlights and autocompletes against the
  // right keyword set + identifier rules.
  const sqlDialect: ExplorerSqlDialect = useMemo(
    () => explorerSqlDialect(connections.find((c) => c.id === selectedConnection)?.connector_type),
    [connections, selectedConnection],
  )

  // Columns the server masked before sending this preview, detected from the
  // values rather than by re-implementing the Go naming rules (which would
  // drift). The grid has always shown "ab***" with no explanation, and the
  // export path deliberately does NOT mask — both now get said out loud.
  const maskedResultColumns = useMemo(
    () => maskedColumns(queryResult?.columns, queryResult?.rows),
    [queryResult]
  )

  // Cancel the active tab's in-flight Run Query. Triggered by the explicit
  // "Cancel" button on the Run Query row and by Cmd/Ctrl+. inside the editor.
  // The AbortController.signal we pass to fetch propagates to the
  // server-side request context, which cancels the running DB query. The
  // run's own `finally` clears the controller and the spinner.
  const cancelQuery = useCallback(() => {
    abortsRef.current.get(activeTabId)?.abort()
  }, [activeTabId])

  // DX-ClickToInsert: insert a snippet at the SQL editor's cursor.
  // Used by the sidebar's per-table and per-column quick-insert
  // affordances. If the editor is empty we drop the snippet at
  // position 0 (focus + insertAtCursor handles both cases).
  const insertIntoEditor = useCallback((snippet: string) => {
    const handle = sqlEditorRef.current
    if (!handle) return
    // Add a leading space if the editor already has content and the
    // last character isn't whitespace — keeps inserts from joining
    // accidentally with the previous token.
    let toInsert = snippet
    if (sqlQuery.length > 0 && !/\s$/.test(sqlQuery)) {
      toInsert = " " + snippet
    }
    handle.insertAtCursor(toInsert)
    if (queryError) setQueryError(null)
  }, [sqlQuery, queryError, setQueryError])

  // attemptRunQuery routes the Run action: destructive statements (DROP/TRUNCATE) open a
  // typed-confirmation dialog first; everything else runs immediately. executeQuery still
  // re-checks the role gate as insurance.
  const attemptRunQuery = () => {
    const target = runTarget
    // A selection spanning a `;` is the one case we refuse outright. The server rejects
    // stacked statements by design (validators.ValidateExplorerStatement, before the
    // class/role gate), so say why here instead of surfacing its generic message.
    if (target.multi) {
      const msg =
        "The selection contains more than one statement. Select a single statement, or put the cursor inside one and run without a selection."
      setQueryError({ code: "validation", title: "Select one statement", message: msg })
      toast.error(msg)
      return
    }
    if (classifyExplorerStatement(target.sql) === "destructive") {
      setDestructiveConfirmText("")
      setPendingRunTarget(target)
      setShowDestructiveConfirm(true)
      return
    }
    executeQuery(target)
  }

  // `target` is the statement to run. It is always passed explicitly by the callers above
  // so the string that passed the gates is the string that gets executed; the default is
  // only for callers that legitimately mean "whatever the editor is pointing at now".
  const executeQuery = async (target: RunTarget = runTarget) => {
    const targetSql = target.sql
    if (!targetSql.trim()) {
      toast.error("Please enter a SQL query")
      return
    }

    if (!selectedConnection) {
      toast.error("Please select a connection")
      return
    }

    // Guardrail: gate the statement class on the caller's workspace role. Reads run for
    // any member; writes need admin, DROP/TRUNCATE need owner. Advisory — the api-gateway
    // re-checks via validators.ValidateExplorerStatement — but keeps the UX honest.
    const stmtClass = classifyExplorerStatement(targetSql)
    if (stmtClass === "blocked") {
      const msg = `${firstSqlVerb(targetSql)} statements can't be run from Explorer.`
      setQueryError({ code: "validation", title: "Statement not allowed", message: msg })
      toast.error(msg)
      return
    }
    if (!canRunExplorerStatement(workspaceRole, targetSql)) {
      const min = minRoleForStmtClass(stmtClass)
      const msg = `This statement requires the ${min ?? "owner"} role. Ask a workspace ${min ?? "owner"} to run it.`
      setQueryError({ code: "validation", title: "Insufficient role", message: msg })
      toast.error(msg)
      return
    }

    // The run belongs to the tab it started in, whichever tab is on screen when
    // it finishes: its result, error and spinner are written under this id.
    const tabId = activeTabId
    const connId = selectedConnection
    // Re-running a tab replaces its run; other tabs' runs carry on.
    abortsRef.current.get(tabId)?.abort()
    const abortController = new AbortController()
    abortsRef.current.set(tabId, abortController)

    try {
      patchRun(tabId, { running: true, error: null, result: null, sql: null })

      const effectiveLimit = clampInt(extractLimitFromSQL(targetSql) ?? DEFAULT_ROW_LIMIT, 1, MAX_ROW_LIMIT)

      const res = await authFetch("/api/v1/explorer/query", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Accept: "application/json",
        },
        body: JSON.stringify({
          connection_id: connId,
          sql: targetSql,
          limit: effectiveLimit,
        }),
        signal: abortController.signal,
      })

      const raw = await res.text()
      let data: Record<string, unknown> = {}
      try {
        data = raw ? JSON.parse(raw) : {}
      } catch {
        throw new ExplorerApiError({
          code: "network",
          title: "Unexpected server response",
          message: raw || "The server returned a non-JSON response.",
          hint: "This may be a temporary issue. Try again.",
        })
      }

      if (data.error || !res.ok) {
        const e = classifyApiError(res.status, data, "execute")
        patchRun(tabId, { error: e })
        toast.error(e.title, { description: e.hint ?? e.message })
      } else {
        const rowCount = typeof data.row_count === "number" ? data.row_count : 0
        const executionTimeMs = typeof data.execution_time_ms === "number" ? data.execution_time_ms : 0
        const truncated = Boolean(data.truncated)
        const columns = Array.isArray(data.columns) ? data.columns : []
        // Write statements return an affected-row count + statement type instead of rows.
        const rowsAffected = typeof data.rows_affected === "number" ? data.rows_affected : undefined
        const statementType = typeof data.statement_type === "string" ? data.statement_type : undefined

        const nextResult: QueryResult = {
          columns,
          rows: Array.isArray(data.rows) ? (data.rows as Record<string, unknown>[]) : [],
          row_count: rowCount,
          execution_time_ms: executionTimeMs,
          truncated,
          rows_affected: rowsAffected,
          statement_type: statementType,
        }
        patchRun(tabId, { result: nextResult, sql: targetSql })
        appendHistory(connId, {
          id:
            (globalThis.crypto as unknown as { randomUUID?: () => string })?.randomUUID?.() ||
            `${Date.now()}-${Math.random().toString(16).slice(2)}`,
          // The statement that ran — not the whole editor buffer, which may hold several.
          sql: targetSql,
          timestamp: new Date(),
          executionTimeMs,
          rowCount,
        })
        setHistoryVersion((n) => n + 1)
        // No success toast: the Results header already says the rows (or rows
        // affected) and the time, and the toast sat on top of it (#54).
        if (isWriteResult(statementType)) {
          // The write just invalidated the schema we're showing. Row counts, and
          // the table list itself after a CREATE/DROP, are both wrong now — the
          // sidebar kept a row count of 3 for a table that had grown to 8 and
          // then been dropped. Drop both scope entries (the write may have hit a
          // table outside the current scope) and re-fetch past the server cache;
          // a connection no longer on screen re-fetches when it is next shown.
          schemaCacheRef.current.delete(`${connId}::db`)
          schemaCacheRef.current.delete(`${connId}::server`)
          if (connId === selectedConnectionRef.current) void loadSchemaRef.current({ force: true })
        }
      }
    } catch (err) {
      // Cancellation is a normal user action, not an error — show
      // a neutral status and don't push an ExplorerError into the UI.
      if (err instanceof DOMException && err.name === "AbortError") {
        toast.info("Query cancelled")
        return
      }
      console.error("Failed to execute query:", err)
      const e: ExplorerError =
        err instanceof ExplorerApiError
          ? err.explorerError
          : classifyApiError(undefined, { error: err instanceof Error ? err.message : String(err) }, "execute")
      patchRun(tabId, { error: e })
      toast.error(e.title, { description: e.hint ?? e.message })
    } finally {
      // Only if this is still the tab's run: a re-run has replaced it, and a
      // closed tab or a workspace switch has already dropped it.
      if (abortsRef.current.get(tabId) === abortController) {
        abortsRef.current.delete(tabId)
        patchRun(tabId, { running: false })
      }
    }
  }

  // Page-level Cmd/Ctrl+Enter fallback, for when focus is NOT in a control that owns the
  // shortcut itself (schema browser, a button, the page body). The decision lives in
  // lib/explorer/runShortcut so it can be tested, and so the deps type can withhold
  // executeQuery — see the defect write-up there.
  const handleKeyDown = (e: React.KeyboardEvent) => {
    // Document mode has no SQL to run; DocumentExplorer handles its own shortcut.
    if (connections.find((c) => c.id === selectedConnection)?.explorer_mode === "document") return
    // Saved and History put no editor on screen, so there is nothing in view to run.
    if (view !== "query") return
    handleExplorerRunShortcut(e, {
      hasSql: Boolean(sqlQuery.trim()),
      attemptRunQuery,
    })
  }

  const copyToClipboard = async (text: string) => {
    if (await copyTextToClipboard(text)) {
      toast.success("Copied to clipboard")
    } else {
      toast.error("Could not copy to clipboard")
    }
  }

  const downloadBlob = (blob: Blob, filename: string) => {
    const url = window.URL.createObjectURL(blob)
    const a = document.createElement("a")
    a.href = url
    a.download = filename
    document.body.appendChild(a)
    a.click()
    a.remove()
    window.URL.revokeObjectURL(url)
  }

  const parseFilenameFromDisposition = (disposition: string | null, fallback: string): string => {
    if (!disposition) return fallback
    // e.g. attachment; filename=export.csv OR attachment; filename="export.csv"
    const m = disposition.match(/filename\*?=(?:UTF-8''|")?([^\";]+)\"?/i)
    const raw = m?.[1]?.trim()
    if (!raw) return fallback
    try {
      return decodeURIComponent(raw)
    } catch {
      return raw
    }
  }

  const exportResults = async (format: ExportFormat) => {
    if (!selectedConnection || !sqlQuery.trim()) {
      toast.error("Run a query first")
      return
    }
    try {
      setExportingFormat(format)
      // Re-run export server-side with a safe cap (keeps downloads consistent even if UI is truncated).
      // /proxy/ prefix so Traefik doesn't intercept it (only routes /api/* to api-gateway).
      // authFetch sends the CSRF header the route checks; an absolute same-origin URL
      // keeps it off the gateway's base URL.
      const res = await authFetch(new URL("/proxy/explorer/export", window.location.origin).href, {
        method: "POST",
        headers: { Accept: "*/*" },
        body: JSON.stringify({
          connection_id: selectedConnection,
          sql: sqlQuery,
          format,
          limit: 10000,
        }),
      })

      if (!res.ok) {
        const raw = await res.text().catch(() => "")
        let data: Record<string, unknown> = {}
        try { data = raw ? JSON.parse(raw) : {} } catch { data = { error: raw || `HTTP ${res.status}` } }
        const e = classifyApiError(res.status, data, "export")
        toast.error(e.title, { description: e.hint ?? e.message })
        return
      }

      const blob = await res.blob()
      const ext = format === "xlsx" ? "xlsx" : format
      const fallback = `explorer-export.${ext}`
      const filename = parseFilenameFromDisposition(res.headers.get("Content-Disposition"), fallback)
      downloadBlob(blob, filename)
      toast.success(`Downloaded ${format.toUpperCase()}`)
    } catch (e) {
      const err = classifyApiError(undefined, { error: e instanceof Error ? e.message : String(e) }, "export")
      toast.error(err.title, { description: err.hint ?? err.message })
    } finally {
      setExportingFormat(null)
    }
  }

  // Start here (a bar, or a recent query): the SQL goes in this tab when it is
  // empty, else in a new one so nothing typed is lost. It is not run: a person runs it.
  const startQuery = (sql: string) => {
    if (!sqlQuery.trim() || sqlQuery === sql) {
      setSqlQuery(sql)
      return
    }
    const next = addTab(tabsState, { connectionId: selectedConnection, sql })
    if (!next) {
      toast.error(TAB_LIMIT_MESSAGE)
      return
    }
    setTabsState(() => next)
    setEditorSelection({ from: 0, to: 0 })
  }

  const loadFromHistory = (entry: QueryHistoryEntry) => {
    setSqlQuery(entry.sql)
    setView("query")
    toast.info("Query loaded from history")
  }

  // ── Query tab actions ──
  const openNewTab = () => {
    const next = addTab(tabsState, { connectionId: selectedConnection })
    if (!next) {
      toast.error(TAB_LIMIT_MESSAGE)
      return
    }
    setTabsState(() => next)
    setEditorSelection({ from: 0, to: 0 })
    setView("query")
  }

  const selectTab = (id: string) => {
    if (id !== activeTabId) {
      setTabsState((s) => ({ ...s, active: id }))
      // The caret belonged to the other tab's editor.
      setEditorSelection({ from: 0, to: 0 })
    }
    setView("query")
  }

  // Closing a tab cancels its run and drops its result with it.
  const closeQueryTab = (id: string) => {
    const ctl = abortsRef.current.get(id)
    if (ctl) {
      abortsRef.current.delete(id)
      ctl.abort()
    }
    setRuns((prev) => {
      if (!(id in prev)) return prev
      const next = { ...prev }
      delete next[id]
      return next
    })
    setTabsState((s) => closeTab(s, id))
    if (id === activeTabId) setEditorSelection({ from: 0, to: 0 })
    if (id === renamingTabId) setRenamingTabId(null)
  }

  const finishRename = (id: string, name: string) => {
    setTabsState((s) => renameTab(s, id, name))
    setRenamingTabId(null)
  }

  const atTabLimit = tabsState.tabs.length >= MAX_QUERY_TABS

  // ── Open in Explorer (Ask rsync.ai's SQL) ──
  // The draft waits until this person's stored tabs are on screen, so loading
  // them can never replace the tab it opens. Signed out there is no key, and
  // the tabs are the in-memory ones as soon as we know nobody is signed in.
  const tabsReady = tabsKey ? tabsBox.key === tabsKey : !userLoading
  const [draftSignal, setDraftSignal] = useState(0)
  useEffect(() => {
    const onDraft = () => setDraftSignal((n) => n + 1)
    window.addEventListener(EXPLORER_OPEN_SQL_EVENT, onDraft)
    return () => window.removeEventListener(EXPLORER_OPEN_SQL_EVENT, onDraft)
  }, [])
  useEffect(() => {
    if (!tabsReady) return
    const draft = takeExplorerSql()
    if (!draft) return
    const next = addTab(tabsState, {
      sql: draft.sql,
      name: draft.name,
      connectionId: draft.connectionId || selectedConnection,
    })
    if (!next) {
      toast.error(TAB_LIMIT_MESSAGE)
      return
    }
    // The draft lives in sessionStorage until it is taken, so it can only be
    // opened from an effect; takeExplorerSql removes it, so this runs once.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setTabsState(() => next)
    setView("query")
  }, [tabsReady, draftSignal, tabsState, selectedConnection, setTabsState])

  // Engine-aware naming: MySQL exposes sibling databases, Postgres exposes
  // schemas under one database. The tier label and the "all databases" toggle
  // key off this.
  const selectedConn = connections.find((c) => c.id === selectedConnection)
  const isMySQLConn = isMySQLFamily(selectedConn)
  const isDocumentConn = selectedConn?.explorer_mode === "document"
  // Alert on this query: the one statement Run would execute, if it reads, opens
  // the workflow builder with it and this connection as the query; the person
  // adds the condition and where to send. The builder's Save checks it as any draft.
  const canAlertOnQuery =
    !!selectedConnection && !!runTarget.sql.trim() && !runTarget.multi && currentStmtClass === "read"
  const alertOnQuery = () => {
    if (!canAlertOnQuery) return
    stashWorkflowQuery({ sql: runTarget.sql.trim(), connectionId: selectedConnection })
    router.push(WORKFLOW_FROM_EXPLORER_HREF)
  }
  const toolbarBlurb = isDocumentConn
    ? "Browse and filter the documents in your MongoDB collections"
    : "Query and explore your connected data sources using SQL"
  const namespaceLabel = isMySQLConn || isDocumentConn ? "databases" : "schemas"
  // Database/schema count for the panel header label. Counts the *visible*
  // tables (rsync-internal `_rsync_*` excluded) so the header total matches the
  // tree below it. The full tree (grouping, search, expand/collapse) lives in
  // <SchemaBrowser>, which is also fed visibleTables.
  const databaseCount = groupTablesByDatabase(visibleTables).length
  const isDestinationConn = (selectedConn?.type || "").toLowerCase() === "destination"
  const pinnedDatabase = pinnedDatabaseOf(selectedConn?.config)
  // A document destination is browsed through the database it names AND the
  // databases this workspace's pipelines write to on it, which the gateway adds
  // to its scope (KI-EXPLORER-PIPELINE-DEST-DB-UNREACHABLE). A database is
  // listed once it holds data, so the tree is the only place to read them from.
  const reachesPipelineDatabases = isDocumentConn && isDestinationConn
  const pipelineDatabases =
    isDocumentConn && pinnedDatabase
      ? groupTablesByDatabase(visibleTables)
          .map((g) => g.database)
          .filter((d) => d !== pinnedDatabase && d !== DEFAULT_DATABASE)
      : []
  const dialectLabel = SQL_DIALECT_LABELS[sqlDialect] ?? "SQL"
  const composePct = Math.round(composeRatio * 100)

  // What the tree says when it has nothing to show. "No tables found" answers a
  // question nobody asked: discovery is pinned to ONE database, so an empty tree
  // almost always means "that database is empty", not "the server is". This is
  // the whole of the reported "my MongoDB destination does not show up in
  // Explorer" — discovery was returning success with zero collections for the
  // database the connection names, and the UI never named it.
  const emptyTreeHint = (() => {
    if (!selectedConnection) return "Select a connection to browse tables"
    const items = isDocumentConn ? "collections" : "tables"
    if (!pinnedDatabase || (isMySQLConn && allDatabases)) return `No ${items} found`
    if (reachesPipelineDatabases) {
      return `No ${items} in "${pinnedDatabase}", the database this connection names, or in the databases this workspace's pipelines write to on it. A pipeline's database is listed here once it holds data.`
    }
    const head = `No ${items} in "${pinnedDatabase}" — the one database this connection browses.`
    return isDestinationConn
      ? `${head} This is a destination: a pipeline writing to it targets its own database, which can be a different one, so an empty tree here does not mean the pipeline is not landing data.`
      : `${head} Point the connection at another database to browse that one instead.`
  })()

  return (
    <div
      data-testid="explorer-shell"
      className="flex flex-col @2xl:min-h-0 @2xl:flex-1"
      onKeyDown={handleKeyDown}
    >
      {/* A toolbar rather than a page header: the panes below are the page. */}
      <header className="flex h-11 shrink-0 items-center gap-3 border-b border-zinc-200 bg-white px-4 dark:border-zinc-800 dark:bg-zinc-950">
        <h1 className="shrink-0 text-sm font-semibold text-zinc-900 dark:text-white">Explorer</h1>
        <p className="hidden min-w-0 truncate text-xs text-zinc-500 @xl:block dark:text-zinc-400" title={toolbarBlurb}>
          {toolbarBlurb}
        </p>
      </header>

      {/* Workspace: collapsible schema rail · compose pane above results pane,
          edge to edge and divided by borders. Flex (not grid) so the schema rail
          can collapse to a thin strip by animating its width. From @2xl up the
          row IS the window's remaining height and each pane scrolls inside it —
          that is what makes the schema tree and the result grid as tall as the
          window allows. @2xl is the page's own width, not the window's, so the
          panes stay side by side with the assistant docked on a 1440px screen.
          Narrower, they stack at their content height and the page scrolls. */}
      <div className="flex flex-col @2xl:min-h-0 @2xl:flex-1 @2xl:flex-row @2xl:items-stretch">
        {/* ── Left rail: schema browser ── */}
        <aside
          className={cn(
            "w-full shrink-0 border-b border-zinc-200 bg-white transition-[width] duration-200 dark:border-zinc-800 dark:bg-zinc-950",
            "@2xl:min-h-0 @2xl:border-r @2xl:border-b-0",
            leftRailOpen ? "@2xl:w-60 @5xl:w-[280px]" : "@2xl:w-12"
          )}
        >
        {leftRailOpen ? (
        <div className="flex flex-col @2xl:h-full @2xl:min-h-0">
          <div className="flex h-10 shrink-0 items-center justify-between gap-2 border-b border-zinc-200 px-3 dark:border-zinc-800">
            <h2 className="flex items-center gap-2 text-sm font-medium text-zinc-900 dark:text-zinc-100">
              <Database className="h-4 w-4 text-violet-500" />
              Schema
            </h2>
            <Button
              variant="ghost"
              size="sm"
              className="h-7 w-7 p-0"
              onClick={() => setLeftRailOpen(false)}
              title="Collapse schema browser"
              aria-label="Collapse schema browser"
            >
              <PanelLeftClose className="h-4 w-4" />
            </Button>
          </div>
          <div className="flex min-h-0 flex-1 flex-col gap-3 p-3">
            {/* Connection Selector */}
            <div className="shrink-0 space-y-2">
              <Label className="text-xs">Connection</Label>
              <Select
                value={selectedConnection}
                onValueChange={(id) => setTabsState((s) => updateTab(s, s.active, { connectionId: id }))}
                disabled={loadingConnections}
              >
                <SelectTrigger className="h-9">
                  <SelectValue placeholder="Select connection..." />
                </SelectTrigger>
                <SelectContent>
                  {connections.map((conn) => (
                    <SelectItem key={conn.id} value={conn.id}>
                      <div className="flex items-center gap-2">
                        <Database className="h-3 w-3" />
                        <span className="truncate">{conn.name}</span>
                        <Badge variant="secondary" className="text-[10px] px-1 py-0">
                          {conn.type}
                        </Badge>
                      </div>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            {/* The one database this connection browses. Discovery is hard-pinned
                to it, so it is the difference between "the server is empty" and
                "this database is empty" — and for a destination, between "the
                pipeline is not landing data" and "it lands it somewhere else". */}
            {selectedConnection && pinnedDatabase && !(isMySQLConn && allDatabases) && (
              <div
                className="flex shrink-0 flex-wrap items-center gap-1.5 text-[11px] text-zinc-500 dark:text-zinc-400"
                title={
                  reachesPipelineDatabases || pipelineDatabases.length > 0
                    ? `Browsing "${pinnedDatabase}", the database this connection names, and the databases this workspace's pipelines write to on it${
                        pipelineDatabases.length > 0 ? `: ${pipelineDatabases.join(", ")}` : ""
                      }. A pipeline's database is listed once it holds data.`
                    : isDestinationConn
                      ? `Browsing the database this connection names. It is a destination: a pipeline writing to it targets its own database, which may be a different one.`
                      : `Browsing "${pinnedDatabase}". Discovery is pinned to this one database — other databases on the server are not listed.`
                }
              >
                <Database className="h-3 w-3 shrink-0 text-violet-500" />
                <span>Browsing</span>
                <code className="rounded bg-zinc-100 px-1 py-px font-mono text-[10px] dark:bg-zinc-800">
                  {pinnedDatabase}
                </code>
                {pipelineDatabases.slice(0, 3).map((d) => (
                  <code key={d} className="rounded bg-zinc-100 px-1 py-px font-mono text-[10px] dark:bg-zinc-800">
                    {d}
                  </code>
                ))}
                {pipelineDatabases.length > 3 && <span>+{pipelineDatabases.length - 3} more</span>}
                {isDestinationConn && (
                  <Badge variant="outline" className="h-4 px-1 text-[9px] font-normal">
                    destination
                  </Badge>
                )}
              </div>
            )}

            {/* Schema browser controls */}
            {selectedConnection && (
              <div className="shrink-0 space-y-2">
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-2">
                    {/* A failed or in-flight schema load has no tables yet; a count
                        here would read "0 databases" as if the cluster were empty. */}
                    <Label
                      className={cn("text-xs", schemaError && !loadingSchema && "text-red-600 dark:text-red-400")}
                      data-testid="schema-count-label"
                    >
                      {loadingSchema
                        ? `Loading ${namespaceLabel}…`
                        : schemaError
                        ? `Could not load ${namespaceLabel}`
                        : <span className="capitalize">{databaseCount} {namespaceLabel} · {visibleTables.length} {isDocumentConn ? "collections" : "tables"}</span>}
                    </Label>
                    {schemaFromCache && visibleTables.length > 0 && (
                      <Badge
                        variant="outline"
                        className="text-[9px] py-0 h-4 px-1.5 text-zinc-500 dark:text-zinc-400 border-zinc-300 dark:border-zinc-700"
                        title="Schema and row counts are from a local cache (≤10 min old) and do not reflect changes made outside this tab. Click refresh to re-fetch from the database."
                      >
                        {schemaCacheAgeLabel ? `Cached · ${schemaCacheAgeLabel}` : "Cached"}
                      </Badge>
                    )}
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 px-1.5"
                    onClick={() => loadSchema({ force: true })}
                    disabled={loadingSchema}
                    title="Refresh schema from the database"
                  >
                    <RefreshCw className={cn("h-3 w-3", loadingSchema && "animate-spin")} />
                  </Button>
                </div>
                {/* MySQL server-scope toggle: browse every database on the
                    server, not just the one the connection points at. */}
                {isMySQLConn && (
                  <label
                    className="flex items-center justify-between gap-2 rounded-md border border-dashed border-zinc-200 dark:border-zinc-800 px-2 py-1.5 cursor-pointer"
                    title="Browse every database on this MySQL server from one connection"
                  >
                    <div className="flex items-center gap-1.5 text-[11px] text-zinc-600 dark:text-zinc-300">
                      <Server className="h-3 w-3 text-violet-500" />
                      Show all databases on server
                    </div>
                    <Switch
                      checked={allDatabases}
                      onCheckedChange={setAllDatabases}
                      disabled={loadingSchema}
                      aria-label="Show all databases on server"
                    />
                  </label>
                )}
              </div>
            )}

            {/* Schema tree: namespace → table → columns. Takes whatever height is
                left, so "how many tables can I see at once" is set by the window
                rather than by a hardcoded 340px box. */}
            <div
              data-testid="schema-tree-pane"
              className="min-h-[240px] overflow-hidden rounded-md border @2xl:min-h-0 @2xl:flex-1"
            >
              {loadingSchema ? (
                <div className="flex h-full items-center justify-center py-8">
                  <Loader2 className="h-5 w-5 animate-spin text-violet-500" />
                </div>
              ) : schemaError ? (
                <div className="flex h-full flex-col items-center justify-center py-8 px-4 gap-2 text-center">
                  <AlertCircle className="h-5 w-5 text-red-400 shrink-0" />
                  <div className="text-xs font-semibold text-red-600 dark:text-red-400">{schemaError.title}</div>
                  <div className="text-xs text-zinc-500 dark:text-zinc-400 break-words">{schemaError.message}</div>
                  {schemaError.hint && (
                    <div className="text-[11px] text-zinc-400 italic">{schemaError.hint}</div>
                  )}
                  <Button
                    variant="outline"
                    size="sm"
                    className="mt-1 h-7 text-xs"
                    onClick={() => loadSchema({ force: true })}
                  >
                    <RefreshCw className="h-3 w-3 mr-1" />
                    Retry
                  </Button>
                </div>
              ) : (
                <SchemaBrowser
                  tables={visibleTables}
                  // Showing them makes a join inspectable before you write it.
                  foreignKeys={foreignKeys}
                  // Document mode: no SQL editor to insert into — clicking a
                  // collection opens it in the browser.
                  onInsertTable={
                    isDocumentConn
                      ? (qualified) => {
                          const t = visibleTables.find((x) => tableKeyFromMeta(x) === qualified || x.name === qualified)
                          if (t) pickDocumentCollection(t.name, t.schema || undefined)
                        }
                      : insertIntoEditor
                  }
                  onInsertColumn={isDocumentConn ? undefined : insertIntoEditor}
                  itemLabel={isDocumentConn ? "collections" : "tables"}
                  insertTitle={isDocumentConn ? "Open collection" : "Add to SQL"}
                  insertLabel={isDocumentConn ? "Open collection" : "Insert table"}
                  namespaceLabel={namespaceLabel === "schemas" ? "Schema" : "Database"}
                  emptyHint={emptyTreeHint}
                  className="p-1.5"
                />
              )}
            </div>

          </div>
        </div>
        ) : (
          // A strip above the editor when the panes stack, a vertical rail beside
          // it when they sit side by side — never hidden, or a rail collapsed on
          // a wide page could not be reopened once the page narrows.
          <div className="flex items-center gap-2 px-3 py-1.5 @2xl:h-full @2xl:flex-col @2xl:gap-3 @2xl:px-0 @2xl:py-3">
            <Button
              variant="ghost"
              size="sm"
              className="h-7 w-7 p-0"
              onClick={() => setLeftRailOpen(true)}
              title="Show schema browser"
              aria-label="Show schema browser"
            >
              <PanelLeftOpen className="h-4 w-4 text-violet-500" />
            </Button>
            <span className="text-[10px] font-medium text-zinc-500 dark:text-zinc-400 @2xl:[writing-mode:vertical-rl]">
              Schema
            </span>
          </div>
        )}
        </aside>

        {/* ── Center: compose pane · draggable split · results pane ── */}
        <main
          ref={splitColumnRef}
          className="flex w-full min-w-0 flex-col @2xl:min-h-0 @2xl:flex-1"
        >
          {/* ── Query tabs · Saved · History (dashboards-design.md § 7.5) ── */}
          <div
            data-testid="query-tab-strip"
            className="flex h-10 shrink-0 items-center gap-2 border-b border-zinc-200 bg-zinc-50 px-2 dark:border-zinc-800 dark:bg-zinc-900"
          >
            <div role="group" aria-label="Query tabs" className="flex min-w-0 flex-1 items-center gap-1 overflow-x-auto">
              {tabsState.tabs.map((tab) => {
                // A document connection has no Saved/History views: its one tab is always the page.
                const isActive = tab.id === activeTab.id && (isDocumentConn || view === "query")
                const running = Boolean(runs[tab.id]?.running)
                return (
                  <div
                    key={tab.id}
                    data-testid="query-tab"
                    className={cn(
                      "flex h-7 max-w-[12rem] shrink-0 items-center rounded-md border text-xs",
                      isActive
                        ? "border-zinc-300 bg-white text-zinc-900 shadow-sm dark:border-zinc-700 dark:bg-zinc-950 dark:text-zinc-100"
                        : "border-transparent text-zinc-600 hover:bg-white/70 dark:text-zinc-400 dark:hover:bg-zinc-950/60",
                    )}
                  >
                    {renamingTabId === tab.id ? (
                      <input
                        aria-label="Tab name"
                        autoFocus
                        defaultValue={tab.name}
                        maxLength={60}
                        className="h-6 w-28 rounded bg-transparent px-2 text-xs outline-none ring-1 ring-violet-500"
                        onBlur={(e) => finishRename(tab.id, e.currentTarget.value)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") finishRename(tab.id, e.currentTarget.value)
                          else if (e.key === "Escape") setRenamingTabId(null)
                        }}
                      />
                    ) : (
                      <button
                        type="button"
                        aria-current={isActive ? "true" : undefined}
                        title={`${tab.name} — double-click to rename`}
                        className="flex h-full min-w-0 items-center gap-1.5 pl-2 pr-1"
                        onClick={() => selectTab(tab.id)}
                        onDoubleClick={() => setRenamingTabId(tab.id)}
                      >
                        {running ? (
                          <Loader2 aria-label="Running" className="h-3 w-3 shrink-0 animate-spin text-violet-500" />
                        ) : (
                          <Code className="h-3 w-3 shrink-0 text-zinc-400" />
                        )}
                        <span className="truncate">{tab.name}</span>
                      </button>
                    )}
                    <button
                      type="button"
                      aria-label={`Close ${tab.name}`}
                      title={`Close ${tab.name}`}
                      className="mr-1 rounded p-0.5 text-zinc-400 hover:bg-zinc-200 hover:text-zinc-700 dark:hover:bg-zinc-800 dark:hover:text-zinc-200"
                      onClick={() => closeQueryTab(tab.id)}
                    >
                      <X className="h-3 w-3" />
                    </button>
                  </div>
                )
              })}
              {/* Not `disabled` at the limit: a disabled button cannot be
                  clicked, so it could never say why nothing opened. */}
              <Button
                variant="ghost"
                size="sm"
                className="h-7 w-7 shrink-0 p-0"
                aria-label="New query tab"
                aria-disabled={atTabLimit || undefined}
                title={atTabLimit ? TAB_LIMIT_MESSAGE : "New query tab"}
                onClick={openNewTab}
              >
                <Plus className="h-4 w-4" />
              </Button>
            </div>
            {!isDocumentConn && (
              <div className="flex shrink-0 items-center gap-1">
                <Button
                  variant={view === "saved" ? "secondary" : "ghost"}
                  size="sm"
                  className="h-7 px-2.5 text-xs"
                  aria-pressed={view === "saved"}
                  onClick={() => setView((v) => (v === "saved" ? "query" : "saved"))}
                >
                  <Bookmark className="mr-1 h-3 w-3" />
                  Saved
                </Button>
                <Button
                  variant={view === "history" ? "secondary" : "ghost"}
                  size="sm"
                  className="h-7 px-2.5 text-xs"
                  aria-pressed={view === "history"}
                  onClick={() => setView((v) => (v === "history" ? "query" : "history"))}
                >
                  <History className="mr-1 h-3 w-3" />
                  History
                </Button>
              </div>
            )}
          </div>
          {isDocumentConn ? (
          <div className="space-y-6 p-3 @2xl:min-h-0 @2xl:flex-1 @2xl:overflow-auto">
            <DocumentExplorer
              connectionId={selectedConnection}
              collections={visibleTables}
              collection={documentCollection}
              database={documentDatabase}
              onCollectionChange={pickDocumentCollection}
              loadingCollections={loadingSchema}
              collectionsError={schemaError}
              onRetryCollections={() => loadSchema({ force: true })}
              mode={documentMode}
              onModeChange={setDocumentMode}
              pipeline={documentPipeline}
              onPipelineChange={setDocumentPipeline}
            />
            {/* A saved pipeline is a MongoDB model: Model on its row builds a collection
                from it, on a schedule. */}
            <section
              aria-labelledby="saved-pipelines-heading"
              className="space-y-2 rounded-lg border border-zinc-200 bg-white p-4 dark:border-zinc-800 dark:bg-zinc-950"
            >
              <h2 id="saved-pipelines-heading" className="text-base font-semibold">
                Saved pipelines
              </h2>
              <SavedQueries
                connectionId={selectedConnection}
                currentSql={
                  documentMode === "pipeline"
                    ? buildDocumentQueryText({
                        database: documentDatabase,
                        collection: documentCollection,
                        pipeline: documentPipeline,
                      })
                    : ""
                }
                queryLanguage="document"
                onLoad={loadSavedPipeline}
              />
            </section>
          </div>
          ) : view === "saved" ? (
            <section
              data-testid="saved-pane"
              aria-label="Saved queries"
              className="bg-white p-3 dark:bg-zinc-950 @2xl:min-h-0 @2xl:flex-1 @2xl:overflow-auto"
            >
              <SavedQueries
                connectionId={selectedConnection}
                currentSql={sqlQuery}
                onLoad={(sql, _question, id) => {
                  setSqlQuery(sql)
                  setOpenedSaved(id ? { id, sql } : null)
                  setView("query")
                }}
              />
            </section>
          ) : view === "history" ? (
            <section
              data-testid="history-pane"
              aria-label="History"
              className="bg-white p-3 dark:bg-zinc-950 @2xl:min-h-0 @2xl:flex-1 @2xl:overflow-auto"
            >
              <div className="rounded-md border p-1">
                  {queryHistory.length === 0 ? (
                    <div className="text-sm text-zinc-500 dark:text-zinc-400 py-6 text-center">
                      No history yet. Run a query to populate this list.
                    </div>
                  ) : (
                    <div className="space-y-2">
                      {queryHistory.map((entry) => (
                        <div
                          key={entry.id}
                          role="button"
                          tabIndex={0}
                          className="p-3 border rounded-lg hover:bg-zinc-50 dark:hover:bg-zinc-900 cursor-pointer transition-colors"
                          onClick={() => loadFromHistory(entry)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter" || e.key === " ") {
                              e.preventDefault()
                              loadFromHistory(entry)
                            }
                          }}
                        >
                          {/* Runs from before the Explorer stopped writing SQL from a
                              question still carry it. */}
                          {entry.question && (
                            <div className="mb-1 text-xs text-zinc-700 dark:text-zinc-300">{entry.question}</div>
                          )}
                          <div className="font-mono text-xs text-zinc-600 dark:text-zinc-400 truncate">
                            {entry.sql}
                          </div>
                          <div className="flex items-center gap-3 mt-2 text-[10px] text-zinc-400">
                            <span className="flex items-center gap-1">
                              <Clock className="h-3 w-3" />
                              {entry.executionTimeMs}ms
                            </span>
                            <span>{typeof entry.rowCount === "number" ? pluralizeRows(entry.rowCount) : "—"}</span>
                            <span>{entry.timestamp.toLocaleTimeString()}</span>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
              </div>
            </section>
          ) : (
          <>
          {/* ── Compose pane ── */}
          {/* `basis` is the dragged split and `shrink-0` holds it there, so the
              results pane below absorbs whatever is left. Below @2xl it is
              content-height and the page scrolls as it always did. */}
          <section
            data-testid="compose-pane"
            aria-labelledby="explorer-compose-heading"
            className={cn(
              "flex shrink-0 flex-col border-b border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-950",
              "@2xl:basis-[var(--compose-basis)] @2xl:overflow-hidden @2xl:border-b-0",
              // A percentage basis cannot promise the editor a single pixel: on
              // a short window the editor was squeezed to 0 and the pane showed
              // a bare "SQL Query" label. The floor is the measured content
              // minimum, and the drag cannot go below it.
              "@2xl:min-h-[17rem]",
            )}
            style={{ "--compose-basis": `${composePct}%` } as CSSProperties}
          >
            <div className="flex min-h-10 shrink-0 flex-wrap items-center justify-between gap-2 border-b border-zinc-200 px-3 py-1.5 dark:border-zinc-800">
              <div className="flex min-w-0 items-center gap-2">
                <h2
                  id="explorer-compose-heading"
                  className="flex items-center gap-2 text-sm font-medium text-zinc-900 dark:text-zinc-100"
                >
                  <Code className="h-4 w-4 text-violet-500" />
                  Query Editor
                </h2>
                {/* The dialect the editor highlights and completes against,
                    resolved per connector server-side. */}
                <Badge
                  variant="outline"
                  className="text-[10px] font-normal"
                  title={`SQL is highlighted and completed for ${dialectLabel}`}
                >
                  {dialectLabel}
                </Badge>
              </div>
            </div>
            <div className="flex min-h-0 flex-1 flex-col gap-2.5 p-3">
              {/* SQL Editor — the pane's only growing child */}
              {/* An explicit floor, not `min-h-0`: it still lets this shrink with
                  the pane, but stops at ~five editor lines instead of reaching 0.
                  It holds at every width — when the panes stack (a docked
                  assistant on a 1280px window does it) the pane is content-
                  height and the editor otherwise collapsed to one 26px line. */}
              <div data-testid="sql-editor-pane" className="flex min-h-[7.5rem] flex-1 flex-col gap-2">
                <div className="flex shrink-0 items-center justify-between">
                  <Label htmlFor="explorer-sql" className="text-xs">SQL Query</Label>
                  {sqlQuery && (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-6 px-2"
                      onClick={() => copyToClipboard(sqlQuery)}
                    >
                      <Copy className="h-3 w-3 mr-1" />
                      Copy
                    </Button>
                  )}
                </div>
                {/* One editor per tab: a new key gives the next tab its own undo
                    history and cursor instead of inheriting this one's. */}
                <SqlEditor
                  key={activeTab.id}
                  ref={sqlEditorRef}
                  value={sqlQuery}
                  onChange={(next) => {
                    setSqlQuery(next)
                    if (queryError) setQueryError(null)
                  }}
                  tables={visibleTables}
                  foreignKeys={foreignKeys}
                  dialect={sqlDialect}
                  placeholder="SELECT * FROM table_name LIMIT 100"
                  // Fill the pane rather than a fixed ~200px box: from @2xl the
                  // dragged split decides the height; below it the explicit
                  // h-[220px] keeps CodeMirror's height:100% resolvable.
                  fill
                  className="h-[220px] rounded-md border border-zinc-200 dark:border-zinc-800 overflow-hidden @2xl:h-auto"
                  onSubmit={() => {
                    // Gate on the resolved target, matching the Run button. `multi` still
                    // calls through so attemptRunQuery can explain why it won't run.
                    if (
                      !executingQuery &&
                      runTarget.sql.trim() &&
                      selectedConnection &&
                      (canRunCurrentStatement || runTarget.multi)
                    ) {
                      attemptRunQuery()
                    }
                  }}
                  onCancel={() => {
                    if (executingQuery) cancelQuery()
                  }}
                  onSelectionChange={handleEditorSelectionChange}
                />
              </div>

              {/* Actions */}
              <div className="flex shrink-0 flex-wrap items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs text-zinc-500 dark:text-zinc-400">
                  <kbd className="px-1.5 py-0.5 bg-zinc-100 dark:bg-zinc-800 rounded text-[10px]">{modKey}</kbd>
                  <span>+</span>
                  <kbd className="px-1.5 py-0.5 bg-zinc-100 dark:bg-zinc-800 rounded text-[10px]">Enter</kbd>
                  <span>to run</span>
                  {runTarget.total > 1 && (
                    <>
                      <span className="mx-1 text-zinc-300">·</span>
                      <span className="text-zinc-600 dark:text-zinc-400">
                        {runTarget.source === "selection"
                          ? "runs the selection"
                          : `runs statement ${runTarget.index} of ${runTarget.total}`}
                      </span>
                    </>
                  )}
                  {executingQuery && (
                    <>
                      <span className="mx-1 text-zinc-300">·</span>
                      <kbd className="px-1.5 py-0.5 bg-zinc-100 dark:bg-zinc-800 rounded text-[10px]">{modKey}</kbd>
                      <span>+</span>
                      <kbd className="px-1.5 py-0.5 bg-zinc-100 dark:bg-zinc-800 rounded text-[10px]">.</kbd>
                      <span>to cancel</span>
                    </>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  {canMakeCharts && (
                    <Button asChild variant="outline" size="sm">
                      <Link
                        href={chartHref}
                        title={
                          chartHref === "/charts/new"
                            ? "Chart a model or a saved query: save this query first to chart it"
                            : "Chart this saved query"
                        }
                      >
                        <ChartColumn className="h-4 w-4 mr-2" />
                        Create chart
                      </Link>
                    </Button>
                  )}
                  {canBuildWorkflows && !isDocumentConn && (
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={!canAlertOnQuery}
                      onClick={alertOnQuery}
                      title={
                        canAlertOnQuery
                          ? "Build a workflow that runs this query on a schedule and sends a message when a condition holds"
                          : "Alert on one SELECT: write one, or put the cursor in it"
                      }
                    >
                      <BellRing className="h-4 w-4 mr-2" />
                      Alert on this query
                    </Button>
                  )}
                  {executingQuery ? (
                    <Button
                      onClick={cancelQuery}
                      variant="outline"
                      className="border-red-300 text-red-700 hover:bg-red-50 dark:border-red-700 dark:text-red-300 dark:hover:bg-red-950/30"
                    >
                      <X className="h-4 w-4 mr-2" />
                      Cancel
                    </Button>
                  ) : (
                    <Button
                      onClick={attemptRunQuery}
                      disabled={
                        !runTarget.sql.trim() ||
                        !selectedConnection ||
                        (!canRunCurrentStatement && !runTarget.multi)
                      }
                      title={runTarget.total > 1 ? runTarget.sql : undefined}
                    >
                      <Play className="h-4 w-4 mr-2" />
                      {runButtonLabel}
                    </Button>
                  )}
                </div>
              </div>
              {runTarget.multi ? (
                <div className="shrink-0 text-xs text-amber-700 dark:text-amber-300">
                  Your selection covers more than one statement. Queries run one at a time —
                  select a single statement, or clear the selection and put the cursor inside
                  the one you want.
                </div>
              ) : runTarget.sql.trim() && !canRunCurrentStatement ? (
                <div className="shrink-0 text-xs text-amber-700 dark:text-amber-300">
                  {currentStmtClass === "blocked" ? (
                    <>
                      <span className="font-medium">{firstSqlVerb(runTarget.sql)}</span>{" "}
                      statements can&apos;t be run from Explorer.
                    </>
                  ) : (
                    <>
                      This statement needs the{" "}
                      <span className="font-medium">{minRoleForStmtClass(currentStmtClass) ?? "owner"}</span>{" "}
                      role. Your role is <span className="font-medium">{workspaceRole || "unknown"}</span>.
                    </>
                  )}
                </div>
              ) : currentStmtClass === "destructive" && runTarget.sql.trim() ? (
                <div className="shrink-0 text-xs text-red-700 dark:text-red-300">
                  <span className="font-medium">{destructiveLabel(runTarget.sql)}</span>{" "}
                  is destructive and irreversible — you&apos;ll be asked to confirm.
                </div>
              ) : null}
            </div>
          </section>

          {/* Drag to re-balance editor against results. Keyboard-operable and
              double-click-to-reset, because a handle that only works with a
              mouse is not one. */}
          <div
            role="separator"
            aria-orientation="horizontal"
            aria-label="Resize the editor and results panes"
            aria-valuenow={composePct}
            aria-valuemin={Math.round(MIN_COMPOSE_RATIO * 100)}
            aria-valuemax={Math.round(MAX_COMPOSE_RATIO * 100)}
            tabIndex={0}
            onPointerDown={startSplitDrag}
            onKeyDown={handleSplitKeyDown}
            onDoubleClick={() => persistComposeRatio(DEFAULT_COMPOSE_RATIO)}
            title="Drag to resize · double-click to reset"
            className="group hidden h-3 shrink-0 cursor-row-resize items-center justify-center border-y border-zinc-200 bg-zinc-50 outline-none hover:bg-violet-50 focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-violet-500 dark:border-zinc-800 dark:bg-zinc-900 dark:hover:bg-violet-950/30 @2xl:flex"
          >
            <GripHorizontal className="h-3 w-3 text-zinc-300 transition-colors group-hover:text-violet-500 dark:text-zinc-700" />
          </div>

          {/* ── Results pane ── */}
          {/* An explicit floor rather than `min-h-0`: the compose pane's
              floor leaves this one 62px on a 706px-tall window, and 62px of
              results pane renders a 2px grid scroller. Measured on the deployed
              page at that height, with the masked-columns notice showing (the
              worst realistic case — it costs two lines above the grid), the
              floor buys this much of the scroller, which contains the 48px
              column header: 15rem → 43px, 16rem → 59px, 18rem → 91px,
              20rem → 123px. 15rem cannot show the header at all, so 18rem: the
              header plus a 49px row. The floor only binds on a short window,
              where the page already scrolls, so the extra 48px costs scroll
              rather than layout. Past it the pane grows with `flex-1`. */}
          <section
            data-testid="results-pane"
            aria-label="Results"
            className="flex flex-col bg-white dark:bg-zinc-950 @2xl:min-h-[18rem] @2xl:flex-1 @2xl:overflow-hidden"
          >
            <div className="flex min-h-0 flex-1 flex-col overflow-hidden p-3">
                <div className="flex min-h-0 flex-1 flex-col gap-3">
                  {/* Error Display */}
                  {queryError && (
                    <div className="shrink-0 rounded-md border border-red-200 bg-red-50 p-3 dark:border-red-900 dark:bg-red-950/20">
                        <div className="flex items-start gap-3">
                          <AlertCircle className="h-5 w-5 text-red-500 mt-0.5 shrink-0" />
                          <div className="flex-1 min-w-0 space-y-1">
                            <div className="font-semibold text-sm text-red-700 dark:text-red-400">
                              {queryError.title}
                            </div>
                            <div className="text-sm text-red-700 dark:text-red-300 font-mono break-words">
                              {queryError.message}
                            </div>
                            {queryError.hint && (
                              <div className="flex items-start gap-1.5 mt-2 text-xs text-zinc-600 dark:text-zinc-400 bg-white/60 dark:bg-zinc-900/60 rounded px-2 py-1.5">
                                <span className="shrink-0 mt-px">💡</span>
                                <span>{queryError.hint}</span>
                              </div>
                            )}
                          </div>
                        </div>
                    </div>
                  )}

                  {/* A run in flight: the pane the result will land in says so, instead
                      of looking like nothing happened. */}
                  {executingQuery && !queryResult && (
                    <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 py-12 text-center">
                      <Loader2 className="h-6 w-6 animate-spin text-violet-500" />
                      <div className="text-sm text-zinc-500 dark:text-zinc-400">Running your query…</div>
                      <div className="text-xs text-zinc-400 dark:text-zinc-500">
                        Press {modKey} + . to cancel
                      </div>
                    </div>
                  )}

                  {/* Results */}
                  {queryResult && (
                    <>
                      {/* Toolbar: only what came back and what you can do with it. */}
                      <div className="shrink-0">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <div className="flex shrink-0 items-center gap-1.5 text-sm font-medium text-emerald-600 dark:text-emerald-400">
                            <CheckCircle2 className="h-4 w-4" />
                            Ran
                          </div>
                          <div className="flex flex-wrap items-center gap-2">
                            <div className="flex flex-wrap items-center gap-3 text-xs text-zinc-500 dark:text-zinc-400">
                              {isWriteResult(queryResult.statement_type) ? (
                                <span>
                                  {queryResult.statement_type}
                                  {queryResult.rows_affected !== undefined
                                    ? ` — ${pluralizeRows(queryResult.rows_affected)} affected`
                                    : " executed"}
                                </span>
                              ) : (
                                <span>{pluralizeRows(queryResult.row_count)}</span>
                              )}
                              <span>{queryResult.execution_time_ms}ms</span>
                              {queryResult.truncated && (
                                <Badge
                                  variant="outline"
                                  className="text-[10px]"
                                  title="Result set exceeded the row cap — increase LIMIT or add filters to see more."
                                >
                                  Truncated
                                </Badge>
                              )}
                            </div>
                            {/* Export + BI act on a returned result set — hidden for write
                                statements, which return an affected-row count, not rows. */}
                            {!isWriteResult(queryResult.statement_type) && (
                              <>
                                <DropdownMenu>
                                  <DropdownMenuTrigger asChild>
                                    <Button variant="outline" size="sm" disabled={!sqlQuery.trim() || exportingFormat !== null}>
                                      <Download className="h-4 w-4 mr-2" />
                                      {exportingFormat ? "Preparing…" : "Download"}
                                    </Button>
                                  </DropdownMenuTrigger>
                                  <DropdownMenuContent align="end">
                                    {/* The grid masks secret/PII columns; the export
                                        executors deliberately do not. Say so here,
                                        where the download is actually chosen. */}
                                    {maskedResultColumns.length > 0 && (
                                      <>
                                        <DropdownMenuLabel className="max-w-[17rem] text-xs font-normal whitespace-normal text-amber-700 dark:text-amber-300">
                                          Downloads are not masked — the{" "}
                                          {maskedResultColumns.length === 1 ? "column" : "columns"} hidden in
                                          this preview will contain real values.
                                        </DropdownMenuLabel>
                                        <DropdownMenuSeparator />
                                      </>
                                    )}
                                    <DropdownMenuItem onClick={() => exportResults("csv")}>CSV</DropdownMenuItem>
                                    <DropdownMenuItem onClick={() => exportResults("tsv")}>TSV</DropdownMenuItem>
                                    <DropdownMenuItem onClick={() => exportResults("json")}>JSON</DropdownMenuItem>
                                    {/* Excel (.xlsx) export intentionally not exposed: the api-gateway has no Go xlsx renderer wired up.
                                        If demand surfaces, swap in xlsx-populate or excelize-go and add a `xlsx` branch to ExportQueryHandler. */}
                                  </DropdownMenuContent>
                                </DropdownMenu>
                                <Button
                                  variant="outline"
                                  size="sm"
                                  disabled={!sqlQuery.trim()}
                                  onClick={() => {
                                    setView("saved")
                                    toast.info(
                                      "Save this query, then use Schedule on it to materialize the result as a table on a schedule."
                                    )
                                  }}
                                  title="Save this query, then schedule it to materialize as a table"
                                >
                                  <Table2 className="h-4 w-4 mr-2" />
                                  Turn into a table
                                </Button>
                                {canMakeCharts && (
                                  <Button asChild variant="outline" size="sm">
                                    <Link
                                      href={chartHref}
                                      title={
                                        chartHref === "/charts/new"
                                          ? "Chart a model or a saved query: save this query first to chart it"
                                          : "Chart this saved query"
                                      }
                                    >
                                      <ChartColumn className="h-4 w-4 mr-2" />
                                      Create chart
                                    </Link>
                                  </Button>
                                )}
                              </>
                            )}
                          </div>
                        </div>
                      </div>

                      {/* Masking, said out loud. The grid has always rendered "ab***"
                          with no explanation of why or of what a download returns. */}
                      {maskedResultColumns.length > 0 && (
                        <div className="flex shrink-0 items-start gap-2 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs dark:border-amber-900 dark:bg-amber-950/20">
                          <EyeOff className="mt-px h-3.5 w-3.5 shrink-0 text-amber-600 dark:text-amber-400" />
                          <div className="min-w-0 space-y-1">
                            <div className="text-amber-800 dark:text-amber-200">
                              <span className="font-medium">
                                {maskedResultColumns.length}{" "}
                                {maskedResultColumns.length === 1 ? "column is" : "columns are"} masked in
                                this preview:
                              </span>{" "}
                              <span className="font-mono break-all">{maskedResultColumns.join(", ")}</span>
                            </div>
                            <div className="text-amber-700 dark:text-amber-300">
                              Values read <span className="font-mono">ab***</span> because the column name
                              marks it as a secret or as personal data. A download is not masked and
                              returns the real values.
                            </div>
                          </div>
                        </div>
                      )}

                      <div className="flex min-h-0 flex-1 flex-col gap-3">
                        {isWriteResult(queryResult.statement_type) ? (
                          // WriteOutcome: a write/DDL statement returns no result grid — show a
                          // clear "N rows affected" confirmation instead.
                          <div className="flex min-h-0 flex-1 flex-col justify-center rounded-md border border-emerald-200 dark:border-emerald-900 bg-emerald-50/60 dark:bg-emerald-950/20 py-12 px-6 text-center">
                            <CheckCircle2 className="mx-auto h-8 w-8 text-emerald-500" />
                            <div className="mt-3 text-sm font-medium text-zinc-800 dark:text-zinc-100">
                              {queryResult.statement_type} statement executed
                            </div>
                            <div className="mt-1 text-xs text-zinc-500 dark:text-zinc-400 max-w-md mx-auto">
                              {queryResult.rows_affected !== undefined
                                ? `${pluralizeRows(queryResult.rows_affected)} affected.`
                                : "The statement completed successfully."}
                            </div>
                          </div>
                        ) : queryResult.rows.length === 0 ? (
                          // ZeroRowState: a successful query that returned no rows is
                          // a common, expected outcome (filter too narrow, table really
                          // is empty, etc.). Show a friendly empty state instead of
                          // an awkward blank table so the user knows the run succeeded.
                          <div className="flex min-h-0 flex-1 flex-col justify-center rounded-md border border-dashed border-zinc-200 dark:border-zinc-800 py-12 px-6 text-center">
                            <Database className="mx-auto h-8 w-8 text-zinc-300 dark:text-zinc-700" />
                            <div className="mt-3 text-sm font-medium text-zinc-700 dark:text-zinc-200">
                              Query ran successfully — no rows matched
                            </div>
                            {zeroRowFilters.length > 0 ? (
                              <div data-testid="zero-row-text-filters" className="mt-1 text-xs text-zinc-500 dark:text-zinc-400 max-w-md mx-auto">
                                <p>A text value matches only when it is stored exactly as written, case included. This query matched on:</p>
                                <ul className="mt-2 flex flex-wrap justify-center gap-1.5">
                                  {zeroRowFilters.map((f) => (
                                    <li key={f}>
                                      <code className="rounded bg-zinc-100 dark:bg-zinc-800 px-1.5 py-0.5 font-mono text-[11px] text-zinc-700 dark:text-zinc-200">
                                        {f}
                                      </code>
                                    </li>
                                  ))}
                                </ul>
                                <p className="mt-2">Check the values these columns hold, then run again.</p>
                              </div>
                            ) : (
                              <div className="mt-1 text-xs text-zinc-500 dark:text-zinc-400 max-w-md mx-auto">
                                {queryResult.columns.length > 0
                                  ? `The result set has ${queryResult.columns.length} column${queryResult.columns.length === 1 ? "" : "s"} but zero rows. Loosen your WHERE clause, or pick a different time range, then run again.`
                                  : "The query returned zero rows. Loosen your WHERE clause or pick a different time range, then run again."}
                              </div>
                            )}
                          </div>
                        ) : (
                          <div
                            data-testid="results-grid-scroller"
                            className="min-h-0 max-h-[70vh] flex-1 overflow-auto rounded-md border @2xl:max-h-none"
                          >
                            <Table>
                              <TableHeader>
                                <TableRow>
                                  {queryResult.columns.map((col) => (
                                    <TableHead key={col} className="text-xs font-medium">
                                      {col}
                                    </TableHead>
                                  ))}
                                </TableRow>
                              </TableHeader>
                              <TableBody>
                                {queryResult.rows.map((row, i) => (
                                  <TableRow key={i}>
                                    {queryResult.columns.map((col) => {
                                      const full = formatCellValue(row[col])
                                      const long = full.length > CELL_PREVIEW_CHARS
                                      // One line, cut at the cell's width: a JSON value has no
                                      // space to wrap at and used to run across the next columns.
                                      // A click opens any value the cell cut off.
                                      return (
                                        <TableCell
                                          key={col}
                                          className={cn(
                                            "text-xs font-mono max-w-[520px] truncate",
                                            long ? "cursor-pointer hover:bg-zinc-50 dark:hover:bg-zinc-900" : ""
                                          )}
                                          title={long ? "Click to view full value" : undefined}
                                          onClick={(e) => {
                                            const cell = e.currentTarget
                                            if (long || cell.scrollWidth > cell.clientWidth) {
                                              setCellViewerTitle(col)
                                              setCellViewerValue(full)
                                              setCellViewerOpen(true)
                                            }
                                          }}
                                        >
                                          {long ? full.slice(0, CELL_PREVIEW_CHARS) + "…" : full}
                                        </TableCell>
                                      )
                                    })}
                                  </TableRow>
                                ))}
                              </TableBody>
                            </Table>
                          </div>
                        )}
                        {queryResult.warnings && queryResult.warnings.length > 0 && (
                          <div className="shrink-0 p-3 bg-amber-50 dark:bg-amber-950/20 rounded-lg">
                            <div className="text-xs text-amber-700 dark:text-amber-400">
                              {queryResult.warnings.join("; ")}
                            </div>
                          </div>
                        )}
                      </div>
                    </>
                  )}

                  {!queryResult && !queryError && !executingQuery && (
                    <ExplorerStartHere
                      tables={visibleTables}
                      history={queryHistory}
                      onQueryTable={(qualified) => startQuery(`SELECT * FROM ${qualified} LIMIT 100`)}
                      onOpenQuery={(entry) => startQuery(entry.sql)}
                    />
                  )}
                </div>
            </div>
          </section>
          </>
          )}
        </main>
      </div>

      {/* Cell Viewer Dialog (for long values) */}
      <Dialog open={cellViewerOpen} onOpenChange={setCellViewerOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <Code className="h-5 w-5 text-violet-500" />
              {cellViewerTitle}
            </DialogTitle>
            <DialogDescription>Full cell value</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Textarea value={cellViewerValue} readOnly className="font-mono text-xs min-h-[240px]" />
            <div className="flex justify-end">
              <Button
                variant="outline"
                size="sm"
                onClick={() => copyToClipboard(cellViewerValue)}
              >
                <Copy className="h-4 w-4 mr-2" />
                Copy
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>

      {/* Destructive-statement confirmation (DROP / TRUNCATE): owner-only, type-to-confirm.
          The backend independently enforces owner via validators.ValidateExplorerStatement;
          this dialog is the UX guardrail against an accidental irreversible run. */}
      <Dialog
        open={showDestructiveConfirm}
        closeOnOutsideClick={false}
        onOpenChange={(open) => {
          setShowDestructiveConfirm(open)
          if (!open) {
            setDestructiveConfirmText("")
            setPendingRunTarget(null)
          }
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2 text-red-600 dark:text-red-400">
              <AlertCircle className="h-5 w-5" />
              Confirm destructive statement
            </DialogTitle>
            <DialogDescription>
              This runs a{" "}
              <span className="font-semibold">{destructiveLabel(pendingRunTarget?.sql ?? "")}</span>{" "}
              against the selected connection. It is irreversible and can permanently delete data
              or schema.
            </DialogDescription>
          </DialogHeader>
          {/* Show the exact statement being confirmed. With several queries in the buffer the
              verb alone is ambiguous — the user needs to see WHICH DROP this is. */}
          {pendingRunTarget && (
            <pre className="max-h-32 overflow-auto rounded-md border bg-muted/50 p-2 font-mono text-xs whitespace-pre-wrap break-all">
              {pendingRunTarget.sql}
            </pre>
          )}
          <div className="space-y-2">
            <Label htmlFor="destructive-confirm" className="text-xs">
              Type{" "}
              <span className="font-mono font-semibold">
                {firstSqlVerb(pendingRunTarget?.sql ?? "")}
              </span>{" "}
              to confirm
            </Label>
            <Input
              id="destructive-confirm"
              value={destructiveConfirmText}
              onChange={(e) => setDestructiveConfirmText(e.target.value)}
              placeholder={firstSqlVerb(pendingRunTarget?.sql ?? "")}
              autoComplete="off"
            />
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                setShowDestructiveConfirm(false)
                setDestructiveConfirmText("")
                setPendingRunTarget(null)
              }}
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={
                !pendingRunTarget ||
                destructiveConfirmText.trim().toUpperCase() !== firstSqlVerb(pendingRunTarget.sql)
              }
              onClick={() => {
                // Run the target the dialog was opened for. Falling back to the live
                // runTarget here would re-introduce the confirm-one/run-another gap.
                const target = pendingRunTarget
                setShowDestructiveConfirm(false)
                setDestructiveConfirmText("")
                setPendingRunTarget(null)
                if (target) executeQuery(target)
              }}
            >
              <Play className="h-4 w-4 mr-2" />
              Run {firstSqlVerb(pendingRunTarget?.sql ?? "")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// A result cell renders at most this many characters; a click shows the rest.
const CELL_PREVIEW_CHARS = 200

// Helper to format cell values for display
function formatCellValue(value: unknown): string {
  if (value === null || value === undefined) return "NULL"
  if (typeof value === "object") return JSON.stringify(value)
  if (typeof value === "boolean") return value ? "true" : "false"
  return String(value)
}

// Single classification function: HTTP status + response body + call context → ExplorerError.
// All error paths in this file use this instead of ad-hoc string concatenation.
function classifyApiError(
  status: number | undefined,
  data: any,
  context: "execute" | "schema" | "connections" | "export" | "general"
): ExplorerError {
  const raw = extractRawMessage(data)
  const lower = raw.toLowerCase()
  // Use hint from the response body when the server provided one
  const serverHint: string | undefined =
    data && typeof data === "object" && typeof data.hint === "string" ? data.hint.trim() : undefined

  if (status === 401 || status === 403) {
    // Role/permission rejections from the Explorer statement gate are NOT session
    // problems — surface them as a permission error with the server's hint, not
    // "refresh to sign in".
    const errorCode =
      data && typeof data === "object" && typeof data.error_code === "string" ? data.error_code : undefined
    if (errorCode === "INSUFFICIENT_ROLE_FOR_STATEMENT" || errorCode === "STATEMENT_NOT_ALLOWED") {
      return {
        code: "validation",
        title: "Not permitted",
        message: raw || "You don't have permission to run this statement.",
        hint: serverHint || "Ask a workspace admin or owner to run this, or request a higher role.",
      }
    }
    return {
      code: "auth",
      title: "Authentication error",
      message: raw || "Your session may have expired.",
      hint: serverHint || "Refresh the page to sign in again.",
    }
  }

  if (status === 503) {
    return {
      code: "service_unavailable",
      title: "Service unavailable",
      message: raw || "The service could not be reached.",
      hint: serverHint || "A backend service is temporarily unavailable. Try again shortly.",
    }
  }

  if (status === 422) {
    return {
      code: "validation",
      title: "Invalid request",
      message: raw || "The server rejected the request due to missing or invalid fields.",
      hint: serverHint || "This may indicate a bug. Try refreshing the page and re-running your query.",
    }
  }

  if (!status) {
    return {
      code: "network",
      title: "Connection failed",
      message: raw || "Could not reach the server.",
      hint: serverHint || "Check your network connection and try again.",
    }
  }

  if (context === "execute") {
    // Missing TABLE and missing COLUMN are different fixes, so decide on the
    // engine's own error code before falling back to substring matching. The
    // codes are unambiguous; the words are not — a missing-table error is
    // routinely wrapped in text that mentions columns ("failed to read
    // columns: …"), which used to make every dropped table render the
    // "referenced column may no longer exist" hint.
    // undefined_table: PG 42P01 · MySQL 1146/42S02 · SQL Server 42S02 · Oracle ORA-00942
    // undefined_column: PG 42703 · MySQL 1054/42S22 · SQL Server 42S22 · Oracle ORA-00904
    const missingTableCode = /\b(42P01|42S02|ORA-00942)\b/i.test(raw) || /\b1146\b/.test(raw)
    const missingColumnCode = /\b(42703|42S22|ORA-00904)\b/i.test(raw) || /\b1054\b/.test(raw)
    const looksMissing = lower.includes("not exist") || lower.includes("doesn't exist") || lower.includes("unknown") || lower.includes("invalid object")
    const hint = missingTableCode
      ? "A referenced table was not found. Try refreshing the schema."
      : missingColumnCode
      ? "A referenced column may no longer exist. Try refreshing the schema."
      // No code to go on — check table-ness first, since its errors are the
      // ones that carry stray "column" wrapper text.
      : looksMissing && (lower.includes("relation") || lower.includes("table"))
      ? "A referenced table was not found. Try refreshing the schema."
      : lower.includes("column")
      ? "A referenced column may no longer exist. Try refreshing the schema."
      : lower.includes("relation") || lower.includes("does not exist")
      ? "A referenced table was not found. Try refreshing the schema."
      : lower.includes("timeout") || lower.includes("statement canceled") || lower.includes("canceling")
      ? "The query timed out. Add WHERE filters to reduce the amount of data scanned."
      : lower.includes("syntax")
      ? "The SQL has a syntax error. Edit the query and try again."
      : lower.includes("permission") || lower.includes("denied")
      ? "Your database user may not have permission to read this table."
      : undefined
    return { code: "db_error", title: "Query failed", message: raw || "The database returned an error.", hint }
  }

  if (context === "schema") {
    // Every wording the resolver can produce, not just NXDOMAIN -- see the
    // module for why the one-marker test missed most real outages.
    const isDnsFailure = isNameResolutionFailure(lower)
    const isConnRefused = lower.includes("connection refused") || lower.includes("connect: refused")
    // Rejected logins, across engines: MongoDB "bad auth : authentication failed" /
    // "not authorized", Postgres "password authentication failed", MySQL "access denied for user".
    const isAuthFailure =
      lower.includes("authentication failed") || lower.includes("bad auth") ||
      lower.includes("not authorized") || lower.includes("access denied for user")
    // No answer at all: usually the host is wrong, or the database only accepts
    // listed addresses (MongoDB "no reachable servers" / server selection timeouts).
    // The Python driver ends every server selection timeout with "Timeout: 30s", also
    // when the rest reads "No replica set members found yet".
    const isNoAnswer =
      lower.includes("timed out") || lower.includes("no reachable servers") ||
      lower.includes("server selection") || lower.includes("i/o timeout") ||
      lower.includes("timeout:")
    const hint = isDnsFailure
      ? "The database hostname could not be resolved. Check the host saved on this connection, then retry. If you host rsync.ai yourself, check that its services and the database are running."
      : isConnRefused
      ? "The database server is unreachable. Check that it is running and the host/port are correct."
      : isAuthFailure
      ? "The database rejected the login. Check the username and password saved on this connection, and that the user can read this database, then retry."
      : isNoAnswer
      ? "The database did not answer. Check the host and port, and that the database accepts connections from this server's IP address (network access / IP allowlist), then retry."
      : "Check the username and password saved on this connection, and that the database accepts connections from this server's IP address (network access / IP allowlist), then retry."
    return {
      code: "schema_failed",
      title: "Failed to load schema",
      message: raw || `Schema fetch failed (${status}).`,
      hint,
    }
  }

  if (context === "export") {
    return {
      code: "export_failed",
      title: "Export failed",
      message: raw || `Could not export results (HTTP ${status}).`,
      hint: lower.includes("timeout") || lower.includes("timed out")
        ? "The query took too long. Try adding a LIMIT clause before exporting."
        : "Try re-running the query and exporting again.",
    }
  }

  return {
    code: "unknown",
    title: "Something went wrong",
    message: raw || "An unexpected error occurred.",
    hint: status >= 500 ? "This appears to be a server-side error. Try again in a moment." : undefined,
  }
}

function clampInt(n: number, min: number, max: number): number {
  if (!Number.isFinite(n)) return min
  const x = Math.trunc(n)
  if (x < min) return min
  if (x > max) return max
  return x
}

function extractLimitFromSQL(sql: string): number | null {
  const s = String(sql || "")
  const m = s.match(/\bLIMIT\s+(\d+)\b/i)
  if (!m?.[1]) return null
  const n = Number(m[1])
  if (!Number.isFinite(n) || n <= 0) return null
  return Math.trunc(n)
}

/** A result is a write outcome (render "N rows affected", hide export/BI) when its
 *  statement_type is anything other than a read. */
function isWriteResult(statementType?: string): boolean {
  if (!statementType) return false
  const t = statementType.toUpperCase()
  return t !== "SELECT" && t !== "WITH"
}
