/**
 * Where this tab keeps Ask rsync.ai between reloads (lib/assistant/session.ts).
 * No imports, so sign-out (lib/auth.ts, which the API client imports) can clear
 * it without importing the API client back.
 */

export const ASSISTANT_SESSION_KEY = "rsync-assistant-session"

/** Sign-out: this tab keeps no chat. */
export function clearAssistantSession(): void {
  if (typeof window === "undefined") return
  try {
    window.sessionStorage.removeItem(ASSISTANT_SESSION_KEY)
  } catch {
    // Storage blocked: nothing was kept.
  }
}
