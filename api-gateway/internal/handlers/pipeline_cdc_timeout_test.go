package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// A CDC proxy that stops waiting before the orchestrator's handler finishes does worse
// than fail early. Its client timeout closes the connection, the orchestrator's server
// cancels the request's context, and the handler's budget is built on that context: the
// action stops between two of its Kafka Connect steps. A recovery cut that way leaves the
// connector paused or stopped, its offsets possibly cleared, and the answer saying so is
// lost: the browser got a 502 "Orchestrator is unreachable" at 45s of a 90s budget.
//
// Each row pairs a gateway wait with the orchestrator constant it has to outlast, read
// from the orchestrator's source (the modules do not import each other). The wait must
// also end before Cloudflare's 100s cut in front of app.rsync.ai, or the browser gets a
// 524 in place of the orchestrator's answer.
func TestCDCProxyOutwaitsTheOrchestratorBudget(t *testing.T) {
	const cloudflareProxyTimeout = 100 * time.Second
	for _, tc := range []struct {
		action      string
		gatewayWait time.Duration
		file        string // the orchestrator source that declares the budget
		budget      string // the budget constant's name
	}{
		{"recover", cdcRecoverProxyTimeout, "../../../backend-orchestrator/internal/handlers/cdc_recover.go", "cdcRecoverBudget"},
		{"restart", cdcRestartProxyTimeout, "../../../backend-orchestrator/cmd/orchestrator/main.go", "cdcRestartBudget"},
		{"stop", cdcStopProxyTimeout, "../../../backend-orchestrator/internal/handlers/cdc_stop.go", "cdcStopBudget"},
		// The Backfill button and the backfill after a table change share the wait.
		{"backfill", cdcBackfillProxyTimeout, "../../../backend-orchestrator/internal/handlers/cdc.go", "cdcBackfillBudget"},
		{"table-list-update", cdcUpdateTablesProxyTimeout, "../../../backend-orchestrator/internal/handlers/cdc.go", "cdcUpdateTablesBudget"},
		{"sink-restart", cdcSinkRestartProxyTimeout, "../../../backend-orchestrator/internal/handlers/cdc_sink.go", "cdcSinkRestartBudget"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatalf("read the orchestrator's budget: %v", err)
			}
			decl := regexp.MustCompile(`(?m)^const `+tc.budget+` = (\d+) \* time\.Second$`).FindAllSubmatch(src, -1)
			if len(decl) != 1 {
				t.Fatalf("%s declares `const %s = N * time.Second` %d time(s), want 1", tc.file, tc.budget, len(decl))
			}
			secs, err := strconv.Atoi(string(decl[0][1]))
			if err != nil {
				t.Fatalf("parse %s: %v", tc.budget, err)
			}
			budget := time.Duration(secs) * time.Second

			if tc.gatewayWait <= budget {
				t.Errorf("the gateway waits %v for %s, the orchestrator's %s is %v: the gateway gives up first, "+
					"which cancels the %s between two steps and answers 502", tc.gatewayWait, tc.action, tc.budget, budget, tc.action)
			}
			if tc.gatewayWait >= cloudflareProxyTimeout {
				t.Errorf("the gateway waits %v for %s, past Cloudflare's %v cut: the browser gets a 524, not the answer",
					tc.gatewayWait, tc.action, cloudflareProxyTimeout)
			}
		})
	}
}

// The table-change helpers run one after another, after the change is committed: the
// push, then the backfill and the sink restart once the list is applied. Their callers'
// contexts end early: Cloudflare cuts the browser's request at 100s, the watcher's sweep
// of a pipeline has a 3-minute deadline that discovery also spends, and three waits past
// their budgets add up past both. A helper that stops when its caller does leaves the
// change half-made (a sink restart cut between stop_sink and start_sink leaves no
// worker), so each one runs detached from its caller, bounded by its own wait.
func TestCDCTableChangeHelpersOutliveTheCallersContext(t *testing.T) {
	var mu sync.Mutex
	var received []string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		received = append(received, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer orch.Close()
	t.Setenv("ORCHESTRATOR_URL", orch.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already gone

	if status, _, err := pushCDCTableList(ctx, "p1", []string{"public.a"}); err != nil || status != http.StatusOK {
		t.Errorf("push after the caller left: status %d, err %v; want it sent and answered", status, err)
	}
	if res := requestCDCBackfill(ctx, "p1", []string{"public.a"}, "", cdcSnapshotSourceTableEdit); res["success"] != true {
		t.Errorf("backfill after the caller left: %v; want it sent and answered", res["error"])
	}
	if res := restartCDCSink(ctx, "p1"); res["success"] != true {
		t.Errorf("sink restart after the caller left: %v; want it sent and answered", res["error"])
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"PUT /api/v1/cdc/tables",
		"POST /api/v1/cdc/pipelines/p1/backfill",
		"POST /api/v1/cdc/pipelines/p1/sink/restart",
	}
	if len(received) != len(want) {
		t.Fatalf("the orchestrator received %v, want %v", received, want)
	}
	for i := range want {
		if received[i] != want[i] {
			t.Errorf("request %d: %q, want %q", i, received[i], want[i])
		}
	}
}
