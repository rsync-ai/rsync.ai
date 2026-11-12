package handlers

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"api-gateway/internal/security"

	"github.com/DATA-DOG/go-sqlmock"
)

// REST CreatePipeline runs runPipelinePreflight before its INSERT; the chat create
// path did not (sibling found in #1293). A chat pipeline on a connection whose
// connector is not installed was created, then failed at run time. The class is
// "a create path skips a create gate", so both a behaviour test and a scan that
// every pipeline INSERT runs the same preflight.

const (
	chatPreflightSrc = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	chatPreflightDst = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// expectChatGatesBeforePreflight queues the reads the chat path makes before the
// preflight: the draft-lifecycle lookups (no row, so no draft refusal) and the
// workspace-tenancy checks (both connections in the workspace).
func expectChatGatesBeforePreflight(mock sqlmock.Sqlmock) {
	for _, id := range []string{chatPreflightSrc, chatPreflightDst} {
		mock.ExpectQuery(`SELECT connector_type, connector_version FROM connections`).
			WithArgs(id, wsScopeWS).WillReturnError(sql.ErrNoRows)
	}
	for _, id := range []string{chatPreflightSrc, chatPreflightDst} {
		mock.ExpectQuery(`SELECT 1 FROM connections`).
			WithArgs(id, wsScopeWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	}
}

func installedConnectors(t *testing.T, ids ...string) {
	t.Helper()
	files := map[string]string{}
	for _, id := range ids {
		files["public/"+id+"/latest.json"] = `{"current_version":"v1.0.0"}`
	}
	t.Setenv("TOOLS_DIR", makeConnectorDir(t, files))
}

// expectSourceInstalled installs connectorType and queues the preflight's lookup
// of the source connection, for chat-create tests about later steps.
func expectSourceInstalled(t *testing.T, mock sqlmock.Sqlmock, connID, connectorType string) {
	t.Helper()
	installedConnectors(t, connectorType)
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(connID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow(connectorType))
}

func TestChatCreatePipeline_ConnectorNotInstalledIsRefused(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	installedConnectors(t, "mongodb")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectChatGatesBeforePreflight(mock)
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(chatPreflightSrc, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("ghostdb"))

	id, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"sync ghostdb to mongo", wsScopeUser, "trace", chatPreflightSrc, chatPreflightDst,
		"batch", "", "", nlTransformSpec{}, true, nil, "")
	if err == nil || id != "" {
		t.Fatalf("create on an uninstalled connector: id=%q err=%v, want a refusal", id, err)
	}
	if !strings.Contains(err.Error(), `"ghostdb" is installed`) {
		t.Fatalf("refusal = %q, want the preflight's message naming the connector", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// Installed connectors pass the preflight and reach the INSERT. The INSERT fails
// on purpose so the test stops there.
func TestChatCreatePipeline_InstalledConnectorsReachInsert(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	installedConnectors(t, "postgresql", "mongodb")
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	expectChatGatesBeforePreflight(mock)
	for _, side := range [][2]string{{chatPreflightSrc, "postgresql"}, {chatPreflightDst, "mongodb"}} {
		mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
			WithArgs(side[0], wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow(side[1]))
	}
	stop := errors.New("stop at insert")
	mock.ExpectExec(`INSERT INTO pipelines`).WillReturnError(stop)

	_, _, _, err := (&ChatHandler{}).createAndRunPipeline(chatAuthzContext(t, string(security.WSMember)),
		"sync pg to mongo", wsScopeUser, "trace", chatPreflightSrc, chatPreflightDst,
		"batch", "", "", nlTransformSpec{}, true, nil, "")
	if !errors.Is(err, stop) && (err == nil || !strings.Contains(err.Error(), stop.Error())) {
		t.Fatalf("err = %v, want the INSERT to be reached", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// Every file that inserts a pipeline row runs runPipelinePreflight. A create path
// added later without it fails here.
func TestEveryPipelineInsertRunsPreflight(t *testing.T) {
	insert := regexp.MustCompile(`(?i)INSERT\s+INTO\s+pipelines\s*\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !insert.Match(src) {
			continue
		}
		found++
		if !bytes.Contains(src, []byte("runPipelinePreflight(")) {
			t.Errorf("%s inserts into pipelines but never calls runPipelinePreflight", f)
		}
	}
	if found < 2 {
		t.Fatalf("found %d files inserting pipelines, want at least 2 (chat and REST)", found)
	}
}
