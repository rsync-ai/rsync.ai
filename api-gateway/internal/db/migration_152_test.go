package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Static guard for migration 152: the saved column lineage that
// handlers/asset_column_lineage.go reads and writes. Its upsert names the primary key
// as the conflict target, and saveColumnLineage maps any unknown status to 'failed',
// so the key and the CHECK here have to stay as the handler expects.

func TestMigration152_ColumnLineageTable(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "152_*.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one 152_*.sql migration, found %v (err %v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	body := normSQL(stripSQLComments(string(b)))
	for _, want := range []string{
		"create table if not exists model_column_lineage (",
		// A deleted model takes its lineage with it.
		"saved_query_id uuid not null references saved_queries(id) on delete cascade,",
		"workspace_id uuid not null,",
		"check (status in ('ok', 'partial', 'failed', 'unsupported'))",
		"result jsonb not null default '{}',",
		"had_schema boolean not null default false,",
		// ON CONFLICT (saved_query_id, sql_hash) in saveColumnLineage.
		"primary key (saved_query_id, sql_hash)",
		// loadColumnLineage reads by workspace.
		"create index if not exists idx_model_column_lineage_workspace on model_column_lineage (workspace_id);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migration 152 lost %q", want)
		}
	}
}
