package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A CDC run into an API destination that addresses each record by the id its
// API assigned fails at start (KI-API-DEST-CDC-WRITES-BY-SOURCE-ID). Bug class: a
// write lane that addresses records by a key the destination never gave them.

func TestCDCRecordIDsRefusal(t *testing.T) {
	cases := []struct {
		name    string
		caps    map[string]interface{}
		cfg     map[string]string
		refused bool
	}{
		{"record_ids at the top level", map[string]interface{}{"record_ids": true}, nil, true},
		{"record_ids under capabilities", map[string]interface{}{"capabilities": map[string]interface{}{"record_ids": true}}, nil, true},
		{"record_ids as a string", map[string]interface{}{"record_ids": "true"}, map[string]string{"cdc_write_mode": "upsert"}, true},
		// Controls: each differs from a refused case in one input.
		{"append-only history", map[string]interface{}{"record_ids": true}, map[string]string{"cdc_write_mode": " Append "}, false},
		{"record_ids false", map[string]interface{}{"record_ids": false}, nil, false},
		{"no record_ids (a database)", map[string]interface{}{"capabilities": map[string]interface{}{"supports_ddl": true}}, nil, false},
		{"no answer", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := CDCRecordIDsRefusal(c.caps, "t-clickup", c.cfg)
			if !c.refused {
				if msg != "" {
					t.Fatalf("refused: %s", msg)
				}
				return
			}
			for _, want := range []string{"t-clickup", "batch sync", "cdc_write_mode=append"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not name %q", msg, want)
				}
			}
		})
	}
}

// The words that exempt a destination are the ones the sink writes as
// append-only history; the gateway's list is held to the same sink function
// (api-gateway preflight_cdc_record_ids_test.go).
func TestCDCAppendOnlyDestination_MatchesTheSink(t *testing.T) {
	m := regexp.MustCompile(`(?s)func cdcAppendMode\(.*?\n\tcase ([^\n]*):\n\t\treturn true`).FindStringSubmatch(readSinkWorkerMain(t))
	if m == nil {
		t.Fatal("cdcAppendMode's case list not found in the sink's main.go")
	}
	words := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1)
	if len(words) < 3 {
		t.Fatalf("parsed only %v from the sink — the parse is wrong, not the lists", words)
	}
	for _, w := range words {
		if !CDCAppendOnlyDestination(map[string]string{"cdc_write_mode": w[1]}) {
			t.Errorf("the sink writes %q as append-only history; CDCAppendOnlyDestination does not", w[1])
		}
	}
	for _, w := range []string{"", "upsert", "merge"} {
		if CDCAppendOnlyDestination(map[string]string{"cdc_write_mode": w}) {
			t.Errorf("%q is not an append mode", w)
		}
	}
}

func parseExecutorFunc(t *testing.T, file, name string) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fset, fd
		}
	}
	t.Fatalf("%s not found in %s; if it moved or was renamed, move this guard with it", name, file)
	return nil, nil
}

// executeDataTransfer runs the check once the run's sync mode is known (the DB
// backfill) and before any lane starts: the topic, the blob lane, the table
// pause, Debezium (executeStreamingDataTransfer) or the hybrid batch load. A
// refusal at the sink start would come after Debezium had made its slot.
func TestExecuteDataTransfer_RefusesCDCIntoARecordIDsDestinationBeforeAnyLane(t *testing.T) {
	fset, fn := parseExecutorFunc(t, "executor.go", "executeDataTransfer")
	backfillEnd, checkAt := token.NoPos, token.NoPos
	for _, st := range fn.Body.List {
		ifs, ok := st.(*ast.IfStmt)
		if !ok {
			continue
		}
		cond := nodeSrc(fset, ifs.Cond)
		if strings.Contains(cond, `strings.EqualFold(strings.TrimSpace(syncMode), "batch")`) {
			backfillEnd = ifs.End()
		}
		body := nodeSrc(fset, ifs.Body)
		if checkAt == token.NoPos && strings.Contains(body, "a.cdcRecordIDsCheck(ctx, task)") {
			checkAt = ifs.Pos()
			if !strings.Contains(cond, `syncMode == "cdc"`) || !strings.Contains(cond, `syncMode == "streaming"`) {
				t.Errorf("the check is not gated on the CDC lanes' own condition: %s", cond)
			}
			if !strings.Contains(body, `Status:     "failed"`) || !strings.Contains(body, "Error:      msg") {
				t.Errorf("the check's refusal does not fail the run with its message: %s", body)
			}
		}
	}
	lanes := map[string]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			name := nodeSrc(fset, call.Fun)
			switch name {
			case "resolvePipelineTopic", "deriveMoveModality", "a.tableSelectionPause",
				"a.executeHybridCDCDataTransfer", "a.executeStreamingDataTransfer":
				if _, seen := lanes[name]; !seen {
					lanes[name] = call.Pos()
				}
			}
		}
		return true
	})
	if backfillEnd == token.NoPos || len(lanes) != 5 {
		t.Fatalf("anchors not found (DB backfill %v, lanes %v) — the test is stale, not passing", backfillEnd, lanes)
	}
	if checkAt == token.NoPos {
		t.Fatal("executeDataTransfer never calls a.cdcRecordIDsCheck as a direct statement of its body: " +
			"a CDC run into a destination that keys records by its API's ids starts")
	}
	if checkAt < backfillEnd {
		t.Fatal("the check runs before the DB backfill, so a plan-less CDC run is checked as batch")
	}
	for name, at := range lanes {
		if at < checkAt {
			t.Errorf("%s runs before the record-ids check", name)
		}
	}
}

// Census: the CDC lanes are entered only from executeDataTransfer (past the
// check) and from the hybrid lane it starts, and the orchestrator starts a sink
// only in startKafkaMCPSink and the CDC sink restart, which refuses on its own
// (handlers cdc_sink_record_ids_test.go). A new entry fails here until it is
// shown to pass a record-ids check.
func TestCDCSinkStarts_AllPassARecordIDsCheck(t *testing.T) {
	wantCallers := map[string][]string{
		"executeStreamingDataTransfer": {"executeDataTransfer", "executeHybridCDCDataTransfer"},
		"executeHybridCDCDataTransfer": {"executeDataTransfer"},
		`Operation: "start_sink"`:      {"handlers.restartCDCSinkWorker", "executor.startKafkaMCPSink"},
	}
	got := map[string]map[string]bool{}
	note := func(key, fn string) {
		if got[key] == nil {
			got[key] = map[string]bool{}
		}
		got[key][fn] = true
	}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr: // a call, or a method value handed on
					if _, tracked := wantCallers[x.Sel.Name]; tracked {
						note(x.Sel.Name, fd.Name.Name)
					}
				case *ast.KeyValueExpr:
					if nodeSrc(fset, x) == `Operation: "start_sink"` {
						note(`Operation: "start_sink"`, f.Name.Name+"."+fd.Name.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for key, want := range wantCallers {
		var have []string
		for fn := range got[key] {
			have = append(have, fn)
		}
		sort.Strings(have)
		want = append([]string(nil), want...)
		sort.Strings(want)
		if strings.Join(have, ",") != strings.Join(want, ",") {
			t.Errorf("%s is reached from %v, want %v — a new entry must pass a record-ids check", key, have, want)
		}
	}
}
