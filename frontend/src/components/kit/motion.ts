/** Items after the sixth arrive together, so a long grid never waits seconds for its last card. */
export const MAX_STAGGERED = 6

/** Entrance delay for the item at `index` in a staggered list, in seconds. */
export function staggerDelay(index: number, step = 0.05): number {
  return Math.min(Math.max(index, 0), MAX_STAGGERED) * step
}
