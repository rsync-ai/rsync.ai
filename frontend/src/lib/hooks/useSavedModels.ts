"use client"

import { useCallback, useEffect, useState } from "react"
import { useWorkspace } from "@/contexts/WorkspaceContext"
import { authFetch } from "@/lib/api/auth-fetch"

/** A saved query as GET /explorer/saved lists it: what a Lineage card shows and the Schedule dialog opens with. */
export interface SavedModel {
  id: string
  name: string
  connection_id?: string
  connector_type?: string
  query_language?: string
  materialization: string
  target_table?: string
  layer?: string
  statement_class?: string
  supports_materialization?: boolean
  last_run_at?: string
  last_run_status?: string
  last_run_error?: string
  /** The live schedule's status; "" when the model has none. */
  schedule_status?: string
}

const NONE: ReadonlyMap<string, SavedModel> = new Map()

/**
 * The workspace's saved queries by id, for a model's last run on the Lineage page and
 * for opening its Schedule dialog there. Context for a page that works without it: a
 * failed read leaves what it had, and says nothing. The list stops at the 500 most
 * recently edited (the models page says so), so an older model has no entry.
 */
export function useSavedModels(reloadTick = 0): { saved: ReadonlyMap<string, SavedModel>; reload: () => void } {
  const workspaceId = useWorkspace().activeWorkspace?.id ?? ""
  const [loaded, setLoaded] = useState<{ workspaceId: string; saved: ReadonlyMap<string, SavedModel> } | null>(null)
  const [ownTick, setOwnTick] = useState(0)

  useEffect(() => {
    let alive = true
    authFetch("/api/v1/explorer/saved", { cache: "no-store" })
      .then(async (res) => (res.ok ? ((await res.json()) as { saved_queries?: unknown }) : null))
      .then((body) => {
        if (!alive || !Array.isArray(body?.saved_queries)) return
        const rows = body.saved_queries as SavedModel[]
        setLoaded({ workspaceId, saved: new Map(rows.map((q) => [q.id, q])) })
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [workspaceId, reloadTick, ownTick])

  const reload = useCallback(() => setOwnTick((t) => t + 1), [])
  // Never another workspace's rows under this one's models.
  return { saved: loaded?.workspaceId === workspaceId ? loaded.saved : NONE, reload }
}
