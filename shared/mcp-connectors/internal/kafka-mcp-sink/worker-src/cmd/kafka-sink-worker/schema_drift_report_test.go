package main

// CDC-A1: the sink applied a new source column inside ensureDestinationTable and told
// nobody, so a CDC pipeline changed its destination with no drift row, no badge, no
// notification and no schema changes entry. These pin the reporting half: WHAT is
// reported (only genuinely new or dropped columns, in a stable order) and WHEN (never
// on the first ensure of a process, or every restart would file drift).
//
// The drift policy is one for batch and CDC: only a dropped table waits for a person.
// A dropped column used to halt the CDC stream (os.Exit) while batch kept going; now
// the destination keeps the column and the drop is reported once.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func set(cols ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(cols))
	for _, c := range cols {
		m[c] = struct{}{}
	}
	return m
}

func TestAddedColumnNames(t *testing.T) {
	tests := []struct {
		name   string
		prev   map[string]struct{}
		merged map[string]struct{}
		want   []string
	}{
		{
			name:   "no change reports nothing",
			prev:   set("id", "name"),
			merged: set("id", "name"),
			want:   []string{},
		},
		{
			name:   "one new column",
			prev:   set("id", "name"),
			merged: set("id", "name", "email"),
			want:   []string{"email"},
		},
		{
			// Sorted, not map order: the DDL string becomes the
			// schema_change_approvals UNIQUE (pipeline_id, ddl) key, so an unstable
			// order would file the same change twice under two different DDLs.
			name:   "multiple new columns come back sorted",
			prev:   set("id"),
			merged: set("id", "zeta", "alpha", "mid"),
			want:   []string{"alpha", "mid", "zeta"},
		},
		{
			// __pk__ entries are ensure-bookkeeping, not columns. Reporting one would
			// emit "ALTER TABLE t ADD COLUMN __pk__id unknown" — DDL for a column that
			// does not exist.
			name:   "pk bookkeeping entries are not columns",
			prev:   set("id"),
			merged: set("id", "__pk__id"),
			want:   []string{},
		},
		{
			name:   "new column alongside new pk bookkeeping",
			prev:   set("id"),
			merged: set("id", "__pk__id", "email"),
			want:   []string{"email"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addedColumnNames(tt.prev, tt.merged)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("addedColumnNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A nil writer is the "reporting disabled" state (batch path, or a worker built
// before the drift writer existed). It must be a no-op, not a nil-pointer panic on
// the CDC apply path — the destination write has already committed by then.
func TestReportAppliedSchemaDrift_NilWriterIsNoop(t *testing.T) {
	reportAppliedSchemaDrift(context.Background(), nil, &WorkerConfig{PipelineID: "p1"},
		"p1", "orders", "public", []string{"email"}, map[string]interface{}{"email": "TEXT"})
}

// An unattributable change cannot be filed: schema_change_approvals.pipeline_id is
// NOT NULL and FK-checked against pipelines(id). Dropping it is the only option, and
// it must not panic or block on a broker to do so. A non-nil writer here would
// attempt a real publish, so the empty-column guard is what keeps this offline.
func TestReportAppliedSchemaDrift_NoColumnsIsNoop(t *testing.T) {
	reportAppliedSchemaDrift(context.Background(), nil, &WorkerConfig{},
		"", "orders", "public", nil, nil)
}

func TestDroppedColumnNames(t *testing.T) {
	tests := []struct {
		name string
		prev map[string]struct{}
		cols []string
		keys []string
		want []string
	}{
		{"nothing dropped", set("id", "name"), []string{"id", "name"}, nil, []string{}},
		{"one column dropped", set("id", "name", "note"), []string{"id", "name"}, nil, []string{"note"}},
		{"sorted, not map order", set("id", "zeta", "alpha", "mid"), []string{"id"}, nil, []string{"alpha", "mid", "zeta"}},
		// A key column the row does not carry as data is still present.
		{"a key column counts as present", set("id", "name"), []string{"name"}, []string{"id"}, []string{}},
		// __pk__ entries are ensure bookkeeping, not columns.
		{"pk bookkeeping is not a column", set("id", "__pk__id"), []string{"id"}, nil, []string{}},
		{"a new column is not a drop", set("id"), []string{"id", "email"}, nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := droppedColumnNames(tt.prev, tt.cols, tt.keys); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("droppedColumnNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ddlOn is DDL support already resolved as enabled, so ensureDestinationTable runs
// without probing a destination.
func ddlOn() *DDLSupport { return &DDLSupport{Enabled: true, resolved: true} }

// A column the source dropped used to return a fatalError, which both CDC apply paths
// turned into os.Exit: the stream stopped until someone intervened. The drift policy
// keeps the destination column and moves on. The table was already ensured with the
// column, so nothing reaches the destination (a nil HTTP client would fail any call).
func TestEnsureDestinationTable_DroppedColumnKeepsStreaming(t *testing.T) {
	ddl := ddlOn()
	key := ensuredCacheKey("postgresql", "public", "orders")
	ddl.Ensured.Store(key, set("id", "name", "note", "__pk__id"))

	rows := []map[string]interface{}{{"id": 1, "name": "a"}}
	for i := 0; i < 3; i++ {
		err := ensureDestinationTable(context.Background(), nil, &WorkerConfig{PipelineID: "p1"}, ddl,
			"postgresql", "orders", "public", rows, []string{"id"}, nil, "p1", true, true, true)
		if err != nil {
			t.Fatalf("row %d without a dropped column: ensureDestinationTable = %v, want nil", i, err)
		}
	}

	// The destination still has the column, so the ensured set keeps it: a column
	// that comes back is not re-ensured or reported as added.
	v, _ := ddl.Ensured.Load(key)
	if _, ok := v.(map[string]struct{})["note"]; !ok {
		t.Errorf("the dropped column left the ensured set: %v", v)
	}
	// Reported once, not once per row.
	if again := ddl.firstDrops(key, []string{"note"}); len(again) != 0 {
		t.Errorf("the drop was not marked reported after the first row: firstDrops = %v", again)
	}
}

// A delete image under non-FULL replica identity carries only the key; with detection
// off it must not mark every data column dropped.
func TestEnsureDestinationTable_DeleteImageIsNotADrop(t *testing.T) {
	ddl := ddlOn()
	key := ensuredCacheKey("postgresql", "public", "orders")
	ddl.Ensured.Store(key, set("id", "name", "__pk__id"))

	err := ensureDestinationTable(context.Background(), nil, &WorkerConfig{PipelineID: "p1"}, ddl,
		"postgresql", "orders", "public", []map[string]interface{}{{"id": 1}}, []string{"id"}, nil, "p1", true, false, true)
	if err != nil {
		t.Fatalf("ensureDestinationTable = %v", err)
	}
	if first := ddl.firstDrops(key, []string{"name"}); len(first) != 1 {
		t.Errorf("a delete image marked a column dropped")
	}
}

func TestFirstDropsReportsEachColumnOnce(t *testing.T) {
	ddl := &DDLSupport{}
	if got := ddl.firstDrops("pg:s:t", []string{"a", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("first sighting = %v, want [a b]", got)
	}
	if got := ddl.firstDrops("pg:s:t", []string{"a", "c"}); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("second sighting = %v, want [c]", got)
	}
	// Another table's column of the same name is its own drop.
	if got := ddl.firstDrops("pg:s:u", []string{"a"}); !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("other table = %v, want [a]", got)
	}
}

func TestReportDroppedColumns_NilWriterIsNoop(t *testing.T) {
	reportDroppedColumns(context.Background(), nil, &WorkerConfig{PipelineID: "p1"},
		"p1", "orders", "public", []string{"note"})
}

// The healer records an applied drop_column as history and files its own note as the
// DDL, the text the batch detector files too. So the event carries no statement at
// all: a DROP COLUMN on the topic is one a consumer could run.
func TestDroppedColumnEventShape(t *testing.T) {
	msgs := appliedSchemaChangeMessages("p1", qualifiedDriftTable("orders", "public"), "public",
		[]map[string]interface{}{{"change_type": "drop_column", "column_name": "note"}})
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	var ev struct {
		PipelineID   string                 `json:"pipeline_id"`
		ActionNeeded *bool                  `json:"action_needed"`
		SchemaChange map[string]interface{} `json:"schema_change"`
	}
	if err := json.Unmarshal(msgs[0].Value, &ev); err != nil {
		t.Fatal(err)
	}
	sc := ev.SchemaChange
	if ev.PipelineID != "p1" || string(msgs[0].Key) != "p1" {
		t.Errorf("pipeline id = %q, key = %q, want p1", ev.PipelineID, msgs[0].Key)
	}
	if sc["change_type"] != "drop_column" || sc["column_name"] != "note" || sc["table"] != "public.orders" {
		t.Errorf("schema_change = %v", sc)
	}
	if sc["applied"] != true || ev.ActionNeeded == nil || *ev.ActionNeeded {
		t.Errorf("a dropped column must be filed as handled: applied=%v action_needed=%v", sc["applied"], ev.ActionNeeded)
	}
	if _, has := sc["ddl"]; has {
		t.Errorf("a dropped column event carries a statement: %v", sc["ddl"])
	}
}
