import { cn } from "@/lib/utils"

/**
 * A model's layer is a name from the workspace's own list (Silver, Gold, Marts…),
 * which owners and admins make in Workspace settings (migration 135). "" is a model
 * in no layer. A table a pipeline writes is not in a layer: it is a pipeline table.
 *
 * Shared by the model dialog, the Models lists and the lineage graph, so all of
 * them name and colour a layer the same way: by its place in the workspace's order.
 */

/** Where a layer sits in the workspace's order, or undefined when the list does not have it. */
export function layerPosition(
  layers: { name: string; position: number }[] | null | undefined,
  name: string | undefined | null,
): number | undefined {
  const key = (name ?? "").trim().toLowerCase()
  if (!key || !layers) return undefined
  return layers.find((l) => l.name.toLowerCase() === key)?.position
}

// One colour per place in the order, so the first two layers (Silver and Gold,
// for a workspace that had them before layers were its own) keep their colours.
const LAYER_PALETTE = [
  "border-zinc-300 bg-zinc-100 text-zinc-700 dark:border-zinc-600 dark:bg-zinc-800 dark:text-zinc-200",
  "border-amber-300 bg-amber-50 text-amber-800 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-300",
  "border-sky-300 bg-sky-50 text-sky-800 dark:border-sky-700 dark:bg-sky-950/40 dark:text-sky-300",
  "border-violet-300 bg-violet-50 text-violet-800 dark:border-violet-700 dark:bg-violet-950/40 dark:text-violet-300",
  "border-emerald-300 bg-emerald-50 text-emerald-800 dark:border-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300",
  "border-rose-300 bg-rose-50 text-rose-800 dark:border-rose-700 dark:bg-rose-950/40 dark:text-rose-300",
]
const LAYER_UNPLACED =
  "border-zinc-200 bg-white text-zinc-600 dark:border-zinc-700 dark:bg-zinc-900 dark:text-zinc-300"

const BADGE = "inline-flex shrink-0 items-center rounded-full border px-1.5 text-[10px] font-medium uppercase leading-4 tracking-wide"

/**
 * Renders nothing for a model in no layer. Without a position (the layer list has
 * not loaded, or failed) the badge still names the layer, in a neutral colour.
 */
export function LayerBadge({
  layer,
  position,
  className,
}: {
  layer: string | undefined | null
  position?: number
  className?: string
}) {
  const name = (layer ?? "").trim()
  if (!name) return null
  const style = position === undefined ? LAYER_UNPLACED : LAYER_PALETTE[position % LAYER_PALETTE.length]
  return (
    <span data-layer={name} className={cn(BADGE, style, className)}>
      {name}
    </span>
  )
}

/** Marks a table a pipeline writes, which every model is built on. */
export function PipelineTableBadge({ className }: { className?: string }) {
  return (
    <span
      data-pipeline-table=""
      className={cn(
        BADGE,
        "border-orange-300 bg-orange-50 text-orange-800 dark:border-orange-800 dark:bg-orange-950/40 dark:text-orange-300",
        className,
      )}
    >
      Pipeline table
    </span>
  )
}

/**
 * The layer filter: "all" shows every model, "none" the models in no layer, and
 * "layer:<name>" one layer's. The prefix keeps a layer a workspace named "none"
 * from meaning no layer.
 */
export type LayerFilter = "all" | "none" | `layer:${string}`

export function layerFilterFor(name: string): LayerFilter {
  return `layer:${name}`
}

export function matchesLayer(raw: string | undefined | null, filter: LayerFilter): boolean {
  if (filter === "all") return true
  const name = (raw ?? "").trim().toLowerCase()
  if (filter === "none") return name === ""
  return name === filter.slice("layer:".length).trim().toLowerCase()
}
