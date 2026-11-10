package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

// KI-WAREHOUSE-READS-RELY-ON-THE-GATE: a viewer's statement on a delegated engine
// (Snowflake, ClickHouse, Oracle, BigQuery) reached the connector through
// executeWithRetry, which sent it up to MaxRetries more times when the failure named a
// timeout, a connection, a 5xx or a rate limit. A timeout on our side does not stop the
// statement on the warehouse, so each retry ran it again beside the first: a read
// multiplied the load and the wait, a write was applied twice.

// statementFailures are failures after which the statement may still be running, or
// may already have run, on the warehouse.
var statementFailures = []struct {
	name string
	resp *mcp.ExecuteResponse
	err  error
}{
	{"our HTTP client gave up", nil,
		errors.New(`Post "http://snowflake-mcp:8000/execute": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)},
	{"the connection dropped after the send", nil,
		errors.New("read tcp 172.18.0.5:41234->172.18.0.9:8000: read: connection reset by peer")},
	{"the connector hit its own time limit",
		&mcp.ExecuteResponse{Error: "Query timeout: the statement ran past 30 s"}, nil},
	{"the warehouse answered 503",
		&mcp.ExecuteResponse{Error: "Request failed with status 503"}, nil},
}

// countingAgent answers every connector call with answer(n), n counting from 1.
func countingAgent(answer func(n int32) (*mcp.ExecuteResponse, error)) (*Agent, *int32) {
	calls := new(int32)
	return &Agent{connectorCallStub: func(context.Context, mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
		return answer(atomic.AddInt32(calls, 1))
	}}, calls
}

func TestDelegatedStatementIsSentOnce(t *testing.T) {
	ctx := context.Background()
	config := map[string]interface{}{"account": "acme", "warehouse": "WH"}

	for _, f := range statementFailures {
		f := f
		fail := func(int32) (*mcp.ExecuteResponse, error) { return f.resp, f.err }
		var want string
		if f.err != nil {
			want = f.err.Error()
		} else {
			want = f.resp.Error
		}

		t.Run(f.name, func(t *testing.T) {
			// Control: executeWithRetry sends this failure again, so a statement sent
			// once below was kept from the retry loop, not passed over by its classifier.
			a, calls := countingAgent(func(n int32) (*mcp.ExecuteResponse, error) {
				if n == 1 {
					return f.resp, f.err
				}
				return &mcp.ExecuteResponse{Success: true}, nil
			})
			if _, err := a.executeWithRetry(ctx, mcp.ExecuteRequest{Connector: "snowflake", Operation: "export"}); err != nil || atomic.LoadInt32(calls) != 2 {
				t.Fatalf("control: executeWithRetry made %d calls (err %v); want this failure retried once, then the answer", atomic.LoadInt32(calls), err)
			}

			a, calls = countingAgent(fail)
			_, _, err := a.ExplorerQuery(ctx, "snowflake", config, "SELECT id FROM orders", 100)
			if n := atomic.LoadInt32(calls); n != 1 {
				t.Errorf("ExplorerQuery sent the read %d times; want once", n)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ExplorerQuery error = %v; want the failure %q", err, want)
			}

			a, calls = countingAgent(fail)
			_, err = a.ExplorerExecute(ctx, "snowflake", config, "INSERT INTO orders VALUES (1)")
			if n := atomic.LoadInt32(calls); n != 1 {
				t.Errorf("ExplorerExecute sent the write %d times; want once", n)
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ExplorerExecute error = %v; want the failure %q", err, want)
			}
		})
	}

	t.Run("an answer is read", func(t *testing.T) {
		a, calls := countingAgent(func(int32) (*mcp.ExecuteResponse, error) {
			return &mcp.ExecuteResponse{Success: true, Result: map[string]interface{}{
				"data":    []interface{}{map[string]interface{}{"id": float64(7)}},
				"columns": []interface{}{"id"},
			}}, nil
		})
		rows, cols, err := a.ExplorerQuery(ctx, "snowflake", config, "SELECT id FROM orders", 100)
		if err != nil || len(rows) != 1 || len(cols) != 1 || cols[0] != "id" || atomic.LoadInt32(calls) != 1 {
			t.Errorf("ExplorerQuery = %v, %v, %v after %d calls; want one row of id, one call", rows, cols, err, atomic.LoadInt32(calls))
		}

		a, calls = countingAgent(func(int32) (*mcp.ExecuteResponse, error) {
			return &mcp.ExecuteResponse{Success: true, Result: map[string]interface{}{"rows_affected": float64(3)}}, nil
		})
		affected, err := a.ExplorerExecute(ctx, "snowflake", config, "INSERT INTO orders VALUES (1)")
		if err != nil || affected == nil || *affected != 3 || atomic.LoadInt32(calls) != 1 {
			t.Errorf("ExplorerExecute = %v, %v after %d calls; want 3 rows affected, one call", affected, err, atomic.LoadInt32(calls))
		}
	})
}
