package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"

	"api-gateway/internal/charts"
)

// The server's side of charts and dashboards (dashboards-design.md § 3, § 4, § 7): which
// sources a chart may read, who sees a chart, a dashboard or a draft, who may save one,
// and the pieces of a drafted dashboard.

func TestChartSourceRefusal(t *testing.T) {
	for _, c := range []struct {
		name string
		src  chartSourceRow
		say  string // "" = accepted
	}{
		{"saved query", chartSourceRow{ConnectorType: "postgresql", Materialization: matNone, SQLText: "SELECT day, amount FROM orders"}, ""},
		{"LIMIT at the cap", chartSourceRow{ConnectorType: "postgresql", Materialization: matNone, SQLText: "SELECT * FROM orders LIMIT 10001"}, ""},
		{"LIMIT above the cap", chartSourceRow{ConnectorType: "postgresql", Materialization: matNone, SQLText: "SELECT * FROM orders LIMIT 20000"}, "LIMIT 20000"},
		{"postgres WITH", chartSourceRow{ConnectorType: "postgresql", Materialization: matNone, SQLText: "WITH o AS (SELECT 1 AS n) SELECT n FROM o"}, ""},
		{"SQL Server WITH", chartSourceRow{ConnectorType: "sqlserver", Materialization: matNone, SQLText: "WITH o AS (SELECT 1 AS n) SELECT n FROM o"}, "starts with WITH"},
		{"SQL Server WITH behind a comment", chartSourceRow{ConnectorType: "sqlserver", Materialization: matNone, SQLText: "-- totals\n/* by day */ with o AS (SELECT 1 AS n) SELECT n FROM o"}, "starts with WITH"},
		{"SQL Server select", chartSourceRow{ConnectorType: "sqlserver", Materialization: matNone, SQLText: "SELECT TOP (10) n FROM o"}, ""},
		{"a write", chartSourceRow{ConnectorType: "postgresql", Materialization: matNone, SQLText: "DELETE FROM orders"}, "read-only SELECT"},
		{"model table", chartSourceRow{ConnectorType: "postgresql", Materialization: matTable, TargetTable: "gold.orders"}, ""},
		{"model with a bad table", chartSourceRow{ConnectorType: "postgresql", Materialization: matTable, TargetTable: "a.b.c"}, "no table to chart"},
		{"statement model", chartSourceRow{ConnectorType: "postgresql", Materialization: matStatement, SQLText: "CALL refresh()"}, "runs a statement"},
		{"not SQL", chartSourceRow{ConnectorType: "mongodb", Materialization: matNone, SQLText: "{}"}, "not a SQL connection"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := chartSourceRefusal(&c.src)
			if c.say == "" && got != "" {
				t.Fatalf("refused %q, want accepted", got)
			}
			if c.say != "" && !strings.Contains(got, c.say) {
				t.Fatalf("refusal %q, want one saying %q", got, c.say)
			}
		})
	}
}

func TestChartSourceBreak(t *testing.T) {
	if b := chartSourceBreak(nil); b == nil || b.Reason != "source_deleted" {
		t.Fatalf("deleted source = %+v, want source_deleted", b)
	}
	model := &chartSourceRow{Name: "daily", ConnectorType: "postgresql", Materialization: matTable, TargetTable: "gold.daily"}
	if b := chartSourceBreak(model); b == nil || b.Reason != "never_built" || !strings.Contains(b.Message, `"daily"`) {
		t.Fatalf("unbuilt model = %+v, want never_built naming it", b)
	}
	model.Built = true
	if b := chartSourceBreak(model); b != nil {
		t.Fatalf("built model = %+v, want no break", b)
	}
	if b := chartSourceBreak(&chartSourceRow{ConnectorType: "mongodb", Materialization: matNone}); b == nil || b.Reason != "source_invalid" {
		t.Fatalf("mongodb source = %+v, want source_invalid", b)
	}
}

// ---------------------------------------------------------------------------
// Who sees what: a chart, a dashboard or a draft outside the caller's view is a 404,
// the same answer as an id that does not exist.
// ---------------------------------------------------------------------------

const chartID = "33333333-3333-3333-3333-333333333333"

// The predicates are spelled out here, not read from chartSelect, so a change to the
// handler's predicate fails the test.
var (
	chartVisibleWhere     = `WHERE ch\.workspace_id = \$1 AND \(ch\.visibility = 'workspace' OR ch\.created_by = \$2\)`
	dashboardVisibleWhere = `WHERE d\.workspace_id = \$1 AND \(d\.visibility = 'workspace' OR d\.created_by = \$2\)`
)

func chartTestCall(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestChartReadsAreScopedToWhatTheViewerMaySee(t *testing.T) {
	for _, c := range []struct {
		name    string
		method  string
		route   string
		h       gin.HandlerFunc
		where   string
		columns []string
		say     string
	}{
		{"chart", http.MethodGet, "/charts/:id", GetChart, chartVisibleWhere + `\s+AND ch\.id = \$3`, []string{"id"}, "chart not found"},
		{"chart draw", http.MethodPost, "/charts/:id", DrawChart, chartVisibleWhere + `\s+AND ch\.id = \$3`, []string{"id"}, "chart not found"},
		{"dashboard", http.MethodGet, "/dashboards/:id", GetDashboard, dashboardVisibleWhere + `\s+AND d\.id = \$3`, []string{"id"}, "dashboard not found"},
		{"draft", http.MethodGet, "/dashboard-proposals/:id", GetDashboardProposal,
			`FROM dashboard_proposals\s+WHERE id = \$1 AND workspace_id = \$2 AND created_by = \$3`, []string{"id"}, "dashboard draft not found"},
	} {
		t.Run(c.name+" not visible", func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			q := mock.ExpectQuery(c.where)
			if c.name == "draft" {
				q.WithArgs(chartID, wsScopeWS, wsScopeUser)
			} else {
				q.WithArgs(wsScopeWS, wsScopeUser, chartID)
			}
			q.WillReturnRows(sqlmock.NewRows(c.columns))
			path := strings.Replace(c.route, ":id", chartID, 1)
			w := chartTestCall(wsScopeRouterAsRole(c.method, c.route, c.h, "viewer"), c.method, path, "{}")
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), c.say) {
				t.Fatalf("status %d %s, want 404 %q", w.Code, w.Body.String(), c.say)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
		t.Run(c.name+" bad id", func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			path := strings.Replace(c.route, ":id", "not-a-uuid", 1)
			w := chartTestCall(wsScopeRouterAsRole(c.method, c.route, c.h, "viewer"), c.method, path, "{}")
			// sqlmock refuses any query it was not told of, which would be a 500.
			if w.Code != http.StatusNotFound {
				t.Fatalf("status %d %s, want 404 before any query", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A dashboard shared with the workspace can hold a chart private to its author. Another
// viewer gets the tile's place with a message, and nothing of the chart is read or run.
func TestDashboardTileHidesAnotherPersonsPrivateChart(t *testing.T) {
	const dashID = "44444444-4444-4444-4444-444444444444"
	const tileID = "55555555-5555-5555-5555-555555555555"
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(dashboardVisibleWhere+`\s+AND d\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, dashID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "filters", "visibility", "proposed_by",
			"created_by", "created_at", "updated_at", "tiles"}).
			AddRow(dashID, "Revenue", "", []byte(`[]`), "workspace", "person",
				"99999999-9999-9999-9999-999999999999", now, now, 1))
	// The charts on it the viewer may see: the same predicate as a chart's own page.
	mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id IN \(SELECT t\.chart_id FROM dashboard_tiles t WHERE t\.dashboard_id = \$3`).
		WithArgs(wsScopeWS, wsScopeUser, dashID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(`FROM dashboard_tiles\s+WHERE dashboard_id = \$1 AND workspace_id = \$2`).
		WithArgs(dashID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "chart_id", "body", "x", "y", "w", "h"}).
			AddRow(tileID, charts.TileChart, chartID, "", 0, 0, 6, 7))

	route := "/dashboards/:id/tiles/:tileId/draw"
	w := chartTestCall(wsScopeRouterAsRole(http.MethodPost, route, DrawDashboardTile, "viewer"), http.MethodPost,
		"/dashboards/"+dashID+"/tiles/"+tileID+"/draw", `{"filters":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		Chart  json.RawMessage `json:"chart"`
		Result json.RawMessage `json:"result"`
		Break  *chartBreak     `json:"break"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Break == nil || got.Break.Reason != "chart_hidden" || string(got.Chart) != "null" || string(got.Result) != "null" {
		t.Fatalf("tile = %s, want a chart_hidden break and no chart", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Saving is for owners and admins (§ 4.1); a member is refused before anything is read.
func TestChartWritesNeedAnAdmin(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		route  string
		h      gin.HandlerFunc
	}{
		{"create chart", http.MethodPost, "/charts", CreateChart},
		{"update chart", http.MethodPut, "/charts/:id", UpdateChart},
		{"delete chart", http.MethodDelete, "/charts/:id", DeleteChart},
		{"create dashboard", http.MethodPost, "/dashboards", CreateDashboard},
		{"update dashboard", http.MethodPut, "/dashboards/:id", UpdateDashboard},
		{"delete dashboard", http.MethodDelete, "/dashboards/:id", DeleteDashboard},
		{"save draft", http.MethodPost, "/dashboard-proposals/:id", SaveDashboardProposal},
		{"apply dashboard change", http.MethodPost, "/dashboard-changes/:id/apply", ApplyDashboardChange},
	} {
		for _, role := range []string{"member", "viewer"} {
			t.Run(c.name+" as "+role, func(t *testing.T) {
				mock, cleanup := wsScopeMockDB(t)
				defer cleanup()
				path := strings.Replace(c.route, ":id", chartID, 1)
				w := chartTestCall(wsScopeRouterAsRole(c.method, c.route, c.h, role), c.method, path, `{"name":"x"}`)
				if w.Code != http.StatusForbidden {
					t.Fatalf("status %d %s, want 403", w.Code, w.Body.String())
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestBindStrictJSON(t *testing.T) {
	type body struct {
		Visibility string `json:"visibility"`
	}
	r := gin.New()
	r.POST("/x", func(c *gin.Context) {
		var b body
		if bindStrictJSON(c, &b) {
			c.JSON(http.StatusOK, gin.H{"visibility": b.Visibility})
		}
	})
	if w := chartTestCall(r, http.MethodPost, "/x", `{"visibility":"workspace"}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "workspace") {
		t.Fatalf("known keys: %d %s, want 200", w.Code, w.Body.String())
	}
	w := chartTestCall(r, http.MethodPost, "/x", `{"visibility":"workspace","created_by":"someone-else"}`)
	var got struct {
		Code string `json:"code"`
		Rule int    `json:"rule"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusUnprocessableEntity || got.Code != "invalid_settings" || got.Rule != 1 || !strings.Contains(w.Body.String(), "created_by") {
		t.Fatalf("unknown key: %d %s, want 422 invalid_settings rule 1 naming created_by", w.Code, w.Body.String())
	}
	if w := chartTestCall(r, http.MethodPost, "/x", `{"visibility":`); w.Code != http.StatusBadRequest {
		t.Fatalf("broken body: %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// A drafted dashboard (§ 7.3)
// ---------------------------------------------------------------------------

// A refusal goes back to the model, so an engine's error that quotes a value is scrubbed.
func TestPlanFromChartRefusalScrubs(t *testing.T) {
	r := &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "source_failed",
		Message: `ERROR: duplicate key value violates unique constraint; Key (email)=(alice@example.com) already exists`}
	got := planFromChartRefusal(r, "chart 2")
	if strings.Contains(got.Message, "alice@example.com") {
		t.Fatalf("message %q quotes the value", got.Message)
	}
	if !strings.HasPrefix(got.Message, "chart 2: ") || got.Code != "source_failed" || got.Status != http.StatusUnprocessableEntity {
		t.Fatalf("refusal = %+v, want it about chart 2 with the check's code and status", got)
	}
}

func TestFilterSlug(t *testing.T) {
	taken := map[string]bool{}
	for _, c := range []struct{ label, want string }{
		{"Order date", "order-date"},
		{"Order date", "order-date-2"},
		{"  Order -- date!  ", "order-date-3"},
		// An accent is dropped, not made a dash: the editor's filterId makes the same id.
		{"Région", "region"},
		{"Ångström", "angstrom"},
		{"日付", "filter-6"},
		{"", "filter-7"},
		{"A very long label that goes on past the cap", "a-very-long-label-that-goes"},
	} {
		n := len(taken) + 1
		got := filterSlug(c.label, taken, n)
		if got != c.want {
			t.Errorf("filterSlug(%q) = %q, want %q", c.label, got, c.want)
		}
	}
	// Every slug is a filter id the dashboard's own check accepts.
	var fs []charts.Filter
	for id := range taken {
		fs = append(fs, charts.Filter{ID: id, Kind: charts.FilterValues, Label: "L", Column: "c"})
	}
	if err := charts.ValidateFilters(fs[:charts.MaxFilters]); err != nil {
		t.Fatalf("slugs refused: %v", err)
	}
	idPattern := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	long := filterSlug(strings.Repeat("ab ", 40), map[string]bool{}, 1)
	if !idPattern.MatchString(long) {
		t.Fatalf("long label slug %q is not a filter id", long)
	}
}

func TestLayoutProposal(t *testing.T) {
	types := []string{
		charts.TypeBar, charts.TypeScorecard, charts.TypeScorecard, charts.TypeTable, charts.TypeScorecard,
		charts.TypeScorecard, charts.TypeLine, charts.TypeScorecard, charts.TypeHeatmap,
	}
	got := layoutProposal(types)
	want := []DashboardProposalTile{
		// Scorecards first, in the model's order: four fill a row.
		{Chart: 1, X: 0, Y: 0, W: 3, H: 5}, {Chart: 2, X: 3, Y: 0, W: 3, H: 5},
		{Chart: 4, X: 6, Y: 0, W: 3, H: 5}, {Chart: 5, X: 9, Y: 0, W: 3, H: 5},
		// The fifth shares a row with the first plot, and the 3 spare columns go to
		// them, the odd one to the right; the row is as tall as the plot.
		{Chart: 7, X: 0, Y: 5, W: 4, H: 7}, {Chart: 0, X: 4, Y: 5, W: 8, H: 7},
		// The other two plots fill the next row.
		{Chart: 6, X: 0, Y: 12, W: 6, H: 7}, {Chart: 8, X: 6, Y: 12, W: 6, H: 7},
		// Tables, the full width.
		{Chart: 3, X: 0, Y: 19, W: 12, H: 8},
	}
	sameLayout(t, got, want)

	// "t1426 swipes" on prod (2026-10-06): two scorecards, a bar and a table. Each
	// group had its own row, so the bar's row and the scorecards' row ended in an
	// empty half; now the scorecards and the bar share one full row.
	sameLayout(t, layoutProposal([]string{charts.TypeScorecard, charts.TypeScorecard, charts.TypeBar, charts.TypeTable}), []DashboardProposalTile{
		{Chart: 0, X: 0, Y: 0, W: 3, H: 7}, {Chart: 1, X: 3, Y: 0, W: 3, H: 7}, {Chart: 2, X: 6, Y: 0, W: 6, H: 7},
		{Chart: 3, X: 0, Y: 7, W: 12, H: 8},
	})
	// A lone plot is the full width, not half of it.
	sameLayout(t, layoutProposal([]string{charts.TypeLine}), []DashboardProposalTile{{Chart: 0, X: 0, Y: 0, W: 12, H: 7}})
	// A pivot table goes with the tables, after the plots and the full width.
	sameLayout(t, layoutProposal([]string{charts.TypePivot, charts.TypeScatter}), []DashboardProposalTile{
		{Chart: 1, X: 0, Y: 0, W: 12, H: 7}, {Chart: 0, X: 0, Y: 7, W: 12, H: 8},
	})

	// The largest draft fits the dashboard's own tile checks.
	all := make([]string, dashboardProposalChartsMax)
	ids := make([]string, dashboardProposalChartsMax)
	for i := range all {
		all[i] = []string{charts.TypeScorecard, charts.TypeBar, charts.TypeTable}[i%3]
		ids[i] = "c" + string(rune('a'+i))
	}
	tiles := layoutProposal(all)
	if len(tiles) != dashboardProposalChartsMax {
		t.Fatalf("placed %d of %d charts", len(tiles), dashboardProposalChartsMax)
	}
	if err := charts.ValidateTiles(proposalTiles(tiles, ids)); err != nil {
		t.Fatalf("the largest draft's layout is refused: %v", err)
	}
	if got := layoutProposal(nil); len(got) != 0 {
		t.Fatalf("no charts placed %+v", got)
	}
}

// Every draft of 1 to 6 charts, of each mix and order: each row of tiles is the full 12
// columns, with no gap and no overlap, and its tiles are one height. A row that ends
// early leaves an empty space on the right of the dashboard (the bug on prod).
func TestLayoutProposalFillsEveryRow(t *testing.T) {
	kinds := []string{charts.TypeScorecard, charts.TypeBar, charts.TypeTable}
	var draft func(types []string)
	checked := 0
	draft = func(types []string) {
		if len(types) > 0 {
			checked++
			// Walk the tiles in their order: each one sits right of the last at its
			// height, and a new row starts only under a full one.
			tiles := layoutProposal(types)
			x, y, h := 0, 0, 0
			for _, tile := range tiles {
				if x == charts.GridColumns {
					x, y = 0, y+h
				}
				if x == 0 {
					h = tile.H
				}
				if tile.X != x || tile.Y != y || tile.H != h {
					t.Fatalf("%v: tile %+v is not at x %d, y %d, h %d: %+v", types, tile, x, y, h, tiles)
				}
				x += tile.W
			}
			if x != charts.GridColumns {
				t.Fatalf("%v: the last row is %d of %d columns: %+v", types, x, charts.GridColumns, tiles)
			}
		}
		if len(types) == 6 {
			return
		}
		for _, k := range kinds {
			draft(append(append([]string{}, types...), k))
		}
	}
	draft(nil)
	if checked != 1092 { // 3 + 9 + 27 + 81 + 243 + 729
		t.Fatalf("checked %d drafts, want 1092", checked)
	}
}

func sameLayout(t *testing.T, got, want []DashboardProposalTile) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("layout = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tile %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestApplicableFilters(t *testing.T) {
	fs := []charts.Filter{
		{ID: "day", Kind: charts.FilterDateRange, Label: "Day", Column: "order_date"},
		{ID: "region", Kind: charts.FilterValues, Label: "Region", Column: "region"},
		{ID: "plan", Kind: charts.FilterValues, Label: "Plan", Column: "plan"},
	}
	bound := []charts.Bound{
		{ID: "day", Column: "order_date", Kind: charts.FilterDateRange},
		{ID: "region", Column: "region", Kind: charts.FilterValues, Values: []string{"EU"}},
		{ID: "plan", Column: "plan", Kind: charts.FilterValues, Values: []string{"pro"}},
	}
	apply, ignored := applicableFilters(bound, []string{"order_date", "plan", "amount"})
	applied, without := appliedFilters(apply, nil)
	if len(apply) != 2 || apply[0].Column != "order_date" || apply[1].Column != "plan" {
		t.Fatalf("apply = %+v, want order_date and plan", apply)
	}
	if strings.Join(applied, ",") != "day,plan" || strings.Join(ignored, ",") != "region" || without == nil || len(without) != 0 {
		t.Fatalf("applied %v ignored %v without %v, want [day plan] [region] []", applied, ignored, without)
	}
	// A filter the connection could not bind (the result's unfiltered column) is not
	// applied: the tile was drawn without it.
	if got, out := appliedFilters(apply, []string{"plan"}); strings.Join(got, ",") != "day" || strings.Join(out, ",") != "plan" {
		t.Fatalf("plan unfiltered: applied %v without %v, want [day] [plan]", got, out)
	}
	if got, out := appliedFilters(apply, []string{"order_date", "plan"}); got == nil || len(got) != 0 || strings.Join(out, ",") != "day,plan" {
		t.Fatalf("all unfiltered: applied %v without %v, want [] [day plan]", got, out)
	}
	// No pick, nothing applied or ignored; both are lists, never null.
	apply, ignored = applicableFilters(nil, []string{"region"})
	applied, without = appliedFilters(apply, []string{"region"})
	if apply != nil || applied == nil || ignored == nil || without == nil || len(applied)+len(ignored)+len(without) != 0 {
		t.Fatalf("no picks = %v %v %v %v", apply, applied, ignored, without)
	}
	// Two filters on one column: only the one picked is named, once.
	twice := append(fs, charts.Filter{ID: "tier", Kind: charts.FilterValues, Label: "Tier", Column: "plan"},
		charts.Filter{ID: "area", Kind: charts.FilterValues, Label: "Area", Column: "region"})
	bound, err := charts.BindFilters(twice, map[string]charts.FilterValue{"plan": {Values: []string{"pro"}}, "region": {Values: []string{"EU"}}})
	if err != nil {
		t.Fatal(err)
	}
	apply, ignored = applicableFilters(bound, []string{"plan"})
	if applied, _ = appliedFilters(apply, nil); strings.Join(applied, ",") != "plan" || strings.Join(ignored, ",") != "region" {
		t.Fatalf("two filters on a column: applied %v ignored %v, want [plan] [region]", applied, ignored)
	}
	// Drawn without its column: only the picked filter on it is named, not Tier.
	if _, without = appliedFilters(apply, []string{"plan"}); strings.Join(without, ",") != "plan" {
		t.Fatalf("two filters on an unfiltered column: without %v, want [plan]", without)
	}
}

// ---------------------------------------------------------------------------
// A drawn chart's result
// ---------------------------------------------------------------------------

func TestShapeChartResult(t *testing.T) {
	s := mustSettings(t, sqlBarSum)
	// Snowflake and Oracle hand back upper-cased names; the order is the settings' own.
	res := &ExplorerQueryResponse{
		Columns: []string{"AMOUNT", "DAY"},
		Rows:    []map[string]interface{}{{"AMOUNT": 12.5, "DAY": "2026-10-01"}},
	}
	got, err := shapeChartResult(s, res)
	if err != nil {
		t.Fatal(err)
	}
	if got.Columns[0].Alias != "day" || got.Rows[0][0] != "2026-10-01" || got.Rows[0][1] != 12.5 {
		t.Fatalf("shaped = %+v %v, want day then amount", got.Columns, got.Rows)
	}
	if len(got.Masked) != 0 {
		t.Fatalf("masked %v, want none", got.Masked)
	}
	if _, err := shapeChartResult(s, &ExplorerQueryResponse{Columns: []string{"x"}, Rows: nil}); err == nil {
		t.Fatal("a result without the chart's columns was shaped")
	}
}

// A scorecard over a column the Explorer masks: the Explorer masks the measure by its
// name, and the earlier period, named "previous", is the same number, so it is masked too.
func TestShapeChartResultMasksTheEarlierPeriod(t *testing.T) {
	s := mustSettings(t, `{"type":"scorecard","measure":{"aggregate":"max","column":"phone_number"},"compare":{"column":"day","days":7}}`)
	res := &ExplorerQueryResponse{
		Columns: []string{"phone_number", "previous"},
		Rows:    []map[string]interface{}{{"phone_number": "55***", "previous": "5551234567"}},
	}
	got, err := shapeChartResult(s, res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Masked, ",") != "phone_number,previous" {
		t.Fatalf("masked %v, want phone_number and previous", got.Masked)
	}
	if got.Rows[0][1] == "5551234567" {
		t.Fatalf("the earlier period %v is shown unmasked", got.Rows[0][1])
	}
	// The same chart over a column that is not masked keeps both numbers.
	s = mustSettings(t, `{"type":"scorecard","measure":{"aggregate":"sum","column":"amount"},"compare":{"column":"day","days":7}}`)
	got, err = shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"amount", "previous"},
		Rows:    []map[string]interface{}{{"amount": 10, "previous": 8}},
	})
	if err != nil || len(got.Masked) != 0 || got.Rows[0][1] != 8 {
		t.Fatalf("unmasked scorecard = %+v, %v", got, err)
	}
}

// A histogram of a masked column: its edges are masked by name, and its bin width, the
// column's spread over the bin count, is masked with them. A combo's second measure of
// a masked column keeps the column's name with "_2", which the masking still reads.
func TestShapeChartResultMasksWhatAMaskedColumnDerives(t *testing.T) {
	s := mustSettings(t, `{"version":2,"type":"histogram","roles":{"x":[{"column":"phone_number"}]}}`)
	got, err := shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"phone_number", "rsync_bin_width", "count"},
		Rows:    []map[string]interface{}{{"phone_number": "55***", "rsync_bin_width": 123456.5, "count": 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Masked, ",") != "phone_number,rsync_bin_width" {
		t.Fatalf("masked %v, want phone_number and rsync_bin_width", got.Masked)
	}
	if got.Rows[0][1] == 123456.5 || got.Rows[0][2] != 3 {
		t.Fatalf("row %v: the bin width is shown unmasked or the count is lost", got.Rows[0])
	}
	// Over a column that is not masked, the width is the number.
	s = mustSettings(t, `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount"}]}}`)
	got, err = shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"amount", "rsync_bin_width", "count"},
		Rows:    []map[string]interface{}{{"amount": 0, "rsync_bin_width": 12.5, "count": 3}},
	})
	if err != nil || len(got.Masked) != 0 || got.Rows[0][1] != 12.5 {
		t.Fatalf("unmasked histogram = %+v, %v", got, err)
	}

	s = mustSettings(t, `{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"max","column":"phone_number","mark":"bar","axis":"left"},{"aggregate":"min","column":"phone_number","mark":"line","axis":"right"}]}}`)
	got, err = shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"day", "phone_number", "phone_number_2"},
		Rows:    []map[string]interface{}{{"day": "2026-10-01", "phone_number": "55***", "phone_number_2": "55***"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Masked, ",") != "phone_number,phone_number_2" {
		t.Fatalf("masked %v, want both measures", got.Masked)
	}
}

// A box plot's statistics and a candlestick's prices of a masked column are that column's
// values under rsync.ai's names, which the masking does not read: they are masked with the
// column. Over a column that is not masked they are the numbers, and the group is never
// masked for its numbers.
func TestShapeChartResultMasksAMaskedColumnsStatistics(t *testing.T) {
	s := mustSettings(t, `{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"card_number"}]}}`)
	row := map[string]interface{}{"region": "EU", "rsync_min": 41000, "rsync_q1": 52000, "rsync_median": 61000, "rsync_q3": 70000, "rsync_max": 98000}
	got, err := shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"region", "rsync_min", "rsync_q1", "rsync_median", "rsync_q3", "rsync_max"},
		Rows:    []map[string]interface{}{row},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Masked, ",") != "rsync_min,rsync_q1,rsync_median,rsync_q3,rsync_max" {
		t.Fatalf("masked %v, want the five statistics", got.Masked)
	}
	for i, v := range got.Rows[0][1:] {
		if v == row[charts.BoxAliases[i]] {
			t.Fatalf("%s %v is shown unmasked", charts.BoxAliases[i], v)
		}
	}
	if got.Rows[0][0] != "EU" {
		t.Fatalf("the group %v was masked with its numbers", got.Rows[0][0])
	}
	// The control: the same box plot of a column that is not masked.
	s = mustSettings(t, `{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"amount"}]}}`)
	got, err = shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"region", "rsync_min", "rsync_q1", "rsync_median", "rsync_q3", "rsync_max"},
		Rows:    []map[string]interface{}{row},
	})
	if err != nil || len(got.Masked) != 0 || got.Rows[0][3] != 61000 {
		t.Fatalf("unmasked box plot = %+v, %v", got, err)
	}

	// A candlestick of one masked price column and three that are not.
	s = mustSettings(t, `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"open"},{"column":"card_number"},{"column":"low"},{"column":"close"}]}}`)
	got, err = shapeChartResult(s, &ExplorerQueryResponse{
		Columns: []string{"day", "rsync_open", "rsync_high", "rsync_low", "rsync_close"},
		Rows:    []map[string]interface{}{{"day": "2026-10-01", "rsync_open": 1, "rsync_high": 98000, "rsync_low": 0.5, "rsync_close": 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Masked, ",") != "rsync_high" || got.Rows[0][2] == 98000 || got.Rows[0][1] != 1 {
		t.Fatalf("masked %v, row %v: want only the high, from card_number, masked", got.Masked, got.Rows[0])
	}
}

// A sparkline's rows come back as [date, value], found by alias whatever the case; a
// masked date or value draws none, and a result without its columns is an error.
func TestShapeSpark(t *testing.T) {
	s := mustSettings(t, `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"sum","column":"amount"}],"date":[{"column":"day"}]},"compare":{"days":30},"sparkline":true}`)
	got, err := shapeSpark(s, &ExplorerQueryResponse{
		Columns: []string{"AMOUNT", "DAY"},
		Rows:    []map[string]interface{}{{"AMOUNT": 4, "DAY": "2026-09-06"}, {"AMOUNT": 7, "DAY": "2026-09-07"}},
	})
	if err != nil || len(got) != 2 || got[0][0] != "2026-09-06" || got[1][1] != 7 {
		t.Fatalf("spark = %v, %v; want [date, value] per point", got, err)
	}
	if _, err := shapeSpark(s, &ExplorerQueryResponse{Columns: []string{"day"}}); err == nil {
		t.Fatal("a sparkline without its value was shaped")
	}
	s = mustSettings(t, `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"max","column":"phone_number"}],"date":[{"column":"day"}]},"compare":{"days":30},"sparkline":true}`)
	got, err = shapeSpark(s, &ExplorerQueryResponse{
		Columns: []string{"day", "phone_number"},
		Rows:    []map[string]interface{}{{"day": "2026-09-06", "phone_number": "55***"}},
	})
	if err != nil || got != nil {
		t.Fatalf("a masked sparkline = %v, %v; want none", got, err)
	}
}

// fakeChartEngine stands in for the orchestrator's explorer-query endpoint, which runs a
// delegated engine's statements (BigQuery here): answer has each statement's reply, and
// every statement sent is recorded.
func fakeChartEngine(t *testing.T, answer func(query string) (int, string)) func() []string {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		sent = append(sent, req.Query)
		mu.Unlock()
		status, body := answer(req.Query)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ORCHESTRATOR_URL", srv.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

func bigQuerySavedQuery(t *testing.T) *chartSourceRow {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"project_id":"p","dataset":"d"}`)
	if err != nil {
		t.Fatal(err)
	}
	return &chartSourceRow{Name: "events", ConnectorType: "bigquery", Materialization: matNone,
		SQLText: "SELECT * FROM events", ConfigEncrypted: enc}
}

// A table of rows is ordered by each of its columns, and an engine that cannot order one
// of them (BigQuery an array, Postgres a json value) refuses the statement. The table is
// then drawn from the statement with only the sort picked, as it was before, and says
// which ran; it is saved the same. A source that is slow is not run twice.
func TestChartTableRunsUnorderedWhenAColumnCannotBeOrdered(t *testing.T) {
	sent := fakeChartEngine(t, func(query string) (int, string) {
		if strings.Contains(query, "ORDER BY") {
			return http.StatusBadRequest, `{"error":"query failed","details":"ORDER BY does not support expressions of type ARRAY<INT64> at [3:31]"}`
		}
		return http.StatusOK, `{"columns":["day","tags"],"rows":[{"day":"2026-10-01","tags":[1,2]}],"row_count":1}`
	})
	src := bigQuerySavedQuery(t)
	settings := mustSettings(t, `{"type":"table","columns":["day","tags"]}`)

	res, brk, err := drawChart(context.Background(), src, settings, nil, time.Now())
	if err != nil || brk != nil {
		t.Fatalf("drawChart = %v, %v; want the table drawn", brk, err)
	}
	if len(res.Rows) != 1 || strings.Contains(res.SQL, "ORDER BY") {
		t.Fatalf("rows %d, SQL =\n%s\nwant 1 row from the statement that ran, unordered", len(res.Rows), res.SQL)
	}
	// The page says the rows are in no set order, never silently.
	if body, _ := json.Marshal(res); !strings.Contains(string(body), `"unordered":true`) {
		t.Fatalf("result = %s, want unordered: true", body)
	}
	if got := sent(); len(got) != 2 || !strings.Contains(got[0], "ORDER BY") || strings.Contains(got[1], "ORDER BY") {
		t.Fatalf("sent %q, want the ordered statement, then the one without the order", got)
	}
	if refusal := runChartOnce(context.Background(), src, settings, nil); refusal != nil {
		t.Fatalf("saving the table was refused: %+v", refusal)
	}

	// Ordering every row of a big table can take longer than the engine allows, where
	// the statement without the order stops at the cap: one that times out is run again
	// without it.
	timedOut := `{"error":"query failed","details":"Query exceeded the timeout of 30s"}`
	sent = fakeChartEngine(t, func(query string) (int, string) {
		if strings.Contains(query, "ORDER BY") {
			return http.StatusGatewayTimeout, timedOut
		}
		return http.StatusOK, `{"columns":["day","tags"],"rows":[{"day":"2026-10-01","tags":[1,2]}],"row_count":1}`
	})
	res, brk, err = drawChart(context.Background(), src, settings, nil, time.Now())
	if err != nil || brk != nil || len(res.Rows) != 1 || strings.Contains(res.SQL, "ORDER BY") {
		t.Fatalf("a table whose order timed out = %+v, %v, %v; want it drawn without the order", res, brk, err)
	}
	if got := sent(); len(got) != 2 {
		t.Fatalf("sent %q, want the ordered statement, then the one without the order", got)
	}

	// A source that times out without the order too keeps the timeout, run twice at most.
	sent = fakeChartEngine(t, func(string) (int, string) { return http.StatusGatewayTimeout, timedOut })
	if _, _, err := drawChart(context.Background(), src, settings, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "(timeout)") {
		t.Fatalf("a timed-out table = %v, want the timeout", err)
	}
	// (After a failed run the source is read once for its columns: that is not the table.)
	table := 0
	for _, q := range sent() {
		if strings.Contains(q, "AS `tags`") {
			table++
		}
	}
	if table != 2 {
		t.Fatalf("sent %q, want the table's statement with its order and without", sent())
	}

	// A source that is unreachable is not run again: it would fail the same.
	sent = fakeChartEngine(t, func(string) (int, string) {
		return http.StatusBadGateway, `{"error":"query failed","details":"dial tcp 10.0.0.9:5432: connection refused"}`
	})
	if _, _, err := drawChart(context.Background(), src, settings, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "(network_or_unavailable)") {
		t.Fatalf("an unreachable table = %v, want the network error", err)
	}
	table = 0
	for _, q := range sent() {
		if strings.Contains(q, "AS `tags`") {
			table++
		}
	}
	if table != 1 {
		t.Fatalf("sent %q, want the table's statement once", sent())
	}
}

// An engine's own error is shown to the chart's viewers, a dashboard's and a workflow
// step's: it says why the statement failed, never the engine's links, its job or where
// it ran (a BigQuery job's URL names the project).
func TestChartRunErrorLeavesOutTheEnginesLinksAndJob(t *testing.T) {
	fakeChartEngine(t, func(string) (int, string) {
		return http.StatusBadRequest, `{"error":"query failed","details":"400 POST https://bigquery.googleapis.com/bigquery/v2/projects/acme-prod-4471/queries?prettyPrint=false: Syntax error: Unexpected keyword WHERE at [4:1]; see https://cloud.google.com/bigquery/docs/error-messages\n\nLocation: EU\nJob ID: 2b1e7c1a-91f4-4c55-a0d3-5c2c2c9f0e11"}`
	})
	settings := mustSettings(t, `{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"count"}]}}`)
	_, _, err := drawChart(context.Background(), bigQuerySavedQuery(t), settings, nil, time.Now())
	if err == nil {
		t.Fatal("drawChart passed, want the engine's error")
	}
	msg := err.Error()
	for _, leak := range []string{"https://", "acme-prod-4471", "googleapis", "Job ID", "2b1e7c1a", "Location"} {
		if strings.Contains(msg, leak) {
			t.Errorf("the error names %q: %s", leak, msg)
		}
	}
	if !strings.Contains(msg, "(syntax_error): Syntax error: Unexpected keyword WHERE at [4:1]") {
		t.Errorf("the error = %s\nwant the engine's reason", msg)
	}
}

// A source's probe reads a column's kind from its first value. A warehouse can return a
// timestamp as text with its offset after a space (Python's str of a datetime, Postgres's
// text form): that is a date, as the ISO form is, never text.
func TestValueKindReadsATimestampWithItsOffsetAsADate(t *testing.T) {
	for v, want := range map[interface{}]string{
		"2026-10-01":                       "time",
		"2026-10-01 09:30:00":              "time",
		"2026-10-01T09:30:00Z":             "time",
		"2026-10-01 09:30:00+00:00":        "time",
		"2026-10-01 09:30:00.123456+05:30": "time",
		"2026-10-01 09:30:00+00":           "time",
		"2026-10-01 09:30:00 UTC":          "time",
		"20261001":                         "number",
		"INR":                              "text",
		"Oct 1, 2026":                      "text",
		true:                               "bool",
		nil:                                "unknown",
	} {
		if got := valueKind(v); got != want {
			t.Errorf("valueKind(%v) = %q, want %q", v, got, want)
		}
	}
}

// A column declared as text holds text though its values read as numbers: Postgres and
// MySQL return a NUMERIC as text, so a value alone cannot tell a postcode from an amount,
// and a sum over a text column of digits was let run (MySQL and SQLite then add it up,
// Postgres refuses it). A first value of NULL takes its declared kind; a date held as
// text stays a date the chart casts; an engine that declares nothing is read as before.
func TestSourceColumnsTakeTheirDeclaredTypeOverWhatAValueReadsAs(t *testing.T) {
	res := &ExplorerQueryResponse{
		Columns:     []string{"postcode", "amount", "orders", "region", "opened", "day", "rate"},
		ColumnTypes: []string{"VARCHAR", "NUMERIC", "INT8", "character varying(40)", "TIMESTAMPTZ", "TEXT", "DECIMAL(10,2)"},
		Rows: []map[string]interface{}{{"postcode": "02134", "amount": "12.50", "orders": int64(3), "region": nil,
			"opened": nil, "day": "2026-10-01", "rate": "0.25"}},
	}
	kinds := func() map[string]string { return chartColumnKinds(sourceColumns(res)) }
	want := map[string]string{"postcode": "text", "amount": "number", "orders": "number", "region": "text",
		"opened": "time", "day": "time", "rate": "number"}
	if got := kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("kinds = %v\nwant %v", got, want)
	}
	settings := mustSettings(t, `{"version":2,"type":"table","roles":{"columns":[{"column":"day"}],"measure":[{"aggregate":"sum","column":"postcode"}]}}`)
	if err := settings.CheckKinds(kinds(), false); err == nil || err.Error() != `a sum needs a number column; the column "postcode" holds text` {
		t.Errorf("a sum of the postcode: %v, want it refused before it runs", err)
	}

	res.ColumnTypes = nil
	want = map[string]string{"postcode": "number", "amount": "number", "orders": "number", "region": "unknown",
		"opened": "unknown", "day": "time", "rate": "number"}
	if got := kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("with no declared types, kinds = %v\nwant %v", got, want)
	}
}

// A column of a kind its role or its aggregate cannot use is a refused setting that
// says so (rule 2), never the warehouse's own error. It is refused before the chart runs
// when the probe's kind cannot fit (text for a scatter's x, a sum of text), and after
// the engine refuses the chart's statement when a date role's column reads as text,
// since that may be a date the warehouse returned as text. A preview and a save are
// refused (422 invalid_settings); a saved chart draws the reason in its place. A run
// that fails some other way (a timeout) keeps its own error.
func TestChartColumnKindsAreRefusedWithTheReason(t *testing.T) {
	const sourceID = "66666666-6666-6666-6666-666666666666"
	const probe = `{"columns":["currency","card_spend","day"],"rows":[{"currency":"INR","card_spend":5,"day":"2026-10-01"}],"row_count":1}`
	refused := func() (int, string) {
		return http.StatusBadRequest, `{"error":"query failed","details":"400 POST https://bigquery.googleapis.com/bigquery/v2/projects/p/jobs: Invalid timestamp: 'INR'"}`
	}
	timedOut := func() (int, string) {
		return http.StatusGatewayTimeout, `{"error":"query failed","details":"Query exceeded the timeout of 30s"}`
	}
	// engine answers the source's probe with its columns and each chart statement with chart.
	engine := func(t *testing.T, chart func() (int, string)) func() []string {
		return fakeChartEngine(t, func(q string) (int, string) {
			if strings.HasPrefix(q, "SELECT * FROM") {
				return http.StatusOK, probe
			}
			return chart()
		})
	}
	const calendar = `{"version":2,"type":"calendar","roles":{"x":[{"column":"currency"}],"measure":[{"aggregate":"sum","column":"card_spend"}]}}`
	const wantCalendar = `a calendar heatmap needs a date column for its days; the column "currency" holds text`
	src := bigQuerySavedQuery(t)
	src.ID, src.WorkspaceID, src.Visibility = sourceID, wsScopeWS, "workspace"
	sourceRow := func() *sqlmock.Rows { return savedQuerySourceRow(src) }
	preview := func(t *testing.T, settings string) *httptest.ResponseRecorder {
		mock, cleanup := wsScopeMockDB(t)
		t.Cleanup(cleanup)
		mock.ExpectQuery(`FROM saved_queries sq`).WithArgs(sourceID, wsScopeWS, wsScopeUser).WillReturnRows(sourceRow())
		return chartTestCall(wsScopeRouterAsRole(http.MethodPost, "/charts/preview", PreviewChart, "viewer"),
			http.MethodPost, "/charts/preview", `{"source_id":"`+sourceID+`","settings":`+settings+`}`)
	}
	wantRefused := func(t *testing.T, w *httptest.ResponseRecorder, want string) {
		t.Helper()
		var got struct {
			Code  string `json:"code"`
			Error string `json:"error"`
			Rule  int    `json:"rule"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusUnprocessableEntity || got.Code != "invalid_settings" || got.Rule != 2 || got.Error != want {
			t.Fatalf("preview = %d %s\nwant 422 invalid_settings rule 2: %s", w.Code, w.Body.String(), want)
		}
	}
	kinds := chartColumnKinds([]chartColumn{{"currency", "text"}, {"card_spend", "number"}, {"day", "time"}})

	t.Run("a scatter of text is refused before it runs", func(t *testing.T) {
		sent := engine(t, refused)
		wantRefused(t, preview(t, `{"version":2,"type":"scatter","roles":{"x":[{"column":"currency"}],"y":[{"column":"card_spend"}]}}`),
			`a scatter chart needs a number column for its x; the column "currency" holds text`)
		if got := sent(); len(got) != 1 {
			t.Fatalf("sent %q, want only the source's probe", got)
		}
	})
	t.Run("a sum of text is refused before it runs", func(t *testing.T) {
		engine(t, refused)
		wantRefused(t, preview(t, `{"version":2,"type":"table","roles":{"columns":[{"column":"day"}],"measure":[{"aggregate":"sum","column":"currency"}]}}`),
			`a sum needs a number column; the column "currency" holds text`)
	})
	t.Run("a calendar of text the engine refused", func(t *testing.T) {
		engine(t, refused)
		wantRefused(t, preview(t, calendar), wantCalendar)

		settings := mustSettings(t, calendar)
		if r := runChartOnce(context.Background(), src, settings, kinds); r == nil || r.Code != "invalid_settings" || r.Rule != 2 || r.Message != wantCalendar {
			t.Fatalf("saving = %+v\nwant invalid_settings rule 2: %s", r, wantCalendar)
		}
		_, brk, err := drawChart(context.Background(), src, settings, nil, time.Now())
		if ce, ok := charts.AsError(err); brk != nil || !ok || ce.Message != wantCalendar {
			t.Fatalf("drawChart = %v, %v\nwant the refused setting: %s", brk, err, wantCalendar)
		}

		mockDB, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer mockDB.Close()
		mock.ExpectQuery(`FROM saved_queries sq`).WithArgs(sourceID, wsScopeWS, wsScopeUser).WillReturnRows(sourceRow())
		d := drawSavedChart(context.Background(), mockDB, wsScopeWS, wsScopeUser,
			&chartRecord{SourceState: chartSourceOK, Settings: json.RawMessage(calendar), savedQueryID: sourceID}, nil, time.Now())
		if d.Break == nil || d.Break.Reason != "settings_invalid" || !strings.HasSuffix(d.Break.Message, wantCalendar) || d.Error != "" {
			t.Fatalf("a saved calendar of text = break %+v, error %q\nwant settings_invalid: %s", d.Break, d.Error, wantCalendar)
		}
	})
	t.Run("a calendar of text Postgres refused", func(t *testing.T) {
		// Postgres names the function a text column has none of: "does not exist", which
		// reads as a missing column, though the probe found every column the chart names.
		engine(t, func() (int, string) {
			return http.StatusBadRequest, `{"error":"query failed","details":"pq: function date_trunc(unknown, text) does not exist"}`
		})
		wantRefused(t, preview(t, calendar), wantCalendar)
		if r := runChartOnce(context.Background(), src, mustSettings(t, calendar), kinds); r == nil || r.Code != "invalid_settings" || r.Message != wantCalendar {
			t.Fatalf("saving = %+v\nwant invalid_settings rule 2: %s", r, wantCalendar)
		}
	})
	t.Run("a calendar of text that timed out keeps the timeout", func(t *testing.T) {
		engine(t, timedOut)
		w := preview(t, calendar)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "(timeout)") {
			t.Fatalf("preview = %d %s, want 200 with the timeout", w.Code, w.Body.String())
		}
		if r := runChartOnce(context.Background(), src, mustSettings(t, calendar), kinds); r == nil || r.Code != "chart_failed" {
			t.Fatalf("saving = %+v, want chart_failed", r)
		}
	})
}

// savedQuerySourceRow is src as loadChartSource reads it (FROM saved_queries sq).
func savedQuerySourceRow(src *chartSourceRow) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "workspace_id", "name", "sql_text", "materialization", "target_table",
		"visibility", "created_by", "last_run_status", "last_run_error", "deadline", "built",
		"connection_id", "connection_name", "connector_type", "config", "week_start", "fiscal_month"}).
		AddRow(src.ID, src.WorkspaceID, src.Name, src.SQLText, src.Materialization, "", src.Visibility, wsScopeUser,
			"", "", 0, false, "77777777-7777-7777-7777-777777777777", "warehouse", src.ConnectorType, src.ConfigEncrypted, 1, 1)
}

// A tile's filters.applied names only the filters its statement took. A connection that
// cannot bind a value (BigQuery here) draws the tile unfiltered and names each such
// column in result.unfiltered; that filter is then not applied, and a client reading
// applied alone must not take the rows for filtered. A filter whose column the source
// lacks is ignored, as before; one with no pick is in neither list.
func TestDashboardTileAppliesOnlyTheFiltersItsStatementTook(t *testing.T) {
	const dashID = "44444444-4444-4444-4444-444444444444"
	const tileID = "55555555-5555-5555-5555-555555555555"
	const sourceID = "66666666-6666-6666-6666-666666666666"
	sent := fakeChartEngine(t, func(string) (int, string) {
		return http.StatusOK, `{"columns":["currency","card_spend"],"rows":[{"currency":"INR","card_spend":5}],"row_count":1}`
	})
	src := bigQuerySavedQuery(t)
	src.ID, src.WorkspaceID, src.Visibility = sourceID, wsScopeWS, "workspace"
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	filters := `[{"id":"currency","kind":"values","label":"Currency","column":"currency"},
		{"id":"day","kind":"date_range","label":"Day","column":"day"},
		{"id":"spend","kind":"values","label":"Spend","column":"card_spend"},
		{"id":"plan","kind":"values","label":"Plan","column":"plan"},
		{"id":"money","kind":"values","label":"Money","column":"currency"}]`

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectQuery(dashboardVisibleWhere+`\s+AND d\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, dashID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "filters", "visibility", "proposed_by",
			"created_by", "created_at", "updated_at", "tiles"}).
			AddRow(dashID, "Cards", "", []byte(filters), "workspace", "person", wsScopeUser, now, now, 1))
	mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id IN \(SELECT t\.chart_id FROM dashboard_tiles t WHERE t\.dashboard_id = \$3`).
		WithArgs(wsScopeWS, wsScopeUser, dashID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "saved_query_id", "visible", "source_name", "materialization",
			"settings", "settings_version", "source_columns", "visibility", "proposed_by", "created_by", "created_at", "updated_at"}).
			AddRow(chartID, "Spend", sourceID, true, "cards", "",
				[]byte(`{"version":2,"type":"table","roles":{"columns":[{"column":"currency"},{"column":"card_spend"}]}}`), 2,
				[]byte(`["currency","card_spend","day"]`), "workspace", "person", wsScopeUser, now, now))
	mock.ExpectQuery(`FROM dashboard_tiles\s+WHERE dashboard_id = \$1 AND workspace_id = \$2`).
		WithArgs(dashID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "chart_id", "body", "x", "y", "w", "h"}).
			AddRow(tileID, charts.TileChart, chartID, "", 0, 0, 6, 7))
	mock.ExpectQuery(`FROM saved_queries sq`).WithArgs(sourceID, wsScopeWS, wsScopeUser).WillReturnRows(savedQuerySourceRow(src))

	route := "/dashboards/:id/tiles/:tileId/draw"
	w := chartTestCall(wsScopeRouterAsRole(http.MethodPost, route, DrawDashboardTile, "viewer"), http.MethodPost,
		"/dashboards/"+dashID+"/tiles/"+tileID+"/draw",
		`{"filters":{"currency":{"values":["EUR"]},"day":{"from":"2026-10-01","to":"2026-10-07"},"plan":{"values":["pro"]}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s, want 200", w.Code, w.Body.String())
	}
	var got struct {
		Result struct {
			Rows       [][]any  `json:"rows"`
			Unfiltered []string `json:"unfiltered"`
		} `json:"result"`
		Filters struct {
			Applied    []string `json:"applied"`
			Ignored    []string `json:"ignored"`
			Unfiltered []string `json:"unfiltered"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The filters it was drawn without, by id: the picked ones on those columns, not
	// Money, which is on currency too but was not picked.
	if strings.Join(got.Filters.Unfiltered, ",") != "currency,day" {
		t.Fatalf("filters unfiltered %v, want [currency day]: the picks on the columns BigQuery could not take", got.Filters.Unfiltered)
	}
	if strings.Join(got.Result.Unfiltered, ",") != "currency,day" || len(got.Result.Rows) != 1 {
		t.Fatalf("result = %s\nwant the tile drawn, unfiltered by currency and day", w.Body.String())
	}
	if got.Filters.Applied == nil || len(got.Filters.Applied) != 0 || strings.Join(got.Filters.Ignored, ",") != "plan" {
		t.Fatalf("filters applied %v ignored %v, want [] and [plan]: BigQuery took neither currency nor day, and spend had no pick",
			got.Filters.Applied, got.Filters.Ignored)
	}
	if q := sent(); len(q) != 1 || strings.Contains(q[0], "EUR") {
		t.Fatalf("sent %q, want one statement with no value pasted in", q)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
