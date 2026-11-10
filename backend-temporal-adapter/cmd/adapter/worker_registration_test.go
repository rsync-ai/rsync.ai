package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/rsync-ai/backend-temporal-adapter/internal/workflows"
)

// A workflow schedules an activity by name, and an activity the worker never
// registered fails only when that happens — never at startup, never at compile time.
// So this runs a model run whose rebuild never reports a result through exactly the
// registrations the worker gets, against a stand-in gateway, and checks the failure
// record arrives. Nothing here reaches a real gateway.

// The test environment is used as the registry below; this keeps it one.
var _ worker.Registry = (*testsuite.TestWorkflowEnvironment)(nil)

type registrationGateway struct {
	mu          sync.Mutex
	runCalls    int
	recordCalls int
	recordPath  string
	recordBody  map[string]any
}

func TestWorkerRegistrations_AModelRunThatNeverReportedIsRecorded(t *testing.T) {
	g := &registrationGateway{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		defer g.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/run"):
			g.runCalls++
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream unavailable"))
		case strings.HasSuffix(r.URL.Path, "/run-failed"):
			g.recordCalls++
			g.recordPath = r.URL.Path
			_ = json.Unmarshal(raw, &g.recordBody)
			_, _ = w.Write([]byte(`{"recorded":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "worker-registration-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerWorkflowsAndActivities(env)

	env.ExecuteWorkflow(workflows.ScheduledModelRunWorkflow, workflows.ScheduledModelRunInput{
		ScheduleID: "sched-reg-1", SavedQueryID: "model-reg-1",
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if werr := env.GetWorkflowError(); werr == nil || !strings.Contains(werr.Error(), "model run failed") {
		t.Fatalf("workflow error = %v, want the run's own failure", werr)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Denominator: the run activity was registered, reached the stand-in and spent its budget.
	if g.runCalls != 3 {
		t.Fatalf("run endpoint called %d times, want 3", g.runCalls)
	}
	if g.recordCalls != 1 {
		t.Fatalf("failed-run record endpoint called %d times, want 1 — is RecordModelRunFailureActivity registered on the worker?", g.recordCalls)
	}
	if g.recordPath != "/api/v1/internal/explorer/models/model-reg-1/run-failed" || g.recordBody["schedule_id"] != "sched-reg-1" {
		t.Errorf("record sent to %s with body %v, want model-reg-1 / sched-reg-1", g.recordPath, g.recordBody)
	}
}

// The same check for the next hop: a model run that leaves the hop to its workflow
// must reach the downstream endpoint through ListDownstreamModelsActivity and start
// the fan-out child, through exactly the worker's registrations.
func TestWorkerRegistrations_AFinishedModelRunWakesTheModelsBelowIt(t *testing.T) {
	var mu sync.Mutex
	var runBody, downstreamBody map[string]any
	downstreamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/internal/explorer/models/model-reg-1/run":
			_ = json.Unmarshal(raw, &runBody)
			_, _ = w.Write([]byte(`{"status":"succeeded","target_table":"public.t","run_id":"run-reg-1","next_hop":"caller"}`))
		case "/api/v1/internal/explorer/models/model-reg-1/downstream":
			downstreamCalls++
			_ = json.Unmarshal(raw, &downstreamBody)
			_, _ = w.Write([]byte(`{"upstream_kind":"model","upstream_id":"model-reg-1","upstream_run_id":"run-reg-1","depth":1,` +
				`"targets":[{"schedule_id":"sched-reg-2","saved_query_id":"model-reg-2"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "worker-registration-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerWorkflowsAndActivities(env)
	var children []string
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, _ converter.EncodedValues) {
		children = append(children, info.WorkflowType.Name+" "+info.WorkflowExecution.ID)
	})

	env.ExecuteWorkflow(workflows.ScheduledModelRunWorkflow, workflows.ScheduledModelRunInput{
		ScheduleID: "sched-reg-1", SavedQueryID: "model-reg-1",
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("workflow error = %v", werr)
	}
	mu.Lock()
	defer mu.Unlock()
	if runBody["next_hop"] != "caller" {
		t.Fatalf("run sent %v, want next_hop caller so the gateway leaves the hop to the workflow", runBody)
	}
	if downstreamCalls != 1 || downstreamBody["run_id"] != "run-reg-1" {
		t.Fatalf("downstream endpoint called %d times with %v, want once for run-reg-1 — is ListDownstreamModelsActivity registered on the worker?",
			downstreamCalls, downstreamBody)
	}
	want := "UpstreamFanOutWorkflow upstream-fanout:model:model-reg-1:run-reg-1"
	if len(children) == 0 || children[0] != want {
		t.Fatalf("children started = %v, want first %q", children, want)
	}
}

// The CDC trailing-edge singleton reaches the gateway's sweep endpoint, authenticated,
// through exactly the worker's registrations.
func TestWorkerRegistrations_TheCDCTrailingEdgeSweepReachesTheGateway(t *testing.T) {
	var mu sync.Mutex
	var paths, secrets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		secrets = append(secrets, r.Header.Get("X-Internal-Secret"))
		_, _ = w.Write([]byte(`{"scanned":2,"fired":1,"pending":1,"failed":0}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "worker-registration-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerWorkflowsAndActivities(env)
	env.RegisterDelayedCallback(env.CancelWorkflow, workflows.CDCTrailingEdgeSweepInterval+time.Second)

	env.ExecuteWorkflow(workflows.CDCTrailingEdgeWorkflow, workflows.CDCTrailingEdgeInput{})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	mu.Lock()
	defer mu.Unlock()
	want := "POST /api/v1/internal/cdc/trailing-edge/sweep"
	if len(paths) != 1 || paths[0] != want || secrets[0] != "worker-registration-unit-test-secret" {
		t.Fatalf("gateway saw %v with secrets %v, want one %q with the internal secret — is SweepCDCTrailingEdgeActivity registered on the worker?",
			paths, secrets, want)
	}
}

// A scheduled workflow run takes every step — begin, evaluate, one send per channel,
// finish — through exactly the workflow-runs worker's registrations, authenticated, with
// its own Temporal id as the key the gateway inserts the run on.
func TestWorkerRegistrations_AScheduledWorkflowRunTakesEveryStep(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		key := r.URL.Path
		if ch, isSend := body["channel_id"].(string); isSend {
			key += " " + ch
		}
		calls = append(calls, key+" secret="+r.Header.Get("X-Internal-Secret"))
		bodies[key] = body
		switch r.URL.Path {
		case "/api/v1/internal/workflows/wf-reg-1/runs/begin":
			_, _ = w.Write([]byte(`{"action":"run","run_id":"run-reg-1"}`))
		case "/api/v1/internal/workflow-runs/run-reg-1/evaluate":
			_, _ = w.Write([]byte(`{"outcome":"send","channels":["ch-a","ch-b"]}`))
		case "/api/v1/internal/workflow-runs/run-reg-1/send":
			_, _ = w.Write([]byte(`{"status":"sent"}`))
		case "/api/v1/internal/workflow-runs/run-reg-1/finish":
			_, _ = w.Write([]byte(`{"status":"succeeded"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "worker-registration-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerWorkflowRunWorker(env)
	const temporalID = "workflow-run:wf-reg-1:schedule-2026-10-04T06:00:00Z"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: temporalID, TaskQueue: workflows.WorkflowRunsTaskQueue})
	// The path a run that passed begin before build step 5 replays; the next test has
	// the steps graph.
	env.OnGetVersion("workflow-steps", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)

	env.ExecuteWorkflow(workflows.WorkflowRunWorkflow, workflows.WorkflowRunInput{WorkflowID: "wf-reg-1", Trigger: "schedule"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("workflow error = %v", werr)
	}
	mu.Lock()
	defer mu.Unlock()
	const secret = " secret=worker-registration-unit-test-secret"
	want := []string{
		"/api/v1/internal/workflows/wf-reg-1/runs/begin" + secret,
		"/api/v1/internal/workflow-runs/run-reg-1/evaluate" + secret,
		"/api/v1/internal/workflow-runs/run-reg-1/send ch-a" + secret,
		"/api/v1/internal/workflow-runs/run-reg-1/send ch-b" + secret,
		"/api/v1/internal/workflow-runs/run-reg-1/finish" + secret,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gateway saw\n%s\nwant\n%s\n— are the workflow-run activities registered on the worker?",
			strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	begin := bodies["/api/v1/internal/workflows/wf-reg-1/runs/begin"]
	if begin["trigger"] != "schedule" || begin["temporal_workflow_id"] != temporalID || begin["run_id"] != "" {
		t.Errorf("begin body = %v, want trigger schedule, temporal_workflow_id %q and no run_id", begin, temporalID)
	}
	if finish := bodies["/api/v1/internal/workflow-runs/run-reg-1/finish"]; finish["abort"] != "" {
		t.Errorf("finish body = %v, want abort \"\"", finish)
	}
}

// A run of the steps graph takes every step — the graph, each step's calls, complete —
// through exactly the workflow-runs worker's registrations, authenticated.
func TestWorkerRegistrations_AWorkflowRunOfStepsTakesEveryStep(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		var body struct {
			Attempt   int    `json:"attempt"`
			ChannelID string `json:"channel_id"`
			Status    string `json:"status"`
		}
		_ = json.Unmarshal(raw, &body)
		key := strings.TrimPrefix(r.URL.Path, "/api/v1/internal/workflow-runs/run-reg-2")
		switch {
		case body.Attempt > 0:
			key += " " + strconv.Itoa(body.Attempt)
		case body.ChannelID != "":
			key += " " + body.ChannelID
		case body.Status != "":
			key += " " + body.Status
		}
		calls = append(calls, key+" secret="+r.Header.Get("X-Internal-Secret"))
		switch key {
		case "/api/v1/internal/workflows/wf-reg-2/runs/begin":
			_, _ = w.Write([]byte(`{"action":"run","run_id":"run-reg-2"}`))
		case "/graph":
			_, _ = w.Write([]byte(`{"action":"run","steps":[{"id":"q","type":"query"},` +
				`{"id":"load","type":"run_pipeline","after":["q"],"timeout_minutes":720},` +
				`{"id":"pause","type":"wait","after":["load"],"minutes":1},` +
				`{"id":"tell","type":"send","after":["pause"]}]}`))
		case "/steps/load/start 1":
			_, _ = w.Write([]byte(`{"status":"running","execution_id":"exec-1"}`))
		case "/steps/pause/record running":
			_, _ = w.Write([]byte(`{"status":"waiting"}`))
		case "/steps/tell/plan":
			_, _ = w.Write([]byte(`{"status":"running","channels":["ch-a"]}`))
		case "/steps/tell/send ch-a":
			_, _ = w.Write([]byte(`{"status":"sent"}`))
		case "/steps/q/run", "/steps/load/poll 1", "/steps/pause/record succeeded", "/steps/tell/finish", "/complete":
			_, _ = w.Write([]byte(`{"status":"succeeded"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("API_GATEWAY_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "worker-registration-unit-test-secret")

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerWorkflowRunWorker(env)
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{
		ID: "workflow-run:wf-reg-2:schedule-2026-10-04T06:00:00Z", TaskQueue: workflows.WorkflowRunsTaskQueue,
	})

	env.ExecuteWorkflow(workflows.WorkflowRunWorkflow, workflows.WorkflowRunInput{WorkflowID: "wf-reg-2", Trigger: "schedule"})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow never completed")
	}
	if werr := env.GetWorkflowError(); werr != nil {
		t.Fatalf("workflow error = %v", werr)
	}
	mu.Lock()
	defer mu.Unlock()
	const secret = " secret=worker-registration-unit-test-secret"
	want := []string{
		"/api/v1/internal/workflows/wf-reg-2/runs/begin" + secret,
		"/graph" + secret,
		"/steps/q/run" + secret,
		"/steps/load/start 1" + secret,
		"/steps/load/poll 1" + secret,
		"/steps/pause/record running" + secret,
		"/steps/pause/record succeeded" + secret,
		"/steps/tell/plan" + secret,
		"/steps/tell/send ch-a" + secret,
		"/steps/tell/finish" + secret,
		"/complete" + secret,
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gateway saw\n%s\nwant\n%s\n— are the steps-graph activities registered on the worker?",
			strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// The registrations only count if main applies them to the worker it starts, on that
// worker's queue, and before it starts it (the SDK refuses registration on a started
// worker). Ordering is the point, so this reads main.go's syntax tree; the controls
// prove the reader rejects each wrong wiring.
func TestMainRegistersOnTheWorkerBeforeStartingIt(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, wiring := range []struct{ register, queue string }{
		{"registerWorkflowsAndActivities", "adapterTaskQueue"},
		{"registerWorkflowRunWorker", "workflows.WorkflowRunsTaskQueue"},
	} {
		t.Run(wiring.register, func(t *testing.T) {
			ok := `package main
func main() {
	w := worker.New(c, ` + wiring.queue + `, worker.Options{})
	` + wiring.register + `(w)
	if err := w.Start(); err != nil {
		log.Fatal(err)
	}
}`
			call := wiring.register + "(w)"
			controls := map[string]string{
				"not called":         strings.Replace(ok, call, "", 1),
				"another registry":   strings.Replace(ok, call, wiring.register+"(other)", 1),
				"after start":        strings.Replace(strings.Replace(ok, call+"\n", "", 1), "\tlog.Fatal(err)\n\t}\n", "\tlog.Fatal(err)\n\t}\n\t"+call+"\n", 1),
				"only on one branch": strings.Replace(ok, call, "if debug { "+call+" }", 1),
				"in a goroutine":     strings.Replace(ok, call, "go "+call, 1),
				"worker not started": strings.Replace(ok, "w.Start()", "w.Run(nil)", 1),
				"another queue":      strings.Replace(ok, wiring.queue, "otherQueue", 1),
			}
			if problem := workerRegistrationWiringProblem(ok, wiring.register, wiring.queue); problem != "" {
				t.Fatalf("reader rejects correct wiring: %s", problem)
			}
			for name, control := range controls {
				if control == ok {
					t.Fatalf("control %q did not change the source", name)
				}
				if workerRegistrationWiringProblem(control, wiring.register, wiring.queue) == "" {
					t.Fatalf("reader accepts wrong wiring %q", name)
				}
			}

			if problem := workerRegistrationWiringProblem(string(src), wiring.register, wiring.queue); problem != "" {
				t.Fatal(problem)
			}
		})
	}
}

// workerRegistrationWiringProblem says why func main in src does not call register(v),
// as a top-level statement, on a v assigned from worker.New on queue, before the
// top-level statement that calls v.Start(). "" means right.
func workerRegistrationWiringProblem(src, register, queue string) string {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", src, 0)
	if err != nil {
		return "parse main.go: " + err.Error()
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, isFunc := decl.(*ast.FuncDecl); isFunc && fn.Recv == nil && fn.Name.Name == "main" {
			body = fn.Body
		}
	}
	if body == nil {
		return "main.go has no func main"
	}

	queues := map[string]string{} // worker variable → the queue it was built on
	starts := map[string]int{}    // worker variable → first top-level statement calling its Start
	workerVar, registerAt := "", -1
	for i, stmt := range body.List {
		if assign, isAssign := stmt.(*ast.AssignStmt); isAssign && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
			if call, isCall := assign.Rhs[0].(*ast.CallExpr); isCall && isSelectorCall(call, "worker", "New") && len(call.Args) >= 2 {
				if id, isIdent := assign.Lhs[0].(*ast.Ident); isIdent {
					queues[id.Name] = types.ExprString(call.Args[1])
				}
			}
			continue
		}
		if expr, isExpr := stmt.(*ast.ExprStmt); isExpr && registerAt < 0 {
			if call, isCall := expr.X.(*ast.CallExpr); isCall && len(call.Args) == 1 {
				fn, fnIsIdent := call.Fun.(*ast.Ident)
				arg, argIsIdent := call.Args[0].(*ast.Ident)
				if fnIsIdent && fn.Name == register && argIsIdent {
					if _, isWorker := queues[arg.Name]; isWorker {
						workerVar, registerAt = arg.Name, i
						continue
					}
				}
			}
		}
		for v := range queues {
			if _, seen := starts[v]; seen {
				continue
			}
			ast.Inspect(stmt, func(n ast.Node) bool {
				if e, isExpr := n.(ast.Expr); isExpr && isSelectorCall(e, v, "Start") {
					starts[v] = i
					return false
				}
				return true
			})
		}
	}
	startAt, started := starts[workerVar]
	switch {
	case len(queues) == 0:
		return "main.go: func main never assigns a worker from worker.New"
	case registerAt < 0:
		return "main.go: func main does not call " + register + " on a worker from worker.New as a top-level statement"
	case queues[workerVar] != queue:
		return "main.go: " + register + " registers on " + workerVar + ", built on " + queues[workerVar] + ", not " + queue
	case !started:
		return "main.go: func main never calls " + workerVar + ".Start()"
	case registerAt > startAt:
		return "main.go: " + register + " runs after " + workerVar + ".Start()"
	}
	return ""
}

func isSelectorCall(e ast.Expr, recv, name string) bool {
	call, isCall := e.(*ast.CallExpr)
	if !isCall {
		return false
	}
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != name {
		return false
	}
	x, isIdent := sel.X.(*ast.Ident)
	return isIdent && x.Name == recv
}
