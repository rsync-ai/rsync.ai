"use client"

import { useCallback, useSyncExternalStore } from "react"

import { authFetchOrThrow } from "@/lib/api/auth-fetch"
import type { SchemaChange } from "@/lib/api/schema-changes"
import { API_ENDPOINTS } from "@/lib/config/api"
import { classifyError } from "@/lib/utils/error-handling"

// One schema-changes poller per pipeline, shared by the header's drift badge and
// the Steps tab's panel. Each used to run its own 30 s interval on the same
// route, so the Steps tab read it twice, and an approval made in the panel left
// the badge counting the change for up to 30 s more. Same shape as
// usePipelineStatePoll: the first reader starts the poll, the last one to leave
// stops it and drops it.

export const SCHEMA_CHANGES_POLL_MS = 30_000

export interface SchemaChangesPoll {
  /** null until a read succeeds; a later failure keeps the last good list. */
  changes: SchemaChange[] | null
  /** The last read failed, in words the panel can show. */
  error: string | null
  /** The first read has answered, either way. */
  settled: boolean
}

const IDLE: SchemaChangesPoll = Object.freeze({ changes: null, error: null, settled: false })

interface Poller {
  state: SchemaChangesPoll
  listeners: Set<() => void>
  refresh: () => Promise<void>
  stop: () => void
}

const pollers = new Map<string, Poller>()

function isHidden(): boolean {
  return typeof document !== "undefined" && document.visibilityState === "hidden"
}

function startPoller(pipelineId: string): Poller {
  let stopped = false
  let inflight: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | undefined

  const poller: Poller = { state: IDLE, listeners: new Set(), refresh: async () => {}, stop: () => {} }

  const update = (next: Partial<SchemaChangesPoll>) => {
    if (stopped) return
    poller.state = { ...poller.state, ...next }
    poller.listeners.forEach((l) => l())
  }

  const schedule = () => {
    if (stopped) return
    clearTimeout(timer)
    timer = setTimeout(() => {
      // A hidden tab skips its tick; the visibilitychange handler reads on return.
      if (isHidden()) schedule()
      else void read()
    }, SCHEMA_CHANGES_POLL_MS)
  }

  // A newer read aborts the one still out, so an approval's re-read is never
  // overwritten by a poll that started before the approval landed.
  const read = async () => {
    if (stopped) return
    inflight?.abort()
    const ac = new AbortController()
    inflight = ac
    clearTimeout(timer)
    try {
      const res = await authFetchOrThrow(API_ENDPOINTS.PIPELINES.SCHEMA_CHANGES(pipelineId), {
        cache: "no-store",
        signal: ac.signal,
      })
      const data = await res.json()
      if (ac.signal.aborted) return
      update({ changes: data.schema_changes ?? [], error: null, settled: true })
    } catch (e) {
      if (ac.signal.aborted) return
      update({ error: classifyError(e, "general").message, settled: true })
    } finally {
      if (inflight === ac) {
        inflight = null
        schedule()
      }
    }
  }

  const onVisibility = () => {
    if (!isHidden()) void read()
  }
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility)

  poller.refresh = read
  poller.stop = () => {
    stopped = true
    clearTimeout(timer)
    if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility)
    inflight?.abort()
  }
  void read()
  return poller
}

function subscribePoller(pipelineId: string, listener: () => void) {
  let poller = pollers.get(pipelineId)
  if (!poller) {
    poller = startPoller(pipelineId)
    pollers.set(pipelineId, poller)
  }
  const p = poller
  p.listeners.add(listener)
  return () => {
    p.listeners.delete(listener)
    if (p.listeners.size > 0) return
    p.stop()
    if (pollers.get(pipelineId) === p) pollers.delete(pipelineId)
  }
}

/**
 * The pipeline's schema changes, read by one poller however many readers mount
 * it, and a refresh that re-reads for all of them.
 */
export function usePipelineSchemaChanges(pipelineId: string): SchemaChangesPoll & { refresh: () => Promise<void> } {
  const subscribe = useCallback((listener: () => void) => subscribePoller(pipelineId, listener), [pipelineId])
  // Keyed by pipeline, so navigating to another pipeline never shows the
  // previous one's changes for a render.
  const getSnapshot = useCallback(() => pollers.get(pipelineId)?.state ?? IDLE, [pipelineId])
  const state = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  const refresh = useCallback(() => pollers.get(pipelineId)?.refresh() ?? Promise.resolve(), [pipelineId])
  return { ...state, refresh }
}
