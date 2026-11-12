//go:build integration

// Integration test for migration 135 (a workspace's own model layers). The
// backfill, the composite foreign key and the deferred position constraint need
// a real, disposable PostgreSQL. Same gate and helpers as
// migration_069_integration_test.go; applyMigration is the runner's own.
//
//	docker run -d --rm --name pg135 -e POSTGRES_PASSWORD=pg -p 55435:5432 postgres:16-alpine
//	MIGRATION_TEST_DSN='postgres://postgres:pg@localhost:55435/postgres?sslmode=disable' \
//	  go test -tags=integration -run TestMigration135 ./internal/db/ -v
//	docker rm -f pg135
package db

import (
	"database/sql"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/rsync-ai/shared/pgdriver"
)

func applyBefore135(t *testing.T, conn *sql.DB) {
	t.Helper()
	if _, err := conn.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(255) PRIMARY KEY, applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".sql") && n < "135" {
			files = append(files, n)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		if err := applyMigration(conn, migrationsDir, f); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// seedWorkspace135 makes a workspace with a connection, and returns both ids.
func seedWorkspace135(t *testing.T, conn *sql.DB, name string) (ws, connID, user string) {
	t.Helper()
	user = qStr(t, conn, `INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`, name+"@acme.test")
	ws = qStr(t, conn, `INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$1,$2) RETURNING id`, name, user)
	connID = qStr(t, conn, `INSERT INTO connections (user_id, workspace_id, name, type, connector_type, config)
		VALUES ($1, $2, 'pg', 'source', 'postgresql', '{}') RETURNING id`, user, ws)
	return ws, connID, user
}

// seedModel135 inserts a model with a 127 label ("" = none), last edited 2026-01-01.
func seedModel135(t *testing.T, conn *sql.DB, ws, connID, user, name, layer string) string {
	t.Helper()
	return qStr(t, conn, `INSERT INTO saved_queries (workspace_id, connection_id, name, sql_text, created_by, layer, updated_at)
		VALUES ($1, $2, $3, 'SELECT 1', $4, NULLIF($5, ''), '2026-01-01T00:00:00Z') RETURNING id`,
		ws, connID, name, user, layer)
}

// TestMigration135_BackfillAndConstraints: a workspace with any silver or gold
// model gets both layers, in order, and its models point at them without their
// edit token moving; a workspace with none gets no layers. Then the constraints
// the handlers rely on: a model cannot take another workspace's layer, a layer
// with models cannot be deleted under them, names collide ignoring case, and
// a swap of two positions commits.
func TestMigration135_BackfillAndConstraints(t *testing.T) {
	conn, cleanup := freshSchema(t, testDSN(t))
	defer cleanup()
	applyBefore135(t, conn)

	wsA, connA, userA := seedWorkspace135(t, conn, "acme")
	silverA := seedModel135(t, conn, wsA, connA, userA, "orders clean", "silver")
	goldA := seedModel135(t, conn, wsA, connA, userA, "revenue", "gold")
	plainA := seedModel135(t, conn, wsA, connA, userA, "scratch", "")
	wsB, connB, userB := seedWorkspace135(t, conn, "globex")
	goldB := seedModel135(t, conn, wsB, connB, userB, "churn", "gold")
	wsC, connC, userC := seedWorkspace135(t, conn, "initech")
	seedModel135(t, conn, wsC, connC, userC, "plain", "")

	if err := applyMigration(conn, migrationsDir, "135_workspace_layers.sql"); err != nil {
		t.Fatalf("apply 135: %v", err)
	}

	layers := func(ws string) string {
		return qStr(t, conn, `SELECT COALESCE(string_agg(name || ':' || position, ',' ORDER BY position), '')
			FROM workspace_layers WHERE workspace_id = $1`, ws)
	}
	if got := layers(wsA); got != "Silver:0,Gold:1" {
		t.Errorf("acme layers = %q, want Silver:0,Gold:1", got)
	}
	// Only a gold model, still both: the gold models were built from silver.
	if got := layers(wsB); got != "Silver:0,Gold:1" {
		t.Errorf("globex layers = %q, want Silver:0,Gold:1", got)
	}
	if got := layers(wsC); got != "" {
		t.Errorf("initech has no labelled model, so no layers; got %q", got)
	}

	layerOf := func(id string) string {
		return qStr(t, conn, `SELECT COALESCE(wl.name, '') FROM saved_queries sq
			LEFT JOIN workspace_layers wl ON wl.id = sq.layer_id WHERE sq.id = $1`, id)
	}
	for id, want := range map[string]string{silverA: "Silver", goldA: "Gold", plainA: "", goldB: "Gold"} {
		if got := layerOf(id); got != want {
			t.Errorf("model %s layer = %q, want %q", id, got, want)
		}
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM saved_queries WHERE updated_at <> '2026-01-01T00:00:00Z'`); n != 0 {
		t.Errorf("the backfill moved updated_at on %d models; it is the edit concurrency token", n)
	}
	// The trigger is back on: an edit moves updated_at.
	if _, err := conn.Exec(`UPDATE saved_queries SET name = 'revenue v2' WHERE id = $1`, goldA); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM saved_queries WHERE id = $1 AND updated_at > '2026-01-02'`, goldA); n != 1 {
		t.Error("an edit after 135 did not move updated_at: the trigger was left off")
	}

	goldLayerB := qStr(t, conn, `SELECT id FROM workspace_layers WHERE workspace_id = $1 AND name = 'Gold'`, wsB)
	_, err := conn.Exec(`UPDATE saved_queries SET layer_id = $2 WHERE id = $1`, goldA, goldLayerB)
	if pgdriver.SQLState(err) != "23503" {
		t.Errorf("a model took another workspace's layer: err %v, want foreign_key_violation", err)
	}

	goldLayerA := qStr(t, conn, `SELECT id FROM workspace_layers WHERE workspace_id = $1 AND name = 'Gold'`, wsA)
	_, err = conn.Exec(`DELETE FROM workspace_layers WHERE id = $1`, goldLayerA)
	if pgdriver.SQLState(err) != "23503" {
		t.Errorf("deleting a layer with models: err %v, want foreign_key_violation", err)
	}

	_, err = conn.Exec(`INSERT INTO workspace_layers (workspace_id, name, position) VALUES ($1, 'GOLD', 2)`, wsA)
	if pgdriver.SQLState(err) != "23505" {
		t.Errorf("a second layer named GOLD: err %v, want unique_violation", err)
	}
	_, err = conn.Exec(`INSERT INTO workspace_layers (workspace_id, name, position) VALUES ($1, ' Bronze', 2)`, wsA)
	if pgdriver.SQLState(err) != "23514" {
		t.Errorf("an untrimmed name: err %v, want check_violation", err)
	}

	tx, err := conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`UPDATE workspace_layers SET position = 1 - position WHERE workspace_id = $1`, wsA); err != nil {
		_ = tx.Rollback()
		t.Fatalf("swap positions: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("a swap of two positions did not commit: %v", err)
	}
	if got := layers(wsA); got != "Gold:0,Silver:1" {
		t.Errorf("after the swap acme layers = %q, want Gold:0,Silver:1", got)
	}

	// Deleting the workspace takes its layers with it.
	if _, err := conn.Exec(`DELETE FROM workspaces WHERE id = $1`, wsC); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM saved_queries WHERE workspace_id = $1`, wsB); err != nil {
		t.Fatalf("delete models: %v", err)
	}
	if _, err := conn.Exec(`DELETE FROM workspaces WHERE id = $1`, wsB); err != nil {
		t.Fatalf("delete a workspace with layers: %v", err)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM workspace_layers WHERE workspace_id = $1`, wsB); n != 0 {
		t.Errorf("a deleted workspace left %d layers", n)
	}
	// Models still in their layers: both cascades from workspaces, one into the
	// layers and one into the models that point at them, run in one statement.
	if _, err := conn.Exec(`DELETE FROM workspaces WHERE id = $1`, wsA); err != nil {
		t.Fatalf("delete a workspace whose models are in its layers: %v", err)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM workspace_layers WHERE workspace_id = $1`, wsA); n != 0 {
		t.Errorf("a deleted workspace left %d layers", n)
	}
}

// TestMigration135_ARerunChangesNothing: should 135 run again (a restore that
// lost its schema_migrations row), its backfill must not bring back a layer an
// admin renamed, nor put back a model they took out of one: the 127 label it
// reads is never cleared.
func TestMigration135_ARerunChangesNothing(t *testing.T) {
	conn, cleanup := freshSchema(t, testDSN(t))
	defer cleanup()
	applyBefore135(t, conn)

	ws, connID, user := seedWorkspace135(t, conn, "acme")
	seedModel135(t, conn, ws, connID, user, "orders clean", "silver")
	gold := seedModel135(t, conn, ws, connID, user, "revenue", "gold")
	if err := applyMigration(conn, migrationsDir, "135_workspace_layers.sql"); err != nil {
		t.Fatalf("apply 135: %v", err)
	}

	// What an admin does afterwards.
	if _, err := conn.Exec(`UPDATE workspace_layers SET name = 'Bronze' WHERE workspace_id = $1 AND name = 'Silver'`, ws); err != nil {
		t.Fatalf("rename Silver: %v", err)
	}
	if _, err := conn.Exec(`UPDATE saved_queries SET layer_id = NULL WHERE id = $1`, gold); err != nil {
		t.Fatalf("take a model out of Gold: %v", err)
	}

	if _, err := conn.Exec(`DELETE FROM schema_migrations WHERE version = '135_workspace_layers.sql'`); err != nil {
		t.Fatalf("forget 135: %v", err)
	}
	if err := applyMigration(conn, migrationsDir, "135_workspace_layers.sql"); err != nil {
		t.Fatalf("135 failed when it ran again: %v", err)
	}
	if got := qStr(t, conn, `SELECT string_agg(name || ':' || position, ',' ORDER BY position)
		FROM workspace_layers WHERE workspace_id = $1`, ws); got != "Bronze:0,Gold:1" {
		t.Errorf("after 135 ran again the layers are %q, want Bronze:0,Gold:1", got)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM saved_queries WHERE id = $1 AND layer_id IS NOT NULL`, gold); n != 0 {
		t.Error("135 ran again and put a model back in the layer an admin took it out of")
	}
}

// TestMigration135_FreshBoot: a full Migrate() on an empty database.
func TestMigration135_FreshBoot(t *testing.T) {
	conn, cleanup := freshSchema(t, testDSN(t))
	defer cleanup()
	if err := Migrate(migrationsDir); err != nil {
		t.Fatalf("full Migrate() including 135 failed on a fresh DB: %v", err)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM workspace_layers`); n != 0 {
		t.Errorf("a fresh database has %d layers, want 0", n)
	}
}
