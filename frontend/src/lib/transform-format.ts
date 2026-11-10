// Counts for the transform-monitoring surface (rollup, config history, execution
// logs). Ages, durations and absolute times used to live here too, as copies of the
// app-wide ones that rendered the same moment differently; they now come from
// `@/lib/utils` (formatRelativeTime, formatAbsoluteTime) and `@/lib/duration`.

const numberFmt = new Intl.NumberFormat()

/** Locale-grouped integer, e.g. 100588 -> "100,588". */
export function formatCount(n: number | null | undefined): string {
  return numberFmt.format(Number(n ?? 0))
}
