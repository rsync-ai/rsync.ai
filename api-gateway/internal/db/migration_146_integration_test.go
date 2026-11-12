//go:build integration

// Integration tests for migration 146 and the batch-ack janitor
// (KI-CDC-ACK-LEDGER-UNBOUNDED): what the fold counts, which batch runs the prune
// may delete, and that two janitors at once fold every row exactly once. These are
// engine semantics (data-modifying CTEs, ON CONFLICT, SKIP LOCKED), so they run
// against a REAL, disposable PostgreSQL. Same gate as the 069 tests: the
// `integration` build tag plus MIGRATION_TEST_DSN (or TEST_DATABASE_URL); without a
// DSN every test skips. The helpers (testDSN, freshSchema, qInt, qStr, qBool,
// migrationsDir) live in migration_069_integration_test.go.
//
//	docker run -d --rm --name pg146 -e POSTGRES_PASSWORD=pg -p 55434:5432 postgres:16-alpine
//	MIGRATION_TEST_DSN='postgres://postgres:pg@localhost:55434/postgres?sslmode=disable' \
//	  go test -tags=integration -run 'TestMigration146' ./internal/db/ -v
//	docker rm -f pg146
package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"api-gateway/internal/retention"
)

// sinkCounterSeedQuery is the kafka-sink's restart seed (cdc_counter_ledger.go
// cdcCounterSeedQuery; the sink is a Go module of its own): stored totals plus the
// per-event rows the janitor has not folded yet. The fold must leave the counts it
// seeds (ops c / r / u / d) unchanged — that is what keeps the UI totals right
// across the fold.
const sinkCounterSeedQuery = `
	SELECT table_name, op, SUM(n)::bigint FROM (
		SELECT table_name, op, total AS n
		FROM cdc_counter_totals
		WHERE pipeline_id = $1 AND execution_id = $2
		UNION ALL
		SELECT table_name, LOWER(TRIM(COALESCE(cdc_op, ''))), COUNT(*)
		FROM pipeline_batch_acks
		WHERE pipeline_id = $1 AND execution_id = $2
		  AND storage_type = 'cdc' AND last_error IS NULL
		GROUP BY 1, 2
	) s GROUP BY 1, 2`

func janitorTestCfg(retentionDays, chunk int) retention.BatchAckJanitorConfig {
	return retention.BatchAckJanitorConfig{RetentionDays: retentionDays, Chunk: chunk, Budget: time.Minute}
}

func migrate146(t *testing.T) *sql.DB {
	t.Helper()
	conn, cleanup := freshSchema(t, testDSN(t))
	t.Cleanup(cleanup)
	if err := Migrate(migrationsDir); err != nil {
		t.Fatalf("full Migrate() including 146 failed on a fresh DB: %v", err)
	}
	return conn
}

func mustExec146(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("%v\n%s", err, q)
	}
}

type ack146 struct {
	exec, table, storage string
	op                   any // string or nil
	topic                string
	partition            int
	offset               int64
	lastErr              any // string or nil
	ackedAgo             time.Duration
}

func seedAcks146(t *testing.T, conn *sql.DB, pipeline string, acks ...ack146) {
	t.Helper()
	for _, a := range acks {
		mustExec146(t, conn, `
			INSERT INTO pipeline_batch_acks (pipeline_id, execution_id, table_name, batch_offset,
				storage_type, cdc_op, kafka_topic, kafka_partition, kafka_offset, last_error, acked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW() - make_interval(secs => $11))`,
			pipeline, a.exec, a.table, a.offset, a.storage, a.op, a.topic, a.partition, a.offset, a.lastErr,
			a.ackedAgo.Seconds())
	}
}

func seedTotals146(t *testing.T, conn *sql.DB, pipeline, execution string) map[string]int64 {
	t.Helper()
	rows, err := conn.Query(sinkCounterSeedQuery, pipeline, execution)
	if err != nil {
		t.Fatalf("sink seed query: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var table, op string
		var n int64
		if err := rows.Scan(&table, &op, &n); err != nil {
			t.Fatal(err)
		}
		switch op {
		case "c", "r", "u", "d": // the sink seeds these four and skips any other op
			out[table+"/"+op] = n
		}
	}
	return out
}

func newCDCPipeline146(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	p := seedPipeline106(t, conn, name)
	// The sink's anchor: the CDC execution's id is the pipeline's.
	mustExec146(t, conn, `INSERT INTO executions (id, pipeline_id, status) VALUES ($1, $1, 'running')`, p)
	return p
}

// TestMigration146_FoldMovesEveryRowOnceAndKeepsTheSeed: per-event CDC rows move
// into the totals and floors exactly as the sink's seed counted them, failed rows
// raise a floor but count nothing, rows from the last hour and batch rows stay, and
// a second pass changes nothing.
func TestMigration146_FoldMovesEveryRowOnceAndKeepsTheSeed(t *testing.T) {
	conn := migrate146(t)
	p := newCDCPipeline146(t, conn, "cdcfold")
	batchExec := qStr(t, conn, `INSERT INTO executions (pipeline_id, status) VALUES ($1, 'running') RETURNING id`, p)

	old := 2 * time.Hour
	seedAcks146(t, conn, p,
		ack146{p, "public.orders", "cdc", "c", "t", 0, 1, nil, old},
		ack146{p, "public.orders", "cdc", "c", "t", 0, 2, nil, old},
		ack146{p, "public.orders", "cdc", "r", "t", 0, 3, nil, old},
		ack146{p, "public.orders", "cdc", "u", "t", 0, 4, nil, old},
		ack146{p, "public.orders", "cdc", "d", "t", 0, 5, nil, old},
		ack146{p, "public.orders", "cdc", " C ", "t", 0, 6, nil, old}, // normalised like the seed
		ack146{p, "public.orders", "cdc", "r", "t", 0, 7, nil, old},
		ack146{p, "public.orders", "cdc", nil, "t", 0, 9, nil, old},              // no op: no count, raises the floor
		ack146{p, "public.orders", "cdc", "u", "t", 1, 3, nil, old},              // a second partition
		ack146{p, "public.items", "cdc", "c", "t", 0, 20, "boom", old},           // never landed: floor only
		ack146{p, "public.orders", "cdc", "c", "t", 0, 10, nil, 5 * time.Minute}, // too recent to fold
		ack146{batchExec, "public.orders", "inline", nil, "b", 0, 1, nil, old},   // batch: not the fold's
	)
	// Totals and a floor already stored by a 146 sink: the fold adds to the one and
	// never lowers the other.
	mustExec146(t, conn, `INSERT INTO cdc_counter_totals (pipeline_id, execution_id, table_name, op, total) VALUES ($1, $1, 'public.orders', 'c', 100)`, p)
	mustExec146(t, conn, `INSERT INTO cdc_offset_floors (pipeline_id, execution_id, table_name, kafka_topic, kafka_partition, max_offset) VALUES ($1, $1, 'public.orders', 't', 0, 50)`, p)

	before := seedTotals146(t, conn, p, p)

	res := retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(0, 3))
	if res.FoldErr != nil {
		t.Fatalf("fold: %v", res.FoldErr)
	}
	if res.FoldedCDCRows != 10 {
		t.Errorf("folded %d rows, want 10 (every CDC row older than an hour, in chunks of 3)", res.FoldedCDCRows)
	}

	after := seedTotals146(t, conn, p, p)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("the sink's seed changed across the fold:\n before %v\n after  %v", before, after)
	}
	wantTotals := map[string]int64{"c": 103, "r": 2, "u": 2, "d": 1}
	for op, want := range wantTotals {
		got := qInt(t, conn, `SELECT COALESCE(SUM(total), 0) FROM cdc_counter_totals WHERE pipeline_id = $1 AND table_name = 'public.orders' AND op = $2`, p, op)
		if int64(got) != want {
			t.Errorf("cdc_counter_totals orders/%s = %d, want %d", op, got, want)
		}
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM cdc_counter_totals WHERE table_name = 'public.items'`); n != 0 {
		t.Errorf("a failed ack was counted: %d totals rows for items", n)
	}
	for _, f := range []struct {
		table     string
		partition int
		want      int
	}{
		{"public.orders", 0, 50}, // the stored floor was higher; GREATEST keeps it
		{"public.orders", 1, 3},
		{"public.items", 0, 20}, // a failed row's offset still reads as a replay
	} {
		got := qInt(t, conn, `SELECT max_offset FROM cdc_offset_floors WHERE pipeline_id = $1 AND table_name = $2 AND kafka_topic = 't' AND kafka_partition = $3`, p, f.table, f.partition)
		if got != f.want {
			t.Errorf("floor %s/%d = %d, want %d", f.table, f.partition, got, f.want)
		}
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM pipeline_batch_acks WHERE storage_type = 'cdc'`); n != 1 {
		t.Errorf("%d CDC rows left, want 1 (the recent one)", n)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM pipeline_batch_acks WHERE storage_type = 'inline'`); n != 1 {
		t.Errorf("the fold touched batch rows: %d left, want 1", n)
	}

	// Idempotent: nothing old is left to move.
	again := retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(0, 3))
	if again.FoldErr != nil || again.FoldedCDCRows != 0 {
		t.Errorf("second pass folded %d (err %v), want 0", again.FoldedCDCRows, again.FoldErr)
	}
	if fmt.Sprint(seedTotals146(t, conn, p, p)) != fmt.Sprint(before) {
		t.Error("a second pass changed the seed")
	}

	// The tables follow their pipeline, and op is one of the four the sink counts.
	if _, err := conn.Exec(`INSERT INTO cdc_counter_totals (pipeline_id, execution_id, table_name, op, total) VALUES ($1, $1, 'x', 'x', 1)`, p); err == nil {
		t.Error("op 'x' was accepted, want the CHECK to reject it")
	}
	mustExec146(t, conn, `DELETE FROM pipeline_batch_acks WHERE pipeline_id = $1`, p)
	mustExec146(t, conn, `DELETE FROM executions WHERE pipeline_id = $1`, p)
	mustExec146(t, conn, `DELETE FROM pipelines WHERE id = $1`, p)
	if n := qInt(t, conn, `SELECT (SELECT COUNT(*) FROM cdc_counter_totals) + (SELECT COUNT(*) FROM cdc_offset_floors)`); n != 0 {
		t.Errorf("%d counter rows outlived their pipeline", n)
	}
}

// TestMigration146_TwoJanitorsFoldEachRowOnce: two api-gateway replicas folding the
// same backlog at once count every row exactly once (FOR UPDATE SKIP LOCKED).
func TestMigration146_TwoJanitorsFoldEachRowOnce(t *testing.T) {
	conn := migrate146(t)
	p := newCDCPipeline146(t, conn, "cdcrace")
	const rows = 3000
	mustExec146(t, conn, `
		INSERT INTO pipeline_batch_acks (pipeline_id, execution_id, table_name, batch_offset,
			storage_type, cdc_op, kafka_topic, kafka_partition, kafka_offset, acked_at)
		SELECT $1, $1, 'public.orders', g, 'cdc', 'c', 't', g % 3, g, NOW() - INTERVAL '2 hours'
		FROM generate_series(1, $2) g`, p, rows)

	var wg sync.WaitGroup
	results := make([]retention.BatchAckJanitorResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(0, 50))
		}(i)
	}
	wg.Wait()

	var folded int64
	for i, r := range results {
		if r.FoldErr != nil {
			t.Fatalf("janitor %d: %v", i, r.FoldErr)
		}
		folded += r.FoldedCDCRows
	}
	// A replica can stop on a short chunk while the other still holds rows; one more
	// pass picks those up.
	folded += retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(0, 50)).FoldedCDCRows

	if folded != rows {
		t.Errorf("folded %d rows in all, want %d", folded, rows)
	}
	if total := qInt(t, conn, `SELECT total FROM cdc_counter_totals WHERE pipeline_id = $1 AND op = 'c'`, p); total != rows {
		t.Errorf("total = %d, want %d: a row was counted twice or not at all", total, rows)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM pipeline_batch_acks`); n != 0 {
		t.Errorf("%d rows left unfolded", n)
	}
	if f := qInt(t, conn, `SELECT MAX(max_offset) FROM cdc_offset_floors WHERE pipeline_id = $1`, p); f != rows {
		t.Errorf("highest floor = %d, want %d", f, rows)
	}
}

// TestMigration146_PruneTakesOnlyOldFinishedUncheckpointedBatchRuns: the prune
// deletes a finished batch run's acks after the retention, and never a recent run, a
// running one, one a resume checkpoint names, or the CDC execution.
func TestMigration146_PruneTakesOnlyOldFinishedUncheckpointedBatchRuns(t *testing.T) {
	conn := migrate146(t)
	p := newCDCPipeline146(t, conn, "batchprune")
	user := qStr(t, conn, `SELECT created_by::text FROM pipelines WHERE id = $1`, p)
	ws := qStr(t, conn, `SELECT workspace_id::text FROM pipelines WHERE id = $1`, p)
	connID := qStr(t, conn, `INSERT INTO connections (user_id, workspace_id, name, type, connector_type, config)
		VALUES ($1, $2, 'pg-src', 'source', 'postgresql', '{}') RETURNING id`, user, ws)

	run := func(status string, start, end any) string {
		return qStr(t, conn, `INSERT INTO executions (pipeline_id, status, start_time, end_time)
			VALUES ($1, $2, NOW() - make_interval(days => $3), CASE WHEN $4::int IS NULL THEN NULL ELSE NOW() - make_interval(days => $4::int) END)
			RETURNING id`, p, status, start, end)
	}
	oldDone := run("completed", 41, 40)
	oldFailedNoEnd := run("failed", 40, nil) // never stamped end_time: aged by its start
	oldDrop := run("silent_drop_detected", 41, 40)
	recentDone := run("completed", 6, 5)
	oldRunning := run("running", 40, nil)
	oldCheckpointed := run("completed", 41, 40)
	mustExec146(t, conn, `INSERT INTO pipeline_checkpoints (pipeline_id, connection_id, source_table, position)
		VALUES ($1, $2, 'public.orders', jsonb_build_object('execution_id', $3::text, 'offset', 7))`, p, connID, oldCheckpointed)

	for _, e := range []string{oldDone, oldFailedNoEnd, oldDrop, recentDone, oldRunning, oldCheckpointed} {
		for i := int64(1); i <= 5; i++ {
			seedAcks146(t, conn, p, ack146{e, "public.orders", "inline", nil, "b", 0, i, nil, 40 * 24 * time.Hour})
		}
	}
	// A CDC pipeline stopped long ago: its execution reads as an old finished run, and
	// its rows are recent, so the fold leaves them. The prune must leave them too.
	mustExec146(t, conn, `UPDATE executions SET status = 'cancelled', end_time = NOW() - INTERVAL '40 days' WHERE id = $1`, p)
	seedAcks146(t, conn, p, ack146{p, "public.orders", "cdc", "c", "t", 0, 1, nil, time.Minute})

	res := retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(30, 2))
	if res.PruneErr != nil {
		t.Fatalf("prune: %v", res.PruneErr)
	}
	if res.PrunedRuns != 3 || res.PrunedBatchRows != 15 {
		t.Errorf("pruned %d runs / %d rows, want 3 / 15", res.PrunedRuns, res.PrunedBatchRows)
	}
	for _, c := range []struct {
		name, exec string
		want       int
	}{
		{"old completed", oldDone, 0},
		{"old failed without end_time", oldFailedNoEnd, 0},
		{"old silent drop", oldDrop, 0},
		{"recent completed", recentDone, 5},
		{"old but running", oldRunning, 5},
		{"old but checkpointed", oldCheckpointed, 5},
		{"the CDC execution", p, 1},
	} {
		if n := qInt(t, conn, `SELECT COUNT(*) FROM pipeline_batch_acks WHERE execution_id = $1`, c.exec); n != c.want {
			t.Errorf("%s: %d acks left, want %d", c.name, n, c.want)
		}
	}
	// The runs themselves stay: only their ledger rows go.
	if n := qInt(t, conn, `SELECT COUNT(*) FROM executions WHERE pipeline_id = $1`, p); n != 7 {
		t.Errorf("%d executions left, want 7", n)
	}

	// Retention 0 keeps every batch row.
	mustExec146(t, conn, `UPDATE executions SET end_time = NOW() - INTERVAL '400 days' WHERE id = $1`, recentDone)
	if r := retention.RunBatchAckJanitorOnce(context.Background(), conn, janitorTestCfg(0, 2)); r.PrunedRuns != 0 {
		t.Errorf("retention 0 pruned %d runs", r.PrunedRuns)
	}
	if n := qInt(t, conn, `SELECT COUNT(*) FROM pipeline_batch_acks WHERE execution_id = $1`, recentDone); n != 5 {
		t.Errorf("retention 0 left %d acks, want 5", n)
	}
}
