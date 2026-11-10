package charts

import (
	"regexp"
	"time"
	"unicode/utf8"
)

// Dashboard limits.
const (
	MaxFilters      = 4
	MaxTiles        = 30
	MaxFilterValues = 50
	MaxFilterValue  = 200
	MaxTextTile     = 2000
	maxFilterLabel  = 60
	maxDashName     = 200
	maxDashDesc     = 1000
	GridColumns     = 12
	MaxTileHeight   = 24
	maxTileY        = 999
)

// Filter kinds (§ 4.2). Their values are bound parameters, never pasted into the SQL.
const (
	FilterDateRange = "date_range"
	FilterValues    = "values"
)

// Filter is one dashboard filter. It applies to each tile whose source has Column.
type Filter struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Column string `json:"column"`
}

var filterID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateFilters checks a dashboard's filters.
func ValidateFilters(fs []Filter) error {
	if len(fs) > MaxFilters {
		return refuse(1, "a dashboard has at most %d filters", MaxFilters)
	}
	seen := map[string]bool{}
	for _, f := range fs {
		if !filterID.MatchString(f.ID) {
			return refuse(1, "a filter id is 1 to 32 lower-case letters, digits, _ or -")
		}
		if seen[f.ID] {
			return refuse(1, "two filters have the id %q", f.ID)
		}
		seen[f.ID] = true
		if f.Kind != FilterDateRange && f.Kind != FilterValues {
			return refuse(1, "the filter kind %q is not date_range or values", clip(f.Kind))
		}
		if f.Label == "" {
			return refuse(4, "each filter needs a label")
		}
		if err := plainText("filter label", f.Label, maxFilterLabel); err != nil {
			return err
		}
		if err := columnName("filter column", f.Column); err != nil {
			return err
		}
	}
	return nil
}

// ValidateDashboardText checks a dashboard's name and description.
func ValidateDashboardText(name, description string) error {
	if name == "" {
		return refuse(4, "a dashboard needs a name")
	}
	if err := plainText("name", name, maxDashName); err != nil {
		return err
	}
	return plainText("description", description, maxDashDesc)
}

// ValidateChartName checks a chart's name.
func ValidateChartName(name string) error {
	if name == "" {
		return refuse(4, "a chart needs a name")
	}
	return plainText("name", name, maxDashName)
}

// Tile kinds stored in dashboard_tiles. A scorecard or a table is a chart of that type.
const (
	TileChart = "chart"
	TileText  = "text"
)

// Tile is a tile's kind, content and place on the 12-column grid.
type Tile struct {
	Kind    string `json:"kind"`
	ChartID string `json:"chart_id,omitempty"`
	Body    string `json:"body,omitempty"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

// ValidateTiles checks the tiles' kinds, text and grid places.
func ValidateTiles(ts []Tile) error {
	if len(ts) > MaxTiles {
		return refuse(1, "a dashboard has at most %d tiles", MaxTiles)
	}
	for i, t := range ts {
		switch t.Kind {
		case TileChart:
			if t.ChartID == "" || t.Body != "" {
				return refuse(1, "tile %d: a chart tile names a chart and has no text", i+1)
			}
		case TileText:
			if t.ChartID != "" {
				return refuse(1, "tile %d: a text tile names no chart", i+1)
			}
			if t.Body == "" {
				return refuse(4, "tile %d: a text tile needs text", i+1)
			}
			if err := textTile(t.Body); err != nil {
				return err
			}
		default:
			return refuse(1, "tile %d: the kind %q is not chart or text", i+1, clip(t.Kind))
		}
		if t.X < 0 || t.W < 1 || t.X+t.W > GridColumns {
			return refuse(1, "tile %d does not fit the %d-column grid", i+1, GridColumns)
		}
		if t.Y < 0 || t.Y > maxTileY || t.H < 1 || t.H > MaxTileHeight {
			return refuse(1, "tile %d: its row is 0 to %d and its height 1 to %d", i+1, maxTileY, MaxTileHeight)
		}
	}
	return nil
}

// textTile is a text tile's body: plain text, drawn escaped, with line breaks.
func textTile(body string) error {
	if !utf8.ValidString(body) {
		return refuse(4, "a text tile is not valid text")
	}
	if n := utf8.RuneCountInString(body); n > MaxTextTile {
		return refuse(4, "a text tile is %d characters; the most is %d", n, MaxTextTile)
	}
	for _, r := range body {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return refuse(4, "a text tile is plain text: no control characters")
		}
	}
	return nil
}

// FilterValue is what a viewer picked for one filter: a date range (From and To, both
// days, To included) or a list of values.
type FilterValue struct {
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
	Values []string `json:"values,omitempty"`
}

// Bound is one filter, checked and ready to bind: a half-open time range [From, To) or
// a value list. ID is the filter's own id: two filters can share a column.
type Bound struct {
	ID     string
	Column string
	Kind   string
	From   time.Time
	To     time.Time
	Values []string
}

// BindFilters checks the viewer's picks against the dashboard's filters. A filter with
// no pick is left out; a pick for an unknown filter is refused.
func BindFilters(fs []Filter, picks map[string]FilterValue) ([]Bound, error) {
	byID := map[string]Filter{}
	for _, f := range fs {
		byID[f.ID] = f
	}
	var out []Bound
	for _, f := range fs {
		p, ok := picks[f.ID]
		if !ok {
			continue
		}
		switch f.Kind {
		case FilterDateRange:
			if p.From == "" && p.To == "" {
				continue
			}
			from, err1 := time.Parse("2006-01-02", p.From)
			to, err2 := time.Parse("2006-01-02", p.To)
			if err1 != nil || err2 != nil {
				return nil, refuse(1, "the %s filter takes two dates, YYYY-MM-DD", f.Label)
			}
			if to.Before(from) {
				return nil, refuse(1, "the %s filter ends before it starts", f.Label)
			}
			out = append(out, Bound{ID: f.ID, Column: f.Column, Kind: f.Kind, From: from, To: to.AddDate(0, 0, 1)})
		case FilterValues:
			if len(p.Values) == 0 {
				continue
			}
			if len(p.Values) > MaxFilterValues {
				return nil, refuse(1, "the %s filter takes at most %d values", f.Label, MaxFilterValues)
			}
			for _, v := range p.Values {
				if !utf8.ValidString(v) || utf8.RuneCountInString(v) > MaxFilterValue {
					return nil, refuse(1, "a value of the %s filter is longer than %d characters", f.Label, MaxFilterValue)
				}
			}
			out = append(out, Bound{ID: f.ID, Column: f.Column, Kind: f.Kind, Values: p.Values})
		}
	}
	for id := range picks {
		if _, ok := byID[id]; !ok {
			return nil, refuse(1, "the dashboard has no filter %q", clip(id))
		}
	}
	return out, nil
}
