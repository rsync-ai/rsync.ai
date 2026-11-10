package handlers

// dashboard_proposals.go is plan_dashboard (dashboards-design.md § 7.3): Ask rsync.ai, or an
// agent over the MCP gateway, drafts a dashboard from the workspace's metadata; the server
// checks every chart as a save would (§ 3.2), running each one's query once, lays the tiles
// out, and keeps the draft for its person for 24 hours. A draft saves nothing: the person
// opens it in rsync.ai and saves it, and only then are its charts and dashboard made. A
// check that fails goes back to the model with the check's message, scrubbed, never to the
// person as a broken chart.

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
	"unicode"

	"api-gateway/internal/charts"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"
	"golang.org/x/text/unicode/norm"
)

const (
	dashboardProposalTTL = 24 * time.Hour
	// dashboardProposalsOpenMax caps a person's unsaved, unexpired drafts.
	dashboardProposalsOpenMax = 20
	// dashboardProposalChartsMax caps a draft's charts: each one's query runs once to
	// check it.
	dashboardProposalChartsMax = 12
)

// DashboardProposalChartInput is one chart of a draft: a name, a visible saved query or
// model, and its settings (§ 3).
type DashboardProposalChartInput struct {
	Name     string          `json:"name"`
	SourceID string          `json:"source_id"`
	Settings json.RawMessage `json:"settings"`
}

// DashboardProposalFilterInput is one filter of a draft; the server names it.
type DashboardProposalFilterInput struct {
	Label  string `json:"label"`
	Kind   string `json:"kind"`
	Column string `json:"column"`
}

// DashboardProposalInput is what Ask rsync.ai or an agent drafts.
type DashboardProposalInput struct {
	Name        string                         `json:"name"`
	Description string                         `json:"description"`
	Charts      []DashboardProposalChartInput  `json:"charts"`
	Filters     []DashboardProposalFilterInput `json:"filters"`
}

// DashboardProposalChart is one checked chart of a draft.
type DashboardProposalChart struct {
	Name       string          `json:"name"`
	SourceID   string          `json:"source_id"`
	SourceName string          `json:"source_name"`
	SourceKind string          `json:"source_kind"`
	Type       string          `json:"type"`
	Settings   json.RawMessage `json:"settings"`
}

// DashboardProposalTile places a draft's chart, by its index in Charts.
type DashboardProposalTile struct {
	Chart int `json:"chart"`
	X     int `json:"x"`
	Y     int `json:"y"`
	W     int `json:"w"`
	H     int `json:"h"`
}

// DashboardProposal is a checked draft.
type DashboardProposal struct {
	ID               string                   `json:"id"`
	Name             string                   `json:"name"`
	Description      string                   `json:"description"`
	Charts           []DashboardProposalChart `json:"charts"`
	Filters          []charts.Filter          `json:"filters"`
	Tiles            []DashboardProposalTile  `json:"tiles"`
	Status           string                   `json:"status"`
	Expired          bool                     `json:"expired"`
	SavedDashboardID string                   `json:"saved_dashboard_id,omitempty"`
	ProposedBy       string                   `json:"proposed_by"`
	CreatedAt        time.Time                `json:"created_at"`
	ExpiresAt        time.Time                `json:"expires_at"`
	URL              string                   `json:"url"`
}

// dashboardProposalBody is what the proposal column holds.
type dashboardProposalBody struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	Charts      []DashboardProposalChart `json:"charts"`
	Filters     []charts.Filter          `json:"filters"`
	Tiles       []DashboardProposalTile  `json:"tiles"`
}

// dashboardProposalURL is the page in rsync.ai where the person reviews and saves a draft.
func dashboardProposalURL(id string) string {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("APP_BASE_URL")), "/")
	if base == "" {
		base = "http://localhost:3000"
	}
	return base + "/dashboard-proposals/" + id
}

// dashboardForbidden says why the caller may not draft a dashboard.
func dashboardForbidden(actor services.Actor) *PlanRefusal {
	if !actor.Role.Meets(chartWriteRole) {
		return refusePlan("forbidden", http.StatusForbidden,
			"Only a workspace owner or admin can save dashboards, so only they can draft one. Ask an owner or admin.")
	}
	return refusePlan("forbidden", http.StatusForbidden,
		"This token lacks the %s scope. An owner or admin can create a token that has it.", services.ScopeDashboardsWrite)
}

// planFromChartRefusal turns a check's refusal into the message the model gets back,
// scrubbed: an engine's error can quote a value.
func planFromChartRefusal(r *chartRefusal, about string) *PlanRefusal {
	msg := r.Message
	if about != "" {
		msg = about + ": " + msg
	}
	return refusePlan(r.Code, r.Status, "%s", llmscrub.ScrubMax(msg, workflowErrorMax))
}

// filterSlug is a filter id made from its label: lower case, a-z 0-9 and dashes. An
// accent splits off its letter and is dropped, so "Région" is region, as the dashboard
// editor makes it (dashboard-layout.ts filterId).
func filterSlug(label string, taken map[string]bool, n int) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(strings.ToLower(label)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 28 {
			break
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = fmt.Sprintf("filter-%d", n)
	}
	base := id
	for i := 2; taken[id]; i++ {
		id = fmt.Sprintf("%s-%d", base, i)
	}
	taken[id] = true
	return id
}

// A drafted scorecard is 5 rows (#1426); in a row with a chart it is the chart's height.
const proposalScorecardRows = 5

// proposalSize is a drafted tile's size before its row shares out its spare columns.
func proposalSize(t string) (w, h int) {
	switch t {
	case charts.TypeScorecard:
		return 3, proposalScorecardRows
	case charts.TypeTable, charts.TypePivot:
		return charts.GridColumns, 8
	}
	return 6, 7
}

// proposalGroup orders a draft: scorecards, then the other charts, then tables and
// pivot tables.
func proposalGroup(t string) int {
	switch t {
	case charts.TypeScorecard:
		return 0
	case charts.TypeTable, charts.TypePivot:
		return 2
	}
	return 1
}

// layoutProposal places a draft's charts in one flow, left to right and row by row:
// scorecards, then the other charts, then tables, each group in the model's order. A
// tile that does not fit starts a new row. A row's spare columns go to its tiles evenly,
// the odd ones to the rightmost, so every row is the full width and the dashboard has no
// empty right side (#1426's draft had one beside its scorecards and its bar); a row's
// tiles are as tall as its tallest.
func layoutProposal(types []string) []DashboardProposalTile {
	order := make([]int, len(types))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return proposalGroup(types[a]) - proposalGroup(types[b]) })

	out := []DashboardProposalTile{}
	var row []DashboardProposalTile
	y, used := 0, 0
	flush := func() {
		if len(row) == 0 {
			return
		}
		spare, x, h := charts.GridColumns-used, 0, 0
		for _, t := range row {
			h = max(h, t.H)
		}
		for i := range row {
			row[i].W += spare / len(row)
			if i >= len(row)-spare%len(row) {
				row[i].W++
			}
			row[i].X, row[i].Y, row[i].H = x, y, h
			x += row[i].W
		}
		out = append(out, row...)
		row, used, y = nil, 0, y+h
	}
	for _, i := range order {
		w, h := proposalSize(types[i])
		if used+w > charts.GridColumns {
			flush()
		}
		row = append(row, DashboardProposalTile{Chart: i, W: w, H: h})
		used += w
	}
	flush()
	return out
}

// proposalTiles is a draft's layout as dashboard tiles, for the grid's checks.
func proposalTiles(ts []DashboardProposalTile, chartIDs []string) []charts.Tile {
	out := make([]charts.Tile, len(ts))
	for i, t := range ts {
		out[i] = charts.Tile{Kind: charts.TileChart, ChartID: chartIDs[t.Chart], X: t.X, Y: t.Y, W: t.W, H: t.H}
	}
	return out
}

// checkProposalCharts checks each chart as a save would, with userID's rights, and the
// filters against the charts' columns.
func checkProposalCharts(ctx context.Context, database *sql.DB, ws, userID, visibility string,
	cs []DashboardProposalChartInput, fs []charts.Filter) ([]*checkedChart, *chartRefusal, int, error) {
	checked := make([]*checkedChart, 0, len(cs))
	columns := map[string]bool{}
	for i, in := range cs {
		ch, refusal, err := checkChartForSave(ctx, database, ws, userID, chartInput{
			Name: in.Name, SourceID: in.SourceID, Settings: in.Settings, Visibility: visibility,
		})
		if err != nil {
			return nil, nil, i, err
		}
		if refusal != nil {
			return nil, refusal, i, nil
		}
		for _, col := range ch.SourceColumns {
			columns[col] = true
		}
		checked = append(checked, ch)
	}
	if err := charts.ValidateFilters(fs); err != nil {
		return nil, settingsRefusal(err), -1, nil
	}
	for _, f := range fs {
		if !columns[f.Column] {
			return nil, &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Rule: 2,
				Message: fmt.Sprintf("The filter %q is on the column %q, which no chart's source has.", f.Label, f.Column)}, -1, nil
		}
	}
	return checked, nil, -1, nil
}

// ProposeDashboard checks a dashboard Ask rsync.ai or an agent drafted and keeps it for
// actor's user to save. Each chart's query runs once, to check it; nothing is saved.
func ProposeDashboard(c *gin.Context, actor services.Actor, in DashboardProposalInput) (*DashboardProposal, error) {
	if actor.Channel != services.ChannelAssistant && actor.Channel != services.ChannelAgent {
		return nil, refusePlan("forbidden", http.StatusForbidden, "Only Ask rsync.ai or an agent token drafts a dashboard.")
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

	name := strings.Join(strings.Fields(in.Name), " ")
	desc := strings.TrimSpace(in.Description)
	if err := charts.ValidateDashboardText(name, desc); err != nil {
		return nil, planFromChartRefusal(settingsRefusal(err), "")
	}
	if len(in.Charts) == 0 {
		return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity, "A dashboard draft needs at least one chart.")
	}
	if len(in.Charts) > dashboardProposalChartsMax {
		return nil, refusePlan("invalid_settings", http.StatusUnprocessableEntity,
			"A dashboard draft has at most %d charts; this one has %d.", dashboardProposalChartsMax, len(in.Charts))
	}
	taken := map[string]bool{}
	filters := make([]charts.Filter, len(in.Filters))
	for i, f := range in.Filters {
		label := strings.TrimSpace(f.Label)
		filters[i] = charts.Filter{ID: filterSlug(label, taken, i+1), Kind: f.Kind, Label: label, Column: f.Column}
	}

	checked, refusal, at, err := checkProposalCharts(ctx, database, ws, userID, "private", in.Charts, filters)
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		about := ""
		if at >= 0 {
			about = fmt.Sprintf("Chart %d (%q)", at+1, strings.TrimSpace(in.Charts[at].Name))
		}
		return nil, planFromChartRefusal(refusal, about)
	}

	types := make([]string, len(checked))
	body := dashboardProposalBody{Name: name, Description: desc, Filters: filters,
		Charts: make([]DashboardProposalChart, len(checked))}
	placeholders := make([]string, len(checked))
	for i, ch := range checked {
		types[i] = ch.Settings.Type
		placeholders[i] = ch.Source.ID
		body.Charts[i] = DashboardProposalChart{
			Name: ch.Name, SourceID: ch.Source.ID, SourceName: ch.Source.Name, SourceKind: ch.Source.Kind(),
			Type: ch.Settings.Type, Settings: ch.SettingsJSON,
		}
	}
	body.Tiles = layoutProposal(types)
	if err := charts.ValidateTiles(proposalTiles(body.Tiles, placeholders)); err != nil {
		return nil, planFromChartRefusal(settingsRefusal(err), "")
	}

	var open int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dashboard_proposals
		WHERE workspace_id = $1 AND created_by = $2 AND status = 'proposed' AND expires_at > NOW()
	`, ws, userID).Scan(&open); err != nil {
		return nil, fmt.Errorf("count dashboard drafts: %w", err)
	}
	if open >= dashboardProposalsOpenMax {
		return nil, refusePlan("too_many_drafts", http.StatusTooManyRequests,
			"You have %d unsaved dashboard drafts. Save some, or let them expire, before drafting more.", open)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal dashboard draft: %w", err)
	}
	var tokenID *string
	if actor.Channel == services.ChannelAgent {
		tokenID = nullString(c.GetString(ctxTokenID))
	}
	p := &DashboardProposal{Name: body.Name, Description: body.Description, Charts: body.Charts,
		Filters: body.Filters, Tiles: body.Tiles, Status: planStatusProposed, ProposedBy: actorKindFor(actor.Channel)}
	if err := database.QueryRowContext(ctx, `
		INSERT INTO dashboard_proposals (workspace_id, created_by, actor_kind, token_id, proposal, expires_at)
		VALUES ($1, $2, $3, $4, $5, NOW() + ($6 * INTERVAL '1 second'))
		RETURNING id::text, created_at, expires_at
	`, ws, userID, p.ProposedBy, tokenID, string(raw), int(dashboardProposalTTL.Seconds()),
	).Scan(&p.ID, &p.CreatedAt, &p.ExpiresAt); err != nil {
		return nil, fmt.Errorf("store dashboard draft: %w", err)
	}
	p.URL = dashboardProposalURL(p.ID)

	logAudit(c, "plan_dashboard", "dashboard_proposal", p.ID, map[string]interface{}{
		"charts":  len(p.Charts),
		"types":   types,
		"filters": len(p.Filters),
		"channel": string(actor.Channel),
	})
	return p, nil
}

// loadDashboardProposal reads a draft of userID's in ws, or sql.ErrNoRows.
func loadDashboardProposal(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ws, userID, id string, forUpdate bool) (*DashboardProposal, error) {
	if !validUUID(id) {
		return nil, sql.ErrNoRows
	}
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	p := &DashboardProposal{}
	var raw []byte
	var saved sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id::text, actor_kind, proposal, status, saved_dashboard_id::text, created_at, expires_at,
		       expires_at <= NOW()
		FROM dashboard_proposals
		WHERE id = $1 AND workspace_id = $2 AND created_by = $3`+lock,
		id, ws, userID).Scan(&p.ID, &p.ProposedBy, &raw, &p.Status, &saved, &p.CreatedAt, &p.ExpiresAt, &p.Expired)
	if err != nil {
		return nil, err
	}
	var body dashboardProposalBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("read dashboard draft: %w", err)
	}
	p.Name, p.Description, p.Charts, p.Filters, p.Tiles = body.Name, body.Description, body.Charts, body.Filters, body.Tiles
	for i := range p.Charts {
		p.Charts[i].Settings = charts.AsVersion2(p.Charts[i].Settings)
	}
	if p.Filters == nil {
		p.Filters = []charts.Filter{}
	}
	p.SavedDashboardID = saved.String
	p.Expired = p.Expired && p.Status == planStatusProposed
	p.URL = dashboardProposalURL(p.ID)
	return p, nil
}

func writeDashboardProposalLoadError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "dashboard draft not found"})
		return
	}
	log.WithError(err).Error("[dashboard_proposals] load failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the dashboard draft"})
}

// GetDashboardProposal returns one of the caller's drafts.
// GET /api/v1/dashboard-proposals/:id
func GetDashboardProposal(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	p, err := loadDashboardProposal(c.Request.Context(), database, ws, userID, c.Param("id"), false)
	if err != nil {
		writeDashboardProposalLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"proposal": p, "can_manage": role.Meets(chartWriteRole)})
}

// saveDashboardProposalRequest is the person's choice when saving a draft.
type saveDashboardProposalRequest struct {
	Visibility string `json:"visibility"`
}

// SaveDashboardProposal saves a draft as the caller's charts and dashboard, after
// checking every chart again with the caller's rights.
// POST /api/v1/dashboard-proposals/:id/save
func SaveDashboardProposal(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	var in saveDashboardProposalRequest
	if !bindStrictJSON(c, &in) {
		return
	}
	if in.Visibility == "" {
		in.Visibility = "private"
	}
	if in.Visibility != "private" && in.Visibility != "workspace" {
		writeChartRefusal(c, refuseChart(http.StatusUnprocessableEntity, "invalid_visibility", "A dashboard is private or shared with the workspace."))
		return
	}
	ctx := c.Request.Context()
	p, err := loadDashboardProposal(ctx, database, ws, userID, c.Param("id"), false)
	if err != nil {
		writeDashboardProposalLoadError(c, err)
		return
	}
	if p.Status != planStatusProposed {
		c.JSON(http.StatusConflict, gin.H{"error": "This draft is already saved.", "code": "already_saved", "dashboard_id": p.SavedDashboardID})
		return
	}
	if p.Expired {
		c.JSON(http.StatusGone, gin.H{"error": "This draft expired. Ask rsync.ai to draft it again.", "code": "expired"})
		return
	}

	inputs := make([]DashboardProposalChartInput, len(p.Charts))
	for i, ch := range p.Charts {
		inputs[i] = DashboardProposalChartInput{Name: ch.Name, SourceID: ch.SourceID, Settings: ch.Settings}
	}
	checked, refusal, at, err := checkProposalCharts(ctx, database, ws, userID, in.Visibility, inputs, p.Filters)
	if err != nil {
		log.WithError(err).Error("[dashboard_proposals] check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the dashboard draft"})
		return
	}
	if refusal != nil {
		if at >= 0 {
			refusal.Message = fmt.Sprintf("Chart %d (%q): %s", at+1, p.Charts[at].Name, refusal.Message)
		}
		writeChartRefusal(c, refusal)
		return
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	// The draft is locked, so two saves of it cannot both make a dashboard.
	locked, err := loadDashboardProposal(ctx, tx, ws, userID, p.ID, true)
	if err != nil {
		writeDashboardProposalLoadError(c, err)
		return
	}
	if locked.Status != planStatusProposed || locked.Expired {
		c.JSON(http.StatusConflict, gin.H{"error": "This draft was saved or expired meanwhile.", "code": "already_saved", "dashboard_id": locked.SavedDashboardID})
		return
	}
	chartIDs := make([]string, len(checked))
	for i, ch := range checked {
		id, err := insertChart(ctx, tx, ws, userID, ch, p.ProposedBy)
		if err != nil {
			log.WithError(err).Error("[dashboard_proposals] chart insert failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
			return
		}
		chartIDs[i] = id
	}
	d := &checkedDashboard{Name: p.Name, Description: p.Description, Filters: p.Filters,
		Tiles: proposalTiles(p.Tiles, chartIDs), Visibility: in.Visibility}
	dashboardID, err := insertDashboard(ctx, tx, ws, userID, d, p.ProposedBy)
	if err == nil {
		err = insertDashboardTiles(ctx, tx, ws, dashboardID, d.Tiles)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `
			UPDATE dashboard_proposals SET status = 'saved', saved_dashboard_id = $2, saved_at = NOW()
			WHERE id = $1 AND status = 'proposed'
		`, p.ID, dashboardID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.WithError(err).Error("[dashboard_proposals] save failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	for i, id := range chartIDs {
		logAudit(c, "create_chart", "chart", id, gin.H{
			"name": checked[i].Name, "type": checked[i].Settings.Type, "visibility": in.Visibility,
			"proposal_id": p.ID, "proposed_by": p.ProposedBy,
		})
	}
	logAudit(c, "create_dashboard", "dashboard", dashboardID, gin.H{
		"name": d.Name, "tiles": len(d.Tiles), "filters": len(d.Filters), "visibility": in.Visibility,
		"proposal_id": p.ID, "proposed_by": p.ProposedBy,
	})
	c.JSON(http.StatusCreated, gin.H{"dashboard_id": dashboardID, "chart_ids": chartIDs})
}
