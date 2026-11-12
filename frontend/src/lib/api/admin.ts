import { authFetch, authPost, redirectToLogout } from "./auth-fetch"
import { readResponseErrorMessage } from "@/lib/utils/error-handling"

// ============================================================================
// Types
// ============================================================================

export interface AdminUser {
  id: string
  email: string
  name: string
  role: string
  status: string
  created_at: string
  last_login?: string | null
}

export interface AdminInvitation {
  id: string
  token: string
  email_hint: string
  role: string
  status: "pending" | "used" | "expired"
  created_by_email: string
  expires_at: string
  used_by_email?: string | null
  used_at?: string | null
  created_at: string
}

export interface AdminAuditLog {
  id: string
  user_id?: string | null
  user_email?: string | null
  action: string
  resource_type: string
  resource_id?: string | null
  details?: Record<string, unknown> | null
  ip_address?: string | null
  created_at: string
}

export interface ServiceHealth {
  service: string
  // "unknown" means nothing was asked (admin_health.go, the explorer-freshness-sweep
  // check) — a different fact from "down", and it arrives on the wire as its own value.
  status: "up" | "down" | "degraded" | "unknown"
  latency_ms: number
  error?: string
  /**
   * Whatever a check knows beyond reachability (admin_health.go serviceHealth.Detail).
   * Absent for the socket checks; the freshness sweep reports its interval, tick counts
   * and last result here.
   */
  detail?: Record<string, unknown>
}

export interface PaginatedResponse<T> {
  data: T[]
  total: number
  limit: number
  offset: number
}

export interface InviteValidation {
  valid: boolean
  reason?: string
  email_hint?: string
  role?: string
  expires_at?: string
}

// ============================================================================
// Reads
// ============================================================================

/** Why an admin page's read came back without a body. */
export interface AdminReadFailure {
  ok: false
  /** 0 when the request never got an answer. */
  status: number
  /** Seconds, from a 429's Retry-After; null when it named none. */
  retryAfter: number | null
  /** The server's reason, or the HTTP status when it gave none. */
  error: string
}

export type AdminRead<T> = { ok: true; data: T } | AdminReadFailure

/**
 * One GET for an admin page, which shows a different state for each way it can
 * fail: 401 signs out, 403 is "Access denied", 429 is "retry in ~Ns", and any
 * other status is the server's reason with a Retry. Every admin page carried its
 * own copy of that ladder, and four of them (users, invitations, audit, health)
 * stopped at 429: a 500 went on to `res.json()`, which succeeds on the API's
 * `{"error": …}` body, so the page drew an empty table, read as "there are no
 * users" when the read had failed. Settings had been fixed for the same reason.
 */
export async function adminRead<T>(endpoint: string, init: RequestInit = {}): Promise<AdminRead<T>> {
  let res: Response
  try {
    res = await authFetch(endpoint, { method: "GET", ...init })
  } catch {
    return { ok: false, status: 0, retryAfter: null, error: "Network error" }
  }
  if (res.status === 401) redirectToLogout()
  if (!res.ok) {
    const ra = parseInt(res.headers.get("Retry-After") || "", 10)
    return {
      ok: false,
      status: res.status,
      retryAfter: res.status === 429 && Number.isFinite(ra) ? ra : null,
      error: await readResponseErrorMessage(res),
    }
  }
  try {
    return { ok: true, data: (await res.json()) as T }
  } catch {
    return { ok: false, status: res.status, retryAfter: null, error: "The response was not JSON" }
  }
}

/** A mutation's JSON body, or its server reason thrown as the Error's message. */
async function jsonOrThrow<T>(res: Response): Promise<T> {
  if (!res.ok) throw new Error(await readResponseErrorMessage(res))
  return res.json()
}

function query(params: Record<string, string | number | undefined>): string {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) if (v) sp.set(k, String(v))
  const qs = sp.toString()
  return qs ? `?${qs}` : ""
}

// ============================================================================
// Users
// ============================================================================

export async function adminListUsers(params: {
  q?: string
  role?: string
  status?: string
  limit?: number
  offset?: number
} = {}): Promise<AdminRead<PaginatedResponse<AdminUser>>> {
  return adminRead(`/api/v1/admin/users${query(params)}`)
}

export async function adminUpdateUserRole(id: string, role: string): Promise<{ message: string; role: string }> {
  const res = await authFetch(`/api/v1/admin/users/${id}/role`, {
    method: "PATCH",
    body: JSON.stringify({ role }),
  })
  return jsonOrThrow(res)
}

/**
 * The plan names shipped by the migrations, in the order an admin reads them
 * (cheapest first). There is NO plans-listing endpoint — `plans` is a DB table
 * the API never exposes — so this list is seeded from
 * api-gateway/migrations/060_user_plans.sql and 071_starter_plan_and_workspace_plans.sql
 * rather than fetched.
 *
 * That makes it drift-prone in one direction only: a plan added to the catalogue
 * later is missing here. The caller therefore unions this list with the plans it
 * can actually SEE in the usage payload, and the server is the real authority —
 * it validates against the `plans` table and answers 400 {"error":"unknown plan"}
 * for anything it does not know (admin_plans.go). So a stale entry here fails
 * loudly on save instead of writing a typo'd tier.
 */
export const KNOWN_PLAN_NAMES = ["trial", "free", "starter", "pro"] as const

/**
 * Sets a workspace's billing plan (the interim manual-upgrade path until Stripe).
 *
 * Side effect worth surfacing in the UI: the server also clears plan_expires_at,
 * so a manual grant never auto-expires.
 */
export async function adminSetWorkspacePlan(
  workspaceId: string,
  plan: string
): Promise<{ success: boolean; workspace_id: string; plan: string }> {
  const res = await authFetch(`/api/v1/admin/workspaces/${workspaceId}/plan`, {
    method: "POST",
    body: JSON.stringify({ plan }),
  })
  return jsonOrThrow(res)
}

export async function adminUpdateUserStatus(id: string, status: string): Promise<{ message: string; status: string }> {
  const res = await authFetch(`/api/v1/admin/users/${id}/status`, {
    method: "PATCH",
    body: JSON.stringify({ status }),
  })
  return jsonOrThrow(res)
}

export async function adminDeleteUser(id: string): Promise<{ message: string }> {
  const res = await authFetch(`/api/v1/admin/users/${id}`, { method: "DELETE" })
  return jsonOrThrow(res)
}

// ============================================================================
// Invitations
// ============================================================================

export async function adminCreateInvitation(body: {
  email_hint?: string
  role?: string
}): Promise<{ id: string; token: string; url: string; expires_at: string }> {
  return authPost("/api/v1/admin/invitations", body)
}

export async function adminListInvitations(params: {
  limit?: number
  offset?: number
} = {}): Promise<AdminRead<PaginatedResponse<AdminInvitation>>> {
  return adminRead(`/api/v1/admin/invitations${query(params)}`)
}

export async function adminRevokeInvitation(id: string): Promise<{ message: string }> {
  const res = await authFetch(`/api/v1/admin/invitations/${id}`, { method: "DELETE" })
  return jsonOrThrow(res)
}

// ============================================================================
// Audit Logs
// ============================================================================

export async function adminListAuditLogs(params: {
  user_id?: string
  action?: string
  resource_type?: string
  from?: string
  to?: string
  q?: string
  limit?: number
  offset?: number
} = {}): Promise<AdminRead<PaginatedResponse<AdminAuditLog>>> {
  return adminRead(`/api/v1/admin/audit-logs${query(params)}`)
}

// ============================================================================
// Settings
// ============================================================================

export async function adminGetSettings(): Promise<AdminRead<{ settings?: Record<string, string> }>> {
  return adminRead("/api/v1/admin/settings")
}

export async function adminUpdateSettings(settings: Record<string, string>): Promise<{ message: string }> {
  const res = await authFetch("/api/v1/admin/settings", {
    method: "PATCH",
    body: JSON.stringify(settings),
  })
  return jsonOrThrow(res)
}

// ============================================================================
// Health
// ============================================================================

export async function adminGetHealth(): Promise<AdminRead<{ services?: ServiceHealth[] }>> {
  return adminRead("/api/v1/admin/health")
}

// ============================================================================
// Public (no admin required)
// ============================================================================

/**
 * Whether a signup link's invitation can still be used. The answer is a 200
 * with `valid: false` and a `reason` for a used, expired or unknown token, so
 * only a request that got no such answer throws.
 */
export async function validateInvite(token: string): Promise<InviteValidation> {
  const res = await authFetch(`/api/v1/auth/invite/${encodeURIComponent(token)}`, { method: "GET", skipAuth: true })
  return jsonOrThrow<InviteValidation>(res)
}
