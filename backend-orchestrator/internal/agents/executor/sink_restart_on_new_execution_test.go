package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// A batch run's sink worker belongs to that run. The batch consumer group is a function
// of the pipeline ("sink-<id8>-batch", sink_consumer_group.go), and kafka-mcp-sink
// dedups start_sink by consumer group, so without this flag the next run found the
// previous run's worker still up and reused it: "already_running", with the previous
// run's execution id and destination config. With it, a start_sink for a new execution
// replaces the worker and a repeat for the same execution keeps it (connector.py
// start_sink). The CDC streaming sink must not set it: its worker outlives executions.
//
// An AST test for the reason sink_registration_order_test.go gives: driving
// startKafkaMCPSink needs a stubbed MCP transport the Agent does not have.
func TestStartSinkAsksForAFreshWorkerPerBatchRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "executor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "startKafkaMCPSink" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatal("startKafkaMCPSink not found in executor.go — move this guard with it rather than deleting it")
	}

	// The start_sink config literal is the map that carries "consumer_group".
	var got ast.Expr
	var found bool
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		keys := map[string]ast.Expr{}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if k, err := strconv.Unquote(bl.Value); err == nil {
					keys[k] = kv.Value
				}
			}
		}
		if _, isSinkCfg := keys["consumer_group"]; !isSinkCfg {
			return true
		}
		if _, hasExec := keys["execution_id"]; !hasExec {
			return true
		}
		found = true
		got = keys["restart_on_new_execution"]
		return false
	})
	if !found {
		t.Fatal("the start_sink config literal (consumer_group + execution_id) was not found in startKafkaMCPSink")
	}
	id, ok := got.(*ast.Ident)
	if !ok || id.Name != "isBatchBackfillTopic" {
		t.Fatalf(`start_sink must send "restart_on_new_execution": isBatchBackfillTopic, so each batch run gets its own worker; got %#v`, got)
	}
}
