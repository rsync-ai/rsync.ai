package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A new connection is created pinned: with no version asked for (or
// "latest"), it stores the version latest.json names today, so a connector
// version published later cannot move the pipelines built on it.

func pinToolsDir(t *testing.T, current string) string {
	t.Helper()
	base := t.TempDir()
	conn := filepath.Join(base, "public", "database", "postgresql")
	if err := os.MkdirAll(filepath.Join(conn, "versions", current), 0o755); err != nil {
		t.Fatal(err)
	}
	latest := `{"current_version": "` + current + `", "all_versions": ["` + current + `"]}`
	if err := os.WriteFile(filepath.Join(conn, "latest.json"), []byte(latest), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func createConnectionStoresVersion(t *testing.T, requested any, want string) {
	t.Helper()
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectExec(`INSERT INTO connections`).
		WithArgs(
			sqlmock.AnyArg(), // id
			wsScopeUser,      // user_id
			sqlmock.AnyArg(), // name
			sqlmock.AnyArg(), // type
			sqlmock.AnyArg(), // connector_type
			want,             // connector_version  <-- the assertion
			sqlmock.AnyArg(), // sync_mode
			sqlmock.AnyArg(), // cdc_mode
			sqlmock.AnyArg(), // description
			sqlmock.AnyArg(), // config
			sqlmock.AnyArg(), // status
			sqlmock.AnyArg(), // last_tested_at
			sqlmock.AnyArg(), // last_test_status
			sqlmock.AnyArg(), // created_at
			sqlmock.AnyArg(), // updated_at
			wsScopeWS,        // workspace_id
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	payload := map[string]any{
		"name":            "pg-src",
		"connection_type": "source",
		"connector_type":  "postgresql",
		"force_save":      true, // skip the pre-save connectivity test
		"config":          map[string]any{"host": "localhost"},
	}
	if requested != nil {
		payload["connector_version"] = requested
	}
	body, _ := json.Marshal(payload)
	r := wsScopeRouter(http.MethodPost, "/api/v1/connections", CreateConnection)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("connector_version stored must be %q: %v", want, err)
	}
}

func TestCreateConnection_StoresTheCurrentVersionNotLatest(t *testing.T) {
	t.Setenv("TOOLS_DIR", pinToolsDir(t, "v1.0.0"))
	createConnectionStoresVersion(t, nil, "v1.0.0")
}

func TestCreateConnection_ExplicitLatestIsPinnedToo(t *testing.T) {
	t.Setenv("TOOLS_DIR", pinToolsDir(t, "v1.0.0"))
	createConnectionStoresVersion(t, "latest", "v1.0.0")
}

func TestCreateConnection_KeepsAnExplicitVersion(t *testing.T) {
	t.Setenv("TOOLS_DIR", pinToolsDir(t, "v2.0.0"))
	createConnectionStoresVersion(t, "v1.0.0", "v1.0.0")
}

// No latest.json to read (a connector the gateway cannot see): the connection
// is created as before, at "latest", and its first run pins it.
func TestCreateConnection_UnresolvableVersionStaysLatest(t *testing.T) {
	t.Setenv("TOOLS_DIR", t.TempDir())
	createConnectionStoresVersion(t, nil, "latest")
}
