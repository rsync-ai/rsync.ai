"use client"

import { useState } from "react"
import { Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  moveConnectionsToConnectorVersion,
  type MoveConnectionsResponse,
  type PinnedConnection,
} from "@/lib/api/discovery"

/**
 * After a save, the workspace's connections pinned to another version of the
 * connector keep running that version. The gateway lists them on the generate
 * answer (pinned_connections); this offers to move them all to the saved one.
 */
export function PinnedConnectionsPrompt({
  connectorName,
  version,
  pinned,
}: {
  connectorName: string
  version: string
  pinned: PinnedConnection[]
}) {
  const [moving, setMoving] = useState(false)
  const [moved, setMoved] = useState<MoveConnectionsResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  if (pinned.length === 0) return null

  const move = async () => {
    setMoving(true)
    setError(null)
    try {
      setMoved(await moveConnectionsToConnectorVersion(connectorName, version, pinned.map((p) => p.id)))
    } catch (e) {
      setError((e as Error)?.message || "Could not move the connections")
    } finally {
      setMoving(false)
    }
  }

  return (
    <div
      data-testid="pinned-connections"
      className="rounded-lg border border-amber-300 dark:border-amber-800 bg-amber-50 dark:bg-amber-950/30 p-3 text-[11px] text-amber-900 dark:text-amber-200 space-y-2"
    >
      {moved ? (
        <div data-testid="pinned-connections-moved" className="space-y-1">
          <div>{moved.message || `Moved to ${moved.connector_version}.`}</div>
          {moved.warning && <div className="font-medium">{moved.warning}</div>}
        </div>
      ) : (
        <>
          <div>
            {pinned.length === 1 ? "This connection is" : `These ${pinned.length} connections are`} set to
            another version and keep running it until you move {pinned.length === 1 ? "it" : "them"}:
          </div>
          <ul className="list-disc pl-4">
            {pinned.map((p) => (
              <li key={p.id}>
                {p.name || p.id} <span className="font-mono">({p.connector_version})</span>
              </li>
            ))}
          </ul>
          <Button size="sm" variant="outline" onClick={move} disabled={moving} data-testid="pinned-connections-move">
            {moving && <Loader2 className="h-3.5 w-3.5 mr-1.5 animate-spin" />}
            Move {pinned.length === 1 ? "it" : "them"} to {version}
          </Button>
          {error && (
            <div data-testid="pinned-connections-error" className="text-red-700 dark:text-red-300">
              {error}
            </div>
          )}
        </>
      )}
    </div>
  )
}
