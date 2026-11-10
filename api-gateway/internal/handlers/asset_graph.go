package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
)

// The workspace's data assets and what depends on what, as one graph.
//
// Today the product knows its dependencies one question at a time: a schedule dialog
// asks "which pipeline feeds THIS model?", a pipeline detail page draws THAT pipeline's
// steps. Nothing holds the whole shape, so nothing can answer the questions that are
// only askable about the whole shape — which model is stale because two hops upstream
// failed, which producers refresh nothing downstream, where a chain is one manual step
// away from being automatic.
//
// This endpoint derives that shape and reports it. It is deliberately DERIVATION ONLY:
// it writes nothing, schedules nothing and triggers nothing. The graph is the thing
// orchestration will eventually be hung off, and a graph nobody can look at first is a
// graph nobody should be orchestrating on.
//
// The seven edge kinds do not have equal standing, and the response says so on every
// edge rather than flattening them into one "depends on":
//
//   - writes (pipeline -> table)      OBSERVED. A run actually recorded this table.
//   - external (table -> table)       OBSERVED. A warehouse's external table reads the
//                                     folder a lake pipeline writes this table to: the
//                                     warehouse says where its files are
//                                     (lake_external_tables.go).
//   - materializes (model -> table)   DECLARED for a 'table' model: its target_table
//                                     says it builds this table. INFERRED for a
//                                     'statement' model: parsed out of its SQL.
//   - reads (table -> model)          INFERRED. Parsed out of the model's SQL.
//   - triggers (pipeline -> model)    DECLARED. Someone configured this schedule.
//   - draws (model -> chart)          DECLARED. A saved chart is drawn from this model:
//                                     its source, as the chart stores it.
//   - shows (chart -> dashboard)      DECLARED. A dashboard has a tile of this chart.
//
// An inferred edge is a suggestion; a declared edge is a statement of intent that may
// never have run; only an observed edge is evidence something happened. Collapsing the
// three would make the graph look far more authoritative than it is.
//
// The interesting number in the response is not the edge count. It is
// uncovered_upstreams: producers a model depends on that do not cause it to refresh.
// Every one of those is a model whose freshness depends on someone remembering — which is
// the gap a freshness deadline closes from the other side (saved_query_freshness.go). This
// graph says the wiring is incomplete; the deadline says the table actually went stale.
// Neither subsumes the other: a fully wired model whose rebuild keeps failing is invisible
// here, and a model nobody reads can be uncovered forever without mattering.

const (
	assetKindPipeline = "pipeline"
	assetKindTable    = "table"

	assetKindModel = "model"

	// A chart and a dashboard are where a model's data is looked at: drawn after the
	// models, so a change to a model names what it changes downstream.
	assetKindChart     = "chart"
	assetKindDashboard = "dashboard"

	assetEdgeWrites       = "writes"
	assetEdgeMaterializes = "materializes"
	assetEdgeReads        = "reads"
	assetEdgeTriggers     = "triggers"
	assetEdgeExternal     = "external"
	assetEdgeDraws        = "draws"
	assetEdgeShows        = "shows"

	// Evidence grades. See the header: the difference between these is the difference
	// between "this happened" and "someone typed this".
	assetEvidenceObserved = "observed"
	assetEvidenceDeclared = "declared"
	assetEvidenceInferred = "inferred"

	// triggerManualRefresh is the reported trigger for a model with no live schedule.
	// It is not a fourth schedule_type: it is the absence of one, named so the UI does
	// not have to interpret an empty string.
	triggerManualRefresh = "manual"

	// Caps. A workspace with more assets than this gets a truncated graph and is TOLD
	// it was truncated, because a graph silently missing a producer draws a model as
	// having no upstream at all — which is the exact claim this endpoint exists to
	// make, and would be false.
	assetGraphPipelineLimit = 500
	assetGraphModelLimit    = 500
	assetGraphProducedLimit = 5000
	assetGraphChartLimit    = 1000
	// A pair is one chart on one dashboard.
	assetGraphDashboardChartLimit = 2000
)

// assetNode is one thing in the workspace that data lives in or moves through.
type assetNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// RefID is the underlying pipelines.id / saved_queries.id, so the UI can link to
	// the thing itself. Empty for table nodes: a table is not a row we own.
	RefID string `json:"ref_id,omitempty"`
	// ConnectionID is the warehouse the node lives in. Set on tables and models, and
	// on pipelines it is the DESTINATION connection — where the pipeline writes, not
	// where it reads, because that is the side the rest of the graph joins on.
	ConnectionID string `json:"connection_id,omitempty"`
	// Layer is the name of a model's workspace layer (workspace_layers.go), on the
	// model and on the table it builds. "" when the model is in none.
	Layer string `json:"layer,omitempty"`
	// PipelineTable marks a table a pipeline writes. It is not a layer: the
	// workspace's layers are for models, and a pipeline's tables come first.
	PipelineTable bool `json:"pipeline_table,omitempty"`
	// ChartType is a chart's type as its settings name it ("bar", "line"…).
	ChartType string `json:"chart_type,omitempty"`
	// Charts is how many charts a dashboard shows, counting any this graph does not
	// draw: another warehouse's, or a private one of someone else's.
	Charts int `json:"charts,omitempty"`
}

type assetEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Kind     string `json:"kind"`
	Evidence string `json:"evidence"`
}

// modelRefresh answers "when does this model get rebuilt, and does that cover what it
// depends on?" for one model. This is the part of the response that is an argument
// rather than a picture.
type modelRefresh struct {
	ModelID string `json:"model_id"`
	NodeID  string `json:"node_id"`
	Name    string `json:"name"`
	// Trigger is the live schedule_type, or "manual" when the model has no schedule.
	Trigger string `json:"trigger"`
	// TriggerUpstreams are the producer node ids that cause this model to refresh, empty
	// for a clock-scheduled or manual model. A schedule can name several since migration
	// 100, and a named producer that is not in THIS graph — filtered out by connection,
	// or past the node limit — is dropped rather than emitted as an id pointing at no
	// node the caller can draw.
	TriggerUpstreams []string `json:"trigger_upstreams"`
	// TriggerPaused separates "configured but not firing" from "not configured". A
	// paused schedule still describes intent and still occupies the model's one
	// schedule slot, so hiding it would misreport why the model is stale.
	TriggerPaused bool `json:"trigger_paused"`
	// Upstreams are the producer node ids this model reads from, transitively through
	// exactly one table hop.
	Upstreams []string `json:"upstreams"`
	// UncoveredUpstreams are the producers that do NOT cause this model to refresh.
	// For a clock-scheduled or manual model that is every upstream it has: a cron is
	// not a causal link, it is a guess about when the upstream is probably done.
	UncoveredUpstreams []string `json:"uncovered_upstreams"`
	// Unresolved are references in the model's SQL that matched no known producer.
	// Reported rather than dropped, and given no phantom node: the honest answer to
	// "where does this table come from" is sometimes "we do not know".
	Unresolved []string `json:"unresolved"`
	// Ambiguous means some reference in the SQL could be more than one DIFFERENT
	// table, so the upstream list contains a producer that may not be the real one.
	// Two producers of the same table is fan-in and is not reported here.
	Ambiguous bool `json:"ambiguous"`
}

type assetGraphTruncation struct {
	Pipelines      bool `json:"pipelines"`
	ProducedTables bool `json:"produced_tables"`
	Models         bool `json:"models"`
	Charts         bool `json:"charts"`
	// Dashboards means some chart-on-a-dashboard pairs were not read, so a chart may
	// be drawn without a dashboard it is on.
	Dashboards bool `json:"dashboards"`
}

type assetGraphStats struct {
	Pipelines  int            `json:"pipelines"`
	Tables     int            `json:"tables"`
	Models     int            `json:"models"`
	Charts     int            `json:"charts"`
	Dashboards int            `json:"dashboards"`
	Edges      map[string]int `json:"edges"`
	// ModelsWithUncoveredUpstreams is the count of models that depend on at least one
	// producer that does not refresh them.
	ModelsWithUncoveredUpstreams int `json:"models_with_uncovered_upstreams"`
	ModelsWithUnresolvedRefs     int `json:"models_with_unresolved_references"`
	// ModelsWithMultipleUpstreams counts models whose lineage has two or more distinct
	// producers. This was reported as fan_in_blocked until migration 100, and the old
	// name was accurate then: a schedule held exactly ONE upstream column, so every model
	// in this count could not be wired to all of its producers however the user
	// configured it. The upstreams are a child table now and that limit is gone, so the
	// number no longer describes an impossibility — it describes a model worth checking,
	// and ModelsWithUncoveredUpstreams is the one that says whether it was wired up.
	ModelsWithMultipleUpstreams int                  `json:"models_with_multiple_upstreams"`
	Truncated                   assetGraphTruncation `json:"truncated"`
}

type assetGraph struct {
	Nodes  []assetNode     `json:"nodes"`
	Edges  []assetEdge     `json:"edges"`
	Models []modelRefresh  `json:"models"`
	Stats  assetGraphStats `json:"stats"`
	// ExternalTablesPending says a warehouse's external tables are being read for the
	// first time, so lake pipelines may not be linked to it yet. The page asks again.
	ExternalTablesPending bool `json:"external_tables_pending,omitempty"`
	// ColumnLineage is which column feeds which (asset_column_lineage.go); absent when
	// no model has an answer and none is being read.
	ColumnLineage *assetColumnLineage `json:"column_lineage,omitempty"`
}

// pipelineAsset is one pipeline, as the graph needs it.
type pipelineAsset struct {
	ID           string
	Name         string
	ConnectionID string
}

// modelAsset is one saved query plus whatever schedule it currently holds.
type modelAsset struct {
	ID              string
	Name            string
	SQLText         string
	Materialization string
	TargetTable     string
	ConnectionID    string
	// ConnectorType is the connection's, and decides what SQLText holds: SQL, or a
	// document store's stored pipeline (documentModelQuery).
	ConnectorType string
	// Layer is the name of the model's workspace layer, or "" (workspace_layers.go).
	Layer string
	// ScheduleType is "" when the model holds no schedule.
	ScheduleType   string
	ScheduleStatus string
	// TriggerUpstreamIDs are the raw producer ids the schedule waits on, in no
	// particular kind order — see the aggregate in loadAssetGraphInput.
	TriggerUpstreamIDs []string
}

// chartAsset is one saved chart drawn from a model in the graph.
type chartAsset struct {
	ID      string
	Name    string
	ModelID string
	Type    string
}

// dashboardAsset is one dashboard with the graph's charts it shows.
type dashboardAsset struct {
	ID       string
	Name     string
	ChartIDs []string
	// ChartCount is every chart it shows, drawn or not.
	ChartCount int
}

// tableProducer is one producer of one table, carrying the connection that table lives
// in. Connection is part of the identity and not a detail: see producedTableKey.
type tableProducer struct {
	ConnectionID string
	Kind         string
	Table        producedTable
	// SourceQualified is the stats row's own qualified_name: the name the table was
	// captured under, which for CDC is the SOURCE side. Read only for an Unplaced row.
	SourceQualified string
	// Unplaced is a pipeline's stats row that recorded no destination name. The
	// pipeline still wrote the table; this row just cannot say what it is called there.
	Unplaced bool
}

type assetGraphInput struct {
	Pipelines []pipelineAsset
	Produced  []tableProducer
	Models    []modelAsset
	// Charts and Dashboards are where the models' data is looked at.
	Charts     []chartAsset
	Dashboards []dashboardAsset
	Truncated  assetGraphTruncation
	// LakeLinks are the warehouse external tables that read a lake pipeline's table
	// folder (lake_external_tables.go).
	LakeLinks             []lakeTableLink
	ExternalTablesPending bool
	// ColumnLineage is each model's saved column lineage for its current SQL, by model
	// id (asset_column_lineage.go).
	ColumnLineage        map[string]savedColumnLineage
	ColumnLineagePending bool
}

// GetWorkspaceAssetGraph returns the workspace's asset graph.
// GET /api/v1/explorer/asset-graph?connection_id=<uuid>
//
// Viewer-level: everything here is derived from pipelines, saved queries, charts and
// dashboards the caller can already list, and the private saved queries, charts and
// dashboards of other users are excluded exactly as they are in their list endpoints.
func GetWorkspaceAssetGraph(c *gin.Context) {
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSViewer); !ok {
		return
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	connectionID := strings.TrimSpace(c.Query("connection_id"))

	in, err := loadAssetGraphInput(c.Request.Context(), database, workspaceID, userID, connectionID)
	if err != nil {
		log.WithError(err).Error("asset graph: could not load workspace assets")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build asset graph"})
		return
	}
	c.JSON(http.StatusOK, buildAssetGraph(in))
}

// loadAssetGraphInput fetches everything the derivation needs in three queries, after
// the lake links (loadLakeTableLinks), and then the models' column lineage
// (loadColumnLineage) and the charts and dashboards that look at those models
// (loadModelDisplays).
//
// Three, and not one per model: a workspace with fifty models would otherwise spend
// fifty round trips answering a question that is one join and some string matching.
// The matching itself deliberately happens in Go rather than SQL — it is the same rule
// the schedule dialog uses (matchTableReference), and a second copy of it written in
// SQL is exactly the drift that rule was extracted to prevent.
func loadAssetGraphInput(
	ctx context.Context,
	database *sql.DB,
	workspaceID, userID, connectionID string,
) (assetGraphInput, error) {
	var in assetGraphInput

	// Read first, because a graph narrowed to one warehouse keeps the lake pipelines
	// that feed it: they write to a bucket, not to the warehouse, so the connection
	// filter below would drop them. A failure costs the links, not the graph.
	lake, err := loadLakeTableLinks(ctx, database, workspaceID, connectionID)
	if err != nil {
		log.WithError(err).Warn("asset graph: could not load lake links; drawing without them")
	}
	in.LakeLinks, in.ExternalTablesPending = lake.Links, lake.Pending
	feedingIDs := []string{}
	if connectionID != "" {
		for _, l := range lake.Links {
			feedingIDs = append(feedingIDs, l.PipelineID)
		}
	}

	pipeRows, err := database.QueryContext(ctx, `
		SELECT p.id::text,
		       COALESCE(p.name, ''),
		       COALESCE(p.destination_connection_id::text, '')
		FROM pipelines p
		WHERE p.workspace_id = $1::uuid
		  AND ($2 = '' OR p.destination_connection_id::text = $2 OR p.id::text = ANY($4))
		ORDER BY p.created_at, p.id
		LIMIT $3`, workspaceID, connectionID, assetGraphPipelineLimit+1, pgdriver.StringArray(feedingIDs))
	if err != nil {
		return in, err
	}
	defer pipeRows.Close()
	for pipeRows.Next() {
		var p pipelineAsset
		if err := pipeRows.Scan(&p.ID, &p.Name, &p.ConnectionID); err != nil {
			return in, err
		}
		in.Pipelines = append(in.Pipelines, p)
	}
	if err := pipeRows.Err(); err != nil {
		return in, err
	}
	if len(in.Pipelines) > assetGraphPipelineLimit {
		in.Pipelines = in.Pipelines[:assetGraphPipelineLimit]
		in.Truncated.Pipelines = true
	}

	// Every destination table any run has recorded, not only the ones some model
	// happens to read. A pipeline whose output nothing consumes is a finding, and it is
	// invisible if the query is filtered by what the models asked for.
	//
	// Rows with no destination_qualified_name are kept. Only the CDC sink fills that
	// column, and it leaves it NULL for an object-storage destination, a namespace that
	// is empty or literally "default", a row the orchestrator's cdcstats agent wrote
	// before any sink write, and every row older than migration 089. Filtering them out
	// drew a pipeline that was landing data as "Writes 0 tables". buildAssetGraph draws
	// them under the captured name and keeps them away from the read matcher; the
	// upstream suggestion still excludes them (saved_query_upstreams.go), because it
	// offers a confident answer rather than a picture.
	prodRows, err := database.QueryContext(ctx, `
		SELECT DISTINCT
		    p.id::text,
		    COALESCE(p.name, ''),
		    COALESCE(p.destination_connection_id::text, ''),
		    COALESCE(s.destination_qualified_name, ''),
		    COALESCE(s.table_name, ''),
		    COALESCE(s.qualified_name, '')
		FROM pipeline_run_table_stats s
		JOIN pipelines p ON p.id = s.pipeline_id
		WHERE p.workspace_id = $1::uuid
		  AND ($2 = '' OR p.destination_connection_id::text = $2 OR p.id::text = ANY($4))
		ORDER BY 1, 4, 5, 6
		LIMIT $3`, workspaceID, connectionID, assetGraphProducedLimit+1, pgdriver.StringArray(feedingIDs))
	if err != nil {
		return in, err
	}
	defer prodRows.Close()
	for prodRows.Next() {
		tp := tableProducer{Kind: assetKindPipeline}
		if err := prodRows.Scan(&tp.Table.ProducerID, &tp.Table.ProducerName,
			&tp.ConnectionID, &tp.Table.DestQualified, &tp.Table.TableName,
			&tp.SourceQualified); err != nil {
			return in, err
		}
		tp.Unplaced = tp.Table.DestQualified == ""
		in.Produced = append(in.Produced, tp)
	}
	if err := prodRows.Err(); err != nil {
		return in, err
	}
	if len(in.Produced) > assetGraphProducedLimit {
		in.Produced = in.Produced[:assetGraphProducedLimit]
		in.Truncated.ProducedTables = true
	}

	// status != 'deleted' rather than = 'active': a paused schedule still occupies the
	// model's one schedule slot and still says what the user intended, and reporting it
	// as "manual" would answer the freshness question wrongly.
	modelRows, err := database.QueryContext(ctx, `
		SELECT sq.id::text,
		       COALESCE(sq.name, ''),
		       sq.sql_text,
		       sq.materialization,
		       COALESCE(sq.target_table, ''),
		       COALESCE(sq.connection_id::text, ''),
		       COALESCE(cn.connector_type, ''),
		       COALESCE(wl.name, ''),
		       COALESCE(s.schedule_type, ''),
		       COALESCE(s.status, ''),
		       COALESCE((
		           SELECT string_agg(COALESCE(u.upstream_pipeline_id::text, u.upstream_saved_query_id::text), ',')
		           FROM saved_query_schedule_upstreams u
		           WHERE u.schedule_id = s.schedule_id
		       ), '')
		FROM saved_queries sq
		LEFT JOIN saved_query_schedules s
		  ON s.saved_query_id = sq.id AND s.status != 'deleted'
		-- The connector type says what sql_text holds: SQL, or a document store's
		-- stored pipeline (modelReadReferences). LEFT JOIN so a model outlives its
		-- connection, as the saved-query list's own read does.
		LEFT JOIN connections cn ON cn.id = sq.connection_id
		LEFT JOIN workspace_layers wl ON wl.id = sq.layer_id
		WHERE sq.workspace_id = $1
		  AND (sq.visibility = 'workspace' OR sq.created_by = $2)
		  AND ($3 = '' OR sq.connection_id::text = $3)
		ORDER BY sq.updated_at DESC, sq.id
		LIMIT $4`, workspaceID, userID, connectionID, assetGraphModelLimit+1)
	if err != nil {
		return in, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var m modelAsset
		// Aggregated to one comma-joined string rather than returned as an array, and
		// rather than as extra rows. Extra rows would multiply every model by its upstream
		// count and turn the LIMIT above into a limit on upstream rows; an array would
		// need pq.Array, which this module does not have since the move to pgx.
		var upstreamIDs string
		if err := modelRows.Scan(&m.ID, &m.Name, &m.SQLText, &m.Materialization,
			&m.TargetTable, &m.ConnectionID, &m.ConnectorType, &m.Layer, &m.ScheduleType, &m.ScheduleStatus,
			&upstreamIDs); err != nil {
			return in, err
		}
		if upstreamIDs != "" {
			m.TriggerUpstreamIDs = strings.Split(upstreamIDs, ",")
		}
		in.Models = append(in.Models, m)
	}
	if err := modelRows.Err(); err != nil {
		return in, err
	}
	if len(in.Models) > assetGraphModelLimit {
		in.Models = in.Models[:assetGraphModelLimit]
		in.Truncated.Models = true
	}

	// A failure costs the column lineage, not the graph: the tables and models stand
	// without it.
	columns, err := loadColumnLineage(ctx, database, workspaceID, in.Models)
	if err != nil {
		log.WithError(err).Warn("asset graph: could not load column lineage; drawing without it")
	} else {
		in.ColumnLineage, in.ColumnLineagePending = columns.ByModel, columns.Pending
	}

	// A failure costs the charts, not the graph: they are where data is looked at,
	// and the lineage above them stands without them.
	if err := loadModelDisplays(ctx, database, workspaceID, userID, &in); err != nil {
		log.WithError(err).Warn("asset graph: could not load charts and dashboards; drawing without them")
		in.Charts, in.Dashboards = nil, nil
		in.Truncated.Charts, in.Truncated.Dashboards = false, false
	}

	return in, nil
}

// loadModelDisplays reads the saved charts drawn from the loaded models, and the
// dashboards that show those charts, each as visible to the caller as in its own list
// (charts_api.go, dashboards_api.go). Narrowing the graph to one warehouse narrows
// these too, through the models.
func loadModelDisplays(ctx context.Context, database *sql.DB, workspaceID, userID string, in *assetGraphInput) error {
	if len(in.Models) == 0 {
		return nil
	}
	modelIDs := make([]string, 0, len(in.Models))
	for _, m := range in.Models {
		modelIDs = append(modelIDs, m.ID)
	}
	chartRows, err := database.QueryContext(ctx, `
		SELECT ch.id::text,
		       COALESCE(ch.name, ''),
		       ch.saved_query_id::text,
		       COALESCE(ch.settings->>'type', '')
		FROM charts ch
		WHERE ch.workspace_id = $1
		  AND (ch.visibility = 'workspace' OR ch.created_by = $2)
		  AND ch.saved_query_id::text = ANY($3)
		ORDER BY ch.updated_at DESC, ch.id
		LIMIT $4`, workspaceID, userID, pgdriver.StringArray(modelIDs), assetGraphChartLimit+1)
	if err != nil {
		return err
	}
	defer chartRows.Close()
	var charts []chartAsset
	for chartRows.Next() {
		var ch chartAsset
		if err := chartRows.Scan(&ch.ID, &ch.Name, &ch.ModelID, &ch.Type); err != nil {
			return err
		}
		charts = append(charts, ch)
	}
	if err := chartRows.Err(); err != nil {
		return err
	}
	if len(charts) > assetGraphChartLimit {
		charts = charts[:assetGraphChartLimit]
		in.Truncated.Charts = true
	}
	in.Charts = charts
	if len(charts) == 0 {
		return nil
	}

	chartIDs := make([]string, 0, len(charts))
	for _, ch := range charts {
		chartIDs = append(chartIDs, ch.ID)
	}
	// One row per chart a dashboard shows: a chart on two tiles of one dashboard is
	// one link. The count is every chart the dashboard shows, drawn here or not.
	dashRows, err := database.QueryContext(ctx, `
		SELECT d.id::text,
		       COALESCE(d.name, ''),
		       t.chart_id::text,
		       (SELECT count(DISTINCT a.chart_id)
		        FROM dashboard_tiles a
		        WHERE a.dashboard_id = d.id AND a.kind = 'chart')
		FROM dashboards d
		JOIN dashboard_tiles t ON t.dashboard_id = d.id AND t.kind = 'chart'
		WHERE d.workspace_id = $1
		  AND (d.visibility = 'workspace' OR d.created_by = $2)
		  AND t.chart_id::text = ANY($3)
		GROUP BY d.id, d.name, d.updated_at, t.chart_id
		ORDER BY d.updated_at DESC, d.id, t.chart_id
		LIMIT $4`, workspaceID, userID, pgdriver.StringArray(chartIDs), assetGraphDashboardChartLimit+1)
	if err != nil {
		return err
	}
	defer dashRows.Close()
	pairs := 0
	byID := map[string]int{}
	for dashRows.Next() {
		var id, name, chartID string
		var count int
		if err := dashRows.Scan(&id, &name, &chartID, &count); err != nil {
			return err
		}
		if pairs++; pairs > assetGraphDashboardChartLimit {
			in.Truncated.Dashboards = true
			break
		}
		i, ok := byID[id]
		if !ok {
			i = len(in.Dashboards)
			byID[id] = i
			in.Dashboards = append(in.Dashboards, dashboardAsset{ID: id, Name: name, ChartCount: count})
		}
		in.Dashboards[i].ChartIDs = append(in.Dashboards[i].ChartIDs, chartID)
	}
	return dashRows.Err()
}

// buildAssetGraph is the whole derivation, and takes no database on purpose: every
// claim this endpoint makes is a claim about this function, so it has to be testable
// without a Postgres to hand.
func buildAssetGraph(in assetGraphInput) assetGraph {
	g := assetGraph{
		Nodes:                 []assetNode{},
		Edges:                 []assetEdge{},
		Models:                []modelRefresh{},
		Stats:                 assetGraphStats{Edges: map[string]int{}, Truncated: in.Truncated},
		ExternalTablesPending: in.ExternalTablesPending,
	}

	nodes := map[string]assetNode{}
	addNode := func(n assetNode) {
		if _, ok := nodes[n.ID]; !ok {
			nodes[n.ID] = n
		}
	}
	edges := map[assetEdge]bool{}
	addEdge := func(e assetEdge) {
		edges[e] = true
	}

	// Producers indexed by connection, because a table name only identifies a table
	// inside one warehouse. A model is matched against its own connection's producers
	// and nothing else — the same scoping the upstream suggestion enforces in SQL.
	byConnection := map[string][]producedTable{}
	pipelineByID := map[string]pipelineAsset{}
	for _, p := range in.Pipelines {
		pipelineByID[p.ID] = p
		addNode(assetNode{
			ID:           assetKindPipeline + ":" + p.ID,
			Kind:         assetKindPipeline,
			Name:         p.Name,
			RefID:        p.ID,
			ConnectionID: p.ConnectionID,
		})
	}

	// A table a pipeline wrote is a pipeline table; one a model built takes the
	// model's layer. The first producer to draw a table wins, as for the node itself.
	registerProducer := func(tp tableProducer, producerNodeID, edgeKind, evidence, layer string, pipelineTable bool) {
		key := producedTableKey(tp.ConnectionID, tp.Table)
		tableNodeID := assetKindTable + ":" + key
		display := tp.Table.DestQualified
		if display == "" {
			display = tp.Table.TableName
		}
		addNode(assetNode{
			ID:            tableNodeID,
			Kind:          assetKindTable,
			Name:          display,
			ConnectionID:  tp.ConnectionID,
			Layer:         layer,
			PipelineTable: pipelineTable,
		})
		byConnection[tp.ConnectionID] = append(byConnection[tp.ConnectionID], tp.Table)
		addEdge(assetEdge{From: producerNodeID, To: tableNodeID, Kind: edgeKind, Evidence: evidence})
	}

	// An Unplaced row is a table the pipeline wrote under a name the row does not
	// record. It is drawn, so the pipeline does not read as writing nothing, but it is
	// keyed apart from destination-side names and never added to byConnection: a
	// model's `FROM orders` names a table in its warehouse, and matching it against a
	// captured-side name is the confident wrong answer saved_query_upstreams.go exists
	// to avoid.
	//
	// The same table can have a named row from one run and an unnamed one from
	// another (a run from before the sink recorded destinations). The named row
	// already draws it; drawing both would count one table twice.
	sourceKey := func(tp tableProducer) string {
		return tp.Table.ProducerID + "\x00" + strings.ToLower(tp.SourceQualified)
	}
	placedSource := map[string]bool{}
	for _, tp := range in.Produced {
		if !tp.Unplaced && tp.SourceQualified != "" {
			placedSource[sourceKey(tp)] = true
		}
	}
	// The captured tables each pipeline drew, so a lake link can join the one it
	// names instead of drawing the same table twice.
	captured := map[string]map[string]string{} // pipeline id -> node id -> name
	registerUnplaced := func(tp tableProducer) {
		name := tp.SourceQualified
		if name == "" {
			name = tp.Table.TableName
		}
		tableNodeID := assetKindTable + ":" + tp.ConnectionID + "\x00captured\x00" + strings.ToLower(name)
		if captured[tp.Table.ProducerID] == nil {
			captured[tp.Table.ProducerID] = map[string]string{}
		}
		captured[tp.Table.ProducerID][tableNodeID] = name
		addNode(assetNode{
			ID:            tableNodeID,
			Kind:          assetKindTable,
			Name:          name,
			ConnectionID:  tp.ConnectionID,
			PipelineTable: true,
		})
		addEdge(assetEdge{
			From: assetKindPipeline + ":" + tp.Table.ProducerID, To: tableNodeID,
			Kind: assetEdgeWrites, Evidence: assetEvidenceObserved,
		})
	}

	for _, tp := range in.Produced {
		// A stats row whose pipeline fell outside the page is not drawable: its
		// producer node does not exist, and an edge from a node that is not in the
		// response is worse than a missing edge.
		if _, ok := pipelineByID[tp.Table.ProducerID]; !ok {
			continue
		}
		if tp.Unplaced {
			if !placedSource[sourceKey(tp)] {
				registerUnplaced(tp)
			}
			continue
		}
		registerProducer(tp, assetKindPipeline+":"+tp.Table.ProducerID, assetEdgeWrites, assetEvidenceObserved, "", true)
	}

	// A lake pipeline's table is read by the warehouse through an external table, on
	// another connection. The external table is drawn as a pipeline table of the
	// warehouse with the pipeline as its producer, so a model reading it is matched
	// like any other read of a pipeline's table, and a schedule following the pipeline
	// covers it. The edge into it comes from the folder's own table.
	linkedKeys := map[string][]string{} // pipeline id -> its linked table keys
	for _, l := range in.LakeLinks {
		linkedKeys[l.PipelineID] = append(linkedKeys[l.PipelineID], l.TableKey)
	}
	for _, l := range in.LakeLinks {
		p, ok := pipelineByID[l.PipelineID]
		if !ok {
			continue
		}
		lakeNodeID := capturedLakeTable(captured[p.ID], l.TableKey, linkedKeys[p.ID])
		if lakeNodeID == "" {
			lakeNodeID = assetKindTable + ":" + l.LakeConnectionID + "\x00lake\x00" + strings.ToLower(l.TableKey)
			addNode(assetNode{
				ID:            lakeNodeID,
				Kind:          assetKindTable,
				Name:          lakeTableKeyName(l.TableKey),
				ConnectionID:  l.LakeConnectionID,
				PipelineTable: true,
			})
			addEdge(assetEdge{
				From: assetKindPipeline + ":" + p.ID, To: lakeNodeID,
				Kind: assetEdgeWrites, Evidence: assetEvidenceObserved,
			})
		}
		registerProducer(tableProducer{
			ConnectionID: l.WarehouseConnectionID,
			Kind:         assetKindPipeline,
			Table: producedTable{
				ProducerID:    p.ID,
				ProducerName:  p.Name,
				DestQualified: l.Schema + "." + l.Name,
				TableName:     l.Name,
			},
		}, lakeNodeID, assetEdgeExternal, assetEvidenceObserved, "", true)
	}

	// A model that writes a table is a producer of it, exactly like a pipeline: a
	// 'table' model its target_table, a 'statement' model the tables its own SQL names
	// (modelProducedTables — the same rule the upstream suggestion uses, so the graph
	// and the picker cannot disagree about who writes a table). This is what makes
	// model -> table -> model chains derivable at all. That is LINEAGE: this model reads
	// what that one writes. A model-to-model TRIGGER is a different claim, a schedule
	// that names the other model (triggers edges below); a chain can have the first and
	// not the second, which is what uncovered_upstreams reports.
	for _, m := range in.Models {
		addNode(assetNode{
			ID:           assetKindModel + ":" + m.ID,
			Kind:         assetKindModel,
			Name:         m.Name,
			RefID:        m.ID,
			ConnectionID: m.ConnectionID,
			Layer:        m.Layer,
		})
		// A target_table is typed by a person; a statement's target is read out of SQL,
		// which is the same grade as a reads edge.
		evidence := assetEvidenceDeclared
		if m.Materialization == matStatement {
			evidence = assetEvidenceInferred
		}
		for _, table := range modelProducedTables(m.ConnectorType, m.ID, m.Name, m.Materialization, m.TargetTable, m.SQLText) {
			registerProducer(
				tableProducer{ConnectionID: m.ConnectionID, Kind: assetKindModel, Table: table},
				assetKindModel+":"+m.ID, assetEdgeMaterializes, evidence, m.Layer, false,
			)
		}
	}

	producerKindOf := map[string]string{}
	for _, p := range in.Pipelines {
		producerKindOf[p.ID] = assetKindPipeline
	}
	for _, m := range in.Models {
		producerKindOf[m.ID] = assetKindModel
	}
	producerNodeID := func(id string) string {
		if kind, ok := producerKindOf[id]; ok {
			return kind + ":" + id
		}
		return ""
	}

	for _, m := range in.Models {
		modelNodeID := assetKindModel + ":" + m.ID
		summary := modelRefresh{
			ModelID:            m.ID,
			NodeID:             modelNodeID,
			Name:               m.Name,
			Trigger:            triggerManualRefresh,
			TriggerPaused:      m.ScheduleStatus == "paused",
			Upstreams:          []string{},
			UncoveredUpstreams: []string{},
			TriggerUpstreams:   []string{},
			Unresolved:         []string{},
		}
		if m.ScheduleType != "" {
			summary.Trigger = m.ScheduleType
		}

		upstreams := map[string]bool{}
		for _, ref := range modelReadReferences(m.ConnectorType, m.SQLText) {
			matches, _ := matchTableReference(ref, byConnection[m.ConnectionID])
			// A model that materializes into a table it also reads is an incremental
			// pattern, not a dependency on itself. Drawing the self-loop would put a
			// cycle in a graph whose whole purpose is ordering.
			kept := matches[:0:0]
			for _, match := range matches {
				if match.ProducerID != m.ID {
					kept = append(kept, match)
				}
			}
			if len(kept) == 0 {
				summary.Unresolved = append(summary.Unresolved, ref.Qualified())
				continue
			}
			// Two producers of the SAME table is fan-in, not ambiguity: both of them
			// really do write it. Ambiguity is a reference that could be two DIFFERENT
			// tables, which is only reachable through an unqualified match — `orders`
			// hitting both analytics.orders and staging.orders. Conflating the two
			// would report every fan-in as an uncertainty and hide the real ones.
			hitTables := map[string]bool{}
			for _, match := range kept {
				tableNodeID := assetKindTable + ":" + producedTableKey(m.ConnectionID, match)
				hitTables[tableNodeID] = true
				addEdge(assetEdge{
					From: tableNodeID, To: modelNodeID,
					Kind: assetEdgeReads, Evidence: assetEvidenceInferred,
				})
				if nodeID := producerNodeID(match.ProducerID); nodeID != "" {
					upstreams[nodeID] = true
				}
			}
			if len(hitTables) > 1 {
				summary.Ambiguous = true
			}
		}

		// The trigger edges are drawn from the schedule, not from the lineage: a user may
		// point a trigger at a producer this model does not read, and that is a fact about
		// the workspace worth showing rather than correcting.
		triggerNodes := map[string]bool{}
		if m.ScheduleType == scheduleAfterUpstream {
			for _, upstreamID := range m.TriggerUpstreamIDs {
				// producerNodeID answers both questions at once: what node id this producer
				// has, and whether it is in this graph at all. It needs no kind alongside
				// the id because a producer id is a uuid, unique across pipelines and
				// models alike — which is also why the aggregate above does not select one.
				nodeID := producerNodeID(upstreamID)
				if nodeID == "" {
					continue
				}
				triggerNodes[nodeID] = true
				addEdge(assetEdge{
					From: nodeID, To: modelNodeID,
					Kind: assetEdgeTriggers, Evidence: assetEvidenceDeclared,
				})
			}
		}
		for id := range triggerNodes {
			summary.TriggerUpstreams = append(summary.TriggerUpstreams, id)
		}

		for id := range upstreams {
			summary.Upstreams = append(summary.Upstreams, id)
			if !triggerNodes[id] {
				summary.UncoveredUpstreams = append(summary.UncoveredUpstreams, id)
			}
		}
		sort.Strings(summary.Upstreams)
		sort.Strings(summary.UncoveredUpstreams)
		sort.Strings(summary.TriggerUpstreams)

		if len(summary.UncoveredUpstreams) > 0 {
			g.Stats.ModelsWithUncoveredUpstreams++
		}
		if len(summary.Unresolved) > 0 {
			g.Stats.ModelsWithUnresolvedRefs++
		}
		if len(summary.Upstreams) > 1 {
			g.Stats.ModelsWithMultipleUpstreams++
		}
		g.Models = append(g.Models, summary)
	}

	// Column lineage maps its tables onto the same table nodes the reads edges above
	// came from, so it follows the models and byConnection is complete.
	g.ColumnLineage = buildColumnLineage(in, byConnection)

	// Charts after the models, dashboards after the charts: where each model's data is
	// looked at. A chart whose model is not in this graph is not drawn, and neither is
	// a dashboard none of whose charts is: an edge needs both of its ends.
	for _, ch := range in.Charts {
		modelNodeID := assetKindModel + ":" + ch.ModelID
		if _, ok := nodes[modelNodeID]; !ok {
			continue
		}
		chartNodeID := assetKindChart + ":" + ch.ID
		addNode(assetNode{
			ID:           chartNodeID,
			Kind:         assetKindChart,
			Name:         ch.Name,
			RefID:        ch.ID,
			ConnectionID: nodes[modelNodeID].ConnectionID,
			ChartType:    ch.Type,
		})
		addEdge(assetEdge{From: modelNodeID, To: chartNodeID, Kind: assetEdgeDraws, Evidence: assetEvidenceDeclared})
	}
	for _, d := range in.Dashboards {
		dashboardNodeID := assetKindDashboard + ":" + d.ID
		for _, chartID := range d.ChartIDs {
			chartNodeID := assetKindChart + ":" + chartID
			if _, ok := nodes[chartNodeID]; !ok {
				continue
			}
			addNode(assetNode{
				ID:     dashboardNodeID,
				Kind:   assetKindDashboard,
				Name:   d.Name,
				RefID:  d.ID,
				Charts: d.ChartCount,
			})
			addEdge(assetEdge{From: chartNodeID, To: dashboardNodeID, Kind: assetEdgeShows, Evidence: assetEvidenceDeclared})
		}
	}

	for _, n := range nodes {
		g.Nodes = append(g.Nodes, n)
		switch n.Kind {
		case assetKindPipeline:
			g.Stats.Pipelines++
		case assetKindTable:
			g.Stats.Tables++
		case assetKindModel:
			g.Stats.Models++
		case assetKindChart:
			g.Stats.Charts++
		case assetKindDashboard:
			g.Stats.Dashboards++
		}
	}
	for e := range edges {
		g.Edges = append(g.Edges, e)
		g.Stats.Edges[e.Kind]++
	}

	// Map iteration is randomised, and a graph whose node order changes between two
	// identical requests makes every diff and every test worthless.
	sort.Slice(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Kind != g.Nodes[j].Kind {
			return g.Nodes[i].Kind < g.Nodes[j].Kind
		}
		return g.Nodes[i].ID < g.Nodes[j].ID
	})
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].Kind != g.Edges[j].Kind {
			return g.Edges[i].Kind < g.Edges[j].Kind
		}
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})

	return g
}

// capturedLakeTable is the captured table node a lake link's table key names: the one
// whose name ends the key ("public.transactions" for fintech/public/transactions),
// when no other captured table and no other linked key of the pipeline could be the
// same match. "" means draw the folder's table on its own.
func capturedLakeTable(captured map[string]string, tableKey string, pipelineKeys []string) string {
	hit := ""
	for nodeID, name := range captured {
		if !lakeTableKeyEndsWith(tableKey, name) {
			continue
		}
		if hit != "" {
			return ""
		}
		hit = nodeID
	}
	if hit == "" {
		return ""
	}
	for _, other := range pipelineKeys {
		if other != tableKey && lakeTableKeyEndsWith(other, captured[hit]) {
			return ""
		}
	}
	return hit
}

// splitModelTarget reads a stored target_table into the same shape a pipeline's stats
// row produces, so one matcher serves both.
//
// It is lenient where validateModelTarget is strict, and that asymmetry is deliberate:
// validation guards what goes IN, and refusing to draw a row that is already in the
// table would just make the graph quietly incomplete.
func splitModelTarget(raw string) producedTable {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return producedTable{}
	}
	parts := strings.Split(raw, ".")
	name := strings.TrimSpace(parts[len(parts)-1])
	if name == "" {
		return producedTable{}
	}
	if len(parts) == 1 {
		return producedTable{TableName: name}
	}
	schema := strings.TrimSpace(parts[len(parts)-2])
	if schema == "" {
		return producedTable{TableName: name}
	}
	return producedTable{DestQualified: schema + "." + name, TableName: name}
}
