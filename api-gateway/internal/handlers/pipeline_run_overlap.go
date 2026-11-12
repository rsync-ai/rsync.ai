package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
)

// A Run while the pipeline already has one open used to start a second workflow
// beside the first. Both copied into the same destination, both wrote the one
// pipeline_progress row (keyed by pipeline id), and whichever finished last decided
// what the pipeline showed. A run parked on a question stayed open for good, so the
// batch sentinel kept alerting on it after a newer run had finished.
//
// So before a run starts, every open execution of the pipeline is sorted into one of:
//   - live: its workflow is running and not waiting on a person. The new run is refused.
//   - parked: pipeline_progress points at it and waits for a person. The new run
//     replaces it: the execution is closed as cancelled and its workflow told to stop.
//   - stale: its workflow has ended (or is gone) without closing the execution. It is
//     closed as failed, saying how the workflow ended.
//
// A run whose workflow does not exist yet but which started in the last
// runStartGrace counts as live: the execution row is written before its workflow
// starts (the scheduled wrapper runs two activities in between), so "no workflow"
// there means "starting", not "gone".
//
// The CDC stream row (id = pipeline id, trigger_source cdc) is excluded: the sink
// keeps it open for as long as the pipeline streams, and no workflow backs it.

const runStartGrace = 2 * time.Minute

// runOverlap is what a new run has to do about the runs already open.
type runOverlap struct {
	// Blocking is an open run whose workflow is still working; the new run is refused.
	Blocking string
	// Parked runs are replaced once the new run's workflow has started.
	Parked []string
	// Stale runs are closed once the new run's workflow has started.
	Stale []staleRun
}

type staleRun struct {
	ID       string
	HowEnded string
}

const openRunsSQL = `
	SELECT e.id::text,
	       COALESCE(pp.status = 'waiting_for_user', false),
	       COALESCE(e.start_time > NOW() - ($2 * INTERVAL '1 second'), false)
	FROM executions e
	LEFT JOIN pipeline_progress pp ON pp.pipeline_id = e.pipeline_id AND pp.execution_id = e.id
	WHERE e.pipeline_id = $1
	  AND e.status IN ('running', 'pending')
	  AND e.id <> e.pipeline_id
	  AND COALESCE(e.trigger_source, '') <> 'cdc'
	ORDER BY e.start_time DESC`

const workflowNotFound = "not found"

// describeRunWorkflow reports whether the workflow of an execution is still running.
// Every run starts its workflow with the execution id as the workflow id (manual,
// chat and scheduled alike). ended=false with an empty howEnded means "could not
// tell", which callers treat as running. A var so tests can stand in for Temporal.
var describeRunWorkflow = func(ctx context.Context, execID string) (ended bool, howEnded string) {
	tc := getTemporalClient()
	if tc == nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := tc.DescribeWorkflowExecution(ctx, execID, "")
	if err != nil {
		var nf *serviceerror.NotFound
		if errors.As(err, &nf) {
			return true, workflowNotFound
		}
		log.WithError(err).WithField("execution_id", execID).Warn("run overlap: could not describe the run's workflow; treating it as running")
		return false, ""
	}
	status := resp.GetWorkflowExecutionInfo().GetStatus()
	if status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return false, ""
	}
	return true, strings.ToLower(status.String())
}

// classifyOpenRuns sorts the pipeline's open executions (see the top of this file).
func classifyOpenRuns(ctx context.Context, database *sql.DB, pipelineID string) (runOverlap, error) {
	var plan runOverlap
	rows, err := database.QueryContext(ctx, openRunsSQL, pipelineID, int(runStartGrace/time.Second))
	if err != nil {
		return plan, err
	}
	type openRun struct {
		id     string
		parked bool
		recent bool
	}
	var open []openRun
	for rows.Next() {
		var r openRun
		if err := rows.Scan(&r.id, &r.parked, &r.recent); err != nil {
			rows.Close()
			return plan, err
		}
		open = append(open, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}

	for _, r := range open {
		if r.parked {
			plan.Parked = append(plan.Parked, r.id)
			continue
		}
		ended, howEnded := describeRunWorkflow(ctx, r.id)
		if !ended || (howEnded == workflowNotFound && r.recent) {
			plan.Blocking = r.id
			return plan, nil
		}
		plan.Stale = append(plan.Stale, staleRun{ID: r.id, HowEnded: howEnded})
	}
	return plan, nil
}

// refuseOverlappingRun answers 409 when the pipeline already has a run working, and
// 503 when it cannot tell. Otherwise it returns what the new run has to clean up.
// Nothing is written here, so a refused request changes nothing.
func refuseOverlappingRun(c *gin.Context, database *sql.DB, pipelineID string) (runOverlap, bool) {
	plan, refusal := overlapRefusal(c.Request.Context(), database, pipelineID)
	if refusal != nil {
		c.JSON(refusal.status, refusal.body)
		return plan, false
	}
	return plan, true
}

// overlapRefusal is refuseOverlappingRun without the request: the refusal it
// answers with, nil when the run may start.
func overlapRefusal(ctx context.Context, database *sql.DB, pipelineID string) (runOverlap, *pipelineRunRefusal) {
	plan, err := classifyOpenRuns(ctx, database, pipelineID)
	if err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Warn("run overlap: could not read the pipeline's open runs")
		return plan, &pipelineRunRefusal{http.StatusServiceUnavailable, gin.H{
			"error":   "run_check_failed",
			"message": "Could not check whether this pipeline already has a run in progress. Try again shortly.",
		}}
	}
	if plan.Blocking != "" {
		log.WithFields(log.Fields{"pipeline_id": pipelineID, "execution_id": plan.Blocking}).Warn("run overlap: refusing run, one is already in progress")
		return plan, &pipelineRunRefusal{http.StatusConflict, gin.H{
			"error":        "run_in_progress",
			"message":      "This pipeline already has a run in progress. Wait for it to finish or stop it, then run again.",
			"execution_id": plan.Blocking,
		}}
	}
	return plan, nil
}

// applyRunOverlap closes what a new run replaces. It runs once the new run's
// workflow has started, so a run that is refused or fails to start replaces
// nothing, and the replaced workflow's own last writes already see a newer run and
// leave the pipeline to it (see the adapter's writePipelineStatus). Best effort.
func applyRunOverlap(ctx context.Context, database *sql.DB, pipelineID, newExecID string, plan runOverlap) {
	for _, s := range plan.Stale {
		if _, err := database.ExecContext(ctx, `
			UPDATE executions
			SET status = 'failed',
			    end_time = NOW(),
			    error_message = COALESCE(error_message, $2)
			WHERE id = $1 AND status IN ('running', 'pending')
		`, s.ID, "This run stopped without recording a result (workflow "+s.HowEnded+"). Closed when run "+newExecID+" started."); err != nil {
			log.WithError(err).WithField("execution_id", s.ID).Warn("run overlap: could not close a stale run")
		}
	}
	for _, id := range plan.Parked {
		if _, err := database.ExecContext(ctx, `
			UPDATE executions
			SET status = 'cancelled',
			    end_time = NOW(),
			    error_message = $2
			WHERE id = $1 AND status IN ('running', 'pending')
		`, id, "Replaced by run "+newExecID+" while waiting for input."); err != nil {
			log.WithError(err).WithField("execution_id", id).Warn("run overlap: could not close a parked run")
		}
		cancelReplacedRunWorkflow(ctx, pipelineID, id)
	}
}

// cancelReplacedRunWorkflow signals only the replaced run's own workflow. The
// fallback StopPipeline uses also tries the legacy per-pipeline workflow id, which
// could be the run that is starting now. A var so tests can stand in for Temporal.
var cancelReplacedRunWorkflow = func(ctx context.Context, pipelineID, execID string) {
	tc := getTemporalClient()
	if tc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := tc.SignalWorkflow(ctx, execID, "", "cancel", map[string]interface{}{
		"pipeline_id":  pipelineID,
		"execution_id": execID,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"action":       "superseded",
	})
	var nf *serviceerror.NotFound
	if err != nil && !errors.As(err, &nf) {
		log.WithError(err).WithField("execution_id", execID).Warn("run overlap: could not cancel a replaced run's workflow")
	}
}
