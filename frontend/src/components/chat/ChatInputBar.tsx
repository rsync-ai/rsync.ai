"use client"

import { Send } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { TabHint } from "@/components/ui/tab-hint"
import type { ChatMessage } from "@/components/chat/ChatMessageItem"
import { useSuggestionTab } from "@/hooks/useSuggestionTab"

const DEFAULT_PLACEHOLDER = "Describe what data you want to move…"

/**
 * What Tab writes into the empty box: the latest reply's first chip, the one a
 * click on it would write. Only a reply still waiting on the user offers one: a
 * confirmation shows no chips (its card has the buttons), and a connector_missing
 * "generate X" chip is a link to the generator, not text for the box.
 */
export function replySuggestion(messages: readonly ChatMessage[]): string | undefined {
  const last = messages[messages.length - 1]
  if (!last || last.role !== "assistant" || last.responseType === "confirmation") return undefined
  const chips = Array.isArray(last.suggestions) ? last.suggestions.slice(0, 6).map(String) : []
  return chips.find(
    (text) => text.trim() && !(last.responseType === "connector_missing" && /^generate\s+(.+)$/i.test(text)),
  )
}

interface ChatInputBarProps {
  input: string
  onInputChange: (val: string) => void
  isSendingMessage: boolean
  pipelineId: string | undefined
  isPipelineDone: boolean
  executionFailed: boolean
  onStartNew: () => void
  onViewPipeline: () => void
  onRetry: () => void
  /** Written in by Tab while the box is empty; also shown as the placeholder. */
  suggestion?: string
  /** The message box, for a caller that focuses it (the empty chat's Custom card). */
  inputRef?: React.Ref<HTMLInputElement>
}

export function ChatInputBar({
  input,
  onInputChange,
  isSendingMessage,
  pipelineId,
  isPipelineDone,
  executionFailed,
  onStartNew,
  onViewPipeline,
  onRetry,
  suggestion,
  inputRef,
}: ChatInputBarProps) {
  const inputDisabled = (Boolean(pipelineId) && !isPipelineDone) || isSendingMessage
  const tab = useSuggestionTab({ value: input, suggestion, onAccept: onInputChange })

  if (isPipelineDone && pipelineId) {
    return (
      <div className="flex items-center gap-2 flex-wrap">
        <Button
          size="sm"
          variant="default"
          onClick={onStartNew}
          className="bg-violet-600 hover:bg-violet-700 text-white"
        >
          Start new pipeline
        </Button>
        <Button size="sm" variant="outline" onClick={onViewPipeline}>
          View pipeline details
        </Button>
        {executionFailed && (
          <Button
            size="sm"
            variant="outline"
            className="border-red-300 text-red-700 hover:bg-red-50"
            onClick={onRetry}
          >
            Retry with changes
          </Button>
        )}
      </div>
    )
  }

  return (
    <div>
      <div className="flex gap-2">
        <Input
          ref={inputRef}
          value={input}
          onChange={(e) => onInputChange(e.target.value)}
          onKeyDown={tab.onKeyDown}
          placeholder={suggestion?.trim() || DEFAULT_PLACEHOLDER}
          aria-label="Message"
          disabled={inputDisabled}
        />
        <Button type="submit" disabled={inputDisabled || !input.trim()} aria-label="Send message">
          <Send className="h-4 w-4" />
        </Button>
      </div>
      {tab.showHint && !inputDisabled && <TabHint className="mt-1.5" />}
    </div>
  )
}
