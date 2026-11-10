package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// Generator-trust P4a: an overwrite rewrites the connector's current version in
// place, so it is refused while a run uses that version, and every successful save
// lists the workspace's connections a new version did not reach.

const (
	runsOnVersionSQL = `FROM executions e\s+JOIN pipelines p ON p.id = e.pipeline_id\s+JOIN connections c ON c.id IN \(p.source_connection_id, p.destination_connection_id\)`
	pinnedSQL        = `FROM connections\s+WHERE workspace_id = \$1\s+AND lower\(connector_type\) IN \(\$2, \$3\)\s+AND lower\(btrim\(connector_version\)\) NOT IN`
	theirWS          = "33333333-3333-3333-3333-333333333333"
)

var runsCols = []string{"exec_id", "pipeline_id", "pipeline_name", "workspace_id"}

// callGenerateAs calls GenerateConnector as a workspace admin of wsScopeWS.
func callGenerateAs(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/connectors/generate", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxWorkspaceID, wsScopeWS)
	c.Set(ctxWorkspaceRole, "admin")
	c.Set("user_id", wsScopeUser)
	GenerateConnector(c)
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return w, resp
}

// onDisk puts acme-crm v1.0.0 in the fake generator's catalog.
func onDisk(t *testing.T, g *fakeGenerator) {
	t.Helper()
	dir := filepath.Join(g.catalog, "acme-crm")
	mustWrite(t, filepath.Join(dir, "latest.json"), `{"current_version":"v1.0.0"}`)
	mustWrite(t, filepath.Join(dir, "versions", "v1.0.0", "metadata.json"), `{"id":"acme-crm"}`)
}

// workflows stands in for Temporal: the executions in ended have finished.
func workflows(t *testing.T, ended ...string) {
	t.Helper()
	prev := describeRunWorkflow
	describeRunWorkflow = func(_ context.Context, execID string) (bool, string) {
		for _, e := range ended {
			if e == execID {
				return true, "COMPLETED"
			}
		}
		return false, ""
	}
	t.Cleanup(func() { describeRunWorkflow = prev })
}

func expectSavedRun(mock sqlmock.Sqlmock) {
	expectAdmission(mock, 0)
	mock.ExpectQuery(genInsertSQL).
		WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).AddRow(genRunID, time.Now()))
	mock.ExpectCommit()
	mock.ExpectExec(genFinishSQL).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestAnOverwriteIsRefusedWhileARunUsesTheVersion(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
	onDisk(t, g)
	freshGenLimiter(t)
	workflows(t, "exec-ended")
	mock.ExpectQuery(runsOnVersionSQL).
		WithArgs("acme-crm", "acme_crm", "v1.0.0").
		WillReturnRows(sqlmock.NewRows(runsCols).
			AddRow("exec-1", "pipe-1", "Nightly CRM", wsScopeWS).
			AddRow("exec-1", "pipe-1", "Nightly CRM", wsScopeWS). // source and destination both on it
			AddRow("exec-2", "pipe-9", "Their CRM", theirWS).
			AddRow("exec-ended", "pipe-2", "Stale row", wsScopeWS))

	w, resp := callGenerateAs(t, `{"api_name":"acme-crm","overwrite":true,"openapi_spec":"{}"}`)

	if w.Code != http.StatusConflict || resp["code"] != "connector_in_use" {
		t.Fatalf("status %d code %v, want 409 connector_in_use: %s", w.Code, resp["code"], w.Body.String())
	}
	if g.generates.Load() != 0 {
		t.Error("the generator was called: the version would have been rewritten under a running pipeline")
	}
	msg, _ := resp["error_message"].(string)
	if !strings.Contains(msg, "Nightly CRM") || !strings.Contains(msg, "1 run(s) in other workspaces") {
		t.Errorf("message %q does not name this workspace's pipeline and count the other's", msg)
	}
	if strings.Contains(msg, "Their CRM") || strings.Contains(w.Body.String(), "pipe-9") {
		t.Error("another workspace's pipeline was named")
	}
	if strings.Contains(msg, "Stale row") {
		t.Error("a run whose workflow ended blocked the overwrite")
	}
	if pipes, _ := resp["pipelines"].([]any); len(pipes) != 1 {
		t.Errorf("pipelines = %v, want the one pipeline once", resp["pipelines"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAnOverwriteFailsClosedWhenTheRunsCannotBeChecked(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
	onDisk(t, g)
	freshGenLimiter(t)
	mock.ExpectQuery(runsOnVersionSQL).WillReturnError(errors.New("connection reset"))

	w, resp := callGenerateAs(t, `{"api_name":"acme-crm","overwrite":true,"openapi_spec":"{}"}`)

	if w.Code != http.StatusServiceUnavailable || resp["code"] != "connector_in_use_unknown" {
		t.Fatalf("status %d code %v, want 503 connector_in_use_unknown", w.Code, resp["code"])
	}
	if g.generates.Load() != 0 {
		t.Error("the generator was called although the runs could not be checked")
	}
	if strings.Contains(w.Body.String(), "connection reset") {
		t.Error("the database error reached the caller")
	}
}

func TestAnOverwriteWithNoRunGoesThroughAndListsPinnedConnections(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK,
		`{"success": true, "version": "v1.0.0", "overwritten": true, "operation_count": 3}`)
	onDisk(t, g)
	freshGenLimiter(t)
	workflows(t, "exec-ended")
	mock.ExpectQuery(runsOnVersionSQL).
		WithArgs("acme-crm", "acme_crm", "v1.0.0").
		WillReturnRows(sqlmock.NewRows(runsCols).AddRow("exec-ended", "pipe-2", "Stale row", wsScopeWS))
	expectSavedRun(mock)
	mock.ExpectQuery(pinnedSQL).
		WithArgs(wsScopeWS, "acme-crm", "acme_crm", "v1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_version"}).
			AddRow("conn-1", "CRM (old)", "v0.9.0"))

	w, resp := callGenerateAs(t, `{"api_name":"acme-crm","overwrite":true,"openapi_spec":"{}"}`)

	if w.Code != http.StatusOK || resp["success"] != true {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body, _ := g.lastRequest()
	if body["overwrite"] != true {
		t.Errorf("the generator got overwrite=%v; the already-exists short-circuit or the field dropped it", body["overwrite"])
	}
	pinned, _ := resp["pinned_connections"].([]any)
	if len(pinned) != 1 || pinned[0].(map[string]any)["connector_version"] != "v0.9.0" {
		t.Errorf("pinned_connections = %v", resp["pinned_connections"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestANewVersionStillListsPinnedConnectionsAndSkipsTheRunCheck(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, `{"success": true, "version": "v1.0.1"}`)
	onDisk(t, g)
	freshGenLimiter(t)
	expectSavedRun(mock) // no runs query: a new version stops no container
	mock.ExpectQuery(pinnedSQL).
		WithArgs(wsScopeWS, "acme-crm", "acme_crm", "v1.0.1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_version"}))

	w, resp := callGenerateAs(t, `{"api_name":"acme-crm","force_regenerate":true,"openapi_spec":"{}"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body, _ := g.lastRequest()
	if _, sent := body["overwrite"]; sent {
		t.Error("overwrite was sent although the user did not ask for it")
	}
	if pinned, ok := resp["pinned_connections"].([]any); !ok || len(pinned) != 0 {
		t.Errorf("pinned_connections = %#v, want an empty list", resp["pinned_connections"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAnOverwriteOfAConnectorNotOnDiskChecksNoRuns(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
	freshGenLimiter(t)
	expectSavedRun(mock)
	mock.ExpectQuery(pinnedSQL).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_version"}))

	w, _ := callGenerateAs(t, `{"api_name":"acme-crm","overwrite":true,"openapi_spec":"{}"}`)

	if w.Code != http.StatusOK || g.generates.Load() != 1 {
		t.Fatalf("status %d, generates %d: %s", w.Code, g.generates.Load(), w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAnOverwriteCannotBeADryRun(t *testing.T) {
	for _, body := range []string{
		`{"api_name":"acme-crm","overwrite":true,"dry_run":true}`,
		`{"api_name":"acme-crm","overwrite":true,"save_artifacts":false}`,
	} {
		_, cleanup := wsScopeMockDB(t)
		g := newFakeGenerator(t, http.StatusOK, `{}`, http.StatusOK, genOK)
		onDisk(t, g)
		w, _ := callGenerateAs(t, body)
		cleanup()
		if w.Code != http.StatusBadRequest || g.generates.Load() != 0 {
			t.Errorf("%s: status %d, generates %d, want 400 and no generation", body, w.Code, g.generates.Load())
		}
	}
}

// ---- POST /connectors/:name/connections/version ----

const lockConnectionsSQL = `SELECT id::text, COALESCE\(name, ''\), connector_version\s+FROM connections\s+WHERE id::text = ANY \(string_to_array\(\$1, \$2\)\) AND workspace_id = \$3 AND lower\(connector_type\) IN \(\$4, \$5\)\s+ORDER BY name, id\s+FOR UPDATE`

func versionCatalog(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "public", "acme-crm")
	mustWrite(t, filepath.Join(dir, "latest.json"), `{"current_version":"v1.0.1","all_versions":["v1.0.0","v1.0.1"]}`)
	mustWrite(t, filepath.Join(dir, "versions", "v1.0.0", "metadata.json"), `{"id":"acme-crm"}`)
	mustWrite(t, filepath.Join(dir, "versions", "v1.0.1", "metadata.json"), `{"id":"acme-crm"}`)
	t.Setenv("MCP_CONNECTORS_PATH", base)
}

func callMove(t *testing.T, role, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/connectors/acme-crm/connections/version", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "name", Value: "acme-crm"}}
	c.Set(ctxWorkspaceID, wsScopeWS)
	c.Set(ctxWorkspaceRole, role)
	c.Set("user_id", wsScopeUser)
	MoveConnectionsToConnectorVersion(c)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

func TestPinnedConnectionsMoveToTheNewVersionTogether(t *testing.T) {
	versionCatalog(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery(lockConnectionsSQL).
		WithArgs("conn-1"+idListSep+"conn-2", idListSep, wsScopeWS, "acme-crm", "acme_crm").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_version"}).
			AddRow("conn-1", "CRM A", "v1.0.0").AddRow("conn-2", "CRM B", "v1.0.0"))
	mock.ExpectExec(`UPDATE connections SET connector_version = \$1, updated_at = NOW\(\)\s+WHERE id::text = ANY \(string_to_array\(\$2, \$3\)\) AND workspace_id = \$4`).
		WithArgs("v1.0.1", "conn-1"+idListSep+"conn-2", idListSep, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(0, 1))

	w, resp := callMove(t, "member", `{"version":"v1.0.1","connection_ids":["conn-1","conn-2","conn-1"," "]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if conns, _ := resp["connections"].([]any); len(conns) != 2 {
		t.Errorf("connections = %v, want both with their old version", resp["connections"])
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "next manual run") {
		t.Errorf("message %q does not say when pipelines pick the version up", msg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAMoveWithAConnectionOutsideTheWorkspaceChangesNothing(t *testing.T) {
	versionCatalog(t)
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery(lockConnectionsSQL).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "connector_version"}).AddRow("conn-1", "CRM A", "v1.0.0"))
	mock.ExpectRollback()

	w, _ := callMove(t, "member", `{"version":"v1.0.1","connection_ids":["conn-1","someone-elses"]}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err) // no UPDATE was expected: an unexpected one fails here
	}
}

func TestAMoveIsRefusedBeforeTheDatabase(t *testing.T) {
	versionCatalog(t)
	cases := []struct {
		name, role, body string
		want             int
	}{
		{"a viewer", "viewer", `{"version":"v1.0.1","connection_ids":["conn-1"]}`, http.StatusForbidden},
		{"a version not on disk", "member", `{"version":"v2.0.0","connection_ids":["conn-1"]}`, http.StatusNotFound},
		{"latest (not a version)", "member", `{"version":"latest","connection_ids":["conn-1"]}`, http.StatusBadRequest},
		{"no connections", "member", `{"version":"v1.0.1","connection_ids":[]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			w, _ := callMove(t, tc.role, tc.body)
			if w.Code != tc.want {
				t.Errorf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
