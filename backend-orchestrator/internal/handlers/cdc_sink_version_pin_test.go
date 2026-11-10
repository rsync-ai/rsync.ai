package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestRestartCDCSinkWorkerRunsThePinnedDestinationVersion: a sink restart must
// start the worker on the destination connection's pinned connector version,
// pinning a connection still at "latest" as a run would. Resolving "latest"
// straight from latest.json here would move a running CDC pipeline onto any
// connector version published since it started.
func TestRestartCDCSinkWorkerRunsThePinnedDestinationVersion(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("cdc_sink.go")
	if err != nil {
		t.Fatalf("read cdc_sink.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "cdc_sink.go", src, 0)
	if err != nil {
		t.Fatalf("parse cdc_sink.go: %v", err)
	}
	var body string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "restartCDCSinkWorker" {
			body = string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
	}
	if body == "" {
		t.Fatal("restartCDCSinkWorker not found in cdc_sink.go; if it moved or was renamed, update this test")
	}
	if !strings.Contains(body, "connections.ResolvePinned(ctx, db, mcpManager, destConnID, destConnector, destVersion)") {
		t.Errorf("restartCDCSinkWorker must resolve the destination version with connections.ResolvePinned")
	}
	if strings.Contains(body, "mcpManager.ResolveConcreteVersion(") {
		t.Errorf("restartCDCSinkWorker must not resolve the destination version from latest.json directly; a pinned connection would drift")
	}
}
