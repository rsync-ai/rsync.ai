package agentgw

// dashboards.go is the dashboard tools (dashboards-design.md § 7.3, § 7.6):
// list_chart_sources and describe_chart_source read the models and saved queries a
// person may chart, and plan_dashboard drafts a dashboard the person saves in rsync.ai.
// list_dashboards and get_dashboard read the dashboards a person may see, each tile's
// place and its chart's settings, and plan_dashboard_change drafts a change to one tile
// the person applies in rsync.ai. Like every tool here they return metadata only: a
// source's name and its columns' names and kinds, never a row, a value, a count or a
// source's SQL; a chart's settings without its series colours, which name series by
// their values; a text tile's place, never its text. No tool saves, changes or draws a
// chart or a dashboard.

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/charts"
	"api-gateway/internal/handlers"
	"api-gateway/internal/services"
)

// ---- outputs ----------------------------------------------------------------

type listChartSourcesOut struct {
	Sources []chartSourceOut `json:"sources"`
	// Truncated says the workspace has more than the first
	// handlers.ChartSourcesForModelMax sources by name.
	Truncated bool `json:"truncated"`
}

type chartSourceOut struct {
	SourceID      string `json:"source_id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Connection    string `json:"connection"`
	ConnectorType string `json:"connector_type"`
	// Chartable is false for a model never built, or a source no chart can read;
	// Reason says why.
	Chartable bool   `json:"chartable"`
	Reason    string `json:"reason,omitempty"`
}

type describeChartSourceOut struct {
	SourceID string           `json:"source_id"`
	Name     string           `json:"name"`
	Kind     string           `json:"kind"`
	Columns  []chartColumnOut `json:"columns"`
}

type chartColumnOut struct {
	Name string `json:"name"`
	// Kind is number, time, text, bool or unknown.
	Kind string `json:"kind"`
	// Masked columns are shown masked to every viewer: never measure or filter one.
	Masked bool `json:"masked"`
}

type dashboardDraftOut struct {
	ProposalID string                    `json:"proposal_id"`
	Name       string                    `json:"name"`
	Charts     []dashboardDraftChartOut  `json:"charts"`
	Filters    []dashboardDraftFilterOut `json:"filters"`
	TileCount  int                       `json:"tile_count"`
	ExpiresAt  time.Time                 `json:"expires_at"`
	URL        string                    `json:"url"`
	// Check is a fixed sentence: what the server checked.
	Check string `json:"check"`
	// Next is a fixed sentence: what to tell the person.
	Next string `json:"next"`
}

// madeResourceID is the draft plan_dashboard stored, for its audit row and for the
// assistant's dashboard card.
func (d dashboardDraftOut) madeResourceID() string { return d.ProposalID }

type dashboardDraftChartOut struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	SourceID   string `json:"source_id"`
	SourceName string `json:"source_name"`
}

type dashboardDraftFilterOut struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Column string `json:"column"`
}

type listDashboardsOut struct {
	Dashboards []dashboardItemOut `json:"dashboards"`
	// Truncated says the person may see more than the first
	// handlers.DashboardsForModelMax dashboards, the most recently saved first.
	Truncated bool `json:"truncated"`
}

type dashboardItemOut struct {
	DashboardID string    `json:"dashboard_id"`
	Name        string    `json:"name"`
	Visibility  string    `json:"visibility"`
	TileCount   int       `json:"tile_count"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type getDashboardOut struct {
	DashboardID string                    `json:"dashboard_id"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Visibility  string                    `json:"visibility"`
	Filters     []dashboardDraftFilterOut `json:"filters"`
	Tiles       []dashboardTileOut        `json:"tiles"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	URL         string                    `json:"url"`
}

type dashboardTileOut struct {
	TileID string `json:"tile_id"`
	// Kind is chart or text. A text tile's text is never returned.
	Kind string `json:"kind"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	// ChartState is ok, hidden (the chart is private to someone else), or the
	// state of a source the chart can no longer read.
	ChartState string                `json:"chart_state,omitempty"`
	ChartID    string                `json:"chart_id,omitempty"`
	ChartName  string                `json:"chart_name,omitempty"`
	SourceID   string                `json:"source_id,omitempty"`
	SourceName string                `json:"source_name,omitempty"`
	Chart      *dashboardChartSetOut `json:"chart,omitempty"`
	// SharedElsewhere counts the other dashboards with this chart on them.
	SharedElsewhere int `json:"shared_elsewhere"`
}

// dashboardChartSetOut is a chart's settings in plan_dashboard's words, so a
// change can send them back with one setting changed.
type dashboardChartSetOut struct {
	Type          string `json:"type"`
	X             string `json:"x,omitempty"`
	Grain         string `json:"grain,omitempty"`
	Y             string `json:"y,omitempty"`
	Aggregate     string `json:"aggregate,omitempty"`
	MeasureColumn string `json:"measure_column,omitempty"`
	MeasureTitle  string `json:"measure_title,omitempty"`
	NumberFormat  string `json:"number_format,omitempty"`
	Color         string `json:"color,omitempty"`
	Series        string `json:"series,omitempty"`
	Size          string `json:"size,omitempty"`
	Stack         string `json:"stack,omitempty"`
	// The second measure of a combo, a pivot table, a bullet chart (its target) or range
	// bands (the high), and a pivot's third.
	SecondAggregate     string `json:"second_aggregate,omitempty"`
	SecondMeasureColumn string `json:"second_measure_column,omitempty"`
	SecondMeasureTitle  string `json:"second_measure_title,omitempty"`
	SecondMark          string `json:"second_mark,omitempty"`
	SecondAxis          string `json:"second_axis,omitempty"`
	ThirdAggregate      string `json:"third_aggregate,omitempty"`
	ThirdMeasureColumn  string `json:"third_measure_column,omitempty"`
	ThirdMeasureTitle   string `json:"third_measure_title,omitempty"`
	Bins                int    `json:"bins,omitempty"`
	BinWidth            string `json:"bin_width,omitempty"`
	Target              string `json:"target,omitempty"`
	Map                 string `json:"map,omitempty"`
	RowFields           string `json:"row_fields,omitempty"`
	Columns             string `json:"columns,omitempty"`
	Sort                string `json:"sort,omitempty"`
	CompareColumn       string `json:"compare_column,omitempty"`
	CompareDays         int    `json:"compare_days,omitempty"`
	Sparkline           string `json:"sparkline,omitempty"`
	Description         string `json:"description,omitempty"`
}

type dashboardChangeOut struct {
	ChangeID      string                 `json:"change_id"`
	DashboardID   string                 `json:"dashboard_id"`
	DashboardName string                 `json:"dashboard_name"`
	TileID        string                 `json:"tile_id"`
	Summary       string                 `json:"summary,omitempty"`
	Before        dashboardChangeSideOut `json:"before"`
	After         dashboardChangeSideOut `json:"after"`
	// Moved counts the tiles moved down to make room.
	Moved int `json:"moved"`
	// Copy says Apply gives this tile its own copy of the chart, because the chart is
	// also on SharedElsewhere other dashboards, which keep it as it is.
	Copy            bool      `json:"copy"`
	SharedElsewhere int       `json:"shared_elsewhere"`
	ExpiresAt       time.Time `json:"expires_at"`
	URL             string    `json:"url"`
	// Check and Next are fixed sentences, as on a draft.
	Check string `json:"check"`
	Next  string `json:"next"`
}

type dashboardChangeSideOut struct {
	ChartName string `json:"chart_name"`
	Type      string `json:"type"`
	W         int    `json:"w"`
	H         int    `json:"h"`
}

// madeResourceID is the change plan_dashboard_change stored, for its audit row and for
// the assistant's change card.
func (d dashboardChangeOut) madeResourceID() string { return d.ChangeID }

// ---- tools ------------------------------------------------------------------

// colorSynonyms are the series colours by name (dashboards-design.md § 2.1).
var colorSynonyms = map[string]string{
	"blue": "series-1", "series_1": "series-1",
	"orange": "series-2", "series_2": "series-2",
	"sky": "series-3", "sky_blue": "series-3", "light_blue": "series-3", "cyan": "series-3", "series_3": "series-3",
	"ochre": "series-4", "yellow": "series-4", "gold": "series-4", "series_4": "series-4",
	"pink": "series-5", "series_5": "series-5",
	"violet": "series-6", "purple": "series-6", "series_6": "series-6",
	"green": "series-7", "series_7": "series-7",
}

// mapSynonyms are other words for a map chart's shapes, as enumKey writes them.
var mapSynonyms = map[string]string{
	"countries": charts.MapWorld, "country": charts.MapWorld, "world_countries": charts.MapWorld,
	"us": charts.MapUSStates, "usa": charts.MapUSStates, "states": charts.MapUSStates,
	"us_state": charts.MapUSStates, "united_states": charts.MapUSStates,
}

// chartTypeSynonyms are other words for a chart type, as enumKey writes them: the
// registry's, so a new type's words reach the tool with it (dashboards-design.md § 14.2).
var chartTypeSynonyms = func() map[string]string {
	m := map[string]string{}
	for _, t := range charts.Registry {
		if t.Deprecated {
			continue
		}
		for _, w := range t.Synonyms {
			m[w] = t.Type
		}
	}
	return m
}()

var aggregateSynonyms = map[string]string{
	"distinct": charts.AggCountDistinct, "count_unique": charts.AggCountDistinct, "unique": charts.AggCountDistinct,
	"total": charts.AggSum, "average": charts.AggAvg, "mean": charts.AggAvg,
	"minimum": charts.AggMin, "maximum": charts.AggMax,
}

const (
	stackStacked    = "stacked"
	stackNotStacked = "not_stacked"
	stackPercent    = "percent"
)

var stackSynonyms = map[string]string{
	"yes": stackStacked, "true": stackStacked,
	"no": stackNotStacked, "false": stackNotStacked, "grouped": stackNotStacked, "side_by_side": stackNotStacked,
	"100": stackPercent, "100_percent": stackPercent, "percent_stacked": stackPercent, "full": stackPercent,
}

const (
	sparklineYes = "yes"
	sparklineNo  = "no"
)

var sparklineSynonyms = map[string]string{
	"true": sparklineYes, "on": sparklineYes, "show": sparklineYes,
	"false": sparklineNo, "off": sparklineNo, "hide": sparklineNo,
}

// typeMarks is how a bar, line or area chart draws its first measure beside a second.
// The other types that take a second measure (a pivot table, a bullet chart, range
// bands) draw neither measure with a mark or an axis.
var typeMarks = map[string]string{
	charts.TypeBar: charts.MarkBar, charts.TypeLine: charts.MarkLine, charts.TypeArea: charts.MarkArea,
}

var filterKindSynonyms = map[string]string{
	"date": charts.FilterDateRange, "dates": charts.FilterDateRange, "time": charts.FilterDateRange,
	"period": charts.FilterDateRange, "date_range_filter": charts.FilterDateRange,
	"value": charts.FilterValues, "list": charts.FilterValues, "dropdown": charts.FilterValues,
	"category": charts.FilterValues, "select": charts.FilterValues,
}

func dashboardChartArgs() []Arg {
	return []Arg{
		{Name: "name", Type: "string", Required: true, MaxLength: 200,
			Description: "The chart's name, such as Orders per day."},
		{Name: "source_id", Type: "string", Format: "uuid", Required: true,
			Description: "The model or saved query it draws; from list_chart_sources."},
		{Name: "type", Type: "string", Required: true, Enum: charts.Types, Synonyms: chartTypeSynonyms,
			Description: "The kind of chart; the guide lists what each type takes."},
		{Name: "x", Type: "string", MaxLength: 128,
			Description: "The x column: a heatmap's columns, waterfall's steps, funnel's stages, histogram's numbers, " +
				"sankey's sources, calendar's dates, box plot's groups, map's codes."},
		{Name: "grain", Type: "string", Enum: charts.Grains,
			Description: "Buckets a date x."},
		{Name: "y", Type: "string", MaxLength: 128,
			Description: "Heatmap: the column down its rows; scatter, bubble: the number up the y axis; sankey: the targets; " +
				"boxplot: its number; candlestick: open, high, low, close."},
		{Name: "aggregate", Type: "string", Enum: charts.Aggregates, Synonyms: aggregateSynonyms,
			Description: "How the measure is computed: count (rows), count_distinct, sum, avg, min, max."},
		{Name: "measure_column", Type: "string", MaxLength: 128,
			Description: "The column measured; left out for count."},
		{Name: "measure_title", Type: "string", MaxLength: 120,
			Description: "What the measure is called on the chart, such as Revenue."},
		{Name: "number_format", Type: "string", Enum: charts.NumberFormats,
			Description: "How the measure's numbers are written; percent for a share such as a rate (0.081 is 8.1%)."},
		{Name: "series", Type: "string", MaxLength: 128,
			Description: "bar, hbar, line, area: a series per value of this column; pie, donut: the slices; " +
				"scatter, bubble: the colour. Seven are drawn, the rest fold into Other."},
		{Name: "size", Type: "string", MaxLength: 128,
			Description: "Bubble, or a scatter: the number that sizes each point."},
		{Name: "stack", Type: "string", Enum: []string{stackStacked, stackNotStacked, stackPercent}, Synonyms: stackSynonyms,
			Description: "bar, hbar, area with a series: stack the series, or stack them to 100% (count or sum). Default not_stacked."},
		{Name: "second_aggregate", Type: "string", Enum: charts.Aggregates, Synonyms: aggregateSynonyms,
			Description: "bar, line, area with no series: a second measure on its own axis (a combo); pivot: its second; " +
				"bullet: the target; range: the high."},
		{Name: "second_measure_column", Type: "string", MaxLength: 128,
			Description: "The second measure's column."},
		{Name: "second_measure_title", Type: "string", MaxLength: 120,
			Description: "The second measure's axis title."},
		{Name: "second_mark", Type: "string", Enum: charts.Marks,
			Description: "Default line."},
		{Name: "second_axis", Type: "string", Enum: charts.Axes,
			Description: "Default right; left shares the first measure's scale."},
		{Name: "third_aggregate", Type: "string", Enum: charts.Aggregates, Synonyms: aggregateSynonyms,
			Description: "Pivot only: a third measure."},
		{Name: "third_measure_column", Type: "string", MaxLength: 128,
			Description: "The third measure's column."},
		{Name: "third_measure_title", Type: "string", MaxLength: 120,
			Description: "The third measure's title."},
		{Name: "bins", Type: "integer", Min: 0, Max: charts.MaxBins, Default: 0,
			Description: "Histogram only: 1 to 100 equal bins from the least value to the greatest; default 20."},
		{Name: "bin_width", Type: "string", MaxLength: 32,
			Description: "Histogram only, instead of bins: each bin's width, such as 25 or 0.5."},
		{Name: "target", Type: "string", MaxLength: 32,
			Description: "Bullet only, instead of a second measure: the target number, such as 1000."},
		{Name: "map", Type: "string", Enum: charts.Maps, Synonyms: mapSynonyms,
			Description: "Map only: world, x a country's ISO code (FR, FRA or 250), or us_states, x a state's (US-CA or CA). A name is not read."},
		{Name: "row_fields", Type: "string", MaxLength: 400,
			Description: "Pivot: its row fields; treemap, sunburst: its levels, outermost first. Comma-separated, 1 to 3."},
		{Name: "columns", Type: "string", MaxLength: 2000,
			Description: "Table: the columns shown, comma-separated, at most 20; pivot: its column fields, at most 2."},
		{Name: "sort", Type: "string", Enum: []string{charts.SortXAsc, charts.SortXDesc, charts.SortValueAsc, charts.SortValueDesc},
			Description: "x_asc or x_desc orders by the x column (by a table's first); value_asc or value_desc by the measure."},
		{Name: "compare_column", Type: "string", MaxLength: 128,
			Description: "Scorecard only: a date column, to compare the last compare_days days with the days before."},
		{Name: "compare_days", Type: "integer", Min: 0, Max: 365, Default: 0,
			Description: "Scorecard only, with compare_column: 1, 7, 30, 90 or 365."},
		{Name: "sparkline", Type: "string", Enum: []string{sparklineYes, sparklineNo}, Synonyms: sparklineSynonyms,
			Description: "Scorecard compared over 7 days or more: yes draws the period's trend under the number."},
		{Name: "description", Type: "string", MaxLength: 500,
			Description: "One sentence on what the chart shows."},
	}
}

func dashboardFilterArgs() []Arg {
	return []Arg{
		{Name: "label", Type: "string", Required: true, MaxLength: 60,
			Description: "What the person sees, such as Order date or Region."},
		{Name: "kind", Type: "string", Required: true, Enum: []string{charts.FilterDateRange, charts.FilterValues},
			Synonyms:    filterKindSynonyms,
			Description: "date_range filters a date column to a period; values to the values the person picks."},
		{Name: "column", Type: "string", Required: true, MaxLength: 128,
			Description: "The column it filters, by name; it applies to each chart whose source has that column."},
	}
}

func dashboardTools(h Handlers) []*Tool {
	return []*Tool{
		{
			Name:  "list_chart_sources",
			Title: "List what can be charted",
			Description: "Lists the models and saved queries the person may chart: each one's source_id, name, kind (model " +
				"or saved query), connection, and whether a chart can read it now, with the reason when not. " +
				"Rules: read_guide('dashboards').",
			Guide:        "dashboards",
			Action:       services.ActionChartRead,
			ResourceType: "chart_source",
			Output:       listChartSourcesOut{},
			run: func(call *toolCall) (any, error) {
				return listChartSources(call, h)
			},
		},
		{
			Name:  "describe_chart_source",
			Title: "Describe a chart source's columns",
			Description: "Lists a model's or saved query's columns: each one's name, kind (number, time, text, bool or " +
				"unknown) and whether it is masked. rsync.ai runs the source once, one row, to read them; the row " +
				"is never shown. Rules: read_guide('dashboards').",
			Guide: "dashboards",
			Args: []Arg{
				{Name: "source_id", Type: "string", Format: "uuid", Required: true,
					Description: "The model or saved query; from list_chart_sources."},
			},
			Action:       services.ActionChartRead,
			ResourceType: "chart_source",
			ResourceArg:  "source_id",
			Output:       describeChartSourceOut{},
			run: func(call *toolCall) (any, error) {
				return describeChartSource(call, h)
			},
		},
		{
			Name:  "plan_dashboard",
			Title: "Draft a dashboard",
			Description: "Drafts a dashboard for the person to review and save in rsync.ai: up to 12 charts, each drawing " +
				"one model or saved query from list_chart_sources, and up to 4 filters. rsync.ai checks each " +
				"chart as a save would. Rules: read_guide('dashboards').",
			Guide: "dashboards",
			Args: []Arg{
				{Name: "name", Type: "string", Required: true, MaxLength: 200,
					Description: "The dashboard's name, such as Sales overview."},
				{Name: "description", Type: "string", MaxLength: 1000,
					Description: "One or two sentences on what it is for."},
				{Name: "charts", Type: "array", Required: true, MaxItems: 12, Items: dashboardChartArgs(),
					Description: "The charts, in the order they are laid out."},
				{Name: "filters", Type: "array", MaxItems: charts.MaxFilters, Items: dashboardFilterArgs(),
					Description: "Filters above the charts, such as a date range on order_date."},
			},
			Action:       services.ActionDashboardPlan,
			Write:        true,
			ResourceType: "dashboard_proposal",
			Output:       dashboardDraftOut{},
			run: func(call *toolCall) (any, error) {
				return planDashboard(call, h)
			},
		},
		{
			Name:  "list_dashboards",
			Title: "List dashboards",
			Description: "Lists the dashboards the person may see, the most recently saved first: each one's dashboard_id, " +
				"name, who sees it (private or workspace) and how many tiles it has. Use it to find a dashboard " +
				"the person names; get_dashboard gives its tiles. Rules: read_guide('dashboards').",
			Guide:        "dashboards",
			Action:       services.ActionChartRead,
			ResourceType: "dashboard",
			Output:       listDashboardsOut{},
			run: func(call *toolCall) (any, error) {
				return listDashboards(call, h)
			},
		},
		{
			Name:  "get_dashboard",
			Title: "Read a dashboard's tiles",
			Description: "Reads one dashboard: its filters and each tile's tile_id, kind (chart or text) and place on the " +
				"12-column grid (x, y, w, h), and each chart's name, source and settings in plan_dashboard's " +
				"words. It shows no data. Rules: read_guide('dashboards').",
			Guide: "dashboards",
			Args: []Arg{
				{Name: "dashboard_id", Type: "string", Format: "uuid", Required: true,
					Description: "The dashboard; from list_dashboards."},
			},
			Action:       services.ActionChartRead,
			ResourceType: "dashboard",
			ResourceArg:  "dashboard_id",
			Output:       getDashboardOut{},
			run: func(call *toolCall) (any, error) {
				return getDashboard(call, h)
			},
		},
		{
			Name:  "plan_dashboard_change",
			Title: "Propose a change to one dashboard tile",
			Description: "Proposes a change to one chart tile of a dashboard, for the person to apply in rsync.ai: its " +
				"chart's type, columns, measure, sort, colour or name, or the tile's width or height. rsync.ai " +
				"checks it as a save would. Rules: read_guide('dashboards').",
			Guide:        "dashboards",
			Args:         dashboardChangeArgs(),
			Action:       services.ActionDashboardPlan,
			Write:        true,
			ResourceType: "dashboard_change",
			Output:       dashboardChangeOut{},
			run: func(call *toolCall) (any, error) {
				return planDashboardChange(call, h)
			},
		},
	}
}

// dashboardChangeArgs are plan_dashboard_change's arguments: the tile, the chart's
// settings as plan_dashboard takes them (its source stays), a colour and a size.
func dashboardChangeArgs() []Arg {
	args := []Arg{
		{Name: "dashboard_id", Type: "string", Format: "uuid", Required: true,
			Description: "The dashboard; from list_dashboards."},
		{Name: "tile_id", Type: "string", Format: "uuid", Required: true,
			Description: "The chart tile to change; from get_dashboard."},
	}
	for _, a := range dashboardChartArgs() {
		switch a.Name {
		case "source_id":
			continue
		case "name":
			a.Required = false
			a.Description = "The chart's new name; left out, it keeps its name."
		case "type":
			a.Required = false
			a.Description = "Send it to change the chart, with every setting it should have after; left out, the " +
				"settings stay. The types are plan_dashboard's."
		}
		args = append(args, a)
	}
	return append(args,
		Arg{Name: "color", Type: "string", Enum: charts.SeriesTokens, Synonyms: colorSynonyms,
			Description: "The measure's colour, for a chart with no series: series-1 blue, series-2 orange, " +
				"series-3 sky, series-4 ochre, series-5 pink, series-6 violet, series-7 green."},
		Arg{Name: "width", Type: "integer", Min: 0, Max: charts.GridColumns, Default: 0,
			Description: "The tile's new width in columns, 2 to 12 (12 is the whole row); 0 keeps it."},
		Arg{Name: "height", Type: "integer", Min: 0, Max: charts.MaxTileHeight, Default: 0,
			Description: "The tile's new height in rows, 2 to 24 (a scorecard at least 3); 0 keeps it."},
		Arg{Name: "summary", Type: "string", Required: true, MaxLength: 300,
			Description: "One sentence for the person: what the change does, such as Shows revenue by week as a line."},
	)
}

func listChartSources(call *toolCall, h Handlers) (any, error) {
	if h.ListChartSources == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Charts are not available on this rsync.ai."}
	}
	list, err := h.ListChartSources(call.c, call.s.opts.Actor(call.c))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := listChartSourcesOut{Sources: []chartSourceOut{}}
	if len(list) > handlers.ChartSourcesForModelMax {
		list, out.Truncated = list[:handlers.ChartSourcesForModelMax], true
	}
	for _, s := range list {
		out.Sources = append(out.Sources, chartSourceOut{
			SourceID:      s.ID,
			Name:          s.Name,
			Kind:          ident(s.Kind),
			Connection:    s.ConnectionName,
			ConnectorType: ident(s.ConnectorType),
			Chartable:     s.Chartable,
			Reason:        s.Reason,
		})
	}
	return out, nil
}

func describeChartSource(call *toolCall, h Handlers) (any, error) {
	if h.DescribeChartSource == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Charts are not available on this rsync.ai."}
	}
	src, cols, err := h.DescribeChartSource(call.c, call.s.opts.Actor(call.c), call.str("source_id"))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := describeChartSourceOut{SourceID: src.ID, Name: src.Name, Kind: ident(src.Kind), Columns: []chartColumnOut{}}
	for i, col := range cols {
		if i == maxColumns {
			break
		}
		out.Columns = append(out.Columns, chartColumnOut{Name: col.Name, Kind: ident(col.Kind), Masked: col.Masked})
	}
	return out, nil
}

func planDashboard(call *toolCall, h Handlers) (any, error) {
	if h.ProposeDashboard == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Drafting dashboards is not available on this rsync.ai."}
	}
	in := handlers.DashboardProposalInput{
		Name:        call.str("name"),
		Description: call.str("description"),
	}
	for _, item := range call.items("charts") {
		raw, err := json.Marshal(chartSettingsFrom(item))
		if err != nil {
			return nil, err
		}
		in.Charts = append(in.Charts, handlers.DashboardProposalChartInput{
			Name: itemStr(item, "name"), SourceID: itemStr(item, "source_id"), Settings: raw,
		})
	}
	for _, item := range call.items("filters") {
		in.Filters = append(in.Filters, handlers.DashboardProposalFilterInput{
			Label: itemStr(item, "label"), Kind: itemStr(item, "kind"), Column: itemStr(item, "column"),
		})
	}
	actor := call.s.opts.Actor(call.c)
	p, err := h.ProposeDashboard(call.c, actor, in)
	if err != nil {
		return nil, workflowRefusal(err)
	}
	return shapeDashboardDraft(*p, actor.Channel), nil
}

// chartSettingsFrom builds a chart's settings from one charts[] item. It sets only
// what the item names, so the server's checks refuse a setting the type does not draw
// rather than this guessing which the model meant. It never sets series colours: they
// pin a colour to a series by its value, which a model is never shown.
func chartSettingsFrom(item map[string]any) charts.Settings {
	s := charts.Settings{
		Version:     charts.Version,
		Roles:       map[string][]charts.Field{},
		Type:        itemStr(item, "type"),
		Description: itemStr(item, "description"),
		Sort:        itemStr(item, "sort"),
		Stacked:     itemStr(item, "stack") == stackStacked || itemStr(item, "stack") == stackPercent,
		Percent:     itemStr(item, "stack") == stackPercent,
		Sparkline:   itemStr(item, "sparkline") == sparklineYes,
		Map:         itemStr(item, "map"),
	}
	add := func(role string, f charts.Field) { s.Roles[role] = append(s.Roles[role], f) }
	if x := itemStr(item, "x"); x != "" || itemStr(item, "grain") != "" {
		add(charts.RoleX, charts.Field{Column: x, Grain: itemStr(item, "grain"), Bins: itemNum(item, "bins"),
			Width: binWidthFrom(itemStr(item, "bin_width"))})
	}
	if y := itemStr(item, "y"); y != "" {
		if s.Type == charts.TypeCandlestick {
			// A candlestick's four prices are one y role; a count other than four is the
			// server's to refuse.
			for _, c := range splitNames(y) {
				add(charts.RoleY, charts.Field{Column: c})
			}
		} else {
			add(charts.RoleY, charts.Field{Column: y})
		}
	}
	if sr := itemStr(item, "series"); sr != "" {
		add(charts.RoleSeries, charts.Field{Column: sr})
	}
	if sz := itemStr(item, "size"); sz != "" {
		add(charts.RoleSize, charts.Field{Column: sz})
	}
	for _, r := range splitNames(itemStr(item, "row_fields")) {
		add(charts.RoleRows, charts.Field{Column: r})
	}
	for _, c := range splitNames(itemStr(item, "columns")) {
		add(charts.RoleColumns, charts.Field{Column: c})
	}
	agg, col := itemStr(item, "aggregate"), itemStr(item, "measure_column")
	if agg != "" || col != "" {
		add(charts.RoleMeasure, charts.Field{Aggregate: agg, Column: col,
			Title: itemStr(item, "measure_title"), Format: itemStr(item, "number_format")})
	}
	if agg2, col2 := itemStr(item, "second_aggregate"), itemStr(item, "second_measure_column"); agg2 != "" || col2 != "" {
		second := charts.Field{Aggregate: agg2, Column: col2, Title: itemStr(item, "second_measure_title"),
			Mark: itemStr(item, "second_mark"), Axis: itemStr(item, "second_axis")}
		// Each measure of a combo takes a mark and an axis: the first the chart's own, on
		// the left; the second a line on the right unless the item says otherwise. The
		// measures of the other types take neither.
		if typeMarks[s.Type] != "" {
			if ms := s.Roles[charts.RoleMeasure]; len(ms) == 1 {
				ms[0].Mark, ms[0].Axis = typeMarks[s.Type], charts.AxisLeft
			}
			if second.Mark == "" {
				second.Mark = charts.MarkLine
			}
			if second.Axis == "" {
				second.Axis = charts.AxisRight
			}
		}
		add(charts.RoleMeasure, second)
	}
	if agg3, col3 := itemStr(item, "third_aggregate"), itemStr(item, "third_measure_column"); agg3 != "" || col3 != "" {
		add(charts.RoleMeasure, charts.Field{Aggregate: agg3, Column: col3, Title: itemStr(item, "third_measure_title")})
	}
	if t, ok := targetFrom(itemStr(item, "target")); ok {
		s.Target = &t
	}
	if c := itemStr(item, "compare_column"); c != "" || itemNum(item, "compare_days") != 0 {
		if c != "" {
			add(charts.RoleDate, charts.Field{Column: c})
		}
		s.Compare = &charts.Compare{Days: itemNum(item, "compare_days")}
	}
	return s
}

// binWidthFrom reads bin_width. A width that is not a number, or is not finite, is
// sent as -1 so the server's check refuses it in its own words.
func binWidthFrom(raw string) float64 {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	w, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(w) || math.IsInf(w, 0) {
		return -1
	}
	return w
}

// targetFrom reads target. A target that is not a finite number is sent as the
// largest float, which the server's check refuses in its own words; JSON cannot carry
// a NaN or an infinity to it.
func targetFrom(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	t, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(t) || math.IsInf(t, 0) {
		return math.MaxFloat64, true
	}
	return t, true
}

func shapeDashboardDraft(p handlers.DashboardProposal, channel services.Channel) dashboardDraftOut {
	out := dashboardDraftOut{
		ProposalID: p.ID,
		Name:       p.Name,
		Charts:     []dashboardDraftChartOut{},
		Filters:    []dashboardDraftFilterOut{},
		TileCount:  len(p.Tiles),
		ExpiresAt:  p.ExpiresAt,
		URL:        p.URL,
		Check: "Each chart's settings passed the checks a save runs, and its query ran once and returned " +
			"every column it names.",
	}
	for _, c := range p.Charts {
		out.Charts = append(out.Charts, dashboardDraftChartOut{
			Name: c.Name, Type: ident(c.Type), SourceID: c.SourceID, SourceName: c.SourceName,
		})
	}
	for _, f := range p.Filters {
		out.Filters = append(out.Filters, dashboardDraftFilterOut{
			ID: f.ID, Label: f.Label, Kind: ident(f.Kind), Column: f.Column,
		})
	}
	if channel == services.ChannelAssistant {
		out.Next = "Nothing is saved yet. The person sees this draft as a card under your answer, where they open " +
			"it, see each chart drawn, choose who sees it and click Save. Tell them that; only they can save it."
	} else {
		out.Next = "Nothing is saved yet. Give the person the url: within 24 hours they open it in rsync.ai, see " +
			"each chart drawn, choose who sees it and click Save. Only they can; no tool can."
	}
	return out
}

func listDashboards(call *toolCall, h Handlers) (any, error) {
	if h.ListDashboards == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Dashboards are not available on this rsync.ai."}
	}
	list, err := h.ListDashboards(call.c, call.s.opts.Actor(call.c))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := listDashboardsOut{Dashboards: []dashboardItemOut{}}
	if len(list) > handlers.DashboardsForModelMax {
		list, out.Truncated = list[:handlers.DashboardsForModelMax], true
	}
	for _, d := range list {
		out.Dashboards = append(out.Dashboards, dashboardItemOut{
			DashboardID: d.ID, Name: d.Name, Visibility: ident(d.Visibility), TileCount: d.TileCount, UpdatedAt: d.UpdatedAt,
		})
	}
	return out, nil
}

func getDashboard(call *toolCall, h Handlers) (any, error) {
	if h.GetDashboard == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Dashboards are not available on this rsync.ai."}
	}
	d, err := h.GetDashboard(call.c, call.s.opts.Actor(call.c), call.str("dashboard_id"))
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := getDashboardOut{
		DashboardID: d.ID, Name: d.Name, Description: d.Description, Visibility: ident(d.Visibility),
		Filters: []dashboardDraftFilterOut{}, Tiles: []dashboardTileOut{}, UpdatedAt: d.UpdatedAt,
		URL: handlers.DashboardURL(d.ID),
	}
	for _, f := range d.Filters {
		out.Filters = append(out.Filters, dashboardDraftFilterOut{ID: f.ID, Label: f.Label, Kind: ident(f.Kind), Column: f.Column})
	}
	for _, t := range d.Tiles {
		o := dashboardTileOut{TileID: t.ID, Kind: ident(t.Kind), X: t.X, Y: t.Y, W: t.W, H: t.H}
		if t.Kind == charts.TileChart {
			o.ChartState = ident(t.ChartState)
			o.ChartID, o.ChartName, o.SourceID, o.SourceName = t.ChartID, t.ChartName, t.SourceID, t.SourceName
			o.SharedElsewhere = t.SharedElsewhere
			if t.Settings != nil {
				o.Chart = chartSettingsOut(*t.Settings)
			}
		}
		out.Tiles = append(out.Tiles, o)
	}
	return out, nil
}

// chartSettingsOut is s in plan_dashboard's words. It leaves out the series colours,
// which pin a colour to a series by its value, and the axis titles, which no tool sets.
func chartSettingsOut(s charts.Settings) *dashboardChartSetOut {
	out := &dashboardChartSetOut{Type: ident(s.Type), Sort: ident(s.Sort),
		RowFields: strings.Join(s.ColumnsOf(charts.RoleRows), ", "),
		Columns:   strings.Join(s.ColumnsOf(charts.RoleColumns), ", "), Description: s.Description}
	if x := s.One(charts.RoleX); x != nil {
		out.X, out.Grain = x.Column, ident(x.Grain)
	}
	// A candlestick's four prices are its y columns.
	out.Y = strings.Join(s.ColumnsOf(charts.RoleY), ", ")
	if x := s.One(charts.RoleX); x != nil {
		out.Bins = x.Bins
		if x.Width > 0 {
			out.BinWidth = strconv.FormatFloat(x.Width, 'f', -1, 64)
		}
	}
	if s.Target != nil {
		out.Target = strconv.FormatFloat(*s.Target, 'f', -1, 64)
	}
	out.Map = ident(s.Map)
	if m := s.One(charts.RoleMeasure); m != nil {
		out.Aggregate, out.MeasureColumn, out.MeasureTitle = ident(m.Aggregate), m.Column, m.Title
		out.NumberFormat, out.Color = ident(m.Format), ident(m.Color)
	}
	if ms := s.Roles[charts.RoleMeasure]; len(ms) > 1 {
		m := ms[1]
		out.SecondAggregate, out.SecondMeasureColumn, out.SecondMeasureTitle = ident(m.Aggregate), m.Column, m.Title
		out.SecondMark, out.SecondAxis = ident(m.Mark), ident(m.Axis)
	}
	if ms := s.Roles[charts.RoleMeasure]; len(ms) > 2 {
		m := ms[2]
		out.ThirdAggregate, out.ThirdMeasureColumn, out.ThirdMeasureTitle = ident(m.Aggregate), m.Column, m.Title
	}
	if sz := s.One(charts.RoleSize); sz != nil {
		out.Size = sz.Column
	}
	if sr := s.One(charts.RoleSeries); sr != nil {
		out.Series = sr.Column
		switch {
		case s.Percent:
			out.Stack = stackPercent
		case s.Stacked:
			out.Stack = stackStacked
		case typeStacks(s.Type):
			out.Stack = stackNotStacked
		}
	}
	if c := s.Compare; c != nil {
		out.CompareDays = c.Days
		if d := s.One(charts.RoleDate); d != nil {
			out.CompareColumn = d.Column
		}
		if s.Sparkline {
			out.Sparkline = sparklineYes
		}
	}
	return out
}

// typeStacks says the chart type takes stack, so a pie's slices say no stack.
func typeStacks(t string) bool {
	spec := charts.Spec(t)
	return spec != nil && spec.Stackable
}

func planDashboardChange(call *toolCall, h Handlers) (any, error) {
	if h.ProposeDashboardChange == nil {
		return nil, &toolError{status: http.StatusServiceUnavailable, outcome: "unavailable",
			message: "Changing dashboards is not available on this rsync.ai."}
	}
	in := handlers.DashboardChangeInput{
		DashboardID: call.str("dashboard_id"),
		TileID:      call.str("tile_id"),
		Name:        call.str("name"),
		Color:       call.str("color"),
		W:           call.num("width"),
		H:           call.num("height"),
		Summary:     call.str("summary"),
	}
	if call.str("type") != "" {
		raw, err := json.Marshal(chartSettingsFrom(call.args))
		if err != nil {
			return nil, err
		}
		in.Settings = raw
	} else {
		for _, a := range dashboardChartArgs() {
			if a.Name == "name" || a.Name == "type" || a.Name == "source_id" {
				continue
			}
			if v, ok := call.args[a.Name]; ok && v != "" && v != 0 {
				return nil, argError("%s changes the chart: send type with every setting the chart should have, "+
					"as get_dashboard shows them with yours changed", a.Name)
			}
		}
	}
	actor := call.s.opts.Actor(call.c)
	ch, err := h.ProposeDashboardChange(call.c, actor, in)
	if err != nil {
		return nil, workflowRefusal(err)
	}
	out := dashboardChangeOut{
		ChangeID: ch.ID, DashboardID: ch.DashboardID, DashboardName: ch.DashboardName, TileID: ch.TileID,
		Summary: ch.Summary,
		Before:  dashboardChangeSideOut{ChartName: ch.Before.Name, Type: ident(ch.Before.Type), W: ch.Before.W, H: ch.Before.H},
		After:   dashboardChangeSideOut{ChartName: ch.After.Name, Type: ident(ch.After.Type), W: ch.After.W, H: ch.After.H},
		Moved:   len(ch.Moves), Copy: ch.Copy, SharedElsewhere: ch.SharedElsewhere,
		ExpiresAt: ch.ExpiresAt, URL: ch.URL,
	}
	if ch.ChartChanged {
		out.Check = "The chart's settings passed the checks a save runs, and its query ran once and returned every " +
			"column it names; the tile fits the grid."
	} else {
		out.Check = "The tile fits the grid; the chart is unchanged."
	}
	if actor.Channel == services.ChannelAssistant {
		out.Next = "Nothing has changed yet. The person sees this change as a card under your answer, with the tile " +
			"before and after, and clicks Apply within 24 hours. Tell them that; only they can apply it."
	} else {
		out.Next = "Nothing has changed yet. Give the person the url: within 24 hours they open it in rsync.ai, " +
			"compare the tile before and after and click Apply. Only they can; no tool can."
	}
	if out.Copy {
		out.Next += " The chart is also on another dashboard, so Apply gives this tile its own copy and the other " +
			"dashboard keeps the chart as it is; say so."
	}
	return out, nil
}
