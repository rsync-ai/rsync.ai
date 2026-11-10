"use client"

import { useMemo } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"

import { EChartNoSSR } from "@/components/charts/EChartNoSSR"
import { ChartCard } from "@/components/kit/ChartCard"
import { recentRunsChart, type ChartRun } from "@/components/runs/run-chart-specs"

/**
 * Run detail's "Last 10 runs" chart: this pipeline's recent runs as the page read
 * them (trends, newest first), one bar each, this run outlined. A bar opens its
 * run. The CDC stream's own row (its id is the pipeline's) is not a run.
 */
export function RecentRunsCard({
  runs,
  executionId,
  pipelineId,
}: {
  runs: readonly ChartRun[]
  executionId: string
  pipelineId: string
}) {
  const router = useRouter()
  const oldestFirst = useMemo(() => runs.filter((r) => r.execution_id !== pipelineId).reverse(), [runs, pipelineId])
  const chart = useMemo(
    () => recentRunsChart("This pipeline's recent runs: how long each took, coloured by outcome", oldestFirst, { highlight: executionId }),
    [oldestFirst, executionId],
  )
  if (oldestFirst.length === 0) return null

  return (
    <ChartCard
      title={`Last ${oldestFirst.length} ${oldestFirst.length === 1 ? "run" : "runs"} of this pipeline`}
      description={`Bar height is how long a run took, colour its outcome.${
        chart.settings.type === "bar" && chart.settings.highlight ? " This run is outlined." : ""
      } Click a bar to open its run.`}
    >
      <EChartNoSSR
        settings={chart.settings}
        rows={chart.rows}
        onCategoryClick={(i) => {
          const id = oldestFirst[i]?.execution_id
          if (id && id !== executionId) router.push(`/executions/${id}`)
        }}
      />
      <Link
        href={`/pipelines/${pipelineId}?tab=history`}
        className="mt-2 inline-block text-xs font-medium text-zinc-600 hover:underline dark:text-zinc-300"
      >
        All runs →
      </Link>
    </ChartCard>
  )
}
