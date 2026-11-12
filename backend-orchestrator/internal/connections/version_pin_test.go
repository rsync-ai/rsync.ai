package connections

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/rsync-ai/shared/crypto"
)

// A connection runs on the connector version stored on it. One still at
// "latest" is pinned the first time it is resolved, to the version "latest"
// means at that moment, so a connector version published later cannot move a
// pipeline that already runs.

const pinConnID = "33333333-3333-3333-3333-333333333333"

// pinSQL is the guarded write: only a row whose version still floats is
// touched, so an existing pin is never moved.
const pinSQL = `UPDATE connections SET connector_version = \$2 WHERE id = \$1 AND lower\(btrim\(connector_version\)\) IN \('', 'latest'\) RETURNING connector_version`

// fakeLatest stands in for the MCP server manager: it answers what "latest"
// resolves to from latest.json today.
type fakeLatest struct {
	current string
	calls   int
}

func (f *fakeLatest) ResolveConcreteVersion(_ string, version string) (string, error) {
	f.calls++
	if version != "" && version != "latest" {
		return version, nil
	}
	if f.current == "" {
		return "", errors.New("latest.json missing")
	}
	return f.current, nil
}

func pinTestEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("ENVIRONMENT", "development")
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"host":"db.internal","port":5432}`)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	return enc
}

func expectConnRow(mock sqlmock.Sqlmock, enc, version string) {
	mock.ExpectQuery(`SELECT type, connector_type, connector_version, config FROM connections WHERE id = \$1 AND user_id = \$2`).
		WithArgs(pinConnID, "user-1").
		WillReturnRows(sqlmock.NewRows([]string{"type", "connector_type", "connector_version", "config"}).
			AddRow("destination", "postgresql", version, enc))
}

func TestGetForUser_PinsALatestConnectionOnFirstResolve(t *testing.T) {
	enc := pinTestEnv(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectConnRow(mock, enc, "latest")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.0").
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"}).AddRow("v1.0.0"))

	m := NewManager(db).PinVersionsWith(&fakeLatest{current: "v1.0.0"})
	cfg, err := m.GetForUser(context.Background(), "user-1", pinConnID)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if got := cfg["connector_version"]; got != "v1.0.0" {
		t.Fatalf("connector_version = %q, want the pinned v1.0.0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the pin must be written: %v", err)
	}
}

// The reported risk: someone publishes a new connector version by mistake.
// A pinned connection keeps running the version it was pinned to.
func TestGetForUser_PinnedConnectionIgnoresANewerLatest(t *testing.T) {
	enc := pinTestEnv(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectConnRow(mock, enc, "v1.0.0")
	// No UPDATE is declared: sqlmock fails the call if the manager writes one.

	latest := &fakeLatest{current: "v1.0.1"}
	m := NewManager(db).PinVersionsWith(latest)
	cfg, err := m.GetForUser(context.Background(), "user-1", pinConnID)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if got := cfg["connector_version"]; got != "v1.0.0" {
		t.Fatalf("connector_version = %q, want the pinned v1.0.0 (latest.json says v1.0.1)", got)
	}
	if latest.calls != 0 {
		t.Fatalf("a pinned connection must not consult latest.json, got %d calls", latest.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Two resolves race: the other one pinned first. This one must run on that
// pin, not on what it resolved itself, and must not overwrite it.
func TestGetForUser_ConcurrentPinWins(t *testing.T) {
	enc := pinTestEnv(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectConnRow(mock, enc, "latest")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.1").
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"})) // row already pinned
	mock.ExpectQuery(`SELECT connector_version FROM connections WHERE id = \$1`).WithArgs(pinConnID).
		WillReturnRows(sqlmock.NewRows([]string{"connector_version"}).AddRow("v1.0.0"))

	m := NewManager(db).PinVersionsWith(&fakeLatest{current: "v1.0.1"})
	cfg, err := m.GetForUser(context.Background(), "user-1", pinConnID)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if got := cfg["connector_version"]; got != "v1.0.0" {
		t.Fatalf("connector_version = %q, want the pin already on the row (v1.0.0)", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A failed pin never fails the run: it runs on what "latest" resolved to.
func TestGetForUser_PinWriteFailureStillRunsTheResolvedVersion(t *testing.T) {
	enc := pinTestEnv(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectConnRow(mock, enc, "latest")
	mock.ExpectQuery(pinSQL).WithArgs(pinConnID, "v1.0.0").WillReturnError(errors.New("read-only transaction"))

	m := NewManager(db).PinVersionsWith(&fakeLatest{current: "v1.0.0"})
	cfg, err := m.GetForUser(context.Background(), "user-1", pinConnID)
	if err != nil {
		t.Fatalf("GetForUser must not fail on a pin error: %v", err)
	}
	if got := cfg["connector_version"]; got != "v1.0.0" {
		t.Fatalf("connector_version = %q, want v1.0.0", got)
	}
}

// A manager built without a resolver (sentinel, healer, assessment) reads the
// row as stored and never writes it.
func TestGet_WithoutResolverLeavesLatestUntouched(t *testing.T) {
	enc := pinTestEnv(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	expectConnRow(mock, enc, "latest")

	cfg, err := NewManager(db).GetForUser(context.Background(), "user-1", pinConnID)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if got := cfg["connector_version"]; got != "latest" {
		t.Fatalf("connector_version = %q, want latest", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePinned_ExplicitVersionIsNeverWritten(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got := ResolvePinned(context.Background(), db, &fakeLatest{current: "v1.0.1"}, pinConnID, "postgresql", "v1.0.0")
	if got != "v1.0.0" {
		t.Fatalf("ResolvePinned = %q, want v1.0.0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolvePinned_UnresolvableLatestStaysLatest(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// latest.json missing: nothing to pin, and the caller keeps today's behaviour.
	if got := ResolvePinned(context.Background(), db, &fakeLatest{}, pinConnID, "postgresql", "latest"); got != "latest" {
		t.Fatalf("ResolvePinned = %q, want latest", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
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
