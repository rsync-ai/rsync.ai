package handlers

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// runOutcomeStatuses folds every status executionStatusSQL can report into
// the outcome a history question asks about ("how many runs failed?").
// TestRunOutcome checks it agrees with runFailedStatuses and with the Admin →
// Executions filter.
var runOutcomeStatuses = map[string][]string{
	"succeeded":        {"completed", "success"},
	"failed":           {"failed", "error", "credential_check_failed", "silent_drop_detected", "silent_partial_drop_detected"},
	"cancelled":        {"cancelled", "canceled", "stopped"},
	"running":          {"running", "processing"},
	"pending":          {"pending", "queued"},
	"waiting_for_user": {"waiting_for_user"},
}

var runOutcomeOf = func() map[string]string {
	m := map[string]string{}
	for outcome, statuses := range runOutcomeStatuses {
		for _, s := range statuses {
			m[s] = outcome
		}
	}
	return m
}()

// RunOutcome is the outcome of a run in a status executionStatusSQL reports:
// one of the runOutcomeStatuses keys, or "other".
func RunOutcome(status string) string {
	if o, ok := runOutcomeOf[strings.ToLower(strings.TrimSpace(status))]; ok {
		return o
	}
	return "other"
}

// RunHistoryRun is one run in a run-history read. It carries the error class,
// never the error text, and no row counts.
type RunHistoryRun struct {
	ID            string     `json:"id"`
	PipelineID    string     `json:"pipeline_id"`
	PipelineName  string     `json:"pipeline_name,omitempty"`
	Status        string     `json:"status"`
	Outcome       string     `json:"outcome"`
	TriggerSource string     `json:"trigger_source,omitempty"`
	LiveStream    bool       `json:"live_stream"`
	ScheduledTime *time.Time `json:"scheduled_time,omitempty"`
	StartTime     *time.Time `json:"start_time,omitempty"`
	EndTime       *time.Time `json:"end_time,omitempty"`
	ErrorClass    string     `json:"error_class,omitempty"`
}

// ListRunHistory is the agent gateway's read of the active workspace's runs:
// the count of every run by outcome, then the newest runs, optionally of one
// pipeline and one outcome. It is not routed; the gateway calls it
// in-process as the caller. It only reads: ListExecutions repairs stale rows
// as it lists them, which a read-only tool must never do.
//
// Query: pipeline_id (uuid), outcome (a runOutcomeStatuses key), limit (1-100,
// default 25).
func ListRunHistory(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database_unavailable"})
		return
	}
	if _, ok := resolveUserID(c); !ok {
		return
	}
	scopeWS, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}

	pipelineID := strings.TrimSpace(c.Query("pipeline_id"))
	if pipelineID != "" {
		if _, err := uuid.Parse(pipelineID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_pipeline_id"})
			return
		}
	}
	outcome := strings.TrimSpace(c.Query("outcome"))
	if _, known := runOutcomeStatuses[outcome]; outcome != "" && !known {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_outcome"})
		return
	}
	limit := 25
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_limit"})
			return
		}
		limit = n
	}

	// A pipeline outside the workspace is a 404, as on every pipeline read.
	// Fail-open on a DB error: the scoped reads below return nothing.
	if pipelineID != "" {
		var exists bool
		if err := database.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM pipelines WHERE id = $1 AND workspace_id = $2)`,
			pipelineID, scopeWS,
		).Scan(&exists); err == nil && !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "pipeline_not_found", "message": "Pipeline not found"})
			return
		}
	}

	where := `
		WHERE p.workspace_id = $1
		  AND e.id <> e.pipeline_id`
	args := []any{scopeWS}
	if pipelineID != "" {
		args = append(args, pipelineID)
		where += `
		  AND e.pipeline_id = $` + strconv.Itoa(len(args))
	}

	byOutcome := map[string]int{"other": 0}
	for o := range runOutcomeStatuses {
		byOutcome[o] = 0
	}
	countRows, err := database.Query(`SELECT `+executionStatusSQL+` AS status, COUNT(*)`+executionFromSQL+where+`
		GROUP BY 1`, args...)
	if err != nil {
		log.WithError(err).Warn("run history: count query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "run_history_unavailable"})
		return
	}
	defer countRows.Close()
	for countRows.Next() {
		var status sql.NullString
		var n int
		if err := countRows.Scan(&status, &n); err != nil {
			log.WithError(err).Warn("run history: count scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "run_history_unavailable"})
			return
		}
		byOutcome[RunOutcome(status.String)] += n
	}
	total := 0
	for _, n := range byOutcome {
		total += n
	}

	if outcome != "" {
		total = byOutcome[outcome]
		var marks []string
		for _, s := range runOutcomeStatuses[outcome] {
			args = append(args, s)
			marks = append(marks, "$"+strconv.Itoa(len(args)))
		}
		where += `
		  AND (` + executionStatusSQL + `) IN (` + strings.Join(marks, ", ") + `)`
	}
	args = append(args, limit)
	rows, err := database.Query(`
		SELECT e.id,
		       e.pipeline_id,
		       p.name,
		       `+executionStatusSQL+` AS status,
		       ls.live_stream,
		       e.trigger_source,
		       e.scheduled_time,
		       e.start_time,
		       `+executionEndTimeSQL+` AS end_time,
		       e.error_message`+executionFromSQL+where+`
		ORDER BY e.start_time DESC NULLS LAST
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		log.WithError(err).Warn("run history: list query failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "run_history_unavailable"})
		return
	}
	defer rows.Close()
	runs := []RunHistoryRun{}
	for rows.Next() {
		var r RunHistoryRun
		var name, trigger, errMsg sql.NullString
		var scheduled, start, end sql.NullTime
		if err := rows.Scan(&r.ID, &r.PipelineID, &name, &r.Status, &r.LiveStream, &trigger,
			&scheduled, &start, &end, &errMsg); err != nil {
			log.WithError(err).Warn("run history: row scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "run_history_unavailable"})
			return
		}
		r.PipelineName, r.TriggerSource = name.String, trigger.String
		r.ScheduledTime, r.StartTime, r.EndTime = nullTimePtr(scheduled), nullTimePtr(start), nullTimePtr(end)
		r.Outcome = RunOutcome(r.Status)
		r.ErrorClass = RunErrorClass(r.Status, errMsg.String)
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Warn("run history: rows failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "run_history_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs, "total": total, "by_outcome": byOutcome, "limit": limit})
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
