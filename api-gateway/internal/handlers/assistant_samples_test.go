package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
)

func withSampleMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = mockDB
	t.Cleanup(func() {
		db.DB = prev
		mockDB.Close()
	})
	return mock
}

// A model is a third party, so it is withheld more than the preview masks:
// names, addresses and birth dates as well as secrets and contact details.
func TestAssistantWithholdsColumnName(t *testing.T) {
	withheld := []string{
		"password", "api_key", "access_token", "email", "phone", "ssn",
		"first_name", "LastName", "full_name", "surname", "home_address", "street",
		"date_of_birth", "dob", "ip_address", "client_ip", "salary", "iban",
	}
	kept := []string{"id", "status", "created_at", "amount", "currency", "order_id", "ship_date", "country_code"}
	for _, col := range withheld {
		if !assistantWithholdsColumnName(col) {
			t.Errorf("%q should be withheld from a model", col)
		}
	}
	for _, col := range kept {
		if assistantWithholdsColumnName(col) {
			t.Errorf("%q should reach a model", col)
		}
	}
}

func TestTrimAssistantSample(t *testing.T) {
	when := time.Date(2026, 10, 2, 9, 30, 0, 0, time.FixedZone("x", 3600))
	row := map[string]interface{}{
		"id": int64(7), "status": strings.Repeat("é", 80), "email": "a@b.example",
		"meta": map[string]interface{}{"k": "v"}, "tags": []interface{}{"a"},
		"at": when, "flag": true, "blob": []byte("raw"), "note": nil, "risk": "flagged",
	}
	result := &SampleResult{
		Columns: []string{"id", "status", "email", "meta", "tags", "at", "flag", "blob", "note", "risk"},
		Rows:    []map[string]interface{}{row, row, row, row, row, row, row},
	}
	cols, rows, withheld := trimAssistantSample(result, nil, map[string]bool{"risk": true})
	if strings.Join(withheld, ",") != "email,risk" {
		t.Fatalf("withheld = %v", withheld)
	}
	if strings.Join(cols, ",") != "id,status,meta,tags,at,flag,blob,note" {
		t.Fatalf("cols = %v", cols)
	}
	if len(rows) != assistantSampleRows {
		t.Fatalf("rows = %d, want %d", len(rows), assistantSampleRows)
	}
	r := rows[0]
	if _, ok := r["email"]; ok {
		t.Fatal("a withheld column's value is in the row")
	}
	if got := r["status"].(string); len([]rune(got)) != assistantSampleValueRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long value not cut to %d runes: %d", assistantSampleValueRunes, len([]rune(got)))
	}
	if r["meta"] != "(object)" || r["tags"] != "(array)" || r["at"] != "2026-10-02T08:30:00Z" ||
		r["flag"] != true || r["id"] != int64(7) || r["blob"] != "raw" || r["note"] != nil {
		t.Fatalf("row = %v", r)
	}

	// Asked-for columns only, case-insensitively; an asked-for secret is still withheld.
	cols, _, withheld = trimAssistantSample(result, []string{"STATUS", "Email"}, nil)
	if strings.Join(cols, ",") != "status" || strings.Join(withheld, ",") != "email" {
		t.Fatalf("asked for status,email: cols %v withheld %v", cols, withheld)
	}

	// Empty results are lists, never null, in the JSON the model reads.
	cols, rows, withheld = trimAssistantSample(&SampleResult{}, nil, nil)
	raw, _ := json.Marshal(map[string]interface{}{"c": cols, "r": rows, "w": withheld})
	if string(raw) != `{"c":[],"r":[],"w":[]}` {
		t.Fatalf("empty = %s", raw)
	}
}

func TestSampleRowsEntriesParse(t *testing.T) {
	got := parseSampleRowsEntries(" claude, llama ,gemini,,")
	if len(got) != 2 || !got["claude"] || !got["gemini"] {
		t.Fatalf("parsed = %v", got)
	}
	if joined := joinSampleRowsEntries(map[string]bool{"gemini": true, "claude": true, "llama": true}); joined != "claude,gemini" {
		t.Fatalf("joined = %q", joined)
	}
}

// A PII scan's verdict on the table is matched by the full name and by the
// name after the last dot, as a scan may record either; an unreadable scan is
// an error, and Read then samples nothing.
func TestScanFlaggedColumns(t *testing.T) {
	mock := withSampleMockDB(t)
	mock.ExpectQuery(`SELECT DISTINCT column_name FROM pii_scan_results`).
		WithArgs("conn-1", "public.orders", "orders").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("Notes").AddRow("buyer"))
	got, err := scanFlaggedColumns(context.Background(), db.DB, "conn-1", "public.orders")
	if err != nil || !got["notes"] || !got["buyer"] || len(got) != 2 {
		t.Fatalf("flagged = %v, err %v", got, err)
	}

	mock.ExpectQuery(`SELECT DISTINCT column_name FROM pii_scan_results`).WillReturnError(errors.New("relation does not exist"))
	if _, err := scanFlaggedColumns(context.Background(), db.DB, "conn-1", "orders"); err == nil {
		t.Fatal("an unreadable scan must be an error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetWorkspaceLLMData(t *testing.T) {
	mock := withSampleMockDB(t)
	mock.ExpectQuery(`SELECT llm_data_mode FROM workspaces WHERE id = \$1`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"llm_data_mode"}).AddRow("sample_rows"))
	mock.ExpectQuery(`SELECT value FROM instance_settings WHERE key = \$1`).WithArgs(llmSampleRowsSetting).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("gemini,claude"))
	r := tokenRouter(http.MethodGet, "/workspace/llm-data", GetWorkspaceLLMData, "viewer", false)
	w := serveToken(r, http.MethodGet, "/workspace/llm-data", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != `{"mode":"sample_rows","approved_models":["claude","gemini"],"can_edit":false}` {
		t.Fatalf("body = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Only the owner decides whether row values may reach a model; the change is audited.
func TestSetWorkspaceLLMDataIsOwnerOnlyAndAudited(t *testing.T) {
	for _, role := range []string{"admin", "editor", "viewer", ""} {
		withSampleMockDB(t) // any query would fail the expectations
		r := tokenRouter(http.MethodPut, "/workspace/llm-data", SetWorkspaceLLMData, role, false)
		if w := serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"sample_rows"}`); w.Code != http.StatusForbidden {
			t.Fatalf("role %q: status %d, want 403", role, w.Code)
		}
	}

	withSampleMockDB(t)
	r := tokenRouter(http.MethodPut, "/workspace/llm-data", SetWorkspaceLLMData, "owner", false)
	for _, body := range []string{`{"mode":"all_rows"}`, `{}`, `nope`} {
		if w := serveToken(r, http.MethodPut, "/workspace/llm-data", body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", body, w.Code)
		}
	}

	// Bug class: one statement that reads the value its own write changes. The
	// first version read the old mode through a FOR UPDATE CTE inside the
	// UPDATE; Postgres evaluated the CTE after the update, skipped the row the
	// statement had just changed and returned NULL, so every save answered 500
	// after committing and wrote no audit row. The read and the write are now
	// two statements in one transaction.
	mock := withSampleMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT llm_data_mode FROM workspaces WHERE id = \$1 FOR UPDATE`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"llm_data_mode"}).AddRow("names_only"))
	mock.ExpectExec(`UPDATE workspaces SET llm_data_mode = \$2 WHERE id = \$1`).WithArgs(wsScopeWS, "sample_rows").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	details := &argCapture{}
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(sqlmock.AnyArg(), "workspace.llm_data_mode_update", "workspace", sqlmock.AnyArg(), details, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT value FROM instance_settings`).WillReturnRows(sqlmock.NewRows([]string{"value"}))
	w := serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"sample_rows"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mode":"sample_rows"`) || !strings.Contains(w.Body.String(), `"can_edit":true`) {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if s := fmt.Sprintf("%s", details.v); !strings.Contains(s, `"from":"names_only"`) || !strings.Contains(s, `"to":"sample_rows"`) {
		t.Fatalf("audit details = %s", details.v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A save that fails is not half-done: nothing is committed, the caller gets
// the error, and no audit row claims a change.
func TestSetWorkspaceLLMDataFailureCommitsNothing(t *testing.T) {
	mock := withSampleMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT llm_data_mode FROM workspaces WHERE id = \$1 FOR UPDATE`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"llm_data_mode"}).AddRow("names_only"))
	mock.ExpectExec(`UPDATE workspaces SET llm_data_mode`).WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()
	r := tokenRouter(http.MethodPut, "/workspace/llm-data", SetWorkspaceLLMData, "owner", false)
	w := serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"sample_rows"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err) // an audit INSERT or a COMMIT would be unexpected here
	}

	mock = withSampleMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT llm_data_mode FROM workspaces WHERE id = \$1 FOR UPDATE`).WillReturnRows(sqlmock.NewRows([]string{"llm_data_mode"}))
	mock.ExpectRollback()
	if w := serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"sample_rows"}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing workspace: status %d, want 404", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// An install with RSYNC_LLM_SAMPLE_ROWS_ALLOWED=false (the self-host compose and
// chart) sends no row value, whatever was saved before: no model counts as
// approved, and neither switch can be turned on. Turning things off still works.
func TestSampleRowsOffOnThisInstall(t *testing.T) {
	t.Setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", "false")
	ctx := context.Background()

	// No query at all: an approval saved before the install turned them off
	// must not come back.
	mock := withSampleMockDB(t)
	approved, err := AssistantSamples{}.Approved(ctx)
	if err != nil || len(approved) != 0 {
		t.Fatalf("Approved() = %v, %v; want none", approved, err)
	}
	if _, err := (AssistantSamples{}).SetApproved(ctx, "claude", true, "admin-1"); !errors.Is(err, ErrSampleRowsDisabled) {
		t.Fatalf("approve: err = %v, want ErrSampleRowsDisabled", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Withdrawing an old approval is still saved.
	mock = withSampleMockDB(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO instance_settings`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT value FROM instance_settings WHERE key = \$1 FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("claude"))
	mock.ExpectExec(`UPDATE instance_settings SET value = \$2`).WithArgs(llmSampleRowsSetting, "", "admin-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if _, err := (AssistantSamples{}).SetApproved(ctx, "claude", false, "admin-1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// The owner cannot turn sample rows on: 403 before any write.
	withSampleMockDB(t)
	r := tokenRouter(http.MethodPut, "/workspace/llm-data", SetWorkspaceLLMData, "owner", false)
	w := serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"sample_rows"}`)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"sample_rows_disabled"`) {
		t.Fatalf("sample_rows: status %d: %s", w.Code, w.Body.String())
	}

	// names_only still saves, and the view lists no approved model.
	mock = withSampleMockDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT llm_data_mode FROM workspaces WHERE id = \$1 FOR UPDATE`).WithArgs(wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"llm_data_mode"}).AddRow("sample_rows"))
	mock.ExpectExec(`UPDATE workspaces SET llm_data_mode = \$2 WHERE id = \$1`).WithArgs(wsScopeWS, "names_only").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	w = serveToken(r, http.MethodPut, "/workspace/llm-data", `{"mode":"names_only"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"approved_models":[]`) {
		t.Fatalf("names_only: status %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
