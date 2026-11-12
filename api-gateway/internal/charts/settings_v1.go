package charts

import "encoding/json"

// settingsV1 is a chart's settings as saved before roles (version 1, which carried no
// version). Decode reads it for one release after migration 147 rewrote every saved
// chart to version 2 (dashboards-design.md § 14.2, decision 14); a proposal or a change
// drafted on the old gateway while the new one starts is the only writer left.
// shared/chart_settings_v1_v2.json pins upgrade to the migration's SQL.
type settingsV1 struct {
	Version     int        `json:"version,omitempty"`
	Type        string     `json:"type"`
	Description string     `json:"description,omitempty"`
	X           *axisV1    `json:"x,omitempty"`
	Y           *axisV1    `json:"y,omitempty"`
	Measure     *measureV1 `json:"measure,omitempty"`
	Series      *seriesV1  `json:"series,omitempty"`
	Columns     []string   `json:"columns,omitempty"`
	Stacked     bool       `json:"stacked,omitempty"`
	Sort        string     `json:"sort,omitempty"`
	Compare     *compareV1 `json:"compare,omitempty"`
}

type axisV1 struct {
	Column string `json:"column"`
	Title  string `json:"title,omitempty"`
}

type measureV1 struct {
	Column    string `json:"column,omitempty"`
	Aggregate string `json:"aggregate"`
	Title     string `json:"title,omitempty"`
	Format    string `json:"format,omitempty"`
	Color     string `json:"color,omitempty"`
}

type seriesV1 struct {
	Column string        `json:"column"`
	Colors []SeriesColor `json:"colors,omitempty"`
}

type compareV1 struct {
	Column string `json:"column"`
	Days   int    `json:"days"`
}

// upgrade returns the same chart as version 2: each of x, y, series and the measure as
// a role of one field, each table column as a field of the columns role, and the
// comparison's column as the date role.
func (v *settingsV1) upgrade() Settings {
	s := Settings{Version: Version, Type: v.Type, Description: v.Description, Roles: map[string][]Field{},
		Stacked: v.Stacked, Sort: v.Sort}
	if v.X != nil {
		s.Roles[RoleX] = []Field{{Column: v.X.Column, Title: v.X.Title}}
	}
	if v.Y != nil {
		s.Roles[RoleY] = []Field{{Column: v.Y.Column, Title: v.Y.Title}}
	}
	if v.Series != nil {
		s.Roles[RoleSeries] = []Field{{Column: v.Series.Column, Colors: v.Series.Colors}}
	}
	for _, c := range v.Columns {
		s.Roles[RoleColumns] = append(s.Roles[RoleColumns], Field{Column: c})
	}
	if v.Measure != nil {
		m := v.Measure
		s.Roles[RoleMeasure] = []Field{{Column: m.Column, Aggregate: m.Aggregate, Title: m.Title, Format: m.Format, Color: m.Color}}
	}
	if v.Compare != nil {
		s.Roles[RoleDate] = []Field{{Column: v.Compare.Column}}
		s.Compare = &Compare{Days: v.Compare.Days}
	}
	return s
}

// AsVersion2 is saved settings JSON as version 2, for a reader that hands settings on
// as they are stored: a version 1 body upgraded, a version 2 one written out again, and
// one that no longer decodes left as it is.
func AsVersion2(raw json.RawMessage) json.RawMessage {
	s, err := Decode(raw)
	if err != nil {
		return raw
	}
	out, err := json.Marshal(s)
	if err != nil {
		return raw
	}
	return out
}
