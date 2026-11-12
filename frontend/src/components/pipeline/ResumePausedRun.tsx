"use client"

import { useState } from "react"
import { Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { authFetch } from "@/lib/api/auth-fetch"
import { API_ENDPOINTS } from "@/lib/config/api"

// The user paused this run (blocking_reason_type paused_by_user); the workflow
// waits for the "resume" signal, which POST /pipelines/:id/resume sends. Same
// request as the pipeline header's Resume. The chat card and the live-state
// panel both show it, so neither tells the user a pause cannot be answered.
export function ResumePausedRun({ pipelineId, onResumed }: { pipelineId: string; onResumed?: () => void }) {
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const resume = async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await authFetch(API_ENDPOINTS.PIPELINES.RESUME(pipelineId), {
        method: "POST",
        body: JSON.stringify({}),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        setError(data?.error || data?.message || `resume ${res.status}`)
        return
      }
      onResumed?.()
    } catch (e) {
      setError(String((e as Error)?.message ?? e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-2">
      <div className="text-xs text-zinc-600 dark:text-zinc-400">You paused this run.</div>
      <Button size="sm" onClick={resume} disabled={loading}>
        {loading ? (
          <>
            <Loader2 className="h-3 w-3 mr-1 animate-spin" />
            Resuming…
          </>
        ) : (
          "Resume"
        )}
      </Button>
      {error && <div className="text-xs text-red-600 dark:text-red-400">Could not resume: {error}</div>}
    </div>
  )
}
