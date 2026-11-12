"use client"

import { useEffect, useState } from "react"
import { Check, Copy } from "lucide-react"
import { toast } from "sonner"

import { copyTextToClipboard } from "@/lib/clipboard"
import { cn } from "@/lib/utils"

/**
 * An id as a short chip that copies the whole id: its first eight characters,
 * the full id on hover, or the whole id with `full` (a settings row, where the
 * person reads it). `label` names what it is ("run ID"), for a screen reader.
 * On an origin with no clipboard (a self-host on plain HTTP) it says it could
 * not copy. "Copied" is announced beside the button: inside it, the button's
 * aria-label would hide it.
 */
export function CopyId({
  value,
  label,
  full = false,
  className,
}: {
  value: string
  label: string
  full?: boolean
  className?: string
}) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(t)
  }, [copied])

  return (
    <>
      <button
        type="button"
        title={value}
        aria-label={`Copy ${label} ${value}`}
        onClick={async () => {
          if (await copyTextToClipboard(value)) setCopied(true)
          else toast.error(`Could not copy the ${label}`)
        }}
        className={cn(
          "inline-flex items-center gap-1 rounded-md bg-zinc-100 px-1.5 py-0.5 font-mono text-xs text-zinc-700 transition-colors hover:bg-zinc-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400 dark:bg-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-700",
          className,
        )}
      >
        {full ? value : value.slice(0, 8)}
        {copied ? <Check className="h-3 w-3" aria-hidden /> : <Copy className="h-3 w-3" aria-hidden />}
      </button>
      <span className="sr-only" aria-live="polite">
        {copied ? "Copied" : ""}
      </span>
    </>
  )
}
