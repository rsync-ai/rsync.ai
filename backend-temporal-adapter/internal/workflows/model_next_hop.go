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

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ============================================================================
// The next hop — a finished model run wakes the models below it
// ============================================================================
// A model run used to end in the api-gateway: the run endpoint rebuilt the model, wrote
// its history row and then, still inside the request, looked up the models waiting on it
// and started their fan-out. A gateway restart between the row and the fan-out lost the
// hop — the run was recorded, the models below never heard of it, and nothing said so.
//
// Now the run's own workflow owns the hop (workflows-design.md § 4.2). It sends
// next_hop "caller" with the run, the gateway answers with the run's history row instead
// of firing anything, and the workflow asks the gateway who is below that row and starts
// one UpstreamFanOutWorkflow for them. Each step is in this workflow's history, so a
// worker or gateway restart anywhere between the run and the fan-out resumes from the
// last recorded step instead of losing it.
//
// NO GetVersion. The decision is the activity's recorded result: an execution whose
// run went out without next_hop got a result without it, and replays to the same
// commands. That is also what makes the deploy order free — an old gateway ignores
// next_hop and fires as before, an old adapter never sends it, and neither combination
// hops twice or not at all.

// ModelNextHopCaller is the value of next_hop that hands the hop to the workflow. The
// wire contract with the gateway's internalModelRunRequest.NextHop; the two modules
// share no types.
const ModelNextHopCaller = "caller"

// modelNextHopHorizon is how long the workflow keeps asking an unreachable gateway for
// the models below a run. A restart or a rollout is minutes; past this the hop is lost
// with an error in this history, and the models below rebuild on their next upstream.
const modelNextHopHorizon = 30 * time.Minute

// ModelDownstreamInput names the finished run whose downstream models are wanted.
type ModelDownstreamInput struct {
	SavedQueryID string `json:"saved_query_id"`
	RunID        string `json:"run_id"`
	// ExecutionID and Depth are the chain the run belongs to, as the run carried them.
	// The gateway adds the hop to Depth.
	ExecutionID string `json:"execution_id,omitempty"`
	Depth       int    `json:"depth,omitempty"`
}

// ModelNextHopFanOutWorkflowID names the fan-out a model run starts. The same string as
// the gateway's upstreamFanOutWorkflowID for a model completion, so the two paths can
// never fan out one run twice: whichever starts second is refused as a duplicate.
func ModelNextHopFanOutWorkflowID(savedQueryID, runID string) string {
	return "upstream-fanout:model:" + savedQueryID + ":" + runID
}

// startModelNextHop starts the fan-out below a finished run, when the gateway left it
// to this workflow. Best-effort, like recordModelRunFailure: the run itself is done and
// recorded, so a hop that cannot be started is logged in this history and never fails
// the run or the refresh loop.
func startModelNextHop(ctx workflow.Context, run ScheduledModelRunInput, result modelRunActivityResult) {
	if result.NextHop != ModelNextHopCaller || result.RunID == "" {
		return
	}
	logger := workflow.GetLogger(ctx)

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    time.Minute,
		ScheduleToCloseTimeout: modelNextHopHorizon,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
		},
	})
	var fanOut UpstreamFanOutInput
	if err := workflow.ExecuteActivity(actx, ListDownstreamModelsActivity, ModelDownstreamInput{
		SavedQueryID: run.SavedQueryID,
		RunID:        result.RunID,
		ExecutionID:  run.ExecutionID,
		Depth:        run.Depth,
	}).Get(ctx, &fanOut); err != nil {
		logger.Error("could not list the models below this run; they will not rebuild from it",
			"saved_query_id", run.SavedQueryID, "run_id", result.RunID, "error", err)
		return
	}
	if len(fanOut.Targets) == 0 {
		// Nothing waits on this model, or the run did not succeed and the gateway has
		// already recorded the skip rows below it.
		return
	}

	cctx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID: ModelNextHopFanOutWorkflowID(run.SavedQueryID, result.RunID),
		TaskQueue:  workflow.GetInfo(ctx).TaskQueueName,
		// The fan-out outlives this workflow: a refresh loop that finishes or continues
		// as new must not take its last run's fan-out down with it.
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_ABANDON,
		// The same policy the gateway uses for a completion's fan-out: a duplicate of
		// one that delivered is refused, a failed one may be tried again.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	})
	// Wait for the start, not the end. The start is what has to be in this history
	// before the loop moves on or continues as new; the delivery is the fan-out's own.
	if err := workflow.ExecuteChildWorkflow(cctx, UpstreamFanOutWorkflow, fanOut).
		GetChildWorkflowExecution().Get(ctx, nil); err != nil {
		if temporal.IsWorkflowExecutionAlreadyStartedError(err) {
			logger.Info("the models below this run were already woken",
				"saved_query_id", run.SavedQueryID, "run_id", result.RunID)
			return
		}
		logger.Error("could not start the fan-out below this run; the models below will not rebuild from it",
			"saved_query_id", run.SavedQueryID, "run_id", result.RunID, "error", err)
		return
	}
	logger.Info("⏩ woke the models below this run",
		"saved_query_id", run.SavedQueryID, "run_id", result.RunID, "models", len(fanOut.Targets))
}

// ListDownstreamModelsActivity asks the gateway for the models below a finished run.
//
// The gateway answers from the run's own history row: a succeeded run returns the
// fan-out to start, already in UpstreamFanOutInput's shape; a failed or auto-paused one
// records the skip rows below it and returns no targets. The gateway, not this module,
// owns those tables and the tenancy rules on who may wake whom.
func ListDownstreamModelsActivity(ctx context.Context, input ModelDownstreamInput) (*UpstreamFanOutInput, error) {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET"))
	if secret == "" {
		return nil, temporal.NewNonRetryableApplicationError(
			"INTERNAL_SERVICE_SECRET is not configured; the models below a run cannot be listed",
			"InternalSecretMissing", nil)
	}

	apiGatewayURL := strings.TrimSpace(os.Getenv("API_GATEWAY_URL"))
	if apiGatewayURL == "" {
		apiGatewayURL = "http://api-gateway:8080"
	}
	url := fmt.Sprintf("%s/api/v1/internal/explorer/models/%s/downstream", apiGatewayURL, input.SavedQueryID)

	body, err := json.Marshal(map[string]any{
		"run_id":       input.RunID,
		"execution_id": input.ExecutionID,
		"depth":        input.Depth,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode downstream request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("failed to build downstream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)

	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downstream request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
	case code == http.StatusBadRequest:
		// The gateway refused the ids themselves; no retry changes them.
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("downstream endpoint refused the call: %s", truncateForLog(string(raw), 400)),
			"ModelDownstreamRefused", nil)
	default:
		// A 404 here is a gateway that predates the route, mid-rollout: retryable.
		return nil, fmt.Errorf("downstream endpoint returned http %d: %s", code, truncateForLog(string(raw), 400))
	}

	var out UpstreamFanOutInput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to decode downstream response: %w", err)
	}
	return &out, nil
}
