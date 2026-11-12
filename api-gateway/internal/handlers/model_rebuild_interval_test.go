package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	workflow "github.com/rsync-ai/shared/workflowdef"
)

// Bug class: a freshness deadline shorter than the model's own schedule. The model goes
// Overdue between every pair of builds while working exactly as configured, and nothing
// says so when the deadline is set. Seen on prod: Loan portfolio, a 6h deadline on a
// model rebuilt once a day after the 02:00 UTC Nightly, Overdue 18 hours of every day.
// modelRebuildInterval is what lets the dialog say so; these pin its answer.

var rebuildNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func cronClock(expr, tz string) rebuildClock {
	return rebuildClock{ScheduleType: "cron", Spec: ScheduleSpec{Cron: expr, Timezone: tz}}
}

func everyClock(seconds int) rebuildClock {
	return rebuildClock{ScheduleType: "interval", Spec: ScheduleSpec{EverySeconds: seconds}}
}

func workflowClock(expr string) rebuildClock {
	return rebuildClock{ScheduleType: "workflow", Trigger: workflow.Trigger{Type: workflow.TriggerSchedule, Cron: expr}}
}

func clocked(clocks ...rebuildClock) *rebuildNode { return &rebuildNode{Clocks: clocks} }
func after(from ...string) *rebuildNode           { return &rebuildNode{From: from} }

func TestLongestScheduledGap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		g      rebuildGraph
		want   int64  // seconds, when known
		reason string // when unknown
	}{
		{"the Loan portfolio shape: two silvers on one nightly pipeline", rebuildGraph{
			"model:m":          after("model:loans", "model:payments"),
			"model:loans":      after("pipeline:nightly"),
			"model:payments":   after("pipeline:nightly"),
			"pipeline:nightly": clocked(cronClock("00 02 * * *", "UTC")),
		}, 24 * 3600, ""},
		{"any over two crons: the gaps between both", rebuildGraph{
			"model:m":    after("pipeline:a", "pipeline:b"),
			"pipeline:a": clocked(cronClock("0 2 * * *", "UTC")),
			"pipeline:b": clocked(cronClock("0 8 * * *", "UTC")),
		}, 18 * 3600, ""},
		{"a weekday cron leaves the weekend", rebuildGraph{
			"model:m": clocked(cronClock("0 2 * * 1-5", "UTC")),
		}, 72 * 3600, ""},
		{"an interval", rebuildGraph{
			"model:m": clocked(everyClock(3600)),
		}, 3600, ""},
		// Temporal's ScheduleSpec.TimeZoneName: "if you write a calendar spec that fires
		// at 2:30am and specify a time zone that follows DST, that action will not be
		// triggered on the day that has no 2:30am". robfig skips it the same way.
		{"a cron in a zone with DST skips the day with no 02:00", rebuildGraph{
			"model:m": clocked(cronClock("0 2 * * *", "America/New_York")),
		}, 47 * 3600, ""},
		{"a workflow that refreshes the model ticks too", rebuildGraph{
			"model:m":    &rebuildNode{Clocks: []rebuildClock{cronClock("0 2 * * *", "UTC")}, From: []string{"workflow:w"}},
			"workflow:w": clocked(workflowClock("0 */6 * * *")),
		}, 6 * 3600, ""},
		{"a workflow after a pipeline runs another pipeline the model waits on", rebuildGraph{
			"model:m":    after("pipeline:q"),
			"pipeline:q": after("workflow:w"),
			"workflow:w": after("pipeline:p"),
			"pipeline:p": clocked(cronClock("0 */12 * * *", "UTC")),
		}, 12 * 3600, ""},
		{"a paused upstream ticks nothing; the other one sets the gap", rebuildGraph{
			"model:m":         after("pipeline:daily", "pipeline:paused"),
			"pipeline:daily":  clocked(cronClock("0 2 * * *", "UTC")),
			"pipeline:paused": {},
		}, 24 * 3600, ""},
		{"a diamond counts its source once", rebuildGraph{
			"model:m":    after("model:x", "model:y"),
			"model:x":    after("pipeline:p"),
			"model:y":    after("pipeline:p"),
			"pipeline:p": clocked(cronClock("0 2 * * *", "UTC")),
		}, 24 * 3600, ""},
		{"a dense clock past its tick cap does not leave a false gap", rebuildGraph{
			// 2000 ticks of a minute clock cover ~33h; past them only the daily cron is
			// listed, and a window that ran on would report its 24h gap.
			"model:m": clocked(everyClock(60), cronClock("0 2 * * *", "UTC")),
		}, 60, ""},
		{"a yearly cron", rebuildGraph{
			"model:m": clocked(cronClock("0 0 1 1 *", "UTC")),
		}, 365 * 24 * 3600, ""},

		{"a CDC pipeline in the chain", rebuildGraph{
			"model:m":      after("model:s", "pipeline:b"),
			"model:s":      after("pipeline:cdc"),
			"pipeline:b":   clocked(cronClock("0 2 * * *", "UTC")),
			"pipeline:cdc": {Unpredictable: true},
		}, 0, rebuildUnknownUnpredictable},
		{"a ring", rebuildGraph{
			"model:m": after("model:x"),
			"model:x": &rebuildNode{Clocks: []rebuildClock{cronClock("0 2 * * *", "UTC")}, From: []string{"model:m"}},
		}, 0, rebuildUnknownRing},
		{"an upstream outside the graph", rebuildGraph{
			"model:m": &rebuildNode{Clocks: []rebuildClock{cronClock("0 2 * * *", "UTC")}, From: []string{"pipeline:gone"}},
		}, 0, rebuildUnknownMissing},
		{"a cron that cannot be read", rebuildGraph{
			"model:m": clocked(cronClock("not a cron", "UTC"), cronClock("0 2 * * *", "UTC")),
		}, 0, rebuildUnknownBadClock},
		{"nothing scheduled", rebuildGraph{
			"model:m":    after("pipeline:p"),
			"pipeline:p": {},
		}, 0, rebuildUnknownNoSchedule},
		{"one tick in the horizon", rebuildGraph{
			"model:m": clocked(cronClock("0 0 29 2 *", "UTC")),
		}, 0, rebuildUnknownTooRare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := longestScheduledGap(tc.g, "m", rebuildNow)
			if tc.reason != "" {
				if got.Known || got.Reason != tc.reason {
					t.Fatalf("got %+v, want unknown (%s)", got, tc.reason)
				}
				return
			}
			if !got.Known || got.Seconds != tc.want {
				t.Fatalf("got %+v, want %ds", got, tc.want)
			}
		})
	}
}

// A chain longer than the node cap is unknown rather than a walk without end.
func TestLongestScheduledGap_TooManyNodes(t *testing.T) {
	g := rebuildGraph{}
	for i := 0; i < rebuildMaxNodes+5; i++ {
		g[nodeKey(i)] = after(nodeKey(i + 1))
	}
	g[nodeKey(rebuildMaxNodes+5)] = clocked(cronClock("0 2 * * *", "UTC"))
	g["model:m"] = after(nodeKey(0))
	if got := longestScheduledGap(g, "m", rebuildNow); got.Known || got.Reason != rebuildUnknownTooMany {
		t.Fatalf("got %+v, want unknown (%s)", got, rebuildUnknownTooMany)
	}
}

func nodeKey(i int) string { return "model:n" + time.Duration(i).String() }

// --- GET /explorer/saved/:id/freshness ---

const (
	rebuildSilverLoans    = "c0000000-0000-0000-0000-00000000000c"
	rebuildSilverPayments = "d0000000-0000-0000-0000-00000000000d"
	rebuildNightly        = "e0000000-0000-0000-0000-00000000000e"
	rebuildWorkflow       = "f0000000-0000-0000-0000-00000000000f"
)

var (
	rebuildModelsQuery    = `WITH RECURSIVE closure\(model_id\)[\s\S]*s\.status = 'active'`
	rebuildWorkflowsQuery = `FROM workflows\s+WHERE workspace_id = \$1 AND status = 'active'`
	rebuildPipelinesQuery = `FROM pipelines p[\s\S]*LEFT JOIN pipeline_schedules ps`
	rebuildDeadlineQuery  = `SELECT freshness_deadline_seconds FROM saved_queries WHERE id = \$1 AND workspace_id = \$2`
)

func rebuildModelRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"model_id", "schedule_type", "schedule_spec", "has_edge", "upstream_pipeline_id", "upstream_saved_query_id"})
}

func rebuildPipelineRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "is_cdc", "schedule_type", "schedule_spec"})
}

func getFreshness(t *testing.T) map[string]any {
	t.Helper()
	r := savedQueryRouter(http.MethodGet, "/explorer/saved/:id/freshness", "viewer", GetSavedQueryFreshness)
	w := doJSON(r, http.MethodGet, "/explorer/saved/"+savedQueryID+"/freshness", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The prod shape end to end, through the three loader queries: Loan portfolio waits
// ('all') on two silvers, each after the Nightly, whose schedule is 02:00 UTC daily. A
// workflow that refreshes an unrelated model is read and changes nothing.
func TestGetSavedQueryFreshness_LoanPortfolioShapeIsADay(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectVisibleRunsModel(mock)
	mock.ExpectQuery(rebuildDeadlineQuery).WithArgs(savedQueryID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"freshness_deadline_seconds"}).AddRow(21600))
	mock.ExpectQuery(rebuildModelsQuery).WithArgs(wsScopeWS, savedQueryID).
		WillReturnRows(rebuildModelRows().
			AddRow(savedQueryID, "after_upstream", []byte(`{"timezone":"UTC"}`), true, nil, rebuildSilverLoans).
			AddRow(savedQueryID, "after_upstream", []byte(`{"timezone":"UTC"}`), true, nil, rebuildSilverPayments).
			AddRow(rebuildSilverLoans, "after_upstream", []byte(`{}`), true, rebuildNightly, nil).
			AddRow(rebuildSilverPayments, "after_upstream", []byte(`{}`), true, rebuildNightly, nil))
	mock.ExpectQuery(rebuildWorkflowsQuery).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "trigger_kind", "trigger_pipeline_id", "definition"}).
			AddRow(rebuildWorkflow, "schedule", nil,
				[]byte(`{"version":3,"trigger":{"type":"schedule","cron":"0 * * * *"},"steps":[{"id":"r","type":"refresh_model","model_id":"`+provModelB+`"}]}`)))
	mock.ExpectQuery(rebuildPipelinesQuery).WithArgs(wsScopeWS, sqlmock.AnyArg()).
		WillReturnRows(rebuildPipelineRows().
			AddRow(rebuildNightly, false, "cron", []byte(`{"cron":"00 02 * * *","timezone":"UTC"}`)))

	out := getFreshness(t)
	if out["deadline_seconds"] != float64(21600) {
		t.Errorf("deadline_seconds = %v, want 21600", out["deadline_seconds"])
	}
	if out["rebuild_interval_seconds"] != float64(86400) {
		t.Errorf("rebuild_interval_seconds = %v, want 86400", out["rebuild_interval_seconds"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A workflow that refreshes the model on a schedule shortens the interval: the loader
// has to wire refresh_model steps to the model, or the warning would be false.
func TestGetSavedQueryFreshness_AWorkflowRefreshingTheModelCounts(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectVisibleRunsModel(mock)
	mock.ExpectQuery(rebuildDeadlineQuery).WithArgs(savedQueryID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"freshness_deadline_seconds"}).AddRow(21600))
	mock.ExpectQuery(rebuildModelsQuery).WithArgs(wsScopeWS, savedQueryID).
		WillReturnRows(rebuildModelRows().
			AddRow(savedQueryID, "cron", []byte(`{"cron":"0 2 * * *","timezone":"UTC"}`), false, nil, nil))
	mock.ExpectQuery(rebuildWorkflowsQuery).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "trigger_kind", "trigger_pipeline_id", "definition"}).
			AddRow(rebuildWorkflow, "schedule", nil,
				[]byte(`{"version":3,"trigger":{"type":"schedule","cron":"0 */4 * * *"},"steps":[{"id":"r","type":"refresh_model","model_id":"`+savedQueryID+`"}]}`)))

	out := getFreshness(t)
	if out["rebuild_interval_seconds"] != float64(4*3600) {
		t.Errorf("rebuild_interval_seconds = %v, want 14400", out["rebuild_interval_seconds"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A CDC pipeline anywhere above the model: no interval, the deadline still answers.
func TestGetSavedQueryFreshness_CDCUpstreamIsUnknown(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectVisibleRunsModel(mock)
	mock.ExpectQuery(rebuildDeadlineQuery).WithArgs(savedQueryID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"freshness_deadline_seconds"}).AddRow(3600))
	mock.ExpectQuery(rebuildModelsQuery).WithArgs(wsScopeWS, savedQueryID).
		WillReturnRows(rebuildModelRows().
			AddRow(savedQueryID, "after_upstream", []byte(`{}`), true, rebuildNightly, nil))
	mock.ExpectQuery(rebuildWorkflowsQuery).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "trigger_kind", "trigger_pipeline_id", "definition"}))
	mock.ExpectQuery(rebuildPipelinesQuery).WithArgs(wsScopeWS, sqlmock.AnyArg()).
		WillReturnRows(rebuildPipelineRows().AddRow(rebuildNightly, true, nil, nil))

	out := getFreshness(t)
	if out["deadline_seconds"] != float64(3600) {
		t.Errorf("deadline_seconds = %v, want 3600", out["deadline_seconds"])
	}
	if v, ok := out["rebuild_interval_seconds"]; !ok || v != nil {
		t.Errorf("rebuild_interval_seconds = %v (present %v), want null", v, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Working out the interval failing costs the warning, not the deadline.
func TestGetSavedQueryFreshness_ALoaderErrorAnswersNoInterval(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectVisibleRunsModel(mock)
	mock.ExpectQuery(rebuildDeadlineQuery).WithArgs(savedQueryID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"freshness_deadline_seconds"}).AddRow(nil))
	mock.ExpectQuery(rebuildModelsQuery).WillReturnError(errors.New("connection reset"))

	out := getFreshness(t)
	if v, ok := out["deadline_seconds"]; !ok || v != nil {
		t.Errorf("deadline_seconds = %v (present %v), want null", v, ok)
	}
	if v, ok := out["rebuild_interval_seconds"]; !ok || v != nil {
		t.Errorf("rebuild_interval_seconds = %v (present %v), want null", v, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
