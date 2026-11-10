/**
 * API Response Types
 */

// Execution types
export interface Execution {
  id: string
  pipelineId: string
  status: ExecutionStatus
  startedAt: string
  finishedAt?: string
  error?: string
  nodeResults: NodeResult[]
  inputData?: Record<string, unknown>
  outputData?: Record<string, unknown>
  userId: string
}

export type ExecutionStatus = "pending" | "running" | "success" | "failed" | "cancelled"

export interface NodeResult {
  nodeId: string
  status: "pending" | "running" | "success" | "failed"
  startedAt?: string
  finishedAt?: string
  output?: unknown
  error?: string
}

// Note: Connector type moved to @/lib/types/mcp-connector.ts as MCPConnector

// ─── Pipeline State (GET /api/v1/pipelines/:id/state) ─────────────────────────

export interface BlockingReasonDetails {
  source_connection_id?: string
  destination_connection_id?: string
  source_type?: string
  // The database a table-selection pause lists (#14); "" when the tables span several.
  source_database?: string
  // The source connection names no database (a server-level MySQL/MongoDB/
  // ClickHouse connection): its databases are mirrored at the destination.
  source_server_level?: boolean
  // Discovery totals: the source's table count, and whether available_tables is only part of it.
  total_tables_available?: number
  tables_truncated?: boolean
  destination_type?: string
  node_id?: string
  user_request?: string
  available_tables?: Array<Record<string, unknown>>
  validation_errors?: Record<string, unknown>
  missing_connections?: Array<Record<string, unknown>>
  missing_connectors?: Array<Record<string, unknown>>
  selected_tables?: string[]
  ui_hints?: { widget?: string; [key: string]: unknown }
  source_connector?: string
  destination_connector?: string
  required_connectors?: { source?: string; destination?: string; [key: string]: unknown }
  input_schema?: Record<string, unknown>
  [key: string]: unknown
}

export interface PipelineStateResponse {
  pipeline_id: string
  execution_id?: string
  trace_id?: string
  schema_version?: number
  status: string
  current_stage?: string
  stage_group?: string
  state?: string
  message?: string
  summary?: string
  error_message?: string
  started_at?: string
  last_heartbeat_at?: string
  duration_ms?: number
  attempt?: number
  max_attempts?: number
  created_at?: string
  updated_at?: string
  is_stale?: boolean
  stale_reason?: string
  stale_elapsed_seconds?: number
  cancel_recommended?: boolean
  blocking_reason_type?: string
  progress?: {
    percent?: number
    current_step?: number
    total_steps?: number
    stage?: string
  }
  blocking_reason?: {
    type?: string
    description?: string
    estimated_seconds?: number
    available_tables?: Array<Record<string, unknown>>
    details?: BlockingReasonDetails
  }
  execution_plan?: {
    mode?: string
    metadata?: Record<string, unknown>
    stages?: Array<{
      id?: string
      display_name?: string
      description?: string
      icon?: string
      status?: string
      progress?: number
      started_at?: string
      completed_at?: string
      /** Milliseconds the adapter timed the stage at; read via `stageDurationMs()`. */
      actual_duration_ms?: number
      /** Legacy whole seconds, for plans persisted before `actual_duration_ms`. */
      actual_duration?: number
    }>
  }
}

// ─── API Error Response ────────────────────────────────────────────────────────

export interface ApiErrorBody {
  error?: string
  message?: string
  details?: unknown
}

// ─── WebSocket Domain Event Data ──────────────────────────────────────────────

export interface DomainEventData {
  event_type?: string
  eventType?: string
  type?: string
  pipeline_id?: string
  execution_id?: string
  workflow_id?: string
  status?: string
  state?: string
  stage?: string
  current_stage?: string
  message?: string
  summary?: string
  error?: string
  timestamp?: string | number
  attempt?: number
  max_attempts?: number
  progress?: {
    percent?: number
    stage?: string
    current_step?: number
    total_steps?: number
  }
  blocking_reason?: {
    type?: string
    description?: string
    estimated_seconds?: number
    details?: BlockingReasonDetails
    available_tables?: Array<Record<string, unknown>>
  }
  blockingReason?: {
    type?: string
    description?: string
    estimated_seconds?: number
    details?: BlockingReasonDetails
    available_tables?: Array<Record<string, unknown>>
  }
  [key: string]: unknown
}

