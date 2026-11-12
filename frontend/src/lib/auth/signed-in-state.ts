/**
 * What this browser keeps for the person signed in, and who that is.
 *
 * /chat keeps its conversation, its session id and the pipelines it started in
 * localStorage, per workspace (scoped-storage.ts). Signing out removes them, but
 * a session that runs out reaches /login without /logout, so the next person to
 * sign in on the browser was shown the last one's /chat (inbox 2026-10-11). A
 * session only starts at a sign-in or a sign-up (POST /auth/login and
 * /auth/register set the cookie), so that is where the state is claimed: it
 * names the person it belongs to and is dropped for anyone else.
 */

import { clearAssistantSession } from "@/lib/assistant/session-storage"
import { clearAllWorkspaceScopes } from "@/lib/workspace/scoped-storage"

/** The user id the state below belongs to. */
const OWNER_KEY = "rsync_signed_in_user"

/** Workspace-scoped base keys /chat writes for the person signed in. */
const CHAT_KEYS = ["rsync_chat_session_id", "rsync_chat_ui_v2_state", "rsync_active_pipelines"]

/** Everything a signed-in person leaves in this browser, in every workspace. */
export function clearSignedInState(): void {
  if (typeof window === "undefined") return
  try {
    // Back-compat cleanup: older builds stored user metadata here.
    window.localStorage.removeItem("user_id")
    window.localStorage.removeItem("user_email")
    window.localStorage.removeItem("user_role")
    window.localStorage.removeItem(OWNER_KEY)
  } catch {
    // Ignore — private mode / quota.
  }
  // Clearing one workspace's slot would strand every other workspace's.
  for (const key of CHAT_KEYS) clearAllWorkspaceScopes(key)
  clearAssistantSession()
}

/**
 * Called once a sign-in or sign-up succeeds, before the app opens: state another
 * person left, or that no sign-in claimed, is dropped. The same person signing
 * back in keeps theirs.
 */
export function claimSignedInState(userId: string): void {
  if (typeof window === "undefined") return
  let owner: string | null = null
  try {
    owner = window.localStorage.getItem(OWNER_KEY)
  } catch {}
  if (!userId || owner !== userId) clearSignedInState()
  if (!userId) return
  try {
    window.localStorage.setItem(OWNER_KEY, userId)
  } catch {}
}
