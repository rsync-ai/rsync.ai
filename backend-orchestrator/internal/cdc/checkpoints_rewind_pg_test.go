//go:build integration_pg

// Real-PostgreSQL proof for the rewind record (migration 141).
//
// checkpoints_rewind_test.go pins the statements. sqlmock cannot judge the two things
// that matter here: that the INSERT … SELECT type-checks against the real columns
// (uuid pipeline_id, text execution_id), and that a failed record really leaves the
// rewind's transaction usable — without the savepoint, Postgres aborts the whole
// transaction on the first error and the rewind itself would be lost.
//
//	docker run -d --name rewind-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55471:5432 postgres:16-alpine
//	for m in api-gateway/migrations/*.sql; do
//	    docker exec -i rewind-pg psql -U postgres -d pipeline_db -v ON_ERROR_STOP=1 -q < "$m"
//	done
//	REWIND_PG_DSN='postgres://postgres:verify@localhost:55471/pipeline_db?sslmode=disable' \
//	    go test -tags integration_pg ./internal/cdc/ -run PGRewind -v
package cdc

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"testing"

	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgRewindUserID      = "cccccccc-0000-4000-8000-000000000001"
	pgRewindWorkspaceID = "cccccccc-0000-4000-8000-000000000002"
	pgRewindPipelineID  = "cccccccc-0000-4000-8000-000000000003"
	pgRewindConnID      = "cccccccc-0000-4000-8000-000000000004"
	pgRewindExecID      = "cccccccc-0000-4000-8000-000000000005"
	pgRewindOtherExecID = "cccccccc-0000-4000-8000-000000000006"
)

func pgRewindDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("REWIND_PG_DSN")
	if dsn == "" {
		t.Skip("REWIND_PG_DSN not set — see the file header for the commands that provide one")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pgSeedRewind seeds one run's checkpoints: list_task with a run_start to restore,
// list_comment first read by this run (run_start {}, so the rewind deletes it), and
// space last moved by another run, which this rewind must not touch.
func pgSeedRewind(t *testing.T, db *sql.DB) {
	t.Helper()
	purge := func() {
		// Ordered by FK: checkpoints and rewinds cascade from the pipeline.
		for _, d := range []struct{ table, id string }{
			{"pipelines", pgRewindPipelineID},
			{"connections", pgRewindConnID},
			{"workspaces", pgRewindWorkspaceID},
			{"users", pgRewindUserID},
		} {
			if _, err := db.Exec(`DELETE FROM `+d.table+` WHERE id = $1::uuid`, d.id); err != nil {
				t.Fatalf("purge %s: %v", d.table, err)
			}
		}
	}
	purge()
	t.Cleanup(purge)
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed %.70q: %v", q, err)
		}
	}
	mustExec(`INSERT INTO users (id, email, password_hash) VALUES ($1::uuid, 'rewind-verify@example.invalid', 'x')`, pgRewindUserID)
	mustExec(`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1::uuid, 'rewind', 'rewind-verify', $2::uuid)`,
		pgRewindWorkspaceID, pgRewindUserID)
	mustExec(`INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id)
	          VALUES ($1::uuid, $2::uuid, 'rewind src', 'source', 't-clickup', '{}', $3::uuid)`,
		pgRewindConnID, pgRewindUserID, pgRewindWorkspaceID)
	mustExec(`INSERT INTO pipelines (id, name, natural_language_request, workspace_id, status, sync_mode, created_by)
	          VALUES ($1::uuid, 'rewind verify', 'verify', $2::uuid, 'running', 'batch', $3::uuid)`,
		pgRewindPipelineID, pgRewindWorkspaceID, pgRewindUserID)
	for _, cp := range []struct{ table, pos string }{
		{"list_task", `{"execution_id":"` + pgRewindExecID + `","cursor":"p13","run_start":{"cursor":"p10"}}`},
		{"list_comment", `{"execution_id":"` + pgRewindExecID + `","cursor":"p2","run_start":{}}`},
		{"space", `{"execution_id":"` + pgRewindOtherExecID + `","cursor":"p1","run_start":{}}`},
	} {
		mustExec(`INSERT INTO pipeline_checkpoints (pipeline_id, connection_id, source_table, position)
		          VALUES ($1::uuid, $2::uuid, $3, $4::jsonb)`, pgRewindPipelineID, pgRewindConnID, cp.table, cp.pos)
	}
}

func pgRewoundTables(t *testing.T, db *sql.DB, executionID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT source_table FROM pipeline_run_rewinds WHERE pipeline_id = $1 AND execution_id = $2`,
		pgRewindPipelineID, executionID)
	if err != nil {
		t.Fatalf("read rewinds: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
	}
	sort.Strings(got)
	return got
}

// pgCheckpoints maps each remaining checkpoint to its position's cursor.
func pgCheckpoints(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT source_table, COALESCE(position->>'cursor', '') FROM pipeline_checkpoints WHERE pipeline_id = $1`,
		pgRewindPipelineID)
	if err != nil {
		t.Fatalf("read checkpoints: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var table, cursor string
		if err := rows.Scan(&table, &cursor); err != nil {
			t.Fatal(err)
		}
		got[table] = cursor
	}
	return got
}

func assertRewound(t *testing.T, db *sql.DB, n int64) {
	t.Helper()
	if n != 2 {
		t.Fatalf("rewound %d checkpoints, want 2 (list_task restored, list_comment deleted)", n)
	}
	cps := pgCheckpoints(t, db)
	if cps["list_task"] != "p10" {
		t.Errorf("list_task is at %q, want its run_start p10", cps["list_task"])
	}
	if _, ok := cps["list_comment"]; ok {
		t.Error("list_comment's checkpoint survived; this run first read it, so the rewind deletes it")
	}
	if cps["space"] != "p1" {
		t.Errorf("space is at %q; another run's checkpoint must not move", cps["space"])
	}
}

func TestPGRewindRecordsTheTablesItRewinds(t *testing.T) {
	db := pgRewindDB(t)
	pgSeedRewind(t, db)

	n, err := RewindCheckpointsOfExecution(context.Background(), db, pgRewindPipelineID, pgRewindExecID)
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	assertRewound(t, db, n)
	got := pgRewoundTables(t, db, pgRewindExecID)
	if len(got) != 2 || got[0] != "list_comment" || got[1] != "list_task" {
		t.Fatalf("recorded %v, want [list_comment list_task] — the tables this run rewound, not space", got)
	}
	if other := pgRewoundTables(t, db, pgRewindOtherExecID); len(other) != 0 {
		t.Fatalf("recorded %v under the other run", other)
	}
}

func TestPGRewindStillRewindsWhenTheRecordFails(t *testing.T) {
	db := pgRewindDB(t)
	pgSeedRewind(t, db)
	// An orchestrator deployed before the gateway applied migration 141: the
	// record's table is not there. Hide it for this test only.
	if _, err := db.Exec(`ALTER TABLE pipeline_run_rewinds RENAME TO pipeline_run_rewinds_hidden`); err != nil {
		t.Fatalf("hide table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`ALTER TABLE pipeline_run_rewinds_hidden RENAME TO pipeline_run_rewinds`); err != nil {
			t.Errorf("restore table: %v", err)
		}
	})

	n, err := RewindCheckpointsOfExecution(context.Background(), db, pgRewindPipelineID, pgRewindExecID)
	if err != nil {
		t.Fatalf("the rewind failed because its record did (no savepoint?): %v", err)
	}
	assertRewound(t, db, n)
}
