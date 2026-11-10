package executor

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/mcp"
)

// fakeConnector answers checkTables' calls: a discover_schema listing, and one export
// answer per table. It records every request it was sent.
type fakeConnector struct {
	mu       sync.Mutex
	listing  string
	exports  map[string]*mcp.ExecuteResponse
	block    bool // export waits for its context to end
	requests []mcp.ExecuteRequest
}

func (f *fakeConnector) call(ctx context.Context, req mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if req.Operation == "discover_schema" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(f.listing), &m); err != nil {
			return nil, err
		}
		return &mcp.ExecuteResponse{Success: true, Result: m}, nil
	}
	if f.block {
		<-ctx.Done()
		return &mcp.ExecuteResponse{Success: false, Error: "HTTP request failed: " + ctx.Err().Error()}, nil
	}
	return f.exports[req.Params["table"].(string)], nil
}

func noDisplayName(string, string) string { return "Acme" }

func TestCheckTablesReadsOnePageOfEveryTableAndKeepsNoRow(t *testing.T) {
	const rowValue = "jane.doe-private-row-value"
	f := &fakeConnector{
		listing: `{"success":true,"tables":[
			{"name":"tasks","supports_list":true},
			{"name":"goals","supports_list":true},
			{"name":"webhooks","supports_list":false}],
			"tables_needing_settings":[{"name":"list_tasks","missing_settings":["list_id"]}]}`,
		exports: map[string]*mcp.ExecuteResponse{
			"tasks": {Success: true, Result: map[string]interface{}{
				"records":  []interface{}{map[string]interface{}{"id": "1", "name": rowValue}},
				"warnings": []interface{}{"paging not documented — rows past 1 may be missing"},
			}},
			"goals": {Success: false, Error: "HTTP 404: Not Found for /v2/team/goal"},
		},
	}
	config := map[string]string{"api_token": "t", "connector_version": "v1.0.0"}

	got, err := checkTables(context.Background(), f.call, "acme", config, noDisplayName)
	if err != nil {
		t.Fatalf("checkTables: %v", err)
	}

	want := map[string]TableCheck{
		"tasks":      {Name: "tasks", Status: TableCheckOK, Warnings: []string{"paging not documented — rows past 1 may be missing"}},
		"goals":      {Name: "goals", Status: TableCheckFailed, Error: "HTTP 404: Not Found for /v2/team/goal"},
		"list_tasks": {Name: "list_tasks", Status: TableCheckNeedsSetting, MissingSettings: []string{"list_id"}},
	}
	if len(got.Tables) != len(want) {
		t.Fatalf("got %d tables, want %d (a create-only resource is not read): %+v", len(got.Tables), len(want), got.Tables)
	}
	for _, tc := range got.Tables {
		w, ok := want[tc.Name]
		gotJSON, _ := json.Marshal(tc)
		wantJSON, _ := json.Marshal(w)
		if !ok || string(gotJSON) != string(wantJSON) {
			t.Errorf("table %s = %s, want %s", tc.Name, gotJSON, wantJSON)
		}
	}
	if got.OK != 1 || got.Failed != 1 || got.NeedsSetting != 1 || got.NotChecked != 0 {
		t.Errorf("counts = ok %d failed %d needs_setting %d not_checked %d, want 1/1/1/0",
			got.OK, got.Failed, got.NeedsSetting, got.NotChecked)
	}
	if out, _ := json.Marshal(got); strings.Contains(string(out), rowValue) {
		t.Errorf("the result carries a row value: %s", out)
	}

	// Every read is one row, at the connection's version, and never on stdio.
	exports := 0
	for _, req := range f.requests {
		if req.Version != "v1.0.0" || !req.NoStdioWhileDeploying {
			t.Errorf("%s request: version %q, no-stdio %v; want v1.0.0 and true", req.Operation, req.Version, req.NoStdioWhileDeploying)
		}
		switch req.Operation {
		case "discover_schema":
			if req.Params["include_row_counts"] != false {
				t.Errorf("the listing probes every table: include_row_counts = %v", req.Params["include_row_counts"])
			}
		case "export":
			exports++
			if req.Params["limit"] != 1 || req.Params["max_pages"] != 1 {
				t.Errorf("export of %v reads more than one row: limit %v, max_pages %v",
					req.Params["table"], req.Params["limit"], req.Params["max_pages"])
			}
		}
	}
	if exports != 2 {
		t.Errorf("%d exports, want 2 (tasks, goals)", exports)
	}
}

func TestCheckTablesStopsAtItsTimeBudget(t *testing.T) {
	old := tableCheckBudget
	tableCheckBudget.perTable = 40 * time.Millisecond
	tableCheckBudget.total = 130 * time.Millisecond
	t.Cleanup(func() { tableCheckBudget = old })

	f := &fakeConnector{
		block: true,
		listing: `{"success":true,"tables":[{"name":"t1"},{"name":"t2"},{"name":"t3"},
			{"name":"t4"},{"name":"t5"},{"name":"t6"},{"name":"t7"},{"name":"t8"}]}`,
	}
	start := time.Now()
	got, err := checkTables(context.Background(), f.call, "acme", map[string]string{}, noDisplayName)
	if err != nil {
		t.Fatalf("checkTables: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the check ran %s, past its %s budget", elapsed, tableCheckBudget.total)
	}
	if len(got.Tables) != 8 {
		t.Fatalf("got %d tables, want all 8 accounted for", len(got.Tables))
	}
	if first := got.Tables[0]; first.Status != TableCheckFailed || !strings.Contains(first.Error, "no answer within") {
		t.Errorf("a table that does not answer within its share = %+v, want failed, no answer", first)
	}
	if last := got.Tables[7]; last.Status != TableCheckNotChecked {
		t.Errorf("a table left when the budget ran out = %+v, want not_checked", last)
	}
	if got.Failed+got.NotChecked != 8 || got.NotChecked == 0 {
		t.Errorf("counts = failed %d not_checked %d", got.Failed, got.NotChecked)
	}
}

func TestCheckTablesReportsAFailedImageBuildAsItsCause(t *testing.T) {
	build := "connector image failed to build. End of the build log:\nNo module named 'acme'"
	call := func(context.Context, mcp.ExecuteRequest) (*mcp.ExecuteResponse, error) {
		return &mcp.ExecuteResponse{Success: false,
			Error: "Failed to start server: connector acme@v1.0.0 could not be deployed: " + build}, nil
	}
	_, err := checkTables(context.Background(), call, "acme", map[string]string{}, noDisplayName)
	if err == nil || !strings.Contains(err.Error(), "The Acme "+build) || mcp.IsConnectorDeployingMessage(err.Error()) {
		t.Errorf("a failed build reads %v, want its build log", err)
	}
}
