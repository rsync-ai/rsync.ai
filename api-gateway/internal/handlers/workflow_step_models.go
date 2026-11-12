package handlers

// A workflow's refresh_model step (docs/internal/workflows-design.md § 3.1, build step
// 5) asks the model's own refresh workflow for a run: the adapter signals
// ModelRefreshWorkflow, whose commands step 5 left unchanged, and its run activity
// comes back to the event door's endpoint with upstream_kind "workflow". The workflow
// run is that request's authority, as a schedule row is the event door's: it must still
// be running, be the workflow the request names, have a refresh_model step for this
// model, and share the model's workspace. Its actor is checked again here, since the
// step started.
//
// The step does not wait for this request's answer; it waits for the model's run row
// (workflow_graph_runs.go). So a request that cannot run records why, on the row the
// step reads, and a request no step is waiting for any more records nothing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"net/http"
	"strings"
	"time"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// workflowStepModelRun is a refresh_model request the workflow run may make.
type workflowStepModelRun struct {
	run   *workflowRunRow
	actor string
}

// loadWorkflowStepModelRun checks that run runID of workflow workflowID may ask for a
// run of modelID. A refusal is a sentence and no error: the request names a run that
// ended, another workflow, or a model the run has no step for, and no retry changes
// that.
func loadWorkflowStepModelRun(ctx context.Context, database *sql.DB, modelID, workflowID, runID string) (
	*workflowStepModelRun, string, error) {
	if _, err := uuid.Parse(runID); err != nil {
		return nil, "the request names no workflow run", nil
	}
	run, err := loadWorkflowRun(ctx, database, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "the workflow run was deleted", nil
	}
	if err != nil {
		return nil, "", err
	}
	if run.WorkflowID != workflowID {
		return nil, "the run belongs to another workflow", nil
	}
	if run.RunStatus != "running" {
		return nil, "the workflow run has ended", nil
	}
	asks := false
	for _, s := range run.Definition.ProcessSteps() {
		asks = asks || (s.Type == workflow.StepRefreshModel && s.ModelID == modelID)
	}
	if !asks {
		return nil, "the workflow run has no step that refreshes this model", nil
	}
	var ws string
	err = database.QueryRowContext(ctx, `SELECT workspace_id::text FROM saved_queries WHERE id = $1`, modelID).Scan(&ws)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && ws != run.WorkspaceID) {
		return nil, "the model is not in the workflow's workspace", nil
	}
	if err != nil {
		return nil, "", err
	}
	return &workflowStepModelRun{run: run, actor: workflowRunActor(run)}, "", nil
}

// workflowActorRefusal says why the run's actor may not run its steps any more; "" means
// it may. An error is a role that could not be read.
func workflowActorRefusal(ctx context.Context, database *sql.DB, run *workflowRunRow) (string, error) {
	actor := workflowRunActor(run)
	if actor == "" {
		return "The workflow has no run-as user. An owner or admin should turn it on again.", nil
	}
	role, err := resolveWorkspaceRoleForUser(ctx, database, run.WorkspaceID, actor)
	if err != nil {
		return "", err
	}
	return workflowRoleRefusal(role), nil
}

// runWorkflowStepModel is RunSavedQueryModelInternal's workflow door: one run of the
// model as the workflow's actor, recorded as triggered by the workflow run. A run the
// actor may no longer make is recorded as a skip, so the step that asked reads why. No
// schedule is paused: there is none.
func runWorkflowStepModel(c *gin.Context, database *sql.DB, id string, req internalModelRunRequest) {
	ctx := c.Request.Context()
	fields := log.Fields{"model_id": id, "workflow_id": req.UpstreamID, "workflow_run_id": req.ExecutionID}
	wr, refusal, err := loadWorkflowStepModelRun(ctx, database, id, req.UpstreamID, req.ExecutionID)
	if err != nil {
		log.WithError(err).WithFields(fields).Error("workflow model run: could not load the workflow run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the workflow run"})
		return
	}
	if refusal != "" {
		log.WithFields(fields).WithField("reason", refusal).Info("workflow model run: nothing to run")
		c.JSON(http.StatusOK, gin.H{"status": "skipped", "reason": refusal})
		return
	}

	prov := provenanceFor(modelRefreshSource{
		Kind:        upstreamKindWorkflow,
		ID:          req.UpstreamID,
		ExecutionID: req.ExecutionID,
		Depth:       req.Depth,
	}, req.Coalesced)
	startedAt := time.Now()
	var res *modelRunResult
	actorRefusal, err := workflowActorRefusal(ctx, database, wr.run)
	if err != nil {
		log.WithError(err).WithFields(fields).Error("workflow model run: could not check the run-as user's role")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the run-as user's role"})
		return
	}
	if actorRefusal != "" {
		res = &modelRunResult{Status: "skipped", Error: actorRefusal}
	} else {
		res, err = runSavedQueryModel(ctx, database, id, wr.actor)
		if err != nil {
			log.WithError(err).WithFields(fields).Error("workflow model run could not be attempted")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to run model"})
			return
		}
	}
	// A refusal would pause the schedule a model run came from; this one came from no
	// schedule. Its error says why, and the step that asked fails with it.
	res.AutoPauseReason = ""
	runID := recordModelRunOutcome(ctx, database, id, res, modelRunAudit{
		Trigger:    triggerTriggered,
		ActorID:    wr.actor,
		StartedAt:  startedAt,
		Provenance: prov,
	})
	answerInternalModelRun(c, req, res, id, runID)
}

// modelRefreshRetryNext is the sentence ModelRefreshWorkflow ends a failure with
// (describeModelRunFailure in the adapter). True of a trigger, not of a workflow step.
const modelRefreshRetryNext = "It will run again the next time something this model waits on finishes."

// recordWorkflowModelRunFailure is recordScheduledModelRunFailure for a run a workflow
// step asked for and never got an answer to: a failed row the step reads, filed under
// the workflow run instead of a schedule. Deduped on the run and started_at, under a lock
// on the run, for the same at-least-once reason.
func recordWorkflowModelRunFailure(ctx context.Context, database *sql.DB, modelID string,
	req internalModelRunFailureRequest) (modelRunFailureDisposition, error) {
	wr, refusal, err := loadWorkflowStepModelRun(ctx, database, modelID, req.UpstreamID, req.ExecutionID)
	if err != nil {
		return "", err
	}
	if refusal != "" {
		// A run that ended has a step that stopped waiting; one of another workflow or
		// workspace is not this model's to record against.
		return runFailureScheduleGone, nil
	}
	startedAt := req.StartedAt.UTC().Truncate(time.Microsecond)
	msg := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(req.Error), modelRefreshRetryNext))
	msg = modelRunFailureMessage(msg)

	tx, err := database.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin failure record: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit has succeeded

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		int32(modelRunFailureLockNamespace), int32(crc32.ChecksumIEEE([]byte(wr.run.RunID)))); err != nil {
		return "", fmt.Errorf("lock failure record: %w", err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM saved_query_runs
			WHERE saved_query_id = $1 AND upstream_kind = 'workflow' AND origin_execution_id = $2
			  AND started_at = $3
		)
	`, modelID, wr.run.RunID, startedAt).Scan(&exists); err != nil {
		return "", fmt.Errorf("check for an existing record: %w", err)
	}
	if exists {
		return runFailureDuplicate, nil
	}
	if err := stampLastRunFailureIfLatest(ctx, tx, modelID, wr.run.WorkspaceID, msg, startedAt); err != nil {
		return "", fmt.Errorf("stamp model: %w", err)
	}
	prov := provenanceFor(modelRefreshSource{
		Kind:        upstreamKindWorkflow,
		ID:          wr.run.WorkflowID,
		ExecutionID: wr.run.RunID,
		Depth:       req.Depth,
	}, req.Coalesced)
	args := append([]interface{}{modelID, string(triggerTriggered), msg, wr.actor, startedAt}, prov.args()...)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO saved_query_runs (
			saved_query_id, schedule_id, trigger_source, status, error,
			ran_as_user_id, started_at, finished_at,
			`+runProvenanceColumns+`
		) VALUES (
			$1, NULL, $2, 'failed', NULLIF($3, ''),
			NULLIF($4, '')::uuid, $5, NOW(),
			`+runProvenanceValues(6)+`
		)
	`, args...); err != nil {
		return "", fmt.Errorf("append run history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit failure record: %w", err)
	}
	return runFailureRecorded, nil
}

// recordWorkflowModelRunFailureInternal is RecordSavedQueryModelRunFailureInternal's
// workflow branch, with the same status codes: 404 is nothing to record against, which
// the activity treats as done.
func recordWorkflowModelRunFailureInternal(c *gin.Context, database *sql.DB, id string, req internalModelRunFailureRequest) {
	if req.StartedAt.IsZero() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "started_at is required"})
		return
	}
	disp, err := recordWorkflowModelRunFailure(c.Request.Context(), database, id, req)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"model_id": id, "workflow_run_id": req.ExecutionID}).
			Error("internal model run failure: could not record a workflow step's run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record the failed run"})
		return
	}
	switch disp {
	case runFailureScheduleGone:
		c.JSON(http.StatusNotFound, gin.H{"status": "not_found", "reason": "no running workflow step asks for this model"})
	case runFailureDuplicate:
		c.JSON(http.StatusOK, gin.H{"recorded": false, "duplicate": true})
	default:
		c.JSON(http.StatusOK, gin.H{"recorded": true})
	}
}
