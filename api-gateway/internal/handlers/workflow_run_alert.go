package handlers

import (
	"context"
	"database/sql"
	"errors"

	"api-gateway/internal/notifier"

	workflow "github.com/rsync-ai/shared/workflowdef"
	log "github.com/sirupsen/logrus"
)

// A workflow run nobody started by hand (its schedule, or a pipeline finishing)
// that failed used to say so only on the workflow's Runs tab. Whoever it runs as
// is now told, through the notifier's workflow routing (workflow_id → the run-as
// user, else whoever made it, else the instance's admins); the bell shows it in
// the workflow's workspace only.
//
// Quiet about three runs that already have someone told: one started by hand (the
// person who pressed Run is looking at it), one whose only failures are its
// pipelines' runs (each failed run, or the Stop that cancelled it, told the
// pipeline's owner), and one whose own any_failed send went out (it told whoever
// the workflow was set up to tell).

// The body of the notice. Fixed: a step's error can carry a host, a user name or a
// value from a row, and a notice carries ids and codes only. The Runs tab the link
// opens has the error.
const workflowRunFailedMessage = "A scheduled or pipeline-triggered run of this workflow failed."

// workflowRunIsUnattendedFailure is the gate's first half: a failed run through a
// door with no person behind it. An unknown trigger is not an unattended run.
func workflowRunIsUnattendedFailure(run *workflowRunRow, o runOutcome) bool {
	if run == nil || o.status != workflow.StatusFailed {
		return false
	}
	return run.Trigger == workflow.TriggerSchedule || run.Trigger == workflow.TriggerAfterPipeline
}

// workflowFailureSentItsOwnNotice reports whether one of the run's any_failed
// sends went out: the workflow's own failure notice.
func workflowFailureSentItsOwnNotice(d workflow.Definition, deliveries []DeliveryOutcome) bool {
	for _, del := range deliveries {
		if del.Status != deliveryStatusSent {
			continue
		}
		if s := d.Step(del.StepID); s != nil && s.Type == workflow.StepSend && s.Rule == workflow.RuleAnyFailed {
			return true
		}
	}
	return false
}

// workflowFailureWasItsPipelineRuns reports whether every step that failed the run
// is a run_pipeline step whose pipeline run failed or was cancelled. A step that
// failed for its own reason (it could not start the run, or stopped waiting for
// it) is the workflow's failure, and so is a read that fails here.
func workflowFailureWasItsPipelineRuns(ctx context.Context, database *sql.DB, run *workflowRunRow, o runOutcome) bool {
	if len(o.failedSteps) == 0 || database == nil {
		return false
	}
	for _, id := range o.failedSteps {
		if s := run.Definition.Step(id); s == nil || s.Type != workflow.StepRunPipeline {
			return false
		}
	}
	for _, id := range o.failedSteps {
		var status string
		err := database.QueryRowContext(ctx, `
			SELECT COALESCE(e.status, '')
			FROM workflow_step_runs s LEFT JOIN executions e ON e.id::text = s.execution_id
			WHERE s.run_id = $1 AND s.step_id = $2
			ORDER BY s.attempt DESC LIMIT 1
		`, run.RunID, id).Scan(&status)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.WithError(err).WithFields(log.Fields{"run_id": run.RunID, "step_id": id}).
					Warn("[workflows] failure notice: could not read the step's pipeline run; telling the owner")
			}
			return false
		}
		if status != "failed" && status != "cancelled" {
			return false
		}
	}
	return true
}

// notifyWorkflowRunFailed tells the workflow's owner a run that was just recorded
// failed, once per run (keyed by the run's id). Without a publisher it reads
// nothing.
func notifyWorkflowRunFailed(ctx context.Context, database *sql.DB, run *workflowRunRow, o runOutcome) {
	if currentRunNoticeNotifier() == nil || !workflowRunIsUnattendedFailure(run, o) {
		return
	}
	if workflowFailureSentItsOwnNotice(run.Definition, o.deliveries) ||
		workflowFailureWasItsPipelineRuns(ctx, database, run, o) {
		return
	}
	publishRunNotice(runNotice{
		WorkflowID:   run.WorkflowID,
		Code:         notifier.CodeWorkflowRunFailed,
		Severity:     "error",
		FailureType:  "workflow_run_failed",
		Message:      workflowRunFailedMessage,
		ActionURL:    "/workflows/" + run.WorkflowID,
		DedupSubject: run.RunID,
	})
}

// notifyUnloadedWorkflowRunFailed is notifyWorkflowRunFailed for a run recorded
// failed because its workflow could not be read (EvaluateWorkflowRunInternal):
// only its workflow and trigger are read back.
func notifyUnloadedWorkflowRunFailed(ctx context.Context, database *sql.DB, runID string) {
	if currentRunNoticeNotifier() == nil {
		return
	}
	run := &workflowRunRow{RunID: runID}
	if err := database.QueryRowContext(ctx, `
		SELECT workflow_id::text, trigger FROM workflow_runs WHERE id = $1
	`, runID).Scan(&run.WorkflowID, &run.Trigger); err != nil {
		log.WithError(err).WithField("run_id", runID).Warn("[workflows] failure notice: could not read the run")
		return
	}
	notifyWorkflowRunFailed(ctx, database, run, runOutcome{status: workflow.StatusFailed})
}
