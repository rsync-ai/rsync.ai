"use client"

import { useEffect, useState } from "react"
import Link from "next/link"
import { ArrowRight, Boxes, GitBranch, Table2 } from "lucide-react"
import { authFetch } from "@/lib/api/auth-fetch"
import {
  inputWakes,
  modelSurroundings,
  parseAssetGraph,
  type AssetNode,
  type ModelInput,
  type ModelSurroundings,
} from "@/components/explorer/assetLineage"

/**
 * One model's place in the asset graph, read once per dialog open. Silent on failure, like
 * the upstream suggestion: the strip is context for a form that works without it, and an
 * error about context would be noise in a dialog whose job is unaffected.
 */
export function useModelSurroundings(
  savedQueryId: string,
  connectionId: string | undefined,
  open: boolean,
): ModelSurroundings | null {
  const [found, setFound] = useState<ModelSurroundings | null>(null)
  useEffect(() => {
    if (!open) return
    let cancelled = false
    const query = connectionId ? `?connection_id=${encodeURIComponent(connectionId)}` : ""
    void (async () => {
      try {
        const res = await authFetch(`/api/v1/explorer/asset-graph${query}`, { cache: "no-store" })
        if (cancelled || !res.ok) return
        const graph = parseAssetGraph(await res.json())
        if (!cancelled) setFound(modelSurroundings(graph, `model:${savedQueryId}`))
      } catch {
        // Deliberately silent — see above.
      }
    })()
    return () => {
      cancelled = true
    }
  }, [open, savedQueryId, connectionId])
  return found
}

/** How the form says this model runs, which decides what the strip says about its inputs. */
export type ModelWhen = "inputs" | "clock" | "hand"

const SHOWN_INPUTS = 4
const SHOWN_READERS = 3

/** An input by what makes it: the one pipeline or model that produces it, else the table. */
function inputLabel(input: ModelInput): { label: string; producer: AssetNode | null } {
  return input.producers.length === 1
    ? { label: input.producers[0].name, producer: input.producers[0] }
    : { label: input.table.name, producer: null }
}

function KindGlyph({ kind }: { kind: string }) {
  const Icon = kind === "pipeline" ? GitBranch : kind === "model" ? Boxes : Table2
  return <Icon aria-hidden className="h-3 w-3 shrink-0 text-zinc-500 dark:text-zinc-400" />
}

function wakeNote(inputs: ModelInput[], waking: ReadonlySet<string>, when: ModelWhen): string {
  if (inputs.length === 0) return "Its SQL names no table a pipeline or model here produces."
  if (when === "hand") return "It runs only when someone presses Run now, so its inputs never wake it."
  if (when === "clock") return "It runs on a clock, so it can read an input before or after that input refreshes."
  const woken = inputs.filter((i) => inputWakes(i, waking)).length
  if (woken === inputs.length) {
    return inputs.length === 1
      ? "Its input wakes this model."
      : inputs.length === 2
        ? "Both inputs wake this model."
        : `All ${inputs.length} inputs wake this model.`
  }
  return `${woken} of ${inputs.length} inputs wake this model. The rest can change without it rebuilding.`
}

/**
 * "Where it sits": what this model reads, what it builds and what reads that, as one line.
 * An input it is set to follow is drawn solid in the model colour, one it is not dashed, so
 * the effect of ticking a box is visible before the save.
 */
export function ModelPlacement({
  surroundings,
  waking,
  when,
  plannedTable,
  savedQueryId,
}: {
  surroundings: ModelSurroundings
  /** Node ids of the producers whose runs wake this model in the form as it stands. */
  waking: ReadonlySet<string>
  when: ModelWhen
  /** The table the form would build, when the graph does not show one built yet. */
  plannedTable?: string
  savedQueryId: string
}) {
  const { inputs, builds, readers } = surroundings
  const tables = builds.length > 0 ? builds.map((b) => b.name) : plannedTable ? [plannedTable] : []
  const shownInputs = inputs.slice(0, SHOWN_INPUTS)
  const shownReaders = readers.slice(0, SHOWN_READERS)
  return (
    <section
      aria-label="Where it sits"
      className="space-y-2 rounded-lg border border-zinc-200 bg-zinc-50 p-3 dark:border-zinc-800 dark:bg-zinc-900/50"
    >
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-[11px] font-semibold uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
          Where it sits
        </h3>
        <Link
          href={`/explorer/lineage?focus=${encodeURIComponent(`model:${savedQueryId}`)}`}
          className="text-xs font-medium text-violet-700 hover:underline dark:text-violet-300"
        >
          Open in Lineage
        </Link>
      </div>
      <div className="flex items-center gap-2 overflow-x-auto pb-0.5 text-xs">
        {inputs.length > 0 && (
          <>
            <ul aria-label="Reads from" className="flex shrink-0 flex-col gap-1">
              {shownInputs.map((input) => {
                const { label, producer } = inputLabel(input)
                const wakes = when === "inputs" && inputWakes(input, waking)
                return (
                  <li
                    key={input.table.id}
                    title={`${input.table.name}${wakes ? " · wakes this model" : " · doesn't wake this model"}`}
                    data-wakes={wakes ? "true" : "false"}
                    className={`inline-flex max-w-[180px] items-center gap-1.5 rounded-md border bg-white px-2 py-1 dark:bg-zinc-950 ${
                      wakes ? "border-violet-500/70" : "border-dashed border-zinc-300 dark:border-zinc-700"
                    }`}
                  >
                    <KindGlyph kind={producer?.kind ?? "table"} />
                    <span className="truncate">{label}</span>
                  </li>
                )
              })}
              {inputs.length > SHOWN_INPUTS && (
                <li className="px-2 text-zinc-500 dark:text-zinc-400">+{inputs.length - SHOWN_INPUTS} more</li>
              )}
            </ul>
            <ArrowRight aria-hidden className="h-3.5 w-3.5 shrink-0 text-zinc-400" />
          </>
        )}
        <span className="inline-flex max-w-[200px] shrink-0 items-center gap-1.5 rounded-md border-[1.5px] border-violet-500 bg-white px-2 py-1 font-semibold dark:bg-zinc-950">
          <KindGlyph kind="model" />
          <span className="truncate">{surroundings.model.name}</span>
        </span>
        {tables.length > 0 && (
          <>
            <ArrowRight aria-hidden className="h-3.5 w-3.5 shrink-0 text-zinc-400" />
            <span
              className={`inline-flex max-w-[220px] shrink-0 items-center gap-1.5 rounded-md px-2 py-1 font-mono text-[11px] ${
                builds.length > 0 ? "bg-zinc-200/70 dark:bg-zinc-800" : "border border-dashed border-zinc-300 dark:border-zinc-700"
              }`}
              title={builds.length > 0 ? "The table it builds" : "The table it will build once saved"}
            >
              <KindGlyph kind="table" />
              <span className="truncate">{tables.join(", ")}</span>
            </span>
            <ArrowRight aria-hidden className="h-3.5 w-3.5 shrink-0 text-zinc-300 dark:text-zinc-600" />
            {readers.length === 0 ? (
              <span className="shrink-0 whitespace-nowrap text-zinc-500 dark:text-zinc-400">Read by nothing yet</span>
            ) : (
              <span className="shrink-0 whitespace-nowrap text-zinc-600 dark:text-zinc-300">
                Read by {shownReaders.map((r) => r.name).join(", ")}
                {readers.length > SHOWN_READERS && ` +${readers.length - SHOWN_READERS} more`}
              </span>
            )}
          </>
        )}
      </div>
      <p className={`text-xs ${when === "inputs" ? "text-violet-700 dark:text-violet-300" : "text-zinc-500 dark:text-zinc-400"}`}>
        {wakeNote(inputs, waking, when)}
      </p>
    </section>
  )
}
