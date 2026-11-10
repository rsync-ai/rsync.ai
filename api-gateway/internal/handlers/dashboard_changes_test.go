package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"api-gateway/internal/charts"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
)

// A change to one dashboard tile by asking, and a dashboard's versions
// (dashboards-design.md § 7.6, § 7.7): the change is checked and kept, never saved; an
// owner or admin applies it; a chart on another dashboard too is copied for the tile; a
// dashboard saved since makes the change out of date; every save keeps the version
// before it.

const (
	chgDash     = "66666666-6666-6666-6666-666666666666"
	chgTileA    = "77777777-7777-7777-7777-777777777771"
	chgTileB    = "77777777-7777-7777-7777-777777777772"
	chgTileC    = "77777777-7777-7777-7777-777777777773"
	chgID       = "88888888-8888-8888-8888-888888888888"
	chgChartB   = "33333333-3333-3333-3333-333333333334"
	chgNewChart = "33333333-3333-3333-3333-333333333335"
	chgSource   = "99999999-9999-9999-9999-999999999990"

	chgBarSettings    = `{"type":"bar","x":{"column":"day","title":"Day"},"measure":{"column":"amount","aggregate":"sum"}}`
	chgSeriesSettings = `{"type":"bar","x":{"column":"day"},"measure":{"column":"amount","aggregate":"sum"},"series":{"column":"region"}}`
	chgLineSettings   = `{"type":"line","x":{"column":"day","title":"Day"},"measure":{"column":"amount","aggregate":"sum"}}`
)

var (
	chgBase      = time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	chgChartTime = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
)

// expectChangeDashboard expects loadVisibleDashboard of "Revenue": tile A (chart
// chartID with settingsA, unless settingsA is "" and the chart is private to someone
// else) at 0,0 6×7, tile B (chart chgChartB) at 6,0 6×7, and a text tile C at 0,7 12×3.
func expectChangeDashboard(mock sqlmock.Sqlmock, updatedAt time.Time, settingsA string, chartAUpdated time.Time) {
	mock.ExpectQuery(dashboardVisibleWhere+`\s+AND d\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chgDash).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "filters", "visibility", "proposed_by",
			"created_by", "created_at", "updated_at", "tiles"}).
			AddRow(chgDash, "Revenue", "", []byte(`[]`), "workspace", "person", wsScopeUser, chgBase, updatedAt, 3))
	chartRows := sqlmock.NewRows([]string{"id", "name", "saved_query_id", "visible", "sq_name", "materialization",
		"settings", "settings_version", "source_columns", "visibility", "proposed_by", "created_by", "created_at", "updated_at"})
	if settingsA != "" {
		chartRows.AddRow(chartID, "Revenue by day", chgSource, true, "orders", matNone, []byte(settingsA), charts.Version,
			[]byte(`["day","amount","region"]`), "workspace", "person", wsScopeUser, chgChartTime, chartAUpdated)
	}
	chartRows.AddRow(chgChartB, "Orders", chgSource, true, "orders", matNone, []byte(chgBarSettings), charts.Version,
		[]byte(`["day","amount"]`), "workspace", "person", wsScopeUser, chgChartTime, chgChartTime)
	mock.ExpectQuery(chartVisibleWhere+`\s+AND ch\.id IN`).WithArgs(wsScopeWS, wsScopeUser, chgDash).WillReturnRows(chartRows)
	mock.ExpectQuery(`FROM dashboard_tiles\s+WHERE dashboard_id = \$1 AND workspace_id = \$2`).WithArgs(chgDash, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "chart_id", "body", "x", "y", "w", "h"}).
			AddRow(chgTileA, charts.TileChart, chartID, "", 0, 0, 6, 7).
			AddRow(chgTileB, charts.TileChart, chgChartB, "", 6, 0, 6, 7).
			AddRow(chgTileC, charts.TileText, "", "Notes", 0, 7, 12, 3))
}

func chgContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set("user_id", wsScopeUser)
	c.Set(ctxWorkspaceID, wsScopeWS)
	return c
}

// stubChangedChart replaces the chart check, which runs the source; calls counts them.
func stubChangedChart(t *testing.T, calls *[]chartInput) {
	t.Helper()
	prev := checkChangedChart
	checkChangedChart = func(_ context.Context, _ *sql.DB, _, _ string, in chartInput) (*checkedChart, *chartRefusal, error) {
		*calls = append(*calls, in)
		s, err := charts.Decode(in.Settings)
		if err != nil {
			return nil, settingsRefusal(err), nil
		}
		raw, _ := json.Marshal(s)
		return &checkedChart{Name: in.Name, Visibility: in.Visibility, Settings: s, SettingsJSON: raw,
			SourceColumns: []string{"day", "amount"}, Source: &chartSourceRow{ID: in.SourceID}}, nil, nil
	}
	t.Cleanup(func() { checkChangedChart = prev })
}

func planRefusalOf(t *testing.T, err error) *PlanRefusal {
	t.Helper()
	var r *PlanRefusal
	if !errors.As(err, &r) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------
// Drafting a change
// ---------------------------------------------------------------------------

// A wider tile moves the tiles in its way down, as the editor does, and the change is
// kept for its person: nothing on the dashboard is written.
func TestProposeDashboardChange_SizeMovesTilesDownAndSavesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	var calls []chartInput
	stubChangedChart(t, &calls)
	expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM dashboard_changes\s+WHERE workspace_id = \$1 AND created_by = \$2 AND status = 'proposed'`).
		WithArgs(wsScopeWS, wsScopeUser).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT dashboard_id\) FROM dashboard_tiles`).WithArgs(wsScopeWS, chartID, chgDash).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	var body string
	mock.ExpectQuery(`INSERT INTO dashboard_changes`).
		WithArgs(wsScopeWS, chgDash, chgTileA, chartID, wsScopeUser, actorKindAssistant, nil, chgBase,
			captureJSONArg(&body), "Widen the revenue chart", int(dashboardChangeTTL.Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "expires_at"}).AddRow(chgID, chgBase, chgBase.Add(dashboardChangeTTL)))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := ProposeDashboardChange(chgContext(), assistantAs(security.WSAdmin), DashboardChangeInput{
		DashboardID: chgDash, TileID: chgTileA, W: 12, Summary: "  Widen the   revenue chart ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ChartChanged || !got.SizeChanged || got.Copy || got.After.W != 12 || got.After.X != 0 || got.SharedElsewhere != 1 {
		t.Fatalf("change = %+v, want a size change to 12 columns that copies nothing", got)
	}
	wantMoves := []DashboardChangeMove{{TileID: chgTileB, FromY: 0, Y: 7}, {TileID: chgTileC, FromY: 7, Y: 14}}
	if !slices.Equal(got.Moves, wantMoves) {
		t.Fatalf("moves = %+v, want %+v", got.Moves, wantMoves)
	}
	if got.URL != DashboardURL(chgDash)+"?change="+chgID {
		t.Fatalf("url = %q", got.URL)
	}
	if len(calls) != 0 {
		t.Fatalf("a size change ran the chart check %d times, want 0", len(calls))
	}
	var stored dashboardChangeBody
	if err := json.Unmarshal([]byte(body), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.After.W != 12 || len(stored.Moves) != 2 || stored.SourceID != chgSource || !stored.ChartUpdatedAt.Equal(chgChartTime) {
		t.Fatalf("stored change = %s", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A changed chart is checked with the person's rights, as a save would, and the card
// says it will be copied when the chart is on another dashboard too.
func TestProposeDashboardChange_ChangedChartIsCheckedAndCopiedWhenShared(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	var calls []chartInput
	stubChangedChart(t, &calls)
	expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM dashboard_changes`).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT dashboard_id\) FROM dashboard_tiles`).WithArgs(wsScopeWS, chartID, chgDash).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(2))
	var body string
	mock.ExpectQuery(`INSERT INTO dashboard_changes`).
		WithArgs(wsScopeWS, chgDash, chgTileA, chartID, wsScopeUser, actorKindAssistant, nil, chgBase,
			captureJSONArg(&body), "As a line", int(dashboardChangeTTL.Seconds())).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "expires_at"}).AddRow(chgID, chgBase, chgBase.Add(dashboardChangeTTL)))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	got, err := ProposeDashboardChange(chgContext(), assistantAs(security.WSAdmin), DashboardChangeInput{
		DashboardID: chgDash, TileID: chgTileA, Summary: "As a line",
		Settings: json.RawMessage(`{"type":"line","x":{"column":"day"},"measure":{"column":"amount","aggregate":"sum"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].SourceID != chgSource || calls[0].Visibility != "workspace" || calls[0].Name != "Revenue by day" {
		t.Fatalf("chart checks = %+v, want one on the chart's own source and visibility", calls)
	}
	// The axis title the tools cannot set is kept while its column stays.
	if !strings.Contains(string(calls[0].Settings), `"title":"Day"`) {
		t.Fatalf("checked settings %s lost the axis title", calls[0].Settings)
	}
	if !got.ChartChanged || got.SizeChanged || !got.Copy || got.SharedElsewhere != 2 || got.After.Type != charts.TypeLine || got.Before.Type != "bar" {
		t.Fatalf("change = %+v, want a chart change that copies the chart", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Each refusal goes back to the model before anything is stored.
func TestProposeDashboardChange_Refusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		actor  services.Actor
		in     DashboardChangeInput
		expect func(sqlmock.Sqlmock)
		code   string
		say    string
	}{
		{"the REST channel", services.Actor{UserID: wsScopeUser, WorkspaceID: wsScopeWS, Role: security.WSOwner, Channel: services.ChannelREST},
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 12}, func(sqlmock.Sqlmock) {}, "forbidden", "Only Ask rsync.ai"},
		{"a dashboard the person may not see", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 12}, func(m sqlmock.Sqlmock) {
				m.ExpectQuery(dashboardVisibleWhere+`\s+AND d\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chgDash).
					WillReturnRows(sqlmock.NewRows([]string{"id"}))
			}, "not_found", "No dashboard with that id"},
		{"a tile not on it", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgID, W: 12}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
			}, "not_found", "no tile with that id"},
		{"a text tile", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileC, W: 6}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
			}, "invalid_settings", "text tile"},
		{"another person's private chart", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 12}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, "", chgChartTime)
			}, "chart_hidden", "private to someone else"},
		{"a colour on a chart with a series", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, Color: "series-2"}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgSeriesSettings, chgChartTime)
			}, "invalid_settings", "each series takes its own colour"},
		{"a tile too narrow", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 1}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
			}, "invalid_settings", "2 to 12 columns"},
		{"the size it has", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 6, H: 7}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
			}, "no_change", "changes nothing"},
		{"too many open changes", assistantAs(security.WSAdmin),
			DashboardChangeInput{DashboardID: chgDash, TileID: chgTileA, W: 12}, func(m sqlmock.Sqlmock) {
				expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
				m.ExpectQuery(`SELECT COUNT\(\*\) FROM dashboard_changes`).
					WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(dashboardChangesOpenMax))
			}, "too_many_changes", "not applied"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			var calls []chartInput
			stubChangedChart(t, &calls)
			c.expect(mock)
			_, err := ProposeDashboardChange(chgContext(), c.actor, c.in)
			r := planRefusalOf(t, err)
			if r.Code != c.code || !strings.Contains(r.Message, c.say) {
				t.Fatalf("refusal = %+v, want %s saying %q", r, c.code, c.say)
			}
			if len(calls) != 0 {
				t.Fatalf("the chart check ran %d times, want 0", len(calls))
			}
			// sqlmock refuses any statement it was not told of, so no INSERT ran.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Applying a change
// ---------------------------------------------------------------------------

// chgBody is a stored change to tile A: the chart's settings become after (the same
// as before for a size change) and its width w.
func chgBody(after string, w int, moves []DashboardChangeMove) dashboardChangeBody {
	before := DashboardChangeSide{Name: "Revenue by day", Type: "bar", Settings: json.RawMessage(chgBarSettings), X: 0, Y: 0, W: 6, H: 7}
	a := before
	a.Settings, a.W = json.RawMessage(after), w
	if after != chgBarSettings {
		s, _ := charts.Decode(json.RawMessage(after))
		a.Type = s.Type
	}
	if moves == nil {
		moves = []DashboardChangeMove{}
	}
	return dashboardChangeBody{DashboardName: "Revenue", Before: before, After: a, Moves: moves,
		ChartUpdatedAt: chgChartTime, SourceID: chgSource, SourceName: "orders", ChartVisibility: "workspace"}
}

// expectChangeLoad expects the change's own row, scoped to its person.
func expectChangeLoad(mock sqlmock.Sqlmock, forUpdate bool, status string, expired bool, body dashboardChangeBody) {
	raw, _ := json.Marshal(body)
	where := `FROM dashboard_changes\s+WHERE id = \$1 AND workspace_id = \$2 AND created_by = \$3`
	if forUpdate {
		where += ` FOR UPDATE`
	}
	mock.ExpectQuery(where).WithArgs(chgID, wsScopeWS, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id", "dashboard_id", "tile_id", "chart_id", "actor_kind", "base_updated_at",
			"change", "summary", "status", "created_at", "expires_at", "expired", "applied_at", "applied_chart_id", "copied"}).
			AddRow(chgID, chgDash, chgTileA, chartID, actorKindAssistant, chgBase, raw, "a change", status,
				chgBase, chgBase.Add(dashboardChangeTTL), expired, nil, nil, false))
}

func applyChange(role string) *httptest.ResponseRecorder {
	route := "/dashboard-changes/:id/apply"
	return chartTestCall(wsScopeRouterAsRole(http.MethodPost, route, ApplyDashboardChange, role), http.MethodPost,
		"/dashboard-changes/"+chgID+"/apply", "")
}

// expectApplyTxStart expects the apply's transaction up to the kept version: the change
// locked, the dashboard's row taken at the change's base, and the version before it.
func expectApplyTxStart(mock sqlmock.Sqlmock, body dashboardChangeBody, tiles *string) {
	mock.ExpectBegin()
	expectChangeLoad(mock, true, planStatusProposed, false, body)
	mock.ExpectExec(`UPDATE dashboards SET updated_at = now\(\)\s+WHERE id = \$1 AND workspace_id = \$2 AND updated_at = \$3`).
		WithArgs(chgDash, wsScopeWS, chgBase).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO dashboard_versions`).
		WithArgs(chgDash, wsScopeWS, "Revenue", "", "[]", "workspace", captureJSONArg(tiles), wsScopeUser, dashboardSavedByAsk, chgID, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// A size change writes the tile and the moved tiles in one transaction, keeps the
// dashboard as it was as a version, and leaves the chart as it is.
func TestApplyDashboardChange_SizeOnly(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	var calls []chartInput
	stubChangedChart(t, &calls)
	moves := []DashboardChangeMove{{TileID: chgTileB, FromY: 0, Y: 7}, {TileID: chgTileC, FromY: 7, Y: 14}}
	body := chgBody(chgBarSettings, 12, moves)
	expectChangeLoad(mock, false, planStatusProposed, false, body)
	expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
	var tiles string
	expectApplyTxStart(mock, body, &tiles)
	mock.ExpectExec(`UPDATE dashboard_tiles SET chart_id = \$4, x = \$5, w = \$6, h = \$7`).
		WithArgs(chgTileA, chgDash, wsScopeWS, chartID, 0, 12, 7, chartID).WillReturnResult(sqlmock.NewResult(0, 1))
	for _, m := range moves {
		mock.ExpectExec(`UPDATE dashboard_tiles SET y = \$4`).WithArgs(m.TileID, chgDash, wsScopeWS, m.Y, m.FromY).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(`UPDATE dashboard_changes SET status = 'applied'`).WithArgs(chgID, chartID, false).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(wsScopeUser, "apply_dashboard_change", "dashboard", chgDash,
		sqlmock.AnyArg(), sqlmock.AnyArg(), wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))

	w := applyChange("admin")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"copied":false`) || !strings.Contains(w.Body.String(), chartID) {
		t.Fatalf("status %d %s, want 200 with the same chart", w.Code, w.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("a size change ran the chart check %d times, want 0", len(calls))
	}
	// The version holds each chart by id, never its settings, and the board as it was.
	if !strings.Contains(tiles, `"chart_id":"`+chartID+`"`) || strings.Contains(tiles, "settings") || !strings.Contains(tiles, `"w":6`) {
		t.Fatalf("kept tiles = %s", tiles)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A changed chart is checked again with the applier's rights. On this dashboard only, it
// is changed in place; on another dashboard too, this tile gets a copy and the chart the
// other dashboard shows is not written.
func TestApplyDashboardChange_InPlaceOrCopy(t *testing.T) {
	for _, c := range []struct {
		name   string
		shared int
	}{
		{"only here: changed in place", 0},
		{"also elsewhere: copied", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			var calls []chartInput
			stubChangedChart(t, &calls)
			body := chgBody(chgLineSettings, 6, nil)
			expectChangeLoad(mock, false, planStatusProposed, false, body)
			expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
			var tiles string
			expectApplyTxStart(mock, body, &tiles)
			mock.ExpectQuery(`SELECT COUNT\(DISTINCT dashboard_id\) FROM dashboard_tiles`).WithArgs(wsScopeWS, chartID, chgDash).
				WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(c.shared))
			settings, _ := charts.Decode(json.RawMessage(chgLineSettings))
			settingsJSON, _ := json.Marshal(settings)
			chartOnTile := chartID
			if c.shared > 0 {
				chartOnTile = chgNewChart
				mock.ExpectQuery(`INSERT INTO charts`).
					WithArgs(wsScopeWS, "Revenue by day", chgSource, string(settingsJSON), charts.Version, `["day","amount"]`,
						"workspace", actorKindAssistant, wsScopeUser).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(chgNewChart))
			} else {
				mock.ExpectExec(`UPDATE charts\s+SET name = \$3, settings = \$4`).
					WithArgs(chartID, wsScopeWS, "Revenue by day", string(settingsJSON), charts.Version, `["day","amount"]`, chgChartTime).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec(`UPDATE dashboard_tiles SET chart_id = \$4`).
				WithArgs(chgTileA, chgDash, wsScopeWS, chartOnTile, 0, 6, 7, chartID).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`UPDATE dashboard_changes SET status = 'applied'`).WithArgs(chgID, chartOnTile, c.shared > 0).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			chartAction := "update_chart"
			if c.shared > 0 {
				chartAction = "create_chart"
			}
			mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(wsScopeUser, chartAction, "chart", chartOnTile,
				sqlmock.AnyArg(), sqlmock.AnyArg(), wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO audit_logs`).WithArgs(wsScopeUser, "apply_dashboard_change", "dashboard", chgDash,
				sqlmock.AnyArg(), sqlmock.AnyArg(), wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))

			w := applyChange("owner")
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), chartOnTile) {
				t.Fatalf("status %d %s, want 200 naming chart %s", w.Code, w.Body.String(), chartOnTile)
			}
			if len(calls) != 1 || calls[0].Visibility != "workspace" || calls[0].SourceID != chgSource {
				t.Fatalf("chart checks = %+v, want one with the chart's source and visibility", calls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A change is applied once, while it lasts, and never over a dashboard or chart saved
// since it was drafted.
func TestApplyDashboardChange_RefusedWhenNotCurrent(t *testing.T) {
	for _, c := range []struct {
		name   string
		expect func(sqlmock.Sqlmock)
		status int
		code   string
	}{
		{"already applied", func(m sqlmock.Sqlmock) {
			expectChangeLoad(m, false, "applied", false, chgBody(chgBarSettings, 12, nil))
		}, http.StatusConflict, "already_applied"},
		{"expired", func(m sqlmock.Sqlmock) {
			expectChangeLoad(m, false, planStatusProposed, true, chgBody(chgBarSettings, 12, nil))
		}, http.StatusGone, "expired"},
		{"the dashboard saved since", func(m sqlmock.Sqlmock) {
			expectChangeLoad(m, false, planStatusProposed, false, chgBody(chgBarSettings, 12, nil))
			expectChangeDashboard(m, chgBase.Add(time.Minute), chgBarSettings, chgChartTime)
		}, http.StatusConflict, "dashboard_changed"},
		{"the chart saved since", func(m sqlmock.Sqlmock) {
			expectChangeLoad(m, false, planStatusProposed, false, chgBody(chgLineSettings, 6, nil))
			expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime.Add(time.Minute))
		}, http.StatusConflict, "dashboard_changed"},
		{"the dashboard no longer visible", func(m sqlmock.Sqlmock) {
			expectChangeLoad(m, false, planStatusProposed, false, chgBody(chgBarSettings, 12, nil))
			m.ExpectQuery(dashboardVisibleWhere).WillReturnRows(sqlmock.NewRows([]string{"id"}))
		}, http.StatusConflict, "dashboard_changed"},
		{"the dashboard saved between the check and the write", func(m sqlmock.Sqlmock) {
			body := chgBody(chgBarSettings, 12, nil)
			expectChangeLoad(m, false, planStatusProposed, false, body)
			expectChangeDashboard(m, chgBase, chgBarSettings, chgChartTime)
			m.ExpectBegin()
			expectChangeLoad(m, true, planStatusProposed, false, body)
			m.ExpectExec(`UPDATE dashboards SET updated_at = now\(\)`).WithArgs(chgDash, wsScopeWS, chgBase).
				WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectRollback()
		}, http.StatusConflict, "dashboard_changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			var calls []chartInput
			stubChangedChart(t, &calls)
			c.expect(mock)
			w := applyChange("admin")
			if w.Code != c.status || !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) {
				t.Fatalf("status %d %s, want %d %s", w.Code, w.Body.String(), c.status, c.code)
			}
			if len(calls) != 0 {
				t.Fatalf("the chart check ran %d times, want 0", len(calls))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A change is its person's: another person's id is a 404, as one that does not exist.
func TestDashboardChangeIsItsPersons(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		route  string
		h      gin.HandlerFunc
		role   string
	}{
		{"read", http.MethodGet, "/dashboard-changes/:id", GetDashboardChange, "viewer"},
		{"apply", http.MethodPost, "/dashboard-changes/:id/apply", ApplyDashboardChange, "admin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectQuery(`FROM dashboard_changes\s+WHERE id = \$1 AND workspace_id = \$2 AND created_by = \$3`).
				WithArgs(chgID, wsScopeWS, wsScopeUser).WillReturnRows(sqlmock.NewRows([]string{"id"}))
			path := strings.Replace(c.route, ":id", chgID, 1)
			w := chartTestCall(wsScopeRouterAsRole(c.method, c.route, c.h, c.role), c.method, path, "")
			if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "dashboard change not found") {
				t.Fatalf("status %d %s, want 404", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The card reads whether the change is current and whether Apply would copy the chart;
// a viewer sees it and may not apply it.
func TestGetDashboardChange(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectChangeLoad(mock, false, planStatusProposed, false, chgBody(chgLineSettings, 6, nil))
	expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT dashboard_id\) FROM dashboard_tiles`).WithArgs(wsScopeWS, chartID, chgDash).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	route := "/dashboard-changes/:id"
	w := chartTestCall(wsScopeRouterAsRole(http.MethodGet, route, GetDashboardChange, "viewer"), http.MethodGet,
		"/dashboard-changes/"+chgID, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var got struct {
		Change    DashboardChange `json:"change"`
		CanManage bool            `json:"can_manage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CanManage || got.Change.Stale || !got.Change.Copy || !got.Change.ChartChanged || got.Change.SizeChanged || got.Change.SharedElsewhere != 1 {
		t.Fatalf("got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Versions (§ 7.7)
// ---------------------------------------------------------------------------

// History is as private as its dashboard.
func TestListDashboardVersions(t *testing.T) {
	route := "/dashboards/:id/versions"
	t.Run("a dashboard the viewer may not see", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		mock.ExpectQuery(dashboardVisibleWhere+`\s+AND d\.id = \$3`).WithArgs(wsScopeWS, wsScopeUser, chgDash).
			WillReturnRows(sqlmock.NewRows([]string{"id"}))
		w := chartTestCall(wsScopeRouterAsRole(http.MethodGet, route, ListDashboardVersions, "viewer"), http.MethodGet,
			"/dashboards/"+chgDash+"/versions", "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("status %d %s, want 404", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("newest first, with what a restore saves over", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
		mock.ExpectQuery(`FROM dashboard_versions v\s+LEFT JOIN users u ON u\.id = v\.saved_by\s+WHERE v\.dashboard_id = \$1 AND v\.workspace_id = \$2\s+ORDER BY v\.version DESC`).
			WithArgs(chgDash, wsScopeWS, dashboardVersionsMax).
			WillReturnRows(sqlmock.NewRows([]string{"version", "name", "description", "filters", "visibility", "tiles",
				"saved_by", "how", "change_id", "restored_from", "created_at"}).
				AddRow(2, "Revenue", "", []byte(`[]`), "workspace", []byte(`[{"kind":"chart","chart_id":"`+chartID+`","x":0,"y":0,"w":6,"h":7}]`),
					"Asha", dashboardSavedByAsk, chgID, 0, chgBase).
				AddRow(1, "Revenue (old)", "", []byte(`[]`), "private", []byte(`[]`), "Asha", dashboardSavedByEdit, "", 0, chgChartTime))
		w := chartTestCall(wsScopeRouterAsRole(http.MethodGet, route, ListDashboardVersions, "admin"), http.MethodGet,
			"/dashboards/"+chgDash+"/versions", "")
		var got struct {
			Versions []DashboardVersion `json:"versions"`
			Current  struct {
				Visibility string    `json:"visibility"`
				UpdatedAt  time.Time `json:"updated_at"`
			} `json:"current"`
			CanManage bool `json:"can_manage"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
		if len(got.Versions) != 2 || got.Versions[0].Version != 2 || got.Versions[0].How != "ask" || got.Versions[0].ChangeID != chgID ||
			len(got.Versions[0].Tiles) != 1 || got.Versions[1].Tiles == nil {
			t.Fatalf("versions = %s", w.Body.String())
		}
		if !got.CanManage || got.Current.Visibility != "workspace" || !got.Current.UpdatedAt.Equal(chgBase) {
			t.Fatalf("current = %s", w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func saveDashboard(body string) *httptest.ResponseRecorder {
	route := "/dashboards/:id"
	return chartTestCall(wsScopeRouterAsRole(http.MethodPut, route, UpdateDashboard, "admin"), http.MethodPut,
		"/dashboards/"+chgDash, body)
}

// Every save keeps the dashboard as it was before it: an edit as "edit", a restore as
// "restore" naming the version it put back. A restore of a version the dashboard does
// not have is refused before anything is written.
func TestUpdateDashboardKeepsTheVersionBeforeIt(t *testing.T) {
	saved := chgBase.Format(time.RFC3339Nano)
	for _, c := range []struct {
		name     string
		restored int
		how      string
	}{
		{"an edit", 0, dashboardSavedByEdit},
		{"a restore", 2, dashboardSavedByRestore},
	} {
		t.Run(c.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
			var restored any
			if c.restored > 0 {
				restored = c.restored
				mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM dashboard_versions WHERE dashboard_id = \$1 AND workspace_id = \$2 AND version = \$3\)`).
					WithArgs(chgDash, wsScopeWS, c.restored).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			}
			mock.ExpectBegin()
			mock.ExpectExec(`UPDATE dashboards\s+SET name = \$3`).
				WithArgs(chgDash, wsScopeWS, "Revenue", "", "[]", "workspace", chgBase).WillReturnResult(sqlmock.NewResult(0, 1))
			var tiles string
			mock.ExpectExec(`INSERT INTO dashboard_versions`).
				WithArgs(chgDash, wsScopeWS, "Revenue", "", "[]", "workspace", captureJSONArg(&tiles), wsScopeUser, c.how, nil, restored).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`DELETE FROM dashboard_tiles`).WithArgs(chgDash, wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 3))
			mock.ExpectExec(`INSERT INTO dashboard_tiles`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
			expectChangeDashboard(mock, chgBase.Add(time.Second), chgBarSettings, chgChartTime)

			body := `{"name":"Revenue","visibility":"workspace","tiles":[{"kind":"text","body":"Notes","x":0,"y":0,"w":12,"h":3}],` +
				`"updated_at":"` + saved + `","restored_from":` + strconv.Itoa(c.restored) + `}`
			w := saveDashboard(body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d %s", w.Code, w.Body.String())
			}
			// The version is the board before the save: its three tiles, not the one saved.
			var kept []charts.Tile
			if err := json.Unmarshal([]byte(tiles), &kept); err != nil || len(kept) != 3 || kept[0].ChartID != chartID {
				t.Fatalf("kept tiles = %s", tiles)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("a restore of a version it does not have", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectChangeDashboard(mock, chgBase, chgBarSettings, chgChartTime)
		mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM dashboard_versions`).WithArgs(chgDash, wsScopeWS, 9).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		w := saveDashboard(`{"name":"Revenue","visibility":"workspace","tiles":[],"updated_at":"` + saved + `","restored_from":9}`)
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "version_not_found") {
			t.Fatalf("status %d %s, want 422 version_not_found", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// ---------------------------------------------------------------------------
// The layout and the settings a change keeps
// ---------------------------------------------------------------------------

func TestPushTilesDown(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	board := func() []charts.Tile {
		return []charts.Tile{
			{Kind: charts.TileChart, X: 0, Y: 0, W: 6, H: 7},
			{Kind: charts.TileChart, X: 6, Y: 0, W: 6, H: 7},
			{Kind: charts.TileText, X: 0, Y: 7, W: 12, H: 3},
			{Kind: charts.TileText, X: 0, Y: 20, W: 6, H: 3},
		}
	}
	for _, c := range []struct {
		name   string
		resize func(*charts.Tile)
		at     int
		wantX  int
		wantY  []int
		moves  []DashboardChangeMove
	}{
		{"wider: the tile beside it and the one below move down", func(t *charts.Tile) { t.W = 12 }, 0, 0,
			[]int{0, 7, 14, 20}, []DashboardChangeMove{{TileID: "b", FromY: 0, Y: 7}, {TileID: "c", FromY: 7, Y: 14}}},
		{"narrower: nothing moves, and nothing moves up", func(t *charts.Tile) { t.W = 4 }, 0, 0,
			[]int{0, 0, 7, 20}, []DashboardChangeMove{}},
		{"taller: only the tile under it moves", func(t *charts.Tile) { t.H = 10 }, 0, 0,
			[]int{0, 0, 10, 20}, []DashboardChangeMove{{TileID: "c", FromY: 7, Y: 10}}},
		{"wider at the right edge: it moves left to stay on the grid", func(t *charts.Tile) { t.W = 8 }, 1, 4,
			[]int{7, 0, 14, 20}, []DashboardChangeMove{{TileID: "a", FromY: 0, Y: 7}, {TileID: "c", FromY: 7, Y: 14}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tiles := board()
			c.resize(&tiles[c.at])
			out, moves := pushTilesDown(tiles, ids, c.at)
			if out[c.at].X != c.wantX {
				t.Fatalf("x = %d, want %d", out[c.at].X, c.wantX)
			}
			for i, y := range c.wantY {
				if out[i].Y != y {
					t.Fatalf("tile %s y = %d, want %d (%+v)", ids[i], out[i].Y, y, out)
				}
			}
			if c.moves != nil && !slices.Equal(moves, c.moves) {
				t.Fatalf("moves = %+v, want %+v", moves, c.moves)
			}
			for i := range out {
				for j := i + 1; j < len(out); j++ {
					if tilesOverlap(out[i], out[j]) {
						t.Fatalf("tiles %s and %s overlap: %+v", ids[i], ids[j], out)
					}
				}
			}
			if tiles[c.at].X != board()[c.at].X {
				t.Fatalf("the caller's tiles were changed")
			}
		})
	}
}

func TestKeepChartSettings(t *testing.T) {
	type roles = map[string][]charts.Field
	old := func() *charts.Settings {
		return &charts.Settings{Type: "bar", Description: "Revenue by day", Roles: roles{
			charts.RoleX:       {{Column: "day", Title: "Day", Grain: "week"}},
			charts.RoleSeries:  {{Column: "region", Colors: []charts.SeriesColor{{Value: "EU", Color: "series-2"}}}},
			charts.RoleMeasure: {{Column: "amount", Aggregate: "sum", Title: "Revenue", Format: "currency", Color: "series-3"}}}}
	}
	t.Run("same columns keep what the tools cannot set", func(t *testing.T) {
		next := &charts.Settings{Type: "line", Roles: roles{charts.RoleX: {{Column: "day"}},
			charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: {{Column: "amount", Aggregate: "avg"}}}}
		keepChartSettings(old(), next)
		x, sr, m := next.One(charts.RoleX), next.One(charts.RoleSeries), next.One(charts.RoleMeasure)
		if x.Title != "Day" || len(sr.Colors) != 1 || m.Color != "series-3" || next.Description != "Revenue by day" {
			t.Fatalf("next = %+v", next)
		}
		// The tool sets x's grain and the measure's title and format: left out, they are left out.
		if x.Grain != "" || m.Title != "" || m.Format != "" {
			t.Fatalf("kept what the tool sets: %+v %+v", x, m)
		}
	})
	t.Run("a heatmap keeps its y title while its column stays", func(t *testing.T) {
		o := &charts.Settings{Type: "heatmap", Roles: roles{charts.RoleX: {{Column: "hour"}},
			charts.RoleY: {{Column: "dow", Title: "Weekday", Grain: "day"}}, charts.RoleMeasure: {{Aggregate: "count"}}}}
		next := &charts.Settings{Type: "heatmap", Roles: roles{charts.RoleX: {{Column: "hour"}},
			charts.RoleY: {{Column: "dow"}}, charts.RoleMeasure: {{Aggregate: "count"}}}}
		keepChartSettings(o, next)
		if y := next.One(charts.RoleY); y.Title != "Weekday" || y.Grain != "day" {
			t.Fatalf("y = %+v", y)
		}
	})
	t.Run("a new column drops its title and colours; a value sent wins", func(t *testing.T) {
		next := &charts.Settings{Type: "bar", Description: "By week", Roles: roles{charts.RoleX: {{Column: "week"}},
			charts.RoleSeries: {{Column: "country"}}, charts.RoleMeasure: {{Column: "amount", Aggregate: "sum", Color: "series-5"}}}}
		o := old()
		keepChartSettings(o, next)
		x, sr, m := next.One(charts.RoleX), next.One(charts.RoleSeries), next.One(charts.RoleMeasure)
		if x.Title != "" || len(sr.Colors) != 0 || m.Color != "series-5" || next.Description != "By week" {
			t.Fatalf("next = %+v", next)
		}
		sr.Colors = append(sr.Colors, charts.SeriesColor{Value: "x", Color: "series-1"})
		if len(o.One(charts.RoleSeries).Colors) != 1 {
			t.Fatal("the old chart's colours were shared with the new settings")
		}
	})
	t.Run("each measure of a combo keeps its own colour", func(t *testing.T) {
		o := &charts.Settings{Type: "bar", Roles: roles{charts.RoleX: {{Column: "day"}}, charts.RoleMeasure: {
			{Column: "amount", Aggregate: "sum", Color: "series-3", Mark: "bar", Axis: "left"},
			{Aggregate: "count", Color: "series-6", Mark: "line", Axis: "right"}}}}
		next := &charts.Settings{Type: "line", Roles: roles{charts.RoleX: {{Column: "day"}}, charts.RoleMeasure: {
			{Column: "amount", Aggregate: "avg", Mark: "line", Axis: "left"},
			{Column: "amount", Aggregate: "max", Mark: "area", Axis: "right"}}}}
		keepChartSettings(o, next)
		ms := next.Roles[charts.RoleMeasure]
		if ms[0].Color != "series-3" || ms[1].Color != "series-6" {
			t.Fatalf("colours = %q, %q; want series-3, series-6", ms[0].Color, ms[1].Color)
		}
		// The tool sets each mark and axis: what it sent stays.
		if ms[1].Mark != "area" || ms[1].Axis != "right" {
			t.Fatalf("second = %+v", ms[1])
		}
		// A chart that had one measure gives a new second measure nothing.
		one := &charts.Settings{Type: "bar", Roles: roles{charts.RoleX: {{Column: "day"}},
			charts.RoleMeasure: {{Aggregate: "count", Color: "series-3", Mark: "bar", Axis: "left"},
				{Column: "amount", Aggregate: "sum", Mark: "line", Axis: "right"}}}}
		keepChartSettings(old(), one)
		if c := one.Roles[charts.RoleMeasure][1].Color; c != "" {
			t.Fatalf("the second measure took colour %q from the old chart's only measure", c)
		}
	})
	t.Run("a histogram's bins are the tool's", func(t *testing.T) {
		o := &charts.Settings{Type: "histogram", Roles: roles{charts.RoleX: {{Column: "amount", Title: "Amount", Bins: 30}}}}
		next := &charts.Settings{Type: "histogram", Roles: roles{charts.RoleX: {{Column: "amount", Width: 25}}}}
		keepChartSettings(o, next)
		if x := next.One(charts.RoleX); x.Title != "Amount" || x.Bins != 0 || x.Width != 25 {
			t.Fatalf("x = %+v", x)
		}
	})
	t.Run("kept colours are a copy", func(t *testing.T) {
		next := &charts.Settings{Type: "bar", Roles: roles{charts.RoleX: {{Column: "day"}},
			charts.RoleSeries: {{Column: "region"}}, charts.RoleMeasure: {{Aggregate: "count"}}}}
		o := old()
		keepChartSettings(o, next)
		next.One(charts.RoleSeries).Colors[0].Color = "series-7"
		if o.One(charts.RoleSeries).Colors[0].Color != "series-2" {
			t.Fatal("the old chart's colours were shared with the new settings")
		}
	})
}

// A change drafted while its chart was still version 1 reads back at version 2 on both
// sides, so a side still at version 1 does not count as the chart changing.
func TestLoadDashboardChange_ReadsBothSidesAtVersion2(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v2 := charts.AsVersion2(json.RawMessage(chgBarSettings))
	if string(v2) == chgBarSettings {
		t.Fatal("control: chgBarSettings is already version 2")
	}
	side := DashboardChangeSide{Name: "Revenue by day", Type: "bar", W: 6, H: 4}
	before, after := side, side
	before.Settings, after.Settings = json.RawMessage(chgBarSettings), v2
	expectChangeLoad(mock, false, planStatusProposed, false, dashboardChangeBody{DashboardName: "Revenue",
		Before: before, After: after})
	ch, err := loadDashboardChange(context.Background(), db, wsScopeWS, wsScopeUser, chgID, false)
	if err != nil {
		t.Fatal(err)
	}
	if string(ch.Before.Settings) != string(v2) || string(ch.After.Settings) != string(v2) {
		t.Errorf("sides = %s, %s; want both %s", ch.Before.Settings, ch.After.Settings, v2)
	}
	if ch.ChartChanged {
		t.Error("the same chart at version 1 and 2 counts as changed")
	}
}
