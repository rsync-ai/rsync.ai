"use client"

/**
 * A dashboard (dashboards-design.md § 4, § 7.2): saved charts and text on a
 * 12-column grid, with its filters in one row above them. Each chart tile is read
 * live when the page opens, with the viewer's rights, and says when it read its
 * rows (§ 4.3); a tile whose source has no column for a filter is drawn unfiltered
 * by it and says so.
 *
 * Owners and admins edit it: drag a tile by its handle, resize it from its right or
 * bottom edge or its corner, or pick its width from its width menu; add a saved chart
 * (it takes the first space it fits) or a text tile, add up to 4 filters, and save. The server
 * checks the whole dashboard again (charts/dashboard.go) and refuses a save made
 * on a version someone else has since saved. Every save keeps the dashboard as it
 * was before it; History lists those versions and restores one as a new save
 * (§ 7.7). ?change=<id> (the link plan_dashboard_change gives) shows that proposed
 * change to one tile above the grid, to apply (§ 7.6).
 *
 * Schedule email (§ 8, D3) opens Ask rsync.ai about this dashboard with a request
 * to email it, which drafts a workflow with a dashboard step; shown to owners and
 * admins, who may draft workflows, where Ask rsync.ai is switched on.
 */

import Link from "next/link"
import { useRouter } from "next/navigation"
import {
  CalendarClock,
  ChevronLeft,
  Ellipsis,
  FileDown,
  GripVertical,
  History,
  Loader2,
  MoveHorizontal,
  Pencil,
  Plus,
  Save,
  Trash2,
  X,
} from "lucide-react"
import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { toast } from "sonner"

import { TYPE_LABEL } from "@/components/charts/chart-form"
import { ChartExportMenu } from "@/components/charts/ChartExportMenu"
import { ChartInfo } from "@/components/charts/ChartInfo"
import { SavedChart } from "@/components/charts/SavedChart"
import { DashboardChangeReview } from "@/components/dashboards/DashboardChangeReview"
import { DRAG_HANDLE } from "@/components/dashboards/DashboardGridEditor"
import { DashboardGridEditorNoSSR } from "@/components/dashboards/DashboardGridEditorNoSSR"
import {
  GRID_GAP,
  NEW_CHART_TILE,
  NEW_TEXT_TILE,
  ROW_HEIGHT,
  TILE_WIDTHS,
  activePicks,
  draftTiles,
  filterId,
  firstFreePlace,
  ignoredLabels,
  minTileRows,
  nextRow,
  readingOrder,
  tileHeight,
  tileInputs,
  widthLabel,
  type DraftTile,
} from "@/components/dashboards/dashboard-layout"
import { DashboardHistoryDialog } from "@/components/dashboards/DashboardHistoryDialog"
import { FilterBar } from "@/components/dashboards/FilterBar"
import { StatusPill } from "@/components/kit/StatusPill"
import { PageHeader } from "@/components/layout/PageHeader"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Input } from "@/components/ui/input"
import { useFeatureFlags } from "@/config/features"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import { ChartError, listCharts, type ChartRecord, type ChartVisibility } from "@/lib/api/charts"
import {
  MAX_FILTERS,
  MAX_TEXT_TILE,
  MAX_TILES,
  createDashboard,
  deleteDashboard,
  drawTile,
  exportDashboardPdf,
  getDashboard,
  updateDashboard,
  type Dashboard,
  type DashboardFilter,
  type DashboardFilterKind,
  type DashboardFilterValue,
  type DashboardTile,
  type TileDraw,
} from "@/lib/api/dashboards"
import { onDashboardChanged } from "@/lib/events/dashboardChanged"
import { downloadBlob } from "@/lib/export/download"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/** What Schedule email offers to Tab into Ask rsync.ai: a workflow that emails this dashboard. */
export const SCHEDULE_EMAIL_ASK = "Email this dashboard to me every Monday at 9:00"

/** Tiles read at once: a dashboard of 30 tiles does not send 30 queries to one source together. */
const DRAW_CONCURRENCY = 4

const selectClass =
  "h-9 rounded-md border border-zinc-200 bg-white px-2 text-sm text-zinc-900 disabled:opacity-50 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-100"
const labelClass = "text-xs font-medium text-zinc-600 dark:text-zinc-400"

interface TileState {
  draw?: TileDraw
  error?: string
  loading: boolean
}

/** A filter in the editor: a saved one keeps its id, a new one gets one from its label on save. */
interface DraftFilter {
  key: string
  id?: string
  kind: DashboardFilterKind
  label: string
  column: string
}

interface Draft {
  name: string
  description: string
  visibility: ChartVisibility
  filters: DraftFilter[]
  tiles: DraftTile[]
}

const EMPTY_DRAFT: Draft = { name: "", description: "", visibility: "private", filters: [], tiles: [] }

function draftOf(d: Dashboard): Draft {
  return {
    name: d.name,
    description: d.description,
    visibility: d.visibility,
    filters: d.filters.map((f) => ({ key: f.id, ...f })),
    tiles: draftTiles(d.tiles ?? []),
  }
}

let newKey = 0
const nextKey = (prefix: string) => `${prefix}-${++newKey}`

export function DashboardPage({ dashboardId, changeId }: { dashboardId?: string; changeId?: string | null }) {
  const router = useRouter()
  const { meets, isLoading: roleLoading } = useWorkspaceRole()
  const { assistant } = useFeatureFlags()
  const [version, setVersion] = useState(0)

  const [dashboard, setDashboard] = useState<Dashboard | null>(null)
  const [canManage, setCanManage] = useState<boolean | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  const [picks, setPicks] = useState<Record<string, DashboardFilterValue>>({})
  const [tiles, setTiles] = useState<Record<string, TileState>>({})

  const [draft, setDraft] = useState<Draft | null>(dashboardId ? null : EMPTY_DRAFT)
  const [charts, setCharts] = useState<ChartRecord[] | null>(null)
  const [chartsError, setChartsError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [historyOpen, setHistoryOpen] = useState(false)
  const [exporting, setExporting] = useState(false)

  const editing = draft !== null
  const manage = dashboardId ? canManage : roleLoading ? null : meets("admin")

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setDashboard(null)
        setCanManage(null)
        setPicks({})
        setTiles({})
        setCharts(null)
        setDraft(dashboardId ? null : EMPTY_DRAFT)
        setVersion((v) => v + 1)
      }),
    [dashboardId],
  )

  // A change applied to this dashboard, from the card above it or Ask rsync.ai's
  // panel beside it, reads the dashboard again. An editor's draft is kept: its
  // save is refused as out of date, and the dashboard reads itself again when the
  // editing ends.
  const [outdated, setOutdated] = useState(false)
  useEffect(
    () =>
      onDashboardChanged((id) => {
        if (id === dashboardId) setOutdated(true)
      }),
    [dashboardId],
  )
  useEffect(() => {
    if (!outdated || editing) return
    setOutdated(false)
    setTiles({})
    setVersion((v) => v + 1)
  }, [outdated, editing])

  useEffect(() => {
    if (!dashboardId) return
    let alive = true
    const isStale = captureWorkspace()
    setLoadError(null)
    getDashboard(dashboardId).then(
      (got) => {
        if (!alive || isStale()) return
        setDashboard(got.dashboard)
        setCanManage(got.can_manage)
      },
      (err: unknown) => {
        if (alive && !isStale()) setLoadError(err instanceof Error ? err.message : "Could not load the dashboard")
      },
    )
    return () => {
      alive = false
    }
  }, [dashboardId, version])

  const filters = useMemo(() => dashboard?.filters ?? [], [dashboard])
  const sent = useMemo(() => activePicks(filters, picks), [filters, picks])
  const sentKey = JSON.stringify(sent)

  // Each chart tile, read live with the filters picked; a few at a time. A new
  // pick or a save starts over, and a draw that comes back after that is dropped.
  useEffect(() => {
    if (!dashboard || editing) return
    let alive = true
    const isStale = captureWorkspace()
    const queue = readingOrder((dashboard.tiles ?? []).filter((t) => t.kind === "chart" && t.chart))
    const filtersSent = JSON.parse(sentKey) as Record<string, DashboardFilterValue>
    setTiles((prev) => {
      const next: Record<string, TileState> = {}
      for (const t of queue) next[t.id] = { ...prev[t.id], error: undefined, loading: true }
      return next
    })
    const worker = async () => {
      for (let t = queue.shift(); t; t = queue.shift()) {
        const tile = t
        try {
          const draw = await drawTile(dashboard.id, tile.id, filtersSent)
          if (!alive || isStale()) return
          setTiles((prev) => ({ ...prev, [tile.id]: { draw, loading: false } }))
        } catch (err) {
          if (!alive || isStale()) return
          const message = err instanceof Error ? err.message : "Could not draw the tile"
          setTiles((prev) => ({ ...prev, [tile.id]: { draw: prev[tile.id]?.draw, error: message, loading: false } }))
        }
      }
    }
    for (let i = 0; i < Math.min(DRAW_CONCURRENCY, queue.length); i++) void worker()
    return () => {
      alive = false
    }
  }, [dashboard, editing, sentKey])

  // The saved charts an owner or admin can put on the dashboard.
  useEffect(() => {
    if (!editing || charts !== null) return
    let alive = true
    const isStale = captureWorkspace()
    setChartsError(null)
    listCharts().then(
      (got) => {
        if (alive && !isStale()) setCharts(got.charts)
      },
      (err: unknown) => {
        if (alive && !isStale()) setChartsError(err instanceof Error ? err.message : "Could not load the charts")
      },
    )
    return () => {
      alive = false
    }
  }, [editing, charts, version])

  const pick = useCallback((id: string, value: DashboardFilterValue | null) => {
    setPicks((p) => {
      const next = { ...p }
      if (value) next[id] = value
      else delete next[id]
      return next
    })
  }, [])

  if (loadError) {
    return (
      <div className="space-y-4">
        <PageHeader heading="Dashboard" />
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {loadError}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
        <Link href="/dashboards" className="text-sm underline">
          All dashboards
        </Link>
      </div>
    )
  }

  if (manage === null || (dashboardId && !dashboard)) {
    return (
      <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-4 w-4 animate-spin" /> Loading the dashboard…
      </div>
    )
  }

  if (!dashboardId && !manage) {
    return (
      <div className="space-y-4">
        <PageHeader heading="New dashboard" />
        <p className="text-sm text-zinc-600 dark:text-zinc-400">
          Workspace owners and admins make dashboards. You can open every dashboard shared with the workspace.
        </p>
        <Link href="/dashboards" className="text-sm underline">
          All dashboards
        </Link>
      </div>
    )
  }

  if (draft) {
    return (
      <DashboardEditor
        dashboard={dashboard}
        draft={draft}
        setDraft={setDraft as (f: (d: Draft) => Draft) => void}
        charts={charts}
        chartsError={chartsError}
        tiles={tiles}
        saving={saving}
        saveError={saveError}
        onCancel={() => {
          setSaveError(null)
          if (dashboard) setDraft(null)
          else router.push("/dashboards")
        }}
        onSave={async () => {
          setSaveError(null)
          const problem = draftProblem(draft)
          if (problem) {
            setSaveError(problem)
            return
          }
          const taken: string[] = draft.filters.flatMap((f) => (f.id ? [f.id] : []))
          const outFilters: DashboardFilter[] = draft.filters.map((f) => {
            const id = f.id ?? filterId(f.label, taken)
            if (!f.id) taken.push(id)
            return { id, kind: f.kind, label: f.label.trim(), column: f.column }
          })
          const input = {
            name: draft.name.trim(),
            description: draft.description.trim(),
            visibility: draft.visibility,
            filters: outFilters,
            tiles: tileInputs(draft.tiles),
          }
          setSaving(true)
          try {
            if (dashboard) {
              const saved = await updateDashboard(dashboard.id, { ...input, updated_at: dashboard.updated_at })
              setDashboard(saved)
              setTiles({})
              setDraft(null)
              toast.success("Dashboard saved")
            } else {
              const saved = await createDashboard(input)
              toast.success("Dashboard saved")
              router.replace(`/dashboards/${saved.id}`)
            }
          } catch (err) {
            if (err instanceof ChartError && err.code === "stale") {
              setSaveError(
                "Someone saved this dashboard after you opened it. Reload the page to see their change; yours is not saved.",
              )
            } else {
              setSaveError(err instanceof Error ? err.message : "Could not save the dashboard")
            }
          } finally {
            setSaving(false)
          }
        }}
        onDelete={dashboard ? () => setConfirmDelete(true) : undefined}
      >
        <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>Delete this dashboard?</AlertDialogTitle>
              <AlertDialogDescription>
                Its tiles and filters go with it. The charts on it stay, on the Charts page.
              </AlertDialogDescription>
            </AlertDialogHeader>
            <AlertDialogFooter>
              <AlertDialogCancel>Cancel</AlertDialogCancel>
              <AlertDialogAction
                onClick={async () => {
                  if (!dashboard) return
                  setConfirmDelete(false)
                  try {
                    await deleteDashboard(dashboard.id)
                    toast.success("Dashboard deleted")
                    router.push("/dashboards")
                  } catch (err) {
                    setSaveError(err instanceof Error ? err.message : "Could not delete the dashboard")
                  }
                }}
              >
                Delete
              </AlertDialogAction>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </DashboardEditor>
    )
  }

  const d = dashboard!
  const ordered = readingOrder(d.tiles ?? [])
  return (
    <div className="space-y-4">
      <div className="space-y-1.5">
        <Link
          href="/dashboards"
          className="inline-flex items-center gap-1 text-xs text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200"
        >
          <ChevronLeft className="h-3.5 w-3.5" aria-hidden /> Dashboards
        </Link>
        <PageHeader heading={d.name} description={d.description || undefined}>
          {canManage && (
            <Button
              size="sm"
              onClick={() => {
                setSaveError(null)
                setDraft(draftOf(d))
              }}
            >
              <Pencil className="mr-1.5 h-4 w-4" aria-hidden /> Edit
            </Button>
          )}
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button size="sm" variant="outline" aria-label="Dashboard actions" title="Dashboard actions">
                {exporting ? (
                  <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                ) : (
                  <Ellipsis className="h-4 w-4" aria-hidden />
                )}
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onSelect={() => setHistoryOpen(true)}>
                <History className="mr-2 h-4 w-4" aria-hidden /> History
              </DropdownMenuItem>
              <DropdownMenuItem
                disabled={exporting}
                onSelect={async () => {
                  // The PDF draws every tile again on the server, with the filters picked here.
                  setExporting(true)
                  try {
                    const { blob, filename } = await exportDashboardPdf(d.id, sent)
                    downloadBlob(blob, filename)
                  } catch (err) {
                    toast.error(err instanceof Error && err.message ? err.message : "Could not export the dashboard")
                  } finally {
                    setExporting(false)
                  }
                }}
              >
                <FileDown className="mr-2 h-4 w-4" aria-hidden /> {exporting ? "Exporting…" : "Export PDF"}
              </DropdownMenuItem>
              {assistant && !roleLoading && meets("admin") && (
                <DropdownMenuItem
                  onSelect={() => {
                    // A new chat, so the request is offered rather than an old thread shown.
                    const store = useAssistantStore.getState()
                    store.newChat()
                    store.openWith({ kind: "dashboard", id: d.id, name: d.name }, SCHEDULE_EMAIL_ASK)
                  }}
                >
                  <CalendarClock className="mr-2 h-4 w-4" aria-hidden /> Schedule email
                </DropdownMenuItem>
              )}
            </DropdownMenuContent>
          </DropdownMenu>
        </PageHeader>
        <p className="flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs text-zinc-500 dark:text-zinc-400">
          <span>{d.visibility === "workspace" ? "Workspace" : "Only me"}</span>
          <span aria-hidden>·</span>
          <span>
            {ordered.length} {ordered.length === 1 ? "tile" : "tiles"}
          </span>
          <span aria-hidden>·</span>
          {/* The layout's last edit, not the data's: "updated" read as how fresh the numbers are. */}
          <span title={formatAbsoluteTime(d.updated_at)}>Edited {formatRelativeTime(d.updated_at)}</span>
          {d.proposed_by !== "none" && (
            <StatusPill tone="vio" className="ml-1">
              Drafted by {d.proposed_by === "agent" ? "an agent" : "Ask rsync.ai"}
            </StatusPill>
          )}
        </p>
      </div>

      <DashboardHistoryDialog
        dashboard={d}
        open={historyOpen}
        onOpenChange={setHistoryOpen}
        onRestored={(saved) => {
          setDashboard(saved)
          setTiles({})
        }}
      />

      {changeId && <DashboardChangeReview changeId={changeId} hideOpenLink />}

      <FilterBar dashboardId={d.id} filters={d.filters} picks={picks} onChange={pick} />

      {ordered.length === 0 ? (
        <p className="text-sm text-zinc-500 dark:text-zinc-400">
          No tiles yet.{canManage ? " Edit the dashboard to add charts and text." : ""}
        </p>
      ) : (
        // One column on a phone, a tile to a row, as tall as its rows on a wide screen; a
        // text tile grows to fit its text, which wraps to more lines on a narrow screen.
        <div
          className="grid grid-cols-1 @xl:grid-cols-12 @xl:[grid-auto-rows:var(--row-h)]"
          style={{ "--row-h": `${ROW_HEIGHT}px`, gap: GRID_GAP } as React.CSSProperties}
          data-testid="dashboard-grid"
        >
          {ordered.map((t) => (
            <div
              key={t.id}
              className={`min-w-0 @xl:[grid-column:var(--tile-col)] @xl:[grid-row:var(--tile-row)] ${
                t.kind === "text" ? "min-h-[var(--tile-h)] @xl:min-h-0" : "h-[var(--tile-h)] @xl:h-auto"
              }`}
              style={
                {
                  "--tile-h": `${tileHeight(t.h)}px`,
                  "--tile-col": `${t.x + 1} / span ${t.w}`,
                  "--tile-row": `${t.y + 1} / span ${t.h}`,
                } as React.CSSProperties
              }
            >
              {t.kind === "text" ? (
                <TextTile body={t.body ?? ""} />
              ) : (
                <ChartTile tile={t} state={tiles[t.id]} filters={d.filters} />
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

/** What the editor still needs before a save, in words; null when nothing. */
function draftProblem(draft: Draft): string | null {
  if (!draft.name.trim()) return "Name the dashboard."
  const text = draft.tiles.findIndex((t) => t.kind === "text" && !t.body.trim())
  if (text >= 0) return "A text tile is empty: write its text or remove it."
  for (const f of draft.filters) {
    if (!f.label.trim()) return "Each filter needs a label."
    if (!f.column) return `Pick the column the ${f.label.trim()} filter is on.`
  }
  return null
}

function TextTile({ body }: { body: string }) {
  return (
    <div className="h-full overflow-auto whitespace-pre-wrap rounded-xl border border-zinc-200 bg-white p-3 text-sm text-zinc-700 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-300">
      {body}
    </div>
  )
}

function ChartTile({ tile, state, filters }: { tile: DashboardTile; state?: TileState; filters: DashboardFilter[] }) {
  const chart = tile.chart
  const ignored = ignoredLabels(state?.draw?.filters.ignored ?? [], filters)
  const unfiltered = ignoredLabels(state?.draw?.filters.unfiltered ?? [], filters)
  const settings = state?.draw?.chart?.settings ?? chart?.settings
  const box = useRef<HTMLElement>(null)
  return (
    <section
      ref={box}
      aria-label={chart?.name ?? "A chart you cannot see"}
      className="flex h-full min-w-0 flex-col overflow-hidden rounded-xl border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-950"
    >
      <header className="mb-2 flex items-center justify-between gap-2">
        {chart ? (
          // A phone has no hover to show the title: its name wraps to two lines there.
          <Link
            href={`/charts/${chart.id}`}
            title={chart.name}
            className="min-w-0 line-clamp-2 break-words text-sm font-medium text-zinc-900 hover:underline @xl:block @xl:truncate dark:text-zinc-50"
          >
            {chart.name}
          </Link>
        ) : (
          <span className="min-w-0 truncate text-sm font-medium text-zinc-500 dark:text-zinc-400">A chart you cannot see</span>
        )}
        <div className="flex shrink-0 items-center gap-1">
          {state?.loading && state.draw && <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-zinc-400" aria-label="Reading" />}
          {chart && !state?.error && (
            <ChartInfo draw={state?.draw ?? null} ignored={ignored} unfiltered={unfiltered} type={settings?.type} />
          )}
          {chart && settings && !state?.error && state?.draw && (
            <ChartExportMenu name={chart.name} settings={settings} draw={state.draw} plot={box} />
          )}
        </div>
      </header>
      <div className="flex min-h-0 flex-1 flex-col overflow-auto">
        {!chart ? (
          <p role="status" className="text-sm text-zinc-600 dark:text-zinc-400">
            This tile&apos;s chart is private to its author, so it is not shown to you.
          </p>
        ) : state?.error ? (
          <p role="alert" className="text-sm text-red-700 dark:text-red-300">
            {state.error}
          </p>
        ) : (
          <SavedChart
            settings={settings ?? chart.settings}
            draw={state?.draw ?? null}
            loading={state?.loading ?? true}
            fill
            ignored={ignored}
            title={chart.name}
            rows={tile.h}
          />
        )}
      </div>
    </section>
  )
}

interface DashboardEditorProps {
  dashboard: Dashboard | null
  draft: Draft
  setDraft: (f: (d: Draft) => Draft) => void
  charts: ChartRecord[] | null
  chartsError: string | null
  tiles: Record<string, TileState>
  saving: boolean
  saveError: string | null
  onCancel: () => void
  onSave: () => void
  onDelete?: () => void
  children?: React.ReactNode
}

function DashboardEditor({
  dashboard,
  draft,
  setDraft,
  charts,
  chartsError,
  tiles,
  saving,
  saveError,
  onCancel,
  onSave,
  onDelete,
  children,
}: DashboardEditorProps) {
  const [addChart, setAddChart] = useState("")
  const chartById = useMemo(() => {
    const m = new Map<string, ChartRecord>()
    for (const t of dashboard?.tiles ?? []) if (t.chart) m.set(t.chart.id, t.chart)
    for (const c of charts ?? []) m.set(c.id, c)
    return m
  }, [dashboard, charts])

  // A filter is on a column of a chart on the dashboard (the server's rule 2).
  const columns = useMemo(() => {
    const set = new Set<string>()
    for (const t of draft.tiles) {
      if (t.kind !== "chart" || !t.chart_id) continue
      for (const c of chartById.get(t.chart_id)?.source_columns ?? []) set.add(c)
    }
    return [...set].sort((a, b) => a.localeCompare(b))
  }, [draft.tiles, chartById])

  const full = draft.tiles.length >= MAX_TILES
  const update = (patch: Partial<Draft>) => setDraft((d) => ({ ...d, ...patch }))
  const updateTile = (key: string, patch: Partial<DraftTile>) =>
    setDraft((d) => ({ ...d, tiles: d.tiles.map((t) => (t.key === key ? { ...t, ...patch } : t)) }))
  const updateFilter = (key: string, patch: Partial<DraftFilter>) =>
    setDraft((d) => ({ ...d, filters: d.filters.map((f) => (f.key === key ? { ...f, ...patch } : f)) }))

  const addChartTile = (chartId: string) => {
    if (!chartId || full) return
    setDraft((d) => ({
      ...d,
      tiles: [
        ...d.tiles,
        { key: nextKey("chart"), kind: "chart", chart_id: chartId, body: "", ...firstFreePlace(d.tiles, NEW_CHART_TILE.w, NEW_CHART_TILE.h), ...NEW_CHART_TILE },
      ],
    }))
    setAddChart("")
  }
  const addTextTile = () => {
    if (full) return
    setDraft((d) => ({
      ...d,
      tiles: [...d.tiles, { key: nextKey("text"), kind: "text", body: "", x: 0, y: nextRow(d.tiles), ...NEW_TEXT_TILE }],
    }))
  }

  const sharedSourceNote =
    draft.visibility === "workspace"
      ? "Shared with the workspace: every chart on it must be shared too."
      : "Only you see it until you share it with the workspace."

  return (
    <div className="space-y-4">
      <PageHeader
        heading={dashboard ? `Edit ${dashboard.name}` : "New dashboard"}
        description="Add saved charts and text, drag a tile by its handle, size it from its edges or its width menu, and add up to 4 filters."
      >
        <Button size="sm" variant="ghost" onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
        <Button size="sm" onClick={onSave} disabled={saving}>
          {saving ? <Loader2 className="mr-1.5 h-4 w-4 animate-spin" /> : <Save className="mr-1.5 h-4 w-4" />}
          Save dashboard
        </Button>
      </PageHeader>

      {saveError && (
        <p role="alert" className="rounded border border-red-200 bg-red-50 p-2 text-sm text-red-800 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-200">
          {saveError}
        </p>
      )}

      <section aria-label="About the dashboard" className="grid gap-3 rounded-lg border border-zinc-200 p-3 @xl:grid-cols-[1fr_2fr_auto] dark:border-zinc-800">
        <div className="space-y-1">
          <label htmlFor="dashboard-name" className={labelClass}>
            Name
          </label>
          <Input id="dashboard-name" value={draft.name} maxLength={200} onChange={(e) => update({ name: e.target.value })} />
        </div>
        <div className="space-y-1">
          <label htmlFor="dashboard-description" className={labelClass}>
            Description
          </label>
          <Input
            id="dashboard-description"
            value={draft.description}
            maxLength={1000}
            onChange={(e) => update({ description: e.target.value })}
          />
        </div>
        <div className="space-y-1">
          <label htmlFor="dashboard-visibility" className={labelClass}>
            Who sees it
          </label>
          <select
            id="dashboard-visibility"
            className={`${selectClass} w-full`}
            value={draft.visibility}
            onChange={(e) => update({ visibility: e.target.value as ChartVisibility })}
          >
            <option value="private">Only me</option>
            <option value="workspace">The workspace</option>
          </select>
        </div>
        <p className="text-[11px] text-zinc-500 @xl:col-span-3 dark:text-zinc-400">{sharedSourceNote}</p>
      </section>

      <section aria-label="Add tiles" className="flex flex-wrap items-end gap-3">
        <div className="space-y-1">
          <label htmlFor="dashboard-add-chart" className={labelClass}>
            Add a saved chart
          </label>
          <select
            id="dashboard-add-chart"
            className={`${selectClass} min-w-[16rem]`}
            value={addChart}
            disabled={charts === null || full}
            onChange={(e) => {
              setAddChart(e.target.value)
              addChartTile(e.target.value)
            }}
          >
            <option value="">{charts === null ? "Loading charts…" : charts.length === 0 ? "No saved charts yet" : "Pick a chart"}</option>
            {(charts ?? []).map((c) => (
              <option key={c.id} value={c.id}>
                {c.name} ({TYPE_LABEL[c.settings.type] ?? c.settings.type}
                {c.visibility === "private" ? ", only you" : ""})
              </option>
            ))}
          </select>
        </div>
        <Button type="button" variant="outline" size="sm" onClick={addTextTile} disabled={full}>
          <Plus className="mr-1.5 h-4 w-4" /> Add text
        </Button>
        <Link href="/charts/new" className="pb-2 text-sm text-zinc-600 underline dark:text-zinc-400">
          Make a new chart
        </Link>
        {full && <span className="pb-2 text-xs text-amber-700 dark:text-amber-300">A dashboard has at most {MAX_TILES} tiles.</span>}
        {chartsError && (
          <span role="alert" className="pb-2 text-xs text-red-700 dark:text-red-300">
            {chartsError}
          </span>
        )}
      </section>

      <section aria-label="Filters" className="space-y-2 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
        <div className="flex items-center justify-between gap-2">
          <h2 className="text-sm font-medium text-zinc-800 dark:text-zinc-200">Filters</h2>
          <Button
            type="button"
            size="sm"
            variant="outline"
            disabled={draft.filters.length >= MAX_FILTERS || columns.length === 0}
            onClick={() =>
              setDraft((d) => ({
                ...d,
                filters: [...d.filters, { key: nextKey("filter"), kind: "date_range", label: "", column: "" }],
              }))
            }
          >
            <Plus className="mr-1.5 h-4 w-4" /> Add filter
          </Button>
        </div>
        {columns.length === 0 ? (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">Add a chart first: a filter is on a column of a chart&apos;s source.</p>
        ) : draft.filters.length === 0 ? (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            No filters. A filter applies to every tile whose source has its column, as a parameter of its query.
          </p>
        ) : null}
        {draft.filters.map((f, i) => (
          <div key={f.key} className="flex flex-wrap items-end gap-2" role="group" aria-label={`Filter ${i + 1}`}>
            <div className="space-y-1">
              <label htmlFor={`filter-label-${f.key}`} className={labelClass}>
                Label
              </label>
              <Input
                id={`filter-label-${f.key}`}
                className="h-9 w-48"
                value={f.label}
                maxLength={60}
                onChange={(e) => updateFilter(f.key, { label: e.target.value })}
              />
            </div>
            <div className="space-y-1">
              <label htmlFor={`filter-kind-${f.key}`} className={labelClass}>
                Kind
              </label>
              <select
                id={`filter-kind-${f.key}`}
                className={selectClass}
                value={f.kind}
                onChange={(e) => updateFilter(f.key, { kind: e.target.value as DashboardFilterKind })}
              >
                <option value="date_range">Date range</option>
                <option value="values">List of values</option>
              </select>
            </div>
            <div className="space-y-1">
              <label htmlFor={`filter-column-${f.key}`} className={labelClass}>
                Column
              </label>
              <select
                id={`filter-column-${f.key}`}
                className={`${selectClass} min-w-[12rem]`}
                value={f.column}
                onChange={(e) => updateFilter(f.key, { column: e.target.value })}
              >
                <option value="">Pick a column</option>
                {f.column && !columns.includes(f.column) && <option value={f.column}>{f.column} (no chart has it)</option>}
                {columns.map((c) => (
                  <option key={c} value={c}>
                    {c}
                  </option>
                ))}
              </select>
            </div>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              aria-label={`Remove the filter ${f.label || i + 1}`}
              onClick={() => setDraft((d) => ({ ...d, filters: d.filters.filter((x) => x.key !== f.key) }))}
            >
              <X className="h-4 w-4" />
            </Button>
          </div>
        ))}
      </section>

      {draft.tiles.length === 0 ? (
        <p className="rounded-lg border border-dashed border-zinc-300 p-6 text-center text-sm text-zinc-500 dark:border-zinc-700 dark:text-zinc-400">
          No tiles yet. Add a saved chart or a text tile above.
        </p>
      ) : (
        <DashboardGridEditorNoSSR
          tiles={draft.tiles}
          onLayout={(places) =>
            setDraft((d) => {
              let moved = false
              const next = d.tiles.map((t) => {
                const p = places[t.key]
                if (!p || (p.x === t.x && p.y === t.y && p.w === t.w && p.h === t.h)) return t
                moved = true
                return { ...t, ...p }
              })
              return moved ? { ...d, tiles: next } : d
            })
          }
          minRows={(t) => minTileRows(t.chart_id ? chartById.get(t.chart_id)?.settings.type : undefined)}
          renderTile={(t, setWidth) => (
            <EditorTile
              tile={t}
              chart={t.chart_id ? chartById.get(t.chart_id) : undefined}
              state={t.id ? tiles[t.id] : undefined}
              filters={dashboard?.filters ?? []}
              onBody={(body) => updateTile(t.key, { body })}
              onWidth={setWidth}
              onRemove={() => setDraft((d) => ({ ...d, tiles: d.tiles.filter((x) => x.key !== t.key) }))}
            />
          )}
        />
      )}

      {onDelete && (
        <div className="border-t border-zinc-200 pt-4 dark:border-zinc-800">
          <Button type="button" variant="outline" size="sm" className="text-red-700 dark:text-red-300" onClick={onDelete}>
            <Trash2 className="mr-1.5 h-4 w-4" /> Delete dashboard
          </Button>
        </div>
      )}
      {children}
    </div>
  )
}

function EditorTile({
  tile,
  chart,
  state,
  filters,
  onBody,
  onWidth,
  onRemove,
}: {
  tile: DraftTile
  chart?: ChartRecord
  state?: TileState
  /** The saved dashboard's filters, which its tiles were drawn with. */
  filters: readonly DashboardFilter[]
  onBody: (body: string) => void
  onWidth: (w: number) => void
  onRemove: () => void
}) {
  const title = tile.kind === "text" ? "Text" : (chart?.name ?? "A chart you cannot see")
  const unfiltered = ignoredLabels(state?.draw?.filters.unfiltered ?? [], filters)
  return (
    <div
      className="flex h-full min-w-0 flex-col overflow-hidden rounded-lg border border-zinc-300 bg-white dark:border-zinc-700 dark:bg-zinc-950"
      style={{ minHeight: tileHeight(2) }}
    >
      <div className="flex items-center gap-1 border-b border-zinc-100 px-2 py-1 dark:border-zinc-800">
        <span
          className={`${DRAG_HANDLE} flex min-w-0 flex-1 cursor-grab items-center gap-1 text-sm font-medium text-zinc-800 active:cursor-grabbing dark:text-zinc-200`}
          title="Drag to move"
        >
          <GripVertical className="h-4 w-4 shrink-0 text-zinc-400" aria-hidden />
          <span className="line-clamp-2 break-words @xl:block @xl:truncate" title={title}>
            {title}
          </span>
        </span>
        {tile.kind === "chart" && chart && <ChartInfo draw={state?.draw ?? null} unfiltered={unfiltered} type={chart.settings.type} />}
        <DropdownMenu>
          <DropdownMenuTrigger
            className="rounded p-1 text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 hover:text-zinc-900 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
            aria-label={`Width of the tile ${title}, now ${widthLabel(tile.w).toLowerCase()}`}
            title="Width"
          >
            <MoveHorizontal className="h-4 w-4" aria-hidden />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-[10rem]">
            <DropdownMenuLabel className="text-xs font-medium text-zinc-500 dark:text-zinc-400">Width</DropdownMenuLabel>
            <DropdownMenuRadioGroup value={String(tile.w)} onValueChange={(v) => onWidth(Number(v))}>
              {TILE_WIDTHS.map((o) => (
                <DropdownMenuRadioItem key={o.w} value={String(o.w)} aria-label={o.label}>
                  {o.label}
                  <span aria-hidden className="ml-auto pl-4 text-xs tabular-nums text-zinc-500 dark:text-zinc-400">
                    {o.w}/12
                  </span>
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuRadioGroup>
          </DropdownMenuContent>
        </DropdownMenu>
        <button
          type="button"
          className="rounded p-1 text-zinc-500 dark:text-zinc-400 hover:bg-zinc-100 hover:text-zinc-900 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"
          aria-label={`Remove the tile ${title}`}
          onClick={onRemove}
        >
          <X className="h-3.5 w-3.5" />
        </button>
      </div>
      <div className="flex min-h-0 flex-1 flex-col overflow-auto p-2">
        {tile.kind === "text" ? (
          <textarea
            aria-label="Tile text"
            className="h-full w-full resize-none rounded border border-zinc-200 bg-transparent p-2 text-sm dark:border-zinc-800"
            value={tile.body}
            maxLength={MAX_TEXT_TILE}
            placeholder="Plain text: a heading, a note on what the charts show."
            onChange={(e) => onBody(e.target.value)}
          />
        ) : state?.draw && chart ? (
          <SavedChart settings={state.draw.chart?.settings ?? chart.settings} draw={state.draw} fill title={chart.name} rows={tile.h} />
        ) : (
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            {chart ? `${TYPE_LABEL[chart.settings.type] ?? chart.settings.type} of ${chart.source_name ?? "a source"}. ` : ""}
            Drawn when the dashboard is saved.
          </p>
        )}
      </div>
    </div>
  )
}
