package workers

// A /plan call names the workspace of the pipeline it plans.
//
// The planner service logs every model call in an llm_egress line and names its
// workspace from the request's X-Workspace-ID (llm-service src/egress/scope.py).
// Both /plan calls -- the intent worker's and the planner worker's -- dropped
// it, so the planner's lines read workspace=-.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"go.opentelemetry.io/otel"
)

const planWS = "22222222-2222-2222-2222-222222222222"

// planUpstream is a planner that records the X-Workspace-ID of each call.
func planUpstream(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Workspace-ID"))
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func TestPlanCall_TheIntentWorkerNamesTheWorkspace(t *testing.T) {
	srv, seen := planUpstream(t)
	w := &IntentWorker{llmServiceURL: srv.URL, httpClient: srv.Client(), tracer: otel.Tracer("test")}
	// No "X to Y" in the request, so the fast path is skipped and /plan is called.
	_, _ = w.parseIntent(context.Background(), "show me what changed last week", "t1", planWS)
	if got := seen(); len(got) != 1 || got[0] != planWS {
		t.Fatalf("planner saw X-Workspace-ID %q, want [%s]", got, planWS)
	}
}

func TestPlanCall_ThePlannerWorkerNamesTheTaskWorkspace(t *testing.T) {
	srv, seen := planUpstream(t)
	w := &PlannerWorker{plannerURL: srv.URL, httpClient: srv.Client(), tracer: otel.Tracer("test")}
	task := Task{TaskID: "t1", TraceID: "trace-1", Payload: map[string]interface{}{
		"user_request": "move orders to bigquery",
		"workspace_id": planWS,
	}}
	_, _ = w.generatePlan(context.Background(), task, nil)
	if got := seen(); len(got) != 1 || got[0] != planWS {
		t.Fatalf("planner saw X-Workspace-ID %q, want [%s]", got, planWS)
	}
}

// The adapter's intent and planner tasks carry no workspace_id, so the one
// they run for comes off the pipeline row.
func TestTaskWorkspaceID_FallsBackToThePipelineRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT workspace_id FROM pipelines WHERE id = \$1`).WithArgs("p1").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).AddRow(planWS))

	ctx := context.Background()
	if got := taskWorkspaceID(ctx, db, Task{PipelineID: "p1"}); got != planWS {
		t.Errorf("pipeline row: got %q, want %q", got, planWS)
	}
	named := Task{PipelineID: "p1", Context: map[string]interface{}{"workspace_id": "ws-named"}}
	if got := taskWorkspaceID(ctx, db, named); got != "ws-named" {
		t.Errorf("a task that names its workspace: got %q", got)
	}
	if got := taskWorkspaceID(ctx, nil, Task{PipelineID: "p1"}); got != "" {
		t.Errorf("no database: got %q, want none", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
