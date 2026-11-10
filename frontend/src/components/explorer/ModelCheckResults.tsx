"use client"

// What a model's data checks found: on one run (the Runs tab and the Graph tab's run
// panel) or on Test now (the Checks tab). The count each check saw is shown to the person
// only; it is stored for them and never reaches an LLM-bound tool.

import { useEffect, useState } from "react"
import { Ban, Loader2 } from "lucide-react"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { getJson } from "@/components/explorer/getJson"
import {
  CHECK_STATUS_LABEL,
  checkTypeLabel,
  describeObserved,
  type CheckResult,
  type CheckStatus,
} from "@/components/explorer/modelChecks"

const RESULTS_TIMEOUT_MS = 10_000

export function CheckBlockedBadge() {
  return (
    <Badge
      variant="outline"
      className="gap-1 border-red-500 text-red-600 dark:text-red-400"
      title="A blocking data check failed, so the live table was left as it was"
    >
      <Ban className="h-3 w-3" aria-hidden />
      Blocked by check
    </Badge>
  )
}

export function checkStatusBadge(status: CheckStatus | string) {
  const label = CHECK_STATUS_LABEL[status as CheckStatus] ?? status
  const cls =
    status === "pass"
      ? "border-emerald-500 text-emerald-600"
      : status === "fail"
        ? "border-red-500 text-red-600"
        : status === "error"
          ? "border-amber-500 text-amber-600"
          : "text-zinc-500 dark:text-zinc-400"
  return (
    <Badge variant="outline" className={cls}>
      {label}
    </Badge>
  )
}

/**
 * Each check's result: name, outcome, what it counted against what it allows. With
 * onSelect, a saved check's result is a button that opens its card.
 */
export function CheckResultList({
  results,
  onSelect,
}: {
  results: CheckResult[]
  onSelect?: (checkId: string) => void
}) {
  return (
    <ul className="space-y-1.5" aria-label="Check results">
      {results.map((r, i) => {
        const observed = describeObserved(r)
        // Spans, not a div and a p: the same content sits inside a button.
        const content = (
          <>
            <span className="flex flex-wrap items-center gap-2">
              {checkStatusBadge(r.status)}
              <span className="min-w-0 break-words font-medium">{r.name}</span>
              <span className="text-zinc-500 dark:text-zinc-400">
                {checkTypeLabel(r.type)} · {r.severity === "block" ? "blocks" : "warns"}
              </span>
            </span>
            {(observed || r.threshold || r.message) && (
              <span className="mt-0.5 block break-words text-zinc-600 dark:text-zinc-300">
                {observed && <span>Found {observed}</span>}
                {observed && r.threshold && <span> · </span>}
                {r.threshold && <span>allowed: {r.threshold}</span>}
                {r.message && (
                  <span className={r.status === "error" ? "block text-amber-700 dark:text-amber-400" : "block"}>
                    {r.message}
                  </span>
                )}
              </span>
            )}
          </>
        )
        const checkId = r.check_id
        return (
          <li key={checkId ?? `${r.name}-${i}`} className="rounded border text-xs dark:border-zinc-800">
            {onSelect && checkId ? (
              <button
                type="button"
                className="w-full rounded px-2 py-1.5 text-left hover:bg-zinc-50 focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500 dark:hover:bg-zinc-800/60"
                title={`Show ${r.name} in the list below`}
                onClick={() => onSelect(checkId)}
              >
                {content}
              </button>
            ) : (
              <div className="px-2 py-1.5">{content}</div>
            )}
          </li>
        )
      })}
    </ul>
  )
}

type RunChecksState =
  | { status: "loading" }
  | { status: "error"; message: string }
  | { status: "ok"; results: CheckResult[] }

/** One run's check results, from GET …/runs/:runId/checks. Renders nothing for a run without any. */
export function RunCheckResults({ modelId, runId, blocked }: { modelId: string; runId: string; blocked: boolean }) {
  const [retryTick, setRetryTick] = useState(0)
  const loadKey = `${modelId}|${runId}|${retryTick}`
  const [loaded, setLoaded] = useState<{ key: string; state: RunChecksState } | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    ;(async () => {
      let next: RunChecksState
      try {
        const res = await getJson(
          `/api/v1/explorer/saved/${encodeURIComponent(modelId)}/runs/${encodeURIComponent(runId)}/checks`,
          controller.signal,
          RESULTS_TIMEOUT_MS,
        )
        const body = res.body as { results?: CheckResult[] } | null
        if (!res.ok) next = { status: "error", message: `Could not load the check results (HTTP ${res.status}).` }
        else next = { status: "ok", results: Array.isArray(body?.results) ? body.results : [] }
      } catch {
        if (controller.signal.aborted) return
        next = { status: "error", message: "Could not reach the server to load the check results." }
      }
      if (!controller.signal.aborted) setLoaded({ key: loadKey, state: next })
    })()
    return () => controller.abort()
  }, [modelId, runId, loadKey])

  const state: RunChecksState = loaded?.key === loadKey ? loaded.state : { status: "loading" }
  // A run without checks says nothing; a blocked one always says why, or that it could not.
  if (state.status === "ok" && state.results.length === 0 && !blocked) return null
  return (
    <section className="space-y-1">
      <h3 className="text-xs font-medium uppercase tracking-wide text-zinc-500 dark:text-zinc-400">Data checks</h3>
      {state.status === "loading" ? (
        <p className="inline-flex items-center gap-1 text-xs text-zinc-500 dark:text-zinc-400">
          <Loader2 className="h-3 w-3 animate-spin" aria-hidden /> Loading the check results…
        </p>
      ) : state.status === "error" ? (
        <div className="flex flex-wrap items-center gap-2 text-xs text-red-600 dark:text-red-400">
          {state.message}
          {/* Named apart from the SQL's Retry beside it in the run panel. */}
          <Button
            size="sm"
            variant="outline"
            className="h-6 px-2 text-xs"
            aria-label="Retry loading the check results"
            onClick={() => setRetryTick((t) => t + 1)}
          >
            Retry
          </Button>
        </div>
      ) : state.results.length === 0 ? (
        <p className="text-xs text-zinc-500 dark:text-zinc-400">No check results were recorded for this run.</p>
      ) : (
        <CheckResultList results={state.results} />
      )}
    </section>
  )
}
