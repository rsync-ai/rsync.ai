package handlers

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// The audit point on the driver path: a blocking check that fails keeps the
// new table from going live (write-audit-publish)
// ---------------------------------------------------------------------------
// sqlmock answers only what each test expects, in order, so a statement the plan sends
// after a block — a rename, a DROP of the retired table — fails the test.

func ownedPlan(dialect, schema, table string) func(context.Context, modelQueryer) ([]string, error) {
	return func(context.Context, modelQueryer) ([]string, error) {
		return buildMaterializationPlan(dialect, schema, table, "SELECT id FROM src", true), nil
	}
}

func blockingRowCount() []modelCheck {
	return []modelCheck{{ID: "c1", Name: "has rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityBlock}}
}

func driverPlanChecks(dialect, schema, table string, checks []modelCheck) *modelPlanChecks {
	return &modelPlanChecks{
		run:        newModelCheckRun(dialect, true, checks, nil),
		auditAfter: driverPlanAuditAfter,
		auditTable: quoteModelIdent(dialect, schema, table+modelStagingSuffix),
		liveTable:  quoteModelIdent(dialect, schema, table),
	}
}

func expectExact(mock sqlmock.Sqlmock, stmt string) *sqlmock.ExpectedExec {
	return mock.ExpectExec("^" + regexp.QuoteMeta(stmt) + "$")
}

// Postgres, Redshift and SQL Server: the audit runs inside the transaction, and a block
// rolls it back — the staging table goes with it and the live table is never renamed.
func TestRunModelPlan_ABlockOnATransactionalEngineRollsBackBeforeTheSwap(t *testing.T) {
	for _, dialect := range []string{"postgres", "sqlserver"} {
		t.Run(dialect, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			stmts := buildMaterializationPlan(dialect, "s", "orders", "SELECT id FROM src", true)
			staging := quoteModelIdent(dialect, "s", "orders"+modelStagingSuffix)

			mock.ExpectBegin()
			expectExact(mock, stmts[0]).WillReturnResult(sqlmock.NewResult(0, 0))
			expectExact(mock, stmts[1]).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM " + staging + " WHERE 1 = 0")).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectQuery(regexp.QuoteMeta("AS rsync_c0 FROM " + staging)).
				WillReturnRows(sqlmock.NewRows([]string{"rsync_c0"}).AddRow(int64(0)))
			mock.ExpectRollback()

			checks := driverPlanChecks(dialect, "s", "orders", blockingRowCount())
			_, err = runModelPlan(context.Background(), database, dialect, ownedPlan(dialect, "s", "orders"), "", checks)
			if !errors.Is(err, errModelCheckBlocked) {
				t.Fatalf("want the run blocked, got %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			res := checks.run.resultsOrNil()
			if len(res) != 1 || res[0].Status != checkStatusFail || *res[0].Observed != 0 {
				t.Fatalf("results %+v", res)
			}
		})
	}
}

// MySQL commits every DDL, so there is nothing to roll back: a block sweeps the staging
// table, and the RENAME that would publish it never runs.
func TestRunModelPlan_ABlockOnMySQLSweepsTheStagingTableAndNeverRenames(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stmts := buildMaterializationPlan("mysql", "d", "orders", "SELECT id FROM src", true)
	staging := quoteModelIdent("mysql", "d", "orders"+modelStagingSuffix)
	sweep := "DROP TABLE IF EXISTS " + staging

	expectExact(mock, stmts[0]).WillReturnResult(sqlmock.NewResult(0, 0))
	expectExact(mock, stmts[1]).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM " + staging + " WHERE 1 = 0")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("AS rsync_c0 FROM " + staging)).
		WillReturnRows(sqlmock.NewRows([]string{"rsync_c0"}).AddRow(int64(0)))
	expectExact(mock, sweep).WillReturnResult(sqlmock.NewResult(0, 0))

	checks := driverPlanChecks("mysql", "d", "orders", blockingRowCount())
	_, err = runModelPlan(context.Background(), database, "mysql", ownedPlan("mysql", "d", "orders"), sweep, checks)
	if !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A blocking check that cannot run blocks too (fail closed): it cannot vouch for the
// table.
func TestRunModelPlan_ABlockingCheckThatErrorsBlocks(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stmts := buildMaterializationPlan("postgres", "s", "orders", "SELECT id FROM src", true)
	staging := quoteModelIdent("postgres", "s", "orders"+modelStagingSuffix)
	mock.ExpectBegin()
	expectExact(mock, stmts[0]).WillReturnResult(sqlmock.NewResult(0, 0))
	expectExact(mock, stmts[1]).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM " + staging + " WHERE 1 = 0")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("AS rsync_c0 FROM " + staging)).
		WillReturnError(errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"))
	mock.ExpectRollback()

	checks := driverPlanChecks("postgres", "s", "orders", blockingRowCount())
	_, err = runModelPlan(context.Background(), database, "postgres", ownedPlan("postgres", "s", "orders"), "", checks)
	if !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if res := checks.run.resultsOrNil(); res[0].Status != checkStatusError || !strings.Contains(res[0].Message, "statement timeout") {
		t.Fatalf("results %+v", res)
	}
}

// When every blocking check passes the whole plan runs and commits; the warning checks
// then run on the published table, outside the run's transaction, each read in a
// read-only one of its own (TestModelRunWarningChecksReadOnly pins its options).
func TestRunModelPlan_PassingChecksPublishThenWarningsReadTheLiveTable(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stmts := buildMaterializationPlan("postgres", "s", "orders", "SELECT id FROM src", true)
	staging := quoteModelIdent("postgres", "s", "orders"+modelStagingSuffix)
	live := quoteModelIdent("postgres", "s", "orders")

	mock.ExpectBegin()
	expectExact(mock, stmts[0]).WillReturnResult(sqlmock.NewResult(0, 0))
	expectExact(mock, stmts[1]).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM " + staging + " WHERE 1 = 0")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("AS rsync_c0 FROM " + staging)).
		WillReturnRows(sqlmock.NewRows([]string{"rsync_c0"}).AddRow(int64(42)))
	for _, stmt := range stmts[2:] {
		expectExact(mock, stmt).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(`^SET LOCAL statement_timeout = \d+$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM " + live + " WHERE 1 = 0")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectRollback()
	mock.ExpectBegin()
	mock.ExpectExec(`^SET LOCAL statement_timeout = \d+$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(`COUNT("id") - COUNT(DISTINCT "id") AS rsync_c0 FROM ` + live)).
		WillReturnRows(sqlmock.NewRows([]string{"rsync_c0"}).AddRow(int64(3)))
	mock.ExpectRollback()

	checks := driverPlanChecks("postgres", "s", "orders", append(blockingRowCount(),
		modelCheck{Name: "id unique", Type: checkUnique, Columns: []string{"id"}, Severity: checkSeverityWarn}))
	if _, err := runModelPlan(context.Background(), database, "postgres", ownedPlan("postgres", "s", "orders"), "", checks); err != nil {
		t.Fatalf("a passing audit failed the run: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	res := checks.run.resultsOrNil()
	if len(res) != 2 || res[0].Status != checkStatusPass || res[1].Status != checkStatusFail || res[1].Severity != checkSeverityWarn {
		t.Fatalf("results %+v", res)
	}
}

// No checks, or only warnings: the plan runs exactly as before checks existed.
func TestRunModelPlan_WithoutBlockingChecksThePlanIsUnchanged(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stmts := buildMaterializationPlan("postgres", "s", "orders", "SELECT id FROM src", true)
	mock.ExpectBegin()
	for _, stmt := range stmts {
		expectExact(mock, stmt).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectCommit()
	if _, err := runModelPlan(context.Background(), database, "postgres", ownedPlan("postgres", "s", "orders"), "", nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The audit point is an index into the plan, so it holds only while the plan's first two
// statements build the staging table and touch nothing else.
func TestBuildMaterializationPlan_TheFirstTwoStatementsBuildOnlyTheStagingTable(t *testing.T) {
	for _, dialect := range []string{"postgres", "sqlserver", "mysql"} {
		for _, owned := range []bool{false, true} {
			stmts := buildMaterializationPlan(dialect, "s", "orders", "SELECT 1", owned)
			staging := quoteModelIdent(dialect, "s", "orders"+modelStagingSuffix)
			live := quoteModelIdent(dialect, "s", "orders")
			if stmts[0] != "DROP TABLE IF EXISTS "+staging || !strings.Contains(stmts[1], staging) {
				t.Errorf("%s owned=%v: %q", dialect, owned, stmts[:2])
			}
			for _, s := range stmts[:driverPlanAuditAfter] {
				if strings.Contains(strings.ReplaceAll(s, staging, ""), live) {
					t.Errorf("%s owned=%v: %q touches the live table before the audit", dialect, owned, s)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The audit point on the engine path (Oracle, ClickHouse's first run)
// ---------------------------------------------------------------------------

// scriptedEngineRunner records every statement and answers each read from the first
// answer whose text it contains.
type scriptedEngineRunner struct {
	execs   []string
	queries []string
	answers []scriptedRead
}

type scriptedRead struct {
	contains string
	rows     []map[string]interface{}
}

func (s *scriptedEngineRunner) exec(_ context.Context, stmt string) (*int64, error) {
	s.execs = append(s.execs, stmt)
	return nil, nil
}

func (s *scriptedEngineRunner) queryRows(_ context.Context, q string) ([]map[string]interface{}, error) {
	s.queries = append(s.queries, q)
	for _, a := range s.answers {
		if strings.Contains(q, a.contains) {
			return a.rows, nil
		}
	}
	return nil, errors.New("scripted: unexpected read " + q)
}

func countRow(n int64) []map[string]interface{} {
	return []map[string]interface{}{{"RSYNC_C0": n}}
}

func runEngineWithChecks(t *testing.T, runner *scriptedEngineRunner, dialect string, owned bool, checks []modelCheck) (*modelCheckRun, error) {
	t.Helper()
	run := newModelCheckRun(dialect, owned, checks, nil)
	_, err := runEngineModel(context.Background(), runner, dialect, "analytics", "orders", tableModel(owned), run)
	return run, err
}

func assertExecs(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements sent:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// An owned Oracle rebuild audits the staging table; a block sweeps it, and neither
// rename runs, so the live table is the one that was there.
func TestEnginePlan_ABlockOnAnOwnedOracleRebuildSweepsAndNeverRenames(t *testing.T) {
	plan, _ := oracleModelPlan(context.Background(), &scriptedEngineRunner{answers: []scriptedRead{
		{contains: "ALL_TABLES", rows: []map[string]interface{}{{"TABLE_NAME": "ORDERS"}}},
	}}, "analytics", "orders", "SELECT 1", true)
	staging := quoteModelIdent("oracle", "analytics", "orders"+modelStagingSuffix)

	runner := &scriptedEngineRunner{answers: []scriptedRead{
		{contains: "ALL_TABLES", rows: []map[string]interface{}{{"TABLE_NAME": "ORDERS"}}},
		{contains: "FROM " + staging, rows: countRow(0)},
	}}
	run, err := runEngineWithChecks(t, runner, "oracle", true, blockingRowCount())
	if !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	assertExecs(t, runner.execs, []string{plan.stmts[0], plan.stmts[1], plan.sweep})
	if res := run.resultsOrNil(); len(res) != 1 || res[0].Status != checkStatusFail {
		t.Fatalf("results %+v", res)
	}
}

// When the previous run died between its renames, this run first puts the data back
// under the target name; the audit comes after that and after the staging build.
func TestEnginePlan_AnOracleRecoveryRunAuditsAfterTheRecovery(t *testing.T) {
	staging := quoteModelIdent("oracle", "analytics", "orders"+modelStagingSuffix)
	catalog := scriptedRead{contains: "ALL_TABLES", rows: []map[string]interface{}{{"TABLE_NAME": "ORDERS" + strings.ToUpper(modelRetiredSuffix)}}}
	plan, _ := oracleModelPlan(context.Background(), &scriptedEngineRunner{answers: []scriptedRead{catalog}}, "analytics", "orders", "SELECT 1", true)
	if plan.auditAfter != 3 {
		t.Fatalf("auditAfter %d, want 3 (recovery, drop staging, create staging)", plan.auditAfter)
	}
	runner := &scriptedEngineRunner{answers: []scriptedRead{catalog, {contains: "FROM " + staging, rows: countRow(0)}}}
	if _, err := runEngineWithChecks(t, runner, "oracle", true, blockingRowCount()); !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	assertExecs(t, runner.execs, []string{plan.stmts[0], plan.stmts[1], plan.stmts[2], plan.sweep})
}

// An Oracle first run builds straight at the target name, so the audit looks there and
// a block drops what it built: there was nothing at that name before.
func TestEnginePlan_ABlockOnAnOracleFirstRunDropsTheTableItBuilt(t *testing.T) {
	target := quoteModelIdent("oracle", "analytics", "orders")
	runner := &scriptedEngineRunner{answers: []scriptedRead{{contains: "FROM " + target, rows: countRow(0)}}}
	if _, err := runEngineWithChecks(t, runner, "oracle", false, blockingRowCount()); !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	assertExecs(t, runner.execs, []string{
		"CREATE TABLE " + target + " AS SELECT 1",
		"DROP TABLE " + target + " PURGE",
	})
}

// A ClickHouse first run builds aside and RENAMEs; a block sweeps the staging table and
// the RENAME never runs.
func TestEnginePlan_ABlockOnAClickHouseFirstRunSweepsAndNeverRenames(t *testing.T) {
	plan, _ := buildEngineModelPlan(context.Background(), nil, "clickhouse", "analytics", "orders", tableModel(false))
	runner := &scriptedEngineRunner{answers: []scriptedRead{{contains: "FROM " + plan.auditTable, rows: countRow(0)}}}
	if _, err := runEngineWithChecks(t, runner, "clickhouse", false, blockingRowCount()); !errors.Is(err, errModelCheckBlocked) {
		t.Fatalf("want the run blocked, got %v", err)
	}
	assertExecs(t, runner.execs, []string{plan.stmts[0], plan.stmts[1], plan.sweep})
}

// Where a rebuild replaces the table in one statement there is no point to look before
// the publish, so a blocking check saved earlier runs as a warning, after it, and says so.
func TestEnginePlan_AnEngineWithNoAuditPointRunsABlockAsAWarning(t *testing.T) {
	for _, tc := range []struct {
		dialect string
		owned   bool
	}{{"clickhouse", true}, {"snowflake", true}, {"bigquery", false}} {
		t.Run(tc.dialect, func(t *testing.T) {
			target := quoteModelIdent(tc.dialect, "analytics", "orders")
			runner := &scriptedEngineRunner{answers: []scriptedRead{{contains: "FROM " + target, rows: countRow(0)}}}
			run, err := runEngineWithChecks(t, runner, tc.dialect, tc.owned, blockingRowCount())
			if err != nil {
				t.Fatalf("a warning failed the run: %v", err)
			}
			if len(runner.execs) != 1 {
				t.Fatalf("statements %v", runner.execs)
			}
			res := run.resultsOrNil()
			if len(res) != 1 || res[0].Severity != checkSeverityWarn || res[0].Status != checkStatusFail || !strings.Contains(res[0].Message, "Ran as a warning") {
				t.Fatalf("results %+v", res)
			}
		})
	}
}

// A passing audit lets the whole plan run; warnings then read the live table.
func TestEnginePlan_APassingAuditPublishesThenWarningsReadTheLiveTable(t *testing.T) {
	catalog := scriptedRead{contains: "ALL_TABLES", rows: []map[string]interface{}{{"TABLE_NAME": "ORDERS"}}}
	plan, _ := oracleModelPlan(context.Background(), &scriptedEngineRunner{answers: []scriptedRead{catalog}}, "analytics", "orders", "SELECT 1", true)
	target := quoteModelIdent("oracle", "analytics", "orders")
	runner := &scriptedEngineRunner{answers: []scriptedRead{
		catalog,
		{contains: "FROM " + plan.auditTable, rows: countRow(5)},
		{contains: "FROM " + target, rows: countRow(2)},
	}}
	run, err := runEngineWithChecks(t, runner, "oracle", true, append(blockingRowCount(),
		modelCheck{Name: "id unique", Type: checkUnique, Columns: []string{"id"}, Severity: checkSeverityWarn}))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.execs) != len(plan.stmts) {
		t.Fatalf("sent %d of %d statements", len(runner.execs), len(plan.stmts))
	}
	res := run.resultsOrNil()
	if len(res) != 2 || res[0].Status != checkStatusPass || res[1].Status != checkStatusFail {
		t.Fatalf("results %+v", res)
	}
	if last := runner.queries[len(runner.queries)-1]; !strings.Contains(last, "FROM "+target) || strings.Contains(last, modelStagingSuffix) {
		t.Fatalf("the warning read %s", last)
	}
}
