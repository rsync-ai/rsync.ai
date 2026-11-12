"use client"

import { useEffect, useState } from "react"
import { ShieldAlert } from "lucide-react"

import { getWorkspaceLLMData, modelNames, type WorkspaceLLMData } from "@/lib/api/assistant"

/**
 * Says so when Ask rsync.ai may read sample rows in this workspace, where someone
 * is about to add a connection whose tables it could read. Nothing while
 * sample rows are off, no model is approved, the assistant is off, or the
 * setting cannot be read.
 */
export function SampleRowsNotice() {
  const [data, setData] = useState<WorkspaceLLMData | null>(null)

  useEffect(() => {
    let cancelled = false
    getWorkspaceLLMData()
      .then((res) => !cancelled && setData(res))
      .catch(() => !cancelled && setData(null))
    return () => {
      cancelled = true
    }
  }, [])

  if (data?.mode !== "sample_rows" || data.approved_models.length === 0) return null
  return (
    <p
      role="note"
      className="flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-200"
    >
      <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
      <span>
        Sample rows are on in this workspace: Ask rsync.ai may send {modelNames(data.approved_models)} up to 5 rows of
        this connection&apos;s tables, with personal and secret columns withheld. The workspace owner can turn this off
        in Workspace settings, Data.
      </span>
    </p>
  )
}
