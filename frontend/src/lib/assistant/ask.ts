/**
 * Sending a question to the assistant and putting what comes back into the
 * chat it was asked in. The answer is matched to its chat by id, so switching
 * to another chat (or starting one) while it is on its way leaves it in the
 * chat it belongs to, where History finds it.
 */

import { AssistantError, askAssistant, cancelAssistant, workflowIds } from "@/lib/api/assistant"
import { notifyWorkflowChanged } from "@/lib/events/workflowChanged"
import {
  useAssistantStore,
  type AssistantChat,
  type AssistantPageRef,
  type ChatItem,
} from "@/lib/store/useAssistantStore"

/** How long the server keeps a conversation after its last answer (api-gateway store.go conversationTTL). */
export const SERVER_MEMORY_MS = 30 * 60 * 1000

export const NO_MODEL = "No AI model is set up yet. An admin can add a key in Admin → Settings → AI models."

export const DROPPED_TEXT = "The connection to the server dropped before the answer came."

/** What the user reads when a question fails. */
export function assistantErrorText(err: unknown): string {
  if (err instanceof AssistantError) {
    switch (err.code) {
      case "model_not_configured":
        return NO_MODEL
      case "network":
        return DROPPED_TEXT
      case "timeout":
        return "The assistant took too long to answer. Try a narrower question, for example about one pipeline."
      case "provider_error":
        return `The model provider refused the question: ${err.message}`
      case "quota_exceeded":
        return err.message
    }
    if (err.status === 404) return "The assistant is not turned on for this server."
    if (err.status === 401) return "Your session has ended. Sign in again to use the assistant."
    return err.message
  }
  return err instanceof Error && err.message ? err.message : "The assistant could not answer."
}

/** History's name for a chat: its first question, on one line. */
export function chatTitle(question: string): string {
  const line = question.replace(/\s+/g, " ").trim()
  return line.length > 60 ? `${line.slice(0, 59)}…` : line
}

// The request on its way, per chat, so Stop can end it.
const inFlight = new Map<string, { controller: AbortController; requestId: string }>()

/**
 * A question's request_id: 32 hex characters. getRandomValues, not
 * randomUUID, which a self-hosted server on plain http does not have.
 */
function newRequestId(): string {
  const bytes = new Uint8Array(16)
  if (typeof crypto !== "undefined" && typeof crypto.getRandomValues === "function") {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256)
  }
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("")
}

let seq = 0
function newChatId(): string {
  seq += 1
  return `chat-${Date.now().toString(36)}-${seq}`
}

export interface SendInput {
  /** The chat on screen; null starts a new one. */
  chatId: string | null
  question: string
  workspaceId: string | null
  page: AssistantPageRef | null
  model?: string
  /** Try again: the question is already on screen, after it the error it got. */
  retry?: boolean
}

/** Asks one question. A chat takes one question at a time. */
export async function sendQuestion(input: SendInput): Promise<void> {
  const question = input.question.trim()
  if (!question) return
  const store = useAssistantStore.getState()
  const now = Date.now()

  let chat: AssistantChat | undefined = input.chatId
    ? store.chats.find((c) => c.id === input.chatId)
    : undefined
  if (chat?.pendingSince) return

  // Past 30 minutes the server has dropped the conversation: say so on screen,
  // rather than let a follow-up read as if the assistant still had the context.
  let conversationId = chat?.conversationId
  const lead: ChatItem[] = []
  if (chat && conversationId && now - chat.updatedAt > SERVER_MEMORY_MS) {
    conversationId = undefined
    lead.push({ kind: "forgotten" })
  }

  if (!chat) {
    chat = { id: newChatId(), workspaceId: input.workspaceId, title: chatTitle(question), items: [], updatedAt: now }
    store.startChat(chat)
  }
  const id = chat.id
  const requestId = newRequestId()
  store.updateChat(id, (c) => ({
    ...c,
    conversationId,
    items: input.retry
      ? [...c.items.filter((item, i) => !(i === c.items.length - 1 && item.kind === "error")), ...lead]
      : [...c.items, ...lead, { kind: "question", text: question }],
    pendingSince: now,
    requestId,
  }))

  const controller = new AbortController()
  inFlight.set(id, { controller, requestId })
  try {
    const answer = await askAssistant({
      question,
      conversationId,
      page: input.page ? { kind: input.page.kind, id: input.page.id } : null,
      model: input.model,
      requestId,
      signal: controller.signal,
    })
    useAssistantStore.getState().updateChat(id, (c) => ({
      ...c,
      conversationId: answer.conversation_id,
      items: [...c.items, { kind: "answer", answer }],
      pendingSince: undefined,
      requestId: undefined,
      updatedAt: Date.now(),
    }))
    // A draft the answer stored is new to the Workflows list open beside the panel.
    for (const workflowId of new Set(workflowIds(answer))) notifyWorkflowChanged(workflowId)
  } catch (err) {
    // A reload drops the connection too, and the browser may say so before the
    // page goes: this is saved, so the next page can tell (lib/assistant/session.ts).
    const dropped = err instanceof AssistantError && err.code === "network" ? { at: Date.now(), requestId } : undefined
    const item: ChatItem = controller.signal.aborted
      ? { kind: "stopped" }
      : { kind: "error", text: assistantErrorText(err), question, ...(dropped ? { dropped } : {}) }
    useAssistantStore
      .getState()
      .updateChat(id, (c) => ({ ...c, items: [...c.items, item], pendingSince: undefined, requestId: undefined }))
  } finally {
    if (inFlight.get(id)?.controller === controller) inFlight.delete(id)
  }
}

/**
 * Stop: tells the server to end the chat's question (api-gateway
 * assistant/stop.go), then lets go of the request. The abort alone does not
 * reach the server through a proxy, so the cancel is what stops the model
 * calls and the tools; if it fails, the question ends at its time limit.
 */
export function stopQuestion(chatId: string): void {
  const running = inFlight.get(chatId)
  if (!running) return
  void cancelAssistant(running.requestId).catch(() => {})
  running.controller.abort()
}
