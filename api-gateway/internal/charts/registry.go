package charts

// The chart type registry (dashboards-design.md § 14.2): one entry per type, holding the
// roles its settings fill, how its rows are read, its caps, and whether the server can
// draw it for an email or a PDF. validateShape, the SQL, and the LLM tool's enum and
// synonyms read it; frontend/src/components/charts/chart-registry.ts mirrors it for the
// drawing and the builder, and shared/chart_types_golden.json pins the two to each other.

// Roles a chart's fields fill (settings v2's "roles" keys).
const (
	RoleX       = "x"
	RoleY       = "y"
	RoleSize    = "size"
	RoleSeries  = "series"
	RoleRows    = "rows"
	RoleColumns = "columns"
	RoleMeasure = "measure"
	RoleDate    = "date"
)

// roleOrder is every role in the order the settings name their source columns.
var roleOrder = []string{RoleX, RoleY, RoleSize, RoleSeries, RoleRows, RoleColumns, RoleMeasure, RoleDate}

// Field attributes a role may set.
const (
	AttrColumn    = "column"
	AttrAggregate = "aggregate"
	AttrGrain     = "grain"
	AttrTitle     = "title"
	AttrFormat    = "format"
	AttrColor     = "color"
	AttrColors    = "colors"
	AttrMark      = "mark"
	AttrAxis      = "axis"
	AttrBins      = "bins"
	AttrWidth     = "width"
)

// Column kinds a role takes, as the source probe names them; KindAny takes every kind.
const (
	KindAny    = "any"
	KindNumber = "number"
	KindTime   = "time"
	KindText   = "text"
)

// Query paths (§ 14.3): grouped on the engine, the rows as they are, or a statistic the
// engine computes over every row (a histogram's bins).
const (
	PathAggregate  = "aggregate"
	PathRows       = "rows"
	PathStatistics = "statistics"
)

// RoleSpec is one role of a type: the fewest and most fields it takes, the column kinds
// it takes, and the attributes each field may set.
type RoleSpec struct {
	Name  string   `json:"name"`
	Min   int      `json:"min"`
	Max   int      `json:"max"`
	Kinds []string `json:"kinds"`
	Attrs []string `json:"attrs"`
}

// Cap is what a type draws at most: Series series, past which the rest are one Other
// (0: no series), and Rows rows, past which the first Rows are drawn and the chart says
// so. The SQL asks for one row more, to know.
type Cap struct {
	Series int `json:"series"`
	Rows   int `json:"rows"`
}

// TypeSpec is one chart type. Synonyms (as the LLM tool's enumKey writes them) and
// Description (the dashboards guide's list of types) are for the LLM tool only and stay
// out of the shared golden.
type TypeSpec struct {
	Type       string     `json:"type"`
	Label      string     `json:"label"`
	Roles      []RoleSpec `json:"roles"`
	Path       string     `json:"path"`
	Cap        Cap        `json:"cap"`
	Static     bool       `json:"static"`
	Deprecated bool       `json:"deprecated"`
	Version    int        `json:"version"`
	Stackable  bool       `json:"stackable"`
	Sorts      []string   `json:"sorts"`
	Compare    bool       `json:"compare"`
	// Aggregates are the ones its measure may use: every one, or for a chart whose marks
	// are parts of a whole (a pie's slices, a waterfall's steps), those that add up.
	Aggregates  []string `json:"aggregates"`
	Synonyms    []string `json:"-"`
	Description string   `json:"-"`
}

// Role returns the type's spec for a role, or nil when the type has no such role.
func (t *TypeSpec) Role(name string) *RoleSpec {
	for i := range t.Roles {
		if t.Roles[i].Name == name {
			return &t.Roles[i]
		}
	}
	return nil
}

var (
	allSorts  = []string{SortXAsc, SortXDesc, SortValueAsc, SortValueDesc}
	axisAttrs = []string{AttrColumn, AttrTitle, AttrGrain}
	measure   = RoleSpec{Name: RoleMeasure, Min: 1, Max: 1, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat, AttrColor}}
	// A combo: a second measure, each drawn as a bar, a line or an area against the left
	// or the right axis (decision 14: two measures at most, both axes titled).
	comboMeasure = RoleSpec{Name: RoleMeasure, Min: 1, Max: 2, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat, AttrColor, AttrMark, AttrAxis}}
	// The measure of a chart whose marks take their colours from elsewhere: a pie's
	// slices are the series' colours, a waterfall's steps up and down.
	partMeasure = RoleSpec{Name: RoleMeasure, Min: 1, Max: 1, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat}}
	series = RoleSpec{Name: RoleSeries, Min: 0, Max: 1, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrColors}}
	// A pie's slices: its series, which it must have.
	pieSlices = RoleSpec{Name: RoleSeries, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrColors}}
	xAxis     = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: axisAttrs}
	stages    = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrTitle}}
	binned    = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn, AttrTitle, AttrBins, AttrWidth}}
	aggCap    = Cap{Series: 7, Rows: MaxRows}
	// A scatter's or a bubble chart's points: x and y numbers, a size number (a bubble's
	// must be set), and a colour, a text field whose 7 largest values take the series
	// colours and the rest are one Other (§ 14.2).
	pointX      = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn, AttrTitle}}
	pointY      = RoleSpec{Name: RoleY, Min: 1, Max: 1, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn, AttrTitle}}
	pointSize   = RoleSpec{Name: RoleSize, Min: 0, Max: 1, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn, AttrTitle}}
	pointColour = RoleSpec{Name: RoleSeries, Min: 0, Max: 1, Kinds: []string{KindText}, Attrs: []string{AttrColumn, AttrColors}}
	// A pivot table's row and column fields, each a column grouped by its values or a date
	// grain, and its measures (§ 14.8): 1–3 row fields, 0–2 column fields, 1–3 measures.
	pivotAttrs    = []string{AttrColumn, AttrTitle, AttrGrain}
	pivotMeasures = RoleSpec{Name: RoleMeasure, Min: 1, Max: 3, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat}}
	// addsUp are the aggregates whose values add up to a whole (FoldsIntoOther's without
	// min and max): a pie of averages is not a share of anything.
	addsUp = []string{AggCount, AggSum}
	// Tier 2 (§ 14.8). A treemap's or a sunburst's levels, outermost first; a sankey's
	// source (x) and target (y); a calendar's days; a bullet's measure and its target
	// measure; a box plot's groups and the number it summarises; a candlestick's period and
	// its open, high, low and close; a range band's low and high.
	levels   = RoleSpec{Name: RoleRows, Min: 1, Max: 3, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrTitle}}
	flowFrom = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrTitle}}
	flowTo   = RoleSpec{Name: RoleY, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: []string{AttrColumn, AttrTitle}}
	days     = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindTime}, Attrs: []string{AttrColumn, AttrTitle}}
	bulletM  = RoleSpec{Name: RoleMeasure, Min: 1, Max: 2, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat, AttrColor}}
	boxed   = RoleSpec{Name: RoleY, Min: 1, Max: 1, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn, AttrTitle, AttrFormat}}
	period  = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindTime}, Attrs: axisAttrs}
	prices  = RoleSpec{Name: RoleY, Min: 4, Max: 4, Kinds: []string{KindNumber}, Attrs: []string{AttrColumn}}
	lowHigh = RoleSpec{Name: RoleMeasure, Min: 2, Max: 2, Kinds: []string{KindAny},
		Attrs: []string{AttrColumn, AttrAggregate, AttrTitle, AttrFormat}}
	// Tier 3 (§ 14.8). A map's shapes: a country's or a US state's ISO code, as text or a
	// number (a country's numeric code); which of the two is the settings' map.
	regions = RoleSpec{Name: RoleX, Min: 1, Max: 1, Kinds: []string{KindText, KindNumber}, Attrs: []string{AttrColumn, AttrTitle}}
)

// MaxLeaves is the most parts a treemap or a sunburst draws, the largest first (Power BI
// draws 3,500); CalendarDays the most days a calendar draws, the latest first.
const (
	MaxLeaves    = 3500
	CalendarDays = 366
)

// Registry lists every chart type, in the order the chart page offers them.
var Registry = []TypeSpec{
	{Type: TypeBar, Label: "Bar", Roles: []RoleSpec{xAxis, series, comboMeasure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Stackable: true, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"column", "columns", "bar_chart"},
		Description: "x and a measure, optionally a series or a second measure"},
	{Type: TypeHBar, Label: "Horizontal bar", Roles: []RoleSpec{xAxis, series, measure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Stackable: true, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"horizontal_bar", "horizontal", "row"},
		Description: "x and a measure, optionally a series"},
	{Type: TypeLine, Label: "Line", Roles: []RoleSpec{xAxis, series, comboMeasure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"line_chart", "time_series", "trend"},
		Description: "x and a measure, optionally a series or a second measure"},
	{Type: TypeArea, Label: "Area", Roles: []RoleSpec{xAxis, series, comboMeasure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Stackable: true, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"area_chart", "stacked_area"},
		Description: "x and a measure, optionally a series or a second measure"},
	{Type: TypePie, Label: "Pie", Roles: []RoleSpec{pieSlices, partMeasure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Sorts: []string{}, Aggregates: addsUp,
		Synonyms:    []string{"pie_chart", "share"},
		Description: "a series (the slices) and a count or a sum"},
	{Type: TypeDonut, Label: "Donut", Roles: []RoleSpec{pieSlices, partMeasure}, Path: PathAggregate, Cap: aggCap,
		Static: true, Version: Version, Sorts: []string{}, Aggregates: addsUp,
		Synonyms:    []string{"donut_chart", "doughnut", "ring"},
		Description: "a series (the slices) and a count or a sum"},
	{Type: TypeWaterfall, Label: "Waterfall", Roles: []RoleSpec{xAxis, partMeasure}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: allSorts, Aggregates: addsUp,
		Synonyms:    []string{"waterfall_chart", "bridge"},
		Description: "x (the steps) and a count or a sum, then a total bar"},
	{Type: TypeFunnel, Label: "Funnel", Roles: []RoleSpec{stages, measure}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"funnel_chart", "conversion", "stages"},
		Description: "x (the stages) and a measure, largest first"},
	{Type: TypeHistogram, Label: "Histogram", Roles: []RoleSpec{binned}, Path: PathStatistics, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: []string{},
		Synonyms:    []string{"distribution", "histogram_chart", "bins"},
		Description: "x, a number column, its rows counted in bins"},
	{Type: TypeHeatmap, Label: "Heatmap", Roles: []RoleSpec{xAxis, {Name: RoleY, Min: 1, Max: 1, Kinds: []string{KindAny}, Attrs: axisAttrs}, measure},
		Path: PathAggregate, Cap: Cap{Rows: MaxRows}, Static: true, Version: Version, Sorts: []string{SortXAsc, SortXDesc}, Aggregates: Aggregates,
		Synonyms:    []string{"heat_map", "matrix"},
		Description: "x, y and a measure"},
	{Type: TypeScatter, Label: "Scatter", Roles: []RoleSpec{pointX, pointY, pointSize, pointColour}, Path: PathRows,
		Cap: Cap{Series: 7, Rows: MaxRows}, Static: true, Version: Version, Sorts: []string{}, Aggregates: []string{},
		Synonyms:    []string{"scatter_plot", "scatter_chart", "xy", "correlation", "points"},
		Description: "x and y, two number columns, a point a distinct x and y, its rows counted; optionally a size and a colour"},
	{Type: TypeBubble, Label: "Bubble", Roles: []RoleSpec{pointX, pointY, {Name: RoleSize, Min: 1, Max: 1, Kinds: pointSize.Kinds, Attrs: pointSize.Attrs}, pointColour},
		Path: PathRows, Cap: Cap{Series: 7, Rows: MaxRows}, Static: true, Version: Version, Sorts: []string{}, Aggregates: []string{},
		Synonyms:    []string{"bubble_chart", "bubbles"},
		Description: "x, y and size, three number columns, a bubble a distinct x, y and size, its rows counted; optionally a colour"},
	{Type: TypeScorecard, Label: "Scorecard", Roles: []RoleSpec{measure, {Name: RoleDate, Min: 0, Max: 1, Kinds: []string{KindTime}, Attrs: []string{AttrColumn}}},
		Path: PathAggregate, Cap: Cap{Rows: MaxRows}, Static: true, Version: Version, Sorts: []string{}, Compare: true, Aggregates: Aggregates,
		Synonyms:    []string{"kpi", "number", "big_number", "metric", "stat"},
		Description: "one number, a measure"},
	{Type: TypeTable, Label: "Table", Roles: []RoleSpec{{Name: RoleColumns, Min: 1, Max: maxTableCols, Kinds: []string{KindAny}, Attrs: []string{AttrColumn}},
		{Name: RoleMeasure, Min: 0, Max: 1, Kinds: measure.Kinds, Attrs: measure.Attrs}},
		Path: PathAggregate, Cap: Cap{Rows: MaxRows}, Static: true, Version: Version, Sorts: allSorts, Aggregates: Aggregates,
		Synonyms:    []string{"grid", "list"},
		Description: "columns"},
	{Type: TypePivot, Label: "Pivot table", Roles: []RoleSpec{{Name: RoleRows, Min: 1, Max: 3, Kinds: []string{KindAny}, Attrs: pivotAttrs},
		{Name: RoleColumns, Min: 0, Max: 2, Kinds: []string{KindAny}, Attrs: pivotAttrs}, pivotMeasures},
		Path: PathAggregate, Cap: Cap{Rows: MaxRows}, Static: true, Version: Version, Sorts: []string{}, Aggregates: Aggregates,
		Synonyms:    []string{"pivot_table", "crosstab", "cross_tab", "matrix_table"},
		Description: "rows (1-3 fields) and columns (0-2 fields) with 1-3 measures, and their totals"},
	{Type: TypeTreemap, Label: "Treemap", Roles: []RoleSpec{levels, partMeasure}, Path: PathAggregate, Cap: Cap{Rows: MaxLeaves},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: addsUp,
		Synonyms:    []string{"tree_map", "treemap_chart"},
		Description: "rows (1-3 levels) and a count or a sum, a rectangle a part"},
	{Type: TypeSunburst, Label: "Sunburst", Roles: []RoleSpec{levels, partMeasure}, Path: PathAggregate, Cap: Cap{Rows: MaxLeaves},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: addsUp,
		Synonyms:    []string{"sunburst_chart", "rings"},
		Description: "rows (1-3 levels) and a count or a sum, a ring a level"},
	{Type: TypeSankey, Label: "Sankey", Roles: []RoleSpec{flowFrom, flowTo, partMeasure}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: addsUp,
		Synonyms:    []string{"sankey_diagram", "flow", "flows"},
		Description: "x (the sources), y (the targets) and a count or a sum, a band a flow"},
	{Type: TypeCalendar, Label: "Calendar heatmap", Roles: []RoleSpec{days, partMeasure}, Path: PathAggregate, Cap: Cap{Rows: CalendarDays},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: Aggregates,
		Synonyms:    []string{"calendar_heatmap", "contribution", "contributions"},
		Description: "x, a date column, and a measure, a square a day over the latest year"},
	{Type: TypeBullet, Label: "Bullet", Roles: []RoleSpec{bulletM}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: Aggregates,
		Synonyms:    []string{"bullet_chart", "target", "goal"},
		Description: "a measure against a target: a second measure or a number"},
	{Type: TypeBoxplot, Label: "Box plot", Roles: []RoleSpec{xAxis, boxed}, Path: PathStatistics, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{SortXAsc, SortXDesc}, Aggregates: []string{},
		Synonyms:    []string{"box_plot", "box_and_whisker", "whisker", "quartiles"},
		Description: "x (the groups) and y, a number column: each group's least, quartiles and greatest"},
	{Type: TypeCandlestick, Label: "Candlestick", Roles: []RoleSpec{period, prices}, Path: PathStatistics, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: []string{},
		Synonyms:    []string{"ohlc", "candle", "candles", "stock"},
		Description: "x, a date column, and y, four number columns: open, high, low and close"},
	{Type: TypeRange, Label: "Range bands", Roles: []RoleSpec{xAxis, lowHigh}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{SortXAsc, SortXDesc}, Aggregates: Aggregates,
		Synonyms:    []string{"range_band", "band", "bands", "error_band", "min_max"},
		Description: "x and two measures, a low and a high, the band between them"},
	{Type: TypeMap, Label: "Choropleth map", Roles: []RoleSpec{regions, measure}, Path: PathAggregate, Cap: Cap{Rows: MaxRows},
		Static: true, Version: Version, Sorts: []string{}, Aggregates: Aggregates,
		Synonyms:    []string{"choropleth", "geo", "world_map", "country_map", "us_map", "state_map", "filled_map"},
		Description: "x, a column of ISO codes (a country's FR, FRA or 250, or a US state's US-CA or CA; never a name), and a measure, a shape filled a code; map is world or us_states"},
}

// NotStatic is what an email or a PDF says in place of a chart whose type the registry
// marks not static (dashboards-design.md § 14.5); chart-registry.ts NOT_STATIC says the same.
const NotStatic = "This kind of chart is drawn only in rsync.ai, not in an email or a PDF."

// Spec returns a type's registry entry, or nil for a type rsync.ai does not draw.
func Spec(chartType string) *TypeSpec {
	for i := range Registry {
		if Registry[i].Type == chartType {
			return &Registry[i]
		}
	}
	return nil
}

// Types lists every chart type a new chart may be saved as, in the order the chart page
// offers them: the registry's, without the deprecated ones.
var Types = func() []string {
	var out []string
	for _, t := range Registry {
		if !t.Deprecated {
			out = append(out, t.Type)
		}
	}
	return out
}()
