package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// Next-hop tests.
//
// The bug class: a hop that lives only in a process's memory. The gateway used to start
// the models below a run from inside the run request, after the run's history row was
// written; a restart between the two lost the hop with nothing recorded. What is guarded
// here is that both workflows that run a model now make the hop themselves — the run goes
// out with next_hop "caller", and a result that echoes it leads to exactly one listing of
// the models below and exactly one fan-out child named after the run — and that a result
// that does not echo it (an old gateway that fired the hop itself) leads to nothing, so
// no deploy order hops twice.
//
// RunModelActivity, ListDownstreamModelsActivity and the fan-out child are stand-ins
// registered under their real names; nothing here reaches a gateway or a server.

// nextHopProbe stands in for the gateway's two endpoints and for the fan-out child.
type nextHopProbe struct {
	mu sync.Mutex
	// result is what every run returns.
	result modelRunActivityResult
	// fanOut is what the downstream listing returns; listErr fails it instead.
	fanOut  UpstreamFanOutInput
	listErr error

	// childHold keeps the fan-out child open that long before it delivers.
	childHold time.Duration

	runs      []ScheduledModelRunInput
	listings  []ModelDownstreamInput
	children  []string
	fanOuts   []UpstreamFanOutInput
	delivered int
}

func (p *nextHopProbe) run(_ context.Context, in ScheduledModelRunInput) (modelRunActivityResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = append(p.runs, in)
	return p.result, nil
}

func (p *nextHopProbe) list(_ context.Context, in ModelDownstreamInput) (*UpstreamFanOutInput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listings = append(p.listings, in)
	if p.listErr != nil {
		return nil, p.listErr
	}
	out := p.fanOut
	return &out, nil
}

func (p *nextHopProbe) fanOutChild(ctx workflow.Context, in UpstreamFanOutInput) error {
	p.mu.Lock()
	p.fanOuts = append(p.fanOuts, in)
	hold := p.childHold
	p.mu.Unlock()
	if hold > 0 {
		if err := workflow.Sleep(ctx, hold); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delivered++
	return nil
}

func (p *nextHopProbe) snapshot() (runs []ScheduledModelRunInput, listings []ModelDownstreamInput, children []string, fanOuts []UpstreamFanOutInput) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append(runs, p.runs...), append(listings, p.listings...), append(children, p.children...), append(fanOuts, p.fanOuts...)
}

func newNextHopEnv(t *testing.T, probe *nextHopProbe, wf any) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(wf)
	env.RegisterWorkflowWithOptions(probe.fanOutChild, workflow.RegisterOptions{Name: "UpstreamFanOutWorkflow"})
	env.RegisterActivityWithOptions(probe.run, activity.RegisterOptions{Name: "RunModelActivity"})
	env.RegisterActivityWithOptions(probe.list, activity.RegisterOptions{Name: "ListDownstreamModelsActivity"})
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, _ converter.EncodedValues) {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		probe.children = append(probe.children, info.WorkflowType.Name+" "+info.WorkflowExecution.ID)
	})
	return env
}

func oneTargetFanOut(modelID, runID string, depth int) UpstreamFanOutInput {
	return UpstreamFanOutInput{
		UpstreamKind: "model", UpstreamID: modelID, UpstreamRunID: runID, Depth: depth,
		Targets: []UpstreamFanOutTarget{{ScheduleID: "sched-below", SavedQueryID: "model-below"}},
	}
}

// runNextHop runs one of the two model workflows to completion against the probe: the
// clock door's wrapper with a schedule's arguments, or the refresh loop with one
// completion signalled into it.
func runNextHop(t *testing.T, probe *nextHopProbe, refreshLoop bool) {
	t.Helper()
	if refreshLoop {
		env := newNextHopEnv(t, probe, ModelRefreshWorkflow)
		signalAt(env, time.Millisecond, ModelRefreshRequest{
			ScheduleID: "sched-1", UpstreamKind: "model", UpstreamID: "model-above", UpstreamRunID: "run-above",
			ExecutionID: "exec-1", Depth: 2,
		})
		env.ExecuteWorkflow(ModelRefreshWorkflow, ModelRefreshInput{SavedQueryID: "model-1"})
		if !env.IsWorkflowCompleted() {
			t.Fatal("refresh loop never completed")
		}
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("refresh loop returned an error: %v", err)
		}
		return
	}
	env := newNextHopEnv(t, probe, ScheduledModelRunWorkflow)
	env.ExecuteWorkflow(ScheduledModelRunWorkflow, ScheduledModelRunInput{ScheduleID: "sched-1", SavedQueryID: "model-1"})
	if !env.IsWorkflowCompleted() {
		t.Fatal("model run never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("model run returned an error: %v", err)
	}
}

var nextHopDoors = []struct {
	name        string
	refreshLoop bool
	// wantListing is the chain the listing must carry: the clock starts one at depth 0,
	// an event rebuild passes on the chain that woke it.
	wantListing ModelDownstreamInput
}{
	{"clock door", false, ModelDownstreamInput{SavedQueryID: "model-1", RunID: "run-1"}},
	{"event door", true, ModelDownstreamInput{SavedQueryID: "model-1", RunID: "run-1", ExecutionID: "exec-1", Depth: 2}},
}

func TestModelNextHop_AFinishedRunWakesTheModelsBelowItOnce(t *testing.T) {
	for _, door := range nextHopDoors {
		t.Run(door.name, func(t *testing.T) {
			probe := &nextHopProbe{
				result: modelRunActivityResult{Status: "succeeded", TargetTable: "public.t", RunID: "run-1", NextHop: ModelNextHopCaller},
				fanOut: oneTargetFanOut("model-1", "run-1", door.wantListing.Depth+1),
			}
			runNextHop(t, probe, door.refreshLoop)
			runs, listings, children, fanOuts := probe.snapshot()

			if len(runs) != 1 || runs[0].NextHop != ModelNextHopCaller {
				t.Fatalf("runs = %+v, want one that asks for next_hop %q", runs, ModelNextHopCaller)
			}
			if len(listings) != 1 || listings[0] != door.wantListing {
				t.Fatalf("listings = %+v, want exactly %+v", listings, door.wantListing)
			}
			want := "UpstreamFanOutWorkflow " + ModelNextHopFanOutWorkflowID("model-1", "run-1")
			if len(children) != 1 || children[0] != want {
				t.Fatalf("children = %v, want exactly [%s]", children, want)
			}
			// The fan-out carries what the gateway listed, untouched: the workflow adds
			// nothing to the chain the gateway bounds.
			if len(fanOuts) != 1 || fanOuts[0].Depth != door.wantListing.Depth+1 || len(fanOuts[0].Targets) != 1 ||
				fanOuts[0].UpstreamRunID != "run-1" {
				t.Fatalf("fan-out ran with %+v, want the listing verbatim", fanOuts)
			}
		})
	}
}

// The fan-out is the only trigger the models below get, so it must outlive the run that
// started it: the clock door returns right after the start, and the refresh loop finishes
// or continues as new. A child still delivering an hour later, long past both, delivers.
func TestModelNextHop_TheFanOutOutlivesTheRunThatStartedIt(t *testing.T) {
	for _, door := range nextHopDoors {
		t.Run(door.name, func(t *testing.T) {
			probe := &nextHopProbe{
				result:    modelRunActivityResult{Status: "succeeded", TargetTable: "public.t", RunID: "run-1", NextHop: ModelNextHopCaller},
				fanOut:    oneTargetFanOut("model-1", "run-1", door.wantListing.Depth+1),
				childHold: time.Hour,
			}
			runNextHop(t, probe, door.refreshLoop)
			probe.mu.Lock()
			delivered := probe.delivered
			probe.mu.Unlock()
			if delivered != 1 {
				t.Fatalf("the fan-out delivered %d times, want once, after the run that started it closed", delivered)
			}
		})
	}
}

// A failed run goes through the listing too: that call is what makes the gateway record
// the skip rows below it. The gateway answers with no targets, and nothing starts.
func TestModelNextHop_AFailedRunIsListedSoTheGatewayCanRecordTheSkipsBelowIt(t *testing.T) {
	for _, door := range nextHopDoors {
		t.Run(door.name, func(t *testing.T) {
			probe := &nextHopProbe{
				result: modelRunActivityResult{Status: "failed", Error: "boom", RunID: "run-1", NextHop: ModelNextHopCaller},
			}
			runNextHop(t, probe, door.refreshLoop)
			_, listings, children, _ := probe.snapshot()
			if len(listings) != 1 || listings[0].RunID != "run-1" {
				t.Fatalf("listings = %+v, want the failed run listed once", listings)
			}
			if len(children) != 0 {
				t.Fatalf("children = %v, want none for a listing with no targets", children)
			}
		})
	}
}

// The deploy-order guard. An old gateway ignores next_hop, fires the hop itself and
// echoes neither field; a result without the echo must start nothing, or the models
// below would be woken twice. A result that echoes the hop but names no run cannot be
// listed, and starts nothing either.
func TestModelNextHop_AResultThatDoesNotHandBackTheHopStartsNothing(t *testing.T) {
	results := map[string]modelRunActivityResult{
		"no echo (an old gateway fired it)": {Status: "succeeded", TargetTable: "public.t"},
		"run id without the echo":           {Status: "succeeded", RunID: "run-1"},
		"echo without a run id":             {Status: "succeeded", NextHop: ModelNextHopCaller},
		"skipped run":                       {Status: "skipped", SkipReason: "upstream_failed"},
	}
	for _, door := range nextHopDoors {
		for name, result := range results {
			t.Run(door.name+"/"+name, func(t *testing.T) {
				probe := &nextHopProbe{result: result, fanOut: oneTargetFanOut("model-1", "run-1", 1)}
				runNextHop(t, probe, door.refreshLoop)
				_, listings, children, _ := probe.snapshot()
				if len(listings) != 0 || len(children) != 0 {
					t.Fatalf("listings %+v, children %v — want neither", listings, children)
				}
			})
		}
	}
}

// The run is done and recorded before the hop; a hop that cannot be listed is an error
// in the history, never a failed run or a stopped refresh loop.
func TestModelNextHop_AListingThatFailsLeavesTheRunAlone(t *testing.T) {
	for _, door := range nextHopDoors {
		t.Run(door.name, func(t *testing.T) {
			probe := &nextHopProbe{
				result:  modelRunActivityResult{Status: "succeeded", RunID: "run-1", NextHop: ModelNextHopCaller},
				listErr: temporal.NewNonRetryableApplicationError("refused", "ModelDownstreamRefused", nil),
			}
			runNextHop(t, probe, door.refreshLoop) // fails the test itself on a workflow error
			_, listings, children, _ := probe.snapshot()
			if len(listings) != 1 || len(children) != 0 {
				t.Fatalf("listings %+v, children %v — want one refused listing and no child", listings, children)
			}
		})
	}
}

// Two rebuilds in one refresh loop are two runs, so two hops, each named after its own
// run: the second is not refused as a duplicate of the first.
func TestModelNextHop_EachRebuildInALoopHopsUnderItsOwnRun(t *testing.T) {
	probe := &nextHopProbe{fanOut: oneTargetFanOut("model-1", "", 3)}
	env := newNextHopEnv(t, probe, ModelRefreshWorkflow)
	n := 0
	env.RegisterActivityWithOptions(func(_ context.Context, in ScheduledModelRunInput) (modelRunActivityResult, error) {
		probe.mu.Lock()
		probe.runs = append(probe.runs, in)
		probe.mu.Unlock()
		n++
		if n == 1 {
			env.SignalWorkflow(ModelRefreshSignalName, ModelRefreshRequest{ScheduleID: "sched-1", UpstreamKind: "pipeline", UpstreamID: "pipe-1"})
		}
		return modelRunActivityResult{Status: "succeeded", RunID: "run-" + string(rune('0'+n)), NextHop: ModelNextHopCaller}, nil
	}, activity.RegisterOptions{Name: "RunModelActivity"})
	signalAt(env, time.Millisecond, ModelRefreshRequest{ScheduleID: "sched-1", UpstreamKind: "pipeline", UpstreamID: "pipe-1"})
	env.ExecuteWorkflow(ModelRefreshWorkflow, ModelRefreshInput{SavedQueryID: "model-1"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("refresh loop returned an error: %v", err)
	}
	_, listings, children, _ := probe.snapshot()
	wantChildren := []string{
		"UpstreamFanOutWorkflow " + ModelNextHopFanOutWorkflowID("model-1", "run-1"),
		"UpstreamFanOutWorkflow " + ModelNextHopFanOutWorkflowID("model-1", "run-2"),
	}
	if len(listings) != 2 || len(children) != 2 || children[0] != wantChildren[0] || children[1] != wantChildren[1] {
		t.Fatalf("listings %+v, children %v — want two, %v", listings, children, wantChildren)
	}
}

// The listing activity against a stand-in gateway: the request it sends, and how it
// classifies each answer.
func TestListDownstreamModelsActivity_AsksTheGatewayAndClassifiesTheAnswer(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotSecret string
	var gotBody map[string]any
	status, answer := http.StatusOK, `{"upstream_kind":"model","upstream_id":"model-1","upstream_run_id":"run-1","depth":3,"targets":[{"schedule_id":"s2","saved_query_id":"m2"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotSecret = r.URL.Path, r.Header.Get("X-Internal-Secret")
		gotBody = nil
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "next-hop-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(ListDownstreamModelsActivity)
	in := ModelDownstreamInput{SavedQueryID: "model-1", RunID: "run-1", ExecutionID: "exec-1", Depth: 2}

	val, err := env.ExecuteActivity(ListDownstreamModelsActivity, in)
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	var out UpstreamFanOutInput
	if err := val.Get(&out); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if gotPath != "/api/v1/internal/explorer/models/model-1/downstream" || gotSecret != "next-hop-unit-test-secret" ||
		gotBody["run_id"] != "run-1" || gotBody["execution_id"] != "exec-1" || gotBody["depth"] != float64(2) {
		t.Fatalf("sent %s secret=%q body=%v", gotPath, gotSecret, gotBody)
	}
	mu.Unlock()
	if out.Depth != 3 || len(out.Targets) != 1 || out.Targets[0].SavedQueryID != "m2" || out.UpstreamRunID != "run-1" {
		t.Fatalf("decoded %+v", out)
	}

	for _, c := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusNotFound, true}, // a gateway that predates the route, mid-rollout
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
	} {
		mu.Lock()
		status, answer = c.status, `{"error":"x"}`
		mu.Unlock()
		_, err := env.ExecuteActivity(ListDownstreamModelsActivity, in)
		if err == nil {
			t.Fatalf("http %d: no error", c.status)
		}
		var appErr *temporal.ApplicationError
		nonRetryable := false
		if ok := errors.As(err, &appErr); ok {
			nonRetryable = appErr.NonRetryable()
		}
		if nonRetryable == c.retryable {
			t.Fatalf("http %d: non-retryable=%v, want retryable=%v (%v)", c.status, nonRetryable, c.retryable, err)
		}
	}
}

func TestListDownstreamModelsActivity_WithoutTheSecretCallsNothing(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(ListDownstreamModelsActivity)
	_, err := env.ExecuteActivity(ListDownstreamModelsActivity, ModelDownstreamInput{SavedQueryID: "m", RunID: "r"})
	var appErr *temporal.ApplicationError
	if err == nil || !errors.As(err, &appErr) || !appErr.NonRetryable() || hits != 0 {
		t.Fatalf("err=%v hits=%d, want a non-retryable error and no call", err, hits)
	}
}

// The fan-out id is a wire contract with the gateway's upstreamFanOutWorkflowID for a
// model completion; the two must spell it alike or the gateway's Run now hop and this
// one could fan out one run twice.
func TestModelNextHopFanOutWorkflowID_IsTheGatewaysSpelling(t *testing.T) {
	if got := ModelNextHopFanOutWorkflowID("m-1", "r-1"); got != "upstream-fanout:model:m-1:r-1" {
		t.Fatalf("got %q", got)
	}
}
