import { CheckCircle2, Timer, XCircle } from "lucide-react"

import { StatCard } from "@/components/kit/StatCard"
import { formatDurationOrDash } from "@/lib/duration"
import { formatAbsoluteTime, formatRelativeTime } from "@/lib/utils"

// A model's or a workflow's health at a glance, read from its newest page of runs,
// unfiltered: how often it succeeds, how long it usually takes, and when it last
// failed. Any other status (a model's skip, a workflow run stopped or still going)
// did no finished work, so it is neither a success nor a failure and has no run time.

/** A run as the cards count it: succeeded or failed, else not counted. */
export interface HealthRun {
  status: string
  duration_ms: number
  started_at: string
}

function median(values: number[]): number | null {
  if (values.length === 0) return null
  const sorted = [...values].sort((a, b) => a - b)
  const mid = Math.floor(sorted.length / 2)
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2
}

/** `runs` newest first, as the routes send them. */
export function RunHealthKpis({ runs }: { runs: HealthRun[] }) {
  const finished = runs.filter((r) => r.status === "succeeded" || r.status === "failed")
  const succeeded = finished.filter((r) => r.status === "succeeded").length
  const rate = finished.length > 0 ? Math.round((succeeded / finished.length) * 100) : null
  const medianMs = median(finished.map((r) => r.duration_ms).filter((ms) => Number.isFinite(ms) && ms >= 0))
  const lastFailure = finished.find((r) => r.status === "failed")
  // The newest run that did work failed: it is failing now, not just once before.
  const failingNow = finished[0]?.status === "failed"
  const scope = `last ${runs.length} ${runs.length === 1 ? "run" : "runs"}`
  // The runs the rate leaves out, by status: "1 skipped, 1 running".
  const others = new Map<string, number>()
  for (const r of runs) if (r.status !== "succeeded" && r.status !== "failed") others.set(r.status, (others.get(r.status) ?? 0) + 1)
  const notCounted = [...others].map(([status, n]) => `${n} ${status}`).join(", ")
  const none = `No run succeeded or failed in the ${scope}`

  return (
    <div className="grid gap-3 @md:grid-cols-3" data-testid="run-health-kpis">
      <StatCard
        title="Success rate"
        icon={CheckCircle2}
        value={rate === null ? "—" : `${rate}%`}
        tone={rate !== null && rate < 100 ? (rate < 50 ? "bad" : "warn") : "default"}
        hint={
          rate === null
            ? none
            : `${succeeded} of ${finished.length} succeeded, ${scope}${notCounted ? ` (${notCounted}: not counted)` : ""}`
        }
      />
      <StatCard
        title="Median run time"
        icon={Timer}
        value={medianMs === null ? "—" : formatDurationOrDash(medianMs)}
        hint={medianMs === null ? none : `Succeeded and failed runs, ${scope}`}
      />
      <StatCard
        title="Last failure"
        icon={XCircle}
        value={lastFailure ? formatRelativeTime(lastFailure.started_at) : "None"}
        tone={failingNow ? "bad" : "default"}
        hint={
          lastFailure
            ? `${formatAbsoluteTime(lastFailure.started_at)}${failingNow ? " · the newest run" : ""}`
            : `No failure in the ${scope}`
        }
      />
    </div>
  )
}
