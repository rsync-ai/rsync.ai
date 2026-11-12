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

// A REST connector reads a bounded number of pages per export call, so its batch
// comes back short while the table goes on. On prod a 1,250-row ClickUp list
// stopped at 1,000 and the run reported completed (KI-GENERATED-REST-STOPS-PAGING-EARLY).

func TestSourceHasMoreReadsTopLevelAndStats(t *testing.T) {
	cases := []struct {
		name string
		res  string
		want bool
	}{
		{"top level", `{"has_more": true}`, true},
		{"under stats", `{"stats": {"has_more": true}}`, true},
		{"false", `{"has_more": false, "stats": {"has_more": false}}`, false},
		{"absent", `{"records": []}`, false},
		{"not a bool", `{"has_more": "true", "stats": {"has_more": 1}}`, false},
	}
	for _, c := range cases {
		if got := sourceHasMore(decode(t, c.res)); got != c.want {
			t.Errorf("%s: sourceHasMore(%s) = %v, want %v", c.name, c.res, got, c.want)
		}
	}
}

func TestPageEndsTableUnlessMoreFollows(t *testing.T) {
	if !pageEndsTable(1000, 10000, false) {
		t.Error("a short page with no has_more must end the table")
	}
	if pageEndsTable(1000, 10000, true) {
		t.Error("a short page whose connector says more rows follow must NOT end the table")
	}
	if pageEndsTable(10000, 10000, false) {
		t.Error("a full page never ends the table")
	}
}

func TestSourceWarningsAreTrimmedStringsOnly(t *testing.T) {
	got := sourceWarnings(decode(t, `{"warnings": ["  paging not documented — rows past 100 may be missing ", "", 7, null]}`))
	want := []string{"paging not documented — rows past 100 may be missing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sourceWarnings = %q, want %q", got, want)
	}
	if got := sourceWarnings(decode(t, `{"warnings": "not a list"}`)); len(got) != 0 {
		t.Errorf("a non-list warnings field must give none, got %q", got)
	}
}

func TestAddSourceWarningRecordsEachWarningOncePerTable(t *testing.T) {
	byTable := map[string][]string{}
	if !addSourceWarning(byTable, "tasks", "w1") {
		t.Error("first warning must be new")
	}
	if addSourceWarning(byTable, "tasks", "w1") {
		t.Error("the same warning on the next export call must not be new")
	}
	if !addSourceWarning(byTable, "lists", "w1") {
		t.Error("the same warning on another table is new for that table")
	}
	if !reflect.DeepEqual(byTable, map[string][]string{"tasks": {"w1"}, "lists": {"w1"}}) {
		t.Errorf("byTable = %v", byTable)
	}
}

func TestSourceWarningEventIsAWarningThatCannotMoveProgress(t *testing.T) {
	evt := buildExecutorSourceWarningEvent("p1", "e1", "t1", "tasks", "paging not documented")
	if evt["severity"] != "warning" {
		t.Errorf("severity = %v, want warning (the timeline never hides a warning)", evt["severity"])
	}
	if _, ok := evt["progress"]; ok {
		t.Error("a warning event must carry no progress block, or it could reset the run's percent")
	}
	if evt["table_name"] != "tasks" || !strings.Contains(evt["message"].(string), "paging not documented") {
		t.Errorf("event must name the table and carry the warning: %v", evt)
	}
}

// The helpers are only half the fix: every place the batch loop reads a short
// page as end-of-table must ask pageEndsTable, and a short page that says more
// rows follow must either advance or fail the run — never end it as completed.
func TestSourceHasMoreIsWiredIntoTheBatchLoop(t *testing.T) {
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

	if !strings.Contains(batch, "moreFollows := sourceHasMore(res)") {
		t.Error("the batch loop must read the connector's has_more on every page")
	}
	// Code only: comments may still name the old comparison.
	var code strings.Builder
	for _, line := range strings.Split(batch, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			code.WriteString(line + "\n")
		}
	}
	if strings.Contains(code.String(), "sourceRowCount < exportBatchSize") {
		t.Error("a bare `sourceRowCount < exportBatchSize` ends the table on a short page the connector says is not the last; use pageEndsTable")
	}
	if n := strings.Count(batch, "pageEndsTable(sourceRowCount, exportBatchSize, moreFollows)"); n != 3 {
		t.Errorf("pageEndsTable must decide all 3 end-of-table sites (filtered page, table_complete checkpoint, EOF break); found %d", n)
	}
	if n := strings.Count(batch, "SOURCE PAGING:"); n != 2 {
		t.Errorf("a short page with has_more and no usable cursor (none, or not advancing) must fail the run; found %d of 2 fatals", n)
	}
	if !strings.Contains(batch, `result["source_warnings"]`) {
		t.Error("source warnings must be reported on the run's result")
	}
}

// A failed page now fails the export instead of being skipped, so the status a
// REST connector reports for it ("Request failed with status 503") must count
// as transient like "HTTP 503" always did, or one passing blip fails the run.
func TestTransientFailureMatchesBothStatusSpellings(t *testing.T) {
	for _, msg := range []string{
		"request failed with status 503: {\"err\":\"busy\"}",
		"request failed with status 429",
		"operation failed (http 502)",
		"context deadline exceeded (timeout)",
	} {
		if !isTransientFailure(msg) {
			t.Errorf("%q must be retried", msg)
		}
	}
	for _, msg := range []string{
		"request failed with status 404",
		"request failed with status 401: unauthorized",
		"unknown operation 'export_data'",
	} {
		if isTransientFailure(msg) {
			t.Errorf("%q must not be retried", msg)
		}
	}
}
