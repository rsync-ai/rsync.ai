"use client"

// The Lineage row's popup: the pipelines and models chained to a model, each with the
// tables it writes, under headings that say what upstream and downstream mean. The row's
// count opens it, as the counts do in Databricks' "Tables related to this job"; the
// graph is one button away.

import { useEffect, useState } from "react"
import Link from "next/link"
import { Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { getJson } from "@/components/explorer/getJson"
import { SCHEDULE_LIST_LIMIT, runPool, type LineageNode, type ModelLineage } from "@/components/explorer/modelLineage"
import {
  DOWNSTREAM_MEANING,
  UPSTREAM_MEANING,
  describeLinkedNode,
  linkedLookupFromResponse,
  linkedLookupUrl,
  needsLinkedLookup,
  type LinkedLookup,
} from "@/components/explorer/lineageLinks"
import { cn } from "@/lib/utils"

/** Lookups in flight at once, as the graph does. */
const LOOKUP_CONCURRENCY = 6
const LOOKUP_TIMEOUT_MS = 10_000

async function lookupLinked(node: LineageNode, signal: AbortSignal): Promise<LinkedLookup> {
  try {
    const { status, body } = await getJson(linkedLookupUrl(node), signal, LOOKUP_TIMEOUT_MS)
    return linkedLookupFromResponse(node.kind, status, body)
  } catch (err) {
    // The popup closed: nothing is shown for this load.
    if (signal.aborted) throw err
    // A timeout or a network error says nothing about the node.
    return { status: "unavailable" }
  }
}

export function LinkedModelsDialog({
  open,
  onOpenChange,
  lineage,
  capped,
  onOpenGraph,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** The walk the Lineage row counted, so the popup lists what the count counted. */
  lineage: ModelLineage
  /** The schedule list came back full, so there may be links it could not see. */
  capped: boolean
  onOpenGraph: () => void
}) {
  const [loaded, setLoaded] = useState<{ lineage: ModelLineage; lookups: Map<string, LinkedLookup> } | null>(null)

  // Asked again on each open, so closing and reopening is the retry.
  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    let cancelled = false
    ;(async () => {
      const lookups = new Map<string, LinkedLookup>()
      try {
        await runPool(lineage.nodes.filter(needsLinkedLookup), LOOKUP_CONCURRENCY, async (node) => {
          lookups.set(node.key, await lookupLinked(node, controller.signal))
        })
      } catch {
        return
      }
      // Set once every lookup is in, as the graph does: a name only shows once its own
      // route has answered.
      if (!cancelled) setLoaded({ lineage, lookups })
    })()
    return () => {
      cancelled = true
      controller.abort()
    }
  }, [open, lineage])

  const lookups = loaded?.lineage === lineage ? loaded.lookups : null
  const byDistance = (a: LineageNode, b: LineageNode) => a.distance - b.distance
  const sections = [
    {
      label: "Upstream",
      meaning: UPSTREAM_MEANING,
      nodes: lineage.nodes.filter((n) => n.role === "upstream").sort(byDistance),
      found: lineage.upstreamsFound,
    },
    {
      label: "Downstream",
      meaning: DOWNSTREAM_MEANING,
      nodes: lineage.nodes.filter((n) => n.role === "downstream").sort(byDistance),
      found: lineage.downstreamsFound,
    },
  ]

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] max-w-2xl overflow-y-auto">
        <DialogHeader>
          <DialogTitle>Linked to this model</DialogTitle>
          <DialogDescription>
            These links come from schedules set to &ldquo;After a pipeline or model runs&rdquo;.
            {capped && ` Counted from the first ${SCHEDULE_LIST_LIMIT} schedules in this workspace, so there may be more.`}
          </DialogDescription>
        </DialogHeader>

        {!lookups ? (
          <p role="status" className="flex items-center gap-2 py-4 text-sm text-zinc-500 dark:text-zinc-400">
            <Loader2 className="h-4 w-4 motion-safe:animate-spin" aria-hidden />
            Loading names and tables…
          </p>
        ) : (
          <div className="space-y-5 py-2">
            {sections.map((s) => {
              const count = s.found > s.nodes.length ? `${s.nodes.length} of ${s.found}` : `${s.nodes.length}`
              const meaning = s.meaning.charAt(0).toLowerCase() + s.meaning.slice(1)
              return (
                <section key={s.label}>
                  <h3 className="mb-2 text-sm font-medium text-zinc-900 dark:text-white">
                    {s.label}: {meaning} ({count})
                  </h3>
                  {s.nodes.length === 0 ? (
                    <p className="text-xs text-zinc-500 dark:text-zinc-400">None.</p>
                  ) : (
                    <ul className="divide-y rounded-md border dark:divide-zinc-800 dark:border-zinc-800">
                      {s.nodes.map((n) => {
                        const v = describeLinkedNode(n, lookups.get(n.key))
                        return (
                          <li key={v.key} className="px-3 py-2">
                            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
                              <span className="text-[10px] uppercase tracking-wide text-zinc-500 dark:text-zinc-400">
                                {v.kindLabel}
                              </span>
                              {v.href ? (
                                <Link
                                  href={v.href}
                                  onClick={() => onOpenChange(false)}
                                  className="break-all text-sm font-medium text-blue-600 hover:underline dark:text-blue-400"
                                >
                                  {v.title}
                                </Link>
                              ) : (
                                <span
                                  className={cn(
                                    "text-sm font-medium",
                                    v.named ? "text-zinc-900 dark:text-white" : "italic text-zinc-500 dark:text-zinc-400",
                                  )}
                                >
                                  {v.title}
                                </span>
                              )}
                              <span className="text-xs text-zinc-500 dark:text-zinc-400">{v.distance}</span>
                            </div>
                            {v.detail && <p className="break-words text-xs text-zinc-600 dark:text-zinc-300">{v.detail}</p>}
                          </li>
                        )
                      })}
                    </ul>
                  )}
                </section>
              )
            })}
          </div>
        )}

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => {
              onOpenChange(false)
              onOpenGraph()
            }}
          >
            Open the graph
          </Button>
          <Button onClick={() => onOpenChange(false)}>Close</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
