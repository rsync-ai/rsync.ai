"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { LayoutDashboard, Plus } from "lucide-react"

import { LIST_CARD_CLASS, ListCardMeta, ListCardPills, type OwnerFilter, ownerChips, ownerMatches } from "@/components/kit/ListCard"
import { EmptyState } from "@/components/kit/EmptyState"
import { FilterChips } from "@/components/kit/FilterChips"
import { ListCardsSkeleton } from "@/components/kit/PageSkeleton"
import { PageHeader } from "@/components/layout/PageHeader"
import { Button } from "@/components/ui/button"
import { listDashboards, type Dashboard } from "@/lib/api/dashboards"
import { captureWorkspace, onActiveWorkspaceChange } from "@/lib/workspace/active-workspace"

/**
 * The workspace's dashboards (dashboards-design.md § 4, § 7.4): saved charts and
 * text on a grid, with filters. Every member opens the ones shared with the
 * workspace; owners and admins make and change them.
 */
export default function DashboardsPage() {
  const [dashboards, setDashboards] = useState<Dashboard[] | null>(null)
  const [canManage, setCanManage] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [version, setVersion] = useState(0)
  const [owner, setOwner] = useState<OwnerFilter>("all")

  useEffect(
    () =>
      onActiveWorkspaceChange(() => {
        setDashboards(null)
        setError(null)
        setOwner("all")
        setVersion((v) => v + 1)
      }),
    [],
  )

  useEffect(() => {
    let alive = true
    const isStale = captureWorkspace()
    listDashboards().then(
      (got) => {
        if (!alive || isStale()) return
        setDashboards(got.dashboards)
        setCanManage(got.can_manage)
        setError(null)
      },
      (err: unknown) => {
        if (alive && !isStale()) setError(err instanceof Error ? err.message : "Could not load dashboards")
      },
    )
    return () => {
      alive = false
    }
  }, [version])

  const newDashboard = canManage ? (
    <Button asChild size="sm">
      <Link href="/dashboards/new">
        <Plus className="mr-1.5 h-4 w-4" /> New dashboard
      </Link>
    </Button>
  ) : null
  const shown = dashboards?.filter((d) => ownerMatches(d, owner)) ?? []

  return (
    <div className="space-y-5">
      <PageHeader heading="Dashboards" description="Saved charts and text on one page, read live, with filters.">
        {dashboards !== null && dashboards.length > 0 && newDashboard}
      </PageHeader>

      {error && (
        <p role="alert" className="text-sm text-red-700 dark:text-red-300">
          {error}{" "}
          <button type="button" className="underline" onClick={() => setVersion((v) => v + 1)}>
            Retry
          </button>
        </p>
      )}
      {dashboards === null && !error && <ListCardsSkeleton label="Loading dashboards…" />}
      {dashboards !== null && dashboards.length === 0 && (
        <div className="rounded-xl border border-zinc-200 dark:border-zinc-800">
          <EmptyState
            icon={LayoutDashboard}
            title="No dashboards yet"
            description={
              canManage
                ? "Put saved charts and text on one page, with filters everyone can change. Ask rsync.ai can draft one too."
                : "Workspace owners and admins make them."
            }
            action={newDashboard}
          />
        </div>
      )}
      {dashboards !== null && dashboards.length > 0 && (
        <>
          <FilterChips label="Show dashboards" chips={ownerChips(dashboards)} value={owner} onChange={setOwner} />
          {shown.length === 0 ? (
            <p className="text-sm text-zinc-500 dark:text-zinc-400">
              {owner === "mine" ? "You have not made a dashboard yet." : "Nobody else has shared a dashboard yet."}
            </p>
          ) : (
            <ul className="grid gap-4 @xl:grid-cols-2 @4xl:grid-cols-3">
              {shown.map((d) => (
                <li key={d.id} className="flex">
                  <DashboardCard dashboard={d} />
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </div>
  )
}

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`
}

/** A dashboard as a card: what it is, who sees it, how much is on it, when it last changed. */
function DashboardCard({ dashboard: d }: { dashboard: Dashboard }) {
  const filters = d.filters.length === 0 ? "no filters" : plural(d.filters.length, "filter", "filters")
  return (
    <Link href={`/dashboards/${d.id}`} className={LIST_CARD_CLASS}>
      <div className="flex items-start gap-3">
        <span
          aria-hidden
          className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-violet-50 text-violet-600 dark:bg-violet-950/40 dark:text-violet-400"
        >
          <LayoutDashboard className="h-4 w-4" />
        </span>
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-sm font-semibold text-zinc-900 dark:text-white">{d.name}</h2>
          <ListCardPills visibility={d.visibility} proposedBy={d.proposed_by} />
        </div>
      </div>
      {d.description && <p className="mt-3 line-clamp-2 text-sm text-zinc-600 dark:text-zinc-400">{d.description}</p>}
      <ListCardMeta updatedAt={d.updated_at}>
        {plural(d.tile_count, "tile", "tiles")} ·{" "}
        <span title={d.filters.map((f) => f.label).join(", ") || undefined}>{filters}</span>
      </ListCardMeta>
    </Link>
  )
}
