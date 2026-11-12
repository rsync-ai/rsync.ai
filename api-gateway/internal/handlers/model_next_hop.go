package handlers

import (
	"database/sql"
	"errors"
	"net/http"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// ============================================================================
// The next hop — a finished model run wakes the models below it
// ============================================================================
// A model run used to end here: the run endpoint rebuilt the model, wrote its history
// row and then, on a goroutine of the request, looked up the models waiting on it and
// started their fan-out. A gateway restart between the row and the fan-out lost the
// hop, and nothing said so.
//
// The run's own workflow owns the hop now (workflows-design.md § 4.2, the adapter's
// model_next_hop.go). It sends next_hop "caller" with the run; this gateway answers
// with the run's history row instead of firing anything; the workflow then asks
// /downstream who is below that row and starts their fan-out itself. Each of those
// steps is in the workflow's history, so a restart of either service between them
// resumes from the last one recorded instead of losing the hop.

// modelNextHopCaller is the next_hop value that hands the hop to the caller. The wire
// contract with the adapter's ModelNextHopCaller; the two modules share no types.
const modelNextHopCaller = "caller"

// leavesHopToCaller reports whether the run endpoint answers with the run row instead of
// firing the models below. Only when the caller asked AND there is a row to hand back:
// without the row the caller could not list the models below, so the gateway keeps the
// hop rather than drop it.
func leavesHopToCaller(req internalModelRunRequest, runID string) bool {
	return req.NextHop == modelNextHopCaller && runID != ""
}

// modelRunHopResult is the run endpoint's answer when the caller takes the hop: the run's
// own fields where they always were, with its history row and the echoed next_hop beside
// them. The echo is how the adapter tells this gateway from one that predates next_hop
// and fired the hop itself.
type modelRunHopResult struct {
	*modelRunResult
	RunID   string `json:"run_id,omitempty"`
	NextHop string `json:"next_hop,omitempty"`
}

// answerInternalModelRun ends an internal run: it hands the hop back to the workflow
// that asked for it, or takes it here.
func answerInternalModelRun(c *gin.Context, req internalModelRunRequest, res *modelRunResult, id, runID string) {
	if leavesHopToCaller(req, runID) {
		// The workflow wakes the models below from its own history, through /downstream,
		// so a gateway restart after this answer cannot lose the hop.
		c.JSON(http.StatusOK, modelRunHopResult{modelRunResult: res, RunID: runID, NextHop: modelNextHopCaller})
		return
	}
	// An adapter that predates next_hop, or a run with no row to hand back. req.Depth is
	// what the signal carried, so the chain is bounded by the same number either way, and
	// the execution id rides along so every hop remembers the pipeline run that started it.
	fireAfterInternalRun(res, id, runID, req.ExecutionID, req.Depth)
	c.JSON(http.StatusOK, res)
}

// fireAfterInternalRun is a seam so a test can see whether the gateway took the hop.
var fireAfterInternalRun = fireDownstreamModelsAfterRun

// modelDownstreamRequest names the finished run whose downstream is wanted, and the
// chain it belongs to as the run carried it.
type modelDownstreamRequest struct {
	RunID       string `json:"run_id"`
	ExecutionID string `json:"execution_id"`
	Depth       int    `json:"depth"`
}

// ListSavedQueryModelDownstreamInternal answers the adapter's ListDownstreamModelsActivity:
// what a finished run of this model wakes.
//
// It answers from the run's history row, not from anything the caller says about the
// run, so the rules are the ones fireDownstreamModelsAfterRun applies on the gateway's
// own path:
//   - a succeeded run answers the fan-out to start, already in the adapter's
//     UpstreamFanOutInput shape (upstreamFanOutArgs), one hop deeper;
//   - past the chain's depth bound it records the models it stopped short of and starts
//     nothing (G7);
//   - a failed or auto-paused run records the skip cascade below it and starts nothing
//     (G3); a transient skip tells the models below nothing.
//
// The writes happen once per run: an activity retry after a response that never arrived
// finds the skip rows already linked to this run and writes nothing. Every database
// error is a 500, so the activity retries it — an empty answer would read as "nothing
// below" and lose the hop with no error anywhere.
//
// POST /api/v1/internal/explorer/models/:id/downstream, behind the internal secret.
func ListSavedQueryModelDownstreamInternal(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	var req modelDownstreamRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if _, err := uuid.Parse(req.RunID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid run id"})
		return
	}
	if req.Depth < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid depth"})
		return
	}

	ctx := c.Request.Context()
	fields := log.Fields{"model_id": id, "run_id": req.RunID}
	// The root the models below are woken or skipped from: this run, one hop deeper.
	root := modelRefreshSource{
		Kind:        upstreamKindModel,
		ID:          id,
		RunID:       req.RunID,
		ExecutionID: req.ExecutionID,
		Depth:       req.Depth + 1,
	}
	nothingBelow := upstreamFanOutArgs(upstreamFanOut{Source: root})

	// A run a data check blocked is told apart by its error, which is that one fixed
	// sentence and nothing else (modelCheckBlockedError).
	var res modelRunResult
	err := database.QueryRowContext(ctx, `
		SELECT status, COALESCE(auto_pause_reason, ''), COALESCE(error, '') = $3
		FROM saved_query_runs
		WHERE run_id = $1::uuid AND saved_query_id = $2::uuid`,
		req.RunID, id, modelCheckBlockedError).Scan(&res.Status, &res.AutoPauseReason, &res.CheckBlocked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Matched on both ids, so one model's run cannot wake another model's consumers.
		log.WithFields(fields).Warn("next hop: no run of this model by that id; waking nothing")
		c.JSON(http.StatusOK, nothingBelow)
		return
	case err != nil:
		log.WithError(err).WithFields(fields).Error("next hop: could not read the run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read the run"})
		return
	}

	if src := nextChainSource(&res, id, req.RunID, req.ExecutionID, req.Depth); src != nil {
		if chainDepthExceeded(src.Depth) {
			// Info, not Warn: the chain_depth_exceeded rows below are the record of it.
			log.WithFields(fields).WithField("depth", src.Depth).
				Info("next hop: rebuild chain hit its depth bound, stopping here")
			if !recordSkipsOnce(c, database, req.RunID, func() error {
				return recordChainDepthSkips(ctx, database, *src)
			}) {
				return
			}
			c.JSON(http.StatusOK, nothingBelow)
			return
		}
		targets, err := lookupDownstreamModels(ctx, database, src.Kind, src.ID)
		if err != nil {
			log.WithError(err).WithFields(fields).Error("next hop: could not look up the models below")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not look up the models below"})
			return
		}
		c.JSON(http.StatusOK, upstreamFanOutArgs(upstreamFanOut{Source: *src, Targets: targets}))
		return
	}

	reason := cascadeSkipReason(&res)
	if reason == "" {
		c.JSON(http.StatusOK, nothingBelow)
		return
	}
	if !recordSkipsOnce(c, database, req.RunID, func() error {
		return recordSkippedDownstream(ctx, database, root, reason)
	}) {
		return
	}
	c.JSON(http.StatusOK, nothingBelow)
}

// recordSkipsOnce runs write unless skip rows linked to runID already exist, answering
// 500 and returning false on any error. The rows carry no key of their own, so the link
// to the run above them is what makes a retried call write nothing the second time.
func recordSkipsOnce(c *gin.Context, database *sql.DB, runID string, write func() error) bool {
	var recorded bool
	if err := database.QueryRowContext(c.Request.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM saved_query_runs
			WHERE upstream_run_id = $1::uuid AND status = 'skipped'
		)`, runID).Scan(&recorded); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("next hop: could not check for skips already recorded")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not check the models below"})
		return false
	}
	if recorded {
		return true
	}
	if err := write(); err != nil {
		log.WithError(err).WithField("run_id", runID).Error("next hop: could not record the models below")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not record the models below"})
		return false
	}
	return true
}
