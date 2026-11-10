package services

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"api-gateway/internal/db"

	"github.com/DATA-DOG/go-sqlmock"
)

// A connection runs on the connector version stored on it. RunPipeline
// resolves each connection here, so a connection still at "latest" is pinned
// here the first time a run resolves it, to the version "latest" means at
// that moment. A connector version published later then cannot move it.

const pinConnID = "44444444-4444-4444-4444-444444444444"

const pinSQL = `UPDATE connections SET connector_version = \$2 WHERE id = \$1 AND lower\(btrim\(connector_version\)\) IN \('', 'latest'\) RETURNING connector_version`

// pinConnectorsDir builds a postgresql connector whose latest.json names
// v1.0.1 while v1.0.0 still exists: a new version published after a
// connection started running on v1.0.0.
func pinConnectorsDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	conn := filepath.Join(base, "public", "database", "postgresql")
	for _, v := range []string{"v1.0.0", "v1.0.1"} {
		dir := filepath.Join(conn, "versions", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "connector.py"), []byte("# "+v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	latest := `{"current_version": "v1.0.1", "all_versions": ["v1.0.0", "v1.0.1"]}`
	if err := os.WriteFile(filepath.Join(conn, "latest.json"), []byte(latest), 0o644); err != nil {
		t.Fatal(err)
	}
	return base
}

func pinMockDB(t *testing.T) sqlmock.Sqlmock {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	prev := db.DB
	db.DB = mockDB
	t.Cleanup(func() {
		db.DB = prev
		_ = mockDB.Close()
	})
	return mock
}

func expectConnection(mock sqlmock.Sqlmock, version string) {
	mock.ExpectQuery(`SELECT user_id, connector_type, connector_version\s+FROM connections\s+WHERE id = \$1`).
		WithArgs(pinConnID).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "connector_type", "connector_version"}).
			AddRow("user-1", "postgresql", version))
}

func TestResolveConnectorVersion_PinsALatestConnection(t *testing.T) {
	base := pinConnectorsDir(t)
	mock := pinMockDB(t)
	expectConnection(mock, "latest")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.1").
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"}).AddRow("v1.0.1"))

	snap, err := NewVersionResolver(base).ResolveConnectorVersion(pinConnID)
	if err != nil {
		t.Fatalf("ResolveConnectorVersion: %v", err)
	}
	if snap.Version != "v1.0.1" {
		t.Fatalf("snapshot version = %q, want v1.0.1", snap.Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the pin must be written: %v", err)
	}
}

// The reported risk: a new connector version is published by mistake. A
// connection pinned to v1.0.0 keeps running v1.0.0.
func TestResolveConnectorVersion_PinnedConnectionIgnoresANewerLatest(t *testing.T) {
	base := pinConnectorsDir(t)
	mock := pinMockDB(t)
	expectConnection(mock, "v1.0.0")
	// No UPDATE is declared: sqlmock fails the call if the resolver writes one.

	snap, err := NewVersionResolver(base).ResolveConnectorVersion(pinConnID)
	if err != nil {
		t.Fatalf("ResolveConnectorVersion: %v", err)
	}
	if snap.Version != "v1.0.0" {
		t.Fatalf("snapshot version = %q, want the pinned v1.0.0 (latest.json says v1.0.1)", snap.Version)
	}
	if filepath.Base(snap.Path) != "v1.0.0" {
		t.Fatalf("snapshot path = %q, want versions/v1.0.0", snap.Path)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Another resolve (the orchestrator, or a concurrent run) pinned the row
// first. This run uses that pin and does not overwrite it.
func TestResolveConnectorVersion_ConcurrentPinWins(t *testing.T) {
	base := pinConnectorsDir(t)
	mock := pinMockDB(t)
	expectConnection(mock, "latest")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.1").
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"}))
	mock.ExpectQuery(`SELECT connector_version FROM connections WHERE id = \$1`).WithArgs(pinConnID).
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"}).AddRow("v1.0.0"))

	snap, err := NewVersionResolver(base).ResolveConnectorVersion(pinConnID)
	if err != nil {
		t.Fatalf("ResolveConnectorVersion: %v", err)
	}
	if snap.Version != "v1.0.0" || filepath.Base(snap.Path) != "v1.0.0" {
		t.Fatalf("snapshot = %s at %s, want the existing pin v1.0.0", snap.Version, snap.Path)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A failed pin never fails the run: it runs on what "latest" resolved to.
func TestResolveConnectorVersion_PinFailureStillResolves(t *testing.T) {
	base := pinConnectorsDir(t)
	mock := pinMockDB(t)
	expectConnection(mock, "")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.1").WillReturnError(errors.New("read-only transaction"))

	snap, err := NewVersionResolver(base).ResolveConnectorVersion(pinConnID)
	if err != nil {
		t.Fatalf("a pin failure must not fail the resolve: %v", err)
	}
	if snap.Version != "v1.0.1" {
		t.Fatalf("snapshot version = %q, want v1.0.1", snap.Version)
	}
}

func TestCurrentVersion(t *testing.T) {
	vr := NewVersionResolver(pinConnectorsDir(t))
	for _, typ := range []string{"postgresql", "postgres"} {
		v, err := vr.CurrentVersion(typ)
		if err != nil || v != "v1.0.1" {
			t.Errorf("CurrentVersion(%q) = %q, %v; want v1.0.1", typ, v, err)
		}
	}
	if v, err := vr.CurrentVersion("no-such-connector"); err == nil {
		t.Errorf("CurrentVersion(no-such-connector) = %q, want an error", v)
	}
}

func TestIsFloatingVersion(t *testing.T) {
	for _, v := range []string{"", " ", "latest", "LATEST", " latest "} {
		if !IsFloatingVersion(v) {
			t.Errorf("IsFloatingVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"v1.0.0", "1.0.0", "v2.0.0"} {
		if IsFloatingVersion(v) {
			t.Errorf("IsFloatingVersion(%q) = true, want false", v)
		}
	}
}
