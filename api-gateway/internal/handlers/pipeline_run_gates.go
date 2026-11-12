package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/db"
)

// The checks every run of a pipeline passes, whoever starts it: the Run button
// (RunPipeline), a healer retry (RunPipelineInternal), a workflow's
// run_pipeline step (workflowPipelineGate) and a scheduled tick (the adapter's
// ScheduledPipelineRunWorkflow, through CheckScheduledRunInternal).
//
// Only the Run button asks for the pre-migration assessment: its warnings need
// a human to acknowledge them, and no one is there for the other three.

// runQuotaRefusal is the billing gates: the actor's monthly LLM spend, the
// workspace's plan (a trial that has ended) and its monthly GB. Each fails open
// on a read error, as it always has. An empty actor skips the spend check.
func runQuotaRefusal(ctx context.Context, database *sql.DB, wsID, actor string) *pipelineRunRefusal {
	if !checkUserCostQuotaOK(ctx, database, actor) {
		return &pipelineRunRefusal{http.StatusPaymentRequired, gin.H{
			"error":   "quota_exceeded",
			"message": "Monthly LLM spend quota exceeded. Contact admin to raise cost_quota_usd_cents.",
		}}
	}
	if q := resolvePlanQuota(ctx, database, wsID); !q.canRun {
		return &pipelineRunRefusal{http.StatusPaymentRequired, gin.H{
			"error":   "trial_expired",
			"message": "Your free trial has ended. Upgrade to Pro to run pipelines.",
			"plan":    q.plan,
		}}
	}
	if ok, body := checkWorkspaceGBOK(ctx, database, wsID); !ok {
		return &pipelineRunRefusal{http.StatusPaymentRequired, body}
	}
	return nil
}

// runDraftRefusal refuses a run whose source or destination connector is a
// draft, looking both connections up in the pipeline's workspace. An "auto"
// connection is resolved by the run itself, so it is not checked here.
func runDraftRefusal(database *sql.DB, wsID, sourceConnID, destConnID string) *pipelineRunRefusal {
	if sourceConnID == "auto" || destConnID == "auto" {
		return nil
	}
	if body := lifecycleDraftRefusal(database, wsID, sourceConnID, destConnID); body != nil {
		return &pipelineRunRefusal{http.StatusUnprocessableEntity, body}
	}
	return nil
}

// scheduledRunCheck is the answer to "may this scheduled tick start a run?".
type scheduledRunCheck struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

const scheduledRunPipelineSQL = `SELECT workspace_id::text, COALESCE(created_by::text, ''),
	COALESCE(source_connection_id::text, ''), COALESCE(destination_connection_id::text, ''), default_run_mode
	FROM pipelines WHERE id = $1`

// checkScheduledRun is the Run button's checks for a scheduled tick of
// pipeline id: the billing gates as the pipeline's creator (a scheduled run's
// LLM spend is the creator's: FetchPipelineRunContextActivity), the draft gate,
// and a Reload into an app's API. The overlap guard and the parked-run skip
// are the adapter's own (CheckActiveRunActivity).
//
// A deleted pipeline is refused, not an error. An error is a failure to
// decide; the adapter then lets the tick run, as each gate does on a read error.
func checkScheduledRun(ctx context.Context, database *sql.DB, id string) (scheduledRunCheck, error) {
	var wsID, createdBy, srcConn, dstConn string
	var defaultRunMode *string
	err := database.QueryRowContext(ctx, scheduledRunPipelineSQL, id).
		Scan(&wsID, &createdBy, &srcConn, &dstConn, &defaultRunMode)
	if errors.Is(err, sql.ErrNoRows) {
		return scheduledRunCheck{Reason: "pipeline_not_found", Message: "The pipeline no longer exists."}, nil
	}
	if err != nil {
		return scheduledRunCheck{}, err
	}

	ref := runQuotaRefusal(ctx, database, wsID, createdBy)
	if ref == nil {
		ref = runDraftRefusal(database, wsID, srcConn, dstConn)
	}
	if ref == nil {
		if runMode := resolveRunMode("", "", defaultRunMode); runMode == "reload" && dstConn != "" {
			var destType string
			_ = database.QueryRowContext(ctx, `SELECT connector_type FROM connections WHERE id = $1`, dstConn).Scan(&destType)
			ref = apiDestinationReloadRefusal(runMode, destType, true)
		}
	}
	if ref == nil {
		return scheduledRunCheck{Allowed: true}, nil
	}
	reason, _ := ref.body["error"].(string)
	// A scheduled run has no ?allow_draft=true to pass.
	msg := strings.Replace(refusalMessage(ref.body, reason), draftOverrideHint, "", 1)
	return scheduledRunCheck{Reason: reason, Message: msg}, nil
}

// CheckScheduledRunInternal answers the adapter's scheduled wrapper before
// each tick: 200 with the decision either way, 500 when it cannot decide.
// Registered on the internal group (InternalServiceMiddleware).
func CheckScheduledRunInternal(c *gin.Context) {
	id, ok := requireUUIDParam(c, "id", "invalid_pipeline_id", "Invalid pipeline ID format")
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Database not connected"})
		return
	}
	check, err := checkScheduledRun(c.Request.Context(), database, id)
	if err != nil {
		log.WithError(err).WithField("pipeline_id", id).Warn("scheduled run check: could not read the pipeline")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "run_check_failed", "message": "Could not read the pipeline."})
		return
	}
	c.JSON(http.StatusOK, check)
}
