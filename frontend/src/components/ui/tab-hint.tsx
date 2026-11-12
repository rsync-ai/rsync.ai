import { cn } from "@/lib/utils"

/**
 * Says that Tab writes the box's suggestion in. Shown beside an AI prompt box
 * only while useSuggestionTab would take the key (the box is empty and has a
 * suggestion), so it never promises a Tab that would move focus instead.
 */
export function TabHint({ className }: { className?: string }) {
  return (
    <span className={cn("inline-flex items-center gap-1 text-xs text-zinc-500 dark:text-zinc-400", className)}>
      <kbd className="rounded border border-zinc-300 bg-zinc-50 px-1.5 py-px font-sans text-[11px] font-medium text-zinc-600 dark:border-zinc-700 dark:bg-zinc-800 dark:text-zinc-300">
        Tab
      </kbd>
      to use the suggestion
    </span>
  )
}
