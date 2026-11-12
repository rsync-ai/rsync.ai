package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"api-gateway/internal/notifier"
	"api-gateway/internal/security"
	"api-gateway/internal/validators"

	log "github.com/sirupsen/logrus"
)

// ============================================================================
// Data checks on a model's table (migration 138, docs/internal/data-checks-design.md)
// ============================================================================
// A check is a rule on a model's output table, run on every rebuild. A blocking check
// runs on the staging table after the build and before the swap (write-audit-publish);
// if one fails, or cannot run, the new table is not published, the run records
// modelCheckBlockedError and the models below are skipped upstream_check_failed. A
// warning check runs after the swap, on the live table, and only reports.
//
// Nothing here sends a row value anywhere: a typed check compiles to one aggregate
// over column names, and every check returns one count. The count is stored for the
// person (saved_query_check_results.observed) and never leaves through an LLM-bound tool.

const (
	checkRowCount       = "row_count"
	checkRowCountChange = "row_count_change"
	checkNotNull        = "not_null"
	checkUnique         = "unique"
	checkCustomSQL      = "custom_sql"

	checkSeverityBlock = "block"
	checkSeverityWarn  = "warn"

	checkStatusPass  = "pass"
	checkStatusFail  = "fail"
	checkStatusError = "error"

	// maxChecksPerModel bounds what one run spends on checks; each is a scan.
	maxChecksPerModel = 20
	maxCheckNameRunes = 100
	// maxCheckColumnRunes is the longest identifier any supported engine allows.
	maxCheckColumnRunes = 128
	maxUniqueColumns    = 10
	maxCheckSQLBytes    = 20000
)

// modelCheckBlockedError is the whole of a blocked run's error. It names no check and
// no count: run errors reach Ask rsync.ai, and a check's name is the user's own text,
// which can describe data (design § 2.2). The cascade also reads it to tell a blocked
// run from a broken one (cascadeSkipReason).
const modelCheckBlockedError = "Blocked by a data check — see the Checks tab"

// errModelCheckBlocked is what a plan returns when its audit held the table back.
var errModelCheckBlocked = errors.New(modelCheckBlockedError)

// checkNotRunAfterAbort is a check that never reached the engine because an earlier
// check's error ended the transaction it would have run in (Postgres, SQL Server).
const checkNotRunAfterAbort = "not run: an earlier check's error ended the transaction"

// checkThisPlaceholder is how a custom_sql check names the model's table.
var checkThisPlaceholder = regexp.MustCompile(`\{\{\s*this\s*\}\}`)

// modelCheckParams are a typed check's settings. Unused ones stay nil.
type modelCheckParams struct {
	Min          *int64   `json:"min,omitempty"`
	Max          *int64   `json:"max,omitempty"`
	MaxDropPct   *float64 `json:"max_drop_pct,omitempty"`
	MaxGrowthPct *float64 `json:"max_growth_pct,omitempty"`
}

// modelCheck is one saved check, as a run uses it.
type modelCheck struct {
	ID       string
	Name     string
	Type     string
	Columns  []string
	Params   modelCheckParams
	SQLText  string
	Severity string
}

// modelCheckResult is what one check found on one run.
type modelCheckResult struct {
	CheckID string `json:"check_id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	// Severity is what the check did on this run, which is 'warn' for a blocking check
	// on an engine that cannot hold a table back.
	Severity  string `json:"severity"`
	Status    string `json:"status"`
	Observed  *int64 `json:"observed,omitempty"`
	Threshold string `json:"threshold,omitempty"`
	Message   string `json:"message,omitempty"`
}

// defaultCheckSeverity is the severity a check gets when none is given. The invariants
// block; a count's movement warns, because a real day can move a count by half.
func defaultCheckSeverity(checkType string) string {
	if checkType == checkRowCountChange {
		return checkSeverityWarn
	}
	return checkSeverityBlock
}

// modelCheckCanBlock reports whether a run on this engine has a point between the
// build and the publish where a check can look (design § 3.1). ClickHouse has one only
// on a first run, which builds aside; an owned rebuild is one CREATE OR REPLACE. The
// warehouses replace in one statement, so they have none yet.
func modelCheckCanBlock(dialect string, owned bool) bool {
	switch dialect {
	case "postgres", "sqlserver", "mysql", "oracle":
		return true
	case "clickhouse":
		return !owned
	default:
		return false
	}
}

// cannotBlockMessage is the refusal for a blocking check on an engine without an
// audit point.
const cannotBlockMessage = "this engine can't hold a table back yet — add it as a warning"

// ---------------------------------------------------------------------------
// One run's checks
// ---------------------------------------------------------------------------

// modelCheckRun is one run's checks and what they found. Blocking checks audit the
// staging table; warning checks report on the live one.
type modelCheckRun struct {
	dialect string
	block   []modelCheck
	warn    []modelCheck
	// downgraded names the blocking checks this engine runs as warnings.
	downgraded map[string]bool
	// baseline is the count the last published table had, for row_count_change.
	baseline *int64
	results  []modelCheckResult
}

// newModelCheckRun splits checks by what they do on this run. A blocking check on an
// engine that cannot hold the table back runs as a warning. Saving one is refused, so
// this is a check saved before its model's engine lost the audit point: a ClickHouse
// model whose first run has since published.
func newModelCheckRun(dialect string, owned bool, checks []modelCheck, baseline *int64) *modelCheckRun {
	r := &modelCheckRun{dialect: dialect, baseline: baseline, downgraded: map[string]bool{}}
	canBlock := modelCheckCanBlock(dialect, owned)
	for _, c := range checks {
		switch {
		case c.Severity == checkSeverityBlock && canBlock:
			r.block = append(r.block, c)
		case c.Severity == checkSeverityBlock:
			r.downgraded[c.Name] = true
			c.Severity = checkSeverityWarn
			r.warn = append(r.warn, c)
		default:
			c.Severity = checkSeverityWarn
			r.warn = append(r.warn, c)
		}
	}
	return r
}

// hasBlock reports whether the run needs an audit point.
func (r *modelCheckRun) hasBlock() bool { return r != nil && len(r.block) > 0 }

func (r *modelCheckRun) hasWarn() bool { return r != nil && len(r.warn) > 0 }

// audit runs the blocking checks on the staging table and reports whether any of them
// holds it back. A check that could not run holds it back too: a check that could not
// look cannot vouch for the table.
func (r *modelCheckRun) audit(ctx context.Context, rd checkReader, table string) (blocked bool) {
	if !r.hasBlock() {
		return false
	}
	res := runModelChecks(ctx, rd, r.dialect, table, r.block, r.baseline)
	r.results = append(r.results, res...)
	for _, x := range res {
		if x.Status != checkStatusPass {
			blocked = true
		}
	}
	return blocked
}

// auditUnreachable records every blocking check as not run, for a plan that reached
// no audit point. The run is then blocked: fail closed.
func (r *modelCheckRun) auditUnreachable(reason string) {
	for _, c := range r.block {
		r.results = append(r.results, modelCheckResult{
			CheckID: c.ID, Name: c.Name, Type: c.Type, Severity: checkSeverityBlock,
			Status: checkStatusError, Message: reason,
		})
	}
}

// report runs the warning checks on the published table.
func (r *modelCheckRun) report(ctx context.Context, rd checkReader, table string) {
	if !r.hasWarn() {
		return
	}
	res := runModelChecks(ctx, rd, r.dialect, table, r.warn, r.baseline)
	for i := range res {
		if r.downgraded[res[i].Name] {
			note := "Ran as a warning: this engine can't hold a table back yet."
			if res[i].Message != "" {
				note += " " + res[i].Message
			}
			res[i].Message = note
		}
	}
	r.results = append(r.results, res...)
}

// resultsOrNil is what the run carries: nothing when no check ran.
func (r *modelCheckRun) resultsOrNil() []modelCheckResult {
	if r == nil || len(r.results) == 0 {
		return nil
	}
	return r.results
}

// ---------------------------------------------------------------------------
// Reaching the table
// ---------------------------------------------------------------------------

// checkReader is how a check reaches a table: through the plan's own connection, inside
// its transaction where it has one, or through the engine runner.
type checkReader interface {
	// counts runs one SELECT returning one row of n integer columns named
	// rsync_c0..rsync_c<n-1>. A NULL comes back nil.
	counts(ctx context.Context, query string, n int) ([]*int64, error)
	// columns lists the table's columns. known is false when the reader cannot tell
	// cheaply; every check is then sent, and a failed aggregate is retried one check
	// at a time.
	columns(ctx context.Context, table string) (cols []string, known bool, err error)
	// abortsOnError reports whether a failed statement ends the reader's transaction,
	// so that nothing after it can run (Postgres; SQL Server under XACT_ABORT).
	abortsOnError() bool
}

// checkQueryer is the read half of a driver connection or transaction.
type checkQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// driverCheckReader reads through a database/sql connection or transaction.
type driverCheckReader struct {
	q       checkQueryer
	dialect string
	inTx    bool
}

func (d driverCheckReader) counts(ctx context.Context, query string, n int) ([]*int64, error) {
	vals := make([]interface{}, n)
	ptrs := make([]interface{}, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := d.q.QueryRowContext(ctx, query).Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make([]*int64, n)
	for i, v := range vals {
		if v == nil {
			continue
		}
		x, err := parseCheckCount(v)
		if err != nil {
			return nil, err
		}
		out[i] = &x
	}
	return out, nil
}

// columns reads the table's columns without reading a row. On Postgres a check naming
// a column the model no longer returns would otherwise end the transaction, and every
// check after it with it; asked first, only that check errors.
func (d driverCheckReader) columns(ctx context.Context, table string) ([]string, bool, error) {
	rows, err := d.q.QueryContext(ctx, "SELECT * FROM "+table+" WHERE 1 = 0")
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, true, err
	}
	return cols, true, nil
}

// abortsOnError: Postgres (and Redshift) end a transaction at its first error. SQL
// Server does not for most errors, but a conversion error can, and assuming it does
// costs only the checks after it — which are then recorded as not run, never as passed.
func (d driverCheckReader) abortsOnError() bool { return d.inTx }

// engineCheckReader reads through the warehouse runner (orchestrator or Databricks).
// Each statement travels alone, so an error ends nothing but itself.
type engineCheckReader struct {
	runner modelEngineRunner
}

func (e engineCheckReader) counts(ctx context.Context, query string, n int) ([]*int64, error) {
	rows, err := e.runner.queryRows(ctx, query)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("the check query returned %d rows, not 1", len(rows))
	}
	out := make([]*int64, n)
	for i := 0; i < n; i++ {
		// Snowflake and Oracle return an unquoted alias in upper case.
		v, ok := lookupFold(rows[0], fmt.Sprintf("rsync_c%d", i))
		if !ok {
			return nil, fmt.Errorf("the check query returned no column rsync_c%d", i)
		}
		if v == nil {
			continue
		}
		x, err := parseCheckCount(v)
		if err != nil {
			return nil, err
		}
		out[i] = &x
	}
	return out, nil
}

func (e engineCheckReader) columns(context.Context, string) ([]string, bool, error) {
	return nil, false, nil
}

func (e engineCheckReader) abortsOnError() bool { return false }

// unreadableCheckReader answers every read with the error that kept the checks from
// reaching the table, so each check is recorded as not run rather than left out.
type unreadableCheckReader struct{ err error }

func (u unreadableCheckReader) counts(context.Context, string, int) ([]*int64, error) {
	return nil, u.err
}
func (u unreadableCheckReader) columns(context.Context, string) ([]string, bool, error) {
	return nil, true, u.err
}
func (u unreadableCheckReader) abortsOnError() bool { return false }

func lookupFold(row map[string]interface{}, key string) (interface{}, bool) {
	if v, ok := row[key]; ok {
		return v, true
	}
	for k, v := range row {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// parseCheckCount reads a count however a driver or a JSON envelope carries it:
// an integer, a float that is whole (JSON numbers), or a decimal string (BigQuery's
// INT64, MySQL's DECIMAL from SUM, a Postgres NUMERIC).
func parseCheckCount(v interface{}) (int64, error) {
	switch t := v.(type) {
	case int64:
		return t, nil
	case int:
		return int64(t), nil
	case int32:
		return int64(t), nil
	case int16:
		return int64(t), nil
	case int8:
		return int64(t), nil
	case uint64:
		if t > math.MaxInt64 {
			return 0, fmt.Errorf("the count %d is out of range", t)
		}
		return int64(t), nil
	case uint32:
		return int64(t), nil
	case float64:
		return wholeCount(t)
	case float32:
		return wholeCount(float64(t))
	case []byte:
		return parseCountString(string(t))
	case string:
		return parseCountString(t)
	case json.Number:
		return parseCountString(t.String())
	default:
		return parseCountString(fmt.Sprint(v))
	}
}

func wholeCount(f float64) (int64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
		return 0, fmt.Errorf("the check returned %v, not a count", f)
	}
	return int64(f), nil
}

func parseCountString(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("the check returned %q, not a count", s)
	}
	return wholeCount(f)
}

// ---------------------------------------------------------------------------
// Compiling a check
// ---------------------------------------------------------------------------

// quoteCheckColumn quotes a column a person named for one engine. Unlike a model's
// target, a column name is not held to modelIdentPart — real tables have spaces and
// dashes in theirs — so the closing quote is escaped by doubling. Snowflake and Oracle
// fold a plain lower-case name to upper case, the name an unquoted reference resolves
// to there (as quoteModelIdent does for a target).
func quoteCheckColumn(dialect, col string) string {
	switch dialect {
	case "mysql", "bigquery", "databricks", "clickhouse":
		return "`" + strings.ReplaceAll(col, "`", "``") + "`"
	case "sqlserver":
		return "[" + strings.ReplaceAll(col, "]", "]]") + "]"
	case "snowflake", "oracle":
		if plainLowerIdent.MatchString(col) {
			col = strings.ToUpper(col)
		}
		return `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	default: // postgres, redshift
		return `"` + strings.ReplaceAll(col, `"`, `""`) + `"`
	}
}

var plainLowerIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// checkCountFn is the engine's count function. SQL Server's COUNT is an INT and
// overflows past 2^31 rows; COUNT_BIG is its BIGINT.
func checkCountFn(dialect string) string {
	if dialect == "sqlserver" {
		return "COUNT_BIG"
	}
	return "COUNT"
}

// isAggregateCheck reports whether a check is one expression in the shared scan.
func isAggregateCheck(c modelCheck) bool {
	switch c.Type {
	case checkRowCount, checkRowCountChange, checkNotNull:
		return true
	case checkUnique:
		return len(c.Columns) == 1
	default:
		return false
	}
}

// checkAggregateExpr is a check's expression in the shared scan. Every one counts
// what is wrong, or (row_count) what is there.
func checkAggregateExpr(dialect string, c modelCheck) string {
	count := checkCountFn(dialect)
	switch c.Type {
	case checkNotNull:
		col := quoteCheckColumn(dialect, c.Columns[0])
		return fmt.Sprintf("%s(*) - %s(%s)", count, count, col)
	case checkUnique:
		col := quoteCheckColumn(dialect, c.Columns[0])
		// NULLs are ignored, as in dbt's unique: COUNT skips them on both sides.
		return fmt.Sprintf("%s(%s) - %s(DISTINCT %s)", count, col, count, col)
	default: // row_count, row_count_change
		return count + "(*)"
	}
}

// compileAggregateChecks is the one scan every aggregate check shares.
func compileAggregateChecks(dialect, table string, checks []modelCheck) string {
	exprs := make([]string, len(checks))
	for i, c := range checks {
		exprs[i] = fmt.Sprintf("%s AS rsync_c%d", checkAggregateExpr(dialect, c), i)
	}
	return "SELECT " + strings.Join(exprs, ", ") + " FROM " + table
}

// compileMultiUnique counts the rows beyond the first in each duplicated combination.
// A combination with a NULL in it is ignored, as a one-column unique ignores NULLs.
func compileMultiUnique(dialect, table string, cols []string) string {
	count := checkCountFn(dialect)
	quoted := make([]string, len(cols))
	notNull := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteCheckColumn(dialect, c)
		notNull[i] = quoted[i] + " IS NOT NULL"
	}
	group := strings.Join(quoted, ", ")
	return fmt.Sprintf("SELECT COALESCE(SUM(rsync_n - 1), 0) AS rsync_c0 FROM (SELECT %s(*) AS rsync_n FROM %s WHERE %s GROUP BY %s HAVING %s(*) > 1) rsync_d",
		count, table, strings.Join(notNull, " AND "), group, count)
}

// substituteCheckThis puts the table's quoted name where the check says {{this}}.
func substituteCheckThis(sqlText, table string) string {
	return checkThisPlaceholder.ReplaceAllLiteralString(sqlText, table)
}

// compileCustomCheck counts the rows the person's query returns; any row fails it.
func compileCustomCheck(sqlText, table string) string {
	inner := strings.TrimRight(strings.TrimSpace(substituteCheckThis(sqlText, table)), ";")
	return "SELECT COUNT(*) AS rsync_c0 FROM (" + inner + ") rsync_q"
}

// customCheckSQLRefusal says why a custom_sql check's text cannot run, or "". The text
// is judged with {{this}} filled in, as it will run: one statement, a read — the gate
// a workflow's query passes (workflowSQLRefusal). A read needs no role above member,
// so the run-as user's current role (authorizeModelRun) clears it whenever the run
// itself is cleared.
func customCheckSQLRefusal(sqlText, table string) string {
	if !checkThisPlaceholder.MatchString(sqlText) {
		return "A custom SQL check must name the model's table as {{this}}."
	}
	sub := strings.TrimSpace(substituteCheckThis(sqlText, table))
	if v := validators.ValidateExplorerStatement(sub, security.WSViewer); v == nil || !v.Valid {
		msg := "it is not a single valid statement"
		if v != nil && v.ErrorMessage != "" {
			msg = v.ErrorMessage
		}
		return "A custom SQL check must be one read-only SELECT: " + msg + "."
	}
	if class := validators.ClassifyStatementSQL(sub); class != validators.ClassRead {
		return fmt.Sprintf("A custom SQL check must be one read-only SELECT; this one is a %s statement.",
			strings.ReplaceAll(string(class), "_", " "))
	}
	return ""
}

// ---------------------------------------------------------------------------
// Running and judging
// ---------------------------------------------------------------------------

// checkColumnsMatch reports whether a column a check names is in the table. Postgres
// compares a quoted name exactly; MySQL and SQL Server compare column names without
// case by default.
func checkColumnPresent(dialect, col string, cols []string) bool {
	for _, c := range cols {
		if c == col {
			return true
		}
		if dialect != "postgres" && strings.EqualFold(c, col) {
			return true
		}
	}
	return false
}

// runModelChecks runs checks against table and judges each. Every check runs, even
// after one fails, so one run shows everything wrong with the table. Aggregate checks
// share one scan; a several-column unique and each custom_sql get their own, the
// person's SQL last, since it is the likeliest to error.
func runModelChecks(ctx context.Context, rd checkReader, dialect, table string, checks []modelCheck, baseline *int64) []modelCheckResult {
	n := len(checks)
	counts := make([]*int64, n)
	errMsg := make([]string, n)
	settled := make([]bool, n)
	fail := func(i int, msg string) { errMsg[i], settled[i] = msg, true }
	aborted := false

	cols, known, err := rd.columns(ctx, table)
	if err != nil {
		msg := "could not read the model's table: " + modelRunErrorMessage(err.Error())
		for i := range checks {
			fail(i, msg)
		}
		return judgeModelChecks(checks, counts, errMsg, baseline)
	}
	if known {
		for i, c := range checks {
			for _, col := range c.Columns {
				if !checkColumnPresent(dialect, col, cols) {
					fail(i, fmt.Sprintf("column %q is not in the model's table", col))
					break
				}
			}
		}
	}

	// runOne sends one single-count query for check i.
	runOne := func(i int, query string) {
		if aborted {
			fail(i, checkNotRunAfterAbort)
			return
		}
		vals, err := rd.counts(ctx, query, 1)
		if err != nil {
			fail(i, modelRunErrorMessage(err.Error()))
			aborted = rd.abortsOnError()
			return
		}
		counts[i], settled[i] = vals[0], true
	}

	var agg []int
	for i, c := range checks {
		if !settled[i] && isAggregateCheck(c) {
			agg = append(agg, i)
		}
	}
	if len(agg) > 0 {
		aggChecks := make([]modelCheck, len(agg))
		for j, i := range agg {
			aggChecks[j] = checks[i]
		}
		vals, err := rd.counts(ctx, compileAggregateChecks(dialect, table, aggChecks), len(agg))
		switch {
		case err == nil:
			for j, i := range agg {
				counts[i], settled[i] = vals[j], true
			}
		case rd.abortsOnError() || len(agg) == 1:
			msg := modelRunErrorMessage(err.Error())
			for _, i := range agg {
				fail(i, msg)
			}
			aborted = rd.abortsOnError()
		default:
			// Find which one the engine refused: each alone.
			for _, i := range agg {
				runOne(i, compileAggregateChecks(dialect, table, []modelCheck{checks[i]}))
			}
		}
	}

	for i, c := range checks {
		if !settled[i] && c.Type == checkUnique && len(c.Columns) > 1 {
			runOne(i, compileMultiUnique(dialect, table, c.Columns))
		}
	}
	for i, c := range checks {
		if settled[i] || c.Type != checkCustomSQL {
			continue
		}
		if refusal := customCheckSQLRefusal(c.SQLText, table); refusal != "" {
			fail(i, refusal)
			continue
		}
		runOne(i, compileCustomCheck(c.SQLText, table))
	}
	return judgeModelChecks(checks, counts, errMsg, baseline)
}

// judgeModelChecks turns each check's count into a result.
func judgeModelChecks(checks []modelCheck, counts []*int64, errMsg []string, baseline *int64) []modelCheckResult {
	out := make([]modelCheckResult, len(checks))
	for i, c := range checks {
		r := modelCheckResult{CheckID: c.ID, Name: c.Name, Type: c.Type, Severity: c.Severity,
			Threshold: checkThreshold(c, baseline)}
		switch {
		case errMsg[i] != "":
			r.Status, r.Message = checkStatusError, errMsg[i]
		case counts[i] == nil:
			r.Status, r.Message = checkStatusError, "the check returned no count"
		default:
			r.Observed = counts[i]
			r.Status, r.Message = judgeCheckCount(c, *counts[i], baseline)
		}
		out[i] = r
	}
	return out
}

// effectiveMin is row_count's minimum: 1 unless the check says otherwise.
func effectiveMin(p modelCheckParams) int64 {
	if p.Min != nil {
		return *p.Min
	}
	return 1
}

// judgeCheckCount decides pass or fail for one count.
func judgeCheckCount(c modelCheck, observed int64, baseline *int64) (status, message string) {
	switch c.Type {
	case checkRowCount:
		if observed < effectiveMin(c.Params) || (c.Params.Max != nil && observed > *c.Params.Max) {
			return checkStatusFail, ""
		}
		return checkStatusPass, ""
	case checkRowCountChange:
		if baseline == nil {
			return checkStatusPass, "no previous count to compare with"
		}
		b := *baseline
		switch {
		case observed < b && c.Params.MaxDropPct != nil:
			drop := float64(b-observed) / float64(b) * 100
			if drop > *c.Params.MaxDropPct {
				return checkStatusFail, fmt.Sprintf("fell %s%% from %s", formatPct(drop), checkCount(b))
			}
		case observed > b && b == 0:
			// Growth from an empty table has no percentage; it fails only a check that
			// limits growth at all.
			if c.Params.MaxGrowthPct != nil {
				return checkStatusFail, "grew from 0"
			}
		case observed > b && c.Params.MaxGrowthPct != nil:
			growth := float64(observed-b) / float64(b) * 100
			if growth > *c.Params.MaxGrowthPct {
				return checkStatusFail, fmt.Sprintf("grew %s%% from %s", formatPct(growth), checkCount(b))
			}
		}
		return checkStatusPass, ""
	default: // not_null, unique, custom_sql count what is wrong
		if observed > 0 {
			return checkStatusFail, ""
		}
		return checkStatusPass, ""
	}
}

func formatPct(f float64) string {
	return strconv.FormatFloat(math.Round(f*10)/10, 'f', -1, 64)
}

// checkCount is a count as people read it: 1,000,000,000, not 1000000000.
func checkCount(n int64) string {
	digits := strconv.FormatInt(n, 10)
	sign := ""
	if n < 0 {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return sign + b.String()
}

// checkRows is "1 row" or "1,000 rows".
func checkRows(n int64) string {
	if n == 1 {
		return "1 row"
	}
	return checkCount(n) + " rows"
}

// checkThreshold is what a check compares against, for display.
func checkThreshold(c modelCheck, baseline *int64) string {
	switch c.Type {
	case checkRowCount:
		s := "at least " + checkRows(effectiveMin(c.Params))
		if c.Params.Max != nil {
			s += ", at most " + checkCount(*c.Params.Max)
		}
		return s
	case checkRowCountChange:
		var parts []string
		if c.Params.MaxDropPct != nil {
			parts = append(parts, "drop at most "+formatPct(*c.Params.MaxDropPct)+"%")
		}
		if c.Params.MaxGrowthPct != nil {
			parts = append(parts, "growth at most "+formatPct(*c.Params.MaxGrowthPct)+"%")
		}
		s := strings.Join(parts, ", ")
		if baseline != nil {
			s += " (previous count " + checkCount(*baseline) + ")"
		}
		return s
	case checkNotNull:
		return "no NULLs"
	case checkUnique:
		return "no duplicates"
	case checkCustomSQL:
		return "no rows"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// loadModelCheckRun reads a table model's enabled checks and, when one needs it, the
// last published count. nil when the model has none.
//
// Fails closed: a read that errors is returned, and the caller does not run the model
// (a blocking check that cannot be read cannot vouch for the table). The one
// exception is a database that has no checks table at all, migration 138 not yet
// applied: nothing can have been saved there, so there is nothing to run.
func loadModelCheckRun(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}, modelID, dialect string, owned bool) (*modelCheckRun, error) {
	checks, err := loadModelChecks(ctx, q, modelID, true)
	if err != nil {
		if isUndefinedTable(err) { // pipeline_consumers.go
			return nil, nil
		}
		return nil, err
	}
	if len(checks) == 0 {
		return nil, nil
	}
	var baseline *int64
	for _, c := range checks {
		if c.Type == checkRowCountChange {
			if baseline, err = loadCheckBaseline(ctx, q, modelID); err != nil {
				return nil, err
			}
			break
		}
	}
	return newModelCheckRun(dialect, owned, checks, baseline), nil
}

// modelCheckColumns is the SELECT list loadModelChecks scans. columns travels as JSON:
// pgx has no pq.Array to scan a TEXT[] into.
const modelCheckColumns = `check_id::text, name, check_type, COALESCE(to_json(columns)::text, '[]'),
	params::text, COALESCE(sql_text, ''), severity, enabled`

// loadModelChecks reads a model's checks in the order they were added.
func loadModelChecks(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}, modelID string, enabledOnly bool) ([]modelCheck, error) {
	query := `SELECT ` + modelCheckColumns + ` FROM saved_query_checks WHERE saved_query_id = $1`
	if enabledOnly {
		query += ` AND enabled`
	}
	rows, err := q.QueryContext(ctx, query+` ORDER BY created_at, name`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelCheck
	for rows.Next() {
		c, _, err := scanModelCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanModelCheck(rows interface {
	Scan(dest ...interface{}) error
}) (modelCheck, bool, error) {
	var c modelCheck
	var colsJSON, paramsJSON string
	var enabled bool
	if err := rows.Scan(&c.ID, &c.Name, &c.Type, &colsJSON, &paramsJSON, &c.SQLText, &c.Severity, &enabled); err != nil {
		return c, false, err
	}
	if err := json.Unmarshal([]byte(colsJSON), &c.Columns); err != nil {
		return c, false, fmt.Errorf("check %s: unreadable columns: %w", c.ID, err)
	}
	if err := json.Unmarshal([]byte(paramsJSON), &c.Params); err != nil {
		return c, false, fmt.Errorf("check %s: unreadable params: %w", c.ID, err)
	}
	return c, enabled, nil
}

// loadCheckBaseline is the count the last published table had: what a row count check
// saw on the model's most recent succeeded run. A blocked run's count is not it — that
// table never went live. nil when that run counted nothing (a first run, or the check
// is new).
func loadCheckBaseline(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}, modelID string) (*int64, error) {
	var b sql.NullInt64
	err := q.QueryRowContext(ctx, `
		SELECT (SELECT MAX(r.observed) FROM saved_query_check_results r
		         WHERE r.run_id = sr.run_id AND r.observed IS NOT NULL
		           AND r.check_type IN ('row_count', 'row_count_change'))
		FROM saved_query_runs sr
		WHERE sr.saved_query_id = $1 AND sr.status = 'succeeded'
		ORDER BY sr.finished_at DESC
		LIMIT 1`, modelID).Scan(&b)
	if err == sql.ErrNoRows || (err == nil && !b.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v := b.Int64
	return &v, nil
}

// ---------------------------------------------------------------------------
// Validating a check someone saves
// ---------------------------------------------------------------------------

// modelCheckInput is a check as the API and the agent tool receive it.
type modelCheckInput struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Columns  []string         `json:"columns,omitempty"`
	Params   modelCheckParams `json:"params"`
	SQLText  string           `json:"sql_text,omitempty"`
	Severity string           `json:"severity,omitempty"`
	Enabled  *bool            `json:"enabled,omitempty"`
}

// normalizeModelCheck validates in and returns the check to store. table is the model's
// quoted live target, which custom_sql is judged against. It refuses a blocking check
// on an engine that cannot hold a table back (design § 3.1).
func normalizeModelCheck(in modelCheckInput, dialect string, owned bool, table string) (modelCheck, error) {
	c := modelCheck{Name: strings.TrimSpace(in.Name), Type: strings.TrimSpace(in.Type),
		Severity: strings.TrimSpace(in.Severity)}
	if c.Name == "" {
		return c, errors.New("a check needs a name")
	}
	if utf8.RuneCountInString(c.Name) > maxCheckNameRunes {
		return c, fmt.Errorf("a check's name can be at most %d characters", maxCheckNameRunes)
	}
	if hasControlChar(c.Name) {
		return c, errors.New("a check's name cannot contain control characters")
	}
	if c.Severity == "" {
		c.Severity = defaultCheckSeverity(c.Type)
	}
	if c.Severity != checkSeverityBlock && c.Severity != checkSeverityWarn {
		return c, errors.New(`severity must be "block" or "warn"`)
	}

	wantCols := 0
	switch c.Type {
	case checkRowCount:
		if in.Params.Min != nil && *in.Params.Min < 0 {
			return c, errors.New("min cannot be negative")
		}
		minimum := effectiveMin(in.Params)
		if in.Params.Max != nil && *in.Params.Max < minimum {
			return c, errors.New("max cannot be below min")
		}
		c.Params = modelCheckParams{Min: &minimum, Max: in.Params.Max}
	case checkRowCountChange:
		if in.Params.MaxDropPct == nil {
			return c, errors.New("a row count change check needs max_drop_pct")
		}
		if d := *in.Params.MaxDropPct; d < 0 || d > 100 || math.IsNaN(d) {
			return c, errors.New("max_drop_pct must be between 0 and 100")
		}
		if g := in.Params.MaxGrowthPct; g != nil && (*g < 0 || math.IsNaN(*g) || math.IsInf(*g, 0)) {
			return c, errors.New("max_growth_pct cannot be negative")
		}
		c.Params = modelCheckParams{MaxDropPct: in.Params.MaxDropPct, MaxGrowthPct: in.Params.MaxGrowthPct}
	case checkNotNull:
		wantCols = 1
	case checkUnique:
		wantCols = -1
	case checkCustomSQL:
		c.SQLText = strings.TrimSpace(in.SQLText)
		if c.SQLText == "" {
			return c, errors.New("a custom SQL check needs its query")
		}
		if len(c.SQLText) > maxCheckSQLBytes {
			return c, fmt.Errorf("a custom SQL check's query can be at most %d bytes", maxCheckSQLBytes)
		}
		if refusal := customCheckSQLRefusal(c.SQLText, table); refusal != "" {
			return c, errors.New(refusal)
		}
	default:
		return c, fmt.Errorf("unknown check type %q", in.Type)
	}

	switch {
	case wantCols == 0 && len(in.Columns) > 0:
		return c, fmt.Errorf("a %s check takes no columns", c.Type)
	case wantCols == 1 && len(in.Columns) != 1:
		return c, errors.New("a not null check takes exactly one column")
	case wantCols == -1 && (len(in.Columns) < 1 || len(in.Columns) > maxUniqueColumns):
		return c, fmt.Errorf("a unique check takes 1 to %d columns", maxUniqueColumns)
	}
	seen := map[string]bool{}
	for _, col := range in.Columns {
		if col == "" || strings.TrimSpace(col) != col {
			return c, errors.New("a column name cannot be blank or start or end with a space")
		}
		if utf8.RuneCountInString(col) > maxCheckColumnRunes {
			return c, fmt.Errorf("a column name can be at most %d characters", maxCheckColumnRunes)
		}
		if hasControlChar(col) {
			return c, errors.New("a column name cannot contain control characters")
		}
		if seen[col] {
			return c, fmt.Errorf("column %q is listed twice", col)
		}
		seen[col] = true
		c.Columns = append(c.Columns, col)
	}

	if c.Severity == checkSeverityBlock && !modelCheckCanBlock(dialect, owned) {
		return c, errors.New(cannotBlockMessage)
	}
	return c, nil
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// insertModelCheckResults writes what each check saw on a run, in one statement inside
// the run's transaction. check_id goes through a lookup, as a run's upstream_run_id does:
// a check removed while the run was going is recorded by name, not refused by the
// foreign key, which would take the run's whole record with it.
func insertModelCheckResults(ctx context.Context, ex modelExecer, runID string, results []modelCheckResult) error {
	if len(results) == 0 {
		return nil
	}
	args := []interface{}{runID}
	rows := make([]string, 0, len(results))
	for _, r := range results {
		n := len(args)
		rows = append(rows, fmt.Sprintf(
			"($1::uuid, (SELECT c.check_id FROM saved_query_checks c WHERE c.check_id = NULLIF($%d, '')::uuid), $%d, $%d, $%d, $%d, $%d, NULLIF($%d, ''), NULLIF($%d, ''))",
			n+1, n+2, n+3, n+4, n+5, n+6, n+7, n+8))
		var observed interface{}
		if r.Observed != nil {
			observed = *r.Observed
		}
		args = append(args, r.CheckID, r.Name, r.Type, r.Severity, r.Status, observed, r.Threshold, r.Message)
	}
	_, err := ex.ExecContext(ctx, `INSERT INTO saved_query_check_results (
			run_id, check_id, check_name, check_type, severity, status, observed, threshold, message
		) VALUES `+strings.Join(rows, ", "), args...)
	return err
}

// ---------------------------------------------------------------------------
// Alerts (design § 7)
// ---------------------------------------------------------------------------

// modelCheckPublisher delivers an in-process notification through the notifier's
// persist → dedup → Slack/email path (notifier.Emitter.Publish).
type modelCheckPublisher interface {
	Publish(ctx context.Context, raw []byte) error
}

var (
	modelCheckNotifierMu sync.RWMutex
	modelCheckNotifier   modelCheckPublisher
)

// SetModelCheckNotifier wires the publisher that tells a model's owner a data check
// held its table back or warned. Unset, runs and their results are still recorded;
// nobody is told.
func SetModelCheckNotifier(p modelCheckPublisher) {
	modelCheckNotifierMu.Lock()
	defer modelCheckNotifierMu.Unlock()
	modelCheckNotifier = p
}

func currentModelCheckNotifier() modelCheckPublisher {
	modelCheckNotifierMu.RLock()
	defer modelCheckNotifierMu.RUnlock()
	return modelCheckNotifier
}

// modelCheckAlertNamed is how many checks an alert names; the rest are counted.
const modelCheckAlertNamed = 3

// notifyModelCheckOutcome raises the run's alert, if it has one, in the background: a
// slow Slack must never hold up the run that is recording itself.
func notifyModelCheckOutcome(modelID, runID string, res *modelRunResult) {
	p := currentModelCheckNotifier()
	if p == nil {
		return
	}
	raw := modelCheckAlertPayload(modelID, runID, res)
	if raw == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := p.Publish(ctx, raw); err != nil {
			log.WithError(err).WithField("model_id", modelID).Warn("model run: failed to raise the data check alert")
		}
	}()
}

// modelCheckAlertPayload builds the rsync.notifications-shaped event for a run's
// checks, or nil when there is nothing to say: CHECK_BLOCKED when a check held the
// table back, CHECK_WARNED when one that does not block failed or could not run.
//
// It names each check that did not pass, its type, the count it saw and what it
// expected. A count is fine here: the alert goes to the workspace's own bell, Slack and
// email, not to an LLM. An engine's error text is not: it can quote a value, and the
// Checks tab already shows it to the person who opens the link.
func modelCheckAlertPayload(modelID, runID string, res *modelRunResult) []byte {
	if res == nil {
		return nil
	}
	var parts []string
	for _, r := range res.CheckResults {
		var part string
		switch r.Status {
		case checkStatusFail:
			part = fmt.Sprintf("%q (%s) failed", r.Name, r.Type)
			if r.Observed != nil {
				part += ": found " + checkCount(*r.Observed)
				if r.Threshold != "" {
					part += ", expected " + r.Threshold
				}
			}
			if r.Message != "" {
				part += " (" + r.Message + ")"
			}
		case checkStatusError:
			part = fmt.Sprintf("%q (%s) could not run", r.Name, r.Type)
		default:
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil
	}

	// Which table is live is the code's Impact line (notifier/catalog.go), shown under
	// this message in the bell, Slack and email: said here too, it read twice.
	code, severity := notifier.CodeCheckWarned, "warning"
	lead := "A data check warned."
	if res.CheckBlocked {
		code, severity = notifier.CodeCheckBlocked, "error"
		lead = "A data check held the new table back."
	}
	shown := parts
	if len(shown) > modelCheckAlertNamed {
		shown = append(append([]string(nil), parts[:modelCheckAlertNamed]...),
			fmt.Sprintf("and %d more", len(parts)-modelCheckAlertNamed))
	}
	message := lead + " " + strings.Join(shown, "; ") + "."

	actionURL := "/models/" + modelID + "?tab=checks"
	if runID != "" {
		actionURL += "&run=" + runID
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"type":           "data_check",
		"saved_query_id": modelID,
		"message":        message,
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
		"action_url":     actionURL,
		"error": map[string]interface{}{
			"failure_type": "DATA_CHECK",
			"code":         code,
			"severity":     severity,
			"audience":     "user",
			"user_message": message,
			// One alert per run: a model blocked every hour is a new fact every hour.
			"dedup_subject": runID,
		},
	})
	return raw
}
