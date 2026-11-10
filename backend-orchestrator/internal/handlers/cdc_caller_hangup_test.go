package handlers

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// A CDC Stop, Restart, Backfill or Recovery is several side effects in a row: park
// the connector, then save the status; clear the offsets, then write the config,
// then resume. Each used to run on a budget built on the request's context, which
// Go cancels when the caller goes away — the gateway's request when the browser tab
// closes or the gateway stops waiting. The action then stopped between two side
// effects: a parked connector whose row still said running, a connector left
// stopped with its offsets cleared.
//
// These tests hang up the caller while Kafka Connect is answering one step and
// assert that the rest still happened.

// hangUpOn puts a proxy in front of the Kafka Connect fake already installed in
// KAFKA_CONNECT_URL. The returned context is the caller's: it is cancelled the moment
// Connect receives the call named by key ("METHOD path").
func hangUpOn(t *testing.T, key string) context.Context {
	t.Helper()
	inner, err := url.Parse(os.Getenv("KAFKA_CONNECT_URL"))
	if err != nil || inner.Host == "" {
		t.Fatalf("install a Kafka Connect fake before hangUpOn (KAFKA_CONNECT_URL=%q)", os.Getenv("KAFKA_CONNECT_URL"))
	}
	ctx, hangUp := context.WithCancel(context.Background())
	t.Cleanup(hangUp)
	forward := httputil.NewSingleHostReverseProxy(inner)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path == key {
			hangUp()
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("KAFKA_CONNECT_URL", srv.URL)
	return ctx
}

func TestStopFinishesAfterTheCallerHangsUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeConnect(t, map[string]int{"PUT /connectors/cdc-637a0df2/stop": http.StatusNoContent})
	ctx := hangUpOn(t, "PUT /connectors/cdc-637a0df2/stop")

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("FROM cdc_resources").WithArgs(stopTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"resource_name"}).AddRow("cdc-637a0df2"))
	mock.ExpectQuery("SELECT COUNT").WithArgs(stopTestPipelineID, "637a0df2").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("UPDATE pipelines SET status = 'stopped'").WithArgs(stopTestPipelineID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("auth_internal", true) })
	r.PUT("/cdc/pipelines/:pipeline_id/stop", stopCDCPipeline(db, &recordingSinks{}, nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/cdc/pipelines/"+stopTestPipelineID+"/stop", nil).WithContext(ctx))

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the connector was parked but the status was not saved: %v (HTTP %d %s)", err, w.Code, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s; want 200", w.Code, w.Body)
	}
}

func TestRecoverFinishesAfterTheCallerHangsUp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CDC_RECOVERY_ENABLED", "true")
	calls := fakeRecoverConnect(t, map[string]int{
		pathOf("PUT", "/stop"):       http.StatusNoContent,
		pathOf("DELETE", "/offsets"): http.StatusOK,
		pathOf("PUT", "/config"):     http.StatusOK,
		pathOf("PUT", "/resume"):     http.StatusAccepted,
	})
	ctx := hangUpOn(t, pathOf("DELETE", "/offsets"))

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectQuery("FROM cdc_resources").WithArgs(recoverTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"resource_name"}).AddRow(recoverTestConnector))
	mock.ExpectQuery("SELECT source_connection_id").WithArgs(recoverTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"source_connection_id"}).AddRow("0f0e0d0c-1111-4222-8333-944455556666"))

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("auth_internal", true) })
	r.POST("/cdc/pipelines/:pipeline_id/recover", RecoverCDCPipeline(db))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/cdc/pipelines/"+recoverTestPipelineID+"/recover",
		bytes.NewReader([]byte(recoverWithReset))).WithContext(ctx))

	want := []string{pathOf("PUT", "/stop"), pathOf("DELETE", "/offsets"), pathOf("PUT", "/config"), pathOf("PUT", "/resume")}
	if got := connectWrites(*calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("connect writes = %v; want %v — the recovery stopped half-way (HTTP %d %s)", got, want, w.Code, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s; want 200", w.Code, w.Body)
	}
}

func TestBackfillIsQueuedAfterTheCallerHangsUp(t *testing.T) {
	h, r := backfillHarness(t, cfgPostgresIncremental, &fakeSignalProducer{}, true)
	ctx := hangUpOn(t, "GET /connectors/cdc-637a0df2/config")
	h.mock.MatchExpectationsInOrder(false)
	h.mock.ExpectQuery("SELECT source_connection_id").WithArgs(capabilityTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"source_connection_id"}).AddRow("11111111-1111-1111-1111-111111111111"))
	h.mock.ExpectQuery("SELECT c.connector_type").WithArgs(capabilityTestPipelineID).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type"}).AddRow("bigquery"))
	h.mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO cdc_snapshot_requests")).
		WillReturnRows(sqlmock.NewRows(snapshotRequestCols).AddRow("req-3", capabilityTestPipelineID, "cdc-637a0df2", "incremental",
			`["public.users"]`, "resnapshot", "queued", 0, `[]`, "", false, time.Now(), time.Now(), nil, nil, nil, nil, nil))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/cdc/pipelines/"+capabilityTestPipelineID+"/backfill",
		bytes.NewBufferString(`{"tables":["public.users"],"mode":"incremental"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if err := h.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the backfill was never queued: %v (HTTP %d %s)", err, w.Code, w.Body)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s; want 200", w.Code, w.Body)
	}
}

// requestContextAllowed lists the CDC write routes that may still run on the
// caller's context, and why.
var requestContextAllowed = map[string]string{
	// Each slot or publication is dropped and recorded on its own; what a hang-up
	// leaves stays in the ledger for the CDC reconciler, and its one caller, the
	// gateway's forced connection delete, deletes nothing once its own request is gone.
	"ReleaseConnectionCDCResources": "drops are recorded one by one; the reconciler finishes them",
}

// Every CDC write route the orchestrator registers must detach its work from the
// caller: context.WithoutCancel(c.Request.Context()), never the request's context
// itself. The routes are read from main.go, so a new one is checked without being
// listed here.
func TestCDCWriteRoutesOutliveTheCaller(t *testing.T) {
	fset := token.NewFileSet()
	mainFile, err := parser.ParseFile(fset, filepath.Join("..", "..", "cmd", "orchestrator", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}

	checked := map[string]bool{}
	ast.Inspect(mainFile, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isIdent(sel.X, "cdcGrp") {
			return true
		}
		switch sel.Sel.Name {
		case "POST", "PUT", "PATCH", "DELETE":
		default:
			return true
		}
		lit, _ := call.Args[0].(*ast.BasicLit)
		if lit == nil {
			t.Errorf("%s: cdcGrp.%s with a non-literal path", fset.Position(call.Pos()), sel.Sel.Name)
			return true
		}
		route := sel.Sel.Name + " " + strings.Trim(lit.Value, `"`)
		checked[route] = true

		name, bodies := handlerBodies(call.Args[len(call.Args)-1], funcs)
		if _, ok := requestContextAllowed[name]; ok {
			return true
		}
		if len(bodies) == 0 {
			t.Errorf("%s: cannot find the handler of %s", fset.Position(call.Pos()), route)
		}
		for _, body := range bodies {
			for _, pos := range requestContextUses(body) {
				t.Errorf("%s: %s runs on the caller's context — a closed tab cuts it off between two side effects; "+
					"use context.WithoutCancel(c.Request.Context())", fset.Position(pos), route)
			}
		}
		return true
	})

	// The guard must have seen the routes it exists for.
	for _, route := range []string{
		"PUT /cdc/pipelines/:pipeline_id/stop",
		"POST /cdc/pipelines/:pipeline_id/restart",
		"POST /cdc/pipelines/:pipeline_id/backfill",
		"POST /cdc/pipelines/:pipeline_id/recover",
		"PUT /cdc/pipelines/:pipeline_id/pause",
		"PUT /cdc/pipelines/:pipeline_id/resume",
		"POST /cdc/pipelines/:pipeline_id/sink/restart",
		"PUT /pipelines/:id/sink/stop",
	} {
		if !checked[route] {
			t.Errorf("route %s not found in main.go; the guard checked %d routes", route, len(checked))
		}
	}
}

// handlerBodies returns a route's handler name and the bodies to check: an inline
// func literal, or a handlers.X(...) constructor followed through the constructors
// it returns (StopCDCPipeline → stopCDCPipeline).
func handlerBodies(arg ast.Expr, funcs map[string]*ast.FuncDecl) (string, []ast.Node) {
	switch h := arg.(type) {
	case *ast.FuncLit:
		return "", []ast.Node{h.Body}
	case *ast.CallExpr:
		sel, ok := h.Fun.(*ast.SelectorExpr)
		if !ok || !isIdent(sel.X, "handlers") {
			return "", nil
		}
		var bodies []ast.Node
		seen := map[string]bool{}
		var follow func(name string)
		follow = func(name string) {
			fd := funcs[name]
			if fd == nil || seen[name] || fd.Body == nil {
				return
			}
			seen[name] = true
			bodies = append(bodies, fd.Body)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && returnsHandlerFunc(funcs[id.Name]) {
						follow(id.Name)
					}
				}
				return true
			})
		}
		follow(sel.Sel.Name)
		return sel.Sel.Name, bodies
	}
	return "", nil
}

func returnsHandlerFunc(fd *ast.FuncDecl) bool {
	if fd == nil || fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
		return false
	}
	sel, ok := fd.Type.Results.List[0].Type.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, "gin") && sel.Sel.Name == "HandlerFunc"
}

// requestContextUses returns each <x>.Request.Context() in body that is not the
// argument of context.WithoutCancel.
func requestContextUses(body ast.Node) []token.Pos {
	detached := map[ast.Node]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "context") && sel.Sel.Name == "WithoutCancel" {
				detached[call.Args[0]] = true
			}
		}
		return true
	})
	var uses []token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || detached[call] {
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

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
