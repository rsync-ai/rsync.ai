package workflows

import (
	"context"
	"time"

	workflowdef "github.com/rsync-ai/shared/workflowdef"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ============================================================================
// A run's steps graph (workflows-design.md §§ 3.1, 4 and 5, build step 5)
// ============================================================================
// A workflow is a graph of steps: a query, a condition and sends, and process steps that
// run a pipeline, refresh a model or wait. Each step names the steps it runs after and a
// rule over their outcomes. runWorkflowGraph asks the gateway for the run's steps once,
// then starts each step as soon as workflowdef.Gate says its parents allow it, so steps
// on separate branches run side by side. Every step is driven through the gateway's
// /steps/:step/:action endpoint, which records it (one workflow_step_runs row per
// attempt) and answers with its outcome; /complete records the run from its steps.
//
// What this side owns is time: when a step starts, how long it may take (a run_pipeline
// or refresh_model step's timeout covers all its attempts; a wait gets 15 minutes past
// its own), the pauses while a pipeline or model is busy, and the Stop button's signal.
// The gateway owns everything else, so only ids, step statuses and the gateway's notes
// enter this history, never a row.
//
// A person's Stop reaches a run twice: the gateway records it on the run, so the next
// step call answers stop, and it signals workflow_run_stop, so a step waiting on a long
// pipeline run stops now rather than at its next poll.

// WorkflowRunStopSignal is the Stop button's signal, the wire contract with the
// gateway's workflowRunStopSignal. It carries nothing.
const WorkflowRunStopSignal = "workflow_run_stop"

// workflowStepsChange is the GetVersion change id of the steps graph.
const workflowStepsChange = "workflow-steps"

const (
	// workflowGraphHeadroom is how long past its own minutes a wait step may take.
	workflowGraphHeadroom = 15 * time.Minute
	// workflowModelResignalPause is the pause before a refresh_model step asks again
	// after the model skipped its run because another was in flight.
	workflowModelResignalPause = 30 * time.Second
)

// Vars so the activity test need not wait minutes; only the activity reads them.
var (
	// workflowStepPollEvery is how often a long poll asks the gateway again.
	workflowStepPollEvery = 15 * time.Second
	// workflowStepPollFor bounds one long-poll activity: the workflow then asks again,
	// so a 12 hour wait is about 150 activities in the history, not 3000.
	workflowStepPollFor = 5 * time.Minute
	// workflowStepDashboardCallTimeout is a dashboard step's gateway call: inside
	// workflowStepDashboardOptions' 10 minutes.
	workflowStepDashboardCallTimeout = 570 * time.Second
)

// Step-call actions.
const (
	workflowStepActionRun     = "run"
	workflowStepActionPlan    = "plan"
	workflowStepActionSend    = "send"
	workflowStepActionFinish  = "finish"
	workflowStepActionStart   = "start"
	workflowStepActionPoll    = "poll"
	workflowStepActionRequest = "request"
	workflowStepActionRecord  = "record"
	workflowStepActionStop    = "stop"

	workflowStopReasonStop    = "stop"
	workflowStopReasonTimeout = "timeout"
)

// WorkflowRunGraph is /graph's answer: action "run" with the steps, or "none" (the run
// has ended or is gone).
type WorkflowRunGraph struct {
	Action string                 `json:"action"`
	Steps  []WorkflowRunGraphStep `json:"steps,omitempty"`
}

// WorkflowRunGraphStep is a step as this side runs it: its place in the graph and its
// limits. The wire contract with the gateway's graphStep. TimeoutMinutes comes with its
// default already applied.
type WorkflowRunGraphStep struct {
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	After          []string `json:"after,omitempty"`
	Rule           string   `json:"rule,omitempty"`
	Case           *int     `json:"case,omitempty"`
	TimeoutMinutes int      `json:"timeout_minutes,omitempty"`
	Retries        int      `json:"retries,omitempty"`
	Minutes        int      `json:"minutes,omitempty"`
}

func (s WorkflowRunGraphStep) gateStep() workflowdef.Step {
	return workflowdef.Step{ID: s.ID, Type: s.Type, After: s.After, Rule: s.Rule, Case: s.Case}
}

// deadline is how long the step may take from its start; 0 is no deadline of its own
// (a query, a condition, a chart and a send are bounded by their activities' retries).
func (s WorkflowRunGraphStep) deadline() time.Duration {
	switch s.Type {
	case workflowdef.StepRunPipeline, workflowdef.StepRefreshModel:
		m := s.TimeoutMinutes
		if m <= 0 {
			m = workflowdef.DefaultStepTimeoutMinutes
		}
		return time.Duration(m) * time.Minute
	case workflowdef.StepWait:
		return time.Duration(s.Minutes)*time.Minute + workflowGraphHeadroom
	}
	return 0
}

// attempts is how many pipeline runs a run_pipeline step may start.
func (s WorkflowRunGraphStep) attempts() int {
	r := s.Retries
	if r < 0 {
		r = 0
	}
	if r > workflowdef.MaxRetries {
		r = workflowdef.MaxRetries
	}
	return 1 + r
}

// WorkflowRunStepInput is one call of one step. The fields after Action are the
// gateway's graphStepRequest; LongPoll makes the activity ask again, for up to
// workflowStepPollFor, while the gateway answers again.
type WorkflowRunStepInput struct {
	RunID        string `json:"run_id"`
	StepID       string `json:"step_id"`
	Action       string `json:"action"`
	Attempt      int    `json:"attempt,omitempty"`
	ChannelID    string `json:"channel_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Status       string `json:"status,omitempty"`
	Note         string `json:"note,omitempty"`
	Resignal     bool   `json:"resignal,omitempty"`
	SkippedRunID string `json:"skipped_run_id,omitempty"`
	LongPoll     bool   `json:"long_poll,omitempty"`
	// CallTimeout, when set, bounds the gateway call in place of the action's own.
	CallTimeout time.Duration `json:"call_timeout,omitempty"`
}

// WorkflowRunStepAnswer is a step call's answer, the gateway's graphStepAnswer. Status
// is the step's (a send's own for action send). Halt: the run has ended, run nothing
// more. Stop: someone stopped the run. Again: the step waits, ask again. Refused: the
// step failed for a reason no retry changes. Resignal: the model skipped its run because
// another was in flight; ask again. Signal is what a refresh_model request asks this
// side to signal the model's refresh workflow with.
type WorkflowRunStepAnswer struct {
	Status       string                     `json:"status"`
	Case         *int                       `json:"case,omitempty"`
	Halt         bool                       `json:"halt,omitempty"`
	Stop         bool                       `json:"stop,omitempty"`
	Channels     []string                   `json:"channels,omitempty"`
	Again        bool                       `json:"again,omitempty"`
	Refused      bool                       `json:"refused,omitempty"`
	Resignal     bool                       `json:"resignal,omitempty"`
	SkippedRunID string                     `json:"skipped_run_id,omitempty"`
	ExecutionID  string                     `json:"execution_id,omitempty"`
	Signal       *ModelRefreshDispatchInput `json:"signal,omitempty"`
}

// ended says the step has nothing more to do: it reached an outcome, or the run ended.
func (a WorkflowRunStepAnswer) ended() bool {
	return a.Halt || a.Stop || stepStatusFinal(a.Status)
}

func stepStatusFinal(status string) bool {
	switch status {
	case workflowdef.StatusSucceeded, workflowdef.StatusWarned, workflowdef.StatusFailed,
		workflowdef.StatusSkipped, workflowdef.StatusStopped:
		return true
	}
	return false
}

// WorkflowRunCompleteInput records a graph run. Abort: this side could not run the
// graph to its end, and the run fails.
type WorkflowRunCompleteInput struct {
	RunID string `json:"run_id"`
	Abort bool   `json:"abort"`
}

// The activity options of the graph's calls.
var (
	workflowGraphAskOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         workflowRunRetry(6),
	}
	// A query step's run takes as long as the old evaluate did.
	workflowStepCallOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy:         workflowRunRetry(6),
	}
	// A dashboard step draws every tile of a board, then its PDF: the gateway gives the
	// tiles 7 minutes and the PDF 90 seconds (workflow_dashboard_step.go).
	workflowStepDashboardOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy:         workflowRunRetry(6),
	}
	// As the old send: past the gateway's 2m30s workflowSendStaleAfter.
	workflowStepSendOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 150 * time.Second,
		RetryPolicy:         workflowRunRetry(5),
	}
	// A long poll heartbeats between asks, so a stopped step's poll learns of it. More
	// attempts than a call: a gateway rollout mid-wait must not fail a 12 hour step.
	workflowStepPollOptions = workflow.ActivityOptions{
		StartToCloseTimeout: 6 * time.Minute, // workflowStepPollFor and a last ask
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy:         workflowRunRetry(10),
	}
	// SignalModelRefreshActivity runs on the pipeline queue: it starts the model's
	// refresh workflow on the queue it runs on, and that workflow lives there.
	workflowModelSignalOptions = workflow.ActivityOptions{
		TaskQueue:           "pipeline-workflows",
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         workflowRunRetry(6),
	}
	// A stop and a failure are recorded on a disconnected context, as a step ends.
	workflowStepEndOptions = workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: 10 * time.Minute,
		RetryPolicy:            workflowRunRetry(0),
	}
)

// Where the graph run stands. A run moves only forward: running, then stopping (someone
// stopped it: each step in flight records its stop) or halted (the run ended in the
// gateway, the workflow was cancelled, or this side cannot go on: steps in flight end
// without another call).
const (
	graphRunning = iota
	graphStopping
	graphHalted
)

type graphRun struct {
	runID       string
	state       int
	aborted     bool
	cancelSteps workflow.CancelFunc
}

// end moves the run to state (never back) and cancels the steps in flight.
func (g *graphRun) end(state int) {
	if state > g.state {
		g.state = state
	}
	g.cancelSteps()
}

// graphStepResult is how one step ended, for the steps after it.
type graphStepResult struct {
	ID      string
	Outcome workflowdef.StepOutcome
	Halt    bool
	Stop    bool
	// Abort: the step could not be recorded; the run cannot go on.
	Abort bool
}

// runWorkflowGraph runs a run's steps graph and records the run.
func runWorkflowGraph(ctx workflow.Context, runID string) error {
	logger := workflow.GetLogger(ctx)

	var graph WorkflowRunGraph
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflowGraphAskOptions),
		GraphWorkflowRunActivity, runID).Get(ctx, &graph); err != nil {
		logger.Error("could not read the run's steps", "run_id", runID, "error", err)
		return completeWorkflowGraph(ctx, runID, true)
	}
	if graph.Action != workflowRunActionRun {
		return nil
	}

	stepsCtx, cancelSteps := workflow.WithCancel(ctx)
	g := &graphRun{runID: runID, cancelSteps: cancelSteps}
	stop := workflow.GetSignalChannel(ctx, WorkflowRunStopSignal)
	workflow.Go(ctx, func(c workflow.Context) {
		stop.Receive(c, nil)
		logger.Info("someone stopped the run", "run_id", runID)
		g.end(graphStopping)
	})

	results := workflow.NewBufferedChannel(ctx, len(graph.Steps))
	done := make(map[string]workflowdef.StepOutcome, len(graph.Steps))
	started := make(map[string]bool, len(graph.Steps))
	inFlight := 0
	for {
		if g.state == graphRunning && ctx.Err() == nil {
			for _, s := range graph.Steps {
				if started[s.ID] {
					continue
				}
				ready, run, note := workflowdef.Gate(s.gateStep(), done)
				if !ready {
					continue
				}
				started[s.ID] = true
				inFlight++
				s := s
				workflow.Go(stepsCtx, func(c workflow.Context) {
					results.Send(ctx, g.runStep(c, s, run, note))
				})
			}
		}
		if inFlight == 0 {
			break
		}
		var r graphStepResult
		results.Receive(ctx, &r)
		inFlight--
		done[r.ID] = r.Outcome
		switch {
		case r.Abort:
			logger.Error("a step could not be recorded; the run ends", "run_id", runID, "step_id", r.ID)
			g.aborted = true
			g.end(graphHalted)
		case r.Halt:
			g.end(graphHalted)
		case r.Stop:
			g.end(graphStopping)
		}
	}
	cancelSteps()
	if ctx.Err() != nil {
		// The workflow itself was cancelled.
		g.aborted = true
	}
	return completeWorkflowGraph(ctx, runID, g.aborted)
}

// runStep runs one step to its outcome, or records it skipped when run is false. It
// always returns, whatever its activities did: the run waits on every step it started.
func (g *graphRun) runStep(ctx workflow.Context, s WorkflowRunGraphStep, run bool, note string) graphStepResult {
	logger := workflow.GetLogger(ctx)
	stepCtx, cancel := workflow.WithCancel(ctx)
	defer cancel()

	timedOut := false
	if d := s.deadline(); run && d > 0 {
		workflow.Go(stepCtx, func(c workflow.Context) {
			if workflow.NewTimer(c, d).Get(c, nil) == nil {
				timedOut = true
				cancel()
			}
		})
	}

	var (
		ans WorkflowRunStepAnswer
		err error
	)
	if run {
		ans, err = g.drive(stepCtx, s)
	} else {
		ans, err = g.call(stepCtx, s, workflowStepCallOptions, WorkflowRunStepInput{
			Action: workflowStepActionRecord, Status: workflowdef.StatusSkipped, Note: note,
		})
	}
	if err == nil {
		return stepResult(s.ID, ans)
	}

	canceled := temporal.IsCanceledError(err)
	switch {
	case canceled && timedOut:
		logger.Warn("a step outlived its timeout", "run_id", g.runID, "step_id", s.ID, "timeout", s.deadline())
		ans, err = g.endStep(ctx, s, WorkflowRunStepInput{Action: workflowStepActionStop, Reason: workflowStopReasonTimeout})
	case canceled && g.state == graphStopping:
		ans, err = g.endStep(ctx, s, WorkflowRunStepInput{Action: workflowStepActionStop, Reason: workflowStopReasonStop})
	case canceled:
		// The run halted, or the workflow was cancelled: /complete records what is left.
		return graphStepResult{ID: s.ID, Outcome: workflowdef.StepOutcome{Status: workflowdef.StatusFailed, Case: -1}, Halt: true}
	default:
		// No note: the gateway records its own sentence, and an error from here may
		// carry a gateway's answer.
		logger.Error("a step failed", "run_id", g.runID, "step_id", s.ID, "error", err)
		ans, err = g.endStep(ctx, s, WorkflowRunStepInput{Action: workflowStepActionRecord, Status: workflowdef.StatusFailed})
	}
	if err != nil {
		logger.Error("could not record how a step ended", "run_id", g.runID, "step_id", s.ID, "error", err)
		return graphStepResult{ID: s.ID, Outcome: workflowdef.StepOutcome{Status: workflowdef.StatusFailed, Case: -1}, Abort: true}
	}
	return stepResult(s.ID, ans)
}

func stepResult(id string, ans WorkflowRunStepAnswer) graphStepResult {
	r := graphStepResult{ID: id, Outcome: workflowdef.StepOutcome{Status: ans.Status, Case: -1}, Halt: ans.Halt, Stop: ans.Stop}
	if ans.Case != nil {
		r.Outcome.Case = *ans.Case
	}
	return r
}

// drive runs one step's calls, by its type.
func (g *graphRun) drive(ctx workflow.Context, s WorkflowRunGraphStep) (WorkflowRunStepAnswer, error) {
	switch s.Type {
	case workflowdef.StepQuery, workflowdef.StepCondition, workflowdef.StepChart:
		return g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{Action: workflowStepActionRun})
	case workflowdef.StepDashboard:
		return g.call(ctx, s, workflowStepDashboardOptions, WorkflowRunStepInput{
			Action: workflowStepActionRun, CallTimeout: workflowStepDashboardCallTimeout,
		})
	case workflowdef.StepSend:
		return g.send(ctx, s)
	case workflowdef.StepRunPipeline:
		return g.runPipeline(ctx, s)
	case workflowdef.StepRefreshModel:
		return g.refreshModel(ctx, s)
	case workflowdef.StepWait:
		return g.wait(ctx, s)
	}
	// A step type newer than this worker.
	return WorkflowRunStepAnswer{}, temporal.NewNonRetryableApplicationError(
		"this worker does not know the step type "+s.Type, "WorkflowStepTypeUnknown", nil)
}

// send plans the step's sends, sends to each channel once, then records the step.
func (g *graphRun) send(ctx workflow.Context, s WorkflowRunGraphStep) (WorkflowRunStepAnswer, error) {
	plan, err := g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{Action: workflowStepActionPlan})
	if err != nil || plan.ended() {
		return plan, err
	}
	for _, channelID := range plan.Channels {
		sent, err := g.call(ctx, s, workflowStepSendOptions, WorkflowRunStepInput{
			Action: workflowStepActionSend, ChannelID: channelID,
		})
		if err != nil || sent.Halt || sent.Stop {
			return sent, err
		}
	}
	return g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{Action: workflowStepActionFinish})
}

// runPipeline starts the pipeline's run, waiting while the pipeline is busy, and waits
// for it to end; a failed run is started again while the step has retries left, unless
// the gateway refused it (no retry would change that).
func (g *graphRun) runPipeline(ctx workflow.Context, s WorkflowRunGraphStep) (WorkflowRunStepAnswer, error) {
	for attempt := 1; ; attempt++ {
		ans, err := g.call(ctx, s, workflowStepPollOptions, WorkflowRunStepInput{
			Action: workflowStepActionStart, Attempt: attempt, LongPoll: true,
		})
		for err == nil && ans.Again && !ans.Halt && !ans.Stop {
			ans, err = g.call(ctx, s, workflowStepPollOptions, WorkflowRunStepInput{
				Action: workflowStepActionStart, Attempt: attempt, LongPoll: true,
			})
		}
		if err != nil || ans.Halt || ans.Stop {
			return ans, err
		}
		if !stepStatusFinal(ans.Status) {
			ans, err = g.longPoll(ctx, s, WorkflowRunStepInput{Action: workflowStepActionPoll, Attempt: attempt})
			if err != nil || ans.Halt || ans.Stop {
				return ans, err
			}
		}
		if ans.Status != workflowdef.StatusFailed || ans.Refused || attempt >= s.attempts() {
			return ans, nil
		}
		workflow.GetLogger(ctx).Info("the pipeline run failed; starting it again",
			"run_id", g.runID, "step_id", s.ID, "attempt", attempt+1, "of", s.attempts())
	}
}

// refreshModel asks for the model's run, signals the model's refresh workflow, and waits
// for the run. A run the model skipped because another was in flight is asked for again.
func (g *graphRun) refreshModel(ctx workflow.Context, s WorkflowRunGraphStep) (WorkflowRunStepAnswer, error) {
	ans, err := g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{Action: workflowStepActionRequest})
	for {
		if err != nil || ans.ended() {
			return ans, err
		}
		if ans.Signal != nil {
			if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflowModelSignalOptions),
				SignalModelRefreshActivity, *ans.Signal).Get(ctx, nil); err != nil {
				return ans, err
			}
		}
		ans, err = g.longPoll(ctx, s, WorkflowRunStepInput{Action: workflowStepActionPoll})
		if err != nil || ans.Halt || ans.Stop || !ans.Resignal {
			return ans, err
		}
		if err := workflow.Sleep(ctx, workflowModelResignalPause); err != nil {
			return ans, err
		}
		ans, err = g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{
			Action: workflowStepActionRequest, Resignal: true, SkippedRunID: ans.SkippedRunID,
		})
	}
}

// wait records the wait's start, sleeps its minutes on a durable timer, and records its end.
func (g *graphRun) wait(ctx workflow.Context, s WorkflowRunGraphStep) (WorkflowRunStepAnswer, error) {
	ans, err := g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{
		Action: workflowStepActionRecord, Status: workflowdef.StatusRunning,
	})
	if err != nil || ans.ended() {
		return ans, err
	}
	if err := workflow.Sleep(ctx, time.Duration(s.Minutes)*time.Minute); err != nil {
		return ans, err
	}
	return g.call(ctx, s, workflowStepCallOptions, WorkflowRunStepInput{
		Action: workflowStepActionRecord, Status: workflowdef.StatusSucceeded,
	})
}

// longPoll asks until the gateway stops answering again.
func (g *graphRun) longPoll(ctx workflow.Context, s WorkflowRunGraphStep, in WorkflowRunStepInput) (WorkflowRunStepAnswer, error) {
	in.LongPoll = true
	for {
		ans, err := g.call(ctx, s, workflowStepPollOptions, in)
		if err != nil || !ans.Again || ans.Halt || ans.Stop {
			return ans, err
		}
	}
}

// call runs one step call.
func (g *graphRun) call(ctx workflow.Context, s WorkflowRunGraphStep, opts workflow.ActivityOptions,
	in WorkflowRunStepInput) (WorkflowRunStepAnswer, error) {
	in.RunID, in.StepID = g.runID, s.ID
	var ans WorkflowRunStepAnswer
	err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, opts), StepWorkflowRunActivity, in).Get(ctx, &ans)
	return ans, err
}

// endStep records a stop or a failure on a disconnected context: the step's own context
// is cancelled by then.
func (g *graphRun) endStep(ctx workflow.Context, s WorkflowRunGraphStep, in WorkflowRunStepInput) (WorkflowRunStepAnswer, error) {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	return g.call(dctx, s, workflowStepEndOptions, in)
}

// completeWorkflowGraph records the run from its steps. Disconnected and patient, as
// finishWorkflowRunSteps.
func completeWorkflowGraph(ctx workflow.Context, runID string, abort bool) error {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	actx := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: 24 * time.Hour,
		RetryPolicy:            workflowRunRetry(0),
	})
	var completed WorkflowRunStatus
	if err := workflow.ExecuteActivity(actx, CompleteWorkflowRunActivity, WorkflowRunCompleteInput{
		RunID: runID, Abort: abort,
	}).Get(dctx, &completed); err != nil {
		workflow.GetLogger(ctx).Error("could not record the run", "run_id", runID, "abort", abort, "error", err)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// The graph's activities
// ---------------------------------------------------------------------------

// GraphWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/graph.
func GraphWorkflowRunActivity(ctx context.Context, runID string) (*WorkflowRunGraph, error) {
	var out WorkflowRunGraph
	if err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflow-runs/"+runID+"/graph",
		map[string]any{}, 25*time.Second, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StepWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/steps/:step/:action.
// With LongPoll it asks again every workflowStepPollEvery while the gateway answers
// again, heartbeating between asks, and returns the last answer after workflowStepPollFor.
func StepWorkflowRunActivity(ctx context.Context, in WorkflowRunStepInput) (*WorkflowRunStepAnswer, error) {
	path := "/api/v1/internal/workflow-runs/" + in.RunID + "/steps/" + in.StepID + "/" + in.Action
	body := map[string]any{
		"attempt":        in.Attempt,
		"channel_id":     in.ChannelID,
		"reason":         in.Reason,
		"status":         in.Status,
		"note":           in.Note,
		"resignal":       in.Resignal,
		"skipped_run_id": in.SkippedRunID,
	}
	timeout := 50 * time.Second
	switch in.Action {
	case workflowStepActionRun, workflowStepActionPlan:
		// A query step runs the query; a send step renders its message.
		timeout = 170 * time.Second
	case workflowStepActionSend:
		timeout = 140 * time.Second
	}
	if in.CallTimeout > 0 {
		timeout = in.CallTimeout
	}
	until := time.Now().Add(workflowStepPollFor)
	for {
		var out WorkflowRunStepAnswer
		if err := callWorkflowRunEndpoint(ctx, path, body, timeout, &out); err != nil {
			return nil, err
		}
		if !in.LongPoll || !out.Again || out.Halt || out.Stop || time.Now().Add(workflowStepPollEvery).After(until) {
			return &out, nil
		}
		activity.RecordHeartbeat(ctx, out.Status)
		t := time.NewTimer(workflowStepPollEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// CompleteWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/complete.
func CompleteWorkflowRunActivity(ctx context.Context, in WorkflowRunCompleteInput) (*WorkflowRunStatus, error) {
	var out WorkflowRunStatus
	if err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflow-runs/"+in.RunID+"/complete",
		map[string]any{"abort": in.Abort}, 50*time.Second, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
