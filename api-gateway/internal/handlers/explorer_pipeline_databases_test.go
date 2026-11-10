package handlers

// KI-EXPLORER-PIPELINE-DEST-DB-UNREACHABLE: a MongoDB connection that names a
// database is pinned to it, but a pipeline writing to that connection lands its
// rows in its own destination database. Explorer listed only the pinned one and
// the connector refused a find anywhere else, so the data a pipeline delivered
// could not be browsed from the connection it was delivered to (prod pipeline
// 9a094389 -> ctl0926_pg_mongo_batch on a connection pinned to demo). Explorer
// now also reaches the databases this workspace's pipelines write to on the
// connection, and nothing else.
//
// Bug class: a read scope derived from the connection alone, when a pipeline
// can widen where that connection writes. Both Explorer reads (the schema tree
// and a find) take the scope from the same helper, so both are pinned here.

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"

	"api-gateway/internal/db"
)

const pipelineDestDB = "ctl0926_pg_mongo_batch"

// pipelineDatabasesQuery matches the lookup of the databases this workspace's
// pipelines write to on the connection, spelled with the one namespace
// expression every ownership query shares.
var pipelineDatabasesQuery = `(?s)SELECT DISTINCT ` + regexp.QuoteMeta(pipelineNamespaceExpr) +
	`.*FROM pipelines p.*p\.workspace_id::text = \$1.*p\.destination_connection_id::text = \$2`

// explorerDBMock installs a sqlmock as the gateway's database and records every
// query it is sent, so a test can prove a lookup did NOT run (sqlmock alone only
// reports expectations that were not met).
func explorerDBMock(t *testing.T) (sqlmock.Sqlmock, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		mu.Lock()
		sent = append(sent, actual)
		mu.Unlock()
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = mockDB
	t.Cleanup(func() {
		db.DB = prev
		_ = mockDB.Close()
	})
	return mock, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

func expectPipelineDatabases(mock sqlmock.Sqlmock, names ...string) {
	rows := sqlmock.NewRows([]string{"namespace"})
	for _, n := range names {
		rows.AddRow(n)
	}
	mock.ExpectQuery(pipelineDatabasesQuery).WithArgs(wsScopeWS, docFindConnID).WillReturnRows(rows)
}

// orchestratorCalls answers each POST to path with the next of bodies (the last
// one repeats) and records the config every call carried.
func orchestratorCalls(t *testing.T, path string, bodies ...string) func() []map[string]interface{} {
	t.Helper()
	var mu sync.Mutex
	var configs []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Config map[string]interface{} `json:"config"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		configs = append(configs, req.Config)
		body := bodies[len(bodies)-1]
		if len(configs) <= len(bodies) {
			body = bodies[len(configs)-1]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ORCHESTRATOR_URL", srv.URL)
	return func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), configs...)
	}
}

func serveSchemaIndex(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	prevCache := explorerCache
	explorerCache = nil
	t.Cleanup(func() { explorerCache = prevCache })
	rr := httptest.NewRecorder()
	wsScopeRouter(http.MethodGet, "/api/v1/explorer/connections/:id/schema-index", GetSchemaIndex).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/explorer/connections/"+docFindConnID+"/schema-index", nil))
	return rr
}

// wantPinned fails unless config still reaches only the database it names.
func wantPinned(t *testing.T, config map[string]interface{}) {
	t.Helper()
	if config["database"] != "shop" {
		t.Errorf("database = %v, want the connection's own shop", config["database"])
	}
	for _, k := range []string{"namespace_filter_mode", "namespace_filter_patterns"} {
		if _, ok := config[k]; ok {
			t.Errorf("a pinned read carries %s=%v", k, config[k])
		}
	}
}

// wantWidened fails unless config reaches exactly patterns: no database named
// (a named one pins the connector to it), and an include scope.
func wantWidened(t *testing.T, config map[string]interface{}, patterns string) {
	t.Helper()
	for _, k := range []string{"database", "db_name", "db"} {
		if v, ok := config[k]; ok && v != "" {
			t.Errorf("%s = %v, want it blank so the scope applies", k, v)
		}
	}
	if config["namespace_filter_mode"] != "include" || config["namespace_filter_patterns"] != patterns {
		t.Errorf("scope = (%v, %v), want (include, %s)", config["namespace_filter_mode"], config["namespace_filter_patterns"], patterns)
	}
	if config["connection_string"] != "mongodb://h/shop" {
		t.Errorf("the rest of the config was not kept: %v", config)
	}
}

func TestExplorerTreeListsTheDatabasesPipelinesWriteTo(t *testing.T) {
	mock, _ := explorerDBMock(t)
	docFindConnection(t, mock, "mongodb")
	// The connection's own database, the blank and "default" namespaces (no
	// namespace of their own) and names that are not a database are dropped.
	expectPipelineDatabases(mock, pipelineDestDB, "shop", "", "default", "a.b", "x*y", "p,q", "orders_db")
	configs := orchestratorCalls(t, "/api/v1/agent/discover-schema", `{"overall_status":"success","tables":[
		{"name":"orders","schema":"shop","columns":[]},
		{"name":"customers","schema":"`+pipelineDestDB+`","columns":[]}]}`)

	rr := serveSchemaIndex(t)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	sent := configs()
	if len(sent) != 1 {
		t.Fatalf("discovery calls = %d, want 1", len(sent))
	}
	wantWidened(t, sent[0], "shop,"+pipelineDestDB+",orders_db")
	if !strings.Contains(rr.Body.String(), `"schema":"`+pipelineDestDB+`"`) {
		t.Errorf("the pipeline's database is not in the tree: %s", rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A wider listing that fails (a login that cannot list databases, say) must not
// cost the user the database they could browse before.
func TestExplorerTreeFallsBackToThePinnedDatabase(t *testing.T) {
	mock, _ := explorerDBMock(t)
	docFindConnection(t, mock, "mongodb")
	expectPipelineDatabases(mock, pipelineDestDB)
	configs := orchestratorCalls(t, "/api/v1/agent/discover-schema",
		`{"overall_status":"failed","tables":[],"warnings_messages":["not authorized on admin to execute command listDatabases"]}`,
		`{"overall_status":"success","tables":[{"name":"orders","schema":"shop","columns":[]}]}`)

	rr := serveSchemaIndex(t)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	sent := configs()
	if len(sent) != 2 {
		t.Fatalf("discovery calls = %d, want the widened one then the pinned one", len(sent))
	}
	wantWidened(t, sent[0], "shop,"+pipelineDestDB)
	wantPinned(t, sent[1])
	if !strings.Contains(rr.Body.String(), `"name":"orders"`) {
		t.Errorf("the pinned database's collections are missing: %s", rr.Body.String())
	}
}

// Controls: no pipeline database to add, a lookup that fails, and a
// server-level connection (its Scope is the user's) all read as before.
func TestExplorerTreeWithNoPipelineDatabaseIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{"only the connection's own database", func(m sqlmock.Sqlmock) { expectPipelineDatabases(m, "shop", "") }},
		{"no pipelines", func(m sqlmock.Sqlmock) { expectPipelineDatabases(m) }},
		{"the lookup fails", func(m sqlmock.Sqlmock) {
			m.ExpectQuery(pipelineDatabasesQuery).WillReturnError(sql.ErrConnDone)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, _ := explorerDBMock(t)
			docFindConnection(t, mock, "mongodb")
			tc.setup(mock)
			configs := orchestratorCalls(t, "/api/v1/agent/discover-schema",
				`{"overall_status":"success","tables":[{"name":"orders","schema":"shop","columns":[]}]}`)

			if rr := serveSchemaIndex(t); rr.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
			}
			sent := configs()
			if len(sent) != 1 {
				t.Fatalf("discovery calls = %d, want 1", len(sent))
			}
			wantPinned(t, sent[0])
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("a server-level connection is not looked up", func(t *testing.T) {
		mock, queries := explorerDBMock(t)
		serverLevelConnection(t, mock)
		configs := orchestratorCalls(t, "/api/v1/agent/discover-schema", `{"overall_status":"success","tables":[]}`)

		if rr := serveSchemaIndex(t); rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		for _, q := range queries() {
			if strings.Contains(q, "FROM pipelines") {
				t.Errorf("a server-level connection looked up pipeline databases: %s", q)
			}
		}
		if _, ok := configs()[0]["namespace_filter_mode"]; ok {
			t.Errorf("a server-level connection's scope was rewritten: %v", configs()[0])
		}
	})
}

// serverLevelConnection is docFindConnection for a connection naming no database.
func serverLevelConnection(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"connection_string":"mongodb://h/"}`)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mock.ExpectQuery(`SELECT connector_type, config\s+FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(docFindConnID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config"}).AddRow("mongodb", enc))
}

func serveDocFindDatabase(t *testing.T, database string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"connection_id":"` + docFindConnID + `","collection":"customers"`
	if database != "" {
		body += `,"database":"` + database + `"`
	}
	return serveDocFind(t, body+`}`)
}

const docFindOK = `{"success":true,"collection":"customers","returned":0,"has_more":false,"documents":[],"columns":[]}`

func TestExplorerFindReadsADatabaseAPipelineWritesTo(t *testing.T) {
	mock, _ := explorerDBMock(t)
	docFindConnection(t, mock, "mongodb")
	expectPipelineDatabases(mock, pipelineDestDB, "orders_db")
	configs := orchestratorCalls(t, "/api/v1/agent/explorer-find", docFindOK)

	rr := serveDocFindDatabase(t, pipelineDestDB)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	sent := configs()
	if len(sent) != 1 {
		t.Fatalf("find calls = %d, want 1", len(sent))
	}
	wantWidened(t, sent[0], "shop,"+pipelineDestDB+",orders_db")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A database no pipeline writes to stays refused: the config stays pinned, so
// the connector's own check rejects it.
func TestExplorerFindKeepsOtherDatabasesOutOfReach(t *testing.T) {
	t.Run("a database no pipeline writes to", func(t *testing.T) {
		mock, _ := explorerDBMock(t)
		docFindConnection(t, mock, "mongodb")
		expectPipelineDatabases(mock, pipelineDestDB)
		configs := orchestratorCalls(t, "/api/v1/agent/explorer-find", docFindOK)

		serveDocFindDatabase(t, "crm")

		wantPinned(t, configs()[0])
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	for _, database := range []string{"shop", ""} {
		t.Run("the connection's own database "+database, func(t *testing.T) {
			mock, queries := explorerDBMock(t)
			docFindConnection(t, mock, "mongodb")
			configs := orchestratorCalls(t, "/api/v1/agent/explorer-find", docFindOK)

			serveDocFindDatabase(t, database)

			wantPinned(t, configs()[0])
			for _, q := range queries() {
				if strings.Contains(q, "FROM pipelines") {
					t.Errorf("a find of the pinned database looked up pipeline databases: %s", q)
				}
			}
		})
	}
}
