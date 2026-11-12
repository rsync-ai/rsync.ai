package handlers

import (
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// An API destination (category api_saas) is reverse ETL: no Reload. Every way a
// Reload is asked for — the button, a body run_mode, the pipeline's default — is
// refused; a plain run and every other destination are not.
func TestAPIDestinationReloadRefusal_OnlyAReloadIntoAnAPI(t *testing.T) {
	tableCheckToolsDir(t) // acme-api is api_saas, pg-db a database
	if destinationHasReload("acme-api") || !destinationHasReload("pg-db") || !destinationHasReload("") {
		t.Fatalf("destinationHasReload: an API has none, a database and an unknown one do")
	}
	for _, tc := range []struct {
		runMode, destType string
		fromDefault       bool
		refused           bool
	}{
		{"reload", "acme-api", false, true},
		{"reload", "acme-api", true, true},
		{"resume", "acme-api", false, false},
		{"reload", "pg-db", false, false},
		{"reload", "no-such-connector", false, false},
		{"reload", "", false, false},
	} {
		got := apiDestinationReloadRefusal(tc.runMode, tc.destType, tc.fromDefault)
		if (got != nil) != tc.refused {
			t.Fatalf("%s into %q: refused = %v, want %v", tc.runMode, tc.destType, got != nil, tc.refused)
		}
		if got == nil {
			continue
		}
		if got.status != http.StatusUnprocessableEntity || got.body["error"] != "reload_not_supported" {
			t.Fatalf("refusal = %d %v, want 422 reload_not_supported", got.status, got.body)
		}
		msg, _ := got.body["message"].(string)
		if !strings.Contains(msg, "Run it without Reload") {
			t.Fatalf("message = %q, want it to say how to run instead", msg)
		}
		if strings.Contains(msg, "default run mode") != tc.fromDefault {
			t.Fatalf("message = %q, names the default = %v, want %v", msg, !tc.fromDefault, tc.fromDefault)
		}
	}
}

// The refusal sits in startPipelineRun, the one path the Run button, a workflow
// step and the agent share, and comes before anything is written: sqlmock fails
// any statement it was not told to expect, so the pipeline row, the execution row
// and pipeline_progress are provably untouched.
func TestEnqueuePipelineRun_RefusesAReloadIntoAnAPIDestination(t *testing.T) {
	tableCheckToolsDir(t)
	const destConn = "22222222-0000-4000-8000-0000000000d1"
	for _, tc := range []struct {
		name          string
		bodyRunMode   string
		defaultMode   interface{}
		wantInMessage string
	}{
		{"asked for", "reload", nil, "Run it without Reload"},
		{"the pipeline's default", "", "reload", "default run mode is Reload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer sqlDB.Close()
			_, cancelled := stubRunWorkflows(t, nil)

			mock.ExpectQuery(`SELECT name, status, source_connection_id`).
				WithArgs(overlapPipeID, overlapWSID).
				WillReturnRows(sqlmock.NewRows([]string{
					"name", "status", "source_connection_id", "destination_connection_id",
					"natural_language_request", "created_by", "config", "dataset", "default_run_mode",
				}).AddRow("orders to app", "idle", nil, destConn, nil, nil, []byte(`{}`), nil, tc.defaultMode))
			expectOpenRuns(mock, sqlmock.NewRows(openRunColumns))
			mock.ExpectQuery(`SELECT connector_type FROM connections`).
				WithArgs(destConn).
				WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("acme-api"))

			c, rr := overlapContext()
			enqueuePipelineRun(c, sqlDB, overlapPipeID, overlapWSID, "", RunPipelineRequest{RunMode: tc.bodyRunMode})

			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
			}
			body := decodeBody(t, rr)
			if body["error"] != "reload_not_supported" {
				t.Fatalf("body = %v, want reload_not_supported", body)
			}
			if msg, _ := body["message"].(string); !strings.Contains(msg, tc.wantInMessage) {
				t.Fatalf("message = %q, want %q", msg, tc.wantInMessage)
			}
			if len(*cancelled) != 0 {
				t.Fatalf("a refused run cancelled %v", *cancelled)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}
