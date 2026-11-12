package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"api-gateway/internal/db"
)

// ---------------------------------------------------------------------------
// Data checks: compiling, judging, validating (saved_query_checks.go)
// ---------------------------------------------------------------------------

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }
func i64s(vs ...int64) []*int64 {
	out := make([]*int64, len(vs))
	for i := range vs {
		out[i] = i64(vs[i])
	}
	return out
}

// fakeCheckReader answers each count query from the first answer whose text it
// contains, and records what it was sent.
type fakeCheckReader struct {
	cols    []string
	known   bool
	colsErr error
	aborts  bool
	answers []fakeCheckAnswer
	queries []string
}

type fakeCheckAnswer struct {
	contains string
	vals     []*int64
	err      error
}

func (f *fakeCheckReader) counts(_ context.Context, q string, n int) ([]*int64, error) {
	f.queries = append(f.queries, q)
	for _, a := range f.answers {
		if strings.Contains(q, a.contains) {
			if a.err != nil {
				return nil, a.err
			}
			if len(a.vals) != n {
				return nil, fmt.Errorf("fake: %d values for %d columns", len(a.vals), n)
			}
			return a.vals, nil
		}
	}
	return nil, fmt.Errorf("fake: unexpected query %s", q)
}

func (f *fakeCheckReader) columns(context.Context, string) ([]string, bool, error) {
	return f.cols, f.known, f.colsErr
}

func (f *fakeCheckReader) abortsOnError() bool { return f.aborts }

func TestQuoteCheckColumn_EscapesTheClosingQuotePerEngine(t *testing.T) {
	cases := []struct{ dialect, col, want string }{
		{"postgres", `order id`, `"order id"`},
		{"postgres", `we"ird`, `"we""ird"`},
		{"postgres", `Mixed`, `"Mixed"`},
		{"redshift", `a"b`, `"a""b"`},
		{"mysql", "a`b", "`a``b`"},
		{"clickhouse", "x", "`x`"},
		{"bigquery", "x", "`x`"},
		{"databricks", "x", "`x`"},
		{"sqlserver", "a]b", "[a]]b]"},
		{"snowflake", "email", `"EMAIL"`},
		{"snowflake", "Email", `"Email"`},
		{"oracle", "order_id", `"ORDER_ID"`},
		{"oracle", "order id", `"order id"`},
	}
	for _, c := range cases {
		if got := quoteCheckColumn(c.dialect, c.col); got != c.want {
			t.Errorf("quoteCheckColumn(%s, %q) = %s, want %s", c.dialect, c.col, got, c.want)
		}
	}
}

func TestCompileAggregateChecks_OneScanAliasedInOrder(t *testing.T) {
	checks := []modelCheck{
		{Type: checkRowCount},
		{Type: checkNotNull, Columns: []string{"email"}},
		{Type: checkUnique, Columns: []string{"id"}},
		{Type: checkRowCountChange},
	}
	got := compileAggregateChecks("postgres", `"s"."t__rsync_new"`, checks)
	want := `SELECT COUNT(*) AS rsync_c0, COUNT(*) - COUNT("email") AS rsync_c1, COUNT("id") - COUNT(DISTINCT "id") AS rsync_c2, COUNT(*) AS rsync_c3 FROM "s"."t__rsync_new"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// SQL Server's COUNT is an INT; past 2^31 rows only COUNT_BIG is right.
	ss := compileAggregateChecks("sqlserver", "[dbo].[t]", checks[:2])
	if ss != `SELECT COUNT_BIG(*) AS rsync_c0, COUNT_BIG(*) - COUNT_BIG([email]) AS rsync_c1 FROM [dbo].[t]` {
		t.Fatalf("sqlserver: %s", ss)
	}
}

func TestCompileMultiUnique_IgnoresCombinationsWithANull(t *testing.T) {
	got := compileMultiUnique("mysql", "`d`.`t`", []string{"a", "b"})
	want := "SELECT COALESCE(SUM(rsync_n - 1), 0) AS rsync_c0 FROM (SELECT COUNT(*) AS rsync_n FROM `d`.`t` WHERE `a` IS NOT NULL AND `b` IS NOT NULL GROUP BY `a`, `b` HAVING COUNT(*) > 1) rsync_d"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCompileCustomCheck_FillsThisAndCountsTheRows(t *testing.T) {
	got := compileCustomCheck("SELECT * FROM {{ this }} WHERE amount < 0;  ", `"s"."t__rsync_new"`)
	want := `SELECT COUNT(*) AS rsync_c0 FROM (SELECT * FROM "s"."t__rsync_new" WHERE amount < 0) rsync_q`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// The table name goes in literally: a $ in it is not a regexp back-reference.
	if got := substituteCheckThis("SELECT 1 FROM {{this}}", `"a$1"`); got != `SELECT 1 FROM "a$1"` {
		t.Fatalf("literal substitution: %s", got)
	}
}

func TestCustomCheckSQLRefusal(t *testing.T) {
	table := `"public"."orders"`
	ok := []string{
		"SELECT * FROM {{this}} WHERE amount < 0",
		"select id from {{ this }} t where not exists (select 1 from public.customers c where c.id = t.customer_id)",
	}
	for _, q := range ok {
		if r := customCheckSQLRefusal(q, table); r != "" {
			t.Errorf("refused a read: %q: %s", q, r)
		}
	}
	refused := map[string]string{
		"SELECT * FROM public.orders WHERE amount < 0":    "{{this}}",
		"DELETE FROM {{this}}":                            "read-only",
		"SELECT 1 FROM {{this}}; DROP TABLE {{this}}":     "read-only",
		"UPDATE {{this}} SET amount = 0 WHERE amount < 0": "read-only",
		"INSERT INTO public.audit SELECT * FROM {{this}}": "read-only",
	}
	for q, want := range refused {
		if r := customCheckSQLRefusal(q, table); !strings.Contains(r, want) {
			t.Errorf("customCheckSQLRefusal(%q) = %q, want it to mention %q", q, r, want)
		}
	}
}

func TestJudgeCheckCount(t *testing.T) {
	cases := []struct {
		name     string
		c        modelCheck
		observed int64
		baseline *int64
		want     string
	}{
		{"row_count default min 1, empty", modelCheck{Type: checkRowCount}, 0, nil, checkStatusFail},
		{"row_count default min 1, one row", modelCheck{Type: checkRowCount}, 1, nil, checkStatusPass},
		{"row_count min 0 allows empty", modelCheck{Type: checkRowCount, Params: modelCheckParams{Min: i64(0)}}, 0, nil, checkStatusPass},
		{"row_count over max", modelCheck{Type: checkRowCount, Params: modelCheckParams{Min: i64(1), Max: i64(10)}}, 11, nil, checkStatusFail},
		{"row_count at max", modelCheck{Type: checkRowCount, Params: modelCheckParams{Max: i64(10)}}, 10, nil, checkStatusPass},
		{"not_null zero", modelCheck{Type: checkNotNull}, 0, nil, checkStatusPass},
		{"not_null some", modelCheck{Type: checkNotNull}, 3, nil, checkStatusFail},
		{"unique dupes", modelCheck{Type: checkUnique}, 1, nil, checkStatusFail},
		{"custom rows", modelCheck{Type: checkCustomSQL}, 2, nil, checkStatusFail},
		{"custom none", modelCheck{Type: checkCustomSQL}, 0, nil, checkStatusPass},
		{"change no baseline", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10)}}, 0, nil, checkStatusPass},
		{"change drop within", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10)}}, 90, i64(100), checkStatusPass},
		{"change drop over", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10)}}, 89, i64(100), checkStatusFail},
		{"change growth unlimited", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10)}}, 1000, i64(100), checkStatusPass},
		{"change growth over", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10), MaxGrowthPct: f64(50)}}, 151, i64(100), checkStatusFail},
		{"change growth from zero, unlimited", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10)}}, 5, i64(0), checkStatusPass},
		{"change growth from zero, limited", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(10), MaxGrowthPct: f64(1000)}}, 5, i64(0), checkStatusFail},
		{"change both zero", modelCheck{Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(0), MaxGrowthPct: f64(0)}}, 0, i64(0), checkStatusPass},
	}
	for _, c := range cases {
		if got, _ := judgeCheckCount(c.c, c.observed, c.baseline); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParseCheckCount(t *testing.T) {
	good := []struct {
		in   interface{}
		want int64
	}{
		{int64(7), 7}, {int32(7), 7}, {7, 7}, {uint64(7), 7}, {float64(7), 7},
		{[]byte("7"), 7}, {"  7 ", 7}, {"7.0", 7}, {json.Number("7"), 7}, {"9007199254740993", 9007199254740993},
	}
	for _, g := range good {
		got, err := parseCheckCount(g.in)
		if err != nil || got != g.want {
			t.Errorf("parseCheckCount(%#v) = %d, %v; want %d", g.in, got, err, g.want)
		}
	}
	for _, bad := range []interface{}{"abc", 7.5, uint64(1 << 63), "1e400", true} {
		if _, err := parseCheckCount(bad); err == nil {
			t.Errorf("parseCheckCount(%#v) accepted a non-count", bad)
		}
	}
}

func TestModelCheckCanBlock(t *testing.T) {
	cases := []struct {
		dialect string
		owned   bool
		want    bool
	}{
		{"postgres", true, true}, {"sqlserver", true, true}, {"mysql", true, true}, {"oracle", true, true},
		{"clickhouse", false, true}, {"clickhouse", true, false},
		{"snowflake", false, false}, {"bigquery", false, false}, {"databricks", false, false},
	}
	for _, c := range cases {
		if got := modelCheckCanBlock(c.dialect, c.owned); got != c.want {
			t.Errorf("modelCheckCanBlock(%s, owned=%v) = %v", c.dialect, c.owned, got)
		}
	}
}

func TestNormalizeModelCheck(t *testing.T) {
	table := `"public"."orders"`
	ok := []modelCheckInput{
		{Name: "has rows", Type: checkRowCount},
		{Name: "id unique", Type: checkUnique, Columns: []string{"id"}},
		{Name: "pair unique", Type: checkUnique, Columns: []string{"a", "b"}},
		{Name: "email set", Type: checkNotNull, Columns: []string{"e-mail address"}},
		{Name: "steady", Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(20)}},
		{Name: "no negatives", Type: checkCustomSQL, SQLText: "SELECT * FROM {{this}} WHERE amount < 0"},
	}
	for _, in := range ok {
		c, err := normalizeModelCheck(in, "postgres", true, table)
		if err != nil {
			t.Errorf("%s: refused: %v", in.Name, err)
			continue
		}
		if c.Severity != defaultCheckSeverity(in.Type) {
			t.Errorf("%s: severity %s", in.Name, c.Severity)
		}
	}
	if c, _ := normalizeModelCheck(modelCheckInput{Name: "n", Type: checkRowCount}, "postgres", true, table); c.Params.Min == nil || *c.Params.Min != 1 {
		t.Errorf("row_count's min is stored as 1 when not given")
	}
	if c, _ := normalizeModelCheck(modelCheckInput{Name: "n", Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(5)}}, "postgres", true, table); c.Severity != checkSeverityWarn {
		t.Errorf("row_count_change defaults to warn, got %s", c.Severity)
	}

	refused := []struct {
		in      modelCheckInput
		dialect string
		owned   bool
		want    string
	}{
		{modelCheckInput{Type: checkRowCount}, "postgres", true, "name"},
		{modelCheckInput{Name: strings.Repeat("x", 101), Type: checkRowCount}, "postgres", true, "100"},
		{modelCheckInput{Name: "n\x07", Type: checkRowCount}, "postgres", true, "control"},
		{modelCheckInput{Name: "n", Type: "freshness"}, "postgres", true, "unknown"},
		{modelCheckInput{Name: "n", Type: checkRowCount, Severity: "error"}, "postgres", true, "severity"},
		{modelCheckInput{Name: "n", Type: checkRowCount, Params: modelCheckParams{Min: i64(-1)}}, "postgres", true, "negative"},
		{modelCheckInput{Name: "n", Type: checkRowCount, Params: modelCheckParams{Min: i64(5), Max: i64(4)}}, "postgres", true, "below"},
		{modelCheckInput{Name: "n", Type: checkRowCount, Columns: []string{"a"}}, "postgres", true, "no columns"},
		{modelCheckInput{Name: "n", Type: checkRowCountChange}, "postgres", true, "max_drop_pct"},
		{modelCheckInput{Name: "n", Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(101)}}, "postgres", true, "between"},
		{modelCheckInput{Name: "n", Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(1), MaxGrowthPct: f64(-1)}}, "postgres", true, "negative"},
		{modelCheckInput{Name: "n", Type: checkNotNull}, "postgres", true, "exactly one"},
		{modelCheckInput{Name: "n", Type: checkNotNull, Columns: []string{"a", "b"}}, "postgres", true, "exactly one"},
		{modelCheckInput{Name: "n", Type: checkUnique}, "postgres", true, "1 to"},
		{modelCheckInput{Name: "n", Type: checkUnique, Columns: []string{"a", "a"}}, "postgres", true, "twice"},
		{modelCheckInput{Name: "n", Type: checkUnique, Columns: []string{" a"}}, "postgres", true, "space"},
		{modelCheckInput{Name: "n", Type: checkUnique, Columns: []string{strings.Repeat("c", 129)}}, "postgres", true, "128"},
		{modelCheckInput{Name: "n", Type: checkCustomSQL}, "postgres", true, "query"},
		{modelCheckInput{Name: "n", Type: checkCustomSQL, SQLText: "DELETE FROM {{this}}"}, "postgres", true, "read-only"},
		// No audit point: refused as a block, accepted as a warning.
		{modelCheckInput{Name: "n", Type: checkRowCount}, "snowflake", false, "warning"},
		{modelCheckInput{Name: "n", Type: checkRowCount}, "clickhouse", true, "warning"},
	}
	for _, r := range refused {
		_, err := normalizeModelCheck(r.in, r.dialect, r.owned, table)
		if err == nil || !strings.Contains(err.Error(), r.want) {
			t.Errorf("%+v on %s: err %v, want it to mention %q", r.in, r.dialect, err, r.want)
		}
	}
	if _, err := normalizeModelCheck(modelCheckInput{Name: "n", Type: checkRowCount, Severity: checkSeverityWarn}, "snowflake", false, table); err != nil {
		t.Errorf("a warning on snowflake was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// runModelChecks: every check runs, and one that cannot run says so
// ---------------------------------------------------------------------------

func resultByName(t *testing.T, res []modelCheckResult, name string) modelCheckResult {
	t.Helper()
	for _, r := range res {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result for %q in %+v", name, res)
	return modelCheckResult{}
}

func TestRunModelChecks_SharesOneScanAndJudgesEach(t *testing.T) {
	rd := &fakeCheckReader{known: true, cols: []string{"id", "email"}, answers: []fakeCheckAnswer{
		{contains: "rsync_c2", vals: i64s(10, 2, 0)},
	}}
	checks := []modelCheck{
		{Name: "rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityBlock},
		{Name: "email", Type: checkNotNull, Columns: []string{"email"}, Severity: checkSeverityBlock},
		{Name: "id", Type: checkUnique, Columns: []string{"id"}, Severity: checkSeverityBlock},
	}
	res := runModelChecks(context.Background(), rd, "postgres", "t", checks, nil)
	if len(rd.queries) != 1 {
		t.Fatalf("want one shared scan, sent %d: %v", len(rd.queries), rd.queries)
	}
	if r := resultByName(t, res, "rows"); r.Status != checkStatusPass || *r.Observed != 10 {
		t.Errorf("rows: %+v", r)
	}
	if r := resultByName(t, res, "email"); r.Status != checkStatusFail || *r.Observed != 2 {
		t.Errorf("email: %+v", r)
	}
	if r := resultByName(t, res, "id"); r.Status != checkStatusPass {
		t.Errorf("id: %+v", r)
	}
}

// A column the model no longer returns errors that check alone; it is never sent, so on
// Postgres it cannot end the transaction the others run in.
func TestRunModelChecks_AMissingColumnErrorsOnlyItsCheck(t *testing.T) {
	rd := &fakeCheckReader{known: true, cols: []string{"id"}, aborts: true, answers: []fakeCheckAnswer{
		{contains: "FROM t", vals: i64s(5)},
	}}
	checks := []modelCheck{
		{Name: "gone", Type: checkNotNull, Columns: []string{"email"}},
		{Name: "rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}},
	}
	res := runModelChecks(context.Background(), rd, "postgres", "t", checks, nil)
	if r := resultByName(t, res, "gone"); r.Status != checkStatusError || !strings.Contains(r.Message, `"email" is not in`) {
		t.Errorf("gone: %+v", r)
	}
	if r := resultByName(t, res, "rows"); r.Status != checkStatusPass {
		t.Errorf("rows: %+v", r)
	}
	for _, q := range rd.queries {
		if strings.Contains(q, "email") {
			t.Errorf("sent the missing column: %s", q)
		}
	}
	// Postgres compares a quoted name exactly; MySQL does not.
	if checkColumnPresent("postgres", "Email", []string{"email"}) {
		t.Error("postgres matched a column by case")
	}
	if !checkColumnPresent("mysql", "Email", []string{"email"}) {
		t.Error("mysql did not match a column by case")
	}
}

// After an error in a transaction that an error ends, the checks after it are recorded
// as not run — never as passed.
func TestRunModelChecks_AnAbortedTransactionLeavesTheRestNotRun(t *testing.T) {
	rd := &fakeCheckReader{known: true, cols: []string{"a", "b"}, aborts: true, answers: []fakeCheckAnswer{
		{contains: "GROUP BY", err: errors.New("ERROR: out of memory (SQLSTATE 53200)")},
	}}
	checks := []modelCheck{
		{Name: "pair", Type: checkUnique, Columns: []string{"a", "b"}},
		{Name: "custom", Type: checkCustomSQL, SQLText: "SELECT * FROM {{this}} WHERE a < 0"},
	}
	res := runModelChecks(context.Background(), rd, "postgres", "t", checks, nil)
	if r := resultByName(t, res, "pair"); r.Status != checkStatusError || !strings.Contains(r.Message, "out of memory") {
		t.Errorf("pair: %+v", r)
	}
	if r := resultByName(t, res, "custom"); r.Status != checkStatusError || r.Message != checkNotRunAfterAbort {
		t.Errorf("custom: %+v", r)
	}
	if len(rd.queries) != 1 {
		t.Errorf("sent a statement into an aborted transaction: %v", rd.queries)
	}
}

// An engine reader cannot list columns cheaply; when the shared scan fails it finds the
// check the engine refused by sending each alone.
func TestRunModelChecks_AnEngineRetriesEachCheckAlone(t *testing.T) {
	rd := &fakeCheckReader{answers: []fakeCheckAnswer{
		{contains: "rsync_c1", err: errors.New("Invalid identifier EMAIL")},
		{contains: "EMAIL", err: errors.New("Invalid identifier EMAIL")},
		{contains: "COUNT(*) AS rsync_c0", vals: i64s(4)},
	}}
	checks := []modelCheck{
		{Name: "rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}},
		{Name: "email", Type: checkNotNull, Columns: []string{"email"}},
	}
	res := runModelChecks(context.Background(), rd, "snowflake", "T", checks, nil)
	if r := resultByName(t, res, "rows"); r.Status != checkStatusPass || *r.Observed != 4 {
		t.Errorf("rows: %+v", r)
	}
	if r := resultByName(t, res, "email"); r.Status != checkStatusError {
		t.Errorf("email: %+v", r)
	}
	if len(rd.queries) != 3 {
		t.Errorf("want the scan then each alone, sent %v", rd.queries)
	}
}

func TestRunModelChecks_ARefusedCustomCheckIsNeverSent(t *testing.T) {
	rd := &fakeCheckReader{known: true}
	checks := []modelCheck{{Name: "bad", Type: checkCustomSQL, SQLText: "DELETE FROM {{this}}"}}
	res := runModelChecks(context.Background(), rd, "postgres", "t", checks, nil)
	if res[0].Status != checkStatusError || len(rd.queries) != 0 {
		t.Fatalf("result %+v, sent %v", res[0], rd.queries)
	}
}

func TestRunModelChecks_AnUnreadableTableErrorsEveryCheck(t *testing.T) {
	rd := &fakeCheckReader{known: true, colsErr: errors.New(`relation "t" does not exist`)}
	res := runModelChecks(context.Background(), rd, "postgres", "t", []modelCheck{{Name: "a", Type: checkRowCount}, {Name: "b", Type: checkRowCount}}, nil)
	for _, r := range res {
		if r.Status != checkStatusError {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
}

func TestModelCheckRun_AuditBlocksOnAFailOrAnError(t *testing.T) {
	mk := func(answer fakeCheckAnswer) (*modelCheckRun, *fakeCheckReader) {
		run := newModelCheckRun("postgres", true, []modelCheck{
			{Name: "rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityBlock},
			{Name: "drift", Type: checkRowCountChange, Params: modelCheckParams{MaxDropPct: f64(1)}, Severity: checkSeverityWarn},
		}, i64(100))
		return run, &fakeCheckReader{known: true, answers: []fakeCheckAnswer{answer}}
	}
	run, rd := mk(fakeCheckAnswer{contains: "FROM", vals: i64s(5)})
	if run.audit(context.Background(), rd, "s") {
		t.Error("a passing block check blocked")
	}
	if len(run.results) != 1 {
		t.Errorf("the audit ran a warning check: %+v", run.results)
	}
	run, rd = mk(fakeCheckAnswer{contains: "FROM", vals: i64s(0)})
	if !run.audit(context.Background(), rd, "s") {
		t.Error("a failing block check did not block")
	}
	run, rd = mk(fakeCheckAnswer{contains: "FROM", err: errors.New("timeout")})
	if !run.audit(context.Background(), rd, "s") {
		t.Error("a block check that could not run did not block")
	}
}

// A blocking check saved before a ClickHouse model's first run published runs as a
// warning on the owned rebuild, and says so.
func TestNewModelCheckRun_DowngradesABlockTheEngineCannotHold(t *testing.T) {
	run := newModelCheckRun("clickhouse", true, []modelCheck{
		{Name: "rows", Type: checkRowCount, Params: modelCheckParams{Min: i64(1)}, Severity: checkSeverityBlock},
	}, nil)
	if run.hasBlock() {
		t.Fatal("an owned ClickHouse rebuild has no audit point")
	}
	rd := &fakeCheckReader{answers: []fakeCheckAnswer{{contains: "FROM", vals: i64s(0)}}}
	run.report(context.Background(), rd, "t")
	r := run.results[0]
	if r.Severity != checkSeverityWarn || r.Status != checkStatusFail || !strings.Contains(r.Message, "Ran as a warning") {
		t.Fatalf("%+v", r)
	}
	if first := newModelCheckRun("clickhouse", false, []modelCheck{{Name: "rows", Severity: checkSeverityBlock}}, nil); !first.hasBlock() {
		t.Fatal("a first ClickHouse run builds aside and can block")
	}
}

// ---------------------------------------------------------------------------
// The readers
// ---------------------------------------------------------------------------

func TestDriverCheckReader_ReadsCountsHoweverTheDriverCarriesThem(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(`SELECT \* FROM t WHERE 1 = 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "Email"}))
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{"rsync_c0", "rsync_c1", "rsync_c2"}).
		AddRow(int64(3), []byte("12"), nil))
	rd := driverCheckReader{q: database, dialect: "mysql"}
	cols, known, err := rd.columns(context.Background(), "t")
	if err != nil || !known || strings.Join(cols, ",") != "id,Email" {
		t.Fatalf("columns: %v %v %v", cols, known, err)
	}
	vals, err := rd.counts(context.Background(), "SELECT COUNT(*) AS rsync_c0, 1, 2 FROM t", 3)
	if err != nil {
		t.Fatal(err)
	}
	if *vals[0] != 3 || *vals[1] != 12 || vals[2] != nil {
		t.Fatalf("got %v %v %v", vals[0], vals[1], vals[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCheckReader_ReadsTheAliasInAnyCase(t *testing.T) {
	f := &fakeModelRunner{rows: []map[string]interface{}{{"RSYNC_C0": "7", "rsync_c1": float64(2)}}}
	vals, err := engineCheckReader{runner: f}.counts(context.Background(), "q", 2)
	if err != nil || *vals[0] != 7 || *vals[1] != 2 {
		t.Fatalf("%v %v", vals, err)
	}
	f.rows = nil
	if _, err := (engineCheckReader{runner: f}).counts(context.Background(), "q", 1); err == nil {
		t.Fatal("no row read as a count")
	}
}

func TestLoadModelCheckRun(t *testing.T) {
	cols := []string{"check_id", "name", "check_type", "columns", "params", "sql_text", "severity", "enabled"}

	t.Run("no checks table yet means no checks", func(t *testing.T) {
		database, mock, _ := sqlmock.New()
		defer database.Close()
		mock.ExpectQuery(`FROM saved_query_checks`).WillReturnError(errors.New(`ERROR: relation "saved_query_checks" does not exist (SQLSTATE 42P01)`))
		run, err := loadModelCheckRun(context.Background(), database, "m1", "postgres", true)
		if err != nil || run != nil {
			t.Fatalf("%v %v", run, err)
		}
	})
	t.Run("any other read error fails the run", func(t *testing.T) {
		database, mock, _ := sqlmock.New()
		defer database.Close()
		mock.ExpectQuery(`FROM saved_query_checks`).WillReturnError(errors.New("connection reset"))
		if _, err := loadModelCheckRun(context.Background(), database, "m1", "postgres", true); err == nil {
			t.Fatal("a broken read ran the model without its checks")
		}
	})
	t.Run("the baseline is read only for a change check", func(t *testing.T) {
		database, mock, _ := sqlmock.New()
		defer database.Close()
		mock.ExpectQuery(`FROM saved_query_checks WHERE saved_query_id = \$1 AND enabled`).WithArgs("m1").
			WillReturnRows(sqlmock.NewRows(cols).
				AddRow("c1", "rows", "row_count", `[]`, `{"min":1}`, "", "block", true).
				AddRow("c2", "drift", "row_count_change", `[]`, `{"max_drop_pct":20}`, "", "warn", true).
				AddRow("c3", "id", "unique", `["id","Order Id"]`, `{}`, "", "block", true))
		mock.ExpectQuery(`FROM saved_query_runs sr`).WithArgs("m1").
			WillReturnRows(sqlmock.NewRows([]string{"b"}).AddRow(int64(250)))
		run, err := loadModelCheckRun(context.Background(), database, "m1", "postgres", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.block) != 2 || len(run.warn) != 1 || run.baseline == nil || *run.baseline != 250 {
			t.Fatalf("%+v", run)
		}
		if got := run.block[1].Columns; len(got) != 2 || got[1] != "Order Id" {
			t.Fatalf("columns %v", got)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// Each check's result is written in the run's own transaction: a run row with no results
// would read as "no checks ran", the one reading a blocked run must never get.
func TestRecordModelRunOutcome_RecordsEachCheckResultInTheRunsTransaction(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	failOnSwallowedError(t)

	checkID := "7b1c1f0e-0000-4000-8000-000000000001"
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE saved_queries`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(provHistoryInsert).
		WithArgs(savedQueryID, provSchedule, string(triggerScheduled), "failed", "read", "analytics.orders", sqlmock.AnyArg(),
			modelCheckBlockedError, "", provActor, sqlmock.AnyArg(),
			"", "", "", "", int64(0), int64(0)).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(provRunB))
	// check_id goes through a lookup, as upstream_run_id does: a check removed while the
	// run was going must not fail the foreign key and lose the run's whole record.
	mock.ExpectExec(`INSERT INTO saved_query_check_results \(\s*run_id, check_id, check_name, check_type, severity, status, observed, threshold, message\s*\) VALUES `+
		`\(\$1::uuid, \(SELECT c\.check_id FROM saved_query_checks c WHERE c\.check_id = NULLIF\(\$2, ''\)::uuid\), \$3, \$4, \$5, \$6, \$7, NULLIF\(\$8, ''\), NULLIF\(\$9, ''\)\), `+
		`\(\$1::uuid, \(SELECT c\.check_id FROM saved_query_checks c WHERE c\.check_id = NULLIF\(\$10, ''\)::uuid\), \$11, \$12, \$13, \$14, \$15, NULLIF\(\$16, ''\), NULLIF\(\$17, ''\)\)`).
		WithArgs(provRunB,
			checkID, "has rows", checkRowCount, checkSeverityBlock, checkStatusFail, int64(0), ">= 1", "",
			"", "ids unique", checkUnique, checkSeverityWarn, checkStatusError, nil, "", "could not read").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	got := recordModelRunOutcome(context.Background(), db.DB, savedQueryID,
		&modelRunResult{Status: "failed", StatementClass: "read", TargetTable: "analytics.orders",
			Error: modelCheckBlockedError, CheckBlocked: true,
			CheckResults: []modelCheckResult{
				{CheckID: checkID, Name: "has rows", Type: checkRowCount, Severity: checkSeverityBlock,
					Status: checkStatusFail, Observed: i64(0), Threshold: ">= 1"},
				{Name: "ids unique", Type: checkUnique, Severity: checkSeverityWarn,
					Status: checkStatusError, Message: "could not read"},
			}},
		modelRunAudit{Trigger: triggerScheduled, ScheduleID: provSchedule, ActorID: provActor})
	if got != provRunB {
		t.Fatalf("returned run id %q, want %q", got, provRunB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the check results were not recorded with the run: %v", err)
	}
}

// The runs API shows a stored error through the display scrub, and the Runs views and
// the next hop tell a blocked run from a broken one by this exact sentence (the
// frontend's MODEL_CHECK_BLOCKED_ERROR, modelChecks.ts). A scrub that reworded it would
// turn every blocked run into an ordinary failure on screen.
func TestModelCheckBlockedError_SurvivesTheDisplayScrub(t *testing.T) {
	if got := storedRunErrorForDisplay(modelCheckBlockedError); got != modelCheckBlockedError {
		t.Fatalf("storedRunErrorForDisplay(%q) = %q; the Runs views would not see a blocked run", modelCheckBlockedError, got)
	}
}
