package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ============================================================================
// Workflow runs — one WorkflowRunWorkflow per run of a user's workflow
// ============================================================================
// A workflow (Workflows page: a query, a condition, delivery channels) used to run in
// the api-gateway: a ticker claimed due runs and a goroutine ran each one, so a gateway
// restart mid-run lost it and two gateways could race. Now each run is a Temporal
// workflow on its own queue (workflows-design.md § 4.1, build step 4). The gateway starts
// it — a Temporal schedule per scheduled workflow, an after-pipeline fire, or Run now —
// and each step below is an activity that calls one gateway endpoint:
//
//	begin    — which run this is. A scheduled run has no row yet: begin inserts it, keyed
//	           on this workflow's id, so a retried begin finds the same row.
//	evaluate — the checks, the query and the condition. A run that sends gets its
//	           channels back; one that does not is recorded by the gateway, and ends here.
//	send     — one channel, once: the gateway claims the send before it posts, so a
//	           retry after an accepted send sends nothing again.
//	finish   — the run's status from its sends, once.
//
// The query, the message and the transports stay in the gateway. The message carries
// values from the rows, so it never enters this history: the gateway holds it, encrypted,
// between evaluate and finish. Only ids and statuses cross here.
//
// Versioning (workflows-design.md § 11 Q1): right after begin, GetVersion "workflow-steps"
// picks the path. DefaultVersion, a run that passed begin before build step 5, keeps the
// path above, byte for byte; version 1 runs the steps graph (workflow_graph_run.go), whose
// steps carry their own deadlines in place of the 10 minute one. Any later change to the
// commands this workflow emits wraps the new path in workflow.GetVersion, and the replay
// test over testdata/workflow_run_*_history.json fails a change that forgets to.

// WorkflowRunsTaskQueue is the queue workflow runs live on, apart from pipelines so a
// long pipeline cannot hold a workflow's alert behind it. The wire contract with the
// gateway's workflowRunsTaskQueue; the two modules share no types.
const WorkflowRunsTaskQueue = "workflow-runs"

const (
	// workflowRunDeadline bounds a run from begin to its last send. What is running is
	// stopped and the run is recorded failed.
	workflowRunDeadline = 10 * time.Minute
	// workflowRunBusyWait is how long a scheduled run waits for another run of its
	// workflow to finish before it skips its tick. Longer than the gateway's 15 minute
	// workflowAbandonedAfter, so a run whose workflow is gone is failed before this
	// wait gives up on it.
	workflowRunBusyWait  = 16 * time.Minute
	workflowRunBusyPause = 10 * time.Second
)

// The gateway's answers.
const (
	workflowRunActionRun  = "run"
	workflowRunActionBusy = "busy"

	workflowRunOutcomeSend = "send"

	// WorkflowRunAbortDeadline and WorkflowRunAbortStepFailed are finish's abort reasons.
	WorkflowRunAbortDeadline   = "deadline"
	WorkflowRunAbortStepFailed = "step_failed"
)

// WorkflowRunInput is WorkflowRunWorkflow's argument, the wire contract with the
// gateway's workflowRunInput. A scheduled run has no RunID: its begin inserts the run.
type WorkflowRunInput struct {
	RunID      string `json:"run_id,omitempty"`
	WorkflowID string `json:"workflow_id"`
	Trigger    string `json:"trigger"`
}

// WorkflowRunBeginInput asks the gateway which run this is.
type WorkflowRunBeginInput struct {
	WorkflowID         string `json:"workflow_id"`
	RunID              string `json:"run_id,omitempty"`
	Trigger            string `json:"trigger"`
	TemporalWorkflowID string `json:"temporal_workflow_id"`
}

// WorkflowRunBegun is begin's answer: action "run" with the run id, "busy" (ask again
// later) or "none" (end).
type WorkflowRunBegun struct {
	Action string `json:"action"`
	RunID  string `json:"run_id,omitempty"`
}

// WorkflowRunEvaluated is evaluate's answer: outcome "send" with the channels to send
// to, in order, or "finished" (the gateway recorded the run).
type WorkflowRunEvaluated struct {
	Outcome  string   `json:"outcome"`
	Channels []string `json:"channels,omitempty"`
}

// WorkflowRunSendInput names one send of a run.
type WorkflowRunSendInput struct {
	RunID     string `json:"run_id"`
	ChannelID string `json:"channel_id"`
}

// WorkflowRunFinishInput records a run: Abort is "", WorkflowRunAbortDeadline or
// WorkflowRunAbortStepFailed.
type WorkflowRunFinishInput struct {
	RunID string `json:"run_id"`
	Abort string `json:"abort"`
}

// WorkflowRunStatus is send's and finish's answer.
type WorkflowRunStatus struct {
	Status string `json:"status"`
}

// workflowRunRetry is the backoff every step shares; each step sets its own attempts.
func workflowRunRetry(attempts int32) *temporal.RetryPolicy {
	return &temporal.RetryPolicy{
		InitialInterval:    5 * time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    time.Minute,
		MaximumAttempts:    attempts,
	}
}

// WorkflowRunWorkflow runs one run of a user's workflow. It returns nil once the run is
// recorded, or when there is nothing to run.
func WorkflowRunWorkflow(ctx workflow.Context, input WorkflowRunInput) error {
	logger := workflow.GetLogger(ctx)

	runID, err := beginWorkflowRun(ctx, input)
	if err != nil {
		if input.RunID == "" {
			// A scheduled run whose row may not exist: nothing to record it on. If begin
			// did insert it, the gateway fails it as abandoned.
			return err
		}
		logger.Error("could not begin the run", "workflow_id", input.WorkflowID, "run_id", input.RunID, "error", err)
		return finishWorkflowRunSteps(ctx, input.RunID, WorkflowRunAbortStepFailed)
	}
	if runID == "" {
		return nil
	}
	if workflow.GetVersion(ctx, workflowStepsChange, workflow.DefaultVersion, 1) == 1 {
		return runWorkflowGraph(ctx, runID)
	}

	// The deadline cancels the steps, not this workflow, so finish still runs.
	stepsCtx, cancelSteps := workflow.WithCancel(ctx)
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	deadline := workflow.NewTimer(timerCtx, workflowRunDeadline)
	deadlineHit := false
	workflow.Go(ctx, func(gctx workflow.Context) {
		if deadline.Get(gctx, nil) == nil {
			deadlineHit = true
			cancelSteps()
		}
	})

	recorded, err := runWorkflowRunSteps(stepsCtx, runID)
	cancelTimer()
	abort := ""
	switch {
	case deadlineHit:
		logger.Warn("the run hit its deadline", "run_id", runID, "deadline", workflowRunDeadline)
		abort = WorkflowRunAbortDeadline
	case err != nil:
		logger.Error("a step of the run failed", "run_id", runID, "error", err)
		abort = WorkflowRunAbortStepFailed
	case recorded:
		return nil
	}
	return finishWorkflowRunSteps(ctx, runID, abort)
}

// beginWorkflowRun asks the gateway which run this is, waiting while another run of the
// workflow is running. An empty run id means end.
func beginWorkflowRun(ctx workflow.Context, input WorkflowRunInput) (string, error) {
	logger := workflow.GetLogger(ctx)
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         workflowRunRetry(6),
	})
	begin := WorkflowRunBeginInput{
		WorkflowID:         input.WorkflowID,
		RunID:              input.RunID,
		Trigger:            input.Trigger,
		TemporalWorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,
	}
	waitUntil := workflow.Now(ctx).Add(workflowRunBusyWait)
	for {
		var begun WorkflowRunBegun
		if err := workflow.ExecuteActivity(actx, BeginWorkflowRunActivity, begin).Get(ctx, &begun); err != nil {
			return "", err
		}
		switch begun.Action {
		case workflowRunActionRun:
			return begun.RunID, nil
		case workflowRunActionBusy:
			if !workflow.Now(ctx).Before(waitUntil) {
				logger.Info("another run of this workflow is still running; this tick is skipped",
					"workflow_id", input.WorkflowID, "waited", workflowRunBusyWait)
				return "", nil
			}
			if err := workflow.Sleep(ctx, workflowRunBusyPause); err != nil {
				return "", err
			}
		default:
			return "", nil
		}
	}
}

// runWorkflowRunSteps evaluates the run and sends to its channels, one after another.
// recorded says the gateway recorded the run at evaluate, so there is nothing to finish.
func runWorkflowRunSteps(ctx workflow.Context, runID string) (bool, error) {
	ectx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy:         workflowRunRetry(3),
	})
	var evaluated WorkflowRunEvaluated
	if err := workflow.ExecuteActivity(ectx, EvaluateWorkflowRunActivity, runID).Get(ctx, &evaluated); err != nil {
		return false, err
	}
	if evaluated.Outcome != workflowRunOutcomeSend {
		return true, nil
	}

	// Past the gateway's 2m30s workflowSendStaleAfter, so a retry after a timed-out
	// attempt finds that send settled, not still in progress.
	sctx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 150 * time.Second,
		RetryPolicy:         workflowRunRetry(5),
	})
	for _, channelID := range evaluated.Channels {
		var sent WorkflowRunStatus
		if err := workflow.ExecuteActivity(sctx, SendWorkflowRunActivity, WorkflowRunSendInput{
			RunID: runID, ChannelID: channelID,
		}).Get(ctx, &sent); err != nil {
			return false, err
		}
	}
	return false, nil
}

// finishWorkflowRunSteps records the run. It runs disconnected, so a cancelled workflow
// still records its run, and keeps asking an unreachable gateway for a day: a run left
// running is failed as abandoned, but with less to say about why.
func finishWorkflowRunSteps(ctx workflow.Context, runID, abort string) error {
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	actx := workflow.WithActivityOptions(dctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: 24 * time.Hour,
		RetryPolicy:            workflowRunRetry(0),
	})
	var finished WorkflowRunStatus
	if err := workflow.ExecuteActivity(actx, FinishWorkflowRunActivity, WorkflowRunFinishInput{
		RunID: runID, Abort: abort,
	}).Get(dctx, &finished); err != nil {
		workflow.GetLogger(ctx).Error("could not record the run", "run_id", runID, "abort", abort, "error", err)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// The steps' activities: each calls one gateway endpoint
// ---------------------------------------------------------------------------

// BeginWorkflowRunActivity calls POST /api/v1/internal/workflows/:id/runs/begin.
func BeginWorkflowRunActivity(ctx context.Context, input WorkflowRunBeginInput) (*WorkflowRunBegun, error) {
	var out WorkflowRunBegun
	err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflows/"+input.WorkflowID+"/runs/begin", map[string]any{
		"run_id":               input.RunID,
		"trigger":              input.Trigger,
		"temporal_workflow_id": input.TemporalWorkflowID,
	}, 25*time.Second, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// EvaluateWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/evaluate.
func EvaluateWorkflowRunActivity(ctx context.Context, runID string) (*WorkflowRunEvaluated, error) {
	var out WorkflowRunEvaluated
	if err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflow-runs/"+runID+"/evaluate",
		map[string]any{}, 170*time.Second, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SendWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/send. A 409 is
// another attempt still holding the send: retried like any other answer but 200 and 400.
func SendWorkflowRunActivity(ctx context.Context, input WorkflowRunSendInput) (*WorkflowRunStatus, error) {
	var out WorkflowRunStatus
	if err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflow-runs/"+input.RunID+"/send",
		map[string]any{"channel_id": input.ChannelID}, 140*time.Second, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FinishWorkflowRunActivity calls POST /api/v1/internal/workflow-runs/:id/finish.
func FinishWorkflowRunActivity(ctx context.Context, input WorkflowRunFinishInput) (*WorkflowRunStatus, error) {
	var out WorkflowRunStatus
	if err := callWorkflowRunEndpoint(ctx, "/api/v1/internal/workflow-runs/"+input.RunID+"/finish",
		map[string]any{"abort": input.Abort}, 50*time.Second, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// errWorkflowRunRefused is the error type of a 400: the gateway refused the ids or the
// body themselves, so no retry changes the answer.
const errWorkflowRunRefused = "WorkflowRunStepRefused"

// callWorkflowRunEndpoint posts body to one of the gateway's workflow-run endpoints and
// decodes a 200 into out. Every answer but 200 and 400 is retryable: a 401 while the
// secret rotates, a 404 from a gateway mid-rollout, a 409, a 429 and a 5xx all pass.
func callWorkflowRunEndpoint(ctx context.Context, path string, body map[string]any, timeout time.Duration, out any) error {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET"))
	if secret == "" {
		return temporal.NewNonRetryableApplicationError(
			"INTERNAL_SERVICE_SECRET is not configured; a workflow run cannot reach the gateway",
			"InternalSecretMissing", nil)
	}
	apiGatewayURL := strings.TrimSpace(os.Getenv("API_GATEWAY_URL"))
	if apiGatewayURL == "" {
		apiGatewayURL = "http://api-gateway:8080"
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to encode %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiGatewayURL+path, strings.NewReader(string(encoded)))
	if err != nil {
		return fmt.Errorf("failed to build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusBadRequest:
		return temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("%s refused the call: %s", path, truncateForLog(string(raw), 400)),
			errWorkflowRunRefused, nil)
	default:
		return fmt.Errorf("%s returned http %d: %s", path, code, truncateForLog(string(raw), 400))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("failed to decode %s response: %w", path, err)
	}
	return nil
}
