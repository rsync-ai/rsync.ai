/**
 * What a "connection_config" pause needs from the user, read from its
 * blocking_reason.details. One parser for the chat (useHITLState) and the
 * pipeline page (PipelineLiveStatePanel), which had drifted apart.
 *
 * Producers vary:
 * - the Temporal workflow: { source_type, destination_type, missing_connections: [{type, direction}] }
 * - the legacy orchestrator worker: { source_connector, destination_connector, missing_connections: [{connector_type, direction}] }
 * - a run that stopped on a missing setting (connection_fix_wait.go):
 *   missing_connections: [{direction, connection_id, connector_type, missing_fields}]
 */

export type ConnectionDirection = "source" | "destination"

/** A setting to fill in on a connection the run already uses. */
export interface ConnectionFix {
  direction: ConnectionDirection
  connectionId: string
  connectorType: string
  missingFields: string[]
}

export interface ConnectionRequirements {
  source: { connector_type: string }
  destination: { connector_type: string }
  // Directions the user must still pick. The backend lists ONLY unresolved
  // directions in `missing_connections`; absent → both required.
  missing?: ConnectionDirection[]
  // Settings to fill in on an existing connection; the dialog then edits that
  // connection and resumes with the same IDs instead of offering a picker.
  fixes?: ConnectionFix[]
  sourceConnectionId?: string
  destinationConnectionId?: string
}

function asRecord(v: unknown): Record<string, unknown> {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {}
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : ""
}

export function parseConnectionRequirements(rawDetails: unknown): ConnectionRequirements {
  const details = asRecord(rawDetails)
  const required = asRecord(details["required_connections"])
  const requiredConnectors = asRecord(details["required_connectors"])

  let sourceType =
    str(asRecord(required["source"])["connector_type"]) ||
    str(details["source_connector"]) ||
    str(details["source_type"]) ||
    str(requiredConnectors["source"])
  let destType =
    str(asRecord(required["destination"])["connector_type"]) ||
    str(details["destination_connector"]) ||
    str(details["destination_type"]) ||
    str(requiredConnectors["destination"])

  const sourceConnectionId = str(details["source_connection_id"])
  const destinationConnectionId = str(details["destination_connection_id"])

  const dirs: ConnectionDirection[] = []
  const fixes: ConnectionFix[] = []
  const missingRaw = Array.isArray(details["missing_connections"]) ? details["missing_connections"] : []
  for (const item of missingRaw) {
    const m = asRecord(item)
    const dir = str(m["direction"]).toLowerCase()
    if (dir !== "source" && dir !== "destination") continue
    const typ = str(m["type"]) || str(m["connector_type"]) || str(m["connector"])
    if (!dirs.includes(dir)) dirs.push(dir)
    if (dir === "source" && typ && !sourceType) sourceType = typ
    if (dir === "destination" && typ && !destType) destType = typ

    const fields = (Array.isArray(m["missing_fields"]) ? m["missing_fields"] : []).map(str).filter(Boolean)
    const connectionId =
      str(m["connection_id"]) || (dir === "source" ? sourceConnectionId : destinationConnectionId)
    if (fields.length > 0 && connectionId) {
      fixes.push({
        direction: dir,
        connectionId,
        connectorType: typ || (dir === "source" ? sourceType : destType),
        missingFields: Array.from(new Set(fields)),
      })
    }
  }

  const missing = dirs.length > 0 ? dirs : undefined
  // Only a direction the user must actually choose gets the "unknown"
  // placeholder: a resolved one left "unknown" rendered the unsatisfiable
  // "No Unknown sources found" picker.
  const needsSource = !missing || missing.includes("source")
  const needsDest = !missing || missing.includes("destination")

  return {
    source: { connector_type: sourceType || (needsSource ? "unknown" : "") },
    destination: { connector_type: destType || (needsDest ? "unknown" : "") },
    missing,
    ...(fixes.length > 0 ? { fixes } : {}),
    ...(sourceConnectionId ? { sourceConnectionId } : {}),
    ...(destinationConnectionId ? { destinationConnectionId } : {}),
  }
}

/** The connection's page with its edit form open (connections/[id]/page.tsx reads ?edit=1). */
export function connectionEditHref(fix: Pick<ConnectionFix, "connectionId" | "direction">): string {
  return `/connections/${encodeURIComponent(fix.connectionId)}?type=${fix.direction}&edit=1`
}
