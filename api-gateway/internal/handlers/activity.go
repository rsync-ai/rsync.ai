package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// GET /api/v1/activity is the Runs page: the active workspace's pipeline runs,
// scheduled-query runs and workflow runs in one list, newest first.
//
// Each kind keeps its runs in its own table, so each is read newest first and
// cut to the page before the three are merged. A page is keyset, not offset,
// because runs keep landing while someone pages back. The order is
// (started_at, kind, id), all descending; the kind breaks a tie between two
// tables, the id a tie inside one.
//
// Who sees what is each kind's own list rule: every run of the workspace's
// pipelines and workflows (as GET /executions and GET /workflows/:id/runs), and a
// scheduled query's runs only where the caller can see the query, a private one
// being its author's (as GET /explorer/schedules).

const (
	activityPipeline       = "pipeline"
	activityScheduledQuery = "scheduled_query"
	activityWorkflow       = "workflow"
)

// One status word across the three kinds, so the page filters and colours once.
const (
	activityRunning   = "running"
	activityWaiting   = "waiting"
	activitySucceeded = "succeeded"
	activityFailed    = "failed"
	activityCancelled = "cancelled"
	activitySkipped   = "skipped"
)

const (
	defaultActivityPageSize = 50
	maxActivityPageSize     = 200
	activityErrorMaxRunes   = 300
)

// ActivityRun is one row of the Runs page.
type ActivityRun struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	SubjectID   string `json:"subject_id"`
	SubjectName string `json:"subject_name"`
	Status      string `json:"status"`
	// SourceStatus is the kind's own word for the status ("completed",
	// "silent_drop_detected", …), for a tooltip; Status is what to filter on.
	SourceStatus string     `json:"source_status"`
	Trigger      string     `json:"trigger"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	// ScheduleID is the pipeline schedule that started a pipeline run, so the
	// page can link a scheduled run to its schedule; empty for every other run.
	ScheduleID string `json:"schedule_id,omitempty"`
}

// pipelineRunStatuses maps the status the executions endpoints report
// (executionStatusSQL) to the activity word. Anything else is still going:
// running, pending, a pipeline_progress phase.
var pipelineRunStatuses = map[string]string{
	"completed":                    activitySucceeded,
	"success":                      activitySucceeded,
	"failed":                       activityFailed,
	"error":                        activityFailed,
	"silent_drop_detected":         activityFailed,
	"silent_partial_drop_detected": activityFailed,
	"credential_check_failed":      activityFailed,
	"cancelled":                    activityCancelled,
	"waiting_for_user":             activityWaiting,
}

func pipelineRunStatus(source string) string {
	if s, ok := pipelineRunStatuses[source]; ok {
		return s
	}
	return activityRunning
}

// saved_query_runs holds succeeded/failed/skipped (a run is written when it ends)
// and workflow_runs running/succeeded/failed: each is already an activity word.
var (
	scheduledQueryRunStatuses = []string{activitySucceeded, activityFailed, activitySkipped}
	workflowRunStatuses       = []string{activityRunning, activitySucceeded, activityFailed}
)

type activityCursor struct {
	at   time.Time
	kind string
	id   string
}

type activityQuery struct {
	kind   string // "" = every kind
	status string // "" = every status
	limit  int
	after  *activityCursor
}

func parseActivityQuery(kind, status, limit, cursor string) (activityQuery, error) {
	q := activityQuery{kind: kind, status: status, limit: defaultActivityPageSize}
	switch kind {
	case "", activityPipeline, activityScheduledQuery, activityWorkflow:
	default:
		return q, fmt.Errorf("kind must be %s, %s or %s", activityPipeline, activityScheduledQuery, activityWorkflow)
	}
	switch status {
	case "", activityRunning, activityWaiting, activitySucceeded, activityFailed, activityCancelled, activitySkipped:
	default:
		return q, fmt.Errorf("status must be running, waiting, succeeded, failed, cancelled or skipped")
	}
	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > maxActivityPageSize {
			return q, fmt.Errorf("limit must be between 1 and %d", maxActivityPageSize)
		}
		q.limit = n
	}
	if cursor != "" {
		c, err := decodeActivityCursor(cursor)
		if err != nil {
			return q, fmt.Errorf("invalid cursor")
		}
		q.after = &c
	}
	return q, nil
}

// The cursor is the last row's (started_at, kind, id), opaque to the page.
func encodeActivityCursor(r ActivityRun) string {
	raw := r.StartedAt.UTC().Format(time.RFC3339Nano) + "|" + r.Kind + "|" + r.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeActivityCursor(s string) (activityCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return activityCursor{}, err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return activityCursor{}, fmt.Errorf("want 3 parts, got %d", len(parts))
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return activityCursor{}, err
	}
	switch parts[1] {
	case activityPipeline, activityScheduledQuery, activityWorkflow:
	default:
		return activityCursor{}, fmt.Errorf("unknown kind %q", parts[1])
	}
	if _, err := uuid.Parse(parts[2]); err != nil {
		return activityCursor{}, err
	}
	return activityCursor{at: at, kind: parts[1], id: parts[2]}, nil
}

// activityBefore orders two runs as the page lists them.
func activityBefore(a, b ActivityRun) bool {
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	if a.Kind != b.Kind {
		return a.Kind > b.Kind
	}
	return a.ID > b.ID
}

// sqlArgs numbers placeholders as they are added.
type sqlArgs []any

func (a *sqlArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

func (a *sqlArgs) in(values []string) string {
	ph := make([]string, len(values))
	for i, v := range values {
		ph[i] = a.add(v)
	}
	return "(" + strings.Join(ph, ", ") + ")"
}

// afterCursor keeps only the source's rows that come after the cursor in
// (started_at, kind, id) descending order. kind is the same for every row of a
// source, so against another kind only the time decides.
func (q activityQuery) afterCursor(kind, timeCol, idCol string, args *sqlArgs) string {
	if q.after == nil {
		return ""
	}
	at := args.add(q.after.at) + "::timestamptz"
	switch {
	case kind > q.after.kind:
		return " AND " + timeCol + " < " + at
	case kind < q.after.kind:
		return " AND " + timeCol + " <= " + at
	}
	return fmt.Sprintf(" AND (%s, %s) < (%s, %s::uuid)", timeCol, idCol, at, args.add(q.after.id))
}

// sourceStatuses is the part of a kind's status words a status filter keeps;
// none means the kind has no run in that status and is not read at all.
func sourceStatuses(statuses []string, filter string) []string {
	if filter == "" {
		return statuses
	}
	for _, s := range statuses {
		if s == filter {
			return []string{s}
		}
	}
	return nil
}

func (q activityQuery) wantsPipelines() bool {
	if q.kind != "" && q.kind != activityPipeline {
		return false
	}
	return q.status != activitySkipped
}

func (q activityQuery) wantsKind(kind string, statuses []string) bool {
	return (q.kind == "" || q.kind == kind) && len(sourceStatuses(statuses, q.status)) > 0
}

func listPipelineActivity(ctx context.Context, database *sql.DB, ws string, q activityQuery) ([]ActivityRun, error) {
	args := sqlArgs{}
	where := "p.workspace_id = " + args.add(ws) + " AND e.id <> e.pipeline_id AND e.start_time IS NOT NULL"
	if q.status != "" {
		var raw []string
		for source, s := range pipelineRunStatuses {
			if s == q.status || q.status == activityRunning {
				raw = append(raw, source)
			}
		}
		sort.Strings(raw)
		if q.status == activityRunning {
			// Running is every status that is not one of the known others.
			where += " AND COALESCE((" + executionStatusSQL + "), '') NOT IN " + args.in(raw)
		} else {
			where += " AND (" + executionStatusSQL + ") IN " + args.in(raw)
		}
	}
	where += q.afterCursor(activityPipeline, "e.start_time", "e.id", &args)
	rows, err := database.QueryContext(ctx, `
		SELECT e.id, e.pipeline_id, COALESCE(p.name, ''),
		       COALESCE((`+executionStatusSQL+`), ''),
		       COALESCE(e.trigger_source, 'manual'),
		       e.start_time, `+executionEndTimeSQL+`, COALESCE(e.error_message, ''),
		       COALESCE(e.schedule_id::text, '')
	`+executionFromSQL+`
		WHERE `+where+`
		ORDER BY e.start_time DESC, e.id DESC
		LIMIT `+strconv.Itoa(q.limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityRun
	for rows.Next() {
		r := ActivityRun{Kind: activityPipeline}
		var finished sql.NullTime
		if err := rows.Scan(&r.ID, &r.SubjectID, &r.SubjectName, &r.SourceStatus, &r.Trigger,
			&r.StartedAt, &finished, &r.Error, &r.ScheduleID); err != nil {
			return nil, err
		}
		r.Status = pipelineRunStatus(r.SourceStatus)
		if finished.Valid {
			t := finished.Time
			r.FinishedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func listScheduledQueryActivity(ctx context.Context, database *sql.DB, ws, userID string, q activityQuery) ([]ActivityRun, error) {
	args := sqlArgs{}
	// The visibility rule of GET /explorer/schedules: a private query's runs are
	// its author's alone.
	where := "sq.workspace_id = " + args.add(ws) +
		" AND (sq.visibility = 'workspace' OR sq.created_by = " + args.add(userID) + ")"
	if q.status != "" {
		where += " AND r.status = " + args.add(q.status)
	}
	where += q.afterCursor(activityScheduledQuery, "r.started_at", "r.run_id", &args)
	rows, err := database.QueryContext(ctx, `
		SELECT r.run_id, r.saved_query_id, sq.name, r.status,
		       -- A workflow step's rebuild is stored as "triggered", the word for an
		       -- upstream finishing; the list said "After its upstream" for it.
		       CASE WHEN r.upstream_kind = 'workflow' THEN 'workflow' ELSE r.trigger_source END,
		       r.started_at, r.finished_at, COALESCE(r.error, '')
		FROM saved_query_runs r
		JOIN saved_queries sq ON sq.id = r.saved_query_id
		WHERE `+where+`
		ORDER BY r.started_at DESC, r.run_id DESC
		LIMIT `+strconv.Itoa(q.limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityRun
	for rows.Next() {
		r := ActivityRun{Kind: activityScheduledQuery}
		var finished time.Time
		if err := rows.Scan(&r.ID, &r.SubjectID, &r.SubjectName, &r.SourceStatus, &r.Trigger,
			&r.StartedAt, &finished, &r.Error); err != nil {
			return nil, err
		}
		r.Status = r.SourceStatus
		r.FinishedAt = &finished
		r.Error = storedRunErrorForDisplay(r.Error)
		out = append(out, r)
	}
	return out, rows.Err()
}

func listWorkflowActivity(ctx context.Context, database *sql.DB, ws string, q activityQuery) ([]ActivityRun, error) {
	args := sqlArgs{}
	where := "r.workspace_id = " + args.add(ws)
	if q.status != "" {
		where += " AND r.status = " + args.add(q.status)
	}
	where += q.afterCursor(activityWorkflow, "r.started_at", "r.id", &args)
	rows, err := database.QueryContext(ctx, `
		SELECT r.id, r.workflow_id, w.name, r.status, r.trigger,
		       r.started_at, r.finished_at, COALESCE(r.error, '')
		FROM workflow_runs r
		JOIN workflows w ON w.id = r.workflow_id AND w.workspace_id = r.workspace_id
		WHERE `+where+`
		ORDER BY r.started_at DESC, r.id DESC
		LIMIT `+strconv.Itoa(q.limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityRun
	for rows.Next() {
		r := ActivityRun{Kind: activityWorkflow}
		var finished sql.NullTime
		if err := rows.Scan(&r.ID, &r.SubjectID, &r.SubjectName, &r.SourceStatus, &r.Trigger,
			&r.StartedAt, &finished, &r.Error); err != nil {
			return nil, err
		}
		r.Status = r.SourceStatus
		if finished.Valid {
			t := finished.Time
			r.FinishedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// shortActivityError keeps the list readable; the run's own page has the rest.
func shortActivityError(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > activityErrorMaxRunes {
		return string(r[:activityErrorMaxRunes]) + "…"
	}
	return s
}

// ListActivity lists the workspace's runs of every kind, newest first.
// GET /api/v1/activity?kind=&status=&limit=&cursor=
func ListActivity(c *gin.Context) {
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSViewer); !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	q, err := parseActivityQuery(c.Query("kind"), c.Query("status"), c.Query("limit"), c.Query("cursor"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx := c.Request.Context()

	var runs []ActivityRun
	read := func(name string, list func() ([]ActivityRun, error)) bool {
		got, err := list()
		if err != nil && !isUndefinedTable(err) {
			log.WithError(err).WithField("kind", name).Error("[activity] list failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
			return false
		}
		runs = append(runs, got...)
		return true
	}
	if q.wantsPipelines() {
		// The Executions page ran this repair on every load; the Runs page
		// replaces it, so a run whose pipeline moved on still stops reading
		// "running" forever.
		repairStaleWorkspaceExecutions(database, ws)
		if !read(activityPipeline, func() ([]ActivityRun, error) {
			return listPipelineActivity(ctx, database, ws, q)
		}) {
			return
		}
	}
	if q.wantsKind(activityScheduledQuery, scheduledQueryRunStatuses) {
		if !read(activityScheduledQuery, func() ([]ActivityRun, error) {
			return listScheduledQueryActivity(ctx, database, ws, userID, q)
		}) {
			return
		}
	}
	if q.wantsKind(activityWorkflow, workflowRunStatuses) {
		if !read(activityWorkflow, func() ([]ActivityRun, error) {
			return listWorkflowActivity(ctx, database, ws, q)
		}) {
			return
		}
	}

	sort.SliceStable(runs, func(i, j int) bool { return activityBefore(runs[i], runs[j]) })
	resp := gin.H{}
	// Each kind was read one row past the page, so a longer merge means another page.
	if len(runs) > q.limit {
		runs = runs[:q.limit]
		resp["next_cursor"] = encodeActivityCursor(runs[len(runs)-1])
	}
	if runs == nil {
		runs = []ActivityRun{}
	}
	for i := range runs {
		runs[i].Error = shortActivityError(runs[i].Error)
	}
	resp["runs"] = runs
	c.JSON(http.StatusOK, resp)
}
