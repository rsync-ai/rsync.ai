package handlers

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// TestRestartCDCSinkWorkerRefusesARecordIDsDestination: a CDC sink restart starts
// its own worker (start_sink here, not startKafkaMCPSink), so it must refuse a
// destination that addresses each record by the id its API assigned, as a run
// start does (executor cdc_record_ids_check.go; KI-API-DEST-CDC-WRITES-BY-SOURCE-ID).
// The refusal comes before stop_sink, so a refused restart leaves the running
// worker alone, and answers 409 with the executor's message.
func TestRestartCDCSinkWorkerRefusesARecordIDsDestination(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cdc_sink.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cdc_sink.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "restartCDCSinkWorker" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("restartCDCSinkWorker not found in cdc_sink.go; if it moved or was renamed, update this test")
	}
	src := func(n ast.Node) string {
		var b strings.Builder
		_ = printer.Fprint(&b, fset, n)
		return b.String()
	}

	// Each mcp.ExecuteRequest literal by its Operation, and the refusal call.
	ops := map[string]*ast.CompositeLit{}
	var refusal *ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if src(x.Type) != "mcp.ExecuteRequest" {
				return true
			}
			for _, e := range x.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok && src(kv.Key) == "Operation" {
					ops[strings.Trim(src(kv.Value), `"`)] = x
				}
			}
		case *ast.CallExpr:
			if src(x.Fun) == "executor.CDCRecordIDsRefusal" {
				refusal = x
			}
		}
		return true
	})
	stop, caps := ops["stop_sink"], ops["get_capabilities"]
	if stop == nil || ops["start_sink"] == nil {
		t.Fatalf("no stop_sink/start_sink request in restartCDCSinkWorker (found %v) — the test is stale, not passing", ops)
	}
	if refusal == nil {
		t.Fatal("restartCDCSinkWorker never calls executor.CDCRecordIDsRefusal: a restart starts a CDC sink " +
			"into a destination that keys records by its API's ids")
	}
	if refusal.Pos() > stop.Pos() {
		t.Error("the record-ids refusal comes after stop_sink, so a refused restart leaves the pipeline with no worker")
	}
	if caps == nil || caps.Pos() > refusal.Pos() {
		t.Fatal("the refusal must follow the destination's own get_capabilities")
	}
	fields := map[string]string{}
	for _, e := range caps.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			fields[src(kv.Key)] = src(kv.Value)
		}
	}
	if fields["Connector"] != "destConnector" || fields["Version"] != "destConcreteVer" {
		t.Errorf("get_capabilities asks %s@%s; want the destination at the version the sink will run (destConnector@destConcreteVer)",
			fields["Connector"], fields["Version"])
	}
	if len(refusal.Args) != 3 || !strings.HasPrefix(src(refusal.Args[0]), "capResp") || src(refusal.Args[1]) != "destConnector" {
		t.Errorf("the refusal reads %s; want the get_capabilities answer for destConnector", src(refusal))
	}
	// The refusal's own branch answers 409 with its message.
	var refused bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if ok && ifs.Init != nil && ifs.Init.Pos() <= refusal.Pos() && refusal.End() <= ifs.Init.End() {
			body := src(ifs.Body)
			refused = strings.Contains(body, "http.StatusConflict") && strings.Contains(body, "errors.New(msg)")
		}
		return true
	})
	if !refused {
		t.Error("a refused restart must answer 409 with the refusal's message")
	}
}
