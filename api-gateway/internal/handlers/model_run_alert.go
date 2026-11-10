package handlers

import (
	"time"

	"api-gateway/internal/notifier"
)

// A model rebuild nobody started by hand (a schedule's tick, or an upstream pipeline
// or model finishing) that failed used to say so only on the model's own badge and
// Runs tab, which nobody has open at 3am. Its owner is now told, through the
// notifier's model routing (saved_query_id → the model's owner), and so is the owner
// of a schedule that paused itself.
//
// Quiet about three runs that already have someone told: one started by hand (the
// person who pressed Run is looking at it), one a workflow step ran (the workflow's
// own failure notice names the step, workflow_run_alert.go), and one a blocking data
// check held back (CHECK_BLOCKED, saved_query_checks.go).

// The bodies of the model notices. Fixed: an engine's error can carry a host, a user
// name or a value from a row, and a notice carries ids and codes only. The Runs tab
// the link opens has the error.
const (
	modelRunFailedMessage      = "A scheduled or triggered rebuild of this model failed."
	modelSchedulePausedMessage = "rsync.ai paused this model's schedule: its runs would keep failing for the same reason."
)

// modelRunIsUnattendedFailure is the gate: a failed rebuild through a door with no
// person behind it, outside any workflow, that no data check held back. An unknown
// trigger is a wiring bug (writeModelRunOutcome logs it), not an unattended run.
func modelRunIsUnattendedFailure(res *modelRunResult, audit modelRunAudit) bool {
	if res == nil || res.Status != "failed" || res.CheckBlocked {
		return false
	}
	if audit.Trigger != triggerScheduled && audit.Trigger != triggerTriggered {
		return false
	}
	return audit.Provenance.UpstreamKind != upstreamKindWorkflow
}

// notifyModelRunFailed tells the owner an unattended rebuild failed, once per run:
// keyed by the run's history row, or by when it started if that row was not written
// (losing the bookkeeping must not also lose the notice).
func notifyModelRunFailed(modelID, runID string, res *modelRunResult, audit modelRunAudit) {
	if !modelRunIsUnattendedFailure(res, audit) {
		return
	}
	subject := runID
	if subject == "" {
		startedAt := audit.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now()
		}
		subject = modelRunFailureSubject(modelID, startedAt)
	}
	publishModelRunFailed(modelID, subject)
}

// notifyRecordedModelRunFailure is notifyModelRunFailed for a failure the workflow
// reported after the fact (RecordSavedQueryModelRunFailureInternal), which has no
// result to gate on: only a non-workflow report that wrote a row reaches it. Keyed by
// the attempt's started_at, the same value its dedupe uses, so a retry is one notice.
func notifyRecordedModelRunFailure(modelID string, startedAt time.Time) {
	publishModelRunFailed(modelID, modelRunFailureSubject(modelID, startedAt))
}

func modelRunFailureSubject(modelID string, startedAt time.Time) string {
	return modelID + ":" + startedAt.UTC().Format(time.RFC3339Nano)
}

func publishModelRunFailed(modelID, subject string) {
	publishRunNotice(runNotice{
		SavedQueryID: modelID,
		Code:         notifier.CodeModelRunFailed,
		Severity:     "error",
		FailureType:  "model_run_failed",
		Message:      modelRunFailedMessage,
		ActionURL:    "/models/" + modelID,
		DedupSubject: subject,
	})
}

// notifyModelSchedulePaused tells the owner a schedule paused itself
// (autoPauseModelSchedule). The reason is on the run's row, not in the notice: it is
// free text. Keyed by the schedule and the second it paused, so a schedule resumed and
// paused again is told again.
func notifyModelSchedulePaused(modelID, scheduleID string) {
	publishRunNotice(runNotice{
		SavedQueryID: modelID,
		Code:         notifier.CodeModelSchedulePaused,
		Severity:     "warning",
		FailureType:  "schedule_paused",
		Message:      modelSchedulePausedMessage,
		ActionURL:    "/models/" + modelID,
		DedupSubject: "schedule|" + scheduleID + "|" + time.Now().UTC().Format(time.RFC3339),
	})
}
