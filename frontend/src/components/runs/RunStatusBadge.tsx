import { ACTIVITY_STATUS_TONES, StatusPill } from "@/components/kit/StatusPill"
import { ACTIVITY_STATUS_LABELS, type ActivityRun } from "@/lib/api/activity"

/** A run's status, one colour per status word across the three kinds. */
export function RunStatusBadge({ run }: { run: Pick<ActivityRun, "status" | "source_status"> }) {
  const label = ACTIVITY_STATUS_LABELS[run.status] ?? run.status
  // The kind's own word ("silent_drop_detected", "completed") says more than the
  // shared one when they differ, so it rides along as the tooltip.
  const title = run.source_status && run.source_status !== run.status ? run.source_status.replace(/_/g, " ") : undefined
  return (
    <StatusPill tone={ACTIVITY_STATUS_TONES[run.status] ?? "skip"} title={title}>
      {label}
    </StatusPill>
  )
}
