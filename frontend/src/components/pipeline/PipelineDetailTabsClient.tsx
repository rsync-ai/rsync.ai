"use client"

import { useCallback, useEffect, useMemo, useState } from "react"
import { usePathname, useRouter, useSearchParams } from "next/navigation"

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useFeatureFlags } from "@/config/features"

import { DataLoadingStrategyCard } from "@/components/pipeline/DataLoadingStrategyCard"
import { PipelineSchedulePanel } from "@/components/pipeline/PipelineSchedulePanel"
import { PipelineLiveStatePanel } from "@/components/pipeline/PipelineLiveStatePanel"
import { PipelineRecentRuns } from "@/components/pipeline/PipelineRecentRuns"
import { SelfHealingPanel } from "@/components/pipeline/SelfHealingPanel"
import { PipelineMonitoringPanelNoSSR } from "@/components/pipeline/PipelineMonitoringPanelNoSSR"
import { ExecutionHistoryTab } from "@/components/pipeline/ExecutionHistoryTab"
import { StepsDAGTab } from "@/components/pipeline/StepsDAGTab"
import { PipelineTransformsTab } from "@/components/pipeline/PipelineTransformsTab"
import { PipelineCheckpointsCard } from "@/components/pipeline/PipelineCheckpointsCard"
import { CdcResnapshotCard } from "@/components/pipeline/CdcResnapshotCard"
import { CDCLagAlertsPanel } from "@/components/pipeline/CDCLagAlertsPanel"
import { PipelineHealthHeader } from "@/components/pipeline/PipelineHealthHeader"
import { MonitorTab } from "@/components/pipeline/MonitorTab"
import { MonitoringOverviewTab } from "@/components/pipeline/MonitoringOverviewTab"
import { AssessmentTab, AssessmentTabBadge } from "@/components/pipeline/AssessmentTab"

type TabValue = "overview" | "history" | "steps" | "table-stats" | "transforms" | "monitor" | "assessment"

function normalizeTab(v: unknown): TabValue {
  const s = String(v || "").trim()
  if (
    s === "overview" ||
    s === "history" ||
    s === "steps" ||
    s === "table-stats" ||
    s === "transforms" ||
    s === "monitor" ||
    s === "assessment"
  )
    return s
  return "overview"
}

export function PipelineDetailTabsClient(props: {
  pipelineId: string
  pipelineType: "etl" | "cdc"
  initialTab?: string
}) {
  const { pipelineId, pipelineType } = props
  const router = useRouter()
  const pathname = usePathname()
  const searchParams = useSearchParams()
  const flags = useFeatureFlags()

  const tabFromUrl = useMemo(() => normalizeTab(searchParams?.get("tab")), [searchParams])
  const [value, setValue] = useState<TabValue>(() => normalizeTab(props.initialTab))

  // Keep the tab UI in sync with the URL (required for deep-links and header actions).
  useEffect(() => {
    setValue(tabFromUrl)
  }, [tabFromUrl])

  const onValueChange = useCallback(
    (next: string) => {
      const v = normalizeTab(next)
      setValue(v)

      const sp = new URLSearchParams(searchParams?.toString() || "")
      if (v === "overview") sp.delete("tab")
      else sp.set("tab", v)

      // Tab-specific tokens should not leak between tabs.
      if (v !== "table-stats") sp.delete("editTables")

      const qs = sp.toString()
      router.push(qs ? `${pathname}?${qs}` : pathname)
    },
    [router, pathname, searchParams]
  )

  return (
    <div className="space-y-6">
      <PipelineHealthHeader pipelineId={pipelineId} />
      <Tabs value={value} onValueChange={onValueChange} className="space-y-6">
        {/* Seven tabs no longer fit one line below ~1046 px with the sidebar open,
            and the strip scrolled the whole page sideways; on a phone it widened
            the layout viewport to 757 px (#13). They wrap instead. */}
        <TabsList className="h-auto max-w-full flex-wrap justify-start">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="history">Runs</TabsTrigger>
          <TabsTrigger value="steps">Steps/DAG</TabsTrigger>
          <TabsTrigger value="table-stats">Table statistics</TabsTrigger>
          {/* Labeled "Data flow" (not "Monitor") so it doesn't collide with the
              transform-level monitoring that lives inside the Transforms tab. The
              tab value stays "monitor" to keep existing deep links working. */}
          <TabsTrigger value="monitor">Data flow</TabsTrigger>
          {/* Pre-migration assessment (DMS-style checks). The badge is the newest
              run's open Critical count, else its High count. */}
          <TabsTrigger value="assessment">
            Assessment
            <AssessmentTabBadge pipelineId={pipelineId} />
          </TabsTrigger>
          {/* Last: a pipeline's tables and data come first (Table statistics, Data
              flow, Assessment); Transforms is authoring, not monitoring. */}
          <TabsTrigger value="transforms">Transforms</TabsTrigger>
        </TabsList>

      {/* Overview Tab */}
      <TabsContent value="overview" className="space-y-6">
        {/* How the recent runs went (batch only — CDC has no discrete runs; the
            health header covers it). First: it is the page's answer to "is this
            pipeline working?". */}
        {pipelineType !== "cdc" && <PipelineRecentRuns pipelineId={pipelineId} />}

        {/* Data loading strategy (always visible) */}
        <DataLoadingStrategyCard pipelineId={pipelineId} />

        {/* Schedule (batch/ETL only). CDC pipelines are continuous and must not expose
            scheduler UI. The panel draws its own frame: a line when there is none. */}
        {pipelineType !== "cdc" && <PipelineSchedulePanel pipelineId={pipelineId} />}

        {/* Live State */}
        <PipelineLiveStatePanel pipelineId={pipelineId} />

        {/* What watches this pipeline, what the heal agent did and why, and
            (CDC) how many Sentinel alerts are open. Always shown: a card that
            appeared only after a failure left a healthy pipeline with no sign
            that anything was watching it. Below the live state on purpose:
            healing is always a reaction to what that panel is showing. */}
        <SelfHealingPanel pipelineId={pipelineId} pipelineType={pipelineType} />
      </TabsContent>

      {/* Runs tab: this pipeline's runs (the sidebar's Runs page lists every kind).
          No card round it: its charts and its run list are cards of their own. */}
      <TabsContent value="history">
        <ExecutionHistoryTab pipelineId={pipelineId} />
      </TabsContent>

      {/* Steps/DAG, Assessment and Transforms sit on the page like Runs: no card
          round them, since each draws its own cards and a card in a card read
          as a box in a box. */}
      {/* Steps/DAG Tab */}
      <TabsContent value="steps">
        <StepsDAGTab pipelineId={pipelineId} />
      </TabsContent>

      {/* Table statistics Tab: no card round it either; the panel draws its cards. */}
      <TabsContent value="table-stats" className="space-y-6">
        <PipelineMonitoringPanelNoSSR pipelineId={pipelineId} variant="table_stats" pipelineType={pipelineType} />
        {/* Per-table resume positions. Below the stats on purpose: the panel
            above says how much moved per table, this says where each table is
            resumable from. Same granularity, so the same tab. */}
        <PipelineCheckpointsCard pipelineId={pipelineId} pipelineType={pipelineType} />
        {/* Re-read a table that is already streaming (CDC only). Last in the
            tab on purpose: the two panels above say how much moved per table
            and where each one resumes from — this is the action you take when
            those numbers say a table needs reloading. */}
        {pipelineType === "cdc" && <CdcResnapshotCard pipelineId={pipelineId} />}
      </TabsContent>

      {/* Monitor Tab ("Data flow") */}
      {/* One column, top to bottom: lag alerts, the Overview tiles, the numbers
          (Throughput or Flow, Consumers, Dependencies), then the Activity feed.
          No card here repeats the run's status, its current-run link,
          Pause/Resume/Stop or Diagnose: the page header owns those. The tiles
          and the feed used to be two sub-tabs of a second monitoring card. */}
      <TabsContent value="monitor" className="space-y-6">
        {/* CDC Source Lag Alerts (CDC pipelines only) — moved here from Overview. */}
        {pipelineType === "cdc" && (
          <CDCLagAlertsPanel pipelineId={pipelineId} />
        )}
        {flags.monitoringOverview && (
          <MonitoringOverviewTab pipelineId={pipelineId} />
        )}
        <MonitorTab pipelineId={pipelineId} pipelineType={pipelineType} />
        <PipelineMonitoringPanelNoSSR pipelineId={pipelineId} variant="activity" pipelineType={pipelineType} />
      </TabsContent>

      {/* Assessment Tab */}
      <TabsContent value="assessment">
        <AssessmentTab pipelineId={pipelineId} pipelineType={pipelineType} />
      </TabsContent>

      {/* Transforms Tab */}
      <TabsContent value="transforms">
        <PipelineTransformsTab pipelineId={pipelineId} />
      </TabsContent>
      </Tabs>
    </div>
  )
}

