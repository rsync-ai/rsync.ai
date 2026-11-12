package handlers

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/charts"
)

// A saved chart's SQL (dashboards-design.md § 3.1, § 4.2): one read over its source, the
// grouping on the engine, a row cap that shows a capped result, and a viewer's filter
// values bound as parameters on every engine that takes them, never in the SQL text.

func mustSettings(t *testing.T, raw string) *charts.Settings {
	t.Helper()
	s, err := charts.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode(%s) = %v", raw, err)
	}
	return s
}

const sqlBarSum = `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"sum","column":"amount"}}`

// A value a person picked, with every character that would matter in SQL.
const sqlHostile = "EU'); DROP TABLE orders; --"

func sqlFilters() []charts.Bound {
	return []charts.Bound{
		{Column: "day", Kind: charts.FilterDateRange,
			From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
		{Column: "region", Kind: charts.FilterValues, Values: []string{"US", sqlHostile}},
	}
}

func TestWriteChartSQL_EachEngineBindsAndCaps(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		dialect, table string
		want           string
		args           []interface{}
		params         []databricksParam
	}{
		{"postgres", `"gold"."orders"`,
			"SELECT src.\"day\" AS \"day\", SUM(src.\"amount\") AS \"amount\"\nFROM \"gold\".\"orders\" src\n" +
				"WHERE src.\"day\" >= $1 AND src.\"day\" < $2 AND src.\"region\" IN ($3, $4)\n" +
				"GROUP BY src.\"day\"\nORDER BY SUM(src.\"amount\") DESC\nLIMIT 10001",
			[]interface{}{"2026-10-01", "2026-10-06", "US", sqlHostile}, nil},
		{"mysql", "`gold`.`orders`",
			"SELECT src.`day` AS `day`, SUM(src.`amount`) AS `amount`\nFROM `gold`.`orders` src\n" +
				"WHERE src.`day` >= ? AND src.`day` < ? AND src.`region` IN (?, ?)\n" +
				"GROUP BY src.`day`\nORDER BY SUM(src.`amount`) DESC\nLIMIT 10001",
			[]interface{}{"2026-10-01", "2026-10-06", "US", sqlHostile}, nil},
		{"sqlserver", "[gold].[orders]",
			"SELECT TOP (10001) src.[day] AS [day], SUM(src.[amount]) AS [amount]\nFROM [gold].[orders] src\n" +
				"WHERE src.[day] >= @p1 AND src.[day] < @p2 AND src.[region] IN (@p3, @p4)\n" +
				"GROUP BY src.[day]\nORDER BY SUM(src.[amount]) DESC",
			[]interface{}{"2026-10-01", "2026-10-06", "US", sqlHostile}, nil},
		{"databricks", "`gold`.`orders`",
			"SELECT src.`day` AS `day`, SUM(src.`amount`) AS `amount`\nFROM `gold`.`orders` src\n" +
				"WHERE src.`day` >= :p1 AND src.`day` < :p2 AND src.`region` IN (:p3, :p4)\n" +
				"GROUP BY src.`day`\nORDER BY SUM(src.`amount`) DESC\nLIMIT 10001",
			nil, []databricksParam{
				{Name: "p1", Value: "2026-10-01", Type: "STRING"}, {Name: "p2", Value: "2026-10-06", Type: "STRING"},
				{Name: "p3", Value: "US", Type: "STRING"}, {Name: "p4", Value: sqlHostile, Type: "STRING"}}},
	} {
		t.Run(c.dialect, func(t *testing.T) {
			q, err := writeChartSQL(mustSettings(t, sqlBarSum), chartSource{Dialect: c.dialect, Table: c.table}, sqlFilters(), now)
			if err != nil {
				t.Fatal(err)
			}
			if q.SQL != c.want {
				t.Fatalf("SQL =\n%s\nwant\n%s", q.SQL, c.want)
			}
			if !reflect.DeepEqual(q.Args, c.args) || !reflect.DeepEqual(q.Params, c.params) {
				t.Fatalf("bound args %#v params %#v, want %#v %#v", q.Args, q.Params, c.args, c.params)
			}
			if strings.Contains(q.SQL, "DROP") || strings.Contains(q.SQL, "US") || len(q.Unfiltered) != 0 {
				t.Fatalf("a filter value reached the SQL, or a filter was dropped: %s %v", q.SQL, q.Unfiltered)
			}
		})
	}
}

// The orchestrator's engines take no bound values, so their filters are named as not
// applied and nothing a person typed goes into the SQL.
func TestWriteChartSQL_DelegatedEngineDrawsUnfiltered(t *testing.T) {
	for _, d := range []string{"bigquery", "snowflake", "clickhouse", "oracle"} {
		t.Run(d, func(t *testing.T) {
			q, err := writeChartSQL(mustSettings(t, sqlBarSum), chartSource{Dialect: d, Table: "gold.orders"}, sqlFilters(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(q.SQL, "WHERE") || strings.Contains(q.SQL, "DROP") || strings.Contains(q.SQL, "2026-10-01") {
				t.Fatalf("SQL has a filter: %s", q.SQL)
			}
			if !reflect.DeepEqual(q.Unfiltered, []string{"day", "region"}) || len(q.Args) != 0 || len(q.Params) != 0 {
				t.Fatalf("Unfiltered %v args %v params %v, want both filters named and nothing bound", q.Unfiltered, q.Args, q.Params)
			}
			wantCap := "\nLIMIT 10001"
			if d == "oracle" {
				wantCap = "\nFETCH FIRST 10001 ROWS ONLY"
			}
			if !strings.HasSuffix(q.SQL, wantCap) {
				t.Fatalf("SQL ends %q, want %q", q.SQL[len(q.SQL)-30:], wantCap)
			}
		})
	}
	if _, err := writeChartSQL(mustSettings(t, sqlBarSum), chartSource{Table: "t"}, nil, time.Now()); err == nil {
		t.Fatal("a source with no dialect was written")
	}
}

// A column name is quoted with the engine's closing quote escaped, so any name the
// source returned is only a name.
func TestWriteChartSQL_QuotesOddColumnNames(t *testing.T) {
	s := mustSettings(t, `{"type":"table","columns":["a\"b","it's","x]y"]}`)
	q, err := writeChartSQL(s, chartSource{Dialect: "postgres", Table: `"t"`}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(q.SQL, `SELECT src."a""b" AS "a""b", src."it's" AS "it's", src."x]y" AS "x]y"`) {
		t.Fatalf("postgres quoting: %s", q.SQL)
	}
	q, _ = writeChartSQL(s, chartSource{Dialect: "sqlserver", Table: "[t]"}, nil, time.Now())
	if !strings.Contains(q.SQL, `src.[x]]y] AS [x]]y]`) {
		t.Fatalf("sqlserver quoting: %s", q.SQL)
	}
}

// A saved query is read as a subquery, its trailing semicolons dropped.
func TestWriteChartSQL_SavedQueryNests(t *testing.T) {
	src := chartSource{Dialect: "postgres", SQL: "  SELECT day, amount FROM orders ;; \n"}
	q, err := writeChartSQL(mustSettings(t, `{"type":"table","columns":["day"]}`), src, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q.SQL, "FROM (\nSELECT day, amount FROM orders\n) src") {
		t.Fatalf("SQL = %s", q.SQL)
	}
	if got := writeSourceProbeSQL(src); got != "SELECT * FROM (\nSELECT day, amount FROM orders\n) src\nLIMIT 1" {
		t.Fatalf("probe = %q", got)
	}
}

// A scorecard's comparison: the last N days up to today against the N before, bound
// where the engine binds, written as dates made by the server where it does not.
func TestWriteChartSQL_ScorecardCompare(t *testing.T) {
	s := mustSettings(t, `{"type":"scorecard","measure":{"aggregate":"count"},"compare":{"column":"day","days":7}}`)
	now := time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC)

	q, err := writeChartSQL(s, chartSource{Dialect: "postgres", Table: `"t"`}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT COUNT(CASE WHEN src.\"day\" >= $1 AND src.\"day\" < $2 THEN 1 END) AS \"count\", " +
		"COUNT(CASE WHEN src.\"day\" >= $3 AND src.\"day\" < $4 THEN 1 END) AS \"previous\"\nFROM \"t\" src\nLIMIT 10001"
	if q.SQL != want {
		t.Fatalf("SQL =\n%s\nwant\n%s", q.SQL, want)
	}
	// Today is in the current period: [09-29, 10-06) against [09-22, 09-29).
	if !reflect.DeepEqual(q.Args, []interface{}{"2026-09-29", "2026-10-06", "2026-09-22", "2026-09-29"}) {
		t.Fatalf("args = %v", q.Args)
	}

	q, _ = writeChartSQL(s, chartSource{Dialect: "oracle", Table: `"T"`}, nil, now)
	if !strings.Contains(q.SQL, `src."DAY" >= DATE '2026-09-29' AND src."DAY" < DATE '2026-10-06'`) || len(q.Args) != 0 {
		t.Fatalf("oracle SQL = %s args %v", q.SQL, q.Args)
	}
	q, _ = writeChartSQL(s, chartSource{Dialect: "bigquery", Table: "t"}, nil, now)
	if !strings.Contains(q.SQL, "src.`day` >= '2026-09-22' AND src.`day` < '2026-09-29'") {
		t.Fatalf("bigquery SQL = %s", q.SQL)
	}
	// SQL Server counts with COUNT_BIG, whose INT COUNT overflows.
	q, _ = writeChartSQL(s, chartSource{Dialect: "sqlserver", Table: "[t]"}, nil, now)
	if !strings.Contains(q.SQL, "COUNT_BIG(CASE WHEN") {
		t.Fatalf("sqlserver SQL = %s", q.SQL)
	}
}

// With no sort set, a bar of one series is ordered by its value, largest first, unless
// its x is grouped by a date grain, which reads in time order like a line's; everything
// else is ordered by x, and the other dimensions follow so the rows keep their order.
func TestWriteChartSQL_DefaultSorts(t *testing.T) {
	src := chartSource{Dialect: "postgres", Table: `"t"`}
	for _, c := range []struct{ raw, order string }{
		{sqlBarSum, `ORDER BY SUM(src."amount") DESC`},
		{`{"type":"hbar","x":{"column":"r"},"measure":{"aggregate":"count"}}`, `ORDER BY COUNT(*) DESC`},
		// Months and years in time order, oldest first, as a line draws the same settings.
		{`{"version":2,"type":"bar","roles":{"x":[{"column":"d","grain":"month"}],"measure":[{"aggregate":"sum","column":"a"}]}}`, `ORDER BY DATE_TRUNC('month', src."d") ASC`},
		{`{"version":2,"type":"hbar","roles":{"x":[{"column":"d","grain":"year"}],"measure":[{"aggregate":"count"}]}}`, `ORDER BY DATE_TRUNC('year', src."d") ASC`},
		{`{"version":2,"type":"line","roles":{"x":[{"column":"d","grain":"month"}],"measure":[{"aggregate":"sum","column":"a"}]}}`, `ORDER BY DATE_TRUNC('month', src."d") ASC`},
		// A sort picked is kept.
		{`{"version":2,"type":"bar","roles":{"x":[{"column":"d","grain":"month"}],"measure":[{"aggregate":"sum","column":"a"}]},"sort":"value_desc"}`, `ORDER BY SUM(src."a") DESC`},
		{`{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s"}}`,
			`ORDER BY r."d" ASC, CASE WHEN r."rsync_rank" <= 7 THEN r."s" END ASC, CASE WHEN r."rsync_rank" <= 7 THEN 0 ELSE 1 END ASC`},
		{`{"type":"line","x":{"column":"d"},"measure":{"aggregate":"count_distinct","column":"c"}}`, `ORDER BY src."d" ASC`},
		{`{"type":"heatmap","x":{"column":"d"},"y":{"column":"r"},"measure":{"aggregate":"avg","column":"a"}}`, `ORDER BY src."d" ASC, src."r" ASC`},
		{`{"type":"area","x":{"column":"d"},"measure":{"aggregate":"max","column":"a"},"sort":"value_asc"}`, `ORDER BY MAX(src."a") ASC`},
		{`{"type":"table","columns":["d","r"],"measure":{"aggregate":"min","column":"a"},"sort":"x_desc"}`, `ORDER BY src."d" DESC, src."r" ASC`},
	} {
		q, err := writeChartSQL(mustSettings(t, c.raw), src, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL, "\n"+c.order+"\n") {
			t.Errorf("%s\nSQL =\n%s\nwant %s", c.raw, q.SQL, c.order)
		}
	}
	// A table of rows with no measure is not grouped.
	q, _ := writeChartSQL(mustSettings(t, `{"type":"table","columns":["d"]}`), src, nil, time.Now())
	if strings.Contains(q.SQL, "GROUP BY") {
		t.Fatalf("an ungrouped table was grouped: %s", q.SQL)
	}
}

// A table's rows are in an order every run, so the rows a capped one keeps are its first
// in that order, never whatever part of the source the engine read first (a table of
// 19,033 rows kept 4 of its 7 channels): after the sort picked, by each column in turn.
// A table of rows also keeps the statement with only the sort picked, which runs when the
// engine cannot order one of its columns (an array, a JSON value), so a table that draws
// today still draws. A grouped table needs none: its columns are in its GROUP BY, which
// such a column fails the same.
func TestWriteChartSQL_TableOrdersItsRows(t *testing.T) {
	src := chartSource{Dialect: "postgres", Table: `"t"`}
	const noOrder = "no ORDER BY"
	for _, c := range []struct{ raw, order, unordered string }{
		{`{"type":"table","columns":["d","c","n"]}`, `ORDER BY src."d" ASC, src."c" ASC, src."n" ASC`, noOrder},
		{`{"type":"table","columns":["d"]}`, `ORDER BY src."d" ASC`, noOrder},
		{`{"type":"table","columns":["d","c"],"sort":"x_desc"}`, `ORDER BY src."d" DESC, src."c" ASC`, `ORDER BY src."d" DESC`},
		{`{"type":"table","columns":["d","c"],"sort":"x_asc"}`, `ORDER BY src."d" ASC, src."c" ASC`, `ORDER BY src."d" ASC`},
		// One column sorted as picked: nothing is added, so there is nothing to fall back from.
		{`{"type":"table","columns":["d"],"sort":"x_asc"}`, `ORDER BY src."d" ASC`, ""},
		{`{"type":"table","columns":["d","r"],"measure":{"aggregate":"min","column":"a"}}`, `ORDER BY src."d" ASC, src."r" ASC`, ""},
		{`{"type":"table","columns":["d","r"],"measure":{"aggregate":"sum","column":"a"},"sort":"value_desc"}`,
			`ORDER BY SUM(src."a") DESC, src."d" ASC, src."r" ASC`, ""},
	} {
		q, err := writeChartSQL(mustSettings(t, c.raw), src, nil, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q.SQL, "\n"+c.order+"\n") {
			t.Errorf("%s\nSQL =\n%s\nwant %s", c.raw, q.SQL, c.order)
		}
		switch c.unordered {
		case "":
			if q.Unordered != "" {
				t.Errorf("%s: a fallback was written:\n%s", c.raw, q.Unordered)
			}
		case noOrder:
			if strings.Contains(q.Unordered, "ORDER BY") || !strings.Contains(q.Unordered, "\nLIMIT 10001") {
				t.Errorf("%s: fallback =\n%s\nwant the statement unordered, still capped", c.raw, q.Unordered)
			}
		default:
			if !strings.Contains(q.Unordered, "\n"+c.unordered+"\n") {
				t.Errorf("%s: fallback =\n%s\nwant %s", c.raw, q.Unordered, c.unordered)
			}
		}
	}
}

func TestWriteSourceProbeAndFilterValuesSQL(t *testing.T) {
	for _, c := range []struct{ dialect, table, probe, values string }{
		{"postgres", `"t"`, "SELECT * FROM \"t\" src\nLIMIT 1",
			"SELECT DISTINCT src.\"region\" AS \"region\" FROM \"t\" src\nWHERE src.\"region\" IS NOT NULL\nORDER BY src.\"region\"\nLIMIT 200"},
		// SQL Server's TOP for the values is the executor's (clampLimitSQLServer).
		{"sqlserver", "[t]", "SELECT TOP (1) * FROM [t] src",
			"SELECT DISTINCT src.[region] AS [region] FROM [t] src\nWHERE src.[region] IS NOT NULL\nORDER BY src.[region]"},
		{"oracle", `"T"`, "SELECT * FROM \"T\" src\nFETCH FIRST 1 ROWS ONLY",
			"SELECT DISTINCT src.\"REGION\" AS \"REGION\" FROM \"T\" src\nWHERE src.\"REGION\" IS NOT NULL\nORDER BY src.\"REGION\"\nFETCH FIRST 200 ROWS ONLY"},
	} {
		src := chartSource{Dialect: c.dialect, Table: c.table}
		if got := writeSourceProbeSQL(src); got != c.probe {
			t.Errorf("%s probe = %q, want %q", c.dialect, got, c.probe)
		}
		if got := writeFilterValuesSQL(src, "region", filterValuesMax); got != c.values {
			t.Errorf("%s values = %q, want %q", c.dialect, got, c.values)
		}
	}
}

// Every statement a chart writes passes the same one-read check the saved query must.
func TestWriteChartSQL_PassesTheReadCheck(t *testing.T) {
	for _, raw := range []string{
		sqlBarSum,
		`{"type":"scorecard","measure":{"aggregate":"sum","column":"amount"},"compare":{"column":"day","days":30}}`,
		`{"type":"table","columns":["day","region"],"measure":{"aggregate":"count_distinct","column":"c"},"sort":"value_desc"}`,
		// A series chart's Other is three nested SELECTs and window functions (§ 14.3).
		`{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"avg","column":"amount"},"series":{"column":"region"},"sort":"value_desc"}`,
	} {
		for _, d := range []string{"postgres", "mysql", "sqlserver", "databricks"} {
			q, err := writeChartSQL(mustSettings(t, raw), chartSource{Dialect: d, SQL: "SELECT * FROM orders;"}, sqlFilters(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if reason := chartSQLRefusal(q.SQL); reason != "" {
				t.Errorf("%s %s refused: %s\n%s", d, raw, reason, q.SQL)
			}
		}
	}
}

// A point is sized only by a size above zero, which the page draws: a spot whose size is
// zero or below is left out before the cap, so it never takes the place of one drawn
// (contract K7), on the plain path and on the coloured one. A scatter with no size keeps
// every x and y.
func TestWriteChartSQL_PointsNeedASizeAboveZero(t *testing.T) {
	for _, c := range []struct {
		raw   string
		sized bool
	}{
		{`{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}]}}`, false},
		{`{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}]}}`, true},
		{`{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}],"series":[{"column":"pk"}]}}`, true},
		{`{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}]}}`, true},
		{`{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}],"series":[{"column":"pk"}]}}`, true},
	} {
		s := mustSettings(t, c.raw)
		for _, src := range goldenChartSources {
			q, err := writeChartSQL(s, src, sqlFilters(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			above := "src." + quoteCheckColumn(src.Dialect, "pz") + " > 0"
			if got := strings.Contains(q.SQL, above); got != c.sized {
				t.Errorf("%s %s: sized by a size above zero = %v, want %v:\n%s", src.Dialect, c.raw, got, c.sized, q.SQL)
			}
			// Above zero is also not NULL, so the size is held to it once.
			if strings.Contains(q.SQL, "src."+quoteCheckColumn(src.Dialect, "pz")+" IS NOT NULL") {
				t.Errorf("%s %s: the size is held to IS NOT NULL as well as above zero:\n%s", src.Dialect, c.raw, q.SQL)
			}
		}
	}
}

// A scatter or a bubble chart's points are its spots, not its rows
// (KI-CHART-SCATTER-CAP-KEEPS-ONE-CORNER): on every engine the SQL groups the rows at one x, y, size
// and colour into a point with their count as its last column, and the cap keeps the busiest
// points first.
// A row cap ordered by value alone keeps the source's least corner: on a source with
// 20,000 rows at (1, 0) it drew one point. The engines' own run of this is
// TestPG_ChartD4c_PointsMergeThenKeepTheBusiest.
func TestWriteChartSQL_PointsMergeThenKeepTheBusiest(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}]}}`,
		`{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"series":[{"column":"pk"}]}}`,
		`{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}]}}`,
		`{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}],"series":[{"column":"pk"}]}}`,
	} {
		s := mustSettings(t, raw)
		outs := s.Outputs()
		if last := outs[len(outs)-1]; last.Alias != charts.PointRowsAlias || last.Role != charts.RolePointRows || last.Source != "" {
			t.Errorf("%s: the last output is %+v, want each point's row count", raw, last)
		}
		for _, src := range goldenChartSources {
			q, err := writeChartSQL(s, src, sqlFilters(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			count := checkCountFn(src.Dialect) + "(*)"
			outer := q.SQL[strings.LastIndex(q.SQL, "\nGROUP BY ")+1:]
			if !strings.HasPrefix(outer, "GROUP BY ") || !strings.Contains(outer, "\nORDER BY "+count+" DESC, ") {
				t.Errorf("%s %s: the outer query does not group the points and keep the busiest first:\n%s", src.Dialect, raw, q.SQL)
			}
			if !strings.Contains(q.SQL, count+" AS "+quoteCheckColumn(src.Dialect, charts.PointRowsAlias)+"\nFROM ") {
				t.Errorf("%s %s: no row count last in the select list:\n%s", src.Dialect, raw, q.SQL)
			}
		}
	}
}
