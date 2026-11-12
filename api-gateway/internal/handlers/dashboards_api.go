package handlers

// dashboards_api.go is the dashboards' API (dashboards-design.md § 4.2, § 4.3, § 7.2):
// a board of saved charts and text tiles on a 12-column grid, with up to four filters.
// Every member sees the workspace's shared dashboards and their own; an owner or admin
// saves them (§ 10). A tile is drawn live, with the viewer's rights, each time it is
// opened (§ 4.3), and a filter reaches a tile's query as a bound value (§ 4.2) only when
// the tile's source has the filter's column.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"api-gateway/internal/charts"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/shared/sqlexec/mask"
)

// filterValuesMax caps a value-list filter's choices.
const filterValuesMax = 200

// bindStrictJSON reads a request body, refusing keys it does not know (§ 3.2 rule 1).
func bindStrictJSON(c *gin.Context, v any) bool {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return false
	}
	dec := json.NewDecoder(&buf)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			writeChartRefusal(c, &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Rule: 1,
				Message: strings.TrimPrefix(err.Error(), "json: ")})
			return false
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// dashboardRecord is a dashboard as the API returns it.
type dashboardRecord struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Filters     []charts.Filter  `json:"filters"`
	Tiles       []*dashboardTile `json:"tiles,omitempty"`
	TileCount   int              `json:"tile_count"`
	Visibility  string           `json:"visibility"`
	ProposedBy  string           `json:"proposed_by"`
	Mine        bool             `json:"mine"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`

	createdBy string
}

// dashboardTile is one tile. A chart tile carries its chart when the viewer may see it;
// ChartState is "hidden" when the chart is private to someone else.
type dashboardTile struct {
	ID string `json:"id"`
	charts.Tile
	Chart      *chartRecord `json:"chart,omitempty"`
	ChartState string       `json:"chart_state,omitempty"`
}

// dashboardSelect reads dashboards userID may see in ws ($1, $2).
const dashboardSelect = `
	SELECT d.id::text, d.name, d.description, d.filters, d.visibility, d.proposed_by,
	       COALESCE(d.created_by::text, ''), d.created_at, d.updated_at,
	       (SELECT COUNT(*) FROM dashboard_tiles t WHERE t.dashboard_id = d.id)
	FROM dashboards d
	WHERE d.workspace_id = $1 AND (d.visibility = 'workspace' OR d.created_by = $2)`

func scanDashboard(scan func(...any) error, userID string) (*dashboardRecord, error) {
	d := &dashboardRecord{}
	var filters []byte
	if err := scan(&d.ID, &d.Name, &d.Description, &filters, &d.Visibility, &d.ProposedBy,
		&d.createdBy, &d.CreatedAt, &d.UpdatedAt, &d.TileCount); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(filters, &d.Filters); err != nil || d.Filters == nil {
		d.Filters = []charts.Filter{}
	}
	d.Mine = d.createdBy == userID
	return d, nil
}

// loadVisibleDashboard reads one dashboard userID may see, with its tiles and the charts
// on them userID may see, or sql.ErrNoRows.
func loadVisibleDashboard(ctx context.Context, database *sql.DB, ws, userID, id string) (*dashboardRecord, error) {
	if !validUUID(id) {
		return nil, sql.ErrNoRows
	}
	d, err := scanDashboard(database.QueryRowContext(ctx, dashboardSelect+` AND d.id = $3`, ws, userID, id).Scan, userID)
	if err != nil {
		return nil, err
	}
	visible := map[string]*chartRecord{}
	rows, err := database.QueryContext(ctx, chartSelect+`
		AND ch.id IN (SELECT t.chart_id FROM dashboard_tiles t WHERE t.dashboard_id = $3 AND t.chart_id IS NOT NULL)`,
		ws, userID, d.ID)
	if err != nil {
		return nil, fmt.Errorf("load the dashboard's charts: %w", err)
	}
	for rows.Next() {
		r, err := scanChart(rows.Scan, userID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		visible[r.ID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	trows, err := database.QueryContext(ctx, `
		SELECT id::text, kind, COALESCE(chart_id::text, ''), COALESCE(body, ''), x, y, w, h
		FROM dashboard_tiles
		WHERE dashboard_id = $1 AND workspace_id = $2
		ORDER BY y, x, id
	`, d.ID, ws)
	if err != nil {
		return nil, fmt.Errorf("load the dashboard's tiles: %w", err)
	}
	defer trows.Close()
	d.Tiles = []*dashboardTile{}
	for trows.Next() {
		t := &dashboardTile{}
		if err := trows.Scan(&t.ID, &t.Kind, &t.ChartID, &t.Body, &t.X, &t.Y, &t.W, &t.H); err != nil {
			return nil, err
		}
		if t.Kind == charts.TileChart {
			if r, ok := visible[t.ChartID]; ok {
				t.Chart, t.ChartState = r, chartSourceOK
			} else {
				t.ChartState = chartSourceHidden
			}
		}
		d.Tiles = append(d.Tiles, t)
	}
	return d, trows.Err()
}

func writeDashboardLoadError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "dashboard not found"})
		return
	}
	log.WithError(err).Error("[dashboards] load failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the dashboard"})
}

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

// dashboardInput is a whole dashboard as the editor saves it.
type dashboardInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Filters     []charts.Filter `json:"filters"`
	Tiles       []charts.Tile   `json:"tiles"`
	Visibility  string          `json:"visibility"`
	// UpdatedAt is the dashboard's updated_at when the editor loaded it (PUT only).
	UpdatedAt time.Time `json:"updated_at"`
	// RestoredFrom is the version the History dialog put back (PUT only); 0 for an edit.
	RestoredFrom int `json:"restored_from"`
}

// checkedDashboard is a dashboard that passed every check.
type checkedDashboard struct {
	Name        string
	Description string
	Filters     []charts.Filter
	Tiles       []charts.Tile
	Visibility  string
}

// checkDashboardForSave checks a dashboard with userID's rights: its text, filters and
// grid (§ 3.2), each chart tile's chart visible to userID in ws, a shared dashboard's
// charts shared too, and each filter's column in some chart's source (rule 2).
func checkDashboardForSave(ctx context.Context, database *sql.DB, ws, userID string, in dashboardInput) (*checkedDashboard, *chartRefusal, error) {
	out := &checkedDashboard{
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		Filters:     in.Filters,
		Tiles:       in.Tiles,
		Visibility:  in.Visibility,
	}
	if out.Filters == nil {
		out.Filters = []charts.Filter{}
	}
	if out.Tiles == nil {
		out.Tiles = []charts.Tile{}
	}
	if out.Visibility == "" {
		out.Visibility = "private"
	}
	if out.Visibility != "private" && out.Visibility != "workspace" {
		return nil, refuseChart(http.StatusUnprocessableEntity, "invalid_visibility", "A dashboard is private or shared with the workspace."), nil
	}
	if err := charts.ValidateDashboardText(out.Name, out.Description); err != nil {
		return nil, settingsRefusal(err), nil
	}
	if err := charts.ValidateFilters(out.Filters); err != nil {
		return nil, settingsRefusal(err), nil
	}
	if err := charts.ValidateTiles(out.Tiles); err != nil {
		return nil, settingsRefusal(err), nil
	}
	columns := map[string]bool{}
	for i, t := range out.Tiles {
		if t.Kind != charts.TileChart {
			continue
		}
		r, err := loadVisibleChart(ctx, database, ws, userID, t.ChartID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refuseChart(http.StatusUnprocessableEntity, "chart_not_found",
				"Tile %d's chart is not a chart you can see in this workspace.", i+1), nil
		}
		if err != nil {
			return nil, nil, err
		}
		if out.Visibility == "workspace" && r.Visibility != "workspace" {
			return nil, refuseChart(http.StatusUnprocessableEntity, "chart_private",
				"A dashboard shared with the workspace shows only shared charts; the chart %q on tile %d is private. Share it first.", r.Name, i+1), nil
		}
		for _, col := range r.SourceColumns {
			columns[col] = true
		}
	}
	for _, f := range out.Filters {
		if !columns[f.Column] {
			return nil, &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Rule: 2,
				Message: fmt.Sprintf("The filter %q is on the column %q, which no chart on this dashboard has.", f.Label, f.Column)}, nil
		}
	}
	return out, nil, nil
}

// insertDashboardTiles stores tiles; tx holds the dashboard's row.
func insertDashboardTiles(ctx context.Context, tx *sql.Tx, ws, dashboardID string, tiles []charts.Tile) error {
	for _, t := range tiles {
		var chartID, body any
		if t.Kind == charts.TileChart {
			chartID = t.ChartID
		} else {
			body = t.Body
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dashboard_tiles (dashboard_id, workspace_id, kind, chart_id, body, x, y, w, h)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, dashboardID, ws, t.Kind, chartID, body, t.X, t.Y, t.W, t.H); err != nil {
			return fmt.Errorf("store a tile: %w", err)
		}
	}
	return nil
}

// Ways a dashboard is saved, as its versions name them (§ 7.7).
const (
	dashboardSavedByEdit    = "edit"
	dashboardSavedByAsk     = "ask"
	dashboardSavedByRestore = "restore"
)

// insertDashboardVersion keeps d, the dashboard as it was before a save, as its next
// version, with who saved it, how, and the change or version the save came from
// (§ 7.7). tx has updated the dashboard's row, so two saves never take one number; a
// unique violation is a save that raced this one.
func insertDashboardVersion(ctx context.Context, tx *sql.Tx, ws, userID string, d *dashboardRecord, how, changeID string, restoredFrom int) error {
	tiles := make([]charts.Tile, len(d.Tiles))
	for i, t := range d.Tiles {
		tiles[i] = t.Tile
	}
	tilesJSON, err := json.Marshal(tiles)
	if err != nil {
		return err
	}
	filters, err := json.Marshal(d.Filters)
	if err != nil {
		return err
	}
	var change, restored any
	if changeID != "" {
		change = changeID
	}
	if restoredFrom > 0 {
		restored = restoredFrom
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dashboard_versions (dashboard_id, workspace_id, version, name, description, filters,
		                                visibility, tiles, saved_by, how, change_id, restored_from)
		SELECT $1, $2, COALESCE(MAX(version), 0) + 1, $3, $4, $5, $6, $7, $8, $9, $10, $11
		FROM dashboard_versions WHERE dashboard_id = $1
	`, d.ID, ws, d.Name, d.Description, string(filters), d.Visibility, string(tilesJSON), userID, how, change, restored); err != nil {
		return fmt.Errorf("keep the dashboard's version: %w", err)
	}
	return nil
}

// dashboardStale is the 409 a save gets when the dashboard was saved since it was read.
func dashboardStale(c *gin.Context) {
	c.JSON(http.StatusConflict, gin.H{"error": "This dashboard was changed since you opened it. Reload it and make your change again.", "code": "stale"})
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

// ListDashboards lists the dashboards the caller may see.
// GET /api/v1/dashboards
func ListDashboards(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	rows, err := database.QueryContext(c.Request.Context(),
		dashboardSelect+` ORDER BY d.updated_at DESC, d.id LIMIT $3`, ws, userID, chartListMax)
	if err != nil {
		if isUndefinedTable(err) {
			c.JSON(http.StatusOK, gin.H{"dashboards": []dashboardRecord{}, "can_manage": role.Meets(chartWriteRole)})
			return
		}
		log.WithError(err).Error("[dashboards] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dashboards"})
		return
	}
	defer rows.Close()
	out := []*dashboardRecord{}
	for rows.Next() {
		d, err := scanDashboard(rows.Scan, userID)
		if err != nil {
			log.WithError(err).Error("[dashboards] scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dashboards"})
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("[dashboards] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list dashboards"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"dashboards": out, "can_manage": role.Meets(chartWriteRole)})
}

// GetDashboard returns a dashboard with its tiles.
// GET /api/v1/dashboards/:id
func GetDashboard(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	d, err := loadVisibleDashboard(c.Request.Context(), database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"dashboard": d, "can_manage": role.Meets(chartWriteRole)})
}

// CreateDashboard saves a dashboard.
// POST /api/v1/dashboards
func CreateDashboard(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	var in dashboardInput
	if !bindStrictJSON(c, &in) {
		return
	}
	ctx := c.Request.Context()
	checked, refusal, err := checkDashboardForSave(ctx, database, ws, userID, in)
	if err != nil {
		log.WithError(err).Error("[dashboards] check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the dashboard"})
		return
	}
	if refusal != nil {
		writeChartRefusal(c, refusal)
		return
	}
	id, err := storeNewDashboard(ctx, database, ws, userID, checked, "none")
	if err != nil {
		log.WithError(err).Error("[dashboards] insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	logAudit(c, "create_dashboard", "dashboard", id, gin.H{
		"name": checked.Name, "tiles": len(checked.Tiles), "filters": len(checked.Filters), "visibility": checked.Visibility,
	})
	d, err := loadVisibleDashboard(ctx, database, ws, userID, id)
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"dashboard": d})
}

// storeNewDashboard stores a checked dashboard and its tiles in one transaction.
func storeNewDashboard(ctx context.Context, database *sql.DB, ws, userID string, d *checkedDashboard, proposedBy string) (string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertDashboard(ctx, tx, ws, userID, d, proposedBy)
	if err != nil {
		return "", err
	}
	if err := insertDashboardTiles(ctx, tx, ws, id, d.Tiles); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func insertDashboard(ctx context.Context, tx *sql.Tx, ws, userID string, d *checkedDashboard, proposedBy string) (string, error) {
	filters, err := json.Marshal(d.Filters)
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO dashboards (workspace_id, name, description, filters, visibility, proposed_by, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id::text
	`, ws, d.Name, d.Description, string(filters), d.Visibility, proposedBy, userID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store the dashboard: %w", err)
	}
	return id, nil
}

// UpdateDashboard saves a whole dashboard: its text, filters and every tile, which
// replace the tiles it had. The dashboard as it was is kept as a version (§ 7.7); a
// restore from the History dialog is this save with restored_from set.
// PUT /api/v1/dashboards/:id
func UpdateDashboard(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	var in dashboardInput
	if !bindStrictJSON(c, &in) {
		return
	}
	ctx := c.Request.Context()
	have, err := loadVisibleDashboard(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	if !in.UpdatedAt.IsZero() && !in.UpdatedAt.Equal(have.UpdatedAt) {
		dashboardStale(c)
		return
	}
	how := dashboardSavedByEdit
	if in.RestoredFrom != 0 {
		var found bool
		err := database.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM dashboard_versions WHERE dashboard_id = $1 AND workspace_id = $2 AND version = $3)
		`, have.ID, ws, in.RestoredFrom).Scan(&found)
		if err != nil {
			log.WithError(err).Error("[dashboards] version lookup failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
			return
		}
		if !found {
			writeChartRefusal(c, refuseChart(http.StatusUnprocessableEntity, "version_not_found",
				"This dashboard has no version %d to restore.", in.RestoredFrom))
			return
		}
		how = dashboardSavedByRestore
	}
	checked, refusal, err := checkDashboardForSave(ctx, database, ws, userID, in)
	if err != nil {
		log.WithError(err).Error("[dashboards] check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the dashboard"})
		return
	}
	if refusal != nil {
		writeChartRefusal(c, refusal)
		return
	}
	filters, err := json.Marshal(checked.Filters)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE dashboards
		SET name = $3, description = $4, filters = $5, visibility = $6, updated_at = now()
		WHERE id = $1 AND workspace_id = $2 AND updated_at = $7
	`, have.ID, ws, checked.Name, checked.Description, string(filters), checked.Visibility, have.UpdatedAt)
	if err != nil {
		log.WithError(err).Error("[dashboards] update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dashboardStale(c)
		return
	}
	if err := insertDashboardVersion(ctx, tx, ws, userID, have, how, "", in.RestoredFrom); err != nil {
		if isUniqueViolation(err) {
			dashboardStale(c)
			return
		}
		log.WithError(err).Error("[dashboards] version insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM dashboard_tiles WHERE dashboard_id = $1 AND workspace_id = $2`, have.ID, ws); err != nil {
		log.WithError(err).Error("[dashboards] tiles delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	if err := insertDashboardTiles(ctx, tx, ws, have.ID, checked.Tiles); err != nil {
		log.WithError(err).Error("[dashboards] tiles insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the dashboard"})
		return
	}
	audit := gin.H{"name": checked.Name, "tiles": len(checked.Tiles), "filters": len(checked.Filters)}
	if how == dashboardSavedByRestore {
		audit["restored_from"] = in.RestoredFrom
	}
	logAudit(c, "update_dashboard", "dashboard", have.ID, audit)
	if checked.Visibility != have.Visibility {
		logAudit(c, "share_dashboard", "dashboard", have.ID, gin.H{"from": have.Visibility, "to": checked.Visibility})
	}
	d, err := loadVisibleDashboard(ctx, database, ws, userID, have.ID)
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"dashboard": d})
}

// DeleteDashboard deletes a dashboard and its tiles; its charts stay.
// DELETE /api/v1/dashboards/:id
func DeleteDashboard(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	have, err := loadVisibleDashboard(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM dashboards WHERE id = $1 AND workspace_id = $2`, have.ID, ws); err != nil {
		log.WithError(err).Error("[dashboards] delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the dashboard"})
		return
	}
	logAudit(c, "delete_dashboard", "dashboard", have.ID, gin.H{"name": have.Name})
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ---------------------------------------------------------------------------
// Drawing a tile
// ---------------------------------------------------------------------------

// tileDrawRequest is the viewer's filter picks, by filter id.
type tileDrawRequest struct {
	Filters map[string]charts.FilterValue `json:"filters"`
}

// applicableFilters keeps the filters whose column the chart's source has; the rest
// are named in ignored, so the tile can say which filters do not apply to it.
func applicableFilters(bound []charts.Bound, sourceColumns []string) (apply []charts.Bound, ignored []string) {
	has := map[string]bool{}
	for _, c := range sourceColumns {
		has[c] = true
	}
	ignored = []string{}
	for _, b := range bound {
		if has[b.Column] {
			apply = append(apply, b)
		} else {
			ignored = append(ignored, b.ID)
		}
	}
	sort.Strings(ignored)
	return apply, ignored
}

// appliedFilters names the filters a tile's rows were cut by, and those it was drawn
// without: of those that apply to its source, each whose column its connection could
// not bind (the result's unfiltered columns, chartQuery.Unfiltered). Both by id, so a
// filter on the same column that was not picked is in neither.
func appliedFilters(apply []charts.Bound, unfiltered []string) (applied, without []string) {
	skip := map[string]bool{}
	for _, c := range unfiltered {
		skip[c] = true
	}
	applied, without = []string{}, []string{}
	for _, b := range apply {
		if skip[b.Column] {
			without = append(without, b.ID)
		} else {
			applied = append(applied, b.ID)
		}
	}
	sort.Strings(applied)
	sort.Strings(without)
	return applied, without
}

// DrawDashboardTile draws one chart tile live, with the viewer's rights and filters.
// POST /api/v1/dashboards/:id/tiles/:tileId/draw
func DrawDashboardTile(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	var in tileDrawRequest
	if !bindStrictJSON(c, &in) {
		return
	}
	ctx := c.Request.Context()
	d, err := loadVisibleDashboard(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	var tile *dashboardTile
	for _, t := range d.Tiles {
		if t.ID == c.Param("tileId") {
			tile = t
		}
	}
	if tile == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "tile not found"})
		return
	}
	if tile.Kind != charts.TileChart {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "a text tile is not drawn", "code": "not_a_chart"})
		return
	}
	if tile.Chart == nil {
		c.JSON(http.StatusOK, gin.H{"chart": nil, "result": nil, "badges": nil,
			"break":   &chartBreak{Reason: "chart_hidden", Message: "This tile's chart is private to its author, so it is not shown to you."},
			"filters": gin.H{"applied": []string{}, "ignored": []string{}, "unfiltered": []string{}}})
		return
	}
	bound, err := charts.BindFilters(d.Filters, in.Filters)
	if err != nil {
		writeChartRefusal(c, settingsRefusal(err))
		return
	}
	apply, ignored := applicableFilters(bound, tile.Chart.SourceColumns)
	draw := drawSavedChart(ctx, database, ws, userID, tile.Chart, apply, time.Now())
	var unfiltered []string
	if draw.Result != nil {
		unfiltered = draw.Result.Unfiltered
	}
	applied, without := appliedFilters(apply, unfiltered)
	c.JSON(http.StatusOK, gin.H{
		"chart": tile.Chart, "result": draw.Result, "break": draw.Break, "badges": draw.Badges, "error": draw.Error,
		"filters": gin.H{"applied": applied, "ignored": ignored, "unfiltered": without},
	})
}

// GetDashboardFilterValues lists a value-list filter's choices: the distinct values of
// its column in the first tile's source that has it, read with the viewer's rights. A
// column the Explorer masks lists nothing.
// GET /api/v1/dashboards/:id/filters/:filterId/values
func GetDashboardFilterValues(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	d, err := loadVisibleDashboard(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeDashboardLoadError(c, err)
		return
	}
	var filter *charts.Filter
	for i := range d.Filters {
		if d.Filters[i].ID == c.Param("filterId") {
			filter = &d.Filters[i]
		}
	}
	if filter == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "filter not found"})
		return
	}
	if filter.Kind != charts.FilterValues {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "a date filter has no list of values", "code": "not_a_value_filter"})
		return
	}
	if mask.ShouldRedactColumnName(filter.Column) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": fmt.Sprintf("The column %q is masked, so its values are not listed. Type the values instead.", filter.Column), "code": "masked"})
		return
	}
	for _, t := range d.Tiles {
		if t.Chart == nil || t.Chart.SourceState != chartSourceOK || !slices.Contains(t.Chart.SourceColumns, filter.Column) {
			continue
		}
		src, err := loadChartSource(ctx, database, ws, userID, t.Chart.SourceID)
		if err != nil {
			continue
		}
		if chartSourceBreak(src) != nil {
			continue
		}
		from, err := chartSourceFor(src)
		if err != nil {
			continue
		}
		q := &chartQuery{SQL: writeFilterValuesSQL(from, filter.Column, filterValuesMax)}
		res, err := runChartSQL(ctx, src, q, filterValuesMax)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"values": []string{}, "truncated": false, "error": err.Error()})
			return
		}
		values := make([]string, 0, len(res.Rows))
		for _, row := range res.Rows {
			if len(res.Columns) == 0 {
				break
			}
			if v := row[res.Columns[0]]; v != nil {
				values = append(values, fmt.Sprint(v))
			}
		}
		c.JSON(http.StatusOK, gin.H{"values": values, "truncated": res.Truncated})
		return
	}
	c.JSON(http.StatusOK, gin.H{"values": []string{}, "truncated": false,
		"error": "No chart you can see on this dashboard has this column."})
}
