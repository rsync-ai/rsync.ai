package handlers

// charts_api.go is the chart page's API (dashboards-design.md § 3, § 7.2): the sources a
// person may chart, a source's columns, a preview, saved charts and drawing one. Every
// member sees the workspace's shared charts and their own; an owner or admin saves them
// (§ 10). A chart is drawn with the viewer's rights (open question 3, answered): the source
// is loaded only when the viewer may see it, and its query runs read-only, masked as in
// the Explorer (chart_run.go). Every save runs the § 3.2 checks, the source's query and the
// chart's own included (checkChartForSave).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/charts"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/shared/sqlexec/mask"
)

// chartWriteRole saves, changes and deletes charts and dashboards (§ 10).
const chartWriteRole = security.WSAdmin

// chartListMax caps a list of charts or sources.
const chartListMax = 500

// chartRefusal is a save or a preview the server refuses, with the § 3.2 rule it breaks
// when it is one.
type chartRefusal struct {
	Status  int
	Code    string
	Rule    int
	Message string
}

func (r *chartRefusal) Error() string { return r.Message }

func refuseChart(status int, code, format string, a ...any) *chartRefusal {
	return &chartRefusal{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// settingsRefusal turns a charts.Error into a 422 that names its rule.
func settingsRefusal(err error) *chartRefusal {
	if ce, ok := charts.AsError(err); ok {
		return &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Rule: ce.Rule, Message: ce.Message}
	}
	return &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Message: err.Error()}
}

func writeChartRefusal(c *gin.Context, r *chartRefusal) {
	body := gin.H{"error": r.Message, "code": r.Code}
	if r.Rule > 0 {
		body["rule"] = r.Rule
	}
	c.JSON(r.Status, body)
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// ChartSourceItem is a model or saved query in the chart page's picker.
type ChartSourceItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	Visibility     string `json:"visibility"`
	Mine           bool   `json:"mine"`
	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name"`
	ConnectorType  string `json:"connector_type"`
	Built          bool   `json:"built"`
	// Chartable is false for a model never built and for a source no chart can read;
	// Reason says why, and the picker shows it rather than hide the source.
	Chartable bool   `json:"chartable"`
	Reason    string `json:"reason,omitempty"`
}

// listChartSources lists the models and saved queries userID may see in ws.
func listChartSources(ctx context.Context, database *sql.DB, ws, userID string) ([]ChartSourceItem, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT sq.id::text, COALESCE(sq.name, ''), sq.sql_text, sq.materialization,
		       COALESCE(sq.target_table, ''), sq.visibility, COALESCE(sq.created_by::text, ''),
		       COALESCE(sq.last_run_status, ''),
		       EXISTS (SELECT 1 FROM saved_query_runs r
		               WHERE r.saved_query_id = sq.id AND r.status = 'succeeded'),
		       c.id::text, c.name, c.connector_type
		FROM saved_queries sq
		JOIN connections c ON c.id = sq.connection_id AND c.workspace_id = sq.workspace_id
		WHERE sq.workspace_id = $1 AND (sq.visibility = 'workspace' OR sq.created_by = $2)
		ORDER BY lower(sq.name), sq.id
		LIMIT $3
	`, ws, userID, chartListMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChartSourceItem{}
	for rows.Next() {
		var s chartSourceRow
		if err := rows.Scan(&s.ID, &s.Name, &s.SQLText, &s.Materialization, &s.TargetTable, &s.Visibility,
			&s.CreatedBy, &s.LastRunStatus, &s.Built, &s.ConnectionID, &s.ConnectionName, &s.ConnectorType); err != nil {
			return nil, err
		}
		item := ChartSourceItem{
			ID: s.ID, Name: s.Name, Kind: s.Kind(), Visibility: s.Visibility, Mine: s.CreatedBy == userID,
			ConnectionID: s.ConnectionID, ConnectionName: s.ConnectionName, ConnectorType: s.ConnectorType,
			Built: s.Built, Chartable: true,
		}
		if brk := chartSourceBreak(&s); brk != nil {
			item.Chartable, item.Reason = false, brk.Message
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// chartRequest resolves the caller and the workspace and checks the role.
func chartRequest(c *gin.Context, min security.WorkspaceRole) (userID, ws string, role security.WorkspaceRole,
	database *sql.DB, ok bool) {
	return workflowRequest(c, min)
}

// ListChartSources lists what the caller may chart.
// GET /api/v1/charts/sources
func ListChartSources(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	list, err := listChartSources(c.Request.Context(), database, ws, userID)
	if err != nil {
		log.WithError(err).Error("[charts] list sources failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list the sources"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sources": list})
}

// ChartSourceColumn is one column as the chart page lists it.
type ChartSourceColumn struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Masked bool   `json:"masked"`
}

// GetChartSourceColumns runs the source once (one row) and lists its columns by kind.
// GET /api/v1/charts/sources/:id/columns
func GetChartSourceColumns(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	s, err := loadChartSourceByParam(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeChartSourceLoadError(c, err)
		return
	}
	if brk := chartSourceBreak(s); brk != nil {
		writeChartRefusal(c, refuseChart(http.StatusUnprocessableEntity, brk.Reason, "%s", brk.Message))
		return
	}
	cols, err := probeChartSource(ctx, s)
	if err != nil {
		writeChartRefusal(c, refuseChart(http.StatusUnprocessableEntity, "source_failed", "The source did not run: %s", err.Error()))
		return
	}
	out := make([]ChartSourceColumn, len(cols))
	for i, col := range cols {
		out[i] = ChartSourceColumn{Name: col.Name, Kind: col.Kind, Masked: mask.ShouldRedactColumnName(col.Name)}
	}
	c.JSON(http.StatusOK, gin.H{"columns": out})
}

// loadChartSourceByParam is loadChartSource for an id from a URL or a body: an id that
// is not a UUID is not found, like one from another workspace.
func loadChartSourceByParam(ctx context.Context, database *sql.DB, ws, userID, id string) (*chartSourceRow, error) {
	if !validUUID(id) {
		return nil, errChartSourceNotFound
	}
	return loadChartSource(ctx, database, ws, userID, id)
}

func writeChartSourceLoadError(c *gin.Context, err error) {
	if errors.Is(err, errChartSourceNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "source not found"})
		return
	}
	log.WithError(err).Error("[charts] load source failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the source"})
}

// ChartSourcesForModelMax caps list_chart_sources: the first sources by name.
const ChartSourcesForModelMax = 200

// ListChartSourcesAs is ListChartSources for list_chart_sources: the sources actor may
// see, at most ChartSourcesForModelMax+1 so the tool can say it cut the list.
func ListChartSourcesAs(c *gin.Context, actor services.Actor) ([]ChartSourceItem, error) {
	if err := services.Authorize(actor, services.ActionChartRead); err != nil {
		return nil, refusePlan("forbidden", http.StatusForbidden,
			"This token lacks the %s scope. Create a token that has it.", services.ScopeMetadataRead)
	}
	database := db.GetDB()
	if database == nil {
		return nil, fmt.Errorf("database not connected")
	}
	list, err := listChartSources(c.Request.Context(), database, actor.WorkspaceID, actor.UserID)
	if err != nil {
		if isUndefinedTable(err) {
			return []ChartSourceItem{}, nil
		}
		return nil, fmt.Errorf("list chart sources: %w", err)
	}
	if len(list) > ChartSourcesForModelMax+1 {
		list = list[:ChartSourcesForModelMax+1]
	}
	return list, nil
}

// DescribeChartSourceAs is GetChartSourceColumns for describe_chart_source: it runs the
// source once, one row, and returns its column names and kinds, never the row. An
// engine's error is scrubbed before it reaches the model.
func DescribeChartSourceAs(c *gin.Context, actor services.Actor, id string) (*ChartSourceItem, []ChartSourceColumn, error) {
	if err := services.Authorize(actor, services.ActionChartRead); err != nil {
		return nil, nil, refusePlan("forbidden", http.StatusForbidden,
			"This token lacks the %s scope. Create a token that has it.", services.ScopeMetadataRead)
	}
	database := db.GetDB()
	if database == nil {
		return nil, nil, fmt.Errorf("database not connected")
	}
	ctx := c.Request.Context()
	s, err := loadChartSourceByParam(ctx, database, actor.WorkspaceID, actor.UserID, id)
	if errors.Is(err, errChartSourceNotFound) {
		return nil, nil, refusePlan("not_found", http.StatusNotFound,
			"No model or saved query with that id is visible to you; list_chart_sources lists them.")
	}
	if err != nil {
		return nil, nil, err
	}
	item := &ChartSourceItem{ID: s.ID, Name: s.Name, Kind: s.Kind(), Visibility: s.Visibility,
		Mine: s.CreatedBy == actor.UserID, ConnectionID: s.ConnectionID, ConnectionName: s.ConnectionName,
		ConnectorType: s.ConnectorType, Built: s.Built, Chartable: true}
	if brk := chartSourceBreak(s); brk != nil {
		return nil, nil, refusePlan(brk.Reason, http.StatusUnprocessableEntity, "%s", brk.Message)
	}
	cols, err := probeChartSource(ctx, s)
	if err != nil {
		return nil, nil, refusePlan("source_failed", http.StatusUnprocessableEntity,
			"The source did not run: %s", llmscrub.ScrubMax(err.Error(), workflowErrorMax))
	}
	out := make([]ChartSourceColumn, len(cols))
	for i, col := range cols {
		out[i] = ChartSourceColumn{Name: col.Name, Kind: col.Kind, Masked: mask.ShouldRedactColumnName(col.Name)}
	}
	return item, out, nil
}

// ---------------------------------------------------------------------------
// The § 3.2 checks
// ---------------------------------------------------------------------------

// chartInput is a chart as the chart page or a proposal saves it.
type chartInput struct {
	Name       string          `json:"name"`
	SourceID   string          `json:"source_id"`
	Settings   json.RawMessage `json:"settings"`
	Visibility string          `json:"visibility"`
}

// checkedChart is a chart that passed every check, ready to store.
type checkedChart struct {
	Name          string
	Visibility    string
	Settings      *charts.Settings
	SettingsJSON  []byte
	SourceColumns []string
	Source        *chartSourceRow
}

// checkChartForSave runs the § 3.2 checks on a chart, with userID's rights on its source:
// the settings decode with unknown keys refused (rules 1, 3, 4, 5), the source runs once
// and has every column the settings name (rule 2), and the chart's own SQL runs once, so
// a sum over a text column is refused here rather than drawn as an error.
func checkChartForSave(ctx context.Context, database *sql.DB, ws, userID string, in chartInput) (*checkedChart, *chartRefusal, error) {
	name := strings.TrimSpace(in.Name)
	if err := charts.ValidateChartName(name); err != nil {
		return nil, settingsRefusal(err), nil
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "private"
	}
	if visibility != "private" && visibility != "workspace" {
		return nil, refuseChart(http.StatusUnprocessableEntity, "invalid_visibility", "A chart is private or shared with the workspace."), nil
	}
	if len(in.Settings) == 0 {
		return nil, &chartRefusal{Status: http.StatusUnprocessableEntity, Code: "invalid_settings", Rule: 1, Message: "A chart needs its settings."}, nil
	}
	settings, err := charts.Decode(in.Settings)
	if err != nil {
		return nil, settingsRefusal(err), nil
	}
	src, err := loadChartSourceByParam(ctx, database, ws, userID, in.SourceID)
	if errors.Is(err, errChartSourceNotFound) {
		return nil, refuseChart(http.StatusUnprocessableEntity, "source_not_found",
			"The chart's source is not a model or saved query you can see in this workspace."), nil
	}
	if err != nil {
		return nil, nil, err
	}
	if brk := chartSourceBreak(src); brk != nil {
		return nil, refuseChart(http.StatusUnprocessableEntity, brk.Reason, "%s", brk.Message), nil
	}
	if visibility == "workspace" && src.Visibility != "workspace" {
		return nil, refuseChart(http.StatusUnprocessableEntity, "source_private",
			"A chart shared with the workspace needs a source shared with the workspace; %q is private to its author.", src.Name), nil
	}
	cols, err := probeChartSource(ctx, src)
	if err != nil {
		return nil, refuseChart(http.StatusUnprocessableEntity, "source_failed", "The source did not run: %s", err.Error()), nil
	}
	names := chartColumnNames(cols)
	if err := settings.CheckColumns(names); err != nil {
		return nil, settingsRefusal(err), nil
	}
	kinds := chartColumnKinds(cols)
	if err := settings.CheckKinds(kinds, false); err != nil {
		return nil, settingsRefusal(err), nil
	}
	if r := runChartOnce(ctx, src, settings, kinds); r != nil {
		return nil, r, nil
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal the settings: %w", err)
	}
	return &checkedChart{
		Name: name, Visibility: visibility, Settings: settings, SettingsJSON: raw,
		SourceColumns: names, Source: src,
	}, nil, nil
}

// runChartOnce runs the chart's SQL for one row, unfiltered. A statement the engine
// refuses over a column of a kind the chart cannot use (kinds, the source probe's) is a
// refused setting that says so, not the engine's own error.
func runChartOnce(ctx context.Context, src *chartSourceRow, settings *charts.Settings, kinds map[string]string) *chartRefusal {
	from, err := chartSourceFor(src)
	if err != nil {
		return refuseChart(http.StatusUnprocessableEntity, "source_invalid", "%s", err.Error())
	}
	q, err := writeChartSQL(settings, from, nil, time.Now())
	if err != nil {
		return refuseChart(http.StatusUnprocessableEntity, "source_invalid", "%s", err.Error())
	}
	res, err := runChartQuery(ctx, src, q, 1)
	if err != nil {
		if kerr := chartKindsRefusal(settings, kinds, err); kerr != nil {
			return settingsRefusal(kerr)
		}
		return refuseChart(http.StatusUnprocessableEntity, "chart_failed", "The chart's query did not run: %s", err.Error())
	}
	if _, err := shapeChartResult(settings, res); err != nil {
		return refuseChart(http.StatusUnprocessableEntity, "chart_failed", "The chart's query did not return its columns: %s", err.Error())
	}
	for _, st := range q.statements() {
		res, err := runChartSQL(ctx, src, st, 1)
		if err != nil {
			return refuseChart(http.StatusUnprocessableEntity, "chart_failed", "The chart's sparkline query did not run: %s", err.Error())
		}
		if _, err := shapeSpark(settings, res); err != nil {
			return refuseChart(http.StatusUnprocessableEntity, "chart_failed", "The chart's sparkline query did not return its columns: %s", err.Error())
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Saved charts
// ---------------------------------------------------------------------------

// chartRecord is a saved chart as the API returns it. Its source's name and kind are
// shown only when the caller may see the source; SourceState says why when not.
type chartRecord struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	SourceID        string          `json:"source_id,omitempty"`
	SourceName      string          `json:"source_name,omitempty"`
	SourceKind      string          `json:"source_kind,omitempty"`
	SourceState     string          `json:"source_state"`
	Settings        json.RawMessage `json:"settings"`
	SettingsVersion int             `json:"settings_version"`
	SourceColumns   []string        `json:"source_columns"`
	Visibility      string          `json:"visibility"`
	ProposedBy      string          `json:"proposed_by"`
	Mine            bool            `json:"mine"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`

	savedQueryID string
	createdBy    string
}

// Source states.
const (
	chartSourceOK      = "ok"
	chartSourceHidden  = "hidden"
	chartSourceDeleted = "deleted"
)

// chartSelect reads charts userID may see in ws ($1, $2), with their source when userID
// may see it too.
const chartSelect = `
	SELECT ch.id::text, ch.name, COALESCE(ch.saved_query_id::text, ''), (sq.id IS NOT NULL),
	       COALESCE(sq.name, ''), COALESCE(sq.materialization, ''),
	       ch.settings, ch.settings_version, ch.source_columns, ch.visibility, ch.proposed_by,
	       COALESCE(ch.created_by::text, ''), ch.created_at, ch.updated_at
	FROM charts ch
	LEFT JOIN saved_queries sq
	       ON sq.id = ch.saved_query_id AND sq.workspace_id = ch.workspace_id
	      AND (sq.visibility = 'workspace' OR sq.created_by = $2)
	WHERE ch.workspace_id = $1 AND (ch.visibility = 'workspace' OR ch.created_by = $2)`

func scanChart(scan func(...any) error, userID string) (*chartRecord, error) {
	r := &chartRecord{}
	var visible bool
	var mat string
	var settings, cols []byte
	if err := scan(&r.ID, &r.Name, &r.savedQueryID, &visible, &r.SourceName, &mat,
		&settings, &r.SettingsVersion, &cols, &r.Visibility, &r.ProposedBy,
		&r.createdBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Settings = charts.AsVersion2(settings)
	if err := json.Unmarshal(cols, &r.SourceColumns); err != nil || r.SourceColumns == nil {
		r.SourceColumns = []string{}
	}
	switch {
	case r.savedQueryID == "":
		r.SourceState = chartSourceDeleted
	case !visible:
		r.SourceState = chartSourceHidden
	default:
		r.SourceState = chartSourceOK
		r.SourceID = r.savedQueryID
		r.SourceKind = (&chartSourceRow{Materialization: mat}).Kind()
	}
	if r.SourceState != chartSourceOK {
		r.SourceName = ""
	}
	r.Mine = r.createdBy == userID
	return r, nil
}

// loadVisibleChart reads one chart userID may see in ws, or sql.ErrNoRows.
func loadVisibleChart(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ws, userID, id string) (*chartRecord, error) {
	if !validUUID(id) {
		return nil, sql.ErrNoRows
	}
	return scanChart(q.QueryRowContext(ctx, chartSelect+` AND ch.id = $3`, ws, userID, id).Scan, userID)
}

func writeChartLoadError(c *gin.Context, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "chart not found"})
		return
	}
	log.WithError(err).Error("[charts] load failed")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the chart"})
}

// ListCharts lists the charts the caller may see.
// GET /api/v1/charts
func ListCharts(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	rows, err := database.QueryContext(c.Request.Context(),
		chartSelect+` ORDER BY ch.updated_at DESC, ch.id LIMIT $3`, ws, userID, chartListMax)
	if err != nil {
		if isUndefinedTable(err) {
			c.JSON(http.StatusOK, gin.H{"charts": []chartRecord{}, "can_manage": role.Meets(chartWriteRole)})
			return
		}
		log.WithError(err).Error("[charts] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list charts"})
		return
	}
	defer rows.Close()
	out := []*chartRecord{}
	for rows.Next() {
		r, err := scanChart(rows.Scan, userID)
		if err != nil {
			log.WithError(err).Error("[charts] scan failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list charts"})
			return
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("[charts] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list charts"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"charts": out, "can_manage": role.Meets(chartWriteRole)})
}

// GetChart returns one chart.
// GET /api/v1/charts/:id
func GetChart(c *gin.Context) {
	userID, ws, role, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	r, err := loadVisibleChart(c.Request.Context(), database, ws, userID, c.Param("id"))
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"chart": r, "can_manage": role.Meets(chartWriteRole)})
}

// CreateChart saves a chart after the § 3.2 checks.
// POST /api/v1/charts
func CreateChart(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	var in chartInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx := c.Request.Context()
	checked, refusal, err := checkChartForSave(ctx, database, ws, userID, in)
	if err != nil {
		log.WithError(err).Error("[charts] check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the chart"})
		return
	}
	if refusal != nil {
		writeChartRefusal(c, refusal)
		return
	}
	id, err := insertChart(ctx, database, ws, userID, checked, "none")
	if err != nil {
		log.WithError(err).Error("[charts] insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the chart"})
		return
	}
	logAudit(c, "create_chart", "chart", id, gin.H{
		"name": checked.Name, "type": checked.Settings.Type, "source_id": checked.Source.ID,
		"visibility": checked.Visibility,
	})
	r, err := loadVisibleChart(ctx, database, ws, userID, id)
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"chart": r})
}

// insertChart stores a checked chart; q is the database or a transaction.
func insertChart(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ws, userID string, ch *checkedChart, proposedBy string) (string, error) {
	cols, err := json.Marshal(ch.SourceColumns)
	if err != nil {
		return "", err
	}
	var id string
	err = q.QueryRowContext(ctx, `
		INSERT INTO charts (workspace_id, name, saved_query_id, settings, settings_version,
		                    source_columns, visibility, proposed_by, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id::text
	`, ws, ch.Name, ch.Source.ID, string(ch.SettingsJSON), charts.Version, string(cols),
		ch.Visibility, proposedBy, userID).Scan(&id)
	return id, err
}

// chartUpdateRequest is a whole chart as the chart page saves it.
type chartUpdateRequest struct {
	chartInput
	// UpdatedAt is the chart's updated_at when the page loaded it. A chart changed since
	// is not overwritten.
	UpdatedAt time.Time `json:"updated_at"`
}

// UpdateChart saves a change to a chart, checked as a new one is. A chart on a dashboard
// shared with the workspace stays shared: making it private is refused, naming them.
// PUT /api/v1/charts/:id
func UpdateChart(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	var in chartUpdateRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx := c.Request.Context()
	have, err := loadVisibleChart(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	if !in.UpdatedAt.IsZero() && !in.UpdatedAt.Equal(have.UpdatedAt) {
		c.JSON(http.StatusConflict, gin.H{"error": "This chart was changed since you opened it. Reload it and make your change again.", "code": "stale"})
		return
	}
	checked, refusal, err := checkChartForSave(ctx, database, ws, userID, in.chartInput)
	if err != nil {
		log.WithError(err).Error("[charts] check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check the chart"})
		return
	}
	if refusal != nil {
		writeChartRefusal(c, refusal)
		return
	}
	if checked.Visibility == "private" && have.Visibility == "workspace" {
		names, err := sharedDashboardsWithChart(ctx, database, ws, have.ID)
		if err != nil {
			log.WithError(err).Error("[charts] dashboards lookup failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the chart"})
			return
		}
		if len(names) > 0 {
			c.JSON(http.StatusConflict, gin.H{
				"error":      fmt.Sprintf("This chart is on dashboards shared with the workspace (%s). Remove it from them before making it private.", strings.Join(quoteEach(names), ", ")),
				"code":       "on_shared_dashboards",
				"dashboards": names,
			})
			return
		}
	}
	cols, err := json.Marshal(checked.SourceColumns)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the chart"})
		return
	}
	res, err := database.ExecContext(ctx, `
		UPDATE charts
		SET name = $3, saved_query_id = $4, settings = $5, settings_version = $6, source_columns = $7,
		    visibility = $8, updated_at = now()
		WHERE id = $1 AND workspace_id = $2 AND updated_at = $9
	`, have.ID, ws, checked.Name, checked.Source.ID, string(checked.SettingsJSON), charts.Version, string(cols),
		checked.Visibility, have.UpdatedAt)
	if err != nil {
		log.WithError(err).Error("[charts] update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the chart"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "This chart was changed since you opened it. Reload it and make your change again.", "code": "stale"})
		return
	}
	logAudit(c, "update_chart", "chart", have.ID, gin.H{
		"name": checked.Name, "type": checked.Settings.Type, "source_id": checked.Source.ID,
	})
	if checked.Visibility != have.Visibility {
		logAudit(c, "share_chart", "chart", have.ID, gin.H{"from": have.Visibility, "to": checked.Visibility})
	}
	r, err := loadVisibleChart(ctx, database, ws, userID, have.ID)
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"chart": r})
}

// sharedDashboardsWithChart names the workspace-shared dashboards that hold a chart.
func sharedDashboardsWithChart(ctx context.Context, database *sql.DB, ws, chartID string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT DISTINCT d.name
		FROM dashboard_tiles t
		JOIN dashboards d ON d.id = t.dashboard_id AND d.workspace_id = t.workspace_id
		WHERE t.chart_id = $1 AND t.workspace_id = $2 AND d.visibility = 'workspace'
		ORDER BY d.name
		LIMIT 20
	`, chartID, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// DeleteChart deletes a chart that is on no dashboard. One that is, is refused with the
// dashboards the caller can see and a count of the others.
// DELETE /api/v1/charts/:id
func DeleteChart(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, chartWriteRole)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	have, err := loadVisibleChart(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	var names []string
	var others int
	rows, err := database.QueryContext(ctx, `
		SELECT d.name, (d.visibility = 'workspace' OR d.created_by = $3)
		FROM dashboards d
		WHERE d.workspace_id = $2
		  AND EXISTS (SELECT 1 FROM dashboard_tiles t WHERE t.dashboard_id = d.id AND t.chart_id = $1)
		ORDER BY d.name
	`, have.ID, ws, userID)
	if err != nil {
		log.WithError(err).Error("[charts] dashboards lookup failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the chart"})
		return
	}
	for rows.Next() {
		var n string
		var visible bool
		if err := rows.Scan(&n, &visible); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the chart"})
			return
		}
		if visible {
			names = append(names, n)
		} else {
			others++
		}
	}
	rows.Close()
	if len(names) > 0 || others > 0 {
		msg := "This chart is on "
		switch {
		case len(names) > 0 && others > 0:
			msg += fmt.Sprintf("the dashboards %s and %s private to other people", strings.Join(quoteEach(names), ", "), plural(others, "dashboard"))
		case len(names) > 0:
			msg += "the dashboards " + strings.Join(quoteEach(names), ", ")
		default:
			msg += plural(others, "dashboard") + " private to other people"
		}
		c.JSON(http.StatusConflict, gin.H{"error": msg + ". Remove it from them before deleting it.", "code": "on_dashboards",
			"dashboards": names, "other_dashboards": others})
		return
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM charts WHERE id = $1 AND workspace_id = $2`, have.ID, ws); err != nil {
		log.WithError(err).Error("[charts] delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the chart"})
		return
	}
	logAudit(c, "delete_chart", "chart", have.ID, gin.H{"name": have.Name})
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// chartDraw is one drawn chart: its rows, or the break that replaces it, its badges, and
// the query's error when it failed. A warning never hides rows (§ 5).
type chartDraw struct {
	Result *chartResult `json:"result"`
	Break  *chartBreak  `json:"break"`
	Badges *chartBadges `json:"badges"`
	Error  string       `json:"error,omitempty"`
}

// drawSavedChart draws a saved chart with the viewer's rights: its source is loaded only
// when userID may see it.
func drawSavedChart(ctx context.Context, database *sql.DB, ws, userID string, r *chartRecord, filters []charts.Bound, now time.Time) chartDraw {
	out := chartDraw{}
	switch r.SourceState {
	case chartSourceDeleted:
		out.Break = &chartBreak{Reason: "source_deleted", Message: "This chart's source was deleted."}
		return out
	case chartSourceHidden:
		out.Break = &chartBreak{Reason: "source_hidden", Message: "This chart's source is private to its author, so its rows are not shown to you."}
		return out
	}
	settings, err := charts.Decode(r.Settings)
	if err != nil {
		out.Break = &chartBreak{Reason: "settings_invalid", Message: "This chart's settings no longer pass the checks: " + err.Error()}
		return out
	}
	src, err := loadChartSource(ctx, database, ws, userID, r.savedQueryID)
	if errors.Is(err, errChartSourceNotFound) {
		out.Break = &chartBreak{Reason: "source_hidden", Message: "This chart's source is private to its author, so its rows are not shown to you."}
		return out
	}
	if err != nil {
		log.WithError(err).Error("[charts] load source failed")
		out.Error = "The chart's source could not be loaded."
		return out
	}
	result, brk, err := drawChart(ctx, src, settings, filters, now)
	out.Result, out.Break = result, brk
	if _, ok := charts.AsError(err); ok {
		out.Break = &chartBreak{Reason: "settings_invalid", Message: "This chart's settings no longer pass the checks: " + err.Error()}
	} else if err != nil {
		out.Error = err.Error()
	}
	if b, err := loadChartBadges(ctx, database, src, now); err != nil {
		log.WithError(err).Warn("[charts] badges failed")
	} else {
		out.Badges = b
	}
	return out
}

// DrawChart draws a saved chart with the viewer's rights. A failed query is a 200 whose
// error says why, so a page of charts shows each one's state.
// POST /api/v1/charts/:id/draw
func DrawChart(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	r, err := loadVisibleChart(ctx, database, ws, userID, c.Param("id"))
	if err != nil {
		writeChartLoadError(c, err)
		return
	}
	d := drawSavedChart(ctx, database, ws, userID, r, nil, time.Now())
	c.JSON(http.StatusOK, gin.H{"chart": r, "result": d.Result, "break": d.Break, "badges": d.Badges, "error": d.Error})
}

// chartPreviewRequest is the chart page's unsaved chart.
type chartPreviewRequest struct {
	SourceID string          `json:"source_id"`
	Settings json.RawMessage `json:"settings"`
}

// PreviewChart draws unsaved settings, with the same checks a save makes but the chart's
// SQL run in full rather than once. Nothing is stored.
// POST /api/v1/charts/preview
func PreviewChart(c *gin.Context) {
	userID, ws, _, database, ok := chartRequest(c, security.WSViewer)
	if !ok {
		return
	}
	var in chartPreviewRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	settings, err := charts.Decode(in.Settings)
	if err != nil {
		writeChartRefusal(c, settingsRefusal(err))
		return
	}
	ctx := c.Request.Context()
	src, err := loadChartSourceByParam(ctx, database, ws, userID, in.SourceID)
	if err != nil {
		writeChartSourceLoadError(c, err)
		return
	}
	now := time.Now()
	if brk := chartSourceBreak(src); brk != nil {
		c.JSON(http.StatusOK, gin.H{"result": nil, "break": brk, "badges": nil})
		return
	}
	cols, err := probeChartSource(ctx, src)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"result": nil, "break": nil, "badges": nil, "error": "The source did not run: " + err.Error()})
		return
	}
	if err := settings.CheckColumns(chartColumnNames(cols)); err != nil {
		writeChartRefusal(c, settingsRefusal(err))
		return
	}
	if err := settings.CheckKinds(chartColumnKinds(cols), false); err != nil {
		writeChartRefusal(c, settingsRefusal(err))
		return
	}
	result, brk, err := drawChart(ctx, src, settings, nil, now)
	if _, ok := charts.AsError(err); ok {
		writeChartRefusal(c, settingsRefusal(err))
		return
	}
	body := gin.H{"result": result, "break": brk, "badges": nil}
	if err != nil {
		body["error"] = err.Error()
	}
	if b, err := loadChartBadges(ctx, database, src, now); err != nil {
		log.WithError(err).Warn("[charts] badges failed")
	} else {
		body["badges"] = b
	}
	c.JSON(http.StatusOK, body)
}
