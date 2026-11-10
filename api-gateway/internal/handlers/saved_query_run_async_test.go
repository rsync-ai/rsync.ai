package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// stubLaunch replaces the background launch with a recorder for one test.
func stubLaunch(t *testing.T) *[]string {
	t.Helper()
	var launched []string
	prev := launchModelRun
	launchModelRun = func(_ *sql.DB, id, userID string) { launched = append(launched, id+"/"+userID) }
	t.Cleanup(func() { launchModelRun = prev })
	return &launched
}

func expectRunnableModel(mock sqlmock.Sqlmock, materialization, sqlText, runAsRole string) {
	mock.ExpectQuery(`FROM saved_queries sq\s+JOIN connections c`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "connection_id", "name", "sql_text",
			"materialization", "target_table", "target_owned", "connector_type", "config",
		}).AddRow(savedQueryID, wsScopeWS, savedQueryConn, "Daily MRR", sqlText,
			materialization, "public.daily_mrr", false, "postgresql", "{}"))
	if materialization == matNone {
		return
	}
	mock.ExpectQuery(`SELECT role FROM workspace_members`).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(runAsRole))
}

// Everything that can be refused is refused before the 202, so "started" always means
// a run reached the background — and a refusal never does.
func TestStartSavedQueryModelRun(t *testing.T) {
	cases := []struct {
		name            string
		materialization string
		sqlText         string
		runAsRole       string
		status          int
		launched        bool
	}{
		{"a table model starts", matTable, "SELECT 1", "admin", http.StatusAccepted, true},
		{"not a model", matNone, "SELECT 1", "", http.StatusBadRequest, false},
		{"a write query in table mode", matTable, "DELETE FROM orders", "owner", http.StatusBadRequest, false},
		{"run-as user below admin", matTable, "SELECT 1", "member", http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launched := stubLaunch(t)
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectMaterializationPreamble(mock)
			expectRunnableModel(mock, tc.materialization, tc.sqlText, tc.runAsRole)

			r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/run-async", "admin", StartSavedQueryModelRun)
			w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/run-async", nil)
			if w.Code != tc.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if got := len(*launched) == 1; got != tc.launched {
				t.Fatalf("launched = %v, want %v", *launched, tc.launched)
			}
			if tc.launched {
				if (*launched)[0] != savedQueryID+"/"+wsScopeUser {
					t.Fatalf("launched %q, want the model as the caller", (*launched)[0])
				}
				var resp map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if resp["status"] != "started" || resp["model_id"] != savedQueryID {
					t.Fatalf("response = %v", resp)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
