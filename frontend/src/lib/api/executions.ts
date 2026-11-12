/**
 * Executions API functions
 */

import type { Execution } from "./types"
import { API_ENDPOINTS } from "@/lib/config/api"
import { authFetch } from "@/lib/api/auth-fetch"
import { extractErrorMessage } from "@/lib/utils/error-handling"

export interface ExecutionListParams {
  pipelineId?: string
  status?: string
  page?: number
  limit?: number
}

export interface ExecutionWithPipeline extends Execution {
  pipelineName?: string
}

export interface ExecutionStats {
  running: number
  success: number
  failed: number
}

export interface ExecutionListResponse {
  executions: ExecutionWithPipeline[]
  total: number
  stats: ExecutionStats
}

function getLocalAuthHeaders(): HeadersInit {
  // Cookie-based auth: no custom identity headers needed.
  return { "Content-Type": "application/json" }
}

/**
 * List executions and include stats/total (preferred for UI pages).
 * Backend returns `{ executions, total, stats }`.
 */
export async function listExecutionsResponse(
  params?: ExecutionListParams
): Promise<ExecutionListResponse> {
  const searchParams = new URLSearchParams()

  if (params?.pipelineId) {
    searchParams.set("pipeline_id", params.pipelineId)
  }
  if (params?.status) {
    searchParams.set("status", params.status)
  }
  if (params?.page) {
    searchParams.set("page", params.page.toString())
  }
  if (params?.limit) {
    searchParams.set("limit", params.limit.toString())
  }

  const queryString = searchParams.toString()
  const url = `${API_ENDPOINTS.EXECUTIONS.LIST}${queryString ? `?${queryString}` : ""}`

  const response = await authFetch(url, {
    method: "GET",
    headers: getLocalAuthHeaders(),
  })

  if (!response.ok) {
    const error = await response
      .json()
      .catch(() => ({ error: "Failed to fetch executions" }))
    throw new Error(extractErrorMessage(error) || "Failed to fetch executions")
  }

  const data = await response.json().catch(() => null)

  // Backwards compatibility: some backends/callers may return a plain array.
  if (Array.isArray(data)) {
    const executions = data as ExecutionWithPipeline[]
    return {
      executions,
      total: executions.length,
      stats: {
        running: executions.filter((e: any) => e.status === "running").length,
        success: executions.filter((e: any) => e.status === "success" || e.status === "completed").length,
        failed: executions.filter((e: any) => e.status === "failed").length,
      },
    }
  }

  const executions = (data?.executions || []) as ExecutionWithPipeline[]
  const stats = (data?.stats || { running: 0, success: 0, failed: 0 }) as ExecutionStats
  const total = typeof data?.total === "number" ? data.total : executions.length

  return { executions, stats, total }
}

/**
 * Cancel a running execution
 */
export async function cancelExecution(id: string): Promise<void> {
  const response = await authFetch(`${API_ENDPOINTS.EXECUTIONS.GET(id)}/cancel`, {
    method: "POST",
    headers: getLocalAuthHeaders(),
  })

  if (!response.ok) {
    const error = await response.json().catch(() => ({ error: "Failed to cancel execution" }))
    throw new Error(extractErrorMessage(error) || "Failed to cancel execution")
  }
}
