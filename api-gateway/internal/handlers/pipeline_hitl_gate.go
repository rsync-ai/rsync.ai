package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	log "github.com/sirupsen/logrus"
)

// ============================================================================
// One answer per pause, however many tabs show it
// ============================================================================
//
// The same pause shows in more than one place: the chat that built the pipeline
// and the pipeline's own page, in as many tabs as the user has open. Answer it in
// one and the others keep their dialog open. Their Apply used to signal the run
// regardless, and three things then went wrong (pipeline a3b07ece, 2026-10-01):
//
//   - the run had finished, the signal failed, and the handler wrote 'failed' to
//     the execution, the pipeline and its progress row: a completed run shown as
//     failed, because a second tab was late;
//   - a run that had moved on would take the late answer at its NEXT pause of the
//     same kind, because a signal nobody is waiting for sits in the channel;
//   - the duplicate-submit claim could not stop either: it lives in
//     pipeline_progress.metadata, which the adapter's StateUpdateActivity replaces
//     wholesale, so it was gone by the time the late tab submitted.
//
// So before anything is written or signalled the gate asks the run itself
// (gateHITLResume): is this execution this pipeline's, is its workflow still
// running, and is it parked at this kind of pause and listening right now. The
// last question is the adapter's hitl_wait query (nl_pipeline_v2_hitl_query.go),
// whose answer also scopes the duplicate claim to one receive of one pause.
//
// Nothing here records an outcome. The gateway did not see how the run ended, so
// it no longer writes one; a run that ended without recording a result is closed
// honestly by the next run's overlap check (pipeline_run_overlap.go).

// hitlWaitQuery is the adapter's HITLWaitQuery, spelled again because the modules
// share no types. TestTheGatewaySpellsTheHITLWaitQueryTheAdapterRegisters holds
// the two together.
const hitlWaitQuery = "hitl_wait"

// hitlKind is a pause as pipeline_progress.blocking_reason_type names it.
type hitlKind string

const (
	hitlKindTables      hitlKind = "table_selection"
	hitlKindConnections hitlKind = "connection_config"
	hitlKindConnectors  hitlKind = "connector_generation"
	hitlKindNodeInput   hitlKind = "node_input"
)

// hitlWaitTypeOf is the adapter's WaitReasonType for each pause a resume
// endpoint answers. Node input has none: it belongs to DAG mode, which is off, so
// its gate stops at the ownership and run checks.
var hitlWaitTypeOf = map[hitlKind]string{
	hitlKindTables:      "TABLE_SELECTION",
	hitlKindConnections: "CONNECTION_CONFIGURATION",
	hitlKindConnectors:  "CONNECTOR_GENERATION",
}

// hitlWaitAnswer mirrors the adapter's HITLWaitState.
type hitlWaitAnswer struct {
	Type         string    `json:"type,omitempty"`
	WaitingSince time.Time `json:"waiting_since,omitempty"`
	TimeoutAt    time.Time `json:"timeout_at,omitempty"`
	NodeID       string    `json:"node_id,omitempty"`
	Receiving    bool      `json:"receiving"`
	ReceiveSeq   int       `json:"receive_seq"`
}

// errHITLRunNotOpen: Temporal refused the query because the run is closed.
var errHITLRunNotOpen = errors.New("the run is not open")

const hitlWaitQueryTimeout = 3 * time.Second

// queryHITLWait asks a run which pause it is in. Any error other than
// errHITLRunNotOpen means "could not ask" (a worker still on code without the
// query, a slow worker, Temporal down), and the gate falls back to the database.
// A var so tests can stand in for Temporal.
var queryHITLWait = func(ctx context.Context, execID string) (hitlWaitAnswer, error) {
	var ans hitlWaitAnswer
	tc := getTemporalClient()
	if tc == nil {
		return ans, errors.New("temporal client unavailable")
	}
	qctx, cancel := context.WithTimeout(ctx, hitlWaitQueryTimeout)
	defer cancel()
	resp, err := tc.QueryWorkflowWithOptions(qctx, &client.QueryWorkflowWithOptionsRequest{
		WorkflowID: execID,
		QueryType:  hitlWaitQuery,
		// A closed run would otherwise answer from history: its last pause, open.
		QueryRejectCondition: enumspb.QUERY_REJECT_CONDITION_NOT_OPEN,
	})
	if err != nil {
		return ans, err
	}
	if resp.QueryRejected != nil {
		return ans, errHITLRunNotOpen
	}
	if resp.QueryResult == nil {
		return ans, errors.New("empty hitl_wait answer")
	}
	if err := resp.QueryResult.Get(&ans); err != nil {
		return ans, fmt.Errorf("decode %s: %w", hitlWaitQuery, err)
	}
	return ans, nil
}

// hitlRunRow is what the database says about the execution being answered.
type hitlRunRow struct {
	execStatus   string
	progressIsIt bool // pipeline_progress points at this execution
	progress     string
	blocking     string
}

const hitlRunRowSQL = `
	SELECT e.status,
	       COALESCE(pp.execution_id = e.id, false),
	       COALESCE(pp.status, ''),
	       COALESCE(pp.blocking_reason_type, '')
	FROM executions e
	LEFT JOIN pipeline_progress pp ON pp.pipeline_id = e.pipeline_id
	WHERE e.id = $1 AND e.pipeline_id = $2`

// gateHITLResume decides whether an answer to a pause of this kind may reach the
// run. It writes nothing and signals nothing when it refuses, and it runs before
// the handler's own side effects, so a refused answer changes nothing.
//
// pauseKey names the one receive being answered, for the duplicate-submit claim.
// It is empty when the gate could not scope the answer that finely; the caller
// then skips the claim rather than claim something too broad.
func gateHITLResume(c *gin.Context, database *sql.DB, pipelineID, execID string, kind hitlKind) (pauseKey string, ok bool) {
	// Not on the request's context: a hang-up mid-gate failed the ask to the run, and
	// the answer was then let through on the progress row's weaker word.
	ctx := context.WithoutCancel(c.Request.Context())
	notFound := func() {
		c.JSON(http.StatusNotFound, gin.H{"error": "execution_not_found", "message": "This run does not belong to this pipeline."})
	}

	// A client-supplied execution_id is checked against the pipeline the caller has
	// a role on: without this, a member could answer another pipeline's pause.
	if _, err := uuid.Parse(execID); err != nil {
		notFound()
		return "", false
	}
	var row hitlRunRow
	if err := database.QueryRowContext(ctx, hitlRunRowSQL, execID, pipelineID).Scan(
		&row.execStatus, &row.progressIsIt, &row.progress, &row.blocking,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFound()
			return "", false
		}
		log.WithContext(ctx).WithError(err).WithField("execution_id", execID).Error("HITL gate: could not read the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "run_lookup_failed", "message": "Could not check the run. Try again."})
		return "", false
	}

	if ended, howEnded := describeRunWorkflow(ctx, execID); ended {
		respondHITLRunFinished(c, database, pipelineID, execID, howEnded)
		return "", false
	}

	want, asks := hitlWaitTypeOf[kind]
	if !asks {
		return "", true
	}

	ans, err := queryHITLWait(ctx, execID)
	switch {
	case errors.Is(err, errHITLRunNotOpen):
		respondHITLRunFinished(c, database, pipelineID, execID, "closed")
		return "", false
	case err == nil:
		if ans.Type != want {
			respondHITLPauseAnswered(c, pipelineID, execID, ans.Type)
			return "", false
		}
		if !ans.Receiving {
			// Parked at this pause but not listening: the last answer to it is being
			// checked (a connection fix under validation), or the pause is still being
			// announced. An answer sent now would wait in the channel for whichever
			// receive comes next.
			respondHITLConflict(c, pipelineID, execID, gin.H{
				"error":        "pause_busy",
				"message":      "The run is still working on the last answer to this step. Try again in a moment.",
				"execution_id": execID,
				"retryable":    true,
			})
			return "", false
		}
		return fmt.Sprintf("%s|%s|%s|%d", execID, kind, ans.WaitingSince.UTC().Format(time.RFC3339Nano), ans.ReceiveSeq), true
	}

	// Could not ask the run. The progress row is the next best witness: the run
	// writes it before it starts listening, and a successful answer clears it.
	log.WithContext(ctx).WithError(err).WithField("execution_id", execID).Warn("HITL gate: hitl_wait query failed; checking the progress row instead")
	atThisPause := row.blocking == string(kind) || (kind == hitlKindTables && isTableSelectionType(row.blocking))
	if !row.progressIsIt || row.progress != "waiting_for_user" || !atThisPause {
		respondHITLPauseAnswered(c, pipelineID, execID, row.blocking)
		return "", false
	}
	if kind == hitlKindTables {
		// A table selection is answered once per run, so the run scopes it. A
		// connection fix is not (validation retries reuse the pause), so it gets no
		// claim from the row.
		return fmt.Sprintf("%s|%s|db", execID, kind), true
	}
	return "", true
}

// respondHITLConflict refuses an answer with 409 and logs the refusal: the user
// sees "my answer did nothing", and without this line the gateway kept no trace
// of why. It logs the pipeline, the run answered and the code, never what the
// user picked. Every HITL 409 goes through here.
func respondHITLConflict(c *gin.Context, pipelineID, execID string, body gin.H) {
	log.WithContext(c.Request.Context()).WithFields(log.Fields{
		"pipeline_id":  pipelineID,
		"execution_id": execID,
		"code":         body["error"],
	}).Info("HITL: answer refused (409)")
	c.JSON(http.StatusConflict, body)
}

// respondHITLPauseAnswered: the run is alive but not at this pause.
func respondHITLPauseAnswered(c *gin.Context, pipelineID, execID, waitingFor string) {
	respondHITLConflict(c, pipelineID, execID, gin.H{
		"error":        "pause_already_answered",
		"message":      "This step was already answered, in another tab or by someone else, and the run has moved on.",
		"execution_id": execID,
		"waiting_for":  waitingFor,
	})
}

// respondHITLRunFinished answers a pause whose run has ended. The only write is
// clearing a pause the run left on its progress row, and only that run's pause:
// otherwise every open tab would keep reopening a dialog nothing can answer.
func respondHITLRunFinished(c *gin.Context, database *sql.DB, pipelineID, execID, howEnded string) {
	clearEndedRunPause(context.WithoutCancel(c.Request.Context()), database, pipelineID, execID, howEnded)
	respondHITLConflict(c, pipelineID, execID, gin.H{
		"error":        "run_finished",
		"message":      "This run has already finished, so it is no longer waiting for this answer.",
		"execution_id": execID,
		"how_ended":    howEnded,
	})
}

// endedRunProgressStatus maps how a workflow ended to the progress status a
// pause it left behind should not outlive.
func endedRunProgressStatus(howEnded string) string {
	switch howEnded {
	case "completed":
		return "completed"
	case "canceled":
		return "cancelled"
	}
	return "failed"
}

func clearEndedRunPause(ctx context.Context, database *sql.DB, pipelineID, execID, howEnded string) {
	if _, err := database.ExecContext(ctx, `
		UPDATE pipeline_progress
		SET status = $3,
		    blocking_reason_type = NULL,
		    blocking_reason_description = NULL,
		    blocking_reason_estimated_seconds = NULL,
		    message = $4,
		    updated_at = NOW()
		WHERE pipeline_id = $1
		  AND execution_id = $2
		  AND status = 'waiting_for_user'
	`, pipelineID, execID, endedRunProgressStatus(howEnded),
		"This run is no longer waiting for input (workflow "+howEnded+")."); err != nil {
		log.WithContext(ctx).WithError(err).WithField("execution_id", execID).Warn("HITL gate: could not clear an ended run's pause (ignored)")
	}
}

// claimHITLPause takes the duplicate-submit claim for one receive of a
// connection or connector pause. claimed=false with no error means another
// request holds it; the caller answers 409.
//
// The table selection keeps its own claim (hitl_tables_selected), which also
// carries the selection so a double-click can be told apart from a conflict.
func claimHITLPause(ctx context.Context, database *sql.DB, pipelineID, execID string, kind hitlKind, pauseKey string) (bool, error) {
	claim, err := json.Marshal(map[string]interface{}{
		"pause":        pauseKey,
		"kind":         string(kind),
		"execution_id": execID,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return false, err
	}
	res, err := database.ExecContext(ctx, `
		UPDATE pipeline_progress
		SET metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{hitl_pause_claim}', $1::jsonb, true),
		    updated_at = NOW()
		WHERE pipeline_id = $2
		  AND COALESCE(metadata->'hitl_pause_claim'->>'pause', '') <> $3
	`, string(claim), pipelineID, pauseKey)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// No row to hold a claim on is not a competing claim.
	var exists bool
	if err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pipeline_progress WHERE pipeline_id = $1)`, pipelineID).Scan(&exists); err != nil {
		return false, err
	}
	return !exists, nil
}

// releaseHITLPauseClaim drops this request's claim after a failed signal, so the
// user can try again.
func releaseHITLPauseClaim(ctx context.Context, database *sql.DB, pipelineID, pauseKey string) {
	if pauseKey == "" {
		return
	}
	if _, err := database.ExecContext(ctx, `
		UPDATE pipeline_progress
		SET metadata = metadata - 'hitl_pause_claim',
		    updated_at = NOW()
		WHERE pipeline_id = $1
		  AND metadata->'hitl_pause_claim'->>'pause' = $2
	`, pipelineID, pauseKey); err != nil {
		log.WithContext(ctx).WithError(err).WithField("pipeline_id", pipelineID).Warn("HITL: could not release the pause claim (ignored)")
	}
}

func respondHITLAnswerInFlight(c *gin.Context, pipelineID, execID string) {
	respondHITLConflict(c, pipelineID, execID, gin.H{
		"error":        "hitl_answer_in_flight",
		"message":      "This step is already being answered. Wait for it to finish.",
		"execution_id": execID,
	})
}

// respondHITLSignalFailed answers a signal that failed after the gate let it
// through. The run may have ended in between; anything else is a retryable error.
func respondHITLSignalFailed(c *gin.Context, database *sql.DB, pipelineID, execID, signal string, err error) {
	log.WithContext(c.Request.Context()).WithError(err).WithFields(log.Fields{
		"pipeline_id":  pipelineID,
		"execution_id": execID,
	}).Error("Failed to signal workflow: " + signal)

	ended, howEnded := describeRunWorkflow(context.WithoutCancel(c.Request.Context()), execID)
	if !ended && (isWorkflowAlreadyCompletedError(err) || isWorkflowNotFoundError(err)) {
		ended, howEnded = true, "closed"
	}
	if ended {
		respondHITLRunFinished(c, database, pipelineID, execID, howEnded)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_signal_workflow", "message": "Could not reach the run. Try again."})
}
