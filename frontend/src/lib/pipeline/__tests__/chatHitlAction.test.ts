import { describe, it, expect } from "vitest"
import { readFileSync } from "node:fs"
import { resolve } from "node:path"
import { chatHitlAction, type ChatHitlAction, type ChatHitlHandlers } from "../chatHitlAction"

const ALL: ChatHitlHandlers = { selectTables: true, configureConnections: true, generateConnectors: true }
const NONE: ChatHitlHandlers = { selectTables: false, configureConnections: false, generateConnectors: false }

const WORKFLOWS = resolve(__dirname, "../../../../../backend-temporal-adapter/internal/workflows")

/**
 * Every blocking_reason_type the adapter can write to pipeline_progress: the
 * fifth argument of each emitPipelineWaitingEvent call, each literal
 * "blocking_reason_type" in a state update, and StateUpdateActivity's fallback
 * for a wait that names none.
 */
function adapterBlockingTypes(): Set<string> {
  const src = ["nl_pipeline_v2_workflow.go", "dag_scheduler.go", "activities.go"]
    .map((f) => readFileSync(resolve(WORKFLOWS, f), "utf8"))
    .join("\n")
  const found = new Set<string>()
  for (const m of src.matchAll(/emitPipelineWaitingEvent\((?:[^,()]*,){4}\s*"(\w+)"/g)) found.add(m[1])
  for (const m of src.matchAll(/"blocking_reason_type":\s*"(\w+)"/g)) found.add(m[1])
  for (const m of src.matchAll(/\bbt = "(\w+)"/g)) found.add(m[1])
  return found
}

// What the chat card does for each type, decided once. A type the adapter starts
// writing fails the census below until it is given a row here.
const DECIDED: Record<string, ChatHitlAction> = {
  table_selection: "select_tables",
  connection_config: "configure_connections",
  connector_generation: "generate_connectors",
  paused_by_user: "resume",
  // A schema change the run would not apply now fails the run at once; this park
  // is kept only so runs recorded before that change replay, and nothing sends
  // its answer (nl_pipeline_v2_workflow.go, schemaApprovalFailsFastVersion).
  approval: "unsupported",
  // DAG mode is off, and the chat has no node-input form. A node wait that asks
  // for tables is the one exception (below).
  node_input: "unsupported",
  // StateUpdateActivity's fallback when a wait names no type: nothing says what
  // the run wants.
  user_input_required: "unsupported",
}

describe("chatHitlAction", () => {
  it("census: every blocking type the adapter writes has a decided action", () => {
    const written = adapterBlockingTypes()
    // Denominator: the scan sees the types it is known to write.
    for (const known of ["table_selection", "connection_config", "connector_generation", "approval", "node_input", "paused_by_user", "user_input_required"]) {
      expect(written, `the scan missed ${known}; this census cannot fail`).toContain(known)
    }
    const undecided = [...written].filter((t) => !(t in DECIDED)).sort()
    expect(undecided, "blocking types the chat card has no decision for").toEqual([])
  })

  it.each(Object.entries(DECIDED))("%s → %s", (type, want) => {
    expect(chatHitlAction({ blockingType: type, requestType: type, widget: "" }, ALL)).toBe(want)
  })

  it.each([
    // A DAG node that waits for tables carries the executor's action_needed.
    ["node_input", "table_selection", "", "select_tables"],
    // The orchestrator's connection check asks through request_type.
    ["user_input_required", "connection_fix", "", "configure_connections"],
    // ui_hints.widget wins over a type that names nothing.
    ["user_input_required", "user_input_required", "connector_generator", "generate_connectors"],
    // A type the card does not know is never routed to a handler that answers something else.
    ["needs_export_sign_off", "needs_export_sign_off", "", "unsupported"],
    ["", "", "", "unsupported"],
  ] as const)("%s (request %s, widget %s) → %s", (blockingType, requestType, widget, want) => {
    expect(chatHitlAction({ blockingType, requestType, widget }, ALL)).toBe(want)
  })

  it("a known request whose handler the chat did not pass is unsupported, not a dead button", () => {
    for (const type of ["table_selection", "connection_config", "connector_generation"]) {
      expect(chatHitlAction({ blockingType: type, requestType: type, widget: "" }, NONE)).toBe("unsupported")
    }
    // Resume needs no chat handler: the card posts it itself.
    expect(chatHitlAction({ blockingType: "paused_by_user", requestType: "paused_by_user", widget: "" }, NONE)).toBe("resume")
  })
})
