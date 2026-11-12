package handlers

// A Lineage page node's run history (design § 4.4): the runs lineage_events recorded
// against a pipeline, a model or a table, newest first, and for a model how its output
// columns changed between the SQL versions that ran.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	lineageHistoryDefaultLimit = 50
	lineageHistoryMaxLimit     = 200
)

// lineageHistoryNodeFilterSQL is the WHERE of every read of a node's events, bound to
// $1 workspace, $2 user, $3 pipeline id, $4 model id, $5 table keys (lineageHistoryArgs):
// the runs a page lists and the skipped runs it counts are filtered by this one text.
const lineageHistoryNodeFilterSQL = `e.workspace_id = $1::uuid
		  AND (e.saved_query_id IS NULL OR sq.visibility = 'workspace' OR sq.created_by::text = $2)
		  AND ($3 = '' OR e.pipeline_id = NULLIF($3, '')::uuid)
		  AND ($4 = '' OR e.saved_query_id = NULLIF($4, '')::uuid)
		  AND (cardinality($5::text[]) = 0 OR e.table_keys && $5::text[])`

// lineageHistoryFilter is what a node's history is looked up by.
type lineageHistoryFilter struct {
	// Kind is the node's: pipeline, model or table. "" when the node has no history.
	Kind string
	ID   string
	Keys []string
}

// lineageHistoryFilterFor reads a graph node id (buildAssetGraph). A placed table is
// found by its key and, when its name is qualified, by its bare name as well: a model
// that reads it unqualified is keyed so, as the graph matches such a read
// (matchTableReference). A lake table, a chart and a dashboard have none: nothing runs
// against them that rsync.ai records.
func lineageHistoryFilterFor(node string) (lineageHistoryFilter, bool) {
	kind, rest, ok := strings.Cut(node, ":")
	if !ok || rest == "" {
		return lineageHistoryFilter{}, false
	}
	switch kind {
	case assetKindPipeline, assetKindModel:
		if !validUUID(rest) {
			return lineageHistoryFilter{}, false
		}
		return lineageHistoryFilter{Kind: kind, ID: rest}, true
	case assetKindTable:
		parts := strings.Split(rest, "\x00")
		if !validUUID(parts[0]) {
			return lineageHistoryFilter{}, false
		}
		conn := parts[0]
		switch {
		case len(parts) == 2 && parts[1] != "":
			name := parts[1]
			keys := []string{lineageTableKey(conn, name)}
			if i := strings.LastIndex(name, "."); i >= 0 && i < len(name)-1 {
				keys = append(keys, lineageTableKey(conn, name[i+1:]))
			}
			return lineageHistoryFilter{Kind: kind, Keys: keys}, true
		case len(parts) == 3 && parts[1] == "captured" && parts[2] != "":
			return lineageHistoryFilter{Kind: kind, Keys: []string{lineageCapturedKey(conn, parts[2])}}, true
		case len(parts) == 3 && parts[1] == "lake":
			return lineageHistoryFilter{}, true
		}
		return lineageHistoryFilter{}, false
	case assetKindChart, assetKindDashboard:
		return lineageHistoryFilter{}, true
	}
	return lineageHistoryFilter{}, false
}

// lineageHistoryRun is one recorded run as the page shows it.
type lineageHistoryRun struct {
	ID        string           `json:"id"`
	JobKind   string           `json:"job_kind"`
	JobID     string           `json:"job_id"`
	JobName   string           `json:"job_name"`
	RunRef    string           `json:"run_ref"`
	EventType string           `json:"event_type"`
	StartedAt *time.Time       `json:"started_at"`
	EndedAt   time.Time        `json:"ended_at"`
	Inputs    []lineageDataset `json:"inputs"`
	Outputs   []lineageDataset `json:"outputs"`
}

// modelSQLVersion is one SQL version of a model that ran, from its first recorded run,
// with the output columns read for it.
type modelSQLVersion struct {
	FirstRun time.Time
	// Known says the version's columns were read (model_column_lineage ok or partial).
	Known   bool
	Columns []string
}

// lineageColumnChange is how a model's output columns changed when its SQL did.
type lineageColumnChange struct {
	At time.Time `json:"at"`
	// ColumnsKnown is false when either version's columns were not read; Added and
	// Dropped are then empty.
	ColumnsKnown bool     `json:"columns_known"`
	Added        []string `json:"added"`
	Dropped      []string `json:"dropped"`
}

// modelColumnChanges compares each version with the one before it, without case, as the
// warehouses that fold case do.
func modelColumnChanges(versions []modelSQLVersion) []lineageColumnChange {
	out := []lineageColumnChange{}
	for i := 1; i < len(versions); i++ {
		prev, cur := versions[i-1], versions[i]
		ch := lineageColumnChange{At: cur.FirstRun, Added: []string{}, Dropped: []string{}}
		if prev.Known && cur.Known {
			ch.ColumnsKnown = true
			ch.Added = columnsMissingFrom(cur.Columns, prev.Columns)
			ch.Dropped = columnsMissingFrom(prev.Columns, cur.Columns)
		}
		out = append(out, ch)
	}
	return out
}

// columnsMissingFrom is the columns of a that b does not have, in a's order.
func columnsMissingFrom(a, b []string) []string {
	have := map[string]bool{}
	for _, c := range b {
		have[strings.ToLower(c)] = true
	}
	out := []string{}
	for _, c := range a {
		if k := strings.ToLower(c); !have[k] {
			have[k] = true
			out = append(out, c)
		}
	}
	return out
}

// GetLineageHistory answers GET /lineage/history?node=<graph node id>&limit=&skipped=.
//
// Viewer-level and workspace-scoped, as the graph is: a run of another user's private
// model is left out, as the model is from the graph.
//
// A skipped model run is a run that did not run, so the list leaves it out unless
// skipped=1: a model skipped every hour would otherwise fill the newest-50 window with
// them and push the runs that ran out of it. A model's answer says how many it left out
// and when the last ended (skipped_runs, last_skipped_at).
func GetLineageHistory(c *gin.Context) {
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
	node := c.Query("node")
	f, ok := lineageHistoryFilterFor(node)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node is not a Lineage page node"})
		return
	}
	limit := lineageHistoryDefaultLimit
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = min(v, lineageHistoryMaxLimit)
	}
	withSkipped := c.Query("skipped") == "1"
	resp := gin.H{
		"node":            node,
		"supported":       f.Kind != "",
		"retention_days":  lineageEventRetentionDays(),
		"total_runs":      0,
		"runs":            []lineageHistoryRun{},
		"skipped_runs":    0,
		"last_skipped_at": nil,
	}
	if f.Kind == "" {
		c.JSON(http.StatusOK, resp)
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	ctx := c.Request.Context()

	if f.Kind == assetKindModel {
		var visible bool
		err := database.QueryRowContext(ctx, `
			SELECT visibility = 'workspace' OR created_by::text = $3
			FROM saved_queries WHERE id = $1::uuid AND workspace_id = $2::uuid`,
			f.ID, workspaceID, userID).Scan(&visible)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !visible) {
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found"})
			return
		}
		if err != nil {
			log.WithError(err).Error("lineage history: could not read the model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load lineage history"})
			return
		}
	}

	runs, total, err := loadLineageHistory(ctx, database, workspaceID, userID, f, limit, withSkipped)
	if err != nil && !isUndefinedTable(err) {
		log.WithError(err).Error("lineage history: could not read the runs")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load lineage history"})
		return
	}
	if runs != nil {
		resp["runs"] = runs
		resp["total_runs"] = total
	}
	if f.Kind == assetKindModel {
		skipped, lastSkipped, err := loadLineageSkippedRuns(ctx, database, workspaceID, userID, f)
		if err != nil && !isUndefinedTable(err) {
			log.WithError(err).Error("lineage history: could not count the skipped runs")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load lineage history"})
			return
		}
		resp["skipped_runs"] = skipped
		resp["last_skipped_at"] = lastSkipped
		versions, err := loadModelSQLVersions(ctx, database, workspaceID, f.ID)
		if err != nil && !isUndefinedTable(err) {
			log.WithError(err).Error("lineage history: could not read the model's versions")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load lineage history"})
			return
		}
		resp["column_changes"] = modelColumnChanges(versions)
	}
	c.JSON(http.StatusOK, resp)
}

// lineageSkippedRunSQL is lineageEventNamesTables' false case in SQL: a model run that
// was skipped.
const lineageSkippedRunSQL = `(e.job_kind = '` + lineageJobModel + `' AND e.event_type = '` + lineageEventAbort + `')`

// lineageHistoryArgs binds lineageHistoryNodeFilterSQL's $3 to $5 for a node.
func lineageHistoryArgs(f lineageHistoryFilter) (pipelineID, modelID string, keys pgdriver.StringArray) {
	keys = pgdriver.StringArray{}
	switch f.Kind {
	case assetKindPipeline:
		pipelineID = f.ID
	case assetKindModel:
		modelID = f.ID
	default:
		keys = pgdriver.StringArray(f.Keys)
	}
	return pipelineID, modelID, keys
}

// loadLineageSkippedRuns counts a model's skipped runs and finds when the last one ended;
// zero and nil when it was never skipped. Over the same events as loadLineageHistory.
func loadLineageSkippedRuns(ctx context.Context, database *sql.DB, workspaceID, userID string, f lineageHistoryFilter) (int, *time.Time, error) {
	pipelineID, modelID, keys := lineageHistoryArgs(f)
	var n int
	var last sql.NullTime
	err := database.QueryRowContext(ctx, `
		SELECT count(*), max(e.ended_at)
		FROM lineage_events e
		LEFT JOIN saved_queries sq ON sq.id = e.saved_query_id
		WHERE `+lineageHistoryNodeFilterSQL+`
		  AND `+lineageSkippedRunSQL, workspaceID, userID, pipelineID, modelID, keys).Scan(&n, &last)
	if err != nil {
		return 0, nil, err
	}
	if !last.Valid {
		return n, nil, nil
	}
	return n, &last.Time, nil
}

// loadLineageHistory reads a node's runs, newest first, up to limit, and how many runs
// the node has in all: the window count is taken over every matching row before the
// limit cuts them. A skipped model run is read only when withSkipped: it is not a run
// that ran, and a model skipped on every tick would fill the window with them.
func loadLineageHistory(ctx context.Context, database *sql.DB, workspaceID, userID string, f lineageHistoryFilter, limit int, withSkipped bool) ([]lineageHistoryRun, int, error) {
	pipelineID, modelID, keys := lineageHistoryArgs(f)
	rows, err := database.QueryContext(ctx, `
		SELECT e.id::text, e.job_kind, COALESCE(e.saved_query_id::text, e.pipeline_id::text),
		       COALESCE(sq.name, p.name, ''), e.run_ref, e.event_type, e.started_at, e.ended_at,
		       e.inputs, e.outputs, count(*) OVER()
		FROM lineage_events e
		LEFT JOIN saved_queries sq ON sq.id = e.saved_query_id
		LEFT JOIN pipelines p ON p.id = e.pipeline_id
		WHERE `+lineageHistoryNodeFilterSQL+`
		  AND ($6 OR NOT `+lineageSkippedRunSQL+`)
		ORDER BY e.ended_at DESC, e.id DESC
		LIMIT $7`, workspaceID, userID, pipelineID, modelID, keys, withSkipped, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	runs := []lineageHistoryRun{}
	total := 0
	for rows.Next() {
		var r lineageHistoryRun
		var started sql.NullTime
		var inputs, outputs []byte
		if err := rows.Scan(&r.ID, &r.JobKind, &r.JobID, &r.JobName, &r.RunRef, &r.EventType,
			&started, &r.EndedAt, &inputs, &outputs, &total); err != nil {
			return nil, 0, err
		}
		if started.Valid {
			r.StartedAt = &started.Time
		}
		r.Inputs, r.Outputs = []lineageDataset{}, []lineageDataset{}
		// A skipped model run read and wrote nothing; the events recorded before the
		// recorder knew it still carry the SQL's tables (migration 154 clears them).
		if lineageEventNamesTables(r.JobKind, r.EventType) {
			if err := json.Unmarshal(inputs, &r.Inputs); err != nil {
				return nil, 0, err
			}
			if err := json.Unmarshal(outputs, &r.Outputs); err != nil {
				return nil, 0, err
			}
		}
		runs = append(runs, r)
	}
	return runs, total, rows.Err()
}

// loadModelSQLVersions reads the SQL versions of a model that ran, oldest first, with
// the columns read for each.
func loadModelSQLVersions(ctx context.Context, database *sql.DB, workspaceID, modelID string) ([]modelSQLVersion, error) {
	rows, err := database.QueryContext(ctx, `
		WITH versions AS (
		    SELECT sql_hash, MIN(ended_at) AS first_run
		    FROM lineage_events
		    WHERE saved_query_id = $1::uuid AND workspace_id = $2::uuid AND sql_hash <> ''
		    GROUP BY sql_hash
		)
		SELECT v.first_run, COALESCE(m.status, ''), COALESCE(m.result, '{}'::jsonb)
		FROM versions v
		LEFT JOIN model_column_lineage m ON m.saved_query_id = $1::uuid AND m.sql_hash = v.sql_hash
		ORDER BY v.first_run, v.sql_hash`, modelID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var versions []modelSQLVersion
	for rows.Next() {
		var v modelSQLVersion
		var status string
		var result []byte
		if err := rows.Scan(&v.FirstRun, &status, &result); err != nil {
			return nil, err
		}
		if status == columnLineageStatusOK || status == columnLineageStatusPartial {
			var saved savedColumnLineage
			if json.Unmarshal(result, &saved) == nil && len(saved.Columns) > 0 {
				v.Known, v.Columns = true, saved.Columns
			}
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}
