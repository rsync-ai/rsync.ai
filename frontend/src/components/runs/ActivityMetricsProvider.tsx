"use client"

import { createContext, useContext, useEffect, useRef, useState } from "react"

import {
  METRICS_WINDOWS,
  getActivityMetrics,
  type ActivityMetrics,
  type MetricsWindow,
} from "@/lib/api/activity-metrics"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

export interface ActivityMetricsState {
  days: MetricsWindow
  setDays: (n: MetricsWindow) => void
  /** Null while the window loads, and after an error. */
  metrics: ActivityMetrics | null
  /**
   * The newest answer for this workspace, kept while another window loads.
   * Every window ends today, so a trend of the last 7 days can draw from it
   * instead of flashing a placeholder. Null after a workspace switch.
   */
  latest: ActivityMetrics | null
  error: string | null
  reload: () => void
}

const ActivityMetricsContext = createContext<ActivityMetricsState | null>(null)

/**
 * One read of GET /activity/metrics for everything under it: Home's run charts
 * and the trend lines on its overview cards. A window change or a workspace
 * switch re-reads, and an answer either one overtook is dropped.
 */
export function ActivityMetricsProvider({ children }: { children: React.ReactNode }) {
  const [days, setDaysState] = useState<MetricsWindow>(METRICS_WINDOWS[0])
  const [metrics, setMetrics] = useState<ActivityMetrics | null>(null)
  const [latest, setLatest] = useState<ActivityMetrics | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  // Bumped by every load, so an answer a window change or a switch overtook is dropped.
  const loadSeq = useRef(0)

  const reload = () => {
    setMetrics(null)
    setError(null)
    setVersion((v) => v + 1)
  }

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setMetrics(null)
        setLatest(null)
        setError(null)
        setVersion((v) => v + 1)
      }),
    [],
  )

  useEffect(() => {
    const seq = ++loadSeq.current
    const isStale = captureWorkspace()
    getActivityMetrics(days).then(
      (m) => {
        if (seq !== loadSeq.current || isStale()) return
        setMetrics(m)
        setLatest(m)
      },
      (err: unknown) => {
        if (seq !== loadSeq.current || isStale()) return
        setError(err instanceof Error ? err.message : "Could not load run charts.")
      },
    )
  }, [days, version])

  const setDays = (n: MetricsWindow) => {
    if (n === days) return
    setMetrics(null)
    setError(null)
    setDaysState(n)
  }

  return (
    <ActivityMetricsContext.Provider value={{ days, setDays, metrics, latest, error, reload }}>
      {children}
    </ActivityMetricsContext.Provider>
  )
}

/** The shared read, or null outside a provider (a card then draws no trend). */
export function useActivityMetrics(): ActivityMetricsState | null {
  return useContext(ActivityMetricsContext)
}
