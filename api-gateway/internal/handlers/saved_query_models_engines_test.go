package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Models on the engines the gateway has no database/sql driver for
// ---------------------------------------------------------------------------
// BigQuery, Snowflake, ClickHouse and Oracle run each statement through the
// orchestrator and the connector's MCP `execute` tool; Databricks through its own
// Statement Execution API. None of them can hold a transaction around a rebuild, so
// the safety the driver path gets from transactional DDL has to come from the shape
// of the plan itself — these tests pin that shape per engine.

// modelEngineDialects is every modelDialect value that runs without a driver.
var modelEngineDialects = []string{"bigquery", "snowflake", "databricks", "clickhouse", "oracle"}

// fakeModelRunner records what a plan sent and fails on request.
type fakeModelRunner struct {
	mu       sync.Mutex
	execs    []string
	queries  []string
	failOn   string // exec fails when the statement contains this
	rows     []map[string]interface{}
	queryErr error
	affected *int64
}

func (f *fakeModelRunner) exec(_ context.Context, stmt string) (*int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, stmt)
	if f.failOn != "" && strings.Contains(stmt, f.failOn) {
		return nil, errors.New("engine refused the statement")
	}
	return f.affected, nil
}

func (f *fakeModelRunner) queryRows(_ context.Context, q string) ([]map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.rows, nil
}

func tableModel(owned bool) *savedQueryModel {
	return &savedQueryModel{ID: "m1", SQLText: "SELECT 1;", Materialization: matTable, TargetOwned: owned}
}

func TestModelDialect_EveryWarehouse(t *testing.T) {
	cases := map[string]string{
		"bigquery":   "bigquery",
		"BigQuery":   "bigquery",
		"databricks": "databricks",
		"snowflake":  "snowflake",
		"clickhouse": "clickhouse",
		"oracle":     "oracle",
		// Document stores need their own model path (an aggregation, not SQL).
		"mongodb":    "",
		"salesforce": "",
	}
	for in, want := range cases {
		if got := modelDialect(in); got != want {
			t.Errorf("modelDialect(%q) = %q, want %q", in, got, want)
		}
	}
}

// Snowflake and Oracle fold an unquoted identifier to upper case. Quoting the name
// as typed would make `orders` a different table from the ORDERS every other tool
// on that warehouse sees; upper-casing it inside the quotes resolves it exactly as
// the unquoted name would, and still survives a reserved word.
func TestQuoteModelIdent_EveryWarehouse(t *testing.T) {
	cases := []struct{ dialect, want string }{
		{"snowflake", `"ANALYTICS"."ORDERS"`},
		{"oracle", `"ANALYTICS"."ORDERS"`},
		{"bigquery", "`analytics`.`orders`"},
		{"databricks", "`analytics`.`orders`"},
		{"clickhouse", "`analytics`.`orders`"},
	}
	for _, tc := range cases {
		if got := quoteModelIdent(tc.dialect, "analytics", "orders"); got != tc.want {
			t.Errorf("[%s] quoteModelIdent = %s, want %s", tc.dialect, got, tc.want)
		}
	}
	if got := quoteModelIdent("oracle", "", "orders"); got != `"ORDERS"` {
		t.Errorf("an unqualified oracle target = %s, want \"ORDERS\"", got)
	}
}

// The collision guarantee, per engine: a first run has proven no ownership, so
// nothing in its plan may replace or retire whatever stands at the target name.
// Each engine's CREATE (or RENAME) onto an occupied name fails, and that refusal is
// what stops rsync.ai taking over a table it did not create.
func TestEngineModelPlan_UnownedNeverReplaces(t *testing.T) {
	for _, dialect := range modelEngineDialects {
		runner := &fakeModelRunner{}
		plan, err := buildEngineModelPlan(context.Background(), runner, dialect, "analytics", "orders", tableModel(false))
		if err != nil {
			t.Fatalf("[%s] %v", dialect, err)
		}
		all := strings.Join(plan.stmts, "\n")
		if strings.Contains(all, "OR REPLACE") {
			t.Errorf("[%s] an unowned run must not replace anything: %v", dialect, plan.stmts)
		}
		if strings.Contains(strings.ToLower(all), modelRetiredSuffix) {
			t.Errorf("[%s] an unowned run must not name the retired table: %v", dialect, plan.stmts)
		}
		if !strings.Contains(all, "CREATE TABLE") {
			t.Errorf("[%s] an unowned run must still build the table: %v", dialect, plan.stmts)
		}
		if len(runner.queries) != 0 {
			t.Errorf("[%s] an unowned run must not spend a round trip on the catalog: %v", dialect, runner.queries)
		}
		if strings.Contains(all, "SELECT 1;") {
			t.Errorf("[%s] the trailing semicolon must be stripped before the query is nested: %v", dialect, plan.stmts)
		}
	}
}

// The engines with an atomic CREATE OR REPLACE get the swap from the engine: the new
// table is built before the old one is let go, and a failed build leaves the old one
// in place. One statement, so there is no window between two renames to die in.
func TestEngineModelPlan_OwnedReplaceIsOneStatement(t *testing.T) {
	want := map[string]string{
		"bigquery":   "CREATE OR REPLACE TABLE `analytics`.`orders` AS SELECT 1",
		"snowflake":  `CREATE OR REPLACE TABLE "ANALYTICS"."ORDERS" AS SELECT 1`,
		"databricks": "CREATE OR REPLACE TABLE `analytics`.`orders` AS SELECT 1",
		"clickhouse": "CREATE OR REPLACE TABLE `analytics`.`orders` ENGINE = MergeTree ORDER BY tuple() AS SELECT 1",
	}
	for dialect, stmt := range want {
		plan, err := buildEngineModelPlan(context.Background(), &fakeModelRunner{}, dialect, "analytics", "orders", tableModel(true))
		if err != nil {
			t.Fatalf("[%s] %v", dialect, err)
		}
		if len(plan.stmts) != 1 || plan.stmts[0] != stmt {
			t.Errorf("[%s] owned plan = %v, want [%s]", dialect, plan.stmts, stmt)
		}
		if plan.sweep != "" {
			t.Errorf("[%s] one atomic statement leaves nothing to sweep, got %q", dialect, plan.sweep)
		}
	}
}

// ClickHouse's CREATE TABLE … AS SELECT is not atomic: a SELECT that fails part-way
// can leave the table behind. Built straight onto the target, that half-table would
// then sit at the name and every later first run would collide with it. Building
// under the scratch name and renaming in keeps the target untouched until the build
// is whole, and the RENAME still refuses an occupied name.
func TestEngineModelPlan_ClickHouseFirstRunBuildsAsideThenRenames(t *testing.T) {
	plan, err := buildEngineModelPlan(context.Background(), &fakeModelRunner{}, "clickhouse", "analytics", "orders", tableModel(false))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DROP TABLE IF EXISTS `analytics`.`orders__rsync_new`",
		"CREATE TABLE `analytics`.`orders__rsync_new` ENGINE = MergeTree ORDER BY tuple() AS SELECT 1",
		"RENAME TABLE `analytics`.`orders__rsync_new` TO `analytics`.`orders`",
	}
	if strings.Join(plan.stmts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan =\n%s\nwant\n%s", strings.Join(plan.stmts, "\n"), strings.Join(want, "\n"))
	}
	if plan.sweep != "DROP TABLE IF EXISTS `analytics`.`orders__rsync_new`" {
		t.Fatalf("a failed build must sweep the scratch table, sweep = %q", plan.sweep)
	}
}

// ---------------------------------------------------------------------------
// Oracle: DDL commits as it goes, and there is no CREATE OR REPLACE TABLE
// ---------------------------------------------------------------------------

func oraclePlan(t *testing.T, owned bool, present ...string) (engineModelPlan, *fakeModelRunner) {
	t.Helper()
	runner := &fakeModelRunner{}
	for _, name := range present {
		runner.rows = append(runner.rows, map[string]interface{}{"TABLE_NAME": name})
	}
	plan, err := buildEngineModelPlan(context.Background(), runner, "oracle", "analytics", "orders", tableModel(owned))
	if err != nil {
		t.Fatalf("oracle plan: %v", err)
	}
	return plan, runner
}

func TestOracleModelPlan_FirstRunIsOneAtomicCreate(t *testing.T) {
	plan, runner := oraclePlan(t, false)
	want := `CREATE TABLE "ANALYTICS"."ORDERS" AS SELECT 1`
	if len(plan.stmts) != 1 || plan.stmts[0] != want {
		t.Fatalf("plan = %v, want [%s]", plan.stmts, want)
	}
	if len(runner.queries) != 0 {
		t.Fatalf("an unowned run must not read the catalog: %v", runner.queries)
	}
}

// The owned rebuild: build aside, clear the retired name only after the build,
// rename the live table aside, rename the new one in, drop the old one last.
func TestOracleModelPlan_OwnedRebuildSwapsAfterTheBuild(t *testing.T) {
	plan, runner := oraclePlan(t, true, "ORDERS")
	if len(runner.queries) != 1 || !strings.Contains(runner.queries[0], "ALL_TABLES") {
		t.Fatalf("an owned run must read the catalog once: %v", runner.queries)
	}
	if !strings.Contains(runner.queries[0], "OWNER = 'ANALYTICS'") ||
		!strings.Contains(runner.queries[0], "'ORDERS'") || !strings.Contains(runner.queries[0], "'ORDERS__RSYNC_OLD'") {
		t.Fatalf("the catalog read must ask about this schema's target and retired names: %s", runner.queries[0])
	}

	stmts := plan.stmts
	if len(stmts) != 6 {
		t.Fatalf("owned plan has %d statements, want 6: %v", len(stmts), stmts)
	}
	checks := []struct {
		i    int
		want string
	}{
		{0, `DROP TABLE "ANALYTICS"."ORDERS__RSYNC_NEW" PURGE`},
		{1, `CREATE TABLE "ANALYTICS"."ORDERS__RSYNC_NEW" AS SELECT 1`},
		{2, `DROP TABLE "ANALYTICS"."ORDERS__RSYNC_OLD" PURGE`},
		{3, `ALTER TABLE "ANALYTICS"."ORDERS" RENAME TO "ORDERS__RSYNC_OLD"`},
		{4, `ALTER TABLE "ANALYTICS"."ORDERS__RSYNC_NEW" RENAME TO "ORDERS"`},
		{5, `DROP TABLE "ANALYTICS"."ORDERS__RSYNC_OLD" PURGE`},
	}
	for _, c := range checks {
		if !strings.Contains(stmts[c.i], c.want) {
			t.Errorf("statement %d = %q, want it to contain %q", c.i, stmts[c.i], c.want)
		}
	}
	// The two scratch drops tolerate a missing table (ORA-00942) and nothing else.
	for _, i := range []int{0, 2} {
		if !strings.HasPrefix(stmts[i], "BEGIN EXECUTE IMMEDIATE") || !strings.Contains(stmts[i], "-942") {
			t.Errorf("statement %d must be a drop-if-exists block, got %q", i, stmts[i])
		}
	}
	if !strings.Contains(plan.sweep, `DROP TABLE "ANALYTICS"."ORDERS__RSYNC_NEW" PURGE`) {
		t.Errorf("a failed rebuild must sweep the scratch table, sweep = %q", plan.sweep)
	}
}

// A run that dies between its two renames leaves the target name empty and the data
// under the retired name. Oracle commits each rename on its own, so nothing rolls
// that back: the next run has to put the table back before it does anything else,
// and must never begin by dropping the retired name — that is the only copy.
func TestOracleModelPlan_RecoversARunThatDiedMidSwap(t *testing.T) {
	plan, _ := oraclePlan(t, true, "ORDERS__RSYNC_OLD")
	if len(plan.stmts) == 0 {
		t.Fatal("empty plan")
	}
	if want := `ALTER TABLE "ANALYTICS"."ORDERS__RSYNC_OLD" RENAME TO "ORDERS"`; plan.stmts[0] != want {
		t.Fatalf("the first statement must restore the retired table, got %q (plan %v)", plan.stmts[0], plan.stmts)
	}
	for i, stmt := range plan.stmts {
		if strings.Contains(stmt, "DROP TABLE") && strings.Contains(stmt, "ORDERS__RSYNC_OLD") {
			// It may be dropped only after the restore and a fresh build.
			if i < 3 {
				t.Fatalf("statement %d drops the retired table before it was restored and rebuilt: %v", i, plan.stmts)
			}
		}
	}
	if !strings.Contains(strings.Join(plan.stmts, "\n"), `CREATE TABLE "ANALYTICS"."ORDERS__RSYNC_NEW"`) {
		t.Fatalf("after the restore the run must still rebuild: %v", plan.stmts)
	}
}

// Dropped outside rsync.ai: the owned plan degrades to a plain build rather than trying
// to rename a table that is not there — the defect that wedged driver models forever.
func TestOracleModelPlan_OwnedTargetGoneJustBuilds(t *testing.T) {
	plan, _ := oraclePlan(t, true)
	want := `CREATE TABLE "ANALYTICS"."ORDERS" AS SELECT 1`
	if len(plan.stmts) != 1 || plan.stmts[0] != want {
		t.Fatalf("plan = %v, want [%s]", plan.stmts, want)
	}
}

func TestOracleModelPlan_UnqualifiedTargetUsesTheSessionSchema(t *testing.T) {
	runner := &fakeModelRunner{rows: []map[string]interface{}{{"TABLE_NAME": "ORDERS"}}}
	if _, err := buildEngineModelPlan(context.Background(), runner, "oracle", "", "orders", tableModel(true)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.queries[0], "SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')") {
		t.Fatalf("an unqualified target resolves in the session's current schema, so the check must too: %s", runner.queries[0])
	}
}

// Not knowing is not "it is not there": a failed catalog read fails the run, before
// a single statement reaches the warehouse.
func TestOracleModelPlan_ACatalogFailureFailsTheRun(t *testing.T) {
	runner := &fakeModelRunner{queryErr: errors.New("ORA-12170: TNS connect timeout")}
	plan, err := buildEngineModelPlan(context.Background(), runner, "oracle", "analytics", "orders", tableModel(true))
	if err == nil {
		t.Fatalf("a failed catalog read must fail the run, got plan %v", plan.stmts)
	}
	if len(runner.execs) != 0 {
		t.Fatalf("nothing may run after a failed catalog read: %v", runner.execs)
	}
}

// ---------------------------------------------------------------------------
// Statement mode and execution
// ---------------------------------------------------------------------------

func TestEngineModelPlan_StatementModeRunsTheSQLAsWritten(t *testing.T) {
	for _, dialect := range modelEngineDialects {
		m := &savedQueryModel{SQLText: "MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v", Materialization: matStatement}
		runner := &fakeModelRunner{}
		plan, err := buildEngineModelPlan(context.Background(), runner, dialect, "", "", m)
		if err != nil {
			t.Fatalf("[%s] %v", dialect, err)
		}
		if len(plan.stmts) != 1 || plan.stmts[0] != m.SQLText || plan.sweep != "" {
			t.Errorf("[%s] statement plan = %+v, want the SQL alone and no sweep", dialect, plan)
		}
		if len(runner.queries) != 0 {
			t.Errorf("[%s] a statement model has no target to look up: %v", dialect, runner.queries)
		}
	}
}

func TestExecuteEnginePlan_StopsAtTheFirstFailureAndSweeps(t *testing.T) {
	runner := &fakeModelRunner{failOn: "CREATE TABLE"}
	plan := engineModelPlan{
		stmts: []string{"DROP TABLE IF EXISTS s", "CREATE TABLE s AS SELECT 1", "RENAME TABLE s TO t"},
		sweep: "DROP TABLE IF EXISTS s",
	}
	if _, err := executeEnginePlan(context.Background(), runner, plan, nil, ""); err == nil {
		t.Fatal("a failed statement must fail the run")
	}
	want := []string{"DROP TABLE IF EXISTS s", "CREATE TABLE s AS SELECT 1", "DROP TABLE IF EXISTS s"}
	if strings.Join(runner.execs, "|") != strings.Join(want, "|") {
		t.Fatalf("ran %v, want %v (stop, then sweep)", runner.execs, want)
	}
}

// The sweep must still run when the failure was the run's own deadline: the run ctx
// is already dead by then, so the sweep needs a context of its own.
func TestExecuteEnginePlan_SweepsAfterTheDeadline(t *testing.T) {
	runner := &deadlineRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan := engineModelPlan{stmts: []string{"CREATE TABLE s AS SELECT 1"}, sweep: "DROP TABLE IF EXISTS s"}
	if _, err := executeEnginePlan(ctx, runner, plan, nil, ""); err == nil {
		t.Fatal("expected the cancelled run to fail")
	}
	if runner.sweptWithLiveCtx != true {
		t.Fatal("the sweep ran on the dead run context, so it could never reach the warehouse")
	}
}

type deadlineRunner struct{ sweptWithLiveCtx bool }

func (d *deadlineRunner) exec(ctx context.Context, stmt string) (*int64, error) {
	if strings.HasPrefix(stmt, "DROP") {
		d.sweptWithLiveCtx = ctx.Err() == nil
		return nil, nil
	}
	return nil, ctx.Err()
}

func (d *deadlineRunner) queryRows(context.Context, string) ([]map[string]interface{}, error) {
	return nil, nil
}

// A CTAS or a RENAME affects no rows by definition; a count there reads as "0 rows",
// which is what reportsRowsAffected exists to stop on the driver path.
func TestExecuteEnginePlan_ReportsRowsOnlyForDML(t *testing.T) {
	n := int64(42)
	runner := &fakeModelRunner{affected: &n}
	rows, err := executeEnginePlan(context.Background(), runner, engineModelPlan{stmts: []string{"CREATE TABLE t AS SELECT 1"}}, nil, "")
	if err != nil || rows != nil {
		t.Fatalf("DDL reported rows=%v err=%v, want nil", rows, err)
	}
	rows, err = executeEnginePlan(context.Background(), runner, engineModelPlan{stmts: []string{"INSERT INTO t SELECT * FROM s"}}, nil, "")
	if err != nil || rows == nil || *rows != 42 {
		t.Fatalf("DML reported rows=%v err=%v, want 42", rows, err)
	}
}

// ---------------------------------------------------------------------------
// The orchestrator runner: a rebuild is not an interactive query
// ---------------------------------------------------------------------------

func TestOrchestratorModelRunner_SendsAModelRunWithTheWarehouseTimeout(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rows_affected": 7}`))
	}))
	defer srv.Close()
	t.Setenv("ORCHESTRATOR_URL", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	runner := orchestratorModelRunner{connectorType: "snowflake", config: map[string]interface{}{"account": "x"}}
	n, err := runner.exec(ctx, "INSERT INTO t SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || *n != 7 {
		t.Fatalf("rows_affected = %v, want 7", n)
	}
	if got["write"] != true || got["model_run"] != true {
		t.Fatalf("a model statement must be sent as write + model_run, got %v", got)
	}
	secs, _ := got["timeout_seconds"].(float64)
	// Bounded by the run's own deadline, and short of it, so the warehouse stops the
	// statement and reports why before the gateway gives up on the call.
	if secs <= 60 || secs >= 20*60 {
		t.Fatalf("timeout_seconds = %v, want the remaining budget less a margin (60 < t < 1200)", got["timeout_seconds"])
	}
}

func TestOrchestratorModelRunner_ClientOutlastsTheRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), modelRunTimeout)
	defer cancel()
	if got := orchestratorModelClientTimeout(ctx); got <= modelRunTimeout {
		t.Fatalf("the HTTP client gives up after %s, before the %s run it carries", got, modelRunTimeout)
	}
}

func TestOrchestratorModelRunner_RelaysTheEngineError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"explorer_execute failed","details":"Object 'ORDERS' already exists."}`))
	}))
	defer srv.Close()
	t.Setenv("ORCHESTRATOR_URL", srv.URL)

	_, err := orchestratorModelRunner{connectorType: "snowflake"}.exec(context.Background(), "CREATE TABLE t AS SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want the engine's own message", err)
	}
}

// ---------------------------------------------------------------------------
// Databricks: a rebuild outlasts the 50 s the Statement Execution API will block for
// ---------------------------------------------------------------------------

type dbxFake struct {
	mu        sync.Mutex
	submitted map[string]interface{}
	polls     int
	cancelled bool
	// states returned by successive polls; the last one repeats.
	states []string
	// result, when set, is what a SUCCEEDED poll returns in place of a DML count.
	columns []string
	data    [][]interface{}
}

func (f *dbxFake) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/2.0/sql/statements":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &f.submitted)
			_, _ = w.Write([]byte(`{"statement_id":"st-1","status":{"state":"PENDING"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/2.0/sql/statements/st-1":
			state := f.states[len(f.states)-1]
			if f.polls < len(f.states) {
				state = f.states[f.polls]
			}
			f.polls++
			resp := map[string]interface{}{"statement_id": "st-1", "status": map[string]interface{}{"state": state}}
			if state == "FAILED" {
				resp["status"] = map[string]interface{}{"state": state, "error": map[string]interface{}{"message": "[TABLE_OR_VIEW_ALREADY_EXISTS]"}}
			}
			if state == "SUCCEEDED" && f.columns != nil {
				cols := make([]map[string]interface{}, len(f.columns))
				for i, c := range f.columns {
					cols[i] = map[string]interface{}{"name": c}
				}
				resp["manifest"] = map[string]interface{}{"schema": map[string]interface{}{"columns": cols}}
				resp["result"] = map[string]interface{}{"data_array": f.data}
			} else if state == "SUCCEEDED" {
				resp["manifest"] = map[string]interface{}{"schema": map[string]interface{}{"columns": []map[string]interface{}{{"name": "num_affected_rows"}, {"name": "num_inserted_rows"}}}}
				resp["result"] = map[string]interface{}{"data_array": [][]interface{}{{"12", "12"}}}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/2.0/sql/statements/st-1/cancel":
			f.cancelled = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTestDatabricksRunner(srv *httptest.Server) *databricksModelRunner {
	return &databricksModelRunner{
		baseURL:      srv.URL,
		client:       srv.Client(),
		token:        "t",
		warehouseID:  "wh",
		catalog:      "main",
		schemaName:   "analytics",
		pollInterval: time.Millisecond,
	}
}

func TestDatabricksModelRunner_WaitsPastTheSubmitWindow(t *testing.T) {
	fake := &dbxFake{states: []string{"RUNNING", "RUNNING", "SUCCEEDED"}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	n, err := newTestDatabricksRunner(srv).exec(context.Background(), "INSERT INTO t SELECT * FROM s")
	if err != nil {
		t.Fatal(err)
	}
	// Asynchronous submit: the statement id comes straight back, so there is never a
	// window where the statement runs and the gateway has nothing to cancel it by.
	if fake.submitted["wait_timeout"] != "0s" {
		t.Fatalf("submit must be asynchronous, wait_timeout = %v", fake.submitted["wait_timeout"])
	}
	if fake.submitted["catalog"] != "main" || fake.submitted["schema"] != "analytics" {
		t.Fatalf("an unqualified target must land in the connection's catalog and schema: %v", fake.submitted)
	}
	if fake.polls != 3 {
		t.Fatalf("polled %d times, want 3", fake.polls)
	}
	if n == nil || *n != 12 {
		t.Fatalf("rows affected = %v, want 12 from num_affected_rows", n)
	}
}

func TestDatabricksModelRunner_ReportsTheEngineFailure(t *testing.T) {
	fake := &dbxFake{states: []string{"FAILED"}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	_, err := newTestDatabricksRunner(srv).exec(context.Background(), "CREATE TABLE t AS SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "TABLE_OR_VIEW_ALREADY_EXISTS") {
		t.Fatalf("err = %v, want the engine's message", err)
	}
}

// A data check reads its counts through the same submit-and-poll as a rebuild, so a
// check on a large table is not cut off at the API's 50 s blocking window.
func TestDatabricksModelRunner_QueryRowsReadsTheResultByColumnName(t *testing.T) {
	fake := &dbxFake{
		states:  []string{"RUNNING", "SUCCEEDED"},
		columns: []string{"rsync_c0", "rsync_c1"},
		data:    [][]interface{}{{"0", nil}},
	}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	rows, err := newTestDatabricksRunner(srv).queryRows(context.Background(), "SELECT COUNT(*) AS rsync_c0, NULL AS rsync_c1 FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if fake.submitted["wait_timeout"] != "0s" || fake.polls != 2 {
		t.Fatalf("a read must submit asynchronously and poll: wait_timeout=%v polls=%d", fake.submitted["wait_timeout"], fake.polls)
	}
	if len(rows) != 1 || rows[0]["rsync_c0"] != "0" || rows[0]["rsync_c1"] != nil {
		t.Fatalf("rows = %v", rows)
	}
	counts, err := engineCheckReader{runner: newTestDatabricksRunner(srv)}.counts(context.Background(), "SELECT 1", 2)
	if err != nil || counts[0] == nil || *counts[0] != 0 || counts[1] != nil {
		t.Fatalf("counts = %v, %v", counts, err)
	}
}

// Walking away from a running statement is not stopping it. A first-run CREATE TABLE
// that finishes after the run gave up would leave a table rsync.ai never recorded owning,
// and every later run would collide with it — so the deadline cancels it on the server.
func TestDatabricksModelRunner_CancelsTheStatementAtTheDeadline(t *testing.T) {
	fake := &dbxFake{states: []string{"RUNNING"}}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := newTestDatabricksRunner(srv).exec(ctx, "CREATE TABLE t AS SELECT 1"); err == nil {
		t.Fatal("expected the deadline to fail the statement")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.cancelled {
		t.Fatal("the statement was abandoned, not cancelled")
	}
}

// ---------------------------------------------------------------------------
// The driver path is unchanged
// ---------------------------------------------------------------------------

func TestModelDialectRunsOnDriver(t *testing.T) {
	for _, d := range []string{"postgres", "mysql", "sqlserver"} {
		if !modelDialectRunsOnDriver(d) {
			t.Errorf("%s has a database/sql driver in the gateway", d)
		}
	}
	for _, d := range modelEngineDialects {
		if modelDialectRunsOnDriver(d) {
			t.Errorf("%s has no driver in the gateway; sql.Open would fail on it", d)
		}
	}
}

// Transactional DDL is a property the plan relies on for rollback, so it has to be
// claimed only where it is real. None of the warehouses here can roll a rebuild back.
func TestModelDialectTransactionalDDL_OnlyWhereItIsReal(t *testing.T) {
	for _, d := range modelEngineDialects {
		if modelDialectSupportsTransactionalDDL(d) {
			t.Errorf("%s does not roll DDL back; claiming it would skip the sweep a failed build needs", d)
		}
	}
}
