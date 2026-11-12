package handlers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"api-gateway/internal/cache"
	"api-gateway/internal/validators"
)

// Column lineage on the asset graph (asset_column_lineage.go, lineage design step L1).
// The read itself is llm-service's (tests/test_column_lineage.py); these cover what the
// gateway decides: what is sent, when an answer is read again, what is saved, and how
// an answer's tables land on the graph's table nodes.

func TestColumnLineageSQLHash_TheConnectionTypeIsPartOfTheKey(t *testing.T) {
	a := columnLineageSQLHash("postgresql", "SELECT 1")
	if a != columnLineageSQLHash("postgresql", "SELECT 1") {
		t.Fatal("the hash is not stable")
	}
	if a == columnLineageSQLHash("bigquery", "SELECT 1") {
		t.Fatal("the same SQL on another connection type shares a key")
	}
	if a == columnLineageSQLHash("postgresql", "SELECT 2") {
		t.Fatal("an edit shares a key")
	}
	// The separator keeps "ab"+"c" apart from "a"+"bc".
	if columnLineageSQLHash("ab", "c") == columnLineageSQLHash("a", "bc") {
		t.Fatal("type and SQL run together")
	}
}

func TestColumnLineageStale(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name      string
		hadSchema bool
		age       time.Duration
		want      bool
	}{
		{"fresh, read with the columns", true, time.Minute, false},
		{"fresh, read without", false, 5 * time.Minute, false},
		{"read without the columns 11 minutes ago", false, 11 * time.Minute, true},
		{"read with them 11 minutes ago", true, 11 * time.Minute, false},
		{"read with them yesterday", true, 25 * time.Hour, true},
	} {
		if got := columnLineageStale(tc.hadSchema, now.Add(-tc.age), now); got != tc.want {
			t.Errorf("%s: stale = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestColumnLineageDialect(t *testing.T) {
	for ct, want := range map[string]string{
		// Unknown to sqlglot by their own names.
		"azure-postgres":    "postgresql",
		"supabase-postgres": "postgresql",
		"bigquery":          "bigquery",
		"snowflake":         "snowflake",
		// Runs no SQL: sent as it is, and llm-service answers unsupported.
		"mongodb": "mongodb",
	} {
		if got := columnLineageDialect(ct); got != want {
			t.Errorf("%s: dialect %q, want %q", ct, got, want)
		}
	}
}

func lineageIndexTable(schema, name string, columns ...string) cache.ExplorerTableIndex {
	t := cache.ExplorerTableIndex{Schema: schema, Name: name}
	for _, c := range columns {
		t.Columns = append(t.Columns, cache.ExplorerColumnIndex{Name: c})
	}
	return t
}

// Only the tables the SQL names are sent: a qualified name picks its schema's table, a
// bare one every schema's, without case, each once.
func TestColumnLineageTablesFor_SendsOnlyTheTablesTheSQLNames(t *testing.T) {
	index := []cache.ExplorerTableIndex{
		lineageIndexTable("analytics", "Orders", "id", "amount"),
		lineageIndexTable("staging", "orders", "id"),
		lineageIndexTable("public", "customers", "id", "name"),
		lineageIndexTable("sales", "customers", "id", "region"),
		lineageIndexTable("analytics", "unrelated", "x"),
		lineageIndexTable("analytics", "empty"),
	}
	refs := validators.ExtractTableReferences(`
		SELECT o.amount, c.name
		FROM ANALYTICS.orders o
		JOIN customers c ON c.id = o.id
		JOIN analytics.orders again ON again.id = o.id
		JOIN analytics.empty e ON e.x = o.id`)
	got := columnLineageTablesFor(refs, index)
	want := []columnLineageRequestTable{
		{Schema: "analytics", Name: "Orders", Columns: []string{"id", "amount"}},
		{Schema: "public", Name: "customers", Columns: []string{"id", "name"}},
		{Schema: "sales", Name: "customers", Columns: []string{"id", "region"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tables sent:\n got %+v\nwant %+v", got, want)
	}
}

func TestColumnLineageTablesFor_StopsAtWhatLLMServiceTakes(t *testing.T) {
	wide := lineageIndexTable("s", "wide")
	for i := 0; i < columnLineageMaxColumns+3; i++ {
		wide.Columns = append(wide.Columns, cache.ExplorerColumnIndex{Name: "c"})
	}
	got := columnLineageTablesFor([]validators.TableRef{{Parts: []string{"wide"}}}, []cache.ExplorerTableIndex{wide})
	if len(got) != 1 || len(got[0].Columns) != columnLineageMaxColumns {
		t.Fatalf("a table of %d columns sent %d", len(wide.Columns), len(got[0].Columns))
	}

	var many []cache.ExplorerTableIndex
	for i := 0; i < columnLineageMaxTables+2; i++ {
		many = append(many, lineageIndexTable("s"+strings.Repeat("x", i), "t", "id"))
	}
	if got := columnLineageTablesFor([]validators.TableRef{{Parts: []string{"t"}}}, many); len(got) != columnLineageMaxTables {
		t.Fatalf("sent %d tables, want at most %d", len(got), columnLineageMaxTables)
	}
}

// --- on the graph ---------------------------------------------------------------

func columnEdgesTo(g assetGraph, to string) []assetColumnEdge {
	var out []assetColumnEdge
	for _, e := range g.ColumnLineage.Edges {
		if e.To == to {
			out = append(out, e)
		}
	}
	return out
}

// An edge's table lands on the table node the model's reads edge comes from, the
// model-built table included; a table the graph does not know keeps its name and no
// node.
func TestAssetGraph_ColumnLineageLandsOnTheTablesTheModelReads(t *testing.T) {
	g := buildAssetGraph(assetGraphInput{
		Pipelines: []pipelineAsset{{ID: "p1", Name: "Orders sync", ConnectionID: connA}},
		Produced:  []tableProducer{pipeWrites("p1", "Orders sync", connA, "analytics.orders", "orders")},
		Models: []modelAsset{
			{ID: "m1", Name: "Daily", ConnectionID: connA, Materialization: "table", TargetTable: "analytics.daily",
				SQLText: "SELECT day, SUM(amount) AS total FROM analytics.orders JOIN ext.rates r ON true GROUP BY day"},
			{ID: "m2", Name: "Weekly", ConnectionID: connA,
				SQLText: "SELECT total FROM analytics.daily"},
		},
		ColumnLineage: map[string]savedColumnLineage{
			"m1": {Status: columnLineageStatusPartial, Reason: "one column", Columns: []string{"day", "total"}, Edges: []columnLineageEdge{
				{Table: []string{"analytics", "orders"}, Column: "amount", ToColumn: "total", Type: "DIRECT", Subtype: "AGGREGATION"},
				{Table: []string{"ANALYTICS", "Orders"}, Column: "day", ToColumn: "", Type: "INDIRECT", Subtype: "GROUP_BY"},
				{Table: []string{"ext", "rates"}, Column: "rate", ToColumn: "total", Type: "DIRECT", Subtype: "TRANSFORMATION"},
			}},
			"m2": {Status: columnLineageStatusOK, Columns: []string{"total"}, Edges: []columnLineageEdge{
				{Table: []string{"analytics", "daily"}, Column: "total", ToColumn: "total", Type: "DIRECT", Subtype: "IDENTITY"},
			}},
			// Not in this graph: its answer is not either.
			"gone": {Status: columnLineageStatusOK, Columns: []string{"x"}},
		},
	})
	if g.ColumnLineage == nil {
		t.Fatal("no column_lineage on a graph whose models have answers")
	}

	readsFrom := map[string]string{}
	for _, e := range edgeKinds(g, assetEdgeReads) {
		readsFrom[e.To] = e.From
	}
	m1 := columnEdgesTo(g, "model:m1")
	if len(m1) != 3 {
		t.Fatalf("model:m1 has %d column edges, want 3: %+v", len(m1), m1)
	}
	for _, e := range m1[:2] {
		if e.From == "" || e.From != readsFrom["model:m1"] {
			t.Errorf("%s.%s landed on %q, want the reads edge's table %q", e.FromTable, e.FromColumn, e.From, readsFrom["model:m1"])
		}
	}
	if m1[2].From != "" || m1[2].FromTable != "ext.rates" {
		t.Errorf("a table the graph does not draw: %+v, want no node and its name", m1[2])
	}
	m2 := columnEdgesTo(g, "model:m2")
	if len(m2) != 1 || m2[0].From == "" || m2[0].From != readsFrom["model:m2"] {
		t.Errorf("a model reading a model's table: %+v, want the table node %q", m2, readsFrom["model:m2"])
	}

	want := []assetColumnModel{
		{NodeID: "model:m1", Status: columnLineageStatusPartial, Columns: []string{"day", "total"}, Reason: "one column"},
		{NodeID: "model:m2", Status: columnLineageStatusOK, Columns: []string{"total"}},
	}
	if !reflect.DeepEqual(g.ColumnLineage.Models, want) {
		t.Errorf("models:\n got %+v\nwant %+v", g.ColumnLineage.Models, want)
	}
}

// A bare name that could be two tables is not guessed, and a model reading its own
// table (an incremental model) gets no edge from itself.
func TestAssetGraph_ColumnLineageDoesNotGuess(t *testing.T) {
	g := buildAssetGraph(assetGraphInput{
		Pipelines: []pipelineAsset{{ID: "p1", Name: "A", ConnectionID: connA}, {ID: "p2", Name: "B", ConnectionID: connA}},
		Produced: []tableProducer{
			pipeWrites("p1", "A", connA, "analytics.orders", "orders"),
			pipeWrites("p2", "B", connA, "staging.orders", "orders"),
		},
		Models: []modelAsset{
			{ID: "m1", ConnectionID: connA, SQLText: "SELECT id FROM orders"},
			{ID: "m2", ConnectionID: connA, Materialization: "table", TargetTable: "analytics.snapshot",
				SQLText: "SELECT id FROM analytics.snapshot"},
		},
		ColumnLineage: map[string]savedColumnLineage{
			"m1": {Status: columnLineageStatusOK, Edges: []columnLineageEdge{{Table: []string{"orders"}, Column: "id", ToColumn: "id", Type: "DIRECT", Subtype: "IDENTITY"}}},
			"m2": {Status: columnLineageStatusOK, Edges: []columnLineageEdge{{Table: []string{"analytics", "snapshot"}, Column: "id", ToColumn: "id", Type: "DIRECT", Subtype: "IDENTITY"}}},
		},
	})
	for _, e := range g.ColumnLineage.Edges {
		if e.From != "" {
			t.Errorf("%s → %s landed on %q; an ambiguous or self table must not", e.FromTable, e.To, e.From)
		}
	}
	// The control: the model's own columns came through, so the empty From is a
	// decision and not a missing answer.
	if len(g.ColumnLineage.Edges) != 2 || g.ColumnLineage.Models[0].Columns == nil {
		t.Fatalf("edges %+v models %+v", g.ColumnLineage.Edges, g.ColumnLineage.Models)
	}
}

func TestAssetGraph_ColumnLineageAbsentUntilThereIsSomethingToSay(t *testing.T) {
	in := assetGraphInput{Models: []modelAsset{{ID: "m1", ConnectionID: connA, SQLText: "SELECT 1"}}}
	if g := buildAssetGraph(in); g.ColumnLineage != nil {
		t.Fatalf("column_lineage with nothing saved or being read: %+v", g.ColumnLineage)
	}
	in.ColumnLineagePending = true
	g := buildAssetGraph(in)
	if g.ColumnLineage == nil || !g.ColumnLineage.Pending {
		t.Fatalf("a read under way is not reported: %+v", g.ColumnLineage)
	}
	b, _ := json.Marshal(g.ColumnLineage)
	if !strings.Contains(string(b), `"models":[]`) || !strings.Contains(string(b), `"edges":[]`) {
		t.Errorf("pending column_lineage should carry empty lists, got %s", b)
	}
}

func TestAssetGraph_ColumnLineageStopsAtTheEdgeLimit(t *testing.T) {
	edges := make([]columnLineageEdge, columnLineageGraphEdgeLimit+5)
	for i := range edges {
		edges[i] = columnLineageEdge{Table: []string{"t"}, Column: "c", ToColumn: "c", Type: "DIRECT", Subtype: "IDENTITY"}
	}
	g := buildAssetGraph(assetGraphInput{
		Models:        []modelAsset{{ID: "m1", ConnectionID: connA, SQLText: "SELECT c FROM t"}},
		ColumnLineage: map[string]savedColumnLineage{"m1": {Status: columnLineageStatusOK, Edges: edges}},
	})
	if len(g.ColumnLineage.Edges) != columnLineageGraphEdgeLimit || !g.ColumnLineage.Truncated {
		t.Fatalf("%d edges, truncated %v; want %d and truncated", len(g.ColumnLineage.Edges), g.ColumnLineage.Truncated, columnLineageGraphEdgeLimit)
	}
}

// --- loading, and when a model is read again -------------------------------------

func stubColumnLineageRefresh(t *testing.T, busy bool) *[]columnLineageJob {
	t.Helper()
	var got []columnLineageJob
	prev := refreshColumnLineage
	refreshColumnLineage = func(_ string, jobs []columnLineageJob) bool {
		got = append(got, jobs...)
		return busy
	}
	t.Cleanup(func() { refreshColumnLineage = prev })
	return &got
}

var columnLineageRowCols = []string{"saved_query_id", "sql_hash", "status", "reason", "result", "had_schema", "computed_at"}

func TestLoadColumnLineage_ReadsAgainOnlyWhatIsMissingOrOld(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	jobs := stubColumnLineageRefresh(t, true)

	models := []modelAsset{
		{ID: "fresh", ConnectorType: "postgresql", ConnectionID: connA, SQLText: "SELECT 1"},
		{ID: "edited", ConnectorType: "postgresql", ConnectionID: connA, SQLText: "SELECT 2"},
		{ID: "new", ConnectorType: "postgresql", ConnectionID: connA, SQLText: "SELECT 3"},
		{ID: "noschema", ConnectorType: "postgresql", ConnectionID: connA, SQLText: "SELECT 4"},
		{ID: "blank", ConnectorType: "postgresql", ConnectionID: connA, SQLText: "  "},
	}
	hash := func(i int) string { return columnLineageSQLHash(models[i].ConnectorType, models[i].SQLText) }
	now := time.Now()
	mock.ExpectQuery(`FROM model_column_lineage`).
		WithArgs("ws", `{"fresh","edited","new","noschema"}`).
		WillReturnRows(sqlmock.NewRows(columnLineageRowCols).
			AddRow("fresh", hash(0), "ok", "", []byte(`{"columns":["a"],"edges":[],"truncated":false}`), true, now.Add(-time.Hour)).
			AddRow("edited", columnLineageSQLHash("postgresql", "SELECT old"), "ok", "", []byte(`{"columns":["old"]}`), true, now).
			AddRow("noschema", hash(3), "partial", "no columns", []byte(`{"columns":["b"]}`), false, now.Add(-time.Hour)))

	got, err := loadColumnLineage(context.Background(), database, "ws", models)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	if _, ok := got.ByModel["edited"]; ok {
		t.Error("the answer for the old SQL is shown for the new")
	}
	if s := got.ByModel["noschema"]; s.Status != "partial" || s.Reason != "no columns" || !reflect.DeepEqual(s.Columns, []string{"b"}) {
		t.Errorf("an old answer is shown while it is read again; got %+v", s)
	}
	if s := got.ByModel["fresh"]; !reflect.DeepEqual(s.Columns, []string{"a"}) {
		t.Errorf("fresh answer: %+v", s)
	}
	var ids []string
	for _, j := range *jobs {
		ids = append(ids, j.ModelID)
		if j.ConnectionID != connA || j.Hash != columnLineageSQLHash(j.ConnectorType, j.SQL) {
			t.Errorf("job %+v does not carry its model", j)
		}
	}
	if !reflect.DeepEqual(ids, []string{"edited", "new", "noschema"}) {
		t.Errorf("read again %v, want the edited, the new and the schema-less one", ids)
	}
	if !got.Pending {
		t.Error("models with nothing to show and a read under way are not pending")
	}
}

func TestLoadColumnLineage_PendingOnlyForAModelWithNothingToShow(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  bool // the model has an old answer
		busy bool // refreshColumnLineage started or found a read
		want bool
	}{
		{"old answer shown while read again", true, true, false},
		{"nothing yet, read under way", false, true, true},
		{"nothing yet, backing off after a failed read", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			stubColumnLineageRefresh(t, tc.busy)
			m := modelAsset{ID: "m", ConnectorType: "bigquery", SQLText: "SELECT 1"}
			rows := sqlmock.NewRows(columnLineageRowCols)
			if tc.row {
				rows.AddRow("m", columnLineageSQLHash(m.ConnectorType, m.SQLText), "ok", "", []byte(`{}`), true, time.Now().Add(-48*time.Hour))
			}
			mock.ExpectQuery(`FROM model_column_lineage`).WillReturnRows(rows)
			got, err := loadColumnLineage(context.Background(), database, "ws", []modelAsset{m})
			if err != nil {
				t.Fatal(err)
			}
			if got.Pending != tc.want {
				t.Errorf("pending = %v, want %v", got.Pending, tc.want)
			}
		})
	}
}

func TestLoadColumnLineage_NoModelsNoQuery(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	jobs := stubColumnLineageRefresh(t, true)
	got, err := loadColumnLineage(context.Background(), database, "ws", []modelAsset{{ID: "m", SQLText: ""}})
	if err != nil || got.Pending || len(got.ByModel) != 0 || len(*jobs) != 0 {
		t.Fatalf("got %+v, %v, jobs %v", got, err, *jobs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// One read per workspace at a time, and none while backing off: neither branch starts
// a goroutine, so neither needs a database.
func TestRefreshColumnLineage_OneReadAtATimeAndBacksOff(t *testing.T) {
	const ws = "refresh-test-ws"
	t.Cleanup(func() {
		columnLineageRefreshing.Delete(ws)
		columnLineageFailed.Delete(ws)
	})
	columnLineageRefreshing.Store(ws, struct{}{})
	if !refreshColumnLineage(ws, []columnLineageJob{{ModelID: "m"}}) {
		t.Error("a read under way is not reported as one")
	}
	columnLineageRefreshing.Delete(ws)

	columnLineageFailed.Store(ws, time.Now())
	if refreshColumnLineage(ws, []columnLineageJob{{ModelID: "m"}}) {
		t.Error("a workspace backing off after a failed read started another")
	}
	if _, started := columnLineageRefreshing.Load(ws); started {
		t.Error("a read was started while backing off")
	}
}

// --- the read: what is sent, and what is saved ------------------------------------

// columnLineageServer stands in for llm-service. answer gets each request's models and
// says what to answer.
func columnLineageServer(t *testing.T, answer func([]columnLineageRequestModel) []columnLineageAnswer) (*[][]columnLineageRequestModel, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var batches [][]columnLineageRequestModel
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/lineage/columns" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Internal-Secret") != "lineage-test-secret" {
			http.Error(w, "no secret", http.StatusUnauthorized)
			return
		}
		if r.Header.Get(workspaceHeader) != "ws" {
			http.Error(w, "no workspace", http.StatusBadRequest)
			return
		}
		var req struct {
			Models []columnLineageRequestModel `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		batches = append(batches, req.Models)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"models": answer(req.Models)})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("LLM_SERVICE_URL", srv.URL)
	t.Setenv("INTERNAL_SERVICE_SECRET", "lineage-test-secret")
	return &batches, &mu
}

func stubColumnLineageSchemaIndex(t *testing.T, byConnection map[string]*cache.ExplorerSchemaIndex) {
	t.Helper()
	prev := columnLineageSchemaIndex
	columnLineageSchemaIndex = func(_ context.Context, _ *sql.DB, _ string, connectionID string) (*cache.ExplorerSchemaIndex, error) {
		if idx, ok := byConnection[connectionID]; ok {
			return idx, nil
		}
		return nil, errors.New("connection unreachable")
	}
	t.Cleanup(func() { columnLineageSchemaIndex = prev })
}

// jsonResult matches a result argument whose JSON decodes to want.
type jsonResult struct{ want savedColumnLineage }

func (m jsonResult) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var got savedColumnLineage
	return json.Unmarshal([]byte(s), &got) == nil && reflect.DeepEqual(got, m.want)
}

func expectColumnLineageSave(mock sqlmock.Sqlmock, j columnLineageJob, status, reason string, result savedColumnLineage, schemaHash any, hadSchema bool) {
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO model_column_lineage`).
		WithArgs(j.ModelID, "ws", j.Hash, status, reason, jsonResult{result}, schemaHash, hadSchema).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Older versions go, except one a recorded run still names.
	mock.ExpectExec(`DELETE FROM model_column_lineage(.|\n)*sql_hash <> \$3(.|\n)*NOT EXISTS \(\s*SELECT 1 FROM lineage_events`).
		WithArgs(j.ModelID, "ws", j.Hash).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
}

type nonEmpty struct{}

func (nonEmpty) Match(v driver.Value) bool { s, ok := v.(string); return ok && s != "" }

func TestComputeColumnLineage_SendsEachModelWithItsTablesAndSavesTheAnswers(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	job := func(id, ct, conn, sqlText string) columnLineageJob {
		return columnLineageJob{ModelID: id, SQL: sqlText, ConnectorType: ct, ConnectionID: conn, Hash: columnLineageSQLHash(ct, sqlText)}
	}
	read := job("m-read", "azure-postgres", connA, "SELECT amount FROM analytics.orders")
	blind := job("m-blind", "postgresql", connB, "SELECT * FROM t")
	huge := job("m-huge", "postgresql", connA, "SELECT 1 -- "+strings.Repeat("x", columnLineageMaxSQL))
	doc := job("m-doc", "mongodb", connA, `{"collection":"orders","pipeline":[]}`)

	stubColumnLineageSchemaIndex(t, map[string]*cache.ExplorerSchemaIndex{
		connA: {Tables: []cache.ExplorerTableIndex{
			lineageIndexTable("analytics", "orders", "id", "amount"),
			lineageIndexTable("analytics", "customers", "id"),
		}},
	})
	batches, _ := columnLineageServer(t, func(models []columnLineageRequestModel) []columnLineageAnswer {
		return []columnLineageAnswer{
			{ID: "m-read", Status: "ok", Columns: []string{"amount"}, Edges: []columnLineageEdge{
				{Table: []string{"analytics", "orders"}, Column: "amount", ToColumn: "amount", Type: "DIRECT", Subtype: "IDENTITY"},
			}},
			{ID: "m-blind", Status: "partial", Reason: "SELECT * of a table whose columns are unknown"},
			{ID: "m-doc", Status: "bogus"},
			// An answer about a model that was not asked about is not saved.
			{ID: "someone-else", Status: "ok"},
		}
	})

	expectColumnLineageSave(mock, huge, "failed", "the model's SQL is too long to read for column lineage",
		savedColumnLineage{Columns: []string{}, Edges: []columnLineageEdge{}}, "", true)
	expectColumnLineageSave(mock, read, "ok", "", savedColumnLineage{Columns: []string{"amount"}, Edges: []columnLineageEdge{
		{Table: []string{"analytics", "orders"}, Column: "amount", ToColumn: "amount", Type: "DIRECT", Subtype: "IDENTITY"},
	}}, nonEmpty{}, true)
	// Its connection's columns could not be read: saved as read without them.
	expectColumnLineageSave(mock, blind, "partial", "SELECT * of a table whose columns are unknown",
		savedColumnLineage{Columns: []string{}, Edges: []columnLineageEdge{}}, "", false)
	// An unknown status is saved as failed, which the table's CHECK allows.
	expectColumnLineageSave(mock, doc, "failed", "", savedColumnLineage{Columns: []string{}, Edges: []columnLineageEdge{}}, "", true)

	if err := computeColumnLineage(withLLMWorkspace(context.Background(), "ws"), database, "ws", []columnLineageJob{read, blind, huge, doc}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	if len(*batches) != 1 {
		t.Fatalf("%d calls, want 1", len(*batches))
	}
	sent := map[string]columnLineageRequestModel{}
	for _, m := range (*batches)[0] {
		sent[m.ID] = m
	}
	if _, ok := sent["m-huge"]; ok || len(sent) != 3 {
		t.Fatalf("sent %v; the oversize model must not be sent", sent)
	}
	if got := sent["m-read"]; got.ConnectorType != "postgresql" ||
		!reflect.DeepEqual(got.Tables, []columnLineageRequestTable{{Schema: "analytics", Name: "orders", Columns: []string{"id", "amount"}}}) {
		t.Errorf("m-read sent as %+v; want the PostgreSQL dialect and only the table it reads", got)
	}
	if len(sent["m-blind"].Tables) != 0 || len(sent["m-doc"].Tables) != 0 {
		t.Errorf("tables sent with no schema index or for a document model: %+v %+v", sent["m-blind"], sent["m-doc"])
	}
}

func TestComputeColumnLineage_BatchesWhatLLMServiceTakes(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stubColumnLineageSchemaIndex(t, nil)
	batches, mu := columnLineageServer(t, func([]columnLineageRequestModel) []columnLineageAnswer { return nil })

	var jobs []columnLineageJob
	for i := 0; i < columnLineageBatch+1; i++ {
		jobs = append(jobs, columnLineageJob{ModelID: "m", SQL: "SELECT 1", ConnectorType: "postgresql"})
	}
	if err := computeColumnLineage(withLLMWorkspace(context.Background(), "ws"), database, "ws", jobs); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*batches) != 2 || len((*batches)[0]) != columnLineageBatch || len((*batches)[1]) != 1 {
		t.Fatalf("batches of %d", func() []int {
			var n []int
			for _, b := range *batches {
				n = append(n, len(b))
			}
			return n
		}())
	}
}

func TestComputeColumnLineage_AFailedCallSavesNothing(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stubColumnLineageSchemaIndex(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	t.Setenv("LLM_SERVICE_URL", srv.URL)

	err = computeColumnLineage(withLLMWorkspace(context.Background(), "ws"), database, "ws", []columnLineageJob{{ModelID: "m", SQL: "SELECT 1", ConnectorType: "postgresql"}})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want the status", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
