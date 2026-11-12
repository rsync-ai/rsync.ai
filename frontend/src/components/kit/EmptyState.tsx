import type { LucideIcon } from "lucide-react"

import { cn } from "@/lib/utils"

/**
 * What a list, card or chart shows when it has nothing: an icon, a title, one
 * sentence and the next action. md fills a page section; sm sits in a card.
 */
export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  size = "md",
  className,
}: {
  icon?: LucideIcon
  title: string
  description?: React.ReactNode
  action?: React.ReactNode
  size?: "md" | "sm"
  className?: string
}) {
  const sm = size === "sm"
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center text-center",
        sm ? "gap-1.5 px-4 py-6" : "gap-2 px-6 py-12",
        className,
      )}
    >
      {Icon && (
        <span
          aria-hidden
          className={cn(
            "mb-1 flex items-center justify-center rounded-full bg-zinc-100 text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400",
            sm ? "h-8 w-8" : "h-11 w-11",
          )}
        >
          <Icon className={sm ? "h-4 w-4" : "h-5 w-5"} />
        </span>
      )}
      <p className={cn("font-medium text-zinc-900 dark:text-zinc-100", sm ? "text-sm" : "text-base")}>{title}</p>
      {description && (
        <p className={cn("max-w-sm text-zinc-500 dark:text-zinc-400", sm ? "text-xs" : "text-sm")}>{description}</p>
      )}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}
