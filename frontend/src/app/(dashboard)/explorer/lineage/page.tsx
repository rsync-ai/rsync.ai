"use client"

import { Suspense, useState } from "react"
import { useSearchParams } from "next/navigation"
import { RefreshCw } from "lucide-react"
import { Button } from "@/components/ui/button"
import { AssetLineageView } from "@/components/explorer/AssetLineageView"
import { OpenLineageButton } from "@/components/explorer/OpenLineageSettings"

// One line in a compact toolbar, so it truncates; `title` keeps the whole of it.
const BLURB =
  "Which pipelines write which tables, which models read them, and which models are refreshed out of step with what they read."

export default function LineagePage() {
  const [reloadTick, setReloadTick] = useState(0)
  return (
    <div className="flex flex-col">
      {/* The same toolbar as the Data Explorer: the graph below is the page. */}
      <header className="flex h-11 shrink-0 items-center gap-3 border-b border-zinc-200 bg-white px-4 dark:border-zinc-800 dark:bg-zinc-950">
        <h1 className="shrink-0 text-sm font-semibold text-zinc-900 dark:text-white">Lineage</h1>
        <p className="hidden min-w-0 truncate text-xs text-zinc-500 @xl:block dark:text-zinc-400" title={BLURB}>
          {BLURB}
        </p>
        {/* The OpenLineage export's one mount point: an admin's only (OpenLineageSettings.tsx). */}
        <div className="ml-auto flex shrink-0 items-center gap-2">
          <OpenLineageButton />
          <Button variant="outline" size="sm" className="h-7 shrink-0" onClick={() => setReloadTick((t) => t + 1)}>
            <RefreshCw className="mr-1.5 h-3.5 w-3.5" />
            Refresh
          </Button>
        </div>
      </header>
      <div className="p-4">
        {/* useSearchParams needs a Suspense boundary above it (workflows/[id]/page.tsx says why). */}
        <Suspense fallback={null}>
          <FocusedLineage reloadTick={reloadTick} />
        </Suspense>
      </div>
    </div>
  )
}

/** ?focus=<asset id> opens the graph focused on that asset, as a chart's lineage badge links it. */
function FocusedLineage({ reloadTick }: { reloadTick: number }) {
  const focus = useSearchParams()?.get("focus")
  return <AssetLineageView key={focus ?? ""} reloadTick={reloadTick} initialFocus={focus} />
}
