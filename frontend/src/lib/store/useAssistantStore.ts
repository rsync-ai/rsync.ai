import { create } from "zustand"

import type { AssistantAnswer, AssistantPage } from "@/lib/api/assistant"
import { readStoredDockWidth } from "@/lib/assistant/dock-width"

/** The page a question is about, with a name to show when the opener knows it. */
export type AssistantPageRef = AssistantPage & { name?: string }

/** One line of a chat, in the order it happened. */
export type ChatItem =
  | { kind: "question"; text: string }
  | { kind: "answer"; answer: AssistantAnswer }
  /**
   * A failed question; Try again sends `question` once more. `dropped`: the
   * connection dropped, when and on which request_id, so a reload that caused
   * it can say so and tell the server to stop it (lib/assistant/session.ts).
   */
  | { kind: "error"; text: string; question: string; dropped?: { at: number; requestId: string } }
  /** The user pressed Stop before the answer came. */
  | { kind: "stopped" }
  /**
   * On its way when a load that was not a reload brought the chat back: a copy
   * of the tab may still be answering it, so it is neither stopped nor offered
   * again (lib/assistant/session.ts).
   */
  | { kind: "elsewhere" }
  /** The server forgot the questions above (it keeps a conversation 30 minutes). */
  | { kind: "forgotten" }

/**
 * One chat. It lives in this tab only: a reload brings it back
 * (lib/assistant/session.ts), a sign-out or closing the tab drops it. The
 * server keeps the conversation behind it for 30 minutes (api-gateway
 * internal/assistant/store.go).
 */
export interface AssistantChat {
  /** This tab's id for the chat; never sent. */
  id: string
  /** The workspace the chat was started in: it is shown only there. */
  workspaceId: string | null
  /** The first question, cut short. */
  title: string
  /** The server's conversation id, from the first answer. */
  conversationId?: string
  items: ChatItem[]
  /** When the question on its way was sent; unset while none is. */
  pendingSince?: number
  /** That question's request_id, so a reload can tell the server to stop it. */
  requestId?: string
  /**
   * When the chat started or last got an answer: History's order, and the
   * 30-minute rule (the server's copy expires 30 minutes after its last save).
   */
  updatedAt: number
}

/** Chats kept per tab; the oldest goes first. */
export const MAX_CHATS = 20

export interface AssistantState {
  /** Whether the assistant panel is open. A reload keeps it, a new tab does not. */
  open: boolean
  /** The panel fills the page instead of sitting beside it. */
  expanded: boolean
  /**
   * The docked panel's width as the user last set it, in px; kept in this
   * browser (lib/assistant/dock-width.ts). The window may clamp what shows.
   */
  dockWidth: number
  /** The user is dragging the panel's edge: the page stops animating its padding. */
  dockResizing: boolean
  /**
   * The page set by whoever opened the panel (the pipeline dock passes its
   * pipeline). null means the panel reads the page from the URL; undefined
   * is never stored.
   */
  page: AssistantPageRef | null
  /** The user dismissed the "About" chip for this opening. */
  pageCleared: boolean
  /**
   * The question Tab writes into a new chat, set by whoever opened the panel (a
   * dashboard's Schedule email button); null offers the page's first starter.
   */
  suggestion: string | null
  /** Newest first. */
  chats: AssistantChat[]
  /** The chat on screen; null is a new, empty chat. */
  activeChatId: string | null

  openWith: (page?: AssistantPageRef | null, suggestion?: string) => void
  close: () => void
  toggle: () => void
  toggleExpanded: () => void
  setDockWidth: (width: number) => void
  setDockResizing: (resizing: boolean) => void
  clearPage: () => void
  newChat: () => void
  selectChat: (id: string) => void
  /** Adds a chat and makes it the one on screen. */
  startChat: (chat: AssistantChat) => void
  /** Changes one chat by id; a chat that is gone is left alone. */
  updateChat: (id: string, change: (chat: AssistantChat) => AssistantChat) => void
}

export const useAssistantStore = create<AssistantState>()((set) => ({
  open: false,
  expanded: false,
  dockWidth: readStoredDockWidth(),
  dockResizing: false,
  page: null,
  pageCleared: false,
  suggestion: null,
  chats: [],
  activeChatId: null,

  openWith: (page, suggestion) =>
    set({ open: true, page: page ?? null, pageCleared: false, suggestion: suggestion?.trim() || null }),
  close: () => set({ open: false }),
  toggle: () =>
    set((state) => (state.open ? { open: false } : { open: true, page: null, pageCleared: false, suggestion: null })),
  toggleExpanded: () => set((state) => ({ expanded: !state.expanded })),
  setDockWidth: (width) => set({ dockWidth: width }),
  setDockResizing: (resizing) => set({ dockResizing: resizing }),
  // The opener's suggestion is about its page, so it goes with the page.
  clearPage: () => set({ pageCleared: true, suggestion: null }),
  newChat: () => set({ activeChatId: null }),
  selectChat: (id) => set({ activeChatId: id }),
  startChat: (chat) =>
    set((state) => ({ chats: [chat, ...state.chats].slice(0, MAX_CHATS), activeChatId: chat.id })),
  updateChat: (id, change) =>
    set((state) => ({ chats: state.chats.map((c) => (c.id === id ? change(c) : c)) })),
}))
