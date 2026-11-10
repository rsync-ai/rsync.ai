package handlers

// Generator-trust P4a: a regenerated connector reaches the pipelines that use it.
//
// A regeneration writes a NEW version by default, and every connection that has run
// is pinned to the version it ran (pinConnectorVersion), so the new version reached
// nobody. Two ways out, both the user's choice:
//
//   - overwrite: rewrite the version latest.json names in place. tool-generator stops
//     its running container, so the next run starts it on the new code. Refused while a
//     run uses that version: stopping the container would cut the run off mid-way.
//   - pinned connections: after a save, the Generate page lists this workspace's
//     connections pinned to another version and moves the ones the user picks with
//     MoveConnectionsToConnectorVersion.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/services"
	"api-gateway/internal/validators"
)

// connectorTypeVariants are the spellings a connection's connector_type can carry
// for a connector id: as given and with - → _ (the legacy underscore form).
func connectorTypeVariants(connectorID string) (string, string) {
	id := strings.ToLower(strings.TrimSpace(connectorID))
	return id, strings.ReplaceAll(id, "-", "_")
}

// currentConnectorVersion is the version latest.json names for a connector, or ""
// when the connector is not on disk yet (an overwrite then has nothing to rewrite).
func currentConnectorVersion(connectorID string) string {
	for _, dir := range findConnectorDirs(connectorID) {
		if v := readCurrentVersionFromLatestJSON(dir); v != "" {
			return v
		}
	}
	return ""
}

// runsOnConnectorVersionSQL lists the open runs of pipelines whose source or
// destination connection runs connectorID at version: pinned to it ($3, with or
// without the "v"), or floating (empty or 'latest', which resolves to latest.json's
// current version, the one an overwrite rewrites). e.id <> e.pipeline_id drops the
// synthetic CDC audit rows (see admin.go).
const runsOnConnectorVersionSQL = `
	SELECT e.id::text, p.id::text, COALESCE(p.name, ''), COALESCE(p.workspace_id::text, '')
	FROM executions e
	JOIN pipelines p ON p.id = e.pipeline_id
	JOIN connections c ON c.id IN (p.source_connection_id, p.destination_connection_id)
	WHERE e.status IN ('running', 'pending')
	  AND e.id <> e.pipeline_id
	  AND lower(c.connector_type) IN ($1, $2)
	  AND (lower(btrim(c.connector_version)) IN ('', 'latest')
	       OR ltrim(lower(btrim(c.connector_version)), 'v') = ltrim(lower($3), 'v'))
	ORDER BY e.start_time DESC
	LIMIT 50`

type connectorRunInUse struct {
	PipelineID   string `json:"pipeline_id"`
	PipelineName string `json:"pipeline_name"`
}

// connectorRunsInUse returns the open runs on connectorID@version: the caller
// workspace's by pipeline, and a count of the other workspaces' (the catalog is
// shared by every workspace, so their runs use the same container). A run whose
// workflow has already ended is not counted (a row Temporal left open would
// otherwise block every overwrite); a workflow that cannot be described counts.
func connectorRunsInUse(ctx context.Context, database *sql.DB, workspaceID, connectorID, version string) ([]connectorRunInUse, int, error) {
	id, underscored := connectorTypeVariants(connectorID)
	rows, err := database.QueryContext(ctx, runsOnConnectorVersionSQL, id, underscored, version)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	type openRun struct{ execID, pipelineID, pipelineName, workspaceID string }
	var open []openRun
	seen := map[string]bool{}
	for rows.Next() {
		var r openRun
		if err := rows.Scan(&r.execID, &r.pipelineID, &r.pipelineName, &r.workspaceID); err != nil {
			return nil, 0, err
		}
		// A pipeline whose source and destination both use the connector joins twice.
		if !seen[r.execID] {
			seen[r.execID] = true
			open = append(open, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var mine []connectorRunInUse
	minePipelines := map[string]bool{}
	others := 0
	for _, r := range open {
		if ended, _ := describeRunWorkflow(ctx, r.execID); ended {
			continue
		}
		if r.workspaceID != workspaceID {
			others++
			continue
		}
		if !minePipelines[r.pipelineID] {
			minePipelines[r.pipelineID] = true
			mine = append(mine, connectorRunInUse{PipelineID: r.pipelineID, PipelineName: r.pipelineName})
		}
	}
	return mine, others, nil
}

// connectorInUseMessage says which runs block an overwrite and what to do.
func connectorInUseMessage(connectorID, version string, mine []connectorRunInUse, others int) string {
	var parts []string
	if len(mine) > 0 {
		names := make([]string, 0, len(mine))
		for _, r := range mine {
			names = append(names, firstNonEmpty(r.PipelineName, r.PipelineID))
		}
		parts = append(parts, "pipeline(s) "+strings.Join(names, ", ")+" in this workspace")
	}
	if others > 0 {
		parts = append(parts, fmt.Sprintf("%d run(s) in other workspaces", others))
	}
	return fmt.Sprintf(
		"%s %s is in use by %s. Overwriting it restarts its container and would cut those runs off. "+
			"Wait for them to finish (or stop them), then try again, or generate a new version instead.",
		connectorID, version, strings.Join(parts, " and "))
}

// refuseOverwriteWhileInUse writes the refusal and returns true when an overwrite of
// connectorID's current version must not go ahead. It fails closed: a database it
// cannot ask refuses the overwrite rather than risk cutting off a run.
func refuseOverwriteWhileInUse(c *gin.Context, connectorID string) bool {
	version := currentConnectorVersion(connectorID)
	if version == "" {
		return false // nothing on disk to rewrite; the generator writes the first version
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false, "status": "failed", "connector_name": connectorID,
			"code":          "connector_in_use_unknown",
			"error":         "Could not check whether a run uses this connector, so it was not overwritten. Try again shortly.",
			"error_message": "Could not check whether a run uses this connector, so it was not overwritten. Try again shortly.",
		})
		return true
	}
	mine, others, err := connectorRunsInUse(c.Request.Context(), database, activeWorkspaceID(c), connectorID, version)
	if err != nil {
		log.WithError(err).Warnf("connector overwrite: could not check the runs on %s %s", connectorID, version)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false, "status": "failed", "connector_name": connectorID,
			"code":          "connector_in_use_unknown",
			"error":         "Could not check whether a run uses this connector, so it was not overwritten. Try again shortly.",
			"error_message": "Could not check whether a run uses this connector, so it was not overwritten. Try again shortly.",
		})
		return true
	}
	if len(mine) == 0 && others == 0 {
		return false
	}
	msg := connectorInUseMessage(connectorID, version, mine, others)
	if mine == nil {
		mine = []connectorRunInUse{}
	}
	c.JSON(http.StatusConflict, gin.H{
		"success":               false,
		"status":                "failed",
		"connector_name":        connectorID,
		"version":               version,
		"code":                  "connector_in_use",
		"error":                 msg,
		"error_message":         msg,
		"pipelines":             mine,
		"other_workspaces_runs": others,
	})
	return true
}

// pinnedConnectionsSQL lists the workspace's connections of a connector that are
// pinned to a version other than $4. Floating ones (empty or 'latest') follow
// latest.json and already get the new version.
const pinnedConnectionsSQL = `
	SELECT id::text, COALESCE(name, ''), connector_version
	FROM connections
	WHERE workspace_id = $1
	  AND lower(connector_type) IN ($2, $3)
	  AND lower(btrim(connector_version)) NOT IN ('', 'latest')
	  AND ltrim(lower(btrim(connector_version)), 'v') <> ltrim(lower($4), 'v')
	ORDER BY name, id`

type pinnedConnection struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ConnectorVersion string `json:"connector_version"`
}

// connectionsPinnedElsewhere returns the workspace's connections of connectorID that
// a save of version did not reach.
func connectionsPinnedElsewhere(ctx context.Context, database *sql.DB, workspaceID, connectorID, version string) ([]pinnedConnection, error) {
	id, underscored := connectorTypeVariants(connectorID)
	rows, err := database.QueryContext(ctx, pinnedConnectionsSQL, workspaceID, id, underscored, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pinnedConnection{}
	for rows.Next() {
		var p pinnedConnection
		if err := rows.Scan(&p.ID, &p.Name, &p.ConnectorVersion); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// addPinnedConnections puts pinned_connections on a successful save's payload so the
// Generate page can offer to move them. Best effort: a database error leaves the
// field out and the page shows no prompt.
func addPinnedConnections(c *gin.Context, connectorID string, payload map[string]interface{}) {
	version, _ := payload["version"].(string)
	version = strings.TrimSpace(version)
	database := db.GetDB()
	if version == "" || database == nil {
		return
	}
	pinned, err := connectionsPinnedElsewhere(c.Request.Context(), database, activeWorkspaceID(c), connectorID, version)
	if err != nil {
		log.WithError(err).Warnf("connector generation: could not list the connections pinned to another version of %s", connectorID)
		return
	}
	payload["pinned_connections"] = pinned
}

// MoveConnectionsToConnectorVersionRequest is the body of
// POST /connectors/:name/connections/version.
type MoveConnectionsToConnectorVersionRequest struct {
	Version       string   `json:"version"`
	ConnectionIDs []string `json:"connection_ids"`
}

const maxConnectionsPerVersionMove = 200

// MoveConnectionsToConnectorVersion pins the given connections of the active
// workspace to one version of connector :name, all or none. It is the bulk form of
// a version change in UpdateConnection: the same member gate, version checks, audit
// action and breaking-change warning.
func MoveConnectionsToConnectorVersion(c *gin.Context) {
	if _, ok := requireWorkspaceRole(c, security.WSMember); !ok {
		return
	}
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	connectorID := strings.TrimSpace(c.Param("name"))
	if !isKebabCaseConnectorID(connectorID) && !isKebabCaseConnectorID(strings.ReplaceAll(connectorID, "_", "-")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid connector name"})
		return
	}
	var req MoveConnectionsToConnectorVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}
	version := strings.TrimSpace(req.Version)
	ids := uniqueNonEmpty(req.ConnectionIDs)
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connection_ids is required"})
		return
	}
	if len(ids) > maxConnectionsPerVersionMove {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("At most %d connections per request", maxConnectionsPerVersionMove)})
		return
	}
	if err := validators.ValidateVersionFormat(version); err != nil || services.IsFloatingVersion(version) {
		// A concrete version only: this moves pinned connections to a version.
		c.JSON(http.StatusBadRequest, gin.H{"error": "version must be a concrete version such as v1.0.1"})
		return
	}
	validator := validators.NewConnectorVersionValidator(GetMCPConnectorsPath())
	if err := validator.ValidateVersionExists(connectorID, version); err != nil {
		availableVersions, _ := validator.GetAvailableVersions(connectorID)
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error(), "available_versions": availableVersions})
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	moved, err := moveConnectionsToVersion(c.Request.Context(), database, ws, connectorID, version, ids)
	if errors.Is(err, errSomeConnectionsNotFound) {
		// One answer for "not there", "another workspace's" and "another connector's",
		// so the response never tells which ids exist elsewhere.
		c.JSON(http.StatusNotFound, gin.H{"error": "One or more connections were not found in this workspace for this connector. Nothing was changed."})
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "move_failed", "Could not change the connections' version. Nothing was changed.", err)
		return
	}

	breaking := false
	for _, m := range moved {
		logAudit(c, "upgrade_connector_version", "connection", m.ID, map[string]interface{}{
			"old_connector_version": m.ConnectorVersion,
			"new_connector_version": version,
			"bulk":                  true,
		})
		if !breaking && !services.IsFloatingVersion(m.ConnectorVersion) {
			if has, changes, _ := validator.CheckBreakingChanges(connectorID, m.ConnectorVersion, version); has && len(changes) > 0 {
				breaking = true
			}
		}
	}
	resp := gin.H{
		"success":           true,
		"connector_version": version,
		"connections":       moved,
		// Same caveat UpdateConnection gives: a connection's pipelines run the version
		// stored on it from their next manual run or CDC restart.
		"message": fmt.Sprintf("%d connection(s) now use %s from their pipelines' next manual run or CDC restart; scheduled runs keep the version recorded at their last manual run, and runs in progress finish on their old version.", len(moved), version),
	}
	if breaking {
		resp["warning"] = fmt.Sprintf("Version %s contains breaking changes for at least one of these connections. Set a connection's version back to roll it back.", version)
	}
	c.JSON(http.StatusOK, resp)
}

var errSomeConnectionsNotFound = errors.New("some connections not found")

// idListSep joins connection ids into one string_to_array argument: this module runs
// on pgx and has no pq.Array (see pii.go). An id holding it splits into more ids than
// were asked for, which fails the all-or-none count check.
const idListSep = "\x1f"

// moveConnectionsToVersion pins ids to version in one transaction and returns each
// connection with the version it had before. Every id must be a connection of this
// workspace and connector, or nothing changes.
func moveConnectionsToVersion(ctx context.Context, database *sql.DB, workspaceID, connectorID, version string, ids []string) ([]pinnedConnection, error) {
	id, underscored := connectorTypeVariants(connectorID)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	rows, err := tx.QueryContext(ctx, `
		SELECT id::text, COALESCE(name, ''), connector_version
		FROM connections
		WHERE id::text = ANY (string_to_array($1, $2)) AND workspace_id = $3 AND lower(connector_type) IN ($4, $5)
		ORDER BY name, id
		FOR UPDATE`, strings.Join(ids, idListSep), idListSep, workspaceID, id, underscored)
	if err != nil {
		return nil, err
	}
	var before []pinnedConnection
	for rows.Next() {
		var p pinnedConnection
		if err := rows.Scan(&p.ID, &p.Name, &p.ConnectorVersion); err != nil {
			rows.Close()
			return nil, err
		}
		before = append(before, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(before) != len(ids) {
		return nil, errSomeConnectionsNotFound
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE connections SET connector_version = $1, updated_at = NOW()
		WHERE id::text = ANY (string_to_array($2, $3)) AND workspace_id = $4`,
		version, strings.Join(ids, idListSep), idListSep, workspaceID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != int64(len(ids)) {
		if err == nil {
			err = errSomeConnectionsNotFound
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return before, nil
}

func uniqueNonEmpty(vals []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
