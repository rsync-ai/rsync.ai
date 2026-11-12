package workflows

import "testing"

// The PIPELINE_WAITING event's description is what pipeline_progress stores and
// what the live event hands the UI. A table-selection wait carries the
// executor's reason; the description used to drop it — "Waiting for user input"
// on a DAG node wait, a bare count on the batch path.
func TestWaitingEventDescription(t *testing.T) {
	reason := "No table is selected yet. Found 6 tables/resources in mongodb. Select what to sync."
	six := []interface{}{
		map[string]interface{}{"name": "a"}, map[string]interface{}{"name": "b"}, map[string]interface{}{"name": "c"},
		map[string]interface{}{"name": "d"}, map[string]interface{}{"name": "e"}, map[string]interface{}{"name": "f"},
	}
	cases := []struct {
		name    string
		typ     string
		details map[string]interface{}
		want    string
	}{
		{"batch wait uses the executor's reason", "table_selection",
			map[string]interface{}{"reason": reason, "available_tables": six}, reason},
		{"DAG node wait for tables uses the reason", "node_input",
			map[string]interface{}{"action_needed": "table_selection", "node_id": "load_gcs", "reason": reason, "available_tables": six}, reason},
		{"DAG node wait for tables without a reason counts them", "node_input",
			map[string]interface{}{"action_needed": "table_selection", "available_tables": six}, "Select table(s) to sync (6 available)"},
		{"batch wait without a reason counts them", "table_selection",
			map[string]interface{}{"available_tables": []map[string]interface{}{{"name": "a"}, {"name": "b"}}}, "Select table(s) to sync (2 available)"},
		{"batch wait with nothing", "table_selection", map[string]interface{}{}, "Table selection required"},
		{"a blank reason is no reason", "table_selection",
			map[string]interface{}{"reason": "  ", "available_tables": six}, "Select table(s) to sync (6 available)"},
		{"other node waits stay generic", "node_input",
			map[string]interface{}{"action_needed": "approval", "reason": "ignored"}, "Waiting for user input"},
		{"connector wait unchanged", "connector_generation",
			map[string]interface{}{"missing_connectors": []string{"hubspot"}}, "Connector generation required for [hubspot]"},
		{"connection wait unchanged", "connection_config",
			map[string]interface{}{"missing_connections": []interface{}{map[string]interface{}{"connector_type": "mysql", "direction": "source"}}},
			formatMissingConnectionsDescription([]interface{}{map[string]interface{}{"connector_type": "mysql", "direction": "source"}})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingEventDescription(tc.typ, tc.details); got != tc.want {
				t.Fatalf("waitingEventDescription(%q) = %q, want %q", tc.typ, got, tc.want)
			}
		})
	}
}
