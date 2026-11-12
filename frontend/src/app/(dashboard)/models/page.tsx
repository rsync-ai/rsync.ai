"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { PageHeader } from "@/components/layout/PageHeader"
import { authFetch } from "@/lib/api/auth-fetch"
import { Button } from "@/components/ui/button"
import { Card } from "@/components/ui/card"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  AlertTriangle,
  Boxes,
  CirclePause,
  CirclePlay,
  Clock,
  Hand,
  MoreVertical,
  RefreshCw,
  Search,
  SquarePen,
  XCircle,
} from "lucide-react"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { EmptyState } from "@/components/kit/EmptyState"
import { RowsSkeleton, StatCardsSkeleton } from "@/components/kit/PageSkeleton"
import { StatCard } from "@/components/kit/StatCard"
import { StatusPill } from "@/components/kit/StatusPill"
import { RunCharts } from "@/components/runs/RunCharts"
import { formatNextRunOrDue, nextRunRefetchDelay } from "@/components/explorer/scheduleTime"
import { formatAbsoluteTime } from "@/lib/utils"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { useCurrentUser } from "@/contexts/CurrentUserContext"
import { canEditSavedQuery } from "@/lib/workspace/roles"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import { SavedQueryEditDialog } from "@/components/explorer/SavedQueryEditDialog"
import { SavedQueryModelDialog } from "@/components/explorer/SavedQueryModelDialog"
import {
  cadenceCron,
  describeCadence,
  describeUpstreamNextRun,
  runStatusBadge,
  statusBadge,
  type ScheduledQuery,
} from "@/components/explorer/scheduledModel"
import {
  LiveStateBanner,
  LiveStateCell,
  OverdueBadge,
  useOpenFreshnessBreaches,
  useRunningModels,
} from "@/components/explorer/ModelLiveState"
import { AFTER_UPSTREAM, liveCellFor, openBreachFor, waitsForAll } from "@/components/explorer/liveState"
import { LayerBadge, type LayerFilter, layerFilterFor, layerPosition, matchesLayer } from "@/components/explorer/modelLayer"
import { useWorkspaceLayers } from "@/lib/hooks/useWorkspaceLayers"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"

// Every model in the workspace: a saved query that builds a table or runs a statement.
// Scheduled ones first, with when they run next; then the ones with no schedule, which
// run only when someone runs them ("Manual"). It was "Scheduled Queries" and listed the
// scheduled ones only, so a model nobody had scheduled yet could be found nowhere but
// the Explorer panel of its own connection. A row opens the model's own page
// (/models/[id]) for everything else: every run, paged and filtered, what the refresh
// loop is doing now, and the controls. This list keeps only the last run's outcome, so
// a failure at 03:00 is still visible from here.

// The table scrolls sideways when the page is narrow, and macOS hides the scrollbar
// until you scroll, so the row actions looked cut off. The actions column stays pinned
// to the right edge, opaque so the columns slide under it; its hover colour is the row's
// own (muted/50 over the card is zinc-50 light, zinc-900 dark).
const STICKY_ACTIONS = "sticky right-0 bg-white dark:bg-zinc-950 shadow-[-6px_0_6px_-6px_rgb(0_0_0/0.15)]"
const STICKY_ACTIONS_CELL = `${STICKY_ACTIONS} group-hover:bg-zinc-50 dark:group-hover:bg-zinc-900`

/** GET /api/v1/explorer/saved returns at most this many, most recently edited first. */
const SAVED_LIST_LIMIT = 500
/** GET /api/v1/explorer/schedules returns at most this many (its LIMIT 500). */
const SCHEDULE_LIST_LIMIT = 500

/** How much of the unscheduled half loaded: all of it, the newest 500 only, or none. */
type ManualLoad = "all" | "capped" | "failed"

/** A count the page could only partly read is a floor: "500+", never a wrong exact number. */
function atLeast(n: number, partial: boolean): number | string {
  return partial ? `${n}+` : n
}

/** A model with no schedule, as GET /explorer/saved returns it. */
interface ManualModel {
  id: string
  name: string
  connection_id: string
  connector_type?: string
  materialization: string
  target_table?: string
  layer?: string
  statement_class?: string
  supports_materialization?: boolean
  created_by: string
  created_at: string
  last_run_at?: string
  last_run_status?: string
  last_run_error?: string
  /** The live schedule's status; "" when the model has none. */
  schedule_status?: string
}

/** What the two edit dialogs need from a row, scheduled or not. */
type EditTarget = Pick<
  ScheduledQuery,
  | "saved_query_id"
  | "name"
  | "connection_id"
  | "connector_type"
  | "materialization"
  | "target_table"
  | "layer"
  | "statement_class"
  | "supports_materialization"
  | "last_run_status"
  | "last_run_error"
>

function manualTarget(m: ManualModel): EditTarget {
  return { ...m, saved_query_id: m.id }
}

export default function ModelsPage() {
  const [items, setItems] = useState<ScheduledQuery[]>([])
  const [manual, setManual] = useState<ManualModel[]>([])
  // The unscheduled half failing to load is not the page failing: the scheduled rows
  // are still true. It is said under the table instead, so "every model" is never
  // claimed over a list that is missing some.
  const [manualError, setManualError] = useState<string | null>(null)
  const [manualLoad, setManualLoad] = useState<ManualLoad>("all")
  const [loading, setLoading] = useState(true)
  // Distinct from `items.length === 0`, which asserts the workspace has no
  // schedules. This says the list could not be loaded, so their number is unknown —
  // and "you have nothing scheduled" is the worst possible thing to say wrongly here.
  const [error, setError] = useState<string | null>(null)
  const router = useRouter()
  // Before these, this page could only be read. A schedule found here had to be
  // matched back to a saved query in the Explorer panel to change anything about it —
  // including the cadence the row is showing.
  const [editQuery, setEditQuery] = useState<EditTarget | null>(null)
  const [editSchedule, setEditSchedule] = useState<EditTarget | null>(null)
  const [layerFilter, setLayerFilter] = useState<LayerFilter>("all")
  // This page is viewer-readable, so the Edit column has to gate per role. The
  // api-gateway 403s either mutation regardless; this keeps the UI from offering a
  // button that can only fail.
  const { role: workspaceRole, can } = useWorkspaceRole()
  const { user } = useCurrentUser()
  const canSchedule = can("schedule_query")

  // What each model is doing right now, and which tables are past their freshness
  // deadline: one page-level poll each, joined onto rows by saved_query_id, so no row
  // ever asks anything on its own. Live state is asked only when some row is
  // after_upstream — the route lists nothing else — so a workspace of clock schedules
  // never puts a query on the rebuild queue by being looked at.
  const hasUpstreamTriggered = items.some((s) => s.schedule_type === AFTER_UPSTREAM)
  const { running, refresh: refreshRunning } = useRunningModels(!error && hasUpstreamTriggered)
  const hasModels = items.length + manual.length > 0
  const { freshness, refresh: refreshFreshness } = useOpenFreshnessBreaches(!error && hasModels)
  const openBreaches = freshness.status === "ok" ? freshness.data : []
  const unlistedBreaches = openBreaches.filter(
    (b) =>
      !items.some((s) => s.saved_query_id === b.saved_query_id) &&
      !manual.some((m) => m.id === b.saved_query_id),
  )
  // The filter offers the workspace's layers in their order, then any layer a model
  // still names that the list does not have (it failed to load, or changed since).
  const { layers } = useWorkspaceLayers()
  const layerChoices = [...(layers ?? []).map((l) => l.name)]
  for (const name of [...items.map((s) => s.layer), ...manual.map((m) => m.layer)]) {
    const n = (name ?? "").trim()
    if (n && !layerChoices.some((c) => c.toLowerCase() === n.toLowerCase())) layerChoices.push(n)
  }
  // A layer picked before a workspace switch or a delete is not one to filter by.
  const shownLayer: LayerFilter =
    layerFilter === "all" || layerFilter === "none" || layerChoices.some((c) => layerFilterFor(c) === layerFilter)
      ? layerFilter
      : "all"
  const shownItems = items.filter((s) => matchesLayer(s.layer, shownLayer))
  const shownManual = manual.filter((m) => matchesLayer(m.layer, shownLayer))

  // Counts user-visible loads. A background refetch applies its rows only if no such
  // load started while it was in flight, so a timed refetch landing late cannot put
  // older rows back over the ones a Refresh just fetched.
  const foregroundLoads = useRef(0)
  // Bumped when a background refetch settles, success or not, so the timer below
  // re-arms even when a failed refetch left `items` untouched.
  const [backgroundRefetches, setBackgroundRefetches] = useState(0)

  // `background` is the timed refetch below: no spinner, and a failure keeps the rows
  // already on screen rather than replacing a working list with an error nobody asked
  // for. Every other caller passes nothing and behaves exactly as before. A response
  // that lands after a workspace switch holds the other workspace's schedules, so it
  // is dropped, and only the newest foreground load may end the spinner.
  const load = useCallback(async (opts?: { background?: boolean }) => {
    const background = opts?.background === true
    const seq = background ? foregroundLoads.current : ++foregroundLoads.current
    const isStale = captureWorkspace()
    if (!background) {
      setLoading(true)
      setError(null)
    }
    try {
      const [res, savedRes] = await Promise.all([
        authFetch("/api/v1/explorer/schedules", { cache: "no-store" }),
        // A failure here leaves the scheduled rows standing (manualError, below). A
        // background refetch only refreshes next-run times, so it does not ask again
        // for every saved query and its SQL.
        background
          ? Promise.resolve(null)
          : authFetch("/api/v1/explorer/saved", { cache: "no-store" }).catch(() => null),
      ])
      if (isStale()) return
      if (!res.ok) {
        if (!background) setError(`Could not load models (HTTP ${res.status}).`)
        return
      }
      const data = await res.json()
      const saved = savedRes?.ok ? await savedRes.json().catch(() => null) : null
      if (isStale() || (background && seq !== foregroundLoads.current)) return
      const schedules: ScheduledQuery[] = Array.isArray(data?.schedules) ? data.schedules : []
      setItems(schedules)
      const scheduled = new Set(schedules.map((s) => s.saved_query_id))
      if (background) {
        // Scheduled since the last full load (another tab, say): one row, not two.
        setManual((prev) => prev.filter((m) => !scheduled.has(m.id)))
      } else if (Array.isArray(saved?.saved_queries)) {
        const all = saved.saved_queries as ManualModel[]
        // Unscheduled by the saved list's own word (schedule_status), not merely absent
        // from the schedules list: that list stops at 500, and a scheduled model past
        // its end must not be shown here as Manual.
        setManual(
          all.filter(
            (q) =>
              (q.materialization === "table" || q.materialization === "statement") &&
              !q.schedule_status &&
              !scheduled.has(q.id),
          ),
        )
        // The saved list stops at the 500 most recently edited queries, so a model
        // with no schedule older than those is not on it. Said, not left out quietly.
        const capped = all.length >= SAVED_LIST_LIMIT
        setManualLoad(capped ? "capped" : "all")
        setManualError(
          capped
            ? `Only the ${SAVED_LIST_LIMIT} most recently edited saved queries were checked, so an older model with no schedule is not listed.`
            : null,
        )
      } else {
        setManual([])
        setManualLoad("failed")
        setManualError(
          savedRes && !savedRes.ok
            ? `Models without a schedule could not be listed (HTTP ${savedRes.status}).`
            : "Models without a schedule could not be listed.",
        )
      }
      if (background) setError(null)
    } catch {
      if (!background && !isStale()) setError("Could not reach the server to load models.")
    } finally {
      if (background) setBackgroundRefetches((n) => n + 1)
      else if (seq === foregroundLoads.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // A workspace switch (the header switcher or another tab) clears the list and asks
  // again: this is a client page, so nothing else re-reads it. Emptying the rows also
  // stops the live-state polls, which start again with the new workspace's rows.
  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setItems([])
        setManual([])
        setManualError(null)
        setManualLoad("all")
        setEditQuery(null)
        setEditSchedule(null)
        void load()
      }),
    [load],
  )

  // next_run_at is an instant computed server-side, so once it passes the row has
  // nothing true to show until the server is asked for the following one. Re-ask a
  // few seconds after the earliest comes due; the new rows re-arm this for the next.
  useEffect(() => {
    const delay = nextRunRefetchDelay(items.map((s) => s.next_run_at))
    if (delay === null) return
    const timer = setTimeout(() => void load({ background: true }), delay)
    return () => clearTimeout(timer)
  }, [items, load, backgroundRefetches])

  const refreshAll = useCallback(() => {
    void load()
    void refreshRunning()
    void refreshFreshness()
  }, [load, refreshRunning, refreshFreshness])

  return (
    <div className="space-y-5">
      <PageHeader
        heading="Models"
        description="Every saved query in this workspace that builds a table or runs a statement: when it runs, and how it has been getting on."
      >
        {hasModels && (
          <Select value={shownLayer} onValueChange={(v) => setLayerFilter(v as LayerFilter)}>
            <SelectTrigger className="h-9 w-[160px]" aria-label="Layer">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All layers</SelectItem>
              {layerChoices.map((name) => (
                <SelectItem key={name} value={layerFilterFor(name)}>
                  {name}
                </SelectItem>
              ))}
              <SelectItem value="none">No layer</SelectItem>
            </SelectContent>
          </Select>
        )}
        <Button variant="outline" size="sm" onClick={refreshAll} disabled={loading}>
          <RefreshCw className={`h-4 w-4 mr-2 ${loading ? "animate-spin" : ""}`} />
          Refresh
        </Button>
      </PageHeader>

      {!loading && !error && hasModels && (
        <LiveStateBanner
          running={running}
          freshness={freshness}
          liveRelevant={hasUpstreamTriggered}
          unlistedBreaches={unlistedBreaches}
        />
      )}

      {loading ? (
        <StatCardsSkeleton count={5} label="Loading model counts…" className="gap-3 grid-cols-2 @3xl:grid-cols-5" />
      ) : (
        !error &&
        hasModels && (
          <ModelCounts
            scheduled={shownItems}
            manual={shownManual}
            schedulesCapped={items.length >= SCHEDULE_LIST_LIMIT}
            manualLoad={manualLoad}
            layerFiltered={shownLayer !== "all"}
          />
        )
      )}

      {/* Every model's runs per day, from the counts Home's charts use. Not filtered by
          layer, and captioned so: the gateway counts the workspace, not a layer. */}
      {!loading && !error && hasModels && (
        <section aria-labelledby="models-activity-heading">
          <RunCharts
            only="models"
            heading={
              <h2 id="models-activity-heading" className="text-base font-semibold text-zinc-900 dark:text-white">
                Model activity
              </h2>
            }
          />
        </section>
      )}

      <Card className="p-0 overflow-hidden">
        {loading ? (
          <RowsSkeleton rows={4} label="Loading models…" className="px-4" />
        ) : error ? (
          <div className="flex flex-col items-center gap-2 py-12 text-center">
            <AlertTriangle className="h-5 w-5 text-red-600 dark:text-red-400" />
            <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
            <p className="text-xs text-zinc-500 dark:text-zinc-400">
              Any models you have are unaffected, and scheduled ones may still be running.
            </p>
            <Button size="sm" variant="outline" onClick={() => void load()}>
              Retry
            </Button>
          </div>
        ) : !hasModels ? (
          <EmptyState
            icon={Boxes}
            title={manualError ? "Nothing is scheduled in this workspace yet." : "No models in this workspace yet."}
            description={
              <>
                Save a query in the Explorer, then use <span className="font-medium">Schedule</span> on it — either
                to build a table from its results, or to run a MERGE, UPDATE or INSERT as written, on a cadence or
                by hand.
                {manualError && (
                  <span className="mt-2 block text-xs text-amber-600 dark:text-amber-400">{manualError}</span>
                )}
              </>
            }
            action={
              <Button size="sm" variant="outline" asChild>
                <Link href="/explorer">
                  <Search className="h-4 w-4 mr-2" />
                  Open Explorer
                </Link>
              </Button>
            }
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Model</TableHead>
                <TableHead>Cadence</TableHead>
                <TableHead>Now</TableHead>
                <TableHead>Next run</TableHead>
                <TableHead>Last run</TableHead>
                <TableHead className={`w-12 ${STICKY_ACTIONS}`}>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {shownItems.length === 0 && shownManual.length === 0 && (
                <TableRow>
                  <TableCell colSpan={6} className="py-8 text-center text-sm text-zinc-500 dark:text-zinc-400">
                    No model is in this layer.
                  </TableCell>
                </TableRow>
              )}
              {shownItems.map((s) => {
                const href = `/models/${s.saved_query_id}`
                const cell = liveCellFor(s.schedule_type, s.saved_query_id, running)
                const breach = openBreachFor(s.saved_query_id, openBreaches)
                const cron = cadenceCron(s)
                return (
                  <TableRow
                    key={s.schedule_id}
                    className="group cursor-pointer"
                    // The name below is the link keyboards and new tabs use; the rest of
                    // the row is a larger target for the same page.
                    onClick={() => router.push(href)}
                  >
                    <TableCell className="align-top">
                      <Link
                        href={href}
                        className="font-medium text-sm hover:underline"
                        title={`Open ${s.name}: full run history, details and controls`}
                        // The row navigates too. Without this a click here would route
                        // twice, and a Cmd-click would open the tab and also leave this one.
                        onClick={(e) => e.stopPropagation()}
                      >
                        {s.name}
                      </Link>
                      <LayerBadge layer={s.layer} position={layerPosition(layers, s.layer)} className="ml-1.5 align-middle" />
                      <div className="text-xs text-zinc-500 dark:text-zinc-400 font-mono truncate max-w-[220px]">
                        {s.materialization === "statement"
                          ? "runs its SQL as written"
                          : s.target_table || "no target table"}
                      </div>
                      {s.connection_name && (
                        <div className="text-xs text-zinc-400">{s.connection_name}</div>
                      )}
                    </TableCell>
                    {/* A cron put into words keeps its expression on hover; one that could
                        not be is shown as typed, in monospace like any other code. The
                        schedule's status sits under it: when, and whether it is on. */}
                    <TableCell className="align-top text-xs">
                      <div
                        className={cron && !cron.worded ? "font-mono" : undefined}
                        title={cron?.worded ? cron.expression : undefined}
                      >
                        {describeCadence(s)}
                      </div>
                      <div className="mt-1">{statusBadge(s)}</div>
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <LiveStateCell cell={cell} waitsForAll={waitsForAll(s.upstream_policy, s.upstreams)} />
                        {breach && (
                          <OverdueBadge
                            breach={breach}
                            schedule={{ scheduleType: s.schedule_type, upstreams: s.upstreams, upstreamPolicy: s.upstream_policy }}
                          />
                        )}
                      </div>
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      {s.next_run_at ? (
                        <span title={formatAbsoluteTime(s.next_run_at)}>
                          {formatNextRunOrDue(s.next_run_at, formatAbsoluteTime)}
                        </span>
                      ) : s.schedule_type === "after_upstream" && s.status === "active" ? (
                        // Distinct from the bare dash below. This one has no time by
                        // construction, and saying so stops it reading as a fault. Active
                        // only: the fire path skips any other status, so on a paused
                        // schedule this would promise a run that never comes.
                        <span className="text-zinc-400">{describeUpstreamNextRun(s)}</span>
                      ) : (
                        // No next run. Either the schedule is not active (the gateway
                        // sends next_run_at for active ones only, so a paused clock
                        // schedule lands here too) or its time could not be worked out.
                        <span className="text-zinc-400">—</span>
                      )}
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      {s.last_run_at ? (
                        // The same time-and-badge pair as the Details card on the
                        // query's page. A failure's message is on hover; the page has
                        // it in full.
                        <div
                          className="flex flex-wrap items-center gap-1.5"
                          title={s.last_run_status === "failed" ? s.last_run_error || "The last run failed" : undefined}
                        >
                          <span className="text-zinc-500 dark:text-zinc-400">{formatAbsoluteTime(s.last_run_at)}</span>
                          {s.last_run_status && runStatusBadge(s.last_run_status)}
                        </div>
                      ) : (
                        <span className="text-zinc-400">never run</span>
                      )}
                    </TableCell>
                    <ModelActions
                      name={s.name}
                      role={workspaceRole}
                      canEditQuery={canEditSavedQuery(workspaceRole, s.created_by, user?.id)}
                      canSchedule={canSchedule}
                      scheduled
                      onEditQuery={() => setEditQuery(s)}
                      onSchedule={() => setEditSchedule(s)}
                    />
                  </TableRow>
                )
              })}
              {shownManual.map((m) => {
                const href = `/models/${m.id}`
                const breach = openBreachFor(m.id, openBreaches)
                const canEdit = canEditSavedQuery(workspaceRole, m.created_by, user?.id)
                return (
                  <TableRow key={`manual-${m.id}`} className="group cursor-pointer" onClick={() => router.push(href)}>
                    <TableCell className="align-top">
                      <Link
                        href={href}
                        className="font-medium text-sm hover:underline"
                        title={`Open ${m.name}: full run history, details and controls`}
                        onClick={(e) => e.stopPropagation()}
                      >
                        {m.name}
                      </Link>
                      <LayerBadge layer={m.layer} position={layerPosition(layers, m.layer)} className="ml-1.5 align-middle" />
                      <div className="text-xs text-zinc-500 dark:text-zinc-400 font-mono truncate max-w-[220px]">
                        {m.materialization === "statement"
                          ? "runs its SQL as written"
                          : m.target_table || "no target table"}
                      </div>
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      <div title="No schedule: it runs only when someone runs it">Manual</div>
                      <div className="mt-1">
                        <StatusPill tone="skip">Not scheduled</StatusPill>
                      </div>
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      {breach ? <OverdueBadge breach={breach} /> : <span className="text-zinc-400">—</span>}
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      <span className="text-zinc-400">—</span>
                    </TableCell>
                    <TableCell className="align-top text-xs">
                      {m.last_run_at ? (
                        <div
                          className="flex flex-wrap items-center gap-1.5"
                          title={m.last_run_status === "failed" ? m.last_run_error || "The last run failed" : undefined}
                        >
                          <span className="text-zinc-500 dark:text-zinc-400">{formatAbsoluteTime(m.last_run_at)}</span>
                          {m.last_run_status && runStatusBadge(m.last_run_status)}
                        </div>
                      ) : (
                        <span className="text-zinc-400">never run</span>
                      )}
                    </TableCell>
                    <ModelActions
                      name={m.name}
                      role={workspaceRole}
                      canEditQuery={canEdit}
                      canSchedule={canSchedule}
                      scheduled={false}
                      onEditQuery={() => setEditQuery(manualTarget(m))}
                      onSchedule={() => setEditSchedule(manualTarget(m))}
                    />
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>

      {!loading && !error && hasModels && manualError && (
        <p className="text-xs text-amber-600 dark:text-amber-400">{manualError}</p>
      )}

      {editQuery && (
        <SavedQueryEditDialog
          key={`edit-query-${editQuery.saved_query_id}`}
          savedQueryId={editQuery.saved_query_id}
          open
          onOpenChange={(next) => {
            if (!next) setEditQuery(null)
          }}
          // The class and the last-run columns are both derived from the SQL, so an
          // edit can change what this table is showing.
          onSaved={() => void load()}
        />
      )}

      {editSchedule && (
        <SavedQueryModelDialog
          key={`edit-schedule-${editSchedule.saved_query_id}`}
          savedQueryId={editSchedule.saved_query_id}
          savedQueryName={editSchedule.name}
          materialization={editSchedule.materialization}
          targetTable={editSchedule.target_table}
          layer={editSchedule.layer}
          statementClass={editSchedule.statement_class}
          supportsMaterialization={editSchedule.supports_materialization}
          connectorType={editSchedule.connector_type}
          connectionId={editSchedule.connection_id}
          lastRunStatus={editSchedule.last_run_status}
          lastRunError={editSchedule.last_run_error}
          open
          onOpenChange={(next) => {
            if (!next) setEditSchedule(null)
          }}
          onChanged={() => void load()}
        />
      )}
    </div>
  )
}

/**
 * The five counts over the rows the table lists (so they follow the layer filter):
 * how many models, how many run on a schedule or are paused, how many failed last
 * time, and how many run only by hand. A list that stopped at its cap makes a floor.
 */
function ModelCounts({
  scheduled,
  manual,
  schedulesCapped,
  manualLoad,
  layerFiltered,
}: {
  scheduled: ScheduledQuery[]
  manual: ManualModel[]
  schedulesCapped: boolean
  manualLoad: ManualLoad
  layerFiltered: boolean
}) {
  const manualPartial = manualLoad !== "all"
  const failed = [...scheduled, ...manual].filter((m) => m.last_run_status === "failed").length
  const failedPartial = schedulesCapped || manualPartial
  return (
    <div className="grid grid-cols-2 gap-3 @3xl:grid-cols-5" data-testid="model-counts">
      <StatCard
        title="Total"
        icon={Boxes}
        value={atLeast(scheduled.length + manual.length, schedulesCapped || manualPartial)}
        hint={layerFiltered ? "In this layer" : "Scheduled and manual"}
      />
      <StatCard
        title="Active"
        icon={CirclePlay}
        value={atLeast(scheduled.filter((s) => s.status === "active").length, schedulesCapped)}
        hint="Run on a clock or after upstreams"
      />
      <StatCard
        title="Paused"
        icon={CirclePause}
        value={atLeast(scheduled.filter((s) => s.status === "paused").length, schedulesCapped)}
        hint="Won't run until resumed"
      />
      <StatCard
        title="Last run failed"
        icon={XCircle}
        value={atLeast(failed, failedPartial)}
        tone={failed > 0 ? "bad" : "default"}
        hint={
          failed > 0
            ? "Hover a row's last run for the error"
            : // A floor of 0 is no promise: a model not on the list may have failed.
              failedPartial
              ? "None of the listed models' last run failed"
              : "No model's last run failed"
        }
      />
      <StatCard
        title="Manual"
        icon={Hand}
        // None of the unscheduled half loaded: their number is unknown, not 0.
        value={manualLoad === "failed" ? undefined : atLeast(manual.length, manualLoad === "capped")}
        hint="Run only when someone runs them"
      />
    </div>
  )
}

/**
 * A row's ⋮ menu. Two things a row can be wrong about, so two items rather than one
 * ambiguous "Edit": the SQL that runs, and when it runs. The cell stops a click and a
 * pointer-down so neither also opens the model's page under the dialog; the menu is
 * uncontrolled, as PipelinesTable's is, so Radix keeps its own pointer handling. An
 * item the role cannot use stays in the menu, greyed, saying why.
 */
function ModelActions({
  name,
  role,
  canEditQuery,
  canSchedule,
  scheduled,
  onEditQuery,
  onSchedule,
}: {
  name: string
  role: string | null | undefined
  canEditQuery: boolean
  canSchedule: boolean
  scheduled: boolean
  onEditQuery: () => void
  onSchedule: () => void
}) {
  const yourRole = `Your role is ${role || "unknown"}.`
  return (
    <TableCell
      className={`align-top text-right ${STICKY_ACTIONS_CELL}`}
      onClick={(e) => e.stopPropagation()}
      onPointerDown={(e) => e.stopPropagation()}
    >
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="ghost"
            size="icon"
            className="h-7 w-7"
            aria-label={`Actions for ${name}`}
            title={`Actions for ${name}`}
          >
            <MoreVertical className="h-4 w-4" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" sideOffset={6} className="w-64">
          <DropdownMenuItem disabled={!canEditQuery} onClick={onEditQuery}>
            <SquarePen className="h-4 w-4 mr-2 shrink-0" />
            <MenuText
              label="Edit query"
              note={
                canEditQuery
                  ? "Its name, SQL or sharing"
                  : `Only the query's creator or a workspace admin can change it. ${yourRole}`
              }
            />
          </DropdownMenuItem>
          <DropdownMenuItem disabled={!canSchedule} onClick={onSchedule}>
            <Clock className="h-4 w-4 mr-2 shrink-0" />
            <MenuText
              label={scheduled ? "Edit schedule" : "Schedule"}
              note={
                canSchedule
                  ? scheduled
                    ? "Cadence, pause, resume or remove the schedule"
                    : "Give this model a schedule"
                  : `Scheduling needs the admin role or higher. ${yourRole}`
              }
            />
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </TableCell>
  )
}

function MenuText({ label, note }: { label: string; note: string }) {
  return (
    <span className="flex min-w-0 flex-col text-left">
      <span>{label}</span>
      <span className="text-xs text-zinc-500 dark:text-zinc-400">{note}</span>
    </span>
  )
}
