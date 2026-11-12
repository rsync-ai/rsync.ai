"use client"

import { useEffect, useState } from "react"

import { getPipeline } from "@/lib/api/pipelines"

// Whether a Reload may be offered for this pipeline. False when its destination is
// an app's API (reverse ETL): a run updates the records it wrote before, and the
// gateway refuses a Reload (pipelines.go apiDestinationReloadRefusal), so a Reload
// button would only ever answer with that refusal.
//
// True until the gateway says otherwise, and on any error: the refusal still
// stands server-side, so offering the button costs one explained 422 at worst.
export function usePipelineReloadSupported(pipelineId: string | undefined): boolean {
  const [supported, setSupported] = useState(true)

  useEffect(() => {
    if (!pipelineId) return
    let cancelled = false
    const run = async () => {
      try {
        const p = await getPipeline(pipelineId)
        if (!cancelled) setSupported(p.reload_supported !== false)
      } catch {
        // keep offering it; the gateway refuses one it cannot run
      }
    }
    void run()
    return () => {
      cancelled = true
    }
  }, [pipelineId])

  return supported
}
