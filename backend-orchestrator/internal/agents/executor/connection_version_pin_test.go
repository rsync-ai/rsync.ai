package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNewAgentPinsConnectionVersions: the executor's connection manager is the
// one that hydrates a run's source and destination, so it must pin a
// connection still at "latest" to the version it first runs on. Without the
// resolver the manager reads "latest" and every run follows latest.json, so a
// connector version published later moves a pipeline that already runs.
func TestNewAgentPinsConnectionVersions(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "executor.go", src, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	var body string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "NewAgent" {
			body = string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
	}
	if body == "" {
		t.Fatal("NewAgent not found in executor.go; if it moved or was renamed, update this test")
	}
	if !strings.Contains(body, "connections.NewManager(db).PinVersionsWith(mcpManager)") {
		t.Errorf("NewAgent must build its connection manager with PinVersionsWith(mcpManager)")
	}
}
