import type { FilterChip } from "@/components/kit/FilterChips"
import { StatusPill } from "@/components/kit/StatusPill"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"

/**
 * The parts of a card in a grid of saved things (dashboards, charts): who sees it,
 * whether Ask rsync.ai or an agent drafted it, and when it last changed. A grid
 * that filters by owner (Dashboards) does it with ownerChips and ownerMatches.
 */

/** A whole card as one link: lifts its border on hover, keeps its footer at the bottom. */
export const LIST_CARD_CLASS =
  "flex w-full flex-col rounded-xl border border-zinc-200 bg-white p-4 transition-colors hover:border-zinc-300 hover:bg-zinc-50/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-zinc-400 dark:border-zinc-800 dark:bg-zinc-950 dark:hover:border-zinc-700 dark:hover:bg-zinc-900/60"

/** Who sees it, and "Drafted" when Ask rsync.ai or an agent made it. */
export function ListCardPills({ visibility, proposedBy }: { visibility: string; proposedBy: string }) {
  const drafted = proposedBy !== "" && proposedBy !== "none"
  return (
    <div className="mt-1 flex flex-wrap items-center gap-1.5">
      <StatusPill tone="skip">{visibility === "workspace" ? "Workspace" : "Only me"}</StatusPill>
      {drafted && (
        <StatusPill tone="vio" title={`Drafted by ${proposedBy === "agent" ? "an agent" : "Ask rsync.ai"}`}>
          Drafted
        </StatusPill>
      )}
    </div>
  )
}

/** The card's last line: what it holds, then when it last changed (the exact time on hover). */
export function ListCardMeta({ updatedAt, children }: { updatedAt: string; children?: React.ReactNode }) {
  return (
    <p className="mt-auto pt-3 text-xs text-zinc-500 dark:text-zinc-400">
      {children}
      {children ? " · " : null}
      <span title={formatAbsoluteTime(updatedAt)}>updated {formatRelativeTime(updatedAt)}</span>
    </p>
  )
}

export type OwnerFilter = "all" | "mine" | "shared"

/** All, the ones you made, and the ones others shared with the workspace, each with its count. */
export function ownerChips(items: { mine: boolean }[]): FilterChip<OwnerFilter>[] {
  const mine = items.filter((i) => i.mine).length
  return [
    { value: "all", label: "All", count: items.length },
    { value: "mine", label: "Mine", count: mine },
    { value: "shared", label: "Shared", count: items.length - mine },
  ]
}

export function ownerMatches(item: { mine: boolean }, owner: OwnerFilter): boolean {
  return owner === "all" || (owner === "mine") === item.mine
}
