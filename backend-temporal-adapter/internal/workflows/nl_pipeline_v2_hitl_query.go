package workflows

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

// HITLWaitQuery asks an NL pipeline run which pause it is in, if any, and whether
// it is listening for the answer right now.
//
// It exists because pipeline_progress cannot answer that question. Two tabs can
// show the same pause: the chat and the pipeline page. When one answers it, the
// other keeps its dialog open, and its Apply signalled a run that had moved on —
// or finished. The row it would have checked lags the workflow (the projector
// writes PIPELINE_WAITING late, and StateUpdateActivity replaces metadata
// wholesale, wiping any claim the gateway left there), so the gateway asks the
// workflow itself before it signals.
//
// It is a wire contract with api-gateway pipeline_hitl_gate.go, which spells the
// name again because the two modules share no types. Same rules as the explorer
// queries (explorer_workflow_queries.go): read only, never block, no command — so
// a worker on this code registers it on replay of a run started before it.
const HITLWaitQuery = "hitl_wait"

// HITLWaitState is the answer to HITLWaitQuery.
//
// Type is empty when the run is not in a pause. Receiving is true only while the
// run is blocked in awaitHITLSignal: a connection-config pause stays recorded
// while the answer it received is being validated, and a second answer then would
// sit in the channel and be consumed by the NEXT pause of that kind in the run.
// ReceiveSeq counts the receives this run has entered, so the gateway can scope
// its duplicate-submit claim to one receive: a retry after a failed validation is
// a new receive with the same WaitingSince.
type HITLWaitState struct {
	Type         WaitReasonType `json:"type,omitempty"`
	WaitingSince time.Time      `json:"waiting_since,omitempty"`
	TimeoutAt    time.Time      `json:"timeout_at,omitempty"`
	NodeID       string         `json:"node_id,omitempty"`
	Receiving    bool           `json:"receiving"`
	ReceiveSeq   int            `json:"receive_seq"`
}

// hitlWaitTracker records whether awaitHITLSignal is blocked right now. It is
// workflow memory only: nothing reads it but the query handler, so it adds no
// command and replays clean.
type hitlWaitTracker struct {
	receiving  bool
	receiveSeq int
}

type hitlWaitTrackerKey struct{}

// withHITLWaitTracker carries a tracker on ctx so awaitHITLSignal can report its
// receives without a new parameter at every park site.
func withHITLWaitTracker(ctx workflow.Context) (workflow.Context, *hitlWaitTracker) {
	tracker := &hitlWaitTracker{}
	return workflow.WithValue(ctx, hitlWaitTrackerKey{}, tracker), tracker
}

func hitlWaitTrackerFrom(ctx workflow.Context) *hitlWaitTracker {
	tracker, _ := ctx.Value(hitlWaitTrackerKey{}).(*hitlWaitTracker)
	return tracker
}

// hitlWaitStateOf builds the query answer from the run's own state, never a copy
// of it.
func hitlWaitStateOf(state *WorkflowState, tracker *hitlWaitTracker) HITLWaitState {
	st := HITLWaitState{}
	if tracker != nil {
		st.Receiving = tracker.receiving
		st.ReceiveSeq = tracker.receiveSeq
	}
	if wr := state.WaitReason; wr != nil {
		st.Type = wr.Type
		st.WaitingSince = wr.WaitingSince
		st.TimeoutAt = wr.TimeoutAt
		st.NodeID = wr.NodeID
	}
	return st
}
