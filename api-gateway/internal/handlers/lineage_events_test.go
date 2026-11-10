package handlers

// Lineage history (design § 4): what the recorder writes for each kind of run, the CDC
// window it reads back from an occurrence, and what a node's history is looked up by.
// The SQL itself runs against Postgres in lineage_events_pg_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const (
	leWS       = "e0000000-0000-4000-8000-000000000001"
	leModel    = "e0000000-0000-4000-8000-0000000000a1"
	lePipeline = "e0000000-0000-4000-8000-0000000000b1"
	leWH       = "e0000000-0000-4000-8000-0000000000c1" // the warehouse
	leSource   = "e0000000-0000-4000-8000-0000000000c2"
)

var leT0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func leModelRun() modelRunRow {
	return modelRunRow{
		RunID: "run-1", ModelID: leModel, WorkspaceID: leWS, Status: "succeeded",
		StartedAt: leT0, FinishedAt: leT0.Add(time.Minute),
		Name: "orders_enriched", Materialization: matTable, TargetTable: "marts.orders_enriched",
		SQLText:      "SELECT o.id, c.name FROM analytics.orders o JOIN Customers c ON c.id = o.customer_id WHERE o.note <> 'SECRET-LITERAL'",
		ConnectionID: leWH, ConnectorType: "postgresql", SQLKnown: true,
	}
}

// A run whose SQL is known names what that SQL reads and the table it writes, by the
// graph's keys, and the SQL version that ran.
func TestModelRunEvent_NamesTheTablesOfTheSQLThatRan(t *testing.T) {
	r := leModelRun()
	ev := modelRunEvent(r)

	if ev.JobKind != lineageJobModel || ev.SavedQueryID != leModel || ev.PipelineID != "" || ev.RunRef != "run-1" {
		t.Fatalf("job = %s %q/%q run %q", ev.JobKind, ev.SavedQueryID, ev.PipelineID, ev.RunRef)
	}
	if ev.EventType != lineageEventComplete || !ev.StartedAt.Valid || !ev.EndedAt.Equal(leT0.Add(time.Minute)) {
		t.Fatalf("event %s started %v ended %v", ev.EventType, ev.StartedAt, ev.EndedAt)
	}
	wantIn := []lineageDataset{{ConnectionID: leWH, Name: "analytics.orders"}, {ConnectionID: leWH, Name: "customers"}}
	if !reflect.DeepEqual(ev.Inputs, wantIn) {
		t.Errorf("inputs = %+v, want %+v", ev.Inputs, wantIn)
	}
	wantOut := []lineageDataset{{ConnectionID: leWH, Name: "marts.orders_enriched"}}
	if !reflect.DeepEqual(ev.Outputs, wantOut) {
		t.Errorf("outputs = %+v, want %+v", ev.Outputs, wantOut)
	}
	wantKeys := []string{
		lineageTableKey(leWH, "analytics.orders"),
		lineageTableKey(leWH, "customers"),
		lineageTableKey(leWH, "marts.orders_enriched"),
	}
	if !reflect.DeepEqual(ev.TableKeys, sortedCopy(wantKeys)) {
		t.Errorf("table keys = %q, want %q", ev.TableKeys, sortedCopy(wantKeys))
	}
	if want := columnLineageSQLHash("postgresql", r.SQLText); ev.SQLHash != want {
		t.Errorf("sql hash = %q, want the hash of the SQL that ran %q", ev.SQLHash, want)
	}

	// The table the run recorded writing wins over the model's setting.
	r.RunTarget = "marts.orders_v2"
	if got := modelRunEvent(r).Outputs; !reflect.DeepEqual(got, []lineageDataset{{ConnectionID: leWH, Name: "marts.orders_v2"}}) {
		t.Errorf("outputs with a recorded target = %+v", got)
	}
}

// A model edited after its run started: its SQL now is not the SQL that ran, so the
// event names only the table the run recorded, and no SQL version.
func TestModelRunEvent_EditedSinceTheRunNamesOnlyItsRecordedTarget(t *testing.T) {
	r := leModelRun()
	r.SQLKnown, r.RunTarget, r.Status = false, "Marts.Orders_Enriched", "failed"
	ev := modelRunEvent(r)
	if len(ev.Inputs) != 0 || ev.SQLHash != "" {
		t.Fatalf("an edited model's run named inputs %+v and SQL %q", ev.Inputs, ev.SQLHash)
	}
	if want := []lineageDataset{{ConnectionID: leWH, Name: "marts.orders_enriched"}}; !reflect.DeepEqual(ev.Outputs, want) {
		t.Errorf("outputs = %+v, want %+v", ev.Outputs, want)
	}
	if ev.EventType != lineageEventFail {
		t.Errorf("a failed run is %s", ev.EventType)
	}

	r.RunTarget = ""
	if ev := modelRunEvent(r); len(ev.Outputs) != 0 || len(ev.TableKeys) != 0 {
		t.Errorf("a run that recorded no table named %+v", ev.Outputs)
	}
}

// A model run that was skipped ran nothing: it read no table and wrote none, so its
// event names none, and no table's history lists it. It keeps its SQL version, which an
// event that names the version keeps the version's column lineage alive for
// (saveColumnLineage).
func TestModelRunEvent_ASkippedRunNamesNoTables(t *testing.T) {
	r := leModelRun()
	r.Status = "skipped"
	r.RunTarget = "marts.orders_v2"
	for _, known := range []bool{true, false} {
		r.SQLKnown = known
		ev := modelRunEvent(r)
		if ev.EventType != lineageEventAbort {
			t.Fatalf("a skipped run is %s", ev.EventType)
		}
		if len(ev.Inputs) != 0 || len(ev.Outputs) != 0 || len(ev.TableKeys) != 0 {
			t.Errorf("SQL known %v: a skipped run named inputs %+v, outputs %+v, keys %q", known, ev.Inputs, ev.Outputs, ev.TableKeys)
		}
		// The columns are NOT NULL: the lists are empty, not nil.
		if ev.Inputs == nil || ev.Outputs == nil || ev.TableKeys == nil {
			t.Errorf("SQL known %v: a skipped run's lists are nil", known)
		}
		wantHash := ""
		if known {
			wantHash = columnLineageSQLHash(r.ConnectorType, r.SQLText)
		}
		if ev.SQLHash != wantHash {
			t.Errorf("SQL known %v: sql hash = %q, want %q", known, ev.SQLHash, wantHash)
		}
	}

	// The same run, ended another way, names what it touched.
	for _, status := range []string{"succeeded", "failed"} {
		r.Status, r.SQLKnown = status, true
		if ev := modelRunEvent(r); len(ev.Inputs) == 0 || len(ev.Outputs) == 0 || len(ev.TableKeys) == 0 {
			t.Errorf("a %s run named inputs %+v, outputs %+v", status, ev.Inputs, ev.Outputs)
		}
	}
}

// The one place that says whose events name tables. A pipeline run that was cancelled
// may have moved some before it stopped.
func TestLineageEventNamesTables(t *testing.T) {
	for _, c := range []struct {
		kind, event string
		want        bool
	}{
		{lineageJobModel, lineageEventComplete, true},
		{lineageJobModel, lineageEventFail, true},
		{lineageJobModel, lineageEventAbort, false},
		{lineageJobPipeline, lineageEventComplete, true},
		{lineageJobPipeline, lineageEventFail, true},
		{lineageJobPipeline, lineageEventAbort, true},
	} {
		if got := lineageEventNamesTables(c.kind, c.event); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.kind, c.event, got, c.want)
		}
	}
}

func TestLineageRunEventTypes(t *testing.T) {
	for status, want := range map[string]string{"succeeded": lineageEventComplete, "skipped": lineageEventAbort, "failed": lineageEventFail} {
		if got := modelRunEventType(status); got != want {
			t.Errorf("model run %q = %s, want %s", status, got, want)
		}
	}
	for status, want := range map[string]string{
		"completed": lineageEventComplete, "SUCCESS": lineageEventComplete, "cancelled": lineageEventAbort,
		"failed": lineageEventFail, "silent_drop_detected": lineageEventFail, "credential_check_failed": lineageEventFail,
	} {
		if got := executionEventType(status); got != want {
			t.Errorf("execution %q = %s, want %s", status, got, want)
		}
	}
}

// A pipeline run reads its source tables (no graph node, no key) and writes each under
// its destination name; a table it landed under no destination name is the graph's
// captured table.
func TestPipelineRunEvent_PlacedAndCapturedOutputs(t *testing.T) {
	r := pipelineRunRow{RunRef: "exec-1", PipelineID: lePipeline, WorkspaceID: leWS, EventType: lineageEventComplete,
		EndedAt: leT0, SourceConnectionID: leSource, DestConnectionID: leWH}
	ev := pipelineRunEvent(r, []pipelineRunTable{
		{Qualified: "shop.Orders", DestQualified: "analytics.orders"},
		{TableName: "events"},
		{Qualified: "shop.Orders", DestQualified: "analytics.orders"}, // a repeat
	})
	wantIn := []lineageDataset{{ConnectionID: leSource, Name: "shop.orders"}, {ConnectionID: leSource, Name: "events"}}
	wantOut := []lineageDataset{{ConnectionID: leWH, Name: "analytics.orders"}, {ConnectionID: leWH, Name: "events", Captured: true}}
	if !reflect.DeepEqual(ev.Inputs, wantIn) || !reflect.DeepEqual(ev.Outputs, wantOut) {
		t.Fatalf("inputs %+v outputs %+v, want %+v %+v", ev.Inputs, ev.Outputs, wantIn, wantOut)
	}
	wantKeys := sortedCopy([]string{lineageTableKey(leWH, "analytics.orders"), lineageCapturedKey(leWH, "events")})
	if !reflect.DeepEqual(ev.TableKeys, wantKeys) {
		t.Errorf("table keys = %q, want %q", ev.TableKeys, wantKeys)
	}
	if ev.PipelineID != lePipeline || ev.SavedQueryID != "" {
		t.Errorf("job = %q/%q", ev.PipelineID, ev.SavedQueryID)
	}

	// No destination: nothing landed that the graph draws.
	r.DestConnectionID = ""
	if ev := pipelineRunEvent(r, []pipelineRunTable{{Qualified: "shop.orders"}}); len(ev.Outputs) != 0 || len(ev.TableKeys) != 0 {
		t.Errorf("a pipeline with no destination wrote %+v", ev.Outputs)
	}
}

// The keys a run is stored under are the keys a node's history asks for.
func TestLineageHistoryFilterFor(t *testing.T) {
	placed := "table:" + leWH + "\x00analytics.orders"
	bare := "table:" + leWH + "\x00orders"
	captured := "table:" + leWH + "\x00captured\x00Events"
	for _, tc := range []struct {
		node string
		want lineageHistoryFilter
		ok   bool
	}{
		{"pipeline:" + lePipeline, lineageHistoryFilter{Kind: assetKindPipeline, ID: lePipeline}, true},
		{"model:" + leModel, lineageHistoryFilter{Kind: assetKindModel, ID: leModel}, true},
		{placed, lineageHistoryFilter{Kind: assetKindTable, Keys: []string{lineageTableKey(leWH, "analytics.orders"), lineageTableKey(leWH, "orders")}}, true},
		{bare, lineageHistoryFilter{Kind: assetKindTable, Keys: []string{lineageTableKey(leWH, "orders")}}, true},
		{captured, lineageHistoryFilter{Kind: assetKindTable, Keys: []string{lineageCapturedKey(leWH, "events")}}, true},
		// Nothing rsync.ai records runs against these: an empty history, not an error.
		{"table:" + leWH + "\x00lake\x00s3://b/k", lineageHistoryFilter{}, true},
		{"chart:abc", lineageHistoryFilter{}, true},
		{"dashboard:abc", lineageHistoryFilter{}, true},
		{"pipeline:not-a-uuid", lineageHistoryFilter{}, false},
		{"table:not-a-uuid\x00orders", lineageHistoryFilter{}, false},
		{"table:" + leWH, lineageHistoryFilter{}, false},
		{"orders", lineageHistoryFilter{}, false},
		{"source:" + leWH, lineageHistoryFilter{}, false},
	} {
		got, ok := lineageHistoryFilterFor(tc.node)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %+v %v, want %+v %v", tc.node, got, ok, tc.want, tc.ok)
		}
	}

	// The model run's keys meet the placed table's.
	ev := modelRunEvent(leModelRun())
	f, _ := lineageHistoryFilterFor(placed)
	if !overlaps(ev.TableKeys, f.Keys) {
		t.Errorf("a model reading analytics.orders is not in its history: %q vs %q", ev.TableKeys, f.Keys)
	}
	// And the pipeline run's captured key meets the captured node's.
	pev := pipelineRunEvent(pipelineRunRow{PipelineID: lePipeline, SourceConnectionID: leSource, DestConnectionID: leWH},
		[]pipelineRunTable{{TableName: "Events"}})
	f, _ = lineageHistoryFilterFor(captured)
	if !overlaps(pev.TableKeys, f.Keys) {
		t.Errorf("a captured table's run is not in its history: %q vs %q", pev.TableKeys, f.Keys)
	}
}

func TestModelColumnChanges(t *testing.T) {
	t1, t2, t3 := leT0, leT0.Add(time.Hour), leT0.Add(2*time.Hour)
	got := modelColumnChanges([]modelSQLVersion{
		{FirstRun: t1, Known: true, Columns: []string{"id", "name", "total"}},
		{FirstRun: t2, Known: true, Columns: []string{"ID", "total", "region"}},
		{FirstRun: t3, Known: false},
	})
	want := []lineageColumnChange{
		{At: t2, ColumnsKnown: true, Added: []string{"region"}, Dropped: []string{"name"}},
		{At: t3, ColumnsKnown: false, Added: []string{}, Dropped: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	if got := modelColumnChanges(nil); got == nil || len(got) != 0 {
		t.Errorf("no versions = %#v, want an empty list", got)
	}
}

// An occurrence reads back to the window the sweep fired, and nothing else reads as one.
func TestParseCDCTrailingEdgeOccurrence(t *testing.T) {
	window := 15 * time.Minute
	boundary := time.Date(2026, 10, 14, 9, 15, 0, 0, time.UTC)
	occ := cdcTrailingEdgeOccurrence(window, boundary)
	gotB, gotW, ok := parseCDCTrailingEdgeOccurrence(occ)
	if !ok || !gotB.Equal(boundary) || gotW != window {
		t.Fatalf("%q = %v %v %v, want %v %v", occ, gotB, gotW, ok, boundary, window)
	}
	for _, bad := range []string{"", "run-1", "cdc-trail-", "cdc-trail-15m", "cdc-trail-0m-5", "cdc-trail-xm-5", "cdc-trail-15m-x", "cdc-trail-15m-05"} {
		if _, _, ok := parseCDCTrailingEdgeOccurrence(bad); ok {
			t.Errorf("%q read as an occurrence", bad)
		}
	}
}

func TestLineageEventRetentionDays(t *testing.T) {
	for v, want := range map[string]int{"": 365, "30": 30, " 90 ": 90, "0": 365, "-1": 365, "a year": 365} {
		t.Setenv(LineageEventRetentionDaysEnv, v)
		if got := lineageEventRetentionDays(); got != want {
			t.Errorf("%q = %d, want %d", v, got, want)
		}
	}
}

// A run recorded already is not an error and not a new event.
func TestInsertLineageEvent_AlreadyRecordedIsNoEvent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ev := modelRunEvent(leModelRun())
	mock.ExpectQuery(`INSERT INTO lineage_events(.|\n)*ON CONFLICT DO NOTHING(.|\n)*RETURNING id`).
		WithArgs(leWS, lineageJobModel, leModel, "", "run-1", lineageEventComplete,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), ev.SQLHash).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	id, err := insertLineageEvent(context.Background(), database, ev)
	if err != nil || id != "" {
		t.Fatalf("a repeat = %q %v, want no id and no error", id, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The window is recorded after the fire, whatever the fire answers, with the state row
// still holding the previous fire; the new event goes to the push.
func TestRecordingCDCFire_RecordsTheWindowAfterTheFire(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var pushed []string
	prev := lineagePushHook
	lineagePushHook = func(ids []string) { pushed = append(pushed, ids...) }
	t.Cleanup(func() { lineagePushHook = prev })

	window := 15 * time.Minute
	boundary := time.Date(2026, 10, 14, 9, 15, 0, 0, time.UTC)
	occ := cdcTrailingEdgeOccurrence(window, boundary)
	prevEnd := boundary.Add(-20 * time.Minute) // the last fire was a missed window ago
	landedThrough := boundary.Add(-21 * time.Minute)

	fireErr := errors.New("downstream refused")
	fired := false
	fire := func(_ context.Context, _ *sql.DB, pipelineID, occurrence string) error {
		if pipelineID != lePipeline || occurrence != occ {
			t.Errorf("fired %s %s", pipelineID, occurrence)
		}
		fired = true
		return fireErr
	}
	mock.ExpectQuery(`FROM pipelines p\s+LEFT JOIN cdc_trailing_edge_state s`).WithArgs(lePipeline).
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id", "source", "dest", "window_end", "landed_through"}).
			AddRow(leWS, leSource, leWH, prevEnd, landedThrough))
	mock.ExpectQuery(`FROM cdc_table_watermarks(.|\n)*landed_at > COALESCE\(\$2`).WithArgs(lePipeline, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"table_name", "dest"}).
			AddRow("public.orders", "analytics.orders").AddRow("public.events", ""))
	mock.ExpectQuery(`INSERT INTO lineage_events`).
		WithArgs(leWS, lineageJobPipeline, "", lePipeline, occ, lineageEventComplete,
			sql.NullTime{Time: prevEnd, Valid: true}, boundary,
			`[{"connection_id":"`+leSource+`","name":"public.orders"},{"connection_id":"`+leSource+`","name":"public.events"}]`,
			`[{"connection_id":"`+leWH+`","name":"analytics.orders"},{"connection_id":"`+leWH+`","name":"public.events","captured":true}]`,
			sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ev-1"))

	err = recordingCDCFire(fire)(context.Background(), database, lePipeline, occ)
	if !fired || !errors.Is(err, fireErr) {
		t.Fatalf("fired %v, err %v: the fire's answer must come back as it was", fired, err)
	}
	if !reflect.DeepEqual(pushed, []string{"ev-1"}) {
		t.Errorf("pushed %q", pushed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func lineageHistoryGet(t *testing.T, role, query string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", wsScopeUser)
		c.Set(ctxWorkspaceID, wsScopeWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/lineage/history", GetLineageHistory)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/lineage/history"+query, nil))
	return w
}

func TestGetLineageHistory_NodesWithoutHistoryAndBadNodes(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	w := lineageHistoryGet(t, "viewer", "?node=chart:abc")
	if w.Code != http.StatusOK {
		t.Fatalf("chart = %d %s", w.Code, w.Body)
	}
	var body struct {
		Supported     bool                `json:"supported"`
		RetentionDays int                 `json:"retention_days"`
		Runs          []lineageHistoryRun `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Supported || body.Runs == nil || body.RetentionDays != 365 {
		t.Fatalf("chart body %s (err %v)", w.Body, err)
	}
	if !strings.Contains(w.Body.String(), `"total_runs":0`) {
		t.Errorf("a node with no history reports no total: %s", w.Body)
	}
	if w := lineageHistoryGet(t, "viewer", "?node=nope"); w.Code != http.StatusBadRequest {
		t.Errorf("a bad node = %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a node with no history read the database: %v", err)
	}
}

// Another user's private model is not found, as it is not on the graph.
func TestGetLineageHistory_AnotherUsersPrivateModelIsNotFound(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT visibility = 'workspace' OR created_by::text = \$3\s+FROM saved_queries`).
		WithArgs(leModel, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(false))
	if w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel); w.Code != http.StatusNotFound {
		t.Fatalf("another user's private model = %d %s", w.Code, w.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A model's history: its runs, newest first, and how its columns changed.
func TestGetLineageHistory_ModelRunsAndColumnChanges(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`FROM lineage_events e(.|\n)*sq.visibility = 'workspace' OR sq.created_by::text = \$2(.|\n)*ORDER BY e.ended_at DESC, e.id DESC`).
		WithArgs(wsScopeWS, wsScopeUser, "", leModel, sqlmock.AnyArg(), false, 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "job_kind", "job_id", "job_name", "run_ref", "event_type", "started_at", "ended_at", "inputs", "outputs", "total"}).
			AddRow("ev-2", "model", leModel, "orders_enriched", "run-2", "FAIL", nil, leT0.Add(time.Hour),
				[]byte(`[{"connection_id":"`+leWH+`","name":"analytics.orders"}]`), []byte(`[]`), 1))
	expectSkippedRuns(mock, 0, nil)
	mock.ExpectQuery(`WITH versions AS`).WithArgs(leModel, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}).
			AddRow(leT0, "ok", []byte(`{"columns":["id","name"]}`)).
			AddRow(leT0.Add(time.Hour), "partial", []byte(`{"columns":["id","region"]}`)))

	w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel+"&limit=10")
	if w.Code != http.StatusOK {
		t.Fatalf("model = %d %s", w.Code, w.Body)
	}
	var body struct {
		Supported     bool                  `json:"supported"`
		TotalRuns     int                   `json:"total_runs"`
		Runs          []lineageHistoryRun   `json:"runs"`
		ColumnChanges []lineageColumnChange `json:"column_changes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.TotalRuns != 1 {
		t.Errorf("total runs = %d", body.TotalRuns)
	}
	if !body.Supported || len(body.Runs) != 1 || body.Runs[0].EventType != "FAIL" || body.Runs[0].StartedAt != nil ||
		len(body.Runs[0].Inputs) != 1 || body.Runs[0].Outputs == nil {
		t.Fatalf("runs = %+v", body.Runs)
	}
	if len(body.ColumnChanges) != 1 || !reflect.DeepEqual(body.ColumnChanges[0].Added, []string{"region"}) ||
		!reflect.DeepEqual(body.ColumnChanges[0].Dropped, []string{"name"}) {
		t.Fatalf("column changes = %+v", body.ColumnChanges)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The page says how many runs there are, not only how many it was sent: the list is
// capped by ?limit=, the total is every run recorded for the node.
func TestGetLineageHistory_TotalRunsIsEveryRunNotThePage(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`count\(\*\) OVER\(\)(.|\n)*FROM lineage_events e`).
		WithArgs(wsScopeWS, wsScopeUser, "", leModel, sqlmock.AnyArg(), false, 2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "job_kind", "job_id", "job_name", "run_ref", "event_type", "started_at", "ended_at", "inputs", "outputs", "total"}).
			AddRow("ev-3", "model", leModel, "orders_enriched", "run-3", "COMPLETE", leT0, leT0.Add(2*time.Hour), []byte(`[]`), []byte(`[]`), 54).
			AddRow("ev-2", "model", leModel, "orders_enriched", "run-2", "COMPLETE", leT0, leT0.Add(time.Hour), []byte(`[]`), []byte(`[]`), 54))
	expectSkippedRuns(mock, 0, nil)
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))

	w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel+"&limit=2")
	var body struct {
		TotalRuns int                 `json:"total_runs"`
		Runs      []lineageHistoryRun `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if len(body.Runs) != 2 || body.TotalRuns != 54 {
		t.Errorf("sent %d runs of %d, want 2 of 54", len(body.Runs), body.TotalRuns)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// expectSkippedRuns is the model path's second read: how many of its runs were skipped,
// and when the last one ended.
func expectSkippedRuns(mock sqlmock.Sqlmock, n int, last any) {
	mock.ExpectQuery(`SELECT count\(\*\), max\(e.ended_at\)\s+FROM lineage_events e`).
		WithArgs(wsScopeWS, wsScopeUser, "", leModel, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count", "max"}).AddRow(n, last))
}

var leHistoryCols = []string{"id", "job_kind", "job_id", "job_name", "run_ref", "event_type", "started_at", "ended_at", "inputs", "outputs", "total"}

// A skipped run is not a run that ran: the list leaves it out unless asked, so a model
// that is skipped every hour does not fill its newest-50 window with runs that did
// nothing. The page is told how many it left out, and when the last was.
func TestGetLineageHistory_SkippedRunsAreCountedNotListed(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	// The list asks for no skipped run, by the predicate that says what a skipped run is.
	mock.ExpectQuery(`FROM lineage_events e(.|\n)*AND \(\$6 OR NOT \(e\.job_kind = 'model' AND e\.event_type = 'ABORT'\)\)`).
		WithArgs(wsScopeWS, wsScopeUser, "", leModel, sqlmock.AnyArg(), false, lineageHistoryDefaultLimit).
		WillReturnRows(sqlmock.NewRows(leHistoryCols).
			AddRow("ev-1", "model", leModel, "orders_enriched", "run-1", "COMPLETE", leT0, leT0.Add(time.Minute), []byte(`[]`), []byte(`[]`), 1))
	expectSkippedRuns(mock, 53, leT0.Add(9*time.Hour))
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))

	w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel)
	var body struct {
		TotalRuns     int                 `json:"total_runs"`
		Runs          []lineageHistoryRun `json:"runs"`
		SkippedRuns   int                 `json:"skipped_runs"`
		LastSkippedAt *time.Time          `json:"last_skipped_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if len(body.Runs) != 1 || body.Runs[0].EventType != "COMPLETE" || body.TotalRuns != 1 {
		t.Errorf("runs = %+v of %d, want the one COMPLETE run", body.Runs, body.TotalRuns)
	}
	if body.SkippedRuns != 53 || body.LastSkippedAt == nil || !body.LastSkippedAt.Equal(leT0.Add(9*time.Hour)) {
		t.Errorf("skipped_runs = %d, last_skipped_at = %v, want 53 and the last one's end", body.SkippedRuns, body.LastSkippedAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ?skipped=1 puts them back in the list; the count is still sent, for the page's toggle.
func TestGetLineageHistory_SkippedParamListsSkippedRuns(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`FROM lineage_events e`).
		WithArgs(wsScopeWS, wsScopeUser, "", leModel, sqlmock.AnyArg(), true, lineageHistoryDefaultLimit).
		WillReturnRows(sqlmock.NewRows(leHistoryCols).
			AddRow("ev-2", "model", leModel, "orders_enriched", "run-2", "ABORT", leT0, leT0, []byte(`[]`), []byte(`[]`), 2).
			AddRow("ev-1", "model", leModel, "orders_enriched", "run-1", "COMPLETE", leT0, leT0.Add(time.Minute), []byte(`[]`), []byte(`[]`), 2))
	expectSkippedRuns(mock, 1, leT0)
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))

	w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel+"&skipped=1")
	var body struct {
		TotalRuns   int                 `json:"total_runs"`
		Runs        []lineageHistoryRun `json:"runs"`
		SkippedRuns int                 `json:"skipped_runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if len(body.Runs) != 2 || body.TotalRuns != 2 || body.SkippedRuns != 1 {
		t.Errorf("runs %d of %d, skipped_runs %d, want 2 of 2 and 1", len(body.Runs), body.TotalRuns, body.SkippedRuns)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A model that has only ever been skipped has no run to list, and the page must be told
// why, not left to say "no runs".
func TestGetLineageHistory_AModelThatWasOnlyEverSkipped(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`FROM lineage_events e`).WillReturnRows(sqlmock.NewRows(leHistoryCols))
	expectSkippedRuns(mock, 4, leT0)
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))

	w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel)
	var body struct {
		TotalRuns   int                 `json:"total_runs"`
		Runs        []lineageHistoryRun `json:"runs"`
		SkippedRuns int                 `json:"skipped_runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if len(body.Runs) != 0 || body.TotalRuns != 0 || body.SkippedRuns != 4 {
		t.Errorf("runs %d of %d, skipped_runs %d, want none listed and 4 skipped", len(body.Runs), body.TotalRuns, body.SkippedRuns)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Only a model is skipped (a pipeline's ABORT is a cancelled run, and a skipped model run
// names no table): a pipeline's and a table's history do not read the count, and say none.
func TestGetLineageHistory_OnlyAModelHasSkippedRuns(t *testing.T) {
	for _, node := range []string{"pipeline:" + lePipeline, "table:" + leWH + "%00analytics.orders"} {
		mock, cleanup := withMockDB(t)
		mock.ExpectQuery(`FROM lineage_events e`).WillReturnRows(sqlmock.NewRows(leHistoryCols))
		w := lineageHistoryGet(t, "viewer", "?node="+node)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"skipped_runs":0`) ||
			!strings.Contains(w.Body.String(), `"last_skipped_at":null`) {
			t.Errorf("%s = %d %s", node, w.Code, w.Body)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", node, err)
		}
		cleanup()
	}
}

// The runs a page lists and the skipped runs it counts are filtered by one text, so a
// visibility rule changed in one cannot be left behind in the other.
func TestGetLineageHistory_ListAndSkippedCountShareTheNodeFilter(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	filter := regexp.QuoteMeta(lineageHistoryNodeFilterSQL)
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`FROM lineage_events e(.|\n)*` + filter).WillReturnRows(sqlmock.NewRows(leHistoryCols))
	mock.ExpectQuery(`SELECT count\(\*\), max\(e.ended_at\)(.|\n)*` + filter).
		WillReturnRows(sqlmock.NewRows([]string{"count", "max"}).AddRow(0, nil))
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))
	if w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The skipped count failing is a failed read, as the list failing is.
func TestGetLineageHistory_SkippedCountErrorIsAFailedRead(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1::uuid`).
		WillReturnRows(sqlmock.NewRows([]string{"visible"}).AddRow(true))
	mock.ExpectQuery(`FROM lineage_events e`).WillReturnRows(sqlmock.NewRows(leHistoryCols))
	mock.ExpectQuery(`SELECT count\(\*\), max\(e.ended_at\)`).WillReturnError(errors.New("boom"))
	// What a handler that swallowed the error would go on to read.
	mock.ExpectQuery(`WITH versions AS`).WillReturnRows(sqlmock.NewRows([]string{"first_run", "status", "result"}))
	if w := lineageHistoryGet(t, "viewer", "?node=model:"+leModel); w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed skipped count = %d %s, want 500", w.Code, w.Body)
	}
}

// A model run that was skipped read and wrote nothing. The 110 recorded before the
// recorder knew it still carry the tables of the SQL as it stood; the page does not show
// them as a run's reads and writes. A cancelled pipeline run keeps its tables.
func TestGetLineageHistory_ASkippedModelRunIsReadWithoutTables(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	mock.ExpectQuery(`FROM lineage_events e`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "job_kind", "job_id", "job_name", "run_ref", "event_type", "started_at", "ended_at", "inputs", "outputs", "total"}).
			AddRow("ev-skip", "model", leModel, "orders_enriched", "run-3", "ABORT", leT0, leT0.Add(2*time.Hour),
				[]byte(`[{"connection_id":"`+leWH+`","name":"analytics.orders"}]`), []byte(`[{"connection_id":"`+leWH+`","name":"marts.orders_enriched"}]`), 3).
			AddRow("ev-cancel", "pipeline", lePipeline, "orders sync", "exec-1", "ABORT", leT0, leT0.Add(time.Hour),
				[]byte(`[{"connection_id":"`+leSource+`","name":"orders"}]`), []byte(`[{"connection_id":"`+leWH+`","name":"orders"}]`), 3).
			AddRow("ev-fail", "model", leModel, "orders_enriched", "run-1", "FAIL", leT0, leT0.Add(time.Minute),
				[]byte(`[{"connection_id":"`+leWH+`","name":"analytics.orders"}]`), []byte(`[]`), 3))

	w := lineageHistoryGet(t, "viewer", "?node=table:"+leWH+"%00analytics.orders")
	var body struct {
		Runs []lineageHistoryRun `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || len(body.Runs) != 3 {
		t.Fatalf("%d %s (%v)", w.Code, w.Body, err)
	}
	if skipped := body.Runs[0]; len(skipped.Inputs) != 0 || len(skipped.Outputs) != 0 || skipped.Inputs == nil || skipped.Outputs == nil {
		t.Errorf("a skipped model run shows inputs %+v, outputs %+v (lists must be empty, not null)", skipped.Inputs, skipped.Outputs)
	}
	if cancelled := body.Runs[1]; len(cancelled.Inputs) != 1 || len(cancelled.Outputs) != 1 {
		t.Errorf("a cancelled pipeline run lost its tables: %+v / %+v", cancelled.Inputs, cancelled.Outputs)
	}
	if failed := body.Runs[2]; len(failed.Inputs) != 1 {
		t.Errorf("a failed model run lost its inputs: %+v", failed.Inputs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A table's history asks by the table's keys only.
func TestGetLineageHistory_TableAsksByItsKeys(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	keys := `{"` + leWH + "\x1fanalytics.orders" + `","` + leWH + "\x1forders" + `"}`
	mock.ExpectQuery(`FROM lineage_events e`).
		WithArgs(wsScopeWS, wsScopeUser, "", "", keyArrayArg(keys), false, lineageHistoryDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "job_kind", "job_id", "job_name", "run_ref", "event_type", "started_at", "ended_at", "inputs", "outputs", "total"}))
	w := lineageHistoryGet(t, "viewer", "?node=table:"+leWH+"%00analytics.orders")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"runs":[]`) {
		t.Fatalf("table = %d %s", w.Code, w.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// keyArrayArg matches a text[] bound as its Postgres literal.
type keyArrayArg string

func (a keyArrayArg) Match(v any) bool {
	if s, ok := v.(string); ok {
		return s == string(a)
	}
	return false
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
