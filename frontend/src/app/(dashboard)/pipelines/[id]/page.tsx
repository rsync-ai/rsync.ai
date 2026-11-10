import { notFound } from "next/navigation"
import { Suspense } from "react"
import Link from "next/link"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent } from "@/components/ui/card"
import { ArrowLeft, MessageSquare } from "lucide-react"
import { formatRelativeTime } from "@/lib/utils"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { PipelineActions } from "@/components/pipeline/PipelineActions"
import { CDCPipelineActions } from "@/components/pipeline/CDCPipelineActions"
import { SchemaDriftBadge } from "@/components/pipeline/SchemaDriftBadge"
import { PipelineScheduleCreateDialogLauncher } from "@/components/pipeline/PipelineSchedulePanel"
import { PipelineHeaderOverflowMenu } from "@/components/pipeline/PipelineHeaderOverflowMenu"
import { PipelineDetailTabsClient } from "@/components/pipeline/PipelineDetailTabsClient"
import { ConnectionChip } from "@/components/connectors/ConnectionChip"
import { CopyId } from "@/components/kit/CopyId"
import { API_ENDPOINTS, API_GATEWAY_URL_INTERNAL } from "@/lib/config/api"
import { connectionIsCDC, pipelineIsCDC } from "@/lib/pipeline/syncMode"
import { cookies } from "next/headers"
import { activeWorkspaceCookieHeader } from "@/lib/workspace/server-workspace"

export const dynamic = "force-dynamic"

interface Props {
  params: Promise<{ id: string }>
  searchParams: Promise<{ from?: string; tab?: string; editTables?: string; createSchedule?: string }>
}

// Fetch regular pipeline from API
async function getRegularPipeline(id: string) {
  try {
    const cookieStore = await cookies()
    const authToken = cookieStore.get("auth_token")?.value
    // API Gateway expects the raw token value (no Bearer prefix) in local dev.
    // Forward the active-workspace selection (cookie mirror) so the pipeline and
    // its connection fetch scope to the chosen workspace, not the personal one.
    const authHeaders: Record<string, string> = {
      ...(authToken ? { Authorization: authToken } : {}),
      ...activeWorkspaceCookieHeader(cookieStore),
    }

    const res = await fetch(API_ENDPOINTS.PIPELINES.GET_INTERNAL(id), {
      cache: 'no-store',
      signal: AbortSignal.timeout(5000),
      headers: authHeaders,
    })
    
    if (!res.ok) return null
    
    const rawPipeline = await res.json()
    if (!rawPipeline) return null

    // An explicit sync_mode wins; cdc_mode alone is not a CDC signal (see
    // lib/pipeline/syncMode.ts). The old `sync_mode === "cdc" || cdc_mode` test
    // badged batch pipelines "CDC" because they carry the connection's
    // cdc_mode='initial' column default.
    const pipelineMode = pipelineIsCDC(rawPipeline)
    let inferredType = pipelineMode ? ("cdc" as const) : ("etl" as const)

    // Only when the pipeline persisted no mode at all, fall back to the source
    // connection's sync_mode. This prevents legacy CDC pipelines from showing
    // scheduler UI without overriding an explicit sync_mode="batch".
    if (pipelineMode === null && rawPipeline?.source_connection_id) {
      try {
        const connRes = await fetch(`${API_GATEWAY_URL_INTERNAL}/api/v1/connections/${rawPipeline.source_connection_id}`, {
          cache: "no-store",
          signal: AbortSignal.timeout(4000),
          headers: authHeaders,
        })
        if (connRes.ok) {
          const conn = await connRes.json()
          if (connectionIsCDC(conn)) {
            inferredType = "cdc"
          }
        }
      } catch {
        // ignore; we'll fall back to pipeline fields only
      }
    }

    // CDC pipelines are continuous; treat pending/draft as running for UI consistency.
    let status = rawPipeline.status ?? "draft"
    if (inferredType === "cdc") {
      const s = String(status || "").toLowerCase()
      if (["draft", "pending", "active", "idle", "scheduled"].includes(s)) {
        status = "running"
      }
    }

    return {
      id: rawPipeline.id,
      name: rawPipeline.name,
      description: rawPipeline.description || "",
      type: inferredType,
      status,
      createdAt: rawPipeline.created_at ? new Date(rawPipeline.created_at) : null,
      updatedAt: rawPipeline.updated_at ? new Date(rawPipeline.updated_at) : null,
      sourceConnection: rawPipeline.source_connection as { id: string; name: string; connector_type: string; status: string } | null ?? null,
      destinationConnection: rawPipeline.destination_connection as { id: string; name: string; connector_type: string; status: string } | null ?? null,
      lastExecutionId: (rawPipeline.last_execution?.id as string | undefined) ?? null,
      // false when the destination is an app's API (reverse ETL): no Reload.
      reloadSupported: rawPipeline.reload_supported !== false,
    }
  } catch {
    return null
  }
}

export default async function PipelineDetailPage({ params, searchParams }: Props) {
  const { id } = await params
  const { from, tab } = await searchParams
  const isFromChat = from === "chat"
  const tabValue = ["overview", "history", "steps", "monitor", "table-stats", "transforms", "assessment"].includes(String(tab || ""))
    ? (String(tab) as "overview" | "history" | "steps" | "monitor" | "table-stats" | "transforms" | "assessment")
    : "overview"
  
  // Always use API Gateway as the source of truth (auth + consistent shape).
  const pipeline = await getRegularPipeline(id)

  if (!pipeline) {
    notFound()
  }

  return (
    <div className="space-y-5">
      {/* The one Create schedule dialog: the Overview's schedule panel and the ?createSchedule=1 link open it */}
      {pipeline.type !== "cdc" && <PipelineScheduleCreateDialogLauncher pipelineId={pipeline.id} />}
      {/* Header: name and type, then source → destination, when it was made and its id */}
      <div className="flex flex-col gap-4 @3xl:flex-row @3xl:items-start @3xl:justify-between">
        <div className="flex min-w-0 items-start gap-2">
          <Button variant="ghost" size="sm" asChild className="mt-0.5 h-8 w-8 shrink-0 p-0">
            <Link href="/pipelines" aria-label="Back to pipelines" title="Back to pipelines">
              <ArrowLeft className="h-4 w-4" />
            </Link>
          </Button>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="min-w-0 text-xl font-semibold tracking-tight text-zinc-900 dark:text-white break-words">
                {pipeline.name}
              </h1>
              <SchemaDriftBadge pipelineId={pipeline.id} />
              <Badge
                variant="outline"
                className={
                  pipeline.type === "cdc"
                    ? "bg-violet-50 text-violet-700 dark:bg-violet-900/30 dark:text-violet-300"
                    : "bg-blue-50 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300"
                }
              >
                {pipeline.type.toUpperCase()}
              </Badge>
            </div>
            <div className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-zinc-600 dark:text-zinc-400">
              {/* Connection identity strip — shows exactly which connections this pipeline uses */}
              {pipeline.sourceConnection || pipeline.destinationConnection ? (
                <span className="inline-flex min-w-0 items-center gap-1.5">
                  {pipeline.sourceConnection ? (
                    <ConnectionChip
                      name={pipeline.sourceConnection.name}
                      connectorType={pipeline.sourceConnection.connector_type}
                      className="max-w-[200px] font-medium text-zinc-800 dark:text-zinc-200"
                    />
                  ) : (
                    <span className="italic text-zinc-400">No source</span>
                  )}
                  <span className="shrink-0 text-zinc-400" aria-hidden>
                    →
                  </span>
                  <span className="sr-only">to</span>
                  {pipeline.destinationConnection ? (
                    <ConnectionChip
                      name={pipeline.destinationConnection.name}
                      connectorType={pipeline.destinationConnection.connector_type}
                      className="max-w-[200px] font-medium text-zinc-800 dark:text-zinc-200"
                    />
                  ) : (
                    <span className="italic text-zinc-400">No destination</span>
                  )}
                </span>
              ) : pipeline.description ? (
                <span>{pipeline.description}</span>
              ) : null}
              {/* Created / updated / id: they used to be a card under every tab. */}
              {pipeline.createdAt && (
                <span className="text-xs text-zinc-500 dark:text-zinc-400">
                  · Created <LocalDateTime value={pipeline.createdAt} />
                </span>
              )}
              {pipeline.updatedAt && (
                <span className="text-xs text-zinc-500 dark:text-zinc-400">
                  · Updated {formatRelativeTime(pipeline.updatedAt)}
                </span>
              )}
              <span className="inline-flex items-center gap-1 text-xs text-zinc-500 dark:text-zinc-400">
                · <CopyId value={pipeline.id} label="pipeline ID" />
              </span>
            </div>
          </div>
        </div>

        {/* Actions */}
        <div className="flex flex-wrap items-center justify-end gap-2">
          {pipeline.type === "cdc" ? (
            <CDCPipelineActions
              pipelineId={pipeline.id}
              pipelineName={pipeline.name}
              status={pipeline.status}
              destinationName={pipeline.destinationConnection?.name}
              reloadSupported={pipeline.reloadSupported}
            />
          ) : (
            <PipelineActions pipelineId={pipeline.id} status={pipeline.status} />
          )}
          <PipelineHeaderOverflowMenu
            pipelineId={pipeline.id}
            pipelineName={pipeline.name}
            pipelineType={pipeline.type}
            status={pipeline.status}
            lastExecutionId={pipeline.lastExecutionId}
          />
        </div>
      </div>

      {/* From Chat Banner */}
      {isFromChat && (
        <Card className="border-violet-200 bg-violet-50 dark:bg-violet-950/30 dark:border-violet-800">
          <CardContent className="py-3 px-4">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-2 text-violet-700 dark:text-violet-300">
                <MessageSquare className="h-4 w-4" />
                <span className="text-sm font-medium">Pipeline started from chat</span>
              </div>
              <Link href="/chat">
                <Button variant="ghost" size="sm" className="text-violet-600 hover:text-violet-700 hover:bg-violet-100 dark:text-violet-300 dark:hover:bg-violet-900/50">
                  <ArrowLeft className="h-4 w-4 mr-1" />
                  Back to chat
                </Button>
              </Link>
            </div>
          </CardContent>
        </Card>
      )}

      {/* Tabbed Content — wrap in Suspense so useSearchParams() in PipelineDetailTabsClient
           works correctly in Next.js 15+ (without this, the hook bails out of SSR and
           Radix Tabs context is broken, causing all triggers to render with tabIndex=-1). */}
      <Suspense fallback={
        <div className="h-10 w-full rounded-lg bg-zinc-100 dark:bg-zinc-800 animate-pulse" />
      }>
        {/* key={pipeline.id}: every panel below fetches its own data, and going
            from one pipeline to another stays inside this route segment, so
            without a key React reuses their state and each panel renders the
            pipeline the user just left until its own request comes back. The
            two hooks that produced the reported flap reset themselves as well;
            this covers the dozen other panels that would flap the same way. */}
        <PipelineDetailTabsClient key={pipeline.id} pipelineId={pipeline.id} pipelineType={pipeline.type} initialTab={tabValue} />
      </Suspense>

    </div>
  )
}
