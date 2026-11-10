//go:build integration_pg

// A series chart's Other run on a real engine (dashboards-design.md § 14.3): twelve
// series, one filtered out, each aggregate's rows must be the top seven series and one
// Other worked out in Go, and every row must carry the number of series left after the
// filter. Text alone cannot show a rank or a fold; the engine can. Same containers as
// chart_grain_pg_test.go:
//
//	CHART_GRAIN_PG_DSN='postgres://postgres:verify@localhost:55473/postgres?sslmode=disable' \
//	CHART_GRAIN_MYSQL_DSN='root:verify@tcp(localhost:55474)/' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ChartSeries -v
package handlers

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/charts"
)

// seriesRow is one source row; a nil amount is NULL.
type seriesRow struct {
	x, s   string
	amount *int
}

// seriesRows is s01-s10, each with i rows a day and amounts that repeat (so a distinct
// count is not a count), s02 missing d2; neg, whose |sum| is the largest; nul, whose
// amounts are all NULL; and s05, which the filter leaves out.
func seriesRows() []seriesRow {
	var rows []seriesRow
	n := func(v int) *int { return &v }
	for _, x := range []string{"d1", "d2", "d3"} {
		for i := 1; i <= 10; i++ {
			if i == 2 && x == "d2" {
				continue
			}
			for k := 1; k <= i; k++ {
				rows = append(rows, seriesRow{x, fmt.Sprintf("s%02d", i), n(i*10 + k%3)})
			}
		}
		rows = append(rows, seriesRow{x, "neg", n(-400)}, seriesRow{x, "neg", n(-400)})
		rows = append(rows, seriesRow{x, "nul", nil}, seriesRow{x, "nul", nil}, seriesRow{x, "nul", nil})
	}
	return rows
}

// seriesSourceSQL is the rows as one UNION ALL; the first row has an amount, so the
// column is an integer on both engines.
func seriesSourceSQL(rows []seriesRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		a := "NULL"
		if r.amount != nil {
			a = fmt.Sprint(*r.amount)
		}
		parts[i] = fmt.Sprintf("SELECT '%s' AS x, '%s' AS s, %s AS amount", r.x, r.s, a)
	}
	return strings.Join(parts, " UNION ALL ")
}

// cellValue is the aggregate of a group's amounts: nil where SQL gives NULL.
func cellValue(aggregate string, amounts []*int) *float64 {
	var vals []float64
	for _, a := range amounts {
		if a != nil {
			vals = append(vals, float64(*a))
		}
	}
	f := func(v float64) *float64 { return &v }
	switch aggregate {
	case charts.AggCount:
		return f(float64(len(amounts)))
	case charts.AggCountDistinct:
		seen := map[float64]bool{}
		for _, v := range vals {
			seen[v] = true
		}
		return f(float64(len(seen)))
	}
	if len(vals) == 0 {
		return nil
	}
	out := vals[0]
	for _, v := range vals[1:] {
		switch aggregate {
		case charts.AggSum, charts.AggAvg:
			out += v
		case charts.AggMin:
			out = math.Min(out, v)
		case charts.AggMax:
			out = math.Max(out, v)
		}
	}
	if aggregate == charts.AggAvg {
		out /= float64(len(vals))
	}
	return &out
}

// wantSeriesRows is the chart's rows worked out in Go, keyed x|series or x|Other: each
// series totalled across its days (the sum of |value|, or the highest value of a min or
// max), ranked largest first with no value last and then by name, the top limit kept and
// the rest folded (summed, or their min or max; NULL for an average or a distinct count).
// It also returns the number of series.
func wantSeriesRows(aggregate string, rows []seriesRow, limit int) (map[string]*float64, int) {
	groups := map[[2]string][]*int{}
	for _, r := range rows {
		k := [2]string{r.x, r.s}
		groups[k] = append(groups[k], r.amount)
	}
	cells := map[[2]string]*float64{}
	totals := map[string]*float64{}
	for k, amounts := range groups {
		v := cellValue(aggregate, amounts)
		cells[k] = v
		if _, ok := totals[k[1]]; !ok {
			totals[k[1]] = nil
		}
		if v == nil {
			continue
		}
		t := totals[k[1]]
		switch {
		case aggregate == charts.AggMin || aggregate == charts.AggMax:
			if t == nil || *v > *t {
				c := *v
				totals[k[1]] = &c
			}
		default:
			c := math.Abs(*v)
			if t != nil {
				c += *t
			}
			totals[k[1]] = &c
		}
	}
	names := make([]string, 0, len(totals))
	for s := range totals {
		names = append(names, s)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := totals[names[i]], totals[names[j]]
		if (a == nil) != (b == nil) {
			return b == nil
		}
		if a != nil && *a != *b {
			return *a > *b
		}
		return names[i] < names[j]
	})
	kept := map[string]bool{}
	for _, s := range names[:limit] {
		kept[s] = true
	}
	want := map[string]*float64{}
	folded := map[string][]*float64{}
	for k, v := range cells {
		if kept[k[1]] {
			want[k[0]+"|"+k[1]] = v
			continue
		}
		folded[k[0]] = append(folded[k[0]], v)
	}
	for x, vs := range folded {
		var out *float64
		switch aggregate {
		case charts.AggCount, charts.AggSum, charts.AggMin, charts.AggMax:
			for _, v := range vs {
				if v == nil {
					continue
				}
				c := *v
				switch {
				case out == nil:
				case aggregate == charts.AggMin:
					c = math.Min(*out, c)
				case aggregate == charts.AggMax:
					c = math.Max(*out, c)
				default:
					c += *out
				}
				out = &c
			}
		}
		want[x+"|Other"] = out
	}
	return want, len(names)
}

func TestPG_ChartSeries_EachEngineKeepsTheTopSevenAndFoldsTheRest(t *testing.T) {
	rows := seriesRows()
	var keep []string // every series but s05
	seen := map[string]bool{}
	for _, r := range rows {
		if r.s != "s05" && !seen[r.s] {
			seen[r.s] = true
			keep = append(keep, r.s)
		}
	}
	filters := []charts.Bound{{Column: "s", Kind: charts.FilterValues, Values: keep}}
	var filtered []seriesRow
	for _, r := range rows {
		if r.s != "s05" {
			filtered = append(filtered, r)
		}
	}
	for _, eng := range grainEngines {
		t.Run(eng.dialect, func(t *testing.T) {
			dsn := os.Getenv(eng.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s", eng.env, eng.dialect)
			}
			db, err := sql.Open(eng.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			src := chartSource{Dialect: eng.dialect, SQL: seriesSourceSQL(rows)}
			for _, agg := range charts.Aggregates {
				col := `"column":"amount",`
				if agg == charts.AggCount {
					col = ""
				}
				s := mustSettings(t, `{"version":2,"type":"bar","roles":{"x":[{"column":"x"}],"series":[{"column":"s"}],`+
					`"measure":[{`+col+`"aggregate":"`+agg+`"}]}}`)
				q, err := writeChartSQL(s, src, filters, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				want, nSeries := wantSeriesRows(agg, filtered, 7)
				got, others := map[string]*float64{}, 0
				res, err := db.Query(q.SQL, q.Args...)
				if err != nil {
					t.Fatalf("%s: %v\n%s", agg, err, q.SQL)
				}
				for res.Next() {
					var x string
					var series sql.NullString
					var m sql.NullFloat64
					var other, count int
					if err := res.Scan(&x, &series, &m, &other, &count); err != nil {
						t.Fatal(err)
					}
					if count != nSeries {
						t.Errorf("%s: %s carries %d series, want %d", agg, x, count, nSeries)
					}
					k := x + "|" + series.String
					if other == 1 {
						k, others = x+"|Other", others+1
					}
					if m.Valid {
						got[k] = &m.Float64
					} else {
						got[k] = nil
					}
				}
				if err := res.Err(); err != nil {
					t.Fatal(err)
				}
				res.Close()
				// Control: Other is drawn on every day, so a query that folded nothing
				// cannot pass.
				if others != 3 || len(got) != len(want) {
					t.Fatalf("%s: %d rows (%d Other), want %d (3 Other)\ngot  %v\nwant %v", agg, len(got), others, len(want), showSeriesCells(got), showSeriesCells(want))
				}
				for k, w := range want {
					g, ok := got[k]
					if !ok || (g == nil) != (w == nil) || (g != nil && math.Abs(*g-*w) > 1e-3) {
						t.Errorf("%s %s: got %v, want %v", agg, k, showSeriesCells(map[string]*float64{k: g}), showSeriesCells(map[string]*float64{k: w}))
					}
				}
			}
			t.Logf("%s: %d aggregates × %d series match", eng.dialect, len(charts.Aggregates), len(keep))
		})
	}
}

func showSeriesCells(m map[string]*float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if m[k] == nil {
			fmt.Fprintf(&b, "%s=NULL ", k)
		} else {
			fmt.Fprintf(&b, "%s=%g ", k, *m[k])
		}
	}
	return b.String()
}
