//go:build integration_pg

// D4b's SQL run on a real engine (dashboards-design.md § 14.9): a histogram's bins, a
// pie's slices and Other, and a scorecard's two periods and its sparkline, each worked
// out in Go over the same rows. Text alone cannot show a row landing in the right bin or
// a ? taking the right value; the engine can. Same containers as
// chart_grain_pg_test.go:
//
//	CHART_GRAIN_PG_DSN='postgres://postgres:verify@localhost:55473/postgres?sslmode=disable' \
//	CHART_GRAIN_MYSQL_DSN='root:verify@tcp(localhost:55474)/' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ChartD4b -v
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

// d4bEngines opens each engine of chart_grain_pg_test.go, skipping one with no DSN.
func d4bEngines(t *testing.T, run func(t *testing.T, dialect string, db *sql.DB)) {
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
			run(t, eng.dialect, db)
		})
	}
}

// d4bFilter keeps US and EU; every source here also has APAC rows, which a chart that
// lost the filter would draw.
var d4bFilter = charts.Bound{Column: "region", Kind: charts.FilterValues, Values: []string{"US", "EU"}}

// histRow is one histogram source row; a nil amount is NULL.
type histRow struct {
	region string
	amount *int
}

func histSourceSQL(rows []histRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		a := "NULL"
		if r.amount != nil {
			a = fmt.Sprint(*r.amount)
		}
		parts[i] = fmt.Sprintf("SELECT '%s' AS region, %s AS amount", r.region, a)
	}
	return strings.Join(parts, " UNION ALL ")
}

// histRows is -7 to 23 in US and EU, each value (v mod 4) + 1 times, with NULLs; and
// APAC's -100 and 500, which would widen every bin if the filter missed the MIN and MAX.
// The first row has an amount, so the column is an integer on both engines.
func histRows() []histRow {
	n := func(v int) *int { return &v }
	var rows []histRow
	for v := -7; v <= 23; v++ {
		for k := 0; k <= ((v%4)+4)%4; k++ {
			region := "US"
			if k%2 == 1 {
				region = "EU"
			}
			rows = append(rows, histRow{region, n(v)})
		}
	}
	return append(rows, histRow{"US", nil}, histRow{"EU", nil}, histRow{"APAC", n(-100)}, histRow{"APAC", n(500)})
}

// histBin is one bin as the chart reads it: its least value, its width and its rows.
type histBin struct {
	lo, width float64
	count     int
}

// wantHistBins is the bins worked out in Go, least first, leaving out an empty one. With
// a width, each value's bin is it rounded down to a width from zero; with a count, bins
// equal parts of the least to the greatest value, the greatest in the last, and one
// value throughout is one bin of width 0.
func wantHistBins(vals []float64, bins int, width float64) []histBin {
	counts := map[float64]int{}
	w := width
	if w == 0 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, v := range vals {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		w = (hi - lo) / float64(bins)
		for _, v := range vals {
			b := 0.0
			if w > 0 {
				b = math.Min(math.Floor((v-lo)/w), float64(bins-1))
			}
			counts[lo+b*w]++
		}
	} else {
		for _, v := range vals {
			counts[math.Floor(v/w)*w]++
		}
	}
	out := make([]histBin, 0, len(counts))
	for lo, c := range counts {
		out = append(out, histBin{lo, w, c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lo < out[j].lo })
	return out
}

func TestPG_ChartD4b_HistogramBinsEachRowOnce(t *testing.T) {
	rows := histRows()
	var vals []float64
	for _, r := range rows {
		if r.region != "APAC" && r.amount != nil {
			vals = append(vals, float64(*r.amount))
		}
	}
	same := []histRow{}
	five := 5
	for range 6 {
		same = append(same, histRow{"US", &five})
	}
	same = append(same, histRows()[len(rows)-2:]...) // APAC's -100 and 500
	cases := []struct {
		name, x string
		rows    []histRow
		want    []histBin
	}{
		{"7 bins", `"bins":7`, rows, wantHistBins(vals, 7, 0)},
		{"20 bins, the default", ``, rows, wantHistBins(vals, charts.DefaultBins, 0)},
		{"width 2.5", `"width":2.5`, rows, wantHistBins(vals, 0, 2.5)},
		{"one value", `"bins":10`, same, []histBin{{5, 0, 6}}},
	}
	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, c := range cases {
			x := `{"column":"amount"}`
			if c.x != "" {
				x = `{"column":"amount",` + c.x + `}`
			}
			s := mustSettings(t, `{"version":2,"type":"histogram","roles":{"x":[`+x+`]}}`)
			q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: histSourceSQL(c.rows)}, []charts.Bound{d4bFilter}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
			}
			var got []histBin
			total := 0
			for res.Next() {
				var b histBin
				if err := res.Scan(&b.lo, &b.width, &b.count); err != nil {
					t.Fatal(err)
				}
				got, total = append(got, b), total+b.count
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			want := 0
			for _, b := range c.want {
				want += b.count
			}
			// Control: every kept row is in one bin, and there is more than one bin
			// where the values differ, so a query that binned nothing cannot pass.
			if len(got) != len(c.want) || total != want || want == 0 {
				t.Fatalf("%s: %d bins of %d rows, want %d of %d\ngot  %v\nwant %v", c.name, len(got), total, len(c.want), want, got, c.want)
			}
			// MySQL keeps 4 more decimal places in a division than its operands, so a
			// width of 30/7 reads 4.28571 there; the bins are the same.
			for i, w := range c.want {
				g := got[i]
				if g.count != w.count || math.Abs(g.lo-w.lo) > 1e-4 || math.Abs(g.width-w.width) > 1e-4 {
					t.Errorf("%s bin %d: got %+v, want %+v", c.name, i, g, w)
				}
			}
			t.Logf("%s %s: %d bins match", dialect, c.name, len(got))
		}
	})
}

func TestPG_ChartD4b_PieKeepsTheTopSevenSlicesThenOther(t *testing.T) {
	rows := seriesRows()
	var keep []string
	seen := map[string]bool{}
	var filtered []seriesRow
	for _, r := range rows {
		if r.s == "s05" {
			continue
		}
		if !seen[r.s] {
			seen[r.s] = true
			keep = append(keep, r.s)
		}
		filtered = append(filtered, seriesRow{"all", r.s, r.amount})
	}
	filters := []charts.Bound{{Column: "s", Kind: charts.FilterValues, Values: keep}}
	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, agg := range []string{charts.AggCount, charts.AggSum} {
			col := `"column":"amount",`
			if agg == charts.AggCount {
				col = ""
			}
			s := mustSettings(t, `{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{`+col+`"aggregate":"`+agg+`"}]}}`)
			q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: seriesSourceSQL(rows)}, filters, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			want, nSeries := wantSeriesRows(agg, filtered, 7)
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", agg, err, q.SQL)
			}
			got := map[string]*float64{}
			var order []string
			var values []float64
			for res.Next() {
				var series sql.NullString
				var m sql.NullFloat64
				var other, count int
				if err := res.Scan(&series, &m, &other, &count); err != nil {
					t.Fatal(err)
				}
				if count != nSeries {
					t.Errorf("%s: %s carries %d series, want %d", agg, series.String, count, nSeries)
				}
				k := "all|" + series.String
				if other == 1 {
					k = "all|Other"
				}
				order = append(order, k)
				if m.Valid {
					got[k], values = &m.Float64, append(values, m.Float64)
				} else {
					got[k] = nil
				}
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			if len(got) != len(want) || len(order) != 8 || order[7] != "all|Other" {
				t.Fatalf("%s: slices %v, want 7 and Other last\ngot  %v\nwant %v", agg, order, showSeriesCells(got), showSeriesCells(want))
			}
			for k, w := range want {
				g, ok := got[k]
				if !ok || (g == nil) != (w == nil) || (g != nil && math.Abs(*g-*w) > 1e-3) {
					t.Errorf("%s %s: got %v, want %v", agg, k, showSeriesCells(map[string]*float64{k: g}), showSeriesCells(map[string]*float64{k: w}))
				}
			}
			// Largest first: the seven kept slices are drawn in the order the source
			// returns them.
			for i := 1; i < 7; i++ {
				if values[i] > values[i-1] {
					t.Errorf("%s: slice %d (%s, %g) is larger than the one before it (%g)", agg, i, order[i], values[i], values[i-1])
				}
			}
			t.Logf("%s %s: 7 slices and Other of %d series match, largest first", dialect, agg, nSeries)
		}
	})
}

// scoreRow is one scorecard source row: a day, a region and an amount (nil is NULL).
type scoreRow struct {
	day    time.Time
	region string
	amount *int
}

func scoreSourceSQL(rows []scoreRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		a := "NULL"
		if r.amount != nil {
			a = fmt.Sprint(*r.amount)
		}
		parts[i] = fmt.Sprintf("SELECT DATE '%s' AS day, '%s' AS region, %s AS amount", r.day.Format("2006-01-02"), r.region, a)
	}
	return strings.Join(parts, " UNION ALL ")
}

// dayText reads a date the engine returns, as a time, or as text with or without a time.
func dayText(v any) string {
	switch d := v.(type) {
	case time.Time:
		return d.Format("2006-01-02")
	case []byte:
		return string(d)[:10]
	case string:
		return d[:10]
	}
	return fmt.Sprintf("%T %v", v, v)
}

func TestPG_ChartD4b_ScorecardBindsEachPeriodAndItsSparkline(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	first := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	var rows []scoreRow
	for d := first; !d.After(now); d = d.AddDate(0, 0, 1) {
		us, eu, apac := d.YearDay()%7+1, d.YearDay()%5+2, 1000
		rows = append(rows, scoreRow{d, "US", &us}, scoreRow{d, "EU", &eu}, scoreRow{d, "APAC", &apac})
		if d.Day() == 15 {
			rows = append(rows, scoreRow{d, "US", nil})
		}
	}
	// The viewer's date range and regions, bound in the WHERE below the periods: the
	// order a positional ? takes them in.
	from, to := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	filters := []charts.Bound{{Column: "day", Kind: charts.FilterDateRange, From: from, To: to}, d4bFilter}
	// The last 30 days up to today (2026-09-06 to 2026-10-05) and the 30 before them,
	// each narrowed by the date range.
	end := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	mid, start := end.AddDate(0, 0, -30), end.AddDate(0, 0, -60)
	var cur, prev float64
	perDay := map[string]float64{}
	for _, r := range rows {
		if r.region == "APAC" || r.amount == nil || r.day.Before(from) || !r.day.Before(to) {
			continue
		}
		v := float64(*r.amount)
		switch {
		case !r.day.Before(mid) && r.day.Before(end):
			cur += v
			perDay[r.day.Format("2006-01-02")] += v
		case !r.day.Before(start) && r.day.Before(mid):
			prev += v
		}
	}
	days := make([]string, 0, len(perDay))
	for d := range perDay {
		days = append(days, d)
	}
	sort.Strings(days)

	s := mustSettings(t, `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"sum","column":"amount"}],"date":[{"column":"day"}]},"compare":{"days":30},"sparkline":true}`)
	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: scoreSourceSQL(rows)}, filters, now)
		if err != nil {
			t.Fatal(err)
		}
		var gotCur, gotPrev sql.NullFloat64
		if err := db.QueryRow(q.SQL, q.Args...).Scan(&gotCur, &gotPrev); err != nil {
			t.Fatalf("%v\n%s", err, q.SQL)
		}
		// Control: both periods hold rows and differ, so a swap of their values shows.
		if cur == 0 || prev == 0 || cur == prev {
			t.Fatalf("the test's periods are %g and %g; pick rows that tell them apart", cur, prev)
		}
		if gotCur.Float64 != cur || gotPrev.Float64 != prev {
			t.Fatalf("current %v and previous %v, want %g and %g\n%s\nargs %v", gotCur, gotPrev, cur, prev, q.SQL, q.Args)
		}
		if q.Spark == nil {
			t.Fatal("no sparkline statement")
		}
		res, err := db.Query(q.Spark.SQL, q.Spark.Args...)
		if err != nil {
			t.Fatalf("sparkline: %v\n%s", err, q.Spark.SQL)
		}
		var gotDays []string
		for res.Next() {
			var day any
			var v sql.NullFloat64
			if err := res.Scan(&day, &v); err != nil {
				t.Fatal(err)
			}
			d := dayText(day)
			gotDays = append(gotDays, d)
			if v.Float64 != perDay[d] {
				t.Errorf("sparkline %s: got %v, want %g", d, v, perDay[d])
			}
		}
		if err := res.Err(); err != nil {
			t.Fatal(err)
		}
		res.Close()
		if strings.Join(gotDays, " ") != strings.Join(days, " ") {
			t.Fatalf("sparkline days\ngot  %v\nwant %v", gotDays, days)
		}
		t.Logf("%s: current %g, previous %g, %d sparkline points match", dialect, cur, prev, len(days))
	})
}
