package workflows

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

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// Workflow-run tests.
//
// The bug class: a run of a user's workflow that lives only in a process's memory. The
// gateway's ticker and goroutines lost a run on a restart and could run one twice. What
// is guarded here is the order a run's steps take and that every way out of them records
// the run: a run that sends finishes once, after its last send; one the gateway recorded
// at evaluate ends there; a step that fails for good, the 10 minute deadline and a cancel
// each finish the run with the reason; a scheduled run waits for a busy one and then
// skips its tick.
//
// The four activities are stand-ins registered under their real names; nothing here
// reaches a gateway.

const testRunTemporalID = "workflow-run:wf-1:schedule-2026-10-04T06:00:00Z"

// workflowRunProbe stands in for the gateway's four endpoints.
type workflowRunProbe struct {
	mu sync.Mutex
	// begins answers begin in turn; the last answer repeats. beginErr fails it instead.
	begins   []WorkflowRunBegun
	beginErr error
	// evaluated is evaluate's answer; evaluateErr fails it instead.
	evaluated   WorkflowRunEvaluated
	evaluateErr error
	// sendErr fails the send to a channel.
	sendErr map[string]error

	beginCalls int
	steps      []string
}

func (p *workflowRunProbe) begin(_ context.Context, in WorkflowRunBeginInput) (*WorkflowRunBegun, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "begin "+in.WorkflowID+" "+in.Trigger+" "+in.TemporalWorkflowID+" run="+in.RunID)
	p.beginCalls++
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	i := p.beginCalls - 1
	if i >= len(p.begins) {
		i = len(p.begins) - 1
	}
	out := p.begins[i]
	return &out, nil
}

func (p *workflowRunProbe) evaluate(_ context.Context, runID string) (*WorkflowRunEvaluated, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "evaluate "+runID)
	if p.evaluateErr != nil {
		return nil, p.evaluateErr
	}
	out := p.evaluated
	return &out, nil
}

func (p *workflowRunProbe) send(_ context.Context, in WorkflowRunSendInput) (*WorkflowRunStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "send "+in.RunID+" "+in.ChannelID)
	if err := p.sendErr[in.ChannelID]; err != nil {
		return nil, err
	}
	return &WorkflowRunStatus{Status: "sent"}, nil
}

func (p *workflowRunProbe) finish(_ context.Context, in WorkflowRunFinishInput) (*WorkflowRunStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, "finish "+in.RunID+" abort="+in.Abort)
	return &WorkflowRunStatus{Status: "succeeded"}, nil
}

func (p *workflowRunProbe) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.steps...)
}

func newWorkflowRunEnv(probe *workflowRunProbe) *testsuite.TestWorkflowEnvironment {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(WorkflowRunWorkflow)
	env.RegisterActivityWithOptions(probe.begin, activity.RegisterOptions{Name: "BeginWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.evaluate, activity.RegisterOptions{Name: "EvaluateWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.send, activity.RegisterOptions{Name: "SendWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.finish, activity.RegisterOptions{Name: "FinishWorkflowRunActivity"})
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: testRunTemporalID, TaskQueue: WorkflowRunsTaskQueue})
	// These tests are the path a run that passed begin before build step 5 replays;
	// workflow_graph_run_test.go has the steps graph.
	env.OnGetVersion(workflowStepsChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	return env
}

var scheduledRun = WorkflowRunInput{WorkflowID: "wf-1", Trigger: "schedule"}

// runWorkflowRun runs the workflow to completion and returns its error.
func runWorkflowRun(t *testing.T, env *testsuite.TestWorkflowEnvironment, input WorkflowRunInput) error {
	t.Helper()
	env.ExecuteWorkflow(WorkflowRunWorkflow, input)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	return env.GetWorkflowError()
}

func wantSteps(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("steps\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func repeat(step string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = step
	}
	return out
}

func TestWorkflowRun_AScheduledRunBeginsOnItsOwnIdSendsInOrderAndFinishesOnce(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
		evaluated: WorkflowRunEvaluated{Outcome: "send", Channels: []string{"ch-a", "ch-b"}},
	}
	if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	wantSteps(t, probe.snapshot(),
		"begin wf-1 schedule "+testRunTemporalID+" run=",
		"evaluate run-1",
		"send run-1 ch-a",
		"send run-1 ch-b",
		"finish run-1 abort=",
	)
}

func TestWorkflowRun_RunNowBeginsWithItsRunID(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-9"}},
		evaluated: WorkflowRunEvaluated{Outcome: "send", Channels: []string{"ch-a"}},
	}
	env := newWorkflowRunEnv(probe)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: "workflow-run-run-9", TaskQueue: WorkflowRunsTaskQueue})
	if err := runWorkflowRun(t, env, WorkflowRunInput{RunID: "run-9", WorkflowID: "wf-1", Trigger: "manual"}); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	wantSteps(t, probe.snapshot(),
		"begin wf-1 manual workflow-run-run-9 run=run-9",
		"evaluate run-9",
		"send run-9 ch-a",
		"finish run-9 abort=",
	)
}

// A run the gateway recorded at evaluate (nothing to send, or it failed) has nothing
// left to finish.
func TestWorkflowRun_ARunRecordedAtEvaluateEndsThere(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
		evaluated: WorkflowRunEvaluated{Outcome: "finished"},
	}
	if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	wantSteps(t, probe.snapshot(), "begin wf-1 schedule "+testRunTemporalID+" run=", "evaluate run-1")
}

// "none": a Run now whose run is no longer running, or a scheduled one on a paused or
// deleted workflow, or one already run under this id.
func TestWorkflowRun_NothingToRunEndsAtBegin(t *testing.T) {
	probe := &workflowRunProbe{begins: []WorkflowRunBegun{{Action: "none"}}}
	if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	wantSteps(t, probe.snapshot(), "begin wf-1 schedule "+testRunTemporalID+" run=")
}

func TestWorkflowRun_AScheduledRunWaitsForABusyOneThenRuns(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "busy"}, {Action: "busy"}, {Action: "run", RunID: "run-1"}},
		evaluated: WorkflowRunEvaluated{Outcome: "finished"},
	}
	env := newWorkflowRunEnv(probe)
	start := env.Now()
	if err := runWorkflowRun(t, env, scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	begin := "begin wf-1 schedule " + testRunTemporalID + " run="
	wantSteps(t, probe.snapshot(), begin, begin, begin, "evaluate run-1")
	if waited := env.Now().Sub(start); waited < 2*workflowRunBusyPause {
		t.Fatalf("asked again after %v, want a %v pause between asks", waited, workflowRunBusyPause)
	}
}

// Past the wait the tick is skipped, not run late on top of the next one. The wait
// outlasts the gateway's 15 minutes, after which a run whose workflow is gone is failed.
func TestWorkflowRun_AScheduledRunSkipsItsTickWhenTheBusyRunOutlastsTheWait(t *testing.T) {
	probe := &workflowRunProbe{begins: []WorkflowRunBegun{{Action: "busy"}}}
	env := newWorkflowRunEnv(probe)
	start := env.Now()
	if err := runWorkflowRun(t, env, scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	asks := int(workflowRunBusyWait/workflowRunBusyPause) + 1
	wantSteps(t, probe.snapshot(), repeat("begin wf-1 schedule "+testRunTemporalID+" run=", asks)...)
	if waited := env.Now().Sub(start); waited < workflowRunBusyWait || waited < 15*time.Minute {
		t.Fatalf("gave up after %v, want %v and past the gateway's 15 minute abandoned cut-off", waited, workflowRunBusyWait)
	}
}

func TestWorkflowRun_ASendThatFailsForGoodFinishesTheRunAsAFailedStep(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
		evaluated: WorkflowRunEvaluated{Outcome: "send", Channels: []string{"ch-a", "ch-b"}},
		sendErr:   map[string]error{"ch-a": errors.New("gateway returned http 503")},
	}
	if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err != nil {
		t.Fatalf("workflow error = %v, want nil: the run is recorded", err)
	}
	// Five attempts at ch-a, none at ch-b (finish records it skipped), then finish.
	want := append([]string{"begin wf-1 schedule " + testRunTemporalID + " run=", "evaluate run-1"},
		repeat("send run-1 ch-a", 5)...)
	wantSteps(t, probe.snapshot(), append(want, "finish run-1 abort=step_failed")...)
}

func TestWorkflowRun_AnEvaluateThatFailsForGoodFinishesTheRunAsAFailedStep(t *testing.T) {
	probe := &workflowRunProbe{
		begins:      []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
		evaluateErr: errors.New("gateway returned http 500"),
	}
	if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	want := append([]string{"begin wf-1 schedule " + testRunTemporalID + " run="}, repeat("evaluate run-1", 3)...)
	wantSteps(t, probe.snapshot(), append(want, "finish run-1 abort=step_failed")...)
}

// A Run now's row exists before its workflow starts, so a begin that fails for good is
// recorded on it; a scheduled run's may not, so its workflow fails instead (and a row
// begin did insert is failed as abandoned).
func TestWorkflowRun_ABeginThatFailsForGood(t *testing.T) {
	t.Run("run now: recorded", func(t *testing.T) {
		probe := &workflowRunProbe{beginErr: errors.New("gateway returned http 502")}
		if err := runWorkflowRun(t, newWorkflowRunEnv(probe), WorkflowRunInput{RunID: "run-9", WorkflowID: "wf-1", Trigger: "manual"}); err != nil {
			t.Fatalf("workflow error = %v", err)
		}
		want := repeat("begin wf-1 manual "+testRunTemporalID+" run=run-9", 6)
		wantSteps(t, probe.snapshot(), append(want, "finish run-9 abort=step_failed")...)
	})
	t.Run("scheduled: the workflow fails", func(t *testing.T) {
		probe := &workflowRunProbe{beginErr: errors.New("gateway returned http 502")}
		if err := runWorkflowRun(t, newWorkflowRunEnv(probe), scheduledRun); err == nil {
			t.Fatal("workflow error = nil, want the begin failure")
		}
		wantSteps(t, probe.snapshot(), repeat("begin wf-1 schedule "+testRunTemporalID+" run=", 6)...)
	})
}

// Six sends of perSend each. At 110s the sixth is still sending at the 10 minute
// deadline; at 90s (the control) all six finish inside it.
func TestWorkflowRun_TheDeadlineStopsTheStepsAndFinishesTheRun(t *testing.T) {
	channels := []string{"ch-1", "ch-2", "ch-3", "ch-4", "ch-5", "ch-6"}
	for _, c := range []struct {
		name      string
		perSend   time.Duration
		wantAbort string
	}{
		{"past the deadline", 110 * time.Second, WorkflowRunAbortDeadline},
		{"control: inside it", 90 * time.Second, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			probe := &workflowRunProbe{
				begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
				evaluated: WorkflowRunEvaluated{Outcome: "send", Channels: channels},
			}
			env := newWorkflowRunEnv(probe)
			env.OnActivity("SendWorkflowRunActivity", mock.Anything, mock.Anything).After(c.perSend).Return(probe.send)
			start := env.Now()
			if err := runWorkflowRun(t, env, scheduledRun); err != nil {
				t.Fatalf("workflow error = %v", err)
			}
			steps := probe.snapshot()
			if last := steps[len(steps)-1]; last != "finish run-1 abort="+c.wantAbort {
				t.Fatalf("steps end %q, want finish with abort %q (steps %v)", last, c.wantAbort, steps)
			}
			if strings.Count(strings.Join(steps, "\n"), "finish ") != 1 {
				t.Fatalf("finished more than once: %v", steps)
			}
			// At the deadline, not after the send in flight: a cancelled step is let go
			// at once, and the sixth send would end at 11 minutes.
			took := env.Now().Sub(start)
			if c.wantAbort == WorkflowRunAbortDeadline && (took < workflowRunDeadline || took > workflowRunDeadline+30*time.Second) {
				t.Fatalf("stopped after %v, want at the %v deadline", took, workflowRunDeadline)
			}
		})
	}
}

// A cancelled workflow (an operator, a terminate-and-restart) still records its run.
func TestWorkflowRun_ACancelledRunIsStillRecorded(t *testing.T) {
	probe := &workflowRunProbe{
		begins:    []WorkflowRunBegun{{Action: "run", RunID: "run-1"}},
		evaluated: WorkflowRunEvaluated{Outcome: "send", Channels: []string{"ch-a", "ch-b"}},
	}
	env := newWorkflowRunEnv(probe)
	env.OnActivity("SendWorkflowRunActivity", mock.Anything, mock.Anything).After(time.Minute).Return(probe.send)
	env.RegisterDelayedCallback(env.CancelWorkflow, 30*time.Second)
	env.ExecuteWorkflow(WorkflowRunWorkflow, scheduledRun)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	steps := probe.snapshot()
	if last := steps[len(steps)-1]; last != "finish run-1 abort=step_failed" {
		t.Fatalf("steps = %v, want them to end with finish step_failed", steps)
	}
}

// The input is the wire contract with the gateway's workflowRunInput.
func TestWorkflowRunInput_IsTheGatewaysSpelling(t *testing.T) {
	for in, want := range map[WorkflowRunInput]string{
		{RunID: "r", WorkflowID: "w", Trigger: "manual"}: `{"run_id":"r","workflow_id":"w","trigger":"manual"}`,
		{WorkflowID: "w", Trigger: "schedule"}:           `{"workflow_id":"w","trigger":"schedule"}`,
	} {
		got, err := json.Marshal(in)
		if err != nil || string(got) != want {
			t.Fatalf("%+v encodes as %s (%v), want %s", in, got, err, want)
		}
	}
	if WorkflowRunsTaskQueue != "workflow-runs" {
		t.Fatalf("queue %q, want the gateway's workflow-runs", WorkflowRunsTaskQueue)
	}
}

func TestWorkflowRunActivities_CallTheGatewayAndClassifyTheAnswer(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotSecret string
	var gotBody map[string]any
	status, answer := http.StatusOK, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		gotPath, gotSecret = r.Method+" "+r.URL.Path, r.Header.Get("X-Internal-Secret")
		gotBody = nil
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "workflow-run-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(BeginWorkflowRunActivity)
	env.RegisterActivity(EvaluateWorkflowRunActivity)
	env.RegisterActivity(SendWorkflowRunActivity)
	env.RegisterActivity(FinishWorkflowRunActivity)

	steps := []struct {
		activity any
		input    any
		answer   string
		path     string
		body     map[string]any
		out      any
		want     any
	}{
		{BeginWorkflowRunActivity, WorkflowRunBeginInput{WorkflowID: "wf-1", Trigger: "schedule", TemporalWorkflowID: testRunTemporalID},
			`{"action":"run","run_id":"run-1"}`, "POST /api/v1/internal/workflows/wf-1/runs/begin",
			map[string]any{"run_id": "", "trigger": "schedule", "temporal_workflow_id": testRunTemporalID},
			&WorkflowRunBegun{}, &WorkflowRunBegun{Action: "run", RunID: "run-1"}},
		{EvaluateWorkflowRunActivity, "run-1",
			`{"outcome":"send","channels":["ch-a","ch-b"]}`, "POST /api/v1/internal/workflow-runs/run-1/evaluate",
			map[string]any{},
			&WorkflowRunEvaluated{}, &WorkflowRunEvaluated{Outcome: "send", Channels: []string{"ch-a", "ch-b"}}},
		{SendWorkflowRunActivity, WorkflowRunSendInput{RunID: "run-1", ChannelID: "ch-a"},
			`{"status":"sent"}`, "POST /api/v1/internal/workflow-runs/run-1/send",
			map[string]any{"channel_id": "ch-a"},
			&WorkflowRunStatus{}, &WorkflowRunStatus{Status: "sent"}},
		{FinishWorkflowRunActivity, WorkflowRunFinishInput{RunID: "run-1", Abort: WorkflowRunAbortDeadline},
			`{"status":"failed"}`, "POST /api/v1/internal/workflow-runs/run-1/finish",
			map[string]any{"abort": "deadline"},
			&WorkflowRunStatus{}, &WorkflowRunStatus{Status: "failed"}},
	}
	for _, s := range steps {
		mu.Lock()
		status, answer = http.StatusOK, s.answer
		mu.Unlock()
		val, err := env.ExecuteActivity(s.activity, s.input)
		if err != nil {
			t.Fatalf("%s: %v", s.path, err)
		}
		if err := val.Get(s.out); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		gotJSON, _ := json.Marshal(gotBody)
		wantJSON, _ := json.Marshal(s.body)
		if gotPath != s.path || gotSecret != "workflow-run-unit-test-secret" || string(gotJSON) != string(wantJSON) {
			t.Fatalf("sent %s secret=%q body=%s, want %s body=%s", gotPath, gotSecret, gotJSON, s.path, wantJSON)
		}
		mu.Unlock()
		outJSON, _ := json.Marshal(s.out)
		wantOut, _ := json.Marshal(s.want)
		if string(outJSON) != string(wantOut) {
			t.Fatalf("%s decoded %s, want %s", s.path, outJSON, wantOut)
		}

		for _, c := range []struct {
			status    int
			retryable bool
		}{
			{http.StatusBadRequest, false},
			{http.StatusUnauthorized, true},        // the secret mid-rotation
			{http.StatusNotFound, true},            // a gateway that predates the route, mid-rollout
			{http.StatusConflict, true},            // another attempt holds the send
			{http.StatusTooManyRequests, true},     // throttled
			{http.StatusInternalServerError, true}, // the database blinked
			{http.StatusServiceUnavailable, true},  // Redis down: the message cannot be held
		} {
			mu.Lock()
			status, answer = c.status, `{"error":"x"}`
			mu.Unlock()
			_, err := env.ExecuteActivity(s.activity, s.input)
			if err == nil {
				t.Fatalf("%s http %d: no error", s.path, c.status)
			}
			var appErr *temporal.ApplicationError
			nonRetryable := errors.As(err, &appErr) && appErr.NonRetryable()
			if nonRetryable == c.retryable {
				t.Fatalf("%s http %d: non-retryable=%v, want retryable=%v (%v)", s.path, c.status, nonRetryable, c.retryable, err)
			}
		}
	}
}

func TestWorkflowRunActivities_WithoutTheSecretCallNothing(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(SendWorkflowRunActivity)
	_, err := env.ExecuteActivity(SendWorkflowRunActivity, WorkflowRunSendInput{RunID: "run-1", ChannelID: "ch-a"})
	var appErr *temporal.ApplicationError
	if err == nil || !errors.As(err, &appErr) || !appErr.NonRetryable() || hits != 0 {
		t.Fatalf("err=%v hits=%d, want a non-retryable error and no call", err, hits)
	}
}
