package executor

// KI-DAG-TRANSFER-RUNS-WITH-NO-TABLES: a structured transfer that reached
// executeDataTransfer with no table list ran anyway and moved nothing. The DAG
// destination node never carries a table list (the planner leaves table choice
// to the run), so a DAG pipeline "completed" with 0 rows. Only the batch plan
// path asked the user which tables to sync; every other entry into the data
// plane (plan-less batch, the DAG destination node, CDC into an object store,
// the hybrid CDC load) ran the empty list. All of them must pause for table
// selection instead — the DAG scheduler already turns that pause into its
// node_input wait. A blob copy is the one move where no table list is valid: it
// means the whole bucket.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func zeroTablePGTask(params map[string]interface{}) ExecutorTask {
	return ExecutorTask{
		TaskID:      "task-zero",
		PipelineID:  "pl-zero",
		Operation:   "data_transfer",
		Source:      &ConnectorConfig{Type: "postgresql", Config: map[string]string{"host": "db.internal", "database": "appdb"}},
		Destination: &ConnectorConfig{Type: "gcs", Config: map[string]string{"bucket": "lake"}},
		Params:      params,
	}
}

// runPastGuard runs fn on an Agent with no data plane wired. Reaching the data
// plane dereferences it and panics; that is reported, not fatal, so a control
// can assert "not paused" and a guard test can say where the run went instead.
func runPastGuard(fn func() ExecutorResponse) (resp ExecutorResponse, reachedDataPlane interface{}) {
	defer func() {
		if r := recover(); r != nil {
			reachedDataPlane = r
		}
	}()
	return fn(), nil
}

func TestDataTransferWithNoTablesPausesForTableSelection(t *testing.T) {
	pgTables := discovered(t, postgresAppTables)

	cases := []struct {
		name string
		run  func(a *Agent) ExecutorResponse
	}{
		{"plan-less batch", func(a *Agent) ExecutorResponse {
			return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{}))
		}},
		{"empty selected_tables list", func(a *Agent) ExecutorResponse {
			return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{
				"selected_tables": []interface{}{}, "tables": []interface{}{" "},
			}))
		}},
		{"cdc into an object store", func(a *Agent) ExecutorResponse {
			return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{
				"sync_mode": "cdc", "source_connection_id": "src-conn",
			}))
		}},
		{"hybrid cdc batch load", func(a *Agent) ExecutorResponse {
			return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{
				"sync_mode": "cdc", "cdc_initial_load": "batch", "source_connection_id": "src-conn",
			}))
		}},
		{"DAG destination node", func(a *Agent) ExecutorResponse {
			task := nodeTask("destination", "data_transfer")
			pg := zeroTablePGTask(nil)
			task.Source, task.Destination = pg.Source, pg.Destination
			return a.executeTask(context.Background(), task)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, reached := runPastGuard(func() ExecutorResponse {
				return tc.run(agentDiscoveringTotals(pgTables, len(pgTables)))
			})
			if reached != nil {
				t.Fatalf("a transfer with no tables went on into the data plane (panic on the unwired Agent: %v)", reached)
			}
			if resp.Status != "waiting_for_table_selection" {
				t.Fatalf("status = %q (error %q), want the table-selection pause", resp.Status, resp.Error)
			}
			if opts, _ := resp.Result["available_tables"].([]map[string]interface{}); len(opts) != len(pgTables) {
				t.Fatalf("available_tables = %#v, want the %d discovered tables", resp.Result["available_tables"], len(pgTables))
			}
		})
	}
}

// A failed discovery still pauses (the user can type a table name); it must not
// fail the run or run it empty.
func TestDataTransferWithNoTablesPausesWhenDiscoveryFails(t *testing.T) {
	a := &Agent{discoverTotalsStub: func(context.Context, string, map[string]interface{}) ([]TableMetadata, discoveryTotals, error) {
		return nil, discoveryTotals{}, errors.New("connector unreachable")
	}}
	resp, reached := runPastGuard(func() ExecutorResponse {
		return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{}))
	})
	if reached != nil {
		t.Fatalf("went on into the data plane (panic: %v)", reached)
	}
	if resp.Status != "waiting_for_table_selection" || !strings.Contains(resp.Error, "couldn't list tables") {
		t.Fatalf("response = %q / %q, want the manual-entry table-selection pause", resp.Status, resp.Error)
	}
}

// Controls: the guard fires on the empty list only.
func TestDataTransferGuardLeavesRunnableMovesAlone(t *testing.T) {
	discoveryCalled := false
	a := &Agent{discoverTotalsStub: func(context.Context, string, map[string]interface{}) ([]TableMetadata, discoveryTotals, error) {
		discoveryCalled = true
		return nil, discoveryTotals{}, nil
	}}

	t.Run("tables selected", func(t *testing.T) {
		resp, _ := runPastGuard(func() ExecutorResponse {
			return a.executeDataTransfer(context.Background(), zeroTablePGTask(map[string]interface{}{
				"selected_tables": []interface{}{"public.users"},
			}))
		})
		if resp.Status == "waiting_for_table_selection" {
			t.Fatalf("a transfer with a selected table paused for table selection: %q", resp.Error)
		}
	})

	t.Run("blob copy of a whole bucket", func(t *testing.T) {
		discoveryCalled = false
		task := ExecutorTask{
			TaskID:      "task-blob",
			PipelineID:  "pl-blob",
			Operation:   "data_transfer",
			Source:      &ConnectorConfig{Type: "gcs", Config: map[string]string{"bucket": "raw"}},
			Destination: &ConnectorConfig{Type: "s3", Config: map[string]string{"bucket": "copy"}},
			Params:      map[string]interface{}{"copy_raw_files": true},
		}
		resp, _ := runPastGuard(func() ExecutorResponse { return a.executeDataTransfer(context.Background(), task) })
		if resp.Status == "waiting_for_table_selection" || discoveryCalled {
			t.Fatalf("a blob copy with no table list was asked for tables (status %q, discovery called %v)", resp.Status, discoveryCalled)
		}
	})
}

// Defense in depth: the batch data plane itself refuses an empty table list
// instead of reporting a successful run of 0 rows.
func TestBatchDataTransferRefusesAnEmptyTableList(t *testing.T) {
	for _, params := range []map[string]interface{}{
		{},
		{"tables": []interface{}{}},
		{"tables": []string{}},
	} {
		resp, reached := runPastGuard(func() ExecutorResponse {
			return (&Agent{}).executeBatchDataTransfer(context.Background(), zeroTablePGTask(params), "topic", "trace")
		})
		if reached != nil {
			t.Fatalf("params %v: the batch plane ran with no tables (panic: %v)", params, reached)
		}
		if resp.Status != "failed" || !strings.Contains(resp.Error, "no tables") {
			t.Fatalf("params %v: response = %q / %q, want failed naming the empty table list", params, resp.Status, resp.Error)
		}
	}
}
