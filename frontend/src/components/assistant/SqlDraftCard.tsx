"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { Check, Copy } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { openSqlInExplorer } from "@/lib/explorer/queryTabs"
import type { SqlDraft } from "@/lib/api/assistant"

/**
 * The SQL Ask rsync.ai wrote (generate_sql), shown as a card under the answer
 * (docs/internal/dashboards-design.md § 7.5). Nothing has run: Open in Explorer
 * puts it in a new query tab on its connection, where the person runs it. The
 * model never saw this SQL, so the card is the only place it appears.
 */
export function SqlDraftCard({ draft }: { draft: SqlDraft }) {
  const [copied, setCopied] = useState(false)
  useEffect(() => {
    if (!copied) return
    const timer = setTimeout(() => setCopied(false), 1500)
    return () => clearTimeout(timer)
  }, [copied])

  return (
    <section
      aria-label="Written SQL"
      className="space-y-2 rounded-xl border border-zinc-200 bg-white p-4 text-sm dark:border-zinc-800 dark:bg-zinc-900"
    >
      <header className="flex flex-wrap items-start justify-between gap-2">
        <h3 className="font-semibold text-zinc-900 dark:text-white">SQL</h3>
        <Badge variant="secondary">Not run</Badge>
      </header>
      <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-zinc-50 p-3 font-mono text-xs text-zinc-800 dark:bg-zinc-950 dark:text-zinc-200">
        {draft.sql}
      </pre>
      {draft.tables.length > 0 && (
        <p className="break-words text-xs text-zinc-500 dark:text-zinc-400">From {draft.tables.join(", ")}</p>
      )}
      <div className="flex flex-wrap items-center gap-3 text-xs">
        <button
          type="button"
          onClick={() => {
            navigator.clipboard?.writeText(draft.sql).then(
              () => setCopied(true),
              () => {},
            )
          }}
          className="inline-flex items-center gap-1 rounded px-1 py-0.5 font-medium text-zinc-700 hover:bg-zinc-100 dark:text-zinc-300 dark:hover:bg-zinc-800"
          aria-label={copied ? "Copied" : "Copy SQL"}
        >
          {copied ? <Check className="h-3 w-3" /> : <Copy className="h-3 w-3" />}
          {copied ? "Copied" : "Copy"}
        </button>
        <Link
          href="/explorer"
          onClick={() => openSqlInExplorer({ sql: draft.sql, connectionId: draft.connection_id })}
          className="font-medium text-violet-700 hover:underline dark:text-violet-300"
        >
          Open in Explorer
        </Link>
      </div>
    </section>
  )
}
