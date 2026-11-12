package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"api-gateway/internal/cache"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rsync-ai/shared/crypto"
)

// historyColumns is a CDC append table as the sink writes it (kafka-sink-worker
// cdcIdentityColumnTypes): the source's columns plus five _rsync_cdc_ columns.
func historyColumns() []silverColumn {
	return []silverColumn{
		{"id", "integer"}, {"Status", "text"}, {"amount", "numeric"},
		{"_rsync_cdc_op", "varchar(8)"}, {"_rsync_cdc_event_ts", "bigint"}, {"_rsync_cdc_lsn", "bigint"},
		{"_rsync_cdc_gtid", "varchar(255)"}, {"_rsync_cdc_seq", "varchar(40)"},
	}
}

func TestBuildSilverPlan_PostgresHistory(t *testing.T) {
	src := &silverSource{Schema: "public", Table: "orders", Columns: historyColumns()}
	plan, refusal := buildSilverPlan("postgres", src, []string{"ID"})
	if refusal != nil {
		t.Fatal(refusal)
	}
	want := `SELECT "id", "Status", "amount"
FROM (
  SELECT t.*, ROW_NUMBER() OVER (PARTITION BY "id" ORDER BY "_rsync_cdc_seq" DESC) AS rsync_rn
  FROM "public"."orders" t
) rsync_latest
WHERE rsync_latest.rsync_rn = 1
  AND (rsync_latest."_rsync_cdc_op" IS NULL OR rsync_latest."_rsync_cdc_op" <> 'D')`
	if plan.SQLText != want {
		t.Fatalf("sql:\n%s\nwant:\n%s", plan.SQLText, want)
	}
	if plan.Shape != silverShapeHistory {
		t.Fatalf("shape = %s", plan.Shape)
	}
	// The key comes back as the catalog spells it, whatever case the caller used.
	if !reflect.DeepEqual(plan.Keys, []string{"id"}) || !reflect.DeepEqual(plan.Columns, []string{"id", "Status", "amount"}) {
		t.Fatalf("keys %v columns %v", plan.Keys, plan.Columns)
	}
	if plan.TargetTable != "public.orders_silver" {
		t.Fatalf("target = %q", plan.TargetTable)
	}
}

// Every SQL dialect a model runs on gets the same three shapes, quoted its own way and
// qualified the way its catalog names a table.
func TestBuildSilverPlan_EveryDialect(t *testing.T) {
	cases := []struct {
		dialect, schema string
		from, seq, col  string
	}{
		{"postgres", "public", `"public"."orders" t`, `"_rsync_cdc_seq"`, `"Status"`},
		{"mysql", "shop", "`shop`.`orders` t", "`_rsync_cdc_seq`", "`Status`"},
		{"sqlserver", "dbo", "[dbo].[orders] t", "[_rsync_cdc_seq]", "[Status]"},
		{"bigquery", "analytics", "`analytics`.`orders` t", "`_rsync_cdc_seq`", "`Status`"},
		{"snowflake", "PUBLIC", `"PUBLIC"."orders" t`, `"_rsync_cdc_seq"`, `"Status"`},
		{"databricks", "main.bronze", "`main`.`bronze`.`orders` t", "`_rsync_cdc_seq`", "`Status`"},
		{"clickhouse", "default", "`default`.`orders` t", "`_rsync_cdc_seq`", "`Status`"},
		{"oracle", "APP", `"APP"."orders" t`, `"_rsync_cdc_seq"`, `"Status"`},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			src := &silverSource{Schema: tc.schema, Table: "orders", Columns: historyColumns()}
			plan, refusal := buildSilverPlan(tc.dialect, src, []string{"id"})
			if refusal != nil {
				t.Fatal(refusal)
			}
			for _, part := range []string{"FROM " + tc.from, "ORDER BY " + tc.seq + " DESC"} {
				if !strings.Contains(plan.SQLText, part) {
					t.Fatalf("history sql lacks %q:\n%s", part, plan.SQLText)
				}
			}
			if !strings.Contains(plan.SQLText, tc.col) {
				t.Fatalf("history sql lacks the quoted column %s:\n%s", tc.col, plan.SQLText)
			}
			// Oracle accepts no AS before a table alias, so no shape may use one.
			if strings.Contains(plan.SQLText, " AS t") || strings.Contains(plan.SQLText, ") AS rsync_latest") {
				t.Fatalf("table alias with AS:\n%s", plan.SQLText)
			}
			// rsync.ai's own columns never reach silver.
			if strings.Contains(strings.SplitN(plan.SQLText, "\n", 2)[0], "_rsync_") {
				t.Fatalf("select list keeps a bookkeeping column:\n%s", plan.SQLText)
			}

			current := &silverSource{Schema: tc.schema, Table: "orders", Columns: []silverColumn{{"id", "int"}, {"Status", "text"}}}
			plan, refusal = buildSilverPlan(tc.dialect, current, nil)
			if refusal != nil || plan.Shape != silverShapeCurrent {
				t.Fatalf("current: %v %v", plan, refusal)
			}
			if want := "FROM " + strings.TrimSuffix(tc.from, " t"); !strings.HasSuffix(plan.SQLText, want) {
				t.Fatalf("current sql = %q, want suffix %q", plan.SQLText, want)
			}
		})
	}
}

// A warehouse current table keeps a deleted key's row flagged _rsync_deleted; how the
// flag reads depends on the type the warehouse gave it.
func TestBuildSilverPlan_SoftDelete(t *testing.T) {
	cases := []struct {
		dialect, flagType, want string
	}{
		{"bigquery", "BOOL", "WHERE COALESCE(`_rsync_deleted`, FALSE) = FALSE"},
		{"snowflake", "BOOLEAN", `WHERE COALESCE("_rsync_deleted", FALSE) = FALSE`},
		{"sqlserver", "bit", "WHERE COALESCE([_rsync_deleted], 0) = 0"},
		{"mysql", "tinyint(1)", "WHERE COALESCE(`_rsync_deleted`, 0) = 0"},
		{"clickhouse", "Nullable(UInt8)", "WHERE COALESCE(`_rsync_deleted`, 0) = 0"},
		{"oracle", "NUMBER", `WHERE COALESCE("_rsync_deleted", 0) = 0`},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			src := &silverSource{Schema: "s", Table: "orders", Columns: []silverColumn{
				{"id", "int"}, {"_rsync_deleted", tc.flagType}, {"_rsync_synced_at", "timestamp"},
			}}
			plan, refusal := buildSilverPlan(tc.dialect, src, []string{"ignored_on_this_shape"})
			if refusal != nil {
				t.Fatal(refusal)
			}
			if plan.Shape != silverShapeSoftDelete || !strings.HasSuffix(plan.SQLText, tc.want) {
				t.Fatalf("shape %s sql:\n%s\nwant suffix %s", plan.Shape, plan.SQLText, tc.want)
			}
			if len(plan.Keys) != 0 || !reflect.DeepEqual(plan.Columns, []string{"id"}) {
				t.Fatalf("keys %v columns %v", plan.Keys, plan.Columns)
			}
		})
	}
}

func TestBuildSilverPlan_Refusals(t *testing.T) {
	history := func() *silverSource {
		return &silverSource{Schema: "public", Table: "orders", Columns: historyColumns()}
	}
	cases := []struct {
		name    string
		dialect string
		src     *silverSource
		keys    []string
		code    string
	}{
		{"history needs keys", "postgres", history(), nil, "keys_required"},
		{"unknown key", "postgres", history(), []string{"nope"}, "unknown_key"},
		{"bookkeeping key", "postgres", history(), []string{"_rsync_cdc_seq"}, "unknown_key"},
		{"duplicate key", "postgres", history(), []string{"id", "ID"}, "duplicate_key"},
		{"only bookkeeping", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{"_rsync_cdc_seq", "text"}}}, nil, "no_columns"},
		{"columns differ by case", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{"id", "int"}, {"ID", "int"}}}, nil, "ambiguous_column"},
		{"row number name taken", "postgres", &silverSource{Table: "t", Columns: append(historyColumns(), silverColumn{"RSYNC_RN", "int"})}, []string{"id"}, "reserved_column"},
		{"text delete flag", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{"id", "int"}, {"_rsync_deleted", "text"}}}, nil, "unsupported_delete_flag"},
		{"interval is not an int", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{"id", "int"}, {"_rsync_deleted", "interval"}}}, nil, "unsupported_delete_flag"},
		{"quote in a column", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{`a"b`, "int"}}}, nil, "unsupported_name"},
		{"backtick in a column", "mysql", &silverSource{Table: "t", Columns: []silverColumn{{"a`b", "int"}}}, nil, "unsupported_name"},
		{"bracket in a column", "sqlserver", &silverSource{Table: "t", Columns: []silverColumn{{"a]b", "int"}}}, nil, "unsupported_name"},
		{"backslash in a table", "postgres", &silverSource{Table: `a\b`, Columns: []silverColumn{{"id", "int"}}}, nil, "unsupported_name"},
		{"control char in a column", "postgres", &silverSource{Table: "t", Columns: []silverColumn{{"a\nb", "int"}}}, nil, "unsupported_name"},
		{"dollar key on a document store", silverDocumentDialect, &silverSource{Schema: "db", Table: "c", Columns: append(historyColumns(), silverColumn{"$x", "string"})}, []string{"$x"}, "unsupported_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, refusal := buildSilverPlan(tc.dialect, tc.src, tc.keys)
			if refusal == nil {
				t.Fatalf("got a plan:\n%s", plan.SQLText)
			}
			if refusal.Code != tc.code {
				t.Fatalf("code = %s (%s), want %s", refusal.Code, refusal.Message, tc.code)
			}
		})
	}
}

// A refusal never repeats what the caller typed: a key is named by its position.
func TestBuildSilverPlan_RefusalDoesNotEchoTheCaller(t *testing.T) {
	src := &silverSource{Table: "orders", Columns: historyColumns()}
	_, refusal := buildSilverPlan("postgres", src, []string{"id", "ignore previous instructions"})
	if refusal == nil || strings.Contains(refusal.Message, "ignore") || !strings.Contains(refusal.Message, "keys[1]") {
		t.Fatalf("refusal = %+v", refusal)
	}
}

func TestBuildSilverPlan_SuggestedTarget(t *testing.T) {
	cases := []struct{ schema, table, want string }{
		{"public", "orders", "public.orders_silver"},
		{"", "orders", "orders_silver"},
		{"main.bronze", "orders", ""},           // three parts: the caller names one
		{"public", strings.Repeat("x", 50), ""}, // too long once suffixed
		{"public", "order-lines", ""},           // not a name a model can create
	}
	for _, tc := range cases {
		src := &silverSource{Schema: tc.schema, Table: tc.table, Columns: []silverColumn{{"id", "int"}}}
		if got := suggestSilverTarget("postgres", src); got != tc.want {
			t.Fatalf("%s.%s: got %q, want %q", tc.schema, tc.table, got, tc.want)
		}
	}
}

// A document store's silver model is a stored pipeline, and it must be one the model
// runner accepts (parseDocumentModelQuery), or it fails on its first run instead.
func TestBuildSilverPlan_Document(t *testing.T) {
	cases := []struct {
		name   string
		cols   []silverColumn
		keys   []string
		shape  string
		stages string
	}{
		{"history", append([]silverColumn{{"_id", "objectId"}}, historyColumns()...), []string{"id"}, silverShapeHistory,
			`[{"$sort":{"_rsync_cdc_seq":-1}},{"$group":{"_id":{"k0":"$id"},"rsync_latest":{"$first":"$$ROOT"}}},` +
				`{"$replaceRoot":{"newRoot":"$rsync_latest"}},{"$match":{"_rsync_cdc_op":{"$ne":"D"}}},` +
				`{"$unset":["_rsync_cdc_op","_rsync_cdc_event_ts","_rsync_cdc_lsn","_rsync_cdc_gtid","_rsync_cdc_seq"]}]`},
		{"soft delete", []silverColumn{{"_id", "objectId"}, {"_rsync_deleted", "bool"}}, nil, silverShapeSoftDelete,
			`[{"$match":{"_rsync_deleted":{"$ne":true}}},{"$unset":["_rsync_deleted"]}]`},
		{"current", []silverColumn{{"_id", "objectId"}, {"name", "string"}}, nil, silverShapeCurrent,
			`[{"$match":{}}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &silverSource{Schema: "shop", Table: "orders", Columns: tc.cols}
			plan, refusal := buildSilverPlan(silverDocumentDialect, src, tc.keys)
			if refusal != nil {
				t.Fatal(refusal)
			}
			var stored documentModelQuery
			if err := json.Unmarshal([]byte(plan.SQLText), &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Database != "shop" || stored.Collection != "orders" || plan.Shape != tc.shape {
				t.Fatalf("stored %+v shape %s", stored, plan.Shape)
			}
			if string(stored.Pipeline) != tc.stages {
				t.Fatalf("pipeline:\n%s\nwant:\n%s", stored.Pipeline, tc.stages)
			}
			if _, ferr := parseDocumentModelQuery(plan.SQLText); ferr != nil {
				t.Fatalf("the model runner would refuse it: %s", ferr.Message)
			}
		})
	}
}

func TestFindSilverTable(t *testing.T) {
	tables := []cache.ExplorerTableIndex{
		{Schema: "public", Name: "orders"},
		{Schema: "archive", Name: "orders"},
		{Schema: "public", Name: "Users"},
		{Schema: "public", Name: "users"},
		{Schema: "main.bronze", Name: "events"},
		{Schema: "shop", Name: "a.b"}, // a collection name with a dot in it
	}
	cases := []struct {
		arg, wantSchema, wantName, code string
	}{
		{"public.orders", "public", "orders", ""},
		{"PUBLIC.ORDERS", "public", "orders", ""},
		{"orders", "", "", "ambiguous_table"},
		{"users", "public", "users", ""}, // two match apart from case; the exact one wins
		{"USERS", "", "", "ambiguous_table"},
		{"bronze.events", "main.bronze", "events", ""},
		{"main.bronze.events", "main.bronze", "events", ""},
		{"shop.a.b", "shop", "a.b", ""},
		{"missing", "", "", "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			got, refusal := findSilverTable(tables, tc.arg)
			switch {
			case tc.code == "not_found":
				if got != nil || refusal != nil {
					t.Fatalf("got %+v %v", got, refusal)
				}
			case tc.code != "":
				if refusal == nil || refusal.Code != tc.code {
					t.Fatalf("got %+v %v, want %s", got, refusal, tc.code)
				}
			default:
				if refusal != nil || got == nil || got.Schema != tc.wantSchema || got.Name != tc.wantName {
					t.Fatalf("got %+v %v", got, refusal)
				}
			}
		})
	}
}

func TestSilverCatalogSQL(t *testing.T) {
	cases := []struct {
		dialect, schema, table, want, code string
	}{
		{"bigquery", "analytics", "Orders", "SELECT table_schema, table_name, column_name, data_type FROM `analytics`.INFORMATION_SCHEMA.COLUMNS WHERE LOWER(table_name) = 'orders' ORDER BY table_name, ordinal_position", ""},
		{"bigquery", "", "orders", "", "dataset_required"},
		{"snowflake", "", "orders", "SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, DATA_TYPE FROM INFORMATION_SCHEMA.COLUMNS WHERE UPPER(TABLE_NAME) = 'ORDERS' AND TABLE_SCHEMA <> 'INFORMATION_SCHEMA' ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION", ""},
		{"snowflake", "public", "orders", "AND UPPER(TABLE_SCHEMA) = 'PUBLIC' ORDER BY", ""},
		{"clickhouse", "", "orders", "WHERE lower(table) = 'orders' AND database = currentDatabase() ORDER BY database, table, position", ""},
		{"clickhouse", "Bronze", "orders", "AND lower(database) = 'bronze' ORDER BY", ""},
		{"oracle", "", "orders", "WHERE UPPER(TABLE_NAME) = 'ORDERS' AND OWNER = SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') ORDER BY OWNER, TABLE_NAME, COLUMN_ID", ""},
		{"oracle", "app", "orders", "AND UPPER(OWNER) = 'APP' ORDER BY", ""},
		// The names become literals, so anything that could close one is refused.
		{"snowflake", "", "o'rders", "", "unsupported_name"},
		{"oracle", "a'pp", "orders", "", "unsupported_name"},
		{"clickhouse", "", "orders--", "", "unsupported_name"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect+"/"+tc.schema+"/"+tc.table, func(t *testing.T) {
			got, refusal := silverCatalogSQL(tc.dialect, tc.schema, tc.table)
			if tc.code != "" {
				if refusal == nil || refusal.Code != tc.code {
					t.Fatalf("got %q %v, want %s", got, refusal, tc.code)
				}
				return
			}
			if refusal != nil || !strings.Contains(got, tc.want) {
				t.Fatalf("got %q %v, want it to contain %q", got, refusal, tc.want)
			}
		})
	}
}

func TestSilverSourceFromCatalog(t *testing.T) {
	// Oracle and Snowflake answer with upper-case column names.
	rows := []map[string]interface{}{
		{"OWNER": "APP", "TABLE_NAME": "ORDERS", "COLUMN_NAME": "ID", "DATA_TYPE": "NUMBER"},
		{"OWNER": "APP", "TABLE_NAME": "ORDERS", "COLUMN_NAME": "_rsync_deleted", "DATA_TYPE": "NUMBER"},
	}
	src, refusal := silverSourceFromCatalog(rows)
	if refusal != nil || src.Schema != "APP" || src.Table != "ORDERS" || len(src.Columns) != 2 || src.Columns[1].Type != "NUMBER" {
		t.Fatalf("got %+v %v", src, refusal)
	}
	rows = append(rows, map[string]interface{}{"OWNER": "OTHER", "TABLE_NAME": "ORDERS", "COLUMN_NAME": "ID", "DATA_TYPE": "NUMBER"})
	if _, refusal := silverSourceFromCatalog(rows); refusal == nil || refusal.Code != "ambiguous_table" {
		t.Fatalf("two owners: %v", refusal)
	}
	if src, refusal := silverSourceFromCatalog(nil); src != nil || refusal != nil {
		t.Fatalf("no rows: %+v %v", src, refusal)
	}
}

// ---------------------------------------------------------------------------
// The handler
// ---------------------------------------------------------------------------

const silverPath = "/explorer/connections/" + savedQueryConn + "/silver-sql"

func expectSilverConnection(mock sqlmock.Sqlmock, connectorType, config string) {
	mock.ExpectQuery(`SELECT connector_type, config\s+FROM connections\s+WHERE id = \$1 AND workspace_id = \$2`).
		WithArgs(savedQueryConn, wsScopeWS).
		WillReturnRows(sqlmock.NewRows([]string{"connector_type", "config"}).AddRow(connectorType, config))
}

func stubSilverIndex(t *testing.T, tables []cache.ExplorerTableIndex) *[]bool {
	t.Helper()
	prev := silverSchemaIndex
	var calls []bool
	silverSchemaIndex = func(_ context.Context, connectionID, workspaceID, _, _ string, refresh bool) (*cache.ExplorerSchemaIndex, error) {
		if connectionID != savedQueryConn || workspaceID != wsScopeWS {
			t.Fatalf("index read for %s in %s", connectionID, workspaceID)
		}
		calls = append(calls, refresh)
		return &cache.ExplorerSchemaIndex{Tables: tables}, nil
	}
	t.Cleanup(func() { silverSchemaIndex = prev })
	return &calls
}

func TestGetSilverSQL_FromTheSchemaIndex(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectSilverConnection(mock, "postgresql", "enc")
	var cols []cache.ExplorerColumnIndex
	for _, c := range historyColumns() {
		cols = append(cols, cache.ExplorerColumnIndex{Name: c.Name, Type: c.Type})
	}
	calls := stubSilverIndex(t, []cache.ExplorerTableIndex{{Schema: "public", Name: "orders", Columns: cols}})

	// A viewer may ask: the route writes nothing.
	r := savedQueryRouter(http.MethodPost, "/explorer/connections/:id/silver-sql", "viewer", GetSilverSQL)
	w := doJSON(r, http.MethodPost, silverPath, map[string]any{"table": "public.orders", "keys": []string{"id"}})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["shape"] != silverShapeHistory || resp["dialect"] != "postgres" || resp["source_table"] != "public.orders" ||
		resp["target_table"] != "public.orders_silver" || !strings.Contains(resp["sql_text"].(string), "ROW_NUMBER()") {
		t.Fatalf("resp = %v", resp)
	}
	if !reflect.DeepEqual(*calls, []bool{false}) {
		t.Fatalf("index reads = %v, want the cached one only", *calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A table the cached index predates is looked for once more in a fresh read before it
// is called missing.
func TestGetSilverSQL_MissRefreshesOnce(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectSilverConnection(mock, "postgresql", "enc")
	calls := stubSilverIndex(t, []cache.ExplorerTableIndex{{Schema: "public", Name: "customers"}})

	r := savedQueryRouter(http.MethodPost, "/explorer/connections/:id/silver-sql", "viewer", GetSilverSQL)
	w := doJSON(r, http.MethodPost, silverPath, map[string]any{"table": "orders"})
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "table_not_found") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(*calls, []bool{false, true}) {
		t.Fatalf("index reads = %v, want cached then fresh", *calls)
	}
}

// A warehouse the schema index does not cover is read from its own catalog.
func TestGetSilverSQL_FromAWarehouseCatalog(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key-0123456789ab")
	enc, err := crypto.EncryptString(`{"project_id":"p"}`)
	if err != nil {
		t.Fatal(err)
	}
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	expectSilverConnection(mock, "bigquery", enc)

	prev := silverCatalogQuery
	var asked string
	silverCatalogQuery = func(_ context.Context, connectorType string, config map[string]interface{}, query string) ([]map[string]interface{}, error) {
		if connectorType != "bigquery" || config["project_id"] != "p" {
			t.Fatalf("catalog read on %s with %v", connectorType, config)
		}
		asked = query
		return []map[string]interface{}{
			{"table_schema": "analytics", "table_name": "orders", "column_name": "id", "data_type": "INT64"},
			{"table_schema": "analytics", "table_name": "orders", "column_name": "_rsync_deleted", "data_type": "BOOL"},
			{"table_schema": "analytics", "table_name": "orders", "column_name": "_rsync_synced_at", "data_type": "TIMESTAMP"},
		}, nil
	}
	t.Cleanup(func() { silverCatalogQuery = prev })

	r := savedQueryRouter(http.MethodPost, "/explorer/connections/:id/silver-sql", "viewer", GetSilverSQL)
	w := doJSON(r, http.MethodPost, silverPath, map[string]any{"table": "analytics.orders"})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(asked, "`analytics`.INFORMATION_SCHEMA.COLUMNS") {
		t.Fatalf("catalog query = %q", asked)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	want := "SELECT `id`\nFROM `analytics`.`orders`\nWHERE COALESCE(`_rsync_deleted`, FALSE) = FALSE"
	if resp["shape"] != silverShapeSoftDelete || resp["sql_text"] != want {
		t.Fatalf("resp = %v", resp)
	}
}

func TestGetSilverSQL_RequestRefusals(t *testing.T) {
	cases := []struct {
		name      string
		body      any
		connector string // "" = no connection lookup expected
		status    int
		code      string
	}{
		{"no table", map[string]any{}, "", http.StatusBadRequest, "invalid_request"},
		{"blank key", map[string]any{"table": "t", "keys": []string{" "}}, "", http.StatusBadRequest, "invalid_request"},
		{"too many keys", map[string]any{"table": "t", "keys": make([]string, silverMaxKeys+1)}, "", http.StatusBadRequest, "invalid_request"},
		{"not json", "nope", "", http.StatusBadRequest, "invalid_request"},
		{"api connector", map[string]any{"table": "t"}, "stripe", http.StatusBadRequest, "unsupported_connector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			if tc.connector != "" {
				expectSilverConnection(mock, tc.connector, "enc")
			}
			r := savedQueryRouter(http.MethodPost, "/explorer/connections/:id/silver-sql", "viewer", GetSilverSQL)
			w := doJSON(r, http.MethodPost, silverPath, tc.body)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("got %d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
