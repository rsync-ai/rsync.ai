"use client"

/**
 * A saved chart, drawn (dashboards-design.md § 4, § 5): the chart, scorecard or
 * table its settings say, with its badges above it (data as of, on time or late,
 * checks, lineage; a saved query says only when it was read) and its warnings in
 * amber. A warning never hides the rows; a structural break replaces the chart.
 * In a dashboard tile the badges and warnings are not drawn here: the tile's
 * header shows them as ChartInfo.
 */

import Link from "next/link"
import { ArrowDown, ArrowUp, Minus, TriangleAlert } from "lucide-react"
import { useMemo } from "react"

import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import { Sparkline } from "@/components/kit/Sparkline"
import { pillToneClass } from "@/components/kit/StatusPill"
import { CHART_HEIGHT, formatNumber } from "@/components/charts/chart-settings"
import { changeShort, changeText, chartWarnings, savedChartView, type SavedChartView } from "@/components/charts/saved-chart"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { Tooltip, TooltipContent, TooltipPortal, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip"
import type { ChartBadges, ChartDraw, ServerChartSettings } from "@/lib/api/charts"
import { cn } from "@/lib/utils"

export interface SavedChartProps {
  settings: ServerChartSettings
  draw: ChartDraw | null
  loading?: boolean
  /** The plot's height in pixels. */
  height?: number
  /** The dashboard filters this tile's source has no column for: drawn unfiltered by them. */
  ignored?: string[]
  /**
   * In a dashboard tile, whose height is set: the chart takes the tile's room, and its
   * badges and warnings are left to the tile's header (ChartInfo). The parent must be
   * a flex column; `height` is then unused.
   */
  fill?: boolean
  /** The tile's title: a scorecard leaves out its label when it says the same. */
  title?: string
  /** The tile's height in grid rows: from ROOMY_ROWS a tile's scorecard writes its change out. */
  rows?: number
}

/** A tile this many rows tall has room under a scorecard's number for its change as a sentence. */
const ROOMY_ROWS = 4

export function SavedChart({
  settings,
  draw,
  loading,
  height = CHART_HEIGHT,
  ignored = [],
  fill = false,
  title,
  rows,
}: SavedChartProps) {
  const view = useMemo<SavedChartView | null>(
    () => (draw?.result && !draw.break ? savedChartView(settings, draw.result) : null),
    [settings, draw],
  )

  if (loading && !draw) {
    return fill ? (
      <div className="min-h-0 w-full flex-1 animate-pulse rounded bg-zinc-100 dark:bg-zinc-900" />
    ) : (
      <div className="w-full animate-pulse rounded bg-zinc-100 dark:bg-zinc-900" style={{ height }} />
    )
  }
  if (!draw) return null

  const body = draw.break ? (
    <div
      role="status"
      className="flex flex-col items-center justify-center gap-1 rounded border border-dashed border-zinc-300 p-4 text-center text-sm text-zinc-600 dark:border-zinc-700 dark:text-zinc-400"
      style={{ minHeight: Math.min(height, 160) }}
    >
      <span className="font-medium text-zinc-800 dark:text-zinc-200">This chart cannot be drawn</span>
      <span>{draw.break.message}</span>
    </div>
  ) : draw.error ? (
    <div role="alert" className="rounded border border-red-200 bg-red-50 p-3 text-sm text-red-800 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-200">
      The source did not run: {draw.error}
    </div>
  ) : view ? (
    <ChartBody view={view} height={fill ? "fill" : height} title={title} roomy={fill && (rows ?? 0) >= ROOMY_ROWS} />
  ) : null
  const notes = view && "notes" in view && view.notes.length > 0 && (
    <p className="shrink-0 text-xs text-zinc-500 dark:text-zinc-400">{view.notes.join(" ")}</p>
  )

  if (!fill) {
    const warnings = chartWarnings(draw, ignored, settings.type)
    return (
      <div className="flex min-w-0 flex-col gap-2" data-testid="saved-chart">
        {draw.badges && <ChartBadgeRow badges={draw.badges} readAt={draw.result?.read_at} />}
        {warnings.length > 0 && (
          <ul className="space-y-1" aria-label="Warnings">
            {warnings.map((w) => (
              <li
                key={w}
                className="flex items-start gap-1.5 rounded border border-amber-200 bg-amber-50 px-2 py-1 text-xs text-amber-800 dark:border-amber-900/50 dark:bg-amber-950/30 dark:text-amber-200"
              >
                <TriangleAlert className="mt-0.5 h-3 w-3 shrink-0" aria-hidden />
                <span>{w}</span>
              </li>
            ))}
          </ul>
        )}
        {body}
        {notes}
      </div>
    )
  }
  // In a tile the body is the chart alone: its badges and warnings sit behind the
  // header's ChartInfo, so no box above it shrinks to one line and scrolls. A plot or
  // a table grows into the tile's room and keeps at least 80 px, under a 3-row tile's
  // 90 px body. A scorecard grows too, to centre its number, but from its own height,
  // which it never gives up; anything else (a message) keeps its own height.
  const drawn = view !== null && !draw.break && !draw.error
  const grows = drawn && (view.kind === "echart" || view.kind === "table")
  const centres = drawn && view.kind === "scorecard"
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-2" data-testid="saved-chart">
      <div
        className={
          grows
            ? "flex min-h-20 shrink-0 grow basis-0 flex-col overflow-hidden"
            : centres
              ? "flex shrink-0 grow flex-col"
              : "shrink-0"
        }
        data-testid="saved-chart-body"
      >
        {body}
      </div>
      {notes}
    </div>
  )
}

function ChartBody({
  view,
  height,
  title,
  roomy,
}: {
  view: SavedChartView
  height: number | "fill"
  title?: string
  roomy: boolean
}) {
  switch (view.kind) {
    case "message":
      return (
        <p role="status" className="rounded border border-dashed border-zinc-300 p-4 text-sm text-zinc-600 dark:border-zinc-700 dark:text-zinc-400">
          {view.message}
        </p>
      )
    case "scorecard":
      // Out of the flow, so it takes none of a tile's room.
      return (
        <>
          {view.description && <span className="sr-only">{view.description}</span>}
          <Scorecard view={view} tight={height === "fill"} roomy={roomy} title={title} />
        </>
      )
    case "table":
      return <ResultTable view={view} height={height} />
    case "echart":
      if (view.rows.length === 0) {
        return <p className="py-6 text-center text-sm text-zinc-500 dark:text-zinc-400">No rows to draw.</p>
      }
      return <EChartNoSSR settings={view.settings} rows={view.rows} height={height} />
  }
}

/**
 * tight: in a tile. A 3-row tile's body has 90 px for the label and the number (its
 * badges are in the tile's header), so the change goes beside the number in short ("↓ −5.0%"), the
 * sentence on hover, focus and to a screen reader, and nothing pads above or below.
 * roomy: a tile of ROOMY_ROWS or more has room under the number, so the sentence is
 * written out there and the pill beside the number is only its short form.
 * The number is centred in the room the tile has and scales with its width, from
 * text-3xl to 48 px on a 1.2 line (58 px: label 16 + gap 4 + 58 still fits 90). In a
 * font whose text box is taller than its line, the part past the line is clipped, or
 * that sliver scrolls the tile. The digits are inside it. The change is a pill coloured
 * by direction, up green and down red: a scorecard has no setting for which way is good.
 * A sparkline (24 px) goes under the number on a page and in a roomy tile; a 3-row
 * tile has no room for it, so it is left out there.
 */
function Scorecard({
  view,
  tight = false,
  roomy = false,
  title,
}: {
  view: Extract<SavedChartView, { kind: "scorecard" }>
  tight?: boolean
  roomy?: boolean
  title?: string
}) {
  const change =
    view.days !== undefined && view.value !== null && view.previous !== null && view.previous !== undefined
      ? Math.sign(view.value - view.previous)
      : null
  const Icon = change === null ? null : change > 0 ? ArrowUp : change < 0 ? ArrowDown : Minus
  const sentence =
    view.days === undefined
      ? null
      : changeText(view.value, view.previous, view.days) +
        (view.previous !== null && view.previous !== undefined ? ` (${formatNumber(view.previous, view.format)})` : "")
  // The label leaves out what the tile's title already says: the measure's name when
  // the title starts with it ("Loans" under "Loans, all statuses"), the period when the
  // title names it ("Card transactions · last 7 days"), and is left out when that is all.
  const t = title?.trim().toLowerCase() ?? ""
  const name = view.label.trim().toLowerCase()
  const titleNames = t.startsWith(name) && !/[\p{L}\p{N}]/u.test(t.charAt(name.length))
  const period = view.days === undefined ? null : view.days === 1 ? "day" : `${view.days} days`
  const titlePeriod = period !== null && t.includes(`last ${period}`)
  const labelText = titleNames
    ? period && !titlePeriod && `Last ${period}`
    : period && !titlePeriod
      ? `${view.label} · last ${period}`
      : view.label
  const label = labelText ? <span className="text-xs font-medium text-zinc-500 dark:text-zinc-400">{labelText}</span> : null
  const spark = view.spark ? view.spark.map((p) => p.value) : []
  const number = (
    <span
      className={cn(
        "font-semibold tabular-nums text-zinc-900 dark:text-zinc-50",
        tight ? "overflow-y-clip text-[clamp(1.875rem,8cqi,3rem)] leading-[1.2]" : "text-3xl",
      )}
      data-testid="scorecard-value"
    >
      {view.value === null ? "–" : formatNumber(view.value, view.format)}
    </span>
  )
  if (!tight) {
    return (
      <div className="flex flex-col gap-1 py-2">
        {label}
        {number}
        {sentence !== null && (
          <span className="flex items-center gap-1 text-xs text-zinc-600 dark:text-zinc-400" data-testid="scorecard-change">
            {Icon && <Icon className="h-3 w-3" aria-hidden />}
            {sentence}
          </span>
        )}
        <Sparkline values={spark} className="max-w-xs" />
      </div>
    )
  }
  const tone = change === null || change === 0 ? "cancel" : change > 0 ? "ok" : "fail"
  const pillClass = cn("inline-flex items-center gap-0.5 rounded-full px-2 py-0.5 text-xs font-medium", pillToneClass(tone))
  if (roomy) {
    // A taller tile has room for the sentence itself, so it is written out under the pill rather than kept for hover.
    return (
      <div className="@container flex flex-1 flex-col gap-1">
        {label}
        <div className="flex flex-1 flex-col items-center justify-center gap-1">
          <div className="flex flex-wrap items-center justify-center gap-x-2 gap-y-1">
            {number}
            {sentence !== null && (
              <span aria-hidden="true" className={pillClass} data-testid="scorecard-change">
                {Icon && <Icon className="h-3 w-3" />}
                {changeShort(view.value, view.previous) ?? "–"}
              </span>
            )}
          </div>
          {sentence !== null && (
            <span className="text-center text-xs text-zinc-600 dark:text-zinc-400" data-testid="scorecard-sentence">
              {sentence}
            </span>
          )}
          <Sparkline values={spark} className="max-w-[12rem]" />
        </div>
      </div>
    )
  }
  return (
    <div className="@container flex flex-1 flex-col gap-1">
      {label}
      <div className="flex flex-1 flex-wrap content-center items-center justify-center gap-x-2 gap-y-1">
        {number}
        {sentence !== null && (
          <TooltipProvider delayDuration={200}>
            <Tooltip>
              <TooltipTrigger asChild>
                <span
                  tabIndex={0}
                  className={cn(
                    pillClass,
                    "cursor-default focus:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400",
                  )}
                  data-testid="scorecard-change"
                >
                  {Icon && <Icon className="h-3 w-3" aria-hidden />}
                  <span aria-hidden="true">{changeShort(view.value, view.previous) ?? "–"}</span>
                  <span className="sr-only">{sentence}</span>
                </span>
              </TooltipTrigger>
              <TooltipPortal>
                <TooltipContent>{sentence}</TooltipContent>
              </TooltipPortal>
            </Tooltip>
          </TooltipProvider>
        )}
      </div>
    </div>
  )
}

function ResultTable({ view, height }: { view: Extract<SavedChartView, { kind: "table" }>; height: number | "fill" }) {
  if (view.body.length === 0) {
    return <p className="py-6 text-center text-sm text-zinc-500 dark:text-zinc-400">No rows.</p>
  }
  // Filling, a flex column holds it: it shrinks to the room there is and scrolls.
  return (
    <div
      className="min-h-0 overflow-auto rounded border border-zinc-200 dark:border-zinc-800"
      style={height === "fill" ? undefined : { maxHeight: height }}
    >
      <table className="w-full text-xs">
        {view.description && <caption className="sr-only">{view.description}</caption>}
        <thead className="sticky top-0 bg-zinc-50 dark:bg-zinc-900">
          <tr>
            {view.head.map((h, i) => (
              <th
                key={i}
                scope="col"
                className={`px-2 py-1.5 font-medium text-zinc-600 dark:text-zinc-300 ${view.numeric[i] ? "text-right" : "text-left"}`}
              >
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {view.body.map((row, r) => (
            // A pivot table's totals in bold on grey, set apart from its cells.
            <tr
              key={r}
              className={`border-t border-zinc-100 dark:border-zinc-800 ${view.totals?.[r] ? "bg-zinc-50 font-semibold dark:bg-zinc-900/60" : ""}`}
              data-total={view.totals?.[r] ? "" : undefined}
            >
              {row.map((cell, i) => (
                <td
                  key={i}
                  className={`whitespace-nowrap px-2 py-1 text-zinc-800 dark:text-zinc-200 ${view.numeric[i] ? "text-right tabular-nums" : ""}`}
                >
                  {cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** Data as of · on time or late · checks · lineage; a saved query says only when it was read. */
export function ChartBadgeRow({ badges, readAt }: { badges: ChartBadges; readAt?: string }) {
  const isModel = badges.source_kind === "model"
  const passed = badges.checks.filter((c) => c.status === "pass").length
  const item = "inline-flex items-center gap-1 rounded-full border px-2 py-0.5"
  return (
    <div className="flex flex-wrap items-center gap-1.5 text-[11px] text-zinc-600 dark:text-zinc-400" aria-label="About these rows">
      {isModel && (
        <span className={`${item} border-zinc-200 dark:border-zinc-700`}>
          Data as of{" "}
          <LocalDateTime value={badges.data_as_of} fallback="never built" />
        </span>
      )}
      {isModel && badges.deadline_seconds ? (
        badges.late ? (
          <span className={`${item} border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200`}>
            <TriangleAlert className="h-3 w-3" aria-hidden /> Late
          </span>
        ) : (
          <span className={`${item} border-zinc-200 dark:border-zinc-700`}>On time</span>
        )
      ) : null}
      {isModel && badges.checks.length > 0 && (
        <span
          className={`${item} ${
            passed < badges.checks.length
              ? "border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200"
              : "border-zinc-200 dark:border-zinc-700"
          }`}
          title={badges.checks.map((c) => `${c.name}: ${c.status}`).join("\n")}
        >
          Checks {passed}/{badges.checks.length} passed
        </span>
      )}
      {isModel && badges.lineage_url && (
        <Link href={badges.lineage_url} className={`${item} border-zinc-200 hover:bg-zinc-50 dark:border-zinc-700 dark:hover:bg-zinc-800`}>
          Lineage
        </Link>
      )}
      {readAt && (
        <span className={`${item} border-zinc-200 dark:border-zinc-700`}>
          Read <LocalDateTime value={readAt} />
        </span>
      )}
    </div>
  )
}
