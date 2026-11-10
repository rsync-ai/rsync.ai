package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Bug class: a run path that skips a check the Run button makes. The Run
// button, a healer retry, a workflow step and a scheduled tick all start the
// same pipeline run, so they must refuse the same runs. These tests pin each
// unattended path to the button's checks: the billing gates (runQuotaRefusal),
// the draft gate and the Reload-into-an-API refusal.

const (
	gateSrcConn = "33333333-0000-4000-8000-0000000000c1"
	gateDstConn = "33333333-0000-4000-8000-0000000000c2"
	gateCreator = "33333333-0000-4000-8000-0000000000e1"
)

var (
	scheduledRunPipelineCols = []string{"workspace_id", "created_by", "source_connection_id", "destination_connection_id", "default_run_mode"}
	costQuotaCols            = []string{"cost_quota_usd_cents", "current_cents", "monthly_cost_period"}
	draftConnCols            = []string{"connector_type", "connector_version"}
)

func expectScheduledRunPipeline(mock sqlmock.Sqlmock, creator string, defaultMode interface{}) {
	mock.ExpectQuery(regexp.QuoteMeta(scheduledRunPipelineSQL)).
		WithArgs(overlapPipeID).
		WillReturnRows(sqlmock.NewRows(scheduledRunPipelineCols).
			AddRow(overlapWSID, creator, gateSrcConn, gateDstConn, defaultMode))
}

// expectDraftChecks answers the draft gate's lookup of both connections, in
// the pipeline's workspace.
func expectDraftChecks(mock sqlmock.Sqlmock, srcType, dstType string) {
	mock.ExpectQuery(planDraftSQL).WithArgs(gateSrcConn, overlapWSID).
		WillReturnRows(sqlmock.NewRows(draftConnCols).AddRow(srcType, "v1.0.0"))
	mock.ExpectQuery(planDraftSQL).WithArgs(gateDstConn, overlapWSID).
		WillReturnRows(sqlmock.NewRows(draftConnCols).AddRow(dstType, "v1.0.0"))
}

func TestCheckScheduledRun_RefusesWhatTheRunButtonRefuses(t *testing.T) {
	tableCheckToolsDir(t) // acme-api is api_saas, pg-db a database
	past := time.Now().Add(-72 * time.Hour)
	for _, tc := range []struct {
		name       string
		billing    string
		lifecycle  map[string]string
		expect     func(mock sqlmock.Sqlmock)
		wantReason string
		wantMsg    string
		notInMsg   string
	}{
		{
			name:    "the creator is over the LLM spend quota",
			billing: "false",
			expect: func(mock sqlmock.Sqlmock) {
				expectScheduledRunPipeline(mock, gateCreator, nil)
				mock.ExpectQuery(`cost_quota_usd_cents`).WithArgs(gateCreator).
					WillReturnRows(sqlmock.NewRows(costQuotaCols).AddRow(100, 150, "2026-10"))
			},
			wantReason: "quota_exceeded",
			wantMsg:    "Monthly LLM spend quota exceeded",
		},
		{
			name:    "the workspace's trial has ended",
			billing: "true",
			expect: func(mock sqlmock.Sqlmock) {
				expectScheduledRunPipeline(mock, "", nil)
				mock.ExpectQuery(`FROM plans`).WillReturnRows(planCatalogueRows())
				mock.ExpectQuery(`FROM workspaces w`).WithArgs(overlapWSID).
					WillReturnRows(sqlmock.NewRows([]string{"plan", "plan_expires_at", "pipeline_limit_override"}).
						AddRow("free", past, nil))
			},
			wantReason: "trial_expired",
			wantMsg:    "Your free trial has ended",
		},
		{
			name:    "the workspace has used its monthly GB",
			billing: "true",
			expect: func(mock sqlmock.Sqlmock) {
				expectScheduledRunPipeline(mock, "", nil)
				for i := 0; i < 2; i++ { // the can_run gate, then the GB gate
					mock.ExpectQuery(`FROM plans`).WillReturnRows(planCatalogueRows())
					mock.ExpectQuery(`FROM workspaces w`).WithArgs(overlapWSID).
						WillReturnRows(workspacePlanRow("starter"))
				}
				mock.ExpectQuery(`monthly_transfer_bytes`).WithArgs(overlapWSID).
					WillReturnRows(sqlmock.NewRows([]string{"bytes"}).AddRow(int64(150e9)))
			},
			wantReason: "gb_limit_reached",
			wantMsg:    "100 GB of data transfer",
		},
		{
			name:      "the source connector is a draft",
			billing:   "false",
			lifecycle: map[string]string{"pg-db@v1.0.0": "draft"},
			expect: func(mock sqlmock.Sqlmock) {
				expectScheduledRunPipeline(mock, "", nil)
				mock.ExpectQuery(planDraftSQL).WithArgs(gateSrcConn, overlapWSID).
					WillReturnRows(sqlmock.NewRows(draftConnCols).AddRow("pg-db", "v1.0.0"))
			},
			wantReason: "draft_connector_blocked",
			wantMsg:    "Test the connection first",
			// A scheduled run has no ?allow_draft=true to pass.
			notInMsg: "allow_draft",
		},
		{
			name:      "the default run mode is a Reload into an app's API",
			billing:   "false",
			lifecycle: map[string]string{"pg-db@v1.0.0": "ga", "acme-api@v1.0.0": "ga"},
			expect: func(mock sqlmock.Sqlmock) {
				expectScheduledRunPipeline(mock, "", "reload")
				expectDraftChecks(mock, "pg-db", "acme-api")
				mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1`).WithArgs(gateDstConn).
					WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("acme-api"))
			},
			wantReason: "reload_not_supported",
			wantMsg:    "default run mode is Reload",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RSYNC_BILLING_ENFORCED", tc.billing)
			seedLifecycle(t, tc.lifecycle)
			sqlDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer sqlDB.Close()
			tc.expect(mock)

			got, err := checkScheduledRun(context.Background(), sqlDB, overlapPipeID)
			if err != nil {
				t.Fatalf("checkScheduledRun: %v", err)
			}
			if got.Allowed || got.Reason != tc.wantReason {
				t.Fatalf("check = %+v, want refused with %s", got, tc.wantReason)
			}
			if !strings.Contains(got.Message, tc.wantMsg) {
				t.Fatalf("message = %q, want it to contain %q", got.Message, tc.wantMsg)
			}
			if tc.notInMsg != "" && strings.Contains(got.Message, tc.notInMsg) {
				t.Fatalf("message = %q, must not mention %q", got.Message, tc.notInMsg)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("db expectations: %v", err)
			}
		})
	}
}

func TestCheckScheduledRun_AllowsARunTheButtonWouldStart(t *testing.T) {
	tableCheckToolsDir(t)
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	seedLifecycle(t, map[string]string{"pg-db@v1.0.0": "ga", "acme-api@v1.0.0": "preview"})
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	// An unmetered creator, live connectors, and a plain (resume) run into an
	// API: nothing to refuse, and the destination type is never looked up.
	expectScheduledRunPipeline(mock, gateCreator, nil)
	mock.ExpectQuery(`cost_quota_usd_cents`).WithArgs(gateCreator).
		WillReturnRows(sqlmock.NewRows(costQuotaCols).AddRow(0, 0, ""))
	expectDraftChecks(mock, "pg-db", "acme-api")

	got, err := checkScheduledRun(context.Background(), sqlDB, overlapPipeID)
	if err != nil {
		t.Fatalf("checkScheduledRun: %v", err)
	}
	if !got.Allowed || got.Reason != "" || got.Message != "" {
		t.Fatalf("check = %+v, want allowed", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func TestCheckScheduledRun_DeletedPipelineAndUnreadablePipeline(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	mock.ExpectQuery(regexp.QuoteMeta(scheduledRunPipelineSQL)).WithArgs(overlapPipeID).
		WillReturnError(sql.ErrNoRows)
	got, err := checkScheduledRun(context.Background(), sqlDB, overlapPipeID)
	if err != nil || got.Allowed || got.Reason != "pipeline_not_found" {
		t.Fatalf("deleted pipeline: check = %+v, err = %v; want refused pipeline_not_found", got, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(scheduledRunPipelineSQL)).WithArgs(overlapPipeID).
		WillReturnError(errors.New("connection reset"))
	if _, err := checkScheduledRun(context.Background(), sqlDB, overlapPipeID); err == nil {
		t.Fatal("an unreadable pipeline must be an error, not an answer")
	}
}

// The adapter's scheduled wrapper asks this endpoint before each tick. It
// answers 200 with the decision either way; only a failure to decide is 500.
func TestCheckScheduledRunInternal_AnswersTheDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("RSYNC_BILLING_ENFORCED", "true")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	r := gin.New()
	r.POST("/api/v1/internal/pipelines/:id/scheduled-run/check", CheckScheduledRunInternal)
	call := func(id string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/internal/pipelines/"+id+"/scheduled-run/check", nil))
		return rr
	}

	expectScheduledRunPipeline(mock, "", nil)
	mock.ExpectQuery(`FROM plans`).WillReturnRows(planCatalogueRows())
	mock.ExpectQuery(`FROM workspaces w`).WithArgs(overlapWSID).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "plan_expires_at", "pipeline_limit_override"}).
			AddRow("free", time.Now().Add(-72*time.Hour), nil))
	rr := call(overlapPipeID)
	if rr.Code != http.StatusOK {
		t.Fatalf("refused: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody(t, rr)
	if body["allowed"] != false || body["reason"] != "trial_expired" {
		t.Fatalf("refused: body = %v, want allowed=false reason=trial_expired", body)
	}

	mock.ExpectQuery(regexp.QuoteMeta(scheduledRunPipelineSQL)).WithArgs(overlapPipeID).
		WillReturnError(errors.New("connection reset"))
	if rr := call(overlapPipeID); rr.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable: status = %d, want 500: %s", rr.Code, rr.Body.String())
	}

	if rr := call("not-a-uuid"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad id: status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// Sibling of the class: a refusal that comes after the run is written. The
// draft gate used to sit after the pipeline was flipped to running and the
// execution row was inserted, so a refused Run left both behind. sqlmock fails
// any statement it was not told to expect, so no UPDATE or INSERT runs here.
func TestStartPipelineRun_DraftRefusalWritesNothing(t *testing.T) {
	tableCheckToolsDir(t)
	seedLifecycle(t, map[string]string{"pg-db@v1.0.0": "ga", "acme-api@v1.0.0": "draft"})
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	_, cancelled := stubRunWorkflows(t, nil)

	expectRunPipelineRow(mock)
	expectOpenRuns(mock, sqlmock.NewRows(openRunColumns))
	expectConnectorTypes(mock)
	expectDraftChecks(mock, "pg-db", "acme-api")

	_, ref := startPipelineRun(context.Background(), sqlDB, overlapPipeID, overlapWSID, "", RunPipelineRequest{},
		pipelineRunOpts{DraftGate: true, DraftWorkspaceID: overlapWSID})
	if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body["error"] != "draft_connector_blocked" {
		t.Fatalf("refusal = %+v, want 422 draft_connector_blocked", ref)
	}
	if len(*cancelled) != 0 {
		t.Fatalf("a refused run cancelled %v", *cancelled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// Sibling of the class: the healer's retry (RunPipelineInternal) has no active
// workspace in its request, so the draft gate looked the connections up in
// workspace "" — found nothing, and let a draft connector run. It must look
// them up in the pipeline's workspace.
func TestEnqueuePipelineRun_DraftGateUsesThePipelinesWorkspace(t *testing.T) {
	tableCheckToolsDir(t)
	seedLifecycle(t, map[string]string{"pg-db@v1.0.0": "draft", "acme-api@v1.0.0": "ga"})
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	stubRunWorkflows(t, nil)

	expectRunPipelineRow(mock)
	expectOpenRuns(mock, sqlmock.NewRows(openRunColumns))
	expectConnectorTypes(mock)
	mock.ExpectQuery(planDraftSQL).WithArgs(gateSrcConn, overlapWSID).
		WillReturnRows(sqlmock.NewRows(draftConnCols).AddRow("pg-db", "v1.0.0"))

	c, rr := overlapContext() // no active workspace, as on the internal route
	enqueuePipelineRun(c, sqlDB, overlapPipeID, overlapWSID, "", RunPipelineRequest{RunMode: "resume"})
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["error"] != "draft_connector_blocked" {
		t.Fatalf("body = %v, want draft_connector_blocked", body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

func expectRunPipelineRow(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT name, status, source_connection_id`).
		WithArgs(overlapPipeID, overlapWSID).
		WillReturnRows(sqlmock.NewRows([]string{
			"name", "status", "source_connection_id", "destination_connection_id",
			"natural_language_request", "created_by", "config", "dataset", "default_run_mode",
		}).AddRow("orders to app", "idle", gateSrcConn, gateDstConn, nil, nil, []byte(`{}`), nil, nil))
}

func expectConnectorTypes(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1`).WithArgs(gateSrcConn).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("pg-db"))
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1`).WithArgs(gateDstConn).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("acme-api"))
}

// B5: a refused scheduled tick writes no execution row, so the schedule says
// why it is producing no runs, the way it already does for a parked run.
func TestScheduleBlockedReason_SaysWhyTheGatesRefuseTheNextTick(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "true")
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	mock.ExpectQuery(`FROM pipeline_progress`).WithArgs(overlapPipeID).WillReturnError(sql.ErrNoRows)
	expectScheduledRunPipeline(mock, "", nil)
	mock.ExpectQuery(`FROM plans`).WillReturnRows(planCatalogueRows())
	mock.ExpectQuery(`FROM workspaces w`).WithArgs(overlapWSID).
		WillReturnRows(sqlmock.NewRows([]string{"plan", "plan_expires_at", "pipeline_limit_override"}).
			AddRow("free", time.Now().Add(-72*time.Hour), nil))

	got := scheduleBlockedReason(sqlDB, overlapPipeID)
	want := "Scheduled runs are skipped: Your free trial has ended."
	if !strings.HasPrefix(got, want) {
		t.Fatalf("blocked reason = %q, want it to start %q", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A schedule the gates allow and with tables selected shows no banner; a gate
// check that cannot run shows none either (an unknown state is not a refusal).
func TestScheduleBlockedReason_NoBannerWhenTheGatesAllowOrCannotTell(t *testing.T) {
	tableCheckToolsDir(t)
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	seedLifecycle(t, map[string]string{"pg-db@v1.0.0": "ga", "acme-api@v1.0.0": "ga"})
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()
	selected := sqlmock.NewRows([]string{"config"}).AddRow([]byte(`{"selected_tables":["orders"]}`))

	mock.ExpectQuery(`FROM pipeline_progress`).WithArgs(overlapPipeID).WillReturnError(sql.ErrNoRows)
	expectScheduledRunPipeline(mock, "", nil)
	expectDraftChecks(mock, "pg-db", "acme-api")
	mock.ExpectQuery(`SELECT config FROM pipelines`).WithArgs(overlapPipeID).WillReturnRows(selected)
	if got := scheduleBlockedReason(sqlDB, overlapPipeID); got != "" {
		t.Fatalf("allowed: blocked reason = %q, want none", got)
	}

	mock.ExpectQuery(`FROM pipeline_progress`).WithArgs(overlapPipeID).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(scheduledRunPipelineSQL)).WithArgs(overlapPipeID).
		WillReturnError(errors.New("connection reset"))
	mock.ExpectQuery(`SELECT config FROM pipelines`).WithArgs(overlapPipeID).
		WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow([]byte(`{"selected_tables":["orders"]}`)))
	if got := scheduleBlockedReason(sqlDB, overlapPipeID); got != "" {
		t.Fatalf("unknown: blocked reason = %q, want none", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}
