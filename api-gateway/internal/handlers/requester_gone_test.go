package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

// A user's Stop is not a server failure. The Explorer's Stop aborts the
// browser's fetch, and the in-app assistant's Stop cancels the question that
// agentgw runs these handlers on in-process; either way the request's context
// is cancelled mid-flight. GetSchemaIndex logged that as
// "level:error [GetSchemaIndex] Failed to build schema index: … assistant:
// question stopped" and answered 500 (seen on prod 2026-10-01). The class is
// every Explorer handler whose work runs on the request's context: each one
// answers a Stop with 499 and logs nothing at warn or above.

// explorerStopConfigs is the stored config per connector type: a MongoDB connection
// browses through the orchestrator, a PostgreSQL one through a direct driver.
var explorerStopConfigs = map[string]string{
	"mongodb":    `{"connection_string":"mongodb://h/shop","database":"shop"}`,
	"postgresql": `{"host":"127.0.0.1","port":1,"database":"shop","username":"u","password":"p"}`,
}

func explorerStopConnection(connectorType string) func(*testing.T, sqlmock.Sqlmock) {
	return func(t *testing.T, mock sqlmock.Sqlmock) {
		t.Helper()
		enc, err := crypto.EncryptString(explorerStopConfigs[connectorType])
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		mock.ExpectQuery(`SELECT connector_type, config\s+FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
			WithArgs(docFindConnID, wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config"}).AddRow(connectorType, enc))
	}
}

func explorerStopConnectionExists(then func(*testing.T, sqlmock.Sqlmock)) func(*testing.T, sqlmock.Sqlmock) {
	return func(t *testing.T, mock sqlmock.Sqlmock) {
		mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM connections WHERE id = \$1 AND workspace_id = \$2\)`).
			WithArgs(docFindConnID, wsScopeWS).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		if then != nil {
			then(t, mock)
		}
	}
}

func explorerStopConnectorType(t *testing.T, mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(docFindConnID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("postgresql"))
}

type explorerStopCase struct {
	name    string
	method  string
	route   string
	url     string
	body    string
	handler gin.HandlerFunc
	setup   func(*testing.T, sqlmock.Sqlmock)
}

func explorerStopCases() []explorerStopCase {
	conn := `"connection_id":"` + docFindConnID + `"`
	index := "/api/v1/explorer/connections/:id/schema-index"
	indexURL := "/api/v1/explorer/connections/" + docFindConnID + "/schema-index"
	return []explorerStopCase{
		{"schema index (document)", http.MethodGet, index, indexURL, "", GetSchemaIndex, explorerStopConnection("mongodb")},
		{"schema index retry (document)", http.MethodGet, index, indexURL + "?refresh=true", "", GetSchemaIndex, explorerStopConnection("mongodb")},
		{"schema index refresh (document)", http.MethodPost, index + "/refresh", indexURL + "/refresh", "", RefreshSchemaIndex, explorerStopConnection("mongodb")},
		{"schema index (sql)", http.MethodGet, index, indexURL, "", GetSchemaIndex, explorerStopConnection("postgresql")},
		{"schema index refresh (sql)", http.MethodPost, index + "/refresh", indexURL + "/refresh", "", RefreshSchemaIndex, explorerStopConnection("postgresql")},
		{"run query", http.MethodPost, "/q", "/q", `{` + conn + `,"sql":"SELECT 1"}`, ExecuteExplorerQuery, explorerStopConnection("postgresql")},
		{"export (GET)", http.MethodGet, "/x", "/x?connection_id=" + docFindConnID + "&sql=SELECT%201", "", ExportCSVHandler, explorerStopConnection("postgresql")},
		{"export (POST)", http.MethodPost, "/x", "/x", `{` + conn + `,"sql":"SELECT 1","format":"csv"}`, ExportQueryHandler, explorerStopConnection("postgresql")},
		{"retrieve tables", http.MethodPost, "/r", "/r", `{` + conn + `,"question":"orders"}`, RetrieveTables, explorerStopConnectionExists(explorerStopConnection("mongodb"))},
		{"resolve tables", http.MethodPost, "/r", "/r", `{` + conn + `,"question":"orders"}`, ResolveExplorerTables, explorerStopConnection("mongodb")},
		{"resolve columns", http.MethodPost, "/r", "/r", `{` + conn + `,"question":"orders","selected_tables":[{"name":"orders","schema":"shop","columns":[]}]}`, ResolveExplorerColumns, explorerStopConnectionExists(nil)},
		{"next steps", http.MethodPost, "/n", "/n", `{"question":"orders per day","sql":"SELECT 1","result_profile":{"row_count":1}}`, GetExplorerNextSteps, nil},
		{"generate sql", http.MethodPost, "/g", "/g", `{` + conn + `,"question":"orders per day","schema":"shop.orders(id)"}`, GenerateSQL, explorerStopConnectorType},
		{"find documents", http.MethodPost, "/f", "/f", `{` + conn + `,"collection":"customers"}`, FindExplorerDocuments, explorerStopConnection("mongodb")},
		{"aggregate documents", http.MethodPost, "/a", "/a", `{` + conn + `,"collection":"customers","pipeline":[{"$match":{}}]}`, AggregateExplorerDocuments, explorerStopConnection("mongodb")},
	}
}

// serveExplorerStopped runs one case against an unreachable orchestrator and llm-service,
// on ctx, and returns the answer and every log entry at warn or above.
func serveExplorerStopped(t *testing.T, tc explorerStopCase, ctx context.Context) (*httptest.ResponseRecorder, []*logrus.Entry) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-secret")
	for _, env := range []string{"ORCHESTRATOR_URL", "LLM_SERVICE_URL", "TEXT2SQL_ENDPOINT"} {
		t.Setenv(env, "http://127.0.0.1:1")
	}
	prevCache := explorerCache
	explorerCache = nil
	t.Cleanup(func() { explorerCache = prevCache })

	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	if tc.setup != nil {
		tc.setup(t, mock)
	}

	std := logrus.StandardLogger()
	previous := std.ReplaceHooks(make(logrus.LevelHooks))
	hook := logrustest.NewLocal(std)
	defer std.ReplaceHooks(previous)

	req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body)).WithContext(ctx)
	if tc.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	wsScopeRouter(tc.method, tc.route, tc.handler).ServeHTTP(rr, req)

	var loud []*logrus.Entry
	for _, e := range hook.AllEntries() {
		if e.Level <= logrus.WarnLevel {
			loud = append(loud, e)
		}
	}
	return rr, loud
}

func TestExplorer_AStoppedRequestIsNotAServerError(t *testing.T) {
	for _, tc := range explorerStopCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Control: the same request, nobody stopped it, the remote is down.
			// It reaches the work that fails and answers that failure, not 499.
			rr, _ := serveExplorerStopped(t, tc, context.Background())
			if rr.Code == statusClientClosed || rr.Code == http.StatusNotFound || rr.Code == http.StatusBadRequest {
				t.Fatalf("control: status %d before any stop: %s", rr.Code, rr.Body.String())
			}

			// The same request, stopped the way the assistant's Stop does it.
			ctx, stop := context.WithCancelCause(context.Background())
			stop(errors.New("assistant: question stopped"))
			rr, loud := serveExplorerStopped(t, tc, ctx)
			if rr.Code != statusClientClosed {
				t.Errorf("status = %d, want %d (the asker went away): %s", rr.Code, statusClientClosed, rr.Body.String())
			}
			for _, e := range loud {
				t.Errorf("a Stop was logged at %s: %s", e.Level, e.Message)
			}
		})
	}
}

// A deadline is a timeout, not a Stop: it stays a failure.
func TestRequesterGone_IsACancelNotADeadline(t *testing.T) {
	cancelled, stop := context.WithCancelCause(context.Background())
	stop(errors.New("assistant: question stopped"))
	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	derived, cancelDerived := context.WithTimeout(cancelled, 0)
	defer cancelDerived()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"live", context.Background(), false},
		{"stopped", cancelled, true},
		{"timed out", expired, false},
		{"a timeout derived from a stopped request", derived, true},
	} {
		if got := requesterGone(tc.ctx); got != tc.want {
			t.Errorf("%s: requesterGone = %v, want %v", tc.name, got, tc.want)
		}
	}
}
