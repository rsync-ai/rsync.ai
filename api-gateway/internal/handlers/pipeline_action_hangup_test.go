package handlers

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// A pipeline action the browser starts must finish when the browser goes away. Go
// cancels a request's context when its client disconnects, so an action built on
// c.Request.Context() stopped at its next call once the tab closed: a CDC Stop gave up
// on the orchestrator's answer and never wrote the pipeline row (the connector parked,
// the row still read running), and each CDC proxy hung up on the orchestrator, which
// cancelled the orchestrator's own handler between two Kafka Connect steps. The
// actions now run on context.WithoutCancel(c.Request.Context()): the request's values,
// none of its cancellation, still bounded by each client's timeout.

// orchestratorSeen is what the fake orchestrator saw of the gateway's call.
type orchestratorSeen struct {
	sync.Mutex
	called, cut bool
	reached     map[string]bool // the later calls (see `after`) the gateway made
	done        chan struct{}   // closed when the fake has answered or seen the hang-up
}

// reachedPath reports whether the gateway made the later call to path.
func (s *orchestratorSeen) reachedPath(path string) bool {
	s.Lock()
	defer s.Unlock()
	return s.reached[path]
}

// wait returns once the fake has finished with the gateway's call, or after a second.
func (s *orchestratorSeen) wait() (called, cut bool) {
	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
	s.Lock()
	defer s.Unlock()
	return s.called, s.cut
}

// orchestratorThatOutlivesTheCaller answers method+path with 200 after hanging up the
// caller and giving the gateway time to notice. It records whether the gateway hung
// up on it in turn: before the fix, the gateway's outbound request shared the caller's
// context, so the orchestrator's request was cancelled too. Each `after` path is a
// call the action makes once the caller is gone; it is answered with success and
// recorded, so a test can tell the action carried on.
func orchestratorThatOutlivesTheCaller(t *testing.T, method, path string, hangUp context.CancelFunc, after ...string) *orchestratorSeen {
	t.Helper()
	seen := &orchestratorSeen{done: make(chan struct{}), reached: map[string]bool{}}
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(after, r.URL.Path) {
			seen.Lock()
			seen.reached[r.URL.Path] = true
			seen.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		if r.Method != method || r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		defer once.Do(func() { close(seen.done) })
		// Reading the body lets the server notice the gateway closing the connection.
		_, _ = io.Copy(io.Discard, r.Body)
		seen.Lock()
		seen.called = true
		seen.Unlock()
		hangUp()
		select {
		case <-r.Context().Done():
			seen.Lock()
			seen.cut = true
			seen.Unlock()
			return
		case <-time.After(200 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"warnings":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ORCHESTRATOR_URL", srv.URL)
	return seen
}

func TestCDCProxyKeepsTheOrchestratorAfterTheCallerHangsUp(t *testing.T) {
	for _, tc := range []struct {
		name, route, upstreamMethod, upstreamPath string
		handler                                   gin.HandlerFunc
	}{
		{"restart", "/cdc/restart", http.MethodPost, "/restart", RestartPipelineCDC},
		{"recover", "/cdc/recover", http.MethodPost, "/recover", RecoverPipelineCDC},
		{"backfill", "/cdc/backfill", http.MethodPost, "/backfill", BackfillPipelineCDCTables},
		{"pause", "/cdc/pause", http.MethodPut, "/pause", PausePipelineCDC},
		{"resume", "/cdc/resume", http.MethodPut, "/resume", ResumePipelineCDC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			expectPipelineOwnerRole(mock)

			ctx, hangUp := context.WithCancel(context.Background())
			defer hangUp()
			seen := orchestratorThatOutlivesTheCaller(t, tc.upstreamMethod,
				"/api/v1/cdc/pipelines/"+cdcProxyPipelineID+tc.upstreamPath, hangUp)

			r := wsScopeRouter(http.MethodPost, "/api/v1/pipelines/:id"+tc.route, tc.handler)
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+cdcProxyPipelineID+tc.route, strings.NewReader(`{}`))
			r.ServeHTTP(rr, req.WithContext(ctx))

			called, cut := seen.wait()
			if !called {
				t.Fatalf("the orchestrator was never called (status %d: %s)", rr.Code, rr.Body.String())
			}
			if cut {
				t.Fatalf("the gateway hung up on the orchestrator when its caller did (status %d: %s)", rr.Code, rr.Body.String())
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("want the orchestrator's 200, got %d: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// A CDC Stop parks the connector through the orchestrator, then writes the pipeline
// row. With the tab closed mid-call, the gateway answered 502 "Orchestrator is
// unreachable" and never wrote the row, so a stopped stream read running.
func TestStopPipelineWritesTheRowAfterTheCallerHangsUp(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectPipelineOwnerRole(mock)
	mock.ExpectQuery(`SELECT p\.status, .* FROM pipelines p WHERE p\.id = \$1 AND p\.workspace_id = \$2`).
		WithArgs(cdcProxyPipelineID, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "is_cdc"}).AddRow("running", true))
	mock.ExpectExec(`UPDATE pipelines SET status = 'stopped'`).
		WithArgs(cdcProxyPipelineID, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM pipeline_progress`).WithArgs(cdcProxyPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"execution_id"}))
	mock.ExpectQuery(`FROM executions`).WithArgs(cdcProxyPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(`UPDATE executions`).WithArgs(cdcProxyPipelineID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	seen := orchestratorThatOutlivesTheCaller(t, http.MethodPut,
		"/api/v1/cdc/pipelines/"+cdcProxyPipelineID+"/stop", hangUp)

	r := wsScopeRouter(http.MethodPost, "/api/v1/pipelines/:id/stop", StopPipeline)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+cdcProxyPipelineID+"/stop", nil)
	r.ServeHTTP(rr, req.WithContext(ctx))

	if _, cut := seen.wait(); cut {
		t.Errorf("the gateway hung up on the orchestrator's Stop when its caller did")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the Stop did not finish its writes: %v", err)
	}
}

// A Delete drops the source's CDC resources through the orchestrator, deletes the row,
// then has the orchestrator reclaim the Kafka topics. With the tab closed during the
// source cleanup, every later step failed on the cancelled context: the slot and
// connector were gone while the row, its schedules and its topics stayed.
func TestDeletePipelineFinishesAfterTheCallerHangsUp(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectPipelineOwnerRole(mock)
	mock.ExpectQuery(`SELECT temporal_schedule_id FROM pipeline_schedules WHERE pipeline_id = \$1`).
		WithArgs(cdcProxyPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"temporal_schedule_id"}))
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM executions`).
		WithArgs(cdcProxyPipelineID, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE saved_query_schedules s`).
		WithArgs(cdcProxyPipelineID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DELETE FROM pipelines WHERE id=\$1 AND workspace_id=\$2`).
		WithArgs(cdcProxyPipelineID, wsScopeWS).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	const teardown = "/api/v1/cdc/kafka-teardown"
	seen := orchestratorThatOutlivesTheCaller(t, http.MethodPost, "/api/v1/cdc/cleanup", hangUp, teardown)

	r := wsScopeRouter(http.MethodDelete, "/api/v1/pipelines/:id", DeletePipeline)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/pipelines/"+cdcProxyPipelineID, nil)
	r.ServeHTTP(rr, req.WithContext(ctx))

	if _, cut := seen.wait(); cut {
		t.Errorf("the gateway hung up on the orchestrator's CDC cleanup when its caller did")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the Delete did not finish its writes: %v", err)
	}
	if !seen.reachedPath(teardown) {
		t.Errorf("the Delete never asked the orchestrator to reclaim the pipeline's Kafka topics")
	}
}

// requestContextAllowedCalls take the request's context for a purpose a hang-up may
// cancel: a log line's fields.
var requestContextAllowedCalls = map[string]bool{"context.WithoutCancel": true, "log.WithContext": true}

// pipelineActionFuncs are the browser's pipeline actions, the answers to a paused run,
// the schedule and model writes, the executor's namespace lock, and the same-package
// helpers they hand the gin context to for a side effect. A helper that only reads
// (the role gate, the cooldown check) is not listed: cancelling a read is harmless.
var pipelineActionFuncs = []string{
	"StopPipeline", "stopCDCStreaming", "stopBatchSinkWorkers", "DeletePipeline",
	"PausePipeline", "ResumePipeline", "CancelExecution",
	"RestartPipelineCDC", "RecoverPipelineCDC", "BackfillPipelineCDCTables",
	"PausePipelineCDC", "ResumePipelineCDC",
	"ResumePipelineConnections", "ResumePipelineConnectors", "ResumePipelineTables",
	"ResumePipelineNodeInput", "gateHITLResume", "respondHITLRunFinished", "respondHITLSignalFailed",
	"CreatePipelineSchedule", "PausePipelineSchedule", "ResumePipelineSchedule",
	"DeletePipelineSchedule", "UpdatePipelineSchedule",
	"DeleteSavedQuery", "LockPipelineNamespaceInternal",
}

// The behavioural tests above cover the CDC proxies and Stop; Pause, Resume and
// Cancel signal Temporal, which these tests have no fake for. This guard covers all of
// them by their source: none may pass c.Request.Context() to anything but
// context.WithoutCancel or a log call.
func TestPipelineActionsOutliveTheCaller(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	var bad []string
	for _, name := range pipelineActionFuncs {
		fd, ok := funcs[name]
		if !ok {
			t.Errorf("%s not found: renamed? update pipelineActionFuncs", name)
			continue
		}
		for _, pos := range cancellableRequestContexts(fd.Body) {
			bad = append(bad, name+" at "+fset.Position(pos).String())
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s uses c.Request.Context(), which a closed tab cancels: wrap it in context.WithoutCancel", b)
	}
}

// cancellableRequestContexts returns each <x>.Request.Context() in body that is not
// the argument of an allowed call.
func cancellableRequestContexts(body ast.Node) []token.Pos {
	allowed := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && requestContextAllowedCalls[pkg.Name+"."+sel.Sel.Name] {
					allowed[call.Args[0]] = true
				}
			}
		}
		return true
	})
	var uses []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || allowed[call] {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Request" {
				uses = append(uses, call.Pos())
			}
		}
		return true
	})
	return uses
}
