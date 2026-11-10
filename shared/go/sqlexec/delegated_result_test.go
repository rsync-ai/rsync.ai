package sqlexec

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// A delegated read whose connector sent no column list still has one column order,
// the same on every run: the rows arrive as Go maps, and a list read off a map's
// keys came back in a new order each time (KI-DELEGATED-QUERY-COLUMN-ORDER).
func TestDelegatedReadWithoutAColumnListHasOneOrder(t *testing.T) {
	orch := &recordedRequests{reply: `{"rows": [{"zeta": 1, "id": 2, "alpha": 3, "created_at": 4, "mid": 5, "amount": 6}], "row_count": 1}`}
	srv := httptest.NewServer(orch)
	defer srv.Close()
	eng, err := For("bigquery", Config{Settings: map[string]interface{}{"project_id": "p"}, Orchestrator: Orchestrator{URL: srv.URL}})
	if err != nil {
		t.Fatalf("For(bigquery): %v", err)
	}

	want := "alpha,amount,created_at,id,mid,zeta"
	for i := 0; i < 50; i++ {
		res, err := eng.Query(context.Background(), "SELECT * FROM t", nil, Limits{Rows: 5})
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := strings.Join(res.Columns, ","); got != want {
			t.Fatalf("read %d: columns %s, want %s (sorted, never map order)", i, got, want)
		}
	}

	// Control: a column list the connector sent is kept as sent.
	orch.reply = `{"columns": ["zeta", "id"], "rows": [{"zeta": 1, "id": 2}], "row_count": 1}`
	res, err := eng.Query(context.Background(), "SELECT zeta, id FROM t", nil, Limits{Rows: 5})
	if err != nil {
		t.Fatalf("read with a column list: %v", err)
	}
	if got := strings.Join(res.Columns, ","); got != "zeta,id" {
		t.Errorf("columns %s, want the connector's zeta,id", got)
	}
}

// A delegated statement reports how long it took, as a direct engine's does; the
// Explorer showed "0ms" for every BigQuery, Snowflake, ClickHouse and Oracle query.
func TestDelegatedStatementReportsItsTime(t *testing.T) {
	const took = 30 * time.Millisecond
	reply := `{"columns": ["id"], "rows": [{"id": 1}], "row_count": 1, "rows_affected": 1}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(took)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	eng, err := For("snowflake", Config{Settings: map[string]interface{}{"account": "a"}, Orchestrator: Orchestrator{URL: srv.URL}})
	if err != nil {
		t.Fatalf("For(snowflake): %v", err)
	}

	read, err := eng.Query(context.Background(), "SELECT id FROM t", nil, Limits{Rows: 5})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if read.ExecutionTimeMs < took.Milliseconds() {
		t.Errorf("read took %d ms, want at least %d", read.ExecutionTimeMs, took.Milliseconds())
	}
	write, err := eng.Exec(context.Background(), "DELETE FROM t", stmt.ClassDMLWrite)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if write.ExecutionTimeMs < took.Milliseconds() {
		t.Errorf("write took %d ms, want at least %d", write.ExecutionTimeMs, took.Milliseconds())
	}
}
