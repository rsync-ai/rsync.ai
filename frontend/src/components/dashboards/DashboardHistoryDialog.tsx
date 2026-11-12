"use client"

import { useCallback, useEffect, useMemo, useState } from "react"
import { History, Loader2, RotateCcw } from "lucide-react"
import { toast } from "sonner"

import { readingOrder, widthLabel } from "@/components/dashboards/dashboard-layout"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { LocalDateTime } from "@/components/ui/local-date-time"
import { ChartError } from "@/lib/api/charts"
import {
  listDashboardVersions,
  updateDashboard,
  type Dashboard,
  type DashboardTileInput,
  type DashboardVersion,
  type DashboardVersions,
} from "@/lib/api/dashboards"
import { captureWorkspace } from "@/lib/workspace/active-workspace"

/** What the save that replaced a version did, in words. */
export function savedHow(v: Pick<DashboardVersion, "how" | "restored_from">): string {
  if (v.how === "ask") return "applied a change from Ask rsync.ai"
  if (v.how === "restore") return v.restored_from ? `restored version ${v.restored_from}` : "restored a version"
  return "saved an edit"
}

/** The tiles in the order they read, the same way for a version and for the dashboard now. */
function sameTiles(a: readonly DashboardTileInput[], b: readonly DashboardTileInput[]): boolean {
  const key = (ts: readonly DashboardTileInput[]) =>
    JSON.stringify(
      readingOrder(ts).map((t) => [
        t.kind,
        t.chart_id ?? "",
        t.kind === "text" ? (t.body ?? "") : "",
        t.x,
        t.y,
        t.w,
        t.h,
      ]),
    )
  return key(a) === key(b)
}

/**
 * A dashboard's History (dashboards-design.md § 7.7): every save (an edit, an Ask
 * rsync.ai change applied, or a restore) kept the dashboard as it was before it,
 * with who saved and how. Restore puts a version's name, description, filters and
 * tiles back as a new save, which is kept too, so a restore never loses a version.
 * Its visibility stays as it is now, and each chart keeps its current settings.
 */
export function DashboardHistoryDialog({
  dashboard,
  open,
  onOpenChange,
  onRestored,
}: {
  dashboard: Dashboard
  open: boolean
  onOpenChange: (open: boolean) => void
  onRestored: (saved: Dashboard) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-3xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <History className="h-4 w-4" aria-hidden />
            History of &ldquo;{dashboard.name}&rdquo;
          </DialogTitle>
          <DialogDescription>
            Every save keeps the dashboard as it was before it. Restore puts an earlier version back as a new save, so
            nothing is lost.
          </DialogDescription>
        </DialogHeader>
        {/* The Dialog renders nothing while closed, so each open starts fresh. The body
            lays out by the dialog's width, not the window's. */}
        <div className="@container">
          <HistoryBody dashboard={dashboard} onRestored={onRestored} onClose={() => onOpenChange(false)} />
        </div>
      </DialogContent>
    </Dialog>
  )
}

function HistoryBody({
  dashboard,
  onRestored,
  onClose,
}: {
  dashboard: Dashboard
  onRestored: (saved: Dashboard) => void
  onClose: () => void
}) {
  const [data, setData] = useState<DashboardVersions | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [reload, setReload] = useState(0)
  const [selected, setSelected] = useState<number | null>(null)
  // The version whose Restore is being confirmed, and a refused restore's reason,
  // each tied to its version so picking another shows neither.
  const [confirming, setConfirming] = useState<number | null>(null)
  const [restoring, setRestoring] = useState(false)
  const [restoreError, setRestoreError] = useState<{ version: number; message: string } | null>(null)

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    listDashboardVersions(dashboard.id).then(
      (got) => {
        if (!alive || isStale()) return
        setData(got)
        // Keep the reader on the version they picked, unless it is gone.
        setSelected((prev) =>
          prev !== null && got.versions.some((v) => v.version === prev) ? prev : (got.versions[0]?.version ?? null),
        )
      },
      (err: unknown) => {
        if (alive && !isStale()) setLoadError(err instanceof Error ? err.message : "Could not load the history")
      },
    )
    return () => {
      alive = false
    }
  }, [dashboard.id, reload])

  // A chart tile names its chart by id; the dashboard now says what each is called.
  const chartNames = useMemo(() => {
    const names = new Map<string, string>()
    for (const t of dashboard.tiles ?? []) if (t.chart_id && t.chart) names.set(t.chart_id, t.chart.name)
    return names
  }, [dashboard.tiles])

  const nowTiles = useMemo<DashboardTileInput[]>(
    () =>
      (dashboard.tiles ?? []).map((t) =>
        t.kind === "chart"
          ? { kind: "chart", chart_id: t.chart_id, x: t.x, y: t.y, w: t.w, h: t.h }
          : { kind: "text", body: t.body, x: t.x, y: t.y, w: t.w, h: t.h },
      ),
    [dashboard.tiles],
  )

  const version = data?.versions.find((v) => v.version === selected) ?? null

  const restore = useCallback(async () => {
    if (!version || !data?.current || restoring) return
    setRestoring(true)
    setRestoreError(null)
    try {
      const saved = await updateDashboard(dashboard.id, {
        name: version.name,
        description: version.description,
        filters: version.filters,
        tiles: version.tiles,
        visibility: data.current.visibility,
        updated_at: data.current.updated_at,
        restored_from: version.version,
      })
      toast.success(`Restored version ${version.version}`, {
        description: "The dashboard as it was is kept in the History too.",
      })
      onRestored(saved)
      onClose()
    } catch (err) {
      if (err instanceof ChartError && err.code === "stale") {
        setRestoreError({
          version: version.version,
          message:
            "Someone saved this dashboard since the History was loaded, so nothing was restored. The History has been reloaded.",
        })
        setReload((n) => n + 1)
      } else {
        setRestoreError({
          version: version.version,
          message: err instanceof Error ? err.message : "Could not restore the version",
        })
      }
    } finally {
      setRestoring(false)
      setConfirming(null)
    }
  }, [version, data, restoring, dashboard.id, onRestored, onClose])

  return loadError ? (
    <div className="flex flex-col items-start gap-2 py-6">
      <p role="alert" className="text-sm text-red-600 dark:text-red-400">
        {loadError}
      </p>
      <Button
        size="sm"
        variant="outline"
        onClick={() => {
          setLoadError(null)
          setReload((n) => n + 1)
        }}
      >
        Retry
      </Button>
    </div>
  ) : !data ? (
    <div role="status" className="flex items-center gap-2 py-8 text-sm text-zinc-500 dark:text-zinc-400">
      <Loader2 className="h-4 w-4 animate-spin" /> Loading the history…
    </div>
  ) : data.versions.length === 0 ? (
    <p className="rounded-md border bg-zinc-50 p-3 text-sm text-zinc-500 dark:bg-zinc-900 dark:text-zinc-400">
      This dashboard has not been saved since it was made, so there is no earlier version yet. The next save keeps a
      copy of what it replaces.
    </p>
  ) : (
    <div className="grid max-h-[65vh] gap-4 overflow-y-auto pr-1 @md:grid-cols-[minmax(0,14rem)_minmax(0,1fr)]">
      <ol aria-label="Versions" className="space-y-1">
        {data.versions.map((v) => (
          <li key={v.version}>
            <button
              type="button"
              aria-pressed={v.version === selected}
              onClick={() => setSelected(v.version)}
              className={`w-full rounded-md border px-3 py-2 text-left text-xs transition-colors ${
                v.version === selected
                  ? "border-zinc-900 bg-zinc-50 dark:border-zinc-100 dark:bg-zinc-800"
                  : "border-zinc-200 hover:bg-zinc-50 dark:border-zinc-800 dark:hover:bg-zinc-900"
              }`}
            >
              <span className="block font-medium text-zinc-900 dark:text-white">Version {v.version}</span>
              <span className="block text-zinc-500 dark:text-zinc-400">
                Before {v.how === "ask" ? "an Ask rsync.ai change" : v.how === "restore" ? "a restore" : "an edit"}
                {v.saved_by ? ` by ${v.saved_by}` : ""}
              </span>
              <span className="block text-zinc-500 dark:text-zinc-400">
                <LocalDateTime value={v.created_at} />
              </span>
            </button>
          </li>
        ))}
      </ol>

      {version && (
        <section aria-label={`Version ${version.version}`} className="space-y-3 text-sm">
          <p className="text-zinc-700 dark:text-zinc-300">
            The dashboard as it was before {version.saved_by || "someone"} {savedHow(version)} on{" "}
            <LocalDateTime value={version.created_at} />.
          </p>
          <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs">
            <dt className="text-zinc-500 dark:text-zinc-400">Name</dt>
            <dd className="break-words text-zinc-900 dark:text-zinc-100">
              {version.name}
              {version.name !== dashboard.name && (
                <span className="text-zinc-500 dark:text-zinc-400"> (now {dashboard.name})</span>
              )}
            </dd>
            <dt className="text-zinc-500 dark:text-zinc-400">Filters</dt>
            <dd className="text-zinc-900 dark:text-zinc-100">
              {version.filters.length === 0 ? "None" : version.filters.map((f) => f.label).join(", ")}
            </dd>
            <dt className="text-zinc-500 dark:text-zinc-400">Tiles</dt>
            <dd className="text-zinc-900 dark:text-zinc-100">
              {version.tiles.length}
              {sameTiles(version.tiles, nowTiles) && (
                <span className="text-zinc-500 dark:text-zinc-400"> (the same as now)</span>
              )}
            </dd>
          </dl>
          {version.tiles.length > 0 && (
            <ol
              aria-label="Tiles in this version"
              className="space-y-1 rounded-md bg-zinc-50 p-2 text-xs dark:bg-zinc-950"
            >
              {readingOrder(version.tiles).map((t, i) => (
                <li key={i} className="flex flex-wrap justify-between gap-x-3 text-zinc-800 dark:text-zinc-200">
                  <span className="min-w-0 break-words">
                    {t.kind === "text"
                      ? "Text"
                      : (t.chart_id && chartNames.get(t.chart_id)) || "A chart that is not on the dashboard now"}
                  </span>
                  <span className="text-zinc-500 dark:text-zinc-400">
                    {widthLabel(t.w)}, {t.h} {t.h === 1 ? "row" : "rows"} high
                  </span>
                </li>
              ))}
            </ol>
          )}
          <p className="text-xs text-zinc-500 dark:text-zinc-400">
            Each chart keeps its settings as they are now; only the dashboard is restored.
          </p>

          {restoreError?.version === version.version && (
            <p
              role="alert"
              className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-xs text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-200"
            >
              {restoreError.message}
            </p>
          )}

          {data.can_manage && data.current ? (
            confirming === version.version ? (
              <div
                role="group"
                aria-label="Confirm restore"
                className="space-y-2 rounded-md border border-amber-500/50 bg-amber-500/5 p-3 text-xs"
              >
                <p className="text-zinc-800 dark:text-zinc-200">
                  Restore version {version.version}? The dashboard as it is now is kept as the next version, so this can
                  be undone.
                </p>
                <div className="flex flex-wrap gap-2">
                  <Button size="sm" onClick={() => void restore()} loading={restoring}>
                    <RotateCcw className="mr-1 h-3.5 w-3.5" aria-hidden /> Restore version {version.version}
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => setConfirming(null)} disabled={restoring}>
                    Cancel
                  </Button>
                </div>
              </div>
            ) : (
              <Button size="sm" variant="outline" onClick={() => setConfirming(version.version)}>
                <RotateCcw className="mr-1 h-3.5 w-3.5" aria-hidden /> Restore this version
              </Button>
            )
          ) : (
            !data.can_manage && (
              <p className="text-xs text-zinc-500 dark:text-zinc-400">
                Only workspace owners and admins can restore a version.
              </p>
            )
          )}
        </section>
      )}
    </div>
  )
}
