"use client"

import type { LucideIcon } from "lucide-react"

import { EmptyState } from "@/components/kit/EmptyState"
import { Skeleton } from "@/components/ui/skeleton"
import { cn } from "@/lib/utils"

/**
 * The frame round one chart: its title (the figure's caption), a one-line
 * description, then the chart, or a small empty state when the window has
 * nothing to draw. The caption is the title alone, so a figure is named by it.
 * An `action` (a window switch) is drawn top right but comes last in the
 * figure, so the caption stays its first child and its name.
 */
export function ChartCard({
  title,
  description,
  empty,
  emptyIcon,
  loading,
  action,
  children,
  className,
}: {
  title: string
  description?: string
  /** Set when there is nothing to draw: the card says this instead. */
  empty?: string
  emptyIcon?: LucideIcon
  loading?: boolean
  /** A control beside the title, such as the 7d / 30d switch. */
  action?: React.ReactNode
  children?: React.ReactNode
  className?: string
}) {
  return (
    <figure
      className={cn(
        "relative flex min-w-0 flex-col rounded-xl border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950",
        className,
      )}
    >
      <figcaption className={cn("text-sm font-medium text-zinc-900 dark:text-zinc-100", action && "pr-24")}>
        {title}
      </figcaption>
      {description && (
        <p className={cn("mt-0.5 text-xs text-zinc-500 dark:text-zinc-400", action && "pr-24")}>{description}</p>
      )}
      <div className="mt-3 min-w-0">
        {loading ? (
          <Skeleton className="h-[200px] w-full" />
        ) : empty ? (
          <EmptyState size="sm" icon={emptyIcon} title={empty} className="h-[200px]" />
        ) : (
          children
        )}
      </div>
      {action && <div className="absolute right-3 top-3">{action}</div>}
    </figure>
  )
}

/**
 * The 7d / 30d switch for a set of charts. Short on screen, the full window
 * ("Last 30 days") for a screen reader.
 */
export function WindowToggle<T extends number>({
  value,
  options,
  onChange,
  className,
}: {
  value: T
  options: readonly T[]
  onChange: (next: T) => void
  className?: string
}) {
  return (
    <div
      role="group"
      aria-label="Chart window"
      className={cn(
        "inline-flex rounded-lg border border-zinc-200 bg-zinc-50 p-0.5 dark:border-zinc-800 dark:bg-zinc-900",
        className,
      )}
    >
      {options.map((n) => {
        const pressed = n === value
        return (
          <button
            key={n}
            type="button"
            aria-pressed={pressed}
            // Starts with the visible "7d" (WCAG 2.5.3), then says it in full.
            aria-label={`${n}d, last ${n} days`}
            onClick={() => onChange(n)}
            className={cn(
              "rounded-md px-2.5 py-1 text-xs font-medium tabular-nums transition-colors",
              pressed
                ? "bg-white text-zinc-900 shadow-sm dark:bg-zinc-700 dark:text-white"
                : "text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-white",
            )}
          >
            {n}d
          </button>
        )
      })}
    </div>
  )
}
