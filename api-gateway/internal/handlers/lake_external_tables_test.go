package handlers

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// The lake link is two readings of one folder: where the warehouse says an external
// table's files are, and where a layout v2 pipeline writes a table. These tests pin
// both readings and the match, and run in the default suite: no database, no bucket.

const (
	connLake = "33333333-3333-3333-3333-333333333333"
	connWH   = "55555555-5555-5555-5555-555555555555"
)

func TestExternalURIFolder(t *testing.T) {
	for _, tc := range []struct {
		uri      string
		isFolder bool
		want     lakeLocation
		ok       bool
	}{
		// The fintech demo's own table (prod, 2026-10-14).
		{"gs://rsync-ai-fintech-lake/bronze/card_transactions/fintech/public/transactions/*.parquet", false,
			lakeLocation{"gs", "rsync-ai-fintech-lake", "bronze/card_transactions/fintech/public/transactions/"}, true},
		// A hive prefix is a folder with or without its "/".
		{"gs://lake/bronze/p/db/t", true, lakeLocation{"gs", "lake", "bronze/p/db/t/"}, true},
		{"gs://lake/bronze/p/db/t/", true, lakeLocation{"gs", "lake", "bronze/p/db/t/"}, true},
		{"gs://lake/bronze/p/db/t/{dt:DATE}", true, lakeLocation{"gs", "lake", "bronze/p/db/t/"}, true},
		// One file names its folder.
		{"s3://lake/x/y/part-0.parquet", false, lakeLocation{"s3", "lake", "x/y/"}, true},
		// A wildcard mid-name cuts back to the folder before it.
		{"gs://lake/x/y/part-*.parquet", false, lakeLocation{"gs", "lake", "x/y/"}, true},
		{"gs://lake/*", false, lakeLocation{"gs", "lake", ""}, true},
		{"azure://Acct.blob.core.windows.net/cont/x/y/*.parquet", false, lakeLocation{"azure", "acct/cont", "x/y/"}, true},
		// Not a lake folder.
		{"https://drive.google.com/open?id=abc", false, lakeLocation{}, false},
		{"azure://acct.dfs.core.windows.net/cont/x/", false, lakeLocation{}, false},
		{"gs:///x/y", false, lakeLocation{}, false},
		{"orders.csv", false, lakeLocation{}, false},
	} {
		got, ok := externalURIFolder(tc.uri, tc.isFolder)
		if ok != tc.ok || got != tc.want {
			t.Errorf("externalURIFolder(%q, %v) = %+v, %v; want %+v, %v", tc.uri, tc.isFolder, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLakeConnectionRoot(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store string
		cfg   map[string]interface{}
		want  lakeLocation
		ok    bool
	}{
		{"gcs with a prefix", "gs", map[string]interface{}{"bucket": "lake", "path_prefix": "/bronze/"},
			lakeLocation{"gs", "lake", "bronze/"}, true},
		{"bucket_name and no prefix", "s3", map[string]interface{}{"bucket_name": "lake"},
			lakeLocation{"s3", "lake", ""}, true},
		{"first prefix key wins", "gs", map[string]interface{}{"bucket": "lake", "prefix": "a", "path": "b"},
			lakeLocation{"gs", "lake", "a/"}, true},
		{"azure account from the connection string", "azure", map[string]interface{}{
			"container": "cont", "connection_string": "DefaultEndpointsProtocol=https;AccountName=Acct;AccountKey=x"},
			lakeLocation{"azure", "acct/cont", ""}, true},
		{"azure without an account", "azure", map[string]interface{}{"container": "cont"}, lakeLocation{}, false},
		{"no bucket", "gs", map[string]interface{}{"path_prefix": "bronze"}, lakeLocation{}, false},
	} {
		got, ok := lakeConnectionRoot(tc.store, tc.cfg)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: lakeConnectionRoot = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLakeTableFolderAt(t *testing.T) {
	root := lakeLocation{"gs", "lake", "bronze/"}
	got, ok := lakeTableFolderAt(root, "card_transactions", "fintech/public/transactions")
	if want := (lakeLocation{"gs", "lake", "bronze/card_transactions/fintech/public/transactions/"}); !ok || got != want {
		t.Errorf("lakeTableFolderAt = %+v, %v; want %+v", got, ok, want)
	}
	// Layout v2 refuses these prefixes, so no folder of theirs exists to match.
	for _, bad := range []string{"", "Card", "a/b", "_x"} {
		if _, ok := lakeTableFolderAt(root, bad, "db/t"); ok {
			t.Errorf("pipeline prefix %q placed a folder; layout v2 would refuse it", bad)
		}
	}
	if _, ok := lakeTableFolderAt(root, "p", "/"); ok {
		t.Errorf("an empty table key placed a folder")
	}
}

func TestLakeTableKeyEndsWith(t *testing.T) {
	for _, tc := range []struct {
		key, name string
		want      bool
	}{
		{"fintech/public/transactions", "public.transactions", true},
		{"fintech/public/transactions", "transactions", true},
		{"fintech/public/transactions", "FINTECH.public.Transactions", true},
		{"fintech/public/transactions", "ublic.transactions", false},
		{"fintech/public/transactions", "other.transactions", false},
		{"fintech/public/transactions", "", false},
		// The sink percent-encodes a "/" inside a name; the captured name has it plain.
		{"db/a%2Fb", "a/b", true},
	} {
		if got := lakeTableKeyEndsWith(tc.key, tc.name); got != tc.want {
			t.Errorf("lakeTableKeyEndsWith(%q, %q) = %v, want %v", tc.key, tc.name, got, tc.want)
		}
	}
	if got := lakeTableKeyName("fintech/public/a%2Fb"); got != "fintech.public.a/b" {
		t.Errorf("lakeTableKeyName = %q", got)
	}
}

func lakeFolder(pipelineID, tableKey, folder string) lakeTableFolder {
	return lakeTableFolder{
		PipelineID: pipelineID, PipelineName: "pipe " + pipelineID, ConnectionID: connLake,
		TableKey: tableKey, Location: lakeLocation{"gs", "lake", folder},
	}
}

func externalTable(schema, name string, uris ...string) warehouseExternalTable {
	return warehouseExternalTable{Schema: schema, Name: name, SourceURIs: uris}
}

func linkPairs(links []lakeTableLink) []string {
	out := []string{}
	for _, l := range links {
		out = append(out, l.PipelineID+":"+l.TableKey+" -> "+l.Schema+"."+l.Name)
	}
	return out
}

func TestMatchLakeTableLinks(t *testing.T) {
	folders := []lakeTableFolder{
		lakeFolder("p1", "fintech/public/transactions", "bronze/p1/fintech/public/transactions/"),
		lakeFolder("p1", "fintech/public/cards", "bronze/p1/fintech/public/cards/"),
		lakeFolder("p2", "app/orders", "bronze/p2/app/orders/"),
		// A deeper folder inside another's: the deeper one owns its files.
		lakeFolder("p3", "app/orders/archive", "bronze/p2/app/orders/archive/"),
	}
	hive := externalTable("bronze", "cards")
	hive.SourceURIPrefix = "gs://lake/bronze/p1/fintech/public/cards"
	got := matchLakeTableLinks(folders, []savedExternalTables{{
		ConnectionID: connWH,
		Tables: []warehouseExternalTable{
			externalTable("bronze", "transactions", "gs://lake/bronze/p1/fintech/public/transactions/*.parquet"),
			hive,
			// One partition's file still reads that table.
			externalTable("bronze", "orders", "gs://lake/bronze/p2/app/orders/dt=2026-10-01/part-0.parquet"),
			externalTable("bronze", "archive", "gs://lake/bronze/p2/app/orders/archive/*"),
			// The pipeline's whole root is every table at once: no one table.
			externalTable("bronze", "everything", "gs://lake/bronze/p1/*"),
			// Another bucket, same path.
			externalTable("bronze", "elsewhere", "gs://other/bronze/p1/fintech/public/transactions/*"),
		},
	}})
	want := []string{
		"p3:app/orders/archive -> bronze.archive",
		"p1:fintech/public/cards -> bronze.cards",
		"p2:app/orders -> bronze.orders",
		"p1:fintech/public/transactions -> bronze.transactions",
	}
	if !reflect.DeepEqual(linkPairs(got), want) {
		t.Errorf("links = %v\nwant %v", linkPairs(got), want)
	}
	for _, l := range got {
		if l.WarehouseConnectionID != connWH || l.LakeConnectionID != connLake {
			t.Errorf("link %+v lost a connection", l)
		}
	}
}

func TestMergeExternalTables(t *testing.T) {
	saved := []warehouseExternalTable{
		externalTable("bronze", "gone", "gs://lake/a/*"),
		externalTable("silver", "elsewhere", "gs://lake/b/*"),
		externalTable("bronze", "kept", "gs://lake/old/*"),
	}
	listing := warehouseTableListing{
		External: []warehouseExternalTable{
			externalTable("bronze", "kept", "gs://lake/new/*"),
			externalTable("BRONZE", "Kept", "gs://lake/dup/*"),
			{Schema: "bronze", Name: "new"},
		},
		Complete: true,
	}
	keys := func(ts []warehouseExternalTable) []string {
		out := []string{}
		for _, t := range ts {
			out = append(out, externalTableKey(t.Schema, t.Name)+"="+strings.Join(t.SourceURIs, ","))
		}
		return out
	}

	// A complete listing of every dataset drops what it no longer finds.
	got := mergeExternalTables(saved, listing)
	if want := []string{"bronze.kept=gs://lake/new/*", "bronze.new="}; !reflect.DeepEqual(keys(got), want) {
		t.Errorf("complete listing: %v, want %v", keys(got), want)
	}
	if got[1].SourceURIs == nil {
		t.Errorf("a table with no URIs is saved as null, not []")
	}

	// One dataset's listing keeps the others' tables.
	scoped := listing
	scoped.Scope = "bronze"
	if want := []string{"bronze.kept=gs://lake/new/*", "bronze.new=", "silver.elsewhere=gs://lake/b/*"}; !reflect.DeepEqual(keys(mergeExternalTables(saved, scoped)), want) {
		t.Errorf("scoped listing: %v, want %v", keys(mergeExternalTables(saved, scoped)), want)
	}

	// A listing that could not read everything rules nothing out.
	partial := listing
	partial.Complete = false
	if want := []string{"bronze.gone=gs://lake/a/*", "bronze.kept=gs://lake/new/*", "bronze.new=", "silver.elsewhere=gs://lake/b/*"}; !reflect.DeepEqual(keys(mergeExternalTables(saved, partial)), want) {
		t.Errorf("partial listing: %v, want %v", keys(mergeExternalTables(saved, partial)), want)
	}

	// A table naming thousands of files keeps the first hundred.
	many := make([]string, externalTableMaxURIs+50)
	for i := range many {
		many[i] = "gs://lake/x/f.parquet"
	}
	big := mergeExternalTables(nil, warehouseTableListing{External: []warehouseExternalTable{externalTable("b", "t", many...)}, Complete: true})
	if len(big[0].SourceURIs) != externalTableMaxURIs {
		t.Errorf("kept %d URIs, want %d", len(big[0].SourceURIs), externalTableMaxURIs)
	}
}

// The listing the Explorer already reads says which tables are external. A listing
// with any warning, or more tables than it was allowed to return, is not complete.
func TestDiscoverSchemaIndexTables_ReportsExternalTables(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		wantComplete bool
	}{
		{"complete", `{"overall_status":"success","total_tables_available":2,"tables":[
			{"name":"transactions","schema":"fintech_bronze","table_type":"external",
			 "source_uris":["gs://lake/bronze/p/fintech/public/transactions/*.parquet"],"columns":[]},
			{"name":"customers","schema":"fintech_silver","columns":[]}]}`, true},
		{"a warning", `{"overall_status":"partial_success","total_tables_available":2,"tables":[
			{"name":"transactions","schema":"fintech_bronze","table_type":"external",
			 "source_uris":["gs://lake/bronze/p/fintech/public/transactions/*.parquet"],"columns":[]},
			{"name":"customers","schema":"fintech_silver","discovery_status":"partial","columns":[]}]}`, false},
		{"capped", `{"overall_status":"success","total_tables_available":900,"tables":[
			{"name":"transactions","schema":"fintech_bronze","table_type":"external",
			 "source_uris":["gs://lake/bronze/p/fintech/public/transactions/*.parquet"],"columns":[]},
			{"name":"customers","schema":"fintech_silver","columns":[]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warehouseOrchestrator(t, func(int, map[string]interface{}) (int, string) { return http.StatusOK, tc.body })
			tables, listing, err := discoverSchemaIndexTables(t.Context(), warehouseConnID, "bigquery",
				map[string]interface{}{"project_id": "p"}, schemaIndexDiscovery{what: "tables", maxTables: 200})
			if err != nil {
				t.Fatalf("discoverSchemaIndexTables: %v", err)
			}
			if len(tables) != 2 {
				t.Errorf("index tables = %d, want 2: an external table is still a table", len(tables))
			}
			want := []warehouseExternalTable{externalTable("fintech_bronze", "transactions",
				"gs://lake/bronze/p/fintech/public/transactions/*.parquet")}
			if !reflect.DeepEqual(listing.External, want) {
				t.Errorf("external = %+v, want %+v", listing.External, want)
			}
			if listing.Complete != tc.wantComplete {
				t.Errorf("complete = %v, want %v", listing.Complete, tc.wantComplete)
			}
		})
	}
}

func TestWarehouseKeepsExternalTables(t *testing.T) {
	if !warehouseKeepsExternalTables(" BigQuery ") {
		t.Errorf("bigquery listings must be saved")
	}
	for _, other := range []string{"snowflake", "postgresql", "gcs", ""} {
		if warehouseKeepsExternalTables(other) {
			t.Errorf("%q does not report external tables; saving its listing would only clear the row", other)
		}
	}
}

// lakeLink is the fintech demo's shape: a GCS pipeline's transactions folder, read by
// a BigQuery external table.
func lakeLink(pipelineID, tableKey, schema, name string) lakeTableLink {
	return lakeTableLink{
		PipelineID: pipelineID, PipelineName: "Card transactions", LakeConnectionID: connLake,
		TableKey: tableKey, WarehouseConnectionID: connWH, Schema: schema, Name: name,
	}
}

func edgeSet(g assetGraph) map[string]bool {
	out := map[string]bool{}
	for _, e := range g.Edges {
		out[e.Kind+" "+e.From+" -> "+e.To] = true
	}
	return out
}

func nodeByID(t *testing.T, g assetGraph, id string) assetNode {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("node %q not in graph; nodes = %+v", id, g.Nodes)
	return assetNode{}
}

// The bug this exists for: a silver model reading a bronze external table read "a
// table we can't trace", and the pipeline writing its files "refreshes nothing",
// because the two live on different connections.
func TestAssetGraph_LakePipelineFeedsAWarehouseModelThroughItsExternalTable(t *testing.T) {
	in := assetGraphInput{
		Pipelines: []pipelineAsset{{ID: "p1", Name: "Card transactions", ConnectionID: connLake}},
		Produced: []tableProducer{
			pipeWroteUnnamed("p1", "Card transactions", connLake, "public.transactions", "transactions"),
		},
		LakeLinks: []lakeTableLink{lakeLink("p1", "fintech/public/transactions", "fintech_bronze", "transactions")},
		Models: []modelAsset{{
			ID: "m1", Name: "Silver transactions", ConnectionID: connWH, ConnectorType: "bigquery",
			SQLText:      "SELECT * FROM fintech_bronze.transactions",
			ScheduleType: scheduleAfterUpstream, ScheduleStatus: "active", TriggerUpstreamIDs: []string{"p1"},
		}},
	}
	g := buildAssetGraph(in)

	captured := assetKindTable + ":" + connLake + "\x00captured\x00public.transactions"
	external := assetKindTable + ":" + producedTableKey(connWH, producedTable{DestQualified: "fintech_bronze.transactions"})
	edges := edgeSet(g)
	for _, want := range []string{
		"writes pipeline:p1 -> " + captured,
		"external " + captured + " -> " + external,
		"reads " + external + " -> model:m1",
		"triggers pipeline:p1 -> model:m1",
	} {
		if !edges[want] {
			t.Errorf("missing edge %q; edges = %v", want, edges)
		}
	}
	if n := nodeByID(t, g, external); !n.PipelineTable || n.ConnectionID != connWH || n.Name != "fintech_bronze.transactions" {
		t.Errorf("external table node = %+v, want a pipeline table on the warehouse", n)
	}
	if g.Stats.Tables != 2 {
		t.Errorf("tables = %d, want 2: the folder's table joined the captured one, not a third", g.Stats.Tables)
	}
	for _, e := range g.Edges {
		if e.Kind == assetEdgeExternal && e.Evidence != assetEvidenceObserved {
			t.Errorf("external edge evidence = %q, want observed", e.Evidence)
		}
	}

	m := modelByID(t, g, "m1")
	if !reflect.DeepEqual(m.Upstreams, []string{"pipeline:p1"}) || len(m.UncoveredUpstreams) != 0 || len(m.Unresolved) != 0 {
		t.Errorf("model = %+v, want upstream p1, covered, nothing unresolved", m)
	}

	// Without its schedule the same model reads the pipeline but is not refreshed by it.
	in.Models[0].ScheduleType, in.Models[0].TriggerUpstreamIDs = "", nil
	m = modelByID(t, buildAssetGraph(in), "m1")
	if !reflect.DeepEqual(m.UncoveredUpstreams, []string{"pipeline:p1"}) {
		t.Errorf("manual model uncovered = %v, want [pipeline:p1]", m.UncoveredUpstreams)
	}

	// And without the link, today's answer: a table we can't trace.
	in.LakeLinks = nil
	m = modelByID(t, buildAssetGraph(in), "m1")
	if len(m.Upstreams) != 0 || !reflect.DeepEqual(m.Unresolved, []string{"fintech_bronze.transactions"}) {
		t.Errorf("unlinked model = %+v, want no upstream and the table unresolved", m)
	}
}

// A CDC pipeline that has created its folder but recorded no stats row yet still
// feeds the external table: the folder's table is drawn from its key.
func TestAssetGraph_LakeLinkWithoutACapturedTableDrawsTheFolder(t *testing.T) {
	g := buildAssetGraph(assetGraphInput{
		Pipelines: []pipelineAsset{{ID: "p1", Name: "Card transactions", ConnectionID: connLake}},
		LakeLinks: []lakeTableLink{lakeLink("p1", "fintech/public/transactions", "fintech_bronze", "transactions")},
	})
	lake := assetKindTable + ":" + connLake + "\x00lake\x00fintech/public/transactions"
	if n := nodeByID(t, g, lake); n.Name != "fintech.public.transactions" || !n.PipelineTable || n.ConnectionID != connLake {
		t.Errorf("lake node = %+v", n)
	}
	if !edgeSet(g)["writes pipeline:p1 -> "+lake] {
		t.Errorf("the pipeline does not write its folder's table; edges = %v", g.Edges)
	}
}

// Two folders whose tables share a captured name ("public.t" from two databases)
// cannot say which one the captured row is, so each folder is drawn on its own.
func TestAssetGraph_LakeLinkDoesNotGuessBetweenCapturedTables(t *testing.T) {
	g := buildAssetGraph(assetGraphInput{
		Pipelines: []pipelineAsset{{ID: "p1", Name: "Two databases", ConnectionID: connLake}},
		Produced:  []tableProducer{pipeWroteUnnamed("p1", "Two databases", connLake, "public.t", "t")},
		LakeLinks: []lakeTableLink{
			lakeLink("p1", "a/public/t", "bronze", "a_t"),
			lakeLink("p1", "b/public/t", "bronze", "b_t"),
		},
	})
	for _, key := range []string{"a/public/t", "b/public/t"} {
		nodeByID(t, g, assetKindTable+":"+connLake+"\x00lake\x00"+key)
	}
	if got := len(edgeKinds(g, assetEdgeExternal)); got != 2 {
		t.Errorf("external edges = %d, want 2", got)
	}
}

// A link whose pipeline is not in the graph (past the page, or filtered out) draws
// nothing: an edge from a node the response does not hold is worse than none.
func TestAssetGraph_LakeLinkWithoutItsPipelineIsDropped(t *testing.T) {
	g := buildAssetGraph(assetGraphInput{
		LakeLinks:             []lakeTableLink{lakeLink("p1", "fintech/public/transactions", "fintech_bronze", "transactions")},
		ExternalTablesPending: true,
	})
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Errorf("graph = %+v, want empty", g)
	}
	if !g.ExternalTablesPending {
		t.Errorf("the pending flag did not reach the response")
	}
}

// The Schedule dialog offers the lake pipeline for the same read, by the same rule.
func TestUpstreamSuggestion_OffersTheLakePipelineBehindAnExternalTable(t *testing.T) {
	links := []lakeTableLink{
		lakeLink("p1", "fintech/public/transactions", "fintech_bronze", "transactions"),
		// Another warehouse's external table is not this model's.
		{PipelineID: "p2", TableKey: "x/y", WarehouseConnectionID: connA, Schema: "fintech_bronze", Name: "transactions"},
	}
	refs := modelReadReferences("bigquery", "SELECT * FROM fintech_bronze.transactions t JOIN fintech_silver.cards c ON true")
	resp := buildUpstreamSuggestion(refs, lakeLinkProducers(links, connWH), "m1")
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want the one lake pipeline", resp.Candidates)
	}
	c := resp.Candidates[0]
	if c.Kind != assetKindPipeline || c.ID != "p1" || c.Table != "fintech_bronze.transactions" || !c.Qualified {
		t.Errorf("candidate = %+v", c)
	}
	if !reflect.DeepEqual(resp.Unresolved, []string{"fintech_silver.cards"}) {
		t.Errorf("unresolved = %v, want only the table no pipeline feeds", resp.Unresolved)
	}
}
