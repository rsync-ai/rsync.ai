//go:build integration_pg

// Real-Postgres coverage for lineage history and the OpenLineage export (migration
// 153): the SQL that sqlmock cannot judge. The recorder's reads of saved_query_runs,
// executions, pipeline_run_table_stats and the CDC watermarks; the unique indexes that
// make recording a run twice a no-op; the history's table-key overlap and its private
// model filter; the pull's private model filter; a model's SQL versions and the column
// lineage they keep; the retention sweep; the target's upsert and audit; and the key
// rotation of its auth header. And a skipped model run (migration 154): that it is
// recorded naming no table, that the history counts every run beside the page it sends,
// and that the migration clears the same event recorded before.
//
// Needs a Postgres with every api-gateway migration applied (the test applies them
// itself) and nothing else in it:
//
//	docker run -d --name lineage-pg -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=lineage \
//	    -p 55481:5432 postgres:16-alpine
//	LINEAGE_PG_DSN='postgres://postgres:verify@localhost:55481/lineage?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Lineage -v
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	_ "github.com/rsync-ai/shared/pgdriver"
)

const (
	pgLEUser    = "1e000000-0000-4000-8000-000000000001"
	pgLEOther   = "1e000000-0000-4000-8000-000000000002"
	pgLEWS      = "1e000000-0000-4000-8000-0000000000a1"
	pgLESrc     = "1e000000-0000-4000-8000-0000000000c1"
	pgLEWh      = "1e000000-0000-4000-8000-0000000000c2"
	pgLEPipe    = "1e000000-0000-4000-8000-0000000000d1"
	pgLECDC     = "1e000000-0000-4000-8000-0000000000d2"
	pgLEModel   = "1e000000-0000-4000-8000-0000000000e1"
	pgLEPrivate = "1e000000-0000-4000-8000-0000000000e2"
	pgLEExec    = "1e000000-0000-4000-8000-0000000000f1"
	pgLEExecCDC = "1e000000-0000-4000-8000-0000000000f2"
	pgLEExecNew = "1e000000-0000-4000-8000-0000000000f3"

	pgLEOldKey   = "unit-test-lineage-pg-key-old-0123456789"
	pgLENewKey   = "unit-test-lineage-pg-key-new-0123456789"
	pgLEPassword = "SECRET-LINEAGE-PG-PASSWORD"
	pgLEHeader   = "Bearer unit-test-lineage-pg-canary"

	pgLESQLv1 = "SELECT id, total FROM public.orders"
	pgLESQLv2 = "SELECT id, total, status FROM public.orders"
)

func pgLineageDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LINEAGE_PG_DSN")
	if dsn == "" {
		t.Skip("LINEAGE_PG_DSN not set — see the file header")
	}
	d, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	prev := db.DB
	db.DB = d
	t.Cleanup(func() { db.DB = prev; _ = d.Close() })
	if err := db.Migrate("../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", pgLEOldKey)

	for _, q := range []string{
		// executions' pipeline key has no cascade (001 wins over 021b).
		`DELETE FROM executions WHERE pipeline_id IN ('` + pgLEPipe + `', '` + pgLECDC + `')`,
		`DELETE FROM pipelines WHERE id IN ('` + pgLEPipe + `', '` + pgLECDC + `')`,
		`DELETE FROM saved_queries WHERE workspace_id = '` + pgLEWS + `'`,
		`DELETE FROM connections WHERE id IN ('` + pgLESrc + `', '` + pgLEWh + `')`,
		`DELETE FROM audit_logs WHERE workspace_id = '` + pgLEWS + `'`,
		`DELETE FROM workspace_members WHERE workspace_id = '` + pgLEWS + `'`,
		`DELETE FROM workspaces WHERE id = '` + pgLEWS + `'`,
		`DELETE FROM users WHERE id IN ('` + pgLEUser + `', '` + pgLEOther + `')`,
	} {
		wfExec(t, d, q)
	}
	encrypt := func(cfg map[string]any) string {
		b, _ := json.Marshal(cfg)
		enc, err := crypto.EncryptString(string(b))
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	wfExec(t, d, `INSERT INTO users (id, email, password_hash) VALUES ($1, 'lineage-owner@example.com', 'x'), ($2, 'lineage-other@example.com', 'x')`,
		pgLEUser, pgLEOther)
	wfExec(t, d, `INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Lineage', 'lineage-pg', $2)`, pgLEWS, pgLEUser)
	wfExec(t, d, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'admin'), ($1, $3, 'member')`,
		pgLEWS, pgLEUser, pgLEOther)
	wfExec(t, d, `INSERT INTO connections (id, user_id, name, type, connector_type, config, workspace_id)
		VALUES ($1, $3, 'app db', 'source', 'postgresql', $4, $6),
		       ($2, $3, 'warehouse', 'destination', 'postgresql', $5, $6)`,
		pgLESrc, pgLEWh, pgLEUser,
		encrypt(map[string]any{"host": "app.example.com", "port": 5432, "database": "app", "password": pgLEPassword}),
		encrypt(map[string]any{"host": "db.example.com", "port": 5432, "database": "shop", "password": pgLEPassword}),
		pgLEWS)
	wfExec(t, d, `INSERT INTO pipelines (id, name, natural_language_request, workspace_id, source_connection_id, destination_connection_id, sync_mode)
		VALUES ($1, 'orders load', 'x', $3, $4, $5, 'batch'), ($2, 'events stream', 'x', $3, $4, $5, 'cdc')`,
		pgLEPipe, pgLECDC, pgLEWS, pgLESrc, pgLEWh)
	// The models were last edited three hours ago, before their runs.
	wfExec(t, d, `INSERT INTO saved_queries (id, workspace_id, connection_id, name, sql_text, visibility, created_by,
		    materialization, target_table, updated_at)
		VALUES ($1, $3, $4, 'orders_daily', $6, 'workspace', $5, 'table', 'analytics.orders_daily', now() - interval '3 hours'),
		       ($2, $3, $4, 'secret_private_model', 'SELECT id FROM public.payroll', 'private', $7, 'table', 'analytics.payroll_copy', now() - interval '3 hours')`,
		pgLEModel, pgLEPrivate, pgLEWS, pgLEWh, pgLEUser, pgLESQLv1, pgLEOther)
	return d
}

func pgLERouter(userID, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", userID)
		c.Set(ctxWorkspaceID, pgLEWS)
		c.Set(ctxWorkspaceRole, role)
		c.Next()
	})
	r.GET("/lineage/history", GetLineageHistory)
	r.GET("/lineage/openlineage", PullOpenLineageEvents)
	r.PUT("/lineage/openlineage/target", PutOpenLineageTarget)
	r.DELETE("/lineage/openlineage/target", DeleteOpenLineageTarget)
	r.POST("/admin/encryption/rotate", AdminRotateEncryptionKeys)
	return r
}

func pgLEEvent(t *testing.T, d *sql.DB, id string) (kind, runRef string, started sql.NullTime, inputs, outputs []lineageDataset, keys []string, hash string) {
	t.Helper()
	var in, out []byte
	var k pgStringList
	if err := d.QueryRow(`SELECT job_kind, run_ref, started_at, inputs, outputs, table_keys, sql_hash
		FROM lineage_events WHERE id = $1::uuid`, id).Scan(&kind, &runRef, &started, &in, &out, &k, &hash); err != nil {
		t.Fatalf("event %s: %v", id, err)
	}
	if err := json.Unmarshal(in, &inputs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		t.Fatal(err)
	}
	return kind, runRef, started, inputs, outputs, []string(k), hash
}

func pgLERunIDs(runs []lineageHistoryRun) []string {
	out := []string{}
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func TestPG_LineageRecordsHistoryAndExports(t *testing.T) {
	d := pgLineageDB(t)
	ctx := context.Background()
	queued := stubColumnLineageRefresh(t, true)
	now := time.Now()

	// --- Model runs: one each, the private model's too.
	wfExec(t, d, `INSERT INTO saved_query_runs (saved_query_id, trigger_source, status, target_table, started_at, finished_at)
		VALUES ($1, 'manual', 'succeeded', '', $3, $4), ($2, 'scheduled', 'failed', '', $3, $4)`,
		pgLEModel, pgLEPrivate, now.Add(-2*time.Hour), now.Add(-2*time.Hour+time.Minute))
	ids, err := recordModelRuns(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("recorded %d model runs, want 2", len(ids))
	}
	if again, err := recordModelRuns(ctx, d); err != nil || len(again) != 0 {
		t.Fatalf("a second pass recorded %v (err %v), want none", again, err)
	}
	var modelEv1, privateEv string
	if err := d.QueryRow(`SELECT id::text FROM lineage_events WHERE saved_query_id = $1`, pgLEModel).Scan(&modelEv1); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT id::text FROM lineage_events WHERE saved_query_id = $1`, pgLEPrivate).Scan(&privateEv); err != nil {
		t.Fatal(err)
	}
	hash1 := columnLineageSQLHash("postgresql", pgLESQLv1)
	kind, _, started, inputs, outputs, keys, hash := pgLEEvent(t, d, modelEv1)
	if kind != lineageJobModel || !started.Valid || hash != hash1 {
		t.Fatalf("model event kind %q started %v hash %q, want model, a start, %q", kind, started, hash, hash1)
	}
	if !reflect.DeepEqual(inputs, []lineageDataset{{ConnectionID: pgLEWh, Name: "public.orders"}}) ||
		!reflect.DeepEqual(outputs, []lineageDataset{{ConnectionID: pgLEWh, Name: "analytics.orders_daily"}}) {
		t.Fatalf("model event inputs %+v outputs %+v", inputs, outputs)
	}
	for _, k := range []string{lineageTableKey(pgLEWh, "public.orders"), lineageTableKey(pgLEWh, "analytics.orders_daily")} {
		if !slices.Contains(keys, k) {
			t.Fatalf("model event keys %q lack %q", keys, k)
		}
	}
	// The run's model had no column lineage, so its version was queued to be read.
	if !slices.ContainsFunc(*queued, func(j columnLineageJob) bool { return j.ModelID == pgLEModel && j.Hash == hash1 }) {
		t.Fatalf("queued %+v, want the model's version %s", *queued, hash1)
	}

	// --- Batch pipeline runs: an ended execution is recorded; a CDC pipeline's
	// execution and one inside the settle time are not.
	wfExec(t, d, `INSERT INTO executions (id, pipeline_id, status, start_time, end_time) VALUES
		($1, $4, 'completed', $6, $7), ($2, $5, 'completed', $6, $7), ($3, $4, 'completed', $8, $8)`,
		pgLEExec, pgLEExecCDC, pgLEExecNew, pgLEPipe, pgLECDC,
		now.Add(-time.Hour), now.Add(-50*time.Minute), now.Add(-time.Minute))
	wfExec(t, d, `INSERT INTO pipeline_run_table_stats (pipeline_id, execution_id, table_name, qualified_name, mode, status,
		    destination_qualified_name)
		VALUES ($1, $2, 'orders', 'public.orders', 'batch', 'completed', 'raw.orders'),
		       ($1, $2, 'customers', 'public.customers', 'batch', 'completed', NULL)`, pgLEPipe, pgLEExec)
	ids, err = recordPipelineRuns(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("recorded %d pipeline runs, want 1", len(ids))
	}
	pipeEv := ids[0]
	if again, err := recordPipelineRuns(ctx, d); err != nil || len(again) != 0 {
		t.Fatalf("a second pass recorded %v (err %v), want none", again, err)
	}
	_, runRef, _, inputs, outputs, keys, _ := pgLEEvent(t, d, pipeEv)
	if runRef != pgLEExec {
		t.Fatalf("pipeline event run_ref %q, want the execution %s", runRef, pgLEExec)
	}
	if !reflect.DeepEqual(inputs, []lineageDataset{{ConnectionID: pgLESrc, Name: "public.customers"}, {ConnectionID: pgLESrc, Name: "public.orders"}}) ||
		!reflect.DeepEqual(outputs, []lineageDataset{{ConnectionID: pgLEWh, Name: "public.customers", Captured: true}, {ConnectionID: pgLEWh, Name: "raw.orders"}}) {
		t.Fatalf("pipeline event inputs %+v outputs %+v", inputs, outputs)
	}
	if want := []string{lineageCapturedKey(pgLEWh, "public.customers"), lineageTableKey(pgLEWh, "raw.orders")}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("pipeline event keys %q, want %q", keys, want)
	}

	// --- A CDC window: the tables that landed since the previous fire.
	window := 15 * time.Minute
	boundary := now.Truncate(window)
	occurrence := cdcTrailingEdgeOccurrence(window, boundary)
	landedThrough := boundary.Add(-20 * time.Minute)
	wfExec(t, d, `INSERT INTO cdc_trailing_edge_state (pipeline_id, window_end, landed_through) VALUES ($1, $2, $3)`,
		pgLECDC, boundary.Add(-window), landedThrough)
	wfExec(t, d, `INSERT INTO cdc_table_watermarks (pipeline_id, table_name, destination_qualified_name, landed_at) VALUES
		($1, 'public.events', 'raw.events', $2), ($1, 'public.old', 'raw.old', $3)`,
		pgLECDC, landedThrough.Add(time.Minute), landedThrough.Add(-time.Minute))
	cdcEv, err := recordCDCWindow(ctx, d, pgLECDC, occurrence)
	if err != nil || cdcEv == "" {
		t.Fatalf("recordCDCWindow = %q, %v", cdcEv, err)
	}
	if again, err := recordCDCWindow(ctx, d, pgLECDC, occurrence); err != nil || again != "" {
		t.Fatalf("recording the window again = %q, %v; want nothing", again, err)
	}
	_, runRef, started, _, outputs, _, _ = pgLEEvent(t, d, cdcEv)
	if runRef != occurrence || !started.Valid || !started.Time.Equal(boundary.Add(-window)) {
		t.Fatalf("CDC event run_ref %q started %v, want %q from %v", runRef, started, occurrence, boundary.Add(-window))
	}
	if !reflect.DeepEqual(outputs, []lineageDataset{{ConnectionID: pgLEWh, Name: "raw.events"}}) {
		t.Fatalf("CDC event outputs %+v, want only the table that landed in the window", outputs)
	}

	// --- History, by each kind of node.
	for _, tc := range []struct {
		user, node string
		want       []string
	}{
		{pgLEUser, "model:" + pgLEModel, []string{modelEv1}},
		{pgLEUser, "pipeline:" + pgLEPipe, []string{pipeEv}},
		{pgLEUser, "pipeline:" + pgLECDC, []string{cdcEv}},
		{pgLEUser, "table:" + pgLEWh + "\x00analytics.orders_daily", []string{modelEv1}},
		{pgLEUser, "table:" + pgLEWh + "\x00raw.orders", []string{pipeEv}},
		{pgLEUser, "table:" + pgLEWh + "\x00captured\x00public.customers", []string{pipeEv}},
		// A model reading orders unqualified would be keyed by the bare name: found by both.
		{pgLEUser, "table:" + pgLEWh + "\x00public.orders", []string{modelEv1}},
		// Another user's private model: hidden from this user, shown to its author.
		{pgLEUser, "model:" + pgLEPrivate, []string{}},
		{pgLEUser, "table:" + pgLEWh + "\x00analytics.payroll_copy", []string{}},
		{pgLEOther, "table:" + pgLEWh + "\x00analytics.payroll_copy", []string{privateEv}},
	} {
		f, ok := lineageHistoryFilterFor(tc.node)
		if !ok {
			t.Fatalf("%q is not a node", tc.node)
		}
		runs, _, err := loadLineageHistory(ctx, d, pgLEWS, tc.user, f, 50, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := pgLERunIDs(runs); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("history of %q as %s = %v, want %v", tc.node, tc.user, got, tc.want)
		}
	}
	// Another workspace sees none of it.
	if runs, _, err := loadLineageHistory(ctx, d, wsScopeWS, pgLEUser, lineageHistoryFilter{Kind: assetKindPipeline, ID: pgLEPipe}, 50, false); err != nil || len(runs) != 0 {
		t.Fatalf("another workspace's history = %v (err %v)", pgLERunIDs(runs), err)
	}

	// --- An edit, then two runs: one that started before the edit, whose SQL is not
	// known (saved_queries' trigger stamps updated_at with the edit's time), and one
	// after it. Two versions ran; the first one's columns are kept.
	wfExec(t, d, `UPDATE saved_queries SET sql_text = $2 WHERE id = $1`, pgLEModel, pgLESQLv2)
	wfExec(t, d, `INSERT INTO saved_query_runs (run_id, saved_query_id, trigger_source, status, target_table, started_at, finished_at)
		VALUES ('1e000000-0000-4000-8000-0000000000b1', $1, 'scheduled', 'succeeded', '', $2, clock_timestamp()),
		       ('1e000000-0000-4000-8000-0000000000b2', $1, 'scheduled', 'succeeded', '', clock_timestamp(), clock_timestamp() + interval '1 second')`,
		pgLEModel, now.Add(-time.Hour))
	if ids, err := recordModelRuns(ctx, d); err != nil || len(ids) != 2 {
		t.Fatalf("recording the runs around the edit = %v, %v", ids, err)
	}
	hash2 := columnLineageSQLHash("postgresql", pgLESQLv2)
	for runID, want := range map[string]string{"1e000000-0000-4000-8000-0000000000b1": "", "1e000000-0000-4000-8000-0000000000b2": hash2} {
		var h string
		if err := d.QueryRow(`SELECT sql_hash FROM lineage_events WHERE run_ref = $1`, runID).Scan(&h); err != nil || h != want {
			t.Fatalf("run %s recorded SQL version %q (err %v), want %q", runID, h, err, want)
		}
	}
	for _, v := range []struct {
		sql, hash string
		columns   []string
	}{{pgLESQLv1, hash1, []string{"id", "total"}}, {pgLESQLv2, hash2, []string{"id", "total", "status"}}} {
		job := columnLineageJob{ModelID: pgLEModel, SQL: v.sql, ConnectorType: "postgresql", ConnectionID: pgLEWh, Hash: v.hash}
		if err := saveColumnLineage(ctx, d, pgLEWS, columnLineageSent{job: job, hadSchema: true},
			columnLineageAnswer{ID: pgLEModel, Status: "ok", Columns: v.columns}); err != nil {
			t.Fatal(err)
		}
	}
	var kept []string
	rows, err := d.Query(`SELECT sql_hash FROM model_column_lineage WHERE saved_query_id = $1 ORDER BY sql_hash`, pgLEModel)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h string
		_ = rows.Scan(&h)
		kept = append(kept, h)
	}
	rows.Close()
	both := []string{hash1, hash2}
	slices.Sort(both)
	if !reflect.DeepEqual(kept, both) {
		t.Fatalf("column lineage rows %v, want both versions %v: an event names the first", kept, both)
	}
	versions, err := loadModelSQLVersions(ctx, d, pgLEWS, pgLEModel)
	if err != nil {
		t.Fatal(err)
	}
	changes := modelColumnChanges(versions)
	if len(changes) != 1 || !changes[0].ColumnsKnown || !reflect.DeepEqual(changes[0].Added, []string{"status"}) || len(changes[0].Dropped) != 0 {
		t.Fatalf("column changes %+v from versions %+v, want status added", changes, versions)
	}

	// --- The history route, end to end.
	w := olServe(pgLERouter(pgLEUser, "viewer"), http.MethodGet, "/lineage/history?node=model:"+pgLEModel, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"added":["status"]`) {
		t.Fatalf("GET history = %d %s", w.Code, w.Body.String())
	}
	if w := olServe(pgLERouter(pgLEUser, "viewer"), http.MethodGet, "/lineage/history?node=model:"+pgLEPrivate, ""); w.Code != http.StatusNotFound {
		t.Fatalf("another user's private model's history = %d, want 404", w.Code)
	}

	// --- The pull: every event but the private model's, named by the store.
	events, stores, err := loadOpenLineageEvents(ctx, d, pgLEWS, openLineageQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.ID)
		if e.ID == modelEv1 && (e.Columns == nil || !reflect.DeepEqual(e.Columns.Columns, []string{"id", "total"})) {
			t.Fatalf("the first run's columns %+v, want its version's", e.Columns)
		}
	}
	if len(got) != 5 || slices.Contains(got, privateEv) {
		t.Fatalf("pulled %v, want the 5 workspace events and not the private model's %s", got, privateEv)
	}
	if s := stores[pgLEWh]; s.Namespace != "postgres://db.example.com:5432" {
		t.Fatalf("warehouse store %+v", s)
	}
	if only, _, err := loadOpenLineageEvents(ctx, d, pgLEWS, openLineageQuery{IDs: []string{privateEv}, Limit: 10}); err != nil || len(only) != 0 {
		t.Fatalf("asking for the private model's event by id gave %d (err %v)", len(only), err)
	}
	w = olServe(pgLERouter(pgLEUser, "admin"), http.MethodGet, "/lineage/openlineage?limit=2", "")
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "\n") != 2 || w.Header().Get(OpenLineageNextCursorHeader) == "" {
		t.Fatalf("GET pull = %d, cursor %q, body %s", w.Code, w.Header().Get(OpenLineageNextCursorHeader), w.Body.String())
	}
	w = olServe(pgLERouter(pgLEUser, "admin"), http.MethodGet, "/lineage/openlineage", "")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "shop.analytics.orders_daily") || strings.Contains(body, pgLEPassword) ||
		strings.Contains(body, "secret_private_model") || strings.Contains(body, "payroll") || strings.Contains(body, "SELECT") {
		t.Fatalf("GET pull = %d %s", w.Code, body)
	}

	// --- Retention: the first run's event goes, and with it the first version's columns.
	wfExec(t, d, `UPDATE lineage_events SET ended_at = now() - interval '400 days' WHERE id = $1`, modelEv1)
	nEvents, nVersions, err := sweepLineageRetention(ctx, d, 365)
	if err != nil || nEvents != 1 || nVersions != 1 {
		t.Fatalf("sweep deleted %d events and %d versions (err %v), want 1 and 1", nEvents, nVersions, err)
	}
	var left string
	if err := d.QueryRow(`SELECT string_agg(sql_hash, ',') FROM model_column_lineage WHERE saved_query_id = $1`, pgLEModel).Scan(&left); err != nil || left != hash2 {
		t.Fatalf("column lineage left %q (err %v), want only the current version %s", left, err, hash2)
	}

	// --- The target: saved with its header encrypted, kept, audited, rotated, deleted.
	admin := pgLERouter(pgLEUser, "admin")
	w = olServe(admin, http.MethodPut, "/lineage/openlineage/target",
		`{"url":"https://lineage.example.com/api/v1/lineage","auth_header":"`+pgLEHeader+`"}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "canary") {
		t.Fatalf("PUT target = %d %s", w.Code, w.Body.String())
	}
	wfExec(t, d, `UPDATE openlineage_targets SET last_status = 'delivered', last_status_code = 200 WHERE workspace_id = $1`, pgLEWS)
	w = olServe(admin, http.MethodPut, "/lineage/openlineage/target", `{"url":"https://lineage.example.com/api/v1/lineage","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT target again = %d %s", w.Code, w.Body.String())
	}
	var enc, status string
	var enabled bool
	var updatedBy sql.NullString
	if err := d.QueryRow(`SELECT auth_header_enc, enabled, last_status, updated_by::text FROM openlineage_targets WHERE workspace_id = $1`, pgLEWS).
		Scan(&enc, &enabled, &status, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if plain, err := crypto.DecryptString(enc); err != nil || plain != pgLEHeader || enabled || status != "delivered" || updatedBy.String != pgLEUser {
		t.Fatalf("stored target: header %q (err %v) enabled %v status %q by %v", plain, err, enabled, status, updatedBy)
	}
	var audits int
	if err := d.QueryRow(`SELECT count(*) FROM audit_logs WHERE workspace_id = $1 AND action = 'openlineage_target.update'
		AND details::text NOT LIKE '%canary%'`, pgLEWS).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("%d update audits without the header (err %v), want 2", audits, err)
	}

	t.Setenv("ENCRYPTION_KEYS", pgLENewKey+","+pgLEOldKey)
	w = olServe(admin, http.MethodPost, "/admin/encryption/rotate", `{}`)
	var rotation struct {
		Stats map[string]rotateEncryptionStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rotation); err != nil || w.Code != http.StatusOK {
		t.Fatalf("rotate = %d %s", w.Code, w.Body.String())
	}
	if st := rotation.Stats["openlineage_targets.auth_header_enc"]; st.Rotated != 1 || st.Failed != 0 {
		t.Fatalf("rotation of the target's header: %+v", st)
	}
	t.Setenv("ENCRYPTION_KEYS", pgLENewKey)
	if err := d.QueryRow(`SELECT auth_header_enc FROM openlineage_targets WHERE workspace_id = $1`, pgLEWS).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if plain, err := crypto.DecryptString(enc); err != nil || plain != pgLEHeader {
		t.Fatalf("after rotation the new key alone reads %q (err %v)", plain, err)
	}

	if w := olServe(admin, http.MethodDelete, "/lineage/openlineage/target", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE target = %d %s", w.Code, w.Body.String())
	}
	var n int
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM openlineage_targets WHERE workspace_id = $1)
		+ (SELECT 10 * count(*) FROM audit_logs WHERE workspace_id = $1 AND action = 'openlineage_target.delete')`, pgLEWS).Scan(&n); err != nil || n != 10 {
		t.Fatalf("after DELETE: target rows + 10×audits = %d (err %v), want 10", n, err)
	}
}

// A model run that was skipped read and wrote nothing (lineageEventNamesTables): the
// recorder names no table for it, the history of the model's tables does not list it,
// the history counts every run beside the page it sends, and migration 154 puts the
// event recorded before the fix in the same state without touching its SQL version or a
// cancelled pipeline run.
func TestPG_LineageSkippedModelRunsNameNoTables(t *testing.T) {
	d := pgLineageDB(t)
	ctx := context.Background()
	stubColumnLineageRefresh(t, true)
	now := time.Now()
	hash1 := columnLineageSQLHash("postgresql", pgLESQLv1)

	wfExec(t, d, `INSERT INTO saved_query_runs (saved_query_id, trigger_source, status, target_table, started_at, finished_at)
		VALUES ($1, 'scheduled', 'succeeded', '', $2, $3), ($1, 'scheduled', 'skipped', '', $4, $5)`,
		pgLEModel, now.Add(-3*time.Hour), now.Add(-3*time.Hour+time.Minute), now.Add(-2*time.Hour), now.Add(-2*time.Hour+time.Second))
	if ids, err := recordModelRuns(ctx, d); err != nil || len(ids) != 2 {
		t.Fatalf("recorded %v (err %v), want 2 model runs", ids, err)
	}
	var skippedEv string
	if err := d.QueryRow(`SELECT id::text FROM lineage_events WHERE saved_query_id = $1 AND event_type = 'ABORT'`, pgLEModel).Scan(&skippedEv); err != nil {
		t.Fatalf("no ABORT event for the skipped run: %v", err)
	}
	_, _, _, inputs, outputs, keys, hash := pgLEEvent(t, d, skippedEv)
	if len(inputs) != 0 || len(outputs) != 0 || len(keys) != 0 {
		t.Fatalf("the skipped run named inputs %+v outputs %+v keys %q", inputs, outputs, keys)
	}
	if hash != hash1 {
		t.Fatalf("the skipped run's sql hash = %q, want %q", hash, hash1)
	}

	// A table the model reads has the one run that read it, not the skipped one.
	f, _ := lineageHistoryFilterFor("table:" + pgLEWh + "\x00public.orders")
	runs, total, err := loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, 50, false)
	if err != nil || len(runs) != 1 || total != 1 || runs[0].EventType != "COMPLETE" {
		t.Fatalf("history of public.orders = %+v total %d (err %v), want the one COMPLETE run", runs, total, err)
	}
	// The model's own history lists the run that ran and counts the one skipped beside it;
	// asked for them, it lists both, and says so when the page holds one.
	f, _ = lineageHistoryFilterFor("model:" + pgLEModel)
	if runs, total, err = loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, 50, false); err != nil || len(runs) != 1 || total != 1 || runs[0].EventType != "COMPLETE" {
		t.Fatalf("model history = %+v total %d (err %v), want the one COMPLETE run", runs, total, err)
	}
	if n, last, err := loadLineageSkippedRuns(ctx, d, pgLEWS, pgLEUser, f); err != nil || n != 1 || last == nil || last.Before(now.Add(-2*time.Hour-time.Minute)) {
		t.Fatalf("skipped runs = %d, last %v (err %v), want 1", n, last, err)
	}
	if runs, total, err = loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, 1, true); err != nil || len(runs) != 1 || total != 2 {
		t.Fatalf("model history page = %d runs, total %d (err %v), want 1 of 2", len(runs), total, err)
	}
	if runs, total, err = loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, 50, true); err != nil || len(runs) != 2 || total != 2 || runs[0].EventType != "ABORT" ||
		len(runs[0].Inputs) != 0 || len(runs[0].Outputs) != 0 {
		t.Fatalf("model history with skipped = %+v total %d (err %v)", runs, total, err)
	}

	// An event recorded before the fix: a skipped model run that names the SQL's tables,
	// and a cancelled pipeline run that moved one.
	stale := `[{"connection_id":"` + pgLEWh + `","name":"public.orders"}]`
	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, saved_query_id, run_ref, event_type, started_at, ended_at,
		    inputs, outputs, table_keys, sql_hash)
		VALUES ($1, 'model', $2, 'old-skipped', 'ABORT', $3, $3, $4::jsonb, $5::jsonb, ARRAY[$6, $7], $8)`,
		pgLEWS, pgLEModel, now.Add(-time.Hour), stale, `[{"connection_id":"`+pgLEWh+`","name":"analytics.orders_daily"}]`,
		lineageTableKey(pgLEWh, "public.orders"), lineageTableKey(pgLEWh, "analytics.orders_daily"), hash1)
	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, pipeline_id, run_ref, event_type, ended_at, inputs, outputs, table_keys)
		VALUES ($1, 'pipeline', $2, 'old-cancelled', 'ABORT', $3, $4::jsonb, $4::jsonb, ARRAY[$5])`,
		pgLEWS, pgLEPipe, now.Add(-time.Hour), stale, lineageTableKey(pgLEWh, "public.orders"))

	body, err := os.ReadFile("../../migrations/154_lineage_skipped_runs_name_no_tables.sql")
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Exec(string(body))
	if err != nil {
		t.Fatalf("migration 154: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("migration 154 changed %d events, want only the old skipped model run", n)
	}
	var oldIn, oldOut, oldHash string
	var oldKeys int
	if err := d.QueryRow(`SELECT inputs::text, outputs::text, cardinality(table_keys), sql_hash FROM lineage_events
		WHERE saved_query_id = $1 AND run_ref = 'old-skipped'`, pgLEModel).Scan(&oldIn, &oldOut, &oldKeys, &oldHash); err != nil {
		t.Fatal(err)
	}
	if oldIn != "[]" || oldOut != "[]" || oldKeys != 0 || oldHash != hash1 {
		t.Fatalf("old skipped event = %s %s %d keys, hash %q; want cleared and the hash kept", oldIn, oldOut, oldKeys, oldHash)
	}
	var cancelledKeys int
	if err := d.QueryRow(`SELECT cardinality(table_keys) FROM lineage_events WHERE pipeline_id = $1 AND run_ref = 'old-cancelled'`, pgLEPipe).Scan(&cancelledKeys); err != nil || cancelledKeys != 1 {
		t.Fatalf("the cancelled pipeline run has %d keys (err %v), want its one table kept", cancelledKeys, err)
	}
	// A second run finds nothing to change.
	if res, err = d.Exec(string(body)); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 0 {
		t.Fatalf("migration 154 run again changed %d events", n)
	}
}

// The bug class: a skipped run is not a run that ran, so a model skipped on every tick
// must not fill the newest-50 window with runs that did nothing and push the runs that
// ran out of it. 60 skipped runs, all newer than the one that ran: the default page still
// holds it, counts 1, and says 60 were left out; asked for them, it holds 50 of 61. A
// private model's skipped runs are no more visible than its runs; a cancelled pipeline
// run is not a skipped one.
func TestPG_LineageSkippedRunsDoNotCrowdTheHistoryWindow(t *testing.T) {
	d := pgLineageDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	hash := columnLineageSQLHash("postgresql", pgLESQLv1)

	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, saved_query_id, run_ref, event_type, started_at, ended_at, sql_hash)
		VALUES ($1, 'model', $2, 'ran', 'COMPLETE', $3, $3, $4)`, pgLEWS, pgLEModel, now.Add(-72*time.Hour), hash)
	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, saved_query_id, run_ref, event_type, started_at, ended_at, sql_hash)
		SELECT $1, 'model', $2, 'skipped-' || g, 'ABORT', $3::timestamptz + g * interval '1 minute', $3::timestamptz + g * interval '1 minute', $4
		FROM generate_series(1, 60) g`, pgLEWS, pgLEModel, now.Add(-48*time.Hour), hash)
	// A private model of another user: skipped, and not this user's to see counted.
	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, saved_query_id, run_ref, event_type, started_at, ended_at, sql_hash)
		VALUES ($1, 'model', $2, 'private-skipped', 'ABORT', $3, $3, $4)`, pgLEWS, pgLEPrivate, now, hash)
	// A cancelled pipeline run is not a skipped model run.
	wfExec(t, d, `INSERT INTO lineage_events (workspace_id, job_kind, pipeline_id, run_ref, event_type, ended_at)
		VALUES ($1, 'pipeline', $2, 'cancelled', 'ABORT', $3)`, pgLEWS, pgLEPipe, now)

	f, _ := lineageHistoryFilterFor("model:" + pgLEModel)
	runs, total, err := loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, lineageHistoryDefaultLimit, false)
	if err != nil || len(runs) != 1 || total != 1 || runs[0].EventType != "COMPLETE" || runs[0].RunRef != "ran" {
		t.Fatalf("default history = %d runs of %d (err %v), want the one run that ran: %+v", len(runs), total, err, runs)
	}
	n, last, err := loadLineageSkippedRuns(ctx, d, pgLEWS, pgLEUser, f)
	if err != nil || n != 60 || last == nil || !last.Equal(now.Add(-48*time.Hour+60*time.Minute)) {
		t.Fatalf("skipped runs = %d, last %v (err %v), want 60, the last at +60 min", n, last, err)
	}
	runs, total, err = loadLineageHistory(ctx, d, pgLEWS, pgLEUser, f, lineageHistoryDefaultLimit, true)
	if err != nil || len(runs) != lineageHistoryDefaultLimit || total != 61 {
		t.Fatalf("history with skipped = %d runs of %d (err %v), want 50 of 61", len(runs), total, err)
	}

	// Through the route, as the page asks.
	for _, tc := range []struct {
		query   string
		runs    int
		total   int
		skipped int
	}{
		{"", 1, 1, 60},
		{"&skipped=1", 50, 61, 60},
		{"&skipped=0", 1, 1, 60},
	} {
		w := httptest.NewRecorder()
		pgLERouter(pgLEUser, "viewer").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/lineage/history?node=model:"+pgLEModel+tc.query, nil))
		var body struct {
			TotalRuns     int                 `json:"total_runs"`
			Runs          []lineageHistoryRun `json:"runs"`
			SkippedRuns   int                 `json:"skipped_runs"`
			LastSkippedAt *time.Time          `json:"last_skipped_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK ||
			len(body.Runs) != tc.runs || body.TotalRuns != tc.total || body.SkippedRuns != tc.skipped || body.LastSkippedAt == nil {
			t.Fatalf("GET %q = %d: %d runs of %d, skipped %d, last %v (err %v); want %d of %d, skipped %d",
				tc.query, w.Code, len(body.Runs), body.TotalRuns, body.SkippedRuns, body.LastSkippedAt, err, tc.runs, tc.total, tc.skipped)
		}
	}

	// A private model: hidden from this user as a model, so is the count of its skips.
	fp, _ := lineageHistoryFilterFor("model:" + pgLEPrivate)
	if n, last, err := loadLineageSkippedRuns(ctx, d, pgLEWS, pgLEUser, fp); err != nil || n != 0 || last != nil {
		t.Fatalf("another user's private model: skipped runs = %d, last %v (err %v), want 0", n, last, err)
	}
	if n, _, err := loadLineageSkippedRuns(ctx, d, pgLEWS, pgLEOther, fp); err != nil || n != 1 {
		t.Fatalf("its author's skipped runs = %d (err %v), want 1", n, err)
	}
	// A pipeline keeps its cancelled run in its history by default.
	fpipe, _ := lineageHistoryFilterFor("pipeline:" + pgLEPipe)
	if runs, _, err := loadLineageHistory(ctx, d, pgLEWS, pgLEUser, fpipe, 50, false); err != nil || len(runs) != 1 || runs[0].EventType != "ABORT" {
		t.Fatalf("pipeline history = %+v (err %v), want its cancelled run listed", runs, err)
	}
}
