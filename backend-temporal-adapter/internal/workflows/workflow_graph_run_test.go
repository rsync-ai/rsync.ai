package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	workflowdef "github.com/rsync-ai/shared/workflowdef"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// Steps-graph tests (build step 5).
//
// The bug class: a step that starts before its parents allow it, runs when its rule
// says skip, or outlives what should end it (its timeout, someone's Stop, the run's end),
// and a run that is never recorded. What is guarded here is that each step starts
// exactly when workflowdef.Gate lets it, side by side with the steps on other branches;
// that a skipped step is recorded with Gate's note; that a run_pipeline step retries
// only a failed run and only while it has retries; that a busy pipeline and a busy model
// are waited for; that a step's timeout, a Stop signal, a stop or halt the gateway
// answers and a cancelled workflow each end the steps in flight and start no more; and
// that every run is completed once.
//
// The activities are stand-ins registered under their real names; nothing here reaches
// a gateway (the activity tests at the end use an httptest one).

// graphProbe stands in for the gateway's graph endpoints and the model signal.
type graphProbe struct {
	mu       sync.Mutex
	graph    WorkflowRunGraph
	graphErr error
	// answers answers a step call by "step action attempt" (a run_pipeline call) or
	// "step action", in turn; the last answer repeats. errs fails it instead.
	answers map[string][]WorkflowRunStepAnswer
	errs    map[string]error

	counts map[string]int
	calls  []string
}

func newGraphProbe(steps ...WorkflowRunGraphStep) *graphProbe {
	return &graphProbe{
		graph:   WorkflowRunGraph{Action: "run", Steps: steps},
		answers: map[string][]WorkflowRunStepAnswer{},
		errs:    map[string]error{},
		counts:  map[string]int{},
	}
}

func (p *graphProbe) begin(_ context.Context, _ WorkflowRunBeginInput) (*WorkflowRunBegun, error) {
	return &WorkflowRunBegun{Action: "run", RunID: "run-1"}, nil
}

func (p *graphProbe) readGraph(_ context.Context, runID string) (*WorkflowRunGraph, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "graph "+runID)
	if p.graphErr != nil {
		return nil, p.graphErr
	}
	out := p.graph
	return &out, nil
}

func (p *graphProbe) step(_ context.Context, in WorkflowRunStepInput) (*WorkflowRunStepAnswer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := in.StepID + " " + in.Action
	switch {
	case in.Attempt > 0:
		call += fmt.Sprintf(" %d", in.Attempt)
	case in.ChannelID != "":
		call += " " + in.ChannelID
	case in.Reason != "":
		call += " " + in.Reason
	case in.Status != "":
		call += " " + in.Status
		if in.Note != "" {
			call += ": " + in.Note
		}
	}
	if in.Resignal {
		call += " resignal " + in.SkippedRunID
	}
	if in.RunID != "run-1" {
		call += " run=" + in.RunID
	}
	if in.CallTimeout > 0 {
		call += " timeout=" + in.CallTimeout.String()
	}
	p.calls = append(p.calls, call)

	key := in.StepID + " " + in.Action
	if in.Attempt > 0 {
		if _, ok := p.answers[fmt.Sprintf("%s %d", key, in.Attempt)]; ok {
			key = fmt.Sprintf("%s %d", key, in.Attempt)
		}
	}
	if err := p.errs[key]; err != nil {
		return nil, err
	}
	n := p.counts[key]
	p.counts[key]++
	if list := p.answers[key]; len(list) > 0 {
		if n >= len(list) {
			n = len(list) - 1
		}
		out := list[n]
		return &out, nil
	}
	switch in.Action {
	case workflowStepActionRecord:
		if in.Status == workflowdef.StatusRunning {
			return &WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting}, nil
		}
		return &WorkflowRunStepAnswer{Status: in.Status}, nil
	case workflowStepActionStop:
		if in.Reason == workflowStopReasonStop {
			return &WorkflowRunStepAnswer{Status: workflowdef.StatusStopped, Stop: true}, nil
		}
		return &WorkflowRunStepAnswer{Status: workflowdef.StatusFailed}, nil
	case workflowStepActionSend:
		return &WorkflowRunStepAnswer{Status: "sent"}, nil
	}
	return &WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded}, nil
}

func (p *graphProbe) signal(_ context.Context, in ModelRefreshDispatchInput) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "signal "+in.SavedQueryID+" "+in.UpstreamKind+" "+in.UpstreamID)
	return nil
}

func (p *graphProbe) complete(_ context.Context, in WorkflowRunCompleteInput) (*WorkflowRunStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fmt.Sprintf("complete %s abort=%v", in.RunID, in.Abort))
	return &WorkflowRunStatus{Status: "succeeded"}, nil
}

func (p *graphProbe) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *graphProbe) answer(key string, answers ...WorkflowRunStepAnswer) {
	p.answers[key] = answers
}

func newGraphEnv(probe *graphProbe) *testsuite.TestWorkflowEnvironment {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(WorkflowRunWorkflow)
	env.RegisterActivityWithOptions(probe.begin, activity.RegisterOptions{Name: "BeginWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.readGraph, activity.RegisterOptions{Name: "GraphWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.step, activity.RegisterOptions{Name: "StepWorkflowRunActivity"})
	env.RegisterActivityWithOptions(probe.signal, activity.RegisterOptions{Name: "SignalModelRefreshActivity"})
	env.RegisterActivityWithOptions(probe.complete, activity.RegisterOptions{Name: "CompleteWorkflowRunActivity"})
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: testRunTemporalID, TaskQueue: WorkflowRunsTaskQueue})
	return env
}

// slowPolls makes every long-poll call (a run_pipeline step's start and poll, a
// refresh_model step's poll) take per on the workflow's clock, so a timer can fire while
// a step waits on a long pipeline run. The probe logs a call as it returns: a call
// cancelled in flight is not logged.
func slowPolls(env *testsuite.TestWorkflowEnvironment, probe *graphProbe, per time.Duration) {
	isPoll := func(in WorkflowRunStepInput) bool { return in.LongPoll }
	env.OnActivity("StepWorkflowRunActivity", mock.Anything, mock.MatchedBy(isPoll)).After(per).Return(probe.step)
	env.OnActivity("StepWorkflowRunActivity", mock.Anything,
		mock.MatchedBy(func(in WorkflowRunStepInput) bool { return !isPoll(in) })).Return(probe.step)
}

func runGraph(t *testing.T, env *testsuite.TestWorkflowEnvironment) {
	t.Helper()
	env.ExecuteWorkflow(WorkflowRunWorkflow, scheduledRun)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
}

// stepCalls is the calls of one step, in order.
func stepCalls(calls []string, step string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, step+" ") {
			out = append(out, c)
		}
	}
	return out
}

func wantCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// wantCompletedOnce: the run is completed once, last, with abort.
func wantCompletedOnce(t *testing.T, calls []string, abort bool) {
	t.Helper()
	want := fmt.Sprintf("complete run-1 abort=%v", abort)
	if n := strings.Count(strings.Join(calls, "\n"), "complete "); n != 1 || calls[len(calls)-1] != want {
		t.Fatalf("calls = %v, want them to end with one %q", calls, want)
	}
}

func intp(n int) *int { return &n }

func gs(id, typ string, after ...string) WorkflowRunGraphStep {
	return WorkflowRunGraphStep{ID: id, Type: typ, After: after}
}

// A query, a condition with a send per case, and beside them a pipeline, a model and a
// wait: each step starts when its parents are done, the pipeline before the condition's
// sends, the send whose case the condition did not take is skipped with Gate's note, a
// warned model passes, and the run is completed once.
func TestWorkflowGraph_EachStepStartsWhenItsParentsAllowAndTheRunIsCompletedOnce(t *testing.T) {
	s0 := gs("s0", workflowdef.StepSend, "c")
	s0.Case = intp(0)
	s1 := gs("s1", workflowdef.StepSend, "c")
	s1.Case = intp(1)
	p := gs("p", workflowdef.StepRunPipeline, "q")
	p.TimeoutMinutes = 720
	m := gs("m", workflowdef.StepRefreshModel, "p")
	m.TimeoutMinutes = 720
	w := gs("w", workflowdef.StepWait, "m")
	w.Minutes = 5
	d := gs("d", workflowdef.StepSend, "s0", "s1", "w")
	d.Rule = workflowdef.RuleAllDone
	probe := newGraphProbe(gs("q", workflowdef.StepQuery), gs("c", workflowdef.StepCondition, "q"), s0, s1, p, m, w, d)
	probe.answer("c run", WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded, Case: intp(0)})
	probe.answer("s0 plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a", "ch-b"}})
	probe.answer("d plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-c"}})
	probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, ExecutionID: "exec-1"})
	probe.answer("m request", WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting,
		Signal: &ModelRefreshDispatchInput{SavedQueryID: "model-1", UpstreamKind: "workflow", UpstreamID: "run-1"}})
	probe.answer("m poll", WorkflowRunStepAnswer{Status: workflowdef.StatusWarned})
	env := newGraphEnv(probe)
	start := env.Now()
	runGraph(t, env)

	calls := probe.snapshot()
	wantCalls(t, calls[:2], "graph run-1", "q run")
	// The condition and the pipeline both wait on the query only, so both start in the
	// workflow task that learns the query ended.
	first := append([]string(nil), calls[2:4]...)
	sort.Strings(first)
	wantCalls(t, first, "c run", "p start 1")
	wantCalls(t, stepCalls(calls, "s0"), "s0 plan", "s0 send ch-a", "s0 send ch-b", "s0 finish")
	wantCalls(t, stepCalls(calls, "s1"), "s1 record skipped: Condition 1 was true, not this one.")
	wantCalls(t, stepCalls(calls, "p"), "p start 1", "p poll 1")
	wantCalls(t, stepCalls(calls, "m"), "m request", "m poll")
	wantCalls(t, stepCalls(calls, "signal"), "signal model-1 workflow run-1")
	wantCalls(t, stepCalls(calls, "w"), "w record running", "w record succeeded")
	wantCalls(t, stepCalls(calls, "d"), "d plan", "d send ch-c", "d finish")
	if at, after := indexOf(calls, "d plan"), indexOf(calls, "w record succeeded"); at < after {
		t.Errorf("d started before w ended: %v", calls)
	}
	if at, after := indexOf(calls, "m request"), indexOf(calls, "p poll 1"); at < after {
		t.Errorf("m started before p ended: %v", calls)
	}
	wantCompletedOnce(t, calls, false)
	if took := env.Now().Sub(start); took < 5*time.Minute {
		t.Errorf("the run took %v, want the 5 minute wait in it", took)
	}
}

// A failed query: the steps that need it to pass are recorded skipped with Gate's note,
// the step for a failure runs, an all_done step runs, and the run is still completed.
func TestWorkflowGraph_AFailedStepSkipsTheStepsThatNeedItAndRunsTheOnesForAFailure(t *testing.T) {
	alert := gs("alert", workflowdef.StepSend, "q")
	alert.Rule = workflowdef.RuleAnyFailed
	always := gs("always", workflowdef.StepSend, "c")
	always.Rule = workflowdef.RuleAllDone
	s0 := gs("s0", workflowdef.StepSend, "c")
	s0.Case = intp(0)
	probe := newGraphProbe(gs("q", workflowdef.StepQuery), gs("c", workflowdef.StepCondition, "q"), s0, alert, always)
	probe.answer("q run", WorkflowRunStepAnswer{Status: workflowdef.StatusFailed})
	probe.answer("alert plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a"}})
	probe.answer("always plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-b"}})
	env := newGraphEnv(probe)
	runGraph(t, env)

	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "c"), "c record skipped: It runs only when every step before it passes, and q did not.")
	wantCalls(t, stepCalls(calls, "s0"), "s0 record skipped: It runs only when every step before it passes, and c did not.")
	wantCalls(t, stepCalls(calls, "alert"), "alert plan", "alert send ch-a", "alert finish")
	wantCalls(t, stepCalls(calls, "always"), "always plan", "always send ch-b", "always finish")
	wantCompletedOnce(t, calls, false)

	// Control: a query that passes skips the step for a failure.
	probe = newGraphProbe(gs("q", workflowdef.StepQuery), alert)
	runGraph(t, newGraphEnv(probe))
	wantCalls(t, stepCalls(probe.snapshot(), "alert"),
		"alert record skipped: It runs only when a step before it fails, and none of q did.")
}

// A failed pipeline run is started again, as the next attempt, while the step has
// retries; a refused one is not, and retries past MaxRetries are clamped.
func TestWorkflowGraph_AFailedPipelineRunIsStartedAgainWhileTheStepHasRetries(t *testing.T) {
	failed := WorkflowRunStepAnswer{Status: workflowdef.StatusFailed}
	for _, c := range []struct {
		name    string
		retries int
		answers map[string][]WorkflowRunStepAnswer
		want    []string
	}{
		{"the third attempt passes", 2, map[string][]WorkflowRunStepAnswer{
			"p poll 1": {failed}, "p poll 2": {failed},
		}, []string{"p start 1", "p poll 1", "p start 2", "p poll 2", "p start 3", "p poll 3"}},
		{"no retries", 0, map[string][]WorkflowRunStepAnswer{
			"p poll": {failed},
		}, []string{"p start 1", "p poll 1"}},
		{"a refused start is not retried", 3, map[string][]WorkflowRunStepAnswer{
			"p start": {{Status: workflowdef.StatusFailed, Refused: true}},
		}, []string{"p start 1"}},
		{"retries past MaxRetries are clamped", 9, map[string][]WorkflowRunStepAnswer{
			"p poll": {failed},
		}, []string{"p start 1", "p poll 1", "p start 2", "p poll 2", "p start 3", "p poll 3", "p start 4", "p poll 4"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := gs("p", workflowdef.StepRunPipeline)
			p.TimeoutMinutes, p.Retries = 720, c.retries
			probe := newGraphProbe(p)
			probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning})
			for k, v := range c.answers {
				probe.answer(k, v...)
			}
			runGraph(t, newGraphEnv(probe))
			calls := probe.snapshot()
			wantCalls(t, stepCalls(calls, "p"), c.want...)
			wantCompletedOnce(t, calls, false)
		})
	}
}

// A busy pipeline is waited for, then started; a run still going is polled until it ends.
func TestWorkflowGraph_ABusyPipelineIsWaitedForThenItsRunIsPolledToTheEnd(t *testing.T) {
	p := gs("p", workflowdef.StepRunPipeline)
	p.TimeoutMinutes = 720
	probe := newGraphProbe(p)
	again := WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Again: true}
	probe.answer("p start", again, again, WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, ExecutionID: "exec-1"})
	probe.answer("p poll", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true},
		WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded})
	runGraph(t, newGraphEnv(probe))
	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "p"), "p start 1", "p start 1", "p start 1", "p poll 1", "p poll 1")
	wantCompletedOnce(t, calls, false)
}

// A model that skipped its run because another was in flight is asked again after a
// pause, with the skipped run, and signalled again.
func TestWorkflowGraph_ABusyModelIsAskedAgain(t *testing.T) {
	m := gs("m", workflowdef.StepRefreshModel)
	m.TimeoutMinutes = 720
	probe := newGraphProbe(m)
	signal := &ModelRefreshDispatchInput{SavedQueryID: "model-1", UpstreamKind: "workflow", UpstreamID: "run-1"}
	probe.answer("m request", WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Signal: signal})
	probe.answer("m poll", WorkflowRunStepAnswer{Status: workflowdef.StatusWaiting, Resignal: true, SkippedRunID: "smr-1"},
		WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded})
	env := newGraphEnv(probe)
	start := env.Now()
	runGraph(t, env)
	calls := probe.snapshot()
	wantCalls(t, calls[1:len(calls)-1], "m request", "signal model-1 workflow run-1", "m poll",
		"m request resignal smr-1", "signal model-1 workflow run-1", "m poll")
	wantCompletedOnce(t, calls, false)
	if took := env.Now().Sub(start); took < workflowModelResignalPause {
		t.Errorf("asked again after %v, want a %v pause", took, workflowModelResignalPause)
	}
}

// A pipeline run past the step's timeout is stopped at the timeout and recorded failed;
// the step after it is skipped and the one for a failure runs. Each call takes 7 minutes:
// past a 30 minute timeout the poll that would end at 35 is cut at 30; the control's run
// ends at 70, inside its 90.
func TestWorkflowGraph_AStepPastItsTimeoutIsStoppedThere(t *testing.T) {
	for _, c := range []struct {
		name    string
		timeout int
		want    []string
	}{
		{"past it", 30, append(repeat("p poll 1", 3), "p stop timeout")},
		{"control: inside it", 90, repeat("p poll 1", 9)},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := gs("p", workflowdef.StepRunPipeline)
			p.TimeoutMinutes = c.timeout
			alert := gs("alert", workflowdef.StepSend, "p")
			alert.Rule = workflowdef.RuleAnyFailed
			probe := newGraphProbe(p, gs("next", workflowdef.StepQuery, "p"), alert)
			probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning})
			running := WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true}
			probe.answer("p poll", append(repeat2(running, 8), WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded})...)
			probe.answer("alert plan", WorkflowRunStepAnswer{Status: workflowdef.StatusSkipped})
			env := newGraphEnv(probe)
			slowPolls(env, probe, 7*time.Minute)
			start := env.Now()
			runGraph(t, env)
			calls := probe.snapshot()
			wantCalls(t, stepCalls(calls, "p"), append([]string{"p start 1"}, c.want...)...)
			wantCompletedOnce(t, calls, false)
			took := env.Now().Sub(start)
			if c.timeout == 30 {
				if took < 30*time.Minute || took > 31*time.Minute {
					t.Fatalf("stopped after %v, want at the 30 minute timeout", took)
				}
				wantCalls(t, stepCalls(calls, "next"),
					"next record skipped: It runs only when every step before it passes, and p did not.")
				wantCalls(t, stepCalls(calls, "alert"), "alert plan")
			} else {
				wantCalls(t, stepCalls(calls, "next"), "next run")
			}
		})
	}
}

// A run_pipeline step with no timeout of its own (a definition from before timeouts) gets
// the 12 hour default.
func TestWorkflowGraph_AStepWithNoTimeoutGetsTheDefault(t *testing.T) {
	probe := newGraphProbe(gs("p", workflowdef.StepRunPipeline))
	running := WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true}
	probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning})
	// Without the default the run would succeed at its 7th poll, 40 hours in.
	probe.answer("p poll", append(repeat2(running, 6), WorkflowRunStepAnswer{Status: workflowdef.StatusSucceeded})...)
	env := newGraphEnv(probe)
	slowPolls(env, probe, 5*time.Hour)
	start := env.Now()
	runGraph(t, env)
	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "p"), "p start 1", "p poll 1", "p stop timeout")
	if took := env.Now().Sub(start); took < 12*time.Hour || took > 12*time.Hour+time.Minute {
		t.Fatalf("stopped after %v, want at the 12 hour default", took)
	}
}

func repeat2(a WorkflowRunStepAnswer, n int) []WorkflowRunStepAnswer {
	out := make([]WorkflowRunStepAnswer, n)
	for i := range out {
		out[i] = a
	}
	return out
}

// Stop: the steps in flight record their stop at once, not at their next poll, no step
// starts after, and the run is completed (the gateway records it stopped).
func TestWorkflowGraph_StopEndsTheStepsInFlightAndStartsNoMore(t *testing.T) {
	p := gs("p", workflowdef.StepRunPipeline)
	p.TimeoutMinutes = 720
	w := gs("w", workflowdef.StepWait)
	w.Minutes = 60
	probe := newGraphProbe(p, w, gs("after", workflowdef.StepQuery, "p", "w"))
	probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning})
	probe.answer("p poll", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true})
	env := newGraphEnv(probe)
	slowPolls(env, probe, 5*time.Minute)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(WorkflowRunStopSignal, nil) }, 12*time.Minute)
	start := env.Now()
	runGraph(t, env)
	calls := probe.snapshot()
	// The start ends at 5 minutes, a poll at 10; the poll in flight at 12 is cut there.
	wantCalls(t, stepCalls(calls, "p"), "p start 1", "p poll 1", "p stop stop")
	wantCalls(t, stepCalls(calls, "w"), "w record running", "w stop stop")
	wantCalls(t, stepCalls(calls, "after"))
	wantCompletedOnce(t, calls, false)
	if took := env.Now().Sub(start); took > 13*time.Minute {
		t.Fatalf("stopped after %v, want at the 12 minute stop", took)
	}
}

// What the gateway answers ends the run as well: a stop (someone pressed Stop between
// the signal's sends) or a halt (the run already ended there). Nothing starts after.
func TestWorkflowGraph_AStopOrHaltTheGatewayAnswersStartsNoMoreSteps(t *testing.T) {
	for _, ans := range []WorkflowRunStepAnswer{
		{Status: workflowdef.StatusStopped, Stop: true},
		{Status: "failed", Halt: true},
	} {
		probe := newGraphProbe(gs("q", workflowdef.StepQuery), gs("c", workflowdef.StepCondition, "q"))
		probe.answer("q run", ans)
		runGraph(t, newGraphEnv(probe))
		calls := probe.snapshot()
		wantCalls(t, calls, "graph run-1", "q run", "complete run-1 abort=false")
	}

	// A send answered stop or halt sends to no more channels and does not finish.
	for _, ans := range []WorkflowRunStepAnswer{
		{Status: workflowdef.StatusStopped, Stop: true},
		{Status: "failed", Halt: true},
		{Status: "sent"}, // the control
	} {
		probe := newGraphProbe(gs("tell", workflowdef.StepSend), gs("after", workflowdef.StepQuery, "tell"))
		probe.answer("tell plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a", "ch-b"}})
		probe.answer("tell send", ans)
		runGraph(t, newGraphEnv(probe))
		calls := probe.snapshot()
		if ans.Status == "sent" {
			wantCalls(t, stepCalls(calls, "tell"), "tell plan", "tell send ch-a", "tell send ch-b", "tell finish")
			continue
		}
		wantCalls(t, calls, "graph run-1", "tell plan", "tell send ch-a", "complete run-1 abort=false")
	}
}

// A step whose activity fails for good is recorded failed with no note (the gateway has
// its own sentence); one that cannot be recorded either aborts the run.
func TestWorkflowGraph_AStepThatFailsForGoodIsRecordedFailed(t *testing.T) {
	refused := temporal.NewNonRetryableApplicationError("refused", errWorkflowRunRefused, nil)

	probe := newGraphProbe(gs("q", workflowdef.StepQuery), gs("c", workflowdef.StepCondition, "q"))
	probe.errs["q run"] = refused
	runGraph(t, newGraphEnv(probe))
	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "q"), "q run", "q record failed")
	wantCalls(t, stepCalls(calls, "c"), "c record skipped: It runs only when every step before it passes, and q did not.")
	wantCompletedOnce(t, calls, false)

	probe = newGraphProbe(gs("q", workflowdef.StepQuery), gs("c", workflowdef.StepCondition, "q"))
	probe.errs["q run"] = refused
	probe.errs["q record"] = refused
	runGraph(t, newGraphEnv(probe))
	calls = probe.snapshot()
	wantCalls(t, calls, "graph run-1", "q run", "q record failed", "complete run-1 abort=true")

	// A step type newer than this worker fails, recorded.
	probe = newGraphProbe(gs("x", "launch_rocket"))
	runGraph(t, newGraphEnv(probe))
	wantCalls(t, probe.snapshot(), "graph run-1", "x record failed", "complete run-1 abort=false")
}

// A chart step is drawn by one run call, and the send after it plans only once the
// chart is drawn. A chart that fails skips that send and the run still completes.
func TestWorkflowGraph_AChartStepIsDrawnBeforeTheSendAfterIt(t *testing.T) {
	probe := newGraphProbe(gs("chart", workflowdef.StepChart), gs("tell", workflowdef.StepSend, "chart"))
	probe.answer("tell plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a"}})
	runGraph(t, newGraphEnv(probe))
	wantCalls(t, probe.snapshot(), "graph run-1", "chart run", "tell plan", "tell send ch-a", "tell finish",
		"complete run-1 abort=false")

	probe = newGraphProbe(gs("chart", workflowdef.StepChart), gs("tell", workflowdef.StepSend, "chart"))
	probe.answer("chart run", WorkflowRunStepAnswer{Status: workflowdef.StatusFailed})
	runGraph(t, newGraphEnv(probe))
	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "tell"), "tell record skipped: It runs only when every step before it passes, and chart did not.")
	wantCompletedOnce(t, calls, false)
}

// A dashboard step is drawn by one run call with its own longer timeout, and the send
// after it plans only once the board is drawn.
func TestWorkflowGraph_ADashboardStepIsDrawnBeforeTheSendAfterIt(t *testing.T) {
	probe := newGraphProbe(gs("board", workflowdef.StepDashboard), gs("tell", workflowdef.StepSend, "board"))
	probe.answer("tell plan", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Channels: []string{"ch-a"}})
	runGraph(t, newGraphEnv(probe))
	wantCalls(t, probe.snapshot(), "graph run-1", "board run timeout=9m30s", "tell plan", "tell send ch-a", "tell finish",
		"complete run-1 abort=false")
	if workflowStepDashboardCallTimeout >= workflowStepDashboardOptions.StartToCloseTimeout {
		t.Fatalf("the call's %v does not fit in the activity's %v", workflowStepDashboardCallTimeout,
			workflowStepDashboardOptions.StartToCloseTimeout)
	}

	probe = newGraphProbe(gs("board", workflowdef.StepDashboard), gs("tell", workflowdef.StepSend, "board"))
	probe.answer("board run", WorkflowRunStepAnswer{Status: workflowdef.StatusFailed})
	runGraph(t, newGraphEnv(probe))
	calls := probe.snapshot()
	wantCalls(t, stepCalls(calls, "tell"), "tell record skipped: It runs only when every step before it passes, and board did not.")
	wantCompletedOnce(t, calls, false)
}

// A cancelled workflow (an operator, a terminate-and-restart) ends its steps without
// another call and completes the run as aborted.
func TestWorkflowGraph_ACancelledRunIsCompletedAborted(t *testing.T) {
	p := gs("p", workflowdef.StepRunPipeline)
	p.TimeoutMinutes = 720
	probe := newGraphProbe(p, gs("after", workflowdef.StepQuery, "p"))
	probe.answer("p start", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning})
	probe.answer("p poll", WorkflowRunStepAnswer{Status: workflowdef.StatusRunning, Again: true})
	env := newGraphEnv(probe)
	slowPolls(env, probe, 5*time.Minute)
	env.RegisterDelayedCallback(env.CancelWorkflow, 12*time.Minute)
	env.ExecuteWorkflow(WorkflowRunWorkflow, scheduledRun)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	calls := probe.snapshot()
	wantCalls(t, calls, "graph run-1", "p start 1", "p poll 1", "complete run-1 abort=true")
}

// A run that ended before its graph was read has nothing to run and nothing to record;
// a graph that cannot be read fails the run.
func TestWorkflowGraph_ARunWithNoGraph(t *testing.T) {
	probe := newGraphProbe()
	probe.graph = WorkflowRunGraph{Action: "none"}
	runGraph(t, newGraphEnv(probe))
	wantCalls(t, probe.snapshot(), "graph run-1")

	probe = newGraphProbe()
	probe.graphErr = errors.New("gateway returned http 503")
	runGraph(t, newGraphEnv(probe))
	calls := probe.snapshot()
	wantCalls(t, calls, append(repeat("graph run-1", 6), "complete run-1 abort=true")...)
}

// ---------------------------------------------------------------------------
// The activities against an httptest gateway
// ---------------------------------------------------------------------------

type stepGateway struct {
	mu      sync.Mutex
	paths   []string
	bodies  []map[string]any
	answers []string
}

func (g *stepGateway) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g.paths = append(g.paths, r.URL.Path+" secret="+r.Header.Get("X-Internal-Secret"))
		g.bodies = append(g.bodies, body)
		i := len(g.paths) - 1
		if i >= len(g.answers) {
			i = len(g.answers) - 1
		}
		_, _ = w.Write([]byte(g.answers[i]))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "graph-activity-unit-test-secret")
}

func shortPolls(t *testing.T, every, pollFor time.Duration) {
	t.Helper()
	oldEvery, oldFor := workflowStepPollEvery, workflowStepPollFor
	workflowStepPollEvery, workflowStepPollFor = every, pollFor
	t.Cleanup(func() { workflowStepPollEvery, workflowStepPollFor = oldEvery, oldFor })
}

// A long poll asks again while the gateway answers again, and returns the first answer
// that is not; the call carries the step's fields under the gateway's names.
func TestStepWorkflowRunActivity_ALongPollAsksUntilTheAnswerIsNotAgain(t *testing.T) {
	shortPolls(t, 5*time.Millisecond, time.Minute)
	g := &stepGateway{answers: []string{
		`{"status":"running","again":true}`, `{"status":"running","again":true}`,
		`{"status":"succeeded","execution_id":"exec-1"}`,
	}}
	g.serve(t)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(StepWorkflowRunActivity)
	val, err := env.ExecuteActivity(StepWorkflowRunActivity, WorkflowRunStepInput{
		RunID: "run-1", StepID: "load_orders", Action: "poll", Attempt: 2, LongPoll: true,
	})
	if err != nil {
		t.Fatalf("activity error = %v", err)
	}
	var ans WorkflowRunStepAnswer
	if err := val.Get(&ans); err != nil {
		t.Fatal(err)
	}
	if ans.Status != "succeeded" || ans.ExecutionID != "exec-1" || ans.Again {
		t.Fatalf("answer = %+v, want succeeded exec-1", ans)
	}
	const path = "/api/v1/internal/workflow-runs/run-1/steps/load_orders/poll secret=graph-activity-unit-test-secret"
	wantCalls(t, g.paths, path, path, path)
	if b := g.bodies[0]; b["attempt"] != float64(2) || b["resignal"] != false || b["skipped_run_id"] != "" {
		t.Errorf("body = %v, want attempt 2 under the gateway's names", b)
	}

	// Control: without LongPoll the first answer comes back, again and all.
	g.paths, g.bodies = nil, nil
	val, err = env.ExecuteActivity(StepWorkflowRunActivity, WorkflowRunStepInput{
		RunID: "run-1", StepID: "load_orders", Action: "poll", Attempt: 2,
	})
	if err != nil {
		t.Fatalf("activity error = %v", err)
	}
	ans = WorkflowRunStepAnswer{}
	_ = val.Get(&ans)
	if !ans.Again || len(g.paths) != 1 {
		t.Fatalf("answer = %+v after %d calls, want the first answer, again", ans, len(g.paths))
	}
}

// A long poll is bounded: past workflowStepPollFor it returns the last again answer, and
// the workflow asks again with a fresh activity.
func TestStepWorkflowRunActivity_ALongPollIsBounded(t *testing.T) {
	shortPolls(t, 5*time.Millisecond, 60*time.Millisecond)
	g := &stepGateway{answers: []string{`{"status":"waiting","again":true}`}}
	g.serve(t)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(StepWorkflowRunActivity)
	done := make(chan error, 1)
	var ans WorkflowRunStepAnswer
	go func() {
		val, err := env.ExecuteActivity(StepWorkflowRunActivity, WorkflowRunStepInput{
			RunID: "run-1", StepID: "load_orders", Action: "start", Attempt: 1, LongPoll: true,
		})
		if err == nil {
			err = val.Get(&ans)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("activity error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll never returned")
	}
	if !ans.Again || len(g.paths) < 2 {
		t.Fatalf("answer = %+v after %d calls, want an again answer after asking more than once", ans, len(g.paths))
	}
}

// A call's own timeout replaces the action's: a gateway slower than it fails the call.
func TestStepWorkflowRunActivity_CallTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "graph-activity-unit-test-secret")
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(StepWorkflowRunActivity)
	start := time.Now()
	_, err := env.ExecuteActivity(StepWorkflowRunActivity, WorkflowRunStepInput{
		RunID: "run-1", StepID: "board", Action: "run", CallTimeout: 50 * time.Millisecond,
	})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v, want the call cut at 50ms", err, time.Since(start))
	}
}

// The graph and complete calls reach their endpoints.
func TestGraphAndCompleteWorkflowRunActivities(t *testing.T) {
	g := &stepGateway{answers: []string{
		`{"action":"run","steps":[{"id":"load","type":"run_pipeline","timeout_minutes":720,"retries":2,"required":true},` +
			`{"id":"tell","type":"send","after":["load"],"rule":"any_failed","case":1}]}`,
		`{"status":"failed"}`,
	}}
	g.serve(t)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestActivityEnvironment()
	env.RegisterActivity(GraphWorkflowRunActivity)
	env.RegisterActivity(CompleteWorkflowRunActivity)
	val, err := env.ExecuteActivity(GraphWorkflowRunActivity, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var graph WorkflowRunGraph
	_ = val.Get(&graph)
	if graph.Action != "run" || len(graph.Steps) != 2 {
		t.Fatalf("graph = %+v", graph)
	}
	load, tell := graph.Steps[0], graph.Steps[1]
	if load.Type != workflowdef.StepRunPipeline || load.TimeoutMinutes != 720 || load.Retries != 2 {
		t.Errorf("load = %+v", load)
	}
	if tell.Rule != workflowdef.RuleAnyFailed || len(tell.After) != 1 || tell.Case == nil || *tell.Case != 1 {
		t.Errorf("tell = %+v", tell)
	}
	val, err = env.ExecuteActivity(CompleteWorkflowRunActivity, WorkflowRunCompleteInput{RunID: "run-1", Abort: true})
	if err != nil {
		t.Fatal(err)
	}
	var st WorkflowRunStatus
	_ = val.Get(&st)
	const secret = " secret=graph-activity-unit-test-secret"
	wantCalls(t, g.paths, "/api/v1/internal/workflow-runs/run-1/graph"+secret,
		"/api/v1/internal/workflow-runs/run-1/complete"+secret)
	if st.Status != "failed" || g.bodies[1]["abort"] != true {
		t.Errorf("complete = %+v with body %v, want failed and abort true", st, g.bodies[1])
	}
}

// The answer is the wire contract with the gateway's graphStepAnswer, the signal with
// its modelRefreshSignal.
func TestWorkflowRunStepAnswer_IsTheGatewaysSpelling(t *testing.T) {
	raw := `{"status":"waiting","case":2,"halt":true,"channels":["ch-a"],"again":true,"refused":true,` +
		`"resignal":true,"skipped_run_id":"smr-1","execution_id":"exec-1","stop":true,` +
		`"signal":{"saved_query_id":"model-1","upstream_kind":"workflow","upstream_id":"run-1","execution_id":""}}`
	var a WorkflowRunStepAnswer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	if a.Status != "waiting" || a.Case == nil || *a.Case != 2 || !a.Halt || len(a.Channels) != 1 || !a.Again ||
		!a.Refused || !a.Resignal || a.SkippedRunID != "smr-1" || a.ExecutionID != "exec-1" || !a.Stop ||
		a.Signal == nil || a.Signal.SavedQueryID != "model-1" || a.Signal.UpstreamKind != "workflow" ||
		a.Signal.UpstreamID != "run-1" {
		t.Fatalf("decoded %+v (signal %+v)", a, a.Signal)
	}
}
