package handlers

// A workflow's chart step (docs/internal/dashboards-design.md § 8, dashboards D3 PR 2).
// It draws a saved chart as the run's actor, the way the chart's page draws it, and
// holds what it drew for the sends after it: a bar, line, area or heatmap chart as a
// PNG the frontend draws with the app's own ECharts code, and every chart's numbers as
// text — a scorecard's number, a table's first rows, a plotted chart's first rows for
// Slack, which cannot carry an image through an incoming webhook.
//
// The rows go to the frontend inside the deployment (POST /api/internal/charts/png,
// X-Internal-Secret) and, as the picture and the text, to the delivery channels. Not
// to a log line, not to the run's rows, not to a model. What the step drew waits in
// Redis, encrypted, and is deleted when the run ends.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"api-gateway/internal/charts"

	"github.com/redis/go-redis/v9"
	"github.com/rsync-ai/shared/crypto"
	workflow "github.com/rsync-ai/shared/workflowdef"
)

const (
	// workflowChartRowsShown is how many rows a table, or a plotted chart's numbers
	// in Slack, show; the rest are counted.
	workflowChartRowsShown = 10
	// workflowChartCellMax caps one cell's text.
	workflowChartCellMax = 60
	// chartPNGMax is the largest PNG the frontend may hand back.
	chartPNGMax     = 4 << 20
	chartPNGTimeout = 30 * time.Second
	// chartMasked is the chart page's own words (saved-chart.ts MASKED), and
	// chartColumnMasked a box plot's or a candlestick's (COLUMN_MASKED).
	chartMasked       = "This chart's measure is masked for you, so it is not drawn."
	chartColumnMasked = "A column this chart draws is masked for you, so it is not drawn."
	// chartUnorderedNote is saved-chart.ts UNORDERED: a table drawn without the order
	// of each column (chartResult.Unordered).
	chartUnorderedNote = "Rows may come in a different order each time: the table could not be sorted by all its columns."
)

// Seams for tests: the chart's rows, where a chart step's drawing waits, and the
// frontend's drawing.
var (
	drawWorkflowChart                       = drawSavedChart
	workflowCharts       workflowChartStore = redisWorkflowCharts{}
	drawWorkflowChartPNG                    = frontendChartPNG
)

// workflowChart is what a chart step drew.
type workflowChart struct {
	StepID  string    `json:"step_id"`
	ChartID string    `json:"chart_id"`
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Link    string    `json:"link"`
	PNG     []byte    `json:"png,omitempty"`
	Text    chartText `json:"text"`
}

// chartText is a chart's numbers as words, as the chart's page writes them.
type chartText struct {
	// Message stands in for the numbers: the measure is masked, a column is
	// missing, or there are no rows.
	Message string `json:"message,omitempty"`
	// A scorecard: its label, its number, and the change on the earlier period.
	Label  string `json:"label,omitempty"`
	Number string `json:"number,omitempty"`
	Change string `json:"change,omitempty"`
	// A table, or a plotted chart's numbers: the first rows, and how many more.
	Head    []string   `json:"head,omitempty"`
	Numeric []bool     `json:"numeric,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
	More    int        `json:"more,omitempty"`
	// Notes are the chart page's warnings that hold here.
	Notes []string `json:"notes,omitempty"`
}

// chartPlotted says whether a chart type is a picture: every type in the registry but a
// scorecard, a table and a pivot table, which a message shows as text.
func chartPlotted(t string) bool {
	return charts.Spec(t) != nil && t != charts.TypeScorecard && t != charts.TypeTable && t != charts.TypePivot
}

// chartURL is the chart's page in rsync.ai.
func chartURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/")
	if base == "" {
		base = "http://localhost:3000"
	}
	return base + "/charts/" + id
}

// ---------------------------------------------------------------------------
// The step
// ---------------------------------------------------------------------------

// runChart draws the step's chart as the run's actor and holds it for the sends
// after it. A chart that cannot be drawn fails the step; a send after it with rule
// all_done goes without it and says so.
func (s *graphStepCall) runChart() {
	if s.actorRefused() {
		return
	}
	actor := workflowRunActor(s.run)
	rec, err := loadVisibleChart(s.ctx, s.db, s.run.WorkspaceID, actor, s.step.ChartID)
	if errors.Is(err, sql.ErrNoRows) {
		s.refuse("The chart was deleted, or the run-as user cannot see it.", true)
		return
	}
	if err != nil {
		s.unavail(err, "could not load the chart")
		return
	}
	d := drawWorkflowChart(s.ctx, s.db, s.run.WorkspaceID, actor, rec, nil, time.Now())
	if d.Break != nil {
		s.finish(workflow.StatusFailed, d.Break.Message, "", graphStepAnswer{})
		return
	}
	if d.Error != "" || d.Result == nil {
		msg := d.Error
		if msg == "" {
			msg = "The chart's query gave no result."
		}
		s.finish(workflow.StatusFailed, workflow.CapRunes(msg, workflowErrorMax), "", graphStepAnswer{})
		return
	}
	held, err := messageChart(s.ctx, rec, d.Result)
	if err != nil {
		s.finish(workflow.StatusFailed, workflow.CapRunes(err.Error(), workflowErrorMax), "", graphStepAnswer{})
		return
	}
	held.StepID = s.step.ID
	if err := workflowCharts.Put(s.ctx, workflowResultKey(s.run.RunID, s.step.ID), held,
		workflowResultTTL(s.run.Definition)); err != nil {
		s.unavail(err, "could not hold the chart")
		return
	}
	s.finish(workflow.StatusSucceeded, "", "", graphStepAnswer{})
}

// messageChart writes a drawn chart for a message: its numbers as text and, for a
// plotted chart with rows to show, its picture. A type the server does not draw
// (the registry's Static) goes without one, and says so (dashboards-design.md § 14.5).
// The chart step and each chart tile of the dashboard step write a chart this way.
func messageChart(ctx context.Context, rec *chartRecord, res *chartResult) (workflowChart, error) {
	settings, err := charts.Decode(rec.Settings)
	if err != nil {
		return workflowChart{}, errors.New("This chart's settings no longer pass the checks: " + err.Error())
	}
	c := workflowChart{ChartID: rec.ID, Name: rec.Name, Type: settings.Type, Link: chartURL(rec.ID),
		Text: workflowChartText(settings, res)}
	if chartPlotted(settings.Type) && c.Text.Message == "" {
		if !charts.Spec(settings.Type).Static {
			c.Text.Notes = append(c.Text.Notes, charts.NotStatic)
			return c, nil
		}
		png, err := drawWorkflowChartPNG(ctx, rec.Name, charts.AsVersion2(rec.Settings), res)
		if err != nil {
			return workflowChart{}, errors.New("The chart could not be drawn as a picture: " + err.Error())
		}
		c.PNG = png
	}
	return c, nil
}

// workflowDrawing is what one chart or dashboard step before a send drew. Both are
// nil when the step drew nothing (it failed, and the send runs anyway).
type workflowDrawing struct {
	Kind      string
	Chart     *workflowChart
	Dashboard *workflowDashboard
}

// sendDrawings reads what each chart and dashboard step before the send drew, in
// the send's order. A drawing that was not held is empty, with a note.
func (s *graphStepCall) sendDrawings() ([]workflowDrawing, []string, error) {
	var (
		out   []workflowDrawing
		notes []string
	)
	for _, st := range s.run.Definition.DrawingsBefore(&s.step) {
		key := workflowResultKey(s.run.RunID, st.ID)
		d := workflowDrawing{Kind: st.Type}
		var held bool
		var err error
		if st.Type == workflow.StepDashboard {
			var b workflowDashboard
			if b, held, err = workflowDashboards.Get(s.ctx, key); held {
				d.Dashboard = &b
			}
		} else {
			var c workflowChart
			if c, held, err = workflowCharts.Get(s.ctx, key); held {
				d.Chart = &c
			}
		}
		if err != nil {
			return nil, nil, err
		}
		if !held {
			what := "chart"
			if st.Type == workflow.StepDashboard {
				what = "dashboard"
			}
			notes = append(notes, fmt.Sprintf("The %s step %q drew nothing, so the message went without its %s.", what, st.ID, what))
		}
		out = append(out, d)
	}
	return out, notes, nil
}

// ---------------------------------------------------------------------------
// Where a chart waits for its sends
// ---------------------------------------------------------------------------

// workflowChartStore holds a chart step's drawing for the sends after it.
type workflowChartStore interface {
	Put(ctx context.Context, key string, c workflowChart, ttl time.Duration) error
	Get(ctx context.Context, key string) (workflowChart, bool, error)
	Delete(ctx context.Context, key string) error
}

// redisWorkflowCharts keeps each drawing encrypted, under run/step.
type redisWorkflowCharts struct{}

func workflowChartRedisKey(key string) string { return "workflow-run-chart:" + key }

func (redisWorkflowCharts) client() (*redis.Client, error) {
	if schemaCache == nil {
		return nil, errors.New("Redis is not available")
	}
	return schemaCache.GetClient(), nil
}

func (s redisWorkflowCharts) Put(ctx context.Context, key string, c workflowChart, ttl time.Duration) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	enc, err := crypto.Encrypt(raw)
	if err != nil {
		return err
	}
	return rc.Set(ctx, workflowChartRedisKey(key), enc, ttl).Err()
}

func (s redisWorkflowCharts) Get(ctx context.Context, key string) (workflowChart, bool, error) {
	rc, err := s.client()
	if err != nil {
		return workflowChart{}, false, err
	}
	enc, err := rc.Get(ctx, workflowChartRedisKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return workflowChart{}, false, nil
	}
	if err != nil {
		return workflowChart{}, false, err
	}
	raw, err := crypto.Decrypt(enc)
	if err != nil {
		return workflowChart{}, false, err
	}
	var c workflowChart
	if err := json.Unmarshal(raw, &c); err != nil {
		return workflowChart{}, false, err
	}
	return c, true, nil
}

func (s redisWorkflowCharts) Delete(ctx context.Context, key string) error {
	rc, err := s.client()
	if err != nil {
		return err
	}
	return rc.Del(ctx, workflowChartRedisKey(key)).Err()
}

// ---------------------------------------------------------------------------
// The picture: the frontend draws it
// ---------------------------------------------------------------------------

// chartPNGRequest is what the frontend's internal route draws from: the chart's
// saved settings and its rows, as POST /api/v1/charts/:id/draw hands them to the
// chart's page.
type chartPNGRequest struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings"`
	Result   chartPNGResult  `json:"result"`
}

type chartPNGResult struct {
	Columns    []chartOutput   `json:"columns"`
	Rows       [][]interface{} `json:"rows"`
	Truncated  bool            `json:"truncated"`
	Masked     []string        `json:"masked"`
	Unfiltered []string        `json:"unfiltered"`
}

var chartPNGClient = &http.Client{Timeout: chartPNGTimeout}

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// frontendChartPNG asks the frontend to draw the chart with the code behind the
// chart's Download PNG, so the email shows what the app shows.
func frontendChartPNG(ctx context.Context, name string, settings json.RawMessage, res *chartResult) ([]byte, error) {
	secret := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET"))
	if secret == "" {
		return nil, errors.New("INTERNAL_SERVICE_SECRET is not set, so the frontend will not draw it")
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("FRONTEND_INTERNAL_URL")), "/")
	if base == "" {
		base = "http://frontend:3000"
	}
	masked, unfiltered := res.Masked, res.Unfiltered
	if masked == nil {
		masked = []string{}
	}
	if unfiltered == nil {
		unfiltered = []string{}
	}
	body, err := json.Marshal(chartPNGRequest{Name: name, Settings: settings, Result: chartPNGResult{
		Columns: res.Columns, Rows: res.Rows, Truncated: res.Truncated, Masked: masked, Unfiltered: unfiltered}})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, chartPNGTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/internal/charts/png", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Secret", secret)
	resp, err := chartPNGClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the frontend could not be reached: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("the frontend answered %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("the frontend answered %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		return nil, fmt.Errorf("the frontend answered %q, not a PNG", ct)
	}
	png, err := io.ReadAll(io.LimitReader(resp.Body, chartPNGMax+1))
	if err != nil {
		return nil, fmt.Errorf("the picture could not be read: %w", err)
	}
	if len(png) > chartPNGMax {
		return nil, fmt.Errorf("the picture is over %d MB", chartPNGMax>>20)
	}
	if !bytes.HasPrefix(png, pngSignature) {
		return nil, errors.New("the frontend's answer is not a PNG")
	}
	return png, nil
}

// ---------------------------------------------------------------------------
// The numbers: written as the chart's page writes them (saved-chart.ts)
// ---------------------------------------------------------------------------

const (
	chartRoleX        = "the x"
	chartRoleY        = "the y"
	chartRoleSeries   = "the series"
	chartRoleMeasure  = "the measure"
	chartRolePrevious = "the earlier period"
)

// workflowChartText writes the chart's numbers: a scorecard's number and its
// change, or the first rows of the columns the chart shows.
func workflowChartText(s *charts.Settings, res *chartResult) chartText {
	roles := map[string]int{}
	for i, c := range res.Columns {
		roles[c.Role] = i
	}
	masked := func(i int) bool {
		for _, a := range res.Masked {
			if a == res.Columns[i].Alias {
				return true
			}
		}
		return false
	}
	var t chartText
	if res.Truncated {
		t.Notes = append(t.Notes, chartTruncatedNote(s.Type))
	}
	format := chartMeasureFormat(s)

	if s.Type == charts.TypeScorecard {
		mi, ok := roles[chartRoleMeasure]
		if !ok {
			t.Message = "The result has no number to show."
			return t
		}
		if masked(mi) {
			t.Message = chartMasked
			return t
		}
		var row []interface{}
		if len(res.Rows) > 0 {
			row = res.Rows[0]
		}
		value, hasValue := chartNumber(chartCell(row, mi))
		t.Label = chartMeasureTitle(s)
		t.Number = "–"
		if hasValue {
			t.Number = formatChartNumber(value, format)
		}
		if s.Compare != nil {
			if s.Compare.Days == 1 {
				t.Label += " · last day"
			} else {
				t.Label += fmt.Sprintf(" · last %d days", s.Compare.Days)
			}
			pi, ok := roles[chartRolePrevious]
			previous, hasPrevious := 0.0, false
			if ok {
				previous, hasPrevious = chartNumber(chartCell(row, pi))
			}
			t.Change = chartChangeText(value, hasValue, previous, hasPrevious, s.Compare.Days)
			if hasPrevious {
				t.Change += " (" + formatChartNumber(previous, format) + ")"
			}
		}
		return t
	}

	type column struct {
		head    string
		index   int
		measure bool
		format  string
		// fixed is a number typed into the settings, the same on every row: a bullet
		// chart's target.
		fixed *float64
	}
	var cols []column
	// col adds role's column, or says why the chart is not drawn: false when the result
	// has no such column, or it is a measure the masking redacted.
	col := func(role string, c column) bool {
		i, ok := roles[role]
		switch {
		case !ok:
			t.Message = "The result is missing a column the chart needs."
		case c.measure && masked(i):
			t.Message = chartMasked
		default:
			c.index = i
			cols = append(cols, c)
			return true
		}
		return false
	}
	// totals maps a pivot table's field column to its total flag's: the field reads
	// Total on a row where its flag is 1.
	totals := map[int]int{}
	switch s.Type {
	case charts.TypePivot:
		// Its cells and totals as rows, as the SQL orders them: the totals first.
		for _, part := range []struct {
			fields      []charts.Field
			role, total func(int) string
		}{
			{s.Roles[charts.RoleRows], charts.PivotRowRole, charts.PivotRowTotalRole},
			{s.Roles[charts.RoleColumns], charts.PivotColumnRole, charts.PivotColumnTotalRole},
		} {
			for n := range part.fields {
				i, ok := roles[part.role(n+1)]
				ti, tok := roles[part.total(n+1)]
				if !ok || !tok {
					t.Message = "The result is missing a column the chart needs."
					return t
				}
				totals[i] = ti
				cols = append(cols, column{head: chartAxisTitle(&part.fields[n]), index: i})
			}
		}
		for n, role := range []string{chartRoleMeasure, charts.RoleSecondMeasure, charts.RoleThirdMeasure}[:len(s.Roles[charts.RoleMeasure])] {
			i, ok := roles[role]
			if !ok {
				t.Message = "The result is missing a column the chart needs."
				return t
			}
			m := &s.Roles[charts.RoleMeasure][n]
			cols = append(cols, column{head: chartFieldTitle(m), index: i, measure: true, format: chartFieldFormat(m)})
		}
		if len(res.Rows) == 0 {
			t.Message = "No rows to draw."
			return t
		}
	case charts.TypeScatter, charts.TypeBubble:
		// A point a result row: its x, y and size, then its colour and how many of the source's
		// rows are at it.
		for _, role := range []string{charts.RoleX, charts.RoleY, charts.RoleSize} {
			f := s.One(role)
			if f == nil {
				continue
			}
			out := "the " + role
			if role == charts.RoleSize {
				out = charts.RolePointSize
			}
			i, ok := roles[out]
			if !ok {
				t.Message = "The result is missing a column the chart needs."
				return t
			}
			cols = append(cols, column{head: chartAxisTitle(f), index: i, measure: true, format: chartFieldFormat(nil)})
		}
		if f := s.One(charts.RoleSeries); f != nil {
			i, ok := roles[chartRoleSeries]
			if !ok {
				t.Message = "The result is missing a column the chart needs."
				return t
			}
			cols = append(cols, column{head: f.Column, index: i})
		}
		i, ok := roles[charts.RolePointRows]
		if !ok {
			t.Message = "The result is missing a column the chart needs."
			return t
		}
		cols = append(cols, column{head: "Rows", index: i, measure: true, format: chartFieldFormat(nil)})
		if len(res.Rows) == 0 {
			t.Message = "No rows to draw."
			return t
		}
	case charts.TypeTreemap, charts.TypeSunburst:
		// A part a row: its name at each level, then its measure.
		levels := s.Roles[charts.RoleRows]
		for n := range levels {
			if !col(charts.LevelRole(n+1), column{head: chartAxisTitle(&levels[n])}) {
				return t
			}
		}
		if !col(chartRoleMeasure, column{head: chartMeasureTitle(s), measure: true, format: format}) {
			return t
		}
		if len(res.Rows) == 0 {
			t.Message = "No rows to draw."
			return t
		}
	case charts.TypeBullet:
		// Its one number, then its target: a second measure, or a typed number, in the
		// number's format.
		if !col(chartRoleMeasure, column{head: chartMeasureTitle(s), measure: true, format: format}) {
			return t
		}
		if ms := s.Roles[charts.RoleMeasure]; len(ms) > 1 {
			if !col(charts.RoleSecondMeasure, column{head: chartFieldTitle(&ms[1]), measure: true, format: format}) {
				return t
			}
		} else if s.Target != nil {
			cols = append(cols, column{head: "Target", index: -1, measure: true, format: format, fixed: s.Target})
		}
		var row []interface{}
		if len(res.Rows) > 0 {
			row = res.Rows[0]
		}
		if _, ok := chartNumber(chartCell(row, cols[0].index)); !ok {
			t.Message = "The result has no number to show."
			return t
		}
	case charts.TypeBoxplot, charts.TypeCandlestick:
		// A category a row: its least, quartiles and greatest, or its period's prices.
		if !col(chartRoleX, column{head: chartAxisTitle(s.One(charts.RoleX))}) {
			return t
		}
		stats, heads, f := charts.BoxRoles, chartBoxHeads, chartFieldFormat(s.One(charts.RoleY))
		if s.Type == charts.TypeCandlestick {
			stats, heads, f = charts.PriceRoles, chartPriceHeads, "decimal"
		}
		for k, role := range stats {
			if !col(role, column{head: heads[k], format: f}) {
				return t
			}
		}
		// Each is read from a y column: one masked is the page's own words for it.
		for k := 1; k < len(cols); k++ {
			if masked(cols[k].index) {
				t.Message = chartColumnMasked
				return t
			}
			cols[k].measure = true
		}
		if len(res.Rows) == 0 {
			t.Message = "No rows to draw."
			return t
		}
		if res.Approximate {
			t.Notes = append(t.Notes, "The quartiles are approximate: this engine estimates them.")
		}
	case charts.TypeTable:
		for _, c := range s.ColumnsOf(charts.RoleColumns) {
			i, ok := roles["table column "+c]
			if !ok {
				i = -1
			}
			cols = append(cols, column{head: c, index: i})
		}
		if s.One(charts.RoleMeasure) != nil {
			i, ok := roles[chartRoleMeasure]
			if !ok {
				i = -1
			}
			cols = append(cols, column{head: chartMeasureTitle(s), index: i, measure: true, format: format})
		}
		if res.Unordered {
			t.Notes = append(t.Notes, chartUnorderedNote)
		}
	default:
		// A pie's slices are its series and it has no x; a calendar's x is its day; a
		// sankey's flow goes from its x to its y; a map's x is its ISO codes, as the column
		// holds them (the picture names each shape).
		var need, heads []string
		if x := s.One(charts.RoleX); x != nil {
			head := chartAxisTitle(x)
			if s.Type == charts.TypeCalendar {
				head = "Day"
			}
			need, heads = append(need, chartRoleX), append(heads, head)
		}
		if s.Type == charts.TypeHeatmap || s.Type == charts.TypeSankey {
			need, heads = append(need, chartRoleY), append(heads, chartAxisTitle(s.One(charts.RoleY)))
		} else if series := s.One(charts.RoleSeries); series != nil {
			need, heads = append(need, chartRoleSeries), append(heads, series.Column)
		}
		for k, role := range need {
			i, ok := roles[role]
			if !ok {
				t.Message = "The result is missing a column the chart needs."
				return t
			}
			cols = append(cols, column{head: heads[k], index: i})
		}
		mi, ok := roles[chartRoleMeasure]
		if !ok {
			t.Message = "The result is missing a column the chart needs."
			return t
		}
		if masked(mi) {
			t.Message = chartMasked
			return t
		}
		cols = append(cols, column{head: chartMeasureTitle(s), index: mi, measure: true, format: format})
		// A combo's second measure, in its own format.
		if ms := s.Roles[charts.RoleMeasure]; len(ms) > 1 {
			i, ok := roles[charts.RoleSecondMeasure]
			if !ok {
				t.Message = "The result is missing a column the chart needs."
				return t
			}
			if masked(i) {
				t.Message = chartMasked
				return t
			}
			cols = append(cols, column{head: chartFieldTitle(&ms[1]), index: i, measure: true, format: chartFieldFormat(&ms[1])})
		}
		if len(res.Rows) == 0 {
			t.Message = "No rows to draw."
			return t
		}
	}

	// A series chart's SQL keeps the top series and folds the rest into Other (§ 14.3):
	// its rows are named Other, or left out where the measure does not add up.
	rows := res.Rows
	seriesAt, otherAt := -1, -1
	if oi, ok := roles[charts.RoleOtherSeries]; ok && s.FoldsSeries() {
		seriesAt, otherAt = roles[chartRoleSeries], oi
		// A scatter's points past its top colours are all drawn, grey, as Other.
		agg := charts.AggCount
		if m := s.One(charts.RoleMeasure); m != nil {
			agg = m.Aggregate
		}
		if !charts.FoldsIntoOther(agg) {
			rows = nil
			for _, row := range res.Rows {
				if !chartIsOther(row, otherAt) {
					rows = append(rows, row)
				}
			}
		}
		if note := chartOtherNote(s, agg, res.Rows, roles); note != "" {
			t.Notes = append(t.Notes, note)
		}
	}
	for _, c := range cols {
		t.Head = append(t.Head, workflow.CapRunes(c.head, workflowChartCellMax))
		t.Numeric = append(t.Numeric, c.measure)
	}
	// A histogram's x is each bin's least value; the row says the bin, least to greatest.
	xAt, widthAt := -1, -1
	if wi, ok := roles[charts.RoleBinWidth]; ok {
		xAt, widthAt = roles[chartRoleX], wi
	}
	shown := rows
	if len(shown) > workflowChartRowsShown {
		shown, t.More = shown[:workflowChartRowsShown], len(rows)-workflowChartRowsShown
	}
	for _, row := range shown {
		cells := make([]string, 0, len(cols))
		for _, c := range cols {
			cell := ""
			if c.fixed != nil {
				cell = formatChartNumber(*c.fixed, c.format)
			}
			if c.index >= 0 {
				v := chartCell(row, c.index)
				ti, isField := totals[c.index]
				switch {
				case isField && chartIsOther(row, ti):
					cell = "Total"
				case isField:
					cell = chartLabel(v)
				case c.index == seriesAt && chartIsOther(row, otherAt):
					cell = "Other"
				case c.index == xAt:
					cell = chartBinLabel(v, chartCell(row, widthAt))
				case c.measure && !masked(c.index):
					// A table's lowest or highest of dates or text reads as the column holds it.
					if n, ok := chartNumber(v); ok {
						cell = formatChartNumber(n, c.format)
					} else if v != nil {
						cell = chartLabel(v)
					}
				case v != nil:
					cell = chartLabel(v)
				}
			}
			cells = append(cells, workflow.CapRunes(cell, workflowChartCellMax))
		}
		t.Rows = append(t.Rows, cells)
	}
	return t
}

// chartBoxHeads and chartPriceHeads are a box plot's and a candlestick's numbers as the
// chart's table heads them (chart-option.ts BOX_LINES, least first, and PRICE_LINES), in
// charts.BoxRoles' and PriceRoles' order.
var (
	chartBoxHeads   = []string{"Minimum", "Lower quartile", "Median", "Upper quartile", "Maximum"}
	chartPriceHeads = []string{"Open", "High", "Low", "Close"}
)

// chartTruncatedNote is saved-chart.ts truncatedNote: what a chart whose rows were cut
// says. A treemap or a sunburst keeps its largest parts, a calendar its latest days (the
// type's row cap), a scatter or a bubble chart its points with the most rows, the smallest
// x then y first among equals (as chart_sql.go orders them), every other type its first
// charts.MaxRows rows. shared/chart_notes_golden.json holds both sides to the same words.
func chartTruncatedNote(t string) string {
	n := charts.MaxRows
	if spec := charts.Spec(t); spec != nil && spec.Cap.Rows > 0 {
		n = spec.Cap.Rows
	}
	shown := groupDigits(strconv.Itoa(n))
	switch t {
	case charts.TypeTreemap, charts.TypeSunburst:
		return fmt.Sprintf("The largest %s parts drawn: the source has more.", shown)
	case charts.TypeCalendar:
		return fmt.Sprintf("The latest %s days drawn: the source has more.", shown)
	case charts.TypeScatter, charts.TypeBubble:
		return fmt.Sprintf("The %s points with the most rows drawn, ties broken by the smallest x then y: the source has more.", shown)
	}
	return fmt.Sprintf("First %s rows drawn: the source has more.", shown)
}

// chartIsOther reports whether a series chart's row is its Other: the Other column is 1.
func chartIsOther(row []interface{}, otherAt int) bool {
	n, ok := chartNumber(chartCell(row, otherAt))
	return otherAt >= 0 && ok && n == 1
}

// chartOtherNote is saved-chart.ts's note on the series past the type's cap: drawn as
// Other, or left out when the measure does not add up; empty when none are. It names them
// as the builder does (a pie's and a donut's slices, a scatter's and a bubble chart's
// colours, every other type's series), the verb agreeing with how many.
// shared/chart_notes_golden.json holds both sides to the same words.
func chartOtherNote(s *charts.Settings, aggregate string, rows [][]interface{}, roles map[string]int) string {
	ci, ok := roles[charts.RoleSeriesCount]
	if !ok || len(rows) == 0 {
		return ""
	}
	n, ok := chartNumber(chartCell(rows[0], ci))
	rest := int(n) - charts.Spec(s.Type).Cap.Series
	if !ok || rest <= 0 {
		return ""
	}
	one, many := "series", "series"
	switch s.Type {
	case charts.TypePie, charts.TypeDonut:
		one, many = "slice", "slices"
	case charts.TypeScatter, charts.TypeBubble:
		one, many = "colour", "colours"
	}
	what := fmt.Sprintf("%d more %s are", rest, many)
	if rest == 1 {
		what = fmt.Sprintf("1 more %s is", one)
	}
	switch {
	case charts.FoldsIntoOther(aggregate):
		return what + " drawn as Other."
	case aggregate == charts.AggAvg:
		return what + " left out: an average cannot be added into Other."
	default:
		return what + " left out: a distinct count cannot be added into Other."
	}
}

// chartCell is row[i] as the chart's page receives it, through JSON: a time is its
// RFC 3339 text, bytes are base64, a number is a json.Number.
func chartCell(row []interface{}, i int) interface{} {
	if i < 0 || i >= len(row) {
		return nil
	}
	raw, err := json.Marshal(row[i])
	if err != nil {
		return fmt.Sprint(row[i])
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return string(raw)
	}
	return v
}

var (
	chartMidnight = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})[T ]00:00:00(\.0+)?(Z|[+-]00(:?00)?)?$`)
	chartNumeric  = regexp.MustCompile(`^-?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)
)

// chartLabel is saved-chart.ts labelOf: a midnight timestamp is its date, null is
// "(empty)".
func chartLabel(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "(empty)"
	case string:
		if m := chartMidnight.FindStringSubmatch(t); m != nil {
			return m[1]
		}
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// chartNumber is saved-chart.ts toNumber: a number, or a database's numeric text.
func chartNumber(v interface{}) (float64, bool) {
	var text string
	switch t := v.(type) {
	case json.Number:
		text = t.String()
	case string:
		text = strings.TrimSpace(t)
		if !chartNumeric.MatchString(text) {
			return 0, false
		}
	default:
		return 0, false
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	return n, true
}

var chartAggregateTitle = map[string]func(string) string{
	charts.AggCount: func(c string) string {
		if c == "" {
			return "Count"
		}
		return "Count of " + c
	},
	charts.AggCountDistinct: func(c string) string { return "Distinct " + c },
	charts.AggSum:           func(c string) string { return "Sum of " + c },
	charts.AggAvg:           func(c string) string { return "Average " + c },
	charts.AggMin:           func(c string) string { return "Lowest " + c },
	charts.AggMax:           func(c string) string { return "Highest " + c },
}

// chartMeasureTitle is saved-chart.ts measureTitle: a histogram counts its rows.
func chartMeasureTitle(s *charts.Settings) string {
	if s.Type == charts.TypeHistogram {
		return "Count"
	}
	return chartFieldTitle(s.One(charts.RoleMeasure))
}

// chartFieldTitle is one measure's title: its own, or its aggregate's.
func chartFieldTitle(m *charts.Field) string {
	if m == nil {
		return ""
	}
	if t := strings.TrimSpace(m.Title); t != "" {
		return t
	}
	if f, ok := chartAggregateTitle[m.Aggregate]; ok {
		return f(m.Column)
	}
	return m.Column
}

// chartMeasureFormat is saved-chart.ts measureFormat: a histogram's count is a whole
// number.
func chartMeasureFormat(s *charts.Settings) string {
	if s.Type == charts.TypeHistogram {
		return "integer"
	}
	return chartFieldFormat(s.One(charts.RoleMeasure))
}

// chartFieldFormat is one measure's format: its own, a count's whole number, or a
// decimal.
func chartFieldFormat(m *charts.Field) string {
	if m != nil && m.Format != "" {
		return m.Format
	}
	if m != nil && (m.Aggregate == charts.AggCount || m.Aggregate == charts.AggCountDistinct) {
		return "integer"
	}
	return "decimal"
}

// chartBinLabel is saved-chart.ts binLabel: a histogram's bin as "least–greatest", each
// edge with the digits the bin width needs to tell neighbours apart (2–8,075, not
// 2.4–8074.6165), or its one value in full when every row holds the same number (a width
// of 0).
func chartBinLabel(lo, width interface{}) string {
	l, ok := chartNumber(lo)
	if !ok {
		return chartLabel(lo)
	}
	w, ok := chartNumber(width)
	if !ok || w == 0 {
		return chartEdge(l, min(20, chartDecimals(chartClean10(l))))
	}
	d := chartEdgeDigits(w)
	return chartEdge(l, d) + "–" + chartEdge(l+w, d)
}

// chartEdge is chart-settings.ts formatEdge of a plain number: v in ten significant
// digits (so 0.1 + 0.2 reads 0.3), at most digits fraction digits, grouped, never "-0".
func chartEdge(v float64, digits int) string {
	r := chartClean10(v)
	ip, fp := roundDecimal(math.Abs(r), digits)
	out := groupDigits(ip)
	if fp != "" {
		out += "." + fp
	}
	if math.Signbit(r) && out != "0" {
		return "-" + out
	}
	return out
}

// chartEdgeDigits is chart-settings.ts edgeDigits: the width's own decimals, to its third
// significant digit at most.
func chartEdgeDigits(width float64) int {
	w := chartClean10(math.Abs(width))
	return min(20, max(0, min(chartDecimals(w), 2-chartExp(w))))
}

// chartClean10 is v in ten significant digits: what a sum's float error leaves reads as meant.
func chartClean10(v float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 10, 64), 64)
	if err != nil {
		return v
	}
	return r
}

// chartExp is chart-settings.ts exponentOf: the power of ten of |v|'s first digit, from
// the shortest digits that read back as v, as JavaScript's toExponential() writes them.
func chartExp(v float64) int {
	_, e, _ := strings.Cut(strconv.FormatFloat(math.Abs(v), 'e', -1, 64), "e")
	n, _ := strconv.Atoi(e)
	return n
}

// chartDecimals is chart-settings.ts decimalsOf: the digits after the point v is written
// with in full (2 for 1.25, 0 for 1,200).
func chartDecimals(v float64) int {
	m, _, _ := strings.Cut(strconv.FormatFloat(math.Abs(v), 'e', -1, 64), "e")
	_, fraction, _ := strings.Cut(m, ".")
	return max(0, len(fraction)-chartExp(v))
}

func chartAxisTitle(a *charts.Field) string {
	if a == nil {
		return ""
	}
	if t := strings.TrimSpace(a.Title); t != "" {
		return t
	}
	return a.Column
}

// chartChangeText is saved-chart.ts changeText.
func chartChangeText(value float64, hasValue bool, previous float64, hasPrevious bool, days int) string {
	before := fmt.Sprintf("the %d days before", days)
	if days == 1 {
		before = "the day before"
	}
	if !hasValue || !hasPrevious {
		return "No number for " + before
	}
	var short string
	switch {
	case previous == 0 && value == 0:
		short = "No change"
	case previous == 0:
		short = "Up from 0"
	default:
		pct := (value - previous) / math.Abs(previous) * 100
		if pct == 0 {
			short = "No change"
			break
		}
		abs := math.Abs(pct)
		text := chartToFixed1(abs)
		if abs >= 10 {
			text = groupDigits(strconv.FormatFloat(math.Floor(abs+0.5), 'f', 0, 64))
		}
		sign := "+"
		if pct < 0 {
			sign = "−"
		}
		short = sign + text + "%"
	}
	return short + " on " + before
}

// chartToFixed1 is JavaScript's toFixed(1) of a non-negative v: both round v's
// exact value, but on an exact half JavaScript goes up and Go goes to even. The
// only exact halves a float64 holds are odd quarters (0.25, 1.75), and v*4 is exact.
func chartToFixed1(v float64) string {
	if t := v * 4; t == math.Trunc(t) && math.Mod(t, 2) == 1 {
		return strconv.FormatFloat(math.Ceil(v*10)/10, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

// chartFractionDigits is chart-settings.ts fractionDigits: none for an integer, else one,
// but two significant digits under 1 (0.03, 0.45%), so a value that is not 0 never reads 0.
func chartFractionDigits(abs float64, format string) int {
	if format == "integer" {
		return 0
	}
	if abs == 0 || math.IsInf(abs, 0) || (format == "compact" && abs >= 1) {
		return 1
	}
	e := chartExp(abs)
	if format == "percent" {
		e += 2
	}
	return min(20, max(1, 1-e))
}

// formatChartNumber is chart-settings.ts formatNumber: Intl.NumberFormat("en-US")
// with chartFractionDigits fraction digits at most, rounding half away from zero on the
// number's shortest decimal form, as ICU does.
func formatChartNumber(v float64, format string) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	sign := ""
	if math.Signbit(v) {
		sign = "-"
	}
	abs := math.Abs(v)
	if math.IsInf(abs, 0) {
		return sign + "∞"
	}
	digits := chartFractionDigits(abs, format)
	switch format {
	case "integer":
		ip, _ := roundDecimal(abs, 0)
		return sign + groupDigits(ip)
	case "percent":
		// ×100 by moving the point, as ICU does: 0.0045*100 is 0.4499… in float64.
		ip, fp := roundDecimalText(timesHundred(strconv.FormatFloat(abs, 'f', -1, 64)), digits)
		out := groupDigits(ip)
		if fp != "" {
			out += "." + fp
		}
		return sign + out + "%"
	case "compact":
		units := []string{"", "K", "M", "B", "T"}
		u := 0
		for u < len(units)-1 && abs >= math.Pow(1000, float64(u+1)) {
			u++
		}
		for {
			ip, fp := roundDecimal(abs/math.Pow(1000, float64(u)), digits)
			if len(ip) > 3 && u < len(units)-1 {
				u++
				continue
			}
			if len(ip) >= 5 {
				ip = groupDigits(ip)
			}
			if fp != "" {
				ip += "." + fp
			}
			return sign + ip + units[u]
		}
	default:
		ip, fp := roundDecimal(abs, digits)
		out := groupDigits(ip)
		if fp != "" {
			out += "." + fp
		}
		return sign + out
	}
}

// roundDecimal rounds a non-negative v to at most digits fraction digits, half away
// from zero, on its shortest decimal form; the fraction comes back without trailing
// zeros.
func roundDecimal(v float64, digits int) (string, string) {
	return roundDecimalText(strconv.FormatFloat(v, 'f', -1, 64), digits)
}

// roundDecimalText is roundDecimal of a non-negative number's decimal text.
func roundDecimalText(s string, digits int) (string, string) {
	ip, fp, _ := strings.Cut(s, ".")
	if len(fp) > digits {
		up := fp[digits] >= '5'
		kept := []byte(ip + fp[:digits])
		if up {
			i := len(kept) - 1
			for ; i >= 0; i-- {
				if kept[i] == '9' {
					kept[i] = '0'
					continue
				}
				kept[i]++
				break
			}
			if i < 0 {
				kept = append([]byte{'1'}, kept...)
			}
		}
		ip, fp = string(kept[:len(kept)-digits]), string(kept[len(kept)-digits:])
	}
	return ip, strings.TrimRight(fp, "0")
}

// timesHundred is a non-negative number's decimal text times 100, with the point moved
// two places and no leading zeros.
func timesHundred(s string) string {
	ip, fp, _ := strings.Cut(s, ".")
	fp += "00"
	ip = strings.TrimLeft(ip+fp[:2], "0")
	if ip == "" {
		ip = "0"
	}
	if rest := strings.TrimRight(fp[2:], "0"); rest != "" {
		return ip + "." + rest
	}
	return ip
}

// groupDigits puts a comma between each three digits of a whole number.
func groupDigits(ip string) string {
	if len(ip) <= 3 {
		return ip
	}
	var b strings.Builder
	head := len(ip) % 3
	if head > 0 {
		b.WriteString(ip[:head])
	}
	for i := head; i < len(ip); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(ip[i : i+3])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// In the message
// ---------------------------------------------------------------------------

// messageImage is a picture an email carries, shown in place through its
// content id (Resend) or attached after the text (SMTP).
type messageImage struct {
	Filename  string
	ContentID string
	PNG       []byte
}

// workflowChartsSlackMax is how much of a Slack message the charts may take,
// shared between them; the message's own body keeps the rest.
const workflowChartsSlackMax = 2000

// chartNotDrawn stands in for a chart whose step drew nothing.
const chartNotDrawn = "A chart could not be drawn for this message; the run shows why."

// chartSections is the charts and dashboards a send runs after, in each of its
// formats, with the pictures and files an email carries.
type chartSections struct {
	Slack, Text, HTML string
	Images            []messageImage
	Files             []messageFile
}

// renderWorkflowDrawings writes each chart and dashboard for each format: email
// shows a plotted chart's picture, every format shows a scorecard's number and a
// table's first rows, and Slack, which cannot carry a picture, shows a plotted
// chart's first rows. Each links to the chart. A dashboard is its name, when it
// was read and a link, then its tiles in reading order with its PDF attached;
// in Slack, its name, its scorecards' numbers and the link.
func renderWorkflowDrawings(ds []workflowDrawing) chartSections {
	var out chartSections
	if len(ds) == 0 {
		return out
	}
	names, files := map[string]bool{}, map[string]bool{}
	budget := workflowChartsSlackMax / len(ds)
	picture := func(c *workflowChart) *messageImage {
		if len(c.PNG) == 0 {
			return nil
		}
		im := messageImage{Filename: chartFilename(c.Name, names),
			ContentID: fmt.Sprintf("chart-%d", len(out.Images)+1), PNG: c.PNG}
		out.Images = append(out.Images, im)
		return &im
	}
	var slack, text, htm []string
	for _, d := range ds {
		switch {
		case d.Dashboard != nil:
			b := d.Dashboard
			imgs := map[int]*messageImage{}
			for i, t := range b.Tiles {
				if t.Chart != nil {
					imgs[i] = picture(t.Chart)
				}
			}
			var file *messageFile
			if len(b.PDF) > 0 {
				out.Files = append(out.Files, messageFile{Filename: dashboardFilename(b.Name, b.ReadAt, files),
					ContentType: "application/pdf", Content: b.PDF})
				file = &out.Files[len(out.Files)-1]
			}
			slack = append(slack, dashboardSlack(b, budget))
			text = append(text, dashboardPlainText(b, imgs, file))
			htm = append(htm, dashboardHTML(b, imgs, file))
		case d.Chart != nil:
			img := picture(d.Chart)
			slack = append(slack, chartSlack(d.Chart, budget))
			text = append(text, chartPlainText(d.Chart, img))
			htm = append(htm, chartHTML(d.Chart, img))
		default:
			missing := chartNotDrawn
			if d.Kind == workflow.StepDashboard {
				missing = dashboardNotDrawn
			}
			slack = append(slack, "_"+slackEscapeText(missing)+"_")
			text = append(text, missing)
			htm = append(htm, `<p style="margin:24px 0 0;color:#71717a">`+html.EscapeString(missing)+`</p>`)
		}
	}
	out.Slack = strings.Join(slack, "\n\n")
	out.Text = "\n\n" + strings.Join(text, "\n\n")
	out.HTML = strings.Join(htm, "")
	return out
}

// chartMore is how many of a chart's rows the message does not show.
func chartMore(n int) string { return fmt.Sprintf("%d more row(s) not shown", n) }

// chartGrid lays rows out in aligned columns under their heads, as a workflow
// message's {{table}} does.
func chartGrid(t chartText, rows [][]string) []string {
	widths := make([]int, len(t.Head))
	for i, h := range t.Head {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if n := utf8.RuneCountInString(c); i < len(widths) && n > widths[i] {
				widths[i] = n
			}
		}
	}
	line := func(cs []string) string {
		parts := make([]string, len(cs))
		for i, c := range cs {
			pad := strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c))
			if i < len(t.Numeric) && t.Numeric[i] {
				parts[i] = pad + c
			} else {
				parts[i] = c + pad
			}
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	rule := make([]string, len(t.Head))
	for i := range rule {
		rule[i] = strings.Repeat("-", widths[i])
	}
	lines := []string{line(t.Head), line(rule)}
	for _, r := range rows {
		lines = append(lines, line(r))
	}
	return lines
}

// chartSlack is a chart in a Slack message, within budget runes: its name
// linked, then its number, its first rows, or why it has neither.
func chartSlack(c *workflowChart, budget int) string {
	t := c.Text
	var b strings.Builder
	b.WriteString(chartSlackHead(c, budget))
	var tail []string
	for _, n := range t.Notes {
		tail = append(tail, "_"+slackEscapeText(n)+"_")
	}
	switch {
	case t.Message != "":
		b.WriteString("\n_" + slackEscapeText(t.Message) + "_")
	case t.Number != "":
		b.WriteString("\n" + slackEscapeText(t.Label) + ": *" + slackEscapeText(t.Number) + "*")
		if t.Change != "" {
			b.WriteString("\n" + slackEscapeText(t.Change))
		}
	case len(t.Rows) == 0:
		b.WriteString("\n_(no rows)_")
	default:
		// The rows that fit: a line at a time, leaving room for the count of
		// the rest and the notes.
		reserve := utf8.RuneCountInString(b.String()) + len("\n```\n\n```\n_") + len(chartMore(len(t.Rows)+t.More)) + 1
		for _, l := range tail {
			reserve += utf8.RuneCountInString(l) + 1
		}
		lines := chartGrid(t, t.Rows)
		used, shown := utf8.RuneCountInString(lines[0])+1+utf8.RuneCountInString(lines[1]), 0
		for _, l := range lines[2:] {
			n := utf8.RuneCountInString(slackEscapeText(l)) + 1
			if reserve+used+n > budget {
				break
			}
			used += n
			shown++
		}
		b.WriteString("\n```\n" + slackEscapeText(strings.Join(lines[:2+shown], "\n")) + "\n```")
		if rest := len(t.Rows) - shown + t.More; rest > 0 {
			b.WriteString("\n_" + chartMore(rest) + "_")
		}
	}
	for _, l := range tail {
		b.WriteString("\n" + l)
	}
	return workflow.CapRunes(b.String(), budget)
}

// chartSlackHead is the chart's name linked to its page, within budget runes: a
// long name is cut, so the cap on the whole never cuts the link's markup. With
// no room for the link (many charts in one send), the name alone, cut to fit;
// the message's own link still ends it.
func chartSlackHead(c *workflowChart, budget int) string {
	for _, frame := range []struct{ open, close string }{{"*<" + c.Link + "|", ">*"}, {"*", "*"}} {
		room := budget - utf8.RuneCountInString(frame.open) - len(frame.close)
		for n := utf8.RuneCountInString(c.Name); n >= 1 && room > 0; n-- {
			if name := slackEscapeText(workflow.CapRunes(c.Name, n)); utf8.RuneCountInString(name) <= room {
				return frame.open + name + frame.close
			}
		}
	}
	return ""
}

// chartPlainText is a chart in a plain-text email.
func chartPlainText(c *workflowChart, img *messageImage) string {
	t := c.Text
	lines := []string{c.Name}
	switch {
	case t.Message != "":
		lines = append(lines, t.Message)
	case img != nil:
		lines = append(lines, "The chart is attached as "+img.Filename+".")
	case t.Number != "":
		lines = append(lines, t.Label+": "+t.Number)
		if t.Change != "" {
			lines = append(lines, t.Change)
		}
	case len(t.Rows) == 0:
		lines = append(lines, "(no rows)")
	default:
		lines = append(lines, chartGrid(t, t.Rows)...)
		if t.More > 0 {
			lines = append(lines, "("+chartMore(t.More)+")")
		}
	}
	lines = append(lines, t.Notes...)
	lines = append(lines, "Open the chart: "+c.Link)
	return strings.Join(lines, "\n")
}

// chartHTML is a chart in an HTML email: its picture, its number or its first
// rows, and a link to it.
func chartHTML(c *workflowChart, img *messageImage) string {
	t := c.Text
	esc := html.EscapeString
	link := esc(c.Link)
	var b strings.Builder
	b.WriteString(`<div style="margin-top:24px">`)
	b.WriteString(`<p style="margin:0 0 8px;font-weight:600"><a href="` + link +
		`" style="color:#18181b;text-decoration:none">` + esc(c.Name) + `</a></p>`)
	switch {
	case t.Message != "":
		b.WriteString(`<p style="margin:0;color:#71717a">` + esc(t.Message) + `</p>`)
	case img != nil:
		b.WriteString(`<a href="` + link + `"><img src="cid:` + esc(img.ContentID) + `" alt="` + esc(c.Name) +
			`" width="600" style="display:block;max-width:100%;height:auto;border:0"></a>`)
	case t.Number != "":
		b.WriteString(`<p style="margin:0;color:#71717a;font-size:12px">` + esc(t.Label) + `</p>`)
		b.WriteString(`<p style="margin:0;font-size:28px;font-weight:600">` + esc(t.Number) + `</p>`)
		if t.Change != "" {
			b.WriteString(`<p style="margin:4px 0 0;color:#71717a;font-size:12px">` + esc(t.Change) + `</p>`)
		}
	case len(t.Rows) == 0:
		b.WriteString(`<p style="margin:0;color:#71717a">(no rows)</p>`)
	default:
		b.WriteString(`<table style="border-collapse:collapse;font-family:sans-serif;font-size:13px"><tr>`)
		for i, h := range t.Head {
			b.WriteString(`<th style="border:1px solid #ccc;padding:4px 8px;text-align:` + chartAlign(t, i) +
				`;background:#f4f4f5">` + esc(h) + `</th>`)
		}
		b.WriteString(`</tr>`)
		for _, r := range t.Rows {
			b.WriteString(`<tr>`)
			for i, cell := range r {
				b.WriteString(`<td style="border:1px solid #ccc;padding:4px 8px;text-align:` + chartAlign(t, i) + `">` +
					esc(cell) + `</td>`)
			}
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</table>`)
		if t.More > 0 {
			b.WriteString(`<p style="margin:4px 0 0"><em>` + esc(chartMore(t.More)) + `</em></p>`)
		}
	}
	for _, n := range t.Notes {
		b.WriteString(`<p style="margin:4px 0 0;color:#b45309;font-size:12px">` + esc(n) + `</p>`)
	}
	b.WriteString(`<p style="margin:8px 0 0;font-size:12px"><a href="` + link + `">Open the chart in rsync.ai</a></p></div>`)
	return b.String()
}

func chartAlign(t chartText, i int) string {
	if i < len(t.Numeric) && t.Numeric[i] {
		return "right"
	}
	return "left"
}

var chartFileUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// chartFilename is a chart's name as a file name, once each in a message.
func chartFilename(name string, seen map[string]bool) string {
	slug := strings.Trim(chartFileUnsafe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	if slug == "" {
		slug = "chart"
	}
	base := slug
	for n := 2; seen[slug]; n++ {
		slug = fmt.Sprintf("%s-%d", base, n)
	}
	seen[slug] = true
	return slug + ".png"
}
