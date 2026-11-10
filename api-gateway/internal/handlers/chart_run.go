package handlers

// chart_run.go runs a chart's query (dashboards-design.md § 4.1): the source is loaded
// in the caller's workspace and only when the caller may see it, the SQL is held to one
// read statement for every role at save and at draw, and it runs through the Explorer's
// executors (a read-only transaction, their timeouts, their per-column masking) with a
// cap of charts.MaxRows. Nothing is cached (§ 4.3).

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
	"time"

	"api-gateway/internal/charts"
	"api-gateway/internal/security"
	"api-gateway/internal/validators"
	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/sqlexec"

	"github.com/rsync-ai/shared/sqlexec/mask"
	log "github.com/sirupsen/logrus"
)

// chartSourceRow is a chart's source, a model or a saved query, with its connection.
type chartSourceRow struct {
	ID              string
	WorkspaceID     string
	Name            string
	SQLText         string
	Materialization string
	TargetTable     string
	Visibility      string
	CreatedBy       string
	LastRunStatus   string
	LastRunError    string
	DeadlineSeconds int
	// Built is true once the source has a succeeded run; a model that has never been
	// built has no table to read.
	Built           bool
	ConnectionID    string
	ConnectionName  string
	ConnectorType   string
	ConfigEncrypted string
	// WeekStart and FiscalMonth are the connection's calendar (chartSource's).
	WeekStart   int
	FiscalMonth int
}

// IsModel reports whether the source is a model with a table rsync.ai builds.
func (s *chartSourceRow) IsModel() bool { return s.Materialization == matTable }

// Kind is "model" or "saved_query".
func (s *chartSourceRow) Kind() string {
	if s.IsModel() {
		return "model"
	}
	return "saved_query"
}

var errChartSourceNotFound = errors.New("the chart's source is not in this workspace, or is private to its author")

// loadChartSource reads a source the caller may see: in workspaceID, and shared with the
// workspace or the caller's own. Anything else is errChartSourceNotFound, the same
// answer as for an id that does not exist.
func loadChartSource(ctx context.Context, database *sql.DB, workspaceID, userID, id string) (*chartSourceRow, error) {
	var s chartSourceRow
	err := database.QueryRowContext(ctx, `
		SELECT sq.id, sq.workspace_id, COALESCE(sq.name, ''), sq.sql_text, sq.materialization,
		       COALESCE(sq.target_table, ''), sq.visibility, COALESCE(sq.created_by::text, ''),
		       COALESCE(sq.last_run_status, ''), COALESCE(sq.last_run_error, ''),
		       COALESCE(sq.freshness_deadline_seconds, 0),
		       EXISTS (SELECT 1 FROM saved_query_runs r
		               WHERE r.saved_query_id = sq.id AND r.status = 'succeeded'),
		       c.id, c.name, c.connector_type, c.config, c.week_start, c.fiscal_year_start_month
		FROM saved_queries sq
		JOIN connections c ON c.id = sq.connection_id AND c.workspace_id = sq.workspace_id
		WHERE sq.id = $1 AND sq.workspace_id = $2
		  AND (sq.visibility = 'workspace' OR sq.created_by = $3)
	`, id, workspaceID, userID).Scan(&s.ID, &s.WorkspaceID, &s.Name, &s.SQLText, &s.Materialization,
		&s.TargetTable, &s.Visibility, &s.CreatedBy, &s.LastRunStatus, &s.LastRunError,
		&s.DeadlineSeconds, &s.Built,
		&s.ConnectionID, &s.ConnectionName, &s.ConnectorType, &s.ConfigEncrypted, &s.WeekStart, &s.FiscalMonth)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errChartSourceNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// chartSourceRefusal says why a source cannot be charted at all, or "".
func chartSourceRefusal(s *chartSourceRow) string {
	ecap := ResolveExplorerCapability(s.ConnectorType)
	if !ecap.Supported || ecap.QueryLanguage != langSQL || modelDialect(s.ConnectorType) == "" {
		return fmt.Sprintf("Charts read SQL, and %s is not a SQL connection the Explorer can query.",
			getFriendlyName(s.ConnectorType))
	}
	switch s.Materialization {
	case matTable:
		if _, _, err := validateModelTarget(s.TargetTable); err != nil {
			return "The model has no table to chart: " + err.Error() + "."
		}
	case matNone:
		if reason := chartSQLRefusal(s.SQLText); reason != "" {
			return reason
		}
		// The Explorer's path lowers every LIMIT in a statement to its cap once the first
		// is above it (stmt.ClampLimit), the saved query's own included, so a chart
		// over one with a larger LIMIT would count fewer rows than the query returns.
		if n := largestSQLLimit(s.SQLText); n > charts.MaxRows+1 {
			return fmt.Sprintf("This saved query has LIMIT %d. A chart reads it through the Explorer's path, which lowers any LIMIT above %d, so the chart would count fewer rows than the query returns. Lower its LIMIT to %d or less, or chart a model built from it.",
				n, charts.MaxRows+1, charts.MaxRows)
		}
		// SQL Server takes no WITH inside a derived table, and a chart reads its saved
		// query as one.
		if modelDialect(s.ConnectorType) == "sqlserver" &&
			strings.HasPrefix(strings.ToUpper(strings.TrimSpace(stripLeadingSQLComments(s.SQLText))), "WITH") {
			return "A SQL Server saved query that starts with WITH cannot be charted; save it as a model and chart its table."
		}
	default:
		return "A model that runs a statement has no table to chart; chart a model that builds a table, or a saved query."
	}
	return ""
}

var sqlLimitPattern = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)

// largestSQLLimit is the largest LIMIT n written in a statement, or 0.
func largestSQLLimit(sqlText string) int {
	most := 0
	for _, m := range sqlLimitPattern.FindAllStringSubmatch(sqlText, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return math.MaxInt32
		}
		if n > most {
			most = n
		}
	}
	return most
}

// stripLeadingSQLComments drops the comments and space before a statement's first word.
func stripLeadingSQLComments(sqlText string) string {
	t := strings.TrimSpace(sqlText)
	for {
		switch {
		case strings.HasPrefix(t, "--"):
			if i := strings.IndexByte(t, '\n'); i >= 0 {
				t = strings.TrimSpace(t[i+1:])
				continue
			}
			return ""
		case strings.HasPrefix(t, "/*"):
			if i := strings.Index(t, "*/"); i >= 0 {
				t = strings.TrimSpace(t[i+2:])
				continue
			}
			return ""
		}
		return t
	}
}

// chartSQLRefusal holds a statement to one read, for every role (§ 4.1): a dashboard an
// owner saved never writes when a viewer opens it.
func chartSQLRefusal(sqlText string) string {
	if v := validators.ValidateExplorerStatement(sqlText, security.WSViewer); v == nil || !v.Valid {
		msg := "it is not a single valid statement"
		if v != nil && v.ErrorMessage != "" {
			msg = v.ErrorMessage
		}
		return "A chart reads one read-only SELECT: " + msg + "."
	}
	if class := validators.ClassifyStatementSQL(sqlText); class != validators.ClassRead {
		return fmt.Sprintf("A chart reads one read-only SELECT; this is a %s statement. A chart never changes data.",
			strings.ReplaceAll(string(class), "_", " "))
	}
	return ""
}

// chartSourceFor is the FROM item a chart reads: the model's table, or the saved
// query as a subquery.
func chartSourceFor(s *chartSourceRow) (chartSource, error) {
	d := modelDialect(s.ConnectorType)
	if s.IsModel() {
		schema, table, err := validateModelTarget(s.TargetTable)
		if err != nil {
			return chartSource{}, err
		}
		return chartSource{Dialect: d, Table: quoteModelIdent(d, schema, table), WeekStart: s.WeekStart, FiscalMonth: s.FiscalMonth}, nil
	}
	return chartSource{Dialect: d, SQL: s.SQLText, WeekStart: s.WeekStart, FiscalMonth: s.FiscalMonth}, nil
}

// runChartSQL runs one chart statement on the source's connection, masked as the
// Explorer masks, at most limit rows.
func runChartSQL(ctx context.Context, s *chartSourceRow, q *chartQuery, limit int) (*ExplorerQueryResponse, error) {
	if reason := chartSQLRefusal(q.SQL); reason != "" {
		return nil, errors.New(reason)
	}
	configJSON, err := crypto.DecryptString(s.ConfigEncrypted)
	if err != nil {
		return nil, errors.New("the connection's saved credentials could not be decrypted")
	}
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, errors.New("the connection's saved settings could not be read")
	}
	// A dialect binds either positional args or Databricks' named params, never both
	// (chartBinder.bind); the engine takes whichever its dialect binds.
	args := append([]any{}, q.Args...)
	for _, p := range q.Params {
		args = append(args, p)
	}
	var res *ExplorerQueryResponse
	eng, err := explorerEngine(s.ConnectorType, config)
	if err == nil {
		res, err = eng.Query(ctx, q.SQL, args, sqlexec.Limits{Rows: limit, Mask: true})
	}
	if err != nil {
		errorType, _ := validators.ClassifyExecutionError(err)
		log.WithError(err).WithField("error_type", errorType).Warn("[charts] the chart statement failed")
		return nil, &chartRunError{Type: errorType, msg: fmt.Sprintf("the query failed (%s): %s", errorType, capChartError(err.Error()))}
	}
	if res == nil {
		return nil, errors.New("the query returned no result")
	}
	return res, nil
}

// chartRunError is the engine's own failure of a chart statement, with its class
// (validators.ClassifyExecutionError), so a caller can tell a slow or unreachable source
// from a statement the engine refused.
type chartRunError struct {
	Type string
	msg  string
}

func (e *chartRunError) Error() string { return e.msg }

// runChartQuery runs a chart's statement, and when the engine refuses it or it times out
// and the chart kept one without the order it added (chartQuery.Unordered: a table of
// rows ordered by each of its columns, one of which the engine may not order, an array
// or a JSON value, or may take too long ordering on a big table), runs that one instead
// and records it as the SQL that ran. A source that is unreachable, closed to the
// connection or missing a column is not run twice: the second run would fail the same.
func runChartQuery(ctx context.Context, s *chartSourceRow, q *chartQuery, limit int) (*ExplorerQueryResponse, error) {
	res, err := runChartSQL(ctx, s, q, limit)
	if err == nil || q.Unordered == "" || ctx.Err() != nil {
		return res, err
	}
	switch chartRunClass(err) {
	case "", "network_or_unavailable", "permission_denied", "missing_table_or_column":
		return res, err
	}
	unordered := *q
	unordered.SQL = q.Unordered
	if res, rerr := runChartSQL(ctx, s, &unordered, limit); rerr == nil {
		q.SQL = q.Unordered
		return res, nil
	}
	return nil, err
}

// chartRunClass is the class of the engine's own failure of a chart statement
// (chartRunError.Type), or "" for a failure before the engine ran it.
func chartRunClass(err error) string {
	var runErr *chartRunError
	if !errors.As(err, &runErr) {
		return ""
	}
	return runErr.Type
}

// chartKindsRefusal is why the engine refused a chart's statement when a column the
// settings name holds a kind its role or its aggregate cannot use (CheckKinds, failed),
// so the chart says that in place of the engine's own error; nil when the run failed
// some other way (slow, unreachable, closed to the connection) or every kind fits. A
// "does not exist" counts as refused: each caller has already found every column the
// settings name, so it is a function the column's kind has none of (Postgres "function
// date_trunc(unknown, text) does not exist").
func chartKindsRefusal(settings *charts.Settings, kinds map[string]string, runErr error) error {
	switch chartRunClass(runErr) {
	case "", "timeout", "network_or_unavailable", "permission_denied":
		return nil
	}
	return settings.CheckKinds(kinds, true)
}

// The parts of an engine's error that say where it ran, not why it failed: the request
// a client library sent ("400 POST https://bigquery.googleapis.com/…/projects/<id>/
// queries: "), any other link, and BigQuery's "Location:" and "Job ID:" lines.
var (
	chartErrorRequest = regexp.MustCompile(`(?:\b\d{3} )?\b(?:GET|POST|PUT|PATCH|DELETE) https?://\S+?: `)
	chartErrorLink    = regexp.MustCompile(`;?\s*\bsee https?://\S+|https?://\S+`)
	chartErrorJob     = regexp.MustCompile(`(?m)^\s*(?:Location|Job ID): \S*\s*$`)
)

// capChartError keeps an engine's error to a sentence or two, without the engine's
// links, its job or where it ran: the chart's viewers see it, and the run's own logs
// keep the whole text (runChartSQL).
func capChartError(msg string) string {
	const max = 400
	msg = chartErrorRequest.ReplaceAllString(msg, "")
	msg = chartErrorLink.ReplaceAllString(msg, "")
	msg = chartErrorJob.ReplaceAllString(msg, "")
	lines := strings.Split(msg, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line = strings.TrimRight(line, " \t\r"); line != "" {
			kept = append(kept, line)
		}
	}
	msg = strings.TrimSpace(strings.Join(kept, "\n"))
	r := []rune(msg)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return msg
}

// chartColumn is one of a source's columns: its name and a kind read from the type of
// its first value (number, time, text, bool), or unknown. Never the value itself.
type chartColumn struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// probeChartSource runs the source once, one row (§ 3.2 rule 2), for its columns.
func probeChartSource(ctx context.Context, s *chartSourceRow) ([]chartColumn, error) {
	src, err := chartSourceFor(s)
	if err != nil {
		return nil, err
	}
	res, err := runChartSQL(ctx, s, &chartQuery{SQL: writeSourceProbeSQL(src)}, 1)
	if err != nil {
		return nil, err
	}
	if len(res.Columns) == 0 {
		return nil, errors.New("the source returned no columns")
	}
	return sourceColumns(res), nil
}

// sourceColumns is each of a probe's columns with the kind of its first value, or of
// the type the database declared for it where the value says less: a column declared
// as text holds text even where its values read as numbers (a code, a postcode), which
// a sum would otherwise be let run over, and a first value of NULL says nothing.
func sourceColumns(res *ExplorerQueryResponse) []chartColumn {
	var first map[string]interface{}
	if len(res.Rows) > 0 {
		first = res.Rows[0]
	}
	out := make([]chartColumn, 0, len(res.Columns))
	for i, name := range res.Columns {
		kind := "unknown"
		if first != nil {
			kind = valueKind(first[name])
		}
		declared := ""
		if i < len(res.ColumnTypes) {
			declared = declaredKind(res.ColumnTypes[i])
		}
		if declared == "text" && kind == "number" || declared != "" && kind == "unknown" {
			kind = declared
		}
		if mask.ShouldRedactColumnName(name) {
			// A masked value is text whatever the column holds.
			kind = "unknown"
		}
		out = append(out, chartColumn{Name: name, Kind: kind})
	}
	return out
}

// declaredKind is the kind a column's declared type names (VARCHAR text, NUMERIC a
// number, TIMESTAMPTZ a time), as Postgres, MySQL, SQL Server and Databricks name them;
// "" for a type it does not know.
func declaredKind(typ string) string {
	t := strings.ToUpper(strings.TrimSpace(typ))
	if i := strings.IndexAny(t, "(<"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	t = strings.TrimPrefix(t, "UNSIGNED ")
	switch t {
	case "TEXT", "VARCHAR", "CHAR", "BPCHAR", "CHARACTER", "CHARACTER VARYING", "CITEXT", "NAME",
		"NVARCHAR", "NCHAR", "NTEXT", "TINYTEXT", "MEDIUMTEXT", "LONGTEXT", "STRING":
		return "text"
	case "INT2", "INT4", "INT8", "SMALLINT", "INTEGER", "INT", "BIGINT", "TINYINT", "MEDIUMINT",
		"FLOAT4", "FLOAT8", "REAL", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "NUMERIC", "DECIMAL":
		return "number"
	case "DATE", "TIMESTAMP", "TIMESTAMPTZ", "TIMESTAMP_NTZ", "DATETIME", "DATETIME2", "DATETIMEOFFSET", "SMALLDATETIME":
		return "time"
	}
	return ""
}

func chartColumnNames(cols []chartColumn) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

// chartColumnKinds maps each of a source's columns to the kind its probe read.
func chartColumnKinds(cols []chartColumn) map[string]string {
	kinds := make(map[string]string, len(cols))
	for _, c := range cols {
		kinds[c.Name] = c.Kind
	}
	return kinds
}

// valueKind names a value's type for the chart page's column list.
func valueKind(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "unknown"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return "number"
	case time.Time:
		return "time"
	case string:
		if _, err := strconv.ParseFloat(t, 64); err == nil {
			return "number"
		}
		// A timestamp may come as text with its offset after a space (Python's str of a
		// datetime, Postgres's text form, BigQuery's "UTC"); a fraction of a second after
		// the seconds parses with any of these.
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02",
			"2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05Z07", "2006-01-02 15:04:05 MST"} {
			if _, err := time.Parse(layout, t); err == nil {
				return "time"
			}
		}
		return "text"
	}
	return "unknown"
}

// chartResult is a drawn chart: its rows in the order of the settings' outputs.
type chartResult struct {
	Columns []chartOutput   `json:"columns"`
	Rows    [][]interface{} `json:"rows"`
	// Truncated says the query hit its type's row cap (charts.MaxRows, a treemap's
	// 3,500 parts, a calendar's 366 days); the chart says so, never silently.
	Truncated bool `json:"truncated"`
	// Masked lists the outputs the Explorer's masking redacted.
	Masked []string `json:"masked"`
	// Unfiltered lists the filter columns this connection could not apply.
	Unfiltered []string `json:"unfiltered"`
	// Spark is a scorecard's sparkline over the current period, a [date, value] per
	// point, oldest first; absent when its measure or date is masked.
	Spark [][]interface{} `json:"spark,omitempty"`
	// Approximate is a box plot drawn from the engine's approximate quartiles, which
	// the chart says under it.
	Approximate bool `json:"approximate,omitempty"`
	// Unordered is a table of rows drawn from its statement without the order of each
	// column (runChartQuery), so rows may come in a different order each draw; the
	// chart says so.
	Unordered       bool      `json:"unordered,omitempty"`
	SQL             string    `json:"sql"`
	ReadAt          time.Time `json:"read_at"`
	ExecutionTimeMs int64     `json:"execution_time_ms"`
}

type chartOutput struct {
	Alias string `json:"alias"`
	Role  string `json:"role"`
}

// shapeChartResult puts the result's rows in the order of outs. A column is found by
// its alias, then without case (Snowflake and Oracle hand back an upper-cased name),
// then by place when the counts match.
func shapeChartResult(s *charts.Settings, res *ExplorerQueryResponse) (*chartResult, error) {
	outs := s.Outputs()
	idx := make([]string, len(outs))
	for i, o := range outs {
		for _, c := range res.Columns {
			if c == o.Alias {
				idx[i] = c
				break
			}
		}
		if idx[i] == "" {
			for _, c := range res.Columns {
				if strings.EqualFold(c, o.Alias) {
					idx[i] = c
					break
				}
			}
		}
		if idx[i] == "" && len(res.Columns) == len(outs) {
			idx[i] = res.Columns[i]
		}
		if idx[i] == "" {
			return nil, fmt.Errorf("the result has no column %q", o.Alias)
		}
	}
	out := &chartResult{
		Columns:         make([]chartOutput, len(outs)),
		Rows:            make([][]interface{}, 0, len(res.Rows)),
		Truncated:       res.Truncated,
		Masked:          []string{},
		Unfiltered:      []string{},
		ExecutionTimeMs: res.ExecutionTimeMs,
	}
	masked := map[string]bool{}
	for i, o := range outs {
		out.Columns[i] = chartOutput{Alias: o.Alias, Role: o.Role}
		if mask.ShouldRedactColumnName(idx[i]) {
			out.Masked = append(out.Masked, o.Alias)
			masked[o.Alias] = true
		}
	}
	// An output read from a masked column under a name the masking does not know is
	// masked with it: a scorecard's earlier period is the same measure, a histogram's bin
	// width is its x's spread, and a box plot's statistics and a candlestick's prices are
	// their y column's values. (A second measure's "_2" keeps its column's name, which the
	// masking reads.)
	redact := map[int]bool{}
	for i, o := range outs {
		var hidden bool
		switch {
		case o.Alias == charts.PreviousAlias && s.Type == charts.TypeScorecard:
			if m := s.One(charts.RoleMeasure); m != nil {
				hidden = masked[m.MeasureAlias()]
			}
		case o.Alias == charts.BinWidthAlias && s.Type == charts.TypeHistogram:
			hidden = masked[o.Source]
		case (s.Type == charts.TypeBoxplot || s.Type == charts.TypeCandlestick) && o.Alias != o.Source:
			// The y column is not itself an output, so its name is read here.
			hidden = mask.ShouldRedactColumnName(o.Source)
		}
		if hidden && !masked[o.Alias] {
			redact[i] = true
			out.Masked = append(out.Masked, o.Alias)
		}
	}
	for _, row := range res.Rows {
		r := make([]interface{}, len(outs))
		for i := range outs {
			v := row[idx[i]]
			if redact[i] {
				v = mask.RedactForPreview(v)
			}
			r[i] = v
		}
		out.Rows = append(out.Rows, r)
	}
	return out, nil
}

// drawChart writes and runs a chart's SQL. A failure that comes from a column the source
// no longer has is a break in structure and says so (§ 5); one that comes from a column
// of a kind the chart cannot use is the *charts.Error that says so, which a caller shows
// as a refused setting.
func drawChart(ctx context.Context, s *chartSourceRow, settings *charts.Settings, filters []charts.Bound, now time.Time) (*chartResult, *chartBreak, error) {
	if brk := chartSourceBreak(s); brk != nil {
		return nil, brk, nil
	}
	src, err := chartSourceFor(s)
	if err != nil {
		return nil, &chartBreak{Reason: "source_invalid", Message: err.Error()}, nil
	}
	q, err := writeChartSQL(settings, src, filters, now)
	if err != nil {
		return nil, nil, err
	}
	res, runErr := runChartQuery(ctx, s, q, charts.MaxRows)
	if runErr != nil {
		// Was it a column that has gone? Run the source once to find out.
		if cols, perr := probeChartSource(ctx, s); perr == nil {
			if missing := settings.MissingColumns(chartColumnNames(cols)); len(missing) > 0 {
				return nil, &chartBreak{
					Reason:  "column_gone",
					Message: fmt.Sprintf("The source no longer has the column %s this chart uses.", strings.Join(quoteEach(missing), ", ")),
				}, nil
			}
			// Or a column of a kind the chart cannot use: a refused setting (a
			// *charts.Error), said in place of the engine's own error.
			if err := chartKindsRefusal(settings, chartColumnKinds(cols), runErr); err != nil {
				return nil, nil, err
			}
		}
		return nil, nil, runErr
	}
	out, err := shapeChartResult(settings, res)
	if err != nil {
		return nil, nil, err
	}
	if spec := charts.Spec(settings.Type); spec != nil && len(out.Rows) > spec.Cap.Rows {
		// A type that draws fewer rows than the run reads (a treemap's parts, a
		// calendar's days) reads one more than it draws, to know it was cut. The run's own
		// cap stays MaxRows: a lower one would lower a saved query's own LIMIT with it.
		out.Rows = out.Rows[:spec.Cap.Rows]
		out.Truncated = true
	}
	out.Approximate = q.Approximate
	out.Unordered = q.Unordered != "" && q.SQL == q.Unordered
	out.SQL = q.SQL
	out.ReadAt = now.UTC()
	if q.Unfiltered != nil {
		out.Unfiltered = q.Unfiltered
	}
	if q.Spark != nil && len(out.Masked) == 0 {
		// A sparkline that does not run fails the draw, rather than draw a scorecard
		// that silently lost it.
		res, err := runChartSQL(ctx, s, q.Spark, settings.Compare.Days+1)
		if err != nil {
			return nil, nil, fmt.Errorf("the sparkline: %w", err)
		}
		if out.Spark, err = shapeSpark(settings, res); err != nil {
			return nil, nil, err
		}
		out.SQL += ";\n\n-- the sparkline\n" + q.Spark.SQL
	}
	return out, nil, nil
}

// shapeSpark puts a sparkline's rows in [date, value] order, found by alias as
// shapeChartResult finds them. A masked date or value draws no sparkline.
func shapeSpark(s *charts.Settings, res *ExplorerQueryResponse) ([][]interface{}, error) {
	aliases := []string{s.One(charts.RoleDate).Column, s.One(charts.RoleMeasure).MeasureAlias()}
	idx := make([]string, len(aliases))
	for i, a := range aliases {
		for _, c := range res.Columns {
			if strings.EqualFold(c, a) && (idx[i] == "" || c == a) {
				idx[i] = c
			}
		}
		if idx[i] == "" {
			return nil, fmt.Errorf("the sparkline's result has no column %q", a)
		}
		if mask.ShouldRedactColumnName(idx[i]) {
			return nil, nil
		}
	}
	out := make([][]interface{}, 0, len(res.Rows))
	for _, row := range res.Rows {
		out = append(out, []interface{}{row[idx[0]], row[idx[1]]})
	}
	return out, nil
}

func quoteEach(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return out
}

// chartBreak replaces a chart with a message: its source was deleted, a column it uses
// is gone, or the model has never been built (§ 5).
type chartBreak struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// chartSourceBreak is the break a source shows before anything is run.
func chartSourceBreak(s *chartSourceRow) *chartBreak {
	if s == nil {
		return &chartBreak{Reason: "source_deleted", Message: "This chart's source was deleted."}
	}
	if s.IsModel() && !s.Built {
		return &chartBreak{Reason: "never_built", Message: "The model " + strconv.Quote(s.Name) + " has never been built, so it has no rows yet."}
	}
	if reason := chartSourceRefusal(s); reason != "" {
		return &chartBreak{Reason: "source_invalid", Message: reason}
	}
	return nil
}
