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
// CDC trailing edge — the timer behind "run after pipeline" for CDC
// ============================================================================
//
// A batch pipeline fires its models and workflows on PIPELINE_COMPLETED. A CDC pipeline
// never completes, so it fires on a window's trailing edge instead: the first window
// boundary after rows landed (docs/internal/workflows-design.md § 6). A boundary passing
// is not an event anything emits, so, like the freshness sweep, it needs a timer that
// exists without one. One singleton sweep looks at every CDC pipeline on an interval;
// the gateway decides, from cdc_table_watermarks and cdc_trailing_edge_state, which of
// them crossed a boundary since rows landed, and fires those once per window
// (api-gateway handlers/cdc_trailing_edge.go).
//
// The interval is the resolution of the trailing edge, not the window: with the
// default 15-minute window and a 60-second sweep, a window's models start within a
// minute of its boundary.

// CDCTrailingEdgeSweepInterval is how often the sweep looks.
const CDCTrailingEdgeSweepInterval = 60 * time.Second

// cdcTrailingEdgeTicksPerRun bounds one run's history before continuing as new, as
// modelFreshnessTicksPerRun does.
const cdcTrailingEdgeTicksPerRun = 500

// CDCTrailingEdgeWorkflowID is the singleton's fixed id: started at every adapter boot
// with USE_EXISTING, so boot is idempotent and a second replica joins the existing sweep.
const CDCTrailingEdgeWorkflowID = "cdc-trailing-edge-sweep"

// CDCTrailingEdgeInput is the workflow argument.
type CDCTrailingEdgeInput struct {
	// IntervalSeconds overrides CDCTrailingEdgeSweepInterval. An argument, not an
	// environment read in workflow code, which would not replay deterministically. The
	// singleton starts with USE_EXISTING, so a changed interval reaches a running sweep
	// only once it is terminated.
	IntervalSeconds int `json:"interval_seconds,omitempty"`

	// Ticks already spent, carried across continue-as-new.
	Ticks int `json:"ticks,omitempty"`
}

// CDCTrailingEdgeSweepResult is what the gateway reports back about one sweep.
type CDCTrailingEdgeSweepResult struct {
	Scanned int `json:"scanned"`
	Fired   int `json:"fired"`
	Pending int `json:"pending"`
	Failed  int `json:"failed"`
}

// CDCTrailingEdgeWorkflow sleeps, sweeps, and repeats, forever.
func CDCTrailingEdgeWorkflow(ctx workflow.Context, input CDCTrailingEdgeInput) error {
	logger := workflow.GetLogger(ctx)

	interval := CDCTrailingEdgeSweepInterval
	if input.IntervalSeconds > 0 {
		interval = time.Duration(input.IntervalSeconds) * time.Second
	}

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		// A sweep fires each due pipeline synchronously, and one fire's model fan-out
		// retries for up to 30 seconds before it records trigger_lost — so a sweep with a
		// few due pipelines and Temporal briefly away can take minutes. A timed-out
		// attempt that did fire is harmless: its repeat carries the same occurrence,
		// which the fan-out and workflow_runs both drop.
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			// A skipped tick costs latency and nothing else: an unfired window is still
			// unfired at the next sweep, and fires then under the same occurrence.
			MaximumAttempts: 3,
		},
	})

	ticks := input.Ticks
	for {
		// Sleep first: at boot the gateway is usually still coming up beside the adapter.
		if err := workflow.Sleep(ctx, interval); err != nil {
			// Cancellation; returning it is what lets an operator stop the singleton.
			return err
		}

		var res CDCTrailingEdgeSweepResult
		if err := workflow.ExecuteActivity(actx, SweepCDCTrailingEdgeActivity).Get(ctx, &res); err != nil {
			// Logged, not returned: a failing gateway must not end the only thing that
			// fires a CDC pipeline's downstream.
			logger.Warn("⚠️  CDC trailing-edge sweep failed; will retry on the next tick", "error", err)
		} else if res.Fired > 0 || res.Failed > 0 {
			// Quiet otherwise: a line per tick would be 1,440 a day saying nothing.
			logger.Info("🌊 CDC trailing edge swept",
				"scanned", res.Scanned, "fired", res.Fired,
				"pending", res.Pending, "failed", res.Failed)
		}

		ticks++
		if ticks >= cdcTrailingEdgeTicksPerRun {
			return workflow.NewContinueAsNewError(ctx, CDCTrailingEdgeWorkflow, CDCTrailingEdgeInput{
				IntervalSeconds: input.IntervalSeconds,
				Ticks:           0,
			})
		}
	}
}

// SweepCDCTrailingEdgeActivity asks the gateway to fire every CDC pipeline whose window
// closed after rows landed. HTTP because the gateway owns the tables the decision reads
// and the fan-out it fires, as SweepModelFreshnessActivity does.
func SweepCDCTrailingEdgeActivity(ctx context.Context) (*CDCTrailingEdgeSweepResult, error) {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET"))
	if secret == "" {
		// Fail loudly: silence here looks exactly like CDC pipelines with nothing landing.
		return nil, fmt.Errorf("INTERNAL_SERVICE_SECRET is not configured; the CDC trailing-edge sweep cannot authenticate")
	}

	apiGatewayURL := strings.TrimSpace(os.Getenv("API_GATEWAY_URL"))
	if apiGatewayURL == "" {
		apiGatewayURL = "http://api-gateway:8080"
	}
	url := apiGatewayURL + "/api/v1/internal/cdc/trailing-edge/sweep"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return nil, fmt.Errorf("failed to build CDC trailing-edge sweep request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)

	// Below the activity's StartToClose, so a slow sweep ends as this activity's own
	// error rather than as a timeout Temporal reports without the cause.
	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("CDC trailing-edge sweep request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDC trailing-edge sweep endpoint returned http %d: %s", resp.StatusCode, truncateForLog(string(raw), 400))
	}

	var out CDCTrailingEdgeSweepResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to decode CDC trailing-edge sweep response: %w", err)
	}
	return &out, nil
}
