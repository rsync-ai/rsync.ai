//go:build temporal_integration

package workflows

// The restart proof for the next hop (workflows-design.md § 13, step 2): kill the worker
// between a model run and its hop; the hop still happens once.
//
// It needs a real Temporal frontend and is skipped without one:
//
//	docker run -d --name rsync-hop-test-temporal -p 127.0.0.1:27233:7233 \
//	  --entrypoint temporal temporalio/admin-tools:1.22.4 server start-dev --ip 0.0.0.0 --headless
//	TEMPORAL_TEST_HOSTPORT=127.0.0.1:27233 go test -tags temporal_integration -run Restart ./internal/workflows/
//
// The gateway is a stand-in. The chain is three models — A on the clock, B below A, C
// below B — so both doors hop for real: the clock door (A → B) across the restart, the
// event door (B → C) after it.
//
// With NEXT_HOP_CAPTURE_DIR set it also waits for the refresh loops to finish and writes
// A's and B's histories there; that is how the testdata/*_next_hop_history.json replay
// fixtures were captured.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/encoding/protojson"
)

// hopGateway stands in for the api-gateway's run and downstream endpoints.
type hopGateway struct {
	mu     sync.Mutex
	hits   map[string]int    // "run <model>", "downstream <model>"
	asked  map[string]string // the next_hop each model's run carried
	depths map[string]int    // the depth each downstream call carried
	below  map[string]string // the model below each model, "" for none
	onRun  func(model string)
}

func (g *hopGateway) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[key]
}

func (g *hopGateway) setOnRun(f func(model string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onRun = f
}

func (g *hopGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Secret") != "test-secret" {
		http.Error(w, "bad secret", http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/internal/explorer/models/")
	model, endpoint, _ := strings.Cut(rest, "/")
	var body map[string]any
	if !ok || json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.hits[endpoint+" "+model]++
	onRun := g.onRun
	g.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch endpoint {
	case "run":
		nextHop, _ := body["next_hop"].(string)
		g.mu.Lock()
		g.asked[model] = nextHop
		g.mu.Unlock()
		if onRun != nil {
			onRun(model)
		}
		res := map[string]any{"status": "succeeded", "target_table": "public." + model}
		if nextHop == ModelNextHopCaller {
			res["run_id"] = "run-" + model
			res["next_hop"] = nextHop
		}
		_ = json.NewEncoder(w).Encode(res)
	case "downstream":
		depth, _ := body["depth"].(float64)
		runID, _ := body["run_id"].(string)
		g.mu.Lock()
		g.depths[model] = int(depth)
		below := g.below[model]
		g.mu.Unlock()
		out := UpstreamFanOutInput{UpstreamKind: "model", UpstreamID: model, UpstreamRunID: runID, Depth: int(depth) + 1,
			Targets: []UpstreamFanOutTarget{}}
		if below != "" {
			out.Targets = append(out.Targets, UpstreamFanOutTarget{ScheduleID: "sched-" + below, SavedQueryID: below})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func registerNextHopWorkflows(w worker.Worker) {
	w.RegisterWorkflow(ScheduledModelRunWorkflow)
	w.RegisterWorkflow(ModelRefreshWorkflow)
	w.RegisterWorkflow(UpstreamFanOutWorkflow)
	w.RegisterWorkflow(ModelRefreshDispatchWorkflow)
}

func registerNextHopActivities(w worker.Worker) {
	w.RegisterActivity(RunModelActivity)
	w.RegisterActivity(RecordModelRunFailureActivity)
	w.RegisterActivity(ListDownstreamModelsActivity)
	w.RegisterActivity(SignalModelRefreshActivity)
}

func readHistory(t *testing.T, c client.Client, workflowID string) *historypb.History {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	it := c.GetWorkflowHistory(ctx, workflowID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	h := &historypb.History{}
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			t.Fatalf("read the history of %s: %v", workflowID, err)
		}
		h.Events = append(h.Events, ev)
	}
	return h
}

// afterRun returns the events after the first completed activity — the model run.
func afterRun(h *historypb.History) (completed bool, rest []*historypb.HistoryEvent) {
	for i, ev := range h.GetEvents() {
		if ev.GetEventType() == enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED {
			return true, h.GetEvents()[i+1:]
		}
	}
	return false, nil
}

func waitUntil(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestModelNextHop_RestartBetweenTheRunAndItsHopLosesNothing(t *testing.T) {
	hostPort := os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if hostPort == "" {
		t.Skip("set TEMPORAL_TEST_HOSTPORT to a throwaway Temporal frontend")
	}
	c, err := client.Dial(client.Options{HostPort: hostPort, Identity: "next-hop-restart-test"})
	if err != nil {
		t.Fatalf("dial %s: %v", hostPort, err)
	}
	defer c.Close()
	prevCtx := activityCtx
	activityCtx = &ActivityContext{Temporal: c}
	t.Cleanup(func() { activityCtx = prevCtx })

	suffix := fmt.Sprint(time.Now().UnixNano())
	a, b, cm := "model-a-"+suffix, "model-b-"+suffix, "model-c-"+suffix
	queue := "next-hop-restart-" + suffix
	gw := &hopGateway{hits: map[string]int{}, asked: map[string]string{}, depths: map[string]int{},
		below: map[string]string{a: b, b: cm}}
	srv := httptest.NewServer(gw)
	defer srv.Close()
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "test-secret")

	// Before the restart the workflow side and the activity side are separate workers,
	// so the workflow side can die while A's run is in flight and the run still answers.
	before := worker.New(c, queue, worker.Options{Identity: "before-restart", LocalActivityWorkerOnly: true})
	registerNextHopWorkflows(before)
	activities := worker.New(c, queue, worker.Options{Identity: "activities-before-restart", DisableWorkflowWorker: true})
	registerNextHopActivities(activities)

	stopped := make(chan struct{})
	var once sync.Once
	gw.setOnRun(func(model string) {
		if model != a {
			return
		}
		// A's run is in flight: rebuilt, its row written, the answer not yet sent. The
		// workflow worker dies now, so nothing that could remember the hop is running.
		once.Do(func() {
			before.Stop()
			close(stopped)
		})
	})
	for _, w := range []worker.Worker{before, activities} {
		if err := w.Start(); err != nil {
			t.Fatalf("start worker: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "scheduled-model-run:" + a, TaskQueue: queue},
		ScheduledModelRunWorkflow, ScheduledModelRunInput{ScheduleID: "sched-" + a, SavedQueryID: a})
	if err != nil {
		t.Fatalf("start A's run: %v", err)
	}

	waitUntil(t, 30*time.Second, "A's run to complete into its history", func() bool {
		done, _ := afterRun(readHistory(t, c, run.GetID()))
		return done
	})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the workflow worker never stopped")
	}

	// The control. With no workflow worker nothing past the run may happen, or the
	// assertions below would pass on a hop made before the "restart".
	time.Sleep(3 * time.Second)
	if n := gw.count("downstream " + a); n != 0 {
		t.Fatalf("A's hop was listed %d times with no workflow worker running; the restart proves nothing", n)
	}
	_, rest := afterRun(readHistory(t, c, run.GetID()))
	for _, ev := range rest {
		if ev.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			t.Fatalf("a workflow task started after A's run with no workflow worker: %v", ev)
		}
	}
	activities.Stop()
	gw.setOnRun(nil)

	// The restart: one worker, as the adapter runs, under a new identity.
	after := worker.New(c, queue, worker.Options{Identity: "after-restart"})
	registerNextHopWorkflows(after)
	registerNextHopActivities(after)
	if err := after.Start(); err != nil {
		t.Fatalf("start the worker after the restart: %v", err)
	}
	defer after.Stop()

	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("A's run did not finish after the restart: %v", err)
	}
	_, rest = afterRun(readHistory(t, c, run.GetID()))
	firstWorker := ""
	for _, ev := range rest {
		if ev.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			firstWorker = ev.GetWorkflowTaskStartedEventAttributes().GetIdentity()
			break
		}
	}
	if firstWorker != "after-restart" {
		t.Fatalf("the first workflow task after A's run ran on %q, want the restarted worker", firstWorker)
	}

	// The hop reached B, B rebuilt and hopped to C, C rebuilt and found nothing below.
	waitUntil(t, 90*time.Second, "C's rebuild to list the models below it", func() bool {
		return gw.count("downstream "+cm) > 0
	})
	waitUntil(t, 30*time.Second, "A's fan-out to complete", func() bool {
		desc, err := c.DescribeWorkflowExecution(ctx, ModelNextHopFanOutWorkflowID(a, "run-"+a), "")
		return err == nil && desc.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	})
	time.Sleep(2 * time.Second) // a duplicate run or hop would arrive in this window
	for _, key := range []string{"run " + a, "downstream " + a, "run " + b, "downstream " + b, "run " + cm, "downstream " + cm} {
		if n := gw.count(key); n != 1 {
			t.Errorf("%s: %d calls, want exactly one", key, n)
		}
	}
	gw.mu.Lock()
	for _, m := range []string{a, b, cm} {
		if gw.asked[m] != ModelNextHopCaller {
			t.Errorf("%s's run asked for next_hop %q, want %q", m, gw.asked[m], ModelNextHopCaller)
		}
	}
	// The clock starts the chain at depth 0 and each hop adds one.
	if gw.depths[a] != 0 || gw.depths[b] != 1 || gw.depths[cm] != 2 {
		t.Errorf("downstream depths A=%d B=%d C=%d, want 0, 1, 2", gw.depths[a], gw.depths[b], gw.depths[cm])
	}
	gw.mu.Unlock()

	dir := os.Getenv("NEXT_HOP_CAPTURE_DIR")
	for _, m := range []string{b, cm} {
		id := ModelRefreshWorkflowID(m)
		if dir == "" {
			_ = c.TerminateWorkflow(ctx, id, "", "test over")
			continue
		}
		// The refresh loop finishes after its idle window.
		if err := c.GetWorkflow(ctx, id, "").Get(ctx, nil); err != nil {
			t.Fatalf("%s did not finish: %v", id, err)
		}
	}
	if dir != "" {
		for name, id := range map[string]string{
			"scheduled_model_run_next_hop_history.json": run.GetID(),
			"model_refresh_next_hop_history.json":       ModelRefreshWorkflowID(b),
		} {
			raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(readHistory(t, c, id))
			if err != nil {
				t.Fatalf("encode %s: %v", id, err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
	}
}
