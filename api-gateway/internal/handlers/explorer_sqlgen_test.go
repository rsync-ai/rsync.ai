package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestDialectFromConnectorType pins the connector_type -> SQL dialect mapping
// used to harden POST /api/v1/sql/generate so it can derive the dialect from a
// connection instead of blindly defaulting to postgresql.
func TestDialectFromConnectorType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"mysql", "mysql"},
		{"MySQL", "mysql"},
		{"mysql-cdc", "mysql"},
		{"mariadb", "mysql"},
		{"postgresql", "postgresql"},
		{"postgres", "postgresql"},
		{"PostgreSQL", "postgresql"},
		{"redshift", "postgresql"},
		{"databricks", "databricks"},
		{"sqlserver", "tsql"},
		{"mssql", "tsql"},
		{"bigquery", "bigquery"},
		{"BigQuery", "bigquery"},
		{"snowflake", "snowflake"},
		{"clickhouse", "clickhouse"},
		{"oracle", "oracle"},
		{"mongodb", ""},
		{"aws-s3", ""},
		{"google-sheets", ""},
		{"shopify-admin-graphql", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := SQLDialect(tc.in); got != tc.want {
			t.Errorf("SQLDialect(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGenerateSQL_TheConnectionDecidesTheDialect is the request the Explorer and
// Ask rsync.ai make: a connection id and no dialect. The SQL writer must be told
// the connection's own dialect — a BigQuery connection used to be sent
// "postgresql", so the model wrote `->>` and `::json` that BigQuery rejects.
func TestGenerateSQL_TheConnectionDecidesTheDialect(t *testing.T) {
	t.Setenv("RSYNC_BILLING_ENFORCED", "false") // no plan read: the one query is the connection's
	for _, ct := range []string{"bigquery", "snowflake", "clickhouse", "oracle", "databricks", "sqlserver", "mysql", "postgresql"} {
		t.Run(ct, func(t *testing.T) {
			want := ResolveExplorerCapability(ct).Dialect
			if want == "" {
				t.Fatalf("control: %q has no dialect in the capability table", ct)
			}
			var sent map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&sent)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"sql":"SELECT 1"}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("TEXT2SQL_ENDPOINT", srv.URL)

			mock := withSampleMockDB(t)
			mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
				WithArgs(sqlTestConn, "ws-1").
				WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow(ct))
			mock.ExpectExec(`INSERT INTO nl_query_usage_events`).WillReturnResult(sqlmock.NewResult(0, 1))

			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sql/generate",
				strings.NewReader(`{"question":"alerts by severity","connection_id":"`+sqlTestConn+`","schema":"fintech_bronze.fraud_alerts(op, after_json)"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(ctxWorkspaceID, "ws-1")
			GenerateSQL(c)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
			}
			if sent["dialect"] != want {
				t.Fatalf("%s connection: the SQL writer was told dialect %v, want %q", ct, sent["dialect"], want)
			}
		})
	}
}

// askHop runs GenerateSQL as a request on a postgresql connection and returns
// the body and headers llm-service (or the dedicated endpoint) was sent. set,
// when given, sets keys on the handler's context as AssistantSQL.Write does.
func askHop(t *testing.T, text2sqlEndpoint bool, reqBody string, set map[string]any) (map[string]any, http.Header) {
	t.Helper()
	t.Setenv("RSYNC_BILLING_ENFORCED", "false")
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-secret")
	var sent map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sql":"SELECT 1"}`))
	}))
	t.Cleanup(srv.Close)
	if text2sqlEndpoint {
		t.Setenv("TEXT2SQL_ENDPOINT", srv.URL)
		t.Setenv("LLM_SERVICE_URL", "http://127.0.0.1:1") // never reached
	} else {
		t.Setenv("TEXT2SQL_ENDPOINT", "")
		t.Setenv("LLM_SERVICE_URL", srv.URL)
	}

	mock := withSampleMockDB(t)
	mock.ExpectQuery(`SELECT connector_type FROM connections WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(sqlTestConn, "ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("postgresql"))
	mock.ExpectExec(`INSERT INTO nl_query_usage_events`).WillReturnResult(sqlmock.NewResult(0, 1))

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/sql/generate", strings.NewReader(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxWorkspaceID, "ws-1")
	for k, v := range set {
		c.Set(k, v)
	}
	GenerateSQL(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if sent == nil {
		t.Fatal("control: the SQL writer was never called")
	}
	return sent, hdr
}

const askHopBody = `{"question":"alerts by severity","connection_id":"` + sqlTestConn + `","schema":"public.alerts(severity)"}`

// Ask rsync.ai's generate_sql writes with the model the question is answered
// with: llm-service is sent the entry, its model and the saved key, with the
// internal secret it checks before taking a key.
func TestGenerateSQL_AskSendsItsModelToLLMService(t *testing.T) {
	m := AskModel{Entry: "claude", Model: "claude-sonnet-5-5", Key: "unit-test-claude-key"}
	sent, hdr := askHop(t, false, askHopBody, map[string]any{ctxAskModel: m})
	if sent["entry"] != "claude" || sent["model"] != "claude-sonnet-5-5" || sent["api_key"] != "unit-test-claude-key" {
		t.Fatalf("llm-service was sent entry=%v model=%v api_key set=%v", sent["entry"], sent["model"], sent["api_key"] != nil)
	}
	if hdr.Get("X-Internal-Secret") != "unit-secret" {
		t.Fatal("the internal secret was not sent with the key")
	}

	// An entry llm-service's own environment key serves: no key, no model.
	sent, hdr = askHop(t, false, askHopBody, map[string]any{ctxAskModel: AskModel{Entry: "chatgpt"}})
	if sent["entry"] != "chatgpt" || sent["model"] != nil || sent["api_key"] != nil || hdr.Get("X-Internal-Secret") != "unit-secret" {
		t.Fatalf("env entry: entry=%v model=%v api_key set=%v", sent["entry"], sent["model"], sent["api_key"] != nil)
	}
}

// The Explorer's own generation, a request that names a model or key in its
// body, and a dedicated TEXT2SQL_ENDPOINT are sent none of it.
func TestGenerateSQL_OnlyAskInProcessPicksTheModel(t *testing.T) {
	ask := map[string]any{ctxAskModel: AskModel{Entry: "claude", Model: "claude-sonnet-5-5", Key: "unit-test-claude-key"}}
	for _, tc := range []struct {
		name     string
		endpoint bool
		body     string
		set      map[string]any
	}{
		{"explorer", false, askHopBody, nil},
		{"body names a model and key", false, `{"question":"q","connection_id":"` + sqlTestConn + `","schema":"public.alerts(severity)","entry":"claude","model":"claude-sonnet-5-5","api_key":"from-the-body"}`, nil},
		{"wrong type under the key", false, askHopBody, map[string]any{ctxAskModel: map[string]any{"Entry": "claude"}}},
		{"dedicated endpoint", true, askHopBody, ask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent, hdr := askHop(t, tc.endpoint, tc.body, tc.set)
			for _, f := range []string{"entry", "model", "api_key"} {
				if _, ok := sent[f]; ok {
					t.Errorf("%s was sent", f)
				}
			}
			if hdr.Get("X-Internal-Secret") != "" {
				t.Error("the internal secret was sent")
			}
		})
	}
}

// llm-service records each model call /sql/generate makes against the user it
// is for (llm-service tests/test_sql_generate_records_its_usage.py), so the
// user is sent with every generation it serves, Ask's and the Explorer's own.
func TestGenerateSQL_SendsTheUserToLLMService(t *testing.T) {
	ask := AskModel{Entry: "claude", Model: "claude-sonnet-5-5", Key: "unit-test-claude-key"}
	for _, tc := range []struct {
		name     string
		endpoint bool
		set      map[string]any
		want     any
	}{
		{"explorer", false, map[string]any{"user_id": "u-1"}, "u-1"},
		{"ask", false, map[string]any{"user_id": "u-1", ctxAskModel: ask}, "u-1"},
		{"no user", false, nil, nil},
		{"dedicated endpoint", true, map[string]any{"user_id": "u-1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent, _ := askHop(t, tc.endpoint, askHopBody, tc.set)
			if got := sent["user_id"]; got != tc.want {
				t.Fatalf("user_id sent = %v, want %v", got, tc.want)
			}
		})
	}
}

// A saved key never prints: not in a log line, not in an error.
func TestAskKeyPrintsRedacted(t *testing.T) {
	m := AskModel{Entry: "claude", Key: "unit-test-claude-key"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, m); strings.Contains(got, "unit-test-claude-key") {
			t.Errorf("%s printed the key: %s", verb, got)
		}
	}
}
