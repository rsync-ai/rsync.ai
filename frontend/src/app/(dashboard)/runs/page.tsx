import { PageHeader } from "@/components/layout/PageHeader"
import { ActivityMetricsProvider } from "@/components/runs/ActivityMetricsProvider"
import { RunsList } from "@/components/runs/RunsList"
import { ViewerTimeZone } from "@/components/ui/local-date-time"
import { isActivityKind, isActivityStatus } from "@/lib/api/activity"

export const dynamic = "force-dynamic"

/**
 * Runs: every pipeline run, scheduled-query run and workflow run in the active
 * workspace, newest first. It replaced the Executions list, which showed pipeline
 * runs only and stopped at 200; /executions redirects here, and a pipeline run
 * still opens at /executions/:id. The run charts are on Home (dashboards-design.md
 * decision 11); Runs shows only the last 7 days' totals above its list, from the
 * same read.
 */
export default async function RunsPage({
  searchParams,
}: {
  searchParams: Promise<{ kind?: string; status?: string }>
}) {
  const { kind, status } = await searchParams
  return (
    <div className="space-y-5">
      <PageHeader
        heading="Runs"
        description={
          <>
            Every pipeline, model and workflow run in this workspace, newest first.
            <ViewerTimeZone />
          </>
        }
      />
      <ActivityMetricsProvider>
        <RunsList
          initialKind={isActivityKind(kind) ? kind : undefined}
          initialStatus={isActivityStatus(status) ? status : undefined}
        />
      </ActivityMetricsProvider>
    </div>
  )
}
