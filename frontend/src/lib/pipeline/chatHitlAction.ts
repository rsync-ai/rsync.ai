// What the chat card's "Action required" box offers for a parked run.
//
// The card used to know three requests (tables, connections, connectors). Any
// other blocking type fell through to a JSON-schema form whose Submit handed the
// answer to whichever of those three handlers the chat passed, so it opened the
// table picker for a request that had nothing to do with tables. With no schema
// it rendered nothing: an "Action required" box with no action in it. That is
// what a user's own pause (paused_by_user) and a schema approval (approval) got.
//
// Every type the adapter writes now gets either a real action or the explicit
// "not supported by this build" notice. Nothing is ever posted to an endpoint
// that does not answer the request.

export type ChatHitlAction =
  | "select_tables"
  | "configure_connections"
  | "generate_connectors"
  | "resume"
  | "unsupported"

export interface ChatHitlRequest {
  /** blocking_reason.type, as pipeline_progress.blocking_reason_type stores it. */
  blockingType: string
  /** details.request_type || details.action_needed || blockingType. */
  requestType: string
  /** details.ui_hints.widget, or "". */
  widget: string
}

/** Which of the chat's own handlers the card was given. */
export interface ChatHitlHandlers {
  selectTables: boolean
  configureConnections: boolean
  generateConnectors: boolean
}

/**
 * The adapter writes paused_by_user when the user pauses a run
 * (nl_pipeline_v2_workflow.go, the pause handler) and waits for the "resume"
 * signal, which POST /pipelines/:id/resume sends.
 */
export const PAUSED_BY_USER = "paused_by_user"

export function chatHitlAction(req: ChatHitlRequest, has: ChatHitlHandlers): ChatHitlAction {
  const type = String(req.blockingType || "").trim()
  const requestType = String(req.requestType || "")
  const widget = String(req.widget || "")

  if (type === PAUSED_BY_USER) return "resume"
  if ((widget === "table_selector" || requestType.includes("table")) && has.selectTables) {
    return "select_tables"
  }
  if ((widget === "connection_configurator" || requestType.includes("connection")) && has.configureConnections) {
    return "configure_connections"
  }
  if ((widget === "connector_generator" || requestType.includes("connector")) && has.generateConnectors) {
    return "generate_connectors"
  }
  return "unsupported"
}
