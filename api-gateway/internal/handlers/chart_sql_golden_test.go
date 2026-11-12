package handlers

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The SQL each chart type writes, on each engine, pinned as golden files
// (dashboards-design.md § 14.9, D4a and D4b): the settings, the filters and the clock are
// fixed, so a change to how a chart reads its source shows up here as a diff to review.
// The first cases are the saved shape (v1) a chart was stored in before D4a, so the files
// also prove a chart saved then is read the same after; the grained cases are settings
// v2, on a connection whose week starts on Sunday or whose fiscal year starts in April;
// D4b's types are settings v2. A statement a chart runs after its main one (a
// scorecard's sparkline) follows it under "-- then". An engine a type is refused on (a
// box plot where there is no exact percentile, D4d) has its refusal pinned instead,
// under "-- refused", and only there.
//
//	go test ./internal/handlers -run TestWriteChartSQL_Golden -update-chart-sql

var updateChartSQL = flag.Bool("update-chart-sql", false, "rewrite testdata/chart_sql/*.golden from the code")

// Each engine with its table named as the gateway names it.
var goldenChartSources = []chartSource{
	{Dialect: "postgres", Table: `"gold"."orders"`},
	{Dialect: "mysql", Table: "`gold`.`orders`"},
	{Dialect: "sqlserver", Table: "[gold].[orders]"},
	{Dialect: "bigquery", Table: "`gold`.`orders`"},
	{Dialect: "snowflake", Table: `"GOLD"."ORDERS"`},
	{Dialect: "databricks", Table: "`gold`.`orders`"},
	{Dialect: "clickhouse", Table: "`gold`.`orders`"},
	{Dialect: "oracle", Table: `"GOLD"."ORDERS"`},
}

type goldenChartCase struct {
	name, settings         string
	query                  bool     // read from a saved query rather than a table
	weekStart, fiscalMonth int      // the connection's, when not the default
	refusedOn              []string // the engines the type is refused on
}

var goldenChartCases = []goldenChartCase{
	{name: "bar_sum", settings: sqlBarSum},
	{name: "bar_series_stacked", settings: `{"type":"bar","x":{"column":"day","title":"Day"},"measure":{"aggregate":"count"},"series":{"column":"region","colors":[{"value":"EU","color":"series-2"}]},"stacked":true}`},
	{name: "hbar_avg_sorted", settings: `{"type":"hbar","x":{"column":"region"},"measure":{"aggregate":"avg","column":"amount","format":"decimal"},"sort":"value_asc"}`},
	{name: "line_count_distinct", settings: `{"type":"line","x":{"column":"day"},"measure":{"aggregate":"count_distinct","column":"customer_id","title":"Customers"}}`},
	{name: "area_series", settings: `{"type":"area","x":{"column":"day"},"measure":{"aggregate":"max","column":"amount"},"series":{"column":"region"},"sort":"x_desc"}`},
	{name: "heatmap", settings: `{"type":"heatmap","x":{"column":"day"},"y":{"column":"region"},"measure":{"aggregate":"min","column":"amount","color":"series-4"}}`},
	{name: "scorecard_plain", settings: `{"type":"scorecard","measure":{"aggregate":"sum","column":"amount","format":"compact"}}`},
	{name: "scorecard_compare", settings: `{"type":"scorecard","measure":{"aggregate":"count"},"compare":{"column":"day","days":7}}`},
	{name: "table_rows", settings: `{"type":"table","columns":["day","region","amount"],"sort":"x_desc"}`},
	{name: "table_grouped", settings: `{"type":"table","columns":["region","day"],"measure":{"aggregate":"sum","column":"amount"},"sort":"value_desc"}`},
	{name: "line_query_source", settings: `{"type":"line","x":{"column":"day"},"measure":{"aggregate":"sum","column":"amount"}}`, query: true},
	{name: "line_week_sunday", settings: `{"version":2,"type":"line","roles":{"x":[{"column":"day","grain":"week"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`, weekStart: 7},
	{name: "heatmap_fiscal_quarter", settings: `{"version":2,"type":"heatmap","roles":{"x":[{"column":"day","grain":"quarter"}],"y":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`, fiscalMonth: 4},
	{name: "combo_two_axes", settings: `{"version":2,"type":"bar","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"sum","column":"amount","mark":"bar","axis":"left"},{"aggregate":"count","mark":"line","axis":"right"}]}}`},
	{name: "area_percent_stacked", settings: `{"version":2,"type":"area","roles":{"x":[{"column":"day"}],"series":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]},"stacked":true,"percent":true}`},
	{name: "pie", settings: `{"version":2,"type":"pie","roles":{"series":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`},
	{name: "donut_count", settings: `{"version":2,"type":"donut","roles":{"series":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`},
	{name: "waterfall", settings: `{"version":2,"type":"waterfall","roles":{"x":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`},
	{name: "funnel", settings: `{"version":2,"type":"funnel","roles":{"x":[{"column":"region"}],"measure":[{"aggregate":"count_distinct","column":"customer_id"}]}}`},
	{name: "histogram_bins", settings: `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount","bins":10}]}}`},
	{name: "histogram_width", settings: `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount","width":25}]}}`},
	{name: "scorecard_sparkline", settings: `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"sum","column":"amount"}],"date":[{"column":"day"}]},"compare":{"days":30},"sparkline":true}`},
	{name: "scatter", settings: `{"version":2,"type":"scatter","roles":{"x":[{"column":"amount"}],"y":[{"column":"quantity"}]}}`},
	{name: "bubble", settings: `{"version":2,"type":"bubble","roles":{"x":[{"column":"amount"}],"y":[{"column":"quantity"}],"size":[{"column":"discount"}]}}`},
	{name: "bubble_colour", settings: `{"version":2,"type":"bubble","roles":{"x":[{"column":"amount"}],"y":[{"column":"quantity"}],"size":[{"column":"discount"}],"series":[{"column":"region"}]}}`},
	{name: "pivot_rows", settings: `{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`},
	{name: "pivot_rows_columns", settings: `{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"},{"column":"day","grain":"month"}],"columns":[{"column":"channel"}],"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"avg","column":"amount"},{"aggregate":"count"}]}}`},
	// D4d: the second tier.
	{name: "treemap", settings: `{"version":2,"type":"treemap","roles":{"rows":[{"column":"region"},{"column":"channel"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`},
	{name: "sunburst_count", settings: `{"version":2,"type":"sunburst","roles":{"rows":[{"column":"channel"},{"column":"region"},{"column":"customer_id"}],"measure":[{"aggregate":"count"}]}}`},
	{name: "sankey", settings: `{"version":2,"type":"sankey","roles":{"x":[{"column":"channel"}],"y":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`},
	{name: "calendar", settings: `{"version":2,"type":"calendar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"avg","column":"amount"}]}}`},
	{name: "bullet_target_measure", settings: `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"sum","column":"quota"}]}}`},
	{name: "bullet_target_number", settings: `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count_distinct","column":"customer_id"}]},"target":250}`},
	{name: "boxplot", settings: `{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"amount"}]}}`, refusedOn: []string{"mysql", "sqlserver"}},
	{name: "boxplot_by_month", settings: `{"version":2,"type":"boxplot","roles":{"x":[{"column":"day","grain":"month"}],"y":[{"column":"amount"}]},"sort":"x_desc"}`, refusedOn: []string{"mysql", "sqlserver"}},
	{name: "candlestick", settings: `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day","grain":"week"}],"y":[{"column":"open"},{"column":"high"},{"column":"low"},{"column":"close"}]}}`},
	{name: "candlestick_ticks", settings: `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day","grain":"day"}],"y":[{"column":"amount"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`},
	{name: "range", settings: `{"version":2,"type":"range","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"min","column":"amount"},{"aggregate":"max","column":"amount"}]}}`},
	{name: "map_world", settings: `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`},
}

// refused reports whether the case's type is refused on the engine.
func (c goldenChartCase) refused(dialect string) bool {
	for _, d := range c.refusedOn {
		if d == dialect {
			return true
		}
	}
	return false
}

func TestWriteChartSQL_Golden(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range goldenChartCases {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			fmt.Fprintf(&b, "# settings: %s\n", c.settings)
			for _, src := range goldenChartSources {
				src.WeekStart, src.FiscalMonth = c.weekStart, c.fiscalMonth
				if c.query {
					src.Table = ""
					src.SQL = "SELECT day, amount FROM orders;"
				}
				q, err := writeChartSQL(mustSettings(t, c.settings), src, sqlFilters(), now)
				fmt.Fprintf(&b, "\n-- %s\n", src.Dialect)
				if c.refused(src.Dialect) {
					if err == nil {
						t.Fatalf("%s: written, want it refused:\n%s", src.Dialect, q.SQL)
					}
					fmt.Fprintf(&b, "-- refused: %v\n", err)
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", src.Dialect, err)
				}
				for i, st := range append([]*chartQuery{q}, q.statements()...) {
					if i > 0 {
						b.WriteString("-- then\n")
					}
					fmt.Fprintf(&b, "%s\n", st.SQL)
					if len(st.Args) > 0 {
						fmt.Fprintf(&b, "-- args: %q\n", st.Args)
					}
					for _, p := range st.Params {
						fmt.Fprintf(&b, "-- param %s %s: %q\n", p.Name, p.Type, p.Value)
					}
					if len(st.Unfiltered) > 0 {
						fmt.Fprintf(&b, "-- unfiltered: %s\n", strings.Join(st.Unfiltered, ", "))
					}
					if st.Approximate {
						b.WriteString("-- approximate\n")
					}
					if st.Unordered != "" {
						fmt.Fprintf(&b, "-- when a column cannot be ordered, with the same values\n%s\n", st.Unordered)
					}
				}
			}
			path := filepath.Join("testdata", "chart_sql", c.name+".golden")
			if *updateChartSQL {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (write it with -update-chart-sql)", err)
			}
			if got := b.String(); got != string(want) {
				t.Fatalf("%s differs from the code; the code writes:\n%s", path, got)
			}
		})
	}
}

// A bound value's placeholder is written where the value is bound, so the text order of
// the placeholders is the order of the values. MySQL's ? is positional: a value bound
// before a placeholder earlier in the text lands in the wrong place, with no error (a
// scorecard bound its filters first and its periods, which sit in the SELECT above the
// WHERE, after them). Postgres numbers its placeholders in bind order, so on every golden
// case they must read $1, $2, … down the text.
func TestWriteChartSQL_PlaceholdersFollowTheText(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ph := regexp.MustCompile(`\$(\d+)`)
	for _, c := range goldenChartCases {
		t.Run(c.name, func(t *testing.T) {
			src := goldenChartSources[0]
			src.WeekStart, src.FiscalMonth = c.weekStart, c.fiscalMonth
			q, err := writeChartSQL(mustSettings(t, c.settings), src, sqlFilters(), now)
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range append([]*chartQuery{q}, q.statements()...) {
				m := ph.FindAllStringSubmatch(st.SQL, -1)
				if len(m) != len(st.Args) {
					t.Fatalf("%d placeholders for %d values:\n%s", len(m), len(st.Args), st.SQL)
				}
				for i, n := range m {
					if n[1] != strconv.Itoa(i+1) {
						t.Fatalf("placeholder %d in the text is $%s:\n%s", i+1, n[1], st.SQL)
					}
				}
			}
		})
	}
}

// On MySQL the values go to the ? marks in the order they come: the scorecard's current
// period reads the last seven days and the earlier one the seven before, with the
// viewer's filters in the WHERE.
func TestWriteChartSQL_MySQLScorecardBindsEachPeriodInPlace(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := mustSettings(t, `{"type":"scorecard","measure":{"aggregate":"count"},"compare":{"column":"day","days":7}}`)
	q, err := writeChartSQL(s, goldenChartSources[1], sqlFilters(), now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(q.SQL, "?")
	if len(parts)-1 != len(q.Args) {
		t.Fatalf("%d ? marks for %d values", len(parts)-1, len(q.Args))
	}
	var b strings.Builder
	for i, p := range parts {
		b.WriteString(p)
		if i < len(q.Args) {
			fmt.Fprintf(&b, "'%v'", q.Args[i])
		}
	}
	want := "SELECT COUNT(CASE WHEN src.`day` >= '2026-09-29' AND src.`day` < '2026-10-06' THEN 1 END) AS `count`, " +
		"COUNT(CASE WHEN src.`day` >= '2026-09-22' AND src.`day` < '2026-09-29' THEN 1 END) AS `previous`\n" +
		"FROM `gold`.`orders` src\n" +
		"WHERE src.`day` >= '2026-10-01' AND src.`day` < '2026-10-06' AND src.`region` IN ('US', 'EU'); DROP TABLE orders; --')\n" +
		"LIMIT 10001"
	if got := b.String(); got != want {
		t.Fatalf("with its values in place the SQL reads\n%s\nwant\n%s", got, want)
	}
}
