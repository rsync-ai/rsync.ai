/**
 * Ask rsync.ai as it was on screen, kept for this tab so a reload brings it
 * back: the panel open or closed, Expand, every chat and the one on screen.
 * sessionStorage, so a new tab starts closed and closing the tab forgets it.
 * The copy names the user it belongs to and is dropped for anyone else (a
 * session that runs out reaches /login without /logout); signing out removes
 * it (lib/auth.ts, the /logout page).
 *
 * A question on its way when the page reloaded cannot come back: the server
 * has no way to fetch an answer later. It shows as failed, with Try again, and
 * the server is told to stop it. The browser may fail the request as the page
 * unloads, and that is saved too ("Failed to fetch" in Chrome): a dropped
 * connection saved after this page's load began is the reload's doing. Only a
 * reload sends the stop. A copy of the tab (Duplicate, reopen a closed tab)
 * comes with this storage while the question still runs in the first tab.
 */

import { useEffect } from "react"

import { useCurrentUser } from "@/contexts/CurrentUserContext"
import { cancelAssistant } from "@/lib/api/assistant"
import {
  useAssistantStore,
  type AssistantChat,
  type AssistantState,
  type ChatItem,
} from "@/lib/store/useAssistantStore"
import { ASSISTANT_SESSION_KEY } from "./session-storage"

/** What a reload brings back. The dock width is kept per browser already (dock-width.ts). */
const KEPT = ["open", "expanded", "page", "pageCleared", "suggestion", "chats", "activeChatId"] as const
type Kept = Pick<AssistantState, (typeof KEPT)[number]>

interface Saved extends Kept {
  version: 1
  userId: string
}

export const RELOADED_TEXT = "The page reloaded before the answer came."

function kept(state: Partial<Kept>): Kept {
  return {
    open: state.open === true,
    expanded: state.expanded === true,
    page: state.page ?? null,
    pageCleared: state.pageCleared === true,
    suggestion: typeof state.suggestion === "string" ? state.suggestion : null,
    chats: Array.isArray(state.chats) ? state.chats : [],
    activeChatId: typeof state.activeChatId === "string" ? state.activeChatId : null,
  }
}

function forget(): void {
  try {
    window.sessionStorage.removeItem(ASSISTANT_SESSION_KEY)
  } catch {}
}

function readSaved(userId: string): Kept | null {
  let raw: string | null
  try {
    raw = window.sessionStorage.getItem(ASSISTANT_SESSION_KEY)
  } catch {
    return null
  }
  if (!raw) return null
  let saved: Partial<Saved> | null = null
  try {
    saved = JSON.parse(raw)
  } catch {}
  if (!saved || saved.version !== 1 || saved.userId !== userId || !Array.isArray(saved.chats)) {
    // Another user's, or a shape this build does not know: never shown.
    forget()
    return null
  }
  return kept(saved)
}

interface PageLoad {
  /** When this page's load began, on the wall clock that dated the chat's items. */
  startedAt: number
  /** A reload, not a copy of the tab or a way back to it. */
  reloaded: boolean
}

/**
 * performance.timeOrigin can run behind Date.now() by as long as the machine
 * slept, so the load's start is taken from both clocks read now.
 */
function pageLoad(): PageLoad {
  let type: string | undefined
  try {
    type = (performance.getEntriesByType("navigation")[0] as PerformanceNavigationTiming | undefined)?.type
  } catch {
    // No navigation timing: not known to be a reload, so nothing is stopped.
  }
  return { startedAt: Math.floor(Date.now() - performance.now()), reloaded: type === "reload" }
}

function stop(requestId: string, load: PageLoad): void {
  if (load.reloaded) void cancelAssistant(requestId).catch(() => {})
}

/**
 * A question the reload cut off shows as failed, and a reload tells the server
 * to stop it. Any other load (a copy of the tab) may still have it running in
 * the first tab: it says where the answer is, with no Try again to ask it twice.
 */
function settle(chat: AssistantChat, load: PageLoad): AssistantChat {
  if (chat.pendingSince !== undefined) {
    if (chat.requestId) stop(chat.requestId, load)
    const asked = [...chat.items].reverse().find((item) => item.kind === "question")
    const item: ChatItem =
      asked?.kind !== "question"
        ? { kind: "stopped" }
        : load.reloaded
          ? { kind: "error", text: RELOADED_TEXT, question: asked.text }
          : { kind: "elsewhere" }
    return { ...chat, items: [...chat.items, item], pendingSince: undefined, requestId: undefined }
  }
  const last = chat.items[chat.items.length - 1]
  if (last?.kind !== "error" || !last.dropped || last.dropped.at < load.startedAt) return chat
  stop(last.dropped.requestId, load)
  const item: ChatItem = { kind: "error", text: RELOADED_TEXT, question: last.question }
  return { ...chat, items: [...chat.items.slice(0, -1), item] }
}

/** Puts back what this tab showed before the reload, if it was this user's. */
export function restoreAssistantSession(userId: string): void {
  const saved = readSaved(userId)
  if (!saved) return
  const now = useAssistantStore.getState()
  // Opened or asked before the page knew who was signed in: what was done since the reload wins.
  if (now.open || now.chats.length > 0) return
  const load = pageLoad()
  useAssistantStore.setState({ ...saved, chats: saved.chats.map((chat) => settle(chat, load)) })
}

/** Writes the panel to this tab's storage now and on every change; returns the unsubscribe. */
export function keepAssistantSession(userId: string): () => void {
  const write = (state: Kept) => {
    const saved: Saved = { version: 1, userId, ...kept(state) }
    try {
      window.sessionStorage.setItem(ASSISTANT_SESSION_KEY, JSON.stringify(saved))
    } catch {
      // Over the tab's storage limit (or blocked): keep the chat on screen, else nothing.
      try {
        const onScreen = saved.chats.filter((c) => c.id === saved.activeChatId)
        window.sessionStorage.setItem(ASSISTANT_SESSION_KEY, JSON.stringify({ ...saved, chats: onScreen }))
      } catch {
        forget()
      }
    }
  }
  write(useAssistantStore.getState())
  return useAssistantStore.subscribe((state, prev) => {
    if (KEPT.some((k) => state[k] !== prev[k])) write(state)
  })
}

/**
 * Brings this tab's Ask rsync.ai back after a reload once the signed-in user is
 * known, and keeps it from then on. After hydration, so the server's HTML (panel
 * closed) and the first client render agree.
 */
export function useAssistantSession(): void {
  const userId = useCurrentUser().user?.id
  useEffect(() => {
    if (!userId) return
    restoreAssistantSession(userId)
    return keepAssistantSession(userId)
  }, [userId])
}
