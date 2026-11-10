package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/telemetry"

	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	"github.com/rsync-ai/shared/crypto"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// connectorBuildFailedMarker starts tool-generator's answer for a connector image that
// failed to build; the answer carries the end of the build log. A failed build is not
// a wait (the same files fail the same way), so a save answers it with its cause, never
// as "still being set up". Lockstep with backend-orchestrator
// internal/mcp.ConnectorBuildFailedMarker, tool-generator deployment/routes.py
// BUILD_FAILED_PREFIX and the frontend's CONNECTOR_BUILD_FAILED_MARKER
// (src/lib/errors/connector-deploying.ts).
const connectorBuildFailedMarker = "connector image failed to build"

// isConnectorBuildFailedMessage reports whether a connection-test error is a failed
// connector image build.
func isConnectorBuildFailedMessage(s string) bool {
	return strings.Contains(s, connectorBuildFailedMarker)
}

// scrubTestError scrubs a connection-test error before it is stored or answered. A
// failed image build is kept whole: its log is the connector's own code and
// requirements, never connection data, and its last lines name the cause
// ("No module named 'acme'"), which the scrub would mask.
func scrubTestError(s string) string {
	if isConnectorBuildFailedMessage(s) {
		return s
	}
	return llmscrub.Scrub(s)
}

// respondConnectorBuildFailedOnSave answers a create/update whose pre-save test hit a
// connector whose image failed to build. Nothing was saved, and a retry fails the same
// way until the connector is fixed, so it is a 422 (not the retryable 503 of a
// connector still being set up), with the end of the build log.
func respondConnectorBuildFailedOnSave(c *gin.Context, traceID, testErr string) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"error":      "connector_build_failed",
		"status":     "connector_build_failed",
		"message":    "The connector image failed to build, so nothing was saved. The end of the build log says why; fix or regenerate the connector, then save again.",
		"test_error": scrubTestError(testErr),
		"trace_id":   traceID,
	})
}

// Table check waits. A save answers within Cloudflare's 100s (a longer request is cut
// with a 524), and its pre-save test can take up to 90s, so a save waits for the check
// only until tableCheckSaveDeadline from the request's start; the "Check tables" button
// waits until tableCheckButtonDeadline. A check that is still running then is stored
// when it ends. tableCheckMinWait: with less time left, the answer does not wait.
// tableCheckRunTimeout bounds the check itself (orchestrator CheckTables: 30s listing
// + 60s of tables). Vars so a test can shorten them.
var (
	tableCheckSaveDeadline   = 80 * time.Second
	tableCheckButtonDeadline = 85 * time.Second
	tableCheckMinWait        = 5 * time.Second
	tableCheckRunTimeout     = 120 * time.Second
)

// connectorChecksTables reports whether a connection of this connector type gets the
// table check: every API connector (metadata.json "category": "api_saas"). A database
// lists its tables from its own catalog, so one readable table says the rest are too.
func connectorChecksTables(connectorType string) bool {
	return connectorMetadataString(connectorType, "category") == "api_saas"
}

// checkTablesViaOrchestrator asks the orchestrator to read one page of every table of
// a connection (POST /api/v1/agent/check-tables, executor CheckTables). The answer
// holds each table's status and error, never a row. The config carries
// connector_version, as for sample-rows. A var so tests can stub it.
var checkTablesViaOrchestrator = func(ctx context.Context, connectorType string, config map[string]interface{}, connectionID string) (map[string]interface{}, error) {
	orchestratorURL := strings.TrimRight(os.Getenv("AGENT_ORCHESTRATOR_URL"), "/")
	if orchestratorURL == "" {
		orchestratorURL = "http://orchestrator:8080"
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"connector_type": connectorType,
		"config":         config,
		"connection_id":  connectionID,
	})
	if err != nil {
		return nil, fmt.Errorf("could not build the table check request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		orchestratorURL+"/api/v1/agent/check-tables", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, fmt.Errorf("could not build the table check request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		httpReq.Header.Set(k, v)
	}
	setInternalServiceSecret(httpReq)

	resp, err := (&http.Client{Timeout: tableCheckRunTimeout}).Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("the table check could not reach the orchestrator: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("the table check answer could not be read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 502 carries the connector's own reason (it could not list its tables).
		var errEnv struct {
			Error   string `json:"error"`
			Details string `json:"details"`
		}
		_ = json.Unmarshal(body, &errEnv)
		msg := strings.TrimSpace(errEnv.Details)
		if msg == "" {
			msg = strings.TrimSpace(errEnv.Error)
		}
		if msg == "" {
			msg = fmt.Sprintf("the orchestrator answered HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("the table check answer could not be read: %w", err)
	}
	return result, nil
}

// tableCheckRecord is what connections.last_table_check holds (migration 142): the
// orchestrator's answer and when it was checked, or the reason the tables could not be
// listed.
func tableCheckRecord(result map[string]interface{}, err error, checkedAt time.Time) json.RawMessage {
	record := map[string]interface{}{}
	if err != nil {
		record["error"] = scrubTestError(err.Error())
	} else {
		for k, v := range result {
			record[k] = v
		}
	}
	record["checked_at"] = checkedAt.UTC().Format(time.RFC3339)
	out, _ := json.Marshal(record)
	return out
}

// tableChecks keeps the newest check of each connection, so a slow check of settings
// that have since changed cannot overwrite a newer answer (or the NULL an edit of the
// settings writes). In-process: the gateway runs as one process; a second replica
// would need the same guard in SQL.
var tableChecks = struct {
	sync.Mutex
	seq    uint64
	newest map[string]uint64
}{newest: map[string]uint64{}}

// supersedeTableChecks makes every running check of a connection drop its answer.
// Called before an edit of the connection's settings clears last_table_check.
func supersedeTableChecks(connectionID string) uint64 {
	tableChecks.Lock()
	defer tableChecks.Unlock()
	tableChecks.seq++
	tableChecks.newest[connectionID] = tableChecks.seq
	return tableChecks.seq
}

// storeTableCheck writes a check's record when it is still the connection's newest.
func storeTableCheck(database *sql.DB, connectionID, workspaceID string, seq uint64, record json.RawMessage) {
	tableChecks.Lock()
	defer tableChecks.Unlock()
	if tableChecks.newest[connectionID] != seq {
		log.WithField("connection_id", connectionID).Info("table check superseded by a newer one; its answer is dropped")
		return
	}
	delete(tableChecks.newest, connectionID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Scoped on workspace_id as every connection write (P2f).
	if _, err := database.ExecContext(ctx,
		`UPDATE connections SET last_table_check = $1 WHERE id = $2 AND workspace_id = $3`,
		[]byte(record), connectionID, workspaceID); err != nil {
		log.WithError(err).WithField("connection_id", connectionID).Error("failed to store the table check")
	}
}

// runTableCheck checks a stored connection's tables and stores the answer on it. The
// check runs apart from the request (it may outlive it: see tableCheckSaveDeadline);
// the caller waits for it at most `wait`. It returns the record and true when the check
// ended within the wait, else nil and false: the record is stored when the check ends.
// config is copied; it must carry connector_version when the connection has one.
func runTableCheck(c *gin.Context, database *sql.DB, connectionID, workspaceID, connectorType string, config map[string]interface{}, wait time.Duration) (json.RawMessage, bool) {
	cfg := make(map[string]interface{}, len(config))
	for k, v := range config {
		cfg[k] = v
	}
	seq := supersedeTableChecks(connectionID)
	// The caller's identity and trace stay on the context; its cancellation does not.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), tableCheckRunTimeout)
	done := make(chan json.RawMessage, 1)
	go func() {
		defer cancel()
		result, err := checkTablesViaOrchestrator(ctx, connectorType, cfg, connectionID)
		record := tableCheckRecord(result, err, time.Now())
		storeTableCheck(database, connectionID, workspaceID, seq, record)
		done <- record
	}()
	if wait < tableCheckMinWait {
		return nil, false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case record := <-done:
		return record, true
	case <-timer.C:
		return nil, false
	}
}

// tableCheckAnswer is the part of a save or "Check tables" answer that carries the
// check: the record, or that it is still running.
func tableCheckAnswer(record json.RawMessage, done bool) gin.H {
	if done {
		return gin.H{"last_table_check": record}
	}
	return gin.H{"table_check_pending": true}
}

// CheckConnectionTables reads one page of every table of a stored API connection and
// keeps no row (POST /connections/:id/check-tables, the "Check tables" button on the
// connection page). The answer is stored on the connection (last_table_check) until
// the next check. 200 with the record, or 202 when the check is still running at the
// wait's end; its record is stored when it ends.
func CheckConnectionTables(c *gin.Context) {
	start := time.Now()
	traceID := getTraceID(c)
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	// Reading a connection's tables uses its stored credentials: >= member, as Test.
	if _, ok := requireWorkspaceRole(c, security.WSMember); !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}
	wsID := activeWorkspaceID(c)
	connectionID := strings.TrimSpace(c.Param("id"))
	if wsID == "" || connectionID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	connectionID, err := resolveConnectionID(connectionID, wsID, database)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	var connectorType, configEncrypted string
	var connectorVersion sql.NullString
	err = database.QueryRow(`
		SELECT connector_type, config, connector_version
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, connectionID, wsID).Scan(&connectorType, &configEncrypted, &connectorVersion)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		respondError(c, http.StatusInternalServerError, "load_connection_failed", "Failed to load connection", err)
		return
	}
	if isInternalConnectorType(connectorType) && !canAccessInternalConnectorsFromUI(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "This connector is internal-only and cannot be used from the UI"})
		return
	}
	if !connectorChecksTables(connectorType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "table_check_not_supported",
			"message": "Only API connections have a table check.",
		})
		return
	}

	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		logConnectionAccess(c, connectionID, "check_tables", false, "decrypt_failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decrypt config"})
		return
	}
	logConnectionAccess(c, connectionID, "check_tables", true, "")
	config := map[string]interface{}{}
	_ = json.Unmarshal([]byte(configJSON), &config)
	if config == nil {
		config = map[string]interface{}{}
	}
	// The connector instance the connection's pipelines run (as TestConnection).
	if connectorVersion.Valid && strings.TrimSpace(connectorVersion.String) != "" {
		if _, has := config["connector_version"]; !has {
			config["connector_version"] = connectorVersion.String
		}
	}
	// A stored OAuth token may have been refreshed since the save (as TestConnection).
	if tokenID, _ := config["oauth_token_id"].(string); strings.TrimSpace(tokenID) != "" {
		delete(config, "access_token")
		delete(config, "refresh_token")
	}
	_ = enrichConfigWithOAuthToken(database, config, userID)

	log.WithFields(log.Fields{
		"trace_id":       traceID,
		"connection_id":  connectionID,
		"connector_type": connectorType,
	}).Info("CheckConnectionTables")
	record, done := runTableCheck(c, database, connectionID, wsID, connectorType, config, tableCheckButtonDeadline-time.Since(start))
	status := http.StatusOK
	if !done {
		status = http.StatusAccepted
	}
	c.JSON(status, tableCheckAnswer(record, done))
}
