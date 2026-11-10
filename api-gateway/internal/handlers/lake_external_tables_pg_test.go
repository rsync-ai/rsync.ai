//go:build integration_pg

// Lake links against a real Postgres. The matching is covered without a database
// (lake_external_tables_test.go); what only a database can prove is the SQL around
// it: which table folders the layout v2 query finds, which warehouses' saved lists
// it reads (tenancy, connector type, the one connection named), when it asks for a
// re-read, the save's upsert, and that narrowing the graph to a warehouse keeps the
// lake pipelines feeding it. Same scratch database and command as
// asset_graph_pg_test.go:
//
//	UPSTREAM_PG_DSN='postgres://postgres:verify@localhost:55443/cplane?sslmode=disable' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_Lake -v

package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/rsync-ai/shared/crypto"
)

const (
	lkOtherWS = "a0000000-0000-4000-8000-000000000002"
	lkPG      = "a0000000-0000-4000-8000-0000000000c4" // a Postgres warehouse: never listed
	lkOtherWH = "a0000000-0000-4000-8000-0000000000c5" // a BigQuery in another workspace
	lkSilver  = "a0000000-0000-4000-8000-0000000000d3"
	lkLakeURI = "gs://rsync-lake/bronze/card_transactions/fintech/public/transactions/*.parquet"
	lkLink    = agToGCS + ":fintech/public/transactions -> fintech_bronze.transactions"
)

// lakeLinkFixture turns assetGraphFixture's "Mongo to GCS" pipeline into a layout v2
// lake pipeline writing two tables, and gives the BigQuery warehouse a saved external
// table over one of them and a silver model reading it. The two other warehouses
// have no saved list, so the loader would ask to re-read them if it ever read them.
func lakeLinkFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	assetGraphFixture(t, db)
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc := func(cfg string) string {
		s, err := crypto.EncryptString(cfg)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		return s
	}
	warehouse := enc(`{"project_id":"p","dataset_id":"fintech_bronze"}`)
	mustExec(t, db, `INSERT INTO connections (id, connector_type, workspace_id, config) VALUES
		($1, 'gcs',        $5, $6),
		($2, 'bigquery',   $5, $7),
		($3, 'postgresql', $5, $7),
		($4, 'bigquery',   $8, $7)`,
		agGCS, agWH, lkPG, lkOtherWH, agWS,
		enc(`{"bucket_name":"rsync-lake","path_prefix":"bronze"}`), warehouse, lkOtherWS)
	mustExec(t, db, `UPDATE pipelines SET storage_layout_version = 2,
		config = '{"destination_namespace":"card_transactions"}' WHERE id = $1`, agToGCS)
	mustExec(t, db, `INSERT INTO object_load_counters (pipeline_id, table_key) VALUES
		($1, 'fintech/public/transactions'), ($1, 'fintech/public/customers')`, agToGCS)
	// The run stats of a lake pipeline name the source table, as in prod.
	mustExec(t, db, `INSERT INTO pipeline_run_table_stats
		(pipeline_id, table_name, schema_name, qualified_name) VALUES
		($1, 'transactions', 'public', 'public.transactions')`, agToGCS)
	mustExec(t, db, `INSERT INTO warehouse_external_tables (connection_id, tables) VALUES ($1, $2)`,
		agWH, `[{"schema":"fintech_bronze","name":"transactions","source_uris":["`+lkLakeURI+`"]}]`)
	mustExec(t, db, `INSERT INTO saved_queries
		(id, workspace_id, connection_id, name, sql_text, visibility, created_by, materialization, target_table)
		VALUES ($1, $2, $3, 'Transactions silver', 'SELECT * FROM fintech_bronze.transactions',
		        'workspace', $4, 'table', 'fintech_silver.transactions')`,
		lkSilver, agWS, agWH, agUser)
}

// stubExternalTableRefresh records which warehouses the loader asks to re-read,
// answering underWay for each.
func stubExternalTableRefresh(t *testing.T, underWay bool) *[]string {
	t.Helper()
	asked := &[]string{}
	prev := refreshExternalTables
	refreshExternalTables = func(connectionID, _, _ string) bool {
		*asked = append(*asked, connectionID)
		return underWay
	}
	t.Cleanup(func() { refreshExternalTables = prev })
	return asked
}

func TestPG_LakeLinksDrawThePipelineIntoTheWarehouseModel(t *testing.T) {
	db := upstreamPGDB(t)
	lakeLinkFixture(t, db)
	asked := stubExternalTableRefresh(t, true)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := linkPairs(in.LakeLinks); !reflect.DeepEqual(got, []string{lkLink}) {
		t.Fatalf("links = %v, want the transactions folder only", got)
	}
	if l := in.LakeLinks[0]; l.LakeConnectionID != agGCS || l.WarehouseConnectionID != agWH || l.PipelineName != "Mongo to GCS" {
		t.Errorf("link = %+v", l)
	}
	// The saved list is fresh; the Postgres warehouse and the other workspace's
	// BigQuery have none, and are not this graph's to read.
	if len(*asked) != 0 || in.ExternalTablesPending {
		t.Errorf("re-read %v (pending %v), want none", *asked, in.ExternalTablesPending)
	}

	g := buildAssetGraph(in)
	if got := modelByID(t, g, lkSilver).Upstreams; !reflect.DeepEqual(got, []string{"pipeline:" + agToGCS}) {
		t.Errorf("silver upstreams = %v, want the lake pipeline", got)
	}
	// The pipeline's captured transactions table is the folder the external table reads.
	captured := assetKindTable + ":" + agGCS + "\x00captured\x00public.transactions"
	var external []assetEdge
	for _, e := range g.Edges {
		if e.Kind == assetEdgeExternal {
			external = append(external, e)
		}
	}
	if len(external) != 1 || external[0].From != captured {
		t.Fatalf("external edges = %+v, want one from %q", external, captured)
	}
	if to := nodeByID(t, g, external[0].To); to.ConnectionID != agWH || !to.PipelineTable {
		t.Errorf("external table node = %+v, want a pipeline table on the warehouse", to)
	}
}

func TestPG_LakeLinksKeepTheFeedingPipelineWhenNarrowedToTheWarehouse(t *testing.T) {
	db := upstreamPGDB(t)
	lakeLinkFixture(t, db)
	stubExternalTableRefresh(t, true)

	in, err := loadAssetGraphInput(context.Background(), db, agWS, agUser, agWH)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var pipelines []string
	for _, p := range in.Pipelines {
		pipelines = append(pipelines, p.ID)
	}
	slices.Sort(pipelines)
	if want := []string{agToGCS, agShop}; !reflect.DeepEqual(pipelines, want) {
		t.Fatalf("pipelines = %v, want the warehouse's own and the lake pipeline feeding it", pipelines)
	}
	if !slices.ContainsFunc(in.Produced, func(tp tableProducer) bool { return tp.Table.ProducerID == agToGCS }) {
		t.Errorf("the lake pipeline's tables were filtered out: %+v", in.Produced)
	}
	g := buildAssetGraph(in)
	if got := modelByID(t, g, lkSilver).Upstreams; !reflect.DeepEqual(got, []string{"pipeline:" + agToGCS}) {
		t.Errorf("silver upstreams = %v, want the lake pipeline", got)
	}

	// A warehouse that no lake pipeline feeds keeps only its own pipelines.
	in, err = loadAssetGraphInput(context.Background(), db, agWS, agUser, lkPG)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(in.Pipelines) != 0 || len(in.LakeLinks) != 0 {
		t.Errorf("narrowed to Postgres: pipelines %+v, links %v", in.Pipelines, linkPairs(in.LakeLinks))
	}
}

func TestPG_LakeLinksRereadAStaleOrMissingList(t *testing.T) {
	db := upstreamPGDB(t)
	lakeLinkFixture(t, db)
	ctx := context.Background()

	// Stale: re-read, and the saved copy is drawn meanwhile.
	mustExec(t, db, `UPDATE warehouse_external_tables SET read_at = NOW() - interval '2 hours'`)
	asked := stubExternalTableRefresh(t, true)
	lake, err := loadLakeTableLinks(ctx, db, agWS, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(*asked, []string{agWH}) || lake.Pending {
		t.Errorf("stale: re-read %v pending %v, want %s and not pending", *asked, lake.Pending, agWH)
	}
	if got := linkPairs(lake.Links); !reflect.DeepEqual(got, []string{lkLink}) {
		t.Errorf("stale: links = %v, want the saved copy's", got)
	}

	// Missing: re-read, and the page is told to ask again.
	mustExec(t, db, `DELETE FROM warehouse_external_tables`)
	asked = stubExternalTableRefresh(t, true)
	if lake, err = loadLakeTableLinks(ctx, db, agWS, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(*asked, []string{agWH}) || !lake.Pending || len(lake.Links) != 0 {
		t.Errorf("missing: re-read %v pending %v links %v", *asked, lake.Pending, linkPairs(lake.Links))
	}

	// A re-read that is not under way (its last one failed) leaves nothing pending.
	stubExternalTableRefresh(t, false)
	if lake, err = loadLakeTableLinks(ctx, db, agWS, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if lake.Pending {
		t.Error("pending with no re-read under way")
	}

	// A workspace with no layout v2 pipeline reads no warehouse at all.
	mustExec(t, db, `UPDATE pipelines SET storage_layout_version = 1`)
	asked = stubExternalTableRefresh(t, true)
	if lake, err = loadLakeTableLinks(ctx, db, agWS, ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(*asked) != 0 || lake.Pending {
		t.Errorf("no lake: re-read %v pending %v", *asked, lake.Pending)
	}
}

func TestPG_LakeSaveMergesWhatTheListingCouldNotRuleOut(t *testing.T) {
	db := upstreamPGDB(t)
	lakeLinkFixture(t, db)
	ctx := context.Background()

	saved := func(conn string) []string {
		t.Helper()
		var raw []byte
		if err := db.QueryRowContext(ctx,
			`SELECT tables FROM warehouse_external_tables WHERE connection_id = $1`, conn).Scan(&raw); err != nil {
			t.Fatalf("read %s: %v", conn, err)
		}
		var tables []warehouseExternalTable
		if err := json.Unmarshal(raw, &tables); err != nil {
			t.Fatalf("parse %s: %v", conn, err)
		}
		out := []string{}
		for _, tb := range tables {
			out = append(out, tb.Schema+"."+tb.Name)
		}
		return out
	}
	mustExec(t, db, `UPDATE warehouse_external_tables SET read_at = NOW() - interval '2 hours'`)

	// Another dataset's complete listing keeps this dataset's table, and stamps the row.
	if err := saveWarehouseExternalTables(ctx, db, agWH, warehouseTableListing{
		Complete: true, Scope: "events_bronze",
		External: []warehouseExternalTable{externalTable("events_bronze", "clicks", "gs://rsync-lake/clicks/*.parquet")},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := saved(agWH); !reflect.DeepEqual(got, []string{"events_bronze.clicks", "fintech_bronze.transactions"}) {
		t.Errorf("after a scoped listing: %v", got)
	}
	var fresh bool
	if err := db.QueryRowContext(ctx, `SELECT read_at > NOW() - interval '1 minute'
		FROM warehouse_external_tables WHERE connection_id = $1`, agWH).Scan(&fresh); err != nil || !fresh {
		t.Errorf("read_at not stamped (fresh %v, err %v)", fresh, err)
	}

	// A complete listing of every dataset drops what it did not find external.
	if err := saveWarehouseExternalTables(ctx, db, agWH, warehouseTableListing{
		Complete: true,
		External: []warehouseExternalTable{externalTable("events_bronze", "clicks", "gs://rsync-lake/clicks/*.parquet")},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := saved(agWH); !reflect.DeepEqual(got, []string{"events_bronze.clicks"}) {
		t.Errorf("after a complete listing: %v", got)
	}

	// The first save for a warehouse inserts its row.
	if err := saveWarehouseExternalTables(ctx, db, lkOtherWH, warehouseTableListing{Complete: true}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if got := saved(lkOtherWH); len(got) != 0 {
		t.Errorf("first save: %v, want an empty list", got)
	}
}

func TestPG_LakeUpstreamSuggestionOffersThePipeline(t *testing.T) {
	db := upstreamPGDB(t)
	lakeLinkFixture(t, db)
	stubExternalTableRefresh(t, true)

	got, err := resolveUpstreams(context.Background(), db, upstreamLookup{
		SavedQueryID:  lkSilver,
		SQLText:       "SELECT * FROM fintech_bronze.transactions",
		ConnectionID:  agWH,
		ConnectorType: "bigquery",
		WorkspaceID:   agWS,
		UserID:        agUser,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.Unresolved) != 0 || len(got.Candidates) != 1 {
		t.Fatalf("suggestion = %+v, want the lake pipeline only", got)
	}
	if c := got.Candidates[0]; c.Kind != assetKindPipeline || c.ID != agToGCS {
		t.Errorf("candidate = %+v, want pipeline %s", c, agToGCS)
	}
}
