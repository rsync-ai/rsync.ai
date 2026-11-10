package handlers

// chart_sql.go writes a saved chart's SQL from its settings (dashboards-design.md
// § 3.1): one read statement over the chart's source, a model's table or a saved
// query's SQL as a subquery, with the GROUP BY running on the source and a row cap of
// charts.MaxRows+1, so a capped result is known. A dashboard filter's values are bound
// parameters, never pasted into the SQL (§ 4.2). Column names are quoted with
// quoteCheckColumn, which escapes the engine's closing quote, so any name the source
// returned is safe to reference.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/charts"
)

// chartSource is where a chart's rows come from: a model's built table or a saved
// query's SQL.
type chartSource struct {
	Dialect string
	// Table is a model's target, already quoted; empty for a saved query.
	Table string
	// SQL is a saved query's statement; empty for a model's table.
	SQL string
	// WeekStart is the connection's first day of the week, 1 (Monday, ISO) to 7
	// (Sunday), and FiscalMonth the month its year starts, 1 to 12 (decision 14). Zero
	// reads as 1.
	WeekStart   int
	FiscalMonth int
}

// fromClause is the source as a FROM item named src.
func (s chartSource) fromClause() string {
	if s.Table != "" {
		return s.Table + " src"
	}
	return "(\n" + trimStatement(s.SQL) + "\n) src"
}

// trimStatement drops trailing semicolons and space, so a saved query nests as a
// subquery.
func trimStatement(sqlText string) string {
	t := strings.TrimSpace(sqlText)
	for strings.HasSuffix(t, ";") {
		t = strings.TrimSpace(strings.TrimSuffix(t, ";"))
	}
	return t
}

// chartQuery is a chart's SQL and its bound values.
type chartQuery struct {
	SQL string
	// Args are the values bound to $n, ? or @pN, in order (Postgres, MySQL, SQL Server).
	Args []interface{}
	// Params are Databricks' named parameters, bound to :pN.
	Params []databricksParam
	// Unfiltered names the filters a delegated engine could not apply: the
	// orchestrator's connectors take no bound values, so the tile is drawn unfiltered
	// and says so, rather than paste a value into the SQL.
	Unfiltered []string
	// Spark is a scorecard's sparkline, its own statement with its own bound values,
	// run after this one.
	Spark *chartQuery
	// Approximate is a box plot whose quartiles are the engine's approximate ones
	// (BigQuery's APPROX_QUANTILES, ClickHouse's quantile), which the chart says under it
	// (dashboards-design.md open question 10).
	Approximate bool
	// Unordered is a table of rows' statement with only the sort picked, without the
	// columns added after it to put its rows in one order. It runs when the engine cannot
	// order by one of those columns (an array, a JSON value), so a table that drew before
	// they were added still draws (runChartQuery). Empty when nothing was added.
	Unordered string
}

// chartBinder hands out each engine's placeholder for the next bound value.
type chartBinder struct {
	dialect string
	args    []interface{}
	params  []databricksParam
}

// canBind reports whether the engine takes bound values on rsync.ai's path.
func (b *chartBinder) canBind() bool {
	switch b.dialect {
	case "postgres", "mysql", "sqlserver", "databricks":
		return true
	}
	return false
}

func (b *chartBinder) bind(v string) string {
	n := len(b.args) + len(b.params) + 1
	switch b.dialect {
	case "postgres":
		b.args = append(b.args, v)
		return fmt.Sprintf("$%d", n)
	case "mysql":
		b.args = append(b.args, v)
		return "?"
	case "sqlserver":
		b.args = append(b.args, v)
		return fmt.Sprintf("@p%d", n)
	case "databricks":
		name := fmt.Sprintf("p%d", n)
		b.params = append(b.params, databricksParam{Name: name, Value: v, Type: "STRING"})
		return ":" + name
	}
	panic("chartBinder.bind on an engine that takes no bound values")
}

// dayLiteral is a server-made date for an engine that takes no bound values: a
// scorecard's period bounds, formatted from a time.Time, digits and dashes only. A
// person's filter value never goes this way.
func dayLiteral(dialect string, t time.Time) string {
	d := t.UTC().Format("2006-01-02")
	if dialect == "oracle" {
		return "DATE '" + d + "'"
	}
	return "'" + d + "'"
}

// writeChartSQL writes the chart's SQL for its source and the viewer's filters.
// filters must already be held to the chart's source columns (applicableFilters). now
// sets a scorecard's comparison periods.
func writeChartSQL(s *charts.Settings, src chartSource, filters []charts.Bound, now time.Time) (*chartQuery, error) {
	d := src.Dialect
	if d == "" {
		return nil, fmt.Errorf("this connection cannot be charted")
	}
	q := func(col string) string { return "src." + quoteCheckColumn(d, col) }
	alias := func(a string) string { return quoteCheckColumn(d, a) }
	b := &chartBinder{dialect: d}
	out := &chartQuery{}

	// bindFilters binds the viewer's filters to a statement's binder and returns them as
	// WHERE conditions. Each statement calls it where its WHERE falls in the text, after
	// any value bound above it: MySQL's ? is positional, so a value bound out of text
	// order lands in another's place, with no error
	// (TestWriteChartSQL_PlaceholdersFollowTheText).
	bindFilters := func(b *chartBinder, into *chartQuery) []string {
		var where []string
		for _, f := range filters {
			if !b.canBind() {
				into.Unfiltered = append(into.Unfiltered, f.Column)
				continue
			}
			switch f.Kind {
			case charts.FilterDateRange:
				where = append(where, fmt.Sprintf("%s >= %s AND %s < %s",
					q(f.Column), b.bind(f.From.Format("2006-01-02")),
					q(f.Column), b.bind(f.To.Format("2006-01-02"))))
			case charts.FilterValues:
				ph := make([]string, len(f.Values))
				for i, v := range f.Values {
					ph[i] = b.bind(v)
				}
				where = append(where, fmt.Sprintf("%s IN (%s)", q(f.Column), strings.Join(ph, ", ")))
			}
		}
		return where
	}

	agg := func(m *charts.Field, cond string) string {
		val := "1"
		if m.Column != "" {
			val = q(m.Column)
		}
		if cond != "" {
			val = "CASE WHEN " + cond + " THEN " + val + " END"
		}
		switch m.Aggregate {
		case charts.AggCount:
			if m.Column == "" && cond == "" {
				return checkCountFn(d) + "(*)"
			}
			return checkCountFn(d) + "(" + val + ")"
		case charts.AggCountDistinct:
			return checkCountFn(d) + "(DISTINCT " + val + ")"
		default:
			return strings.ToUpper(m.Aggregate) + "(" + val + ")"
		}
	}

	spec := charts.Spec(s.Type)
	if spec == nil {
		return nil, fmt.Errorf("the chart type %q is not one rsync.ai draws", s.Type)
	}
	m := s.One(charts.RoleMeasure)
	aliases := s.MeasureAliases()
	from := src.fromClause()
	var sel, group, order, where []string
	switch s.Type {
	case charts.TypeScorecard:
		if s.Compare == nil {
			sel = append(sel, agg(m, "")+" AS "+alias(m.MeasureAlias()))
			where = bindFilters(b, out)
			break
		}
		// The last Compare.Days days up to today, against the Days before them.
		today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
		end := today.AddDate(0, 0, 1)
		mid := end.AddDate(0, 0, -s.Compare.Days)
		start := mid.AddDate(0, 0, -s.Compare.Days)
		bound := func(b *chartBinder, t time.Time) string {
			if b.canBind() {
				return b.bind(t.Format("2006-01-02"))
			}
			return dayLiteral(d, t)
		}
		dateCol := s.One(charts.RoleDate).Column
		c := q(dateCol)
		cur := fmt.Sprintf("%s >= %s AND %s < %s", c, bound(b, mid), c, bound(b, end))
		prev := fmt.Sprintf("%s >= %s AND %s < %s", c, bound(b, start), c, bound(b, mid))
		sel = append(sel, agg(m, cur)+" AS "+alias(m.MeasureAlias()))
		sel = append(sel, agg(m, prev)+" AS "+alias(charts.PreviousAlias))
		// The periods are bound above the WHERE, so they come first.
		where = bindFilters(b, out)
		if s.Sparkline {
			// The current period, a point per sparkGrain: the filters, then the period,
			// bound in that order on the sparkline's own binder.
			sb, spark := &chartBinder{dialect: d}, &chartQuery{}
			sw := bindFilters(sb, spark)
			sw = append(sw, fmt.Sprintf("%s >= %s AND %s < %s", c, bound(sb, mid), c, bound(sb, end)))
			e := chartDateGrain(d, sparkGrain(s.Compare.Days), c, src.WeekStart, src.FiscalMonth)
			spark.SQL = assembleChartSQL(d, []string{e + " AS " + alias(dateCol), agg(m, "") + " AS " + alias(m.MeasureAlias())},
				from, sw, []string{e}, []string{e + " ASC"}, s.Compare.Days+1)
			spark.Args, spark.Params = sb.args, sb.params
			out.Spark = spark
		}
	case charts.TypeHistogram:
		x := s.One(charts.RoleX)
		if x == nil {
			return nil, fmt.Errorf("a histogram needs an x column")
		}
		v := q(x.Column)
		count := checkCountFn(d) + "(*) AS " + alias("count")
		where = append([]string{v + " IS NOT NULL"}, bindFilters(b, out)...)
		if x.Width > 0 {
			// Bins Width wide from zero: each row's bin is its value rounded down to one.
			w := binWidthLiteral(x.Width)
			e := "FLOOR(" + v + " / " + w + ") * " + w
			sel = append(sel, e+" AS "+alias(x.Column), w+" AS "+alias(charts.BinWidthAlias), count)
			group = append(group, e)
			order = append(order, e+" ASC")
			break
		}
		// Bins equal parts of the least to the greatest value, over every row (s), each
		// row's bin its distance from the least in widths, the greatest in the last (b).
		// One value throughout is one bin of width 0; NULLIF keeps its division from
		// running on an engine that evaluates every branch.
		bins := x.Bins
		if bins == 0 {
			bins = charts.DefaultBins
		}
		rv, lo, w, bin := alias("rsync_v"), alias("rsync_lo"), alias("rsync_w"), alias("rsync_bin")
		inner := fmt.Sprintf("SELECT %s AS %s, MIN(%s) OVER () AS %s, (MAX(%s) OVER () - MIN(%s) OVER ()) * 1.0 / %d AS %s\nFROM %s\nWHERE %s",
			v, rv, v, lo, v, v, bins, w, from, strings.Join(where, " AND "))
		step := fmt.Sprintf("FLOOR((s.%s - s.%s) / NULLIF(s.%s, 0))", rv, lo, w)
		binned := fmt.Sprintf("SELECT CASE WHEN s.%s = 0 THEN 0 WHEN %s >= %d THEN %d ELSE %s END AS %s, s.%s, s.%s\nFROM (\n%s\n) s",
			w, step, bins, bins-1, step, bin, lo, w, inner)
		from, where = "(\n"+binned+"\n) b", nil
		sel = append(sel, fmt.Sprintf("b.%s + b.%s * b.%s AS %s", lo, bin, w, alias(x.Column)), "b."+w+" AS "+alias(charts.BinWidthAlias), count)
		group = append(group, "b."+bin, "b."+lo, "b."+w)
		order = append(order, "b."+bin+" ASC")
	case charts.TypeScatter, charts.TypeBubble:
		// A point a spot (§ 14.3's rows path): the rows at one x, y, size and colour are
		// one point, their count last, and at most Cap.Rows points, the busiest first and
		// then by value so the same points come back each time. Ordered by value alone,
		// the cap kept the source's least corner and drew nothing else when its rows
		// outnumbered the cap. A row with no x or y is not a point, nor one whose size is
		// not above zero, which the page cannot draw: left out here, it never takes the
		// place of a point drawn (contract K7). Above zero is also not NULL.
		var cols []string
		for _, role := range []string{charts.RoleX, charts.RoleY, charts.RoleSize} {
			if f := s.One(role); f != nil {
				cols = append(cols, f.Column)
				if role == charts.RoleSize {
					where = append(where, q(f.Column)+" > 0")
				} else {
					where = append(where, q(f.Column)+" IS NOT NULL")
				}
			}
		}
		where = append(where, bindFilters(b, out)...)
		if s.FoldsSeries() {
			from, sel, group = foldPointsSQL(d, spec.Cap.Series, s, cols, from, where)
			where = nil
		} else {
			for _, c := range cols {
				sel = append(sel, q(c)+" AS "+alias(c))
				group = append(group, q(c))
			}
		}
		count := checkCountFn(d) + "(*)"
		sel = append(sel, count+" AS "+alias(charts.PointRowsAlias))
		order = append(order, count+" DESC")
		for _, e := range group {
			order = append(order, e+" ASC")
		}
	case charts.TypePivot:
		p := pivotParts{rows: len(s.Roles[charts.RoleRows])}
		for _, role := range []string{charts.RoleRows, charts.RoleColumns} {
			for i, f := range s.Roles[role] {
				e := q(f.Column)
				if f.Grain != "" {
					e = chartDateGrain(d, f.Grain, e, src.WeekStart, src.FiscalMonth)
				}
				rc := "r"
				if role == charts.RoleColumns {
					rc = "c"
				}
				p.fields = append(p.fields, pivotField{expr: e, alias: alias(f.Column), flag: alias(charts.PivotTotalAlias(rc, i+1))})
			}
		}
		for i, mf := range s.Roles[charts.RoleMeasure] {
			p.measures = append(p.measures, agg(&mf, ""))
			p.measureAliases = append(p.measureAliases, alias(aliases[i]))
		}
		from, sel, order = pivotSQL(d, p, from, func() []string { return bindFilters(b, out) })
	case charts.TypeTreemap, charts.TypeSunburst:
		// A part a row: each level's value, outermost first, and the measure, the largest
		// parts first so a capped tree keeps its largest (§ 14.8).
		for _, f := range s.Roles[charts.RoleRows] {
			e := q(f.Column)
			sel = append(sel, e+" AS "+alias(f.Column))
			group = append(group, e)
		}
		where = bindFilters(b, out)
		measure := agg(m, "")
		sel = append(sel, measure+" AS "+alias(aliases[0]))
		order = append(order, measure+" DESC")
		for _, e := range group {
			order = append(order, e+" ASC")
		}
	case charts.TypeBullet:
		// The measure and, when the target is a measure, the target: one row, like a
		// scorecard's.
		where = bindFilters(b, out)
		for i, mf := range s.Roles[charts.RoleMeasure] {
			sel = append(sel, agg(&mf, "")+" AS "+alias(aliases[i]))
		}
	case charts.TypeBoxplot:
		// Each group's least, quartiles and greatest, from its rows' values: the engine's
		// exact percentile where it has one as an aggregate, its approximate one where that
		// is all it has, and none on an engine that has it only as a window function.
		x, y := s.One(charts.RoleX), s.One(charts.RoleY)
		e := q(x.Column)
		if x.Grain != "" {
			e = chartDateGrain(d, x.Grain, e, src.WeekStart, src.FiscalMonth)
		}
		v := q(y.Column)
		var quartile func(p int) string
		switch d {
		case "mysql":
			return nil, fmt.Errorf("a box plot needs an exact percentile, which MySQL and MariaDB do not have (MariaDB has one only as a window function)")
		case "sqlserver":
			return nil, fmt.Errorf("a box plot needs an exact percentile, which SQL Server has only as a window function")
		case "bigquery":
			out.Approximate = true
			quartile = func(p int) string { return fmt.Sprintf("APPROX_QUANTILES(%s, 4)[OFFSET(%d)]", v, p) }
		case "clickhouse":
			out.Approximate = true
			quartile = func(p int) string { return fmt.Sprintf("quantile(0.%d)(%s)", p*25, v) }
		default:
			// Redshift takes several PERCENTILE_CONTs only with the same ORDER BY, as here.
			quartile = func(p int) string { return fmt.Sprintf("PERCENTILE_CONT(0.%d) WITHIN GROUP (ORDER BY %s)", p*25, v) }
		}
		where = append([]string{v + " IS NOT NULL"}, bindFilters(b, out)...)
		sel = append(sel, e+" AS "+alias(x.Column), "MIN("+v+") AS "+alias(charts.BoxAliases[0]))
		for p := 1; p <= 3; p++ {
			sel = append(sel, quartile(p)+" AS "+alias(charts.BoxAliases[p]))
		}
		sel = append(sel, "MAX("+v+") AS "+alias(charts.BoxAliases[4]))
		group = append(group, e)
		if s.Sort == charts.SortXDesc {
			order = append(order, e+" DESC")
		} else {
			order = append(order, e+" ASC")
		}
	case charts.TypeCandlestick:
		// A candle a period: its first row's open, its highest high, its lowest low and its
		// last row's close, each row numbered within its period from either end. A tie on
		// the date is broken by the prices, so the same candle comes back each time.
		x, ys := s.One(charts.RoleX), s.ColumnsOf(charts.RoleY)
		t := q(x.Column)
		e := t
		if x.Grain != "" {
			e = chartDateGrain(d, x.Grain, t, src.WeekStart, src.FiscalMonth)
		}
		inner := []string{t + " IS NOT NULL"}
		seen := map[string]bool{}
		for _, c := range ys {
			if !seen[c] {
				seen[c] = true
				inner = append(inner, q(c)+" IS NOT NULL")
			}
		}
		inner = append(inner, bindFilters(b, out)...)
		// rank orders by the date, then by each named price once: SQL Server refuses a
		// column listed twice in an ORDER BY, which a candle of one price column would.
		rank := func(dir string, cols ...string) string {
			by, listed := []string{t + " " + dir}, map[string]bool{}
			for _, c := range cols {
				if !listed[c] {
					listed[c] = true
					by = append(by, q(c)+" "+dir)
				}
			}
			return fmt.Sprintf("ROW_NUMBER() OVER (PARTITION BY %s ORDER BY %s)", e, strings.Join(by, ", "))
		}
		rx, ro, rh, rl, rc := alias("rsync_x"), alias("rsync_o"), alias("rsync_h"), alias("rsync_l"), alias("rsync_c")
		first, last := alias("rsync_first"), alias("rsync_last")
		from = fmt.Sprintf("(\nSELECT %s AS %s, %s AS %s, %s AS %s, %s AS %s, %s AS %s, %s AS %s, %s AS %s\nFROM %s\nWHERE %s\n) k",
			e, rx, q(ys[0]), ro, q(ys[1]), rh, q(ys[2]), rl, q(ys[3]), rc,
			rank("ASC", ys[0], ys[3]), first, rank("DESC", ys[3], ys[0]), last,
			from, strings.Join(inner, " AND "))
		sel = append(sel, "k."+rx+" AS "+alias(x.Column),
			"MAX(CASE WHEN k."+first+" = 1 THEN k."+ro+" END) AS "+alias(charts.PriceAliases[0]),
			"MAX(k."+rh+") AS "+alias(charts.PriceAliases[1]),
			"MIN(k."+rl+") AS "+alias(charts.PriceAliases[2]),
			"MAX(CASE WHEN k."+last+" = 1 THEN k."+rc+" END) AS "+alias(charts.PriceAliases[3]))
		group = append(group, "k."+rx)
		order = append(order, "k."+rx+" ASC")
	case charts.TypeTable:
		where = bindFilters(b, out)
		cols := s.ColumnsOf(charts.RoleColumns)
		for _, col := range cols {
			sel = append(sel, q(col)+" AS "+alias(col))
			if m != nil {
				group = append(group, q(col))
			}
		}
		if m != nil {
			sel = append(sel, agg(m, "")+" AS "+alias(m.MeasureAlias()))
		}
		switch s.Sort {
		case charts.SortXAsc:
			order = append(order, q(cols[0])+" ASC")
		case charts.SortXDesc:
			order = append(order, q(cols[0])+" DESC")
		case charts.SortValueAsc:
			order = append(order, agg(m, "")+" ASC")
		case charts.SortValueDesc:
			order = append(order, agg(m, "")+" DESC")
		}
		// Then each column in turn, so the rows are in one order on every run and a capped
		// table keeps its first rows in that order, not whatever part of the source the
		// engine read first.
		picked := len(order)
		for i, col := range cols {
			if i == 0 && (s.Sort == charts.SortXAsc || s.Sort == charts.SortXDesc) {
				continue
			}
			order = append(order, q(col)+" ASC")
		}
		if m == nil && len(order) > picked {
			// A column of rows the engine cannot order (an array, a JSON value) fails the
			// added order, so the statement without it is kept to run instead. A grouped
			// table needs none: its GROUP BY fails such a column the same.
			out.Unordered = assembleChartSQL(d, sel, from, where, group, order[:picked], spec.Cap.Rows+1)
		}
	default: // the type's dimensions in the registry's order, x first (a pie has only its series), then its measures
		// Each dimension's expression is repeated in GROUP BY and ORDER BY, never its
		// alias, which SQL Server and Oracle refuse there (§ 14.3).
		var dims []string
		for _, r := range spec.Roles {
			f := s.One(r.Name)
			if r.Name == charts.RoleMeasure || f == nil {
				continue
			}
			e := q(f.Column)
			grain := f.Grain
			if s.Type == charts.TypeCalendar {
				// A calendar's square is a day, whatever the column's own time.
				grain = charts.GrainDay
			}
			if grain != "" {
				e = chartDateGrain(d, grain, e, src.WeekStart, src.FiscalMonth)
			}
			dims = append(dims, e)
			sel = append(sel, e+" AS "+alias(f.Column))
			group = append(group, e)
		}
		if len(dims) == 0 || m == nil {
			return nil, fmt.Errorf("a %s chart needs a column to group by and a measure", s.Type)
		}
		where = bindFilters(b, out)
		measure := agg(m, "")
		if s.FoldsSeries() {
			from, sel, dims, measure = foldSeriesSQL(d, spec.Cap.Series, s, from, sel, group, where, measure)
			group, where = dims, nil
		} else {
			sel = append(sel, measure+" AS "+alias(aliases[0]))
			// A combo's second measure, read against its own axis; the sort is by the first.
			for i, m2 := range s.Roles[charts.RoleMeasure][1:] {
				sel = append(sel, agg(&m2, "")+" AS "+alias(aliases[i+1]))
			}
		}
		if s.One(charts.RoleX) == nil {
			// A pie's slices, largest first, then Other (foldSeriesSQL's dims are the
			// series and Other's flag).
			order = append(order, dims[1]+" ASC", measure+" DESC", dims[0]+" ASC")
			break
		}
		sort := s.Sort
		if sort == "" {
			sort = charts.SortXAsc
			// A bar of one measure, largest first, unless its x is grouped by a date grain:
			// periods read in time order, oldest first, as a line draws them.
			oneBar := (s.Type == charts.TypeBar || s.Type == charts.TypeHBar) && s.One(charts.RoleSeries) == nil && len(aliases) == 1 &&
				s.One(charts.RoleX).Grain == ""
			switch {
			case oneBar || s.Type == charts.TypeFunnel || s.Type == charts.TypeSankey || s.Type == charts.TypeMap:
				// A sankey's largest flows first, so a capped one keeps them (§ 14.5); a
				// map's largest first, so a shape two codes name (FR and FRA) is filled by
				// the larger.
				sort = charts.SortValueDesc
			case s.Type == charts.TypeCalendar:
				// The latest days first, so a capped calendar is the latest year.
				sort = charts.SortXDesc
			}
		}
		switch sort {
		case charts.SortXAsc:
			order = append(order, dims[0]+" ASC")
		case charts.SortXDesc:
			order = append(order, dims[0]+" DESC")
		case charts.SortValueAsc:
			order = append(order, measure+" ASC")
		case charts.SortValueDesc:
			order = append(order, measure+" DESC")
		}
		if s.Type == charts.TypeSankey || s.Type == charts.TypeMap {
			// A tie between flows by their source, then their target: a sankey places its
			// nodes in the order of its rows. A tie between a map's codes by the code, so
			// the same rows fill the same shapes on every run.
			order = append(order, dims[0]+" ASC")
		}
		// Then the other dimensions, so the order of the rows is stable.
		for _, e := range dims[1:] {
			order = append(order, e+" ASC")
		}
	}

	out.SQL = assembleChartSQL(d, sel, from, where, group, order, spec.Cap.Rows+1)
	out.Args = b.args
	out.Params = b.params
	return out, nil
}

// assembleChartSQL writes one read statement from its parts, capped at limit rows in
// the engine's own words.
func assembleChartSQL(d string, sel []string, from string, where, group, order []string, limit int) string {
	var sb strings.Builder
	sb.WriteString("SELECT ")
	if d == "sqlserver" {
		fmt.Fprintf(&sb, "TOP (%d) ", limit)
	}
	sb.WriteString(strings.Join(sel, ", "))
	sb.WriteString("\nFROM ")
	sb.WriteString(from)
	if len(where) > 0 {
		sb.WriteString("\nWHERE ")
		sb.WriteString(strings.Join(where, " AND "))
	}
	if len(group) > 0 {
		sb.WriteString("\nGROUP BY ")
		sb.WriteString(strings.Join(group, ", "))
	}
	if len(order) > 0 {
		sb.WriteString("\nORDER BY ")
		sb.WriteString(strings.Join(order, ", "))
	}
	switch d {
	case "sqlserver":
	case "oracle":
		fmt.Fprintf(&sb, "\nFETCH FIRST %d ROWS ONLY", limit)
	default:
		fmt.Fprintf(&sb, "\nLIMIT %d", limit)
	}
	return sb.String()
}

// sparkGrain is a sparkline's date grain: a point a day over 7 or 30 days, a week over
// 90 and a month over 365. A week or a month that starts before the period is its
// first point, counted from the period's first day.
func sparkGrain(days int) string {
	switch {
	case days <= 30:
		return charts.GrainDay
	case days <= 90:
		return charts.GrainWeek
	}
	return charts.GrainMonth
}

// binWidthLiteral writes a histogram's bin width as a decimal literal, never an integer,
// so an integer column divides as a decimal. Validate holds it between 1e-9 and 1e15
// (charts' minBinWidth and maxBinWidth), which 'f' writes without an exponent.
func binWidthLiteral(w float64) string {
	lit := strconv.FormatFloat(w, 'f', -1, 64)
	if !strings.Contains(lit, ".") {
		lit += ".0"
	}
	return lit
}

// foldSeriesSQL wraps a series chart's grouped query so the source keeps the top
// limit series and folds the rest into one Other (dashboards-design.md § 14.3), over
// every row rather than the 10,001 the browser receives. sel and group are the grouped
// query's x and series (in that order; a pie has no x) and measure its aggregate; where
// is already bound.
//
// One pass, so the filters are bound once and cannot be missing from a half (Superset's
// first bug, § 14.1): g groups by x and series and totals each series across its x
// values; r ranks the series by that total, largest first, a series with no value last,
// then by name; the outer query keeps the top limit and groups the rest as Other. The
// total of a count, sum, average or distinct count, always a number, is the sum of its
// |measure| (the browser's old ranking); a min or max may be a date or text, which ABS
// refuses, so its total is its highest value. A count, sum, min or max folds into Other;
// an average or a distinct count cannot be added up, so Other's measure is NULL there and
// the chart says how many series it left out. Every row carries the source's number of
// series.
//
// It returns the outer query's FROM item, select list, dimensions (its GROUP BY, and its
// ORDER BY after the sort) and measure.
func foldSeriesSQL(d string, limit int, s *charts.Settings, from string, sel, group, where []string, measure string) (string, []string, []string, string) {
	alias := func(a string) string { return quoteCheckColumn(d, a) }
	series, m := alias(s.One(charts.RoleSeries).Column), alias(s.One(charts.RoleMeasure).MeasureAlias())
	total, rank := alias(charts.TotalAlias), alias(charts.RankAlias)
	// gx and rx carry x through g and r, and are empty for a pie.
	var x, gx, rx string
	if f := s.One(charts.RoleX); f != nil {
		x = alias(f.Column)
		gx, rx = "g."+x+", ", "r."+x
	}

	var g strings.Builder
	g.WriteString("SELECT ")
	g.WriteString(strings.Join(sel, ", "))
	totalOf := "SUM(ABS(" + measure + "))"
	if a := s.One(charts.RoleMeasure).Aggregate; a == charts.AggMin || a == charts.AggMax {
		totalOf = "MAX(" + measure + ")"
	}
	fmt.Fprintf(&g, ", %s AS %s, %s OVER (PARTITION BY %s) AS %s", measure, m, totalOf, group[len(group)-1], total)
	g.WriteString("\nFROM ")
	g.WriteString(from)
	if len(where) > 0 {
		g.WriteString("\nWHERE ")
		g.WriteString(strings.Join(where, " AND "))
	}
	g.WriteString("\nGROUP BY ")
	g.WriteString(strings.Join(group, ", "))

	r := fmt.Sprintf("(\nSELECT %sg.%s, g.%s, DENSE_RANK() OVER (ORDER BY CASE WHEN g.%s IS NULL THEN 1 ELSE 0 END, g.%s DESC, g.%s ASC) AS %s\nFROM (\n%s\n) g\n) r",
		gx, series, m, total, total, series, rank, g.String())

	kept := fmt.Sprintf("r.%s <= %d", rank, limit)
	keptSeries := "CASE WHEN " + kept + " THEN r." + series + " END"
	other := "CASE WHEN " + kept + " THEN 0 ELSE 1 END"
	// A kept series has one row in each group, so its value is its own; Other's is the
	// fold, or NULL for an aggregate that does not add up (charts.FoldsIntoOther).
	fold := "MAX(CASE WHEN " + kept + " THEN r." + m + " END)"
	switch a := s.One(charts.RoleMeasure).Aggregate; {
	case !charts.FoldsIntoOther(a):
	case a == charts.AggMin:
		fold = "MIN(r." + m + ")"
	case a == charts.AggMax:
		fold = "MAX(r." + m + ")"
	default:
		fold = "SUM(r." + m + ")"
	}
	outSel := []string{
		keptSeries + " AS " + series,
		fold + " AS " + m,
		other + " AS " + alias(charts.OtherAlias),
		"MAX(MAX(r." + rank + ")) OVER () AS " + alias(charts.SeriesCountAlias),
	}
	dims := []string{keptSeries, other}
	if rx != "" {
		outSel = append([]string{rx + " AS " + x}, outSel...)
		dims = append([]string{rx}, dims...)
	}
	return r, outSel, dims, fold
}

// foldPointsSQL is a scatter's or a bubble chart's points coloured by a text field, its
// top limit colours by how many rows each has and the rest as Other, ranked over every
// row like foldSeriesSQL's series: g counts each colour's rows, r ranks the colours,
// most rows first, then by name, and the outer query keeps a kept colour's name and
// flags the rest. cols are the point's x, y and size columns; where is already bound.
// It returns the FROM item, the select list and the point's dimensions, which the
// caller groups the rows by: x, y and size, the kept colour, and the Other flag, so
// Other's rows at one spot are one point.
func foldPointsSQL(d string, limit int, s *charts.Settings, cols []string, from string, where []string) (string, []string, []string) {
	alias := func(a string) string { return quoteCheckColumn(d, a) }
	colour := s.One(charts.RoleSeries).Column
	c := alias(colour)
	total, rank := alias(charts.TotalAlias), alias(charts.RankAlias)
	var gSel, rSel, sel, dims []string
	for _, col := range append(append([]string{}, cols...), colour) {
		a := alias(col)
		gSel = append(gSel, "src."+a+" AS "+a)
		rSel = append(rSel, "g."+a)
	}
	g := fmt.Sprintf("SELECT %s, %s(*) OVER (PARTITION BY src.%s) AS %s\nFROM %s\nWHERE %s",
		strings.Join(gSel, ", "), checkCountFn(d), c, total, from, strings.Join(where, " AND "))
	r := fmt.Sprintf("(\nSELECT %s, DENSE_RANK() OVER (ORDER BY g.%s DESC, g.%s ASC) AS %s\nFROM (\n%s\n) g\n) r",
		strings.Join(rSel, ", "), total, c, rank, g)
	kept := fmt.Sprintf("r.%s <= %d", rank, limit)
	for _, col := range cols {
		a := alias(col)
		sel = append(sel, "r."+a+" AS "+a)
		dims = append(dims, "r."+a)
	}
	keptColour := "CASE WHEN " + kept + " THEN r." + c + " END"
	other := "CASE WHEN " + kept + " THEN 0 ELSE 1 END"
	sel = append(sel,
		keptColour+" AS "+c,
		other+" AS "+alias(charts.OtherAlias),
		"MAX(MAX(r."+rank+")) OVER () AS "+alias(charts.SeriesCountAlias))
	return r, sel, append(dims, keptColour, other)
}

// pivotField is one of a pivot table's row or column fields: its expression, repeated
// wherever it is grouped (SQL Server and Oracle refuse an alias in GROUP BY, § 14.3),
// its result name and its total flag's.
type pivotField struct{ expr, alias, flag string }

// pivotParts are a pivot table's fields, its row fields first, and its measures'
// expressions and result names.
type pivotParts struct {
	fields                   []pivotField
	rows                     int
	measures, measureAliases []string
}

// pivotSQL is a pivot table's cells and totals in one statement (dashboards-design.md
// § 14.3): a grouping set for each prefix of its row fields (none to all) with each
// prefix of its column fields, at most 4 × 3, finest first. Each measure is computed
// over its set's own rows, so an average's total is the source's average, never an
// average of averages (Metabase's lesson, § 14.1). Each field's flag is 1 on a row
// where that field is totalled, so a total is told from a NULL value. The outer query
// orders the totals first, the most totalled first, then the cells by their fields,
// so a pivot cut at the row cap keeps every total, unless the totals alone reach the
// cap: then no cell is kept and the last totals are cut (the page says so). The page
// marks a value past the cut by this order (saved-chart.ts pivotCells): the sets the
// fewer fields held first and, as many held, the fewer row fields first, then by the
// fields; testdata/chart_sql/pivot_*.golden pins it.
//
// MySQL (and MariaDB) have no GROUPING SETS, only WITH ROLLUP, which cannot cross the
// row fields' totals with the column fields': there each set is its own GROUP BY, the
// sets joined by UNION ALL, an absent field NULL and its flag 1, and the source read
// once a set. bindWhere binds the filters where each WHERE falls in the text.
func pivotSQL(d string, p pivotParts, from string, bindWhere func() []string) (string, []string, []string) {
	rows, cols := p.fields[:p.rows], p.fields[p.rows:]
	var sets [][]pivotField
	for i := len(rows); i >= 0; i-- {
		for j := len(cols); j >= 0; j-- {
			sets = append(sets, append(append([]pivotField{}, rows[:i]...), cols[:j]...))
		}
	}
	measures := make([]string, len(p.measures))
	for i, m := range p.measures {
		measures[i] = m + " AS " + p.measureAliases[i]
	}
	var inner string
	if d == "mysql" {
		branches := make([]string, len(sets))
		for n, set := range sets {
			in := map[string]bool{}
			var group []string
			for _, f := range set {
				in[f.alias] = true
				group = append(group, f.expr)
			}
			var sel, flags []string
			for _, f := range p.fields {
				if in[f.alias] {
					sel = append(sel, f.expr+" AS "+f.alias)
					flags = append(flags, "0 AS "+f.flag)
				} else {
					sel = append(sel, "NULL AS "+f.alias)
					flags = append(flags, "1 AS "+f.flag)
				}
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "SELECT %s\nFROM %s", strings.Join(append(append(sel, measures...), flags...), ", "), from)
			if where := bindWhere(); len(where) > 0 {
				sb.WriteString("\nWHERE " + strings.Join(where, " AND "))
			}
			if len(group) > 0 {
				sb.WriteString("\nGROUP BY " + strings.Join(group, ", "))
			}
			branches[n] = sb.String()
		}
		inner = strings.Join(branches, "\nUNION ALL\n")
	} else {
		var sel, flags, groupingSets []string
		for _, f := range p.fields {
			sel = append(sel, f.expr+" AS "+f.alias)
			flags = append(flags, "GROUPING("+f.expr+") AS "+f.flag)
		}
		for _, set := range sets {
			exprs := make([]string, len(set))
			for i, f := range set {
				exprs[i] = f.expr
			}
			groupingSets = append(groupingSets, "("+strings.Join(exprs, ", ")+")")
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "SELECT %s\nFROM %s", strings.Join(append(append(sel, measures...), flags...), ", "), from)
		if where := bindWhere(); len(where) > 0 {
			sb.WriteString("\nWHERE " + strings.Join(where, " AND "))
		}
		sb.WriteString("\nGROUP BY GROUPING SETS (" + strings.Join(groupingSets, ", ") + ")")
		inner = sb.String()
	}

	var sel, flagSum, flagOrder, fieldOrder []string
	for _, f := range p.fields {
		sel = append(sel, "p."+f.alias)
		flagSum = append(flagSum, "p."+f.flag)
		flagOrder = append(flagOrder, "p."+f.flag+" DESC")
		fieldOrder = append(fieldOrder, "p."+f.alias+" ASC")
	}
	for _, a := range p.measureAliases {
		sel = append(sel, "p."+a)
	}
	sel = append(sel, flagSum...)
	var order []string
	if len(flagSum) > 1 {
		order = append(order, strings.Join(flagSum, " + ")+" DESC")
	}
	order = append(append(order, flagOrder...), fieldOrder...)
	return "(\n" + inner + "\n) p", sel, order
}

// writeSourceProbeSQL is the source run once to read its columns (§ 3.2 rule 2): the
// same FROM item a chart reads, one row.
func writeSourceProbeSQL(src chartSource) string {
	switch src.Dialect {
	case "sqlserver":
		return "SELECT TOP (1) * FROM " + src.fromClause()
	case "oracle":
		return "SELECT * FROM " + src.fromClause() + "\nFETCH FIRST 1 ROWS ONLY"
	default:
		return "SELECT * FROM " + src.fromClause() + "\nLIMIT 1"
	}
}

// writeFilterValuesSQL lists up to limit distinct values of one column of the source,
// for a value-list filter's choices. SQL Server's TOP is left to the executor's
// clampLimitSQLServer, which puts it after SELECT DISTINCT; writing it here as well
// would give the statement two.
func writeFilterValuesSQL(src chartSource, column string, limit int) string {
	c := "src." + quoteCheckColumn(src.Dialect, column)
	a := quoteCheckColumn(src.Dialect, column)
	switch src.Dialect {
	case "sqlserver":
		return fmt.Sprintf("SELECT DISTINCT %s AS %s FROM %s\nWHERE %s IS NOT NULL\nORDER BY %s", c, a, src.fromClause(), c, c)
	case "oracle":
		return fmt.Sprintf("SELECT DISTINCT %s AS %s FROM %s\nWHERE %s IS NOT NULL\nORDER BY %s\nFETCH FIRST %d ROWS ONLY", c, a, src.fromClause(), c, c, limit)
	default:
		return fmt.Sprintf("SELECT DISTINCT %s AS %s FROM %s\nWHERE %s IS NOT NULL\nORDER BY %s\nLIMIT %d", c, a, src.fromClause(), c, c, limit)
	}
}

// statements lists the statements a chart runs after its main one: a scorecard's
// sparkline.
func (q *chartQuery) statements() []*chartQuery {
	if q.Spark != nil {
		return []*chartQuery{q.Spark}
	}
	return nil
}
