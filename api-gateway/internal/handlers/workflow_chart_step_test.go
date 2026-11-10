package handlers

// A workflow's chart step (workflow_chart_step.go): what it draws and holds, as
// whom, what each failure says, and how the sends after it show the chart.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"api-gateway/internal/charts"
	"api-gateway/internal/security"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// fakeWorkflowCharts holds drawings in memory, through JSON as Redis would.
type fakeWorkflowCharts struct {
	held    map[string]workflowChart
	ttls    map[string]time.Duration
	deleted []string
}

func (f *fakeWorkflowCharts) Put(_ context.Context, key string, c workflowChart, ttl time.Duration) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	var back workflowChart
	if err := json.Unmarshal(raw, &back); err != nil {
		return err
	}
	f.held[key], f.ttls[key] = back, ttl
	return nil
}

func (f *fakeWorkflowCharts) Get(_ context.Context, key string) (workflowChart, bool, error) {
	c, ok := f.held[key]
	return c, ok, nil
}

func (f *fakeWorkflowCharts) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.held, key)
	return nil
}

func stubWorkflowCharts(t *testing.T) *fakeWorkflowCharts {
	t.Helper()
	f := &fakeWorkflowCharts{held: map[string]workflowChart{}, ttls: map[string]time.Duration{}}
	prev := workflowCharts
	workflowCharts = f
	t.Cleanup(func() { workflowCharts = prev })
	return f
}

// chartDrawCall is what the step asked the chart's draw for.
type chartDrawCall struct {
	ws, actor, chart string
}

func stubChartDraw(t *testing.T, d chartDraw) *[]chartDrawCall {
	t.Helper()
	var calls []chartDrawCall
	prev := drawWorkflowChart
	drawWorkflowChart = func(_ context.Context, _ *sql.DB, ws, userID string, r *chartRecord, _ []charts.Bound,
		_ time.Time) chartDraw {
		calls = append(calls, chartDrawCall{ws: ws, actor: userID, chart: r.ID})
		return d
	}
	t.Cleanup(func() { drawWorkflowChart = prev })
	return &calls
}

// chartPNGCall is what the step handed the frontend to draw.
type chartPNGCall struct {
	name     string
	settings string
	rows     int
}

var fakeChartPNG = append(append([]byte{}, pngSignature...), "picture"...)

func stubChartPNG(t *testing.T, png []byte, err error) *[]chartPNGCall {
	t.Helper()
	var calls []chartPNGCall
	prev := drawWorkflowChartPNG
	drawWorkflowChartPNG = func(_ context.Context, name string, settings json.RawMessage, res *chartResult) ([]byte, error) {
		calls = append(calls, chartPNGCall{name: name, settings: string(settings), rows: len(res.Rows)})
		return png, err
	}
	t.Cleanup(func() { drawWorkflowChartPNG = prev })
	return &calls
}

const (
	chartStepBar       = `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"}}`
	chartStepScorecard = `{"type":"scorecard","measure":{"aggregate":"count"}}`
	// chartStepBarV2 is chartStepBar as a chart's read hands it on, at version 2.
	chartStepBarV2 = `{"version":2,"type":"bar","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"day"}]}}`
)

// chartStepRun is a manual run of a chart, then a send.
func chartStepRun() *workflowRunRow {
	def := workflow.Definition{
		Version: 3,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{
			{ID: "chart", Type: workflow.StepChart, ChartID: chartID},
			{ID: "send_1", Type: workflow.StepSend, After: []string{"chart"},
				Message: &workflow.Message{Subject: "Orders", Body: "This week's orders"}},
		},
	}
	return &workflowRunRow{RunID: wfRunUUID, Trigger: workflow.TriggerManual, RunStatus: "running", WorkflowID: wfID,
		WorkspaceID: wsScopeWS, Name: "Orders", Status: "active", Definition: def, RunAsUserID: wsScopeUser}
}

// chartStepCall is the chart step's run call on database, and what it answered.
func chartStepCall(t *testing.T, database *sql.DB, run *workflowRunRow, stepID string) (*graphStepCall, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	step := run.Definition.Step(stepID)
	if step == nil {
		t.Fatalf("no step %q", stepID)
	}
	s := &graphStepCall{c: c, ctx: context.Background(), db: database, run: run, step: *step, attempt: 1}
	s.unavail = func(err error, what string) {
		s.answered = true
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": what})
	}
	return s, w
}

func expectStepChart(mock sqlmock.Sqlmock, settings string) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chartID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "saved_query_id", "visible", "source_name",
			"materialization", "settings", "settings_version", "source_columns", "visibility", "proposed_by",
			"created_by", "created_at", "updated_at"}).
			AddRow(chartID, "Orders by day", "44444444-4444-4444-4444-444444444444", true, "orders", "view",
				[]byte(settings), 1, []byte(`["day"]`), "workspace", "person", wsScopeUser, at, at))
}

// expectStepSettled expects the chart step's outcome written, then read back.
func expectStepSettled(mock sqlmock.Sqlmock, status, errText string) {
	expectStepSettledFor(mock, "chart", status, errText)
}

// expectStepSettledFor expects stepID's outcome written, then read back.
func expectStepSettledFor(mock sqlmock.Sqlmock, stepID, status, errText string) {
	e := interface{}(sqlmock.AnyArg())
	if errText != "" {
		e = errText
	}
	mock.ExpectExec(`UPDATE workflow_step_runs SET status = \$4`).
		WithArgs(wfRunUUID, stepID, 1, status, e, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	var errCol interface{}
	if errText != "" {
		errCol = errText
	}
	mock.ExpectQuery(`SELECT status, execution_id, saved_query_run_id::text, error, note, row_count, case_index, requested_at`).
		WithArgs(wfRunUUID, stepID, 1).
		WillReturnRows(sqlmock.NewRows([]string{"status", "execution_id", "saved_query_run_id", "error", "note",
			"row_count", "case_index", "requested_at"}).AddRow(status, nil, nil, errCol, nil, nil, nil, nil))
}

func stepAnswerOf(t *testing.T, w *httptest.ResponseRecorder) graphStepAnswer {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var a graphStepAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func barDraw() chartDraw {
	return chartDraw{Result: &chartResult{
		Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "count", Role: chartRoleMeasure}},
		Rows: [][]interface{}{
			{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), int64(5)},
			{time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), int64(1234)},
		},
	}}
}

// ---------------------------------------------------------------------------
// The step
// ---------------------------------------------------------------------------

// A bar chart is drawn as the run's actor, as a picture and as numbers, and held
// for the sends for as long as the run may last.
func TestRunChart_PlottedChartIsHeldAsPictureAndNumbers(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	held := stubWorkflowCharts(t)
	draws := stubChartDraw(t, barDraw())
	pngs := stubChartPNG(t, fakeChartPNG, nil)
	t.Setenv("APP_BASE_URL", "https://app.example.test/")

	run := chartStepRun()
	expectRole(mock, "owner")
	expectStepChart(mock, chartStepBar)
	expectStepSettled(mock, workflow.StatusSucceeded, "")
	s, w := chartStepCall(t, database, run, "chart")
	s.runChart()
	mockMet(t, mock)

	if a := stepAnswerOf(t, w); a.Status != workflow.StatusSucceeded || a.Refused {
		t.Fatalf("answer = %+v", a)
	}
	if want := []chartDrawCall{{ws: wsScopeWS, actor: wsScopeUser, chart: chartID}}; !reflect.DeepEqual(*draws, want) {
		t.Fatalf("draws = %+v, want %+v", *draws, want)
	}
	if want := []chartPNGCall{{name: "Orders by day", settings: chartStepBarV2, rows: 2}}; !reflect.DeepEqual(*pngs, want) {
		t.Fatalf("pictures = %+v, want %+v", *pngs, want)
	}
	key := workflowResultKey(wfRunUUID, "chart")
	c, ok := held.held[key]
	if !ok {
		t.Fatalf("nothing held under %s", key)
	}
	if held.ttls[key] != workflowResultTTL(run.Definition) {
		t.Fatalf("ttl = %v, want %v", held.ttls[key], workflowResultTTL(run.Definition))
	}
	if !bytes.Equal(c.PNG, fakeChartPNG) || c.StepID != "chart" || c.ChartID != chartID || c.Name != "Orders by day" ||
		c.Type != charts.TypeBar || c.Link != "https://app.example.test/charts/"+chartID {
		t.Fatalf("held = %+v", c)
	}
	want := chartText{Head: []string{"day", "Count"}, Numeric: []bool{false, true},
		Rows: [][]string{{"2026-10-01", "5"}, {"2026-10-02", "1,234"}}}
	if !reflect.DeepEqual(c.Text, want) {
		t.Fatalf("text = %+v, want %+v", c.Text, want)
	}
}

// A scorecard has no picture: its number is held, and the frontend is not asked.
func TestRunChart_ScorecardIsHeldAsItsNumber(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	held := stubWorkflowCharts(t)
	stubChartDraw(t, chartDraw{Result: &chartResult{
		Columns: []chartOutput{{Alias: "count", Role: chartRoleMeasure}},
		Rows:    [][]interface{}{{int64(40005)}},
	}})
	pngs := stubChartPNG(t, fakeChartPNG, nil)

	expectRole(mock, "admin")
	expectStepChart(mock, chartStepScorecard)
	expectStepSettled(mock, workflow.StatusSucceeded, "")
	s, w := chartStepCall(t, database, chartStepRun(), "chart")
	s.runChart()
	mockMet(t, mock)

	if a := stepAnswerOf(t, w); a.Status != workflow.StatusSucceeded {
		t.Fatalf("answer = %+v", a)
	}
	if len(*pngs) != 0 {
		t.Fatalf("the frontend was asked to draw a scorecard: %+v", *pngs)
	}
	c := held.held[workflowResultKey(wfRunUUID, "chart")]
	if c.PNG != nil || c.Text.Label != "Count" || c.Text.Number != "40,005" {
		t.Fatalf("held = %+v", c)
	}
}

// Each way a chart step fails says why, holds nothing, and asks for no picture it
// cannot use.
func TestRunChart_Failures(t *testing.T) {
	masked := barDraw()
	masked.Result.Masked = []string{"count"}
	for _, tc := range []struct {
		name     string
		draw     chartDraw
		pngErr   error
		settings string
		want     string
		pictures int
		held     bool
	}{
		{name: "source deleted", draw: chartDraw{Break: &chartBreak{Reason: "source_deleted",
			Message: "This chart's source was deleted."}}, want: "This chart's source was deleted."},
		{name: "query failed", draw: chartDraw{Error: "the connection refused the query"},
			want: "the connection refused the query"},
		{name: "no result", draw: chartDraw{}, want: "The chart's query gave no result."},
		{name: "picture failed", draw: barDraw(), pngErr: errors.New("the frontend answered 503"),
			want: "The chart could not be drawn as a picture: the frontend answered 503", pictures: 1},
		{name: "settings no longer valid", draw: barDraw(), settings: `{"type":"pie"}`,
			want: "This chart's settings no longer pass the checks: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			held := stubWorkflowCharts(t)
			stubChartDraw(t, tc.draw)
			pngs := stubChartPNG(t, fakeChartPNG, tc.pngErr)
			settings := tc.settings
			if settings == "" {
				settings = chartStepBar
			}
			want := tc.want
			if tc.settings != "" {
				_, derr := charts.Decode([]byte(tc.settings))
				if derr == nil {
					t.Fatal("the settings pass the checks")
				}
				want += derr.Error()
			}
			expectRole(mock, "owner")
			expectStepChart(mock, settings)
			expectStepSettled(mock, workflow.StatusFailed, want)
			s, w := chartStepCall(t, database, chartStepRun(), "chart")
			s.runChart()
			mockMet(t, mock)
			if a := stepAnswerOf(t, w); a.Status != workflow.StatusFailed || a.Refused {
				t.Fatalf("answer = %+v", a)
			}
			if len(*pngs) != tc.pictures {
				t.Fatalf("pictures asked = %d, want %d", len(*pngs), tc.pictures)
			}
			if len(held.held) != 0 {
				t.Fatalf("held = %+v", held.held)
			}
		})
	}

	// A masked measure is not a failure: the chart is held with the page's words,
	// and no picture is drawn.
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	held := stubWorkflowCharts(t)
	stubChartDraw(t, masked)
	pngs := stubChartPNG(t, fakeChartPNG, nil)
	expectRole(mock, "owner")
	expectStepChart(mock, chartStepBar)
	expectStepSettled(mock, workflow.StatusSucceeded, "")
	s, _ := chartStepCall(t, database, chartStepRun(), "chart")
	s.runChart()
	mockMet(t, mock)
	if len(*pngs) != 0 {
		t.Fatalf("a masked chart was drawn: %+v", *pngs)
	}
	if c := held.held[workflowResultKey(wfRunUUID, "chart")]; c.Text.Message != chartMasked || c.PNG != nil {
		t.Fatalf("held = %+v", c)
	}
}

// A chart the actor cannot see, and an actor who may no longer run the
// workflow, are refusals: nothing is drawn, nothing is held.
func TestRunChart_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expect func(sqlmock.Sqlmock)
		want   string
	}{
		{name: "chart not visible", expect: func(mock sqlmock.Sqlmock) {
			expectRole(mock, "owner")
			mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chartID).
				WillReturnError(sql.ErrNoRows)
		}, want: "The chart was deleted, or the run-as user cannot see it."},
		{name: "actor no longer a member", expect: func(mock sqlmock.Sqlmock) {
			expectRole(mock, "")
		}, want: workflowRoleRefusal("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			held := stubWorkflowCharts(t)
			draws := stubChartDraw(t, barDraw())
			pngs := stubChartPNG(t, fakeChartPNG, nil)
			tc.expect(mock)
			expectStepSettled(mock, workflow.StatusFailed, tc.want)
			s, w := chartStepCall(t, database, chartStepRun(), "chart")
			s.runChart()
			mockMet(t, mock)
			if a := stepAnswerOf(t, w); a.Status != workflow.StatusFailed || !a.Refused {
				t.Fatalf("answer = %+v", a)
			}
			if len(*draws) != 0 || len(*pngs) != 0 || len(held.held) != 0 {
				t.Fatalf("draws %d, pictures %d, held %d", len(*draws), len(*pngs), len(held.held))
			}
		})
	}
}

// A send reads the charts before it in its own order; a chart whose step drew
// nothing is a gap with a note, not an error.
func TestSendDrawings_Charts(t *testing.T) {
	held := stubWorkflowCharts(t)
	run := chartStepRun()
	s := &graphStepCall{ctx: context.Background(), run: run, step: *run.Definition.Step("send_1")}
	held.held[workflowResultKey(run.RunID, "chart")] = workflowChart{StepID: "chart", Name: "Orders by day"}

	ds, notes, err := s.sendDrawings()
	if err != nil || len(ds) != 1 || ds[0].Chart == nil || ds[0].Chart.Name != "Orders by day" || len(notes) != 0 {
		t.Fatalf("drawings %+v, notes %v, err %v", ds, notes, err)
	}

	delete(held.held, workflowResultKey(run.RunID, "chart"))
	ds, notes, err = s.sendDrawings()
	if err != nil || len(ds) != 1 || ds[0].Chart != nil || ds[0].Kind != workflow.StepChart || len(notes) != 1 ||
		!strings.Contains(notes[0], `The chart step "chart" drew nothing`) {
		t.Fatalf("drawings %+v, notes %v, err %v", ds, notes, err)
	}
}

// A type the registry marks not static is not sent to the frontend to draw: the message
// keeps its numbers and says the picture is in rsync.ai (dashboards-design.md § 14.5).
// Every type is static today, so the test marks bar not static for its length.
func TestMessageChart_NotStaticTypeGoesWithoutAPicture(t *testing.T) {
	pngs := stubChartPNG(t, fakeChartPNG, nil)
	spec := charts.Spec(charts.TypeBar)
	spec.Static = false
	t.Cleanup(func() { spec.Static = true })

	d := barDraw()
	c, err := messageChart(context.Background(), &chartRecord{ID: chartID, Name: "Orders by day", Settings: json.RawMessage(chartStepBar)}, d.Result)
	if err != nil {
		t.Fatal(err)
	}
	if len(*pngs) != 0 || c.PNG != nil {
		t.Fatalf("drawn: calls %+v, png %d bytes", *pngs, len(c.PNG))
	}
	if !reflect.DeepEqual(c.Text.Notes, []string{charts.NotStatic}) || len(c.Text.Rows) != 2 {
		t.Fatalf("text = %+v", c.Text)
	}

	spec.Static = true
	if c, err = messageChart(context.Background(), &chartRecord{ID: chartID, Name: "Orders by day", Settings: json.RawMessage(chartStepBar)}, d.Result); err != nil ||
		!bytes.Equal(c.PNG, fakeChartPNG) || len(c.Text.Notes) != 0 {
		t.Fatalf("static: png %d bytes, notes %v, err %v", len(c.PNG), c.Text.Notes, err)
	}
}

// chartPlotted is the registry's types less the three a message shows as text.
func TestChartPlotted(t *testing.T) {
	for _, spec := range charts.Registry {
		want := spec.Type != charts.TypeScorecard && spec.Type != charts.TypeTable && spec.Type != charts.TypePivot
		if got := chartPlotted(spec.Type); got != want {
			t.Errorf("chartPlotted(%q) = %v, want %v", spec.Type, got, want)
		}
	}
	if chartPlotted("gauge") || chartPlotted("") {
		t.Error("a type outside the registry is plotted")
	}
}

// chartDrawings are charts as the drawings a send runs after; nil is a chart
// step that drew nothing.
func chartDrawings(cs []*workflowChart) []workflowDrawing {
	var out []workflowDrawing
	for _, c := range cs {
		out = append(out, workflowDrawing{Kind: workflow.StepChart, Chart: c})
	}
	return out
}

// ---------------------------------------------------------------------------
// The numbers, as the chart's page writes them
// ---------------------------------------------------------------------------

// TestFormatChartNumber: a workflow's message writes a chart's numbers as the chart page
// does. shared/chart_number_format_golden.json is read by saved-chart.test.ts too, which
// checks each text against Intl.NumberFormat("en-US"), so a change to either side fails
// one of the two.
func TestFormatChartNumber(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/chart_number_format_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			Value  float64 `json:"value"`
			Format string  `json:"format"`
			Text   string  `json:"text"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("no cases in shared/chart_number_format_golden.json")
	}
	for _, c := range golden.Cases {
		if got := formatChartNumber(c.Value, c.Format); got != c.Text {
			t.Errorf("formatChartNumber(%v, %s) = %q, want %q", c.Value, c.Format, got, c.Text)
		}
	}
}

// JavaScript's toFixed(1) sends an exact half up; Go's rounding sends it to even.
func TestChartToFixed1(t *testing.T) {
	for v, want := range map[float64]string{0.25: "0.3", 0.75: "0.8", 1.25: "1.3", 1.15: "1.1", 1.05: "1.1",
		2.5: "2.5", 9.95: "9.9", 0.05: "0.1", 5: "5.0"} {
		if got := chartToFixed1(v); got != want {
			t.Errorf("chartToFixed1(%v) = %q, want %q", v, got, want)
		}
	}
}

// saved-chart.ts changeText, word for word.
func TestChartChangeText(t *testing.T) {
	for _, tc := range []struct {
		value, previous float64
		hasV, hasP      bool
		days            int
		want            string
	}{
		{110, 100, true, true, 7, "+10% on the 7 days before"},
		{90, 100, true, true, 30, "−10% on the 30 days before"},
		{105, 100, true, true, 1, "+5.0% on the day before"},
		{99.5, 100, true, true, 7, "−0.5% on the 7 days before"},
		{110.5, 100, true, true, 7, "+11% on the 7 days before"},
		{-50, -100, true, true, 7, "+50% on the 7 days before"},
		{100, 100, true, true, 7, "No change on the 7 days before"},
		{0, 0, true, true, 7, "No change on the 7 days before"},
		{5, 0, true, true, 7, "Up from 0 on the 7 days before"},
		{5, 0, true, false, 1, "No number for the day before"},
		{12345, 1, true, true, 7, "+1,234,400% on the 7 days before"},
		{-12345, 1, true, true, 7, "−1,234,600% on the 7 days before"},
		{0, 5, false, true, 90, "No number for the 90 days before"},
	} {
		if got := chartChangeText(tc.value, tc.hasV, tc.previous, tc.hasP, tc.days); got != tc.want {
			t.Errorf("change(%v, %v, %d) = %q, want %q", tc.value, tc.previous, tc.days, got, tc.want)
		}
	}
}

func TestChartLabelAndNumber(t *testing.T) {
	for v, want := range map[interface{}]string{
		nil:                        "(empty)",
		"2026-10-01T00:00:00Z":     "2026-10-01",
		"2026-10-01 00:00:00+00":   "2026-10-01",
		"2026-10-01T00:00:00.000Z": "2026-10-01",
		"2026-10-01T10:00:00Z":     "2026-10-01T10:00:00Z",
		json.Number("12"):          "12",
		true:                       "true",
		"Bengaluru":                "Bengaluru",
	} {
		if got := chartLabel(v); got != want {
			t.Errorf("chartLabel(%#v) = %q, want %q", v, got, want)
		}
	}
	for _, tc := range []struct {
		v    interface{}
		want float64
		ok   bool
	}{
		{json.Number("1.5"), 1.5, true},
		{"12.50", 12.5, true},
		{" 7 ", 7, true},
		{"1e3", 1000, true},
		{".5", 0.5, true},
		{"abc", 0, false},
		{"Infinity", 0, false},
		{"1e400", 0, false},
		{nil, 0, false},
		{true, 0, false},
	} {
		got, ok := chartNumber(tc.v)
		if got != tc.want || ok != tc.ok {
			t.Errorf("chartNumber(%#v) = %v, %v; want %v, %v", tc.v, got, ok, tc.want, tc.ok)
		}
	}
	// A cell reaches the text as the chart's page receives it, through JSON.
	row := []interface{}{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), []byte("ab"), int64(9007199254740993)}
	if got := chartLabel(chartCell(row, 0)); got != "2026-10-01" {
		t.Errorf("time cell = %q", got)
	}
	if got := chartLabel(chartCell(row, 1)); got != "YWI=" {
		t.Errorf("bytes cell = %q", got)
	}
	if got := chartLabel(chartCell(row, 2)); got != "9007199254740993" {
		t.Errorf("bigint cell = %q", got)
	}
	if chartCell(row, 3) != nil || chartCell(row, -1) != nil {
		t.Error("a cell past the row is not nil")
	}
}

func TestWorkflowChartText(t *testing.T) {
	sum := []charts.Field{{Aggregate: charts.AggSum, Column: "amount"}}
	count := []charts.Field{{Aggregate: charts.AggCount}}
	day := []charts.Field{{Column: "day"}}

	t.Run("scorecard with its earlier period", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeScorecard, Roles: map[string][]charts.Field{charts.RoleMeasure: sum, charts.RoleDate: day},
			Compare: &charts.Compare{Days: 7}}
		res := &chartResult{Columns: []chartOutput{{Alias: "amount", Role: chartRoleMeasure},
			{Alias: charts.PreviousAlias, Role: chartRolePrevious}}, Rows: [][]interface{}{{1234.56, "1000"}}}
		got := workflowChartText(s, res)
		want := chartText{Label: "Sum of amount · last 7 days", Number: "1,234.6",
			Change: "+23% on the 7 days before (1,000)"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("scorecard with no earlier number", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeScorecard, Roles: map[string][]charts.Field{charts.RoleMeasure: count, charts.RoleDate: day},
			Compare: &charts.Compare{Days: 1}}
		res := &chartResult{Columns: []chartOutput{{Alias: "count", Role: chartRoleMeasure},
			{Alias: charts.PreviousAlias, Role: chartRolePrevious}}, Rows: [][]interface{}{{int64(5), nil}}}
		got := workflowChartText(s, res)
		if got.Label != "Count · last day" || got.Number != "5" || got.Change != "No number for the day before" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("scorecard with no rows", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeScorecard, Roles: map[string][]charts.Field{charts.RoleMeasure: count}}
		got := workflowChartText(s, &chartResult{Columns: []chartOutput{{Alias: "count", Role: chartRoleMeasure}}})
		if got.Number != "–" || got.Message != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("scorecard masked", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeScorecard, Roles: map[string][]charts.Field{charts.RoleMeasure: sum}}
		got := workflowChartText(s, &chartResult{Columns: []chartOutput{{Alias: "amount", Role: chartRoleMeasure}},
			Rows: [][]interface{}{{int64(5)}}, Masked: []string{"amount"}})
		if got.Message != chartMasked || got.Number != "" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("table: first rows, the rest counted, cells capped", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeTable, Roles: map[string][]charts.Field{
			charts.RoleColumns: {{Column: "name"}, {Column: "city"}}, charts.RoleMeasure: count}}
		res := &chartResult{Columns: []chartOutput{{Alias: "name", Role: "table column name"},
			{Alias: "city", Role: "table column city"}, {Alias: "count", Role: chartRoleMeasure}}}
		res.Rows = append(res.Rows, []interface{}{strings.Repeat("é", 100), nil, int64(1500)})
		for i := 0; i < 11; i++ {
			res.Rows = append(res.Rows, []interface{}{"x", "Pune", int64(i)})
		}
		got := workflowChartText(s, res)
		if !reflect.DeepEqual(got.Head, []string{"name", "city", "Count"}) ||
			!reflect.DeepEqual(got.Numeric, []bool{false, false, true}) {
			t.Fatalf("head %v numeric %v", got.Head, got.Numeric)
		}
		if len(got.Rows) != workflowChartRowsShown || got.More != 2 {
			t.Fatalf("rows %d more %d", len(got.Rows), got.More)
		}
		if n := utf8.RuneCountInString(got.Rows[0][0]); n != workflowChartCellMax {
			t.Fatalf("a long cell is %d runes", n)
		}
		if got.Rows[0][1] != "" || got.Rows[0][2] != "1,500" {
			t.Fatalf("row 0 = %q", got.Rows[0])
		}
		if got.Notes != nil {
			t.Fatalf("a sorted table's notes: %q", got.Notes)
		}
		// A table drawn without the order of each column says its rows have none.
		res.Unordered = true
		if got := workflowChartText(s, res); !reflect.DeepEqual(got.Notes, []string{chartUnorderedNote}) ||
			chartUnorderedNote != "Rows may come in a different order each time: the table could not be sorted by all its columns." {
			t.Fatalf("an unordered table's notes: %q", got.Notes)
		}
	})
	t.Run("table: a highest of dates or text reads as the column holds it", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeTable, Roles: map[string][]charts.Field{
			charts.RoleColumns: {{Column: "city"}}, charts.RoleMeasure: {{Aggregate: charts.AggMax, Column: "day"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "city", Role: "table column city"},
			{Alias: "day", Role: chartRoleMeasure}}, Rows: [][]interface{}{{"Pune", "2026-10-01T00:00:00Z"}, {"Agra", "north"}, {"Goa", nil}}}
		got := workflowChartText(s, res)
		want := [][]string{{"Pune", "2026-10-01"}, {"Agra", "north"}, {"Goa", ""}}
		if !reflect.DeepEqual(got.Rows, want) {
			t.Fatalf("rows %q, want %q", got.Rows, want)
		}
	})
	t.Run("plotted: series, truncated", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeLine, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "day", Title: "Day"}},
			charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: sum}}
		res := &chartResult{Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "region", Role: chartRoleSeries},
			{Alias: "amount", Role: chartRoleMeasure}}, Rows: [][]interface{}{{"2026-10-01", "north", "12.25"}},
			Truncated: true}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Day", "region", "Sum of amount"}, Numeric: []bool{false, false, true},
			Rows: [][]string{{"2026-10-01", "north", "12.3"}}, Notes: []string{"First 10,000 rows drawn: the source has more."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("plotted: Other named, or left out where it does not add up", func(t *testing.T) {
		cols := []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "region", Role: chartRoleSeries},
			{Alias: "amount", Role: chartRoleMeasure}, {Alias: charts.OtherAlias, Role: charts.RoleOtherSeries},
			{Alias: charts.SeriesCountAlias, Role: charts.RoleSeriesCount}}
		rows := [][]interface{}{{"2026-10-01", "north", "12", int64(0), int64(10)}, {"2026-10-01", nil, "30", int64(1), int64(10)},
			{"2026-10-01", nil, "4", int64(0), int64(10)}}
		for agg, want := range map[string]chartText{
			charts.AggSum: {Rows: [][]string{{"2026-10-01", "north", "12"}, {"2026-10-01", "Other", "30"}, {"2026-10-01", "", "4"}},
				Notes: []string{"3 more series are drawn as Other."}},
			charts.AggAvg: {Rows: [][]string{{"2026-10-01", "north", "12"}, {"2026-10-01", "", "4"}},
				Notes: []string{"3 more series are left out: an average cannot be added into Other."}},
			charts.AggCountDistinct: {Rows: [][]string{{"2026-10-01", "north", "12"}, {"2026-10-01", "", "4"}},
				Notes: []string{"3 more series are left out: a distinct count cannot be added into Other."}},
		} {
			s := &charts.Settings{Type: charts.TypeBar, Roles: map[string][]charts.Field{charts.RoleX: day,
				charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: {{Column: "amount", Aggregate: agg}}}}
			got := workflowChartText(s, &chartResult{Columns: cols, Rows: rows})
			if !reflect.DeepEqual(got.Rows, want.Rows) || !reflect.DeepEqual(got.Notes, want.Notes) {
				t.Errorf("%s: rows %q notes %q, want %q %q", agg, got.Rows, got.Notes, want.Rows, want.Notes)
			}
		}
		// One past the cap is one series, and the verb agrees with it.
		for agg, want := range map[string]string{
			charts.AggSum:           "1 more series is drawn as Other.",
			charts.AggAvg:           "1 more series is left out: an average cannot be added into Other.",
			charts.AggCountDistinct: "1 more series is left out: a distinct count cannot be added into Other.",
		} {
			s := &charts.Settings{Type: charts.TypeBar, Roles: map[string][]charts.Field{charts.RoleX: day,
				charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: {{Column: "amount", Aggregate: agg}}}}
			got := workflowChartText(s, &chartResult{Columns: cols, Rows: [][]interface{}{{"2026-10-01", "north", "12", int64(0), int64(8)}}})
			if !reflect.DeepEqual(got.Notes, []string{want}) {
				t.Errorf("%s, one past the cap: notes %q, want %q", agg, got.Notes, want)
			}
		}
		// Seven series or fewer: nothing folded, no note.
		s := &charts.Settings{Type: charts.TypeBar, Roles: map[string][]charts.Field{charts.RoleX: day,
			charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: sum}}
		got := workflowChartText(s, &chartResult{Columns: cols, Rows: [][]interface{}{{"2026-10-01", "north", "12", int64(0), int64(7)}}})
		if got.Notes != nil || !reflect.DeepEqual(got.Rows, [][]string{{"2026-10-01", "north", "12"}}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("heatmap reads its y", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeHeatmap, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "hour"}},
			charts.RoleY: {{Column: "dow"}}, charts.RoleMeasure: count}}
		res := &chartResult{Columns: []chartOutput{{Alias: "hour", Role: chartRoleX}, {Alias: "dow", Role: chartRoleY},
			{Alias: "count", Role: chartRoleMeasure}}, Rows: [][]interface{}{{int64(9), "Mon", int64(3)}}}
		got := workflowChartText(s, res)
		if !reflect.DeepEqual(got.Rows, [][]string{{"9", "Mon", "3"}}) {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("pie: its slices, no x", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypePie, Roles: map[string][]charts.Field{charts.RoleSeries: {{Column: "region"}},
			charts.RoleMeasure: sum}}
		res := &chartResult{Columns: []chartOutput{{Alias: "region", Role: chartRoleSeries}, {Alias: "amount", Role: chartRoleMeasure},
			{Alias: charts.OtherAlias, Role: charts.RoleOtherSeries}, {Alias: charts.SeriesCountAlias, Role: charts.RoleSeriesCount}},
			Rows: [][]interface{}{{"north", "12", int64(0), int64(9)}, {nil, "30", int64(1), int64(9)}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"region", "Sum of amount"}, Numeric: []bool{false, true},
			Rows: [][]string{{"north", "12"}, {"Other", "30"}}, Notes: []string{"2 more slices are drawn as Other."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("combo: both measures, each in its format", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeBar, Roles: map[string][]charts.Field{charts.RoleX: day,
			charts.RoleMeasure: {{Aggregate: charts.AggSum, Column: "amount", Mark: charts.MarkBar, Axis: charts.AxisLeft},
				{Aggregate: charts.AggCount, Title: "Orders", Mark: charts.MarkLine, Axis: charts.AxisRight}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "amount", Role: chartRoleMeasure},
			{Alias: "count", Role: charts.RoleSecondMeasure}}, Rows: [][]interface{}{{"2026-10-01", "12.25", int64(1500)}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"day", "Sum of amount", "Orders"}, Numeric: []bool{false, true, true},
			Rows: [][]string{{"2026-10-01", "12.3", "1,500"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Masked = []string{"count"}
		if got := workflowChartText(s, res); got.Message != chartMasked {
			t.Fatalf("a masked second measure: %+v", got)
		}
		res.Columns = res.Columns[:2]
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no second measure column: %+v", got)
		}
	})
	t.Run("scatter: a point a row with its row count, a colour past the top seven named Other", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeBubble, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "price", Title: "Price"}},
			charts.RoleY: {{Column: "units"}}, charts.RoleSize: {{Column: "margin"}}, charts.RoleSeries: {{Column: "region"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "price", Role: chartRoleX}, {Alias: "units", Role: chartRoleY},
			{Alias: "margin", Role: charts.RolePointSize}, {Alias: "region", Role: chartRoleSeries},
			{Alias: charts.OtherAlias, Role: charts.RoleOtherSeries}, {Alias: charts.SeriesCountAlias, Role: charts.RoleSeriesCount},
			{Alias: charts.PointRowsAlias, Role: charts.RolePointRows}},
			Rows: [][]interface{}{{"1.5", int64(1200), "0.25", "north", int64(0), int64(9), int64(20000)}, {"2", int64(3), "1", nil, int64(1), int64(9), int64(1)}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Price", "units", "margin", "region", "Rows"}, Numeric: []bool{true, true, true, false, true},
			Rows: [][]string{{"1.5", "1,200", "0.25", "north", "20,000"}, {"2", "3", "1", "Other", "1"}}, Notes: []string{"2 more colours are drawn as Other."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Columns = res.Columns[:6]
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no row count column: %+v", got)
		}
		res.Columns = res.Columns[:2]
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no size column: %+v", got)
		}
	})
	t.Run("pivot: its cells and totals as rows, a totalled field reads Total", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypePivot, Roles: map[string][]charts.Field{charts.RoleRows: {{Column: "region", Title: "Region"}},
			charts.RoleColumns: {{Column: "plan"}},
			charts.RoleMeasure: {{Aggregate: charts.AggSum, Column: "amount"}, {Aggregate: charts.AggAvg, Column: "amount"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "region", Role: charts.PivotRowRole(1)}, {Alias: "plan", Role: charts.PivotColumnRole(1)},
			{Alias: "amount", Role: chartRoleMeasure}, {Alias: "amount_2", Role: charts.RoleSecondMeasure},
			{Alias: "rsync_total_r1", Role: charts.PivotRowTotalRole(1)}, {Alias: "rsync_total_c1", Role: charts.PivotColumnTotalRole(1)}},
			Rows: [][]interface{}{{nil, nil, "60", "20", int64(1), int64(1)}, {"north", nil, "40", "20", int64(0), int64(1)},
				{"north", nil, "5", "5", int64(0), int64(0)}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Region", "plan", "Sum of amount", "Average amount"}, Numeric: []bool{false, false, true, true},
			Rows: [][]string{{"Total", "Total", "60", "20"}, {"north", "Total", "40", "20"}, {"north", "(empty)", "5", "5"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Columns = res.Columns[:5]
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no column total flag: %+v", got)
		}
	})
	t.Run("histogram: each bin least to greatest, counted", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeHistogram, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "amount", Bins: 10}}}}
		cols := []chartOutput{{Alias: "amount", Role: chartRoleX}, {Alias: charts.BinWidthAlias, Role: charts.RoleBinWidth},
			{Alias: "count", Role: chartRoleMeasure}}
		got := workflowChartText(s, &chartResult{Columns: cols, Rows: [][]interface{}{{"0.1", "0.2", int64(1200)}, {"0.3", "0.2", int64(4)}}})
		want := chartText{Head: []string{"amount", "Count"}, Numeric: []bool{false, true},
			Rows: [][]string{{"0.1–0.3", "1,200"}, {"0.3–0.5", "4"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		// Every row holds one number: one bin of width 0, named by its value.
		got = workflowChartText(s, &chartResult{Columns: cols, Rows: [][]interface{}{{"7", "0", int64(3)}}})
		if !reflect.DeepEqual(got.Rows, [][]string{{"7", "3"}}) {
			t.Fatalf("one value: %+v", got)
		}
	})
	t.Run("treemap: a part a row, its levels then its measure, cut at its largest parts", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeTreemap, Roles: map[string][]charts.Field{
			charts.RoleRows: {{Column: "region", Title: "Region"}, {Column: "city"}}, charts.RoleMeasure: sum}}
		res := &chartResult{Columns: []chartOutput{{Alias: "region", Role: charts.LevelRole(1)}, {Alias: "city", Role: charts.LevelRole(2)},
			{Alias: "amount", Role: chartRoleMeasure}}, Rows: [][]interface{}{{"north", "Pune", "1500"}}, Truncated: true}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Region", "city", "Sum of amount"}, Numeric: []bool{false, false, true},
			Rows: [][]string{{"north", "Pune", "1,500"}}, Notes: []string{"The largest 3,500 parts drawn: the source has more."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Masked = []string{"amount"}
		if got := workflowChartText(s, res); got.Message != chartMasked {
			t.Fatalf("a masked measure: %+v", got)
		}
		res.Columns = []chartOutput{res.Columns[0], res.Columns[2]}
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no second level column: %+v", got)
		}
	})
	t.Run("sankey: a flow from its x to its y", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeSankey, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "from", Title: "From"}},
			charts.RoleY: {{Column: "to"}}, charts.RoleMeasure: sum}}
		res := &chartResult{Columns: []chartOutput{{Alias: "from", Role: chartRoleX}, {Alias: "to", Role: chartRoleY},
			{Alias: "amount", Role: chartRoleMeasure}}, Rows: [][]interface{}{{"site", "cart", "12"}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"From", "to", "Sum of amount"}, Numeric: []bool{false, false, true},
			Rows: [][]string{{"site", "cart", "12"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Columns = []chartOutput{res.Columns[0], res.Columns[2]}
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no y column: %+v", got)
		}
	})
	t.Run("calendar: a day a row, cut at its latest days", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeCalendar, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "ordered_at", Title: "Ordered"}},
			charts.RoleMeasure: count}}
		res := &chartResult{Columns: []chartOutput{{Alias: "ordered_at", Role: chartRoleX}, {Alias: "count", Role: chartRoleMeasure}},
			Rows: [][]interface{}{{"2026-10-01T00:00:00Z", int64(1200)}}, Truncated: true}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Day", "Count"}, Numeric: []bool{false, true},
			Rows: [][]string{{"2026-10-01", "1,200"}}, Notes: []string{"The latest 366 days drawn: the source has more."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("bullet: its number against a measure or a typed target, in the number's format", func(t *testing.T) {
		score := charts.Field{Aggregate: charts.AggAvg, Column: "score"}
		s := &charts.Settings{Type: charts.TypeBullet, Roles: map[string][]charts.Field{
			charts.RoleMeasure: {score, {Aggregate: charts.AggCount, Title: "Goal"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "score", Role: chartRoleMeasure}, {Alias: "count", Role: charts.RoleSecondMeasure}},
			Rows: [][]interface{}{{"1234.5", "12.25"}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"Average score", "Goal"}, Numeric: []bool{true, true}, Rows: [][]string{{"1,234.5", "12.3"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("a target measure: got %+v, want %+v", got, want)
		}
		res.Masked = []string{"count"}
		if got := workflowChartText(s, res); got.Message != chartMasked {
			t.Fatalf("a masked target: %+v", got)
		}

		target := 2000.0
		s = &charts.Settings{Type: charts.TypeBullet, Roles: map[string][]charts.Field{charts.RoleMeasure: {score}}, Target: &target}
		res = &chartResult{Columns: []chartOutput{{Alias: "score", Role: chartRoleMeasure}}, Rows: [][]interface{}{{"1234.5"}}}
		got = workflowChartText(s, res)
		want = chartText{Head: []string{"Average score", "Target"}, Numeric: []bool{true, true}, Rows: [][]string{{"1,234.5", "2,000"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("a typed target: got %+v, want %+v", got, want)
		}
		for _, rows := range [][][]interface{}{nil, {{nil}}} {
			res.Rows = rows
			if got := workflowChartText(s, res); got.Message != "The result has no number to show." || got.Rows != nil {
				t.Fatalf("no number in %v: %+v", rows, got)
			}
		}
	})
	t.Run("box plot: its group's five numbers least first, approximate said", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeBoxplot, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "region"}},
			charts.RoleY: {{Column: "amount", Format: "integer"}}}}
		cols := []chartOutput{{Alias: "region", Role: chartRoleX}}
		for i, role := range charts.BoxRoles {
			cols = append(cols, chartOutput{Alias: charts.BoxAliases[i], Role: role})
		}
		res := &chartResult{Columns: cols, Rows: [][]interface{}{{"north", "1", "2.5", "3", "4", "1200"}}, Approximate: true}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"region", "Minimum", "Lower quartile", "Median", "Upper quartile", "Maximum"},
			Numeric: []bool{false, true, true, true, true, true}, Rows: [][]string{{"north", "1", "3", "3", "4", "1,200"}},
			Notes: []string{"The quartiles are approximate: this engine estimates them."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Approximate = false
		if got := workflowChartText(s, res); got.Notes != nil {
			t.Fatalf("an exact engine's quartiles: %+v", got.Notes)
		}
		res.Masked = []string{"rsync_q1"}
		if got := workflowChartText(s, res); got.Message != chartColumnMasked {
			t.Fatalf("a masked y: %+v", got)
		}
		res.Columns = cols[:3]
		if got := workflowChartText(s, res); got.Message != "The result is missing a column the chart needs." {
			t.Fatalf("no median column: %+v", got)
		}
	})
	t.Run("candlestick: its period's open, high, low and close", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeCandlestick, Roles: map[string][]charts.Field{charts.RoleX: day,
			charts.RoleY: {{Column: "o"}, {Column: "h"}, {Column: "l"}, {Column: "c"}}}}
		cols := []chartOutput{{Alias: "day", Role: chartRoleX}}
		for i, role := range charts.PriceRoles {
			cols = append(cols, chartOutput{Alias: charts.PriceAliases[i], Role: role})
		}
		res := &chartResult{Columns: cols, Rows: [][]interface{}{{"2026-10-01", "1.25", "2", "1", "1500"}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"day", "Open", "High", "Low", "Close"}, Numeric: []bool{false, true, true, true, true},
			Rows: [][]string{{"2026-10-01", "1.3", "2", "1", "1,500"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("range bands: the low and the high, each in its format", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeRange, Roles: map[string][]charts.Field{charts.RoleX: day,
			charts.RoleMeasure: {{Aggregate: charts.AggMin, Column: "amount"}, {Aggregate: charts.AggMax, Column: "amount", Format: "integer"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "amount", Role: chartRoleMeasure},
			{Alias: "amount_2", Role: charts.RoleSecondMeasure}}, Rows: [][]interface{}{{"2026-10-01", "1.25", "9.5"}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"day", "Lowest amount", "Highest amount"}, Numeric: []bool{false, true, true},
			Rows: [][]string{{"2026-10-01", "1.3", "10"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("map: a row a code, as the column holds it, and its measure", func(t *testing.T) {
		s := &charts.Settings{Type: charts.TypeMap, Map: charts.MapWorld, Roles: map[string][]charts.Field{charts.RoleX: {{Column: "country"}},
			charts.RoleMeasure: {{Aggregate: charts.AggSum, Column: "amount"}}}}
		res := &chartResult{Columns: []chartOutput{{Alias: "country", Role: chartRoleX}, {Alias: "amount", Role: chartRoleMeasure}},
			Rows: [][]interface{}{{"FR", "1200.5"}, {"usa", "90"}}}
		got := workflowChartText(s, res)
		want := chartText{Head: []string{"country", "Sum of amount"}, Numeric: []bool{false, true},
			Rows: [][]string{{"FR", "1,200.5"}, {"usa", "90"}}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		res.Masked = []string{"amount"}
		if got := workflowChartText(s, res); got.Message != chartMasked {
			t.Fatalf("a masked measure: %+v", got)
		}
	})
	for name, res := range map[string]*chartResult{
		"plotted: a column is missing": {Columns: []chartOutput{{Alias: "day", Role: chartRoleX}}},
		"plotted: no rows":             {Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "count", Role: chartRoleMeasure}}},
		"plotted: masked": {Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "count", Role: chartRoleMeasure}},
			Rows: [][]interface{}{{"a", int64(1)}}, Masked: []string{"count"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := &charts.Settings{Type: charts.TypeBar, Roles: map[string][]charts.Field{charts.RoleX: day, charts.RoleMeasure: count}}
			got := workflowChartText(s, res)
			want := map[string]string{
				"plotted: a column is missing": "The result is missing a column the chart needs.",
				"plotted: no rows":             "No rows to draw.",
				"plotted: masked":              chartMasked,
			}[name]
			if got.Message != want || got.Rows != nil {
				t.Fatalf("got %+v, want message %q", got, want)
			}
		})
	}
}

// TestChartNotes_MatchTheBrowser: a workflow's message says what a chart cut at its type's
// row cap or its series cap left out as the chart page does. shared/chart_notes_golden.json
// is read by saved-chart.test.ts too, so a change to either side's wording fails one of the
// two, and every chart type in the registry has a truncated case.
func TestChartNotes_MatchTheBrowser(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/chart_notes_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Truncated []struct {
			Type string `json:"type"`
			Note string `json:"note"`
		} `json:"truncated"`
		Other []struct {
			Type      string `json:"type"`
			Aggregate string `json:"aggregate"`
			Rest      int    `json:"rest"`
			Note      string `json:"note"`
		} `json:"other"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Truncated) == 0 || len(golden.Other) == 0 {
		t.Fatal("no cases in shared/chart_notes_golden.json")
	}
	covered := map[string]bool{}
	for _, c := range golden.Truncated {
		covered[c.Type] = true
		if got := chartTruncatedNote(c.Type); got != c.Note {
			t.Errorf("chartTruncatedNote(%q) = %q, want %q", c.Type, got, c.Note)
		}
	}
	for _, spec := range charts.Registry {
		if !covered[spec.Type] {
			t.Errorf("%s: no truncated case in shared/chart_notes_golden.json", spec.Type)
		}
	}
	for _, c := range golden.Other {
		// The series count is on every row, as the gateway's SQL returns it.
		count := json.Number(strconv.Itoa(charts.Spec(c.Type).Cap.Series + c.Rest))
		roles := map[string]int{charts.RoleSeriesCount: 0}
		if got := chartOtherNote(&charts.Settings{Type: c.Type}, c.Aggregate, [][]interface{}{{count}}, roles); got != c.Note {
			t.Errorf("chartOtherNote(%s, %s, %d more) = %q, want %q", c.Type, c.Aggregate, c.Rest, got, c.Note)
		}
	}
}

// ---------------------------------------------------------------------------
// In the message
// ---------------------------------------------------------------------------

func plottedChart(name string, rows int) *workflowChart {
	c := &workflowChart{StepID: "chart", ChartID: chartID, Name: name, Type: charts.TypeBar,
		Link: "https://app.example.test/charts/" + chartID, PNG: fakeChartPNG,
		Text: chartText{Head: []string{"day", "Count"}, Numeric: []bool{false, true}}}
	for i := 0; i < rows; i++ {
		c.Text.Rows = append(c.Text.Rows, []string{strings.Repeat("d", 50), "1,234"})
	}
	return c
}

// Email shows each plotted chart's picture in place, once each by name; a
// scorecard is its number; a chart that was not drawn says so.
func TestRenderWorkflowDrawings_Charts(t *testing.T) {
	score := &workflowChart{Name: "Orders <today>", Type: charts.TypeScorecard, Link: "https://app.example.test/charts/x",
		Text: chartText{Label: "Count", Number: "40,005", Change: "+5.0% on the day before (38,100)"}}
	out := renderWorkflowDrawings(chartDrawings([]*workflowChart{plottedChart("Orders by day", 2), plottedChart("Orders by day", 2), score, nil}))

	if len(out.Images) != 2 || out.Images[0].Filename != "orders-by-day.png" || out.Images[1].Filename != "orders-by-day-2.png" ||
		out.Images[0].ContentID != "chart-1" || out.Images[1].ContentID != "chart-2" || !bytes.Equal(out.Images[0].PNG, fakeChartPNG) {
		t.Fatalf("images = %+v", out.Images)
	}
	for _, want := range []string{`<img src="cid:chart-1"`, `<img src="cid:chart-2"`, "Orders &lt;today&gt;", "40,005",
		"+5.0% on the day before (38,100)", chartNotDrawn, `href="https://app.example.test/charts/` + chartID + `"`} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(out.HTML, "<today>") {
		t.Error("HTML carries an unescaped name")
	}
	for _, want := range []string{"The chart is attached as orders-by-day.png.", "The chart is attached as orders-by-day-2.png.",
		"Count: 40,005", chartNotDrawn, "Open the chart: https://app.example.test/charts/x"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	// Slack carries no picture: a plotted chart's rows, as text.
	for _, want := range []string{"*<https://app.example.test/charts/" + chartID + "|Orders by day>*", "```\nday",
		"Count: *40,005*", "Orders &lt;today&gt;"} {
		if !strings.Contains(out.Slack, want) {
			t.Errorf("Slack lacks %q:\n%s", want, out.Slack)
		}
	}
	if strings.Count(out.Slack, "```")%2 != 0 {
		t.Errorf("Slack has an open code block:\n%s", out.Slack)
	}
	if got := renderWorkflowDrawings(nil); !reflect.DeepEqual(got, chartSections{}) {
		t.Fatalf("no charts = %+v", got)
	}
}

// A Slack message stays within Slack's limit however many charts a send has, and
// each chart keeps its link whole; with no room for it (40 charts), the chart's
// name alone, never a cut link.
func TestWorkflowMessageWithCharts_SlackStaysWithinItsLimit(t *testing.T) {
	links := map[int]int{1: 1, 3: 3, 12: 12, 40: 0}
	for _, n := range []int{1, 3, 12, 40} {
		var cs []*workflowChart
		for i := 0; i < n; i++ {
			cs = append(cs, plottedChart(strings.Repeat("Orders & refunds by day ", 5), 10))
		}
		m := workflow.Message{Subject: "Orders", Body: strings.Repeat("A long body. ", 400)}
		msg, _ := renderWorkflowMessageWithDrawings("Orders", m, workflow.Result{}, workflow.Outcome{},
			"https://app.example.test/workflows/"+wfID, chartDrawings(cs), time.Now())
		if got := utf8.RuneCountInString(msg.Slack); got > workflow.SlackMessageMax {
			t.Errorf("%d charts: Slack message is %d runes", n, got)
		}
		whole := strings.Count(msg.Slack, "*<https://app.example.test/charts/"+chartID+"|")
		if whole != links[n] || strings.Count(msg.Slack, "<https://app.example.test/charts/") != whole ||
			strings.Count(msg.Slack, "|Orders &amp; refunds") != whole {
			t.Errorf("%d charts: %d whole links, want %d:\n%s", n, whole, links[n], msg.Slack)
		}
		if got := strings.Count(msg.Slack, "*Orders &amp; refunds"); got != n-whole {
			t.Errorf("%d charts: %d bare names, want %d", n, got, n-whole)
		}
		if strings.Contains(msg.Slack, "&a…") || strings.Contains(msg.Slack, "&am…") || strings.Contains(msg.Slack, "&amp…") {
			t.Errorf("%d charts: an escape was cut:\n%s", n, msg.Slack)
		}
		if !strings.HasSuffix(msg.Slack, "\nhttps://app.example.test/workflows/"+wfID) {
			t.Errorf("%d charts: the workflow's link is not last", n)
		}
		if len(msg.Images) != n {
			t.Errorf("%d charts: %d images", n, len(msg.Images))
		}
	}
	// One chart with room shows its rows, and counts none as left out.
	c := plottedChart("Orders by day", 3)
	out := chartSlack(c, workflowChartsSlackMax)
	if strings.Count(out, "1,234") != 3 || strings.Contains(out, "not shown") {
		t.Fatalf("one chart:\n%s", out)
	}
	// A tight budget shows fewer rows and counts the rest.
	out = chartSlack(plottedChart("Orders by day", 10), 400)
	if utf8.RuneCountInString(out) > 400 || !strings.Contains(out, "more row(s) not shown") ||
		strings.Count(out, "```")%2 != 0 {
		t.Fatalf("tight budget (%d runes):\n%s", utf8.RuneCountInString(out), out)
	}
}

// ---------------------------------------------------------------------------
// The picture: the frontend draws it
// ---------------------------------------------------------------------------

func TestFrontendChartPNG(t *testing.T) {
	res := &chartResult{Columns: []chartOutput{{Alias: "day", Role: chartRoleX}, {Alias: "count", Role: chartRoleMeasure}},
		Rows: [][]interface{}{{"2026-10-01", int64(5)}}}
	serve := func(t *testing.T, h http.HandlerFunc) {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		t.Setenv("FRONTEND_INTERNAL_URL", srv.URL+"/")
		t.Setenv("INTERNAL_SERVICE_SECRET", "s3cret")
	}

	t.Run("draws with the secret", func(t *testing.T) {
		var got map[string]json.RawMessage
		serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/internal/charts/png" ||
				r.Header.Get("X-Internal-Secret") != "s3cret" || r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &got)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fakeChartPNG)
		})
		png, err := frontendChartPNG(context.Background(), "Orders by day", json.RawMessage(chartStepBar), res)
		if err != nil || !bytes.Equal(png, fakeChartPNG) {
			t.Fatalf("png %q, err %v", png, err)
		}
		if string(got["name"]) != `"Orders by day"` || string(got["settings"]) != chartStepBar {
			t.Fatalf("sent %s / %s", got["name"], got["settings"])
		}
		var r map[string]json.RawMessage
		_ = json.Unmarshal(got["result"], &r)
		if string(r["masked"]) != "[]" || string(r["unfiltered"]) != "[]" ||
			string(r["rows"]) != `[["2026-10-01",5]]` || string(r["truncated"]) != "false" {
			t.Fatalf("result sent = %s", got["result"])
		}
		if _, ok := r["sql"]; ok {
			t.Fatal("the SQL went to the frontend")
		}
	})
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"its refusal", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":"a scorecard is not drawn as a picture"}`))
		}, "the frontend answered 422: a scorecard is not drawn as a picture"},
		{"a bare error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, "the frontend answered 502"},
		{"not an image", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>"))
		}, `the frontend answered "text/html", not a PNG`},
		{"says PNG, is not", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("GIF89a"))
		}, "the frontend's answer is not a PNG"},
		{"too large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(append(append([]byte{}, pngSignature...), make([]byte, chartPNGMax)...))
		}, "the picture is over 4 MB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serve(t, tc.h)
			png, err := frontendChartPNG(context.Background(), "x", json.RawMessage(chartStepBar), res)
			if err == nil || err.Error() != tc.want || png != nil {
				t.Fatalf("png %d bytes, err %v; want %q", len(png), err, tc.want)
			}
		})
	}
	t.Run("no secret, no call", func(t *testing.T) {
		called := false
		serve(t, func(http.ResponseWriter, *http.Request) { called = true })
		t.Setenv("INTERNAL_SERVICE_SECRET", " ")
		if _, err := frontendChartPNG(context.Background(), "x", json.RawMessage(chartStepBar), res); err == nil || called {
			t.Fatalf("err %v, called %v", err, called)
		}
	})
}

// A run draws a chart step's chart as the person the workflow runs as, so a
// save checks the chart is one the person saving sees. An id that is not a
// chart's is refused before anything is read; a chart deleted, or private to
// someone else, is refused on its step, telling the model where ids come from
// and a person what to do.
func TestValidateWorkflowDefinition_ChartStepsChart(t *testing.T) {
	notSeen := func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chartID).
			WillReturnRows(sqlmock.NewRows([]string{"id"}))
	}
	for _, tc := range []struct {
		name     string
		chart    string
		forModel bool
		expect   func(sqlmock.Sqlmock)
		code     string
		status   int
		want     []string
	}{
		{name: "not a chart id", chart: "orders by day", forModel: true, expect: func(sqlmock.Sqlmock) {},
			code: "invalid_chart", status: http.StatusBadRequest,
			want: []string{`Step chart: "orders by day" is not a chart id.`, "get_dashboard gives each chart's id."}},
		{name: "a chart the saver does not see, for the model", chart: chartID, forModel: true, expect: notSeen,
			code: "chart_not_found", status: http.StatusNotFound,
			want: []string{"no chart " + chartID + " you can see", "get_dashboard gives each chart's id."}},
		{name: "a chart the saver does not see, for a person", chart: chartID, expect: notSeen,
			code: "chart_not_found", status: http.StatusNotFound,
			want: []string{"Step chart: no chart " + chartID, "Pick another chart."}},
		{name: "a chart the saver sees", chart: chartID,
			expect: func(mock sqlmock.Sqlmock) { expectStepChart(mock, chartStepBar) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			tc.expect(mock)
			def := chartStepRun().Definition
			def.Steps[0].ChartID = tc.chart

			_, _, refusal, err := validateWorkflowDefinition(context.Background(), database, wsScopeWS, wsScopeUser,
				&def, security.WSAdmin, tc.forModel)
			if err != nil {
				t.Fatal(err)
			}
			mockMet(t, mock)
			if tc.code == "" {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal)
				}
				return
			}
			if refusal == nil || refusal.Code != tc.code || refusal.Status != tc.status || refusal.Step != "chart" {
				t.Fatalf("refusal = %+v, want %s %d on step chart", refusal, tc.code, tc.status)
			}
			for _, s := range tc.want {
				if !strings.Contains(refusal.Message, s) {
					t.Errorf("message %q lacks %q", refusal.Message, s)
				}
			}
		})
	}
}

// A chart that cannot be read is an error, not a refusal: the person did
// nothing wrong.
func TestValidateWorkflowDefinition_ChartLookupFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(chartVisibleWhere).WillReturnError(errors.New("connection reset"))
	def := chartStepRun().Definition
	_, _, refusal, err := validateWorkflowDefinition(context.Background(), database, wsScopeWS, wsScopeUser,
		&def, security.WSAdmin, false)
	if err == nil || refusal != nil {
		t.Fatalf("err = %v, refusal = %+v; want the lookup's error", err, refusal)
	}
	mockMet(t, mock)
}

// An edit in rsync.ai changes what the steps hold, not which chart a chart
// step draws: the chart was checked as the person who saved it.
func TestWorkflowEditShapeRefusal_KeepsTheChart(t *testing.T) {
	have := chartStepRun().Definition
	if err := workflow.Normalize(&have); err != nil {
		t.Fatal(err)
	}
	edit := func(change func(*workflow.Definition)) workflow.Definition {
		got := have
		got.Steps = make([]workflow.Step, len(have.Steps))
		copy(got.Steps, have.Steps)
		msg := *have.Steps[1].Message
		got.Steps[1].Message = &msg
		change(&got)
		return got
	}
	reworded := edit(func(d *workflow.Definition) { d.Steps[1].Message.Body = "Orders this week" })
	if r := workflowEditShapeRefusal(have, reworded); r != nil {
		t.Fatalf("a new message was refused: %+v", r)
	}
	swapped := edit(func(d *workflow.Definition) { d.Steps[0].ChartID = "55555555-5555-4555-8555-555555555555" })
	r := workflowEditShapeRefusal(have, swapped)
	if r == nil || r.Code != "invalid_steps" || r.Step != "chart" {
		t.Fatalf("refusal = %+v, want invalid_steps on step chart", r)
	}
}

// The workflow page names the chart a chart step draws, as it names a
// step's pipeline or model, from this workspace only.
func TestAttachStepRefs_NamesTheChart(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT 'chart', id::text, name FROM charts WHERE workspace_id = \$1 AND id = ANY\(\$4::uuid\[\]\)`).
		WithArgs(wsScopeWS, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "id", "name"}).AddRow("chart", chartID, "Orders by day"))
	w := &Workflow{ID: wfID, Definition: chartStepRun().Definition}
	if err := attachStepRefs(context.Background(), database, wsScopeWS, []*Workflow{w}); err != nil {
		t.Fatal(err)
	}
	mockMet(t, mock)
	if want := map[string]WorkflowRef{"chart": {ID: chartID, Name: "Orders by day"}}; !reflect.DeepEqual(w.StepRefs, want) {
		t.Fatalf("step refs = %+v, want %+v", w.StepRefs, want)
	}
}

// expectChartWorkflowLoad answers loadWorkflow for chartStepRun's workflow:
// the row, its send's channel, its chart's name, its last run.
func expectChartWorkflowLoad(mock sqlmock.Sqlmock, status string) {
	def, _ := json.Marshal(chartStepRun().Definition)
	mock.ExpectQuery(`FROM workflows w`).WithArgs(wfID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status", "paused_reason", "definition", "next_run_at",
			"proposed_by", "created_at", "updated_at", "connection_id", "cname", "ctype", "pipeline_id", "pname"}).
			AddRow(wfID, "Orders", status, "", string(def),
				nil, "assistant", wfUpdatedAt, wfUpdatedAt, nil, "", "", nil, ""))
	mock.ExpectQuery(`FROM workflow_channels wc JOIN delivery_channels dc`).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "id", "name", "kind", "node_id"}).
			AddRow(wfID, wfChannel, "#ops", "slack", "send_1"))
	mock.ExpectQuery(`SELECT 'chart', id::text, name FROM charts`).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "id", "name"}).AddRow("chart", chartID, "Orders by day"))
	mock.ExpectQuery(`SELECT DISTINCT ON \(r.workflow_id\)`).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "id", "trigger", "status", "condition_met", "row_count",
			"deliveries", "notes", "error", "started_at", "finished_at", "branch", "stop_requested"}))
}

// The workflow runs as the person who turns it on, so its chart must be one
// they see: a chart private to someone else, or deleted since the save,
// keeps it off, and nothing is updated.
func TestActivateWorkflow_ChecksTheChartAsTheActivator(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectChartWorkflowLoad(mock, "draft")
	mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chartID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	w := activateWorkflow("admin")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, body = %s; want 404", w.Code, w.Body.String())
	}
	if msg := wfErrorOf(t, w); !strings.Contains(msg, "no chart "+chartID+" you can see") ||
		!strings.Contains(msg, "Edit the workflow to pick another chart, then turn it on.") {
		t.Fatalf("error = %q", msg)
	}
	mockMet(t, mock)
}

// A chart the activator sees turns the workflow on, as them.
func TestActivateWorkflow_TurnsOnWithAChartTheActivatorSees(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectChartWorkflowLoad(mock, "draft")
	expectStepChart(mock, chartStepBar)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE workflows SET status = 'active'`).
		WithArgs(wfID, wsScopeWS, sqlmock.AnyArg(), wsScopeUser).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	expectChartWorkflowLoad(mock, "active")

	w := activateWorkflow("admin")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	mockMet(t, mock)
}

// A drafted chart step goes before the one send of a workflow with no query:
// the send runs after it, so its message carries the chart.
func TestDraftDefinition_ChartBeforeTheOneSend(t *testing.T) {
	in := WorkflowDraftInput{Trigger: workflow.TriggerManual, Steps: []WorkflowStepInput{
		{ID: "refresh", Type: "refresh_model", ModelID: "eeeeeeee-0000-4000-8000-000000000001"},
		{ID: "orders", Type: " Chart ", After: []string{"refresh"}, ChartID: chartID},
	}}
	branches := []workflow.Branch{{Condition: workflow.Condition{Type: workflow.ConditionAlways},
		Message: workflow.Message{Subject: "Orders", Body: "This week's orders"}}}
	def, r := draftDefinition(in, branches)
	if r != nil {
		t.Fatalf("refusal = %+v", r)
	}
	if err := workflow.Normalize(&def); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	send := def.Step("send_1")
	if send == nil || !reflect.DeepEqual(send.After, []string{"orders"}) {
		t.Fatalf("send_1 = %+v, want it after the chart", send)
	}
	charts := def.DrawingsBefore(send)
	if len(charts) != 1 || charts[0].ID != "orders" || charts[0].ChartID != chartID {
		t.Fatalf("charts before send_1 = %+v, want orders drawing %s", charts, chartID)
	}
	if got := draftStepTypes(def); !reflect.DeepEqual(got, []string{"refresh_model", "chart"}) {
		t.Fatalf("draftStepTypes = %v, want the model refresh and the chart", got)
	}
}

// With a query each condition sends its own message, after the condition
// alone, so a drafted chart step is refused by name.
func TestDraftDefinition_RefusesAChartWithAQuery(t *testing.T) {
	in := WorkflowDraftInput{Trigger: workflow.TriggerManual, ConnectionID: "11111111-1111-4111-8111-111111111111",
		SQL: "SELECT 1", Steps: []WorkflowStepInput{{ID: "orders", Type: "chart", ChartID: chartID}}}
	branches := []workflow.Branch{{Condition: workflow.Condition{Type: workflow.ConditionAlways},
		Message: workflow.Message{Subject: "Orders", Body: "Orders"}}}
	_, r := draftDefinition(in, branches)
	if r == nil || r.Code != "invalid_steps" || r.Status != http.StatusBadRequest || r.Step != "orders" ||
		!strings.Contains(r.Message, "a chart goes into the one message of a workflow with no query") {
		t.Fatalf("refusal = %+v, want invalid_steps naming step orders", r)
	}
}

func TestDraftDefinition_StepTypesNameTheChart(t *testing.T) {
	in := WorkflowDraftInput{Trigger: workflow.TriggerManual, Steps: []WorkflowStepInput{{ID: "q", Type: "query"}}}
	_, r := draftDefinition(in, []workflow.Branch{{Condition: workflow.Condition{Type: workflow.ConditionAlways}}})
	if r == nil || r.Code != "invalid_type" || r.Step != "q" ||
		!strings.Contains(r.Message, "run_pipeline, refresh_model, wait, chart and dashboard steps") {
		t.Fatalf("refusal = %+v, want invalid_type listing chart", r)
	}
}

// A test draws no chart, so it says the messages it shows lack what a run puts in
// them; a workflow without a chart step gets no such note.
func TestWorkflowTestNotes_SayATestDrawsNoChart(t *testing.T) {
	const noChart = "A test draws no chart: a run puts each chart step's chart in the messages after it, and the messages here leave it out."
	def := chartStepRun().Definition
	if got := workflowTestNotes(def); len(got) != 1 || got[0] != noChart {
		t.Fatalf("notes = %q, want only the no-chart note", got)
	}

	def.Steps = append([]workflow.Step{{ID: "wait", Type: workflow.StepWait, Minutes: 5}}, def.Steps...)
	got := workflowTestNotes(def)
	if len(got) != 2 || !strings.HasPrefix(got[0], "A test runs no step") || got[1] != noChart {
		t.Fatalf("notes = %q, want the no-step note then the no-chart note", got)
	}

	plain := workflow.Definition{Version: 3, Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{{ID: "send_1", Type: workflow.StepSend, Message: &workflow.Message{Body: "Hi"}}}}
	if got := workflowTestNotes(plain); len(got) != 0 {
		t.Fatalf("notes = %q, want none for a workflow without a chart or a process step", got)
	}
}

// TestChartBinLabel_MatchesTheBrowser: a workflow's message names a histogram's bins as
// the chart page does. shared/chart_bin_labels_golden.json is read by saved-chart.test.ts
// too, so a change to how either side writes an edge fails one of the two.
func TestChartBinLabel_MatchesTheBrowser(t *testing.T) {
	raw, err := os.ReadFile("../../../shared/chart_bin_labels_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Cases []struct {
			// As chartCell hands a cell over: decoded with UseNumber.
			Lo    json.Number `json:"lo"`
			Width json.Number `json:"width"`
			Label string      `json:"label"`
		} `json:"cases"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("no cases in shared/chart_bin_labels_golden.json")
	}
	for _, c := range golden.Cases {
		if got := chartBinLabel(c.Lo, c.Width); got != c.Label {
			t.Errorf("chartBinLabel(%s, %s) = %q, want %q", c.Lo, c.Width, got, c.Label)
		}
	}
}
