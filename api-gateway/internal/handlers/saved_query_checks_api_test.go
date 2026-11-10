package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The Checks tab's API (data-checks-design.md § 6, § 9). What these pin:
//
//  1. who may change a check is who may edit the model — its creator or an admin;
//  2. a custom_sql check whose query is new or changed on a scheduled model waits for
//     an admin in saved_query_pending_edits, and an unscheduled one applies;
//  3. the model's own SQL queue and the check queue share a table but not their rows;
//  4. a check is refused where it cannot work: no table, or a block on an engine that
//     cannot hold one back.

const (
	checkAPICheckID   = "66666666-6666-6666-6666-666666666666"
	checkAPIPendingID = "77777777-7777-7777-7777-777777777777"
	checkAPIRunID     = "88888888-8888-8888-8888-888888888888"
)

var checkTargetColumns = []string{"materialization", "target_table", "target_owned", "connector_type"}

func expectCheckRole(mock sqlmock.Sqlmock, role string) {
	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
}

// expectCheckEditor is requireCheckEditor: the role, then the model as the caller may
// see it, created by createdBy.
func expectCheckEditor(mock sqlmock.Sqlmock, role, createdBy string) {
	expectCheckRole(mock, role)
	mock.ExpectQuery(`FROM saved_queries sq[\s\S]+WHERE sq\.id = \$1`).
		WithArgs(savedQueryID).
		WillReturnRows(savedQueryRows(createdBy, "workspace", "SELECT 1", "read"))
}

// expectCheckTargetLock is the model's row lock, which every change takes first.
func expectCheckTargetLock(mock sqlmock.Sqlmock, mat, target, connector string) {
	mock.ExpectQuery(`FROM saved_queries sq\s+LEFT JOIN connections[\s\S]+FOR UPDATE OF sq`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows(checkTargetColumns).AddRow(mat, target, true, connector))
}

func expectCheckCount(mock sqlmock.Sqlmock, n int) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM saved_query_checks WHERE saved_query_id = \$1`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
}

func checkRow(id, name, typ, cols, params, sqlText, severity string, enabled bool) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"check_id", "name", "check_type", "columns", "params", "sql_text", "severity", "enabled"}).
		AddRow(id, name, typ, cols, params, sqlText, severity, enabled)
}

const negativeAmounts = "SELECT * FROM {{this}} WHERE amount < 0"

// A member can change the checks on a model they made, not on a teammate's: a check
// can hold that model's table back, which is editing what the model publishes.
func TestCreateModelCheck_AMemberCannotChangeATeammatesModel(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", savedQueryOther)
	// Nothing else: no transaction, no write.

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "has rows", "type": "row_count"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A viewer may not change a check at all; the role gate turns them away before the
// model is read.
func TestDeleteModelCheck_AViewerIsTurnedAway(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckRole(mock, "viewer")

	r := savedQueryRouter(http.MethodDelete, "/explorer/saved/:id/checks/:checkId", "viewer", DeleteModelCheck)
	w := doJSON(r, http.MethodDelete, "/explorer/saved/"+savedQueryID+"/checks/"+checkAPICheckID, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A check needs a table of the model's own on a SQL engine. Each refusal is a 400 with
// the reason, and nothing is written.
func TestCreateModelCheck_RefusesAModelWithNoTableToCheck(t *testing.T) {
	for _, tc := range []struct {
		name, mat, target, connector, want string
	}{
		{"statement", "statement", "", "postgresql", "statement model"},
		{"none", "none", "", "postgresql", "only a model that builds a table"},
		{"document", "table", "orders_daily", "mongodb", "document databases"},
		{"unknown engine", "table", "orders_daily", "salesforce", "not supported for salesforce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectCheckEditor(mock, "member", wsScopeUser)
			mock.ExpectBegin()
			expectCheckTargetLock(mock, tc.mat, tc.target, tc.connector)
			mock.ExpectRollback()

			r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
			w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
				map[string]any{"name": "has rows", "type": "row_count"})
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("got %d %s, want 400 naming %q", w.Code, w.Body.String(), tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Snowflake has no audit point, so a blocking check there would promise a hold the run
// cannot make. Refused at save, with the way forward.
func TestCreateModelCheck_RefusesABlockAnEngineCannotHold(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "analytics.orders_daily", "snowflake")
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "id present", "type": "not_null", "columns": []string{"id"}, "severity": "block"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "add it as a warning") {
		t.Fatalf("got %d %s, want 400 with the cannot-block refusal", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Each check is a scan on every run; twenty is the most a model may carry.
func TestCreateModelCheck_TwentyIsTheCap(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckCount(mock, maxChecksPerModel)
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "has rows", "type": "row_count"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at most 20") {
		t.Fatalf("got %d %s, want 400 at the cap", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A typed check applies at once, columns bound as JSON (pgx has no pq.Array), and is
// audited after the commit.
func TestCreateModelCheck_ATypedCheckApplies(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckCount(mock, 2)
	mock.ExpectQuery(`INSERT INTO saved_query_checks[\s\S]+RETURNING check_id::text`).
		WithArgs(savedQueryID, "id unique", "unique", `["id"]`, `{}`, "", "block", true, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"check_id"}).AddRow(checkAPICheckID))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "id unique", "type": "unique", "columns": []string{"id"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	var got modelCheckView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != checkAPICheckID || got.Severity != "block" || !got.Enabled {
		t.Errorf("response = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The gate (D1): a custom_sql check runs as the schedule's user and may read any table
// on the connection, so on a scheduled model it becomes a proposal. Nothing is written
// to saved_query_checks — sqlmock fails on any statement not listed here.
func TestCreateModelCheck_CustomSQLOnAScheduledModelWaitsForAnAdmin(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckCount(mock, 0)
	// Inside the transaction and after the lock: a schedule created in the gap is seen.
	expectScheduleProbe(mock, true)
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM saved_query_checks\s+WHERE saved_query_id = \$1 AND name = \$2`).
		WithArgs(savedQueryID, "no negatives", "").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	mock.ExpectQuery(`INSERT INTO saved_query_pending_edits[\s\S]+VALUES \(\$1, 'check'`).
		WithArgs(savedQueryID, "", sqlmock.AnyArg(), negativeAmounts, "", "please", wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(checkAPIPendingID))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts, "note": "please"})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 with a proposal: %s", w.Code, w.Body.String())
	}
	var body struct {
		Pending struct {
			ID string `json:"id"`
		} `json:"pending_approval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Pending.ID != checkAPIPendingID {
		t.Fatalf("pending_approval = %+v (%v): %s", body.Pending, err, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Not knowing whether the model is scheduled is not "unscheduled": the gate fails
// closed.
func TestCreateModelCheck_AFailedScheduleProbeWritesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckCount(mock, 0)
	mock.ExpectQuery(`SELECT EXISTS`).WithArgs(savedQueryID).WillReturnError(errors.New("connection reset"))
	// The write a handler that read the failure as "unscheduled" would make, queued to
	// succeed: a 500 can then only be the refusal, not this write failing in its place.
	mock.ExpectQuery(`INSERT INTO saved_query_checks`).
		WillReturnRows(sqlmock.NewRows([]string{"check_id"}).AddRow(checkAPICheckID))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500: %s", w.Code, w.Body.String())
	}
}

// An unscheduled model runs only when somebody clicks Run, as themselves, so its
// custom_sql check applies at once.
func TestCreateModelCheck_CustomSQLOnAnUnscheduledModelApplies(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckCount(mock, 0)
	expectScheduleProbe(mock, false)
	mock.ExpectQuery(`INSERT INTO saved_query_checks`).
		WithArgs(savedQueryID, "no negatives", "custom_sql", `[]`, `{}`, negativeAmounts, "block", true, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"check_id"}).AddRow(checkAPICheckID))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks", "member", CreateModelCheck)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks",
		map[string]any{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts})
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An approval writes the whole proposed check, so a direct edit landing first would be
// undone without a word. While a proposal is open the check is refused other edits.
func TestUpdateModelCheck_AnOpenProposalConflicts(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	mock.ExpectQuery(`FROM saved_query_checks WHERE check_id = \$1 AND saved_query_id = \$2 FOR UPDATE`).
		WithArgs(checkAPICheckID, savedQueryID).
		WillReturnRows(checkRow(checkAPICheckID, "no negatives", "custom_sql", "[]", "{}", negativeAmounts, "block", true))
	mock.ExpectQuery(`FROM saved_query_pending_edits\s+WHERE saved_query_id = \$1 AND check_id = \$2 AND kind = 'check' AND status = 'pending'`).
		WithArgs(savedQueryID, checkAPICheckID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPut, "/explorer/saved/:id/checks/:checkId", "member", UpdateModelCheck)
	w := doJSON(r, http.MethodPut, "/explorer/saved/"+savedQueryID+"/checks/"+checkAPICheckID,
		map[string]any{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts, "severity": "warn"})
	if w.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Changing only a custom_sql check's severity runs no query nobody approved, so it
// applies on a scheduled model without asking; enabled left out keeps its value.
func TestUpdateModelCheck_AnUnchangedQueryIsNotGated(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "member", wsScopeUser)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	mock.ExpectQuery(`FROM saved_query_checks WHERE check_id = \$1 AND saved_query_id = \$2 FOR UPDATE`).
		WithArgs(checkAPICheckID, savedQueryID).
		WillReturnRows(checkRow(checkAPICheckID, "no negatives", "custom_sql", "[]", "{}", negativeAmounts, "block", false))
	mock.ExpectQuery(`FROM saved_query_pending_edits`).
		WithArgs(savedQueryID, checkAPICheckID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	// No schedule probe: the query did not change.
	mock.ExpectExec(`UPDATE saved_query_checks`).
		WithArgs(checkAPICheckID, savedQueryID, "no negatives", "custom_sql", `[]`, `{}`, negativeAmounts, "warn", false, wsScopeUser).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPut, "/explorer/saved/:id/checks/:checkId", "member", UpdateModelCheck)
	w := doJSON(r, http.MethodPut, "/explorer/saved/"+savedQueryID+"/checks/"+checkAPICheckID,
		map[string]any{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts, "severity": "warn"})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Deciding what a scheduled model's check may read is an admin's act.
func TestApproveModelCheckEdit_AMemberIsTurnedAway(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckRole(mock, "member")

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/pending/:pendingId/approve", "member", ApproveModelCheckEdit)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/pending/"+checkAPIPendingID+"/approve", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectCheckProposal(mock sqlmock.Sqlmock, checkID, sqlText, base, payload string) {
	mock.ExpectQuery(`FROM saved_query_pending_edits\s+WHERE id = \$1 AND saved_query_id = \$2 AND kind = 'check' AND status = 'pending'\s+FOR UPDATE`).
		WithArgs(checkAPIPendingID, savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{"check_id", "sql_text", "base_sql_text", "check_payload", "proposed_by"}).
			AddRow(checkID, sqlText, base, payload, savedQueryOther))
}

const proposedPayload = `{"name":"no negatives","type":"custom_sql","columns":null,"params":{},"severity":"block","enabled":true}`

// A proposal whose check was removed has nothing to edit; approving it must not
// quietly create the check again.
func TestApproveModelCheckEdit_ARemovedCheckConflicts(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "admin", savedQueryOther)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckProposal(mock, checkAPICheckID, negativeAmounts, "SELECT * FROM {{this}} WHERE amount < -1", proposedPayload)
	mock.ExpectQuery(`FROM saved_query_checks WHERE check_id = \$1 AND saved_query_id = \$2 FOR UPDATE`).
		WithArgs(checkAPICheckID, savedQueryID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/pending/:pendingId/approve", "admin", ApproveModelCheckEdit)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/pending/"+checkAPIPendingID+"/approve", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "was removed") {
		t.Fatalf("got %d %s, want 409", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The reviewer compared against base_sql_text; a check that moved since would land a
// query nobody compared.
func TestApproveModelCheckEdit_AMovedBaseIsStale(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "admin", savedQueryOther)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckProposal(mock, checkAPICheckID, negativeAmounts, "SELECT * FROM {{this}} WHERE amount < -1", proposedPayload)
	mock.ExpectQuery(`FROM saved_query_checks WHERE check_id = \$1`).
		WithArgs(checkAPICheckID, savedQueryID).
		WillReturnRows(checkRow(checkAPICheckID, "no negatives", "custom_sql", "[]", "{}",
			"SELECT * FROM {{this}} WHERE amount < -5", "block", true))
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/pending/:pendingId/approve", "admin", ApproveModelCheckEdit)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/pending/"+checkAPIPendingID+"/approve", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stale_write") {
		t.Fatalf("got %d %s, want 409 stale_write", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Approving a new check creates it with the proposer as its author and the admin as its
// last editor, then closes the proposal — in one transaction, model lock first.
func TestApproveModelCheckEdit_ANewCheckIsCreated(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "admin", savedQueryOther)
	mock.ExpectBegin()
	expectCheckTargetLock(mock, "table", "public.daily_mrr", "postgresql")
	expectCheckProposal(mock, "", negativeAmounts, "", proposedPayload)
	expectCheckCount(mock, 3)
	mock.ExpectQuery(`INSERT INTO saved_query_checks[\s\S]+RETURNING check_id::text`).
		WithArgs(savedQueryID, "no negatives", "custom_sql", `[]`, `{}`, negativeAmounts, "block", true, savedQueryOther, wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"check_id"}).AddRow(checkAPICheckID))
	mock.ExpectExec(`UPDATE saved_query_pending_edits`).
		WithArgs(checkAPIPendingID, "approved", wsScopeUser, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/pending/:pendingId/approve", "admin", ApproveModelCheckEdit)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/pending/"+checkAPIPendingID+"/approve", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		CheckID string `json:"check_id"`
		Applied bool   `json:"applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.CheckID != checkAPICheckID || !body.Applied {
		t.Fatalf("response = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A rejection writes only its verdict, so it takes no model lock.
func TestRejectModelCheckEdit_TakesNoModelLock(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckEditor(mock, "admin", savedQueryOther)
	mock.ExpectBegin()
	expectCheckProposal(mock, checkAPICheckID, negativeAmounts, "", proposedPayload)
	mock.ExpectExec(`UPDATE saved_query_pending_edits`).
		WithArgs(checkAPIPendingID, "rejected", wsScopeUser, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/pending/:pendingId/reject", "admin", RejectModelCheckEdit)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/pending/"+checkAPIPendingID+"/reject", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A viewer can see the checks, their last results, whether this engine can hold a
// table back, and what waits for approval.
func TestListModelChecks_AViewerSeesChecksHistoryAndProposals(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectVisibleRunsModel(mock)
	mock.ExpectQuery(`FROM saved_queries sq\s+LEFT JOIN connections`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows(checkTargetColumns).AddRow("table", "analytics.orders_daily", true, "snowflake"))
	mock.ExpectQuery(`FROM saved_query_checks WHERE saved_query_id = \$1 ORDER BY created_at, name`).
		WithArgs(savedQueryID).
		WillReturnRows(checkRow(checkAPICheckID, "has rows", "row_count", "[]", `{"min":1}`, "", "warn", true))
	at := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`ROW_NUMBER\(\) OVER \(PARTITION BY r\.check_id`).
		WithArgs(savedQueryID, modelCheckHistoryLen).
		WillReturnRows(sqlmock.NewRows([]string{"check_id", "run_id", "finished_at", "status", "severity", "observed", "threshold", "message"}).
			AddRow(checkAPICheckID, checkAPIRunID, at, "fail", "warn", int64(0), ">= 1", "saw 0").
			// A result under a check id the list does not have is left out.
			AddRow(checkAPIPendingID, checkAPIRunID, at, "pass", "warn", nil, "", ""))
	mock.ExpectQuery(`FROM saved_query_pending_edits\s+WHERE saved_query_id = \$1 AND kind = 'check' AND status = 'pending'`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "check_id", "sql_text", "base_sql_text", "check_payload", "note", "proposed_by", "proposed_at"}).
			AddRow(checkAPIPendingID, "", negativeAmounts, "", proposedPayload, "", savedQueryOther, at))

	r := savedQueryRouter(http.MethodGet, "/explorer/saved/:id/checks", "viewer", ListModelChecks)
	w := doJSON(r, http.MethodGet, "/explorer/saved/"+savedQueryID+"/checks", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var body struct {
		Checks            []modelCheckView     `json:"checks"`
		Pending           []modelCheckProposal `json:"pending"`
		CanBlock          bool                 `json:"can_block"`
		CannotBlockReason string               `json:"cannot_block_reason"`
		ConnectorType     string               `json:"connector_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Checks) != 1 || len(body.Checks[0].History) != 1 || body.Checks[0].History[0].Status != "fail" ||
		body.Checks[0].History[0].Observed == nil || *body.Checks[0].History[0].Observed != 0 {
		t.Errorf("checks = %+v", body.Checks)
	}
	if body.CanBlock || body.CannotBlockReason != cannotBlockMessage || body.ConnectorType != "snowflake" {
		t.Errorf("snowflake: can_block = %v, reason = %q, connector_type = %q",
			body.CanBlock, body.CannotBlockReason, body.ConnectorType)
	}
	if len(body.Pending) != 1 || body.Pending[0].SQLText != negativeAmounts {
		t.Errorf("pending = %+v", body.Pending)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A run's results are read through the model, so a run id from another model reads as
// nothing rather than leaking its checks.
func TestListModelRunChecks_JoinsTheRunOnTheModel(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectVisibleRunsModel(mock)
	mock.ExpectQuery(`JOIN saved_query_runs sr ON sr\.run_id = r\.run_id AND sr\.saved_query_id = \$1\s+WHERE r\.run_id = \$2`).
		WithArgs(savedQueryID, checkAPIRunID).
		WillReturnRows(sqlmock.NewRows([]string{"check_id", "check_name", "check_type", "severity", "status", "observed", "threshold", "message"}).
			AddRow("", "removed check", "not_null", "block", "fail", int64(3), "0", "3 rows have no id"))

	r := savedQueryRouter(http.MethodGet, "/explorer/saved/:id/runs/:runId/checks", "viewer", ListModelRunChecks)
	w := doJSON(r, http.MethodGet, "/explorer/saved/"+savedQueryID+"/runs/"+checkAPIRunID+"/checks", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"removed check"`) {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The model's own SQL queue and the check queue share saved_query_pending_edits (138).
// The SQL queue's reads must see only kind 'sql' rows: otherwise an open check
// proposal reads as the model's pending SQL edit, and approving it would write a
// check's query over the model's SQL.
func TestModelSQLQueueReadsOnlySQLProposals(t *testing.T) {
	t.Run("versions", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectCheckRole(mock, "viewer")
		mock.ExpectQuery(`FROM saved_queries sq[\s\S]+WHERE sq\.id = \$1`).
			WithArgs(savedQueryID).
			WillReturnRows(savedQueryRows(wsScopeUser, "workspace", "SELECT 2", "read"))
		mock.ExpectQuery(`FROM saved_query_versions`).
			WithArgs(savedQueryID).
			WillReturnRows(sqlmock.NewRows([]string{"version", "name", "sql_text", "statement_class", "changed_by", "created_at"}))
		mock.ExpectQuery(`FROM saved_query_pending_edits\s+WHERE saved_query_id = \$1 AND status = 'pending' AND kind = 'sql'`).
			WithArgs(savedQueryID).
			WillReturnError(sql.ErrNoRows)

		r := savedQueryRouter(http.MethodGet, "/explorer/saved/:id/versions", "viewer", ListSavedQueryVersions)
		w := doJSON(r, http.MethodGet, "/explorer/saved/"+savedQueryID+"/versions", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("approve", func(t *testing.T) {
		mock, cleanup := wsScopeMockDB(t)
		defer cleanup()
		expectCheckRole(mock, "admin")
		mock.ExpectQuery(`FROM saved_queries sq[\s\S]+WHERE sq\.id = \$1`).
			WithArgs(savedQueryID).
			WillReturnRows(savedQueryRows(wsScopeUser, "workspace", "SELECT 1", "read"))
		mock.ExpectBegin()
		expectPendingReviewLock(mock, "Daily MRR", "SELECT 1", "read")
		mock.ExpectQuery(`FROM saved_query_pending_edits\s+WHERE saved_query_id = \$1 AND status = 'pending' AND kind = 'sql'\s+FOR UPDATE`).
			WithArgs(savedQueryID).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectRollback()

		r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/pending/approve", "admin", ApproveSavedQueryEdit)
		w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/pending/approve", nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404: %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// ---------------------------------------------------------------------------
// plan_model_checks (PlanModelChecks)
// ---------------------------------------------------------------------------

var checkPlanTargetColumns = []string{"materialization", "target_table", "target_owned", "connector_type", "connection_id"}

const checkPlanConnectionID = "99999999-9999-9999-9999-999999999999"

// expectCheckPlan is PlanModelChecks' reads: the caller may see the model, its target,
// and the names of its checks. Nothing after them: a plan writes nothing.
func expectCheckPlan(mock sqlmock.Sqlmock, target, connector string, existing ...string) {
	expectVisibleRunsModel(mock)
	mock.ExpectQuery(`FROM saved_queries sq\s+LEFT JOIN connections`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows(checkPlanTargetColumns).AddRow("table", target, true, connector, checkPlanConnectionID))
	rows := sqlmock.NewRows([]string{"name"})
	for _, n := range existing {
		rows.AddRow(n)
	}
	mock.ExpectQuery(`SELECT name FROM saved_query_checks WHERE saved_query_id = \$1`).
		WithArgs(savedQueryID).WillReturnRows(rows)
}

func postCheckPlan(t *testing.T, checks []map[string]any) modelChecksPlan {
	t.Helper()
	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/plan", "viewer", PlanModelChecks)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/plan", map[string]any{"checks": checks})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", w.Code, w.Body.String())
	}
	var plan modelChecksPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// A plan judges each check as the Checks tab would save it — its settings, its name
// against the model's checks and the plan's, a custom_sql query's read-only gate — and
// writes nothing: sqlmock holds no write, so one would fail the call.
func TestPlanModelChecks_JudgesEachCheckAndSavesNothing(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckPlan(mock, "analytics.orders_daily", "postgresql", "has rows")

	plan := postCheckPlan(t, []map[string]any{
		{"name": "id present", "type": "not_null", "columns": []string{"id"}},
		{"name": "has rows", "type": "row_count"},
		{"name": "one per id", "type": "unique"},
		{"name": "wipe", "type": "custom_sql", "sql_text": "DELETE FROM {{this}}"},
		{"name": "no negatives", "type": "custom_sql", "sql_text": negativeAmounts},
		{"name": "id present", "type": "unique", "columns": []string{"id"}},
		{"name": "volume", "type": "row_count_change", "params": map[string]any{"max_drop_pct": 50}},
	})
	want := []struct{ refusal, severity string }{
		{"", "block"},
		{"already has a check named has rows", ""},
		{"a unique check takes 1 to 10 columns", ""},
		{"read-only SELECT", ""},
		{"", "block"},
		{"two of the checks are named id present", ""},
		{"", "warn"},
	}
	if len(plan.Checks) != len(want) {
		t.Fatalf("checks = %+v", plan.Checks)
	}
	for i, w := range want {
		got := plan.Checks[i]
		if w.refusal == "" && (got.Refusal != "" || got.Severity != w.severity) {
			t.Errorf("check %d: got %+v, want it valid with severity %s", i+1, got, w.severity)
		}
		if w.refusal != "" && !strings.Contains(got.Refusal, w.refusal) {
			t.Errorf("check %d: refusal %q, want it to say %q", i+1, got.Refusal, w.refusal)
		}
	}
	if plan.ConnectionID != checkPlanConnectionID || plan.TargetSchema != "analytics" || plan.TargetTable != "orders_daily" ||
		!plan.ColumnsCaseSensitive || !plan.CanBlock || plan.Existing != 1 || plan.MaxChecks != maxChecksPerModel {
		t.Errorf("plan = %+v", plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// On an engine that cannot hold a table back, a check left without a severity is
// planned as a warning, as the Checks tab fixes it; one that asks to block is refused
// with the way forward.
func TestPlanModelChecks_AnEngineThatCannotBlockPlansWarnings(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckPlan(mock, "orders_daily", "snowflake")

	plan := postCheckPlan(t, []map[string]any{
		{"name": "id present", "type": "not_null", "columns": []string{"ID"}},
		{"name": "has rows", "type": "row_count", "severity": "block"},
	})
	if got := plan.Checks[0]; got.Refusal != "" || got.Severity != "warn" {
		t.Errorf("unstated severity: %+v, want a valid warning", got)
	}
	if got := plan.Checks[1]; !strings.Contains(got.Refusal, "add it as a warning") {
		t.Errorf("block: %+v, want the cannot-block refusal", got)
	}
	if plan.CanBlock || plan.CannotBlockReason != cannotBlockMessage || plan.ColumnsCaseSensitive || plan.TargetSchema != "" {
		t.Errorf("plan = %+v", plan)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The cap counts the model's own checks: with 19, a plan of two has room for one.
func TestPlanModelChecks_TheCapCountsTheModelsChecks(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	existing := make([]string, 19)
	for i := range existing {
		existing[i] = "check " + string(rune('a'+i))
	}
	expectCheckPlan(mock, "orders_daily", "postgresql", existing...)

	plan := postCheckPlan(t, []map[string]any{
		{"name": "has rows", "type": "row_count"},
		{"name": "id present", "type": "not_null", "columns": []string{"id"}},
	})
	if plan.Checks[0].Refusal != "" || !strings.Contains(plan.Checks[1].Refusal, "at most 20 checks") {
		t.Errorf("checks = %+v, want the first valid and the second over the cap", plan.Checks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A model with no table of its own cannot have checks; the plan says why, as a 400 the
// tool passes on.
func TestPlanModelChecks_RefusesAModelWithNoTable(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectVisibleRunsModel(mock)
	mock.ExpectQuery(`FROM saved_queries sq\s+LEFT JOIN connections`).
		WithArgs(savedQueryID).
		WillReturnRows(sqlmock.NewRows(checkPlanTargetColumns).AddRow("statement", "", false, "postgresql", checkPlanConnectionID))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved/:id/checks/plan", "viewer", PlanModelChecks)
	w := doJSON(r, http.MethodPost, "/explorer/saved/"+savedQueryID+"/checks/plan",
		map[string]any{"checks": []map[string]any{{"name": "has rows", "type": "row_count"}}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot have checks: a statement model") {
		t.Fatalf("got %d %s, want 400 with the reason", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
