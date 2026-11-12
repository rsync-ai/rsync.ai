package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"api-gateway/internal/cache"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const sqlTestConn = "6f1c3a52-6b1f-4c39-9d36-0c6f1f2b7a10"

func sqlTestIndex() *cache.ExplorerSchemaIndex {
	col := func(names ...string) []cache.ExplorerColumnIndex {
		out := make([]cache.ExplorerColumnIndex, len(names))
		for i, n := range names {
			out[i] = cache.ExplorerColumnIndex{Name: n, Type: "text"}
		}
		return out
	}
	return &cache.ExplorerSchemaIndex{
		Tables: []cache.ExplorerTableIndex{
			{Name: "orders", Schema: "public", RowCount: 912345, Columns: col("id", "status", "customer_id"), PrimaryKey: []string{"id"}, SearchTokens: "orders status shipped"},
			{Name: "customers", Schema: "public", RowCount: 4321, Columns: col("id", "country"), SearchTokens: "customers"},
			{Name: "events", Schema: "public", Columns: col("id")},
			{Name: "events", Schema: "audit", Columns: col("id")},
		},
		ForeignKeys: []cache.ExplorerForeignKeyIndex{
			{FromSchema: "public", FromTable: "orders", FromColumn: "customer_id", ToSchema: "public", ToTable: "customers", ToColumn: "id"},
			{FromSchema: "audit", FromTable: "events", FromColumn: "id", ToSchema: "public", ToTable: "orders", ToColumn: "id"},
		},
	}
}

func TestFindTableNeedsOneMatch(t *testing.T) {
	all := sqlTestIndex().Tables
	for ref, want := range map[string]string{
		"orders": "public.orders", "PUBLIC.Orders": "public.orders", " customers ": "public.customers",
		"audit.events": "audit.events", "events": "", "nope": "", "other.orders": "",
	} {
		got, ok := findTable(all, ref)
		if want == "" {
			if ok {
				t.Errorf("findTable(%q) = %s, want none", ref, explorerTableKey(got))
			}
			continue
		}
		if !ok || explorerTableKey(got) != want {
			t.Errorf("findTable(%q) = %s %v, want %s", ref, explorerTableKey(got), ok, want)
		}
	}
}

func TestNamedTablesAndCandidates(t *testing.T) {
	all := sqlTestIndex().Tables
	picked, missing := namedTables(all, []string{"orders", "public.orders", "events", "customers"})
	if keys(picked) != "public.orders,public.customers" || strings.Join(missing, ",") != "events" {
		t.Fatalf("namedTables = %s, missing %v", keys(picked), missing)
	}
	got := candidateTables(all, []TableCandidate{
		{Table: "ghost"}, {Table: "events", SchemaName: "audit"}, {Table: "public.orders"},
		{Table: "orders", SchemaName: "public"}, {Table: "customers", SchemaName: "public"}, {Table: "events", SchemaName: "public"},
	}, 3)
	if keys(got) != "audit.events,public.orders,public.customers" {
		t.Fatalf("candidateTables = %s", keys(got))
	}
}

func keys(ts []cache.ExplorerTableIndex) string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = explorerTableKey(t)
	}
	return strings.Join(out, ",")
}

// The request is the Explorer's: names and types, never a row count or the
// index's search tokens, and only the foreign keys among the chosen tables.
func TestGenerateSQLBodyIsNamesOnly(t *testing.T) {
	idx := sqlTestIndex()
	body := generateSQLBody(sqlTestConn, "orders by country", idx.Tables[:2], idx.ForeignKeys)
	raw, _ := json.Marshal(body)
	for _, leak := range []string{"912345", "4321", "row_count", "shipped"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("body carries %q: %s", leak, raw)
		}
	}
	var req SQLGenerateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.Question != "orders by country" || req.ConnectionID != sqlTestConn || req.Tables != "public.orders,public.customers" ||
		req.Schema != "public.orders(id, status, customer_id); public.customers(id, country)" || len(req.TablesTyped) != 2 {
		t.Fatalf("request = %+v", req)
	}
	if len(req.ForeignKeys) != 1 || req.ForeignKeys[0]["to_table"] != "customers" {
		t.Fatalf("foreign keys = %+v", req.ForeignKeys)
	}
}

func TestSQLStepErrorTextsAreFixed(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusPaymentRequired, `{"error":"trial_expired","message":"ends 2026-10-01"}`, sqlNotRunTrial},
		{http.StatusPaymentRequired, `{"error":"query_limit_reached","used":41,"limit":40}`, sqlNotRunQuota},
		{http.StatusBadRequest, `{"error":"x","error_code":"document_connection"}`, sqlNotRunDocuments},
		{http.StatusNotFound, `{"error":"Connection not found"}`, sqlNotRunConnection},
		{http.StatusServiceUnavailable, `{"error":"llm_not_configured"}`, sqlNotRunNoLLM},
		{http.StatusServiceUnavailable, `{"error":"upstream"}`, sqlNotRunFailed},
		{http.StatusInternalServerError, `{"error":"pq: password authentication failed"}`, sqlNotRunFailed},
		{http.StatusBadRequest, `not json`, sqlNotRunFailed},
	}
	for _, tc := range cases {
		if got := sqlStepError(tc.status, []byte(tc.body)).Error(); got != tc.want {
			t.Errorf("%d %s: %q, want %q", tc.status, tc.body, got, tc.want)
		}
	}
}

// sqlWriteHarness stands in for the database, the schema index and the two
// Explorer steps, and records what each step was asked.
type sqlWriteHarness struct {
	resolveBodies, generateBodies []map[string]any
	actorKinds                    []string
	askModels                     []any // ctxAskModel on each step's context, nil when unset
	resolveStatus, generateStatus int
	resolveBody, generateBody     string
}

func newSQLWrite(t *testing.T, connectorType string, rowErr error) (*sqlWriteHarness, *gin.Context) {
	t.Helper()
	mock := withSampleMockDB(t)
	q := mock.ExpectQuery(`SELECT connector_type, config FROM connections WHERE id = \$1 AND workspace_id = \$2`).WithArgs(sqlTestConn, "ws-1")
	if rowErr != nil {
		q.WillReturnError(rowErr)
	} else {
		q.WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config"}).AddRow(connectorType, "enc"))
	}
	h := &sqlWriteHarness{
		resolveStatus: http.StatusOK, generateStatus: http.StatusOK,
		resolveBody:  `{"candidates":[{"table":"orders","schema_name":"public"},{"table":"customers","schema_name":"public"}]}`,
		generateBody: `{"sql":"SELECT c.country, COUNT(*) FROM public.orders o JOIN public.customers c ON c.id = o.customer_id GROUP BY 1"}`,
	}
	prevIdx, prevResolve, prevGenerate := silverSchemaIndex, assistantResolveTables, assistantGenerateSQL
	t.Cleanup(func() {
		silverSchemaIndex, assistantResolveTables, assistantGenerateSQL = prevIdx, prevResolve, prevGenerate
	})
	silverSchemaIndex = func(_ context.Context, connID, wsID, ct, cfg string, refresh bool) (*cache.ExplorerSchemaIndex, error) {
		if connID != sqlTestConn || wsID != "ws-1" || ct != connectorType || cfg != "enc" || refresh {
			t.Errorf("schema index read with %s %s %s %s %v", connID, wsID, ct, cfg, refresh)
		}
		return sqlTestIndex(), nil
	}
	step := func(bodies *[]map[string]any, status *int, out *string) gin.HandlerFunc {
		return func(c *gin.Context) {
			var b map[string]any
			_ = c.ShouldBindJSON(&b)
			*bodies = append(*bodies, b)
			h.actorKinds = append(h.actorKinds, c.GetString(ctxActorKind)+"/"+c.GetString("workspace_id"))
			m, _ := c.Get(ctxAskModel)
			h.askModels = append(h.askModels, m)
			c.Data(*status, "application/json", []byte(*out))
		}
	}
	assistantResolveTables = step(&h.resolveBodies, &h.resolveStatus, &h.resolveBody)
	assistantGenerateSQL = step(&h.generateBodies, &h.generateStatus, &h.generateBody)

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/assistant/messages", nil)
	c.Set("workspace_id", "ws-1")
	c.Set("user_id", "u1")
	return h, c
}

// A handler run as the caller sees the caller's client IP, the one the
// gateway trusts (CF-Connecting-IP in production), not the proxy's address.
func TestRunAsCallerKeepsTheCallersClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, ip := range []string{"203.0.113.7", "2001:db8::7"} {
		c, engine := gin.CreateTestContext(httptest.NewRecorder())
		engine.TrustedPlatform = gin.PlatformCloudflare
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/assistant/messages", nil)
		c.Request.RemoteAddr = "10.0.0.5:41234"
		c.Request.Header.Set("CF-Connecting-IP", ip)
		var seen string
		runAsCaller(c, func(sc *gin.Context) { seen = sc.ClientIP(); sc.Status(http.StatusOK) }, map[string]any{}, nil)
		if seen != ip {
			t.Errorf("the handler saw client IP %q, want the caller's %q (the proxy is 10.0.0.5)", seen, ip)
		}
	}
}

func TestAssistantSQLWritePicksTablesThenWrites(t *testing.T) {
	h, c := newSQLWrite(t, "postgresql", nil)
	got, used, err := AssistantSQL{}.Write(c, sqlTestConn, "orders by country", nil, AskModel{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "SELECT c.country") || strings.Join(used, ",") != "public.orders,public.customers" {
		t.Fatalf("Write = %q %v", got, used)
	}
	if len(h.resolveBodies) != 1 || h.resolveBodies[0]["connection_id"] != sqlTestConn || h.resolveBodies[0]["question"] != "orders by country" {
		t.Fatalf("resolve bodies = %+v", h.resolveBodies)
	}
	if len(h.generateBodies) != 1 || h.generateBodies[0]["tables"] != "public.orders,public.customers" {
		t.Fatalf("generate bodies = %+v", h.generateBodies)
	}
	// Both steps ran as the asking user's workspace, marked as Ask rsync.ai.
	if strings.Join(h.actorKinds, ",") != "assistant/ws-1,assistant/ws-1" {
		t.Fatalf("actor kinds = %v", h.actorKinds)
	}
	if c.GetString(ctxActorKind) != "" {
		t.Fatalf("the caller's own context was marked: %q", c.GetString(ctxActorKind))
	}
}

// The model the question is answered with reaches the generate step only:
// the resolve step links tables with the server's model, and the caller's own
// context is never given it.
func TestAssistantSQLWriteHandsTheAskModelToTheGenerateStep(t *testing.T) {
	m := AskModel{Entry: "claude", Model: "claude-sonnet-5-5", Key: "unit-test-claude-key"}
	h, c := newSQLWrite(t, "postgresql", nil)
	if _, _, err := (AssistantSQL{}).Write(c, sqlTestConn, "orders by country", nil, m); err != nil {
		t.Fatal(err)
	}
	if len(h.askModels) != 2 || h.askModels[0] != nil || h.askModels[1] != m {
		t.Fatalf("ask model on resolve, generate = %v", h.askModels)
	}
	if _, ok := c.Get(ctxAskModel); ok {
		t.Fatal("the caller's own context was given the ask model")
	}

	h, c = newSQLWrite(t, "postgresql", nil)
	if _, _, err := (AssistantSQL{}).Write(c, sqlTestConn, "orders by country", nil, AskModel{}); err != nil {
		t.Fatal(err)
	}
	if len(h.askModels) != 2 || h.askModels[0] != nil || h.askModels[1] != nil {
		t.Fatalf("no entry: ask model on resolve, generate = %v", h.askModels)
	}
}

func TestAssistantSQLWriteNamedTablesSkipLinking(t *testing.T) {
	h, c := newSQLWrite(t, "postgresql", nil)
	_, used, err := AssistantSQL{}.Write(c, sqlTestConn, "q", []string{"customers"}, AskModel{})
	if err != nil || strings.Join(used, ",") != "public.customers" || len(h.resolveBodies) != 0 {
		t.Fatalf("Write = %v %v, resolve ran %d times", used, err, len(h.resolveBodies))
	}
}

func TestAssistantSQLWriteRefusals(t *testing.T) {
	cases := []struct {
		name      string
		connector string
		rowErr    error
		tables    []string
		edit      func(*sqlWriteHarness)
		want      string
	}{
		{"unknown connection", "", sql.ErrNoRows, nil, nil, sqlNotRunConnection},
		{"documents", "mongodb", nil, nil, nil, sqlNotRunDocuments},
		{"unknown table", "postgresql", nil, []string{"orders", "events"}, nil,
			"Not run: no table events on this connection, or the name is in more than one schema (write schema.table). describe_schema lists the tables."},
		{"no candidates", "postgresql", nil, nil, func(h *sqlWriteHarness) { h.resolveBody = `{"candidates":[{"table":"ghost"}]}` }, sqlNotRunNoTables},
		{"quota", "postgresql", nil, nil, func(h *sqlWriteHarness) {
			h.generateStatus, h.generateBody = http.StatusPaymentRequired, `{"error":"query_limit_reached"}`
		}, sqlNotRunQuota},
		{"empty sql", "postgresql", nil, nil, func(h *sqlWriteHarness) { h.generateBody = `{"sql":"  "}` }, sqlNotRunFailed},
		{"not a read", "postgresql", nil, nil, func(h *sqlWriteHarness) { h.generateBody = `{"sql":"DELETE FROM public.orders"}` }, sqlNotRunNotRead},
		{"read then write", "postgresql", nil, nil, func(h *sqlWriteHarness) {
			h.generateBody = `{"sql":"SELECT 1; DROP TABLE public.orders"}`
		}, sqlNotRunNotRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, c := newSQLWrite(t, tc.connector, tc.rowErr)
			if tc.edit != nil {
				tc.edit(h)
			}
			got, used, err := AssistantSQL{}.Write(c, sqlTestConn, "q", tc.tables, AskModel{})
			if err == nil || err.Error() != tc.want || got != "" || used != nil {
				t.Fatalf("Write = %q %v %v, want %q", got, used, err, tc.want)
			}
		})
	}
}
