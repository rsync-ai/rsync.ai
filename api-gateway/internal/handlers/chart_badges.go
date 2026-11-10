package handlers

// chart_badges.go is what a tile says about its rows (dashboards-design.md § 5): when
// they are from, whether the model is late, its last good run's checks, and the
// warnings that sit above a chart without hiding it. Every fact is already stored; no
// row value is read here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// chartBadges is one tile's badges.
type chartBadges struct {
	SourceKind string `json:"source_kind"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
	// DataAsOf is the oldest CDC watermark the model's last good run read, or that run's
	// finish when it read no CDC table. Nil for a saved query, or a model never built.
	DataAsOf *time.Time `json:"data_as_of,omitempty"`
	// DataAsOfFrom says which: "cdc_watermark" or "run_finished".
	DataAsOfFrom    string       `json:"data_as_of_from,omitempty"`
	RunID           string       `json:"run_id,omitempty"`
	DeadlineSeconds int          `json:"deadline_seconds,omitempty"`
	Late            bool         `json:"late"`
	LateCause       string       `json:"late_cause,omitempty"`
	Checks          []chartCheck `json:"checks"`
	Warnings        []string     `json:"warnings"`
	LineageURL      string       `json:"lineage_url"`
}

// chartCheck is one check's outcome on the run the rows came from: a name and a status,
// never what it observed.
type chartCheck struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
}

// loadChartBadges reads the badges for a source the caller has already been shown.
func loadChartBadges(ctx context.Context, database *sql.DB, s *chartSourceRow, now time.Time) (*chartBadges, error) {
	b := &chartBadges{
		SourceKind: s.Kind(),
		SourceID:   s.ID,
		SourceName: s.Name,
		Checks:     []chartCheck{},
		Warnings:   []string{},
		LineageURL: "/explorer/lineage?focus=" + url.QueryEscape("model:"+s.ID),
	}
	if !s.IsModel() {
		if s.LastRunStatus == "failed" {
			b.Warnings = append(b.Warnings, "This saved query's last run failed. The chart reads it live, so its rows are current.")
		}
		return b, nil
	}

	// The last good run: where the table's rows came from.
	var runID string
	var finished sql.NullTime
	var marks []byte
	err := database.QueryRowContext(ctx, `
		SELECT run_id::text, finished_at, COALESCE(cdc_watermarks, 'null'::jsonb)
		FROM saved_query_runs
		WHERE saved_query_id = $1 AND status = 'succeeded'
		ORDER BY finished_at DESC NULLS LAST
		LIMIT 1
	`, s.ID).Scan(&runID, &finished, &marks)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("load the model's last run: %w", err)
	}
	if err == nil {
		b.RunID = runID
		if at := oldestWatermark(marks); at != nil {
			b.DataAsOf, b.DataAsOfFrom = at, "cdc_watermark"
		} else if finished.Valid {
			t := finished.Time
			b.DataAsOf, b.DataAsOfFrom = &t, "run_finished"
		}
		rows, err := database.QueryContext(ctx, `
			SELECT check_name, severity, status
			FROM saved_query_check_results
			WHERE run_id = $1
			ORDER BY check_name
		`, runID)
		if err != nil {
			return nil, fmt.Errorf("load the run's checks: %w", err)
		}
		for rows.Next() {
			var c chartCheck
			if err := rows.Scan(&c.Name, &c.Severity, &c.Status); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read the run's checks: %w", err)
			}
			b.Checks = append(b.Checks, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the run's checks: %w", err)
		}
	}

	// On time or late, as the freshness sweep decides it.
	fact, err := loadOneModelFreshnessFact(ctx, database, s.ID)
	if err != nil {
		return nil, err
	}
	if fact != nil {
		b.DeadlineSeconds = fact.DeadlineSeconds
		late, cause := modelIsLate(*fact, now)
		b.Late, b.LateCause = late, cause
	}

	age := "never built"
	if b.DataAsOf != nil {
		age = "from " + b.DataAsOf.UTC().Format("2006-01-02 15:04 UTC")
	}
	if b.Late {
		// "Overdue", the models pages' word for a missed deadline (#1503).
		b.Warnings = append(b.Warnings, fmt.Sprintf("Overdue: the model's rows are %s, past its %s deadline (%s).",
			age, humanSeconds(b.DeadlineSeconds), strings.ReplaceAll(b.LateCause, "_", " ")))
	}
	switch {
	case s.LastRunStatus == "failed" && strings.TrimSpace(s.LastRunError) == modelCheckBlockedError:
		b.Warnings = append(b.Warnings, "The model's last run was stopped by a data check; these rows are "+age+".")
	case s.LastRunStatus == "failed":
		b.Warnings = append(b.Warnings, "The model's last run failed; these rows are "+age+".")
	}
	return b, nil
}

// oldestWatermark is the oldest source_time in a run's recorded CDC watermarks.
func oldestWatermark(raw []byte) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]modelRunCDCWatermark
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	var oldest *time.Time
	for _, w := range m {
		if w.SourceTime == nil {
			continue
		}
		if oldest == nil || w.SourceTime.Before(*oldest) {
			t := *w.SourceTime
			oldest = &t
		}
	}
	return oldest
}

// loadOneModelFreshnessFact reads one model's freshness fact, the columns the sweep
// reads (modelFreshnessFactsQuery), or nil when the model makes no freshness promise.
func loadOneModelFreshnessFact(ctx context.Context, database *sql.DB, modelID string) (*modelFreshnessFact, error) {
	var f modelFreshnessFact
	err := database.QueryRowContext(ctx, `
		SELECT sq.id::text,
		       COALESCE(sq.freshness_deadline_seconds, 0),
		       sq.materialization,
		       COALESCE(lr.last_success, sq.created_at),
		       (lr.last_success IS NULL),
		       (s.schedule_id IS NOT NULL),
		       COALESCE(s.status, ''),
		       COALESCE(s.schedule_type, ''),
		       (s.auto_paused_at IS NOT NULL),
		       COALESCE(up.n, 0)
		FROM saved_queries sq
		LEFT JOIN LATERAL (
		    SELECT MAX(r.finished_at) AS last_success
		    FROM saved_query_runs r
		    WHERE r.saved_query_id = sq.id AND r.status = 'succeeded'
		) lr ON TRUE
		LEFT JOIN saved_query_schedules s
		       ON s.saved_query_id = sq.id AND s.status != 'deleted'
		LEFT JOIN LATERAL (
		    SELECT COUNT(*) AS n
		    FROM saved_query_schedule_upstreams u
		    WHERE u.schedule_id = s.schedule_id
		) up ON TRUE
		WHERE sq.id = $1 AND sq.freshness_deadline_seconds IS NOT NULL
	`, modelID).Scan(&f.SavedQueryID, &f.DeadlineSeconds, &f.Materialization,
		&f.ReferenceAt, &f.NeverSucceeded,
		&f.HasSchedule, &f.ScheduleStatus, &f.ScheduleType, &f.AutoPaused, &f.UpstreamCount)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load the model's freshness: %w", err)
	}
	return &f, nil
}

// modelIsLate asks evaluateModelFreshness the sweep's own question with no breach on
// record: would it open one now? So a tile and the freshness page never disagree.
func modelIsLate(f modelFreshnessFact, now time.Time) (bool, string) {
	f.OpenBreachID = ""
	for _, d := range evaluateModelFreshness([]modelFreshnessFact{f}, now) {
		if d.Open {
			return true, d.Cause
		}
	}
	return false, ""
}

// humanSeconds says a deadline in the largest whole unit.
func humanSeconds(s int) string {
	switch {
	case s > 0 && s%86400 == 0:
		return plural(s/86400, "day")
	case s > 0 && s%3600 == 0:
		return plural(s/3600, "hour")
	case s > 0 && s%60 == 0:
		return plural(s/60, "minute")
	}
	return plural(s, "second")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
