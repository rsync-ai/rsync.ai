package handlers

// A workflow's dashboard step (workflow_dashboard_step.go): what it draws and holds,
// as whom, what a tile that cannot be drawn says, and how the sends after it show
// the board.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"api-gateway/internal/charts"
	"api-gateway/internal/security"

	workflow "github.com/rsync-ai/shared/workflowdef"

	"github.com/DATA-DOG/go-sqlmock"
)

const dashboardID = "55555555-5555-5555-5555-555555555555"

// fakeWorkflowDashboards holds drawings in memory, through JSON as Redis would.
type fakeWorkflowDashboards struct {
	held    map[string]workflowDashboard
	ttls    map[string]time.Duration
	deleted []string
}

func (f *fakeWorkflowDashboards) Put(_ context.Context, key string, d workflowDashboard, ttl time.Duration) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	var back workflowDashboard
	if err := json.Unmarshal(raw, &back); err != nil {
		return err
	}
	f.held[key], f.ttls[key] = back, ttl
	return nil
}

func (f *fakeWorkflowDashboards) Get(_ context.Context, key string) (workflowDashboard, bool, error) {
	d, ok := f.held[key]
	return d, ok, nil
}

func (f *fakeWorkflowDashboards) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.held, key)
	return nil
}

func stubWorkflowDashboards(t *testing.T) *fakeWorkflowDashboards {
	t.Helper()
	f := &fakeWorkflowDashboards{held: map[string]workflowDashboard{}, ttls: map[string]time.Duration{}}
	prev := workflowDashboards
	workflowDashboards = f
	t.Cleanup(func() { workflowDashboards = prev })
	return f
}

// stubDashboardLoad hands the step rec, or err, and records as whom it was asked.
func stubDashboardLoad(t *testing.T, rec *dashboardRecord, err error) *[]chartDrawCall {
	t.Helper()
	var calls []chartDrawCall
	prev := loadWorkflowDashboard
	loadWorkflowDashboard = func(_ context.Context, _ *sql.DB, ws, userID, id string) (*dashboardRecord, error) {
		calls = append(calls, chartDrawCall{ws: ws, actor: userID, chart: id})
		return rec, err
	}
	t.Cleanup(func() { loadWorkflowDashboard = prev })
	return &calls
}

// stubTileDraws draws each chart by its id; one not in draws blocks until the
// step's budget runs out.
func stubTileDraws(t *testing.T, draws map[string]chartDraw) *[]chartDrawCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []chartDrawCall
	)
	prev := drawWorkflowChart
	drawWorkflowChart = func(ctx context.Context, _ *sql.DB, ws, userID string, r *chartRecord, filters []charts.Bound,
		_ time.Time) chartDraw {
		mu.Lock()
		calls = append(calls, chartDrawCall{ws: ws, actor: userID, chart: r.ID})
		mu.Unlock()
		if filters != nil {
			t.Errorf("chart %s drawn with filters %+v", r.ID, filters)
		}
		if d, ok := draws[r.ID]; ok {
			return d
		}
		<-ctx.Done()
		return chartDraw{Error: ctx.Err().Error()}
	}
	t.Cleanup(func() { drawWorkflowChart = prev })
	return &calls
}

// dashboardPDFCall is what the step handed the frontend to lay out.
type dashboardPDFCall struct {
	name  string
	draws map[string]dashboardTileDraw
}

var fakeDashboardPDF = []byte("%PDF-1.3 the board")

func stubDashboardPDF(t *testing.T, pdf []byte, err error) *[]dashboardPDFCall {
	t.Helper()
	var calls []dashboardPDFCall
	prev := drawWorkflowDashboardPDF
	drawWorkflowDashboardPDF = func(_ context.Context, rec *dashboardRecord, draws map[string]dashboardTileDraw,
		_ time.Time) ([]byte, error) {
		calls = append(calls, dashboardPDFCall{name: rec.Name, draws: draws})
		return pdf, err
	}
	t.Cleanup(func() { drawWorkflowDashboardPDF = prev })
	return &calls
}

// dashboardStepRun is a manual run of a dashboard, then a send.
func dashboardStepRun() *workflowRunRow {
	def := workflow.Definition{
		Version: 3,
		Trigger: workflow.Trigger{Type: workflow.TriggerManual},
		Steps: []workflow.Step{
			{ID: "board", Type: workflow.StepDashboard, DashboardID: dashboardID},
			{ID: "send_1", Type: workflow.StepSend, After: []string{"board"},
				Message: &workflow.Message{Subject: "Weekly", Body: "This week's numbers"}},
		},
	}
	return &workflowRunRow{RunID: wfRunUUID, Trigger: workflow.TriggerManual, RunStatus: "running", WorkflowID: wfID,
		WorkspaceID: wsScopeWS, Name: "Weekly", Status: "active", Definition: def, RunAsUserID: wsScopeUser}
}

func boardChart(id, name, settings string) *chartRecord {
	return &chartRecord{ID: id, Name: name, Settings: json.RawMessage(settings), SourceState: chartSourceOK}
}

func boardTile(id string, x, y int, ch *chartRecord) *dashboardTile {
	t := &dashboardTile{ID: id, Tile: charts.Tile{Kind: charts.TileChart, X: x, Y: y, W: 6, H: 4}, Chart: ch}
	if ch != nil {
		t.ChartID = ch.ID
	} else {
		t.ChartID, t.ChartState = "66666666-6666-6666-6666-666666666666", chartSourceHidden
	}
	return t
}

const (
	boardScoreID = "a0000000-0000-4000-8000-000000000001"
	boardBarID   = "a0000000-0000-4000-8000-000000000002"
	boardGoneID  = "a0000000-0000-4000-8000-000000000003"
)

// testBoard is five tiles, saved out of reading order: a scorecard and a bar chart
// side by side, a text tile, a chart the actor cannot see and one whose source is
// gone.
func testBoard() *dashboardRecord {
	return &dashboardRecord{ID: dashboardID, Name: "Weekly <sales>", Tiles: []*dashboardTile{
		boardTile("t-gone", 6, 8, boardChart(boardGoneID, "Refunds", chartStepBar)),
		boardTile("t-bar", 6, 0, boardChart(boardBarID, "Orders by day", chartStepBar)),
		{ID: "t-text", Tile: charts.Tile{Kind: charts.TileText, Body: "Read me\nfirst", X: 0, Y: 4, W: 12, H: 2}},
		boardTile("t-score", 0, 0, boardChart(boardScoreID, "Orders", chartStepScorecard)),
		boardTile("t-hidden", 0, 8, nil),
	}}
}

func boardDraws() map[string]chartDraw {
	return map[string]chartDraw{
		boardScoreID: {Result: &chartResult{Columns: []chartOutput{{Alias: "count", Role: chartRoleMeasure}},
			Rows: [][]interface{}{{int64(40005)}}}},
		boardBarID:  barDraw(),
		boardGoneID: {Break: &chartBreak{Reason: "source_deleted", Message: "This chart's source was deleted."}},
	}
}

// ---------------------------------------------------------------------------
// The step
// ---------------------------------------------------------------------------

// Every tile is drawn as the run's actor with no filters, written in the board's
// reading order and held with the board's PDF; a tile that cannot be drawn says
// why in the page's words and does not fail the step.
func TestRunDashboard_DrawsEveryTileAsTheActor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	held := stubWorkflowDashboards(t)
	loads := stubDashboardLoad(t, testBoard(), nil)
	draws := stubTileDraws(t, boardDraws())
	pngs := stubChartPNG(t, fakeChartPNG, nil)
	pdfs := stubDashboardPDF(t, fakeDashboardPDF, nil)
	t.Setenv("APP_BASE_URL", "https://app.example.test")

	run := dashboardStepRun()
	expectRole(mock, "owner")
	expectStepSettledFor(mock, "board", workflow.StatusSucceeded, "")
	s, w := chartStepCall(t, database, run, "board")
	before := time.Now().UTC()
	s.runDashboard()
	mockMet(t, mock)

	if a := stepAnswerOf(t, w); a.Status != workflow.StatusSucceeded || a.Refused {
		t.Fatalf("answer = %+v", a)
	}
	if want := []chartDrawCall{{ws: wsScopeWS, actor: wsScopeUser, chart: dashboardID}}; !reflect.DeepEqual(*loads, want) {
		t.Fatalf("loads = %+v, want %+v", *loads, want)
	}
	if len(*draws) != 3 {
		t.Fatalf("draws = %+v, want the 3 charts the actor may see", *draws)
	}
	for _, d := range *draws {
		if d.ws != wsScopeWS || d.actor != wsScopeUser {
			t.Fatalf("drawn as %+v", d)
		}
	}
	if want := []chartPNGCall{{name: "Orders by day", settings: chartStepBarV2, rows: 2}}; !reflect.DeepEqual(*pngs, want) {
		t.Fatalf("pictures = %+v, want %+v", *pngs, want)
	}

	key := workflowResultKey(wfRunUUID, "board")
	b, ok := held.held[key]
	if !ok {
		t.Fatalf("nothing held under %s", key)
	}
	if held.ttls[key] != workflowResultTTL(run.Definition) {
		t.Fatalf("ttl = %v", held.ttls[key])
	}
	if b.StepID != "board" || b.DashboardID != dashboardID || b.Name != "Weekly <sales>" ||
		b.Link != "https://app.example.test/dashboards/"+dashboardID || b.ReadAt.Before(before.Add(-time.Second)) ||
		!bytes.Equal(b.PDF, fakeDashboardPDF) || b.PDFError != "" {
		t.Fatalf("held = %+v", b)
	}
	// Reading order: y, then x.
	var got []string
	for _, tile := range b.Tiles {
		switch {
		case tile.Chart != nil:
			got = append(got, "chart:"+tile.Chart.Name)
		case tile.Kind == charts.TileText:
			got = append(got, "text:"+tile.Body)
		default:
			got = append(got, "not drawn:"+tile.Name+": "+tile.NotDrawn)
		}
	}
	want := []string{"chart:Orders", "chart:Orders by day", "text:Read me\nfirst",
		"not drawn:" + dashboardHiddenName + ": " + dashboardHiddenChart,
		"not drawn:Refunds: This chart cannot be drawn: This chart's source was deleted."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tiles =\n%q\nwant\n%q", got, want)
	}
	if b.Tiles[0].Chart.Text.Number != "40,005" || b.Tiles[0].Chart.PNG != nil || !bytes.Equal(b.Tiles[1].Chart.PNG, fakeChartPNG) {
		t.Fatalf("charts = %+v / %+v", b.Tiles[0].Chart, b.Tiles[1].Chart)
	}

	// The PDF is laid out from the same draws: every chart the actor may see.
	if len(*pdfs) != 1 || (*pdfs)[0].name != "Weekly <sales>" {
		t.Fatalf("PDFs = %+v", *pdfs)
	}
	pd := (*pdfs)[0].draws
	if len(pd) != 3 || pd["t-score"].Draw == nil || pd["t-bar"].Draw.Result == nil || pd["t-gone"].Draw.Break == nil {
		t.Fatalf("PDF draws = %+v", pd)
	}
	if _, ok := pd["t-hidden"]; ok {
		t.Fatal("the PDF was handed a draw for a chart the actor cannot see")
	}
}

// The note counts the tiles not drawn, names the first reason, and says why the
// PDF is missing; the step still succeeds and holds the tiles it drew.
func TestRunDashboard_PDFFailureIsANote(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	held := stubWorkflowDashboards(t)
	stubDashboardLoad(t, testBoard(), nil)
	stubTileDraws(t, boardDraws())
	stubChartPNG(t, fakeChartPNG, nil)
	stubDashboardPDF(t, nil, errors.New("the frontend answered 500"))

	expectRole(mock, "admin")
	note := "2 of 5 tiles were not drawn; the first: " + dashboardHiddenChart +
		" The dashboard's PDF could not be made: the frontend answered 500"
	mock.ExpectExec(`UPDATE workflow_step_runs SET status = \$4`).
		WithArgs(wfRunUUID, "board", 1, workflow.StatusSucceeded, sqlmock.AnyArg(), note).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT status, execution_id, saved_query_run_id::text, error, note, row_count, case_index, requested_at`).
		WithArgs(wfRunUUID, "board", 1).
		WillReturnRows(sqlmock.NewRows([]string{"status", "execution_id", "saved_query_run_id", "error", "note",
			"row_count", "case_index", "requested_at"}).AddRow(workflow.StatusSucceeded, nil, nil, nil, note, nil, nil, nil))
	s, w := chartStepCall(t, database, dashboardStepRun(), "board")
	s.runDashboard()
	mockMet(t, mock)

	if a := stepAnswerOf(t, w); a.Status != workflow.StatusSucceeded {
		t.Fatalf("answer = %+v", a)
	}
	b := held.held[workflowResultKey(wfRunUUID, "board")]
	if b.PDF != nil || b.PDFError != "the frontend answered 500" || len(b.Tiles) != 5 {
		t.Fatalf("held = %+v", b)
	}
}

// A board that is gone or hidden, and an actor who may no longer run the workflow,
// are refusals: nothing is drawn, nothing is held.
func TestRunDashboard_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   string
		loaded error
		want   string
	}{
		{name: "board not visible", role: "owner", loaded: sql.ErrNoRows,
			want: "The dashboard was deleted, or the run-as user cannot see it."},
		{name: "actor no longer a member", role: "", want: workflowRoleRefusal("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			held := stubWorkflowDashboards(t)
			stubDashboardLoad(t, nil, tc.loaded)
			draws := stubTileDraws(t, nil)
			pdfs := stubDashboardPDF(t, fakeDashboardPDF, nil)
			expectRole(mock, tc.role)
			expectStepSettledFor(mock, "board", workflow.StatusFailed, tc.want)
			s, w := chartStepCall(t, database, dashboardStepRun(), "board")
			s.runDashboard()
			if a := stepAnswerOf(t, w); a.Status != workflow.StatusFailed || !a.Refused {
				t.Fatalf("answer = %+v", a)
			}
			if len(*draws) != 0 || len(*pdfs) != 0 || len(held.held) != 0 {
				t.Fatalf("draws %d, PDFs %d, held %d", len(*draws), len(*pdfs), len(held.held))
			}
		})
	}
}

// Tiles not read within the budget are not drawn, in the page's words, and the
// step still holds the rest.
func TestDrawWorkflowDashboardTiles_Budget(t *testing.T) {
	prev := dashboardDrawBudget
	dashboardDrawBudget = 50 * time.Millisecond
	t.Cleanup(func() { dashboardDrawBudget = prev })
	stubChartPNG(t, fakeChartPNG, nil)

	rec := &dashboardRecord{ID: dashboardID, Name: "Slow"}
	slow := map[string]chartDraw{boardScoreID: boardDraws()[boardScoreID]}
	rec.Tiles = append(rec.Tiles, boardTile("t-score", 0, 0, boardChart(boardScoreID, "Orders", chartStepScorecard)))
	for i := 0; i < 6; i++ {
		id := "b0000000-0000-4000-8000-00000000000" + string(rune('1'+i))
		rec.Tiles = append(rec.Tiles, boardTile("t-slow-"+id, 0, 4*(i+1), boardChart(id, "Slow "+id, chartStepBar)))
	}
	stubTileDraws(t, slow)

	out, draws := drawWorkflowDashboardTiles(context.Background(), nil, wsScopeWS, wsScopeUser, rec, time.Now())
	if out.Tiles[0].Chart == nil || out.Tiles[0].Chart.Text.Number != "40,005" {
		t.Fatalf("first tile = %+v", out.Tiles[0])
	}
	for _, tile := range out.Tiles[1:] {
		if tile.NotDrawn != dashboardTooLong || tile.Chart != nil {
			t.Fatalf("slow tile = %+v", tile)
		}
	}
	if len(draws) != 7 {
		t.Fatalf("draws = %d", len(draws))
	}
	for id, d := range draws {
		if id != "t-score" && (d.Draw != nil || d.Failure != "it took too long to read") {
			t.Fatalf("draw %s = %+v", id, d)
		}
	}
}

// ---------------------------------------------------------------------------
// The sends after it
// ---------------------------------------------------------------------------

// A send reads the dashboard before it; one whose step drew nothing is a gap
// with a note, not an error. Its drawing is deleted with the run.
func TestSendDrawings_Dashboard(t *testing.T) {
	held := stubWorkflowDashboards(t)
	run := dashboardStepRun()
	s := &graphStepCall{ctx: context.Background(), run: run, step: *run.Definition.Step("send_1")}
	key := workflowResultKey(run.RunID, "board")
	held.held[key] = workflowDashboard{StepID: "board", Name: "Weekly"}

	ds, notes, err := s.sendDrawings()
	if err != nil || len(ds) != 1 || ds[0].Dashboard == nil || ds[0].Dashboard.Name != "Weekly" || len(notes) != 0 {
		t.Fatalf("drawings %+v, notes %v, err %v", ds, notes, err)
	}

	dropWorkflowRunHeld(context.Background(), run)
	if !reflect.DeepEqual(held.deleted, []string{key}) {
		t.Fatalf("deleted = %v", held.deleted)
	}
	ds, notes, err = s.sendDrawings()
	if err != nil || len(ds) != 1 || ds[0].Dashboard != nil || ds[0].Kind != workflow.StepDashboard || len(notes) != 1 ||
		!strings.Contains(notes[0], `The dashboard step "board" drew nothing, so the message went without its dashboard.`) {
		t.Fatalf("drawings %+v, notes %v, err %v", ds, notes, err)
	}
}

func heldBoard() *workflowDashboard {
	score := &workflowChart{Name: "Orders", Type: charts.TypeScorecard, Link: "https://app.example.test/charts/s",
		Text: chartText{Label: "Count", Number: "40,005", Change: "+5.0% on the week before (38,100)"}}
	return &workflowDashboard{StepID: "board", DashboardID: dashboardID, Name: "Weekly <sales>",
		Link: "https://app.example.test/dashboards/" + dashboardID, ReadAt: time.Date(2026, 10, 7, 9, 5, 0, 0, time.UTC),
		PDF: fakeDashboardPDF, Tiles: []workflowDashboardTile{
			{Kind: charts.TileChart, Name: "Orders", Chart: score},
			{Kind: charts.TileChart, Name: "Orders by day", Chart: plottedChart("Orders by day", 2)},
			{Kind: charts.TileText, Body: "Read <me>\nfirst"},
			{Kind: charts.TileChart, Name: "Refunds", NotDrawn: "This chart cannot be drawn: This chart's source was deleted."},
		}}
}

// Email shows the board's name, when it was read and a button to it, then each
// tile in order, with the whole board attached as a PDF; a chart before it and the
// board's own charts share the pictures' numbering. Slack shows the name, the
// scorecards' numbers and the link.
func TestRenderWorkflowDrawings_Dashboard(t *testing.T) {
	out := renderWorkflowDrawings([]workflowDrawing{
		{Kind: workflow.StepChart, Chart: plottedChart("Orders by day", 2)},
		{Kind: workflow.StepDashboard, Dashboard: heldBoard()},
		{Kind: workflow.StepDashboard},
	})

	if len(out.Images) != 2 || out.Images[0].ContentID != "chart-1" || out.Images[1].ContentID != "chart-2" ||
		out.Images[1].Filename != "orders-by-day-2.png" {
		t.Fatalf("images = %+v", out.Images)
	}
	if len(out.Files) != 1 || out.Files[0].Filename != "weekly-sales-2026-10-07.pdf" ||
		out.Files[0].ContentType != "application/pdf" || !bytes.Equal(out.Files[0].Content, fakeDashboardPDF) {
		t.Fatalf("files = %+v", out.Files)
	}
	for _, want := range []string{"Weekly &lt;sales&gt;", "Data read 2026-10-07 09:05 UTC", ">Open dashboard</a>",
		`href="https://app.example.test/dashboards/` + dashboardID + `"`, "40,005", `<img src="cid:chart-2"`,
		"Read &lt;me&gt;<br>first", "Refunds", "This chart cannot be drawn: This chart&#39;s source was deleted.",
		"The whole dashboard is attached as weekly-sales-2026-10-07.pdf.", dashboardNotDrawn} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	if strings.Contains(out.HTML, "<sales>") || strings.Contains(out.HTML, "<me>") {
		t.Error("HTML carries unescaped text")
	}
	for _, want := range []string{"Weekly <sales>\nData read 2026-10-07 09:05 UTC\nOpen the dashboard: https://app.example.test/dashboards/",
		"Count: 40,005", "The chart is attached as orders-by-day-2.png.", "Read <me>\nfirst",
		"Refunds\nThis chart cannot be drawn", "The whole dashboard is attached as weekly-sales-2026-10-07.pdf.",
		dashboardNotDrawn} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	for _, want := range []string{"*<https://app.example.test/dashboards/" + dashboardID + "|Weekly &lt;sales&gt;>*",
		"_Data read 2026-10-07 09:05 UTC_", "• Orders: *40,005* (+5.0% on the week before (38,100))",
		"_2 more tile(s): open the dashboard to see them._", "_" + dashboardNotDrawn + "_"} {
		if !strings.Contains(out.Slack, want) {
			t.Errorf("Slack lacks %q:\n%s", want, out.Slack)
		}
	}

	// A board with no PDF says why; one with no tiles says so.
	b := heldBoard()
	b.PDF, b.PDFError, b.Tiles = nil, "the frontend answered 500", nil
	out = renderWorkflowDrawings([]workflowDrawing{{Kind: workflow.StepDashboard, Dashboard: b}})
	if len(out.Files) != 0 || !strings.Contains(out.HTML, "The dashboard&#39;s PDF could not be made: the frontend answered 500") ||
		!strings.Contains(out.Text, "This dashboard has no tiles yet.") {
		t.Fatalf("no PDF:\n%s\n%s", out.HTML, out.Text)
	}
}

// A board in Slack stays within its share of the message, however many
// scorecards it has.
func TestDashboardSlack_StaysWithinItsBudget(t *testing.T) {
	b := heldBoard()
	for i := 0; i < 30; i++ {
		b.Tiles = append(b.Tiles, workflowDashboardTile{Kind: charts.TileChart, Chart: &workflowChart{
			Name: strings.Repeat("A scorecard & more ", 4), Text: chartText{Number: "1,234,567"}}})
	}
	for _, budget := range []int{2000, 400, 120} {
		out := dashboardSlack(b, budget)
		if n := utf8.RuneCountInString(out); n > budget {
			t.Errorf("budget %d: %d runes:\n%s", budget, n, out)
		}
		if !strings.Contains(out, "|Weekly &lt;sales&gt;>*") {
			t.Errorf("budget %d: the link to the board is gone:\n%s", budget, out)
		}
		if budget == 400 && !strings.Contains(out, "more tile(s)") {
			t.Errorf("budget %d: the tiles left out are not counted:\n%s", budget, out)
		}
	}
}

// A message's files go out with every email: after the HTML through Resend, after
// the text through SMTP.
func TestWorkflowEmailFiles(t *testing.T) {
	files := []messageFile{{Filename: "weekly-2026-10-07.pdf", ContentType: "application/pdf", Content: fakeDashboardPDF}}
	r := resendFiles(files)
	if len(r) != 1 || r[0].Filename != "weekly-2026-10-07.pdf" || r[0].ContentType != "application/pdf" ||
		r[0].ContentID != "" || !bytes.Equal(r[0].Content, fakeDashboardPDF) {
		t.Fatalf("Resend = %+v", r)
	}
	m := smtpFiles(files)
	if len(m) != 1 || m[0].Filename != "weekly-2026-10-07.pdf" || m[0].ContentType != "application/pdf" ||
		!bytes.Equal(m[0].Content, fakeDashboardPDF) {
		t.Fatalf("SMTP = %+v", m)
	}
	msg, _ := renderWorkflowMessageWithDrawings("Weekly", workflow.Message{Body: "Hi"}, workflow.Result{}, workflow.Outcome{},
		"https://app.example.test/workflows/"+wfID, []workflowDrawing{{Kind: workflow.StepDashboard, Dashboard: heldBoard()}},
		time.Now())
	if len(msg.Files) != 1 || len(msg.Images) != 1 {
		t.Fatalf("message carries %d files, %d images", len(msg.Files), len(msg.Images))
	}
}

// ---------------------------------------------------------------------------
// The PDF: the frontend lays it out
// ---------------------------------------------------------------------------

func TestFrontendDashboardPDF(t *testing.T) {
	rec := testBoard()
	d := boardDraws()
	scoreDraw, goneDraw := d[boardScoreID], d[boardGoneID]
	scoreDraw.Result.SQL = "SELECT secret FROM orders"
	draws := map[string]dashboardTileDraw{
		"t-score": {Draw: &scoreDraw},
		"t-gone":  {Draw: &goneDraw},
		"t-bar":   {Failure: "it took too long to read"},
	}
	at := time.Date(2026, 10, 7, 9, 5, 0, 0, time.UTC)
	serve := func(t *testing.T, h http.HandlerFunc) {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		t.Setenv("FRONTEND_INTERNAL_URL", srv.URL+"/")
		t.Setenv("INTERNAL_SERVICE_SECRET", "s3cret")
	}

	t.Run("lays out with the secret", func(t *testing.T) {
		var raw []byte
		serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/internal/dashboards/pdf" ||
				r.Header.Get("X-Internal-Secret") != "s3cret" || r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			raw, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(fakeDashboardPDF)
		})
		pdf, err := frontendDashboardPDF(context.Background(), rec, draws, at)
		if err != nil || !bytes.Equal(pdf, fakeDashboardPDF) {
			t.Fatalf("pdf %q, err %v", pdf, err)
		}
		if bytes.Contains(raw, []byte("SELECT secret")) || bytes.Contains(raw, []byte(`"sql"`)) {
			t.Fatalf("the chart's SQL went to the frontend: %s", raw)
		}
		var got struct {
			Name        string `json:"name"`
			GeneratedAt string `json:"generated_at"`
			Tiles       []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"tiles"`
			Draws map[string]struct {
				Draw *struct {
					Chart  *struct{ ID string } `json:"chart"`
					Result *struct {
						Rows       [][]any  `json:"rows"`
						Masked     []string `json:"masked"`
						Unfiltered []string `json:"unfiltered"`
					} `json:"result"`
					Break   *chartBreak `json:"break"`
					Filters struct {
						Applied []string `json:"applied"`
						Ignored []string `json:"ignored"`
					} `json:"filters"`
				} `json:"draw"`
				Failure *string `json:"failure"`
			} `json:"draws"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Name != "Weekly <sales>" || got.GeneratedAt != "2026-10-07T09:05:00Z" || len(got.Tiles) != 5 || len(got.Draws) != 3 {
			t.Fatalf("body = %s", raw)
		}
		s := got.Draws["t-score"].Draw
		if s == nil || s.Chart == nil || s.Chart.ID != boardScoreID || s.Result == nil || len(s.Result.Rows) != 1 ||
			s.Result.Masked == nil || s.Result.Unfiltered == nil || s.Filters.Applied == nil || s.Filters.Ignored == nil {
			t.Fatalf("score draw = %s", raw)
		}
		if g := got.Draws["t-gone"].Draw; g == nil || g.Break == nil || g.Result != nil {
			t.Fatalf("gone draw = %s", raw)
		}
		if b := got.Draws["t-bar"]; b.Draw != nil || b.Failure == nil || *b.Failure != "it took too long to read" {
			t.Fatalf("bar draw = %s", raw)
		}
	})

	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"refused", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"tiles must be a list"}`))
		}, "the frontend answered 400: tiles must be a list"},
		{"not a PDF type", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(fakeDashboardPDF)
		}, `the frontend answered "text/html", not a PDF`},
		{"not a PDF body", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("<html>"))
		}, "the frontend's answer is not a PDF"},
		{"too large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(append(append([]byte{}, pdfSignature...), make([]byte, dashboardPDFMax)...))
		}, "the PDF is over 10 MB, too large to email"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serve(t, tc.h)
			if _, err := frontendDashboardPDF(context.Background(), rec, draws, at); err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("no secret", func(t *testing.T) {
		t.Setenv("INTERNAL_SERVICE_SECRET", "")
		if _, err := frontendDashboardPDF(context.Background(), rec, draws, at); err == nil ||
			!strings.Contains(err.Error(), "INTERNAL_SERVICE_SECRET") {
			t.Fatalf("err = %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Saving a workflow with a dashboard step
// ---------------------------------------------------------------------------

// expectBoardSeen answers the save's look at the board: one row when the saver
// sees it, none when it is deleted or private to someone else.
func expectBoardSeen(mock sqlmock.Sqlmock, seen bool) {
	rows := sqlmock.NewRows([]string{"id", "name", "description", "filters", "visibility", "proposed_by",
		"created_by", "created_at", "updated_at", "tiles"})
	if seen {
		rows.AddRow(dashboardID, "Weekly", "", []byte("[]"), "workspace", "", wsScopeUser, time.Now(), time.Now(), 5)
	}
	mock.ExpectQuery(`FROM dashboards d\s+WHERE d\.workspace_id = \$1 AND \(d\.visibility = 'workspace' OR d\.created_by = \$2\)\s+AND d\.id = \$3`).
		WithArgs(wsScopeWS, wsScopeUser, dashboardID).WillReturnRows(rows)
}

// A run draws the board as the person the workflow runs as, so a save checks
// the board is one the person saving sees, telling the model where ids come
// from and a person what to do.
func TestValidateWorkflowDefinition_DashboardStepsBoard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		board    string
		forModel bool
		expect   func(sqlmock.Sqlmock)
		code     string
		status   int
		want     []string
	}{
		{name: "not a dashboard id", board: "weekly sales", forModel: true, expect: func(sqlmock.Sqlmock) {},
			code: "invalid_dashboard", status: http.StatusBadRequest,
			want: []string{`Step board: "weekly sales" is not a dashboard id.`, "list_dashboards gives each dashboard's id."}},
		{name: "a board the saver does not see, for the model", board: dashboardID, forModel: true,
			expect: func(m sqlmock.Sqlmock) { expectBoardSeen(m, false) }, code: "dashboard_not_found",
			status: http.StatusNotFound,
			want:   []string{"no dashboard " + dashboardID + " you can see", "list_dashboards gives each dashboard's id."}},
		{name: "a board the saver does not see, for a person", board: dashboardID,
			expect: func(m sqlmock.Sqlmock) { expectBoardSeen(m, false) }, code: "dashboard_not_found",
			status: http.StatusNotFound, want: []string{"Step board: no dashboard " + dashboardID, "Pick another dashboard."}},
		{name: "a board the saver sees", board: dashboardID, expect: func(m sqlmock.Sqlmock) { expectBoardSeen(m, true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			tc.expect(mock)
			def := dashboardStepRun().Definition
			def.Steps[0].DashboardID = tc.board

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
			if refusal == nil || refusal.Code != tc.code || refusal.Status != tc.status || refusal.Step != "board" {
				t.Fatalf("refusal = %+v, want %s %d on step board", refusal, tc.code, tc.status)
			}
			for _, s := range tc.want {
				if !strings.Contains(refusal.Message, s) {
					t.Errorf("message %q lacks %q", refusal.Message, s)
				}
			}
		})
	}

	// A board that cannot be read is an error, not a refusal.
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`FROM dashboards d`).WillReturnError(errors.New("connection reset"))
	def := dashboardStepRun().Definition
	_, _, refusal, err := validateWorkflowDefinition(context.Background(), database, wsScopeWS, wsScopeUser,
		&def, security.WSAdmin, false)
	if err == nil || refusal != nil {
		t.Fatalf("err = %v, refusal = %+v; want the lookup's error", err, refusal)
	}
}

// rsync.ai drafts a dashboard step as it does a chart step: into the one message
// of a workflow with no query.
func TestDraftDefinition_DashboardStep(t *testing.T) {
	branches := []workflow.Branch{{Condition: workflow.Condition{Type: workflow.ConditionAlways},
		Message: workflow.Message{Subject: "Weekly", Body: "Weekly"}}}
	in := WorkflowDraftInput{Trigger: workflow.TriggerManual,
		Steps: []WorkflowStepInput{{ID: "board", Type: "Dashboard", DashboardID: dashboardID}}}
	def, r := draftDefinition(in, branches)
	if r != nil {
		t.Fatalf("refused: %+v", r)
	}
	if def.Steps[0].Type != workflow.StepDashboard || def.Steps[0].DashboardID != dashboardID ||
		!reflect.DeepEqual(def.Steps[1].After, []string{"board"}) {
		t.Fatalf("steps = %+v", def.Steps)
	}
	if got := draftStepTypes(def); !reflect.DeepEqual(got, []string{"dashboard"}) {
		t.Fatalf("draftStepTypes = %v", got)
	}

	in.ConnectionID, in.SQL = "11111111-1111-4111-8111-111111111111", "SELECT 1"
	_, r = draftDefinition(in, branches)
	if r == nil || r.Code != "invalid_steps" || r.Step != "board" ||
		!strings.Contains(r.Message, "a dashboard goes into the one message of a workflow with no query") {
		t.Fatalf("refusal = %+v, want invalid_steps naming step board", r)
	}
}

// An edit in rsync.ai does not change which board a dashboard step draws.
func TestWorkflowEditShapeRefusal_KeepsTheDashboard(t *testing.T) {
	have := dashboardStepRun().Definition
	if err := workflow.Normalize(&have); err != nil {
		t.Fatal(err)
	}
	got := have
	got.Steps = append([]workflow.Step(nil), have.Steps...)
	got.Steps[0].DashboardID = "77777777-7777-4777-8777-777777777777"
	if r := workflowEditShapeRefusal(have, got); r == nil || r.Code != "invalid_steps" || r.Step != "board" {
		t.Fatalf("refusal = %+v, want invalid_steps on step board", r)
	}
	if r := workflowEditShapeRefusal(have, have); r != nil {
		t.Fatalf("the same workflow was refused: %+v", r)
	}
}

// The workflow page names the board a dashboard step draws, from this
// workspace only.
func TestAttachStepRefs_NamesTheDashboard(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT 'dashboard', id::text, name FROM dashboards WHERE workspace_id = \$1 AND id = ANY\(\$5::uuid\[\]\)`).
		WithArgs(wsScopeWS, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "id", "name"}).AddRow("dashboard", dashboardID, "Weekly"))
	w := &Workflow{ID: wfID, Definition: dashboardStepRun().Definition}
	if err := attachStepRefs(context.Background(), database, wsScopeWS, []*Workflow{w}); err != nil {
		t.Fatal(err)
	}
	mockMet(t, mock)
	if want := map[string]WorkflowRef{"board": {ID: dashboardID, Name: "Weekly"}}; !reflect.DeepEqual(w.StepRefs, want) {
		t.Fatalf("step refs = %+v, want %+v", w.StepRefs, want)
	}
}

// A test draws no dashboard, and says so.
func TestWorkflowTestNotes_SayATestDrawsNoDashboard(t *testing.T) {
	got := workflowTestNotes(dashboardStepRun().Definition)
	if len(got) != 1 || !strings.HasPrefix(got[0], "A test draws no dashboard") {
		t.Fatalf("notes = %q", got)
	}
}
