package charts

import (
	"strings"
	"testing"
)

// Every refusal of dashboards-design.md § 3.2, each by the rule it breaks. A case that
// is refused for the wrong rule fails too: a check that refuses everything would pass a
// test that only asked for an error. Each type's valid settings are the control.

func TestDecodeAcceptsEachType(t *testing.T) {
	for name, raw := range map[string]string{
		"bar":       `{"type":"bar","x":{"column":"day","title":"Day"},"measure":{"aggregate":"sum","column":"amount","format":"compact","color":"series-2"},"sort":"x_asc"}`,
		"stacked":   `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"},"series":{"column":"region","colors":[{"value":"EU","color":"series-1"},{"value":"US","color":"series-2"}]},"stacked":true}`,
		"hbar":      `{"type":"hbar","x":{"column":"region"},"measure":{"aggregate":"count_distinct","column":"customer_id"},"sort":"value_desc"}`,
		"line":      `{"type":"line","x":{"column":"day"},"measure":{"aggregate":"avg","column":"amount"}}`,
		"percent":   `{"type":"line","x":{"column":"day"},"measure":{"aggregate":"avg","column":"default_rate","format":"percent"}}`,
		"area":      `{"type":"area","x":{"column":"day"},"measure":{"aggregate":"max","column":"amount"},"series":{"column":"region"},"stacked":true}`,
		"heatmap":   `{"type":"heatmap","x":{"column":"day"},"y":{"column":"region"},"measure":{"aggregate":"min","column":"amount"}}`,
		"scorecard": `{"type":"scorecard","measure":{"aggregate":"sum","column":"amount"},"compare":{"column":"day","days":7}}`,
		"table":     `{"type":"table","columns":["day","region","amount"],"sort":"x_desc"}`,
		"unicode":   `{"type":"bar","x":{"column":"Straße","title":"Umsatz – € je Tag"},"measure":{"aggregate":"count"}}`,
		// Version 2: fields by role, and a date grain on an axis.
		"v2 bar by month": `{"version":2,"type":"bar","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`,
		"v2 heatmap":      `{"version":2,"type":"heatmap","roles":{"x":[{"column":"day","grain":"week"}],"y":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`,
		"v2 scorecard":    `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"day"}]},"compare":{"days":30}}`,
		"v2 table":        `{"version":2,"type":"table","roles":{"columns":[{"column":"a"},{"column":"b"}]}}`,
		// D4b: a combo, 100% stacking, the parts-of-a-whole types, a histogram, a sparkline.
		"combo":             `{"version":2,"type":"bar","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"sum","column":"amount","title":"Revenue"},{"aggregate":"avg","column":"amount","title":"Average order","mark":"line","axis":"right"}]}}`,
		"combo on one axis": `{"version":2,"type":"line","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"count"},{"aggregate":"count_distinct","column":"customer_id"}]}}`,
		"100% area":         `{"version":2,"type":"area","roles":{"x":[{"column":"day"}],"series":[{"column":"region"}],"measure":[{"aggregate":"sum","column":"amount"}]},"stacked":true,"percent":true}`,
		"pie":               `{"version":2,"type":"pie","roles":{"series":[{"column":"region","colors":[{"value":"EU","color":"series-3"}]}],"measure":[{"aggregate":"sum","column":"amount","format":"compact"}]}}`,
		"donut":             `{"version":2,"type":"donut","roles":{"series":[{"column":"plan"}],"measure":[{"aggregate":"count"}]}}`,
		"waterfall":         `{"version":2,"type":"waterfall","roles":{"x":[{"column":"month","grain":"month"}],"measure":[{"aggregate":"sum","column":"net_change"}]},"sort":"x_asc"}`,
		"funnel":            `{"version":2,"type":"funnel","roles":{"x":[{"column":"stage","title":"Stage"}],"measure":[{"aggregate":"count_distinct","column":"user_id","color":"series-4"}]}}`,
		"histogram":         `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount","title":"Amount","bins":10}]}}`,
		"histogram of 50":   `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount","bins":50}]}}`,
		"histogram by 2.5":  `{"version":2,"type":"histogram","roles":{"x":[{"column":"amount","width":2.5}]}}`,
		"sparkline":         `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"sum","column":"amount"}],"date":[{"column":"day"}]},"compare":{"days":90},"sparkline":true}`,
		// D4c: points, and a pivot table with its totals.
		"scatter":           `{"version":2,"type":"scatter","roles":{"x":[{"column":"price","title":"Price"}],"y":[{"column":"units"}]}}`,
		"scatter by colour": `{"version":2,"type":"scatter","roles":{"x":[{"column":"price"}],"y":[{"column":"units"}],"size":[{"column":"margin"}],"series":[{"column":"region","colors":[{"value":"EU","color":"series-2"}]}]}}`,
		"bubble":            `{"version":2,"type":"bubble","roles":{"x":[{"column":"price"}],"y":[{"column":"units"}],"size":[{"column":"margin","title":"Margin"}]}}`,
		"pivot":             `{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`,
		"pivot in full":     `{"version":2,"type":"pivot","roles":{"rows":[{"column":"region","title":"Region"},{"column":"plan"},{"column":"day","grain":"month"}],"columns":[{"column":"channel"},{"column":"shipped","grain":"year"}],"measure":[{"aggregate":"sum","column":"amount","format":"compact"},{"aggregate":"avg","column":"amount"},{"aggregate":"count"}]}}`,
		// D4d: the second tier.
		"treemap":              `{"version":2,"type":"treemap","roles":{"rows":[{"column":"region","title":"Region"},{"column":"plan"},{"column":"product"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`,
		"sunburst":             `{"version":2,"type":"sunburst","roles":{"rows":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`,
		"sankey":               `{"version":2,"type":"sankey","roles":{"x":[{"column":"from_page"}],"y":[{"column":"to_page","title":"To"}],"measure":[{"aggregate":"count"}]}}`,
		"calendar":             `{"version":2,"type":"calendar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"avg","column":"amount","format":"decimal"}]}}`,
		"bullet by a measure":  `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount","color":"series-2"},{"aggregate":"sum","column":"goal"}]}}`,
		"bullet by a number":   `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"}]},"target":125000.5}`,
		"bullet below zero":    `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"min","column":"margin"}]},"target":-3}`,
		"box plot":             `{"version":2,"type":"boxplot","roles":{"x":[{"column":"day","grain":"month"}],"y":[{"column":"amount","format":"compact"}]},"sort":"x_desc"}`,
		"candlestick":          `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day","grain":"week"}],"y":[{"column":"open"},{"column":"high"},{"column":"low"},{"column":"close"}]}}`,
		"candlestick of ticks": `{"version":2,"type":"candlestick","roles":{"x":[{"column":"at","grain":"day"}],"y":[{"column":"price"},{"column":"price"},{"column":"price"},{"column":"price"}]}}`,
		"range bands":          `{"version":2,"type":"range","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"min","column":"amount","title":"Low"},{"aggregate":"max","column":"amount"}]}}`,
		// D4e: maps.
		"world map":    `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"country","title":"Country"}],"measure":[{"aggregate":"sum","column":"amount","color":"series-3","format":"compact"}]}}`,
		"US-state map": `{"version":2,"type":"map","map":"us_states","roles":{"x":[{"column":"state"}],"measure":[{"aggregate":"count"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err != nil {
				t.Fatalf("Decode(%s) = %v, want accepted", raw, err)
			}
		})
	}
}

func TestDecodeRefusesEachRule(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		rule int
		want string
	}{
		// Rule 1: one Go type, unknown keys refused, a type a translator draws.
		{"unknown key", `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"},"color":"#ff0000"}`, 1, "unknown setting"},
		{"echarts option key", `{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"},"tooltip":{"formatter":"x"}}`, 1, "never a chart library's options"},
		{"nested unknown key", `{"type":"bar","x":{"column":"day","axisLabel":{}},"measure":{"aggregate":"count"}}`, 1, "unknown setting"},
		{"type not drawn", `{"type":"gauge","measure":{"aggregate":"count"}}`, 1, "not one rsync.ai draws"},
		{"two JSON values", `{"type":"table","columns":["a"]} {}`, 1, "more than one JSON value"},
		{"wrong JSON type", `{"type":"table","columns":"a"}`, 1, "columns must be a list"},
		{"not an object", `[]`, 1, "not valid: not a JSON object"},
		{"x as a list", `{"type":"bar","x":["d"],"measure":{"aggregate":"count"}}`, 1, "x must be a JSON object"},
		{"stacked as text", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"stacked":"yes"}`, 1, "stacked must be true or false"},
		{"bar without x", `{"type":"bar","measure":{"aggregate":"count"}}`, 1, "needs an x column"},
		{"line stacked", `{"type":"line","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s"},"stacked":true}`, 1, "use an area chart"},
		{"heatmap with series", `{"type":"heatmap","x":{"column":"d"},"y":{"column":"r"},"measure":{"aggregate":"count"},"series":{"column":"s"}}`, 1, "series column"},
		{"scorecard with sort", `{"type":"scorecard","measure":{"aggregate":"count"},"sort":"x_asc"}`, 1, "has no sort"},
		{"table without columns", `{"type":"table"}`, 1, "at least one column"},
		{"compare period", `{"type":"scorecard","measure":{"aggregate":"count"},"compare":{"column":"d","days":14}}`, 1, "1, 7, 30, 90 or 365"},
		{"alias collision", `{"type":"bar","x":{"column":"amount"},"measure":{"aggregate":"sum","column":"amount"}}`, 1, "would both be named"},
		{"series column named as Other", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"RSYNC_OTHER"}}`, 1, "a series chart's SQL uses itself"},
		{"x named as the rank", `{"type":"line","x":{"column":"rsync_rank"},"measure":{"aggregate":"count"},"series":{"column":"s"}}`, 1, "a series chart's SQL uses itself"},
		{"control char in column", "{\"type\":\"table\",\"columns\":[\"a\\u0000b\"]}", 1, "not a column name"},
		{"unknown sort", `{"type":"table","columns":["a"],"sort":"random"}`, 1, "the sort"},
		{"unknown format", `{"type":"scorecard","measure":{"aggregate":"count","format":"{value}"}}`, 1, "number format"},
		// Version 2's roles, held to the type's registry entry.
		{"version not read", `{"version":3,"type":"table","roles":{"columns":[{"column":"a"}]}}`, 1, "rsync.ai reads 1 and 2"},
		{"version as text", `{"version":"2","type":"table","roles":{"columns":[{"column":"a"}]}}`, 1, "version must be a number"},
		{"unknown role", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}],"radius":[{"column":"s"}]}}`, 1, "unknown role"},
		{"role the type lacks", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}],"y":[{"column":"r"}]}}`, 1, "has no y column"},
		{"two x fields", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"},{"column":"e"}],"measure":[{"aggregate":"count"}]}}`, 1, "at most 1 x column"},
		{"attribute the role lacks", `{"version":2,"type":"bar","roles":{"x":[{"column":"d","aggregate":"sum"}],"measure":[{"aggregate":"count"}]}}`, 1, "x.aggregate is not a setting"},
		{"grain on a measure", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count","grain":"day"}]}}`, 1, "measure.grain is not a setting"},
		{"unknown grain", `{"version":2,"type":"bar","roles":{"x":[{"column":"d","grain":"fortnight"}],"measure":[{"aggregate":"count"}]}}`, 1, "date grain"},
		{"compare without a date", `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}]},"compare":{"days":7}}`, 1, "needs a date column to compare by"},
		{"date without compare", `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"d"}]}}`, 1, "without a comparison period"},
		{"table of 21 columns", `{"version":2,"type":"table","roles":{"columns":[` + strings.TrimSuffix(strings.Repeat(`{"column":"c"},`, maxTableCols+1), ",") + `]}}`, 1, "at most 20 columns"},
		{"empty x column", `{"version":2,"type":"bar","roles":{"x":[{"title":"Day"}],"measure":[{"aggregate":"count"}]}}`, 1, "x.column is empty"},
		// D4b's guardrails.
		{"three measures", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"},{"aggregate":"avg","column":"a"}]}}`, 1, "at most 2 measure"},
		{"two measures on hbar", `{"version":2,"type":"hbar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"}]}}`, 1, "at most 1 measure"},
		{"two measures and a series", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"}]}}`, 1, "two measures has no series column"},
		{"both on the right", `{"version":2,"type":"line","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count","axis":"right"},{"aggregate":"sum","column":"a","axis":"right"}]}}`, 1, "both measures are on the right axis"},
		{"the same measure twice", `{"version":2,"type":"line","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"sum","column":"a"},{"aggregate":"sum","column":"a"}]}}`, 1, "the two measures are the same"},
		{"mark on one measure", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count","mark":"line"}]}}`, 1, "only beside a second measure"},
		{"unknown mark", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a","mark":"scatter"}]}}`, 1, "the mark"},
		{"unknown axis", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a","axis":"top"}]}}`, 1, "the axis"},
		{"second measure collides", `{"version":2,"type":"bar","roles":{"x":[{"column":"amount_2"}],"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"avg","column":"amount"}]}}`, 1, "would both be named"},
		{"percent unstacked", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]},"percent":true}`, 1, "100% stacking unless it is stacked"},
		{"pie without slices", `{"version":2,"type":"pie","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "needs a series column"},
		{"pie with x", `{"version":2,"type":"pie","roles":{"x":[{"column":"d"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]}}`, 1, "has no x column"},
		{"pie sorted", `{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]},"sort":"value_desc"}`, 1, "a pie chart has no sort"},
		{"funnel with a grain", `{"version":2,"type":"funnel","roles":{"x":[{"column":"d","grain":"day"}],"measure":[{"aggregate":"count"}]}}`, 1, "x.grain is not a setting"},
		{"histogram with a measure", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`, 1, "has no measure"},
		{"histogram of 101 bins", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","bins":101}]}}`, 1, "1 to 100 bins"},
		{"histogram of -1 bins", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","bins":-1}]}}`, 1, "1 to 100 bins"},
		{"negative bin width", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","width":-2}]}}`, 1, "a number from 0.000000001 to 1,000,000,000,000,000"},
		{"bin width too fine", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","width":1e-12}]}}`, 1, "a number from 0.000000001"},
		{"bin width too wide", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","width":1e16}]}}`, 1, "a number from 0.000000001"},
		{"bins and width", `{"version":2,"type":"histogram","roles":{"x":[{"column":"a","bins":10,"width":2}]}}`, 1, "not both"},
		{"bins on a bar", `{"version":2,"type":"bar","roles":{"x":[{"column":"d","bins":10}],"measure":[{"aggregate":"count"}]}}`, 1, "x.bins is not a setting"},
		{"histogram x named as the width", `{"version":2,"type":"histogram","roles":{"x":[{"column":"RSYNC_BIN_WIDTH"}]}}`, 1, "would both be named"},
		{"sparkline without compare", `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}]},"sparkline":true}`, 1, "a comparison period to draw a sparkline over"},
		{"sparkline over a day", `{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"d"}]},"compare":{"days":1},"sparkline":true}`, 1, "compare 7 days or more"},
		{"sparkline on a bar", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}]},"sparkline":true}`, 1, "has no sparkline"},
		{"pie of averages", `{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"aggregate":"avg","column":"a"}]}}`, 2, "must add up, so it takes count or sum, not avg"},
		{"waterfall of max", `{"version":2,"type":"waterfall","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"max","column":"a"}]}}`, 2, "takes count or sum, not max"},
		{"percent of averages", `{"version":2,"type":"area","roles":{"x":[{"column":"d"}],"series":[{"column":"s"}],"measure":[{"aggregate":"avg","column":"a"}]},"stacked":true,"percent":true}`, 2, "count or sum, not avg"},
		// D4c's scatter, bubble and pivot table.
		{"size on a bar", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}],"size":[{"column":"s"}]}}`, 1, "a bar chart has no size column"},
		{"scatter without y", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}]}}`, 1, "needs a y column"},
		{"bubble without a size", `{"version":2,"type":"bubble","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}]}}`, 1, "needs a size column"},
		{"scatter with a measure", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}],"measure":[{"aggregate":"count"}]}}`, 1, "a scatter chart has no measure"},
		{"scatter sorted", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}]},"sort":"x_asc"}`, 1, "a scatter chart has no sort"},
		{"scatter x with a grain", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a","grain":"day"}],"y":[{"column":"b"}]}}`, 1, "x.grain is not a setting"},
		{"scatter colour named as Other", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}],"series":[{"column":"RSYNC_OTHER"}]}}`, 1, "a series chart's SQL uses itself"},
		{"scatter x and y one column", `{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}],"y":[{"column":"a"}]}}`, 1, "would both be named"},
		{"scatter x named as the row count", `{"version":2,"type":"scatter","roles":{"x":[{"column":"RSYNC_ROWS"}],"y":[{"column":"b"}]}}`, 1, "a scatter chart's SQL uses itself for each point's row count"},
		{"bubble size named as the row count", `{"version":2,"type":"bubble","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}],"size":[{"column":"rsync_rows"}]}}`, 1, "a bubble chart's SQL uses itself for each point's row count"},
		{"pivot without rows", `{"version":2,"type":"pivot","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "needs at least one row field"},
		{"pivot without a measure", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}]}}`, 1, "needs a measure"},
		{"pivot of 4 row fields", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"a"},{"column":"b"},{"column":"c"},{"column":"d"}],"measure":[{"aggregate":"count"}]}}`, 1, "at most 3 row fields"},
		{"pivot of 3 column fields", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"columns":[{"column":"a"},{"column":"b"},{"column":"c"}],"measure":[{"aggregate":"count"}]}}`, 1, "at most 2 column fields"},
		{"pivot of 4 measures", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"},{"aggregate":"avg","column":"a"},{"aggregate":"max","column":"a"}]}}`, 1, "at most 3 measures"},
		{"pivot measures the same", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"measure":[{"aggregate":"sum","column":"a"},{"aggregate":"count"},{"aggregate":"sum","column":"a"}]}}`, 1, "measures 1 and 3 are the same"},
		{"pivot measure with a mark", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"measure":[{"aggregate":"count","mark":"line"}]}}`, 1, "measure.mark is not a setting"},
		{"pivot column with colours", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"columns":[{"column":"c","colors":[{"value":"EU","color":"series-1"}]}],"measure":[{"aggregate":"count"}]}}`, 1, "its column fields take"},
		{"pivot sorted", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"measure":[{"aggregate":"count"}]},"sort":"value_desc"}`, 1, "a pivot table has no sort"},
		{"pivot with a series", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]}}`, 1, "has no series column"},
		{"pivot field named as a total", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"RSYNC_TOTAL_R1"}],"measure":[{"aggregate":"count"}]}}`, 1, "a pivot table's SQL uses itself"},
		{"pivot row and measure collide", `{"version":2,"type":"pivot","roles":{"rows":[{"column":"amount"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`, 1, "would both be named"},
		// D4d's second tier: each type's roles named in its own words, a bullet's one target,
		// and the names a box plot's or a candlestick's SQL uses itself.
		{"treemap without levels", `{"version":2,"type":"treemap","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "a treemap needs at least one level"},
		{"treemap of 4 levels", `{"version":2,"type":"treemap","roles":{"rows":[{"column":"a"},{"column":"b"},{"column":"c"},{"column":"d"}],"measure":[{"aggregate":"count"}]}}`, 1, "at most 3 levels"},
		{"treemap of averages", `{"version":2,"type":"treemap","roles":{"rows":[{"column":"a"}],"measure":[{"aggregate":"avg","column":"v"}]}}`, 2, "must add up"},
		{"sunburst level grain", `{"version":2,"type":"sunburst","roles":{"rows":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"count"}]}}`, 1, "its level takes column, title"},
		{"treemap sorted", `{"version":2,"type":"treemap","roles":{"rows":[{"column":"a"}],"measure":[{"aggregate":"count"}]},"sort":"value_desc"}`, 1, "has no sort"},
		{"sankey without a target", `{"version":2,"type":"sankey","roles":{"x":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`, 1, "needs a target column (y)"},
		{"sankey without a source", `{"version":2,"type":"sankey","roles":{"y":[{"column":"b"}],"measure":[{"aggregate":"count"}]}}`, 1, "needs a source column (x)"},
		{"sankey of one column", `{"version":2,"type":"sankey","roles":{"x":[{"column":"a"}],"y":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`, 1, "would both be named"},
		{"sankey of maxima", `{"version":2,"type":"sankey","roles":{"x":[{"column":"a"}],"y":[{"column":"b"}],"measure":[{"aggregate":"max","column":"v"}]}}`, 2, "must add up"},
		{"calendar without a date", `{"version":2,"type":"calendar","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "needs a date column (x)"},
		{"calendar by month", `{"version":2,"type":"calendar","roles":{"x":[{"column":"day","grain":"month"}],"measure":[{"aggregate":"count"}]}}`, 1, "x.grain is not a setting"},
		{"bullet without a target", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"}]}}`, 1, "needs a target: a second measure or a number"},
		{"bullet with two targets", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"sum","column":"goal"}]},"target":5}`, 1, "a target measure or a target number, not both"},
		{"bullet target too large", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"}]},"target":1e16}`, 1, "target is a number from -1,000,000,000,000,000 to 1,000,000,000,000,000"},
		{"bullet of three measures", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"},{"aggregate":"sum","column":"b"}]}}`, 1, "at most 2 measures"},
		{"bullet measure with a mark", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count","mark":"line"}]},"target":5}`, 1, "measure.mark is not a setting"},
		{"target on a bar", `{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}]},"target":5}`, 1, "a bar chart has no target"},
		{"bullet target as text", `{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count"}]},"target":"5"}`, 1, "target must be a number"},
		{"box plot without a number", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}]}}`, 1, "needs a number column (y)"},
		{"box plot without groups", `{"version":2,"type":"boxplot","roles":{"y":[{"column":"amount"}]}}`, 1, "needs a group column (x)"},
		{"box plot with a measure", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"r"}],"y":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`, 1, "has no measure"},
		{"box plot value sort", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"r"}],"y":[{"column":"a"}]},"sort":"value_desc"}`, 1, "has no value sort"},
		{"box plot group named as a statistic", `{"version":2,"type":"boxplot","roles":{"x":[{"column":"rsync_median"}],"y":[{"column":"a"}]}}`, 1, "would both be named"},
		{"candlestick of three prices", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"o"},{"column":"h"},{"column":"l"}]}}`, 1, "needs open, high, low and close columns (y)"},
		{"candlestick of five prices", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"o"},{"column":"h"},{"column":"l"},{"column":"c"},{"column":"v"}]}}`, 1, "at most 4 y columns"},
		{"candlestick price title", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"o","title":"Open"},{"column":"h"},{"column":"l"},{"column":"c"}]}}`, 1, "y.title is not a setting"},
		{"candlestick date named as a price", `{"version":2,"type":"candlestick","roles":{"x":[{"column":"rsync_close"}],"y":[{"column":"o"},{"column":"h"},{"column":"l"},{"column":"c"}]}}`, 1, "would both be named"},
		{"range of one measure", `{"version":2,"type":"range","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"min","column":"a"}]}}`, 1, "needs a low and a high measure"},
		{"range low and high the same", `{"version":2,"type":"range","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"min","column":"a"},{"aggregate":"min","column":"a"}]}}`, 1, "the two measures are the same"},
		{"range with a series", `{"version":2,"type":"range","roles":{"x":[{"column":"day"}],"series":[{"column":"s"}],"measure":[{"aggregate":"min","column":"a"},{"aggregate":"max","column":"a"}]}}`, 1, "has no series column"},
		{"map without a map", `{"version":2,"type":"map","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"}]}}`, 1, "a map chart needs a map: world or us_states"},
		{"map of shapes not shipped", `{"version":2,"type":"map","map":"europe","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"}]}}`, 1, `the map "europe" is not one rsync.ai draws; use world or us_states`},
		{"map on a bar", `{"version":2,"type":"bar","map":"world","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"}]}}`, 1, "a bar chart has no map"},
		{"map without codes", `{"version":2,"type":"map","map":"world","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "needs a column of ISO codes (x)"},
		{"map without a measure", `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c"}]}}`, 1, "needs a measure"},
		{"map with a series", `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]}}`, 1, "has no series column"},
		{"map of two measures", `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"}]}}`, 1, "at most 1 measure"},
		{"map with a grain", `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c","grain":"month"}],"measure":[{"aggregate":"count"}]}}`, 1, "x.grain is not a setting of a choropleth map"},
		{"map with a sort", `{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"}]},"sort":"value_desc"}`, 1, "a choropleth map has no sort"},
		{"no gauge", `{"version":2,"type":"gauge","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "not one rsync.ai draws"},
		{"no radar", `{"version":2,"type":"radar","roles":{"measure":[{"aggregate":"count"}]}}`, 1, "not one rsync.ai draws"},
		// Rule 2 (its second half): every aggregate is from a fixed list.
		{"aggregate not offered", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"median","column":"a"}}`, 2, "not one rsync.ai offers"},
		{"measure without an aggregate", `{"type":"bar","x":{"column":"d"},"measure":{"column":"a"}}`, 2, "a measure needs an aggregate: one of count, "},
		{"v2 measure without an aggregate", `{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"column":"a"}]}}`, 2, "a measure needs an aggregate: one of count, sum"},
		{"aggregate as sql", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"sum(a); drop table t","column":"a"}}`, 2, "not one rsync.ai offers"},
		{"sum without column", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"sum"}}`, 2, "needs a column"},
		// Rule 3: every colour is a token, never a colour value.
		{"measure hex colour", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count","color":"#ff0000"}}`, 3, "not a colour token"},
		{"series rgb colour", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s","colors":[{"value":"EU","color":"rgb(1,2,3)"}]}}`, 3, "not a colour token"},
		{"other is not a series colour", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s","colors":[{"value":"EU","color":"other"}]}}`, 3, "not a colour token"},
		{"one token twice", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s","colors":[{"value":"EU","color":"series-1"},{"value":"US","color":"series-1"}]}}`, 3, "two series"},
		{"eight colours", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s","colors":[` + eightColours() + `]}}`, 3, "at most 7"},
		// Rule 4: plain text, nothing a chart library or a browser would run.
		{"formatter template", `{"type":"bar","x":{"column":"d","title":"{b}: {c}"},"measure":{"aggregate":"count"}}`, 4, "no { or }"},
		{"markup in title", `{"type":"bar","x":{"column":"d","title":"<img src=x onerror=alert(1)>"},"measure":{"aggregate":"count"}}`, 4, "no < or >"},
		{"markup in description", `{"type":"table","columns":["a"],"description":"<script>"}`, 4, "no < or >"},
		{"template in series value", `{"type":"bar","x":{"column":"d"},"measure":{"aggregate":"count"},"series":{"column":"s","colors":[{"value":"{a}","color":"series-1"}]}}`, 4, "no { or }"},
		{"control char in title", "{\"type\":\"bar\",\"x\":{\"column\":\"d\",\"title\":\"a\\u0007\"},\"measure\":{\"aggregate\":\"count\"}}", 4, "control characters"},
		{"title too long", `{"type":"bar","x":{"column":"d","title":"` + strings.Repeat("a", maxTitle+1) + `"},"measure":{"aggregate":"count"}}`, 4, "the most is"},
		// Rule 5: a size cap on the settings' JSON.
		{"over the size cap", `{"type":"table","columns":["a"],"description":"` + strings.Repeat(" ", MaxSettingsBytes) + `"}`, 5, "bytes; the most is"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Decode([]byte(c.raw))
			if err == nil {
				t.Fatalf("Decode accepted %s as %+v; want refused by rule %d", c.raw, s, c.rule)
			}
			ce, ok := AsError(err)
			if !ok {
				t.Fatalf("Decode error %T %v is not a *charts.Error", err, err)
			}
			if ce.Rule != c.rule {
				t.Errorf("refused by rule %d (%q), want rule %d", ce.Rule, ce.Message, c.rule)
			}
			if !strings.Contains(ce.Message, c.want) {
				t.Errorf("message %q does not say %q", ce.Message, c.want)
			}
		})
	}
}

// A refusal that suggests another value suggests one the type then takes, and names the
// type in plain words with its article ("an area chart", "a horizontal bar chart").
func TestRefusalsSuggestWhatTheTypeTakes(t *testing.T) {
	for name, c := range map[string]struct{ raw, want string }{
		"box plot, unknown sort": {`{"version":2,"type":"boxplot","roles":{"x":[{"column":"r"}],"y":[{"column":"a"}]},"sort":"y_desc"}`,
			`the sort "y_desc" is not one of x_asc, x_desc`},
		"pie, unknown sort": {`{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]},"sort":"random"}`,
			`the sort "random" is not one rsync.ai offers; a pie chart has no sort`},
		"bar, unknown sort": {`{"version":2,"type":"bar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}]},"sort":"random"}`,
			`the sort "random" is not one of x_asc, x_desc, value_asc, value_desc`},
		"box plot, value sort": {`{"version":2,"type":"boxplot","roles":{"x":[{"column":"r"}],"y":[{"column":"a"}]},"sort":"value_desc"}`,
			`a box plot has no value sort; use x_asc or x_desc`},
		"pie, aggregate not offered": {`{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"aggregate":"median","column":"a"}]}}`,
			`the aggregate "median" is not one rsync.ai offers; use one of count, sum`},
		"line, 100%": {`{"version":2,"type":"line","roles":{"x":[{"column":"d","grain":"month"}],"series":[{"column":"s"}],"measure":[{"aggregate":"sum","column":"a"}]},"percent":true}`,
			`a line chart has no 100% stacking; use an area chart`},
		"pie, 100%": {`{"version":2,"type":"pie","roles":{"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]},"percent":true}`,
			`a pie chart has no 100% stacking`},
		"area, 100% unstacked": {`{"version":2,"type":"area","roles":{"x":[{"column":"d"}],"series":[{"column":"s"}],"measure":[{"aggregate":"count"}]},"percent":true}`,
			`an area chart has no 100% stacking unless it is stacked`},
		"area, mark on one measure": {`{"version":2,"type":"area","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count","mark":"line"}]}}`,
			`a measure takes a mark and an axis only beside a second measure; an area chart with one measure is drawn as its type`},
		"hbar, two measures": {`{"version":2,"type":"hbar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"},{"aggregate":"sum","column":"a"}]}}`,
			`a horizontal bar chart takes at most 1 measure`},
		"area, a target": {`{"version":2,"type":"area","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}]},"target":5}`,
			`an area chart has no target`},
		// A type is named as the chart page labels it, a label that names its shape
		// taking no "chart".
		"pivot, a target": {`{"version":2,"type":"pivot","roles":{"rows":[{"column":"r"}],"measure":[{"aggregate":"count"}]},"target":5}`,
			`a pivot table has no target`},
		"calendar, a target": {`{"version":2,"type":"calendar","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"count"}]},"target":5}`,
			`a calendar heatmap has no target`},
		"map, a target": {`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"c"}],"measure":[{"aggregate":"count"}]},"target":5}`,
			`a choropleth map has no target`},
		"histogram, a target": {`{"version":2,"type":"histogram","roles":{"x":[{"column":"a"}]},"target":5}`,
			`a histogram has no target`},
		"table, a target": {`{"version":2,"type":"table","roles":{"columns":[{"column":"a"}]},"target":5}`,
			`a table has no target`},
		"range, a target": {`{"version":2,"type":"range","roles":{"x":[{"column":"d"}],"measure":[{"aggregate":"min","column":"a"},{"aggregate":"max","column":"a"}]},"target":5}`,
			`a range bands chart has no target`},
		"sankey from and to one column": {`{"version":2,"type":"sankey","roles":{"x":[{"column":"a"}],"y":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`,
			`the x and the y would both be named "a" in the result; pick a different column for where the flows start and where they end`},
		"scatter x and y one column": {`{"version":2,"type":"scatter","roles":{"x":[{"column":"a"}],"y":[{"column":"a"}]}}`,
			`the x and the y would both be named "a" in the result; pick a different column for one of them`},
		"bar x and series one column": {`{"version":2,"type":"bar","roles":{"x":[{"column":"a"}],"series":[{"column":"a"}],"measure":[{"aggregate":"count"}]}}`,
			`the x and the series would both be named "a" in the result; pick a different column for one of them`},
		"measure of the x column": {`{"version":2,"type":"bar","roles":{"x":[{"column":"amount"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`,
			`the x and the measure would both be named "amount" in the result; measure another column or use count`},
		"x named as the count": {`{"version":2,"type":"bar","roles":{"x":[{"column":"count"}],"measure":[{"aggregate":"count"}]}}`,
			`the x and the measure would both be named "count" in the result; the chart's SQL uses that name itself, so chart a column with another name`},
		"box plot group named as a statistic": {`{"version":2,"type":"boxplot","roles":{"x":[{"column":"rsync_median"}],"y":[{"column":"a"}]}}`,
			`the x and the median would both be named "rsync_median" in the result; the chart's SQL uses that name itself, so chart a column with another name`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(c.raw))
			ce, ok := AsError(err)
			if !ok {
				t.Fatalf("Decode(%s) = %v, want refused", c.raw, err)
			}
			if ce.Message != c.want {
				t.Errorf("message %q\n           want %q", ce.Message, c.want)
			}
		})
	}
	// Each sort a refusal suggests is one the type takes.
	for _, spec := range Registry {
		for _, sort := range spec.Sorts {
			if !contains(allSorts, sort) {
				t.Errorf("%s offers the sort %q, which no chart takes", spec.Type, sort)
			}
		}
	}
}

func eightColours() string {
	var parts []string
	for i := 0; i < 8; i++ {
		parts = append(parts, `{"value":"v`+string(rune('a'+i))+`","color":"series-1"}`)
	}
	return strings.Join(parts, ",")
}

// The refused value is echoed short, and nothing else of the body: a message goes back
// to the model drafting a proposal (§ 7.3).
func TestDecodeMessageNamesOnlyTheKey(t *testing.T) {
	_, err := Decode([]byte(`{"type":"bar","secret_value_here":"s3cr3t-row-value"}`))
	if err == nil || strings.Contains(err.Error(), "s3cr3t-row-value") {
		t.Fatalf("error %v echoes the body", err)
	}
	_, err = Decode([]byte(`{"type":"` + strings.Repeat("x", 200) + `"}`))
	if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 41)) {
		t.Fatalf("error %v echoes the whole of a long value", err)
	}
}

// Rule 2's column half: every column the settings name is a column of the source's result.
func TestCheckColumns(t *testing.T) {
	s, err := Decode([]byte(`{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"sum","column":"amount"},"series":{"column":"region"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckColumns([]string{"day", "amount", "region", "extra"}); err != nil {
		t.Fatalf("CheckColumns with every column = %v", err)
	}
	err = s.CheckColumns([]string{"day", "amount"})
	ce, ok := AsError(err)
	if !ok || ce.Rule != 2 || !strings.Contains(ce.Message, `"region"`) {
		t.Fatalf("CheckColumns without region = %v, want rule 2 naming region", err)
	}
	// Names are matched exactly, as the engine returned them.
	if err := s.CheckColumns([]string{"DAY", "amount", "region"}); err == nil {
		t.Fatal("CheckColumns matched DAY to day")
	}
	if got := s.MissingColumns([]string{"day"}); strings.Join(got, ",") != "amount,region" && strings.Join(got, ",") != "region,amount" {
		t.Fatalf("MissingColumns = %v, want region and amount", got)
	}
}

// Rule 2's kind half: a column whose kind its role or its aggregate cannot use is
// refused with the reason. kinds are the source probe's (number, time, text, bool or
// unknown). A date role takes a column that reads as text or a number until the chart
// fails to run, since a warehouse's dates can come back as text; an unknown kind passes.
func TestCheckKinds(t *testing.T) {
	kinds := map[string]string{"amount": "number", "day": "time", "category": "text", "flag": "bool",
		"code": "number", "score": "number", "masked": "unknown", "yyyymmdd": "number"}
	type check struct {
		raw    string
		failed bool
		want   string // "" passes
	}
	for name, c := range map[string]check{
		"histogram of text": {`{"version":2,"type":"histogram","roles":{"x":[{"column":"category"}]}}`, false,
			`a histogram needs a number column for its x; the column "category" holds text`},
		"histogram of a number": {`{"version":2,"type":"histogram","roles":{"x":[{"column":"amount"}]}}`, false, ""},
		"scatter x of text": {`{"version":2,"type":"scatter","roles":{"x":[{"column":"category"}],"y":[{"column":"amount"}]}}`, false,
			`a scatter chart needs a number column for its x; the column "category" holds text`},
		"scatter y of dates": {`{"version":2,"type":"scatter","roles":{"x":[{"column":"amount"}],"y":[{"column":"day"}]}}`, false,
			`a scatter chart needs a number column for its y; the column "day" holds dates`},
		"bubble size of true or false": {`{"version":2,"type":"bubble","roles":{"x":[{"column":"amount"}],"y":[{"column":"code"}],"size":[{"column":"flag"}]}}`, false,
			`a bubble chart needs a number column for its size; the column "flag" holds true or false`},
		"scatter colour of a number": {`{"version":2,"type":"scatter","roles":{"x":[{"column":"amount"}],"y":[{"column":"score"}],"series":[{"column":"code"}]}}`, false,
			`a scatter chart needs a text or true-or-false column for its colour; the column "code" holds numbers`},
		"scatter colour of text": {`{"version":2,"type":"scatter","roles":{"x":[{"column":"amount"}],"y":[{"column":"score"}],"series":[{"column":"category"}]}}`, false, ""},
		// The builder offers a true-or-false column for a point's colour: two colours.
		"bubble colour of true or false": {`{"version":2,"type":"bubble","roles":{"x":[{"column":"amount"}],"y":[{"column":"score"}],"size":[{"column":"code"}],"series":[{"column":"flag"}]}}`, true, ""},
		"box plot of text": {`{"version":2,"type":"boxplot","roles":{"x":[{"column":"category"}],"y":[{"column":"category"}]}}`, false,
			`a box plot needs a number column for its values; the column "category" holds text`},
		"candlestick price of text": {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"day","grain":"week"}],"y":[{"column":"category"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`, false,
			`a candlestick chart needs a number column for its open, high, low and close; the column "category" holds text`},
		"candlestick price of dates": {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"day","grain":"month"}],"y":[{"column":"amount"},{"column":"day"},{"column":"amount"},{"column":"amount"}]}}`, false,
			`a candlestick chart needs a number column for its open, high, low and close; the column "day" holds dates`},
		"map of dates": {`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"count"}]}}`, false,
			`a choropleth map needs a column of codes, as text or numbers, for its regions; the column "day" holds dates`},
		"map of true or false": {`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"flag"}],"measure":[{"aggregate":"count"}]}}`, false,
			`a choropleth map needs a column of codes, as text or numbers, for its regions; the column "flag" holds true or false`},
		"map of text codes":   {`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"category"}],"measure":[{"aggregate":"count"}]}}`, false, ""},
		"map of number codes": {`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"code"}],"measure":[{"aggregate":"count"}]}}`, false, ""},
		// A date role: text or a number may be a date the warehouse returned as such.
		"calendar of text that ran":     {`{"version":2,"type":"calendar","roles":{"x":[{"column":"category"}],"measure":[{"aggregate":"count"}]}}`, false, ""},
		"calendar of a number that ran": {`{"version":2,"type":"calendar","roles":{"x":[{"column":"yyyymmdd"}],"measure":[{"aggregate":"count"}]}}`, false, ""},
		"calendar of text that failed": {`{"version":2,"type":"calendar","roles":{"x":[{"column":"category"}],"measure":[{"aggregate":"count"}]}}`, true,
			`a calendar heatmap needs a date column for its days; the column "category" holds text`},
		"calendar of true or false": {`{"version":2,"type":"calendar","roles":{"x":[{"column":"flag"}],"measure":[{"aggregate":"count"}]}}`, false,
			`a calendar heatmap needs a date column for its days; the column "flag" holds true or false`},
		"candlestick dates of text that failed": {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"category"}],"y":[{"column":"amount"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`, true,
			`a candlestick chart needs a date column for its dates; the column "category" holds text`},
		"scorecard date of a number that failed": {`{"version":2,"type":"scorecard","compare":{"days":7},"roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"code"}]}}`, true,
			`a scorecard needs a date column for its date; the column "code" holds numbers`},
		"a dated column that ran": {`{"version":2,"type":"calendar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"count"}]}}`, true, ""},
		// Text is tried as a date only where the chart casts it to one (a grain, a
		// calendar's day); elsewhere it would be drawn or compared as text.
		"candlestick dates of text": {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"category"}],"y":[{"column":"amount"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`, false,
			`a candlestick chart needs a date column for its dates; the column "category" holds text`},
		"candlestick dates of text by week that ran": {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"category","grain":"week"}],"y":[{"column":"amount"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`, false, ""},
		"candlestick dates of a number that ran":     {`{"version":2,"type":"candlestick","roles":{"x":[{"column":"yyyymmdd"}],"y":[{"column":"amount"},{"column":"amount"},{"column":"amount"},{"column":"amount"}]}}`, false, ""},
		"scorecard date of text": {`{"version":2,"type":"scorecard","compare":{"days":7},"roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"category"}]}}`, false,
			`a scorecard needs a date column for its date; the column "category" holds text`},
		"scorecard date of a number that ran": {`{"version":2,"type":"scorecard","compare":{"days":7},"roles":{"measure":[{"aggregate":"count"}],"date":[{"column":"yyyymmdd"}]}}`, false, ""},
		// A grain groups a date: the builder offers it only on one.
		"a grain on text that failed": {`{"version":2,"type":"bar","roles":{"x":[{"column":"category","grain":"month"}],"measure":[{"aggregate":"count"}]}}`, true,
			`grouping by month needs a date column; the column "category" holds text`},
		"a grain on text that ran": {`{"version":2,"type":"bar","roles":{"x":[{"column":"category","grain":"month"}],"measure":[{"aggregate":"count"}]}}`, false, ""},
		"a grain on true or false": {`{"version":2,"type":"line","roles":{"x":[{"column":"flag","grain":"day"}],"measure":[{"aggregate":"count"}]}}`, false,
			`grouping by day needs a date column; the column "flag" holds true or false`},
		// A sum or an average adds the column up; a lowest or a highest is drawn as a number
		// everywhere but a table and a pivot table, which show it as the column holds it.
		"sum of text": {`{"version":2,"type":"table","roles":{"columns":[{"column":"day"}],"measure":[{"aggregate":"sum","column":"category"}]}}`, false,
			`a sum needs a number column; the column "category" holds text`},
		"average of dates": {`{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"avg","column":"day"}]}}`, false,
			`an average needs a number column; the column "day" holds dates`},
		"min of text": {`{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"min","column":"category"}]}}`, false,
			`a bar chart draws the lowest of a number column; the column "category" holds text, which a table or a pivot table shows as it is`},
		"max of dates on a scorecard": {`{"version":2,"type":"scorecard","roles":{"measure":[{"aggregate":"max","column":"day"}]}}`, false,
			`a scorecard draws the highest of a number column; the column "day" holds dates, which a table or a pivot table shows as it is`},
		"max of true or false on a bullet": {`{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count"},{"aggregate":"max","column":"flag"}]}}`, false,
			`a bullet chart draws the highest of a number column; the column "flag" holds true or false, which a table or a pivot table shows as it is`},
		"min of text as a range's low": {`{"version":2,"type":"range","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"min","column":"category"},{"aggregate":"max","column":"amount"}]}}`, false,
			`a range bands chart draws the lowest of a number column; the column "category" holds text, which a table or a pivot table shows as it is`},
		"min of a number":         {`{"version":2,"type":"heatmap","roles":{"x":[{"column":"day"}],"y":[{"column":"category"}],"measure":[{"aggregate":"min","column":"amount"}]}}`, false, ""},
		"max of dates on a table": {`{"version":2,"type":"table","roles":{"columns":[{"column":"category"}],"measure":[{"aggregate":"max","column":"day"}]}}`, false, ""},
		"min of text on a pivot":  {`{"version":2,"type":"pivot","roles":{"rows":[{"column":"day"}],"measure":[{"aggregate":"min","column":"category"}]}}`, false, ""},
		"count distinct of text":  {`{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"count_distinct","column":"category"}]}}`, false, ""},
		"count":                   {`{"version":2,"type":"bar","roles":{"x":[{"column":"category"}],"measure":[{"aggregate":"count"}]}}`, true, ""},
		// What the probe could not read passes: the run says.
		"an unknown kind":   {`{"version":2,"type":"histogram","roles":{"x":[{"column":"masked"}]}}`, true, ""},
		"a column not read": {`{"version":2,"type":"histogram","roles":{"x":[{"column":"other"}]}}`, true, ""},
		// Any column fits a role of any kind.
		"bar of true or false": {`{"version":2,"type":"bar","roles":{"x":[{"column":"flag"}],"measure":[{"aggregate":"max","column":"amount"}]}}`, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Decode([]byte(c.raw))
			if err != nil {
				t.Fatalf("Decode(%s) = %v", c.raw, err)
			}
			err = s.CheckKinds(kinds, c.failed)
			if c.want == "" {
				if err != nil {
					t.Fatalf("CheckKinds(failed=%v) = %v, want it to pass", c.failed, err)
				}
				return
			}
			ce, ok := AsError(err)
			if !ok || ce.Rule != 2 {
				t.Fatalf("CheckKinds(failed=%v) = %v, want rule 2: %s", c.failed, err, c.want)
			}
			if ce.Message != c.want {
				t.Errorf("message %q\n           want %q", ce.Message, c.want)
			}
		})
	}
	// A source the probe did not read has no kinds: nothing is refused for them.
	s, err := Decode([]byte(`{"version":2,"type":"histogram","roles":{"x":[{"column":"category"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckKinds(nil, true); err != nil {
		t.Fatalf("CheckKinds(nil) = %v, want it to pass", err)
	}
}

func TestSourceColumnsAndOutputs(t *testing.T) {
	s, err := Decode([]byte(`{"type":"scorecard","measure":{"aggregate":"sum","column":"amount"},"compare":{"column":"day","days":30}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.SourceColumns(), ","); got != "amount,day" {
		t.Fatalf("SourceColumns = %s, want amount,day", got)
	}
	var aliases []string
	for _, o := range s.Outputs() {
		aliases = append(aliases, o.Alias)
	}
	if got := strings.Join(aliases, ","); got != "amount,previous" {
		t.Fatalf("Outputs = %s, want amount,previous", got)
	}
	// A series chart's SQL adds Other and the number of series (§ 14.3); a heatmap,
	// whose cap has no series, and a chart without one do not.
	for raw, want := range map[string]string{
		`{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"},"series":{"column":"region"}}`:     "day,region,count,rsync_other,rsync_series",
		`{"type":"bar","x":{"column":"day"},"measure":{"aggregate":"count"}}`:                                  "day,count",
		`{"type":"heatmap","x":{"column":"day"},"y":{"column":"rsync_other"},"measure":{"aggregate":"count"}}`: "day,rsync_other,count",
	} {
		s, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		aliases = aliases[:0]
		for _, o := range s.Outputs() {
			aliases = append(aliases, o.Alias)
		}
		if got := strings.Join(aliases, ","); got != want {
			t.Errorf("%s: Outputs = %s, want %s", raw, got, want)
		}
	}
	// A combo's second measure, and a histogram's bin width and count.
	for raw, want := range map[string]string{
		`{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"sum","column":"Amount"},{"aggregate":"avg","column":"amount"}]}}`: "day,Amount,amount_2",
		`{"version":2,"type":"bar","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"count"}]}}`:                 "day,amount,count",
		`{"version":2,"type":"histogram","roles":{"x":[{"column":"amount"}]}}`:                                                                                "amount,rsync_bin_width,count",
		`{"version":2,"type":"pie","roles":{"series":[{"column":"region"}],"measure":[{"aggregate":"count"}]}}`:                                               "region,count,rsync_other,rsync_series",
		// D4c: a scatter's points with their size and colour and, last, the rows at each,
		// and a pivot table's fields, measures (a third one named by its place) and total
		// flags.
		`{"version":2,"type":"bubble","roles":{"x":[{"column":"price"}],"y":[{"column":"units"}],"size":[{"column":"margin"}],"series":[{"column":"region"}]}}`:                                                                                                "price,units,margin,region,rsync_other,rsync_series,rsync_rows",
		`{"version":2,"type":"scatter","roles":{"x":[{"column":"price"}],"y":[{"column":"units"}]}}`:                                                                                                                                                           "price,units,rsync_rows",
		`{"version":2,"type":"pivot","roles":{"rows":[{"column":"region"},{"column":"plan"}],"columns":[{"column":"channel"}],"measure":[{"aggregate":"sum","column":"amount"},{"aggregate":"avg","column":"amount"},{"aggregate":"max","column":"amount"}]}}`: "region,plan,channel,amount,amount_2,amount_3,rsync_total_r1,rsync_total_r2,rsync_total_c1",
		// D4d: a treemap's levels, a sankey's ends, a box plot's five numbers and a
		// candlestick's four prices, a range band's low and high.
		`{"version":2,"type":"treemap","roles":{"rows":[{"column":"region"},{"column":"plan"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`:             "region,plan,amount",
		`{"version":2,"type":"sankey","roles":{"x":[{"column":"from"}],"y":[{"column":"to"}],"measure":[{"aggregate":"count"}]}}`:                               "from,to,count",
		`{"version":2,"type":"boxplot","roles":{"x":[{"column":"region"}],"y":[{"column":"amount"}]}}`:                                                          "region,rsync_min,rsync_q1,rsync_median,rsync_q3,rsync_max",
		`{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"o"},{"column":"h"},{"column":"l"},{"column":"c"}]}}`:                 "day,rsync_open,rsync_high,rsync_low,rsync_close",
		`{"version":2,"type":"range","roles":{"x":[{"column":"day"}],"measure":[{"aggregate":"min","column":"amount"},{"aggregate":"max","column":"amount"}]}}`: "day,amount,amount_2",
		`{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"sum","column":"amount"}]},"target":10}`:                                                 "amount",
		// D4e: a map's codes and its measure.
		`{"version":2,"type":"map","map":"world","roles":{"x":[{"column":"country"}],"measure":[{"aggregate":"sum","column":"amount"}]}}`: "country,amount",
	} {
		s, err := Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		aliases = aliases[:0]
		for _, o := range s.Outputs() {
			aliases = append(aliases, o.Alias)
		}
		if got := strings.Join(aliases, ","); got != want {
			t.Errorf("%s: Outputs = %s, want %s", raw, got, want)
		}
	}
	// A box plot's statistics and a candlestick's prices are each read from their y
	// column, which masks them with it (shapeChartResult).
	box, err := Decode([]byte(`{"version":2,"type":"candlestick","roles":{"x":[{"column":"day"}],"y":[{"column":"o"},{"column":"h"},{"column":"l"},{"column":"c"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, o := range box.Outputs() {
		sources = append(sources, o.Role+"="+o.Source)
	}
	if got := strings.Join(sources, ","); got != "the x=day,the open=o,the high=h,the low=l,the close=c" {
		t.Errorf("candlestick output sources = %s", got)
	}
	// Clone copies a bullet's target, sharing nothing.
	bullet, err := Decode([]byte(`{"version":2,"type":"bullet","roles":{"measure":[{"aggregate":"count"}]},"target":10}`))
	if err != nil {
		t.Fatal(err)
	}
	c := bullet.Clone()
	*c.Target = 20
	if *bullet.Target != 10 {
		t.Errorf("Clone shares the target: the original's is now %v", *bullet.Target)
	}
	cnt := &Field{Aggregate: AggCount}
	if cnt.MeasureAlias() != "count" {
		t.Fatalf("count alias = %s", cnt.MeasureAlias())
	}
}
