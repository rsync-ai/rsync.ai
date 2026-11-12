package workflows

import (
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// The gateway asks hitl_wait before it signals a pause, so a second tab whose
// dialog outlived the pause cannot answer it again (api-gateway
// pipeline_hitl_gate.go). These tests pin what the answer must say.

func queryHITLWait(t *testing.T, env *testsuite.TestWorkflowEnvironment) HITLWaitState {
	t.Helper()
	res, err := env.QueryWorkflow(HITLWaitQuery)
	if err != nil {
		t.Fatalf("hitl_wait query failed: %v", err)
	}
	var st HITLWaitState
	if err := res.Get(&st); err != nil {
		t.Fatalf("decode hitl_wait answer: %v", err)
	}
	return st
}

// The real workflow, parked at table selection: the query names the pause and
// says the run is listening; once the answer is taken it names nothing.
func TestNLPipelineV2_HITLWaitQueryReportsTheOpenPause(t *testing.T) {
	h := &tableSelectionHarness{}
	env := newTableSelectionEnv(h)

	var during HITLWaitState
	env.RegisterDelayedCallback(func() {
		during = queryHITLWait(t, env)
		env.SignalWorkflow(SignalTablesSelected, TablesSelectedPayload{
			PipelineID:     "pipe-cancel-1",
			SelectedTables: []string{"orders"},
		})
	}, time.Hour)
	env.ExecuteWorkflow(NLPipelineWorkflowV2, tableSelectionInput())

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow returned an error: %v", err)
	}
	_, events, _ := h.snapshot()
	if !containsString(events, "PIPELINE_WAITING") {
		t.Fatalf("the harness never reached the table-selection park (state events %v) — the test proves nothing", events)
	}

	if during.Type != WaitReasonTableSelection {
		t.Fatalf("while parked, hitl_wait type = %q, want %q", during.Type, WaitReasonTableSelection)
	}
	if !during.Receiving || during.ReceiveSeq < 1 {
		t.Fatalf("while parked, hitl_wait must say the run is listening (receiving=%v seq=%d)", during.Receiving, during.ReceiveSeq)
	}
	if during.WaitingSince.IsZero() || !during.TimeoutAt.After(during.WaitingSince) {
		t.Fatalf("while parked, hitl_wait must carry the pause's window, got since=%s timeout=%s", during.WaitingSince, during.TimeoutAt)
	}

	after := queryHITLWait(t, env)
	if after.Type != "" || after.Receiving {
		t.Fatalf("after the answer was taken, hitl_wait must name no pause, got type=%q receiving=%v", after.Type, after.Receiving)
	}
}

// hitlRetryProbeWorkflow mirrors a connection-config park whose first answer fails
// validation: receive, work while still recorded as waiting, then receive again.
func hitlRetryProbeWorkflow(ctx workflow.Context) (string, error) {
	state := &WorkflowState{}
	ctx, tracker := withHITLWaitTracker(ctx)
	if err := workflow.SetQueryHandler(ctx, HITLWaitQuery, func() (HITLWaitState, error) {
		return hitlWaitStateOf(state, tracker), nil
	}); err != nil {
		return "", err
	}
	state.SetWaitReason(&WaitReason{
		Type:         WaitReasonConnectionConfiguration,
		WaitingSince: workflow.Now(ctx),
		TimeoutAt:    workflow.Now(ctx).Add(24 * time.Hour),
	})
	ch := workflow.GetSignalChannel(ctx, hitlProbeSignal)
	var payload hitlProbePayload
	if !awaitHITLSignal(ctx, ctx, ch, &payload, state.WaitReason.TimeoutAt) {
		return "timeout", nil
	}
	// "Validation": the pause is still recorded but nobody is listening.
	if err := workflow.Sleep(ctx, time.Hour); err != nil {
		return "", err
	}
	if !awaitHITLSignal(ctx, ctx, ch, &payload, state.WaitReason.TimeoutAt) {
		return "timeout", nil
	}
	state.ClearWaitReason()
	return "received:" + payload.Value, nil
}

func TestHITLWaitQueryTracksEachReceive(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.SetStartTime(hitlProbeStart)
	env.RegisterWorkflow(hitlRetryProbeWorkflow)

	var first, validating, retry HITLWaitState
	env.RegisterDelayedCallback(func() {
		first = queryHITLWait(t, env)
		env.SignalWorkflow(hitlProbeSignal, hitlProbePayload{Value: "a"})
	}, 10*time.Minute)
	env.RegisterDelayedCallback(func() {
		validating = queryHITLWait(t, env)
	}, 40*time.Minute)
	env.RegisterDelayedCallback(func() {
		retry = queryHITLWait(t, env)
		env.SignalWorkflow(hitlProbeSignal, hitlProbePayload{Value: "b"})
	}, 2*time.Hour)
	env.ExecuteWorkflow(hitlRetryProbeWorkflow)

	if !env.IsWorkflowCompleted() {
		t.Fatal("probe did not complete")
	}
	var got string
	if err := env.GetWorkflowResult(&got); err != nil || got != "received:b" {
		t.Fatalf("probe result = %q, %v; want received:b", got, err)
	}

	if !first.Receiving || first.ReceiveSeq != 1 || first.Type != WaitReasonConnectionConfiguration {
		t.Fatalf("first receive: %+v, want receiving seq=1 CONNECTION_CONFIGURATION", first)
	}
	// Recorded as waiting, but an answer sent now would be buffered and taken by
	// whichever receive comes next — the gateway must see that it is not listening.
	if validating.Receiving || validating.Type != WaitReasonConnectionConfiguration {
		t.Fatalf("during validation: %+v, want the pause recorded but receiving=false", validating)
	}
	// A retry is a new receive of the same pause, so a claim scoped to the first
	// receive must not block it.
	if !retry.Receiving || retry.ReceiveSeq != 2 || !retry.WaitingSince.Equal(first.WaitingSince) {
		t.Fatalf("retry receive: %+v, want receiving seq=2 with the first receive's waiting_since", retry)
	}
}
