"use client"

import { useCallback, useEffect, useState } from "react"
import { useWorkspace } from "@/contexts/WorkspaceContext"
import { listModelLayers, type WorkspaceLayer } from "@/lib/api/modelLayers"

interface Loaded {
  /** The workspace this answer is for. */
  workspaceId: string
  layers: WorkspaceLayer[] | null
  canManage: boolean
  error: string | null
}

/**
 * The active workspace's model layers, in their order, fetched once per page and
 * again when the workspace changes. A failed load is not fatal: the page still
 * shows each model's layer by name, only without its colour or its order.
 */
export function useWorkspaceLayers(): {
  layers: WorkspaceLayer[] | null
  canManage: boolean
  error: string | null
  reload: () => Promise<void>
} {
  const workspaceId = useWorkspace().activeWorkspace?.id ?? ""
  const [loaded, setLoaded] = useState<Loaded | null>(null)

  const settle = useCallback((forWorkspace: string, outcome: { data: Awaited<ReturnType<typeof listModelLayers>> } | { err: unknown }) => {
    if ("data" in outcome) {
      setLoaded({ workspaceId: forWorkspace, layers: outcome.data.layers, canManage: outcome.data.can_manage, error: null })
      return
    }
    const error = outcome.err instanceof Error ? outcome.err.message : "Could not load the model layers"
    // A failed reload keeps the list it had.
    setLoaded((prev) =>
      prev?.workspaceId === forWorkspace ? { ...prev, error } : { workspaceId: forWorkspace, layers: null, canManage: false, error }
    )
  }, [])

  // A response that lands after the workspace changed (or the page unmounted) is
  // dropped, so one workspace's layers never show under another's models.
  useEffect(() => {
    let alive = true
    listModelLayers().then(
      (data) => alive && settle(workspaceId, { data }),
      (err: unknown) => alive && settle(workspaceId, { err })
    )
    return () => {
      alive = false
    }
  }, [workspaceId, settle])

  const reload = useCallback(
    () =>
      listModelLayers().then(
        (data) => settle(workspaceId, { data }),
        (err: unknown) => settle(workspaceId, { err })
      ),
    [workspaceId, settle]
  )

  // Until this workspace's answer arrives, its layers are not loaded: never the
  // previous workspace's.
  const current = loaded?.workspaceId === workspaceId ? loaded : null
  return {
    layers: current?.layers ?? null,
    canManage: current?.canManage ?? false,
    error: current?.error ?? null,
    reload,
  }
}
