import type { LucideIcon } from "lucide-react"

import { cn } from "@/lib/utils"

export interface FilterChip<T extends string> {
  value: T
  label: string
  /** Shown after the label; left out when undefined. */
  count?: number
  icon?: LucideIcon
}

/**
 * One row of rounded chips that narrow a list or pick a range: one is on at a time,
 * and each may carry its exact count. A group a screen reader names, each chip a
 * pressed or unpressed button.
 */
export function FilterChips<T extends string>({
  label,
  chips,
  value,
  onChange,
  className,
}: {
  label: string
  chips: FilterChip<T>[]
  value: T
  onChange: (next: T) => void
  className?: string
}) {
  return (
    <div role="group" aria-label={label} className={cn("flex flex-wrap items-center gap-1.5", className)}>
      {chips.map(({ value: v, label: text, count, icon: Icon }) => {
        const on = v === value
        return (
          <button
            key={v}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(v)}
            className={cn(
              "inline-flex h-7 items-center gap-1.5 rounded-full border px-3 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400",
              on
                ? "border-zinc-900 bg-zinc-900 text-white dark:border-white dark:bg-white dark:text-zinc-900"
                : "border-zinc-200 text-zinc-600 hover:bg-zinc-100 dark:border-zinc-800 dark:text-zinc-300 dark:hover:bg-zinc-800",
            )}
          >
            {Icon && <Icon className="h-3.5 w-3.5" aria-hidden />}
            {text}
            {count !== undefined && (
              <span
                className={cn(
                  "tabular-nums",
                  on ? "text-zinc-300 dark:text-zinc-600" : "text-zinc-500 dark:text-zinc-400",
                )}
              >
                {count}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}
