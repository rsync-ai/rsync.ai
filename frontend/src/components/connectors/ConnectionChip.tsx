"use client"

import { memo } from "react"

import { ConnectorLogo } from "@/components/connectors/ConnectorLogo"
import { getConnectorLogoUrl } from "@/lib/api/mcp-connectors"
import { getConnectorDisplayName } from "@/lib/types/mcp-connector"
import { cn } from "@/lib/utils"

/**
 * A connection as its connector's logo and its own name ("orders-db"), the
 * connector type in the tooltip. Memoised: the Pipelines list polls every few
 * seconds, and an unchanged row keeps its logo element instead of re-rendering it.
 */
export const ConnectionChip = memo(function ConnectionChip({
  name,
  connectorType,
  className,
}: {
  name?: string
  connectorType?: string
  className?: string
}) {
  const type = connectorType ? getConnectorDisplayName(connectorType) : ""
  const label = name?.trim() || type || "—"
  return (
    <span
      className={cn("inline-flex min-w-0 items-center gap-1.5", className)}
      title={type && type !== label ? `${label} (${type})` : label}
    >
      {connectorType && (
        <ConnectorLogo
          connectorType={connectorType}
          logoUrl={getConnectorLogoUrl(connectorType)}
          size="sm"
          className="h-5 w-5 shrink-0 rounded [&>img]:h-full [&>img]:w-full [&>img]:p-0.5"
        />
      )}
      <span className="truncate">{label}</span>
    </span>
  )
})
