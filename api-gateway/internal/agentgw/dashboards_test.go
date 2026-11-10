package agentgw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"api-gateway/internal/charts"
	"api-gateway/internal/handlers"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// dashboardRecorder is a ProposeDashboard that keeps what it was asked.
func dashboardRecorder(got *[]handlers.DashboardProposalInput) func(*gin.Context, services.Actor, handlers.DashboardProposalInput) (*handlers.DashboardProposal, error) {
	return func(_ *gin.Context, _ services.Actor, in handlers.DashboardProposalInput) (*handlers.DashboardProposal, error) {
		*got = append(*got, in)
		return leakyDashboardProposal(in), nil
	}
}

// plan_dashboard needs the dashboards:write scope and an owner or admin: dashboards
// are saved by them (§ 10), so only they draft one. The refusal names both, and the
// store is never reached. Control: an admin token with the scope drafts, and the
// audit row names the draft it stored.
func TestPlanDashboard_NeedsScopeAndAdmin(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)

	noScope := adminActor
	noScope.Scopes = []string{services.ScopeMetadataRead, services.ScopeWorkflowsWrite}
	member := adminActor
	member.Role = security.WSMember
	for who, actor := range map[string]services.Actor{"no scope": noScope, "member": member} {
		r, audit := testServer(t, h, actor)
		res := call(t, r, "plan_dashboard", validArgs["plan_dashboard"])
		if !isError(res) || !strings.Contains(resultText(res), services.ScopeDashboardsWrite) || !strings.Contains(resultText(res), "admin") {
			t.Fatalf("%s: want a refusal naming the scope and role, got %v", who, res)
		}
		if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_dashboard", "dashboard_proposal", "", "forbidden", 403}) {
			t.Fatalf("%s: audit = %+v", who, *audit)
		}
	}
	if len(got) != 0 {
		t.Fatalf("a refused call reached the store: %+v", got)
	}

	r, audit := testServer(t, h, adminActor)
	d := data(t, call(t, r, "plan_dashboard", validArgs["plan_dashboard"]))
	if d["proposal_id"] != proposalID || d["url"] != "https://app.rsync.example/dashboard-proposals/"+proposalID ||
		d["tile_count"] != float64(2) {
		t.Fatalf("draft = %v", d)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_dashboard", "dashboard_proposal", proposalID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}
	if len(got) != 1 || len(got[0].Charts) != 2 || len(got[0].Filters) != 1 || got[0].Filters[0].Column != "order_date" {
		t.Fatalf("store got %+v", got)
	}
}

// The charts a model drafts reach the store as settings the server's own decoder
// accepts (§ 3.2 rules 1-5), with only what the model named: the words it uses for a
// type or an aggregate are the type and the aggregate, and no setting is guessed.
func TestPlanDashboard_ChartsBecomeCheckedSettings(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Revenue", "source_id": sourceID, "type": "KPI", "aggregate": "total",
			"measure_column": "total", "number_format": "compact", "compare_column": "order_date", "compare_days": 30},
		map[string]any{"name": "By region", "source_id": sourceID, "type": "horizontal bar", "x": "region",
			"aggregate": "count", "sort": "value_desc"},
		map[string]any{"name": "Orders", "source_id": sourceID, "type": "area", "x": "order_date", "grain": "month",
			"aggregate": "count", "series": "region", "stack": "yes"},
		map[string]any{"name": "Latest", "source_id": sourceID, "type": "table", "columns": " id, total ,, region"},
	}}))
	if len(got) != 1 || len(got[0].Charts) != 4 {
		t.Fatalf("store got %+v", got)
	}
	want := []string{
		`{"version":2,"type":"scorecard","roles":{"date":[{"column":"order_date"}],"measure":[{"column":"total","aggregate":"sum","format":"compact"}]},"compare":{"days":30}}`,
		`{"version":2,"type":"hbar","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"region"}]},"sort":"value_desc"}`,
		`{"version":2,"type":"area","roles":{"measure":[{"aggregate":"count"}],"series":[{"column":"region"}],"x":[{"column":"order_date","grain":"month"}]},"stacked":true}`,
		`{"version":2,"type":"table","roles":{"columns":[{"column":"id"},{"column":"total"},{"column":"region"}]}}`,
	}
	for i, c := range got[0].Charts {
		if string(c.Settings) != want[i] {
			t.Errorf("chart %d settings = %s, want %s", i+1, c.Settings, want[i])
		}
		if _, err := charts.Decode(c.Settings); err != nil {
			t.Errorf("chart %d: the server would refuse what the tool built: %v", i+1, err)
		}
	}
	// Control: a setting the type does not draw goes to the server as named, which
	// refuses it, rather than the tool dropping it.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Revenue", "source_id": sourceID, "type": "scorecard", "aggregate": "sum",
			"measure_column": "total", "x": "order_date"}}}))
	if _, err := charts.Decode(got[0].Charts[0].Settings); err == nil || !strings.Contains(err.Error(), "has no x column") {
		t.Fatalf("control: Decode(%s) = %v, want the x column refused", got[0].Charts[0].Settings, err)
	}
}

// D4b's charts in the tool's words: a combo's second measure on its own axis, a 100%
// stack, a pie's slices, a waterfall, a funnel, a histogram's bins or bin width, and a
// scorecard's sparkline, each reaching the store as settings the server accepts; and
// get_dashboard gives back the same words, each an argument plan_dashboard_change takes.
func TestPlanDashboard_D4bChartsBecomeCheckedSettings(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	items := []map[string]any{
		{"name": "Revenue and orders", "source_id": sourceID, "type": "bar", "x": "order_date", "grain": "month",
			"aggregate": "sum", "measure_column": "total", "measure_title": "Revenue",
			"second_aggregate": "count", "second_measure_title": "Orders"},
		{"name": "Mix", "source_id": sourceID, "type": "area", "x": "order_date", "aggregate": "count",
			"series": "region", "stack": "100"},
		{"name": "Share", "source_id": sourceID, "type": "pie chart", "series": "region", "aggregate": "sum",
			"measure_column": "total"},
		{"name": "Ring", "source_id": sourceID, "type": "doughnut", "series": "region", "aggregate": "count"},
		{"name": "Bridge", "source_id": sourceID, "type": "waterfall", "x": "region", "aggregate": "sum",
			"measure_column": "total"},
		{"name": "Stages", "source_id": sourceID, "type": "funnel", "x": "region", "aggregate": "count"},
		{"name": "Sizes", "source_id": sourceID, "type": "distribution", "x": "total", "bins": 10},
		{"name": "Sizes by 25", "source_id": sourceID, "type": "histogram", "x": "total", "bin_width": "25"},
		{"name": "Revenue", "source_id": sourceID, "type": "kpi", "aggregate": "sum", "measure_column": "total",
			"compare_column": "order_date", "compare_days": 30, "sparkline": "show"},
	}
	var in []any
	for _, it := range items {
		in = append(in, it)
	}
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": in}))
	if len(got) != 1 || len(got[0].Charts) != len(items) {
		t.Fatalf("store got %+v", got)
	}
	want := []string{
		`{"version":2,"type":"bar","roles":{"measure":[{"column":"total","aggregate":"sum","title":"Revenue","mark":"bar","axis":"left"},{"aggregate":"count","title":"Orders","mark":"line","axis":"right"}],"x":[{"column":"order_date","grain":"month"}]}}`,
		`{"version":2,"type":"area","roles":{"measure":[{"aggregate":"count"}],"series":[{"column":"region"}],"x":[{"column":"order_date"}]},"stacked":true,"percent":true}`,
		`{"version":2,"type":"pie","roles":{"measure":[{"column":"total","aggregate":"sum"}],"series":[{"column":"region"}]}}`,
		`{"version":2,"type":"donut","roles":{"measure":[{"aggregate":"count"}],"series":[{"column":"region"}]}}`,
		`{"version":2,"type":"waterfall","roles":{"measure":[{"column":"total","aggregate":"sum"}],"x":[{"column":"region"}]}}`,
		`{"version":2,"type":"funnel","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"region"}]}}`,
		`{"version":2,"type":"histogram","roles":{"x":[{"column":"total","bins":10}]}}`,
		`{"version":2,"type":"histogram","roles":{"x":[{"column":"total","width":25}]}}`,
		`{"version":2,"type":"scorecard","roles":{"date":[{"column":"order_date"}],"measure":[{"column":"total","aggregate":"sum"}]},"compare":{"days":30},"sparkline":true}`,
	}
	changeArgs := map[string]bool{}
	for _, a := range dashboardChangeArgs() {
		changeArgs[a.Name] = true
	}
	for i, c := range got[0].Charts {
		if string(c.Settings) != want[i] {
			t.Errorf("chart %d settings = %s, want %s", i+1, c.Settings, want[i])
			continue
		}
		s, err := charts.Decode(c.Settings)
		if err != nil {
			t.Errorf("chart %d: the server would refuse what the tool built: %v", i+1, err)
			continue
		}
		raw, _ := json.Marshal(chartSettingsOut(*s))
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		for k := range out {
			if !changeArgs[k] {
				t.Errorf("chart %d: get_dashboard returns %q, which plan_dashboard_change does not take", i+1, k)
			}
		}
		for k, v := range map[int]map[string]any{
			0: {"second_aggregate": "count", "second_measure_title": "Orders", "second_mark": "line", "second_axis": "right"},
			1: {"stack": "percent"},
			2: {"series": "region"},
			6: {"bins": float64(10)},
			7: {"bin_width": "25"},
			8: {"sparkline": "yes", "compare_column": "order_date"},
		}[i] {
			if out[k] != v {
				t.Errorf("chart %d: get_dashboard %s = %v, want %v (all: %v)", i+1, k, out[k], v, out)
			}
		}
		if i == 2 && out["stack"] != nil {
			t.Errorf("a pie says stack %v; it takes none", out["stack"])
		}
	}
	// A bin width that is not a number reaches the server as one it refuses, rather
	// than the tool dropping it and the chart drawing twenty bins.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Sizes", "source_id": sourceID, "type": "histogram", "x": "total", "bin_width": "wide"}}}))
	if _, err := charts.Decode(got[0].Charts[0].Settings); err == nil || !strings.Contains(err.Error(), "bin width") {
		t.Fatalf("Decode(%s) = %v, want the bin width refused", got[0].Charts[0].Settings, err)
	}
}

// A check that fails comes back to the model as the tool's error, with the
// handler's message, so it can fix the chart and draft again.
// D4c's charts in the tool's words: a scatter with a colour, a bubble's size, and a pivot
// table's row fields, column fields and three measures, which take no mark or axis; each
// reaches the store as settings the server accepts, and get_dashboard gives back the same
// words, each an argument plan_dashboard_change takes.
func TestPlanDashboard_D4cChartsBecomeCheckedSettings(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	items := []map[string]any{
		{"name": "Price and units", "source_id": sourceID, "type": "scatter plot", "x": "total", "y": "quantity",
			"series": "region"},
		{"name": "Margin", "source_id": sourceID, "type": "bubbles", "x": "total", "y": "quantity", "size": "discount"},
		{"name": "By region", "source_id": sourceID, "type": "crosstab", "row_fields": "region, city",
			"columns": "channel", "aggregate": "sum", "measure_column": "total", "measure_title": "Revenue",
			"second_aggregate": "avg", "second_measure_column": "total", "third_aggregate": "count",
			"third_measure_title": "Orders"},
	}
	var in []any
	for _, it := range items {
		in = append(in, it)
	}
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": in}))
	if len(got) != 1 || len(got[0].Charts) != len(items) {
		t.Fatalf("store got %+v", got)
	}
	want := []string{
		`{"version":2,"type":"scatter","roles":{"series":[{"column":"region"}],"x":[{"column":"total"}],"y":[{"column":"quantity"}]}}`,
		`{"version":2,"type":"bubble","roles":{"size":[{"column":"discount"}],"x":[{"column":"total"}],"y":[{"column":"quantity"}]}}`,
		`{"version":2,"type":"pivot","roles":{"columns":[{"column":"channel"}],"measure":[{"column":"total","aggregate":"sum","title":"Revenue"},{"column":"total","aggregate":"avg"},{"aggregate":"count","title":"Orders"}],"rows":[{"column":"region"},{"column":"city"}]}}`,
	}
	changeArgs := map[string]bool{}
	for _, a := range dashboardChangeArgs() {
		changeArgs[a.Name] = true
	}
	for i, c := range got[0].Charts {
		if string(c.Settings) != want[i] {
			t.Errorf("chart %d settings = %s, want %s", i+1, c.Settings, want[i])
			continue
		}
		s, err := charts.Decode(c.Settings)
		if err != nil {
			t.Errorf("chart %d: the server would refuse what the tool built: %v", i+1, err)
			continue
		}
		raw, _ := json.Marshal(chartSettingsOut(*s))
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		for k := range out {
			if !changeArgs[k] {
				t.Errorf("chart %d: get_dashboard returns %q, which plan_dashboard_change does not take", i+1, k)
			}
		}
		for k, v := range map[int]map[string]any{
			0: {"x": "total", "y": "quantity", "series": "region"},
			1: {"size": "discount"},
			2: {"row_fields": "region, city", "columns": "channel", "second_aggregate": "avg",
				"third_aggregate": "count", "third_measure_title": "Orders"},
		}[i] {
			if out[k] != v {
				t.Errorf("chart %d: get_dashboard %s = %v, want %v (all: %v)", i+1, k, out[k], v, out)
			}
		}
		if i == 2 && (out["second_mark"] != nil || out["second_axis"] != nil) {
			t.Errorf("a pivot's second measure says a mark or an axis: %v", out)
		}
	}
	// Control: a bar's second measure still takes its mark and axis.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Combo", "source_id": sourceID, "type": "bar", "x": "region", "aggregate": "sum",
			"measure_column": "total", "second_aggregate": "count"}}}))
	if !strings.Contains(string(got[0].Charts[0].Settings), `"mark":"line","axis":"right"`) {
		t.Fatalf("control: a combo's second measure = %s", got[0].Charts[0].Settings)
	}
}

// D4d's charts in the tool's words: a treemap's and a sunburst's levels, a sankey's
// sources and targets, a calendar's days, a bullet chart's target as a measure and as a
// number, a box plot, a candlestick's four prices and range bands' low and high. The
// second measures take no mark or axis; each reaches the store as settings the server
// accepts, and get_dashboard gives back the same words, each an argument
// plan_dashboard_change takes.
func TestPlanDashboard_D4dChartsBecomeCheckedSettings(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	items := []map[string]any{
		{"name": "Revenue by place", "source_id": sourceID, "type": "tree map", "row_fields": "region, city",
			"aggregate": "sum", "measure_column": "total"},
		{"name": "Orders by region", "source_id": sourceID, "type": "rings", "row_fields": "region", "aggregate": "count"},
		{"name": "Channel to region", "source_id": sourceID, "type": "flows", "x": "channel", "y": "region",
			"aggregate": "count"},
		{"name": "Orders a day", "source_id": sourceID, "type": "contributions", "x": "created_at", "aggregate": "count"},
		{"name": "Revenue to quota", "source_id": sourceID, "type": "bullet chart", "aggregate": "sum",
			"measure_column": "total", "second_aggregate": "sum", "second_measure_column": "quota"},
		{"name": "Customers to goal", "source_id": sourceID, "type": "goal", "aggregate": "count_distinct",
			"measure_column": "customer_id", "target": "250"},
		{"name": "Order sizes", "source_id": sourceID, "type": "box and whisker", "x": "region", "y": "total"},
		{"name": "Price a week", "source_id": sourceID, "type": "ohlc", "x": "created_at", "grain": "week",
			"y": "open_price, high_price, low_price, close_price"},
		{"name": "Order range", "source_id": sourceID, "type": "min max", "x": "created_at", "grain": "month",
			"aggregate": "min", "measure_column": "total", "second_aggregate": "max", "second_measure_column": "total"},
	}
	var in []any
	for _, it := range items {
		in = append(in, it)
	}
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": in}))
	if len(got) != 1 || len(got[0].Charts) != len(items) {
		t.Fatalf("store got %+v", got)
	}
	want := []string{
		`{"version":2,"type":"treemap","roles":{"measure":[{"column":"total","aggregate":"sum"}],"rows":[{"column":"region"},{"column":"city"}]}}`,
		`{"version":2,"type":"sunburst","roles":{"measure":[{"aggregate":"count"}],"rows":[{"column":"region"}]}}`,
		`{"version":2,"type":"sankey","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"channel"}],"y":[{"column":"region"}]}}`,
		`{"version":2,"type":"calendar","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"created_at"}]}}`,
		`{"version":2,"type":"bullet","roles":{"measure":[{"column":"total","aggregate":"sum"},{"column":"quota","aggregate":"sum"}]}}`,
		`{"version":2,"type":"bullet","roles":{"measure":[{"column":"customer_id","aggregate":"count_distinct"}]},"target":250}`,
		`{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"total"}]}}`,
		`{"version":2,"type":"candlestick","roles":{"x":[{"column":"created_at","grain":"week"}],"y":[{"column":"open_price"},{"column":"high_price"},{"column":"low_price"},{"column":"close_price"}]}}`,
		`{"version":2,"type":"range","roles":{"measure":[{"column":"total","aggregate":"min"},{"column":"total","aggregate":"max"}],"x":[{"column":"created_at","grain":"month"}]}}`,
	}
	changeArgs := map[string]bool{}
	for _, a := range dashboardChangeArgs() {
		changeArgs[a.Name] = true
	}
	for i, c := range got[0].Charts {
		if string(c.Settings) != want[i] {
			t.Errorf("chart %d settings = %s, want %s", i+1, c.Settings, want[i])
			continue
		}
		s, err := charts.Decode(c.Settings)
		if err != nil {
			t.Errorf("chart %d: the server would refuse what the tool built: %v", i+1, err)
			continue
		}
		raw, _ := json.Marshal(chartSettingsOut(*s))
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		for k := range out {
			if !changeArgs[k] {
				t.Errorf("chart %d: get_dashboard returns %q, which plan_dashboard_change does not take", i+1, k)
			}
		}
		for k, v := range map[int]map[string]any{
			0: {"row_fields": "region, city"},
			2: {"x": "channel", "y": "region"},
			4: {"second_aggregate": "sum", "second_measure_column": "quota"},
			5: {"target": "250"},
			7: {"y": "open_price, high_price, low_price, close_price", "grain": "week"},
			8: {"aggregate": "min", "second_aggregate": "max"},
		}[i] {
			if out[k] != v {
				t.Errorf("chart %d: get_dashboard %s = %v, want %v (all: %v)", i+1, k, out[k], v, out)
			}
		}
		if out["second_mark"] != nil || out["second_axis"] != nil {
			t.Errorf("chart %d: a second measure says a mark or an axis: %v", i+1, out)
		}
	}
	// A target that is not a number reaches the server, which refuses it in its words.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Goal", "source_id": sourceID, "type": "bullet", "aggregate": "count", "target": "lots"}}}))
	if _, err := charts.Decode(got[0].Charts[0].Settings); err == nil || !strings.Contains(err.Error(), "target is a number from") {
		t.Fatalf("a target of lots = %s, decoded: %v", got[0].Charts[0].Settings, err)
	}
	// Control: a candlestick's y is split, any other type's y is one column as typed.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Grid", "source_id": sourceID, "type": "heatmap", "x": "region", "y": "a, b",
			"aggregate": "count"}}}))
	if !strings.Contains(string(got[0].Charts[0].Settings), `"y":[{"column":"a, b"}]`) {
		t.Fatalf("control: a heatmap's y = %s", got[0].Charts[0].Settings)
	}
}

// D4e's map in the tool's words: its shapes by name or by another word for them, and
// x a column of codes. It reaches the store as settings the server accepts, and
// get_dashboard gives the shapes back as the map argument plan_dashboard_change takes.
// A map with no shapes, or with shapes rsync.ai does not ship, is the server's to refuse.
func TestPlanDashboard_D4eMapBecomesCheckedSettings(t *testing.T) {
	var got []handlers.DashboardProposalInput
	h := leakyHandlers()
	h.ProposeDashboard = dashboardRecorder(&got)
	r, _ := testServer(t, h, adminActor)
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Revenue by country", "source_id": sourceID, "type": "choropleth", "map": "countries",
			"x": "country_code", "aggregate": "sum", "measure_column": "total"},
		map[string]any{"name": "Orders by state", "source_id": sourceID, "type": "map", "map": "US states",
			"x": "state", "aggregate": "count"},
	}}))
	want := []string{
		`{"version":2,"type":"map","roles":{"measure":[{"column":"total","aggregate":"sum"}],"x":[{"column":"country_code"}]},"map":"world"}`,
		`{"version":2,"type":"map","roles":{"measure":[{"aggregate":"count"}],"x":[{"column":"state"}]},"map":"us_states"}`,
	}
	if len(got) != 1 || len(got[0].Charts) != len(want) {
		t.Fatalf("store got %+v", got)
	}
	changeArgs := map[string]bool{}
	for _, a := range dashboardChangeArgs() {
		changeArgs[a.Name] = true
	}
	for i, c := range got[0].Charts {
		if string(c.Settings) != want[i] {
			t.Fatalf("chart %d settings = %s, want %s", i+1, c.Settings, want[i])
		}
		s, err := charts.Decode(c.Settings)
		if err != nil {
			t.Fatalf("chart %d: the server would refuse what the tool built: %v", i+1, err)
		}
		out := chartSettingsOut(*s)
		if want := []string{charts.MapWorld, charts.MapUSStates}[i]; out.Map != want || !changeArgs["map"] {
			t.Fatalf("chart %d: get_dashboard map = %q, want %q, a change argument", i+1, out.Map, want)
		}
	}
	// No shapes reach the server, which refuses them in its words; shapes rsync.ai does not
	// ship are the tool's enum to refuse.
	got = nil
	item := map[string]any{"name": "Orders", "source_id": sourceID, "type": "map", "x": "country_code", "aggregate": "count"}
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{item}}))
	if _, err := charts.Decode(got[0].Charts[0].Settings); err == nil || !strings.Contains(err.Error(), "a map chart needs a map: world or us_states") {
		t.Fatalf("a map with no shapes = %s, decoded: %v", got[0].Charts[0].Settings, err)
	}
	got, item["map"] = nil, "europe"
	res := call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{item}})
	if !isError(res) || resultText(res) != "Invalid arguments: charts[0].map must be one of world, us_states" || got != nil {
		t.Fatalf("a map of europe = %v, stored %+v", res, got)
	}
	// Control: a type other than map has no map.
	got = nil
	data(t, call(t, r, "plan_dashboard", map[string]any{"name": "Sales", "charts": []any{
		map[string]any{"name": "Orders", "source_id": sourceID, "type": "bar", "x": "country_code", "aggregate": "count"}}}))
	if strings.Contains(string(got[0].Charts[0].Settings), `"map"`) {
		t.Fatalf("control: a bar's settings = %s", got[0].Charts[0].Settings)
	}
}

func TestPlanDashboard_RefusalGoesBackToTheModel(t *testing.T) {
	h := leakyHandlers()
	h.ProposeDashboard = func(*gin.Context, services.Actor, handlers.DashboardProposalInput) (*handlers.DashboardProposal, error) {
		return nil, &handlers.PlanRefusal{Code: "invalid_settings", Status: http.StatusUnprocessableEntity,
			Message: `Chart 2 ("Orders per day"): the column "order_dat" is not in the source; it has order_date, total`}
	}
	r, audit := testServer(t, h, adminActor)
	res := call(t, r, "plan_dashboard", validArgs["plan_dashboard"])
	if !isError(res) || !strings.Contains(resultText(res), `"order_dat" is not in the source`) {
		t.Fatalf("want the check's message, got %v", res)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_dashboard", "dashboard_proposal", "", "invalid_settings", 422}) {
		t.Fatalf("audit = %+v", *audit)
	}
}

// No tool saves, changes, deletes or draws a chart or a dashboard: the person does,
// in rsync.ai (§ 7.3, § 7.6). The dashboard writes are drafts: a new dashboard and a
// change to one tile, each stored for the person; the tools that read a saved
// dashboard write nothing.
func TestNoToolSavesADashboard(t *testing.T) {
	s, err := New(Options{Handlers: leakyHandlers(), Actor: func(*gin.Context) services.Actor { return adminActor },
		Audit: func(*gin.Context, string, string, string, string, int) {}, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seen := 0
	for _, tool := range s.Tools() {
		if !strings.Contains(tool.Name, "chart") && !strings.Contains(tool.Name, "dashboard") {
			continue
		}
		seen++
		drafts := map[string]string{"plan_dashboard": "dashboard_proposal", "plan_dashboard_change": "dashboard_change"}
		if tool.Write && drafts[tool.Name] != tool.ResourceType {
			t.Errorf("%s writes a %s; only plan_dashboard and plan_dashboard_change may, and each stores a draft",
				tool.Name, tool.ResourceType)
		}
		if tool.ResourceType == "chart" || (tool.ResourceType == "dashboard" && tool.Write) {
			t.Errorf("%s acts on a saved %s", tool.Name, tool.ResourceType)
		}
	}
	// Control: the check saw the six dashboard tools.
	if seen != 6 {
		t.Fatalf("saw %d chart or dashboard tools, want 6", seen)
	}
}

// get_dashboard gives each chart tile's settings in plan_dashboard's words, so a
// change can send them back; it leaves out the series colours, which name series by
// their values, and a text tile's text, which the handler never reads.
func TestGetDashboard_SettingsInTheToolsWords(t *testing.T) {
	r, _ := testServer(t, leakyHandlers(), readerActor)
	res := call(t, r, "get_dashboard", validArgs["get_dashboard"])
	d := data(t, res)
	tiles := d["tiles"].([]any)
	if len(tiles) != 2 {
		t.Fatalf("tiles = %v", tiles)
	}
	chart := tiles[0].(map[string]any)["chart"].(map[string]any)
	want := map[string]any{"type": "bar", "x": "order_date", "grain": "week", "aggregate": "sum", "measure_column": "total",
		"number_format": "compact", "series": "region", "stack": "stacked", "sort": "x_asc"}
	for k, v := range want {
		if chart[k] != v {
			t.Errorf("chart %s = %v, want %v", k, chart[k], v)
		}
	}
	if len(chart) != len(want) {
		t.Errorf("chart = %v, want only %v", chart, want)
	}
	if text := tiles[1].(map[string]any); text["kind"] != "text" || text["chart"] != nil {
		t.Errorf("text tile = %v", text)
	}
	// Each name get_dashboard returns is an argument plan_dashboard_change takes.
	args := map[string]bool{}
	for _, a := range dashboardChangeArgs() {
		args[a.Name] = true
	}
	for k := range chart {
		if !args[k] {
			t.Errorf("get_dashboard returns %q, which plan_dashboard_change does not take", k)
		}
	}
}

// changeRecorder is a ProposeDashboardChange that keeps what it was asked.
func changeRecorder(got *[]handlers.DashboardChangeInput) func(*gin.Context, services.Actor, handlers.DashboardChangeInput) (*handlers.DashboardChange, error) {
	return func(_ *gin.Context, _ services.Actor, in handlers.DashboardChangeInput) (*handlers.DashboardChange, error) {
		*got = append(*got, in)
		return leakyDashboardChange(in), nil
	}
}

// plan_dashboard_change sends the whole new settings when type is given, and none
// otherwise; a chart setting without type is refused before the store, so a change
// never drops the settings the model left out. A colour by name is its token.
func TestPlanDashboardChange_SettingsOnlyWithType(t *testing.T) {
	var got []handlers.DashboardChangeInput
	h := leakyHandlers()
	h.ProposeDashboardChange = changeRecorder(&got)
	r, audit := testServer(t, h, adminActor)
	d := data(t, call(t, r, "plan_dashboard_change", validArgs["plan_dashboard_change"]))
	if len(got) != 1 || string(got[0].Settings) != `{"version":2,"type":"line","roles":{"measure":[{"column":"total","aggregate":"sum"}],"x":[{"column":"order_date"}]}}` ||
		got[0].W != 12 || got[0].H != 0 || got[0].TileID != tileID {
		t.Fatalf("store got %+v (settings %s)", got, got[0].Settings)
	}
	if d["change_id"] != dashChgID || d["copy"] != true || d["moved"] != float64(1) ||
		!strings.Contains(d["next"].(string), "its own copy") {
		t.Errorf("out = %v", d)
	}
	if len(*audit) != 1 || (*audit)[0] != (auditRow{"plan_dashboard_change", "dashboard_change", dashChgID, "ok", 200}) {
		t.Fatalf("audit = %+v", *audit)
	}

	got = nil
	data(t, call(t, r, "plan_dashboard_change", map[string]any{"dashboard_id": dashID, "tile_id": tileID,
		"color": "Purple", "height": 9, "summary": "Makes the bars violet."}))
	if len(got) != 1 || got[0].Settings != nil || got[0].Color != "series-6" || got[0].H != 9 {
		t.Fatalf("store got %+v", got)
	}

	got = nil
	res := call(t, r, "plan_dashboard_change", map[string]any{"dashboard_id": dashID, "tile_id": tileID,
		"sort": "value_desc", "summary": "Sorts the bars."})
	if !isError(res) || !strings.Contains(resultText(res), "sort changes the chart: send type") || len(got) != 0 {
		t.Fatalf("want sort without type refused before the store, got %v (store %+v)", res, got)
	}
	// Control: the same sort with type reaches the store.
	data(t, call(t, r, "plan_dashboard_change", map[string]any{"dashboard_id": dashID, "tile_id": tileID,
		"type": "bar", "x": "region", "aggregate": "count", "sort": "value_desc", "summary": "Sorts the bars."}))
	if len(got) != 1 || !strings.Contains(string(got[0].Settings), `"sort":"value_desc"`) {
		t.Fatalf("control: store got %+v", got)
	}
}

// A handler's refusal (an out-of-date or not-found dashboard, a chart check that
// fails) goes back to the model with its message, and nothing is stored.
func TestPlanDashboardChange_RefusalGoesBackToTheModel(t *testing.T) {
	h := leakyHandlers()
	h.ProposeDashboardChange = func(*gin.Context, services.Actor, handlers.DashboardChangeInput) (*handlers.DashboardChange, error) {
		return nil, &handlers.PlanRefusal{Code: "no_change", Status: http.StatusUnprocessableEntity,
			Message: "The change leaves the tile as it is."}
	}
	r, audit := testServer(t, h, adminActor)
	res := call(t, r, "plan_dashboard_change", validArgs["plan_dashboard_change"])
	if !isError(res) || !strings.Contains(resultText(res), "leaves the tile as it is") {
		t.Fatalf("want the handler's message, got %v", res)
	}
	if len(*audit) != 1 || (*audit)[0].outcome != "no_change" || (*audit)[0].status != 422 {
		t.Fatalf("audit = %+v", *audit)
	}
}

// describe_chart_source returns column names, kinds and masks only, and a source the
// person may not see is the handler's not-found refusal.
func TestDescribeChartSource_ColumnsOnly(t *testing.T) {
	h := leakyHandlers()
	r, _ := testServer(t, h, readerActor)
	d := data(t, call(t, r, "describe_chart_source", validArgs["describe_chart_source"]))
	cols := d["columns"].([]any)
	if len(cols) != 3 || cols[2].(map[string]any)["masked"] != true || cols[0].(map[string]any)["kind"] != "time" {
		t.Fatalf("columns = %v", cols)
	}
	h.DescribeChartSource = func(*gin.Context, services.Actor, string) (*handlers.ChartSourceItem, []handlers.ChartSourceColumn, error) {
		return nil, nil, &handlers.PlanRefusal{Code: "not_found", Status: http.StatusNotFound,
			Message: "No model or saved query with that id is visible to you; list_chart_sources lists them."}
	}
	r, _ = testServer(t, h, readerActor)
	res := call(t, r, "describe_chart_source", validArgs["describe_chart_source"])
	if !isError(res) || !strings.Contains(resultText(res), "list_chart_sources") {
		t.Fatalf("want the not-found refusal, got %v", res)
	}
}
