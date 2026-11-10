//go:build integration_pg

// D4d's SQL run on a real engine (dashboards-design.md § 14.9): a box plot's quartiles,
// each worked out in Go over the same rows the way PERCENTILE_CONT interpolates, and its
// refusal on MySQL, which has no percentile to give; and a candlestick's open and close,
// each its period's first and last row, a tie broken by the prices. Same containers as
// chart_grain_pg_test.go:
//
//	CHART_GRAIN_PG_DSN='postgres://postgres:verify@localhost:55473/postgres?sslmode=disable' \
//	CHART_GRAIN_MYSQL_DSN='root:verify@tcp(localhost:55474)/' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ChartD4d -v
package handlers

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"api-gateway/internal/charts"
)

// boxRow is one box plot source row; a nil amount is NULL.
type boxRow struct {
	region string
	day    time.Time
	amount *int
}

// boxRows is 60 rows over July to September in US, EU and APAC, NULL among the amounts.
// US's and EU's amounts are 1 to 47; APAC's are below -400, so a box plot that lost the
// filter would draw a lower least value in every box. The first row has every value, so
// each column's type is set by it on both engines.
func boxRows() []boxRow {
	regions := []string{"US", "EU", "APAC"}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]boxRow, 60)
	for i := range rows {
		var a *int
		if i%11 != 4 {
			v := i*29%47 + 1
			if i%3 == 2 {
				v -= 500
			}
			a = &v
		}
		rows[i] = boxRow{regions[i%3], start.AddDate(0, 0, i*3%92), a}
	}
	return rows
}

func boxSourceSQL(rows []boxRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		a := "NULL"
		if r.amount != nil {
			a = fmt.Sprint(*r.amount)
		}
		parts[i] = fmt.Sprintf("SELECT '%s' AS region, DATE '%s' AS day, %s AS amount", r.region, r.day.Format("2006-01-02"), a)
	}
	return strings.Join(parts, " UNION ALL ")
}

// box is one box: the least value, the quartiles and the greatest.
type box [5]float64

// cont is PERCENTILE_CONT's p over sorted values: the value at p of the way along,
// between the two values either side of it.
func cont(v []float64, p float64) float64 {
	h := p * float64(len(v)-1)
	lo := math.Floor(h)
	if int(lo)+1 >= len(v) {
		return v[int(lo)]
	}
	return v[int(lo)] + (h-lo)*(v[int(lo)+1]-v[int(lo)])
}

// disc is PERCENTILE_DISC's p, the first value at or past p of the rows: what an engine
// that picked a row instead of interpolating would return.
func disc(v []float64, p float64) float64 {
	return v[int(math.Ceil(p*float64(len(v))))-1]
}

func TestPG_ChartD4d_BoxPlotQuartilesAreTheSourcesOwn(t *testing.T) {
	all := boxRows()
	cases := []struct {
		name, settings string
		key            func(boxRow) string
		desc           bool
	}{
		{"by region", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"amount"}]}}`,
			func(r boxRow) string { return r.region }, false},
		{"by month, latest first", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"day","grain":"month"}],"y":[{"column":"amount"}]},"sort":"x_desc"}`,
			func(r boxRow) string { return r.day.Format("2006-01") + "-01" }, true},
	}

	// want works out each box in Go over the rows the filter keeps, NULL amounts left out.
	want := func(key func(boxRow) string) (map[string]box, []string, bool) {
		values := map[string][]float64{}
		nulls := false
		for _, r := range all {
			if r.region == "APAC" {
				continue
			}
			if r.amount == nil {
				nulls = true
				continue
			}
			values[key(r)] = append(values[key(r)], float64(*r.amount))
		}
		out := map[string]box{}
		var keys []string
		// Control: an engine that picked a row for a quartile, as PERCENTILE_DISC does,
		// would differ from PERCENTILE_CONT in some box, so the test can tell them apart.
		differs := false
		for k, v := range values {
			sort.Float64s(v)
			b := box{v[0], cont(v, 0.25), cont(v, 0.5), cont(v, 0.75), v[len(v)-1]}
			for p := 1; p <= 3; p++ {
				if b[p] != disc(v, float64(p)*0.25) {
					differs = true
				}
			}
			out[k] = b
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return out, keys, differs && nulls
	}
	for _, c := range cases {
		boxes, keys, controls := want(c.key)
		if !controls || len(keys) < 2 {
			t.Fatalf("%s: %d boxes, an interpolated quartile and a NULL amount %v: pick rows that tell them apart", c.name, len(keys), controls)
		}
		for _, k := range keys {
			// A NULL read as 0 would be every box's least value; the rows have none below 1.
			if boxes[k][0] < 1 {
				t.Fatalf("%s %s: least value %g, want every kept amount above 0", c.name, k, boxes[k][0])
			}
		}
	}

	// probe is the percentile the box plot would need: Postgres has it, MySQL does not, so
	// the refusal is the engine's, not the chart's.
	probe := "SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY s.amount) FROM (" + boxSourceSQL(all) + ") s"

	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		var median sql.NullFloat64
		probeErr := db.QueryRow(probe).Scan(&median)
		if dialect == "mysql" {
			if probeErr == nil {
				t.Fatalf("MySQL ran PERCENTILE_CONT (%v): a box plot need not be refused here", median)
			}
			for _, c := range cases {
				_, err := writeChartSQL(mustSettings(t, c.settings), chartSource{Dialect: dialect, SQL: boxSourceSQL(all)}, []charts.Bound{d4bFilter}, time.Now())
				if err == nil || !strings.Contains(err.Error(), "exact percentile") {
					t.Fatalf("%s: writeChartSQL = %v, want the box plot refused for its percentile", c.name, err)
				}
			}
			t.Logf("mysql: the box plot is refused, and the engine refuses PERCENTILE_CONT itself: %v", probeErr)
			return
		}
		if probeErr != nil {
			t.Fatalf("the percentile probe failed on %s, so it cannot tell an engine without one: %v", dialect, probeErr)
		}
		for _, c := range cases {
			boxes, keys, _ := want(c.key)
			if c.desc {
				sort.Sort(sort.Reverse(sort.StringSlice(keys)))
			}
			q, err := writeChartSQL(mustSettings(t, c.settings), chartSource{Dialect: dialect, SQL: boxSourceSQL(all)}, []charts.Bound{d4bFilter}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if q.Approximate {
				t.Errorf("%s: %s's quartiles are exact, but the query says approximate", c.name, dialect)
			}
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
			}
			var got []string
			for res.Next() {
				var x any
				var b [5]sql.NullFloat64
				if err := res.Scan(&x, &b[0], &b[1], &b[2], &b[3], &b[4]); err != nil {
					t.Fatal(err)
				}
				k := string(textOf(x))
				if c.desc {
					k = dayText(x)
				}
				got = append(got, k)
				w, ok := boxes[k]
				if !ok {
					t.Errorf("%s: a box for %s, which the filter leaves out", c.name, k)
					continue
				}
				for i := range b {
					if !sameNumber(floatOf(b[i]), &w[i]) {
						t.Errorf("%s %s %s: got %s, want %g", c.name, k, charts.BoxAliases[i], nullNumber(b[i]), w[i])
					}
				}
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			if strings.Join(got, ",") != strings.Join(keys, ",") {
				t.Errorf("%s: boxes %v, want %v in that order", c.name, got, keys)
			}
			t.Logf("%s %s: %d boxes match in order, e.g. %s %v", dialect, c.name, len(got), keys[0], boxes[keys[0]])
		}
	})
}

// candleRow is one candlestick source row; a nil price is NULL.
type candleRow struct {
	region string
	at     string
	prices [4]*int // open, high, low, close
}

func candleRows() []candleRow {
	n := func(v int) *int { return &v }
	p := func(o, h, l, c *int) [4]*int { return [4]*int{o, h, l, c} }
	return []candleRow{
		// The week of Monday 6 July. 09:00's two rows tie on the time: the lower open is the
		// first. 08:00's has no open, so it is no row of the full candle, though its high and
		// low are the widest. Sunday 12 July is the week's last day (ISO): 23:00's two rows
		// tie, and the higher close is the last.
		{"US", "2026-07-06 09:00:00", p(n(10), n(12), n(9), n(11))},
		{"EU", "2026-07-06 09:00:00", p(n(9), n(13), n(8), n(12))},
		{"US", "2026-07-06 08:00:00", p(nil, n(50), n(1), n(5))},
		{"EU", "2026-07-08 12:00:00", p(n(6), n(15), n(6), n(14))},
		{"US", "2026-07-10 12:00:00", p(n(15), n(21), n(14), n(20))},
		{"US", "2026-07-12 23:00:00", p(n(14), n(16), n(13), n(13))},
		{"EU", "2026-07-12 23:00:00", p(n(15), n(17), n(12), n(16))},
		{"APAC", "2026-07-09 10:00:00", p(n(1), n(999), n(-999), n(1))},
		// The week of 13 July, from Monday's midnight.
		{"US", "2026-07-13 00:00:00", p(n(20), n(22), n(19), n(21))},
		{"EU", "2026-07-15 12:00:00", p(n(18), n(25), n(17), n(24))},
		{"US", "2026-07-19 22:00:00", p(n(23), n(24), n(20), n(19))},
		// The week of 20 July: one row, and APAC's earlier one the filter leaves out.
		{"EU", "2026-07-21 10:00:00", p(n(30), n(32), n(29), n(31))},
		{"APAC", "2026-07-20 00:00:00", p(n(-5), n(100), n(-100), n(50))},
	}
}

func candleSourceSQL(rows []candleRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		v := make([]string, 4)
		for j, p := range r.prices {
			v[j] = "NULL"
			if p != nil {
				v[j] = fmt.Sprint(*p)
			}
		}
		parts[i] = fmt.Sprintf("SELECT '%s' AS region, TIMESTAMP '%s' AS at, %s AS opening, %s AS highest, %s AS lowest, %s AS closing",
			r.region, r.at, v[0], v[1], v[2], v[3])
	}
	return strings.Join(parts, " UNION ALL ")
}

func TestPG_ChartD4d_CandlestickOpensAndClosesOnItsFirstAndLastRow(t *testing.T) {
	rows := candleRows()
	cases := []struct {
		name, settings string
		want           map[string][4]float64
	}{
		{"open, high, low, close", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"at","grain":"week"}],"y":[{"column":"opening"},{"column":"highest"},{"column":"lowest"},{"column":"closing"}]}}`,
			map[string][4]float64{"2026-07-06": {9, 21, 6, 16}, "2026-07-13": {20, 25, 17, 19}, "2026-07-20": {30, 32, 29, 31}}},
		// One price as all four: 08:00's close is a row now, the week's first.
		{"the close as all four", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"at","grain":"week"}],"y":[{"column":"closing"},{"column":"closing"},{"column":"closing"},{"column":"closing"}]}}`,
			map[string][4]float64{"2026-07-06": {5, 20, 5, 16}, "2026-07-13": {21, 24, 19, 19}, "2026-07-20": {31, 31, 31, 31}}},
	}

	// Controls, worked out over the week of 6 July's full rows: its open is neither its
	// least nor its greatest open, nor the open of the row it ties with, and its close the
	// same; so neither MIN nor MAX, nor a tie broken the other way, gives the candle.
	var opens, closes []int
	for _, r := range rows {
		if r.region != "APAC" && r.prices[0] != nil && r.at < "2026-07-13" {
			opens, closes = append(opens, *r.prices[0]), append(closes, *r.prices[3])
		}
	}
	sort.Ints(opens)
	sort.Ints(closes)
	full := cases[0].want["2026-07-06"]
	for _, wrong := range []float64{float64(opens[0]), float64(opens[len(opens)-1]), 10} {
		if full[0] == wrong {
			t.Fatalf("the week's open %g is also %g: pick rows that tell them apart", full[0], wrong)
		}
	}
	for _, wrong := range []float64{float64(closes[0]), float64(closes[len(closes)-1]), 13} {
		if full[3] == wrong {
			t.Fatalf("the week's close %g is also %g: pick rows that tell them apart", full[3], wrong)
		}
	}

	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, c := range cases {
			q, err := writeChartSQL(mustSettings(t, c.settings), chartSource{Dialect: dialect, SQL: candleSourceSQL(rows)}, []charts.Bound{d4bFilter}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			// The same candles every run: a tie broken by the engine's whim would not be.
			for run := 0; run < 3; run++ {
				res, err := db.Query(q.SQL, q.Args...)
				if err != nil {
					t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
				}
				var weeks []string
				for res.Next() {
					var x any
					var v [4]sql.NullFloat64
					if err := res.Scan(&x, &v[0], &v[1], &v[2], &v[3]); err != nil {
						t.Fatal(err)
					}
					week := dayText(x)
					weeks = append(weeks, week)
					w, ok := c.want[week]
					if !ok {
						t.Errorf("%s: a candle for the week of %s", c.name, week)
						continue
					}
					for i := range v {
						if !sameNumber(floatOf(v[i]), &w[i]) {
							t.Errorf("%s week of %s %s: got %s, want %g", c.name, week, charts.PriceAliases[i], nullNumber(v[i]), w[i])
						}
					}
				}
				if err := res.Err(); err != nil {
					t.Fatal(err)
				}
				res.Close()
				if strings.Join(weeks, ",") != "2026-07-06,2026-07-13,2026-07-20" {
					t.Fatalf("%s: weeks %v, want the three Mondays in order", c.name, weeks)
				}
			}
			t.Logf("%s %s: 3 weekly candles match, three runs; the week of 6 July is %v", dialect, c.name, c.want["2026-07-06"])
		}
	})
}

// nullNumber shows a number the engine returned, or NULL.
func nullNumber(v sql.NullFloat64) string {
	if !v.Valid {
		return "NULL"
	}
	return fmt.Sprint(v.Float64)
}
