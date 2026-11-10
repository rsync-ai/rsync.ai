"use client"

import { useEffect } from "react"
import { createPortal } from "react-dom"
import { Sparkles } from "lucide-react"

import { Button } from "@/components/ui/button"
import { useHydrated } from "@/components/ui/local-date-time"
import { useAssistantSession } from "@/lib/assistant/session"
import { useModKey } from "@/lib/hooks/useModKey"
import { useAssistantStore } from "@/lib/store/useAssistantStore"
import { ASSISTANT_LAUNCHER_ID, ASSISTANT_PANEL_ID, AssistantPanel } from "./AssistantPanel"

/**
 * The header's "Ask rsync.ai" button, its ⌘/ (Ctrl+/) shortcut, and the panel it
 * opens. The header renders it only when the assistant flag is on. A reload
 * shows the panel as it was (lib/assistant/session.ts).
 *
 * The panel is portaled to <body>: the header's backdrop blur makes it the
 * containing block of any `position: fixed` element inside it, which would
 * squeeze the panel into the header's own box.
 */
export function AssistantLauncher() {
  const open = useAssistantStore((s) => s.open)
  const toggle = useAssistantStore((s) => s.toggle)
  const hydrated = useHydrated()
  const modKey = useModKey()
  useAssistantSession()

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      // A widget that already handled the key keeps it: Mod-/ comments a line
      // in the SQL editor.
      if (e.defaultPrevented) return
      if (e.key === "/" && (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey) {
        e.preventDefault()
        toggle()
      }
    }
    window.addEventListener("keydown", onKey)
    return () => window.removeEventListener("keydown", onKey)
  }, [toggle])

  return (
    <>
      <Button
        id={ASSISTANT_LAUNCHER_ID}
        variant="ghost"
        size="sm"
        onClick={toggle}
        aria-label="Ask rsync.ai"
        aria-expanded={open}
        aria-controls={open ? ASSISTANT_PANEL_ID : undefined}
        aria-keyshortcuts="Meta+/ Control+/"
        title={`Ask rsync.ai (${modKey === "⌘" ? "⌘/" : "Ctrl+/"})`}
        className="gap-1.5 text-violet-700 hover:text-violet-800 dark:text-violet-300 dark:hover:text-violet-200"
      >
        <Sparkles className="h-4 w-4" />
        <span className="hidden text-sm md:inline">Ask rsync.ai</span>
      </Button>
      {hydrated && createPortal(<AssistantPanel />, document.body)}
    </>
  )
}
