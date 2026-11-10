import { Fragment } from "react"
import { ACTIVITY_STATUS_TONES, StatusPill } from "@/components/kit/StatusPill"
import { ACTIVITY_STATUS_LABELS, isActivityStatus } from "@/lib/api/activity"
import { describeUpstreamSet, type RunProvenanceFields } from "@/components/explorer/runProvenance"
import { describeCron } from "@/components/explorer/cronSentence"
import { type ScheduleUpstream } from "@/components/explorer/liveState"

// Shared by the Scheduled Queries list and the per-model page, so the two cannot
// disagree about what a schedule's status or a run's outcome is called.

export interface ScheduleSpec {
  cron?: string
  every_seconds?: number
  timezone?: string
}

export interface ScheduledQuery {
  schedule_id: string
  saved_query_id: string
  name: string
  description?: string
  connection_id: string
  connection_name?: string
  connector_type?: string
  schedule_type: string
  schedule_spec: ScheduleSpec
  status: string
  materialization: string
  target_table?: string
  /** The name of the workspace layer the model is in, or absent when it is in none (migration 135). */
  layer?: string
  statement_class?: string
  /** Resolved from connector_type server-side by the Explorer capability table. */
  supports_materialization?: boolean
  last_run_at?: string
  last_run_status?: string
  last_run_error?: string
  next_run_at?: string
  /**
   * Set only for schedule_type "after_upstream": every pipeline and model whose
   * completion wakes this one. Whether one landing is enough or all of them must have
   * landed is upstream_policy. Names are joined server-side and are empty for a producer
   * since deleted.
   */
  upstreams?: ScheduleUpstream[]
  /** after_upstream only: "any" rebuilds on each landing, "all" waits for every upstream. */
  upstream_policy?: "any" | "all"
  created_by: string
  created_at: string
  updated_at: string
  paused_at?: string
  paused_reason?: string
  auto_paused_at?: string
  auto_paused_reason?: string
}

export interface SavedQueryRun extends RunProvenanceFields {
  run_id: string
  saved_query_id: string
  schedule_id?: string
  trigger_source: string
  status: string
  target_table?: string
  rows_affected?: number
  error?: string
  auto_pause_reason?: string
  started_at: string
  finished_at: string
  duration_ms: number
}

/** What describeCadence reads. The model dialog's own schedule type carries the same fields. */
export type CadenceFields = Pick<ScheduledQuery, "schedule_type" | "schedule_spec" | "upstreams" | "upstream_policy">

export function describeCadence(s: CadenceFields): string {
  if (s.schedule_type === "after_upstream") {
    return describeUpstreamSet(s.upstreams, s.upstream_policy)
  }
  if (s.schedule_type === "interval") {
    const seconds = s.schedule_spec.every_seconds || 0
    if (seconds % 3600 === 0) return `Every ${seconds / 3600} hour${seconds === 3600 ? "" : "s"}`
    if (seconds % 60 === 0) return `Every ${seconds / 60} minute${seconds === 60 ? "" : "s"}`
    return `Every ${seconds} seconds`
  }
  const zone = s.schedule_spec.timezone || "UTC"
  return `${describeCron(s.schedule_spec.cron) ?? s.schedule_spec.cron} (${zone})`
}

/**
 * A cron schedule's expression as stored, and whether describeCadence put it into words.
 * Null for any other kind of schedule. Where it was worded, the expression is still worth
 * showing beside the sentence: it is what the Edit schedule form holds.
 */
export function cadenceCron(s: CadenceFields): { expression: string; worded: boolean } | null {
  if (s.schedule_type === "after_upstream" || s.schedule_type === "interval") return null
  const expression = s.schedule_spec.cron ?? ""
  return { expression, worded: describeCron(expression) !== null }
}

/**
 * The Next run of an active after_upstream schedule, which has no time by construction.
 * A fan-in on "all" is not woken by the next landing: the gateway rebuilds it only once
 * every upstream has completed since its last successful rebuild. Same rule as
 * describeUpstreamSet, so with one upstream the policy changes nothing.
 */
export function describeUpstreamNextRun(s: Pick<ScheduledQuery, "upstreams" | "upstream_policy">): string {
  return s.upstream_policy === "all" && (s.upstreams?.length ?? 0) > 1
    ? "When all its upstreams have run"
    : "When an upstream runs"
}

/**
 * Whether a fire of this schedule would actually do anything. A statement model has
 * no target table by design — it writes wherever its own SQL says — so the absence of
 * one is only a fault in table mode.
 */
export function runsDoSomething(s: ScheduledQuery): boolean {
  if (s.materialization === "statement") return true
  return s.materialization === "table" && !!s.target_table
}

/**
 * A schema-qualified name that wraps after a dot before anywhere else. The Details column
 * is narrow, and break-all cut public.swipes_total into "public.swipes_tota" and "l".
 */
export function QualifiedName({ name }: { name: string }) {
  return (
    <span className="font-mono break-words">
      {name.split(".").map((part, i) => (
        <Fragment key={i}>
          {i > 0 && (
            <>
              .<wbr />
            </>
          )}
          {part}
        </Fragment>
      ))}
    </span>
  )
}

/** What each run of the model writes: the model page's Does row, and the Graph tab's run panel. */
export function describeWhatItDoes(s: Pick<ScheduledQuery, "materialization" | "target_table">) {
  if (s.materialization === "statement") return <span>Runs its SQL as written</span>
  if (s.materialization === "table" && s.target_table) {
    return (
      <span>
        Rebuilds <QualifiedName name={s.target_table} />
      </span>
    )
  }
  return <span className="text-amber-600 dark:text-amber-400">Nothing: no target table is set</span>
}

export function statusBadge(s: ScheduledQuery) {
  // A schedule whose runs do nothing is `active` and firing, and every fire refuses.
  // The status alone would report that as healthy, so it is called out separately.
  if (s.status === "active" && !runsDoSomething(s)) {
    return <StatusPill tone="wait">Does nothing</StatusPill>
  }
  if (s.status === "active") return <StatusPill tone="ok">Active</StatusPill>
  return <StatusPill tone="skip">{capitalize(s.status)}</StatusPill>
}

export function runStatusBadge(status: string) {
  // A run's status in the words and colours every run list in the app uses.
  if (isActivityStatus(status)) {
    return <StatusPill tone={ACTIVITY_STATUS_TONES[status]}>{ACTIVITY_STATUS_LABELS[status]}</StatusPill>
  }
  return <StatusPill tone="cancel">{capitalize(status)}</StatusPill>
}

const capitalize = (word: string) => word.charAt(0).toUpperCase() + word.slice(1)
