package sqlexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsync-ai/shared/sqlexec/mask"
	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// recordedRequests is an httptest handler that keeps each request's JSON body and
// answers every one with reply.
type recordedRequests struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
	reply  string
}

func (r *recordedRequests) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	var body map[string]interface{}
	_ = json.Unmarshal(raw, &body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, r.reply)
}

func (r *recordedRequests) all() []map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]interface{}(nil), r.bodies...)
}

// An engine refuses a write above what its caller may run before it reaches the
// database, whatever the gateway checked first (query-service-design.md § 4.1).
func TestExecRefusesAStatementAboveTheCaller(t *testing.T) {
	orch := &recordedRequests{reply: `{"rows_affected": 0}`}
	orchSrv := httptest.NewServer(orch)
	defer orchSrv.Close()
	dbx := &recordedRequests{reply: `{"status": {"state": "SUCCEEDED"}}`}
	dbxSrv := httptest.NewTLSServer(dbx)
	defer dbxSrv.Close()

	cfg := Config{
		Settings: map[string]interface{}{
			"host": strings.TrimPrefix(dbxSrv.URL, "https://"), "access_token": "t", "warehouse_id": "w",
		},
		Orchestrator: Orchestrator{URL: orchSrv.URL},
		HTTPClient:   func(time.Duration) *http.Client { return dbxSrv.Client() },
	}
	refusals := []struct {
		sql     string
		allowed stmt.StatementClass
	}{
		{"SELECT 1", ""}, // no class granted: nothing runs
		{"DELETE FROM t", stmt.ClassRead},
		{"CREATE TABLE t (a int)", stmt.ClassRead},
		{"DROP TABLE t", stmt.ClassDMLWrite},
		{"TRUNCATE t", stmt.ClassDDL},
		{"GRANT ALL ON t TO u", stmt.ClassDestructive}, // blocked for every caller
	}
	for _, ct := range []string{"postgres", "mysql", "sqlserver", "databricks", "bigquery", "snowflake"} {
		eng, err := For(ct, cfg)
		if err != nil {
			t.Fatalf("For(%q): %v", ct, err)
		}
		for _, r := range refusals {
			_, err := eng.Exec(context.Background(), r.sql, r.allowed)
			if err == nil || !strings.Contains(err.Error(), "above what this caller may run") {
				t.Errorf("%s: Exec(%q, allowed %q) = %v, want a refusal", ct, r.sql, r.allowed, err)
			}
		}
	}
	if n := len(orch.all()) + len(dbx.all()); n != 0 {
		t.Fatalf("a refused statement reached the database: %d requests", n)
	}

	// Control: the same engines send a statement the caller may run.
	for _, ct := range []string{"databricks", "bigquery"} {
		eng, _ := For(ct, cfg)
		if _, err := eng.Exec(context.Background(), "DELETE FROM t", stmt.ClassDMLWrite); err != nil {
			t.Errorf("%s: an allowed write failed: %v", ct, err)
		}
	}
	if len(orch.all()) != 1 || len(dbx.all()) != 1 {
		t.Fatalf("allowed writes sent %d orchestrator and %d Databricks requests, want 1 each", len(orch.all()), len(dbx.all()))
	}
}

// A delegated engine sends a read to the connector's export tool with the row cap, a
// write to its execute tool with no rows asked for, and masks a preview's personal
// data the way a direct engine does.
func TestDelegatedEngine(t *testing.T) {
	orch := &recordedRequests{reply: `{"columns": ["id", "email"], "rows": [{"id": 1, "email": "a@b.co"}], "row_count": 1}`}
	srv := httptest.NewServer(orch)
	defer srv.Close()
	eng, err := For("bigquery", Config{Settings: map[string]interface{}{"project_id": "p"}, Orchestrator: Orchestrator{URL: srv.URL}})
	if err != nil || !IsDelegated(eng) {
		t.Fatalf("For(bigquery) = %T, %v; want a delegated engine", eng, err)
	}

	if _, err := eng.Query(context.Background(), "SELECT 1", []any{1}, Limits{Rows: 5}); err == nil {
		t.Errorf("a delegated read took bound values")
	}
	if n := len(orch.all()); n != 0 {
		t.Fatalf("a refused read reached the orchestrator: %d requests", n)
	}

	masked, err := eng.Query(context.Background(), "SELECT id, email FROM t", nil, Limits{Rows: 5, Mask: true})
	if err != nil {
		t.Fatalf("masked read: %v", err)
	}
	if got := masked.Rows[0]["email"]; got != mask.RedactForPreview("a@b.co") {
		t.Errorf("a masked read returned email %v", got)
	}
	raw, err := eng.Query(context.Background(), "SELECT id, email FROM t", nil, Limits{Rows: 5, Mask: false})
	if err != nil {
		t.Fatalf("unmasked read: %v", err)
	}
	if got := raw.Rows[0]["email"]; got != "a@b.co" {
		t.Errorf("an unmasked read returned email %v, want the raw value", got)
	}
	if _, err := eng.Exec(context.Background(), "DELETE FROM t", stmt.ClassDMLWrite); err != nil {
		t.Fatalf("write: %v", err)
	}

	sent := orch.all()
	if len(sent) != 3 {
		t.Fatalf("sent %d requests, want 3", len(sent))
	}
	for i, want := range []struct {
		limit float64
		write bool
	}{{5, false}, {5, false}, {0, true}} {
		if sent[i]["limit"] != want.limit || sent[i]["write"] != want.write || sent[i]["connector_type"] != "bigquery" {
			t.Errorf("request %d = limit %v write %v connector %v, want limit %v write %v bigquery",
				i, sent[i]["limit"], sent[i]["write"], sent[i]["connector_type"], want.limit, want.write)
		}
	}

	if _, err := eng.SchemaIndex(context.Background(), false); !errors.Is(err, ErrNoSchemaIndex) {
		t.Errorf("SchemaIndex = %v, want ErrNoSchemaIndex", err)
	}
}

// A Databricks read is capped at Limits.Rows (asking the warehouse for one more, to
// know it was truncated), masked when asked, and binds only named parameters.
func TestDatabricksEngine(t *testing.T) {
	dbx := &recordedRequests{reply: `{
		"status": {"state": "SUCCEEDED"},
		"manifest": {"schema": {"columns": [{"name": "id"}, {"name": "email"}]}},
		"result": {"data_array": [["1", "a@b.co"], ["2", "c@d.co"], ["3", "e@f.co"]]}
	}`}
	srv := httptest.NewTLSServer(dbx)
	defer srv.Close()
	settings := map[string]interface{}{
		"host": strings.TrimPrefix(srv.URL, "https://"), "access_token": "t", "http_path": "/sql/1.0/warehouses/w1",
	}
	eng, err := For("databricks", Config{Settings: settings, HTTPClient: func(time.Duration) *http.Client { return srv.Client() }})
	if err != nil {
		t.Fatalf("For(databricks): %v", err)
	}

	res, err := eng.Query(context.Background(), "SELECT id, email FROM t", []any{DatabricksParam{Name: "p", Value: "1"}}, Limits{Rows: 2, Mask: true})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.RowCount != 2 || !res.Truncated {
		t.Errorf("got %d rows, truncated %v; want 2, truncated", res.RowCount, res.Truncated)
	}
	if got := res.Rows[0]["email"]; got != mask.RedactForPreview("a@b.co") {
		t.Errorf("a masked read returned email %v", got)
	}
	sent := dbx.all()[0]
	if s, _ := sent["statement"].(string); !strings.Contains(s, "LIMIT 3") {
		t.Errorf("statement %q does not ask for Rows+1", s)
	}
	if sent["warehouse_id"] != "w1" {
		t.Errorf("warehouse_id %v, want w1 from http_path", sent["warehouse_id"])
	}
	if p, _ := sent["parameters"].([]interface{}); len(p) != 1 {
		t.Errorf("parameters %v, want the one named parameter", sent["parameters"])
	}

	raw, err := eng.Query(context.Background(), "SELECT id, email FROM t", nil, Limits{Rows: 5})
	if err != nil {
		t.Fatalf("unmasked read: %v", err)
	}
	if got := raw.Rows[0]["email"]; got != "a@b.co" {
		t.Errorf("an unmasked read returned email %v, want the raw value", got)
	}

	if _, err := eng.Query(context.Background(), "SELECT 1", []any{"1"}, Limits{Rows: 5}); err == nil {
		t.Errorf("a Databricks read took a positional value")
	}
	if n := len(dbx.all()); n != 2 {
		t.Errorf("sent %d requests, want 2 (the refused read sends none)", n)
	}

	// Without the SSRF-guarded client the engine reaches nothing.
	bare, _ := For("databricks", Config{Settings: settings})
	if _, err := bare.Query(context.Background(), "SELECT 1", nil, Limits{Rows: 5}); err == nil || !strings.Contains(err.Error(), "no HTTP client") {
		t.Errorf("a Databricks engine with no HTTP client = %v, want it to refuse", err)
	}
	if n := len(dbx.all()); n != 2 {
		t.Errorf("the engine with no client sent a request")
	}
}
