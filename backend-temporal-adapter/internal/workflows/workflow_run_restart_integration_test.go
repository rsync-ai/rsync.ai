//go:build temporal_integration

package workflows

// The restart proof for workflow runs (workflows-design.md § 9, step 4): kill the worker
// while a run's send is in flight; the run still sends to every channel once and is
// finished once.
//
// It needs a real Temporal frontend and is skipped without one:
//
//	docker run -d --name rsync-hop-test-temporal -p 127.0.0.1:27233:7233 \
//	  --entrypoint temporal temporalio/admin-tools:1.22.4 server start-dev --ip 0.0.0.0 --headless
//	TEMPORAL_TEST_HOSTPORT=127.0.0.1:27233 go test -tags temporal_integration -run WorkflowRun ./internal/workflows/
//
// The gateway is a stand-in. With WORKFLOW_RUN_CAPTURE_DIR set it writes the runs'
// histories there; that is how the testdata/workflow_run_*_history.json replay fixtures
// were captured.

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

// workflowRunScript is how the stand-in gateway answers one workflow's run.
type workflowRunScript struct {
	runID     string
	busyFirst int      // begins answered busy before the one answered run
	channels  []string // nil: evaluate records the run and answers finished
	refuse    string   // a channel whose send is refused with a 400
}

// workflowRunGateway stands in for the api-gateway's workflow-run endpoints.
type workflowRunGateway struct {
	mu      sync.Mutex
	scripts map[string]*workflowRunScript // by workflow id
	byRun   map[string]*workflowRunScript // by run id
	hits    map[string]int                // "begin <wf>", "evaluate <run>", "send <run> <ch>", "finish <run>"
	aborts  map[string]string             // the abort each finish carried
	tids    map[string]string             // the temporal_workflow_id each begin carried
	onSend  func(runID, channel string)
}

func (g *workflowRunGateway) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[key]
}

func (g *workflowRunGateway) setOnSend(f func(runID, channel string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onSend = f
}

func (g *workflowRunGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Secret") != "test-secret" {
		http.Error(w, "bad secret", http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }

	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/internal/workflows/"); ok {
		wfID, isBegin := strings.CutSuffix(rest, "/runs/begin")
		g.mu.Lock()
		s := g.scripts[wfID]
		g.hits["begin "+wfID]++
		n := g.hits["begin "+wfID]
		g.tids[wfID], _ = body["temporal_workflow_id"].(string)
		g.mu.Unlock()
		if !isBegin || s == nil {
			http.NotFound(w, r)
			return
		}
		if n <= s.busyFirst {
			reply(WorkflowRunBegun{Action: workflowRunActionBusy})
			return
		}
		reply(WorkflowRunBegun{Action: workflowRunActionRun, RunID: s.runID})
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/internal/workflow-runs/")
	runID, step, _ := strings.Cut(rest, "/")
	g.mu.Lock()
	s := g.byRun[runID]
	onSend := g.onSend
	g.mu.Unlock()
	if !ok || s == nil {
		http.NotFound(w, r)
		return
	}
	switch step {
	case "evaluate":
		g.mu.Lock()
		g.hits["evaluate "+runID]++
		g.mu.Unlock()
		if s.channels == nil {
			reply(WorkflowRunEvaluated{Outcome: "finished"})
			return
		}
		reply(WorkflowRunEvaluated{Outcome: workflowRunOutcomeSend, Channels: s.channels})
	case "send":
		ch, _ := body["channel_id"].(string)
		g.mu.Lock()
		g.hits["send "+runID+" "+ch]++
		g.mu.Unlock()
		if onSend != nil {
			onSend(runID, ch)
		}
		if ch == s.refuse {
			http.Error(w, `{"error":"channel refused"}`, http.StatusBadRequest)
			return
		}
		reply(WorkflowRunStatus{Status: "sent"})
	case "finish":
		abort, _ := body["abort"].(string)
		g.mu.Lock()
		g.hits["finish "+runID]++
		g.aborts[runID] = abort
		g.mu.Unlock()
		reply(WorkflowRunStatus{Status: "succeeded"})
	default:
		http.NotFound(w, r)
	}
}

func registerWorkflowRunWorkflow(w worker.Worker) { w.RegisterWorkflow(WorkflowRunWorkflow) }

func registerWorkflowRunActivities(w worker.Worker) {
	w.RegisterActivity(BeginWorkflowRunActivity)
	w.RegisterActivity(EvaluateWorkflowRunActivity)
	w.RegisterActivity(SendWorkflowRunActivity)
	w.RegisterActivity(FinishWorkflowRunActivity)
}

// afterSend returns the events after the send to channel completed.
func afterSend(t *testing.T, h *historypb.History, channel string) (completed bool, rest []*historypb.HistoryEvent) {
	t.Helper()
	scheduled := map[int64]bool{}
	for i, ev := range h.GetEvents() {
		if a := ev.GetActivityTaskScheduledEventAttributes(); a != nil && a.GetActivityType().GetName() == "SendWorkflowRunActivity" {
			var in WorkflowRunSendInput
			if p := a.GetInput().GetPayloads(); len(p) == 1 && json.Unmarshal(p[0].GetData(), &in) == nil && in.ChannelID == channel {
				scheduled[ev.GetEventId()] = true
			}
		}
		if c := ev.GetActivityTaskCompletedEventAttributes(); c != nil && scheduled[c.GetScheduledEventId()] {
			return true, h.GetEvents()[i+1:]
		}
	}
	return false, nil
}

func writeWorkflowRunHistory(t *testing.T, c client.Client, dir, name, workflowID string) {
	t.Helper()
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(readHistory(t, c, workflowID))
	if err != nil {
		t.Fatalf("encode %s: %v", workflowID, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestWorkflowRun_RestartWhileASendIsInFlightLosesNothing(t *testing.T) {
	hostPort := os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if hostPort == "" {
		t.Skip("set TEMPORAL_TEST_HOSTPORT to a throwaway Temporal frontend")
	}
	c, err := client.Dial(client.Options{HostPort: hostPort, Identity: "workflow-run-restart-test"})
	if err != nil {
		t.Fatalf("dial %s: %v", hostPort, err)
	}
	defer c.Close()

	suffix := fmt.Sprint(time.Now().UnixNano())
	sched := &workflowRunScript{runID: "run-sched-" + suffix, busyFirst: 1, channels: []string{"ch-a", "ch-b"}}
	now := &workflowRunScript{runID: "run-now-" + suffix}
	refused := &workflowRunScript{runID: "run-refused-" + suffix, channels: []string{"ch-x", "ch-y"}, refuse: "ch-x"}
	gw := &workflowRunGateway{
		scripts: map[string]*workflowRunScript{"wf-sched-" + suffix: sched, "wf-now-" + suffix: now, "wf-refused-" + suffix: refused},
		byRun:   map[string]*workflowRunScript{sched.runID: sched, now.runID: now, refused.runID: refused},
		hits:    map[string]int{}, aborts: map[string]string{}, tids: map[string]string{},
	}
	srv := httptest.NewServer(gw)
	defer srv.Close()
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "test-secret")
	queue := "workflow-run-restart-" + suffix

	// Before the restart the workflow side and the activity side are separate workers, so
	// the workflow side can die while the send to ch-a is in flight and the send still answers.
	before := worker.New(c, queue, worker.Options{Identity: "before-restart", LocalActivityWorkerOnly: true})
	registerWorkflowRunWorkflow(before)
	activities := worker.New(c, queue, worker.Options{Identity: "activities-before-restart", DisableWorkflowWorker: true})
	registerWorkflowRunActivities(activities)

	stopped := make(chan struct{})
	var once sync.Once
	gw.setOnSend(func(runID, channel string) {
		if runID != sched.runID || channel != "ch-a" {
			return
		}
		// The send to ch-a is in flight. The workflow worker dies now, so nothing that
		// could remember the send to ch-b is running.
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
	schedID := "workflow-run:wf-sched-" + suffix + ":schedule-2026-10-04T06:00:00Z"
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: schedID, TaskQueue: queue},
		WorkflowRunWorkflow, WorkflowRunInput{WorkflowID: "wf-sched-" + suffix, Trigger: "schedule"})
	if err != nil {
		t.Fatalf("start the scheduled run: %v", err)
	}

	// The busy answer holds the run 10s before its second begin.
	waitUntil(t, 60*time.Second, "the send to ch-a to complete into the history", func() bool {
		done, _ := afterSend(t, readHistory(t, c, run.GetID()), "ch-a")
		return done
	})
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the workflow worker never stopped")
	}

	// The control. With no workflow worker nothing past the send to ch-a may happen, or
	// the assertions below would pass on a send made before the "restart".
	time.Sleep(3 * time.Second)
	if n := gw.count("send " + sched.runID + " ch-b"); n != 0 {
		t.Fatalf("ch-b was sent %d times with no workflow worker running; the restart proves nothing", n)
	}
	_, rest := afterSend(t, readHistory(t, c, run.GetID()), "ch-a")
	for _, ev := range rest {
		if ev.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			t.Fatalf("a workflow task started after the send to ch-a with no workflow worker: %v", ev)
		}
	}
	activities.Stop()
	gw.setOnSend(nil)

	// The restart: one worker, as the adapter runs, under a new identity.
	after := worker.New(c, queue, worker.Options{Identity: "after-restart"})
	registerWorkflowRunWorkflow(after)
	registerWorkflowRunActivities(after)
	if err := after.Start(); err != nil {
		t.Fatalf("start the worker after the restart: %v", err)
	}
	defer after.Stop()

	if err := run.Get(ctx, nil); err != nil {
		t.Fatalf("the scheduled run did not finish after the restart: %v", err)
	}
	_, rest = afterSend(t, readHistory(t, c, run.GetID()), "ch-a")
	firstWorker := ""
	for _, ev := range rest {
		if ev.GetEventType() == enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
			firstWorker = ev.GetWorkflowTaskStartedEventAttributes().GetIdentity()
			break
		}
	}
	if firstWorker != "after-restart" {
		t.Fatalf("the first workflow task after the send to ch-a ran on %q, want the restarted worker", firstWorker)
	}

	// Run now, recorded at evaluate; and a run whose first send is refused.
	nowRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "workflow-run-" + now.runID, TaskQueue: queue},
		WorkflowRunWorkflow, WorkflowRunInput{RunID: now.runID, WorkflowID: "wf-now-" + suffix, Trigger: "manual"})
	if err != nil {
		t.Fatalf("start the run-now run: %v", err)
	}
	refusedRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "workflow-run-" + refused.runID, TaskQueue: queue},
		WorkflowRunWorkflow, WorkflowRunInput{RunID: refused.runID, WorkflowID: "wf-refused-" + suffix, Trigger: "manual"})
	if err != nil {
		t.Fatalf("start the refused run: %v", err)
	}
	for _, r := range []client.WorkflowRun{nowRun, refusedRun} {
		if err := r.Get(ctx, nil); err != nil {
			t.Fatalf("%s did not finish: %v", r.GetID(), err)
		}
	}

	time.Sleep(2 * time.Second) // a duplicate step would arrive in this window
	for key, want := range map[string]int{
		"begin wf-sched-" + suffix: 2, "evaluate " + sched.runID: 1,
		"send " + sched.runID + " ch-a": 1, "send " + sched.runID + " ch-b": 1, "finish " + sched.runID: 1,
		"begin wf-now-" + suffix: 1, "evaluate " + now.runID: 1, "finish " + now.runID: 0,
		"begin wf-refused-" + suffix: 1, "evaluate " + refused.runID: 1,
		"send " + refused.runID + " ch-x": 1, "send " + refused.runID + " ch-y": 0, "finish " + refused.runID: 1,
	} {
		if n := gw.count(key); n != want {
			t.Errorf("%s: %d calls, want %d", key, n, want)
		}
	}
	gw.mu.Lock()
	if gw.aborts[sched.runID] != "" || gw.aborts[refused.runID] != WorkflowRunAbortStepFailed {
		t.Errorf("finish aborts: scheduled %q, refused %q; want \"\" and %q",
			gw.aborts[sched.runID], gw.aborts[refused.runID], WorkflowRunAbortStepFailed)
	}
	if gw.tids["wf-sched-"+suffix] != schedID {
		t.Errorf("the scheduled begin carried temporal_workflow_id %q, want %q", gw.tids["wf-sched-"+suffix], schedID)
	}
	gw.mu.Unlock()

	if dir := os.Getenv("WORKFLOW_RUN_CAPTURE_DIR"); dir != "" {
		writeWorkflowRunHistory(t, c, dir, "workflow_run_scheduled_restart_history.json", run.GetID())
		writeWorkflowRunHistory(t, c, dir, "workflow_run_now_recorded_history.json", nowRun.GetID())
		writeWorkflowRunHistory(t, c, dir, "workflow_run_send_refused_history.json", refusedRun.GetID())
	}
}
