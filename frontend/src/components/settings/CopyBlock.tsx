"use client"

import { useEffect, useRef, useState } from "react"
import { Check, Copy } from "lucide-react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"

/** A labelled value with a Copy button: a token, a command, a config file. */
export function CopyBlock({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false)
  const preRef = useRef<HTMLPreElement>(null)
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)

  useEffect(() => () => clearTimeout(timer.current), [])

  const copy = async () => {
    try {
      if (!navigator.clipboard) throw new Error("no clipboard")
      await navigator.clipboard.writeText(value)
      setCopied(true)
      clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), 2000)
    } catch {
      // No clipboard (plain http, a denied permission): select the text so a
      // keyboard copy takes it, and say so.
      const pre = preRef.current
      const selection = window.getSelection()
      if (pre && selection) {
        const range = document.createRange()
        range.selectNodeContents(pre)
        selection.removeAllRanges()
        selection.addRange(range)
      }
      toast.error(`Could not copy the ${label}`, { description: "It is selected: press ⌘C or Ctrl+C to copy it." })
    }
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs font-medium text-zinc-600 dark:text-zinc-400">{label}</span>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-7 gap-1.5 px-2 text-xs"
          onClick={() => void copy()}
          aria-label={`Copy ${label}`}
        >
          {copied ? <Check className="h-3.5 w-3.5 text-emerald-600" /> : <Copy className="h-3.5 w-3.5" />}
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
      <pre
        ref={preRef}
        className="whitespace-pre-wrap break-words rounded-md border border-zinc-200 bg-zinc-50 p-3 font-mono text-xs leading-relaxed text-zinc-800 [overflow-wrap:anywhere] dark:border-zinc-800 dark:bg-zinc-950 dark:text-zinc-200"
      >
        {value}
      </pre>
      <span className="sr-only" aria-live="polite">
        {copied ? `${label} copied` : ""}
      </span>
    </div>
  )
}
