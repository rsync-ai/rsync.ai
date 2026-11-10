//go:build integration_pg

// Real-Postgres coverage for the CDC watermark (migration 137): the SQL that sqlmock
// cannot judge. Whether a policy='all' fan-in really counts an idle CDC upstream as
// fresh, and only a CDC one, and only after its own baseline
// (KI-CDC-UPSTREAM-ALL-NEVER-FRESH).
//
// Uses the workflows harness and its database (see workflows_pg_test.go's header):
//
//	WF_PG_DSN='postgres://postgres:verify@localhost:55471/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_CDCWatermark -v
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// cdcwFanIn is an 'all' model with two pipeline upstreams: a batch one, which is the
// one firing, and a second one whose freshness the case decides.
type cdcwFanIn struct {
	model, schedule, batch, other string
}

func cdcwNewFanIn(t *testing.T, d *sql.DB, otherSyncMode string) cdcwFanIn {
	t.Helper()
	f := cdcwFanIn{model: uuid.NewString(), batch: uuid.NewString(), other: uuid.NewString()}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM saved_queries WHERE id = $1`, f.model)
		_, _ = d.Exec(`DELETE FROM pipelines WHERE id = ANY($1::uuid[])`, "{"+f.batch+","+f.other+"}")
	})
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode)
		VALUES ($1, 'orders batch', 'x', $2, 'batch')`, f.batch, pgWFWS)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode)
		VALUES ($1, 'customers', 'x', $2, $3)`, f.other, pgWFWS, otherSyncMode)
	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by)
		VALUES ($1, $2, $3, $4, 'SELECT 1 AS n', $5)`, f.model, pgWFWS, pgWFConn, "cdcw-"+f.model[:8], pgWFUser)
	if err := d.QueryRow(`INSERT INTO saved_query_schedules
		(saved_query_id, schedule_type, schedule_spec, temporal_schedule_id, run_as_user_id, created_by, upstream_policy)
		VALUES ($1, 'after_upstream', '{}', NULL, $2, $2, 'all') RETURNING schedule_id`, f.model, pgWFUser).Scan(&f.schedule); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.batch, f.other} {
		wfExec(t, d, `INSERT INTO saved_query_schedule_upstreams (schedule_id, upstream_kind, upstream_pipeline_id)
			VALUES ($1, 'pipeline', $2)`, f.schedule, p)
	}
	// The model's last successful build started an hour ago: its baseline.
	wfExec(t, d, `INSERT INTO saved_query_runs (saved_query_id, trigger_source, status, started_at, finished_at)
		VALUES ($1, 'manual', 'succeeded', NOW() - interval '1 hour', NOW() - interval '59 minutes')`, f.model)
	// The batch sibling completes now; it is the upstream firing.
	wfExec(t, d, `INSERT INTO pipeline_run_events (pipeline_id, event_id, event_type, occurred_at)
		VALUES ($1, $2, 'PIPELINE_COMPLETED', NOW())`, f.batch, "cdcw-"+f.batch)
	return f
}

func cdcwSinkLag(t *testing.T, d *sql.DB, pipelineID string, lag int64, ago string) {
	t.Helper()
	wfExec(t, d, `INSERT INTO pipeline_sink_lag (pipeline_id, consumer_group, total_lag, committed, measured_at)
		VALUES ($1, 'sink-x', $2, 100, NOW() - $3::interval)`, pipelineID, lag, ago)
}

func cdcwWatermark(t *testing.T, d *sql.DB, pipelineID, table, ago string) {
	t.Helper()
	wfExec(t, d, `INSERT INTO cdc_table_watermarks (pipeline_id, table_name, source_time)
		VALUES ($1, $2, NOW() - $3::interval)`, pipelineID, table, ago)
}

// § 13: an 'all' model with an idle CDC upstream rebuilds when its batch sibling
// completes. The CDC upstream never completes; what makes it fresh is a sink that had
// drained everything after the model's last build started, or a watermark past it.
// The controls are what keep the rule from saying "fresh" to everything CDC.
func TestPG_CDCWatermark_AnAllModelWithAnIdleCDCUpstreamRebuildsWhenItsBatchSiblingCompletes(t *testing.T) {
	d := pgWorkflowDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		syncMode string
		setup    func(t *testing.T, f cdcwFanIn)
		waiting  bool
	}{
		{"idle CDC upstream, sink drained after the baseline", "cdc",
			func(t *testing.T, f cdcwFanIn) { cdcwSinkLag(t, d, f.other, 0, "30 seconds") }, false},
		{"CDC upstream with a watermark past the baseline", "cdc",
			func(t *testing.T, f cdcwFanIn) { cdcwWatermark(t, d, f.other, "public.customers", "5 minutes") }, false},
		{"control: CDC upstream still behind, watermark older than the baseline", "cdc",
			func(t *testing.T, f cdcwFanIn) {
				cdcwSinkLag(t, d, f.other, 42, "30 seconds")
				cdcwWatermark(t, d, f.other, "public.customers", "2 hours")
			}, true},
		{"control: CDC sink last drained before the baseline", "cdc",
			func(t *testing.T, f cdcwFanIn) { cdcwSinkLag(t, d, f.other, 0, "2 hours") }, true},
		{"control: CDC upstream with no reading at all", "cdc",
			func(t *testing.T, f cdcwFanIn) {}, true},
		{"control: a batch upstream is never fresh by sink lag or watermark", "batch",
			func(t *testing.T, f cdcwFanIn) {
				cdcwSinkLag(t, d, f.other, 0, "30 seconds")
				cdcwWatermark(t, d, f.other, "public.customers", "5 minutes")
			}, true},
		{"control: a batch upstream that completed after the baseline is fresh", "batch",
			func(t *testing.T, f cdcwFanIn) {
				wfExec(t, d, `INSERT INTO pipeline_run_events (pipeline_id, event_id, event_type, occurred_at)
					VALUES ($1, $2, 'PIPELINE_COMPLETED', NOW())`, f.other, "cdcw-"+f.other)
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := cdcwNewFanIn(t, d, tc.syncMode)
			tc.setup(t, f)
			got, err := unsatisfiedUpstreams(ctx, d, f.schedule, f.batch)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			if tc.waiting {
				want = []string{f.other}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("waiting on %v, want %v", got, want)
			}
		})
	}
}

// § 13: a burst then silence builds the tail once — the sweep's SQL on a real
// database, with landed_at and the sweep's clock set by hand. A batch pipeline with a
// watermark row (it never has one in practice) is the control: the sweep reads CDC
// pipelines only.
func TestPG_CDCTrailingEdge_ABurstThenSilenceBuildsTheTailOnce(t *testing.T) {
	d := pgWorkflowDB(t)
	ctx := context.Background()
	cdc, batch := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM pipelines WHERE id = ANY($1::uuid[])`, "{"+cdc+","+batch+"}")
	})
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode)
		VALUES ($1, 'orders cdc', 'x', $2, 'cdc')`, cdc, pgWFWS)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, sync_mode)
		VALUES ($1, 'orders batch', 'x', $2, 'batch')`, batch, pgWFWS)

	at := func(clock string) sql.NullTime {
		v, err := time.Parse(time.RFC3339, "2026-10-03T"+clock+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return sql.NullTime{Time: v, Valid: true}
	}
	land := func(pipelineID, clock string) {
		wfExec(t, d, `INSERT INTO cdc_table_watermarks (pipeline_id, table_name, landed_at)
			VALUES ($1, 'public.orders', $2)
			ON CONFLICT (pipeline_id, table_name) DO UPDATE SET landed_at = EXCLUDED.landed_at`, pipelineID, at(clock).Time)
	}
	var fired []string
	fire := func(_ context.Context, _ *sql.DB, pipelineID, occurrence string) error {
		if pipelineID == cdc || pipelineID == batch {
			fired = append(fired, pipelineID+" "+occurrence)
		}
		return nil
	}
	sweep := func(clock string) {
		t.Helper()
		if _, err := sweepCDCTrailingEdge(ctx, d, 15*time.Minute, at(clock), fire); err != nil {
			t.Fatalf("sweep at %s: %v", clock, err)
		}
	}

	land(batch, "10:03:00")
	for _, step := range []struct{ landing, sweep string }{
		{"10:03:00", "10:03:30"}, {"10:05:00", "10:05:30"}, {"10:07:00", "10:07:30"},
	} {
		land(cdc, step.landing)
		sweep(step.sweep)
	}
	if len(fired) != 0 {
		t.Fatalf("fired mid-window: %v", fired)
	}
	var pending sql.NullTime
	if err := d.QueryRow(`SELECT pending_since FROM cdc_trailing_edge_state WHERE pipeline_id = $1`, cdc).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending.Valid || !pending.Time.Equal(at("10:03:00").Time) {
		t.Fatalf("pending_since = %v, want the burst's first landing parked once, 10:03", pending)
	}

	for _, clock := range []string{"10:15:30", "10:16:30", "10:30:30", "11:00:30"} {
		sweep(clock)
	}
	want := []string{cdc + " " + cdcTrailingEdgeOccurrence(15*time.Minute, at("10:15:00").Time)}
	if !reflect.DeepEqual(fired, want) {
		t.Fatalf("fired %v, want once, for the CDC pipeline, at the 10:15 boundary: %v", fired, want)
	}
	var windowEnd, landedThrough time.Time
	if err := d.QueryRow(`SELECT window_end, landed_through, pending_since FROM cdc_trailing_edge_state WHERE pipeline_id = $1`, cdc).
		Scan(&windowEnd, &landedThrough, &pending); err != nil {
		t.Fatal(err)
	}
	if !windowEnd.Equal(at("10:15:00").Time) || !landedThrough.Equal(at("10:07:00").Time) || pending.Valid {
		t.Fatalf("state = (%v, %v, %v), want (10:15, 10:07, NULL)", windowEnd, landedThrough, pending)
	}

	// A slower sweep that fired an earlier boundary cannot pull the state back.
	wfExec(t, d, cdcTrailingEdgeRecordFireSQL, cdc, at("10:00:00").Time, at("10:03:00").Time)
	if err := d.QueryRow(`SELECT window_end FROM cdc_trailing_edge_state WHERE pipeline_id = $1`, cdc).Scan(&windowEnd); err != nil {
		t.Fatal(err)
	}
	if !windowEnd.Equal(at("10:15:00").Time) {
		t.Fatalf("window_end moved back to %v", windowEnd)
	}

	// Parking keeps a landing the last fire has not covered, and replaces one it has.
	readPending := func() time.Time {
		t.Helper()
		var p sql.NullTime
		if err := d.QueryRow(`SELECT pending_since FROM cdc_trailing_edge_state WHERE pipeline_id = $1`, cdc).Scan(&p); err != nil || !p.Valid {
			t.Fatalf("pending_since = %v, %v", p, err)
		}
		return p.Time
	}
	wfExec(t, d, cdcTrailingEdgeParkSQL, cdc, at("10:20:00").Time)
	wfExec(t, d, cdcTrailingEdgeParkSQL, cdc, at("10:25:00").Time)
	if got := readPending(); !got.Equal(at("10:20:00").Time) {
		t.Fatalf("an uncovered parked landing was replaced: pending_since = %v, want 10:20", got)
	}
	wfExec(t, d, `UPDATE cdc_trailing_edge_state SET pending_since = $2 WHERE pipeline_id = $1`, cdc, at("10:05:00").Time)
	wfExec(t, d, cdcTrailingEdgeParkSQL, cdc, at("10:25:00").Time)
	if got := readPending(); !got.Equal(at("10:25:00").Time) {
		t.Fatalf("a parked landing the fire covered (10:05 ≤ 10:07) was kept: pending_since = %v, want 10:25", got)
	}
}

// § 13: a model run records its watermark. A real rebuild, through the manual door, of
// a model reading a CDC-landed table and a batch-landed one, materialized on the
// harness connection (this same Postgres). The run row carries the CDC table's
// watermark and nothing for the controls: the batch pipeline, a CDC pipeline landing
// the same table into another connection, and one outside the workspace. A model
// reading no CDC table records NULL.
func TestPG_CDCWatermark_AModelRunRecordsItsWatermark(t *testing.T) {
	d := pgWorkflowDB(t)
	ctx := context.Background()
	cdc, batch, elsewhere, otherWS := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	model, plain, ws2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	schema := "cdcw_" + cdc[:8]
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM saved_queries WHERE id = ANY($1::uuid[])`, "{"+model+","+plain+"}")
		_, _ = d.Exec(`DELETE FROM pipelines WHERE id = ANY($1::uuid[])`, "{"+cdc+","+batch+","+elsewhere+","+otherWS+"}")
		_, _ = d.Exec(`DELETE FROM workspaces WHERE id = $1`, ws2)
		_, _ = d.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	})
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'WF2', $2, $3)`, ws2, "wf-pg-"+ws2[:8], pgWFUser)
	wfExec(t, d, `CREATE SCHEMA `+schema)
	wfExec(t, d, `CREATE TABLE `+schema+`.orders (id int)`)
	wfExec(t, d, `CREATE TABLE `+schema+`.customers (id int)`)
	wfExec(t, d, `INSERT INTO `+schema+`.orders VALUES (1)`)
	wfExec(t, d, `INSERT INTO `+schema+`.customers VALUES (1)`)

	for _, p := range []struct {
		id, mode string
		ws, conn any
	}{
		{cdc, "cdc", pgWFWS, pgWFConn},
		{batch, "batch", pgWFWS, pgWFConn},
		{elsewhere, "cdc", pgWFWS, nil},
		{otherWS, "cdc", ws2, pgWFConn},
	} {
		wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, destination_connection_id, sync_mode)
			VALUES ($1, 'p', 'x', $2, $3, $4)`, p.id, p.ws, p.conn, p.mode)
	}
	landed := time.Date(2026, 10, 3, 10, 7, 0, 0, time.UTC)
	for _, w := range []struct {
		pipeline, table, dest string
		at                    time.Time
	}{
		{cdc, "public.orders", schema + ".orders", landed},
		{batch, "public.customers", schema + ".customers", landed.Add(time.Minute)},
		{elsewhere, "public.orders", schema + ".orders", landed.Add(-time.Hour)},
		{otherWS, "public.orders", schema + ".orders", landed.Add(-2 * time.Hour)},
	} {
		wfExec(t, d, `INSERT INTO cdc_table_watermarks (pipeline_id, table_name, destination_qualified_name, source_time)
			VALUES ($1, $2, $3, $4)`, w.pipeline, w.table, w.dest, w.at)
	}

	for _, q := range []struct{ id, sql, target string }{
		{model, `SELECT o.id FROM ` + schema + `.orders o JOIN ` + schema + `.customers c ON c.id = o.id`, schema + ".summary"},
		{plain, `SELECT 1 AS n`, schema + ".plain"},
	} {
		wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, created_by, materialization, target_table)
			VALUES ($1, $2, $3, $4, $5, $6, 'table', $7)`, q.id, pgWFWS, pgWFConn, "cdcw-"+q.id[:8], q.sql, pgWFUser, q.target)
	}

	recorded := func(id string) sql.NullString {
		t.Helper()
		res, err := runManualModel(ctx, d, id, pgWFUser)
		if err != nil || res.Status != "succeeded" {
			t.Fatalf("model %s: run = %+v, %v; want succeeded", id, res, err)
		}
		var raw sql.NullString
		if err := d.QueryRow(`SELECT cdc_watermarks::text FROM saved_query_runs
			WHERE saved_query_id = $1 ORDER BY started_at DESC LIMIT 1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	raw := recorded(model)
	if !raw.Valid {
		t.Fatal("the run of a model reading a CDC table recorded no watermark")
	}
	var got map[string]modelRunCDCWatermark
	if err := json.Unmarshal([]byte(raw.String), &got); err != nil {
		t.Fatalf("cdc_watermarks %q: %v", raw.String, err)
	}
	entry, ok := got[schema+".orders"]
	if len(got) != 1 || !ok || entry.PipelineID != cdc || entry.SourceTime == nil || !entry.SourceTime.Equal(landed) {
		t.Fatalf("cdc_watermarks = %s, want only %s.orders from %s at %s", raw.String, schema, cdc, landed)
	}

	if raw := recorded(plain); raw.Valid {
		t.Fatalf("a model reading no CDC table recorded %s, want NULL", raw.String)
	}
}
