package executor

import (
	"context"
	"strings"
	"testing"
)

// The executor never sees the NL request, so its pause reason must hold
// whether or not the user named a table: "You didn’t specify a table" was
// wrong for a request that named one the source lacks (pipeline 05ace10c).
// The gateway adds the named-table detail from the request.
func TestTableSelectionPause_ReasonHoldsWhetherOrNotATableWasNamed(t *testing.T) {
	pgTables := discovered(t, postgresAppTables)
	for _, path := range []string{"cdc", "batch", "step guard"} {
		t.Run(path, func(t *testing.T) {
			a := agentDiscoveringTotals(pgTables, len(pgTables))
			task := ExecutorTask{
				TaskID:      "task-reason",
				PipelineID:  "pl-reason",
				Operation:   "export",
				Source:      &ConnectorConfig{Type: "postgresql", Config: map[string]string{"host": "db.internal", "database": "appdb"}},
				Destination: &ConnectorConfig{Type: "gcs", Config: map[string]string{"bucket": "lake"}},
				Params:      map[string]interface{}{},
			}
			var resp ExecutorResponse
			if path == "step guard" {
				task.Operation = "execute"
				resp = a.executePlan(context.Background(), task, map[string]interface{}{
					"steps": []interface{}{
						map[string]interface{}{"id": "export_1", "tool": "postgresql", "method": "export", "params": map[string]interface{}{"table": "postgresql"}},
					},
				})
			} else {
				task.Payload = map[string]interface{}{"plan": map[string]interface{}{"sync_mode": path, "steps": []interface{}{}}}
				resp = a.executeTask(context.Background(), task)
			}
			if resp.Status != "waiting_for_table_selection" {
				t.Fatalf("status = %q (error %q), want the table-selection pause", resp.Status, resp.Error)
			}
			reason, _ := resp.Result["reason"].(string)
			if strings.Contains(reason, "specify") {
				t.Fatalf("reason %q claims no table was specified", reason)
			}
			if !strings.Contains(reason, "postgresql") || !strings.Contains(reason, "Select") {
				t.Fatalf("reason %q must name the source and ask for a pick", reason)
			}
		})
	}
}
