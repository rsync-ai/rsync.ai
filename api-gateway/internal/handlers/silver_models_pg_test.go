//go:build integration_pg

// Real-Postgres coverage for the silver generator: the SQL it writes is run against
// bronze tables shaped the way kafka-sink-worker writes them, and the rows that come
// back are compared with what the source table holds after the same changes.
//
// A string comparison of the SQL (silver_models_test.go) cannot tell whether the
// latest-event-wins window picks the right row, whether a delete removes the key, or
// whether a key deleted and inserted again comes back. Only running it can.
//
// Same bare postgres as saved_query_upstreams_pg_test.go:
//
//	UPSTREAM_PG_DSN='postgres://postgres:verify@localhost:55443/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Silver -v
//
// The test creates and drops its own schema, silver_pg_test.

package handlers

import (
	"fmt"
	"reflect"
	"testing"
)

// cdcSeq is the sink's _rsync_cdc_seq: fixed width, so text order is event order.
func cdcSeq(ts, lsn int64) string { return fmt.Sprintf("%016d%019d", ts, lsn) }

func TestPG_SilverHistoryMatchesTheSource(t *testing.T) {
	db := upstreamPGDB(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`DROP SCHEMA IF EXISTS silver_pg_test CASCADE`)
	mustExec(`CREATE SCHEMA silver_pg_test`)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS silver_pg_test CASCADE`) })

	// The append table the sink creates in history mode: the source's columns, then
	// rsync.ai's, and no key — every change is a row.
	mustExec(`CREATE TABLE silver_pg_test.orders (
		id integer, "Status" text, amount numeric,
		_rsync_cdc_op varchar(8), _rsync_cdc_event_ts bigint, _rsync_cdc_lsn bigint,
		_rsync_cdc_gtid varchar(255), _rsync_cdc_seq varchar(40))`)
	type event struct {
		id      int
		status  string
		amount  float64
		op      string
		ts, lsn int64
	}
	events := []event{
		// Updated across a digit-count boundary in the timestamp: only the fixed-width
		// sequence orders 999 before 1000 as text.
		{1, "new", 10, "I", 999, 1},
		{1, "paid", 12, "U", 1000, 2},
		// Deleted: the sink appends the before image with op D.
		{2, "new", 5, "I", 1001, 3},
		{2, "new", 5, "D", 1002, 4},
		// Untouched since its insert.
		{3, "new", 7, "I", 1003, 5},
		// Deleted, then inserted again: it is back.
		{4, "old", 1, "I", 1004, 6},
		{4, "old", 1, "D", 1005, 7},
		{4, "again", 2, "I", 1006, 8},
		// Two changes in one transaction share a timestamp; the LSN orders them.
		{5, "first", 1, "I", 1007, 9},
		{5, "second", 3, "U", 1007, 10},
	}
	for _, e := range events {
		mustExec(`INSERT INTO silver_pg_test.orders VALUES ($1, $2, $3, $4, $5, $6, NULL, $7)`,
			e.id, e.status, e.amount, e.op, e.ts, e.lsn, cdcSeq(e.ts, e.lsn))
	}

	src := &silverSource{Schema: "silver_pg_test", Table: "orders", Columns: historyColumns()}
	plan, refusal := buildSilverPlan("postgres", src, []string{"id"})
	if refusal != nil {
		t.Fatal(refusal)
	}
	rows, err := db.Query(`SELECT * FROM (` + plan.SQLText + `) s ORDER BY id`)
	if err != nil {
		t.Fatalf("generated SQL failed: %v\n%s", err, plan.SQLText)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !reflect.DeepEqual(cols, []string{"id", "Status", "amount"}) {
		t.Fatalf("silver columns = %v", cols)
	}
	var got []string
	for rows.Next() {
		var id int
		var status string
		var amount float64
		if err := rows.Scan(&id, &status, &amount); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s:%g", id, status, amount))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"1:paid:12", "3:new:7", "4:again:2", "5:second:3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("silver rows = %v, want the source's current rows %v", got, want)
	}
}

func TestPG_SilverSoftDeleteDropsDeletedKeys(t *testing.T) {
	db := upstreamPGDB(t)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS silver_pg_test CASCADE`,
		`CREATE SCHEMA silver_pg_test`,
		`CREATE TABLE silver_pg_test.customers (id integer, name text, _rsync_deleted boolean, _rsync_synced_at timestamptz)`,
		// NULL is a row synced before the flag existed: it is live.
		`INSERT INTO silver_pg_test.customers VALUES (1, 'a', false, now()), (2, 'b', true, now()), (3, 'c', NULL, now())`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA IF EXISTS silver_pg_test CASCADE`) })

	src := &silverSource{Schema: "silver_pg_test", Table: "customers", Columns: []silverColumn{
		{"id", "integer"}, {"name", "text"}, {"_rsync_deleted", "boolean"}, {"_rsync_synced_at", "timestamp with time zone"},
	}}
	plan, refusal := buildSilverPlan("postgres", src, nil)
	if refusal != nil {
		t.Fatal(refusal)
	}
	var ids []int
	rows, err := db.Query(`SELECT id FROM (` + plan.SQLText + `) s ORDER BY id`)
	if err != nil {
		t.Fatalf("generated SQL failed: %v\n%s", err, plan.SQLText)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if !reflect.DeepEqual(ids, []int{1, 3}) {
		t.Fatalf("live ids = %v, want [1 3]", ids)
	}
}
