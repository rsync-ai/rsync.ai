"use client"

import { useEffect, useRef } from "react"

/** How often a workflow view reads a run again while it is going. */
export const RUN_POLL_MS = 3000

/**
 * While `running`, calls `poll` every RUN_POLL_MS. A tick that comes while the
 * last poll is still waiting for its answer is skipped, so an answer slower
 * than the interval lands instead of being cut off by the next read. `poll`
 * handles its own errors.
 */
export function usePollWhileRunning(running: boolean, poll: () => Promise<void>) {
  const latest = useRef(poll)
  const inFlight = useRef(false)
  useEffect(() => {
    latest.current = poll
  })
  useEffect(() => {
    if (!running) return
    const t = setInterval(() => {
      if (inFlight.current) return
      inFlight.current = true
      void latest.current().finally(() => {
        inFlight.current = false
      })
    }, RUN_POLL_MS)
    return () => clearInterval(t)
  }, [running])
}
