package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Static guard for migration 154: a model run that was skipped read and wrote nothing,
// and handlers/lineage_events.go lineageEventNamesTables records it so. The migration
// puts the events recorded before that in the same state, and keeps their sql_hash (the
// version's column lineage lives while an event names it: saveColumnLineage). The
// behaviour itself is proved against Postgres in handlers/lineage_events_pg_test.go.

func TestMigration154_SkippedModelRunsNameNoTables(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "154_*.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one 154_*.sql migration, found %v (err %v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	body := normSQL(stripSQLComments(string(b)))
	for _, want := range []string{
		"update lineage_events",
		"set inputs = '[]'::jsonb, outputs = '[]'::jsonb, table_keys = '{}'",
		// Only a model run's skipped event, and only one that still names a table.
		"where job_kind = 'model' and event_type = 'abort'",
		"(inputs <> '[]'::jsonb or outputs <> '[]'::jsonb or cardinality(table_keys) > 0)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migration 154 lost %q", want)
		}
	}
	// The version stays: dropping it would let the column-lineage cleanup delete the
	// columns of a SQL version a skipped run was the last to name.
	if strings.Contains(body, "sql_hash") {
		t.Error("migration 154 touches sql_hash")
	}
	// Nothing but the UPDATE: no DDL, no delete.
	for _, bad := range []string{"drop ", "delete ", "alter ", "truncate "} {
		if strings.Contains(body, bad) {
			t.Errorf("migration 154 contains %q", bad)
		}
	}
}
