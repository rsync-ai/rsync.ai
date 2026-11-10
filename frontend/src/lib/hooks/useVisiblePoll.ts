"use client"

import { useCallback, useEffect, useRef } from "react"

import { captureWorkspace, getActiveWorkspaceId } from "@/lib/workspace/active-workspace"

/**
 * Runs `ask` now and then every `intervalMs` — only while the tab is visible, and never
 * two at a time — and hands each answer to `apply`. Returns a manual refresh that does
 * nothing while disabled.
 *
 * A timeout chain rather than setInterval, for the last of those. A workspace at the
 * fifty-model cap can take ~14 s to answer (eight concurrent queries, two seconds each),
 * so a fixed interval would start the next poll on top of the one still running —
 * doubling the load on the rebuild queue exactly when it is slowest. The next poll is
 * scheduled only once the last one has settled.
 *
 * An answer to an ask made in another workspace is dropped, not applied: one that lands
 * after a switch would show the old workspace's state under the new one's rows. So the
 * page's state changes only in `apply`, never in `ask`. After a switch the next ask is
 * for the new workspace rather than a wait on the old one still out.
 */
export function useVisiblePoll<T>(
  ask: () => Promise<T>,
  apply: (answer: T) => void,
  intervalMs: number,
  enabled: boolean,
): () => Promise<void> {
  // The check still out, and the workspace it asks about. The timer and Refresh both wait
  // on it rather than start a second one, so whichever of them meets it, the next check is
  // timed from when it lands.
  const inFlight = useRef<{ workspace: string | null; done: Promise<void> } | null>(null)

  const run = useCallback(() => {
    const workspace = getActiveWorkspaceId()
    if (!inFlight.current || inFlight.current.workspace !== workspace) {
      const isStale = captureWorkspace()
      const done: Promise<void> = ask()
        .then((answer) => {
          if (!isStale()) apply(answer)
        })
        .finally(() => {
          if (inFlight.current?.done === done) inFlight.current = null
        })
      inFlight.current = { workspace, done }
    }
    return inFlight.current!.done
  }, [ask, apply])

  useEffect(() => {
    if (!enabled) return
    let timer: ReturnType<typeof setTimeout> | null = null
    // Whether this chain is waiting on a check, and so will schedule its own next one.
    let waiting = false
    let stopped = false

    const tick = async () => {
      timer = null
      // A hidden tab stops the chain outright; the listener below restarts it. A page
      // left open in a background tab overnight asks nothing.
      if (document.visibilityState !== "visible") return
      waiting = true
      try {
        await run()
      } finally {
        waiting = false
      }
      if (!stopped) timer = setTimeout(() => void tick(), intervalMs)
    }
    const onVisibility = () => {
      // Gated on this chain, not on whether a check is out: a check Refresh started
      // schedules nothing after itself, so deferring to it would stop the page for good.
      if (document.visibilityState === "visible" && timer === null && !waiting) {
        void tick()
      }
    }

    void tick()
    document.addEventListener("visibilitychange", onVisibility)
    return () => {
      stopped = true
      if (timer !== null) clearTimeout(timer)
      document.removeEventListener("visibilitychange", onVisibility)
    }
  }, [enabled, intervalMs, run])

  return useCallback(() => (enabled ? run() : Promise.resolve()), [enabled, run])
}
