package handlers

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
)

// Generator-trust plan, phase 3b: an API connection's tables are read (one page
// each, no row kept) when it is saved and on "Check tables"; the answer is kept on
// the connection. A connector whose image failed to build answers with its build
// log, never as "still being set up".

// tableCheckToolsDir lays out two connectors as the repo does (latest.json +
// versions/v1.0.0/metadata.json): acme-api (an API connector) and pg-db (a
// database). TOOLS_DIR resolves a new connection's version; the public path feeds
// the metadata index connectorChecksTables reads.
func tableCheckToolsDir(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	for _, c := range []struct{ dir, id, category string }{
		{"api/acme-api", "acme-api", "api_saas"},
		{"database/pg-db", "pg-db", "database"},
	} {
		root := filepath.Join(base, "public", c.dir)
		if err := os.MkdirAll(filepath.Join(root, "versions", "v1.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		latest := `{"current_version": "v1.0.0", "all_versions": ["v1.0.0"]}`
		if err := os.WriteFile(filepath.Join(root, "latest.json"), []byte(latest), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := `{"id": "` + c.id + `", "name": "` + c.id + `", "category": "` + c.category + `"}`
		if err := os.WriteFile(filepath.Join(root, "versions", "v1.0.0", "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TOOLS_DIR", base)
	t.Setenv("MCP_PUBLIC_CONNECTORS_PATH", filepath.Join(base, "public"))
}

// stubCheckTables replaces the orchestrator call for one test.
func stubCheckTables(t *testing.T, fn func(ctx context.Context, connectorType string, config map[string]interface{}, connectionID string) (map[string]interface{}, error)) {
	t.Helper()
	prev := checkTablesViaOrchestrator
	checkTablesViaOrchestrator = fn
	t.Cleanup(func() { checkTablesViaOrchestrator = prev })
}

// shortenTableCheckWaits makes a running check outlast the answer's wait.
func shortenTableCheckWaits(t *testing.T, wait time.Duration) {
	t.Helper()
	prevSave, prevButton, prevMin := tableCheckSaveDeadline, tableCheckButtonDeadline, tableCheckMinWait
	tableCheckSaveDeadline, tableCheckButtonDeadline, tableCheckMinWait = wait, wait, 0
	t.Cleanup(func() {
		tableCheckSaveDeadline, tableCheckButtonDeadline, tableCheckMinWait = prevSave, prevButton, prevMin
	})
}

// A failed image build as the orchestrator answers it (mcp.FriendlyTestConnectionError
// over tool-generator's BUILD_FAILED_PREFIX). The last line names the cause.
const buildFailedTestErr = "The Acme connector image failed to build. End of the build log:\n" +
	"Step 5/9 : RUN pip install -r requirements.txt\n" +
	"ERROR: No matching distribution found for acme-sdk==9.9\n" +
	"ModuleNotFoundError: No module named 'acme'"

var tableCheckResult = map[string]interface{}{
	"tables": []interface{}{
		map[string]interface{}{"name": "tasks", "status": "ok"},
		map[string]interface{}{"name": "goals", "status": "failed", "error": "HTTP 404 from /goals"},
		map[string]interface{}{"name": "time_entries", "status": "needs_setting", "missing_settings": []interface{}{"team_id"}},
	},
	"ok": 1, "failed": 1, "needs_setting": 1, "not_checked": 0,
}

// tableCheckRecordArg matches the record bound into the last_table_check UPDATE.
type tableCheckRecordArg struct {
	mu  sync.Mutex
	got map[string]interface{}
}

func (a *tableCheckRecordArg) Match(v driver.Value) bool {
	b, ok := v.([]byte)
	if !ok {
		return false
	}
	var m map[string]interface{}
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	a.mu.Lock()
	a.got = m
	a.mu.Unlock()
	return true
}

func (a *tableCheckRecordArg) record() map[string]interface{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got
}

func expectCheckTablesLoad(t *testing.T, mock sqlmock.Sqlmock, connectorType string) {
	t.Helper()
	enc, err := crypto.EncryptString(`{"api_token": "t0k-unit-test"}`)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	mock.ExpectQuery(`SELECT connector_type, config, connector_version\s+FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(wsScopeConnection, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config", "connector_version"}).
			AddRow(connectorType, enc, "v1.0.0"))
}

const storeTableCheckSQL = `UPDATE connections SET last_table_check = \$1 WHERE id = \$2 AND workspace_id = \$3`

func postCheckTables(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections/:id/check-tables", CheckConnectionTables)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+wsScopeConnection+"/check-tables", nil)
	r.ServeHTTP(w, req)
	return w
}

// waitForExpectations waits for a check that ends after its answer to store.
func waitForExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := mock.ExpectationsWereMet()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("db expectations: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConnectorChecksTables_EveryAPIConnectorAndNoDatabase(t *testing.T) {
	tableCheckToolsDir(t)
	if !connectorChecksTables("acme-api") {
		t.Fatalf("an api_saas connector must get the table check")
	}
	if connectorChecksTables("pg-db") {
		t.Fatalf("a database connector must not get the table check")
	}
	if connectorChecksTables("no-such-connector") {
		t.Fatalf("an unknown connector must not get the table check")
	}
	if got := connectorMetadataString("acme-api", "category"); got != "api_saas" {
		t.Fatalf("connectorMetadataString(category) = %q, want api_saas", got)
	}
}

func TestCheckConnectionTables_StoresAndAnswersTheCheck(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	var gotConfig map[string]interface{}
	var gotID string
	stubCheckTables(t, func(_ context.Context, connectorType string, config map[string]interface{}, connectionID string) (map[string]interface{}, error) {
		if connectorType != "acme-api" {
			t.Errorf("connector type = %q, want acme-api", connectorType)
		}
		gotConfig, gotID = config, connectionID
		return tableCheckResult, nil
	})

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckTablesLoad(t, mock, "acme-api")
	mock.ExpectExec(`INSERT INTO connection_access_logs`).
		WithArgs(wsScopeConnection, wsScopeUser, "check_tables", true, "").
		WillReturnResult(sqlmock.NewResult(1, 1))
	stored := &tableCheckRecordArg{}
	mock.ExpectExec(storeTableCheckSQL).
		WithArgs(stored, wsScopeConnection, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := postCheckTables(t)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	// The pinned version routes the check to the container the pipelines run.
	if gotConfig["connector_version"] != "v1.0.0" || gotConfig["api_token"] != "t0k-unit-test" {
		t.Fatalf("the check must get the stored config and its version, got %v", gotConfig)
	}
	if gotID != wsScopeConnection {
		t.Fatalf("connection id = %q, want %q", gotID, wsScopeConnection)
	}
	answer, _ := decodeBody(t, w)["last_table_check"].(map[string]interface{})
	if answer["failed"] != float64(1) || answer["needs_setting"] != float64(1) || answer["checked_at"] == nil {
		t.Fatalf("the answer must carry the check and when it ran, got %v", answer)
	}
	rec := stored.record()
	if rec["failed"] != float64(1) || rec["checked_at"] != answer["checked_at"] {
		t.Fatalf("the stored record must be the answered one, got %v", rec)
	}
}

func TestCheckConnectionTables_ADatabaseConnectionIsRefused(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	stubCheckTables(t, func(context.Context, string, map[string]interface{}, string) (map[string]interface{}, error) {
		t.Errorf("a database connection must not be checked")
		return nil, nil
	})
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckTablesLoad(t, mock, "pg-db")

	w := postCheckTables(t)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w)["error"]; got != "table_check_not_supported" {
		t.Fatalf("error = %v, want table_check_not_supported", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
}

// A check still running when the wait ends answers 202, and its record is stored
// when it ends.
func TestCheckConnectionTables_AnswersPendingThenStores(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	shortenTableCheckWaits(t, 50*time.Millisecond)
	release := make(chan struct{})
	stubCheckTables(t, func(context.Context, string, map[string]interface{}, string) (map[string]interface{}, error) {
		<-release
		return tableCheckResult, nil
	})
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectCheckTablesLoad(t, mock, "acme-api")
	mock.ExpectExec(`INSERT INTO connection_access_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	stored := &tableCheckRecordArg{}
	mock.ExpectExec(storeTableCheckSQL).
		WithArgs(stored, wsScopeConnection, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := postCheckTables(t)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if got := decodeBody(t, w)["table_check_pending"]; got != true {
		t.Fatalf("table_check_pending = %v, want true", got)
	}
	close(release)
	waitForExpectations(t, mock)
	if rec := stored.record(); rec["failed"] != float64(1) {
		t.Fatalf("the late record must be stored, got %v", rec)
	}
}

// An edit of the settings while a check of the old ones runs: the old check's
// answer is dropped, so it cannot overwrite the NULL the edit wrote.
func TestTableCheck_AnEditDropsTheRunningCheck(t *testing.T) {
	release := make(chan struct{})
	var ended int32
	stubCheckTables(t, func(context.Context, string, map[string]interface{}, string) (map[string]interface{}, error) {
		<-release
		atomic.StoreInt32(&ended, 1)
		return tableCheckResult, nil
	})
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	// Declared so a write is observed, and required to stay unfulfilled.
	mock.ExpectExec(storeTableCheckSQL).WillReturnResult(sqlmock.NewResult(0, 1))

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	if _, done := runTableCheck(c, db.DB, wsScopeConnection, wsScopeWS, "acme-api", map[string]interface{}{}, 0); done {
		t.Fatalf("a check given no wait must answer pending")
	}
	supersedeTableChecks(wsScopeConnection) // what UpdateConnection does on an edit
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&ended) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the store follows the call
	if err := mock.ExpectationsWereMet(); err == nil || !strings.Contains(err.Error(), "last_table_check") {
		t.Fatalf("the superseded check must not store its answer, got: %v", err)
	}
}

func TestCreateConnection_AnAPIConnectionChecksItsTables(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	var checkBody map[string]interface{}
	var checks int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agent/test-connection":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		case "/api/v1/agent/check-tables":
			atomic.AddInt32(&checks, 1)
			_ = json.NewDecoder(r.Body).Decode(&checkBody)
			_ = json.NewEncoder(w).Encode(tableCheckResult)
		default:
			t.Errorf("unexpected orchestrator call: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("AGENT_ORCHESTRATOR_URL", srv.URL)

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectExec(`INSERT INTO connections`).WillReturnResult(sqlmock.NewResult(0, 1))
	stored := &tableCheckRecordArg{}
	mock.ExpectExec(storeTableCheckSQL).
		WithArgs(stored, sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{
		"name":            "acme-src",
		"connection_type": "source",
		"connector_type":  "acme-api",
		"config":          map[string]any{"api_token": "t0k-unit-test"},
	})
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections", CreateConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	if n := atomic.LoadInt32(&checks); n != 1 {
		t.Fatalf("expected one table check, got %d", n)
	}
	resp := decodeBody(t, w)
	answer, _ := resp["last_table_check"].(map[string]interface{})
	if answer["failed"] != float64(1) || resp["table_check_pending"] != nil {
		t.Fatalf("the save must answer the check, got %s", w.Body.String())
	}
	cfg, _ := checkBody["config"].(map[string]interface{})
	if cfg["connector_version"] != "v1.0.0" || checkBody["connection_id"] != resp["id"] {
		t.Fatalf("the check must run the new connection at its pinned version, got %v", checkBody)
	}
}

// A database connection is saved as before: no table check.
func TestCreateConnection_ADatabaseConnectionIsNotTableChecked(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	_, stop := fakeTestConnectionOrchestrator(t, true, "")
	defer stop()
	stubCheckTables(t, func(context.Context, string, map[string]interface{}, string) (map[string]interface{}, error) {
		t.Errorf("a database connection must not be checked")
		return nil, nil
	})
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectExec(`INSERT INTO connections`).WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{
		"name":            "pg-src",
		"connection_type": "source",
		"connector_type":  "pg-db",
		"config":          map[string]any{"host": "db.example.net"},
	})
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections", CreateConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeBody(t, w)
	if _, has := resp["last_table_check"]; has {
		t.Fatalf("no table check answer expected, got %s", w.Body.String())
	}
	if _, has := resp["table_check_pending"]; has {
		t.Fatalf("no table check answer expected, got %s", w.Body.String())
	}
}

// An edit of an API connection's settings clears the old check and checks again.
func TestUpdateConnection_AnAPIConnectionChecksItsTablesAgain(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	_, stop := fakeTestConnectionOrchestrator(t, true, "")
	defer stop()
	var gotConfig map[string]interface{}
	stubCheckTables(t, func(_ context.Context, _ string, config map[string]interface{}, _ string) (map[string]interface{}, error) {
		gotConfig = config
		return tableCheckResult, nil
	})

	enc, err := crypto.EncryptString(`{"api_token": "old-t0k"}`)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`SELECT wm\.role\s+FROM connections r`).
		WithArgs(wsScopeConnection, wsScopeUser, wsScopeWS).
		WillReturnRows(gateRoleRows("member"))
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(wsScopeConnection, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("acme-api"))
	mock.ExpectQuery(`SELECT config, connector_version FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(wsScopeConnection, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"config", "connector_version"}).AddRow(enc, "v1.0.0"))
	mock.ExpectExec(`INSERT INTO connection_access_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE connections SET config = \$1, .*last_table_check = NULL, updated_at = \$5 WHERE id = \$6 AND workspace_id = \$7`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(storeTableCheckSQL).
		WithArgs(&tableCheckRecordArg{}, wsScopeConnection, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := putConnection(t, map[string]interface{}{"config": map[string]interface{}{"api_token": "new-t0k"}})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("db expectations: %v", err)
	}
	if gotConfig["api_token"] != "new-t0k" || gotConfig["connector_version"] != "v1.0.0" {
		t.Fatalf("the check must read the edited settings at the pinned version, got %v", gotConfig)
	}
	answer, _ := decodeBody(t, w)["last_table_check"].(map[string]interface{})
	if answer["failed"] != float64(1) {
		t.Fatalf("the edit must answer the check, got %s", w.Body.String())
	}
}

func assertBuildFailedSaveResponse(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a failed build is not retryable, got Retry-After %q", got)
	}
	resp := decodeBody(t, w)
	if resp["error"] != "connector_build_failed" || resp["status"] != "connector_build_failed" {
		t.Fatalf("expected error/status=connector_build_failed, got error=%v status=%v", resp["error"], resp["status"])
	}
	// The build log is answered whole: its last line names the cause.
	if resp["test_error"] != buildFailedTestErr {
		t.Fatalf("test_error must be the whole build log, got %q", resp["test_error"])
	}
}

func TestCreateConnection_AFailedImageBuildAnswersItsLog(t *testing.T) {
	editSecretsEnv(t)
	_, stop := fakeTestConnectionOrchestrator(t, false, buildFailedTestErr)
	defer stop()
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	// Required to stay unfulfilled: nothing is saved.
	mock.ExpectExec(`INSERT INTO connections`).WillReturnResult(sqlmock.NewResult(0, 1))

	body, _ := json.Marshal(map[string]any{
		"name":            "acme-src",
		"connection_type": "source",
		"connector_type":  "acme-api",
		"config":          map[string]any{"api_token": "t0k-unit-test"},
	})
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections", CreateConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assertBuildFailedSaveResponse(t, w)
	if err := mock.ExpectationsWereMet(); err == nil || !strings.Contains(err.Error(), "INSERT INTO connections") {
		t.Fatalf("the INSERT must stay unfulfilled (nothing saved), got: %v", err)
	}
}

func TestUpdateConnection_AFailedImageBuildAnswersItsLog(t *testing.T) {
	editSecretsEnv(t)
	_, stop := fakeTestConnectionOrchestrator(t, false, buildFailedTestErr)
	defer stop()
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.MatchExpectationsInOrder(false)
	expectEditPreamble(t, mock)
	// Required to stay unfulfilled: the edit is not saved.
	mock.ExpectExec(`UPDATE connections SET`).WillReturnResult(sqlmock.NewResult(0, 1))

	w := putConnection(t, map[string]interface{}{
		"config": map[string]interface{}{
			"connection_string": "mongodb+srv://reader:s3cret@cluster1.example.net/shop",
			"database":          "shop",
		},
	})

	assertBuildFailedSaveResponse(t, w)
	if err := mock.ExpectationsWereMet(); err == nil || !strings.Contains(err.Error(), "UPDATE connections SET") {
		t.Fatalf("the UPDATE must stay unfulfilled (nothing saved), got: %v", err)
	}
}

// Test Connection shows the build log whole, as the save does.
func TestTestConnection_AFailedImageBuildKeepsItsLog(t *testing.T) {
	_, stop := fakeTestConnectionOrchestrator(t, false, buildFailedTestErr)
	defer stop()

	body, _ := json.Marshal(map[string]any{
		"connector_type": "acme-api",
		"config":         map[string]any{"api_token": "t0k-unit-test"},
	})
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections/test", TestConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeBody(t, w)
	if resp["status"] != "failed" {
		t.Fatalf("a failed build is a failed test, not a wait: status=%v", resp["status"])
	}
	if resp["error"] != buildFailedTestErr {
		t.Fatalf("error must be the whole build log, got %q", resp["error"])
	}
}

// The orchestrator's 502 carries the connector's reason it could not list its
// tables; that reason is what the record keeps.
func TestCheckTablesViaOrchestrator_KeepsTheListingFailure(t *testing.T) {
	var gotSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent/check-tables" {
			t.Errorf("unexpected orchestrator call: %s", r.URL.Path)
		}
		gotSecret = r.Header.Get("X-Internal-Secret")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":   "table check failed",
			"details": buildFailedTestErr,
		})
	}))
	defer srv.Close()
	t.Setenv("AGENT_ORCHESTRATOR_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-internal-secret")

	_, err := checkTablesViaOrchestrator(context.Background(), "acme-api", map[string]interface{}{}, wsScopeConnection)
	if err == nil || err.Error() != buildFailedTestErr {
		t.Fatalf("the error must be the orchestrator's details, got %v", err)
	}
	if gotSecret != "unit-test-internal-secret" {
		t.Fatalf("the call must carry the internal secret")
	}
	var rec map[string]interface{}
	if jerr := json.Unmarshal(tableCheckRecord(nil, err, time.Now()), &rec); jerr != nil {
		t.Fatal(jerr)
	}
	if rec["error"] != buildFailedTestErr || rec["checked_at"] == nil {
		t.Fatalf("the record must keep the build log whole, got %v", rec)
	}
}

// The connection page reads the stored check and whether to offer "Check tables".
func TestGetConnection_AnswersTheStoredTableCheck(t *testing.T) {
	editSecretsEnv(t)
	tableCheckToolsDir(t)
	enc, err := crypto.EncryptString("{}")
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	stored := `{"checked_at": "2026-10-05T12:00:00Z", "ok": 1, "failed": 1, "needs_setting": 0, "not_checked": 0}`
	for _, tc := range []struct {
		connectorType string
		check         interface{}
		supported     bool
	}{
		{"acme-api", []byte(stored), true},
		{"pg-db", nil, false},
	} {
		t.Run(tc.connectorType, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			now := time.Now()
			mock.ExpectQuery(`FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
				WithArgs(wsScopeConnection, wsScopeWS).
				WillReturnRows(sqlmock.NewRows([]string{
					"id", "user_id", "name", "type", "connector_type", "connector_version",
					"sync_mode", "cdc_mode", "description", "config", "status",
					"last_tested_at", "last_test_status", "last_test_error", "last_table_check", "created_at", "updated_at", "week_start", "fiscal_year_start_month",
				}).AddRow(
					wsScopeConnection, wsScopeUser, "conn", "source", tc.connectorType, "v1.0.0",
					nil, nil, nil, enc, "active",
					nil, nil, nil, tc.check, now, now, 1, 1,
				))

			r := wsScopeRouter(http.MethodGet, "/api/v1/connections/:id", GetConnection)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+wsScopeConnection, nil))

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			resp := decodeBody(t, w)
			if got, _ := resp["table_check_supported"].(bool); got != tc.supported {
				t.Fatalf("table_check_supported = %v, want %v", resp["table_check_supported"], tc.supported)
			}
			answer, has := resp["last_table_check"].(map[string]interface{})
			if tc.check == nil {
				if has {
					t.Fatalf("a never-checked connection has no last_table_check, got %v", answer)
				}
				return
			}
			if answer["failed"] != float64(1) || answer["checked_at"] != "2026-10-05T12:00:00Z" {
				t.Fatalf("last_table_check must be the stored record, got %v", resp["last_table_check"])
			}
		})
	}
}
