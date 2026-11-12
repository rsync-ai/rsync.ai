"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { ChartColumn, Plus, TriangleAlert } from "lucide-react"

import { TYPE_GROUPS, TYPE_LABEL } from "@/components/charts/chart-form"
import { CHART_TYPE_ICON, chartSeriesColor } from "@/components/charts/chartTypeIcons"
import { EmptyState } from "@/components/kit/EmptyState"
import { FilterChips, type FilterChip } from "@/components/kit/FilterChips"
import { LIST_CARD_CLASS, ListCardMeta, ListCardPills } from "@/components/kit/ListCard"
import { ListCardsSkeleton } from "@/components/kit/PageSkeleton"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { listCharts, type ChartRecord, type ChartType } from "@/lib/api/charts"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/**
 * The workspace's saved charts (dashboards-design.md § 3, § 7.4): each a model or
 * a saved query drawn one way. Every member opens them; owners and admins make
 * and change them, and put them on dashboards.
 */
export default function ChartsPage() {
  const [charts, setCharts] = useState<ChartRecord[] | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const [type, setType] = useState<ChartType | "all">("all")

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setCharts(null)
        setError(null)
        setType("all")
        setVersion((v) => v + 1)
      }),
    [],
  )

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    listCharts().then(
      (got) => {
        if (!alive || isStale()) return
        setCharts(got.charts)
        setCanManage(got.can_manage)
        setError(null)
      },
      (err: unknown) => {
        if (alive && !isStale()) setError(err instanceof Error ? err.message : "Could not load charts")
      },
    )
    return () => {
      alive = false
    }
  }, [version])

  const newChart = canManage ? (
    <Button asChild size="sm">
      <Link href="/charts/new">
        <Plus className="mr-1.5 h-4 w-4" /> New chart
      </Link>
    </Button>
  ) : null
  const shown = charts?.filter((c) => type === "all" || c.settings.type === type) ?? []

  return (
    <div className="space-y-5">
      <PageHeader heading="Charts" description="A model or a saved query, drawn one way. Charts go on dashboards.">
        {charts !== null && charts.length > 0 && newChart}
      </PageHeader>

      {error && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {error}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
      )}
      {charts === null && !error && <ListCardsSkeleton label="Loading charts…" />}
      {charts !== null && charts.length === 0 && (
        <div className="rounded-xl border border-zinc-200 dark:border-zinc-800">
          <EmptyState
            icon={ChartColumn}
            title="No charts yet"
            description={
              canManage
                ? "Draw a model or a saved query as a line, bars, a heatmap, one number or a table. Create chart in the Explorer makes one too."
                : "Workspace owners and admins make them."
            }
            action={newChart}
          />
        </div>
      )}
      {charts !== null && charts.length > 0 && (
        <>
          <FilterChips label="Show chart types" chips={typeChips(charts)} value={type} onChange={setType} />
          <ul className="grid gap-4 @xl:grid-cols-2 @4xl:grid-cols-3">
            {shown.map((c) => (
              <li key={c.id} className="flex">
                <ChartListCard chart={c} />
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  )
}

/** All, then each type the workspace has a chart of, in the type picker's order, with its count. */
function typeChips(charts: ChartRecord[]): FilterChip<ChartType | "all">[] {
  const count = (t: ChartType) => charts.filter((c) => c.settings.type === t).length
  const types = TYPE_GROUPS.flatMap((g) => g.types.map((t) => t.type)).filter((t) => count(t) > 0)
  return [
    { value: "all", label: "All", count: charts.length },
    ...types.map((t) => ({ value: t, label: TYPE_LABEL[t], count: count(t), icon: CHART_TYPE_ICON[t] })),
  ]
}

/** A chart as a card: its type in its own colour, what it reads, who sees it, when it changed. */
function ChartListCard({ chart: c }: { chart: ChartRecord }) {
  const Icon = CHART_TYPE_ICON[c.settings.type] ?? ChartColumn
  const description = c.settings.description?.trim()
  return (
    <Link href={`/charts/${c.id}`} className={LIST_CARD_CLASS}>
      <div className="flex items-start gap-3">
        <span
          className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg"
          style={{ backgroundColor: chartSeriesColor(c.settings, 0.12), color: chartSeriesColor(c.settings) }}
          title={TYPE_LABEL[c.settings.type] ?? c.settings.type}
        >
          <Icon className="h-4 w-4" aria-hidden />
          <span className="sr-only">{TYPE_LABEL[c.settings.type] ?? c.settings.type}</span>
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-semibold text-zinc-900 dark:text-white">{c.name}</h2>
          <ListCardPills visibility={c.visibility} proposedBy={c.proposed_by} />
        </div>
      </div>
      {description && <p className="mt-3 line-clamp-2 text-sm text-zinc-600 dark:text-zinc-400">{description}</p>}
      {c.source_state !== "ok" && (
        <p className="mt-3 flex items-start gap-1.5 rounded-md bg-amber-50 px-2 py-1.5 text-xs text-amber-800 dark:bg-amber-400/10 dark:text-amber-200">
          <TriangleAlert className="mt-px h-3.5 w-3.5 shrink-0" aria-hidden />
          {c.source_state === "deleted"
            ? "Its source was deleted, so it draws nothing."
            : "Its source is not shared with you, so it draws nothing for you."}
        </p>
      )}
      <ListCardMeta updatedAt={c.updated_at}>
        {c.source_state === "ok" && c.source_name && (
          <>
            <span className="font-medium text-zinc-700 dark:text-zinc-300">{c.source_name}</span>{" "}
            {c.source_kind === "model" ? "model" : "saved query"}
          </>
        )}
      </ListCardMeta>
    </Link>
  )
}
