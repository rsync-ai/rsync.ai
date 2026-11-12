package handlers

// dashboard_changes.go is a change to one dashboard tile by asking, and a dashboard's
// versions (dashboards-design.md § 7.6, § 7.7). Ask rsync.ai, or an agent over the MCP
// gateway, drafts a change to one tile: its chart (type, columns, measure, sort, colour,
// name) and its size. The server checks the chart as a save would, running its query
// once, moves the tiles in the way down, and keeps the change for its person for 24
// hours. A change saves nothing: an owner or admin applies it from a card that shows the
// tile before and after. A dashboard saved since the change was drafted makes it out of
// date; it is never applied over the newer dashboard. A chart that is also on another
// dashboard is copied for this tile, so the other dashboard does not change; a chart on
// this dashboard only is changed in place. Every save of a dashboard keeps the version
// before it, and a restore is a save of an old version, so no version is ever lost.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"api-gateway/internal/charts"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	workflow "github.com/rsync-ai/shared/workflowdef"
	log "github.com/sirupsen/logrus"
)

const (
	dashboardChangeTTL = 24 * time.Hour
	// dashboardChangesOpenMax caps a person's unapplied, unexpired changes.
	dashboardChangesOpenMax = 20
	// dashboardChangeSummaryMax caps the model's one-line summary of a change.
	dashboardChangeSummaryMax = 300
	// dashboardVersionsMax is how many versions the History dialog lists.
	dashboardVersionsMax = 100
	// DashboardsForModelMax caps list_dashboards: the most recently saved first.
	DashboardsForModelMax = 200
	// dashboardMinTileWidth is the editor's narrowest tile (DashboardGridEditor minW).
	dashboardMinTileWidth = 2
	// dashboardMinTileRows and dashboardMinScorecardRows are the editor's shortest
	// tiles (dashboard-layout.ts MIN_TILE_ROWS, MIN_SCORECARD_ROWS).
	dashboardMinTileRows      = 2
	dashboardMinScorecardRows = 3
)

// checkChangedChart is checkChartForSave, which runs the chart's source once; a test
// replaces it, as that needs a live connection.
var checkChangedChart = checkChartForSave

// DashboardChangeInput is what Ask rsync.ai or an agent drafts: one tile of one
// dashboard and what changes. A field left empty keeps what the tile has.
type DashboardChangeInput struct {
	DashboardID string
	TileID      string
	// Name is the chart's new name; "" keeps it.
	Name string
	// Settings are the chart's whole new settings; nil keeps them. Settings the tools
	// cannot set are kept while their column stays (keepChartSettings).
	Settings json.RawMessage
	// Color is a series token for the measure; "" keeps it.
	Color string
	// W and H are the tile's new width in columns and height in rows; 0 keeps them.
	W int
	H int
	// Summary is the model's one line on what the change does.
	Summary string
}

// DashboardChangeSide is the tile before or after a change.
type DashboardChangeSide struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Settings json.RawMessage `json:"settings"`
	X        int             `json:"x"`
	Y        int             `json:"y"`
	W        int             `json:"w"`
	H        int             `json:"h"`
}

// DashboardChangeMove is a tile moved down to make room for the changed one.
type DashboardChangeMove struct {
	TileID string `json:"tile_id"`
	FromY  int    `json:"from_y"`
	Y      int    `json:"y"`
}

// dashboardChangeBody is what the change column holds.
type dashboardChangeBody struct {
	DashboardName   string                `json:"dashboard_name"`
	Before          DashboardChangeSide   `json:"before"`
	After           DashboardChangeSide   `json:"after"`
	Moves           []DashboardChangeMove `json:"moves"`
	ChartUpdatedAt  time.Time             `json:"chart_updated_at"`
	SourceID        string                `json:"source_id"`
	SourceName      string                `json:"source_name"`
	ChartVisibility string                `json:"chart_visibility"`
}

// DashboardChange is a checked change to one tile.
type DashboardChange struct {
	ID            string                `json:"id"`
	DashboardID   string                `json:"dashboard_id"`
	DashboardName string                `json:"dashboard_name"`
	TileID        string                `json:"tile_id"`
	ChartID       string                `json:"chart_id"`
	Before        DashboardChangeSide   `json:"before"`
	After         DashboardChangeSide   `json:"after"`
	Moves         []DashboardChangeMove `json:"moves"`
	Summary       string                `json:"summary"`
	// ChartChanged and SizeChanged say what the change does to the tile.
	ChartChanged bool `json:"chart_changed"`
	SizeChanged  bool `json:"size_changed"`
	// SharedElsewhere counts the other dashboards with the chart on them; Copy says the
	// change copies the chart for this tile rather than changing it on them all.
	SharedElsewhere int    `json:"shared_elsewhere"`
	Copy            bool   `json:"copy"`
	SourceID        string `json:"source_id"`
	SourceName      string `json:"source_name"`
	Status          string `json:"status"`
	Expired         bool   `json:"expired"`
	// Stale says the dashboard, the tile or its chart changed since the change was
	// drafted; such a change is never applied.
	Stale          bool       `json:"stale"`
	AppliedChartID string     `json:"applied_chart_id,omitempty"`
	Copied         bool       `json:"copied"`
	ProposedBy     string     `json:"proposed_by"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	AppliedAt      *time.Time `json:"applied_at,omitempty"`
	URL            string     `json:"url"`

	baseUpdatedAt  time.Time
	chartUpdatedAt time.Time
	chartVis       string
}

// DashboardURL is the dashboard's page in rsync.ai.
func DashboardURL(dashboardID string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/")
	if base == "" {
		base = "http://localhost:3000"
	}
	return base + "/dashboards/" + dashboardID
}

// dashboardChangeURL is the dashboard in rsync.ai, with the change's card open on it.
func dashboardChangeURL(dashboardID, changeID string) string {
	return DashboardURL(dashboardID) + "?change=" + changeID
}

// chartToolAttrs are the attributes the LLM tool sets on each role's fields (agentgw's
// dashboard chart arguments); a role not listed takes only its column from the tool.
var chartToolAttrs = map[string][]string{
	charts.RoleX: {charts.AttrColumn, charts.AttrGrain, charts.AttrBins, charts.AttrWidth},
	charts.RoleMeasure: {charts.AttrColumn, charts.AttrAggregate, charts.AttrTitle, charts.AttrFormat,
		charts.AttrMark, charts.AttrAxis},
}

// keepChartSettings carries over what the tools cannot set from the chart's settings to
// its new ones (dashboards-design.md § 7.6, § 14.2 gap 3): every attribute the new
// type's role takes and the tool does not set, such as an axis title or the series
// colours, while the field's column stays; each measure's colour, whatever its column;
// and the description. It reads the registry, so a new type's or a new attribute's
// settings are kept with no change here. Series colours pin a colour to a series by its
// value, which a model is never shown.
func keepChartSettings(old, next *charts.Settings) {
	if spec := charts.Spec(next.Type); spec != nil {
		for role, fields := range next.Roles {
			rs := spec.Role(role)
			if rs == nil {
				continue
			}
			var keep []string
			for _, a := range rs.Attrs {
				if a != charts.AttrColumn && !slices.Contains(chartToolAttrs[role], a) {
					keep = append(keep, a)
				}
			}
			for i := range fields {
				if o := keptField(old, role, i, fields[i].Column); o != nil {
					fields[i].KeepFrom(o, keep)
				}
			}
		}
	}
	if next.Description == "" {
		next.Description = old.Description
	}
}

// keptField is the old chart's field a new one keeps settings from: the one in the same
// role with the same column, or the measure in the same place (a combo's first or
// second), which stays that number when its column changes.
func keptField(old *charts.Settings, role string, at int, column string) *charts.Field {
	if role == charts.RoleMeasure {
		if at < len(old.Roles[role]) {
			return &old.Roles[role][at]
		}
		return nil
	}
	for i, f := range old.Roles[role] {
		if f.Column == column {
			return &old.Roles[role][i]
		}
	}
	return nil
}

// dashboardMinRows is the fewest rows a tile of chart type t may have.
func dashboardMinRows(t string) int {
	if t == charts.TypeScorecard {
		return dashboardMinScorecardRows
	}
	return dashboardMinTileRows
}

func tilesOverlap(a, b charts.Tile) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// pushTilesDown places tile at its new size and moves the tiles in its way down, as the
// editor does when a tile's edge is dragged: the tile keeps its row, moved left only to
// stay inside the grid; each other tile, in reading order, moves down below any tile it
// would overlap. No tile moves up. ids name tiles by index; moves are the tiles that
// moved.
func pushTilesDown(tiles []charts.Tile, ids []string, at int) ([]charts.Tile, []DashboardChangeMove) {
	out := slices.Clone(tiles)
	out[at].X = min(out[at].X, charts.GridColumns-out[at].W)
	order := make([]int, 0, len(out))
	for i := range out {
		if i != at {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if out[a].Y != out[b].Y {
			return out[a].Y - out[b].Y
		}
		return out[a].X - out[b].X
	})
	placed := []int{at}
	moves := []DashboardChangeMove{}
	for _, i := range order {
		from := out[i].Y
		for {
			hit := -1
			for _, p := range placed {
				if tilesOverlap(out[i], out[p]) {
					hit = p
					break
				}
			}
			if hit < 0 {
				break
			}
			out[i].Y = out[hit].Y + out[hit].H
		}
		if out[i].Y != from {
			moves = append(moves, DashboardChangeMove{TileID: ids[i], FromY: from, Y: out[i].Y})
		}
		placed = append(placed, i)
	}
	return out, moves
}

// chartOnOtherDashboards counts the dashboards other than dashboardID with chartID on a
// tile, whoever may see them.
func chartOnOtherDashboards(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ws, chartID, dashboardID string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT dashboard_id) FROM dashboard_tiles
		WHERE workspace_id = $1 AND chart_id = $2 AND dashboard_id <> $3
	`, ws, chartID, dashboardID).Scan(&n)
	return n, err
}

// ProposeDashboardChange checks a change to one tile that Ask rsync.ai or an agent
// drafted and keeps it for actor's user to apply. A changed chart's query runs once, to
// check it; nothing is saved.
func ProposeDashboardChange(c *gin.Context, actor services.Actor, in DashboardChangeInput) (*DashboardChange, error) {
	if actor.Channel != services.ChannelAssistant && actor.Channel != services.ChannelAgent {
		return nil, refusePlan("forbidden", http.StatusForbidden, "Only Ask rsync.ai or an agent token drafts a dashboard change.")
	}
	if err := services.Authorize(actor, services.ActionDashboardPlan); err != nil {
		return nil, dashboardForbidden(actor)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	ws, userID := actor.WorkspaceID, actor.UserID

	d, err := loadVisibleDashboard(ctx, database, ws, userID, strings.TrimSpace(in.DashboardID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refusePlan("not_found", http.StatusNotFound,
			"No dashboard with that id is visible to you; list_dashboards lists them.")
	}
	if err != nil {
		return nil, err
	}
	at := slices.IndexFunc(d.Tiles, func(t *dashboardTile) bool { return t.ID == strings.TrimSpace(in.TileID) })
	if at < 0 {
		return nil, refusePlan("not_found", http.StatusNotFound,
			"The dashboard %q has no tile with that id; get_dashboard lists its tiles.", d.Name)
	}
	tile := d.Tiles[at]
	if tile.Kind != charts.TileChart {
		return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
			"That tile is a text tile; only a chart tile is changed by asking. The person edits text in the dashboard's editor.")
	}
	if tile.Chart == nil {
		return nil, refusePlan("chart_hidden", http.StatusUnprocessableEntity,
			"That tile's chart is private to someone else, so it cannot be changed.")
	}
	ch := tile.Chart
	if ch.SourceState != chartSourceOK {
		return nil, refusePlan("source_"+ch.SourceState, http.StatusUnprocessableEntity,
			"That tile's chart draws a source that is %s, so it cannot be changed; the person picks a new source in the chart's editor.", ch.SourceState)
	}
	old, err := charts.Decode(ch.Settings)
	if err != nil {
		return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
			"That tile's chart has settings rsync.ai no longer reads; the person fixes it in the chart's editor.")
	}

	before := DashboardChangeSide{Name: ch.Name, Type: old.Type, Settings: ch.Settings,
		X: tile.X, Y: tile.Y, W: tile.W, H: tile.H}
	after := before

	// The chart.
	name := strings.Join(strings.Fields(in.Name), " ")
	if name == "" {
		name = ch.Name
	}
	color := strings.TrimSpace(in.Color)
	next := old
	if len(in.Settings) > 0 {
		next, err = charts.Decode(in.Settings)
		if err != nil {
			return nil, planFromChartRefusal(settingsRefusal(err), "")
		}
		keepChartSettings(old, next)
	} else if color != "" {
		next = old.Clone()
	}
	if color != "" {
		m := next.One(charts.RoleMeasure)
		if m == nil {
			return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
				"A %s chart has no measure to colour.", next.Type)
		}
		if next.One(charts.RoleSeries) != nil {
			return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
				"This chart has a series, and each series takes its own colour; the person picks them in the chart's editor.")
		}
		m.Color = color
	}
	nextJSON, err := json.Marshal(next)
	if err != nil {
		return nil, fmt.Errorf("marshal the settings: %w", err)
	}
	oldJSON, err := json.Marshal(old)
	if err != nil {
		return nil, fmt.Errorf("marshal the settings: %w", err)
	}
	chartChanged := name != ch.Name || string(nextJSON) != string(oldJSON)
	if chartChanged {
		checked, refusal, err := checkChangedChart(ctx, database, ws, userID, chartInput{
			Name: name, SourceID: ch.SourceID, Settings: nextJSON, Visibility: ch.Visibility,
		})
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return nil, planFromChartRefusal(refusal, "")
		}
		after.Name, after.Type, after.Settings = checked.Name, checked.Settings.Type, checked.SettingsJSON
	}

	// The size. A type change can make the tile too short for its new type.
	if in.W != 0 {
		if in.W < dashboardMinTileWidth || in.W > charts.GridColumns {
			return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
				"A tile is %d to %d columns wide.", dashboardMinTileWidth, charts.GridColumns)
		}
		after.W = in.W
	}
	minRows := dashboardMinRows(after.Type)
	if in.H != 0 {
		if in.H < minRows || in.H > charts.MaxTileHeight {
			return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
				"A %s tile is %d to %d rows tall.", after.Type, minRows, charts.MaxTileHeight)
		}
		after.H = in.H
	}
	after.H = max(after.H, minRows)
	tiles := make([]charts.Tile, len(d.Tiles))
	ids := make([]string, len(d.Tiles))
	for i, t := range d.Tiles {
		tiles[i], ids[i] = t.Tile, t.ID
	}
	tiles[at].W, tiles[at].H = after.W, after.H
	placed, moves := pushTilesDown(tiles, ids, at)
	after.X = placed[at].X
	if err := charts.ValidateTiles(placed); err != nil {
		return nil, planFromChartRefusal(settingsRefusal(err), "")
	}
	sizeChanged := after.X != before.X || after.W != before.W || after.H != before.H
	if !chartChanged && !sizeChanged {
		return nil, refusePlan("no_change", http.StatusUnprocessableEntity,
			"That changes nothing: the tile already has this chart and size.")
	}

	var open int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dashboard_changes
		WHERE workspace_id = $1 AND created_by = $2 AND status = 'proposed' AND expires_at > NOW()
	`, ws, userID).Scan(&open); err != nil {
		return nil, fmt.Errorf("count dashboard changes: %w", err)
	}
	if open >= dashboardChangesOpenMax {
		return nil, refusePlan("too_many_changes", http.StatusTooManyRequests,
			"You have %d dashboard changes not applied. Apply some, or let them expire, before drafting more.", open)
	}
	shared, err := chartOnOtherDashboards(ctx, database, ws, ch.ID, d.ID)
	if err != nil {
		return nil, fmt.Errorf("count the chart's dashboards: %w", err)
	}

	body := dashboardChangeBody{DashboardName: d.Name, Before: before, After: after, Moves: moves,
		ChartUpdatedAt: ch.UpdatedAt, SourceID: ch.SourceID, SourceName: ch.SourceName, ChartVisibility: ch.Visibility}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal dashboard change: %w", err)
	}
	summary := workflow.CapRunes(strings.Join(strings.Fields(in.Summary), " "), dashboardChangeSummaryMax)
	var tokenID *string
	if actor.Channel == services.ChannelAgent {
		tokenID = nullString(c.GetString(ctxTokenID))
	}
	out := &DashboardChange{DashboardID: d.ID, DashboardName: d.Name, TileID: tile.ID, ChartID: ch.ID,
		Before: before, After: after, Moves: moves, Summary: summary,
		ChartChanged: chartChanged, SizeChanged: sizeChanged, SharedElsewhere: shared, Copy: chartChanged && shared > 0,
		SourceID: ch.SourceID, SourceName: ch.SourceName, Status: planStatusProposed, ProposedBy: actorKindFor(actor.Channel)}
	if err := database.QueryRowContext(ctx, `
		INSERT INTO dashboard_changes (workspace_id, dashboard_id, tile_id, chart_id, created_by, actor_kind, token_id,
		                               base_updated_at, change, summary, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW() + ($11 * INTERVAL '1 second'))
		RETURNING id::text, created_at, expires_at
	`, ws, d.ID, tile.ID, ch.ID, userID, out.ProposedBy, tokenID, d.UpdatedAt, string(raw), summary,
		int(dashboardChangeTTL.Seconds()),
	).Scan(&out.ID, &out.CreatedAt, &out.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store dashboard change: %w", err)
	}
	out.URL = dashboardChangeURL(d.ID, out.ID)

	logAudit(c, "plan_dashboard_change", "dashboard_change", out.ID, map[string]interface{}{
		"dashboard_id": d.ID,
		"tile_id":      tile.ID,
		"chart":        chartChanged,
		"size":         sizeChanged,
		"moved":        len(moves),
		"copy":         out.Copy,
		"channel":      string(actor.Channel),
	})
	return out, nil
}

// loadDashboardChange reads a change of userID's in ws, or sql.ErrNoRows.
func loadDashboardChange(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ws, userID, id string, forUpdate bool) (*DashboardChange, error) {
	if !validUUID(id) {
		return nil, sql.ErrNoRows
	}
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	ch := &DashboardChange{}
	var raw []byte
	var appliedChart sql.NullString
	var appliedAt sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT id::text, dashboard_id::text, tile_id::text, chart_id::text, actor_kind, base_updated_at, change,
		       summary, status, created_at, expires_at, expires_at <= NOW(), applied_at,
		       applied_chart_id::text, copied
		FROM dashboard_changes
		WHERE id = $1 AND workspace_id = $2 AND created_by = $3`+lock,
		id, ws, userID).Scan(&ch.ID, &ch.DashboardID, &ch.TileID, &ch.ChartID, &ch.ProposedBy, &ch.baseUpdatedAt, &raw,
		&ch.Summary, &ch.Status, &ch.CreatedAt, &ch.ExpiresAt, &ch.Expired, &appliedAt, &appliedChart, &ch.Copied)
	if err != nil {
		return nil, err
	}
	var body dashboardChangeBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("read dashboard change: %w", err)
	}
	ch.DashboardName, ch.Before, ch.After, ch.Moves = body.DashboardName, body.Before, body.After, body.Moves
	ch.Before.Settings, ch.After.Settings = charts.AsVersion2(ch.Before.Settings), charts.AsVersion2(ch.After.Settings)
	if ch.Moves == nil {
		ch.Moves = []DashboardChangeMove{}
	}
	ch.SourceID, ch.SourceName = body.SourceID, body.SourceName
	ch.chartUpdatedAt, ch.chartVis = body.ChartUpdatedAt, body.ChartVisibility
	ch.ChartChanged = ch.Before.Name != ch.After.Name || string(ch.Before.Settings) != string(ch.After.Settings)
	ch.SizeChanged = ch.Before.X != ch.After.X || ch.Before.W != ch.After.W || ch.Before.H != ch.After.H
	ch.AppliedChartID = appliedChart.String
	if appliedAt.Valid {
		t := appliedAt.Time
		ch.AppliedAt = &t
	}
	ch.Expired = ch.Expired && ch.Status == planStatusProposed
	ch.URL = dashboardChangeURL(ch.DashboardID, ch.ID)
	return ch, nil
}

func writeDashboardChangeLoadError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "dashboard change not found"})
		return
	}
	log.WithError(err).Error("[dashboard_changes] load failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the dashboard change"})
}

// dashboardChangeCurrent reads the dashboard a proposed change is on and says whether
// the change is out of date: the dashboard saved since, or no longer visible to
// userID, or the tile or its chart gone or changed. d is nil when it is not visible.
func dashboardChangeCurrent(ctx context.Context, database *sql.DB, ws, userID string, ch *DashboardChange) (*dashboardRecord, bool, error) {
	d, err := loadVisibleDashboard(ctx, database, ws, userID, ch.DashboardID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !d.UpdatedAt.Equal(ch.baseUpdatedAt) {
		return d, true, nil
	}
	at := slices.IndexFunc(d.Tiles, func(t *dashboardTile) bool { return t.ID == ch.TileID })
	if at < 0 || d.Tiles[at].ChartID != ch.ChartID || d.Tiles[at].Chart == nil {
		return d, true, nil
	}
	if ch.ChartChanged && !d.Tiles[at].Chart.UpdatedAt.Equal(ch.chartUpdatedAt) {
		return d, true, nil
	}
	return d, false, nil
}

// GetDashboardChange returns one of the caller's changes, with whether it is out of
// date and whether it would copy the chart now.
// GET /api/v1/dashboard-changes/:id
func GetDashboardChange(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	ch, err := loadDashboardChange(ctx, database, ws, userID, c.Param("id"), false)
	if err != nil {
		writeDashboardChangeLoadError(c, err)
		return
	}
	if ch.Status == planStatusProposed {
		_, stale, err := dashboardChangeCurrent(ctx, database, ws, userID, ch)
		if err != nil {
			writeDashboardChangeLoadError(c, err)
			return
		}
		ch.Stale = stale
		n, err := chartOnOtherDashboards(ctx, database, ws, ch.ChartID, ch.DashboardID)
		if err != nil {
			writeDashboardChangeLoadError(c, err)
			return
		}
		ch.SharedElsewhere, ch.Copy = n, ch.ChartChanged && n > 0
	} else {
		ch.Copy = ch.Copied
	}
	c.JSON(http.StatusOK, gin.H{"change": ch, "can_manage": role.Meets(chartWriteRole)})
}

// writeDashboardChangeStale is the 409 an out-of-date change gets.
func writeDashboardChangeStale(c *gin.Context) {
	c.JSON(http.StatusConflict, gin.H{
		"error": "This change is out of date: the dashboard was saved after it was drafted. Ask rsync.ai to draft it again.",
		"code":  "dashboard_changed",
	})
}

// ApplyDashboardChange applies one of the caller's changes, after checking the chart
// again with the caller's rights. The dashboard as it was is kept as a version.
// POST /api/v1/dashboard-changes/:id/apply
func ApplyDashboardChange(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	ch, err := loadDashboardChange(ctx, database, ws, userID, c.Param("id"), false)
	if err != nil {
		writeDashboardChangeLoadError(c, err)
		return
	}
	if ch.Status != planStatusProposed {
		c.JSON(http.StatusConflict, gin.H{"error": "This change is already applied.", "code": "already_applied"})
		return
	}
	if ch.Expired {
		c.JSON(http.StatusGone, gin.H{"error": "This change expired. Ask rsync.ai to draft it again.", "code": "expired"})
		return
	}
	d, stale, err := dashboardChangeCurrent(ctx, database, ws, userID, ch)
	if err != nil {
		writeDashboardChangeLoadError(c, err)
		return
	}
	if stale {
		writeDashboardChangeStale(c)
		return
	}
	var checked *checkedChart
	if ch.ChartChanged {
		var refusal *chartRefusal
		checked, refusal, err = checkChangedChart(ctx, database, ws, userID, chartInput{
			Name: ch.After.Name, SourceID: ch.SourceID, Settings: ch.After.Settings, Visibility: ch.chartVis,
		})
		if err != nil {
			log.WithError(err).Error("[dashboard_changes] check failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the chart"})
			return
		}
		if refusal != nil {
			writeChartRefusal(c, refusal)
			return
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply the change"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	fail := func(err error, what string) {
		log.WithError(err).Error("[dashboard_changes] " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to apply the change"})
	}
	// The change is locked, so two applies of it cannot both write.
	locked, err := loadDashboardChange(ctx, tx, ws, userID, ch.ID, true)
	if err != nil {
		writeDashboardChangeLoadError(c, err)
		return
	}
	if locked.Status != planStatusProposed || locked.Expired {
		c.JSON(http.StatusConflict, gin.H{"error": "This change was applied or expired meanwhile.", "code": "already_applied"})
		return
	}
	// The dashboard's row is the save's lock: one saved since the change was drafted
	// matches no row, and the change is out of date.
	res, err := tx.ExecContext(ctx, `
		UPDATE dashboards SET updated_at = now()
		WHERE id = $1 AND workspace_id = $2 AND updated_at = $3
	`, ch.DashboardID, ws, ch.baseUpdatedAt)
	if err != nil {
		fail(err, "dashboard update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeDashboardChangeStale(c)
		return
	}
	if err := insertDashboardVersion(ctx, tx, ws, userID, d, dashboardSavedByAsk, ch.ID, 0); err != nil {
		if isUniqueViolation(err) {
			writeDashboardChangeStale(c)
			return
		}
		fail(err, "version insert failed")
		return
	}

	chartID, copied := ch.ChartID, false
	if checked != nil {
		shared, err := chartOnOtherDashboards(ctx, tx, ws, ch.ChartID, ch.DashboardID)
		if err != nil {
			fail(err, "dashboards lookup failed")
			return
		}
		if shared > 0 {
			// The chart is on another dashboard too: this tile gets its own copy, so the
			// other dashboard does not change.
			chartID, err = insertChart(ctx, tx, ws, userID, checked, ch.ProposedBy)
			if err != nil {
				fail(err, "chart copy failed")
				return
			}
			copied = true
		} else {
			cols, err := json.Marshal(checked.SourceColumns)
			if err != nil {
				fail(err, "marshal columns failed")
				return
			}
			res, err := tx.ExecContext(ctx, `
				UPDATE charts
				SET name = $3, settings = $4, settings_version = $5, source_columns = $6, updated_at = now()
				WHERE id = $1 AND workspace_id = $2 AND updated_at = $7
			`, ch.ChartID, ws, checked.Name, string(checked.SettingsJSON), charts.Version, string(cols), ch.chartUpdatedAt)
			if err != nil {
				fail(err, "chart update failed")
				return
			}
			if n, _ := res.RowsAffected(); n == 0 {
				writeDashboardChangeStale(c)
				return
			}
		}
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE dashboard_tiles SET chart_id = $4, x = $5, w = $6, h = $7
		WHERE id = $1 AND dashboard_id = $2 AND workspace_id = $3 AND chart_id = $8
	`, ch.TileID, ch.DashboardID, ws, chartID, ch.After.X, ch.After.W, ch.After.H, ch.ChartID)
	if err != nil {
		fail(err, "tile update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeDashboardChangeStale(c)
		return
	}
	for _, m := range ch.Moves {
		res, err := tx.ExecContext(ctx, `
			UPDATE dashboard_tiles SET y = $4
			WHERE id = $1 AND dashboard_id = $2 AND workspace_id = $3 AND y = $5
		`, m.TileID, ch.DashboardID, ws, m.Y, m.FromY)
		if err != nil {
			fail(err, "tile move failed")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeDashboardChangeStale(c)
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE dashboard_changes SET status = 'applied', applied_at = NOW(), applied_chart_id = $2, copied = $3
		WHERE id = $1 AND status = 'proposed'
	`, ch.ID, chartID, copied); err != nil {
		fail(err, "change update failed")
		return
	}
	if err := tx.Commit(); err != nil {
		fail(err, "commit failed")
		return
	}

	if checked != nil {
		action := "update_chart"
		if copied {
			action = "create_chart"
		}
		details := gin.H{
			"name": checked.Name, "type": checked.Settings.Type, "source_id": checked.Source.ID,
			"change_id": ch.ID, "proposed_by": ch.ProposedBy,
		}
		if copied {
			details["copied_from"] = ch.ChartID
		}
		logAudit(c, action, "chart", chartID, details)
	}
	logAudit(c, "apply_dashboard_change", "dashboard", ch.DashboardID, gin.H{
		"change_id": ch.ID, "tile_id": ch.TileID, "chart_id": chartID, "copied": copied,
		"moved": len(ch.Moves), "proposed_by": ch.ProposedBy,
	})
	c.JSON(http.StatusOK, gin.H{"dashboard_id": ch.DashboardID, "chart_id": chartID, "copied": copied})
}

// DashboardVersion is a dashboard as it was before one save (§ 7.7): saved_by, how and
// created_at are that save's.
type DashboardVersion struct {
	Version      int             `json:"version"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Filters      []charts.Filter `json:"filters"`
	Visibility   string          `json:"visibility"`
	Tiles        []charts.Tile   `json:"tiles"`
	SavedBy      string          `json:"saved_by"`
	How          string          `json:"how"`
	ChangeID     string          `json:"change_id,omitempty"`
	RestoredFrom int             `json:"restored_from,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ListDashboardVersions lists a dashboard's versions, newest first.
// GET /api/v1/dashboards/:id/versions
func ListDashboardVersions(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	// History is as private as the dashboard: only one the caller may see.
	d, err := loadVisibleDashboard(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	rows, err := database.QueryContext(ctx, `
		SELECT v.version, v.name, v.description, v.filters, v.visibility, v.tiles,
		       COALESCE(NULLIF(u.name, ''), u.email, ''), v.how, COALESCE(v.change_id::text, ''),
		       COALESCE(v.restored_from, 0), v.created_at
		FROM dashboard_versions v
		LEFT JOIN users u ON u.id = v.saved_by
		WHERE v.dashboard_id = $1 AND v.workspace_id = $2
		ORDER BY v.version DESC
		LIMIT $3
	`, d.ID, ws, dashboardVersionsMax)
	if err != nil {
		if isUndefinedTable(err) {
			c.JSON(http.StatusOK, gin.H{"versions": []DashboardVersion{}, "count": 0})
			return
		}
		log.WithError(err).Error("[dashboards] list versions failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list versions"})
		return
	}
	defer rows.Close()
	out := []DashboardVersion{}
	for rows.Next() {
		var v DashboardVersion
		var filters, tiles []byte
		if err := rows.Scan(&v.Version, &v.Name, &v.Description, &filters, &v.Visibility, &tiles,
			&v.SavedBy, &v.How, &v.ChangeID, &v.RestoredFrom, &v.CreatedAt); err != nil {
			log.WithError(err).Error("[dashboards] scan version failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list versions"})
			return
		}
		if err := json.Unmarshal(filters, &v.Filters); err != nil || v.Filters == nil {
			v.Filters = []charts.Filter{}
		}
		if err := json.Unmarshal(tiles, &v.Tiles); err != nil || v.Tiles == nil {
			v.Tiles = []charts.Tile{}
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("[dashboards] list versions failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list versions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"versions": out,
		"count":    len(out),
		// What a restore is saved over, so the dialog sends its updated_at and keeps
		// who sees the dashboard as it is now.
		"current": gin.H{
			"name":       d.Name,
			"visibility": d.Visibility,
			"updated_at": d.UpdatedAt,
		},
		"can_manage": role.Meets(chartWriteRole),
	})
}

// ---------------------------------------------------------------------------
// For the tools (list_dashboards, get_dashboard)
// ---------------------------------------------------------------------------

// DashboardItem is one dashboard as list_dashboards shows it.
type DashboardItem struct {
	ID         string
	Name       string
	Visibility string
	TileCount  int
	UpdatedAt  time.Time
}

// DashboardOutlineTile is one tile as get_dashboard shows it. A chart tile carries its
// chart's settings when the person may see the chart; a text tile never its text.
type DashboardOutlineTile struct {
	ID         string
	Kind       string
	X, Y, W, H int
	ChartID    string
	ChartName  string
	ChartState string
	SourceID   string
	SourceName string
	Settings   *charts.Settings
	// SharedElsewhere counts the other dashboards with the tile's chart on them.
	SharedElsewhere int
}

// DashboardOutline is a dashboard as get_dashboard shows it.
type DashboardOutline struct {
	ID          string
	Name        string
	Description string
	Visibility  string
	Filters     []charts.Filter
	Tiles       []DashboardOutlineTile
	UpdatedAt   time.Time
}

func dashboardReadForbidden() *PlanRefusal {
	return refusePlan("forbidden", http.StatusForbidden,
		"This token lacks the %s scope. Create a token that has it.", services.ScopeMetadataRead)
}

// ListDashboardsAs is ListDashboards for list_dashboards: the dashboards actor may see,
// the most recently saved first, at most DashboardsForModelMax+1 so the tool can say it
// cut the list.
func ListDashboardsAs(c *gin.Context, actor services.Actor) ([]DashboardItem, error) {
	if err := services.Authorize(actor, services.ActionChartRead); err != nil {
		return nil, dashboardReadForbidden()
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	rows, err := database.QueryContext(c.Request.Context(),
		dashboardSelect+` ORDER BY d.updated_at DESC, d.id LIMIT $3`, actor.WorkspaceID, actor.UserID, DashboardsForModelMax+1)
	if err != nil {
		if isUndefinedTable(err) {
			return []DashboardItem{}, nil
		}
		return nil, fmt.Errorf("list dashboards: %w", err)
	}
	defer rows.Close()
	out := []DashboardItem{}
	for rows.Next() {
		d, err := scanDashboard(rows.Scan, actor.UserID)
		if err != nil {
			return nil, err
		}
		out = append(out, DashboardItem{ID: d.ID, Name: d.Name, Visibility: d.Visibility, TileCount: d.TileCount, UpdatedAt: d.UpdatedAt})
	}
	return out, rows.Err()
}

// GetDashboardAs is GetDashboard for get_dashboard: the dashboard's tiles and, for each
// chart the person may see, its settings. No tile is drawn and no text tile's text is
// returned.
func GetDashboardAs(c *gin.Context, actor services.Actor, id string) (*DashboardOutline, error) {
	if err := services.Authorize(actor, services.ActionChartRead); err != nil {
		return nil, dashboardReadForbidden()
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	d, err := loadVisibleDashboard(ctx, database, actor.WorkspaceID, actor.UserID, strings.TrimSpace(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refusePlan("not_found", http.StatusNotFound,
			"No dashboard with that id is visible to you; list_dashboards lists them.")
	}
	if err != nil {
		return nil, err
	}
	out := &DashboardOutline{ID: d.ID, Name: d.Name, Description: d.Description, Visibility: d.Visibility,
		Filters: d.Filters, Tiles: []DashboardOutlineTile{}, UpdatedAt: d.UpdatedAt}
	for _, t := range d.Tiles {
		o := DashboardOutlineTile{ID: t.ID, Kind: t.Kind, X: t.X, Y: t.Y, W: t.W, H: t.H, ChartState: t.ChartState}
		if t.Chart != nil {
			o.ChartID, o.ChartName = t.Chart.ID, t.Chart.Name
			o.SourceID, o.SourceName = t.Chart.SourceID, t.Chart.SourceName
			if t.Chart.SourceState != chartSourceOK {
				o.ChartState = t.Chart.SourceState
			}
			if s, err := charts.Decode(t.Chart.Settings); err == nil {
				o.Settings = s
			}
			if o.SharedElsewhere, err = chartOnOtherDashboards(ctx, database, actor.WorkspaceID, t.Chart.ID, d.ID); err != nil {
				return nil, fmt.Errorf("count the chart's dashboards: %w", err)
			}
		}
		out.Tiles = append(out.Tiles, o)
	}
	return out, nil
}
