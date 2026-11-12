"use client"

/**
 * A drawn chart's "Download" (dashboards-design.md § 8, D3): a chart as a PNG or an SVG
 * image, a table as a CSV file. Made in the browser from what the chart already read,
 * so it runs no query; a scorecard, a message, a break or an error offers nothing.
 */

import { Download, Loader2 } from "lucide-react"
import { useMemo, useState, type RefObject } from "react"
import { toast } from "sonner"

import { savedChartView, tableExport } from "@/components/charts/saved-chart"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { ChartDraw, ServerChartSettings } from "@/lib/api/charts"
import { toCsv } from "@/lib/export/csv"
import { downloadBlob } from "@/lib/export/download"
import { exportFilename } from "@/lib/export/filename"

/** The plot's size when the page has none to measure (it is hidden, or not laid out yet). */
const FALLBACK_PLOT = { width: 800, height: 400 }

type Format = "png" | "svg" | "csv"

export function ChartExportMenu({
  name,
  settings,
  draw,
  plot,
}: {
  name: string
  settings: ServerChartSettings
  draw: ChartDraw | null
  /** An element holding the chart as drawn on the page: an image takes its plot's size. */
  plot: RefObject<HTMLElement | null>
}) {
  const [busy, setBusy] = useState(false)
  const view = useMemo(
    () => (draw?.result && !draw.break && !draw.error ? savedChartView(settings, draw.result) : null),
    [settings, draw],
  )
  const formats: { format: Format; label: string }[] =
    view?.kind === "echart" && view.rows.length > 0
      ? [
          { format: "png", label: "PNG image" },
          { format: "svg", label: "SVG image" },
        ]
      : view?.kind === "table"
        ? [{ format: "csv", label: "CSV file" }]
        : []
  if (!view || formats.length === 0) return null

  const save = async (format: Format) => {
    if (busy || !draw?.result) return
    setBusy(true)
    try {
      let blob: Blob
      if (format === "csv") {
        const table = tableExport(settings, draw.result)
        blob = new Blob([toCsv(table.head, table.body, table.numeric)], { type: "text/csv;charset=utf-8" })
      } else {
        if (view.kind !== "echart") return
        const el = plot.current?.querySelector<HTMLElement>('[data-testid="echart"]')
        const size =
          el && el.clientWidth > 0 && el.clientHeight > 0 ? { width: el.clientWidth, height: el.clientHeight } : FALLBACK_PLOT
        // ECharts' image code loads only when an image is asked for.
        const { chartImage, chartPng } = await import("@/lib/export/chart-image")
        const image = await chartImage(name, view.settings, view.rows, size)
        blob = format === "svg" ? new Blob([image.svg], { type: "image/svg+xml;charset=utf-8" }) : await chartPng(image)
      }
      downloadBlob(blob, exportFilename(name, format, "chart").utf8)
    } catch (err) {
      toast.error(err instanceof Error && err.message ? `Could not download the chart: ${err.message}` : "Could not download the chart")
    } finally {
      setBusy(false)
    }
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        className="-my-0.5 inline-flex h-6 min-w-6 shrink-0 items-center justify-center gap-1 rounded px-1 text-zinc-500 hover:bg-zinc-100 hover:text-zinc-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400 data-[state=open]:bg-zinc-100 data-[state=open]:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100 dark:data-[state=open]:bg-zinc-800 dark:data-[state=open]:text-zinc-100"
        aria-label={`Download ${name}`}
        title="Download"
        disabled={busy}
        data-testid="chart-export"
      >
        {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden /> : <Download className="h-3.5 w-3.5" aria-hidden />}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[9rem]">
        <DropdownMenuLabel className="text-xs font-medium text-zinc-500 dark:text-zinc-400">Download</DropdownMenuLabel>
        {formats.map((f) => (
          <DropdownMenuItem key={f.format} onSelect={() => void save(f.format)}>
            {f.label}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
