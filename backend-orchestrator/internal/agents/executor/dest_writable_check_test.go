package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A run aimed at a resource an API destination cannot write fails before the
// sink starts, naming the resources it can write (P5 live, 2026-10-06: 10 rows
// sent to "products" on a ClickUp connection, every one refused at write time).
// Bug class: a destination target nobody checks until every row is refused.

func TestDestWriteTargets(t *testing.T) {
	cases := []struct {
		name   string
		pinned string
		tables []string
		want   []string
	}{
		{"pinned wins, schema stripped", "public.list_task", []string{"public.products"}, []string{"list_task"}},
		{"no pin: each selected table, bare, deduped", "", []string{"public.products", "sales.products", "orders"}, []string{"products", "orders"}},
		{"a filler word the sink ignores falls back to the tables", "the", []string{"public.products"}, []string{"products"}},
		{"a wildcard names nothing", "*", []string{"public.*"}, nil},
		{"quoted names are unquoted", `"public"."list_task"`, nil, []string{"list_task"}},
		{"nothing to write", "", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := destWriteTargets(c.pinned, c.tables)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("destWriteTargets(%q, %v) = %v, want %v", c.pinned, c.tables, got, c.want)
			}
		})
	}
}

func TestDestResourceNotWritable_NamesTheResourcesItCanWrite(t *testing.T) {
	caps := map[string]interface{}{"writable_resources": []interface{}{"list_task", "comment"}}
	msg := destResourceNotWritable(caps, []string{"products"}, "t-clickup")
	for _, want := range []string{`"products"`, "not writable", "t-clickup", "comment, list_task", `as comment`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q lacks %q", msg, want)
		}
	}
}

func TestDestResourceNotWritable_AWritableTargetPasses(t *testing.T) {
	caps := map[string]interface{}{"writable_resources": []interface{}{"list_task"}}
	if msg := destResourceNotWritable(caps, destWriteTargets("public.list_task", nil), "t-clickup"); msg != "" {
		t.Fatalf("a writable target was refused: %s", msg)
	}
}

func TestDestResourceNotWritable_EveryRefusedTargetIsNamed(t *testing.T) {
	caps := map[string]interface{}{"writable_resources": []interface{}{"list_task"}}
	msg := destResourceNotWritable(caps, []string{"products", "list_task", "orders"}, "t-clickup")
	if !strings.Contains(msg, `"products", "orders" are not writable`) {
		t.Fatalf("message %q should name both refused targets and not the writable one", msg)
	}
}

func TestDestResourceNotWritable_AnEmptyListMeansNothingIsWritable(t *testing.T) {
	caps := map[string]interface{}{"writable_resources": []interface{}{}}
	msg := destResourceNotWritable(caps, []string{"items"}, "dest_readonly")
	if !strings.Contains(msg, "not writable") || !strings.Contains(msg, "no resource") {
		t.Fatalf("an API that writes nowhere must refuse the run, got %q", msg)
	}
}

// A destination that does not list writable resources — every database, any
// hand-made connector — is never blocked by this check.
func TestDestResourceNotWritable_NoListMeansNoCheck(t *testing.T) {
	for name, caps := range map[string]map[string]interface{}{
		"absent":   {"capabilities": map[string]interface{}{"supports_ddl": true}},
		"not list": {"writable_resources": "list_task"},
		"nil caps": nil,
	} {
		if msg := destResourceNotWritable(caps, []string{"products"}, "postgresql"); msg != "" {
			t.Fatalf("%s: a destination that lists nothing was refused: %s", name, msg)
		}
	}
}

// The executor's copy of the sink's filler-word list must equal the sink's, or
// the check names a target the sink never writes.
func TestSinkIgnoresTableOverride_MatchesTheSink(t *testing.T) {
	src := readSinkWorkerMain(t)
	fn := regexp.MustCompile(`(?s)func isSuspiciousTableOverride\(s string\) bool \{.*?case (.*?):`).FindStringSubmatch(src)
	if fn == nil {
		t.Fatal("isSuspiciousTableOverride's case list not found in the sink's main.go")
	}
	var sinkWords []string
	for _, w := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(fn[1], -1) {
		sinkWords = append(sinkWords, w[1])
		if !sinkIgnoresTableOverride(w[1]) {
			t.Errorf("the sink ignores %q as a table override; sinkIgnoresTableOverride does not", w[1])
		}
	}
	if len(sinkWords) < 5 {
		t.Fatalf("parsed only %v from the sink — the parse is wrong, not the lists", sinkWords)
	}
	for _, w := range []string{"products", "list_task", "orders"} {
		if sinkIgnoresTableOverride(w) {
			t.Errorf("%q is a real table name, not a filler word", w)
		}
	}
}

func readSinkWorkerMain(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := cwd; ; {
		p := filepath.Join(d, "shared", "mcp-connectors", "internal", "kafka-mcp-sink",
			"worker-src", "cmd", "kafka-sink-worker", "main.go")
		if b, readErr := os.ReadFile(p); readErr == nil {
			return string(b)
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("no kafka-sink-worker main.go above %s — this guard cannot silently pass", cwd)
		}
		d = parent
	}
}

// startKafkaMCPSink runs the check after the run's destination table is pinned
// and before the sink starts, and returns its failure: a batch run starts its
// sink before the export, so the source is never read for a run that cannot land.
func TestStartKafkaMCPSink_ChecksTheDestinationIsWritableBeforeStartingTheSink(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "executor.go", nil, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "startKafkaMCPSink" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("startKafkaMCPSink not found in executor.go")
	}
	pinEnd, checkAt, startSinkAt := token.NoPos, token.NoPos, token.NoPos
	returnsIt := false
	for _, st := range fn.Body.List {
		if ifs, ok := st.(*ast.IfStmt); ok && strings.Contains(nodeSrc(fset, ifs.Cond), "tablesCount != 1") {
			pinEnd = ifs.End()
		}
		if ifs, ok := st.(*ast.IfStmt); ok && ifs.Init != nil && strings.Contains(nodeSrc(fset, ifs.Init), "a.destWritableCheck(") {
			if checkAt == token.NoPos {
				checkAt = ifs.Pos()
			}
			init := nodeSrc(fset, ifs.Init)
			for _, arg := range []string{"task.Destination.Type", "destConcreteVer", "destCfg", "tablesList"} {
				if !strings.Contains(init, arg) {
					t.Errorf("destWritableCheck is not passed %s: %s", arg, init)
				}
			}
			body := nodeSrc(fset, ifs.Body)
			returnsIt = strings.Contains(body, "return") && strings.Contains(body, "Success: false")
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if kv, ok := n.(*ast.KeyValueExpr); ok && nodeSrc(fset, kv) == `Operation: "start_sink"` && startSinkAt == token.NoPos {
			startSinkAt = kv.Pos()
		}
		return true
	})
	switch {
	case pinEnd == token.NoPos || startSinkAt == token.NoPos:
		t.Fatalf("anchors not found (table pin %v, start_sink %v) — the test is stale, not passing", pinEnd, startSinkAt)
	case checkAt == token.NoPos:
		t.Fatal("startKafkaMCPSink never calls a.destWritableCheck as a direct statement of its body")
	case checkAt < pinEnd:
		t.Fatal("destWritableCheck runs before the destination table is pinned, so it checks the wrong target")
	case checkAt > startSinkAt:
		t.Fatal("destWritableCheck runs after start_sink, so the run fails only after rows are refused")
	case !returnsIt:
		t.Fatal("destWritableCheck's refusal does not end the sink start with Success: false")
	}
}

func nodeSrc(fset *token.FileSet, n ast.Node) string {
	b, err := os.ReadFile(fset.File(n.Pos()).Name())
	if err != nil {
		return ""
	}
	return string(b[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
}
