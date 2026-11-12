package handlers

import "testing"

// Schedules are batch-only, so the gate has to agree with pipelineListIsCDCSQL on
// what a CDC pipeline is. It used to ask the source connection first, which refused
// a schedule on every batch pipeline reading from a CDC-enabled connection — all
// three batch pipelines on the deployed host.
func TestSchedulingMode(t *testing.T) {
	cases := []struct {
		name       string
		syncMode   string
		hasCDCMode bool
		sourceMode string
		allowed    bool
		mode       string
	}{
		{"batch pipeline on a CDC source", "batch", false, "cdc", true, "batch"},
		{"batch pipeline on a batch source", "batch", false, "batch", true, "batch"},
		{"padded, mixed-case batch", "  Batch ", false, "cdc", true, "batch"},
		// A batch pipeline created from chat used to keep the source's cdc_mode default.
		{"batch with a leftover cdc_mode", "batch", true, "cdc", true, "batch"},
		{"cdc pipeline", "cdc", false, "batch", false, "cdc"},
		{"mixed-case cdc", " CDC", false, "", false, "cdc"},
		{"legacy row: no sync_mode, cdc_mode set", "", true, "", false, "cdc"},
		{"no mode of its own, CDC source", "", false, "cdc", false, "cdc"},
		{"no mode anywhere", "", false, "", true, "batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, mode := schedulingMode(tc.syncMode, tc.hasCDCMode, tc.sourceMode)
			if allowed != tc.allowed || mode != tc.mode {
				t.Fatalf("schedulingMode(%q, %v, %q) = (%v, %q), want (%v, %q)",
					tc.syncMode, tc.hasCDCMode, tc.sourceMode, allowed, mode, tc.allowed, tc.mode)
			}
		})
	}
}

// A schedule on a pipeline with no saved table selection is dead on arrival: the
// executor's batch path refuses to infer a table, so the first run parks at the
// table_selection HITL and the wrapper workflow's overlap guard then skips every
// later tick as a "successful" no-op. hasSelectedTables is the gate that stops
// that schedule from being created, so its notion of "selected" has to match the
// one FetchPipelineRunContextActivity uses to hydrate the run.
func TestHasSelectedTables(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   bool
	}{
		{"nil config", "", false},
		{"empty object", `{}`, false},
		{"null selection", `{"selected_tables": null}`, false},
		{"empty array", `{"selected_tables": []}`, false},
		{"blank entries only", `{"selected_tables": ["", "   "]}`, false},
		{"non-array selection", `{"selected_tables": "orders"}`, false},
		{"wrong element type", `{"selected_tables": [1, 2]}`, false},
		{"malformed json", `{"selected_tables": [`, false},

		{"single table", `{"selected_tables": ["orders"]}`, true},
		{"schema-qualified", `{"selected_tables": ["public.orders"]}`, true},
		{"several tables", `{"selected_tables": ["orders", "customers"]}`, true},
		// One usable entry is enough — the run syncs what it can resolve.
		{"blank alongside real", `{"selected_tables": ["", "orders"]}`, true},
		{"padded entry", `{"selected_tables": ["  orders  "]}`, true},
		// Other config keys must not be mistaken for a selection: `table` is
		// exactly the field the executor's HITL policy refuses to infer from.
		{"table key is not a selection", `{"table": "orders"}`, false},
		{"tables key is not a selection", `{"tables": ["orders"]}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.config != "" {
				raw = []byte(tc.config)
			}
			if got := hasSelectedTables(raw); got != tc.want {
				t.Fatalf("hasSelectedTables(%q) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}
