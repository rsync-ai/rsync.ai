"use client"

/**
 * The chart page (dashboards-design.md § 7.2), laid out as Superset's chart
 * builder: pick a model or a saved query, then the source's columns by type on the
 * left, the chart type and its fields in the middle, and a large preview on the
 * right that switches between Chart, Table and the SQL rsync.ai wrote. Picking a
 * type changes the fields it asks for. No LLM is used: the server writes the SQL
 * from the settings and checks them (§ 3.2) before it draws or saves them.
 *
 * Owners and admins make and change charts; a member sees the chart drawn, with
 * its badges, and nothing to change (can_manage comes from the server).
 */

import Link from "next/link"
import { useRouter } from "next/navigation"
import { ChevronDown, ChevronRight, CircleHelp, Clock, Hash, LayoutGrid, Loader2, Lock, Play, Save, Trash2, Type } from "lucide-react"
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react"
import { toast } from "sonner"

import {
  AGGREGATE_LABEL,
  COLUMN_GROUPS,
  EMPTY_FORM,
  MAX_LEVELS,
  MAX_PIVOT_COLUMNS,
  MAX_PIVOT_ROWS,
  PRICE_FIELDS,
  TYPE_GROUPS,
  TYPE_LABEL,
  canStack,
  colourColumns,
  columnGroup,
  defaultsLargestFirst,
  fromSettings,
  isPoints,
  isXY,
  measureAggregates,
  measureColumns,
  missingField,
  numberColumns,
  refusedTypes,
  suggestedTypes,
  takesColor,
  takesGrain,
  takesSecondMeasure,
  takesX,
  toSettings,
  wrongColumn,
  xColumns,
  xKinds,
  type ChartForm,
  type PivotFieldForm,
} from "@/components/charts/chart-form"
import { chartSpec, seriesCap } from "@/components/charts/chart-registry"
import { SERIES_COLORS } from "@/components/charts/chart-theme"
import { CHART_TYPE_ICON } from "@/components/charts/chartTypeIcons"
import { ChartExportMenu } from "@/components/charts/ChartExportMenu"
import { labelOf, savedChartView } from "@/components/charts/saved-chart"
import { SavedChart } from "@/components/charts/SavedChart"
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
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useWorkspaceRole } from "@/contexts/WorkspaceContext"
import {
  CHART_AGGREGATES,
  CHART_GRAINS,
  CHART_MAPS,
  CHART_MARKS,
  COMPARE_DAYS,
  ChartError,
  DEFAULT_BINS,
  MAX_BINS,
  MAX_TABLE_COLUMNS,
  createChart,
  deleteChart,
  drawChart,
  getChart,
  getChartSourceColumns,
  listChartSources,
  one,
  previewChart,
  updateChart,
  type ChartAggregate,
  type ChartAxis,
  type ChartDraw,
  type ChartGrain,
  type ChartMap,
  type ChartMark,
  type ChartNumberFormat,
  type ChartRecord,
  type ChartSort,
  type ChartSourceColumn,
  type ChartSourceItem,
  type ChartType,
  type ChartVisibility,
  type ServerChartSettings,
} from "@/lib/api/charts"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/** The preview's plot height: the page has room for a large one. */
const PREVIEW_HEIGHT = 360
/** The Table tab lists this many rows; the chart draws them all. */
const TABLE_TAB_ROWS = 500

const selectClass =
  "h-9 w-full rounded-md border border-zinc-200 bg-white px-2 text-sm text-zinc-900 disabled:opacity-50 dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-100"
const labelClass = "text-xs font-medium text-zinc-600 dark:text-zinc-400"

interface Drawn {
  sourceId: string
  settings: ServerChartSettings
  draw: ChartDraw
  /** The fields it was drawn from, as drawnFields writes them; none for a chart a member opens. */
  fields?: string
}

/** The fields as the chart draws them: a description is read to a screen reader, never drawn. */
const drawnFields = (form: ChartForm) => JSON.stringify(toSettings({ ...form, description: "" }))

export interface ChartBuilderProps {
  /** The saved chart to open; none for a new chart. */
  chartId?: string
  /** A new chart's source, as the Explorer's Create chart passes it. */
  initialSource?: string | null
}

export function ChartBuilder({ chartId, initialSource }: ChartBuilderProps) {
  const router = useRouter()
  const { meets, isLoading: roleLoading } = useWorkspaceRole()
  const [version, setVersion] = useState(0)

  const [record, setRecord] = useState<ChartRecord | null>(null)
  const [canManage, setCanManage] = useState<boolean | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [sources, setSources] = useState<ChartSourceItem[] | null>(null)
  const [sourceId, setSourceId] = useState(initialSource ?? "")
  const [columns, setColumns] = useState<ChartSourceColumn[] | null>(null)
  const [columnsError, setColumnsError] = useState<string | null>(null)

  const [form, setForm] = useState<ChartForm>(EMPTY_FORM)
  const [name, setName] = useState("")
  const [visibility, setVisibility] = useState<ChartVisibility>("private")

  const [drawn, setDrawn] = useState<Drawn | null>(null)
  const [drawing, setDrawing] = useState(false)
  const [drawError, setDrawError] = useState<string | null>(null)
  const [tab, setTab] = useState("chart")
  const [saving, setSaving] = useState(false)
  const [saveError, setSaveError] = useState<string | null>(null)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const autoDrawn = useRef(false)
  const plotBox = useRef<HTMLElement>(null)

  // A new chart is made by owners and admins; the server refuses anyone else.
  const manage = chartId ? canManage : roleLoading ? null : meets("admin")

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setRecord(null)
        setCanManage(null)
        setSources(null)
        setColumns(null)
        setDrawn(null)
        setSourceId(chartId ? "" : (initialSource ?? ""))
        autoDrawn.current = false
        setVersion((v) => v + 1)
      }),
    [chartId, initialSource],
  )

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    setLoadError(null)
    void (async () => {
      try {
        if (chartId) {
          const got = await getChart(chartId)
          if (!alive || isStale()) return
          setRecord(got.chart)
          setCanManage(got.can_manage)
          setForm(fromSettings(got.chart.settings))
          setName(got.chart.name)
          setVisibility(got.chart.visibility)
          setSourceId(got.chart.source_id ?? "")
          if (!got.can_manage) {
            // A member sees the chart drawn as a dashboard tile draws it.
            const d = await drawChart(chartId)
            if (!alive || isStale()) return
            setDrawn({ sourceId: got.chart.source_id ?? "", settings: d.chart?.settings ?? got.chart.settings, draw: d })
            return
          }
        }
        const list = await listChartSources()
        if (!alive || isStale()) return
        setSources(list)
      } catch (err) {
        if (alive && !isStale()) setLoadError(err instanceof Error ? err.message : "Could not load the chart")
      }
    })()
    return () => {
      alive = false
    }
  }, [chartId, version])

  useEffect(() => {
    if (!sourceId || manage === false) {
      setColumns(null)
      return
    }
    let alive = true
    const isStale = captureWorkspace()
    setColumns(null)
    setColumnsError(null)
    getChartSourceColumns(sourceId).then(
      (cols) => {
        if (alive && !isStale()) setColumns(cols)
      },
      (err: unknown) => {
        if (alive && !isStale()) setColumnsError(err instanceof Error ? err.message : "Could not read the source's columns")
      },
    )
    return () => {
      alive = false
    }
  }, [sourceId, manage, version])

  const source = sources?.find((s) => s.id === sourceId) ?? null
  // A type this source's engine cannot draw, and why (the server refuses it the same way).
  const refused = useMemo(() => refusedTypes(source?.connector_type), [source?.connector_type])

  // Why the fields cannot be drawn yet, in words; null when they can.
  const refusal = useCallback(
    (f: ChartForm) => refused[f.type] ?? missingField(f) ?? (columns ? wrongColumn(f, columns) : null),
    [columns, refused],
  )

  const drawnView = useMemo(
    () => (drawn?.draw.result && !drawn.draw.break ? savedChartView(drawn.settings, drawn.draw.result) : null),
    [drawn],
  )

  /**
   * The series colours to keep: those drawn last, when the drawing is of this
   * source and series column, else those saved. A colour follows its series.
   */
  const pins = useCallback((): { value: string; color: string }[] | undefined => {
    if (
      drawn &&
      drawn.sourceId === sourceId &&
      one(drawn.settings, "series")?.column === form.series &&
      drawnView?.kind === "echart" &&
      drawnView.series.length > 0
    ) {
      return drawnView.series.map((s) => ({ value: s.value, color: s.color }))
    }
    const saved = record ? one(record.settings, "series") : undefined
    if (saved?.column === form.series) return saved?.colors
    return undefined
  }, [drawn, drawnView, form.series, record, sourceId])

  const run = useCallback(async () => {
    const miss = refusal(form)
    if (!sourceId) {
      setDrawError("Pick a model or a saved query.")
      return
    }
    if (miss) {
      setDrawError(miss)
      return
    }
    const settings = toSettings(form, pins())
    const isStale = captureWorkspace()
    setDrawing(true)
    setDrawError(null)
    try {
      const draw = await previewChart(sourceId, settings)
      if (isStale()) return
      setDrawn({ sourceId, settings, draw, fields: drawnFields(form) })
    } catch (err) {
      if (!isStale()) setDrawError(err instanceof Error ? err.message : "Could not draw the chart")
    } finally {
      setDrawing(false)
    }
  }, [form, pins, refusal, sourceId])

  // A saved chart opens drawn, once its source's columns are read.
  useEffect(() => {
    if (record && canManage && columns && !autoDrawn.current) {
      autoDrawn.current = true
      void run()
    }
  }, [record, canManage, columns, run])

  const set = <K extends keyof ChartForm>(key: K, value: ChartForm[K]) => setForm((f) => ({ ...f, [key]: value }))

  const pickSource = (id: string) => {
    setSourceId(id)
    // The fields named the last source's columns.
    setForm((f) => ({ ...EMPTY_FORM, type: f.type }))
    setDrawn(null)
    setDrawError(null)
    const picked = sources?.find((s) => s.id === id)
    if (picked && picked.visibility !== "workspace") setVisibility("private")
  }

  const save = async () => {
    setSaveError(null)
    if (!name.trim()) {
      setSaveError("Name the chart.")
      return
    }
    if (!sourceId) {
      setSaveError("Pick a model or a saved query.")
      return
    }
    const miss = refusal(form)
    if (miss) {
      setSaveError(miss)
      return
    }
    const input = { name: name.trim(), source_id: sourceId, settings: toSettings(form, pins()), visibility }
    setSaving(true)
    try {
      if (record) {
        const saved = await updateChart(record.id, { ...input, updated_at: record.updated_at })
        setRecord(saved)
        toast.success("Chart saved")
      } else {
        const saved = await createChart(input)
        toast.success("Chart saved")
        router.replace(`/charts/${saved.id}`)
      }
    } catch (err) {
      if (err instanceof ChartError && err.code === "stale") {
        setSaveError("Someone saved this chart after you opened it. Reload the page to see their change; yours is not saved.")
      } else {
        setSaveError(err instanceof Error ? err.message : "Could not save the chart")
      }
    } finally {
      setSaving(false)
    }
  }

  const remove = async () => {
    if (!record) return
    setConfirmDelete(false)
    try {
      await deleteChart(record.id)
      toast.success("Chart deleted")
      router.push("/charts")
    } catch (err) {
      const dashboards = err instanceof ChartError && err.dashboards?.length ? ` (${err.dashboards.join(", ")})` : ""
      setSaveError(`${err instanceof Error ? err.message : "Could not delete the chart"}${dashboards}`)
    }
  }

  if (loadError) {
    return (
      <div className="space-y-4">
        <PageHeader heading={chartId ? "Chart" : "New chart"} />
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {loadError}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
        <Link href="/charts" className="text-sm underline">
          All charts
        </Link>
      </div>
    )
  }

  if (manage === null || (chartId && !record)) {
    return (
      <div role="status" className="flex items-center gap-2 py-12 text-sm text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-4 w-4 animate-spin" /> Loading the chart…
      </div>
    )
  }

  if (!manage) {
    if (!record) {
      return (
        <div className="space-y-4">
          <PageHeader heading="New chart" />
          <p className="text-sm text-zinc-600 dark:text-zinc-400">
            Workspace owners and admins make charts. You can open every chart shared with the workspace.
          </p>
          <Link href="/charts" className="text-sm underline">
            All charts
          </Link>
        </div>
      )
    }
    return (
      <div className="space-y-4">
        <PageHeader
          heading={record.name}
          description={`${TYPE_LABEL[record.settings.type] ?? record.settings.type} of ${record.source_name ?? "a source you cannot open"}. Owners and admins change charts.`}
        >
          {drawn && <ChartExportMenu name={record.name} settings={drawn.settings} draw={drawn.draw} plot={plotBox} />}
        </PageHeader>
        <section ref={plotBox} aria-label="Chart" className="rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
          <SavedChart settings={drawn?.settings ?? record.settings} draw={drawn?.draw ?? null} loading={!drawn} height={PREVIEW_HEIGHT} />
        </section>
      </div>
    )
  }

  const suggested = columns ? suggestedTypes(form, columns) : []
  const missing = sourceId ? refusal(form) : "Pick a model or a saved query."
  // A chart drawn from fields changed since stays in view, marked, until Run draws them.
  const outOfDate = !!drawn?.fields && drawn.fields !== drawnFields(form)

  return (
    <div className="space-y-4">
      {/* Run and Save stay in reach however far the fields below scroll (launch review). */}
      <div
        data-testid="chart-editor-bar"
        className="sticky top-16 z-20 flex flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded-xl border border-zinc-200 bg-white/90 px-4 py-3 shadow-sm backdrop-blur dark:border-zinc-800 dark:bg-zinc-900/90"
      >
        <div className="flex min-w-0 items-center gap-1.5">
          <Link href="/charts" className="shrink-0 text-sm text-zinc-500 hover:text-zinc-800 dark:text-zinc-400 dark:hover:text-zinc-200">
            Charts
          </Link>
          <ChevronRight className="h-4 w-4 shrink-0 text-zinc-400" aria-hidden />
          <h1 className="line-clamp-2 break-words text-lg font-semibold tracking-tight text-zinc-900 @xl:block @xl:truncate dark:text-white">
            {record ? record.name : "New chart"}
          </h1>
        </div>
        <div className="flex items-center gap-2">
          {record && (
            <Button type="button" size="sm" variant="ghost" onClick={() => setConfirmDelete(true)} aria-label="Delete chart" title="Delete chart">
              <Trash2 className="h-4 w-4" aria-hidden />
            </Button>
          )}
          <Button type="button" size="sm" variant="outline" onClick={() => void run()} disabled={drawing || !!missing} title={missing ?? undefined}>
            {drawing ? <Loader2 className="mr-1.5 h-4 w-4 animate-spin" aria-hidden /> : <Play className="mr-1.5 h-4 w-4" aria-hidden />}
            Run
          </Button>
          <Button type="button" size="sm" onClick={() => void save()} disabled={saving}>
            {saving ? <Loader2 className="mr-1.5 h-4 w-4 animate-spin" aria-hidden /> : <Save className="mr-1.5 h-4 w-4" aria-hidden />}
            Save chart
          </Button>
        </div>
      </div>
      <p className="text-sm text-zinc-500 dark:text-zinc-400">
        Pick a source, a chart type and its fields, then run it. The server writes the SQL and draws the source live, with your rights.
      </p>
      {saveError && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {saveError}
        </p>
      )}

      <div className="grid gap-4 @3xl:grid-cols-[12rem_19rem_minmax(0,1fr)]">
        {/* Left: the source and its columns, by type. */}
        <section aria-label="Source" className="space-y-3 self-start rounded-xl border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-950">
          <div className="space-y-1">
            <label htmlFor="chart-source" className={labelClass}>
              Model or saved query
            </label>
            <Select value={sourceId} onValueChange={pickSource} disabled={sources === null}>
              <SelectTrigger id="chart-source" className="h-9 text-left">
                <SelectValue placeholder={sources === null ? "Loading…" : "Pick a source"} />
              </SelectTrigger>
              <SelectContent>
                {(["model", "saved_query"] as const).map((kind) => {
                  const items = (sources ?? []).filter((s) => s.kind === kind)
                  if (items.length === 0) return null
                  return (
                    <SelectGroup key={kind}>
                      <SelectLabel>{kind === "model" ? "Models" : "Saved queries"}</SelectLabel>
                      {items.map((s) => (
                        <SelectItem key={s.id} value={s.id} disabled={!s.chartable && s.id !== sourceId}>
                          {s.name}
                          {!s.chartable && s.reason ? ` (${s.reason})` : ""}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  )
                })}
              </SelectContent>
            </Select>
            {source && (
              <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                {source.connection_name} · {source.visibility === "workspace" ? "shared" : "private"}
              </p>
            )}
            {source && !source.chartable && source.reason && (
              <p className="text-xs text-amber-700 dark:text-amber-300">{source.reason}</p>
            )}
            {sources !== null && !sourceId && sources.length === 0 && (
              <p className="text-xs text-zinc-500 dark:text-zinc-400">
                No models or saved queries yet. Save a query in the{" "}
                <Link href="/explorer" className="underline">
                  Explorer
                </Link>{" "}
                first.
              </p>
            )}
          </div>
          <ColumnList columns={columns} error={columnsError} loading={!!sourceId && columns === null && !columnsError} />
        </section>

        {/* Middle: the type and the fields it asks for, then its name and who sees it. */}
        <section aria-label="Chart settings" className="space-y-4 self-start rounded-xl border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-950">
          <TypePicker value={form.type} suggested={suggested} refused={refused} onPick={(t) => set("type", t)} />
          {columns && <Fields form={form} columns={columns} set={set} />}
          <div className="space-y-1">
            <label htmlFor="chart-description" className={labelClass}>
              Description <span className="font-normal">(read to a screen reader; optional)</span>
            </label>
            <Input
              id="chart-description"
              value={form.description}
              maxLength={500}
              onChange={(e) => set("description", e.target.value)}
            />
          </div>

          <div className="space-y-3 border-t border-zinc-200 pt-3 dark:border-zinc-800">
            <div className="space-y-1">
              <label htmlFor="chart-name" className={labelClass}>
                Name
              </label>
              <Input id="chart-name" value={name} maxLength={200} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="space-y-1">
              <label htmlFor="chart-visibility" className={labelClass}>
                Who sees it
              </label>
              <Select value={visibility} onValueChange={(v) => setVisibility(v as ChartVisibility)}>
                <SelectTrigger id="chart-visibility" className="h-9 text-left">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="private">Only me</SelectItem>
                  <SelectItem value="workspace" disabled={!!source && source.visibility !== "workspace"}>
                    The workspace{source && source.visibility !== "workspace" ? " (share the source first)" : ""}
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
        </section>

        {/* Right: the preview, as Chart, Table or the SQL rsync.ai wrote. */}
        <section ref={plotBox} aria-label="Preview" className="min-w-0 rounded-xl border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-950">
          <Tabs value={tab} onValueChange={setTab}>
            <div className="flex items-center justify-between gap-2">
              <TabsList>
                <TabsTrigger value="chart">Chart</TabsTrigger>
                <TabsTrigger value="table">Table</TabsTrigger>
                <TabsTrigger value="sql">SQL</TabsTrigger>
              </TabsList>
              {tab === "chart" && drawn && (
                <ChartExportMenu name={name.trim() || record?.name || "Chart"} settings={drawn.settings} draw={drawn.draw} plot={plotBox} />
              )}
            </div>
            {drawError && (
              <p role="alert" className="mt-3 text-sm text-red-700 dark:text-red-300">
                {drawError}
              </p>
            )}
            {outOfDate && (
              <p role="status" className="mt-3 rounded-md bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
                This chart is out of date: the fields changed since it was drawn. {missing ?? "Run it to draw them."}
              </p>
            )}
            <TabsContent value="chart" className="mt-3">
              {drawn ? (
                <SavedChart settings={drawn.settings} draw={drawn.draw} height={PREVIEW_HEIGHT} />
              ) : drawing ? (
                <SavedChart settings={toSettings(form)} draw={null} loading height={PREVIEW_HEIGHT} />
              ) : (
                <p className="py-16 text-center text-sm text-zinc-500 dark:text-zinc-400">
                  {missing ?? "Run it to draw the chart."}
                </p>
              )}
            </TabsContent>
            <TabsContent value="table" className="mt-3">
              <RawResult draw={drawn?.draw ?? null} />
            </TabsContent>
            <TabsContent value="sql" className="mt-3">
              {drawn?.draw.result?.sql ? (
                <pre className="overflow-auto rounded bg-zinc-50 p-3 text-xs text-zinc-800 dark:bg-zinc-900 dark:text-zinc-200">
                  {drawn.draw.result.sql}
                </pre>
              ) : (
                <p className="py-16 text-center text-sm text-zinc-500 dark:text-zinc-400">Run it to see the SQL.</p>
              )}
            </TabsContent>
          </Tabs>
        </section>
      </div>

      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete this chart?</AlertDialogTitle>
            <AlertDialogDescription>
              The chart is deleted for everyone. A chart on a dashboard cannot be deleted: take it off the dashboard first.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction onClick={() => void remove()}>Delete</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

const GROUP_ICON = { time: Clock, number: Hash, text: Type, other: CircleHelp } as const

function ColumnList({ columns, error, loading }: { columns: ChartSourceColumn[] | null; error: string | null; loading: boolean }) {
  if (error) {
    return (
      <p role="alert" className="text-xs text-red-700 dark:text-red-300">
        {error}
      </p>
    )
  }
  if (loading) {
    return (
      <p role="status" className="flex items-center gap-1.5 text-xs text-zinc-500 dark:text-zinc-400">
        <Loader2 className="h-3 w-3 animate-spin" /> Reading the columns…
      </p>
    )
  }
  if (!columns) return null
  return (
    <div className="space-y-3" aria-label="Columns">
      {COLUMN_GROUPS.map((g) => {
        const cols = columns.filter((c) => columnGroup(c.kind) === g.kind)
        if (cols.length === 0) return null
        const Icon = GROUP_ICON[g.kind as keyof typeof GROUP_ICON]
        return (
          <div key={g.kind}>
            <h3 className="mb-1 text-[11px] font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">{g.label}</h3>
            <ul className="space-y-0.5">
              {cols.map((c) => (
                <li key={c.name} className="flex items-center gap-1.5 truncate text-xs text-zinc-800 dark:text-zinc-200" title={c.name}>
                  <Icon className="h-3 w-3 shrink-0 text-zinc-400" aria-hidden />
                  <span className="truncate">{c.name}</span>
                  {c.masked && (
                    <span className="ml-auto inline-flex items-center gap-0.5 text-[10px] text-zinc-500 dark:text-zinc-400" title="Masked for you">
                      <Lock className="h-3 w-3" aria-hidden /> masked
                    </span>
                  )}
                </li>
              ))}
            </ul>
          </div>
        )
      })}
    </div>
  )
}

function TypePicker({
  value,
  suggested,
  refused,
  onPick,
}: {
  value: ServerChartSettings["type"]
  suggested: ServerChartSettings["type"][]
  /** The types the source's engine cannot draw, each with why; one already picked stays picked. */
  refused: Partial<Record<ChartType, string>>
  onPick: (t: ServerChartSettings["type"]) => void
}) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState("")
  const id = useId()
  // An id has no spaces: aria-labelledby reads a space as a second id.
  const groupId = (label: string) => `${id}-${label.replace(/\W+/g, "-")}`
  // One tile per type, its icon over its name; the group it belongs to is on hover.
  const tile = (t: ChartType, group: string) => {
    const Icon = CHART_TYPE_ICON[t]
    const on = value === t
    const why = refused[t]
    const hint = !why && suggested.includes(t)
    return (
      <button
        key={t}
        type="button"
        aria-pressed={on}
        aria-label={hint ? `${TYPE_LABEL[t]} (suggested)` : undefined}
        title={why ?? (hint ? `${group} · suggested` : group)}
        disabled={!!why && !on}
        onClick={() => {
          onPick(t)
          setOpen(false)
        }}
        className={`relative flex flex-col items-center gap-1 rounded-lg border px-1 py-2 text-[11px] leading-tight disabled:cursor-not-allowed disabled:opacity-40 ${
          on
            ? "border-zinc-900 bg-zinc-900 text-white dark:border-zinc-100 dark:bg-zinc-100 dark:text-zinc-900"
            : "border-zinc-200 text-zinc-700 hover:bg-zinc-50 dark:border-zinc-700 dark:text-zinc-300 dark:hover:bg-zinc-800"
        }`}
      >
        <Icon className="h-4 w-4" aria-hidden />
        <span className="text-center">{TYPE_LABEL[t]}</span>
        {hint && <span aria-hidden className="absolute right-1 top-1 h-1.5 w-1.5 rounded-full bg-emerald-500" />}
      </button>
    )
  }
  const groupOf = (t: ChartType) => TYPE_GROUPS.find((g) => g.types.some((x) => x.type === t))?.label ?? ""
  // The row holds the type picked and up to three the fields suggest, so it stays one
  // row however many types there are; the rest are a click away, in their groups.
  const row = [value, ...suggested.filter((t) => t !== value && !refused[t]).slice(0, 3)]
  // A group's name finds all its types: "part" finds the pie and the rest of a whole.
  const q = query.trim().toLowerCase()
  const found = TYPE_GROUPS.map((g) => ({
    label: g.label,
    types: g.label.toLowerCase().includes(q) ? g.types : g.types.filter((t) => t.label.toLowerCase().includes(q)),
  })).filter((g) => g.types.length > 0)
  return (
    <fieldset className="space-y-2">
      <legend className={labelClass}>Chart type</legend>
      <div className="grid grid-cols-4 gap-1.5">{row.map((t) => tile(t, groupOf(t)))}</div>
      <Popover
        open={open}
        onOpenChange={(o) => {
          setOpen(o)
          // Opened again, it shows every type.
          if (!o) setQuery("")
        }}
      >
        <PopoverTrigger asChild>
          <Button type="button" variant="outline" size="sm" className="w-full gap-1.5 text-xs">
            <LayoutGrid className="h-3.5 w-3.5" aria-hidden />
            All chart types
            <ChevronDown className="h-3.5 w-3.5" aria-hidden />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" aria-label="Chart types" className="w-80 space-y-3">
          <Input type="search" aria-label="Find a chart type" placeholder="Find a chart type" value={query} onChange={(e) => setQuery(e.target.value)} className="h-8 text-xs" />
          {found.map((g) => (
            <div key={g.label} role="group" aria-labelledby={groupId(g.label)} className="space-y-1.5">
              <p id={groupId(g.label)} className={labelClass}>
                {g.label}
              </p>
              <div className="grid grid-cols-4 gap-1.5">{g.types.map((t) => tile(t.type, g.label))}</div>
            </div>
          ))}
          {found.length === 0 && <p className="text-xs text-zinc-500 dark:text-zinc-400">No chart type matches “{query.trim()}”.</p>}
        </PopoverContent>
      </Popover>
      {suggested.length > 0 && (
        <p className="flex items-center gap-1.5 text-[11px] text-zinc-500 dark:text-zinc-400">
          <span aria-hidden className="h-1.5 w-1.5 rounded-full bg-emerald-500" />
          Suggested for the fields you picked
        </p>
      )}
      {refused[value] && (
        <p role="alert" className="text-xs text-amber-700 dark:text-amber-300">
          {refused[value]}
        </p>
      )}
    </fieldset>
  )
}

function ColumnSelect({
  id,
  value,
  onChange,
  columns,
  allow = () => true,
  empty,
}: {
  id: string
  value: string
  onChange: (v: string) => void
  columns: ChartSourceColumn[]
  allow?: (c: ChartSourceColumn) => boolean
  /** The first option's words; it picks no column. */
  empty: string
}) {
  return (
    <select id={id} className={selectClass} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">{empty}</option>
      {COLUMN_GROUPS.map((g) => {
        const cols = columns.filter((c) => columnGroup(c.kind) === g.kind)
        if (cols.length === 0) return null
        return (
          <optgroup key={g.kind} label={g.label}>
            {cols.map((c) => (
              <option key={c.name} value={c.name} disabled={!allow(c) && c.name !== value}>
                {c.name}
                {c.masked ? " (masked)" : ""}
              </option>
            ))}
          </optgroup>
        )
      })}
    </select>
  )
}

type SetField = <K extends keyof ChartForm>(key: K, value: ChartForm[K]) => void

const GRAIN_LABEL: Record<ChartGrain, string> = {
  day: "By day",
  week: "By week",
  month: "By month",
  quarter: "By quarter",
  year: "By year",
}

/** A time axis's date grain (§ 14.3); a week and a year start as the source's connection sets. */
function GrainSelect({ label, value, onChange }: { label: string; value: ChartGrain | ""; onChange: (v: ChartGrain | "") => void }) {
  return (
    <select
      aria-label={label}
      title="Weeks and fiscal years start as the source's connection sets them"
      className={selectClass}
      value={value}
      onChange={(e) => onChange(e.target.value as ChartGrain | "")}
    >
      <option value="">Each value</option>
      {CHART_GRAINS.map((g) => (
        <option key={g} value={g}>
          {GRAIN_LABEL[g]}
        </option>
      ))}
    </select>
  )
}

/** The x role's name on the page, where it is not "X axis". */
const X_LABEL: Partial<Record<ChartType, string>> = {
  hbar: "Categories",
  funnel: "Stages",
  histogram: "Column to count in bins",
  sankey: "From",
  calendar: "Day",
  boxplot: "Groups",
  candlestick: "Date",
}

/** A map's shapes, and the codes its x column holds (§ 14.8). */
const MAP_LABEL: Record<ChartMap, { label: string; x: string; codes: string }> = {
  world: { label: "World countries", x: "Country codes", codes: "a country by its ISO code: FR, FRA or 250" },
  us_states: { label: "US states", x: "US state codes", codes: "a US state by its ISO code: US-CA or CA" },
}

/** A measure's name on the page, where it is not "Measure". */
const MEASURE_LABEL: Partial<Record<ChartType, string>> = { table: "Measure (optional)", range: "Low" }

/** A combo measure's mark (§ 14.8). */
const MARK_LABEL: Record<ChartMark, string> = { bar: "As bars", line: "As a line", area: "As an area" }

/**
 * The form's fields for a chart's first measure; a combo's, a bullet target's or a range's
 * high as the second; and a pivot's second and third.
 */
const FIRST = { aggregate: "aggregate", column: "measureColumn", title: "measureTitle", format: "format" } as const
const SECOND = { aggregate: "aggregate2", column: "measureColumn2", title: "measureTitle2", format: "format2" } as const
const THIRD = { aggregate: "aggregate3", column: "measureColumn3", title: "measureTitle3", format: "format3" } as const

function Fields({ form, columns, set }: { form: ChartForm; columns: ChartSourceColumn[]; set: SetField }) {
  const t = form.type
  const sorts = chartSpec(t)?.sorts ?? []
  const aggregates = measureAggregates(t)
  const pie = t === "pie" || t === "donut"
  // A second measure and a series are one or the other (§ 14.8): each hides the other.
  // A pivot's measures are its cells' columns, never a combo.
  const combo = isXY(t) && takesSecondMeasure(t) && form.aggregate2 !== ""
  const timeColumns = columns.filter((c) => c.kind === "time")
  const isTime = (name: string) => timeColumns.some((c) => c.name === name)
  // A histogram's bins are ranges of a number; a calendar's days and a candlestick's periods
  // are dates; a map's codes are text or a number, and a masked column's are never read.
  const kinds = xKinds(t)
  const map = t === "map"
  const xEmpty = map
    ? "Pick a column of codes"
    : kinds.includes("number")
      ? "Pick a number column"
      : kinds.includes("time")
        ? "Pick a date column"
        : "Pick a column"
  // A grain is for a time column: picking another column drops it.
  const pickAxis = (axis: "x" | "y", v: string) => {
    set(axis, v)
    if (!isTime(v)) set(axis === "x" ? "xGrain" : "yGrain", "")
  }
  const xSort = (dir: "asc" | "desc") => {
    const order = dir === "asc" ? "A to Z" : "Z to A"
    if (t === "table") return `First column, ${order}`
    if (t === "funnel") return `Stages, ${order}`
    return `X, ${order} or ${dir === "asc" ? "oldest" : "newest"} first`
  }
  return (
    <div className="space-y-3">
      {map && (
        <Field id="chart-map" label="Shapes">
          <select id="chart-map" className={selectClass} value={form.map} onChange={(e) => set("map", e.target.value as ChartMap)}>
            {CHART_MAPS.map((m) => (
              <option key={m} value={m}>
                {MAP_LABEL[m].label}
              </option>
            ))}
          </select>
        </Field>
      )}
      {takesX(t) && (
        <Field id="chart-x" label={map ? MAP_LABEL[form.map].x : (X_LABEL[t] ?? "X axis")}>
          <ColumnSelect
            id="chart-x"
            value={form.x}
            onChange={(v) => pickAxis("x", v)}
            columns={columns}
            allow={xColumns(t)}
            empty={xEmpty}
          />
          {map && (
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
              Each row fills {MAP_LABEL[form.map].codes}. A name is not read, and nothing is looked up outside rsync.ai.
            </p>
          )}
          {takesGrain(t) && (isTime(form.x) || form.xGrain) && (
            <GrainSelect label="X axis grain" value={form.xGrain} onChange={(v) => set("xGrain", v)} />
          )}
          <Input aria-label="X axis title" placeholder="Title (optional)" value={form.xTitle} maxLength={120} onChange={(e) => set("xTitle", e.target.value)} />
          {t === "histogram" && (
            <div className="grid grid-cols-2 gap-2">
              <NumberInput
                label="Bins"
                placeholder={`${DEFAULT_BINS} bins`}
                value={form.bins}
                onValue={(n) => {
                  set("bins", Math.min(Math.trunc(n), MAX_BINS))
                  if (n > 0) set("width", 0)
                }}
              />
              <NumberInput
                label="Bin width"
                placeholder="Or a bin width"
                value={form.width}
                onValue={(n) => {
                  set("width", n)
                  if (n > 0) set("bins", 0)
                }}
              />
            </div>
          )}
        </Field>
      )}
      {(t === "heatmap" || t === "sankey") && (
        <Field id="chart-y" label={t === "sankey" ? "To" : "Y axis"}>
          <ColumnSelect id="chart-y" value={form.y} onChange={(v) => pickAxis("y", v)} columns={columns} empty="Pick a column" />
          {t === "heatmap" && (isTime(form.y) || form.yGrain) && (
            <GrainSelect label="Y axis grain" value={form.yGrain} onChange={(v) => set("yGrain", v)} />
          )}
          <Input aria-label="Y axis title" placeholder="Title (optional)" value={form.yTitle} maxLength={120} onChange={(e) => set("yTitle", e.target.value)} />
          {t === "sankey" && (
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
              Each flow runs from its From value to its To value. The 100 largest names are drawn; a flow back to where it came from is left out.
            </p>
          )}
        </Field>
      )}
      {t === "boxplot" && (
        <Field id="chart-y" label="Values">
          <ColumnSelect
            id="chart-y"
            value={form.y}
            onChange={(v) => set("y", v)}
            columns={columns}
            // A box is a number's quartiles; a masked column's values are never read.
            allow={numberColumns}
            empty="Pick a number column"
          />
          <div className="grid grid-cols-2 gap-2">
            <Input aria-label="Y axis title" placeholder="Title (optional)" value={form.yTitle} maxLength={120} onChange={(e) => set("yTitle", e.target.value)} />
            <FormatSelect label="Number format" value={form.format} onChange={(v) => set("format", v)} />
          </div>
          <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
            Each box is its group&apos;s middle half, the line across it the median, and the whiskers reach the least and the greatest value.
          </p>
        </Field>
      )}
      {t === "candlestick" && <PriceFields form={form} columns={columns} set={set} />}
      {(t === "treemap" || t === "sunburst") && (
        <PivotFields part="levels" label="Levels, outermost first" max={MAX_LEVELS} form={form} columns={columns} set={set} />
      )}
      {pie && (
        <Field id="chart-slices" label="Slices">
          <ColumnSelect id="chart-slices" value={form.series} onChange={(v) => set("series", v)} columns={columns} empty="Pick a column" />
          {form.series && (
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
              Each slice keeps its colour when saved. More than {seriesCap(t)} slices draw the top {seriesCap(t)} and Other.
            </p>
          )}
        </Field>
      )}
      {t === "table" && <TableColumns form={form} columns={columns} set={set} />}
      {isPoints(t) && <PointFields form={form} columns={columns} set={set} />}
      {t === "pivot" && (
        <>
          <PivotFields part="rows" label="Rows" max={MAX_PIVOT_ROWS} form={form} columns={columns} set={set} />
          <PivotFields part="columns" label="Columns (optional)" max={MAX_PIVOT_COLUMNS} form={form} columns={columns} set={set} />
        </>
      )}

      {aggregates.length > 0 && (
        <Field id="chart-aggregate" label={MEASURE_LABEL[t] ?? "Measure"}>
          <MeasureFields form={form} set={set} keys={FIRST} columns={columns} aggregates={aggregates} none={t === "table" ? "None" : "Pick what to measure"} />
        </Field>
      )}

      {isXY(t) && !combo && (
        <Field id="chart-series" label="Series (optional)">
          <ColumnSelect id="chart-series" value={form.series} onChange={(v) => set("series", v)} columns={columns} empty="None: one series" />
          {form.series && canStack(t) && (
            <label className="flex items-center gap-2 text-xs text-zinc-700 dark:text-zinc-300">
              <input type="checkbox" checked={form.stacked} onChange={(e) => set("stacked", e.target.checked)} />
              Stack the series
            </label>
          )}
          {form.series && form.stacked && canStack(t) && (
            <label className="flex items-center gap-2 text-xs text-zinc-700 dark:text-zinc-300">
              <input type="checkbox" checked={form.percent} onChange={(e) => set("percent", e.target.checked)} />
              Show each as a share of 100%
            </label>
          )}
          {form.series && (
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
              Each series keeps its colour when saved. More than 7 series draw the top 7 and Other.
            </p>
          )}
        </Field>
      )}

      {/* A combo left with a series from another type still shows, so it can be cleared. */}
      {isXY(t) && takesSecondMeasure(t) && form.aggregate && (combo || !form.series) && (
        <Field id="chart-aggregate-2" label="Second measure (optional)">
          <MeasureFields form={form} set={set} keys={SECOND} columns={columns} aggregates={aggregates} none="None: one measure" />
          {combo && (
            <>
              <MarkAxis
                label="First measure"
                mark={form.mark}
                axis={form.axis}
                onMark={(v) => set("mark", v)}
                onAxis={(v) => set("axis", v)}
              />
              <MarkAxis
                label="Second measure"
                mark={form.mark2}
                axis={form.axis2}
                onMark={(v) => set("mark2", v)}
                onAxis={(v) => set("axis2", v)}
              />
              <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
                Two measures at most. Each axis is titled with the measures it reads.
              </p>
            </>
          )}
        </Field>
      )}

      {/* A pivot's later measure only after an earlier one, as toSettings sends them. */}
      {t === "pivot" && form.aggregate && (
        <Field id="chart-aggregate-2" label="Second measure (optional)">
          <MeasureFields form={form} set={set} keys={SECOND} columns={columns} aggregates={aggregates} none="None: one measure" />
        </Field>
      )}
      {t === "pivot" && form.aggregate && form.aggregate2 && (
        <Field id="chart-aggregate-3" label="Third measure (optional)">
          <MeasureFields form={form} set={set} keys={THIRD} columns={columns} aggregates={aggregates} none="None: two measures" />
          <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
            Each total is the source&apos;s own, so an average&apos;s total is the average of every row, not of the cells.
          </p>
        </Field>
      )}

      {t === "range" && (
        <Field id="chart-aggregate-2" label="High">
          <MeasureFields form={form} set={set} keys={SECOND} columns={columns} aggregates={aggregates} none="Pick what to measure" name="High" />
          <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
            The band is drawn from the lesser of the two to the greater at each x, and the tooltip reads both.
          </p>
        </Field>
      )}
      {t === "bullet" && <TargetFields form={form} columns={columns} aggregates={aggregates} set={set} />}

      {takesColor(t) && t !== "scorecard" && t !== "table" && !form.series && form.aggregate && (
        <fieldset className="space-y-1">
          <legend className={labelClass}>Colour</legend>
          <div className="flex flex-wrap gap-1.5">
            <button
              type="button"
              aria-pressed={form.color === ""}
              onClick={() => set("color", "")}
              className={`rounded border px-1.5 text-[11px] ${form.color === "" ? "border-zinc-900 dark:border-zinc-100" : "border-zinc-200 dark:border-zinc-700"}`}
            >
              Auto
            </button>
            {SERIES_COLORS.map((token, i) => (
              <button
                key={token}
                type="button"
                aria-pressed={form.color === token}
                aria-label={`Colour ${i + 1}`}
                onClick={() => set("color", token)}
                className={`h-5 w-5 rounded border-2 ${form.color === token ? "border-zinc-900 dark:border-zinc-100" : "border-transparent"}`}
                style={{ background: `hsl(var(--chart-${token}))` }}
              />
            ))}
          </div>
        </fieldset>
      )}

      {t === "scorecard" && (
        <Field id="chart-compare" label="Compare with the period before (optional)">
          <select
            id="chart-compare"
            className={selectClass}
            value={form.compareColumn}
            onChange={(e) => set("compareColumn", e.target.value)}
          >
            <option value="">No comparison</option>
            {timeColumns.map((c) => (
              <option key={c.name} value={c.name}>
                by {c.name}
              </option>
            ))}
          </select>
          {form.compareColumn && (
            <select
              aria-label="Days"
              className={selectClass}
              value={form.compareDays}
              onChange={(e) => set("compareDays", Number(e.target.value))}
            >
              {COMPARE_DAYS.map((d) => (
                <option key={d} value={d}>
                  Last {d === 1 ? "day" : `${d} days`} against the {d === 1 ? "day" : `${d} days`} before
                </option>
              ))}
            </select>
          )}
          {/* A sparkline needs more than one day to draw a line through. */}
          {form.compareColumn && form.compareDays > 1 && (
            <label className="flex items-center gap-2 text-xs text-zinc-700 dark:text-zinc-300">
              <input type="checkbox" checked={form.sparkline} onChange={(e) => set("sparkline", e.target.checked)} />
              Draw the last {form.compareDays} days as a line under the number
            </label>
          )}
          {timeColumns.length === 0 && (
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">The source has no date or time column to compare by.</p>
          )}
        </Field>
      )}

      {sorts.length > 0 && (
        <Field id="chart-sort" label="Order">
          <select id="chart-sort" className={selectClass} value={form.sort} onChange={(e) => set("sort", e.target.value as ChartSort | "")}>
            <option value="">{defaultsLargestFirst(form, columns.find((c) => c.name === form.x)?.kind) ? "Default (largest first)" : "Default"}</option>
            {sorts.includes("x_asc") && <option value="x_asc">{xSort("asc")}</option>}
            {sorts.includes("x_desc") && <option value="x_desc">{xSort("desc")}</option>}
            {(t !== "table" || form.aggregate) && (
              <>
                {sorts.includes("value_desc") && <option value="value_desc">Largest first</option>}
                {sorts.includes("value_asc") && <option value="value_asc">Smallest first</option>}
              </>
            )}
          </select>
        </Field>
      )}
    </div>
  )
}

/** A measure's aggregate, the column it reads, its title and its number format. */
function MeasureFields({
  form,
  set,
  keys,
  columns,
  aggregates,
  none,
  name: named,
}: {
  form: ChartForm
  set: SetField
  keys: typeof FIRST | typeof SECOND | typeof THIRD
  columns: ChartSourceColumn[]
  /** The aggregates the type takes; another stays picked, and missingField says why it does not draw. */
  aggregates: readonly ChartAggregate[]
  none: string
  /** A later measure's name on the page, such as a range's "High"; "Second measure" by default. */
  name?: string
}) {
  // The second and third are named on the page; the first is the field's own label.
  const n = keys === THIRD ? 3 : keys === SECOND ? 2 : 1
  const name = named ?? (n === 3 ? "Third measure" : "Second measure")
  const aggregate = form[keys.aggregate]
  return (
    <>
      <select
        id={n > 1 ? `chart-aggregate-${n}` : "chart-aggregate"}
        aria-label={n > 1 ? name : undefined}
        className={selectClass}
        value={aggregate}
        onChange={(e) => set(keys.aggregate, e.target.value as ChartAggregate | "")}
      >
        <option value="">{none}</option>
        {CHART_AGGREGATES.map((a) => (
          <option key={a} value={a} disabled={!aggregates.includes(a) && a !== aggregate}>
            {AGGREGATE_LABEL[a]}
          </option>
        ))}
      </select>
      {aggregate && aggregate !== "count" && (
        <ColumnSelect
          id={n > 1 ? `chart-measure-column-${n}` : "chart-measure-column"}
          value={form[keys.column]}
          onChange={(v) => set(keys.column, v)}
          columns={columns}
          // Sum and average need numbers, as do a chart's lowest and highest outside a
          // table; a masked column is never measured.
          allow={measureColumns(aggregate, form.type)}
          empty="Pick the column to measure"
        />
      )}
      {aggregate && (
        <div className="grid grid-cols-2 gap-2">
          <Input
            aria-label={n > 1 ? `${name} title` : "Measure title"}
            placeholder="Title (optional)"
            value={form[keys.title]}
            maxLength={120}
            onChange={(e) => set(keys.title, e.target.value)}
          />
          <FormatSelect label={n > 1 ? `${name} number format` : "Number format"} value={form[keys.format]} onChange={(v) => set(keys.format, v)} />
        </div>
      )}
    </>
  )
}

/** How a number is written; "" leaves it to the chart. */
function FormatSelect({ label, value, onChange }: { label: string; value: ChartNumberFormat | ""; onChange: (v: ChartNumberFormat | "") => void }) {
  return (
    <select aria-label={label} className={selectClass} value={value} onChange={(e) => onChange(e.target.value as ChartNumberFormat | "")}>
      <option value="">Number: auto</option>
      <option value="integer">Whole number</option>
      <option value="decimal">Decimal</option>
      <option value="compact">Compact (1.2K)</option>
      <option value="percent">Percent (0.081 as 8.1%)</option>
    </select>
  )
}

/**
 * A bullet chart's target: a number typed, or a second measure of the same rows; one or
 * the other, as the server takes it (§ 14.8).
 */
function TargetFields({
  form,
  columns,
  aggregates,
  set,
}: {
  form: ChartForm
  columns: ChartSourceColumn[]
  aggregates: readonly ChartAggregate[]
  set: SetField
}) {
  const pick = (v: ChartForm["bulletTarget"]) => {
    set("bulletTarget", v)
    // The other kind of target is dropped, so the chart never sends both.
    if (v === "number") set("aggregate2", "")
    else set("target", null)
  }
  return (
    <Field id="chart-target-kind" label="Target">
      <select id="chart-target-kind" className={selectClass} value={form.bulletTarget} onChange={(e) => pick(e.target.value as ChartForm["bulletTarget"])}>
        <option value="number">A number</option>
        <option value="measure">A measure</option>
      </select>
      {form.bulletTarget === "number" ? (
        <TargetInput value={form.target} onValue={(n) => set("target", n)} />
      ) : (
        <MeasureFields form={form} set={set} keys={SECOND} columns={columns} aggregates={aggregates} none="Pick what the target measures" name="Target measure" />
      )}
      <p className="text-[11px] text-zinc-500 dark:text-zinc-400">The bar is the measure; the mark across it is the target.</p>
    </Field>
  )
}

/**
 * A bullet chart's typed target: any number, below zero and zero too, typed in full
 * before it is taken ("-" on its way to "-5" stays as typed). Empty is no target.
 */
function TargetInput({ value, onValue }: { value: number | null; onValue: (n: number | null) => void }) {
  const [text, setText] = useState(value === null ? "" : String(value))
  const read = (t: string) => {
    const n = Number(t)
    return t.trim() !== "" && Number.isFinite(n) ? n : null
  }
  const shown = read(text) === value ? text : value === null ? "" : String(value)
  return (
    <Input
      aria-label="Target number"
      inputMode="decimal"
      placeholder="Type the target"
      value={shown}
      onChange={(e) => {
        setText(e.target.value)
        onValue(read(e.target.value))
      }}
    />
  )
}

/** A candlestick's open, high, low and close: each a number column, read in that order. */
function PriceFields({ form, columns, set }: { form: ChartForm; columns: ChartSourceColumn[]; set: SetField }) {
  const change = (i: number, v: string) => set("prices", form.prices.map((p, j) => (j === i ? v : p)))
  return (
    <>
      {PRICE_FIELDS.map((label, i) => (
        <Field key={label} id={`chart-${label.toLowerCase()}`} label={label}>
          <ColumnSelect
            id={`chart-${label.toLowerCase()}`}
            value={form.prices[i] ?? ""}
            onChange={(v) => change(i, v)}
            columns={columns}
            allow={numberColumns}
            empty="Pick a number column"
          />
        </Field>
      ))}
      <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
        Each period opens at its first row&apos;s open and closes at its last row&apos;s close, by the date; the high and the low are the period&apos;s greatest and least.
      </p>
    </>
  )
}

/** How a combo's measure is drawn and the axis it reads against; "" is the type's own mark and the left. */
function MarkAxis({
  label,
  mark,
  axis,
  onMark,
  onAxis,
}: {
  label: string
  mark: ChartMark | ""
  axis: ChartAxis | ""
  onMark: (v: ChartMark | "") => void
  onAxis: (v: ChartAxis | "") => void
}) {
  return (
    <div className="grid grid-cols-2 gap-2">
      <select aria-label={`${label}: drawn`} className={selectClass} value={mark} onChange={(e) => onMark(e.target.value as ChartMark | "")}>
        <option value="">{label}: as the chart</option>
        {CHART_MARKS.map((m) => (
          <option key={m} value={m}>
            {label}: {MARK_LABEL[m].toLowerCase()}
          </option>
        ))}
      </select>
      <select
        aria-label={`${label}: axis`}
        className={selectClass}
        value={axis === "left" ? "" : axis}
        onChange={(e) => onAxis(e.target.value as ChartAxis | "")}
      >
        <option value="">Left axis</option>
        <option value="right">Right axis</option>
      </select>
    </div>
  )
}

/**
 * A number above zero, typed in full before it is taken: "0." on its way to "0.5" stays
 * as typed. Empty, or not a number above zero, is 0, which leaves it to the chart.
 */
function NumberInput({ label, placeholder, value, onValue }: { label: string; placeholder: string; value: number; onValue: (n: number) => void }) {
  const [text, setText] = useState(value > 0 ? String(value) : "")
  // The text while it reads as the value; the value once something else changed it.
  const shown = (text.trim() === "" ? 0 : Number(text)) === value ? text : value > 0 ? String(value) : ""
  return (
    <Input
      aria-label={label}
      inputMode="decimal"
      placeholder={placeholder}
      value={shown}
      onChange={(e) => {
        setText(e.target.value)
        const n = Number(e.target.value)
        onValue(e.target.value.trim() !== "" && Number.isFinite(n) && n > 0 ? n : 0)
      }}
    />
  )
}

function Field({ id, label, children }: { id: string; label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <label htmlFor={id} className={labelClass}>
        {label}
      </label>
      {children}
    </div>
  )
}

/** A scatter's or a bubble's x, y and size, each a number column, and its colour, a text one. */
function PointFields({ form, columns, set }: { form: ChartForm; columns: ChartSourceColumn[]; set: SetField }) {
  const bubble = form.type === "bubble"
  // A point is placed by a number; a masked column places nothing, so it is not offered.
  const number = numberColumns
  return (
    <>
      <Field id="chart-x" label="X axis">
        <ColumnSelect id="chart-x" value={form.x} onChange={(v) => set("x", v)} columns={columns} allow={number} empty="Pick a number column" />
        <Input aria-label="X axis title" placeholder="Title (optional)" value={form.xTitle} maxLength={120} onChange={(e) => set("xTitle", e.target.value)} />
      </Field>
      <Field id="chart-y" label="Y axis">
        <ColumnSelect id="chart-y" value={form.y} onChange={(v) => set("y", v)} columns={columns} allow={number} empty="Pick a number column" />
        <Input aria-label="Y axis title" placeholder="Title (optional)" value={form.yTitle} maxLength={120} onChange={(e) => set("yTitle", e.target.value)} />
      </Field>
      <Field id="chart-size" label={bubble ? "Size" : "Size (optional)"}>
        <ColumnSelect
          id="chart-size"
          value={form.size}
          onChange={(v) => set("size", v)}
          columns={columns}
          allow={number}
          empty={bubble ? "Pick a number column" : "None: every point one size"}
        />
        {form.size && (
          <>
            <Input aria-label="Size title" placeholder="Title (optional)" value={form.sizeTitle} maxLength={120} onChange={(e) => set("sizeTitle", e.target.value)} />
            <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
              Each point&apos;s area reads its size; a point with no size above zero is not drawn.
            </p>
          </>
        )}
      </Field>
      <Field id="chart-series" label="Colour (optional)">
        <ColumnSelect
          id="chart-series"
          value={form.series}
          onChange={(v) => set("series", v)}
          columns={columns}
          allow={colourColumns}
          empty="None: one colour"
        />
        {form.series && (
          <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
            Each colour keeps its colour when saved. More than 7 colours draw the 7 with the most rows and Other.
          </p>
        )}
      </Field>
      <p className="text-[11px] text-zinc-500 dark:text-zinc-400">At most 10,000 points are drawn, the same ones each time.</p>
    </>
  )
}

/**
 * A pivot table's row fields or column fields, or a treemap's or a sunburst's levels:
 * each a column, a pivot field's time grain and a title. The next one shows once the
 * last is picked, up to the most the type takes.
 */
function PivotFields({
  part,
  label,
  max,
  form,
  columns,
  set,
}: {
  part: "rows" | "columns" | "levels"
  label: string
  max: number
  form: ChartForm
  columns: ChartSourceColumn[]
  set: SetField
}) {
  const key = part === "rows" ? "pivotRows" : part === "columns" ? "pivotColumns" : "levels"
  const fields = form[key]
  const levels = part === "levels"
  // A level is grouped by its values, never a grain.
  const isTime = (name: string) => !levels && columns.some((c) => c.name === name && c.kind === "time")
  // A column is a field once, in the rows or the columns, or at one level.
  const picked = new Set((levels ? form.levels : [...form.pivotRows, ...form.pivotColumns]).map((f) => f.column).filter(Boolean))
  const shown = fields.length < max && (fields.length === 0 || fields[fields.length - 1].column) ? [...fields, { column: "", title: "", grain: "" as const }] : fields
  const change = (i: number, next: PivotFieldForm | null) => {
    const list = shown.map((f, j) => (j === i ? next : f)).filter((f): f is PivotFieldForm => f !== null && f.column !== "")
    set(key, list)
  }
  const one = part === "rows" ? "Row field" : part === "columns" ? "Column field" : "Level"
  return (
    <fieldset className="space-y-1.5">
      <legend className={labelClass}>
        {label} ({fields.length}/{max})
      </legend>
      {shown.map((f, i) => (
        <div key={i} className="space-y-1.5">
          <ColumnSelect
            id={`chart-pivot-${part}-${i + 1}`}
            value={f.column}
            onChange={(v) => change(i, v ? { column: v, title: f.title, grain: isTime(v) ? f.grain : "" } : null)}
            columns={columns}
            allow={(c) => !picked.has(c.name)}
            empty={i === 0 && part !== "columns" ? "Pick a column" : i === 0 ? "None: the measures only" : "None"}
          />
          {f.column && (isTime(f.column) || f.grain) && (
            <GrainSelect label={`${one} ${i + 1} grain`} value={f.grain} onChange={(v) => change(i, { ...f, grain: v })} />
          )}
          {f.column && (
            <Input
              aria-label={`${one} ${i + 1} title`}
              placeholder="Title (optional)"
              value={f.title}
              maxLength={120}
              onChange={(e) => change(i, { ...f, title: e.target.value })}
            />
          )}
        </div>
      ))}
      {part === "rows" && (
        <p className="text-[11px] text-zinc-500 dark:text-zinc-400">Each row field gets a subtotal, and the table a total.</p>
      )}
      {levels && (
        <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
          Each part takes its outermost level&apos;s colour. The 3,500 largest parts are drawn.
        </p>
      )}
    </fieldset>
  )
}

function TableColumns({ form, columns, set }: { form: ChartForm; columns: ChartSourceColumn[]; set: SetField }) {
  const toggle = (name: string, on: boolean) =>
    set("columns", on ? [...form.columns, name].slice(0, MAX_TABLE_COLUMNS) : form.columns.filter((c) => c !== name))
  return (
    <fieldset className="space-y-1">
      <legend className={labelClass}>
        Columns, in the order picked ({form.columns.length}/{MAX_TABLE_COLUMNS})
      </legend>
      <ul className="max-h-48 space-y-0.5 overflow-auto rounded border border-zinc-200 p-1.5 dark:border-zinc-800">
        {columns.map((c) => {
          const on = form.columns.includes(c.name)
          return (
            <li key={c.name}>
              <label className="flex items-center gap-2 text-xs text-zinc-800 dark:text-zinc-200">
                <input
                  type="checkbox"
                  checked={on}
                  disabled={!on && form.columns.length >= MAX_TABLE_COLUMNS}
                  onChange={(e) => toggle(c.name, e.target.checked)}
                />
                <span className="truncate">{c.name}</span>
                {on && <span className="ml-auto text-[10px] text-zinc-500 dark:text-zinc-400">{form.columns.indexOf(c.name) + 1}</span>}
              </label>
            </li>
          )
        })}
      </ul>
    </fieldset>
  )
}

/** The rows the chart was drawn from, as the source returned them. */
function RawResult({ draw }: { draw: ChartDraw | null }) {
  const result = draw?.result
  if (!result) {
    return <p className="py-16 text-center text-sm text-zinc-500 dark:text-zinc-400">Run it to see the rows.</p>
  }
  const shown = result.rows.slice(0, TABLE_TAB_ROWS)
  return (
    <div className="space-y-1">
      <div className="max-h-[420px] overflow-auto rounded border border-zinc-200 dark:border-zinc-800">
        <table className="w-full text-xs">
          <thead className="sticky top-0 bg-zinc-50 dark:bg-zinc-900">
            <tr>
              {result.columns.map((c) => (
                <th key={c.alias} scope="col" className="px-2 py-1.5 text-left font-medium text-zinc-600 dark:text-zinc-300">
                  {c.alias}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {shown.map((row, r) => (
              <tr key={r} className="border-t border-zinc-100 dark:border-zinc-800">
                {row.map((cell, i) => (
                  <td key={i} className="whitespace-nowrap px-2 py-1 text-zinc-800 dark:text-zinc-200">
                    {labelOf(cell)}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="text-[11px] text-zinc-500 dark:text-zinc-400">
        {result.rows.length.toLocaleString("en-US")} rows in {result.execution_time_ms} ms
        {result.rows.length > TABLE_TAB_ROWS ? `; the first ${TABLE_TAB_ROWS} are listed here, the chart draws them all` : ""}.
      </p>
    </div>
  )
}
