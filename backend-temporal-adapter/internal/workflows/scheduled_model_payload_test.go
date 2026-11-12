package workflows

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureModelRunPayload runs RunModelActivity against a fake gateway and returns the
// JSON body it sent.
func captureModelRunPayload(t *testing.T, in ScheduledModelRunInput) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("activity sent a body that is not JSON: %q", raw)
		}
		_, _ = w.Write([]byte(`{"status":"succeeded"}`))
	}))
	defer srv.Close()
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-secret")
	t.Setenv("API_GATEWAY_URL", srv.URL)

	if _, err := RunModelActivity(context.Background(), in); err != nil {
		t.Fatalf("activity failed: %v", err)
	}
	return got
}

// TestRunModelActivity_AnEventRunSendsItsProvenance: the gateway records who woke a
// rebuild only from this body, so every field has to reach it.
func TestRunModelActivity_AnEventRunSendsItsProvenance(t *testing.T) {
	got := captureModelRunPayload(t, ScheduledModelRunInput{
		SavedQueryID: "m1", ScheduleID: "s1", Trigger: ModelRefreshTrigger, Depth: 3,
		UpstreamKind: "model", UpstreamID: "u1", UpstreamRunID: "r1", ExecutionID: "e1", Coalesced: 2,
	})
	want := map[string]any{
		"schedule_id": "s1", "trigger": ModelRefreshTrigger, "depth": float64(3),
		"upstream_kind": "model", "upstream_id": "u1", "upstream_run_id": "r1",
		"execution_id": "e1", "coalesced": float64(2),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("payload[%q] = %v, want %v (payload %v)", k, got[k], v, got)
		}
	}
}

// TestRunModelActivity_AClockRunSendsOnlyItsSchedule keeps the clock payload
// byte-compatible with gateways that predate provenance.
func TestRunModelActivity_AClockRunSendsOnlyItsSchedule(t *testing.T) {
	got := captureModelRunPayload(t, ScheduledModelRunInput{SavedQueryID: "m1", ScheduleID: "s1"})
	if len(got) != 1 || got["schedule_id"] != "s1" {
		t.Errorf("clock payload = %v, want only schedule_id", got)
	}
}

// TestRunModelActivity_AskingForTheHopSendsItAndReadsTheRunBack: next_hop reaches the
// gateway on either door, and the run id and echo the gateway answers with reach the
// workflow — without both, the workflow has no run to list the models below.
func TestRunModelActivity_AskingForTheHopSendsItAndReadsTheRunBack(t *testing.T) {
	for _, in := range []ScheduledModelRunInput{
		{SavedQueryID: "m1", ScheduleID: "s1", NextHop: ModelNextHopCaller},
		{SavedQueryID: "m1", ScheduleID: "s1", Trigger: ModelRefreshTrigger, Depth: 1, NextHop: ModelNextHopCaller},
	} {
		got := captureModelRunPayload(t, in)
		if got["next_hop"] != ModelNextHopCaller {
			t.Errorf("trigger %q: payload %v, want next_hop %q", in.Trigger, got, ModelNextHopCaller)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"succeeded","target_table":"public.t","run_id":"run-9","next_hop":"caller"}`))
	}))
	defer srv.Close()
	t.Setenv("INTERNAL_SERVICE_SECRET", "unit-test-secret")
	t.Setenv("API_GATEWAY_URL", srv.URL)
	res, err := RunModelActivity(context.Background(), ScheduledModelRunInput{SavedQueryID: "m1", ScheduleID: "s1", NextHop: ModelNextHopCaller})
	if err != nil {
		t.Fatalf("activity failed: %v", err)
	}
	if res.RunID != "run-9" || res.NextHop != ModelNextHopCaller {
		t.Fatalf("result %+v, want run_id run-9 and next_hop %q", res, ModelNextHopCaller)
	}
}
