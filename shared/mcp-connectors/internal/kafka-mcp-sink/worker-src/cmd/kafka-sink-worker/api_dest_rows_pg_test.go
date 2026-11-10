package main

// Real-PostgreSQL coverage for the key map (pgAPIRecords). api_dest_rows_test.go
// drives the writer with an in-memory map, so it cannot show that the SQL runs
// against migration 144 as written.
//
// Skipped unless API_RECORDS_PG_DSN points at a scratch database with the
// api-gateway migrations applied (it deletes and re-seeds its own pipeline):
//
//	docker run -d --name api-rec-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55452:5432 postgres:16
//	for m in api-gateway/migrations/*.sql; do
//	    docker exec -i api-rec-pg psql -U postgres -d pipeline_db -q < "$m"
//	done
//	API_RECORDS_PG_DSN='postgres://postgres:verify@localhost:55452/pipeline_db?sslmode=disable' \
//	    go test ./cmd/kafka-sink-worker/ -run PG_APIRecords -v
//	docker rm -f api-rec-pg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

const (
	pgARUserID      = "66666666-0000-4000-8000-000000000001"
	pgARWorkspaceID = "66666666-0000-4000-8000-000000000002"
	pgARPipelineID  = "66666666-0000-4000-8000-000000000003"
)

func pgARDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	dsn := os.Getenv("API_RECORDS_PG_DSN")
	if dsn == "" {
		t.Skip("API_RECORDS_PG_DSN not set; see the top of api_dest_rows_pg_test.go")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`DELETE FROM pipeline_batch_acks WHERE pipeline_id = $1`, pgARPipelineID)
	exec(`DELETE FROM pipelines WHERE id = $1`, pgARPipelineID)
	exec(`INSERT INTO users (id, email, password_hash, name)
		VALUES ($1, 'api-records-probe@example.invalid', 'x', 'api records probe')
		ON CONFLICT (id) DO NOTHING`, pgARUserID)
	exec(`INSERT INTO workspaces (id, name, slug, owner_id)
		VALUES ($1, 'api records probe', 'api-records-probe', $2)
		ON CONFLICT (id) DO NOTHING`, pgARWorkspaceID, pgARUserID)
	exec(`INSERT INTO pipelines (id, name, natural_language_request, status, workspace_id)
		VALUES ($1, 'api records probe', 'probe', 'active', $2)`, pgARPipelineID, pgARWorkspaceID)
	// The key map retries a failed statement until the worker stops; a bad
	// statement here shows as this deadline, with each failure logged.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return db, ctx
}

func TestPG_APIRecords_KeyMapRoundTrip(t *testing.T) {
	db, ctx := pgARDB(t)
	store := pgAPIRecords{db: db}
	s := apiRecordScope{pipelineID: pgARPipelineID, sourceTable: "public.users", destTable: "tasks"}
	other := apiRecordScope{pipelineID: pgARPipelineID, sourceTable: "public.orders", destTable: "tasks"}

	if err := store.save(ctx, s, map[string]string{"k:a": "rec-a", "k:b": "rec-b", "k:c": "rec-c"}, "exec-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.save(ctx, other, map[string]string{"k:a": "rec-other"}, "exec-1"); err != nil {
		t.Fatal(err)
	}
	// A save of a key it holds replaces the id (the API answered a new one).
	if err := store.save(ctx, s, map[string]string{"k:c": "rec-c2"}, "exec-1"); err != nil {
		t.Fatal(err)
	}

	got, err := store.lookup(ctx, s, []string{"k:a", "k:c", "k:missing"}, "exec-2")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "map[k:a:rec-a k:c:rec-c2]" {
		t.Fatalf("lookup = %v, want k:a and k:c (its newer id), nothing for k:missing", got)
	}

	// A record the destination wrote with no id is forgotten (apiRowWriter).
	if err := store.forget(ctx, s, []string{"k:b"}); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, err := db.Query(`SELECT source_table || '/' || source_key || '=' || record_id || '@' || COALESCE(last_execution_id, '')
		FROM api_destination_records WHERE pipeline_id = $1`, pgARPipelineID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	sort.Strings(keys)
	want := "[public.orders/k:a=rec-other@exec-1 public.users/k:a=rec-a@exec-2 public.users/k:c=rec-c2@exec-2]"
	if fmt.Sprint(keys) != want {
		t.Fatalf("table holds %v, want %v", keys, want)
	}

	// Deleting the pipeline deletes its map.
	if _, err := db.Exec(`DELETE FROM pipelines WHERE id = $1`, pgARPipelineID); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = db.QueryRow(`SELECT COUNT(*) FROM api_destination_records WHERE pipeline_id = $1`, pgARPipelineID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d records left after the pipeline was deleted, want 0", left)
	}
}
