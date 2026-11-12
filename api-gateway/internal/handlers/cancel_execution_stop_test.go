package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Cancel on a run marks its pipeline stopped, so it must stop what Stop stops
// (StopPipeline, pipelines_stop_status_test.go). On a CDC pipeline it flipped
// the row to "stopped" while the connector and sink kept streaming; on a batch
// run it left the sink worker writing the cancelled run's backlog.
//
// Harness helpers (wsScopeMockDB, wsScopeRouterAsRole, wsScopeExecution,
// wsScopePipeline, wsScopeWS, expectMemberRoleGate) come from the workspace
// scoping tests in this package.

const cancelCDCFlagSQL = `SELECT \(LOWER\(COALESCE\(TRIM\(p\.sync_mode\).* FROM pipelines p WHERE p\.id = \$1 AND p\.workspace_id = \$2`

func cancelHarness(t *testing.T, isCDC bool, orchestratorStatus int) (sqlmock.Sqlmock, *[]string, func()) {
	t.Helper()
	var calls []string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(orchestratorStatus)
		_, _ = w.Write([]byte(`{"success":` + map[bool]string{true: "true", false: "false"}[orchestratorStatus < 300] + `,"warnings":["w1"]}`))
	}))
	t.Setenv("ORCHESTRATOR_URL", orch.URL)

	mock, cleanup := wsScopeMockDB(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT e\.status, e\.pipeline_id`).
		WithArgs(wsScopeExecution, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "pipeline_id"}).AddRow("running", wsScopePipeline))
	expectMemberRoleGate(mock)
	mock.ExpectQuery(cancelCDCFlagSQL).
		WithArgs(wsScopePipeline, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"is_cdc"}).AddRow(isCDC))
	return mock, &calls, func() { cleanup(); orch.Close() }
}

func expectCancelWrites(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`UPDATE executions SET status = 'cancelled'`).
		WithArgs(wsScopeExecution).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipeline_progress`).
		WithArgs(wsScopePipeline, wsScopeExecution).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE pipelines SET status = 'stopped'`).
		WithArgs(wsScopePipeline, wsScopeWS).WillReturnResult(sqlmock.NewResult(0, 1))
}

func serveCancel(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := wsScopeRouterAsRole(http.MethodPost, "/executions/:id/cancel", CancelExecution, "member")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/executions/"+wsScopeExecution+"/cancel", nil))
	return w
}

func TestCancelExecution_CDCStopsTheConnectorAndClosesEveryExecution(t *testing.T) {
	mock, calls, cleanup := cancelHarness(t, true, http.StatusOK)
	defer cleanup()
	expectCancelWrites(mock)
	// The stream row (id = pipeline id) is closed along with the cancelled run.
	mock.ExpectExec(`UPDATE executions[\s\S]*WHERE pipeline_id = \$1 AND status IN \('running','pending'\)`).
		WithArgs(wsScopePipeline).WillReturnResult(sqlmock.NewResult(0, 1))

	w := serveCancel(t)
	want := "PUT /api/v1/cdc/pipelines/" + wsScopePipeline + "/stop"
	if len(*calls) != 1 || (*calls)[0] != want {
		t.Fatalf("orchestrator calls = %v; want exactly [%s] — Cancel marks the pipeline stopped, so its connector must stop too", *calls, want)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "w1") {
		t.Fatalf("stop warnings not passed on: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestCancelExecution_CDCConnectorRefusedChangesNothing(t *testing.T) {
	mock, calls, cleanup := cancelHarness(t, true, http.StatusBadGateway)
	defer cleanup()
	// No UPDATE is expected: sqlmock fails any statement it was not told about.

	w := serveCancel(t)
	if len(*calls) != 1 {
		t.Fatalf("orchestrator calls = %v; want the connector stop attempted", *calls)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s; want the orchestrator's 502, with the run and pipeline untouched", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestCancelExecution_BatchStopsTheSinkWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"sink stopped", http.StatusOK, "w1"},
		{"sink stop failed: still cancelled, with a warning", http.StatusInternalServerError, "sink worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, calls, cleanup := cancelHarness(t, false, tc.status)
			defer cleanup()
			expectCancelWrites(mock)

			w := serveCancel(t)
			want := "PUT /api/v1/pipelines/" + wsScopePipeline + "/sink/stop"
			if len(*calls) != 1 || (*calls)[0] != want {
				t.Fatalf("orchestrator calls = %v; want exactly [%s] — the batch sink worker keeps draining after Cancel", *calls, want)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("response %s does not carry %q", w.Body.String(), tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// Cancel stops the pipeline as Stop does, so it tells the owner as Stop does,
// every time. Unlike Stop it always does: CancelWorkflow ends the run without
// the cancel signal its handler reports, so nothing else is sure to say it. A
// batch notice is keyed by the cancelled run, which is how the adapter keys its
// own PIPELINE_RUN_STOPPED, so the two collapse into one if both are raised.
func TestCancelExecution_CancelTellsTheOwnerForBatchAndCDC(t *testing.T) {
	cases := []struct {
		name        string
		isCDC       bool
		wantCode    string
		wantSubject func(string) bool
	}{
		{"batch", false, "PIPELINE_RUN_STOPPED", func(s string) bool { return s == wsScopeExecution }},
		{"cdc", true, "CDC_PIPELINE_STOPPED", func(s string) bool { return strings.HasPrefix(s, "user-stop|") }},
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, _, cleanup := cancelHarness(t, tc.isCDC, http.StatusOK)
			defer cleanup()
			expectCancelWrites(mock)
			if tc.isCDC {
				mock.ExpectExec(`UPDATE executions[\s\S]*WHERE pipeline_id = \$1 AND status IN \('running','pending'\)`).
					WithArgs(wsScopePipeline).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			rec := captureRunNotices(t)

			w := serveCancel(t)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
			sent := rec.notices(t)
			if len(sent) != 1 {
				t.Fatalf("stop notices sent = %d, want exactly 1", len(sent))
			}
			env := noticeError(t, sent[0])
			if env["code"] != tc.wantCode {
				t.Errorf("code = %v, want %s", env["code"], tc.wantCode)
			}
			if s, _ := env["dedup_subject"].(string); !tc.wantSubject(s) {
				t.Errorf("dedup_subject = %q", s)
			}
			if sent[0]["pipeline_id"] != wsScopePipeline || sent[0]["action_url"] != "/pipelines/"+wsScopePipeline {
				t.Errorf("pipeline_id = %v, action_url = %v; want the cancelled run's pipeline", sent[0]["pipeline_id"], sent[0]["action_url"])
			}
			assertMetadataOnly(t, sent[0], "pipeline_id")
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

// A Cancel whose connector stop was refused changed nothing, so it tells nobody.
func TestCancelExecution_CDCConnectorRefusedTellsNobody(t *testing.T) {
	mock, _, cleanup := cancelHarness(t, true, http.StatusBadGateway)
	defer cleanup()
	rec := captureRunNotices(t)

	w := serveCancel(t)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if sent := rec.notices(t); len(sent) != 0 {
		t.Fatalf("stop notices sent = %d, want 0 for a cancel that did not happen", len(sent))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
