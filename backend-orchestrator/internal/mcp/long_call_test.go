package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A model build sends DDL that runs for minutes through the connector's `execute`
// tool. The HTTP transport stopped every call at 30 s whatever the caller's deadline,
// so a long CREATE TABLE … AS failed on our side while the warehouse kept running it.
// LongCall lets the caller's own deadline bound the call instead.

func TestCallTimeout(t *testing.T) {
	const limit = 30 * time.Second
	long := func(ctx context.Context) context.Context { return context.WithValue(ctx, longCallKey{}, true) }
	within := func(got, want time.Duration) bool { return got <= want+time.Second && got >= want-2*time.Second }

	t.Run("no deadline keeps the cap, long or not", func(t *testing.T) {
		if got := callTimeout(context.Background(), limit); got != limit {
			t.Errorf("plain: got %s, want %s", got, limit)
		}
		if got := callTimeout(long(context.Background()), limit); got != limit {
			t.Errorf("long: got %s, want %s", got, limit)
		}
	})
	t.Run("a nearer deadline shortens the call", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if got := callTimeout(ctx, limit); !within(got, 5*time.Second) {
			t.Errorf("got %s, want about 5s", got)
		}
	})
	t.Run("a later deadline is capped unless the call is long", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if got := callTimeout(ctx, limit); got != limit {
			t.Errorf("plain: got %s, want the cap %s", got, limit)
		}
		if got := callTimeout(long(ctx), limit); !within(got, 10*time.Minute) {
			t.Errorf("long: got %s, want about 10m", got)
		}
	})
	t.Run("a passed deadline falls back to the cap", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
		defer cancel()
		if got := callTimeout(long(ctx), limit); got != limit {
			t.Errorf("got %s, want %s", got, limit)
		}
	})
}

// End to end through ExecuteWithContext: the connector answers after the transport cap
// but well inside the caller's deadline. Only a LongCall waits for it.
func TestExecuteWithContext_LongCallOutlastsTheTransportCap(t *testing.T) {
	const id = "zzfakelongcall"
	tools := writeFakeConnector(t, id, "Fake DB")
	tg, _ := countingToolGenerator(t)
	t.Setenv("TOOL_GENERATOR_URL", tg.URL)

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"success":true,"rows_affected":7}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(slow.Close)

	orig := httpCallCap
	httpCallCap = 100 * time.Millisecond
	t.Cleanup(func() { httpCallCap = orig })

	sm := NewServerManager(tools)
	host, port := hostPort(t, slow.URL)
	cacheHTTPServer(sm, id, host, port)
	client := NewClient(sm)

	run := func(longCall bool) *ExecuteResponse {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := client.ExecuteWithContext(ctx, ExecuteRequest{
			Connector: id,
			Version:   "latest",
			Operation: "execute",
			Config:    map[string]string{},
			Params:    map[string]interface{}{"query": "CREATE TABLE t AS SELECT 1"},
			LongCall:  longCall,
		})
		if err != nil {
			t.Fatalf("ExecuteWithContext(LongCall=%v) error: %v", longCall, err)
		}
		return resp
	}

	// Control: without LongCall the transport cap still applies, so the test can fail.
	if resp := run(false); resp == nil || resp.Success {
		t.Fatalf("plain call: got %+v, want a failure at the %s cap", resp, httpCallCap)
	}
	resp := run(true)
	if resp == nil || !resp.Success {
		t.Fatalf("LongCall: got %+v, want the connector's answer", resp)
	}
	if got, _ := resp.Result["rows_affected"].(float64); got != 7 {
		t.Errorf("LongCall result rows_affected = %v, want 7", resp.Result["rows_affected"])
	}
}
