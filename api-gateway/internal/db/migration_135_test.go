package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Static guards for migration 135 (a workspace's own model layers). Its
// backfill and constraints run against a real PostgreSQL in
// migration_135_integration_test.go (integration tag, not built by CI); these
// pin, in every run, what handlers/workspace_layers.go relies on.

func readMigration135(t *testing.T) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", "135_*.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one 135_*.sql migration, found %v (err %v)", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	return normSQL(stripSQLComments(string(b)))
}

func TestMigration135_AModelOnlyEverPointsAtALayerOfItsOwnWorkspace(t *testing.T) {
	body := readMigration135(t)
	// A foreign key on layer_id alone would let a model in one workspace carry
	// another workspace's layer; the composite key needs the (workspace_id, id)
	// unique constraint as its target.
	for _, want := range []string{
		"constraint workspace_layers_workspace_id_id_key unique (workspace_id, id)",
		"foreign key (workspace_id, layer_id) references workspace_layers (workspace_id, id);",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migration 135 lost %q", want)
		}
	}
	// SET NULL on a composite key nulls workspace_id too before PostgreSQL 15,
	// and CASCADE would delete the models. The delete handler clears them.
	if strings.Contains(body, "references workspace_layers (workspace_id, id) on delete") {
		t.Error("saved_queries_layer_id_fkey must have no ON DELETE action: DeleteWorkspaceLayer clears the models first")
	}
}

func TestMigration135_NamesAndPositionsAreUniquePerWorkspace(t *testing.T) {
	body := readMigration135(t)
	for _, want := range []string{
		// The handlers answer 409 on this index, ignoring case as model names do.
		"create unique index if not exists uq_workspace_layers_name on workspace_layers (workspace_id, lower(name));",
		// A reorder rewrites every position in one transaction; checked per row,
		// the first swap would collide.
		"constraint workspace_layers_position_key unique (workspace_id, position) deferrable initially deferred",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("migration 135 lost %q", want)
		}
	}
}

func TestMigration135_BackfillLeavesTheEditTokenAlone(t *testing.T) {
	body := readMigration135(t)
	disable := strings.Index(body, "alter table saved_queries disable trigger update_saved_queries_updated_at;")
	link := strings.Index(body, "update saved_queries sq set layer_id = wl.id")
	enable := strings.Index(body, "alter table saved_queries enable trigger update_saved_queries_updated_at;")
	if disable < 0 || link < 0 || enable < 0 {
		t.Fatalf("migration 135 lost its trigger-off backfill (disable %d, link %d, enable %d)", disable, link, enable)
	}
	// updated_at is the edit concurrency token: the link UPDATE must run with
	// the trigger off, and the trigger must come back on.
	if !(disable < link && link < enable) {
		t.Errorf("the backfill UPDATE must sit between DISABLE and ENABLE TRIGGER (disable %d, link %d, enable %d)", disable, link, enable)
	}
	if !strings.Contains(body, "and lower(wl.name) = sq.layer") {
		t.Error("the backfill must link a model to the layer named after its 127 label")
	}
}

func TestMigration135_BackfillRunsOnce(t *testing.T) {
	body := readMigration135(t)
	// Run again, the backfill would put "Silver" back at position 0 beside the
	// layer an admin renamed there, and fail at COMMIT, leaving /ready at 503.
	// So it sits behind the guard that returns once layer_id exists.
	guard := strings.Index(body, "where attrelid = 'saved_queries'::regclass and attname = 'layer_id' and not attisdropped ) then return; end if;")
	add := strings.Index(body, "alter table saved_queries add column layer_id uuid;")
	insert := strings.Index(body, "insert into workspace_layers")
	link := strings.Index(body, "update saved_queries sq set layer_id = wl.id")
	end := strings.Index(body, "alter table saved_queries enable trigger update_saved_queries_updated_at; end $$;")
	if guard < 0 || add < 0 || insert < 0 || link < 0 || end < 0 {
		t.Fatalf("migration 135 lost its run-once backfill (guard %d, add %d, insert %d, link %d, end %d)", guard, add, insert, link, end)
	}
	if !(guard < add && add < insert && insert < link && link < end) {
		t.Errorf("the column, the layers and the links must all sit inside the run-once block (guard %d, add %d, insert %d, link %d, end %d)",
			guard, add, insert, link, end)
	}
	if strings.Count(body, "insert into workspace_layers") != 1 || strings.Count(body, "set layer_id") != 1 {
		t.Error("migration 135 writes layers or links models outside its run-once block")
	}
}

func TestMigration135_RunsInsideTheRunnersTransaction(t *testing.T) {
	body := readMigration135(t)
	// The runner wraps a file in a transaction unless it holds BEGIN; a
	// half-applied 135 would leave the trigger off.
	if strings.Contains(body, "begin;") || strings.Contains(body, "commit;") || strings.Contains(body, "concurrently") {
		t.Error("migration 135 must run inside the runner's transaction")
	}
	if !strings.Contains(body, "set local lock_timeout = '3s';") {
		t.Error("migration 135 locks saved_queries and must bound the wait with lock_timeout")
	}
}
