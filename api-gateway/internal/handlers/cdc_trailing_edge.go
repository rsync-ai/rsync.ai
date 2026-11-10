package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/db"
)

// "Run after pipeline" for a CDC pipeline, on a window's trailing edge
// (docs/internal/workflows-design.md § 6).
//
// A batch pipeline fires its models and workflows on PIPELINE_COMPLETED. A CDC pipeline
// never completes, so the equivalent is "rows landed", and the sink records each
// landing in cdc_table_watermarks.landed_at (migration 137) on the database's clock.
// One sweep, driven by a single Temporal timer workflow (CDCTrailingEdgeWorkflow), reads
// every CDC pipeline's newest landing and fires at the first window boundary after a
// landing, at most once per window:
//
//   - a burst followed by silence builds its tail once, at the boundary after the burst;
//   - a continuous stream builds once per window;
//   - an idle pipeline builds nothing.
//
// It replaced a leading edge in the event projector, which fired on the first landing
// in a window: rows landing later in that window waited for the next landing to be
// built, and a burst that ended mid-window was never built in full.
//
// The occurrence (cdc-trail-<N>m-<window>) stands in for an execution id, so the fan-out
// workflow id and the workflow_runs (workflow_id, trigger_ref) index make a fire
// idempotent. The state row is written after the fire: a crash in between fires again
// with the same occurrence, and both drop the repeat.
//
// Known limits:
//   - a landing just before a boundary that the sweep first reads after it, behind a
//     newer landing past the boundary, is built at the next boundary: a window late,
//     never lost;
//   - until the kafka-mcp-sink that writes cdc_table_watermarks is deployed, no landing
//     is recorded and nothing fires.

// CDCAfterPipelineIntervalEnv names the env var that sets the CDC window, in minutes.
const CDCAfterPipelineIntervalEnv = "CDC_AFTER_PIPELINE_INTERVAL_MINUTES"

const defaultCDCAfterPipelineInterval = 15 * time.Minute

// cdcAfterPipelineInterval reads CDC_AFTER_PIPELINE_INTERVAL_MINUTES. Unset means the
// default; anything that is not a positive whole number of minutes is logged and also
// means the default, rather than a window of zero that would fire on every sweep.
func cdcAfterPipelineInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv(CDCAfterPipelineIntervalEnv))
	if v == "" {
		return defaultCDCAfterPipelineInterval
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		if len(v) > 32 {
			v = v[:32]
		}
		log.WithFields(log.Fields{"env": CDCAfterPipelineIntervalEnv, "value": v}).
			Warn("CDC trailing edge: invalid interval, using the default of 15 minutes")
		return defaultCDCAfterPipelineInterval
	}
	return time.Duration(n) * time.Minute
}

// cdcWindowBoundary is the start of the window holding now: the latest boundary at or
// before it, on whole multiples of the window since the Unix epoch.
func cdcWindowBoundary(now time.Time, window time.Duration) time.Time {
	secs := int64(window / time.Second)
	if secs <= 0 {
		secs = int64(defaultCDCAfterPipelineInterval / time.Second)
	}
	u := now.Unix()
	b := u - u%secs
	if u < 0 && u%secs != 0 {
		b -= secs // floor, not truncation
	}
	return time.Unix(b, 0).UTC()
}

// cdcTrailingEdgeOccurrence labels the fire at boundary. It names the window size as
// well as the window, so changing the interval cannot collide with an earlier label,
// and its prefix differs from the leading edge's cdc-window-, so the first fire after
// the switch cannot be dropped as a duplicate of a leading-edge one.
func cdcTrailingEdgeOccurrence(window time.Duration, boundary time.Time) string {
	secs := int64(window / time.Second)
	if secs <= 0 {
		secs = int64(defaultCDCAfterPipelineInterval / time.Second)
	}
	return fmt.Sprintf("cdc-trail-%dm-%d", secs/60, boundary.Unix()/secs)
}

// cdcTrailingEdgeState is a pipeline's cdc_trailing_edge_state row; all NULL when it
// has never fired or been seen.
type cdcTrailingEdgeState struct {
	WindowEnd     sql.NullTime // the boundary it last fired at
	LandedThrough sql.NullTime // the newest landing that fire covered
	PendingSince  sql.NullTime // the newest landing seen by the first sweep after it
}

// cdcTrailingEdgeStep is what one sweep does for one pipeline.
type cdcTrailingEdgeStep struct {
	Fire        bool
	MarkPending bool
	Boundary    time.Time
}

// decideCDCTrailingEdge decides one pipeline's sweep from its newest landing (on the
// database clock), its state, and the database's now.
//
// It fires when some landing not yet covered happened before the current boundary and
// it has not fired at this boundary. Two readings show such a landing: the newest one
// itself, when it is before the boundary; or one an earlier sweep saw and parked in
// PendingSince, which is what still shows it when the stream kept landing past the
// boundary and the newest landing is after it. Otherwise a landing not yet covered is
// parked, once. A parked landing the last fire already covered (a sweep that overlapped
// a fire) counts as none, so it is parked afresh rather than blocking every later
// window. Pure.
func decideCDCTrailingEdge(landedAt time.Time, s cdcTrailingEdgeState, now time.Time, window time.Duration) cdcTrailingEdgeStep {
	boundary := cdcWindowBoundary(now, window)
	uncovered := func(t time.Time) bool { return !s.LandedThrough.Valid || t.After(s.LandedThrough.Time) }
	newer := uncovered(landedAt)
	parked := s.PendingSince.Valid && uncovered(s.PendingSince.Time)
	evidence := (newer && landedAt.Before(boundary)) || (parked && s.PendingSince.Time.Before(boundary))
	if evidence && (!s.WindowEnd.Valid || boundary.After(s.WindowEnd.Time)) {
		return cdcTrailingEdgeStep{Fire: true, Boundary: boundary}
	}
	return cdcTrailingEdgeStep{MarkPending: newer && !parked, Boundary: boundary}
}

// cdcTrailingEdgeFire fires what runs after pipelineID, for occurrence. A nil error
// means the fire is settled — started, already started, or recorded as trigger_lost —
// and the sweep may record it.
type cdcTrailingEdgeFire func(ctx context.Context, database *sql.DB, pipelineID, occurrence string) error

// cdcTrailingEdgeSweepResult is what one sweep did, for the timer's log.
type cdcTrailingEdgeSweepResult struct {
	Scanned int `json:"scanned"`
	Fired   int `json:"fired"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

// cdcTrailingEdgeRecordFireSQL records a settled fire ($1 pipeline, $2 boundary, $3 the
// landing it covered). Only forward: a slower sweep that fired an earlier boundary
// cannot pull the state back behind a later one.
const cdcTrailingEdgeRecordFireSQL = `
	INSERT INTO cdc_trailing_edge_state (pipeline_id, window_end, landed_through, pending_since, updated_at)
	VALUES ($1::uuid, $2, $3, NULL, now())
	ON CONFLICT (pipeline_id) DO UPDATE SET
		window_end = EXCLUDED.window_end,
		landed_through = EXCLUDED.landed_through,
		pending_since = NULL,
		updated_at = now()
	WHERE cdc_trailing_edge_state.window_end IS NULL
	   OR cdc_trailing_edge_state.window_end <= EXCLUDED.window_end`

// cdcTrailingEdgeParkSQL parks a landing ($1 pipeline, $2 landing). It keeps a parked
// landing the last fire has not covered and replaces one it has, as
// decideCDCTrailingEdge reads them.
const cdcTrailingEdgeParkSQL = `
	INSERT INTO cdc_trailing_edge_state (pipeline_id, pending_since, updated_at)
	VALUES ($1::uuid, $2, now())
	ON CONFLICT (pipeline_id) DO UPDATE SET
		pending_since = CASE
			WHEN cdc_trailing_edge_state.pending_since IS NULL
			  OR cdc_trailing_edge_state.pending_since <= cdc_trailing_edge_state.landed_through
			THEN EXCLUDED.pending_since
			ELSE cdc_trailing_edge_state.pending_since END,
		updated_at = now()`

// sweepCDCTrailingEdge runs one sweep over every CDC pipeline with a recorded landing.
// at, when valid, stands in for the database's now() (tests); production passes the
// zero value, so landed_at and now are always read off the same clock.
func sweepCDCTrailingEdge(ctx context.Context, database *sql.DB, window time.Duration, at sql.NullTime, fire cdcTrailingEdgeFire) (cdcTrailingEdgeSweepResult, error) {
	var res cdcTrailingEdgeSweepResult
	rows, err := database.QueryContext(ctx, `
		WITH clock AS (SELECT COALESCE($1::timestamptz, now()) AS now),
		landed AS (
			SELECT pipeline_id, MAX(landed_at) AS landed_at
			FROM cdc_table_watermarks
			GROUP BY pipeline_id
		)
		SELECT p.id::text, l.landed_at, s.window_end, s.landed_through, s.pending_since, clock.now
		FROM landed l
		JOIN pipelines p ON p.id = l.pipeline_id AND `+pipelineRowIsCDCSQL+`
		LEFT JOIN cdc_trailing_edge_state s ON s.pipeline_id = l.pipeline_id
		CROSS JOIN clock
		ORDER BY 1
	`, at)
	if err != nil {
		return res, err
	}
	type row struct {
		pipelineID string
		landedAt   time.Time
		state      cdcTrailingEdgeState
		now        time.Time
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.pipelineID, &r.landedAt, &r.state.WindowEnd, &r.state.LandedThrough, &r.state.PendingSince, &r.now); err != nil {
			rows.Close()
			return res, err
		}
		todo = append(todo, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	res.Scanned = len(todo)
	for _, r := range todo {
		step := decideCDCTrailingEdge(r.landedAt, r.state, r.now, window)
		fields := log.Fields{"pipeline_id": r.pipelineID, "boundary": step.Boundary}
		switch {
		case step.Fire:
			occurrence := cdcTrailingEdgeOccurrence(window, step.Boundary)
			if err := fire(ctx, database, r.pipelineID, occurrence); err != nil {
				// Not recorded, so the next sweep fires again with the same occurrence
				// while the window lasts.
				res.Failed++
				log.WithError(err).WithFields(fields).Warn("CDC trailing edge: fire failed; the next sweep retries")
				continue
			}
			if _, err := database.ExecContext(ctx, cdcTrailingEdgeRecordFireSQL, r.pipelineID, step.Boundary, r.landedAt); err != nil {
				res.Failed++
				log.WithError(err).WithFields(fields).Warn("CDC trailing edge: fired but could not record it; the next sweep repeats it with the same occurrence")
				continue
			}
			res.Fired++
			log.WithFields(fields).WithField("occurrence", occurrence).Info("CDC trailing edge: rows landed before the boundary; fired what runs after this pipeline")
		case step.MarkPending:
			if _, err := database.ExecContext(ctx, cdcTrailingEdgeParkSQL, r.pipelineID, r.landedAt); err != nil {
				res.Failed++
				log.WithError(err).WithFields(fields).Warn("CDC trailing edge: could not park a landing")
				continue
			}
			res.Pending++
		}
	}
	return res, nil
}

// fireAfterCDCWindow fires a CDC pipeline's downstream models and its after-pipeline
// workflows for one window, synchronously, so the sweep records the fire only once it
// is settled. The models' fan-out is the batch completion's (deliverFanOut: retried,
// then trigger_lost); the workflows insert their runs and hand them to Temporal. Both
// are attempted; the first error is returned. A run Temporal did not take stays
// running and fails the fire, so the next sweep repeats this occurrence, finds the
// run, and starts it then.
func fireAfterCDCWindow(ctx context.Context, database *sql.DB, pipelineID, occurrence string) error {
	var firstErr error
	src := modelRefreshSource{Kind: upstreamKindPipeline, ID: pipelineID, ExecutionID: occurrence}
	targets, err := lookupDownstreamModels(ctx, database, src.Kind, src.ID)
	if err != nil {
		firstErr = fmt.Errorf("look up downstream models: %w", err)
	} else if len(targets) > 0 {
		log.WithFields(log.Fields{"upstream_id": pipelineID, "occurrence": occurrence, "models": len(targets)}).
			Info("⚡ CDC window closed: waking downstream models")
		deliverFanOut(database, fanOutTemporalBreaker, newUpstreamFanOutDispatch, upstreamFanOut{
			Occurrence: occurrence,
			Source:     src,
			Targets:    targets,
		})
	}
	runs, err := fireWorkflowsAfterPipeline(ctx, database, pipelineID, occurrence)
	if err != nil && !isUndefinedTable(err) && firstErr == nil {
		firstErr = fmt.Errorf("fire after-pipeline workflows: %w", err)
	}
	if notStarted := startWorkflowRuns(ctx, fanOutTemporalBreaker, runs); len(notStarted) > 0 && firstErr == nil {
		firstErr = fmt.Errorf("start %d after-pipeline workflow runs: Temporal was not reachable", len(notStarted))
	}
	return firstErr
}

// SweepCDCTrailingEdgeInternal is the CDC trailing-edge timer's landing point.
// POST /api/v1/internal/cdc/trailing-edge/sweep
//
// Internal and cross-workspace for the reason the freshness sweep is: the pipelines it
// serves are the ones nobody is watching. Safe to call twice — a repeated fire carries
// the same occurrence, which the fan-out and workflow_runs both drop — so the
// activity can be retried after a timeout that actually succeeded.
func SweepCDCTrailingEdgeInternal(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	// Each fire also records the window's lineage event (lineage_events.go).
	res, err := sweepCDCTrailingEdge(c.Request.Context(), database, cdcAfterPipelineInterval(), sql.NullTime{}, recordingCDCFire(fireAfterCDCWindow))
	if err != nil {
		if isUndefinedTable(err) {
			// Migration 137 not applied yet: nothing to sweep, and not an error worth a retry.
			c.JSON(http.StatusOK, res)
			return
		}
		log.WithError(err).Error("CDC trailing edge: sweep failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "CDC trailing-edge sweep failed"})
		return
	}
	c.JSON(http.StatusOK, res)
}
