//go:build integration_pg

// Saved column lineage against a real Postgres, through migration 152 itself: the
// upsert's conflict target, the workspace check in its SELECT, the cleanup of an
// edited model's old SQL (kept while a lineage event names it), and the text-array
// read loadColumnLineage does. Same
// scratch database as saved_query_upstreams_pg_test.go — see that file's header:
//
//	UPSTREAM_PG_DSN='postgres://postgres:verify@localhost:55443/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ColumnLineage -v

package handlers

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

const (
	clWS    = "c0000000-0000-4000-8000-000000000001"
	clOther = "c0000000-0000-4000-8000-000000000002"
	clModel = "c0000000-0000-4000-8000-0000000000a1"
	clConn  = "c0000000-0000-4000-8000-0000000000c1"
)

func TestPG_ColumnLineageSavesAndLoads(t *testing.T) {
	db := upstreamPGDB(t)
	ctx := context.Background()
	const drop = `DROP TABLE IF EXISTS model_column_lineage, lineage_events, saved_queries`
	_, _ = db.ExecContext(ctx, drop)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, drop) })

	mustExec(t, db, `CREATE TABLE saved_queries (id UUID PRIMARY KEY, workspace_id UUID NOT NULL)`)
	// The columns of migration 153's table that the cleanup reads.
	mustExec(t, db, `CREATE TABLE lineage_events (saved_query_id UUID, sql_hash TEXT NOT NULL DEFAULT '')`)
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "152_model_column_lineage.sql"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, string(migration))
	mustExec(t, db, `INSERT INTO saved_queries (id, workspace_id) VALUES ($1, $2)`, clModel, clWS)

	stubColumnLineageRefresh(t, true)
	model := modelAsset{ID: clModel, ConnectorType: "postgresql", ConnectionID: clConn, SQLText: "SELECT a FROM t"}
	job := columnLineageJob{ModelID: clModel, SQL: model.SQLText, ConnectorType: model.ConnectorType, Hash: columnLineageSQLHash(model.ConnectorType, model.SQLText)}
	answer := columnLineageAnswer{ID: clModel, Status: "partial", Reason: "why", Columns: []string{"a"}, Edges: []columnLineageEdge{
		{Table: []string{"t"}, Column: "a", ToColumn: "a", Type: "DIRECT", Subtype: "IDENTITY"},
	}}

	// Another workspace's id saves nothing: the SELECT finds no model.
	if err := saveColumnLineage(ctx, db, clOther, columnLineageSent{job: job, hadSchema: true}, answer); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM model_column_lineage`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a save under another workspace wrote %d rows (err %v)", n, err)
	}

	// Saved twice under one SQL: one row, the second answer.
	if err := saveColumnLineage(ctx, db, clWS, columnLineageSent{job: job, schemaHash: "h1", hadSchema: false}, columnLineageAnswer{ID: clModel, Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if err := saveColumnLineage(ctx, db, clWS, columnLineageSent{job: job, schemaHash: "h2", hadSchema: true}, answer); err != nil {
		t.Fatal(err)
	}
	got, err := loadColumnLineage(ctx, db, clWS, []modelAsset{model})
	if err != nil {
		t.Fatal(err)
	}
	want := savedColumnLineage{Status: "partial", Reason: "why", Columns: []string{"a"}, Edges: answer.Edges}
	if !reflect.DeepEqual(got.ByModel[clModel], want) || got.Pending {
		t.Fatalf("loaded %+v pending %v, want %+v", got.ByModel[clModel], got.Pending, want)
	}

	// An edit: the new SQL's row replaces the old one.
	edited := model
	edited.SQLText = "SELECT b FROM t"
	job.SQL, job.Hash = edited.SQLText, columnLineageSQLHash(edited.ConnectorType, edited.SQLText)
	if err := saveColumnLineage(ctx, db, clWS, columnLineageSent{job: job, hadSchema: true}, columnLineageAnswer{ID: clModel, Status: "ok", Columns: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	var hashes []string
	rows, err := db.QueryContext(ctx, `SELECT sql_hash FROM model_column_lineage`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	if !reflect.DeepEqual(hashes, []string{job.Hash}) {
		t.Fatalf("rows after an edit: %v, want only %s", hashes, job.Hash)
	}

	// A version a recorded run names outlives the next edit: its history shows its columns.
	ran := job.Hash
	mustExec(t, db, `INSERT INTO lineage_events (saved_query_id, sql_hash) VALUES ($1, $2)`, clModel, ran)
	job.SQL = "SELECT c FROM t"
	job.Hash = columnLineageSQLHash(model.ConnectorType, job.SQL)
	if err := saveColumnLineage(ctx, db, clWS, columnLineageSent{job: job, hadSchema: true}, columnLineageAnswer{ID: clModel, Status: "ok", Columns: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	hashes = nil
	rows, err = db.QueryContext(ctx, `SELECT sql_hash FROM model_column_lineage ORDER BY sql_hash`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	kept := []string{ran, job.Hash}
	slices.Sort(kept)
	if !reflect.DeepEqual(hashes, kept) {
		t.Fatalf("rows after an edit of a version that ran: %v, want %v", hashes, kept)
	}

	// A deleted model takes its lineage with it.
	mustExec(t, db, `DELETE FROM saved_queries WHERE id = $1`, clModel)
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM model_column_lineage`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows outlived their model (err %v)", n, err)
	}
}
