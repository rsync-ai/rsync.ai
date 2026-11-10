import type { ReactNode } from "react"

/** One label and value of a Details card's <dl>. */
export function DetailRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[96px_minmax(0,1fr)] gap-2 py-1.5 text-xs">
      <dt className="text-zinc-500 dark:text-zinc-400">{label}</dt>
      <dd className="min-w-0 text-zinc-800 dark:text-zinc-200">{children}</dd>
    </div>
  )
}
