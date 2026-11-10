package charts

import (
	"strings"
	"testing"
	"time"
)

// A dashboard's own checks (dashboards-design.md § 4.2): its filters, its text, its
// tiles on the 12-column grid, and a viewer's picks, bound as parameters.

func wantRule(t *testing.T, err error, rule int, say string) {
	t.Helper()
	ce, ok := AsError(err)
	if !ok {
		t.Fatalf("error = %v, want a rule %d refusal saying %q", err, rule, say)
	}
	if ce.Rule != rule || !strings.Contains(ce.Message, say) {
		t.Fatalf("refused by rule %d (%q), want rule %d saying %q", ce.Rule, ce.Message, rule, say)
	}
}

func TestValidateFilters(t *testing.T) {
	ok := []Filter{
		{ID: "day", Kind: FilterDateRange, Label: "Day", Column: "order_date"},
		{ID: "region-1", Kind: FilterValues, Label: "Region", Column: "region"},
	}
	if err := ValidateFilters(ok); err != nil {
		t.Fatalf("ValidateFilters(valid) = %v", err)
	}
	five := make([]Filter, MaxFilters+1)
	for i := range five {
		five[i] = Filter{ID: string(rune('a' + i)), Kind: FilterValues, Label: "L", Column: "c"}
	}
	for name, c := range map[string]struct {
		fs   []Filter
		rule int
		say  string
	}{
		"too many":      {five, 1, "at most 4 filters"},
		"id upper case": {[]Filter{{ID: "Day", Kind: FilterValues, Label: "L", Column: "c"}}, 1, "filter id"},
		"id with space": {[]Filter{{ID: "a b", Kind: FilterValues, Label: "L", Column: "c"}}, 1, "filter id"},
		"id too long":   {[]Filter{{ID: strings.Repeat("a", 33), Kind: FilterValues, Label: "L", Column: "c"}}, 1, "filter id"},
		"id twice":      {[]Filter{ok[0], ok[0]}, 1, "two filters"},
		"unknown kind":  {[]Filter{{ID: "a", Kind: "sql", Label: "L", Column: "c"}}, 1, "not date_range or values"},
		"no label":      {[]Filter{{ID: "a", Kind: FilterValues, Column: "c"}}, 4, "needs a label"},
		"markup label":  {[]Filter{{ID: "a", Kind: FilterValues, Label: "<b>", Column: "c"}}, 4, "no < or >"},
		"no column":     {[]Filter{{ID: "a", Kind: FilterValues, Label: "L"}}, 1, "is empty"},
	} {
		t.Run(name, func(t *testing.T) { wantRule(t, ValidateFilters(c.fs), c.rule, c.say) })
	}
}

func TestValidateDashboardText(t *testing.T) {
	if err := ValidateDashboardText("Revenue", "By day, by region"); err != nil {
		t.Fatalf("valid text = %v", err)
	}
	wantRule(t, ValidateDashboardText("", ""), 4, "needs a name")
	wantRule(t, ValidateDashboardText("{a}", ""), 4, "no { or }")
	wantRule(t, ValidateDashboardText("ok", "<script>"), 4, "no < or >")
	wantRule(t, ValidateChartName(""), 4, "needs a name")
}

func TestValidateTiles(t *testing.T) {
	ok := []Tile{
		{Kind: TileChart, ChartID: "c1", X: 0, Y: 0, W: 6, H: 8},
		{Kind: TileChart, ChartID: "c2", X: 6, Y: 0, W: 6, H: 8},
		// A text tile is drawn escaped, so a < is only text; line breaks are kept.
		{Kind: TileText, Body: "Revenue < target?\nSee the notes.", X: 0, Y: 8, W: 12, H: 2},
	}
	if err := ValidateTiles(ok); err != nil {
		t.Fatalf("ValidateTiles(valid) = %v", err)
	}
	many := make([]Tile, MaxTiles+1)
	for i := range many {
		many[i] = Tile{Kind: TileChart, ChartID: "c", W: 1, H: 1}
	}
	for name, c := range map[string]struct {
		t    Tile
		rule int
		say  string
	}{
		"chart without id":   {Tile{Kind: TileChart, W: 1, H: 1}, 1, "names a chart"},
		"chart with text":    {Tile{Kind: TileChart, ChartID: "c", Body: "x", W: 1, H: 1}, 1, "has no text"},
		"text with chart":    {Tile{Kind: TileText, ChartID: "c", Body: "x", W: 1, H: 1}, 1, "names no chart"},
		"empty text":         {Tile{Kind: TileText, W: 1, H: 1}, 4, "needs text"},
		"text too long":      {Tile{Kind: TileText, Body: strings.Repeat("a", MaxTextTile+1), W: 1, H: 1}, 4, "the most is"},
		"text control":       {Tile{Kind: TileText, Body: "a\x00b", W: 1, H: 1}, 4, "control characters"},
		"unknown kind":       {Tile{Kind: "iframe", W: 1, H: 1}, 1, "not chart or text"},
		"past the grid":      {Tile{Kind: TileChart, ChartID: "c", X: 8, W: 6, H: 1}, 1, "12-column grid"},
		"negative x":         {Tile{Kind: TileChart, ChartID: "c", X: -1, W: 2, H: 1}, 1, "12-column grid"},
		"zero width":         {Tile{Kind: TileChart, ChartID: "c", W: 0, H: 1}, 1, "12-column grid"},
		"too tall":           {Tile{Kind: TileChart, ChartID: "c", W: 1, H: 25}, 1, "height 1 to 24"},
		"zero height":        {Tile{Kind: TileChart, ChartID: "c", W: 1, H: 0}, 1, "height 1 to 24"},
		"row past the limit": {Tile{Kind: TileChart, ChartID: "c", Y: 1000, W: 1, H: 1}, 1, "its row is 0 to 999"},
	} {
		t.Run(name, func(t *testing.T) { wantRule(t, ValidateTiles([]Tile{c.t}), c.rule, c.say) })
	}
	wantRule(t, ValidateTiles(many), 1, "at most 30 tiles")
}

func TestBindFilters(t *testing.T) {
	fs := []Filter{
		{ID: "day", Kind: FilterDateRange, Label: "Day", Column: "order_date"},
		{ID: "region", Kind: FilterValues, Label: "Region", Column: "region"},
	}
	got, err := BindFilters(fs, map[string]FilterValue{
		"day":    {From: "2026-10-01", To: "2026-10-05"},
		"region": {Values: []string{"EU", "it's; drop table x"}},
	})
	if err != nil {
		t.Fatalf("BindFilters = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bound %d filters, want 2", len(got))
	}
	// The range is half-open: To is the day after the last day picked.
	if !got[0].From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !got[0].To.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("range = [%v, %v), want [2026-10-01, 2026-10-06)", got[0].From, got[0].To)
	}
	// A value is kept as given: it is bound as a parameter, never pasted into the SQL.
	if got[1].Column != "region" || got[1].Values[1] != "it's; drop table x" {
		t.Fatalf("values = %+v", got[1])
	}

	// No pick, or an empty one, is no filter.
	if got, err := BindFilters(fs, map[string]FilterValue{"day": {}, "region": {}}); err != nil || len(got) != 0 {
		t.Fatalf("empty picks = %v, %v; want none bound", got, err)
	}

	tooMany := make([]string, MaxFilterValues+1)
	for i := range tooMany {
		tooMany[i] = "v"
	}
	for name, c := range map[string]struct {
		picks map[string]FilterValue
		say   string
	}{
		"unknown filter":  {map[string]FilterValue{"secret": {Values: []string{"x"}}}, "no filter"},
		"one day only":    {map[string]FilterValue{"day": {From: "2026-10-01"}}, "two dates"},
		"not a date":      {map[string]FilterValue{"day": {From: "2026-10-01", To: "now()"}}, "two dates"},
		"backwards":       {map[string]FilterValue{"day": {From: "2026-10-05", To: "2026-10-01"}}, "ends before it starts"},
		"too many values": {map[string]FilterValue{"region": {Values: tooMany}}, "at most 50 values"},
		"value too long":  {map[string]FilterValue{"region": {Values: []string{strings.Repeat("a", MaxFilterValue+1)}}}, "longer than 200"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BindFilters(fs, c.picks)
			wantRule(t, err, 1, c.say)
		})
	}
}
