//go:build integration_pg

// Real-PostgreSQL coverage for the retention sweeps. sqlmock matches the SQL text and
// never runs it, so only a real server shows which rows an age or keep predicate
// actually deletes.
//
// Not part of the default suite — needs a live server with every migration applied:
//
//	docker run -d --name retention-pg -e POSTGRES_PASSWORD=verify \
//	    -e POSTGRES_DB=pipeline_db -p 55442:5432 postgres:16
//	for m in api-gateway/migrations/*.sql; do
//	    docker exec -i retention-pg psql -U postgres -d pipeline_db -v ON_ERROR_STOP=1 -q < "$m"
//	done
//	RETENTION_PG_DSN='postgres://postgres:verify@localhost:55442/pipeline_db?sslmode=disable' \
//	    go test -tags integration_pg ./internal/retention/ -run PG_ -v
//	docker rm -f retention-pg
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/rsync-ai/shared/pgdriver"
)

func retentionPG(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("RETENTION_PG_DSN")
	if dsn == "" {
		t.Skip("RETENTION_PG_DSN not set — see the file header for the commands that provide one")
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

// seedWithoutFKs inserts rows whose parents (users, connections, workspaces) the
// sweep never reads; session_replication_role=replica skips the FK triggers for this
// one transaction only.
func seedWithoutFKs(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("replica role: %v", err)
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func survivors(t *testing.T, db *sql.DB, query string) map[string]bool {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out[s] = true
	}
	return out
}

func TestPG_AuditLogRetention_DeletesOnlyRowsPastTheWindow(t *testing.T) {
	db := retentionPG(t)
	const conn = "dddddddd-0000-4000-8000-000000000001"
	seedWithoutFKs(t, db,
		`DELETE FROM audit_logs WHERE action LIKE 'retention-pg-%'`,
		`DELETE FROM connection_access_logs WHERE accessed_by LIKE 'retention-pg-%'`,
		`INSERT INTO audit_logs (action, resource_type, created_at) VALUES
		   ('retention-pg-400d', 'test', NOW() - INTERVAL '400 days'),
		   ('retention-pg-366d', 'test', NOW() - INTERVAL '366 days'),
		   ('retention-pg-364d', 'test', NOW() - INTERVAL '364 days'),
		   ('retention-pg-now',  'test', NOW()),
		   ('retention-pg-null', 'test', NULL)`,
		`INSERT INTO connection_access_logs (connection_id, accessed_by, access_type, success, created_at) VALUES
		   ('`+conn+`', 'retention-pg-400d', 'read', true, NOW() - INTERVAL '400 days'),
		   ('`+conn+`', 'retention-pg-364d', 'read', true, NOW() - INTERVAL '364 days'),
		   ('`+conn+`', 'retention-pg-now',  'read', true, NOW())`,
	)
	auditLeft := `SELECT action FROM audit_logs WHERE action LIKE 'retention-pg-%'`
	accessLeft := `SELECT accessed_by FROM connection_access_logs WHERE accessed_by LIKE 'retention-pg-%'`

	// Control first: a window longer than every row deletes none of them, so the
	// assertions below are about the predicate, not about the table being emptied.
	cfg := AuditLogRetentionConfig{RetentionDays: 1000, Chunk: 1, Budget: time.Minute}
	if res := RunAuditLogRetentionOnce(context.Background(), db, cfg); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := survivors(t, db, auditLeft); len(got) != 5 {
		t.Fatalf("a 1000-day window deleted audit rows: %v left", got)
	}

	// Chunk 1 forces the loop to go round once per row.
	cfg.RetentionDays = 365
	res := RunAuditLogRetentionOnce(context.Background(), db, cfg)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.AuditRows < 2 || res.AccessRows < 1 {
		t.Fatalf("deleted audit=%d access=%d, want at least 2 and 1", res.AuditRows, res.AccessRows)
	}
	gotAudit := survivors(t, db, auditLeft)
	for want, keep := range map[string]bool{
		"retention-pg-400d": false, "retention-pg-366d": false,
		"retention-pg-364d": true, "retention-pg-now": true, "retention-pg-null": true,
	} {
		if gotAudit[want] != keep {
			t.Errorf("audit_logs %s: kept=%v, want kept=%v", want, gotAudit[want], keep)
		}
	}
	gotAccess := survivors(t, db, accessLeft)
	for want, keep := range map[string]bool{
		"retention-pg-400d": false, "retention-pg-364d": true, "retention-pg-now": true,
	} {
		if gotAccess[want] != keep {
			t.Errorf("connection_access_logs %s: kept=%v, want kept=%v", want, gotAccess[want], keep)
		}
	}
}

// Run-history fixtures. Each pipeline id ends in its letter; run n of a pipeline is
// its id with the last two digits n (runOf).
const (
	rrBatch  = "aaaaaaaa-0000-4000-8000-00000000000a" // batch, has completions
	rrNoDone = "bbbbbbbb-0000-4000-8000-00000000000b" // batch, never completed
	rrCDC    = "cccccccc-0000-4000-8000-00000000000c" // sync_mode = 'cdc'
	rrCDCOld = "dddddddd-0000-4000-8000-00000000000d" // no sync_mode, cdc_mode set
	rrStats  = "eeeeeeee-0000-4000-8000-00000000000e" // batch, table stats only
)

var rrPipelines = []string{rrBatch, rrNoDone, rrCDC, rrCDCOld, rrStats}

func runOf(pipeline string, n int) string { return fmt.Sprintf("%s%02d", pipeline[:34], n) }

func rrIn() string { return "('" + strings.Join(rrPipelines, "', '") + "')" }

// rrRun is an executions row; ended "" leaves end_time NULL.
type rrRun struct{ id, pipeline, status, trigger, started, ended string }

// rrEvent is a pipeline_run_events row stored age ago; execution "" is NULL.
type rrEvent struct{ pipeline, execution, id, typ, severity, payload, age string }

func sqlOrNull(v string) string {
	if v == "" {
		return "NULL"
	}
	return "'" + v + "'"
}

func ago(age string) string {
	if age == "" {
		return "NULL"
	}
	return "NOW() - INTERVAL '" + age + "'"
}

func seedRunRetention(t *testing.T, db *sql.DB) {
	t.Helper()
	runs := []rrRun{
		// rrBatch
		{runOf(rrBatch, 1), rrBatch, "completed", "manual", "200 days", "200 days"},         // goes, whole
		{runOf(rrBatch, 2), rrBatch, "completed", "scheduled", "150 days", "150 days"},      // holds the newest completion
		{runOf(rrBatch, 3), rrBatch, "failed", "manual", "92 days", "92 days"},              // the newest run
		{runOf(rrBatch, 4), rrBatch, "running", "manual", "300 days", ""},                   // never finished
		{runOf(rrBatch, 5), rrBatch, "completed", "manual", "130 days", "89 days"},          // ended inside the window
		{runOf(rrBatch, 6), rrBatch, "completed", "manual", "100 days", "90 days 12 hours"}, // ended under a day past it
		// rrNoDone: no completion at all, so its completion lookup is NULL
		{runOf(rrNoDone, 1), rrNoDone, "failed", "manual", "200 days", "200 days"}, // goes
		{runOf(rrNoDone, 2), rrNoDone, "failed", "manual", "100 days", "100 days"}, // the newest run
		// rrCDC: the stream's anchor row, and an old CDC run
		{rrCDC, rrCDC, "running", "cdc", "300 days", ""},
		{runOf(rrCDC, 1), rrCDC, "completed", "cdc", "200 days", "200 days"},
		// rrCDCOld: a CDC pipeline by cdc_mode; its runs are never batch runs
		{runOf(rrCDCOld, 1), rrCDCOld, "completed", "manual", "200 days", "200 days"},
		{runOf(rrCDCOld, 2), rrCDCOld, "completed", "manual", "150 days", "150 days"},
		// rrStats
		{runOf(rrStats, 1), rrStats, "completed", "manual", "200 days", "200 days"}, // goes
		{runOf(rrStats, 2), rrStats, "completed", "manual", "180 days", "180 days"}, // the newest run that delivered
		{runOf(rrStats, 3), rrStats, "failed", "manual", "120 days", "120 days"},    // the newest run, delivered nothing
	}
	const hb = `{"metadata": {"heartbeat": true}}`
	events := []rrEvent{
		{rrBatch, runOf(rrBatch, 1), "A1-started", "PIPELINE_STARTED", "info", "", "200 days"},
		{rrBatch, runOf(rrBatch, 1), "A1-completed", "PIPELINE_COMPLETED", "info", "", "200 days"},
		{rrBatch, runOf(rrBatch, 1), "A1-metrics", "DATA_PLANE_METRICS", "", "", "200 days"},
		{rrBatch, runOf(rrBatch, 2), "A2-completed", "PIPELINE_COMPLETED", "info", "", "150 days"},
		{rrBatch, runOf(rrBatch, 2), "A2-stage", "STAGE_COMPLETED", "info", "", "150 days"},
		{rrBatch, runOf(rrBatch, 3), "A3-failed", "PIPELINE_FAILED", "error", "", "92 days"},
		{rrBatch, runOf(rrBatch, 3), "A3-metrics", "DATA_PLANE_METRICS", "", "", "92 days"},
		{rrBatch, runOf(rrBatch, 4), "A4-started", "PIPELINE_STARTED", "info", "", "300 days"},
		{rrBatch, runOf(rrBatch, 5), "A5-started", "PIPELINE_STARTED", "info", "", "130 days"},
		{rrBatch, runOf(rrBatch, 6), "A6-stage", "STAGE_STARTED", "info", "", "100 days"},
		{rrBatch, "", "A-progress", "PROGRESS", "", "", "200 days"},
		{rrBatch, "", "A-healer", "healer_decision", "info", "", "200 days"},
		{rrBatch, "", "A-progress-new", "PROGRESS", "", "", "10 days"},
		{rrNoDone, runOf(rrNoDone, 1), "B1-started", "PIPELINE_STARTED", "info", "", "200 days"},
		{rrNoDone, runOf(rrNoDone, 1), "B1-failed", "PIPELINE_FAILED", "error", "", "200 days"},
		{rrNoDone, runOf(rrNoDone, 2), "B2-failed", "PIPELINE_FAILED", "error", "", "100 days"},
		{rrCDC, rrCDC, "C-metrics", "DATA_PLANE_METRICS", "", "", "200 days"},
		{rrCDC, rrCDC, "C-tablestats", "TABLE_STATS", "info", "", "200 days"},
		{rrCDC, rrCDC, "C-heartbeat", "STAGE_PROGRESS", "info", hb, "200 days"},
		{rrCDC, rrCDC, "C-progress", "STAGE_PROGRESS", "info", "", "200 days"},
		{rrCDC, rrCDC, "C-started", "PIPELINE_STARTED", "info", "", "200 days"},
		{rrCDC, rrCDC, "C-warn", "TABLE_STATS", "warning", "", "200 days"},
		{rrCDC, rrCDC, "C-metrics-new", "DATA_PLANE_METRICS", "", "", "10 days"},
		{rrCDC, runOf(rrCDC, 1), "C1-started", "PIPELINE_STARTED", "info", "", "200 days"},
		{rrCDC, runOf(rrCDC, 1), "C1-metrics", "DATA_PLANE_METRICS", "", "", "200 days"},
		{rrCDCOld, runOf(rrCDCOld, 1), "D1-started", "PIPELINE_STARTED", "info", "", "200 days"},
		{rrCDCOld, runOf(rrCDCOld, 1), "D1-metrics", "DATA_PLANE_METRICS", "", "", "200 days"},
	}

	in := rrIn()
	stmts := []string{
		// The replica role skips the FK cascades too, so each table is cleared by hand.
		`DELETE FROM pipeline_run_events WHERE pipeline_id IN ` + in,
		`DELETE FROM pipeline_run_metrics WHERE pipeline_id IN ` + in,
		`DELETE FROM pipeline_run_table_stats WHERE pipeline_id IN ` + in,
		`DELETE FROM executions WHERE pipeline_id IN ` + in,
		`DELETE FROM pipelines WHERE id IN ` + in,
		`INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode, cdc_mode) VALUES
		   ('` + rrBatch + `',  'retention-pg-batch',   '', '` + rrBatch + `', 'batch', NULL),
		   ('` + rrNoDone + `', 'retention-pg-no-done', '', '` + rrBatch + `', NULL, NULL),
		   ('` + rrCDC + `',    'retention-pg-cdc',     '', '` + rrBatch + `', 'cdc', 'streaming_only'),
		   ('` + rrCDCOld + `', 'retention-pg-cdc-old', '', '` + rrBatch + `', NULL, 'initial'),
		   ('` + rrStats + `',  'retention-pg-stats',   '', '` + rrBatch + `', 'batch', NULL)`,
	}
	for _, r := range runs {
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO executions (id, pipeline_id, status, trigger_source, start_time, end_time)
			 VALUES ('%s', '%s', '%s', '%s', %s, %s)`,
			r.id, r.pipeline, r.status, r.trigger, ago(r.started), ago(r.ended)))
	}
	for _, e := range events {
		payload := e.payload
		if payload == "" {
			payload = "{}"
		}
		stmts = append(stmts, fmt.Sprintf(
			`INSERT INTO pipeline_run_events (pipeline_id, execution_id, event_id, event_type, severity, payload, received_at, created_at)
			 VALUES ('%s', %s, '%s', '%s', %s, '%s'::jsonb, %s, %s)`,
			e.pipeline, sqlOrNull(e.execution), e.id, e.typ, sqlOrNull(e.severity), payload, ago(e.age), ago(e.age)))
	}
	stmts = append(stmts,
		`INSERT INTO pipeline_run_metrics (pipeline_id, execution_id, ts, metrics) VALUES
		   ('`+rrBatch+`', '`+runOf(rrBatch, 2)+`', NOW() - INTERVAL '200 days', '{"label": "M-old"}'),
		   ('`+rrBatch+`', '`+runOf(rrBatch, 3)+`', NOW() - INTERVAL '10 days',  '{"label": "M-new"}')`,
		`INSERT INTO pipeline_run_table_stats
		   (pipeline_id, execution_id, table_name, qualified_name, mode, status, inserted_rows, updated_at) VALUES
		   ('`+rrStats+`', '`+runOf(rrStats, 1)+`', 'orders',    'S1-orders',    'batch', 'completed', 5, NOW() - INTERVAL '200 days'),
		   ('`+rrStats+`', '`+runOf(rrStats, 1)+`', 'customers', 'S1-customers', 'batch', 'completed', 2, NOW() - INTERVAL '200 days'),
		   ('`+rrStats+`', '`+runOf(rrStats, 2)+`', 'orders',    'S2-orders',    'batch', 'completed', 3, NOW() - INTERVAL '180 days'),
		   ('`+rrStats+`', '`+runOf(rrStats, 3)+`', 'orders',    'S3-orders',    'batch', 'failed',    0, NOW() - INTERVAL '120 days'),
		   ('`+rrCDC+`',   '`+rrCDC+`',             'orders',    'SC-orders',    'cdc',   'running',   9, NOW() - INTERVAL '200 days')`,
	)
	seedWithoutFKs(t, db, stmts...)
}

func checkKept(t *testing.T, table string, got map[string]bool, want map[string]bool) {
	t.Helper()
	for label, keep := range want {
		if got[label] != keep {
			t.Errorf("%s %s: kept=%v, want kept=%v", table, label, got[label], keep)
		}
	}
}

func TestPG_PipelineRunRetention_KeepsRunsWholeAndWhatReadersNeed(t *testing.T) {
	db := retentionPG(t)
	seedRunRetention(t, db)
	eventsLeft := `SELECT event_id FROM pipeline_run_events WHERE pipeline_id IN ` + rrIn()
	metricsLeft := `SELECT metrics->>'label' FROM pipeline_run_metrics WHERE pipeline_id IN ` + rrIn()
	statsLeft := `SELECT qualified_name FROM pipeline_run_table_stats WHERE pipeline_id IN ` + rrIn()

	// Control first: a window longer than every row deletes none of them.
	cfg := PipelineRunRetentionConfig{Enabled: true, RetentionDays: 1000, BatchSize: 2, Budget: time.Minute, PruneTableStats: true}
	if res := RunPipelineRunRetentionOnce(context.Background(), db, cfg); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := survivors(t, db, eventsLeft); len(got) != 27 {
		t.Fatalf("a 1000-day window deleted run events: %d of 27 left", len(got))
	}

	// The default: 90 days, table stats untouched. BatchSize 2 makes each step go round.
	cfg = PipelineRunRetentionConfig{Enabled: true, RetentionDays: 90, BatchSize: 2, Budget: time.Minute}
	res := RunPipelineRunRetentionOnce(context.Background(), db, cfg)
	if res.Err != nil || res.BudgetExhausted {
		t.Fatalf("tick: %+v", res)
	}
	if res.RunEvents < 5 || res.RoutineEvents < 6 || res.Metrics < 1 || res.TableStats != 0 {
		t.Errorf("deleted %+v, want at least 5 run events, 6 routine, 1 metric, no table stats", res)
	}
	checkKept(t, "pipeline_run_events", survivors(t, db, eventsLeft), map[string]bool{
		// An old finished batch run goes whole, its telemetry with it.
		"A1-started": false, "A1-completed": false, "A1-metrics": false,
		// The run holding the newest completion stays: a model's upstream gate reads it.
		"A2-completed": true, "A2-stage": true,
		// The newest run stays, failed or not, telemetry included.
		"A3-failed": true, "A3-metrics": true,
		// A run still going, one that ended inside the window, one that ended under a
		// day past it: their old events stay.
		"A4-started": true, "A5-started": true, "A6-stage": true,
		// No run: routine ticks past the window go, the rest stay.
		"A-progress": false, "A-healer": true, "A-progress-new": true,
		// A pipeline that never completed still loses its old runs.
		"B1-started": false, "B1-failed": false, "B2-failed": true,
		// A CDC stream loses routine ticks past the window, never its lifecycle
		// events or a warning.
		"C-metrics": false, "C-tablestats": false, "C-heartbeat": false,
		"C-progress": true, "C-started": true, "C-warn": true, "C-metrics-new": true,
		// A CDC run, or a run of a CDC pipeline, is never deleted whole.
		"C1-started": true, "C1-metrics": false,
		"D1-started": true, "D1-metrics": false,
	})
	checkKept(t, "pipeline_run_metrics", survivors(t, db, metricsLeft), map[string]bool{
		"M-old": false, "M-new": true,
	})
	statsBefore := map[string]bool{
		"S1-orders": true, "S1-customers": true, "S2-orders": true, "S3-orders": true, "SC-orders": true,
	}
	checkKept(t, "pipeline_run_table_stats (off)", survivors(t, db, statsLeft), statsBefore)

	// Opted in: an old finished batch run's table stats go; the newest run's, the
	// newest run that delivered rows', and a CDC stream's stay.
	cfg.PruneTableStats = true
	res = RunPipelineRunRetentionOnce(context.Background(), db, cfg)
	if res.Err != nil || res.TableStats < 2 {
		t.Fatalf("tick with table stats: %+v, want at least 2 table-stats rows", res)
	}
	checkKept(t, "pipeline_run_table_stats (on)", survivors(t, db, statsLeft), map[string]bool{
		"S1-orders": false, "S1-customers": false, "S2-orders": true, "S3-orders": true, "SC-orders": true,
	})
}
