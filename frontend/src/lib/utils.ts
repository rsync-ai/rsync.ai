import { type ClassValue, clsx } from "clsx"
import { twMerge } from "tailwind-merge"

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

/** "Oct 3, 2026", or "—" for a missing or unparseable date (Intl throws on one). */
export function formatDate(date: Date | string | null | undefined): string {
  if (!date) return "—"
  const d = new Date(date)
  if (Number.isNaN(d.getTime())) return "—"
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    year: "numeric",
  }).format(d)
}

export function formatDateTime(date: Date | string): string {
  return new Intl.DateTimeFormat("en-US", {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(date))
}

export interface RelativeTimeOptions {
  /** Measure against this instant (ms since epoch) instead of the clock — e.g. the
   *  moment the rows were fetched, so the label matches the data it describes. */
  now?: number
  /** Count whole seconds under a minute ("12s ago") instead of saying "just now":
   *  for a liveness readout, where 3 s and 40 s mean different things. */
  seconds?: boolean
  /** What a missing or unparseable time reads as. Defaults to "—". */
  empty?: string
}

/**
 * How long ago, from an age already measured in milliseconds: "just now", "12m ago",
 * "5h ago", "3d ago". Every unit is floored — rounding made 15h40m read "16h ago" on
 * one panel and "15h ago" on the next (#56). A negative age (clock skew) clamps to 0.
 */
export function formatAgeMs(ms: number, opts: Pick<RelativeTimeOptions, "seconds"> = {}): string {
  const age = Math.max(0, ms)
  if (opts.seconds && age < 60_000) return `${Math.floor(age / 1000)}s ago`
  if (age < 45_000) return "just now"
  const mins = Math.floor(age / 60_000)
  if (mins < 60) return `${Math.max(1, mins)}m ago`
  const hrs = Math.floor(age / 3_600_000)
  if (hrs < 24) return `${hrs}h ago`
  return `${Math.floor(age / 86_400_000)}d ago`
}

/**
 * The one "how long ago" in the app. Eleven components each had their own, and they
 * disagreed at every boundary: "Just now" against "just now", 45 s against 60 s for
 * the first minute, 24 h against 48 h before switching to days, "" against "—" for a
 * missing time. They now all call this, with an option where a surface genuinely
 * needs something different.
 */
export function formatRelativeTime(
  date: Date | string | number | null | undefined,
  opts: RelativeTimeOptions = {},
): string {
  const empty = opts.empty ?? "—"
  if (date === null || date === undefined || date === "") return empty
  const then = new Date(date).getTime()
  if (Number.isNaN(then)) return empty
  return formatAgeMs((opts.now ?? Date.now()) - then, opts)
}

/**
 * An absolute time in the zone of whoever runs it, with the zone named ("Aug 15,
 * 10:00 AM GMT+5:30"). A relative string is easier to read at a glance but ambiguous
 * when it matters, and a bare wall-clock time is ambiguous about whose wall.
 *
 * Call it in the browser: on the server it formats in the server's zone (usually
 * UTC). A server-rendered page uses <LocalDateTime> instead. Returns "" for an
 * unparseable timestamp rather than "Invalid Date".
 */
export function formatAbsoluteTime(iso: string | null | undefined, opts: { zone?: boolean } = {}): string {
  if (!iso) return ""
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ""
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    // Left out only where the page names the zone once (a table's caption), so
    // each cell need not repeat it.
    ...(opts.zone === false ? {} : { timeZoneName: "short" as const }),
  })
}

/**
 * The viewer's own zone by its general name ("Central European Time"), for a page
 * that names it once. Not an offset: a list spans a daylight-saving change, and
 * "GMT+2" would be wrong for the rows before it. Call it in the browser.
 */
export function viewerTimeZone(): string {
  try {
    const id = Intl.DateTimeFormat().resolvedOptions().timeZone
    if (!id || id === "UTC" || id === "Etc/UTC") return "UTC"
    const name = new Intl.DateTimeFormat(undefined, { timeZoneName: "longGeneric" })
      .formatToParts(new Date())
      .find((p) => p.type === "timeZoneName")?.value
    return name || id
  } catch {
    return "UTC"
  }
}

