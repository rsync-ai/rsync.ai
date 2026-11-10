//go:build temporal_integration

package workflows

// A run of steps on a real Temporal server (workflows-design.md § 9, step 5): a query, a
// pipeline whose first run fails and whose second waits while the pipeline is busy, a
// model refresh, two sends, and a step for a failure that is skipped; and a run stopped
// while it waits.
//
// It needs a real Temporal frontend and is skipped without one:
//
//	docker run -d --name rsync-hop-test-temporal -p 127.0.0.1:27233:7233 \
//	  --entrypoint temporal temporalio/admin-tools:1.22.4 server start-dev --ip 0.0.0.0 --headless
//	TEMPORAL_TEST_HOSTPORT=127.0.0.1:27233 go test -tags temporal_integration -run WorkflowGraph ./internal/workflows/
//
// The gateway is a stand-in. With WORKFLOW_RUN_CAPTURE_DIR set it writes the runs'
// histories there; that is how testdata/workflow_run_graph_*_history.json were captured.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	workflowdef "github.com/rsync-ai/shared/workflowdef"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// graphScript answers one step call: n counts the calls of the same step, action and
// attempt, from 1.
type graphScript func(step, action string, attempt, n int, body map[string]any) WorkflowRunStepAnswer

// workflowGraphGateway stands in for the api-gateway's begin, /graph, /steps and /complete.
type workflowGraphGateway struct {
	mu      sync.Mutex
	runs    map[string]string // run id by workflow id
	steps   map[string][]WorkflowRunGraphStep
	scripts map[string]graphScript
	hits    map[string]int // "<run> <step> <action> <attempt>", "graph <run>", "complete <run>"
	calls   map[string][]string
	aborts  map[string][]bool
	onCall  func(runID, step, action string)
}

func (g *workflowGraphGateway) count(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits[key]
}

func (g *workflowGraphGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
		wfID, _ := strings.CutSuffix(rest, "/runs/begin")
		g.mu.Lock()
		runID := g.runs[wfID]
		g.mu.Unlock()
		if runID == "" {
			http.NotFound(w, r)
			return
		}
		reply(WorkflowRunBegun{Action: workflowRunActionRun, RunID: runID})
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/internal/workflow-runs/")
	parts := strings.Split(rest, "/")
	if !ok || len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	runID := parts[0]
	g.mu.Lock()
	steps, script, onCall := g.steps[runID], g.scripts[runID], g.onCall
	g.mu.Unlock()
	if script == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case parts[1] == "graph" && len(parts) == 2:
		g.mu.Lock()
		g.hits["graph "+runID]++
		g.mu.Unlock()
		reply(WorkflowRunGraph{Action: workflowRunActionRun, Steps: steps})
	case parts[1] == "complete" && len(parts) == 2:
		abort, _ := body["abort"].(bool)
		g.mu.Lock()
		g.hits["complete "+runID]++
		g.aborts[runID] = append(g.aborts[runID], abort)
		g.mu.Unlock()
		reply(WorkflowRunStatus{Status: "succeeded"})
	case parts[1] == "steps" && len(parts) == 4:
		step, action := parts[2], parts[3]
		attempt := 0
		if f, ok := body["attempt"].(float64); ok {
			attempt = int(f)
		}
		key := fmt.Sprintf("%s %s %s %d", runID, step, action, attempt)
		g.mu.Lock()
		g.hits[key]++
		n := g.hits[key]
		call := step + " " + action
		if attempt > 0 {
			call += fmt.Sprint(" ", attempt)
		}
		for _, k := range []string{"channel_id", "reason", "status"} {
			if v, _ := body[k].(string); v != "" {
				call += " " + v
			}
		}
		if note, _ := body["note"].(string); note != "" {
			call += ": " + note
		}
		g.calls[runID] = append(g.calls[runID], call)
		g.mu.Unlock()
		if onCall != nil {
			onCall(runID, step, action)
		}
		reply(script(step, action, attempt, n, body))
	default:
		http.NotFound(w, r)
	}
}

func TestWorkflowGraph_ARunOfStepsOnARealServer(t *testing.T) {
	hostPort := os.Getenv("TEMPORAL_TEST_HOSTPORT")
	if hostPort == "" {
		t.Skip("set TEMPORAL_TEST_HOSTPORT to a throwaway Temporal frontend")
	}
	c, err := client.Dial(client.Options{HostPort: hostPort, Identity: "workflow-graph-test"})
	if err != nil {
		t.Fatalf("dial %s: %v", hostPort, err)
	}
	defer c.Close()

	// Each ask is its own activity, so the history holds the workflow's own asks again.
	every, pollFor := workflowStepPollEvery, workflowStepPollFor
	workflowStepPollEvery, workflowStepPollFor = 100*time.Millisecond, 0
	defer func() { workflowStepPollEvery, workflowStepPollFor = every, pollFor }()

	suffix := fmt.Sprint(time.Now().UnixNano())
	stepsRun, stopRun := "run-steps-"+suffix, "run-stop-"+suffix
	modelID := "sq-" + suffix
	ran := func(status string) WorkflowRunStepAnswer { return WorkflowRunStepAnswer{Status: status} }

	gw := &workflowGraphGateway{
		runs: map[string]string{"wf-steps-" + suffix: stepsRun, "wf-stop-" + suffix: stopRun},
		steps: map[string][]WorkflowRunGraphStep{
			stepsRun: {
				{ID: "q", Type: workflowdef.StepQuery},
				{ID: "load", Type: workflowdef.StepRunPipeline, After: []string{"q"}, Rule: workflowdef.RuleAllSuccess,
					TimeoutMinutes: 60, Retries: 1},
				{ID: "model", Type: workflowdef.StepRefreshModel, After: []string{"load"}, Rule: workflowdef.RuleAllSuccess,
					TimeoutMinutes: workflowdef.DefaultStepTimeoutMinutes},
				{ID: "tell", Type: workflowdef.StepSend, After: []string{"model"}, Rule: workflowdef.RuleAllSuccess},
				{ID: "oops", Type: workflowdef.StepSend, After: []string{"load"}, Rule: workflowdef.RuleAnyFailed},
			},
			stopRun: {
				{ID: "w", Type: workflowdef.StepWait, Minutes: 30},
				{ID: "after", Type: workflowdef.StepQuery, After: []string{"w"}, Rule: workflowdef.RuleAllSuccess},
			},
		},
		scripts: map[string]graphScript{
			stepsRun: func(step, action string, attempt, n int, body map[string]any) WorkflowRunStepAnswer {
				switch step + " " + action {
				case "load start":
					if attempt == 2 && n == 1 {
						// The pipeline is busy with a run of its own.
						return WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Again: true}
					}
					return WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, ExecutionID: fmt.Sprint("exec-", attempt)}
				case "load poll":
					if n == 1 {
						return WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true}
					}
					if attempt == 1 {
						return ran(workflowdef.StatusFailed)
					}
					return ran(workflowdef.StatusSucceeded)
				case "model request":
					return WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Signal: &ModelRefreshDispatchInput{
						SavedQueryID: modelID, UpstreamKind: "workflow", UpstreamID: "wf-steps-" + suffix, ExecutionID: stepsRun,
					}}
				case "model poll":
					if n == 1 {
						return WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Again: true}
					}
					return ran(workflowdef.StatusWarned)
				case "tell plan":
					return WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a", "ch-b"}}
				case "tell send":
					return ran("sent")
				case "oops record":
					s, _ := body["status"].(string)
					return ran(s)
				}
				return ran(workflowdef.StatusSucceeded)
			},
			stopRun: func(step, action string, attempt, n int, body map[string]any) WorkflowRunStepAnswer {
				switch action {
				case workflowStepActionRecord:
					s, _ := body["status"].(string)
					return ran(s)
				case workflowStepActionStop:
					return WorkflowRunStepAnswer{Status: workflowdef.StatusStopped, Stop: true}
				}
				return ran(workflowdef.StatusSucceeded)
			},
		},
		hits: map[string]int{}, calls: map[string][]string{}, aborts: map[string][]bool{},
	}
	srv := httptest.NewServer(gw)
	defer srv.Close()
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "test-secret")

	queue := "workflow-graph-" + suffix
	w := worker.New(c, queue, worker.Options{Identity: "graph-worker"})
	registerWorkflowRunWorkflow(w)
	registerWorkflowRunActivities(w)
	w.RegisterActivity(GraphWorkflowRunActivity)
	w.RegisterActivity(StepWorkflowRunActivity)
	w.RegisterActivity(CompleteWorkflowRunActivity)

	// The model's refresh workflow is not under test: a stand-in takes the signal on the
	// queue the real activity is registered on.
	var signalled []ModelRefreshDispatchInput
	var signalMu sync.Mutex
	pipelines := worker.New(c, "pipeline-workflows", worker.Options{Identity: "pipeline-worker"})
	pipelines.RegisterActivityWithOptions(func(_ context.Context, in ModelRefreshDispatchInput) error {
		signalMu.Lock()
		defer signalMu.Unlock()
		signalled = append(signalled, in)
		return nil
	}, activity.RegisterOptions{Name: "SignalModelRefreshActivity"})
	for _, wk := range []worker.Worker{w, pipelines} {
		if err := wk.Start(); err != nil {
			t.Fatalf("start worker: %v", err)
		}
		defer wk.Stop()
	}

	// The stopped run: Stop arrives while its wait waits.
	waiting := make(chan struct{})
	var once sync.Once
	gw.mu.Lock()
	gw.onCall = func(runID, step, action string) {
		if runID == stopRun && step == "w" && action == workflowStepActionRecord {
			once.Do(func() { close(waiting) })
		}
	}
	gw.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := func(runID, wfID string) client.WorkflowRun {
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "workflow-run-" + runID, TaskQueue: queue},
			WorkflowRunWorkflow, WorkflowRunInput{RunID: runID, WorkflowID: wfID, Trigger: "manual"})
		if err != nil {
			t.Fatalf("start %s: %v", runID, err)
		}
		return run
	}
	steps := start(stepsRun, "wf-steps-"+suffix)
	stopped := start(stopRun, "wf-stop-"+suffix)

	select {
	case <-waiting:
	case <-time.After(60 * time.Second):
		t.Fatal("the wait step never started")
	}
	// The control: with no Stop the run waits its 30 minutes, so nothing more may happen.
	time.Sleep(2 * time.Second)
	if n := gw.count(stopRun + " w stop 0"); n != 0 {
		t.Fatalf("the wait was stopped %d times before anyone stopped it", n)
	}
	if err := c.SignalWorkflow(ctx, stopped.GetID(), "", WorkflowRunStopSignal, nil); err != nil {
		t.Fatalf("stop %s: %v", stopRun, err)
	}
	for _, r := range []client.WorkflowRun{steps, stopped} {
		if err := r.Get(ctx, nil); err != nil {
			t.Fatalf("%s did not end: %v", r.GetID(), err)
		}
	}

	time.Sleep(2 * time.Second) // a duplicate call would arrive in this window
	gw.mu.Lock()
	defer gw.mu.Unlock()
	// By step: steps on separate branches call side by side.
	wantCalls := map[string]map[string][]string{
		stepsRun: {
			"q":     {"q run"},
			"load":  {"load start 1", "load poll 1", "load poll 1", "load start 2", "load start 2", "load poll 2", "load poll 2"},
			"oops":  {"oops record skipped: It runs only when a step before it fails, and none of load did."},
			"model": {"model request", "model poll", "model poll"},
			"tell":  {"tell plan", "tell send ch-a", "tell send ch-b", "tell finish"},
		},
		stopRun: {"w": {"w record running", "w stop stop"}},
	}
	for runID, byStep := range wantCalls {
		got := map[string][]string{}
		for _, call := range gw.calls[runID] {
			step, _, _ := strings.Cut(call, " ")
			got[step] = append(got[step], call)
		}
		if len(got) != len(byStep) {
			t.Errorf("%s: steps called %v, want %v", runID, got, byStep)
		}
		for step, want := range byStep {
			if g := strings.Join(got[step], " | "); g != strings.Join(want, " | ") {
				t.Errorf("%s %s: calls\n  %s\nwant\n  %s", runID, step, g, strings.Join(want, " | "))
			}
		}
		if n := gw.hits["graph "+runID]; n != 1 {
			t.Errorf("%s: /graph asked %d times, want once", runID, n)
		}
		if a := gw.aborts[runID]; len(a) != 1 || a[0] {
			t.Errorf("%s: completes %v, want one, not aborted", runID, a)
		}
	}
	signalMu.Lock()
	if len(signalled) != 1 || signalled[0].SavedQueryID != modelID || signalled[0].ExecutionID != stepsRun {
		t.Errorf("the model was signalled %+v, want once for %s from %s", signalled, modelID, stepsRun)
	}
	signalMu.Unlock()

	if dir := os.Getenv("WORKFLOW_RUN_CAPTURE_DIR"); dir != "" {
		writeWorkflowRunHistory(t, c, dir, "workflow_run_graph_steps_history.json", steps.GetID())
		writeWorkflowRunHistory(t, c, dir, "workflow_run_graph_stopped_history.json", stopped.GetID())
	}
}
