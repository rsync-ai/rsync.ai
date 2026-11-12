package workers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// A failed /plan call used to put the llm-service body into the error, and the
// error into the STAGE_FAILED progress event the UI shows. A FastAPI 422 quotes
// the request it rejected; an older llm-service quotes the provider's error.
// The error now names the status only; the body goes to the log, scrubbed.
func TestParseIntent_ErrorDoesNotCarryTheUpstreamBody(t *testing.T) {
	const leak = `{"detail":"could not connect to db.internal.acme.io:5432 as alice@example.com with password hunter2-canary"}`
	canaries := []string{"db.internal.acme.io", "alice@example.com", "hunter2-canary"}
	for _, c := range canaries {
		if !strings.Contains(leak, c) {
			t.Fatalf("control: %q missing from the upstream body", c)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(leak))
	}))
	defer srv.Close()

	w := &IntentWorker{llmServiceURL: srv.URL, httpClient: srv.Client(), tracer: otel.Tracer("test")}
	// No "X to Y" in the request, so the fast path is skipped and /plan is called.
	_, err := w.parseIntent(context.Background(), "show me what changed last week", "t1", "")
	if err == nil {
		t.Fatal("a 500 from /plan must be an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error should still name the status: %v", err)
	}
	for _, c := range canaries {
		if strings.Contains(err.Error(), c) {
			t.Errorf("error carries the llm-service body (%q): %v", c, err)
		}
	}
}

// The planner worker makes the same /plan call and put the body into the
// TaskResult error, which the workflow records as the step's failure message.
func TestGeneratePlan_ErrorDoesNotCarryTheUpstreamBody(t *testing.T) {
	const leak = `{"detail":"could not connect to db.internal.acme.io:5432 as alice@example.com with password hunter2-canary"}`
	canaries := []string{"db.internal.acme.io", "alice@example.com", "hunter2-canary"}
	for _, c := range canaries {
		if !strings.Contains(leak, c) {
			t.Fatalf("control: %q missing from the upstream body", c)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(leak))
	}))
	defer srv.Close()

	w := &PlannerWorker{plannerURL: srv.URL, httpClient: srv.Client(), tracer: otel.Tracer("test")}
	task := Task{TaskID: "t1", TraceID: "trace-1", Payload: map[string]interface{}{"user_request": "move orders to bigquery"}}
	_, err := w.generatePlan(context.Background(), task, nil)
	if err == nil {
		t.Fatal("a 500 from /plan must be an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error should still name the status: %v", err)
	}
	for _, c := range canaries {
		if strings.Contains(err.Error(), c) {
			t.Errorf("error carries the llm-service body (%q): %v", c, err)
		}
	}
}
