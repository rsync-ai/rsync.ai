package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/notifier"

	"github.com/DATA-DOG/go-sqlmock"
)

// A data check's alert (data-checks-design.md § 7): CHECK_BLOCKED when a check held the
// table back, CHECK_WARNED when a warning check failed or could not run, nothing when
// every check passed. It names the model, each check that did not pass, its type, the
// count it saw and what it expected, and links to the run's results.

func decodeModelCheckAlert(t *testing.T, raw []byte) (map[string]interface{}, map[string]interface{}) {
	t.Helper()
	if raw == nil {
		t.Fatal("no alert, want one")
	}
	var p map[string]interface{}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("alert is not JSON: %v", err)
	}
	env, _ := p["error"].(map[string]interface{})
	if env == nil {
		t.Fatalf("alert has no error envelope: %s", raw)
	}
	return p, env
}

func TestModelCheckAlert_ABlockedRunAlertsCheckBlocked(t *testing.T) {
	res := &modelRunResult{Status: "failed", Error: modelCheckBlockedError, CheckBlocked: true,
		CheckResults: []modelCheckResult{
			{Name: "has rows", Type: checkRowCount, Severity: checkSeverityBlock, Status: checkStatusFail,
				Observed: i64(0), Threshold: "at least 1 row"},
			{Name: "emails present", Type: checkNotNull, Severity: checkSeverityBlock, Status: checkStatusError,
				Message: `pq: column "email" does not exist`},
			{Name: "ids unique", Type: checkUnique, Severity: checkSeverityBlock, Status: checkStatusPass,
				Observed: i64(0), Threshold: "no duplicates"},
		}}

	p, env := decodeModelCheckAlert(t, modelCheckAlertPayload(savedQueryID, provRunB, res))
	if p["type"] != "data_check" || p["saved_query_id"] != savedQueryID {
		t.Errorf("alert = %v, want a data_check about the model", p)
	}
	if _, ok := p["pipeline_id"]; ok {
		t.Errorf("a model's alert names a pipeline: %v", p)
	}
	if want := "/models/" + savedQueryID + "?tab=checks&run=" + provRunB; p["action_url"] != want {
		t.Errorf("action_url = %v, want %s", p["action_url"], want)
	}
	if env["code"] != notifier.CodeCheckBlocked || env["severity"] != "error" ||
		env["audience"] != "user" || env["dedup_subject"] != provRunB {
		t.Errorf("envelope = %v", env)
	}
	msg, _ := p["message"].(string)
	for _, want := range []string{
		`"has rows" (row_count) failed: found 0, expected at least 1 row`,
		`"emails present" (not_null) could not run`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	// A check that passed is not news; an engine's error text stays on the Checks tab,
	// where the person who can read it is already looking.
	if strings.Contains(msg, "ids unique") || strings.Contains(msg, "does not exist") {
		t.Errorf("message %q names a passing check or carries the engine's error", msg)
	}
	if env["user_message"] != msg {
		t.Errorf("user_message = %v, want the message", env["user_message"])
	}
}

func TestModelCheckAlert_AFailedWarningAlertsCheckWarned(t *testing.T) {
	res := &modelRunResult{Status: "succeeded",
		CheckResults: []modelCheckResult{
			{Name: "steady count", Type: checkRowCountChange, Severity: checkSeverityWarn, Status: checkStatusFail,
				Observed: i64(400), Threshold: "drop at most 50% (previous count 1,000)", Message: "fell 60% from 1,000"},
		}}
	p, env := decodeModelCheckAlert(t, modelCheckAlertPayload(savedQueryID, provRunB, res))
	if env["code"] != notifier.CodeCheckWarned || env["severity"] != "warning" {
		t.Errorf("envelope = %v, want CHECK_WARNED at warning", env)
	}
	want := `"steady count" (row_count_change) failed: found 400, expected drop at most 50% (previous count 1,000) (fell 60% from 1,000)`
	if msg, _ := p["message"].(string); !strings.Contains(msg, want) {
		t.Errorf("message %q does not say %q", msg, want)
	}
}

func TestModelCheckAlert_NothingToSayIsNoAlert(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *modelRunResult
	}{
		{"no result", nil},
		{"no checks", &modelRunResult{Status: "succeeded"}},
		{"a build that failed before any check", &modelRunResult{Status: "failed", Error: "syntax error"}},
		{"every check passed", &modelRunResult{Status: "succeeded", CheckResults: []modelCheckResult{
			{Name: "has rows", Type: checkRowCount, Severity: checkSeverityBlock, Status: checkStatusPass, Observed: i64(9)},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if raw := modelCheckAlertPayload(savedQueryID, provRunB, tc.res); raw != nil {
				t.Errorf("alert = %s, want none", raw)
			}
		})
	}
}

// Twenty checks can fail at once; the alert names a few and counts the rest.
func TestModelCheckAlert_NamesAtMostThreeChecks(t *testing.T) {
	res := &modelRunResult{Status: "failed", Error: modelCheckBlockedError, CheckBlocked: true}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		res.CheckResults = append(res.CheckResults, modelCheckResult{Name: n, Type: checkNotNull,
			Severity: checkSeverityBlock, Status: checkStatusFail, Observed: i64(1), Threshold: "no NULLs"})
	}
	p, _ := decodeModelCheckAlert(t, modelCheckAlertPayload(savedQueryID, provRunB, res))
	msg, _ := p["message"].(string)
	if !strings.Contains(msg, `"c" (not_null)`) || strings.Contains(msg, `"d" (not_null)`) || !strings.Contains(msg, "and 2 more") {
		t.Errorf("message = %q, want three checks named and 2 more counted", msg)
	}
}

// A run with nowhere to link still alerts: losing the run's bookkeeping must not also
// lose the one message that says the table was held back.
func TestModelCheckAlert_WithoutARunRowStillAlerts(t *testing.T) {
	res := &modelRunResult{Status: "failed", Error: modelCheckBlockedError, CheckBlocked: true,
		CheckResults: []modelCheckResult{{Name: "has rows", Type: checkRowCount, Severity: checkSeverityBlock,
			Status: checkStatusFail, Observed: i64(0), Threshold: "at least 1 row"}}}
	p, _ := decodeModelCheckAlert(t, modelCheckAlertPayload(savedQueryID, "", res))
	if want := "/models/" + savedQueryID + "?tab=checks"; p["action_url"] != want {
		t.Errorf("action_url = %v, want %s", p["action_url"], want)
	}
}

type capturePublisher struct{ got chan []byte }

func (c capturePublisher) Publish(_ context.Context, raw []byte) error {
	c.got <- raw
	return nil
}

func TestRecordModelRunOutcome_AlertsAfterTheRunIsCommitted(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)
	pub := capturePublisher{got: make(chan []byte, 1)}
	SetModelCheckNotifier(pub)
	t.Cleanup(func() { SetModelCheckNotifier(nil) })

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	mock.ExpectExec(`INSERT INTO saved_query_check_results`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "failed", StatementClass: "read", TargetTable: "analytics.orders",
			Error: modelCheckBlockedError, CheckBlocked: true,
			CheckResults: []modelCheckResult{{Name: "has rows", Type: checkRowCount, Severity: checkSeverityBlock,
				Status: checkStatusFail, Observed: i64(0), Threshold: "at least 1 row"}}},
		modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule, ActorID: provActor})

	select {
	case raw := <-pub.got:
		_, env := decodeModelCheckAlert(t, raw)
		if env["code"] != notifier.CodeCheckBlocked || env["dedup_subject"] != provRunB {
			t.Errorf("envelope = %v, want CHECK_BLOCKED about run %s", env, provRunB)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked run raised no alert")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// The message sits above its code's Impact line in the bell, Slack and email. It said
// which table is live as well, so a warned run read "the new table is live" twice.
func TestModelCheckAlert_SaysWhichTableIsLiveOnce(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		res := &modelRunResult{Status: "succeeded", CheckBlocked: blocked,
			CheckResults: []modelCheckResult{{Name: "has rows", Type: checkRowCount, Severity: checkSeverityWarn,
				Status: checkStatusFail, Observed: i64(0), Threshold: "at least 1 row"}}}
		p, env := decodeModelCheckAlert(t, modelCheckAlertPayload(savedQueryID, provRunB, res))
		code, _ := env["code"].(string)
		impact := notifier.RenderStored(code, "data_check", "", "", "").Impact
		if !strings.Contains(impact, "live") {
			t.Fatalf("%s impact = %q, want it to say which table is live", code, impact)
		}
		if msg, _ := p["message"].(string); strings.Contains(msg, "live") {
			t.Errorf("%s message = %q says which table is live, and so does its impact %q", code, msg, impact)
		}
	}
}

// A count reads as one: grouped in thousands, and "1 row", not "1 rows".
func TestCheckThreshold_ReadableCounts(t *testing.T) {
	one, billion, max, prev := int64(1), int64(1_000_000_000), int64(25_000), int64(12_345)
	drop := 50.0
	for _, tc := range []struct {
		c        modelCheck
		baseline *int64
		want     string
	}{
		{modelCheck{Type: checkRowCount, Params: modelCheckParams{Min: &one}}, nil, "at least 1 row"},
		{modelCheck{Type: checkRowCount, Params: modelCheckParams{Min: &billion, Max: &max}}, nil, "at least 1,000,000,000 rows, at most 25,000"},
		{modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: &drop}}, &prev, "drop at most 50% (previous count 12,345)"},
	} {
		if got := checkThreshold(tc.c, tc.baseline); got != tc.want {
			t.Errorf("checkThreshold = %q, want %q", got, tc.want)
		}
	}
	if got := checkCount(-1234567); got != "-1,234,567" {
		t.Errorf("checkCount(-1234567) = %q", got)
	}
	if _, msg := judgeCheckCount(modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: &drop}}, 4000, &prev); msg != "fell 67.6% from 12,345" {
		t.Errorf("judgeCheckCount message = %q", msg)
	}
}
