package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// GET /api/v1/activity/metrics is the Runs page's charts (dashboards-design.md
// § 6, build step D1): per day, the workspace's pipeline runs by outcome, their
// run time and the rows they wrote; model runs by outcome and their run time;
// and the checks those model runs ran, by result.
//
// It counts exactly the runs GET /api/v1/activity lists: the same FROM clause and
// status for a pipeline run (executionFromSQL, executionStatusSQL), the same
// visibility rule for a scheduled query's runs, so a bar is the rows the list
// shows for that day. It is read-only: unlike the list, it does not repair a stale
// "running" run (repairStaleWorkspaceExecutions); the list beside it does.
//
// Every number is summed in SQL; the response is only the points to draw. A day
// is a UTC calendar day of the run's start, and a run's rows written are the
// Records the run's own page shows (executionRecordsProcessedSQL), so a CDC
// stream's rows count on the day the stream started.

const (
	defaultActivityMetricsDays = 7
	maxActivityMetricsDays     = 90
	activityMetricsDayFormat   = "2006-01-02"
)

// Checks report one of three results (migration 138).
const (
	checkPass  = "pass"
	checkFail  = "fail"
	checkError = "error"
)

// activityMetricsNow is the clock the window ends on; tests pin it.
var activityMetricsNow = time.Now

// MetricCount is how many runs (or check results) of one status started on a day.
type MetricCount struct {
	Day    string `json:"day"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

// MetricRunTime is the run time of a day's runs that ended in success or failure.
type MetricRunTime struct {
	Day           string  `json:"day"`
	Runs          int64   `json:"runs"`
	MedianSeconds float64 `json:"median_seconds"`
	P95Seconds    float64 `json:"p95_seconds"`
}

// MetricRows is the rows written by the pipeline runs that started on a day.
type MetricRows struct {
	Day  string `json:"day"`
	Rows int64  `json:"rows"`
}

// ActivityMetrics is the whole response.
type ActivityMetrics struct {
	// Every day of the window, oldest first, so a day with no run still has a place.
	Days            []string        `json:"days"`
	TimeZone        string          `json:"time_zone"`
	PipelineRuns    []MetricCount   `json:"pipeline_runs"`
	PipelineRunTime []MetricRunTime `json:"pipeline_run_time"`
	RowsWritten     []MetricRows    `json:"rows_written"`
	ModelRuns       []MetricCount   `json:"model_runs"`
	ModelRunTime    []MetricRunTime `json:"model_run_time"`
	Checks          []MetricCount   `json:"checks"`
}

// The order a day's statuses are listed in, which is also the order they stack in.
var (
	pipelineMetricStatusOrder = []string{activitySucceeded, activityFailed, activityCancelled, activityWaiting, activityRunning}
	modelMetricStatusOrder    = []string{activitySucceeded, activityFailed, activitySkipped}
	checkMetricStatusOrder    = []string{checkPass, checkFail, checkError}
)

// metricsWindow is the last n UTC days, today included: [from, to).
func metricsWindow(now time.Time, n int) (from, to time.Time, days []string) {
	today := now.UTC().Truncate(24 * time.Hour)
	from = today.AddDate(0, 0, -(n - 1))
	to = today.AddDate(0, 0, 1)
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format(activityMetricsDayFormat))
	}
	return from, to, days
}

func parseActivityMetricsDays(s string) (int, error) {
	if s == "" {
		return defaultActivityMetricsDays, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxActivityMetricsDays {
		return 0, fmt.Errorf("days must be between 1 and %d", maxActivityMetricsDays)
	}
	return n, nil
}

// pipelineSourceStatuses is every executionStatusSQL word that maps to one of
// the given activity words, sorted so the SQL text is stable.
func pipelineSourceStatuses(words ...string) []string {
	var out []string
	for source, s := range pipelineRunStatuses {
		for _, w := range words {
			if s == w {
				out = append(out, source)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sortMetricCounts orders by day, then by the status order given; a status not
// in it goes last, by name.
func sortMetricCounts(points []MetricCount, order []string) {
	rank := func(s string) int {
		for i, o := range order {
			if o == s {
				return i
			}
		}
		return len(order)
	}
	sort.SliceStable(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if a.Day != b.Day {
			return a.Day < b.Day
		}
		if ra, rb := rank(a.Status), rank(b.Status); ra != rb {
			return ra < rb
		}
		return a.Status < b.Status
	})
}

// The day a run started on, as the response names it.
func metricDaySQL(col string) string {
	return "to_char(" + col + " AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
}

// pipelineMetricsWhere is listPipelineActivity's rule, cut to the window.
func pipelineMetricsWhere(args *sqlArgs, ws string, from, to time.Time) string {
	return "p.workspace_id = " + args.add(ws) +
		" AND e.id <> e.pipeline_id" +
		" AND e.start_time >= " + args.add(from) + " AND e.start_time < " + args.add(to)
}

// pipelineRunMetrics reads the pipeline runs per day and status, with the rows
// they wrote. Statuses are grouped as executionStatusSQL reports them and mapped
// to the activity word here, through the one map the list uses.
func pipelineRunMetrics(ctx context.Context, database *sql.DB, ws string, from, to time.Time) ([]MetricCount, []MetricRows, error) {
	args := sqlArgs{}
	where := pipelineMetricsWhere(&args, ws, from, to)
	rows, err := database.QueryContext(ctx, `
		SELECT `+metricDaySQL("e.start_time")+` AS day,
		       COALESCE((`+executionStatusSQL+`), '') AS status,
		       COUNT(*),
		       COALESCE(SUM(`+executionRecordsProcessedSQL+`), 0)::bigint
	`+executionFromSQL+`
		WHERE `+where+`
		GROUP BY 1, 2`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := map[[2]string]int64{}
	written := map[string]int64{}
	for rows.Next() {
		var day, source string
		var n, w int64
		if err := rows.Scan(&day, &source, &n, &w); err != nil {
			return nil, nil, err
		}
		counts[[2]string{day, pipelineRunStatus(source)}] += n
		written[day] += w
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var outCounts []MetricCount
	for k, n := range counts {
		outCounts = append(outCounts, MetricCount{Day: k[0], Status: k[1], Count: n})
	}
	sortMetricCounts(outCounts, pipelineMetricStatusOrder)
	var outRows []MetricRows
	for day, w := range written {
		outRows = append(outRows, MetricRows{Day: day, Rows: w})
	}
	sort.Slice(outRows, func(i, j int) bool { return outRows[i].Day < outRows[j].Day })
	return outCounts, outRows, nil
}

// pipelineRunTimeMetrics is the run time of the pipeline runs that ended in
// success or failure. A cancelled run is left out: its end is when someone (or
// the stale-run repair) stopped it, not when its work ended. A live CDC stream
// has no end yet (executionEndTimeSQL) and is left out too.
func pipelineRunTimeMetrics(ctx context.Context, database *sql.DB, ws string, from, to time.Time) ([]MetricRunTime, error) {
	args := sqlArgs{}
	where := pipelineMetricsWhere(&args, ws, from, to)
	where += " AND (" + executionStatusSQL + ") IN " + args.in(pipelineSourceStatuses(activitySucceeded, activityFailed))
	where += " AND (" + executionEndTimeSQL + ") IS NOT NULL"
	return readRunTimes(ctx, database, `
		SELECT day, COUNT(*),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY secs),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY secs)
		FROM (
			SELECT `+metricDaySQL("e.start_time")+` AS day,
			       GREATEST(0, EXTRACT(EPOCH FROM ((`+executionEndTimeSQL+`) - e.start_time)))::float8 AS secs
		`+executionFromSQL+`
			WHERE `+where+`
		) t
		GROUP BY day
		ORDER BY day`, args)
}

// modelMetricsWhere is listScheduledQueryActivity's rule, cut to the window: a
// private query's runs are its author's alone.
func modelMetricsWhere(args *sqlArgs, ws, userID string, from, to time.Time) string {
	return "sq.workspace_id = " + args.add(ws) +
		" AND (sq.visibility = 'workspace' OR sq.created_by = " + args.add(userID) + ")" +
		" AND r.started_at >= " + args.add(from) + " AND r.started_at < " + args.add(to)
}

func modelRunMetrics(ctx context.Context, database *sql.DB, ws, userID string, from, to time.Time) ([]MetricCount, error) {
	args := sqlArgs{}
	where := modelMetricsWhere(&args, ws, userID, from, to)
	out, err := readCounts(ctx, database, `
		SELECT `+metricDaySQL("r.started_at")+`, r.status, COUNT(*)
		FROM saved_query_runs r
		JOIN saved_queries sq ON sq.id = r.saved_query_id
		WHERE `+where+`
		GROUP BY 1, 2`, args)
	sortMetricCounts(out, modelMetricStatusOrder)
	return out, err
}

// modelRunTimeMetrics leaves out a skipped run: it did no work, so its duration
// is not a measurement (as RunDurationChart draws it).
func modelRunTimeMetrics(ctx context.Context, database *sql.DB, ws, userID string, from, to time.Time) ([]MetricRunTime, error) {
	args := sqlArgs{}
	where := modelMetricsWhere(&args, ws, userID, from, to)
	where += " AND r.status IN " + args.in([]string{activitySucceeded, activityFailed})
	return readRunTimes(ctx, database, `
		SELECT day, COUNT(*),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY secs),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY secs)
		FROM (
			SELECT `+metricDaySQL("r.started_at")+` AS day,
			       GREATEST(0, EXTRACT(EPOCH FROM (r.finished_at - r.started_at)))::float8 AS secs
			FROM saved_query_runs r
			JOIN saved_queries sq ON sq.id = r.saved_query_id
			WHERE `+where+`
		) t
		GROUP BY day
		ORDER BY day`, args)
}

// checkMetrics counts the check results of the model runs the caller can see,
// on the day their run started.
func checkMetrics(ctx context.Context, database *sql.DB, ws, userID string, from, to time.Time) ([]MetricCount, error) {
	args := sqlArgs{}
	where := modelMetricsWhere(&args, ws, userID, from, to)
	out, err := readCounts(ctx, database, `
		SELECT `+metricDaySQL("r.started_at")+`, c.status, COUNT(*)
		FROM saved_query_check_results c
		JOIN saved_query_runs r ON r.run_id = c.run_id
		JOIN saved_queries sq ON sq.id = r.saved_query_id
		WHERE `+where+`
		GROUP BY 1, 2`, args)
	sortMetricCounts(out, checkMetricStatusOrder)
	return out, err
}

func readCounts(ctx context.Context, database *sql.DB, query string, args sqlArgs) ([]MetricCount, error) {
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricCount
	for rows.Next() {
		var p MetricCount
		if err := rows.Scan(&p.Day, &p.Status, &p.Count); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func readRunTimes(ctx context.Context, database *sql.DB, query string, args sqlArgs) ([]MetricRunTime, error) {
	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricRunTime
	for rows.Next() {
		var p MetricRunTime
		if err := rows.Scan(&p.Day, &p.Runs, &p.MedianSeconds, &p.P95Seconds); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// orEmpty keeps an empty series a JSON [] rather than null.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ActivityMetricsHandler answers the Runs page's charts for the last `days` UTC days.
// GET /api/v1/activity/metrics?days=
func ActivityMetricsHandler(c *gin.Context) {
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
	n, err := parseActivityMetricsDays(c.Query("days"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	from, to, days := metricsWindow(activityMetricsNow(), n)
	ctx := c.Request.Context()
	resp := ActivityMetrics{Days: days, TimeZone: "UTC"}

	// A table an older install has not migrated yet reads as no runs, as the list does.
	read := func(name string, f func() error) bool {
		if err := f(); err != nil && !isUndefinedTable(err) {
			log.WithError(err).WithField("series", name).Error("[activity] metrics failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read run metrics"})
			return false
		}
		return true
	}
	if !read("pipeline_runs", func() (err error) {
		resp.PipelineRuns, resp.RowsWritten, err = pipelineRunMetrics(ctx, database, ws, from, to)
		return err
	}) {
		return
	}
	if !read("pipeline_run_time", func() (err error) {
		resp.PipelineRunTime, err = pipelineRunTimeMetrics(ctx, database, ws, from, to)
		return err
	}) {
		return
	}
	if !read("model_runs", func() (err error) {
		resp.ModelRuns, err = modelRunMetrics(ctx, database, ws, userID, from, to)
		return err
	}) {
		return
	}
	if !read("model_run_time", func() (err error) {
		resp.ModelRunTime, err = modelRunTimeMetrics(ctx, database, ws, userID, from, to)
		return err
	}) {
		return
	}
	if !read("checks", func() (err error) {
		resp.Checks, err = checkMetrics(ctx, database, ws, userID, from, to)
		return err
	}) {
		return
	}

	resp.PipelineRuns = orEmpty(resp.PipelineRuns)
	resp.PipelineRunTime = orEmpty(resp.PipelineRunTime)
	resp.RowsWritten = orEmpty(resp.RowsWritten)
	resp.ModelRuns = orEmpty(resp.ModelRuns)
	resp.ModelRunTime = orEmpty(resp.ModelRunTime)
	resp.Checks = orEmpty(resp.Checks)
	c.JSON(http.StatusOK, resp)
}
