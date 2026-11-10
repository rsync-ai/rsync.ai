import { cn } from "@/lib/utils"
import type { ActivityStatus } from "@/lib/api/activity"

/**
 * The colour of a status. Run states: ok succeeded, fail failed, err error, wait
 * waiting for you, run running, cancel cancelled, skip skipped (grey). Object
 * states reuse them; vio is a draft.
 */
export type PillTone = "ok" | "fail" | "err" | "wait" | "run" | "cancel" | "skip" | "vio"

// dot -600/-400, text -700/-300 (AA on the -50 wash), wash -50/-400/10.
const TONES: Record<PillTone, { pill: string; dot: string }> = {
  ok: {
    pill: "bg-emerald-50 text-emerald-700 dark:bg-emerald-400/10 dark:text-emerald-300",
    dot: "bg-emerald-600 dark:bg-emerald-400",
  },
  fail: {
    pill: "bg-red-50 text-red-700 dark:bg-red-400/10 dark:text-red-300",
    dot: "bg-red-600 dark:bg-red-400",
  },
  err: {
    pill: "bg-orange-50 text-orange-700 dark:bg-orange-400/10 dark:text-orange-300",
    dot: "bg-orange-600 dark:bg-orange-400",
  },
  wait: {
    pill: "bg-amber-50 text-amber-700 dark:bg-amber-400/10 dark:text-amber-300",
    dot: "bg-amber-600 dark:bg-amber-400",
  },
  run: {
    pill: "bg-blue-50 text-blue-700 dark:bg-blue-400/10 dark:text-blue-300",
    dot: "bg-blue-600 dark:bg-blue-400",
  },
  cancel: {
    pill: "bg-zinc-100 text-zinc-700 dark:bg-zinc-400/10 dark:text-zinc-300",
    dot: "bg-zinc-500 dark:bg-zinc-400",
  },
  skip: {
    pill: "bg-zinc-50 text-zinc-600 dark:bg-zinc-400/10 dark:text-zinc-400",
    dot: "bg-zinc-400 dark:bg-zinc-500",
  },
  vio: {
    pill: "bg-violet-50 text-violet-700 dark:bg-violet-400/10 dark:text-violet-300",
    dot: "bg-violet-600 dark:bg-violet-400",
  },
}

/** A tone's wash and text, for a pill that is not a status (a scorecard's change). */
export function pillToneClass(tone: PillTone): string {
  return TONES[tone].pill
}

/** The tone of each shared run status (lib/api/activity.ts). */
export const ACTIVITY_STATUS_TONES: Record<ActivityStatus, PillTone> = {
  succeeded: "ok",
  failed: "fail",
  waiting: "wait",
  running: "run",
  cancelled: "cancel",
  skipped: "skip",
}

/** A status's dot alone, beside a name that a StatusPill elsewhere on the row spells out. */
export function StatusDot({ tone, className }: { tone: PillTone; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn("h-1.5 w-1.5 shrink-0 rounded-full", TONES[tone].dot, tone === "run" && "motion-safe:animate-pulse", className)}
    />
  )
}

/** One pill for every status in the app: a dot and the status word. */
export function StatusPill({
  tone,
  children,
  title,
  className,
  "data-testid": testId,
}: {
  tone: PillTone
  children: React.ReactNode
  title?: string
  className?: string
  "data-testid"?: string
}) {
  return (
    <span
      title={title}
      data-testid={testId}
      className={cn(
        "inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full px-2 py-0.5 text-xs font-medium",
        TONES[tone].pill,
        className,
      )}
    >
      <StatusDot tone={tone} />
      {children}
    </span>
  )
}
