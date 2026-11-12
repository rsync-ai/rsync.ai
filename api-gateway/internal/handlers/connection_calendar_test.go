package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A connection's calendar (migration 147, decision 14): the first day of its week and the
// first month of its fiscal year, which a chart's week, quarter and year grains start on.

func TestUpdateConnection_WritesTheCalendar(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT wm\.role\s+FROM connections r`).
		WithArgs(wsScopeConnection, wsScopeUser, wsScopeWS).
		WillReturnRows(gateRoleRows("member"))
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(wsScopeConnection, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("postgresql"))
	// A calendar edit is no config change: no connectivity test, no table check.
	mock.ExpectExec(`UPDATE connections SET week_start = \$1, fiscal_year_start_month = \$2, updated_at = \$3 WHERE id = \$4 AND workspace_id = \$5`).
		WithArgs(7, 4, sqlmock.AnyArg(), wsScopeConnection, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{"week_start": 7, "fiscal_year_start_month": 4})
	r := wsScopeRouter(http.MethodPatch, "/api/v1/connections/:id", UpdateConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/connections/"+wsScopeConnection, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A day or a month out of range is a 400 that names the field, before the database.
func TestUpdateConnection_RefusesACalendarOutOfRange(t *testing.T) {
	for name, tc := range map[string]struct {
		body map[string]any
		code string
	}{
		"week 0":  {map[string]any{"week_start": 0}, "invalid_week_start"},
		"week 8":  {map[string]any{"week_start": 8}, "invalid_week_start"},
		"month 0": {map[string]any{"fiscal_year_start_month": 0}, "invalid_fiscal_year_start_month"},
		"month 13": {map[string]any{"name": "ok", "fiscal_year_start_month": 13},
			"invalid_fiscal_year_start_month"},
	} {
		t.Run(name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			body, _ := json.Marshal(tc.body)
			r := wsScopeRouter(http.MethodPatch, "/api/v1/connections/:id", UpdateConnection)
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/connections/"+wsScopeConnection, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || decodeBody(t, w)["error"] != tc.code {
				t.Fatalf("got %d %s, want 400 %s", w.Code, w.Body.String(), tc.code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// The chart's SQL reads the connection's calendar for both kinds of source.
func TestChartSourceFor_CarriesTheConnectionsCalendar(t *testing.T) {
	for _, s := range []chartSourceRow{
		{ConnectorType: "postgresql", Materialization: matNone, SQLText: "SELECT 1", WeekStart: 7, FiscalMonth: 4},
		{ConnectorType: "postgresql", Materialization: matTable, TargetTable: "analytics.daily", WeekStart: 7, FiscalMonth: 4},
	} {
		src, err := chartSourceFor(&s)
		if err != nil {
			t.Fatal(err)
		}
		if src.WeekStart != 7 || src.FiscalMonth != 4 {
			t.Errorf("%s: calendar = %d, %d; want 7, 4", s.Kind(), src.WeekStart, src.FiscalMonth)
		}
	}
}
