//go:build integration_pg

// D4c's SQL run on a real engine (dashboards-design.md § 14.9): a pivot table's cells and
// totals, each worked out in Go over the same rows, its totals first; and a scatter's or
// a bubble's points, capped at 10,000 with their colours past 7 folded into Other over
// every row, not over the rows the cap keeps. Same containers as chart_grain_pg_test.go:
//
//	CHART_GRAIN_PG_DSN='postgres://postgres:verify@localhost:55473/postgres?sslmode=disable' \
//	CHART_GRAIN_MYSQL_DSN='root:verify@tcp(localhost:55474)/' \
//	    go test -tags integration_pg ./internal/handlers/ -run PG_ChartD4c -v
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

// pivotRow is one pivot source row; an empty region and a nil amount are NULL.
type pivotRow struct {
	region  string
	day     time.Time
	channel string
	amount  *int
}

// pivotRows is 72 rows over July to September: every region with every channel, NULL
// among the regions (a value of its own, not a total) and among the amounts. The first
// row has every value, so each column's type is set by it on both engines.
func pivotRows() []pivotRow {
	regions := []string{"US", "EU", "APAC", ""}
	channels := []string{"web", "store", "phone"}
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]pivotRow, 72)
	for i := range rows {
		var a *int
		if i%7 != 3 {
			v := i*37%50 + 1
			a = &v
		}
		rows[i] = pivotRow{regions[i%4], start.AddDate(0, 0, i*5%92), channels[i%3], a}
	}
	return rows
}

func pivotSourceSQL(rows []pivotRow) string {
	parts := make([]string, len(rows))
	for i, r := range rows {
		region, a := "NULL", "NULL"
		if r.region != "" {
			region = "'" + r.region + "'"
		}
		if r.amount != nil {
			a = fmt.Sprint(*r.amount)
		}
		parts[i] = fmt.Sprintf("SELECT %s AS region, DATE '%s' AS day, '%s' AS channel, %s AS amount", region, r.day.Format("2006-01-02"), r.channel, a)
	}
	return strings.Join(parts, " UNION ALL ")
}

// pivotFilter leaves out the phone rows, which a pivot that lost the filter would count.
var pivotFilter = charts.Bound{Column: "channel", Kind: charts.FilterValues, Values: []string{"web", "store"}}

// pivotCell is one cell or total: the sum, the average and the count of its rows.
type pivotCell struct {
	sum, avg *float64
	count    int
}

func (c pivotCell) String() string {
	show := func(v *float64) string {
		if v == nil {
			return "NULL"
		}
		return fmt.Sprintf("%.4f", *v)
	}
	return fmt.Sprintf("{sum %s avg %s count %d}", show(c.sum), show(c.avg), c.count)
}

// wantPivot works out every cell and total the way GROUPING SETS does: one per prefix of
// the row fields with one per prefix of the column fields, a field outside the prefix
// keyed "*". A NULL value is keyed "∅", so it never reads as a total.
func wantPivot(rows []pivotRow, fields []func(pivotRow) string, nRows int) map[string]pivotCell {
	type acc struct {
		sum   float64
		n, nn int
	}
	accs := map[string]*acc{}
	nCols := len(fields) - nRows
	for _, r := range rows {
		for rp := 0; rp <= nRows; rp++ {
			for cp := 0; cp <= nCols; cp++ {
				key := make([]string, len(fields))
				for i, f := range fields {
					if (i < nRows && i < rp) || (i >= nRows && i-nRows < cp) {
						key[i] = f(r)
					} else {
						key[i] = "*"
					}
				}
				k := strings.Join(key, "|")
				a := accs[k]
				if a == nil {
					a = &acc{}
					accs[k] = a
				}
				a.n++
				if r.amount != nil {
					a.sum += float64(*r.amount)
					a.nn++
				}
			}
		}
	}
	out := map[string]pivotCell{}
	for k, a := range accs {
		c := pivotCell{count: a.n}
		if a.nn > 0 {
			sum, avg := a.sum, a.sum/float64(a.nn)
			c.sum, c.avg = &sum, &avg
		}
		out[k] = c
	}
	return out
}

func orNull(v string) string {
	if v == "" {
		return "∅"
	}
	return v
}

func sameNumber(a, b *float64) bool {
	return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 1e-4)
}

func floatOf(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func TestPG_ChartD4c_PivotTotalsAreTheSourcesOwn(t *testing.T) {
	all := pivotRows()
	var rows []pivotRow
	for _, r := range all {
		if r.channel != "phone" {
			rows = append(rows, r)
		}
	}
	region := func(r pivotRow) string { return orNull(r.region) }
	month := func(r pivotRow) string { return r.day.Format("2006-01") + "-01" }
	channel := func(r pivotRow) string { return r.channel }
	cases := []struct {
		name     string
		settings string
		fields   []func(pivotRow) string
		nRows    int
		measures []string // the aggregates, in the order the query returns them
	}{
		{
			"region and month by channel",
			`{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"},{"column":"day","grain":"month"}],"columns":[{"column":"channel"}],"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"avg","column":"amount"},{"aggregate":"count"}]}}`,
			[]func(pivotRow) string{region, month, channel}, 2, []string{"sum", "avg", "count"},
		},
		{
			"region alone",
			`{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"}],"measure":[{"aggregate":"avg","column":"amount"}]}}`,
			[]func(pivotRow) string{region}, 1, []string{"avg"},
		},
	}

	// Control: an average's total that averaged its cells' averages would differ from
	// the source's average, so the test can tell them apart.
	grand := wantPivot(rows, cases[0].fields, 2)
	var cells float64
	nCells := 0
	for k, c := range grand {
		if !strings.Contains(k, "*") && c.avg != nil {
			cells += *c.avg
			nCells++
		}
	}
	total := grand["*|*|*"]
	if math.Abs(cells/float64(nCells)-*total.avg) < 1e-3 {
		t.Fatalf("the cells' averages average %g, the source's average is %g: pick rows that tell them apart", cells/float64(nCells), *total.avg)
	}
	// The grand total's count by hand: the rows the filter keeps.
	if total.count != len(rows) || len(rows) == len(all) || len(rows) == 0 {
		t.Fatalf("grand total counts %d rows, the filter keeps %d of %d", total.count, len(rows), len(all))
	}

	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, c := range cases {
			s := mustSettings(t, c.settings)
			q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: pivotSourceSQL(all)}, []charts.Bound{pivotFilter}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
			}
			want := wantPivot(rows, c.fields, c.nRows)
			got := map[string][]sql.NullFloat64{}
			var order []string
			prevTotals := len(c.fields) + 1
			for res.Next() {
				values := make([]any, len(c.fields))
				measures := make([]sql.NullFloat64, len(c.measures))
				flags := make([]int, len(c.fields))
				dest := []any{}
				for i := range values {
					dest = append(dest, &values[i])
				}
				for i := range measures {
					dest = append(dest, &measures[i])
				}
				for i := range flags {
					dest = append(dest, &flags[i])
				}
				if err := res.Scan(dest...); err != nil {
					t.Fatal(err)
				}
				key := make([]string, len(c.fields))
				totals := 0
				for i, v := range values {
					switch {
					case flags[i] == 1:
						key[i], totals = "*", totals+1
					case v == nil:
						key[i] = "∅"
					case i == 1 && c.nRows == 2:
						key[i] = dayText(v)
					default:
						key[i] = string(textOf(v))
					}
				}
				// Totals first: the grand total, then each field's, then the cells.
				if totals > prevTotals {
					t.Errorf("%s: %v (%d totals) comes after a row with %d", c.name, key, totals, prevTotals)
				}
				prevTotals = totals
				k := strings.Join(key, "|")
				order = append(order, k)
				got[k] = measures
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			if len(got) != len(order) {
				t.Errorf("%s: %d rows but %d keys; a total and a value share one", c.name, len(order), len(got))
			}
			grandKey := strings.TrimSuffix(strings.Repeat("*|", len(c.fields)), "|")
			if len(order) == 0 || order[0] != grandKey {
				t.Fatalf("%s: the first row is %v, want the grand total %s", c.name, order, grandKey)
			}
			if len(got) != len(want) {
				t.Errorf("%s: %d cells and totals, want %d", c.name, len(got), len(want))
			}
			for k, w := range want {
				g, ok := got[k]
				if !ok {
					t.Errorf("%s %s: missing, want %v", c.name, k, w)
					continue
				}
				for i, m := range c.measures {
					count := float64(w.count)
					wm := map[string]*float64{"sum": w.sum, "avg": w.avg, "count": &count}[m]
					if !sameNumber(floatOf(g[i]), wm) {
						t.Errorf("%s %s %s: got %v, want %v", c.name, k, m, floatOf(g[i]), w)
					}
				}
			}
			t.Logf("%s %s: %d cells and totals match, the grand total first; its average %.4f is the source's, not %.4f",
				dialect, c.name, len(got), *want[grandKey].avg, cells/float64(nCells))
		}
	})
}

// textOf reads a text value the engine returns.
func textOf(v any) []byte {
	switch s := v.(type) {
	case []byte:
		return s
	case string:
		return []byte(s)
	}
	return []byte(fmt.Sprint(v))
}

// point is one scatter source row; a nil coordinate is NULL.
type point struct {
	x, y, z *int
	k       string
}

// pointWeights gives k1 to k9 their rows, a thousand per weight: k1 9,000 down to k9
// 1,000. Each colour's x starts at its weight times 1,000, so the least x, the points the
// cap keeps, are k9's, k8's, k7's and k6's: ranked over those alone, k9 would be kept and
// k1 to k4 never seen.
var pointWeights = map[string]int{"k1": 9, "k2": 8, "k3": 7, "k4": 6, "k5": 5, "k6": 4, "k7": 3, "k8": 2, "k9": 1}

// pointRows is pointSourceSQL's rows worked out in Go.
func pointRows() []point {
	n := func(v int) *int { return &v }
	var rows []point
	for k, w := range pointWeights {
		for a := 0; a < w; a++ {
			for rest := 0; rest < 1000; rest++ {
				i := a*1000 + rest
				rows = append(rows, point{n(i*37%1009 + w*1000), n((i*11 + w*3) % 503), n(i%17 + 1), k})
			}
		}
	}
	// k0's three rows each lack a coordinate a bubble needs; a scatter draws the last.
	return append(rows, point{nil, n(5), n(5), "k0"}, point{n(5), nil, n(5), "k0"}, point{n(0), n(0), nil, "k0"})
}

// pointSourceSQL makes pointRows from digits, in SQL both engines read: MOD, not %.
func pointSourceSQL() string {
	digits := make([]string, 10)
	for i := range digits {
		digits[i] = fmt.Sprintf("SELECT %d AS d", i)
	}
	d := "(" + strings.Join(digits, " UNION ALL ") + ")"
	weights := make([]string, 0, len(pointWeights))
	for k, w := range pointWeights {
		weights = append(weights, fmt.Sprintf("SELECT '%s' AS k, %d AS w", k, w))
	}
	sort.Strings(weights)
	return "SELECT MOD(t.i * 37, 1009) + t.w * 1000 AS px, MOD(t.i * 11 + t.w * 3, 503) AS py, MOD(t.i, 17) + 1 AS pz, t.k AS pk FROM (" +
		"SELECT a.d * 1000 + b.d * 100 + c.d * 10 + e.d AS i, kw.k, kw.w FROM " +
		d + " a CROSS JOIN " + d + " b CROSS JOIN " + d + " c CROSS JOIN " + d + " e JOIN (" + strings.Join(weights, " UNION ALL ") + ") kw ON a.d < kw.w" +
		") t UNION ALL SELECT NULL, 5, 5, 'k0' UNION ALL SELECT 5, NULL, 5, 'k0' UNION ALL SELECT 0, 0, NULL, 'k0'"
}

func TestPG_ChartD4c_PointsFoldOtherOverEveryRowThenCap(t *testing.T) {
	src := pointRows()
	// k5 is filtered out; a chart that lost the filter would rank its 5,000 rows fifth.
	var keep []string
	for k := range pointWeights {
		if k != "k5" {
			keep = append(keep, k)
		}
	}
	keep = append(keep, "k0")
	sort.Strings(keep)
	filters := []charts.Bound{{Column: "pk", Kind: charts.FilterValues, Values: keep}}
	cases := []struct {
		name, settings string
		size           bool
	}{
		{"scatter", `{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"series":[{"column":"pk"}]}}`, false},
		{"bubble", `{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}],"series":[{"column":"pk"}]}}`, true},
	}
	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, c := range cases {
			// Worked out in Go: the rows with every coordinate the type draws, each colour
			// ranked by its rows over all of them, the top 7 kept, the rows at one spot
			// merged (Other's colours into one), then the busiest 10,001 by value.
			var rows []point
			counts := map[string]int{}
			for _, p := range src {
				if p.k == "k5" || p.x == nil || p.y == nil || (c.size && p.z == nil) {
					continue
				}
				rows = append(rows, p)
				counts[p.k]++
			}
			ranked := make([]string, 0, len(counts))
			for k := range counts {
				ranked = append(ranked, k)
			}
			sort.Slice(ranked, func(i, j int) bool {
				if counts[ranked[i]] != counts[ranked[j]] {
					return counts[ranked[i]] > counts[ranked[j]]
				}
				return ranked[i] < ranked[j]
			})
			kept := map[string]bool{}
			for _, k := range ranked[:7] {
				kept[k] = true
			}
			type spot struct {
				x, y, z, other int
				k              string
			}
			per := map[spot]int{}
			for _, p := range rows {
				sp := spot{x: *p.x, y: *p.y, k: p.k}
				if c.size {
					sp.z = *p.z
				}
				if !kept[p.k] {
					sp.k, sp.other = "", 1
				}
				per[sp]++
			}
			spots := make([]spot, 0, len(per))
			for sp := range per {
				spots = append(spots, sp)
			}
			// Other's colour is NULL, after the kept colours on Postgres and before them on
			// MySQL.
			nullsFirst := dialect == "mysql"
			sort.Slice(spots, func(i, j int) bool {
				a, b := spots[i], spots[j]
				switch {
				case per[a] != per[b]:
					return per[a] > per[b]
				case a.x != b.x:
					return a.x < b.x
				case a.y != b.y:
					return a.y < b.y
				case a.z != b.z:
					return a.z < b.z
				case a.other != b.other:
					return (a.other == 1) == nullsFirst
				}
				return a.k < b.k
			})
			cap := charts.Spec(c.name).Cap.Rows + 1
			want := spots[:cap]
			others := 0
			for _, sp := range want {
				others += sp.other
			}
			// Control: the cap cuts the points, and some it keeps are Other only because the
			// ranks count every row; ranked over the kept rows alone they would be drawn.
			if len(spots) <= cap || others == 0 {
				t.Fatalf("%s: %d points, %d Other in the first %d: pick points the cap cuts", c.name, len(spots), others, cap)
			}

			s := mustSettings(t, c.settings)
			q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: pointSourceSQL()}, filters, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
			}
			i := 0
			for ; res.Next(); i++ {
				var x, y, size float64
				var colour sql.NullString
				var other, series, n int
				dest := []any{&x, &y}
				if c.size {
					dest = append(dest, &size)
				}
				if err := res.Scan(append(dest, &colour, &other, &series, &n)...); err != nil {
					t.Fatal(err)
				}
				if i >= len(want) {
					continue
				}
				w := want[i]
				if x != float64(w.x) || y != float64(w.y) || size != float64(w.z) || colour.String != w.k || colour.Valid != (w.other == 0) || other != w.other || series != len(ranked) || n != per[w] {
					t.Fatalf("%s row %d: got (%g, %g, %g, %v, other %d, %d series) × %d, want (%d, %d, %d, %q, other %d, %d series) × %d",
						c.name, i, x, y, size, colour, other, series, n, w.x, w.y, w.z, w.k, w.other, len(ranked), per[w])
				}
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			if i != cap {
				t.Fatalf("%s: %d rows, want the cap's %d", c.name, i, cap)
			}
			t.Logf("%s %s: %d of %d points (%d rows) match in order, %d series, %d of those points in Other (%s)",
				dialect, c.name, i, len(spots), len(rows), len(ranked), others, strings.Join(ranked[7:], ", "))
		}
	})
}

// mergeSourceSQL is mergeRows in SQL both engines read: 20,000 rows on one spot, (1, 0),
// half coloured a and half b; 10,000 points a row each from x 2 up; and 100 points far
// right, from x 100,000, of 2 to 4 rows each.
func mergeSourceSQL() string {
	digits := make([]string, 10)
	for i := range digits {
		digits[i] = fmt.Sprintf("SELECT %d AS d", i)
	}
	d := "(" + strings.Join(digits, " UNION ALL ") + ")"
	four := d + " a CROSS JOIN " + d + " b CROSS JOIN " + d + " c CROSS JOIN " + d + " e"
	return "SELECT 1 AS px, 0 AS py, 1 AS pz, CASE WHEN h.d = 0 THEN 'a' ELSE 'b' END AS pk FROM " + four + " CROSS JOIN (SELECT 0 AS d UNION ALL SELECT 1) h" +
		" UNION ALL SELECT 2 + a.d * 1000 + b.d * 100 + c.d * 10 + e.d, MOD(a.d + b.d + c.d + e.d, 7), 1 + MOD(e.d, 4), 's' FROM " + four +
		" UNION ALL SELECT 100000 + 10 * a.d + b.d, MOD(a.d + b.d, 5), 1 + MOD(b.d, 4), CASE MOD(a.d, 3) WHEN 0 THEN 'c' WHEN 1 THEN 'd' ELSE 'e' END FROM " +
		d + " a CROSS JOIN " + d + " b JOIN " + d + " r ON r.d <= 1 + MOD(b.d, 3)"
}

// mergeRows is mergeSourceSQL's rows worked out in Go.
func mergeRows() []point {
	n := func(v int) *int { return &v }
	var rows []point
	for i := 0; i < 20000; i++ {
		k := "a"
		if i%2 == 1 {
			k = "b"
		}
		rows = append(rows, point{n(1), n(0), n(1), k})
	}
	for i := 0; i < 10000; i++ {
		rows = append(rows, point{n(2 + i), n((i/1000 + i/100%10 + i/10%10 + i%10) % 7), n(1 + i%10%4), "s"})
	}
	for a := 0; a < 10; a++ {
		for b := 0; b < 10; b++ {
			for r := 0; r < 2+b%3; r++ {
				rows = append(rows, point{n(100000 + 10*a + b), n((a + b) % 5), n(1 + b%4), string(rune('c' + a%3))})
			}
		}
	}
	return rows
}

// A scatter or a bubble chart draws a point per spot, not a point per row: rows at one
// x, y, size and colour are one point with their count, and when the spots outnumber the
// cap the busiest are kept. Ordered by value alone, the cap kept the source's least
// corner: 10,001 rows all at (1, 0), and nothing else drawn (KI-CHART-SCATTER-CAP-KEEPS-ONE-CORNER).
func TestPG_ChartD4c_PointsMergeThenKeepTheBusiest(t *testing.T) {
	src := mergeRows()
	cases := []struct {
		name, settings string
		size, colour   bool
	}{
		{"scatter", `{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}]}}`, false, false},
		{"scatter_colour", `{"version":2,"type":"scatter","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"series":[{"column":"pk"}]}}`, false, true},
		{"bubble", `{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}]}}`, true, false},
		{"bubble_colour", `{"version":2,"type":"bubble","roles":{"x":[{"column":"px"}],"y":[{"column":"py"}],"size":[{"column":"pz"}],"series":[{"column":"pk"}]}}`, true, true},
	}
	type spot struct {
		x, y, z int
		k       string
	}
	d4bEngines(t, func(t *testing.T, dialect string, db *sql.DB) {
		for _, c := range cases {
			// Worked out in Go: the rows at each spot counted, the busiest first, then by
			// x, y, size and colour, cut to the cap.
			counts := map[spot]int{}
			for _, p := range src {
				sp := spot{*p.x, *p.y, 0, ""}
				if c.size {
					sp.z = *p.z
				}
				if c.colour {
					sp.k = p.k
				}
				counts[sp]++
			}
			spots := make([]spot, 0, len(counts))
			for sp := range counts {
				spots = append(spots, sp)
			}
			sort.Slice(spots, func(i, j int) bool {
				a, b := spots[i], spots[j]
				if counts[a] != counts[b] {
					return counts[a] > counts[b]
				}
				if a.x != b.x {
					return a.x < b.x
				}
				if a.y != b.y {
					return a.y < b.y
				}
				if a.z != b.z {
					return a.z < b.z
				}
				return a.k < b.k
			})
			s := mustSettings(t, c.settings)
			cap := charts.Spec(s.Type).Cap.Rows + 1
			want := spots[:cap]
			far := 0
			for _, sp := range want {
				if sp.x >= 100000 {
					far++
				}
			}
			// Control: the spots outnumber the cap, the cut keeps every far-right point,
			// and the cap's rows taken by value would all be the corner.
			if len(spots) <= cap || far != 100 || counts[want[len(want)-1]] != 1 {
				t.Fatalf("%s: %d spots, %d far kept: pick spots the cap cuts", c.name, len(spots), far)
			}

			q, err := writeChartSQL(s, chartSource{Dialect: dialect, SQL: mergeSourceSQL()}, nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			res, err := db.Query(q.SQL, q.Args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", c.name, err, q.SQL)
			}
			cols, err := res.Columns()
			if err != nil {
				t.Fatal(err)
			}
			i := 0
			for ; res.Next(); i++ {
				var x, y, size float64
				var colour sql.NullString
				var other, series, rows int
				dest := []any{&x, &y}
				if c.size {
					dest = append(dest, &size)
				}
				if c.colour {
					dest = append(dest, &colour, &other, &series)
				}
				counted := len(cols) == len(dest)+1
				if counted {
					dest = append(dest, &rows)
				}
				if err := res.Scan(dest...); err != nil {
					t.Fatal(err)
				}
				if i >= len(want) {
					continue
				}
				w := want[i]
				if x != float64(w.x) || y != float64(w.y) || size != float64(w.z) || colour.String != w.k || (counted && rows != counts[w]) || other != 0 {
					t.Fatalf("%s row %d: got (%g, %g, %g, %v, other %d) × %d, want (%d, %d, %d, %q) × %d",
						c.name, i, x, y, size, colour, other, rows, w.x, w.y, w.z, w.k, counts[w])
				}
			}
			if err := res.Err(); err != nil {
				t.Fatal(err)
			}
			res.Close()
			if i != cap {
				t.Fatalf("%s: %d points, want the cap's %d", c.name, i, cap)
			}
			if cols[len(cols)-1] != charts.PointRowsAlias {
				t.Fatalf("%s: the result is %v, want each point's row count last", c.name, cols)
			}
			t.Logf("%s %s: %d of %d spots match in order, the busiest %d rows, %d far-right points kept",
				dialect, c.name, i, len(spots), counts[want[0]], far)
		}
	})
}
