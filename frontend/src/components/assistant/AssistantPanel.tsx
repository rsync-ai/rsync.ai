"use client"

import { useEffect, useRef, useState, useSyncExternalStore } from "react"
import { usePathname } from "next/navigation"
import { History, Maximize2, Minimize2, SquarePen, X } from "lucide-react"

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { getWorkspaceLLMData, listAssistantModels, type AssistantModel, type WorkspaceLLMData } from "@/lib/api/assistant"
import { assistantErrorText, sendQuestion, stopQuestion } from "@/lib/assistant/ask"
import { pageFromPath } from "@/lib/assistant/page-context"
import { useAssistantStore, type AssistantPageRef } from "@/lib/store/useAssistantStore"
import { useUIStore } from "@/lib/store/useUIStore"
import { cn } from "@/lib/utils"
import { getActiveWorkspaceId, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"
import { AssistantComposer } from "./AssistantComposer"
import { AssistantResizeHandle } from "./AssistantResizeHandle"
import { AssistantLogo, AssistantThread, AssistantWelcome, starters } from "./AssistantThread"

/**
 * The in-app assistant ("Ask rsync.ai"): a chat docked on the right of every
 * dashboard page, or filling it. Each question goes to api-gateway, which runs
 * the read-only tools and returns one answer. Chats live in this tab
 * (useAssistantStore), so closing the panel keeps them and History lists them.
 */

export const ASSISTANT_PANEL_ID = "ask-rsync-panel"
export const ASSISTANT_LAUNCHER_ID = "ask-rsync-launcher"
/**
 * The docked panel's width. Below `xl` it overlays the page at 440 px; from
 * `xl` it sits beside the page at the width the user dragged it to
 * (lib/assistant/dock-width.ts), and DashboardShell pads the page by the same
 * CSS variable.
 */
const DOCK_WIDTH_CLASSES = "sm:w-[440px] xl:w-(--assistant-dock-width)"

const MODEL_STORAGE_KEY = "rsync-assistant-model"

function readStoredModel(): string {
  try {
    return localStorage.getItem(MODEL_STORAGE_KEY) ?? ""
  } catch {
    return ""
  }
}

function storeModel(entry: string) {
  try {
    localStorage.setItem(MODEL_STORAGE_KEY, entry)
  } catch {}
}

const iconButton =
  "inline-flex h-8 w-8 items-center justify-center rounded-md text-zinc-500 transition-colors hover:bg-zinc-100 hover:text-zinc-900 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 disabled:pointer-events-none disabled:opacity-40 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100"

export function AssistantPanel() {
  const open = useAssistantStore((s) => s.open)
  const expanded = useAssistantStore((s) => s.expanded)
  const openedPage = useAssistantStore((s) => s.page)
  const pageCleared = useAssistantStore((s) => s.pageCleared)
  const openedSuggestion = useAssistantStore((s) => s.suggestion)
  const chats = useAssistantStore((s) => s.chats)
  const activeChatId = useAssistantStore((s) => s.activeChatId)
  const close = useAssistantStore((s) => s.close)
  const clearPage = useAssistantStore((s) => s.clearPage)
  const newChat = useAssistantStore((s) => s.newChat)
  const selectChat = useAssistantStore((s) => s.selectChat)
  const toggleExpanded = useAssistantStore((s) => s.toggleExpanded)
  const sidebarCollapsed = useUIStore((s) => s.sidebarCollapsed)
  const pathname = usePathname()
  // A chat belongs to the workspace it was started in; another workspace's chats stay hidden.
  const workspaceId = useSyncExternalStore(onActiveWorkspaceChange, getActiveWorkspaceId, () => null)

  const [models, setModels] = useState<AssistantModel[] | null>(null)
  const [modelsError, setModelsError] = useState<string | null>(null)
  const [model, setModel] = useState("")
  // Whether sample rows are on here, and for which models: shown so the asker
  // knows when a model may read values. A failed read shows nothing.
  const [llmData, setLLMData] = useState<WorkspaceLLMData | null>(null)
  const inputRef = useRef<HTMLTextAreaElement>(null)
  // Picking a chat from History hands focus to the question box, not back to the History button.
  const focusInputAfterMenu = useRef(false)

  const page: AssistantPageRef | null = pageCleared ? null : (openedPage ?? pageFromPath(pathname))
  const workspaceChats = chats.filter((c) => c.workspaceId === workspaceId)
  const chat = workspaceChats.find((c) => c.id === activeChatId) ?? null

  // The picker lists only models with a key; it is read on every opening so a
  // key an admin just saved shows up without a reload.
  useEffect(() => {
    if (!open) return
    let cancelled = false
    listAssistantModels()
      .then((res) => {
        if (cancelled) return
        setModelsError(null)
        setModels(res.entries)
        const stored = readStoredModel()
        setModel(res.entries.some((m) => m.entry === stored) ? stored : res.default)
      })
      .catch((err) => {
        if (!cancelled) setModelsError(assistantErrorText(err))
      })
    return () => {
      cancelled = true
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    let cancelled = false
    getWorkspaceLLMData()
      .then((res) => !cancelled && setLLMData(res))
      .catch(() => !cancelled && setLLMData(null))
    return () => {
      cancelled = true
    }
  }, [open, workspaceId])

  // Focus the question box on open; on close, give focus back to where it was
  // (else the launcher), but only if closing left it nowhere.
  useEffect(() => {
    if (!open) return
    const before = document.activeElement instanceof HTMLElement ? document.activeElement : null
    inputRef.current?.focus()
    return () => {
      if (document.activeElement && document.activeElement !== document.body) return
      const target = before?.isConnected ? before : document.getElementById(ASSISTANT_LAUNCHER_ID)
      target?.focus()
    }
  }, [open])

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      // An open menu or dialog that took the Escape closes itself, not the panel.
      if (e.key === "Escape" && !e.defaultPrevented) close()
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [open, close])

  if (!open) return null

  const noModels = models !== null && models.length === 0
  const pending = chat?.pendingSince !== undefined
  const sampling =
    llmData?.mode === "sample_rows" && llmData.approved_models.some((entry) => entry === model)
      ? (models?.find((m) => m.entry === model)?.label ?? model)
      : null

  const ask = (question: string, retry = false) => {
    void sendQuestion({
      chatId: chat?.id ?? null,
      question,
      workspaceId,
      page,
      model: model || undefined,
      retry,
    })
  }

  const startNewChat = () => {
    newChat()
    inputRef.current?.focus()
  }

  return (
    <aside
      id={ASSISTANT_PANEL_ID}
      role="complementary"
      aria-label="Ask rsync.ai"
      data-expanded={expanded}
      className={cn(
        "fixed bottom-0 right-0 top-16 z-40 flex flex-col border-zinc-200 bg-white dark:border-zinc-800 dark:bg-zinc-900",
        expanded
          ? cn("left-0 md:border-l", sidebarCollapsed ? "md:left-16" : "md:left-64")
          : cn("left-0 sm:left-auto sm:border-l sm:shadow-2xl xl:shadow-none", DOCK_WIDTH_CLASSES),
      )}
    >
      {!expanded && <AssistantResizeHandle controls={ASSISTANT_PANEL_ID} />}
      <header className="flex h-12 shrink-0 items-center gap-2 border-b border-zinc-200 pl-3 pr-2 dark:border-zinc-800">
        <AssistantLogo />
        <div className="min-w-0 flex-1 leading-tight">
          <div className="text-sm font-semibold text-zinc-900 dark:text-white">Ask rsync.ai</div>
          <div className="truncate text-[11px] text-zinc-500 dark:text-zinc-400">{chat?.title ?? "New chat"}</div>
        </div>
        <button
          type="button"
          onClick={startNewChat}
          disabled={!chat}
          className={iconButton}
          aria-label="New chat"
          title="New chat"
        >
          <SquarePen className="h-4 w-4" />
        </button>
        {/* Not modal: a modal menu locks page scroll, and the scrollbar it takes away shifts the page and panel sideways. */}
        <DropdownMenu modal={false}>
          <DropdownMenuTrigger className={iconButton} aria-label="Chat history" title="Chat history">
            <History className="h-4 w-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent
            align="end"
            className="w-72"
            onCloseAutoFocus={(e) => {
              if (!focusInputAfterMenu.current) return
              focusInputAfterMenu.current = false
              e.preventDefault()
              inputRef.current?.focus()
            }}
          >
            <DropdownMenuLabel className="text-xs font-normal text-zinc-500 dark:text-zinc-400">
              Chats in this tab, kept until you close it or sign out.
            </DropdownMenuLabel>
            <DropdownMenuSeparator />
            {workspaceChats.length === 0 ? (
              <p className="px-2 py-3 text-center text-xs text-zinc-500 dark:text-zinc-400">No chats yet.</p>
            ) : (
              <div className="max-h-80 overflow-y-auto">
                {workspaceChats.map((c) => (
                  <DropdownMenuItem
                    key={c.id}
                    aria-current={c.id === chat?.id ? "true" : undefined}
                    onSelect={() => {
                      focusInputAfterMenu.current = true
                      selectChat(c.id)
                    }}
                    className={cn("flex-col items-start gap-0.5", c.id === chat?.id && "bg-violet-50 dark:bg-violet-950/40")}
                  >
                    <span className="w-full truncate">{c.title}</span>
                    <span className="text-[11px] text-zinc-500 dark:text-zinc-400">
                      {c.pendingSince !== undefined
                        ? "Answering…"
                        : new Date(c.updatedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                    </span>
                  </DropdownMenuItem>
                ))}
              </div>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
        <button
          type="button"
          onClick={toggleExpanded}
          className={cn(iconButton, "hidden md:inline-flex")}
          aria-label={expanded ? "Collapse assistant" : "Expand assistant"}
          title={expanded ? "Dock beside the page" : "Fill the page"}
        >
          {expanded ? <Minimize2 className="h-4 w-4" /> : <Maximize2 className="h-4 w-4" />}
        </button>
        <button type="button" onClick={close} className={iconButton} aria-label="Close assistant" title="Close (Esc)">
          <X className="h-4 w-4" />
        </button>
      </header>

      {modelsError && (
        <p role="alert" className="border-b border-red-200 bg-red-50 px-4 py-2 text-xs text-red-700 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300">
          {modelsError}
        </p>
      )}

      {sampling && (
        <p className="border-b border-amber-200 bg-amber-50 px-4 py-2 text-[11px] text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200">
          Sample rows are on in this workspace: {sampling} may read up to 5 rows of a table, with personal and secret
          columns withheld.{" "}
          <a href="/workspace/settings?tab=data" className="underline">
            Workspace settings
          </a>
        </p>
      )}

      {chat && chat.items.length > 0 ? (
        <AssistantThread chat={chat} wide={expanded} onRetry={(question) => ask(question, true)} />
      ) : (
        <AssistantWelcome page={page} noModels={noModels} onAsk={(question) => ask(question)} />
      )}

      <div className={cn("w-full shrink-0", expanded && "mx-auto max-w-3xl")}>
        <AssistantComposer
          inputRef={inputRef}
          questions={chat ? chat.items.flatMap((item) => (item.kind === "question" ? [item.text] : [])) : []}
          pending={pending}
          noModels={noModels}
          page={page}
          onClearPage={clearPage}
          models={models}
          model={model}
          onModelChange={(entry) => {
            setModel(entry)
            storeModel(entry)
          }}
          onSend={(question) => ask(question)}
          onStop={() => chat && stopQuestion(chat.id)}
          // A new chat offers what the opener asked for, else the welcome's first
          // question; a chat under way has its own thread to go on.
          suggestion={chat && chat.items.length > 0 ? undefined : (openedSuggestion ?? starters(page)[0])}
        />
      </div>
    </aside>
  )
}
