"use client"

import { useLayoutEffect, useState, type RefObject } from "react"
import { ArrowUp, Square, X } from "lucide-react"

import { useSuggestionTab } from "@/hooks/useSuggestionTab"
import type { AssistantModel } from "@/lib/api/assistant"
import { pageLabel } from "@/lib/assistant/page-context"
import type { AssistantPageRef } from "@/lib/store/useAssistantStore"

/** Tallest the question box grows before it scrolls, in px. */
const MAX_INPUT_HEIGHT = 192

/**
 * Where the question is typed: the box, what it is about, the model, and Send
 * (Stop while an answer is on its way). ↑ and ↓ in an empty box step through
 * this chat's questions; Tab writes in `suggestion`.
 */
export function AssistantComposer({
  inputRef,
  questions,
  pending,
  noModels,
  page,
  onClearPage,
  models,
  model,
  onModelChange,
  onSend,
  onStop,
  suggestion,
}: {
  inputRef: RefObject<HTMLTextAreaElement | null>
  /** This chat's questions, oldest first. */
  questions: string[]
  pending: boolean
  noModels: boolean
  page: AssistantPageRef | null
  onClearPage: () => void
  models: AssistantModel[] | null
  model: string
  onModelChange: (entry: string) => void
  onSend: (question: string) => void
  onStop: () => void
  /** Written in by Tab while the box is empty; also shown as the placeholder. */
  suggestion?: string
}) {
  const [input, setInput] = useState("")
  const tab = useSuggestionTab({
    value: input,
    suggestion: noModels ? undefined : suggestion,
    onAccept: (text) => {
      setInput(text)
      setRecall(null)
    },
  })
  // Which of `questions` the box shows, counted from the oldest; null while typing.
  const [recall, setRecall] = useState<number | null>(null)
  const recalling = recall !== null && recall < questions.length

  useLayoutEffect(() => {
    const box = inputRef.current
    if (!box) return
    box.style.height = "auto"
    box.style.height = `${Math.min(box.scrollHeight, MAX_INPUT_HEIGHT)}px`
  }, [input, inputRef])

  const send = () => {
    if (!input.trim() || pending || noModels) return
    onSend(input)
    setInput("")
    setRecall(null)
  }

  const step = (by: -1 | 1) => {
    const at = recalling ? recall : null
    if (by === -1) {
      if (questions.length === 0) return false
      const next = at === null ? questions.length - 1 : Math.max(0, at - 1)
      setRecall(next)
      setInput(questions[next])
      return true
    }
    if (at === null) return false
    const next = at + 1
    setRecall(next < questions.length ? next : null)
    setInput(next < questions.length ? questions[next] : "")
    return true
  }

  const oneModel = models?.length === 1 ? models[0] : null

  return (
    <form
      className="px-4 pb-3 pt-2"
      onSubmit={(e) => {
        e.preventDefault()
        send()
      }}
    >
      <div className="rounded-2xl border border-zinc-200 bg-white shadow-sm transition-colors focus-within:border-violet-400 focus-within:ring-2 focus-within:ring-violet-500/20 dark:border-zinc-700 dark:bg-zinc-900 dark:focus-within:border-violet-600">
        <textarea
          ref={inputRef}
          value={input}
          onChange={(e) => {
            setInput(e.target.value)
            setRecall(null)
          }}
          onKeyDown={(e) => {
            if (tab.onKeyDown(e)) return
            if (e.nativeEvent.isComposing) return
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault()
              send()
              return
            }
            if ((e.key === "ArrowUp" && (input === "" || recalling)) || (e.key === "ArrowDown" && recalling)) {
              if (step(e.key === "ArrowUp" ? -1 : 1)) e.preventDefault()
            }
          }}
          disabled={noModels}
          maxLength={4000}
          rows={1}
          placeholder={
            noModels ? "No AI model is set up yet" : suggestion?.trim() || "Ask about your pipelines, connections or runs…"
          }
          aria-label="Your question"
          className="block max-h-48 min-h-[2.75rem] w-full resize-none bg-transparent px-3.5 pt-3 text-sm text-zinc-900 placeholder-zinc-400 focus:outline-none disabled:cursor-not-allowed dark:text-zinc-100 dark:placeholder-zinc-500"
        />
        <div className="flex items-center gap-2 px-2 pb-2 pt-1">
          {page && (
            <span className="inline-flex min-w-0 items-center gap-1 rounded-full border border-violet-200 bg-violet-50 py-0.5 pl-2 pr-0.5 text-[11px] text-violet-800 dark:border-violet-800 dark:bg-violet-950/40 dark:text-violet-200">
              <span className="truncate">About: {pageLabel(page)}</span>
              <button
                type="button"
                onClick={onClearPage}
                className="shrink-0 rounded-full p-0.5 hover:bg-violet-100 dark:hover:bg-violet-900"
                aria-label="Ask without this page"
              >
                <X className="h-3 w-3" />
              </button>
            </span>
          )}
          <div className="ml-auto flex shrink-0 items-center gap-1.5">
            {models && models.length > 1 ? (
              <select
                aria-label="Model"
                value={model}
                onChange={(e) => onModelChange(e.target.value)}
                className="h-7 max-w-[9rem] rounded-md border-0 bg-transparent px-1.5 text-xs text-zinc-600 hover:bg-zinc-100 focus:outline-none focus-visible:ring-2 focus-visible:ring-violet-500 dark:text-zinc-300 dark:hover:bg-zinc-800"
              >
                {models.map((m) => (
                  <option key={m.entry} value={m.entry}>
                    {m.label}
                  </option>
                ))}
              </select>
            ) : (
              oneModel && <span className="px-1.5 text-xs text-zinc-500 dark:text-zinc-400">{oneModel.label}</span>
            )}
            {pending ? (
              <button
                type="button"
                onClick={onStop}
                aria-label="Stop"
                title="Stop"
                className="inline-flex h-8 w-8 items-center justify-center rounded-full bg-zinc-900 text-white transition-colors hover:bg-zinc-700 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
              >
                <Square className="h-3 w-3 fill-current" />
              </button>
            ) : (
              <button
                type="submit"
                disabled={!input.trim() || noModels}
                aria-label="Send"
                title="Send"
                className="inline-flex h-8 w-8 items-center justify-center rounded-full bg-violet-600 text-white transition-colors hover:bg-violet-700 disabled:cursor-not-allowed disabled:bg-zinc-200 disabled:text-zinc-400 dark:disabled:bg-zinc-800 dark:disabled:text-zinc-600"
              >
                <ArrowUp className="h-4 w-4" />
              </button>
            )}
          </div>
        </div>
      </div>
      <p className="mt-1.5 px-1 text-center text-[10px] leading-snug text-zinc-500 dark:text-zinc-400">
        {/* A new chat has no last question for ↑, and only a new chat has a suggestion. */}
        Enter to send · Shift+Enter for a new line · {tab.showHint ? "Tab for the suggestion" : "↑ for your last question"}.
        <br />
        The model sees your question and the names and settings it looks up, never the rows in your data. Check an answer before you act on it.
      </p>
    </form>
  )
}
