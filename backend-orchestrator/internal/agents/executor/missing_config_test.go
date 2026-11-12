package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
)

// A run whose connection lacks a setting must say which one (generator-trust
// phase 2). On prod 2026-10-05 a generated ClickUp connector with a blank list
// ID failed with "Missing required path parameter '[redacted]' for endpoint
// '[redacted]'": the scrub that keeps row values out of the stored error also
// removed the setting's NAME, so the run's wait step could only show
// "Source: Unknown". Bug class: the name of what the user must fix is lost
// before it reaches the user. The name is a connection-setting key (metadata),
// taken from the raw error before scrubbing, and only when it looks like a key.

func TestMissingConfigFieldReadsTheSettingName(t *testing.T) {
	cases := []struct{ raw, want string }{
		// connector.py.j2 _make_request_v2, verbatim.
		{"Missing required path parameter 'list_id' for endpoint '/list/{list_id}/task'", "list_id"},
		{"export failed: ValueError: Missing required path parameter 'team_id' for endpoint '/team/{team_id}/task'", "team_id"},
		// Not a key: a value would be carried out unscrubbed, so take nothing.
		{"Missing required path parameter 'john@example.com' for endpoint '/x'", ""},
		{"Missing required path parameter 'a b' for endpoint '/x'", ""},
		{"Request failed with status 404", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := missingConfigField(c.raw); got != c.want {
			t.Errorf("missingConfigField(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestExportFailureNamesTheMissingSettingOnTheResult(t *testing.T) {
	task := ExecutorTask{TaskID: "t1", PipelineID: "p1"}
	missing := []map[string]interface{}{{"table": "list_task", "field": "list_id", "direction": "source"}}
	resp := exportFailureResponse(task, 1, "Missing required path parameter '[redacted]' for endpoint '[redacted]'", missing)
	if resp.Status != "failed" {
		t.Fatalf("status = %q, want failed", resp.Status)
	}
	if !strings.HasPrefix(resp.Error, "batch export failed (partial sync, 1 rows landed before error): ") {
		t.Fatalf("error = %q — the message the healers classify must not change", resp.Error)
	}
	if got := resp.Result["missing_config"]; !reflect.DeepEqual(got, missing) {
		t.Fatalf("result missing_config = %#v, want %#v", got, missing)
	}

	// No missing setting, no result key.
	if r := exportFailureResponse(task, 0, "", nil); r.Result != nil || r.Error != "batch export failed (partial sync, 0 rows landed before error): export failed (no details)" {
		t.Fatalf("got %#v", r)
	}
}

func TestNoteMissingConfigKeepsEachSettingOnce(t *testing.T) {
	var got []map[string]interface{}
	got = noteMissingConfig(got, "list_task", "Missing required path parameter 'list_id' for endpoint '/a'")
	got = noteMissingConfig(got, "list_task_2", "Missing required path parameter 'list_id' for endpoint '/b'")
	got = noteMissingConfig(got, "team_task", "Request failed with status 500")
	want := []map[string]interface{}{{"table": "list_task", "field": "list_id", "direction": "source"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// Both export-failure branches must record the setting from the RAW error, and
// the run's failure must be built by exportFailureResponse.
func TestMissingConfigIsWiredIntoTheBatchLoop(t *testing.T) {
	fset := token.NewFileSet()
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	f, err := parser.ParseFile(fset, "executor.go", src, 0)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}
	var batch string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "executeBatchDataTransfer" {
			batch = string(src[fset.Position(fd.Body.Pos()).Offset:fset.Position(fd.Body.End()).Offset])
		}
	}
	if batch == "" {
		t.Fatal("executeBatchDataTransfer not found in executor.go; if it moved or was renamed, update this test")
	}
	for _, want := range []string{
		"missingConfig = noteMissingConfig(missingConfig, tableName, err.Error())",
		"missingConfig = noteMissingConfig(missingConfig, tableName, exportResp.Error)",
		"return exportFailureResponse(task, totalRows, lastExportError, missingConfig)",
	} {
		if !strings.Contains(batch, want) {
			t.Errorf("executeBatchDataTransfer is missing %q", want)
		}
	}
}
