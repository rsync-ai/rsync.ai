"use client"

import { useRef, useState, useSyncExternalStore, type FormEvent, type KeyboardEvent } from "react"
import Link from "next/link"
import { useRouter } from "next/navigation"
import { ArrowRight, Sparkles } from "lucide-react"

import { Button } from "@/components/ui/button"
import { TabHint } from "@/components/ui/tab-hint"
import { featureFlagsManager } from "@/config/features"
import { useSuggestionTab } from "@/hooks/useSuggestionTab"
import { NO_MODEL, sendQuestion } from "@/lib/assistant/ask"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { getActiveWorkspaceId } from "@/lib/workspace/active-workspace"

export type HomeAskMode = "loading" | "assistant" | "builder"

/**
 * Which box Home shows. Ask rsync.ai needs both its rollout flag and a model:
 * without a model it can only answer "no model", while the pipeline chat still
 * reads "sync mysql to s3" with its own parser. Until /api/v1/features has
 * answered, neither is shown, so the box never flips under the user's cursor.
 */
export function readHomeAskMode(): HomeAskMode {
  if (!featureFlagsManager.isResolved()) return "loading"
  const { assistant, llmConfigured } = featureFlagsManager.getFlags()
  return assistant && llmConfigured ? "assistant" : "builder"
}

/**
 * The line under the pipeline box when no model is set up. Promising more of the
 * box is only true where Ask rsync.ai is switched on.
 */
export function readNoModelNote(): string | null {
  if (!featureFlagsManager.isResolved()) return null
  const { assistant, llmConfigured } = featureFlagsManager.getFlags()
  if (llmConfigured) return null
  return assistant ? `${NO_MODEL} With one, this box also builds models and workflows.` : NO_MODEL
}

const subscribeFlags = (onChange: () => void) => featureFlagsManager.subscribe(() => onChange())

/** What the assistant can be asked to make, each with a request it can carry out. */
export const ASSISTANT_EXAMPLES = [
  { label: "Pipeline", text: "Sync my Postgres orders table to BigQuery every hour" },
  { label: "Model", text: "Every morning at 7, rebuild a table of yesterday's orders by country" },
  { label: "Workflow", text: "Every Monday at 9, email me how many customers signed up last week" },
] as const

export const BUILDER_SUGGESTION = "Sync Postgres to BigQuery"

/**
 * Home's one box. With Ask rsync.ai on and a model set up, it asks the
 * assistant, whose answer opens in the docked panel as a new chat: it can build
 * a pipeline, a scheduled query or a workflow, or answer a question. Otherwise
 * it is the pipeline chat's first message; queries and schedules are then made
 * by hand in Explorer.
 */
export function HomeAskBox() {
  const mode = useSyncExternalStore(subscribeFlags, readHomeAskMode, () => "loading" as const)
  const noModelNote = useSyncExternalStore(subscribeFlags, readNoModelNote, () => null)
  const router = useRouter()
  const [value, setValue] = useState("")
  const boxRef = useRef<HTMLTextAreaElement>(null)

  const suggestion = mode === "assistant" ? ASSISTANT_EXAMPLES[0].text : BUILDER_SUGGESTION
  const tab = useSuggestionTab({ value, suggestion, onAccept: setValue })

  if (mode === "loading") {
    return <div aria-busy="true" className="h-[148px] rounded-xl border border-zinc-200 dark:border-zinc-800" />
  }

  const submit = () => {
    const text = value.trim()
    if (!text) return
    if (mode === "assistant") {
      useAssistantStore.getState().openWith(null)
      void sendQuestion({ chatId: null, question: text, workspaceId: getActiveWorkspaceId(), page: null })
      setValue("")
      return
    }
    router.push(`/chat?prompt=${encodeURIComponent(text)}&autosend=1`)
  }

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    submit()
  }

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (tab.onKeyDown(e)) return
    // An input method's Enter picks a word; it does not send.
    if (e.nativeEvent.isComposing) return
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  const assistant = mode === "assistant"
  const label = assistant ? "Ask rsync.ai" : "Describe the pipeline you want"

  return (
    <section aria-labelledby="home-ask-heading" className="rounded-xl border border-violet-200 bg-white p-4 shadow-sm dark:border-violet-900 dark:bg-zinc-950">
      <h2 id="home-ask-heading" className="flex items-center gap-2 text-base font-semibold text-zinc-900 dark:text-white">
        <Sparkles className="h-4 w-4 text-violet-600 dark:text-violet-400" aria-hidden />
        {assistant ? "Ask rsync.ai" : "Create a pipeline"}
      </h2>
      <p className="mt-0.5 text-sm text-zinc-500 dark:text-zinc-400">
        {assistant
          ? "Build a pipeline, a model or a workflow, or ask about your data. The answer opens in the side panel."
          : "Name a source and a destination; the pipeline chat takes it from there."}
      </p>

      <form onSubmit={onSubmit} className="mt-3">
        <div className="relative rounded-lg border border-zinc-300 bg-white focus-within:border-violet-500 focus-within:ring-2 focus-within:ring-violet-500/30 dark:border-zinc-700 dark:bg-zinc-900">
          <textarea
            ref={boxRef}
            rows={2}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={onKeyDown}
            aria-label={label}
            placeholder={suggestion}
            className="block w-full resize-none rounded-lg bg-transparent px-3 py-2.5 pr-14 text-sm text-zinc-900 placeholder:text-zinc-500 focus:outline-none dark:text-white dark:placeholder:text-zinc-400"
          />
          <Button
            type="submit"
            size="sm"
            disabled={!value.trim()}
            aria-label={assistant ? "Ask" : "Start pipeline chat"}
            className="absolute bottom-2 right-2 bg-violet-600 text-white hover:bg-violet-700"
          >
            <ArrowRight className="h-4 w-4" />
          </Button>
        </div>
        <div className="mt-2 flex min-h-5 flex-wrap items-center gap-x-3 gap-y-2">
          {tab.showHint && <TabHint />}
          {assistant && (
            <div className="flex flex-wrap items-center gap-1.5">
              {ASSISTANT_EXAMPLES.map((ex) => (
                <button
                  key={ex.label}
                  type="button"
                  onClick={() => {
                    // Fills rather than sends, so the request can be edited first.
                    setValue(ex.text)
                    boxRef.current?.focus()
                  }}
                  className="rounded-full border border-zinc-200 px-2.5 py-0.5 text-xs text-zinc-600 hover:border-violet-300 hover:text-violet-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:border-zinc-700 dark:text-zinc-300 dark:hover:border-violet-600 dark:hover:text-violet-300"
                >
                  {ex.label}
                </button>
              ))}
            </div>
          )}
        </div>
      </form>

      {!assistant && (
        <div className="mt-3 space-y-1 border-t border-zinc-100 pt-3 text-sm text-zinc-600 dark:border-zinc-800 dark:text-zinc-300">
          <p>
            Query your data in{" "}
            <Link href="/explorer" className="font-medium text-violet-700 hover:underline dark:text-violet-300">
              Explorer
            </Link>
            , and save a query as a model from there; it then shows in{" "}
            <Link href="/models" className="font-medium text-violet-700 hover:underline dark:text-violet-300">
              Models
            </Link>
            .
          </p>
          {noModelNote && <p className="text-xs text-zinc-500 dark:text-zinc-400">{noModelNote}</p>}
        </div>
      )}
    </section>
  )
}
