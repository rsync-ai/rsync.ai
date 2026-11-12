//go:build integration_pg

// The loader's SQL against a real Postgres. buildAssetGraph is covered without a
// database (asset_graph_test.go); what only a database can prove is which stats rows
// the produced-tables query hands it. Uses the same scratch database as
// saved_query_upstreams_pg_test.go — see that file's header for the command:
//
//	UPSTREAM_PG_DSN='postgres://postgres:verify@localhost:55443/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_AssetGraph -v

package handlers

import (
	"context"
	"database/sql"
	"os"
	"reflect"
	"slices"
	"sort"
	"testing"
)

const (
	agWS     = "a0000000-0000-4000-8000-000000000001"
	agGCS    = "a0000000-0000-4000-8000-0000000000c1"
	agWH     = "a0000000-0000-4000-8000-0000000000c2"
	agToGCS  = "a0000000-0000-4000-8000-0000000000f1"
	agShop   = "a0000000-0000-4000-8000-0000000000f2"
	agUser   = "a0000000-0000-4000-8000-0000000000e1"
	agMongo  = "a0000000-0000-4000-8000-0000000000c3"
	agToMgo  = "a0000000-0000-4000-8000-0000000000f3"
	agSilver = "a0000000-0000-4000-8000-0000000000b1"
)

func assetGraphFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	const drop = `DROP TABLE IF EXISTS saved_query_schedule_upstreams, saved_query_schedules,
		pipeline_run_table_stats, pipelines, saved_queries, connections, workspace_layers,
		object_load_counters, warehouse_external_tables, dashboard_tiles, dashboards, charts`
	_, _ = db.ExecContext(ctx, drop)
	t.Cleanup(func() { _, _ = db.ExecContext(ctx, drop) })

	mustExec(t, db, `CREATE TABLE pipelines (
		id UUID PRIMARY KEY,
		name TEXT,
		workspace_id UUID,
		destination_connection_id UUID,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		storage_layout_version SMALLINT NOT NULL DEFAULT 1,
		config JSONB
	)`)
	mustExec(t, db, `CREATE TABLE pipeline_run_table_stats (
		pipeline_id UUID,
		table_name TEXT NOT NULL,
		schema_name TEXT,
		qualified_name TEXT NOT NULL,
		destination_schema TEXT,
		destination_qualified_name TEXT
	)`)
	mustExec(t, db, `CREATE TABLE saved_queries (
		id UUID PRIMARY KEY,
		workspace_id UUID NOT NULL,
		connection_id UUID NOT NULL,
		name TEXT NOT NULL,
		sql_text TEXT NOT NULL DEFAULT '',
		visibility TEXT NOT NULL,
		created_by UUID NOT NULL,
		materialization TEXT NOT NULL DEFAULT 'none',
		target_table TEXT,
		layer TEXT,
		layer_id UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	// The loader names each model's layer through this join (migration 135).
	mustExec(t, db, `CREATE TABLE workspace_layers (
		id UUID PRIMARY KEY,
		workspace_id UUID NOT NULL,
		name TEXT NOT NULL,
		position INTEGER NOT NULL
	)`)
	mustExec(t, db, `CREATE TABLE connections (
		id UUID PRIMARY KEY,
		connector_type TEXT,
		workspace_id UUID,
		config TEXT NOT NULL DEFAULT ''
	)`)
	// The lake links the loader reads first (lake_external_tables.go): the table
	// folders of layout v2 pipelines, and the warehouses' external tables, created
	// by the migration itself.
	mustExec(t, db, `CREATE TABLE object_load_counters (
		pipeline_id UUID NOT NULL,
		table_key TEXT NOT NULL,
		PRIMARY KEY (pipeline_id, table_key)
	)`)
	migration, err := os.ReadFile("../../migrations/151_warehouse_external_tables.sql")
	if err != nil {
		t.Fatalf("read migration 151: %v", err)
	}
	mustExec(t, db, string(migration))
	mustExec(t, db, `CREATE TABLE saved_query_schedules (
		schedule_id UUID PRIMARY KEY,
		saved_query_id UUID,
		schedule_type TEXT,
		status TEXT
	)`)
	mustExec(t, db, `CREATE TABLE saved_query_schedule_upstreams (
		schedule_id UUID,
		upstream_pipeline_id UUID,
		upstream_saved_query_id UUID
	)`)
	// Charts and dashboards as migration 143 has them, less the foreign keys to
	// tables this fixture does not make.
	mustExec(t, db, `CREATE TABLE charts (
		id UUID PRIMARY KEY,
		workspace_id UUID NOT NULL,
		name VARCHAR(200) NOT NULL,
		saved_query_id UUID,
		settings JSONB NOT NULL,
		visibility VARCHAR(20) NOT NULL DEFAULT 'private',
		created_by UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	mustExec(t, db, `CREATE TABLE dashboards (
		id UUID PRIMARY KEY,
		workspace_id UUID NOT NULL,
		name VARCHAR(200) NOT NULL,
		visibility VARCHAR(20) NOT NULL DEFAULT 'private',
		created_by UUID,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`)
	mustExec(t, db, `CREATE TABLE dashboard_tiles (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		dashboard_id UUID NOT NULL REFERENCES dashboards(id) ON DELETE CASCADE,
		workspace_id UUID NOT NULL,
		kind VARCHAR(10) NOT NULL,
		chart_id UUID REFERENCES charts(id) ON DELETE RESTRICT,
		body TEXT
	)`)

	mustExec(t, db, `INSERT INTO pipelines (id, name, workspace_id, destination_connection_id) VALUES
		($1, 'Mongo to GCS', $3, $4),
		($2, 'Shop CDC',     $3, $5)`,
		agToGCS, agShop, agWS, agGCS, agWH)

	// The object-storage pipeline's rows carry no destination name at all; the
	// relational one has a named table, an unnamed sibling, and an unnamed row for
	// the same table it later named.
	mustExec(t, db, `INSERT INTO pipeline_run_table_stats
		(pipeline_id, table_name, schema_name, qualified_name, destination_schema, destination_qualified_name)
		VALUES
		($1, 'orders',    'shop', 'shop.orders',    NULL,        NULL),
		($1, 'customers', 'shop', 'shop.customers', NULL,        NULL),
		($2, 'orders',    'shop', 'shop.orders',    'analytics', 'analytics.orders'),
		($2, 'orders',    'shop', 'shop.orders',    NULL,        NULL),
		($2, 'events',    'shop', 'shop.events',    NULL,        NULL)`,
		agToGCS, agShop)
}

// KI-LINEAGE-WRITES-0-TABLES-WHEN-DEST-NAME-NULL: the query filtered on
// `destination_qualified_name IS NOT NULL`, so these rows never reached the builder.
func TestPG_AssetGraphLoadsRowsWithNoDestinationName(t *testing.T) {
	db := upstreamPGDB(t)
	assetGraphFixture(t, db)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	type row struct {
		pipeline, dest, source string
		unplaced               bool
	}
	var got []row
	for _, tp := range in.Produced {
		got = append(got, row{tp.Table.ProducerName, tp.Table.DestQualified, tp.SourceQualified, tp.Unplaced})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].pipeline != got[j].pipeline {
			return got[i].pipeline < got[j].pipeline
		}
		if got[i].dest != got[j].dest {
			return got[i].dest < got[j].dest
		}
		return got[i].source < got[j].source
	})
	want := []row{
		{"Mongo to GCS", "", "shop.customers", true},
		{"Mongo to GCS", "", "shop.orders", true},
		{"Shop CDC", "", "shop.events", true},
		{"Shop CDC", "", "shop.orders", true},
		{"Shop CDC", "analytics.orders", "shop.orders", false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("produced rows =\n  %+v\nwant\n  %+v", got, want)
	}

	// And end to end: both pipelines write their tables, and the old unnamed
	// orders row does not draw a second orders table beside the named one.
	g := buildAssetGraph(in)
	if n := len(writesFrom(g, "pipeline:"+agToGCS)); n != 2 {
		t.Errorf("object-storage pipeline writes %d tables, want 2", n)
	}
	if n := len(writesFrom(g, "pipeline:"+agShop)); n != 2 {
		t.Errorf("relational pipeline writes %d tables, want 2", n)
	}
	if names := tableNames(g); !reflect.DeepEqual(names, []string{"analytics.orders", "shop.customers", "shop.events", "shop.orders"}) {
		t.Errorf("table names = %v", names)
	}
}

// The destination-connection filter still applies to the unnamed rows.
func TestPG_AssetGraphConnectionFilterAppliesToUnnamedRows(t *testing.T) {
	db := upstreamPGDB(t)
	assetGraphFixture(t, db)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, agGCS)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(in.Produced) != 2 {
		t.Fatalf("produced = %+v, want the GCS pipeline's 2 rows only", in.Produced)
	}
	for _, tp := range in.Produced {
		if tp.Table.ProducerID != agToGCS {
			t.Errorf("row from %s leaked through the connection filter", tp.Table.ProducerID)
		}
	}
}

// The loader hands the builder each model's connector type, which is what decides
// whether sql_text is SQL or a stored pipeline. Without it a MongoDB model reads nothing
// (TestAssetGraph_DocumentModelsReadTheCollectionsTheirPipelinesName); this proves the
// join that supplies it, and that a model whose connection is gone still loads.
func TestPG_AssetGraphLoadsEachModelsConnectorType(t *testing.T) {
	db := upstreamPGDB(t)
	assetGraphFixture(t, db)

	mustExec(t, db, `INSERT INTO connections (id, connector_type) VALUES ($1, 'mongodb')`, agMongo)
	mustExec(t, db, `INSERT INTO pipelines (id, name, workspace_id, destination_connection_id)
		VALUES ($1, 'Subscriptions CDC', $2, $3)`, agToMgo, agWS, agMongo)
	mustExec(t, db, `INSERT INTO pipeline_run_table_stats
		(pipeline_id, table_name, schema_name, qualified_name, destination_schema, destination_qualified_name)
		VALUES ($1, 'subscriptions', 'public', 'public.subscriptions', 'rsync_layers_test', 'rsync_layers_test.subscriptions')`,
		agToMgo)
	mustExec(t, db, `INSERT INTO workspace_layers (id, workspace_id, name, position)
		VALUES ($1, $2, 'Silver', 0)`, agSilver, agWS)
	mustExec(t, db, `INSERT INTO saved_queries
		(id, workspace_id, connection_id, name, sql_text, visibility, created_by, materialization, target_table, layer_id)
		VALUES
		('a0000000-0000-4000-8000-0000000000d1', $1, $2, 'Subscriptions silver',
		 '{"database":"rsync_layers_test","collection":"subscriptions","pipeline":[{"$match":{}}]}',
		 'workspace', $3, 'table', 'subscriptions_silver', $4),
		('a0000000-0000-4000-8000-0000000000d2', $1, 'a0000000-0000-4000-8000-0000000000c9', 'Orphan',
		 'SELECT 1', 'workspace', $3, 'none', NULL, NULL)`,
		agWS, agMongo, agUser, agSilver)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	types, layers := map[string]string{}, map[string]string{}
	for _, m := range in.Models {
		types[m.Name] = m.ConnectorType
		layers[m.Name] = m.Layer
	}
	if want := map[string]string{"Subscriptions silver": "mongodb", "Orphan": ""}; !reflect.DeepEqual(types, want) {
		t.Fatalf("connector types = %v, want %v", types, want)
	}
	if want := map[string]string{"Subscriptions silver": "Silver", "Orphan": ""}; !reflect.DeepEqual(layers, want) {
		t.Errorf("layers = %v, want the layer_id's name and none", layers)
	}

	g := buildAssetGraph(in)
	if got := modelByID(t, g, "a0000000-0000-4000-8000-0000000000d1").Upstreams; !reflect.DeepEqual(got, []string{"pipeline:" + agToMgo}) {
		t.Errorf("silver upstreams = %v, want the pipeline that lands its collection", got)
	}
	if names := tableNames(g); !slices.Contains(names, "rsync_layers_test.subscriptions_silver") {
		t.Errorf("a bare target lands in the pipeline's database; tables = %v", names)
	}
}

// The charts and dashboards the loader reads are the ones the caller can list: another
// user's private chart or dashboard is left out, as in charts_api.go and
// dashboards_api.go, and so is a chart whose source was deleted or is in a warehouse
// the graph is narrowed away from. A dashboard counts every chart it shows.
func TestPG_AssetGraphLoadsTheChartsAndDashboardsTheCallerCanSee(t *testing.T) {
	db := upstreamPGDB(t)
	assetGraphFixture(t, db)

	const (
		other     = "a0000000-0000-4000-8000-0000000000e2"
		revenue   = "a0000000-0000-4000-8000-0000000000d1"
		elsewhere = "a0000000-0000-4000-8000-0000000000d2"
		daily     = "a0000000-0000-4000-8000-000000000101"
		total     = "a0000000-0000-4000-8000-000000000102"
		theirs    = "a0000000-0000-4000-8000-000000000103"
		orphaned  = "a0000000-0000-4000-8000-000000000104"
		remote    = "a0000000-0000-4000-8000-000000000105"
		finance   = "a0000000-0000-4000-8000-000000000201"
		private   = "a0000000-0000-4000-8000-000000000202"
	)
	mustExec(t, db, `INSERT INTO saved_queries
		(id, workspace_id, connection_id, name, sql_text, visibility, created_by)
		VALUES ($1, $3, $4, 'Revenue', 'SELECT 1', 'workspace', $5),
		       ($2, $3, $6, 'Elsewhere', 'SELECT 1', 'workspace', $5)`,
		revenue, elsewhere, agWS, agWH, agUser, agGCS)
	mustExec(t, db, `INSERT INTO charts (id, workspace_id, name, saved_query_id, settings, visibility, created_by) VALUES
		($1, $6, 'Revenue by day', $8, '{"type":"line"}',      'private',   $7),
		($2, $6, 'Revenue total',  $8, '{"type":"scorecard"}', 'workspace', $10),
		($3, $6, 'Their draft',    $8, '{"type":"bar"}',       'private',   $10),
		($4, $6, 'Source deleted', NULL, '{"type":"bar"}',     'workspace', $7),
		($5, $6, 'Remote',         $9, '{"type":"pie"}',       'workspace', $7)`,
		daily, total, theirs, orphaned, remote, agWS, agUser, revenue, elsewhere, other)
	mustExec(t, db, `INSERT INTO dashboards (id, workspace_id, name, visibility, created_by) VALUES
		($1, $3, 'Finance', 'workspace', $4),
		($2, $3, 'Their own', 'private', $4)`, finance, private, agWS, other)
	// Finance shows the daily chart twice, the total, someone's private draft and the
	// remote one: four charts, two of them drawable in this warehouse.
	mustExec(t, db, `INSERT INTO dashboard_tiles (dashboard_id, workspace_id, kind, chart_id, body) VALUES
		($1, $3, 'chart', $4, NULL),
		($1, $3, 'chart', $4, NULL),
		($1, $3, 'chart', $5, NULL),
		($1, $3, 'chart', $6, NULL),
		($1, $3, 'chart', $7, NULL),
		($1, $3, 'text', NULL, 'Notes'),
		($2, $3, 'chart', $5, NULL)`, finance, private, agWS, daily, total, theirs, remote)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, agWH)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var charts []string
	for _, ch := range in.Charts {
		charts = append(charts, ch.Name+"/"+ch.Type)
	}
	sort.Strings(charts)
	if want := []string{"Revenue by day/line", "Revenue total/scorecard"}; !reflect.DeepEqual(charts, want) {
		t.Fatalf("charts = %v, want %v", charts, want)
	}
	if len(in.Dashboards) != 1 {
		t.Fatalf("dashboards = %+v, want Finance only", in.Dashboards)
	}
	d := in.Dashboards[0]
	shown := slices.Clone(d.ChartIDs)
	sort.Strings(shown)
	if d.Name != "Finance" || d.ChartCount != 4 || !reflect.DeepEqual(shown, []string{daily, total}) {
		t.Errorf("dashboard = %+v, want Finance with its 2 drawable charts, once each, of 4", d)
	}
	if in.Truncated.Charts || in.Truncated.Dashboards {
		t.Errorf("truncated = %+v, want neither", in.Truncated)
	}

	g := buildAssetGraph(in)
	if g.Stats.Charts != 2 || g.Stats.Dashboards != 1 || g.Stats.Edges[assetEdgeShows] != 2 {
		t.Errorf("stats = %+v, want 2 charts on 1 dashboard", g.Stats)
	}
}
