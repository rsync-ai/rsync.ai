package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The Explorer asks a warehouse for per-table columns and row counts with
// include_table_details; a flag the route does not forward never reaches the
// connector, and the panel stays empty.
func TestDiscoverSchemaRequest_ForwardsEveryFlagTheCallerSet(t *testing.T) {
	var req discoverSchemaRequest
	body := `{"connector_type":"bigquery","include_columns":true,"include_row_counts":true,
		"include_relationships":false,"include_indexes":false,"include_table_details":true,"max_tables":200}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]interface{}{
		"include_columns":       true,
		"include_row_counts":    true,
		"include_relationships": false,
		"include_indexes":       false,
		"include_table_details": true,
		"max_tables":            200,
	}
	if got := req.params(); !reflect.DeepEqual(got, want) {
		t.Fatalf("params = %v, want %v", got, want)
	}
}

// Control: a caller that sets nothing (pipeline discovery's own defaults apply
// in DiscoverSchemaEnvelope) forwards nothing — in particular no
// include_table_details, so a pipeline never pays for per-table metadata.
func TestDiscoverSchemaRequest_ForwardsNothingUnset(t *testing.T) {
	var req discoverSchemaRequest
	if err := json.Unmarshal([]byte(`{"connector_type":"bigquery"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := req.params(); len(got) != 0 {
		t.Fatalf("params = %v, want none", got)
	}
}
