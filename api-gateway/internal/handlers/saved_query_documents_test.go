package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"api-gateway/internal/security"
	"api-gateway/internal/validators"

	"github.com/DATA-DOG/go-sqlmock"
)

// MongoDB models: a saved query on a document connection holds a pipeline, not SQL.
// These pin the four places that difference has to be honoured — storing it, authorizing
// a run, the config a run reaches the data with, and what the run sends the connector.

// expectSavedQueryConnectorType is the create path's lookup of what the connection is,
// which decides whether sql_text is SQL or a pipeline.
func expectSavedQueryConnectorType(mock sqlmock.Sqlmock, connectorType string) {
	mock.ExpectQuery(`SELECT COALESCE\(connector_type, ''\) FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(savedQueryConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow(connectorType))
}

// docModelText is a stored MongoDB model. The $sort keys are deliberately out of
// alphabetical order and the integer is past 2^53: re-encoding the pipeline would
// reorder the first and round the second, and either changes what the model computes.
const docModelText = `{"database":"shop","collection":"orders",` +
	`"pipeline":[{"$match":{"n":9007199254740993}},{"$sort":{"z":1,"a":-1}}]}`

const docModelPipeline = `[{"$match":{"n":9007199254740993}},{"$sort":{"z":1,"a":-1}}]`

func TestParseDocumentModelQuery(t *testing.T) {
	spec, ferr := parseDocumentModelQuery(docModelText)
	if ferr != nil {
		t.Fatalf("a valid model was refused: %+v", ferr)
	}
	if spec.Database != "shop" || spec.Collection != "orders" {
		t.Errorf("namespace = %s.%s, want shop.orders", spec.Database, spec.Collection)
	}
	if string(spec.Pipeline) != docModelPipeline {
		t.Errorf("pipeline = %s, want the stored bytes %s", spec.Pipeline, docModelPipeline)
	}
	// A model writes every document; the preview's page size must not cap it.
	if spec.Limit != 0 {
		t.Errorf("limit = %d, want 0", spec.Limit)
	}

	refused := []struct {
		name, text, code, path string
	}{
		{"SQL on a document connection", "SELECT 1", "invalid_pipeline", ""},
		{"not an object", `[{"$match":{}}]`, "invalid_pipeline", ""},
		{"unknown field", `{"collection":"orders","pipeline":[],"filter":{}}`, "invalid_pipeline", ""},
		{"trailing data", `{"collection":"orders","pipeline":[]} {}`, "invalid_pipeline", ""},
		{"no collection", `{"pipeline":[]}`, "invalid_collection", "collection"},
		{"$out as a stage", `{"collection":"orders","pipeline":[{"$out":"x"}]}`, "operator_not_allowed", "pipeline[0].$out"},
		{"$merge inside $facet", `{"collection":"orders","pipeline":[{"$facet":{"a":[{"$merge":"x"}]}}]}`,
			"operator_not_allowed", "pipeline[0].$facet.a[0].$merge"},
		{"$out inside $lookup.pipeline", `{"collection":"orders","pipeline":[{"$lookup":{"from":"c","as":"j","pipeline":[{"$out":"x"}]}}]}`,
			"operator_not_allowed", "pipeline[0].$lookup.pipeline[0].$out"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, ferr := parseDocumentModelQuery(tc.text)
			if ferr == nil {
				t.Fatal("accepted")
			}
			if ferr.Code != tc.code || ferr.Path != tc.path {
				t.Errorf("got (%s, %q), want (%s, %q): %s", ferr.Code, ferr.Path, tc.code, tc.path, ferr.Message)
			}
		})
	}
}

func TestModelSupportedCoversDocumentConnectors(t *testing.T) {
	for ct, want := range map[string]bool{"mongodb": true, "postgresql": true, "rest-api": false, "": false} {
		if got := modelSupported(ct); got != want {
			t.Errorf("modelSupported(%q) = %v, want %v", ct, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Storing a pipeline
// ---------------------------------------------------------------------------

func TestCreateSavedQuery_MongoPipelineIsStoredAsRead(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`FROM connections r\s+JOIN workspace_members`).
		WithArgs(savedQueryConn, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	expectSavedQueryConnectorType(mock, "mongodb")
	// "read", not ClassifyStatementSQL's "unknown" for text that is not SQL.
	mock.ExpectQuery(`INSERT INTO saved_queries`).
		WithArgs(wsScopeWS, savedQueryConn, "Order totals", "", docModelText, "",
			"read", "workspace", wsScopeUser).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(savedQueryID))
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))

	r := savedQueryRouter(http.MethodPost, "/explorer/saved", "member", CreateSavedQuery)
	w := doJSON(r, http.MethodPost, "/explorer/saved", map[string]any{
		"connection_id": savedQueryConn,
		"name":          "Order totals",
		"sql_text":      docModelText,
	})

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestCreateSavedQuery_RefusesAPipelineThatWrites(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`FROM connections r\s+JOIN workspace_members`).
		WithArgs(savedQueryConn, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	expectSavedQueryConnectorType(mock, "mongodb")
	// No INSERT: an unexpected call fails the test.

	r := savedQueryRouter(http.MethodPost, "/explorer/saved", "member", CreateSavedQuery)
	w := doJSON(r, http.MethodPost, "/explorer/saved", map[string]any{
		"connection_id": savedQueryConn,
		"name":          "Sneaky",
		"sql_text":      `{"collection":"orders","pipeline":[{"$merge":{"into":"customers"}}]}`,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var body struct{ Error, Code, Path string }
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "operator_not_allowed" || body.Path != "pipeline[0].$merge" {
		t.Errorf("got (%s, %s), want (operator_not_allowed, pipeline[0].$merge)", body.Code, body.Path)
	}
	// The refusal names the stage; it never echoes a pipeline value.
	if strings.Contains(body.Error, "customers") {
		t.Errorf("error echoes a pipeline value: %s", body.Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestUpdateSavedQuery_MongoPipelineIsReclassifiedAsRead(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	mock.ExpectQuery(`FROM saved_queries sq[\s\S]+WHERE sq\.id = \$1`).
		WithArgs(savedQueryID).
		WillReturnRows(savedQueryRowsOnConnector("mongodb", wsScopeUser, "workspace", docModelText, "read"))

	const edited = `{"collection":"orders","pipeline":[{"$count":"n"}]}`
	mock.ExpectBegin()
	expectSavedQueryLock(mock, "Daily MRR", docModelText, "read", savedQueryFixtureUpdatedAt)
	expectScheduleProbe(mock, false)
	mock.ExpectExec(`INSERT INTO saved_query_versions`).
		WithArgs(savedQueryID, "Daily MRR", docModelText, "read", wsScopeUser).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE saved_queries`).
		WithArgs(savedQueryID, "Daily MRR", "", edited, "read", "workspace", wsScopeUser).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectRetentionKeepForever(mock)
	mock.ExpectExec(`INSERT INTO audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))

	r := savedQueryRouter(http.MethodPatch, "/explorer/saved/:id", "member", UpdateSavedQuery)
	w := doJSON(r, http.MethodPatch, "/explorer/saved/"+savedQueryID, map[string]any{"sql_text": edited})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestUpdateSavedQuery_RefusesAPipelineThatWrites(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()

	mock.ExpectQuery(`FROM saved_queries r\s+JOIN workspace_members`).
		WithArgs(savedQueryID, wsScopeUser, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow("member"))
	mock.ExpectQuery(`FROM saved_queries sq[\s\S]+WHERE sq\.id = \$1`).
		WithArgs(savedQueryID).
		WillReturnRows(savedQueryRowsOnConnector("mongodb", wsScopeUser, "workspace", docModelText, "read"))
	mock.ExpectBegin()
	expectSavedQueryLock(mock, "Daily MRR", docModelText, "read", savedQueryFixtureUpdatedAt)
	// Refused inside the lock: no probe, no snapshot, no UPDATE.
	mock.ExpectRollback()

	r := savedQueryRouter(http.MethodPatch, "/explorer/saved/:id", "member", UpdateSavedQuery)
	w := doJSON(r, http.MethodPatch, "/explorer/saved/"+savedQueryID, map[string]any{
		"sql_text": `{"collection":"orders","pipeline":[{"$out":"orders"}]}`,
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// ---------------------------------------------------------------------------
// Authorizing a run
// ---------------------------------------------------------------------------

func TestAuthorizeModelRun_MongoModel(t *testing.T) {
	cases := []struct {
		name        string
		role        security.WorkspaceRole
		mode        string
		text        string
		wantRefusal string // "" = runs
	}{
		{"admin, table mode, valid pipeline", security.WSAdmin, matTable, docModelText, ""},
		// The SQL gate (ValidateExplorerStatement) would refuse a pipeline as an
		// unrecognized statement; reaching it at all is the regression this guards.
		{"owner, statement mode", security.WSOwner, matStatement, docModelText, "statement mode is for SQL engines"},
		{"owner, pipeline names a write", security.WSOwner, matTable,
			`{"collection":"orders","pipeline":[{"$facet":{"a":[{"$out":"x"}]}}]}`, "pipeline[0].$facet.a[0].$out"},
		{"owner, SQL on a document connection", security.WSOwner, matTable, "SELECT 1", "not one rsync.ai can run"},
		{"member, valid pipeline", security.WSMember, matTable, docModelText, "does not meet the admin role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, refusal, err := authorizeWithRole(t, tc.role, &savedQueryModel{
				ConnectorType: "mongodb", SQLText: tc.text, Materialization: tc.mode, TargetTable: "shop.order_totals",
			})
			if err != nil {
				t.Fatalf("undecided: %v", err)
			}
			if class != validators.ClassRead {
				t.Errorf("class = %s, want read", class)
			}
			if tc.wantRefusal == "" {
				if refusal != "" {
					t.Fatalf("refused: %s", refusal)
				}
				return
			}
			if !strings.Contains(refusal, tc.wantRefusal) {
				t.Fatalf("refusal = %q, want it to mention %q", refusal, tc.wantRefusal)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Which databases a run reaches
// ---------------------------------------------------------------------------

func docModel() *savedQueryModel {
	return &savedQueryModel{
		ID: "m1", WorkspaceID: wsScopeWS, ConnectionID: docFindConnID, ConnectorType: "mongodb",
		SQLText: docModelText, Materialization: matTable,
	}
}

func pinnedShopConfig() map[string]interface{} {
	return map[string]interface{}{"connection_string": "mongodb://h/shop", "database": "shop"}
}

func TestDocumentModelConfig(t *testing.T) {
	t.Run("the pinned database only: no lookup, config as stored", func(t *testing.T) {
		_, sent := explorerDBMock(t)
		for _, dbs := range [][2]string{{"", ""}, {"shop", ""}, {"", "shop"}, {"shop", "shop"}} {
			cfg, source := documentModelConfig(context.Background(), docModel(), pinnedShopConfig(), dbs[0], dbs[1])
			wantPinned(t, cfg)
			if source != dbs[0] {
				t.Errorf("%v: source = %q, want %q", dbs, source, dbs[0])
			}
		}
		if q := sent(); len(q) != 0 {
			t.Errorf("a run inside the pinned database looked up pipeline databases: %v", q)
		}
	})

	t.Run("a gold model written to a pipeline database is widened, source named", func(t *testing.T) {
		mock, _ := explorerDBMock(t)
		expectPipelineDatabases(mock, pipelineDestDB)
		cfg, source := documentModelConfig(context.Background(), docModel(), pinnedShopConfig(), "", pipelineDestDB)
		wantWidened(t, cfg, "shop,"+pipelineDestDB)
		// The widened config names no database, so the pipeline's own database must
		// travel explicitly or the connector would not know where to read.
		if source != "shop" {
			t.Errorf("source = %q, want the pinned shop", source)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a silver model reading the bronze a pipeline delivered is widened", func(t *testing.T) {
		mock, _ := explorerDBMock(t)
		expectPipelineDatabases(mock, pipelineDestDB)
		cfg, source := documentModelConfig(context.Background(), docModel(), pinnedShopConfig(), pipelineDestDB, "")
		wantWidened(t, cfg, "shop,"+pipelineDestDB)
		if source != pipelineDestDB {
			t.Errorf("source = %q, want %q", source, pipelineDestDB)
		}
	})

	t.Run("no pipeline database: stays pinned, so the connector refuses the other one", func(t *testing.T) {
		mock, _ := explorerDBMock(t)
		expectPipelineDatabases(mock)
		cfg, source := documentModelConfig(context.Background(), docModel(), pinnedShopConfig(), "", "elsewhere")
		wantPinned(t, cfg)
		if source != "" {
			t.Errorf("source = %q, want it left as the pipeline said", source)
		}
	})

	t.Run("a server-level connection is never widened", func(t *testing.T) {
		_, sent := explorerDBMock(t)
		cfg := map[string]interface{}{"connection_string": "mongodb://h/"}
		got, source := documentModelConfig(context.Background(), docModel(), cfg, "a", "b")
		if _, ok := got["namespace_filter_mode"]; ok || source != "a" {
			t.Errorf("server-level config changed: %v, source %q", got, source)
		}
		if q := sent(); len(q) != 0 {
			t.Errorf("looked up pipeline databases for a server-level connection: %v", q)
		}
	})
}

// ---------------------------------------------------------------------------
// What a run sends
// ---------------------------------------------------------------------------

func TestRunDocumentModel_SendsTheMaterializeRequest(t *testing.T) {
	for _, owned := range []bool{false, true} {
		calls, got := docFindOrchestrator(t, "/api/v1/agent/explorer-materialize", http.StatusOK, `{"rows_affected":7}`)
		m := docModel()
		m.TargetOwned = owned
		server := map[string]interface{}{"connection_string": "mongodb://h/"}

		rows, err := runDocumentModel(context.Background(), m, server, "analytics", "order_totals")
		if err != nil {
			t.Fatalf("owned=%v: %v", owned, err)
		}
		if rows == nil || *rows != 7 {
			t.Errorf("owned=%v: rows_affected = %v, want 7", owned, rows)
		}
		if calls.Load() != 1 {
			t.Fatalf("owned=%v: %d orchestrator calls, want exactly one (a run is never retried behind the caller)", owned, calls.Load())
		}
		raw := got.Load().(string)
		// Verbatim: key order and the 64-bit integer survive.
		if !strings.Contains(raw, `"pipeline":`+docModelPipeline) {
			t.Errorf("owned=%v: pipeline not forwarded verbatim: %s", owned, raw)
		}
		var req map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]interface{}{
			"connector_type": "mongodb", "connection_id": docFindConnID,
			"database": "shop", "collection": "orders", "target": "analytics.order_totals",
			// Not owned → the connector builds aside and renames without dropTarget, so
			// MongoDB refuses to replace a collection rsync.ai did not create.
			"replace": owned,
		} {
			if req[k] != want {
				t.Errorf("owned=%v: %s = %v, want %v", owned, k, req[k], want)
			}
		}
		if secs, _ := req["timeout_seconds"].(float64); secs < 1 {
			t.Errorf("owned=%v: timeout_seconds = %v, want a positive budget", owned, req["timeout_seconds"])
		}
		if _, ok := req["limit"]; ok {
			t.Errorf("owned=%v: a model run carries the preview's limit", owned)
		}
	}
}

func TestRunDocumentModel_TargetInThePipelinesOwnDatabase(t *testing.T) {
	_, got := docFindOrchestrator(t, "/api/v1/agent/explorer-materialize", http.StatusOK, `{"rows_affected":0}`)
	m := docModel()
	m.SQLText = `{"collection":"orders","pipeline":[]}`

	if _, err := runDocumentModel(context.Background(), m, map[string]interface{}{"connection_string": "mongodb://h/"}, "", "order_totals"); err != nil {
		t.Fatal(err)
	}
	var req map[string]interface{}
	_ = json.Unmarshal([]byte(got.Load().(string)), &req)
	if req["target"] != "order_totals" {
		t.Errorf("target = %v, want the bare collection", req["target"])
	}
	if _, ok := req["database"]; ok {
		t.Errorf("database = %v, want it absent so the connection's own applies", req["database"])
	}
}

func TestRunDocumentModel_WidenedRunCarriesTheWidenedConfig(t *testing.T) {
	mock, _ := explorerDBMock(t)
	expectPipelineDatabases(mock, pipelineDestDB)
	configs := orchestratorCalls(t, "/api/v1/agent/explorer-materialize", `{"rows_affected":3}`)
	m := docModel()
	m.SQLText = `{"collection":"orders","pipeline":[]}`

	if _, err := runDocumentModel(context.Background(), m, pinnedShopConfig(), pipelineDestDB, "gold_orders"); err != nil {
		t.Fatal(err)
	}
	sent := configs()
	if len(sent) != 1 {
		t.Fatalf("%d calls, want 1", len(sent))
	}
	wantWidened(t, sent[0], "shop,"+pipelineDestDB)
}

func TestRunDocumentModel_RelaysTheConnectorsRefusal(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"target exists", http.StatusConflict,
			`{"error":"target_exists","details":"analytics.order_totals already exists and was not created by this model"}`,
			"analytics.order_totals already exists and was not created by this model"},
		{"error only", http.StatusBadGateway, `{"error":"connection_failed"}`, "connection_failed"},
		{"no body", http.StatusInternalServerError, ``, "orchestrator returned 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docFindOrchestrator(t, "/api/v1/agent/explorer-materialize", tc.status, tc.body)
			rows, err := runDocumentModel(context.Background(), docModel(),
				map[string]interface{}{"connection_string": "mongodb://h/"}, "analytics", "order_totals")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if rows != nil {
				t.Errorf("rows = %v on a failed run", *rows)
			}
		})
	}
}

// Lineage reads a model's upstreams out of its SQL. A pipeline is not SQL, so a MongoDB
// model must contribute no inferred reads edge — not one invented from a $lookup's
// "from" key or from SQL-looking text inside a value, either of which would order a
// schedule after something unrelated.
func TestDocumentModelInfersNoSQLUpstreams(t *testing.T) {
	for _, text := range []string{
		`{"collection":"orders","pipeline":[{"$lookup":{"from":"customers","localField":"c","foreignField":"_id","as":"j"}},{"$unionWith":{"coll":"refunds"}}]}`,
		`{"collection":"orders","pipeline":[{"$match":{"note":"select * from secrets join keys"}}]}`,
	} {
		if refs := validators.ExtractTableReferences(text); len(refs) != 0 {
			t.Errorf("a pipeline yielded SQL table references %+v", refs)
		}
	}
}
