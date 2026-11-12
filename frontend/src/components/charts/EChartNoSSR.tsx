"use client"

import dynamic from "next/dynamic"

import { ChartBoundary } from "@/components/charts/ChartBoundary"
import type { ChartProps } from "@/components/charts/chart-settings"

const LazyEChart = dynamic(
  () => import("@/components/charts/EChart").then((m) => m.EChart),
  { ssr: false, loading: () => <div className="h-[180px] w-full animate-pulse rounded bg-zinc-100 dark:bg-zinc-900" /> },
)

/**
 * EChart in its own chunk, loaded in the browser only when a chart is drawn, so a
 * page that draws none never downloads ECharts. The one way to draw a chart.
 */
export function EChartNoSSR({ settings, rows, height, onCategoryClick }: ChartProps) {
  return (
    <ChartBoundary resetKeys={[settings, rows]}>
      <LazyEChart settings={settings} rows={rows} height={height} onCategoryClick={onCategoryClick} />
    </ChartBoundary>
  )
}
