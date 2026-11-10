// Authentication utilities

import { clearSignedInState } from "@/lib/auth/signed-in-state"
import { API_GATEWAY_URL } from "@/lib/config/api"

export function getUserEmail(): string | null {
  return null
}

export function logout() {
  if (typeof window === "undefined") return

  // Best-effort: ask API gateway to clear the HttpOnly cookie + session.
  // (Even if this fails, we still clear local UI state.)
  // Self-correcting base URL (see @/lib/config/api): on a real origin a
  // mis-baked localhost value is ignored in favor of the current origin.
  fetch(`${API_GATEWAY_URL}/api/v1/auth/logout`, {
    method: "POST",
    credentials: "include",
  }).catch(() => {})

  // Logging out means keep nothing, in any workspace.
  clearSignedInState()

  window.location.href = "/login"
}

export function getAuthHeaders(): Record<string, string> {
  // Cookie-based auth: the browser will attach cookies automatically when
  // requests use `credentials: "include"` (see `authFetch`/`tracedFetch`).
  return {}
}
