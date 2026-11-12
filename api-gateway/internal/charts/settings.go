// Package charts holds a saved chart's settings and a dashboard's layout, and the
// checks the gateway runs on both before anything is saved (dashboards-design.md
// § 3.2). It has no database and no engine: the handlers run the source once for
// the column check (rule 2's first half) and write the SQL from the settings.
//
// The settings are rsync.ai's own format, never an ECharts option: the type, the
// columns, the aggregate, plain-text titles and colour token names. The frontend's
// translators (frontend/src/components/charts) turn them, the rows and the theme into
// the drawing.
package charts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version is the settings format this package writes; charts.settings_version. Decode
// still reads version 1, the shape before roles, for one release (decision 14).
const Version = 2

// MaxSettingsBytes caps a chart's settings JSON (rule 5).
const MaxSettingsBytes = 8 * 1024

// MaxRows is the most rows a chart reads; a query that reaches it is drawn with "first
// 10,000 rows drawn" (§ 4.1). The SQL asks for one more, to know.
const MaxRows = 10000

// Chart types a translator draws (rule 1). Never gauge, radar, 3D or word clouds (§ 7.2).
const (
	TypeBar       = "bar"
	TypeHBar      = "hbar"
	TypeLine      = "line"
	TypeArea      = "area"
	TypePie       = "pie"
	TypeDonut     = "donut"
	TypeWaterfall = "waterfall"
	TypeFunnel    = "funnel"
	TypeHistogram = "histogram"
	TypeHeatmap   = "heatmap"
	TypeScatter   = "scatter"
	TypeBubble    = "bubble"
	TypeScorecard = "scorecard"
	TypeTable     = "table"
	TypePivot     = "pivot"
	// Tier 2 (§ 14.8).
	TypeTreemap     = "treemap"
	TypeSunburst    = "sunburst"
	TypeSankey      = "sankey"
	TypeCalendar    = "calendar"
	TypeBullet      = "bullet"
	TypeBoxplot     = "boxplot"
	TypeCandlestick = "candlestick"
	TypeRange       = "range"
	// Tier 3 (§ 14.8).
	TypeMap = "map"
)

// The shapes a map chart fills (decision 14, open question 11): world countries by their
// ISO 3166-1 code and US states by their ISO 3166-2 code, shipped in the image, so no
// address or row is looked up outside the install.
const (
	MapWorld    = "world"
	MapUSStates = "us_states"
)

// Maps lists them, as the chart page offers them.
var Maps = []string{MapWorld, MapUSStates}

// Aggregates (rule 2's second half).
const (
	AggCount         = "count"
	AggCountDistinct = "count_distinct"
	AggSum           = "sum"
	AggAvg           = "avg"
	AggMin           = "min"
	AggMax           = "max"
)

// Aggregates lists every aggregate a measure may use.
var Aggregates = []string{AggCount, AggCountDistinct, AggSum, AggAvg, AggMin, AggMax}

// Sorts a chart may ask for.
const (
	SortXAsc      = "x_asc"
	SortXDesc     = "x_desc"
	SortValueAsc  = "value_asc"
	SortValueDesc = "value_desc"
)

// Grains are the date grains an axis may group by; unset groups by the raw value. A
// week starts on the connection's week start and a quarter or year on its fiscal
// year's first month (§ 14.3).
const (
	GrainDay     = "day"
	GrainWeek    = "week"
	GrainMonth   = "month"
	GrainQuarter = "quarter"
	GrainYear    = "year"
)

var Grains = []string{GrainDay, GrainWeek, GrainMonth, GrainQuarter, GrainYear}

// NumberFormats are the ways a measure's numbers may be written, as chart-settings.ts's
// NumberFormat; percent writes a share (0.081) as 8.1%. Unset picks one by aggregate.
var NumberFormats = []string{"integer", "decimal", "compact", "percent"}

var formats = func() map[string]bool {
	m := map[string]bool{"": true}
	for _, f := range NumberFormats {
		m[f] = true
	}
	return m
}()

// SeriesTokens are the series colour tokens of § 2.1, in their fixed order; OtherToken
// is the grey every series past the seventh folds into. A chart names tokens, never a
// colour value (rule 3).
var SeriesTokens = []string{"series-1", "series-2", "series-3", "series-4", "series-5", "series-6", "series-7"}

// OtherToken is the token of the folded "Other" series.
const OtherToken = "other"

// CompareDays are the periods a scorecard may compare against.
var CompareDays = map[int]bool{1: true, 7: true, 30: true, 90: true, 365: true}

// Marks a combo's measure may be drawn as, and the axes it may read against; unset is
// the chart's own type and the left axis.
const (
	MarkBar  = "bar"
	MarkLine = "line"
	MarkArea = "area"

	AxisLeft  = "left"
	AxisRight = "right"
)

var (
	Marks = []string{MarkBar, MarkLine, MarkArea}
	Axes  = []string{AxisLeft, AxisRight}
)

// A histogram's bins: Bins of equal width between the column's least and greatest value
// (DefaultBins when neither is set), or bins Width wide from zero. A width is held
// between minBinWidth and maxBinWidth, which the SQL writes as a plain decimal.
const (
	DefaultBins = 20
	MaxBins     = 100
	minBinWidth = 1e-9
	maxBinWidth = 1e15
)

// Text limits.
const (
	maxTitle       = 120
	maxDescription = 500
	maxColumn      = 128
	maxTableCols   = 20
	maxSeriesColor = 7
	// maxTarget bounds a bullet chart's typed target, as maxBinWidth bounds a bin width.
	maxTarget = 1e15
)

// Settings is a chart's saved settings, version 2: its fields by the role each fills,
// as the type's registry entry names the roles.
type Settings struct {
	Version     int                `json:"version"`
	Type        string             `json:"type"`
	Description string             `json:"description,omitempty"`
	Roles       map[string][]Field `json:"roles"`
	Stacked     bool               `json:"stacked,omitempty"`
	// Percent draws a stacked chart's series as shares of each x's whole (100% stacking).
	Percent bool     `json:"percent,omitempty"`
	Sort    string   `json:"sort,omitempty"`
	Compare *Compare `json:"compare,omitempty"`
	// Sparkline draws a scorecard's current period under its number, a point a day for 7
	// and 30 days, a week for 90 and a month for 365.
	Sparkline bool `json:"sparkline,omitempty"`
	// Target is a bullet chart's typed target; its other target is a second measure, and
	// it has exactly one of the two.
	Target *float64 `json:"target,omitempty"`
	// Map is the shapes a map chart's x codes name, world or us_states; a map has one and
	// no other type has any.
	Map string `json:"map,omitempty"`
}

// Field is one column in a role: an axis (column, title, a date grain, a histogram's
// bins), the measure (what is counted or summed, its title, number format and colour
// token, and a combo's mark and axis), the series (with Colors pinning a token to a
// series by its name, so a filter that drops a series never repaints the others, § 2.1),
// a table column, or a scorecard's date.
type Field struct {
	Column    string        `json:"column,omitempty"`
	Aggregate string        `json:"aggregate,omitempty"`
	Grain     string        `json:"grain,omitempty"`
	Title     string        `json:"title,omitempty"`
	Format    string        `json:"format,omitempty"`
	Color     string        `json:"color,omitempty"`
	Colors    []SeriesColor `json:"colors,omitempty"`
	Mark      string        `json:"mark,omitempty"`
	Axis      string        `json:"axis,omitempty"`
	Bins      int           `json:"bins,omitempty"`
	Width     float64       `json:"width,omitempty"`
}

// SeriesColor is one series' colour token.
type SeriesColor struct {
	Value string `json:"value"`
	Color string `json:"color"`
}

// Compare is a scorecard's earlier period: the number for the last Days days of its date
// column against the Days before them.
type Compare struct {
	Days int `json:"days"`
}

// One returns a role's first field, or nil when the role is empty.
func (s *Settings) One(role string) *Field {
	if fs := s.Roles[role]; len(fs) > 0 {
		return &fs[0]
	}
	return nil
}

// ColumnsOf lists the column of each of a role's fields.
func (s *Settings) ColumnsOf(role string) []string {
	var out []string
	for _, f := range s.Roles[role] {
		out = append(out, f.Column)
	}
	return out
}

// Clone is a copy of s that shares nothing with it.
func (s *Settings) Clone() *Settings {
	c := *s
	c.Roles = make(map[string][]Field, len(s.Roles))
	for role, fields := range s.Roles {
		c.Roles[role] = make([]Field, len(fields))
		for i, f := range fields {
			f.Colors = slices.Clone(f.Colors)
			c.Roles[role][i] = f
		}
	}
	if s.Compare != nil {
		cmp := *s.Compare
		c.Compare = &cmp
	}
	if s.Target != nil {
		t := *s.Target
		c.Target = &t
	}
	return &c
}

// Error is a refused setting, said in words a person (or the model drafting a
// proposal) can act on. Rule is the § 3.2 rule it breaks.
type Error struct {
	Rule    int
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(rule int, format string, a ...interface{}) *Error {
	return &Error{Rule: rule, Message: fmt.Sprintf(format, a...)}
}

// AsError returns err's *Error, if it is one.
func AsError(err error) (*Error, bool) {
	var ce *Error
	ok := errors.As(err, &ce)
	return ce, ok
}

// Decode reads settings JSON and runs rules 1, 2 (the aggregate list), 3, 4 and 5.
// Rule 2's column half needs the source's columns: CheckColumns. Version 1 settings,
// which carry no version, are read with their own unknown-key check and returned as
// version 2.
func Decode(raw []byte) (*Settings, error) {
	if len(raw) > MaxSettingsBytes {
		return nil, refuse(5, "the chart settings are %d bytes; the most is %d", len(raw), MaxSettingsBytes)
	}
	version := 1
	var probe struct {
		Version json.RawMessage `json:"version"`
	}
	if json.NewDecoder(bytes.NewReader(raw)).Decode(&probe) == nil && len(probe.Version) > 0 {
		if err := json.Unmarshal(probe.Version, &version); err != nil {
			return nil, refuse(1, "the chart settings are not valid: version must be a number")
		}
	}
	var s Settings
	switch version {
	case 1:
		var v1 settingsV1
		if err := decodeStrict(raw, &v1); err != nil {
			return nil, err
		}
		s = v1.upgrade()
	case Version:
		if err := decodeStrict(raw, &s); err != nil {
			return nil, err
		}
	default:
		return nil, refuse(1, "the chart settings are version %d; rsync.ai reads 1 and %d", version, Version)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// decodeStrict decodes exactly one JSON value into v, refusing unknown keys.
func decodeStrict(raw []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return refuse(1, "the chart settings are not valid: %s", decodeMessage(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return refuse(1, "the chart settings are not valid: more than one JSON value")
	}
	return nil
}

// decodeMessage names the unknown key without echoing anything else of the body.
func decodeMessage(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		return "unknown setting " + strings.TrimPrefix(msg, "json: ") + "; a chart holds only rsync.ai's own settings, never a chart library's options"
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Field != "" {
		return fmt.Sprintf("%s must be %s", te.Field, jsonKind(te.Type))
	}
	return "not a JSON object"
}

// aggregateRefused refuses a measure's aggregate rsync.ai does not offer, or none.
func aggregateRefused(aggregate string, offered []string) error {
	if aggregate == "" {
		return refuse(2, "a measure needs an aggregate: one of %s", strings.Join(offered, ", "))
	}
	return refuse(2, "the aggregate %q is not one rsync.ai offers; use one of %s", clip(aggregate), strings.Join(offered, ", "))
}

// jsonKind names what a setting holds as its JSON reads, never as its Go type.
func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	}
	return "a JSON object"
}

// plainNumber writes a bound as a person reads it, every digit and the whole part
// grouped by threes ("1,000,000,000,000,000", "0.000000001"), never as 1e+15.
func plainNumber(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(v, 'f', -1, 64), ".")
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	if frac != "" {
		whole += "." + frac
	}
	return sign + whole
}

// Validate runs every rule but rule 5 and the column half of rule 2 on decoded
// settings.
func (s *Settings) Validate() error {
	spec := Spec(s.Type)
	if spec == nil {
		return refuse(1, "the chart type %q is not one rsync.ai draws; use one of %s", clip(s.Type), strings.Join(Types, ", "))
	}
	if s.Version != Version {
		return refuse(1, "the chart settings are version %d; rsync.ai writes %d", s.Version, Version)
	}
	if err := plainText("description", s.Description, maxDescription); err != nil {
		return err
	}
	if s.Sort != "" && !contains(allSorts, s.Sort) {
		// The sorts named are the type's own, so the one picked from them is taken.
		if len(spec.Sorts) == 0 {
			return refuse(1, "the sort %q is not one rsync.ai offers; %s has no sort", clip(s.Sort), aChart(s.Type))
		}
		return refuse(1, "the sort %q is not one of %s", clip(s.Sort), strings.Join(spec.Sorts, ", "))
	}
	if c := s.Compare; c != nil && !CompareDays[c.Days] {
		return refuse(1, "a scorecard compares against 1, 7, 30, 90 or 365 days, not %d", c.Days)
	}
	if err := s.validateShape(spec); err != nil {
		return err
	}
	for _, role := range roleOrder {
		for _, f := range s.Roles[role] {
			if err := validateField(role, f); err != nil {
				return err
			}
		}
	}
	return s.validateAliases()
}

// validateField checks one field's values: a column name, plain-text titles, an
// aggregate from the list, a number format, a date grain and colour tokens.
func validateField(role string, f Field) error {
	field := func(attr string) string {
		if role == RoleColumns {
			return RoleColumns
		}
		return role + "." + attr
	}
	if role == RoleMeasure {
		if !contains(Aggregates, f.Aggregate) {
			return aggregateRefused(f.Aggregate, Aggregates)
		}
		if f.Column == "" && f.Aggregate != AggCount {
			return refuse(2, "%s needs a column to measure", f.Aggregate)
		}
	}
	if f.Column != "" || role != RoleMeasure {
		if err := columnName(field(AttrColumn), f.Column); err != nil {
			return err
		}
	}
	if err := plainText(field(AttrTitle), f.Title, maxTitle); err != nil {
		return err
	}
	if !formats[f.Format] {
		return refuse(1, "the number format %q is not one of %s", clip(f.Format), strings.Join(NumberFormats, ", "))
	}
	if f.Grain != "" && !contains(Grains, f.Grain) {
		return refuse(1, "the date grain %q is not one of %s", clip(f.Grain), strings.Join(Grains, ", "))
	}
	if f.Color != "" && !contains(SeriesTokens, f.Color) {
		return refuse(3, "the colour %q is not a colour token; use series-1 to series-7", clip(f.Color))
	}
	if f.Mark != "" && !contains(Marks, f.Mark) {
		return refuse(1, "the mark %q is not one of %s", clip(f.Mark), strings.Join(Marks, ", "))
	}
	if f.Axis != "" && !contains(Axes, f.Axis) {
		return refuse(1, "the axis %q is not one of %s", clip(f.Axis), strings.Join(Axes, ", "))
	}
	if f.Bins < 0 || f.Bins > MaxBins {
		return refuse(1, "a histogram has 1 to %d bins, not %d", MaxBins, f.Bins)
	}
	if f.Width != 0 && !(f.Width >= minBinWidth && f.Width <= maxBinWidth) {
		return refuse(1, "a histogram's bin width is a number from %s to %s", plainNumber(minBinWidth), plainNumber(maxBinWidth))
	}
	if f.Bins > 0 && f.Width > 0 {
		return refuse(1, "a histogram takes a number of bins or a bin width, not both")
	}
	if len(f.Colors) > maxSeriesColor {
		return refuse(3, "at most %d series take their own colour; the rest are Other", maxSeriesColor)
	}
	seenV, seenC := map[string]bool{}, map[string]bool{}
	for _, c := range f.Colors {
		if !contains(SeriesTokens, c.Color) {
			return refuse(3, "the colour %q is not a colour token; use series-1 to series-7", clip(c.Color))
		}
		if err := plainText(field(AttrColors)+".value", c.Value, maxTitle); err != nil {
			return err
		}
		if seenV[c.Value] {
			return refuse(3, "the series %q has two colours", clip(c.Value))
		}
		if seenC[c.Color] {
			return refuse(3, "%s is given to two series; each series needs its own colour", c.Color)
		}
		seenV[c.Value], seenC[c.Color] = true, true
	}
	return nil
}

// roleNeed and roleLack name a role in "a bar chart needs …" and "a heatmap has
// no …".
var (
	roleNeed = map[string]string{RoleX: "an x column", RoleY: "a y column", RoleSize: "a size column", RoleSeries: "a series column",
		RoleRows: "at least one row field", RoleColumns: "at least one column", RoleMeasure: "a measure", RoleDate: "a date column"}
	roleLack = map[string]string{RoleX: "x column", RoleY: "y column", RoleSize: "size column", RoleSeries: "series column",
		RoleRows: "row fields", RoleColumns: "table columns", RoleMeasure: "measure", RoleDate: "date column"}
)

// needOf is roleNeed for a type whose role is something more particular: a treemap's
// levels, a sankey's ends, a candlestick's prices.
func needOf(t, role string) string {
	switch {
	case (t == TypeTreemap || t == TypeSunburst) && role == RoleRows:
		return "at least one level"
	case t == TypeSankey && role == RoleX:
		return "a source column (x)"
	case t == TypeSankey && role == RoleY:
		return "a target column (y)"
	case (t == TypeCalendar || t == TypeCandlestick) && role == RoleX:
		return "a date column (x)"
	case t == TypeBoxplot && role == RoleX:
		return "a group column (x)"
	case t == TypeBoxplot && role == RoleY:
		return "a number column (y)"
	case t == TypeCandlestick && role == RoleY:
		return "open, high, low and close columns (y)"
	case t == TypeRange && role == RoleMeasure:
		return "a low and a high measure"
	case t == TypeMap && role == RoleX:
		return "a column of ISO codes (x)"
	}
	return roleNeed[role]
}

// lackOf is roleLack for a type: a pivot table's columns are its column fields, a
// treemap's rows its levels, and a role of several fields is plural.
func lackOf(t, role string, plural bool) string {
	switch {
	case t == TypePivot && role == RoleColumns:
		return "column fields"
	case (t == TypeTreemap || t == TypeSunburst) && role == RoleRows:
		if plural {
			return "levels"
		}
		return "level"
	case plural && role == RoleMeasure:
		return "measures"
	case plural && role == RoleY:
		return "y columns"
	}
	return roleLack[role]
}

// KeepFrom copies from old each attribute in attrs that f leaves empty. It goes through
// the field's JSON, whose keys are the attribute names (registry_test.go holds them to
// each other), so an attribute added to Field is kept with no change here; nothing is
// shared with old.
func (f *Field) KeepFrom(old *Field, attrs []string) {
	have, from := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	// A Field is strings, numbers and a slice of strings, and Validate refuses a width
	// that is not a number (a NaN would not marshal): marshalling it cannot fail.
	b, _ := json.Marshal(f)
	_ = json.Unmarshal(b, &have)
	b, _ = json.Marshal(old)
	_ = json.Unmarshal(b, &from)
	for _, a := range attrs {
		if _, set := have[a]; !set {
			if v, ok := from[a]; ok {
				have[a] = v
			}
		}
	}
	b, _ = json.Marshal(have)
	*f = Field{}
	_ = json.Unmarshal(b, f)
}

// fieldAttrs lists the attributes a field sets.
func fieldAttrs(f Field) []string {
	var out []string
	for _, a := range []struct {
		name string
		set  bool
	}{{AttrColumn, f.Column != ""}, {AttrAggregate, f.Aggregate != ""}, {AttrGrain, f.Grain != ""},
		{AttrTitle, f.Title != ""}, {AttrFormat, f.Format != ""}, {AttrColor, f.Color != ""}, {AttrColors, len(f.Colors) > 0},
		{AttrMark, f.Mark != ""}, {AttrAxis, f.Axis != ""}, {AttrBins, f.Bins != 0}, {AttrWidth, f.Width != 0}} {
		if a.set {
			out = append(out, a.name)
		}
	}
	return out
}

// validateShape holds each type to the roles its registry entry names, so a saved chart
// never carries a setting its translator ignores; a type with no entry is refused before
// this.
func (s *Settings) validateShape(spec *TypeSpec) error {
	t := s.Type
	need := func(ok bool, what string) error {
		if !ok {
			return refuse(1, "%s needs %s", aChart(t), what)
		}
		return nil
	}
	not := func(set bool, what string) error {
		if set {
			return refuse(1, "%s has no %s", aChart(t), what)
		}
		return nil
	}
	for _, r := range spec.Roles {
		if err := need(len(s.Roles[r.Name]) >= r.Min, needOf(t, r.Name)); err != nil {
			return err
		}
	}
	if err := not(s.Compare != nil && !spec.Compare, "comparison period"); err != nil {
		return err
	}
	for _, role := range sortedRoles(s.Roles) {
		r := spec.Role(role)
		if r == nil {
			what, ok := roleLack[role]
			if !ok {
				return refuse(1, "the chart settings are not valid: unknown role %q", clip(role))
			}
			if role == RoleSeries && t == TypeHeatmap {
				what += "; its colour is the measure"
			}
			return not(true, what)
		}
		fields := s.Roles[role]
		if len(fields) > r.Max {
			if role == RoleColumns && t == TypeTable {
				return refuse(1, "a table shows at most %d columns", r.Max)
			}
			return refuse(1, "%s takes at most %d %s", aChart(t), r.Max, lackOf(t, role, r.Max > 1))
		}
		for _, f := range fields {
			for _, a := range fieldAttrs(f) {
				if !contains(r.Attrs, a) {
					return refuse(1, "%s.%s is not a setting of %s; its %s takes %s", role, a, aChart(t), lackOf(t, role, false), strings.Join(r.Attrs, ", "))
				}
			}
		}
	}
	if err := s.validateMeasures(spec); err != nil {
		return err
	}
	if err := s.validateTarget(); err != nil {
		return err
	}
	if err := s.validateMap(); err != nil {
		return err
	}
	valueSort := s.Sort == SortValueAsc || s.Sort == SortValueDesc
	hasSeries := len(s.Roles[RoleSeries]) > 0
	stacking := "stacking"
	if t == TypeLine {
		stacking = "stacking; use an area chart"
	}
	sortWhat := "sort"
	if valueSort && len(spec.Sorts) > 0 {
		sortWhat = "value sort; use " + strings.Join(spec.Sorts, " or ")
	}
	// A line never stacks, so its 100% points to an area chart, as its stacking does; a type
	// that never stacks is not told to stack.
	percent := "100% stacking unless it is stacked"
	switch {
	case t == TypeLine:
		percent = "100% stacking; use an area chart"
	case !spec.Stackable:
		percent = "100% stacking"
	}
	for _, e := range []error{
		not(s.Stacked && !spec.Stackable, stacking),
		not(s.Stacked && !hasSeries, "stacking without a series column"),
		not(s.Percent && (!s.Stacked || !spec.Stackable), percent),
		not(s.Sort != "" && !contains(spec.Sorts, s.Sort), sortWhat),
		not(valueSort && len(s.Roles[RoleMeasure]) == 0, "value sort without a measure"),
		not(spec.Compare && s.Compare == nil && len(s.Roles[RoleDate]) > 0, "date column without a comparison period"),
		need(!spec.Compare || s.Compare == nil || len(s.Roles[RoleDate]) > 0, "a date column to compare by"),
		not(s.Sparkline && !spec.Compare, "sparkline"),
		need(!s.Sparkline || s.Compare != nil, "a comparison period to draw a sparkline over"),
		not(s.Sparkline && s.Compare != nil && s.Compare.Days == 1, "sparkline over one day; compare 7 days or more"),
	} {
		if e != nil {
			return e
		}
	}
	return nil
}

// validateMeasures holds the measures to the type's aggregates, no two of them the same,
// and a combo to its guardrails (decision 14): a second measure only without a series,
// each measure a mark and an axis only beside another, and never both on the right,
// which would leave the left axis with nothing to read against. A pivot table's
// measures (up to three) are columns of its cells, with no mark or axis.
func (s *Settings) validateMeasures(spec *TypeSpec) error {
	t, ms := s.Type, s.Roles[RoleMeasure]
	for _, m := range ms {
		if !contains(Aggregates, m.Aggregate) {
			// The aggregates named are the type's own, so the one picked from them is taken.
			return aggregateRefused(m.Aggregate, spec.Aggregates)
		}
		if !contains(spec.Aggregates, m.Aggregate) {
			return refuse(2, "%s's measure must add up, so it takes %s, not %s", aChart(t), strings.Join(spec.Aggregates, " or "), m.Aggregate)
		}
		if s.Percent && !contains(addsUp, m.Aggregate) {
			return refuse(2, "100%% stacking shows each series' share of a whole, so its measure takes count or sum, not %s", m.Aggregate)
		}
	}
	if len(ms) < 2 {
		if len(ms) == 1 && (ms[0].Mark != "" || ms[0].Axis != "") {
			return refuse(1, "a measure takes a mark and an axis only beside a second measure; %s with one measure is drawn as its type", aChart(t))
		}
		return nil
	}
	if len(s.Roles[RoleSeries]) > 0 {
		return refuse(1, "%s with two measures has no series column; chart one measure by series, or two measures without one", aChart(t))
	}
	for i := range ms {
		for j := i + 1; j < len(ms); j++ {
			if ms[i].Aggregate == ms[j].Aggregate && ms[i].Column == ms[j].Column {
				if len(ms) == 2 {
					return refuse(1, "the two measures are the same; measure another column or use another aggregate")
				}
				return refuse(1, "measures %d and %d are the same; measure another column or use another aggregate", i+1, j+1)
			}
		}
	}
	if ms[0].Axis == AxisRight && ms[1].Axis == AxisRight {
		return refuse(1, "both measures are on the right axis; put one on the left")
	}
	return nil
}

// validateTarget holds a bullet chart to exactly one target, a second measure or a typed
// number, and every other type to none.
func (s *Settings) validateTarget() error {
	if s.Type != TypeBullet {
		if s.Target != nil {
			return refuse(1, "%s has no target", aChart(s.Type))
		}
		return nil
	}
	if t := s.Target; t != nil && !(*t >= -maxTarget && *t <= maxTarget) {
		return refuse(1, "a bullet chart's target is a number from %s to %s", plainNumber(-maxTarget), plainNumber(maxTarget))
	}
	switch n := len(s.Roles[RoleMeasure]); {
	case n > 1 && s.Target != nil:
		return refuse(1, "a bullet chart takes a target measure or a target number, not both")
	case n == 1 && s.Target == nil:
		return refuse(1, "a bullet chart needs a target: a second measure or a number")
	}
	return nil
}

// validateMap holds a map chart to one of the shapes rsync.ai ships, and every other
// type to none.
func (s *Settings) validateMap() error {
	if s.Type != TypeMap {
		if s.Map != "" {
			return refuse(1, "%s has no map", aChart(s.Type))
		}
		return nil
	}
	if !contains(Maps, s.Map) {
		if s.Map == "" {
			return refuse(1, "a map chart needs a map: %s", strings.Join(Maps, " or "))
		}
		return refuse(1, "the map %q is not one rsync.ai draws; use %s", clip(s.Map), strings.Join(Maps, " or "))
	}
	return nil
}

// sortedRoles lists the roles settings fill, known ones in roleOrder and then any other.
func sortedRoles(roles map[string][]Field) []string {
	var out []string
	for _, r := range roleOrder {
		if _, ok := roles[r]; ok {
			out = append(out, r)
		}
	}
	var other []string
	for r := range roles {
		if !contains(roleOrder, r) {
			other = append(other, r)
		}
	}
	sort.Strings(other)
	return append(out, other...)
}

// validateAliases refuses settings whose result would name one column twice: the
// SQL names each output after the column it comes from, so a measure of the x column
// would collide with it.
func (s *Settings) validateAliases() error {
	seen := map[string]Output{}
	for _, a := range s.outputs() {
		if s.FoldsSeries() && a.Source != "" {
			switch strings.ToLower(a.Alias) {
			case OtherAlias, SeriesCountAlias, TotalAlias, RankAlias:
				return refuse(1, "%s is named %q, a name a series chart's SQL uses itself; chart it without a series", a.Role, a.Alias)
			}
		}
		if (s.Type == TypeScatter || s.Type == TypeBubble) && a.Source != "" && strings.EqualFold(a.Alias, PointRowsAlias) {
			return refuse(1, "%s is named %q, a name %s's SQL uses itself for each point's row count", a.Role, a.Alias, aChart(s.Type))
		}
		if s.Type == TypePivot && a.Source != "" && strings.HasPrefix(strings.ToLower(a.Alias), pivotTotalPrefix) {
			return refuse(1, "%s is named %q, a name a pivot table's SQL uses itself for its totals", a.Role, a.Alias)
		}
		k := strings.ToLower(a.Alias)
		if prev, ok := seen[k]; ok {
			return refuse(1, "%s and %s would both be named %q in the result; %s", prev.Role, a.Role, a.Alias, s.aliasAdvice(prev, a))
		}
		seen[k] = a
	}
	return nil
}

// aliasAdvice says how to part two outputs that share a name, in the words of what
// collides: a measure of a column the chart also groups by, a name the chart's SQL gives a
// column itself, or two columns picked for two roles (a sankey's in the builder's words).
func (s *Settings) aliasAdvice(a, b Output) string {
	measured := func(o Output) bool {
		switch o.Role {
		case "the measure", RoleSecondMeasure, RoleThirdMeasure, "the earlier period":
			return o.Source != ""
		}
		return false
	}
	named := func(o Output) bool { return o.Source == "" || !strings.EqualFold(o.Alias, o.Source) }
	switch {
	case s.Type == TypeSankey && a.Role == "the "+RoleX && b.Role == "the "+RoleY:
		return "pick a different column for where the flows start and where they end"
	case measured(a) || measured(b):
		return "measure another column or use count"
	case named(a) || named(b):
		return "the chart's SQL uses that name itself, so chart a column with another name"
	}
	return "pick a different column for one of them"
}

// aChart names a chart type in a sentence as the chart page labels it (TypeSpec.Label),
// its article agreeing: "an area chart", "a horizontal bar chart", and with no "chart"
// where the label names its shape ("a box plot", "a pivot table", "a choropleth map").
func aChart(t string) string {
	name := t + " chart"
	if spec := Spec(t); spec != nil {
		name = strings.ToLower(spec.Label)
		if words := strings.Fields(name); !shapeNouns[words[len(words)-1]] {
			name += " chart"
		}
	}
	if strings.ContainsRune("aeiou", rune(name[0])) {
		return "an " + name
	}
	return "a " + name
}

// shapeNouns are the last words of a type's label that name its shape, so aChart adds
// no "chart" after them.
var shapeNouns = map[string]bool{"heatmap": true, "histogram": true, "map": true, "plot": true,
	"scorecard": true, "table": true, "treemap": true}

// Output is one column of a chart's result: its alias, the role it plays, and the
// source column it is read from ("" for count(*)).
type Output struct {
	Alias  string
	Role   string
	Source string
}

// PreviousAlias names a scorecard's earlier-period number.
const PreviousAlias = "previous"

// BinWidthAlias names a histogram's bin width, on every row: a bin runs from its x to x
// plus the width.
const BinWidthAlias = "rsync_bin_width"

// PointRowsAlias names a scatter's or a bubble chart's row count: the SQL draws the rows
// at one x, y, size and colour as one point, and this is how many there are.
const PointRowsAlias = "rsync_rows"

// Result roles (Output.Role) of a combo's or a pivot table's second measure, a pivot
// table's third, a histogram's bin width, and a scatter's or a bubble chart's size and
// row count.
const (
	RoleSecondMeasure = "the second measure"
	RoleThirdMeasure  = "the third measure"
	RoleBinWidth      = "the bin width"
	RolePointSize     = "the size"
	RolePointRows     = "the point's rows"
)

// A pivot table's result (dashboards-design.md § 14.3): a row per cell and per total,
// from GROUPING SETS, its row fields, its column fields, its measures, then a flag per
// field, 1 on a row where that field is totalled (its value there is not the field's,
// which on ClickHouse is the type's default rather than NULL). The flags are named
// pivotTotalPrefix and r1–r3 or c1–c2; a source column under such a name would collide.
const pivotTotalPrefix = "rsync_total_"

// PivotRowRole and PivotColumnRole are the result roles of a pivot table's nth row and
// column field (from 1), and PivotRowTotalRole and PivotColumnTotalRole those of their
// total flags; PivotTotalAlias names a flag ("r" or "c", and n).
func PivotRowRole(n int) string         { return fmt.Sprintf("row field %d", n) }
func PivotColumnRole(n int) string      { return fmt.Sprintf("column field %d", n) }
func PivotRowTotalRole(n int) string    { return fmt.Sprintf("row field %d total", n) }
func PivotColumnTotalRole(n int) string { return fmt.Sprintf("column field %d total", n) }
func PivotTotalAlias(rc string, n int) string {
	return fmt.Sprintf("%s%s%d", pivotTotalPrefix, rc, n)
}

// A series chart's SQL keeps the type's top Cap.Series series by total and folds the
// rest into one Other (dashboards-design.md § 14.3). OtherAlias flags a row of the rest
// (1) or of a kept series (0), and SeriesCountAlias is how many series the source has, so
// the chart says how many it folded or left out. The SQL ranks the series under
// TotalAlias and RankAlias inside its subqueries; a source column under any of the four
// names would collide with them.
// The result roles of a series chart's Other (Output.Role): 1 on Other's rows, and the
// number of series the source has, on every row.
const (
	RoleOtherSeries = "the other series"
	RoleSeriesCount = "the series count"
)

const (
	OtherAlias       = "rsync_other"
	SeriesCountAlias = "rsync_series"
	TotalAlias       = "rsync_total"
	RankAlias        = "rsync_rank"
)

// FoldsSeries reports whether the chart's SQL ranks its series and folds those past the
// type's cap into Other: a series field on a type that caps its series.
func (s *Settings) FoldsSeries() bool {
	spec := Spec(s.Type)
	return spec != nil && spec.Cap.Series > 0 && s.One(RoleSeries) != nil
}

// FoldsIntoOther reports whether an aggregate's values add up into one Other: a count,
// a sum, a min or a max. An average or a distinct count of several series is not the
// average or the distinct count of their rows, so their Other is left out (§ 14.3).
func FoldsIntoOther(aggregate string) bool {
	switch aggregate {
	case AggCount, AggSum, AggMin, AggMax:
		return true
	}
	return false
}

// MeasureAlias is the result column a measure is drawn from: count, count_distinct,
// or the measured column's own name for sum, avg, min and max.
func (f *Field) MeasureAlias() string {
	switch f.Aggregate {
	case AggCount:
		return "count"
	case AggCountDistinct:
		return "count_distinct"
	default:
		return f.Column
	}
}

// MeasureAliases are the result columns of the chart's measures, in order: a measure's
// is its MeasureAlias, with "_2" or "_3" (its place) when an earlier measure's names the
// same column (the sum and the average of one column). The frontend finds each by its
// Output.Role, not its alias.
func (s *Settings) MeasureAliases() []string {
	var out []string
	for i, m := range s.Roles[RoleMeasure] {
		a := m.MeasureAlias()
		for _, prev := range out {
			if strings.EqualFold(a, prev) {
				a += "_" + strconv.Itoa(i+1)
				break
			}
		}
		out = append(out, a)
	}
	return out
}

// Outputs lists the chart's result columns in SELECT order.
func (s *Settings) Outputs() []Output { return s.outputs() }

func (s *Settings) outputs() []Output {
	var out []Output
	switch s.Type {
	case TypePivot:
		return s.pivotOutputs()
	case TypeBoxplot, TypeCandlestick:
		return s.statOutputs()
	}
	for _, c := range s.ColumnsOf(RoleColumns) {
		out = append(out, Output{Alias: c, Role: "table column " + c, Source: c})
	}
	for _, role := range []string{RoleX, RoleY, RoleSize, RoleSeries} {
		if f := s.One(role); f != nil {
			r := "the " + role
			if role == RoleSize {
				r = RolePointSize
			}
			out = append(out, Output{Alias: f.Column, Role: r, Source: f.Column})
		}
	}
	// A treemap's or a sunburst's levels (a pivot table's rows returned above).
	for i, f := range s.Roles[RoleRows] {
		out = append(out, Output{Alias: f.Column, Role: LevelRole(i + 1), Source: f.Column})
	}
	if s.Type == TypeHistogram {
		if x := s.One(RoleX); x != nil {
			out = append(out, Output{Alias: BinWidthAlias, Role: RoleBinWidth, Source: x.Column})
		}
		out = append(out, Output{Alias: "count", Role: "the measure"})
	}
	out = append(out, s.measureOutputs()...)
	if s.FoldsSeries() {
		out = append(out, Output{Alias: OtherAlias, Role: RoleOtherSeries},
			Output{Alias: SeriesCountAlias, Role: RoleSeriesCount})
	}
	if s.Type == TypeScatter || s.Type == TypeBubble {
		out = append(out, Output{Alias: PointRowsAlias, Role: RolePointRows})
	}
	return out
}

// measureOutputs are the measures' result columns, in order, with a scorecard's earlier
// period after its measure.
func (s *Settings) measureOutputs() []Output {
	var out []Output
	aliases := s.MeasureAliases()
	for i, m := range s.Roles[RoleMeasure] {
		role := "the measure"
		switch i {
		case 1:
			role = RoleSecondMeasure
		case 2:
			role = RoleThirdMeasure
		}
		out = append(out, Output{Alias: aliases[i], Role: role, Source: m.Column})
		if i == 0 && s.Compare != nil {
			out = append(out, Output{Alias: PreviousAlias, Role: "the earlier period", Source: m.Column})
		}
	}
	return out
}

// pivotOutputs are a pivot table's result columns: its row fields, its column fields,
// its measures, then each row field's total flag and each column field's.
func (s *Settings) pivotOutputs() []Output {
	var out, flags []Output
	for i, f := range s.Roles[RoleRows] {
		out = append(out, Output{Alias: f.Column, Role: PivotRowRole(i + 1), Source: f.Column})
		flags = append(flags, Output{Alias: PivotTotalAlias("r", i+1), Role: PivotRowTotalRole(i + 1)})
	}
	for i, f := range s.Roles[RoleColumns] {
		out = append(out, Output{Alias: f.Column, Role: PivotColumnRole(i + 1), Source: f.Column})
		flags = append(flags, Output{Alias: PivotTotalAlias("c", i+1), Role: PivotColumnTotalRole(i + 1)})
	}
	out = append(out, s.measureOutputs()...)
	return append(out, flags...)
}

// LevelRole is the result role of a treemap's or a sunburst's nth level (from 1).
func LevelRole(n int) string { return fmt.Sprintf("level %d", n) }

// A box plot's result (§ 14.8): its group, then five numbers of each group's values, all
// read from its y column; the whiskers are the least and the greatest value, with no
// outliers drawn apart. A candlestick's: its period, then the period's open, high, low
// and close, read from its four y columns in that order. A source column under one of
// these names would collide with them.
var (
	BoxAliases   = []string{"rsync_min", "rsync_q1", "rsync_median", "rsync_q3", "rsync_max"}
	BoxRoles     = []string{"the minimum", "the lower quartile", "the median", "the upper quartile", "the maximum"}
	PriceAliases = []string{"rsync_open", "rsync_high", "rsync_low", "rsync_close"}
	PriceRoles   = []string{"the open", "the high", "the low", "the close"}
)

// statOutputs are a box plot's or a candlestick's result columns: x, then BoxAliases or
// PriceAliases, each with the y column it is read from as its source, so a masked column's
// statistics are masked with it.
func (s *Settings) statOutputs() []Output {
	var out []Output
	if x := s.One(RoleX); x != nil {
		out = append(out, Output{Alias: x.Column, Role: "the " + RoleX, Source: x.Column})
	}
	ys := s.ColumnsOf(RoleY)
	if s.Type == TypeBoxplot {
		for i := 0; i < len(BoxAliases) && len(ys) > 0; i++ {
			out = append(out, Output{Alias: BoxAliases[i], Role: BoxRoles[i], Source: ys[0]})
		}
		return out
	}
	for i := 0; i < len(PriceAliases) && i < len(ys); i++ {
		out = append(out, Output{Alias: PriceAliases[i], Role: PriceRoles[i], Source: ys[i]})
	}
	return out
}

// SourceColumns lists every source column the settings name, once each.
func (s *Settings) SourceColumns() []string {
	var cols []string
	seen := map[string]bool{}
	for _, role := range roleOrder {
		for _, c := range s.ColumnsOf(role) {
			if c != "" && !seen[c] {
				seen[c] = true
				cols = append(cols, c)
			}
		}
	}
	return cols
}

// CheckColumns is rule 2's column half: every column the settings name is a column of
// the source's result. sourceColumns are the names the source returned when run once.
func (s *Settings) CheckColumns(sourceColumns []string) error {
	have := make(map[string]bool, len(sourceColumns))
	for _, c := range sourceColumns {
		have[c] = true
	}
	for _, c := range s.SourceColumns() {
		if !have[c] {
			return refuse(2, "the column %q is not in the source's result; its columns are %s", clip(c), listColumns(sourceColumns))
		}
	}
	return nil
}

// CheckKinds is rule 2's kind half: each column the settings name holds a kind its role
// takes (the registry's Kinds, the builder's rule too: a point's colour also takes true
// or false), a grain groups a date, a sum or an average adds up numbers, and a lowest or
// a highest is of numbers except in a table or a pivot table. kinds maps
// a source column to the kind the source probe read from its first value; a column it
// has no number, time, text or bool for passes, and the run says. A date role and a
// grain also take a column that reads as a number, since a warehouse can return a date
// so (a MySQL yyyymmdd), and one that reads as text where the chart casts it to a date
// (a grain, a calendar's day), unless failed: the engine refused the chart's statement,
// and such a column is then the likely reason. Text no cast tries as a date (a
// candlestick's or a scorecard's ungrouped date) would be drawn or compared as text.
func (s *Settings) CheckKinds(kinds map[string]string, failed bool) error {
	spec := Spec(s.Type)
	if spec == nil {
		return nil
	}
	for _, r := range spec.Roles {
		for _, f := range s.Roles[r.Name] {
			held := heldAs[kinds[f.Column]]
			if f.Column == "" || held == "" {
				continue
			}
			kind := kinds[f.Column]
			// A colour of true or false is two colours: the builder offers one for a point.
			colourOfBool := r.Name == RoleSeries && kind == "bool" && contains(r.Kinds, KindText)
			cast := f.Grain != "" || s.Type == TypeCalendar
			if !colourOfBool && !takesKind(r.Kinds, kind, failed || kind == KindText && !cast) {
				need := kindNeed(r.Kinds)
				if r.Name == RoleSeries && contains(r.Kinds, KindText) {
					need = "a text or true-or-false column"
				}
				return refuse(2, "%s needs %s for its %s; the column %q holds %s", aChart(s.Type), need, roleWords(s.Type, r.Name), clip(f.Column), held)
			}
			if f.Grain != "" && !takesKind([]string{KindTime}, kind, failed) {
				return refuse(2, "grouping by %s needs a date column; the column %q holds %s", f.Grain, clip(f.Column), held)
			}
			if (f.Aggregate == AggSum || f.Aggregate == AggAvg) && kind != KindNumber {
				what := "a sum"
				if f.Aggregate == AggAvg {
					what = "an average"
				}
				return refuse(2, "%s needs a number column; the column %q holds %s", what, clip(f.Column), held)
			}
			// A lowest or a highest of text, dates or true or false is a value only a
			// table's or a pivot table's cell shows as it is; every other chart draws a
			// number, so it would draw nothing.
			if (f.Aggregate == AggMin || f.Aggregate == AggMax) && kind != KindNumber && s.Type != TypeTable && s.Type != TypePivot {
				what := "lowest"
				if f.Aggregate == AggMax {
					what = "highest"
				}
				return refuse(2, "%s draws the %s of a number column; the column %q holds %s, which a table or a pivot table shows as it is", aChart(s.Type), what, clip(f.Column), held)
			}
		}
	}
	return nil
}

// heldAs names each kind the source probe reads, as a refusal says what a column holds.
var heldAs = map[string]string{KindNumber: "numbers", KindTime: "dates", KindText: "text", "bool": "true or false"}

// takesKind says a role of these kinds takes a column of kind; a date role takes text
// or a number unless the chart failed to run (CheckKinds).
func takesKind(takes []string, kind string, failed bool) bool {
	if contains(takes, KindAny) || contains(takes, kind) {
		return true
	}
	return contains(takes, KindTime) && !failed && (kind == KindText || kind == KindNumber)
}

// kindNeed names the column a role's kinds take, as "a … chart needs …".
func kindNeed(takes []string) string {
	switch {
	case contains(takes, KindTime):
		return "a date column"
	case contains(takes, KindText) && contains(takes, KindNumber):
		return "a column of codes, as text or numbers,"
	case contains(takes, KindText):
		return "a text column"
	}
	return "a number column"
}

// roleWords names a role as the chart page labels it, in "… for its x".
func roleWords(t, role string) string {
	switch {
	case t == TypeCalendar && role == RoleX:
		return "days"
	case t == TypeCandlestick && role == RoleX:
		return "dates"
	case t == TypeCandlestick && role == RoleY:
		return "open, high, low and close"
	case t == TypeMap && role == RoleX:
		return "regions"
	case t == TypeBoxplot && role == RoleY:
		return "values"
	case (t == TypeScatter || t == TypeBubble) && role == RoleSeries:
		return "colour"
	}
	return role
}

// MissingColumns lists the columns the settings name that sourceColumns lack: a break
// in structure, drawn as a message (§ 5).
func (s *Settings) MissingColumns(sourceColumns []string) []string {
	have := make(map[string]bool, len(sourceColumns))
	for _, c := range sourceColumns {
		have[c] = true
	}
	var missing []string
	for _, c := range s.SourceColumns() {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

func listColumns(cols []string) string {
	if len(cols) == 0 {
		return "none"
	}
	const show = 12
	if len(cols) > show {
		return strings.Join(cols[:show], ", ") + fmt.Sprintf(" and %d more", len(cols)-show)
	}
	return strings.Join(cols, ", ")
}

// plainText is rule 4: a title or label is plain text, drawn escaped. Braces would be an
// ECharts formatter template and angle brackets markup; neither is stored.
func plainText(field, v string, max int) error {
	if v == "" {
		return nil
	}
	if !utf8.ValidString(v) {
		return refuse(4, "%s is not valid text", field)
	}
	if n := utf8.RuneCountInString(v); n > max {
		return refuse(4, "%s is %d characters; the most is %d", field, n, max)
	}
	for _, r := range v {
		switch {
		case r == '{' || r == '}':
			return refuse(4, "%s is plain text: no { or }, which a chart library reads as a template", field)
		case r == '<' || r == '>':
			return refuse(4, "%s is plain text: no < or >", field)
		case unicode.IsControl(r):
			return refuse(4, "%s is plain text: no control characters", field)
		}
	}
	return nil
}

// columnName is a source column as the settings name it: any name the engine returned,
// quoted when the SQL is written, but not empty, not over 128 characters, and with no
// control characters.
func columnName(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return refuse(1, "%s is empty", field)
	}
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > maxColumn {
		return refuse(1, "%s is not a column name", field)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return refuse(1, "%s is not a column name", field)
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// clip shortens a refused value echoed in a message.
func clip(v string) string {
	if utf8.RuneCountInString(v) <= 40 {
		return v
	}
	r := []rune(v)
	return string(r[:40]) + "…"
}
