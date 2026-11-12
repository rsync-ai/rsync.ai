"use client"

import { useCallback, useRef, useState, type ReactNode } from "react"
import { useVisiblePoll } from "@/lib/hooks/useVisiblePoll"
import { AlertTriangle, HelpCircle, Loader2 } from "lucide-react"
import { authFetch } from "@/lib/api/auth-fetch"
import { Badge } from "@/components/ui/badge"
import { formatAbsoluteTime } from "@/lib/utils"
import {
  describeFreshnessCause,
  describeRefreshFacts,
  describeUpstream,
  formatSpan,
  ORCHESTRATION_UNAVAILABLE_REASON,
  overdueSeconds,
  parseOpenBreaches,
  parseRunningModels,
  waitsForAll,
  type Fetched,
  type LiveCell,
  type ModelFreshnessBreach,
  type RunningModelsResponse,
  type FreshnessSchedule,
  type ScheduleUpstream,
} from "@/components/explorer/liveState"

// Every model answered costs one Temporal query on pipeline-workflows, the task queue
// that carries real rebuilds (saved_query_explorer_running.go). Fifteen seconds is slow
// enough that a page left open is not load, and fast enough that a rebuild of any length
// worth watching shows up while someone is looking.
export const RUNNING_POLL_MS = 15_000
// The breach list is a Postgres read, and the sweep that writes it runs on a
// minutes-scale interval, so polling it faster would only re-read the same rows.
export const FRESHNESS_POLL_MS = 60_000

// One poll plus slack for a late timer. An answer older than that was not refreshed on
// schedule (the tab was hidden, or polling was off), so it is no longer the page's
// current answer. A tab switched away for a few seconds keeps its rows.
const STALE_AFTER_MS = RUNNING_POLL_MS + 5_000

/**
 * Polls GET /api/v1/explorer/running.
 *
 * A failed check replaces the last answer rather than sitting behind it: "Rebuilding"
 * carried past a poll that failed is a present-tense claim nothing is backing any more.
 * So does an answer too old to vouch for. A tab shown again after ten minutes reads
 * Checking, with that answer's age, until the new one lands — which at the fifty-model
 * cap can take ~14 s — rather than showing a ten-minute-old Rebuilding as current.
 */
export function useRunningModels(enabled: boolean) {
  const [running, setRunning] = useState<Fetched<RunningModelsResponse>>({ status: "loading" })
  const lastSettledAt = useRef<number | null>(null)

  const ask = useCallback(async (): Promise<Fetched<RunningModelsResponse>> => {
    const last = lastSettledAt.current
    if (last !== null && Date.now() - last > STALE_AFTER_MS) {
      const note = `The last answer is ${formatSpan((Date.now() - last) / 1000)} old, so it is not shown while the page asks again.`
      // An error claims nothing, and its banner already says so; only an answer is set aside.
      setRunning((prev) => (prev.status === "ok" ? { status: "loading", note } : prev))
    }
    try {
      const res = await authFetch("/api/v1/explorer/running", { cache: "no-store" })
      // authFetch does not throw on a non-2xx, and an error body has no models in it —
      // which, unchecked, would read as a workspace where nothing is running.
      if (!res.ok) return { status: "error", message: `The live-state check failed (HTTP ${res.status}).` }
      const parsed = parseRunningModels(await res.json())
      return parsed
        ? { status: "ok", data: parsed }
        : { status: "error", message: "The live-state check returned something this page could not read." }
    } catch {
      return { status: "error", message: "Could not reach the server to check live state." }
    }
  }, [])

  const apply = useCallback((answer: Fetched<RunningModelsResponse>) => {
    setRunning(answer)
    lastSettledAt.current = Date.now()
  }, [])

  const refresh = useVisiblePoll(ask, apply, RUNNING_POLL_MS, enabled)
  return { running, refresh }
}

/**
 * Polls GET /api/v1/explorer/freshness for the workspace's open breaches.
 *
 * Unlike live state, an old breach list is not set aside when the tab returns. A freshness
 * check that is out shows no Overdue badge at all, which reads as on time, so withdrawing
 * the badges while it asks would be the calmer claim, not the safer one.
 */
export function useOpenFreshnessBreaches(enabled: boolean) {
  const [freshness, setFreshness] = useState<Fetched<ModelFreshnessBreach[]>>({ status: "loading" })

  const ask = useCallback(async (): Promise<Fetched<ModelFreshnessBreach[]>> => {
    try {
      const res = await authFetch("/api/v1/explorer/freshness", { cache: "no-store" })
      if (!res.ok) return { status: "error", message: `The freshness check failed (HTTP ${res.status}).` }
      const parsed = parseOpenBreaches(await res.json())
      return parsed
        ? { status: "ok", data: parsed }
        : { status: "error", message: "The freshness check returned something this page could not read." }
    } catch {
      return { status: "error", message: "Could not reach the server to check freshness." }
    }
  }, [])

  const refresh = useVisiblePoll(ask, setFreshness, FRESHNESS_POLL_MS, enabled)
  return { freshness, refresh }
}

function queuedSuffix(n: number): string {
  return n > 0 ? ` · ${n} queued` : ""
}

const WAITING_ANY = "Waiting for the next upstream to finish before it rebuilds again"
const WAITING_ALL = "Waiting for every upstream to run before it rebuilds again"

/**
 * The Now cell for one row. waitsForAll is a fan-in on upstream policy "all" (see
 * waitsForAll in liveState): the next upstream to land does not rebuild it, so its
 * Waiting must not say so.
 */
export function LiveStateCell({ cell, waitsForAll: all = false }: { cell: LiveCell; waitsForAll?: boolean }) {
  switch (cell.kind) {
    case "not_applicable":
      return (
        <span
          className="text-zinc-400"
          title="Runs on a clock, so there is nothing to show between runs."
        >
          —
        </span>
      )
    case "loading":
      return (
        <span className="inline-flex items-center gap-1 text-zinc-400" title={cell.reason}>
          <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
          Checking…
        </span>
      )
    case "unavailable":
      return (
        <span className="inline-flex items-center gap-1 text-zinc-500 dark:text-zinc-400" title={cell.reason}>
          <HelpCircle className="h-3 w-3" aria-hidden />
          Unavailable
        </span>
      )
    case "not_checked":
      return (
        <span className="text-zinc-400" title={cell.reason}>
          Not checked
        </span>
      )
    case "idle":
      return (
        <span className="text-zinc-500 dark:text-zinc-400" title={cell.reason}>
          Idle
        </span>
      )
    case "rebuilding":
      return (
        <>
          <Badge variant="info" className="gap-1 font-medium whitespace-nowrap">
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
            {`Rebuilding${queuedSuffix(cell.detail.queued_completions)}`}
          </Badge>
          <RebuildFailedBadge error={cell.detail.last_error} />
        </>
      )
    case "waiting":
      return (
        <>
          <Badge
            variant="secondary"
            className="font-medium whitespace-nowrap"
            title={`${all ? WAITING_ALL : WAITING_ANY}.`}
          >
            {`Waiting${queuedSuffix(cell.detail.queued_completions)}`}
          </Badge>
          <RebuildFailedBadge error={cell.detail.last_error} />
        </>
      )
    case "running_no_detail":
      return (
        <Badge variant="info" className="font-medium whitespace-nowrap" title={cell.reason}>
          Running · no detail
        </Badge>
      )
    case "unreachable":
      return (
        <Badge variant="warning" className="gap-1 font-medium whitespace-nowrap" title={cell.reason}>
          <HelpCircle className="h-3 w-3" aria-hidden />
          Unreachable
        </Badge>
      )
    case "unknown":
      return (
        <span className="inline-flex items-center gap-1 text-zinc-500 dark:text-zinc-400" title={cell.reason}>
          <HelpCircle className="h-3 w-3" aria-hidden />
          Unknown
        </span>
      )
  }
}

/**
 * A rebuild in this run of the refresh loop failed after its retries. The loop survives
 * that and never clears it within the run (model_refresh_workflow.go), so this says a
 * failure happened, not that the model is failing now. Without it a loop carrying one
 * reads as a calm grey Waiting until someone expands the row.
 */
function RebuildFailedBadge({ error }: { error?: string }) {
  if (!error) return null
  return (
    <Badge
      variant="destructive"
      className="gap-1 font-medium whitespace-nowrap"
      title={`An earlier rebuild failed after its retries: ${error}. This stays until the model goes idle, even if a later rebuild succeeded.`}
    >
      <AlertTriangle className="h-3 w-3" aria-hidden />
      Rebuild failed this run
    </Badge>
  )
}

/** The marker for a model whose table is past its freshness deadline. */
export function OverdueBadge({ breach, schedule }: { breach: ModelFreshnessBreach; schedule?: FreshnessSchedule }) {
  const by = formatSpan(overdueSeconds(breach))
  return (
    <Badge
      variant="outline"
      className="gap-1 border-amber-500 font-medium whitespace-nowrap text-amber-600 dark:text-amber-400"
      title={`Past its ${formatSpan(breach.deadline_seconds)} freshness deadline by ${by}: ${describeFreshnessCause(breach.cause, schedule)}.`}
    >
      <AlertTriangle className="h-3 w-3" aria-hidden />
      {`Overdue ${by}`}
    </Badge>
  )
}

/** What an expanded row says about the model right now, above its run history. */
export function LiveStateDetail({
  cell,
  breach,
  scheduleType,
  upstreams,
  upstreamPolicy,
}: {
  cell: LiveCell
  breach?: ModelFreshnessBreach
  scheduleType?: string
  upstreams?: ScheduleUpstream[]
  upstreamPolicy?: string
}) {
  let body: ReactNode = null
  switch (cell.kind) {
    case "rebuilding": {
      const cur = cell.detail.current
      body = (
        <>
          <p>
            {cur
              ? `Rebuilding since ${formatAbsoluteTime(cur.started_at)}, started by ${describeUpstream(cur.upstream_kind, cur.upstream_id, upstreams)}`
              : "Rebuilding. It did not say which upstream started it."}
          </p>
          <p className="text-zinc-500 dark:text-zinc-400">{describeRefreshFacts(cell.detail).join(" · ")}</p>
        </>
      )
      break
    }
    case "waiting":
      body = (
        <>
          <p>{waitsForAll(upstreamPolicy, upstreams) ? WAITING_ALL : WAITING_ANY}</p>
          <p className="text-zinc-500 dark:text-zinc-400">{describeRefreshFacts(cell.detail).join(" · ")}</p>
        </>
      )
      break
    case "loading":
      body = (
        <p className="text-zinc-500 dark:text-zinc-400">{cell.reason ? `Checking live state… ${cell.reason}` : "Checking live state…"}</p>
      )
      break
    case "idle":
      body = <p className="text-zinc-500 dark:text-zinc-400">Idle: {cell.reason}</p>
      break
    case "running_no_detail":
      body = <p className="text-zinc-500 dark:text-zinc-400">Running, detail unavailable: {cell.reason}</p>
      break
    case "unreachable":
      body = <p className="text-amber-700 dark:text-amber-400">Unreachable: {cell.reason}</p>
      break
    case "unknown":
      body = <p className="text-zinc-500 dark:text-zinc-400">Unknown: {cell.reason}</p>
      break
    case "unavailable":
      body = <p className="text-zinc-500 dark:text-zinc-400">Live state unavailable: {cell.reason}</p>
      break
    case "not_checked":
      body = <p className="text-zinc-500 dark:text-zinc-400">Not checked: {cell.reason}</p>
      break
    case "not_applicable":
      break
  }

  const lastError =
    cell.kind === "rebuilding" || cell.kind === "waiting" ? cell.detail.last_error : undefined

  if (!body && !breach) return null
  return (
    <div className="space-y-1 pt-2 text-xs text-zinc-700 dark:text-zinc-300">
      {body}
      {lastError && (
        <p className="text-red-600 dark:text-red-400 break-words">
          Last rebuild failure this run: {lastError}
        </p>
      )}
      {breach && (
        <p className="text-amber-700 dark:text-amber-400">
          {`Past its ${formatSpan(breach.deadline_seconds)} freshness deadline by ${formatSpan(overdueSeconds(breach))} — `}
          {breach.never_succeeded
            ? "it has never rebuilt successfully"
            : `last rebuilt successfully ${formatAbsoluteTime(breach.reference_at)}`}
          {`; ${describeFreshnessCause(breach.cause, { scheduleType, upstreams, upstreamPolicy })}.`}
        </p>
      )}
    </div>
  )
}

/**
 * Everything the page cannot currently vouch for, above the table.
 *
 * Each of these is a way for a row to look calmer than the truth — a missing "Overdue",
 * a cell that is not "Rebuilding" — so each one is said out loud rather than left for the
 * reader to infer from an absence.
 */
export function LiveStateBanner({
  running,
  freshness,
  liveRelevant,
  unlistedBreaches,
}: {
  running: Fetched<RunningModelsResponse>
  freshness: Fetched<ModelFreshnessBreach[]>
  /** Whether any row is after_upstream, i.e. whether live state was asked for at all. */
  liveRelevant: boolean
  /** Open breaches for models that have no row on this page. */
  unlistedBreaches: ModelFreshnessBreach[]
}) {
  const notes: ReactNode[] = []

  if (liveRelevant && running.status === "error") {
    notes.push(
      <>
        <span className="font-semibold">Live state unavailable.</span> {running.message} Rows read
        Unavailable rather than Idle until a check succeeds.
      </>,
    )
  } else if (liveRelevant && running.status === "ok" && !running.data.temporal_available) {
    notes.push(
      <>
        <span className="font-semibold">Live state unavailable.</span> {ORCHESTRATION_UNAVAILABLE_REASON} Schedules
        are unaffected by this page, and no row is shown as Idle.
      </>,
    )
  } else if (liveRelevant && running.status === "ok" && running.data.count >= running.data.limit) {
    notes.push(
      <>
        Live state is checked for the first {running.data.limit} upstream-triggered models by name;
        any others read Not checked.
      </>,
    )
  }

  if (freshness.status === "error") {
    notes.push(
      <>
        <span className="font-semibold">Overdue markers unavailable.</span> {freshness.message} A row
        without one is not known to be on time.
      </>,
    )
  }

  if (unlistedBreaches.length > 0) {
    const names = unlistedBreaches.map((b) => b.name)
    const shown = names.slice(0, 5).join(", ")
    const more = names.length > 5 ? ` and ${names.length - 5} more` : ""
    notes.push(
      <>
        {unlistedBreaches.length === 1
          ? "1 model past its freshness deadline has no row on this page"
          : `${unlistedBreaches.length} models past their freshness deadline have no row on this page`}
        : {shown}
        {more}.
      </>,
    )
  }

  if (notes.length === 0) return null
  return (
    <div
      role="status"
      className="space-y-1 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200"
    >
      {notes.map((note, i) => (
        <p key={i} className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          <span>{note}</span>
        </p>
      ))}
    </div>
  )
}
