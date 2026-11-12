import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

/**
 * Grey blocks shaped like what is loading, so nothing jumps when it arrives.
 * Each is one status region with a sentence for a screen reader.
 */
function Loading({ label, className, children }: { label: string; className?: string; children: React.ReactNode }) {
  return (
    <div role="status" className={className}>
      <span className="sr-only">{label}</span>
      <div aria-hidden>{children}</div>
    </div>
  )
}

function StatBlock() {
  return (
    <div className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800">
      <Skeleton className="h-4 w-24" />
      <Skeleton className="mt-3 h-8 w-16" />
      <Skeleton className="mt-3 h-3 w-40" />
    </div>
  )
}

/** A row of KPI cards. */
export function StatCardsSkeleton({ count = 3, label = "Loading…", className }: { count?: number; label?: string; className?: string }) {
  return (
    <Loading label={label}>
      <div className={cn("grid gap-4 @3xl:grid-cols-3", className)}>
        {Array.from({ length: count }, (_, i) => (
          <StatBlock key={i} />
        ))}
      </div>
    </Loading>
  )
}

/** A grid of chart cards. */
export function ChartCardsSkeleton({ count = 2, label = "Loading charts…", className }: { count?: number; label?: string; className?: string }) {
  return (
    <Loading label={label}>
      <div className={cn("grid gap-4 @3xl:grid-cols-2", className)}>
        {Array.from({ length: count }, (_, i) => (
          <div key={i} className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800">
            <Skeleton className="h-4 w-28" />
            <Skeleton className="mt-1.5 h-3 w-40" />
            <Skeleton className="mt-3 h-[200px] w-full" />
          </div>
        ))}
      </div>
    </Loading>
  )
}

/** A grid of list cards: a title, two lines of description and a meta line. */
export function ListCardsSkeleton({ count = 6, label = "Loading…", className }: { count?: number; label?: string; className?: string }) {
  return (
    <Loading label={label}>
      <div className={cn("grid gap-4 @xl:grid-cols-2 @4xl:grid-cols-3", className)}>
        {Array.from({ length: count }, (_, i) => (
          <div key={i} className="rounded-xl border border-zinc-200 p-4 dark:border-zinc-800">
            <div className="flex items-center gap-3">
              <Skeleton className="h-9 w-9 rounded-lg" />
              <Skeleton className="h-4 w-36" />
            </div>
            <Skeleton className="mt-3 h-3 w-full" />
            <Skeleton className="mt-1.5 h-3 w-2/3" />
            <Skeleton className="mt-4 h-3 w-40" />
          </div>
        ))}
      </div>
    </Loading>
  )
}

/** Lines of a list or a table body. */
export function RowsSkeleton({ rows = 5, label = "Loading…", className }: { rows?: number; label?: string; className?: string }) {
  return (
    <Loading label={label} className={className}>
      <div className="divide-y divide-zinc-100 dark:divide-zinc-800">
        {Array.from({ length: rows }, (_, i) => (
          <div key={i} className="flex items-center gap-3 py-3">
            <Skeleton className="h-5 w-20 rounded-full" />
            <Skeleton className="h-4 flex-1" style={{ maxWidth: `${60 - ((i * 7) % 25)}%` }} />
            <Skeleton className="ml-auto h-4 w-16" />
          </div>
        ))}
      </div>
    </Loading>
  )
}
