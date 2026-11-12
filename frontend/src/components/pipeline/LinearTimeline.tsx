"use client"

import { ArrowDown, Clock } from "lucide-react"
import { Card, CardContent } from "@/components/ui/card"
import { Progress } from "@/components/ui/progress"
import { getConnectorLogoUrl } from "@/lib/api/mcp-connectors"
import { ConnectorLogo } from "@/components/connectors/ConnectorLogo"
import { formatDateTime } from "@/lib/utils"
import { stageDurationMs } from "./dagHelpers"
import { formatDuration } from "@/lib/duration"
import { getStageStatusConfig, type ExecutionPlanStage } from "./dagTypes"

// Tailwind border-l class for timeline cards
function getNodeKindBorderColor(kind: string): string {
  const map: Record<string, string> = {
    source: "border-l-blue-400",
    destination: "border-l-emerald-400",
    transform: "border-l-amber-400",
    llm: "border-l-violet-400",
    condition: "border-l-orange-400",
    api_call: "border-l-indigo-400",
  }
  return map[kind] || ""
}

// Tailwind text color class for kind label in timeline
function getNodeKindTextColor(kind: string): string {
  const map: Record<string, string> = {
    source: "text-blue-600 dark:text-blue-400",
    destination: "text-emerald-600 dark:text-emerald-400",
    transform: "text-amber-600 dark:text-amber-400",
    llm: "text-violet-600 dark:text-violet-400",
    condition: "text-orange-600 dark:text-orange-400",
    api_call: "text-indigo-600 dark:text-indigo-400",
  }
  return map[kind] || "text-zinc-500 dark:text-zinc-400"
}

export interface LinearTimelineProps {
  stages: ExecutionPlanStage[]
  compact?: boolean
  onStageClick?: (stageId: string) => void
  selectedStageId?: string | null
}

/** A pipeline's stages as a vertical list of cards, one under the other: the Steps tab's list view. */
export function LinearTimeline({ stages, compact = false, onStageClick, selectedStageId }: LinearTimelineProps) {
  return (
    <div className={compact ? "space-y-2" : "space-y-4"}>
      {stages.map((stage, index) => {
        const config = getStageStatusConfig(stage.status)
        const StatusIcon = config.icon
        const isLast = index === stages.length - 1
        const nodeKind = String(stage.node_kind || stage.metadata?.node_kind || "").toLowerCase()
        const connectorType =
          (stage.metadata?.resolved_connector_type as string) ||
          (stage.metadata?.node_config?.connector_type as string) ||
          undefined
        const showConnectorLogo =
          (nodeKind === "source" || nodeKind === "destination") && connectorType && connectorType !== "storage"
        const isSelected = selectedStageId === stage.id
        const isClickable = !!onStageClick
        // Kind takes priority for left border when available; status otherwise
        const kindBorder = getNodeKindBorderColor(nodeKind)
        const borderClass = kindBorder || config.bgColor.replace("bg-", "border-l-")
        const kindTextColor = getNodeKindTextColor(nodeKind)
        const hasResult = !!(stage.result_summary && (stage.status === "complete" || stage.status === "failed"))
        const durationMs = stageDurationMs(stage)

        return (
          <div key={stage.id}>
            <Card
              className={`border-l-4 ${borderClass} ${isSelected ? "ring-2 ring-violet-500" : ""} ${isClickable ? "cursor-pointer hover:shadow-md transition-shadow" : ""}`}
              onClick={() => onStageClick?.(stage.id)}
            >
              <CardContent className={compact ? "p-3" : "p-4"}>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-4 min-w-0">
                    <div className={`rounded-full ${compact ? "p-2" : "p-3"} ${config.bgColor} shrink-0`}>
                      <StatusIcon
                        className={`${compact ? "h-4 w-4" : "h-5 w-5"} ${config.color} ${stage.status === "running" ? "animate-spin" : ""}`}
                      />
                    </div>
                    <div className="min-w-0">
                      <div className={`font-semibold text-zinc-900 dark:text-white flex items-center gap-2 ${compact ? "text-sm" : ""}`}>
                        {showConnectorLogo ? (
                          <ConnectorLogo
                            connectorType={connectorType!}
                            logoUrl={getConnectorLogoUrl(connectorType!)}
                            size="sm"
                            className="bg-white dark:bg-zinc-900 border border-zinc-200 dark:border-zinc-800"
                          />
                        ) : null}
                        <span>
                          {stage.icon} {stage.display_name}
                        </span>
                        {nodeKind && nodeKind !== "default" && !compact && (
                          <span className={`text-xs font-normal ${kindTextColor}`}>
                            {nodeKind}
                          </span>
                        )}
                      </div>
                      {!compact && stage.description && (
                        <div className="text-sm text-zinc-600 dark:text-zinc-400 mt-1">
                          {stage.description}
                        </div>
                      )}
                      {/* Result summary — most useful data from Temporal */}
                      {hasResult && (
                        <div className={`${compact ? "text-xs mt-1" : "text-sm mt-1"} text-zinc-500 dark:text-zinc-400`}>
                          {stage.result_summary}
                        </div>
                      )}
                      {stage.error_message && (
                        <div className={`text-red-600 dark:text-red-400 ${compact ? "text-xs mt-1" : "text-sm mt-1"}`}>
                          {stage.error_message}
                        </div>
                      )}
                    </div>
                  </div>

                  <div className={`text-right shrink-0 ml-4 ${compact ? "space-y-1" : "space-y-2"}`}>
                    {typeof stage.progress === "number" && stage.progress > 0 && (
                      <div className="flex items-center gap-2 justify-end">
                        <Progress value={stage.progress} className="w-24" />
                        <span className="text-sm text-zinc-600 dark:text-zinc-400">{stage.progress}%</span>
                      </div>
                    )}
                    {/* Duration pill */}
                    {durationMs !== null && durationMs > 0 && (
                      <div className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 ${compact ? "text-xs" : "text-xs"} font-medium bg-zinc-100 dark:bg-zinc-800 text-zinc-600 dark:text-zinc-400`}>
                        <Clock className="h-3 w-3" />
                        {formatDuration(durationMs)}
                      </div>
                    )}
                    {!compact && stage.started_at && (
                      <div className="text-xs text-zinc-400">
                        {formatDateTime(new Date(stage.started_at))}
                      </div>
                    )}
                  </div>
                </div>
              </CardContent>
            </Card>

            {/* Connector arrow */}
            {!isLast && (
              <div className={`flex justify-center ${compact ? "py-1" : "py-2"}`}>
                <ArrowDown className={`${compact ? "h-4 w-4" : "h-5 w-5"} text-zinc-400`} />
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}
