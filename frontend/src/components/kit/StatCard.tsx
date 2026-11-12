import Link from "next/link"
import type { LucideIcon } from "lucide-react"

import { Skeleton } from "@/components/ui/skeleton"
import { Sparkline, type SparklineColor } from "@/components/kit/Sparkline"
import { cn } from "@/lib/utils"

export interface StatFact {
  label: string
  value: number | undefined
  /** Red when non-zero: a count someone should act on. */
  alert?: boolean
}

const VALUE_TONE = {
  default: "text-zinc-900 dark:text-white",
  warn: "text-amber-700 dark:text-amber-300",
  bad: "text-red-700 dark:text-red-300",
} as const

/**
 * One KPI card: a title, the number, its facts and an optional trend. A value
 * that could not be read shows "—", never 0. The first paragraph is always the
 * number, so a reader (and a test) finds it the same way on every card.
 */
export function StatCard({
  title,
  value,
  href,
  icon: Icon,
  facts,
  hint,
  tone = "default",
  trend,
  trendLabel,
  trendColor,
  className,
}: {
  title: string
  value: number | string | undefined
  href?: string
  icon?: LucideIcon
  facts?: StatFact[]
  hint?: React.ReactNode
  tone?: keyof typeof VALUE_TONE
  /** Per-day values, oldest first; null while they load, so the card keeps its height. */
  trend?: readonly number[] | null
  trendLabel?: string
  trendColor?: SparklineColor
  className?: string
}) {
  return (
    <section
      aria-label={title}
      className={cn(
        "flex min-w-0 flex-col rounded-xl border border-zinc-200 bg-white p-4 shadow-sm dark:border-zinc-800 dark:bg-zinc-950",
        className,
      )}
    >
      <h2 className="flex items-center gap-2 text-sm font-medium text-zinc-600 dark:text-zinc-300">
        {Icon && <Icon className="h-4 w-4 shrink-0" aria-hidden />}
        {href ? (
          <Link href={href} className="truncate hover:underline">
            {title}
          </Link>
        ) : (
          <span className="truncate">{title}</span>
        )}
      </h2>
      <p className={cn("mt-1 text-3xl font-semibold tracking-tight tabular-nums", VALUE_TONE[tone])}>
        <StatValue value={value} />
      </p>
      {facts && facts.length > 0 && (
        <ul className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-sm">
          {facts.map((f) => (
            <li
              key={f.label}
              className={f.alert && f.value ? "text-red-700 dark:text-red-300" : "text-zinc-600 dark:text-zinc-300"}
            >
              <StatValue value={f.value} /> {f.label}
            </li>
          ))}
        </ul>
      )}
      {hint && <p className="mt-1 text-xs text-zinc-500 dark:text-zinc-400">{hint}</p>}
      {trend !== undefined && (
        <div className="mt-auto pt-3">
          {trend === null ? <Skeleton className="h-6 w-full" /> : <Sparkline values={trend} color={trendColor} />}
          {trendLabel && <p className="mt-1 text-[11px] text-zinc-500 dark:text-zinc-400">{trendLabel}</p>}
        </div>
      )}
    </section>
  )
}

function StatValue({ value }: { value: number | string | undefined }) {
  if (value === undefined) return <span title="Could not be loaded">—</span>
  return <>{typeof value === "number" ? value.toLocaleString("en-US") : value}</>
}
