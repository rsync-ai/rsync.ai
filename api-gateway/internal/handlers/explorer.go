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
	"time"

	"api-gateway/internal/cache"
	"api-gateway/internal/db"
	"api-gateway/internal/security"
	"api-gateway/internal/telemetry"
	"api-gateway/internal/validators"

	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/sqlexec"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/microsoft/go-mssqldb"
	_ "github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

// Global explorer cache instance
var explorerCache *cache.ExplorerCache

// SetExplorerCache sets the global explorer cache instance
func SetExplorerCache(c *cache.ExplorerCache) {
	explorerCache = c
}

// =============================================================================
// SQL GENERATION (NL → SQL)
// =============================================================================

// SQLGenerateRequest represents a natural language to SQL request
type SQLGenerateRequest struct {
	Question string `json:"question" binding:"required"`
	// ConnectionID is optional; when present the dialect is derived from the
	// connection's connector_type instead of trusting the client / defaulting.
	ConnectionID string `json:"connection_id"`
	Dialect      string `json:"dialect"` // postgresql, mysql, etc. (default: postgresql)
	Schema       string `json:"schema"`  // Database schema context (tables + columns)
	Tables       string `json:"tables"`  // Comma-separated table names to focus on
	// New typed schema (preferred when present)
	TablesTyped []cache.ExplorerTableIndex `json:"tables_typed,omitempty"`
	ForeignKeys []map[string]interface{}   `json:"foreign_keys,omitempty"` // filled in Phase 2/3; keep flexible for now
	MaxTokens   int                        `json:"max_tokens"`
	Temperature float64                    `json:"temperature"`
}

// SQLGenerateResponse represents the SQL generation response
type SQLGenerateResponse struct {
	SQL         string   `json:"sql"`
	Explanation string   `json:"explanation,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
	Tables      []string `json:"tables,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
}

// SQLDialect is the SQL dialect of a connection's connector_type: the
// capability table's, so the dialect the SQL is written in is the one the query
// runs in. The text2sql service is told it, and Ask rsync.ai's list_connections
// names it. Returns "" for types with no SQL dialect (document stores, SaaS
// sources), in which case GenerateSQL keeps the client-provided/default dialect. It used to be a map of its own with no
// BigQuery case, so a BigQuery connection was sent "postgresql"
// (TestEveryDialectMapAgrees).
func SQLDialect(connectorType string) string {
	ec := ResolveExplorerCapability(connectorType)
	if ec.QueryLanguage != langSQL {
		return ""
	}
	return ec.Dialect
}

// connectionSQLDialect is the SQL dialect of a connection in the caller's
// active workspace, or "" when there is no connection id, no workspace, no
// database, no such connection, or a type with no SQL dialect. Callers keep
// the client-sent dialect on "".
func connectionSQLDialect(c *gin.Context, connectionID string) string {
	if connectionID == "" {
		return ""
	}
	wsID := activeWorkspaceID(c)
	database := db.GetDB()
	if wsID == "" || database == nil {
		return ""
	}
	var connectorType string
	if err := database.QueryRow(
		`SELECT connector_type FROM connections WHERE id = $1 AND workspace_id = $2`,
		connectionID, wsID,
	).Scan(&connectorType); err != nil {
		return ""
	}
	return SQLDialect(connectorType)
}

// GenerateSQL handles POST /api/v1/sql/generate
// Proxies to internal Text2SQL endpoint or uses LLM service
func GenerateSQL(c *gin.Context) {
	var req SQLGenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Query-limit enforcement (Ship 3 Phase 2). Refuse generation when the ACTIVE
	// workspace is over its monthly NL→SQL allowance — BEFORE building/sending the
	// text2sql request, so a blocked workspace pays no LLM cost. Fail-open: empty
	// workspace / nil DB / query error all fall through (checkWorkspaceQueryOK →
	// resolvePlanQuota treats them as unlimited). Direct SQL is never gated here.
	if allowed, errBody := checkWorkspaceQueryOK(c.Request.Context(), db.GetDB(), activeWorkspaceID(c)); !allowed {
		c.JSON(http.StatusPaymentRequired, errBody)
		return
	}

	// When a connection_id is supplied, derive the dialect authoritatively from
	// the connection's type. The Explorer UI already sends the correct dialect,
	// but any caller that omits or mis-sends it would otherwise get SQL in the
	// wrong dialect (the old code blindly defaulted to postgresql). Best-effort:
	// if auth/connection lookup is unavailable we fall through to the
	// client-provided dialect, so this never breaks an existing caller.
	if req.ConnectionID != "" {
		if wsID := activeWorkspaceID(c); wsID != "" {
			if database := db.GetDB(); database != nil {
				var connectorType string
				if err := database.QueryRow(
					`SELECT connector_type FROM connections WHERE id = $1 AND workspace_id = $2`,
					req.ConnectionID, wsID,
				).Scan(&connectorType); err == nil {
					if ResolveExplorerCapability(connectorType).QueryLanguage == langDocument {
						c.JSON(http.StatusBadRequest, gin.H{
							"error":      "SQL generation is not available for document connections",
							"error_code": "document_connection",
						})
						return
					}
					if d := SQLDialect(connectorType); d != "" {
						req.Dialect = d
					}
				}
			}
		}
	}

	// Default dialect to postgresql
	if req.Dialect == "" {
		req.Dialect = "postgresql"
	}

	// Without schema context the generator guesses table/column names and can
	// emit SQL that fails at execution (e.g. `active` vs `is_active`). Surface a
	// warning so the failure is visible rather than silently wrong.
	var schemaWarnings []string
	if strings.TrimSpace(req.Schema) == "" && len(req.TablesTyped) == 0 {
		schemaWarnings = append(schemaWarnings,
			"No schema context provided — generated SQL may reference incorrect table or column names. Pass `schema` or `tables_typed` for accurate results.")
	}

	// Get internal Text2SQL endpoint URL
	text2sqlURL := os.Getenv("TEXT2SQL_ENDPOINT")
	toLLMService := text2sqlURL == ""
	if toLLMService {
		// Fallback to LLM service if no dedicated Text2SQL endpoint
		text2sqlURL = os.Getenv("LLM_SERVICE_URL")
		if text2sqlURL == "" {
			text2sqlURL = "http://llm-service:5000"
		}
		text2sqlURL = text2sqlURL + "/api/v1/sql/generate"
	}

	// Build request for Text2SQL endpoint
	text2sqlReq := map[string]interface{}{
		"question": req.Question,
		"dialect":  req.Dialect,
		"schema":   req.Schema,
	}
	if req.Tables != "" {
		text2sqlReq["tables"] = req.Tables
	}
	if len(req.TablesTyped) > 0 {
		text2sqlReq["tables_typed"] = req.TablesTyped
	}
	if len(req.ForeignKeys) > 0 {
		text2sqlReq["foreign_keys"] = req.ForeignKeys
	}
	if req.MaxTokens > 0 {
		text2sqlReq["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		text2sqlReq["temperature"] = req.Temperature
	}
	// Ask rsync.ai's generate_sql writes with the model the question is answered
	// with (AssistantSQL.Write sets it in-process). Only llm-service is sent it,
	// with the internal secret it checks before taking a key; a dedicated
	// TEXT2SQL_ENDPOINT is sent none of it and writes with its own model.
	ask, _ := c.Get(ctxAskModel)
	askModel, isAsk := ask.(AskModel)
	isAsk = isAsk && askModel.Entry != "" && toLLMService
	if isAsk {
		text2sqlReq["entry"] = askModel.Entry
		if askModel.Model != "" {
			text2sqlReq["model"] = askModel.Model
		}
		if askModel.Key != "" {
			text2sqlReq["api_key"] = string(askModel.Key)
		}
	}
	// llm-service writes a usage row per model call and charges it to this
	// user's quota, as /v1/completion does. A dedicated endpoint is not sent it.
	if userID := c.GetString("user_id"); userID != "" && toLLMService {
		text2sqlReq["user_id"] = userID
	}

	reqBody, _ := json.Marshal(text2sqlReq)

	// Create request with timeout (offline Text2SQL can be slow for complex multi-table queries)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 120*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", text2sqlURL, bytes.NewBuffer(reqBody))
	if err != nil {
		log.Errorf("[GenerateSQL] Failed to create request: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if isAsk {
		if s := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_SECRET")); s != "" {
			httpReq.Header.Set("X-Internal-Secret", s)
		}
	}
	stampLLMWorkspace(httpReq)

	// Propagate trace headers
	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		httpReq.Header.Set(k, v)
	}

	// Execute request
	client := &http.Client{Timeout: 130 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		if answerRequesterGone(c, "GenerateSQL", err) {
			return
		}
		log.Errorf("[GenerateSQL] Text2SQL request failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "SQL generation service unavailable",
			"hint":  "Ensure TEXT2SQL_ENDPOINT or LLM_SERVICE_URL is configured",
		})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if relayLLMNotConfigured(c, resp.StatusCode, body) {
		return
	}
	if resp.StatusCode != http.StatusOK {
		respondLLMUpstreamFailed(c, "GenerateSQL", resp.StatusCode, body, "SQL generation failed")
		return
	}

	// Parse response
	var sqlResp SQLGenerateResponse
	if err := json.Unmarshal(body, &sqlResp); err != nil {
		// Try to extract SQL from raw response
		var rawResp map[string]interface{}
		if json.Unmarshal(body, &rawResp) == nil {
			if sql, ok := rawResp["sql"].(string); ok {
				sqlResp.SQL = sql
			}
			if sql, ok := rawResp["query"].(string); ok && sqlResp.SQL == "" {
				sqlResp.SQL = sql
			}
			if expl, ok := rawResp["explanation"].(string); ok {
				sqlResp.Explanation = expl
			}
		}
	}

	if sqlResp.SQL == "" {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":        "No SQL generated",
			"raw_response": string(body),
		})
		return
	}

	// Prepend any schema-context warning so callers (and the UI) can surface it.
	if len(schemaWarnings) > 0 {
		sqlResp.Warnings = append(schemaWarnings, sqlResp.Warnings...)
	}

	// Meter one NL→SQL query against the workspace's monthly allowance (the metered
	// "queries/month" dimension the pricing page sells). Counted once per SUCCESSFUL
	// generation — reached only after the non-empty-SQL guard above, so failed/empty
	// generations burn nothing — and exactly once regardless of the llm-service's
	// internal retry/repair fan-out, because the gateway sees one request/response.
	// Direct SQL (/explorer/query) has no LLM call and is never metered. Best-effort +
	// fail-open: a metering hiccup must never fail a generation the user already got.
	recordNLQueryUsage(c, req.ConnectionID)

	c.JSON(http.StatusOK, sqlResp)
}

// recordNLQueryUsage increments the active workspace's monthly NL→SQL query counter
// (display meter of record for the query dimension) and appends a ledger row. Keyed
// on the ACTIVE (billable) workspace. Fail-open on every hop: no workspace / nil DB /
// query error → count nothing, never error. The INSERT + charge run in one round-trip
// (data-modifying CTE) so the ledger row and the counter stay consistent.
func recordNLQueryUsage(c *gin.Context, connectionID string) {
	wsID := activeWorkspaceID(c)
	if wsID == "" {
		return // fail-open: never attribute an unscoped query to the wrong tenant
	}
	database := db.GetDB()
	if database == nil {
		return
	}
	// user_id / connection_id are UUID columns — pass NULL (not "") when absent.
	var uid, cid interface{}
	if userID := c.GetString("user_id"); userID != "" {
		uid = userID
	}
	if connectionID != "" {
		cid = connectionID
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if _, err := database.ExecContext(ctx,
		`WITH ins AS (
		   INSERT INTO nl_query_usage_events (workspace_id, user_id, connection_id)
		   VALUES ($1, $2, $3)
		   RETURNING workspace_id
		 )
		 SELECT charge_workspace_queries(workspace_id) FROM ins`,
		wsID, uid, cid,
	); err != nil {
		log.WithError(err).Warnf("nl_query_usage: meter failed for workspace %s; ignoring", wsID)
	}
}

// =============================================================================
// QUERY EXECUTION (Safe SELECT-only)
// =============================================================================

// ExplorerQueryRequest represents a query execution request
type ExplorerQueryRequest struct {
	ConnectionID string `json:"connection_id" binding:"required"`
	SQL          string `json:"sql" binding:"required"`
	Limit        int    `json:"limit"` // Max rows to return (default: 100, max: 500)
}

// ExplorerQueryResponse is a statement's result. It lives with the engines that make it.
type ExplorerQueryResponse = sqlexec.Result

// ExecuteExplorerQuery handles POST /api/v1/explorer/query
// Executes SELECT-only queries against PostgreSQL connections
func ExecuteExplorerQuery(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req ExplorerQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Validate and sanitize limit
	if req.Limit <= 0 {
		req.Limit = 100
	}
	if req.Limit > 500 {
		req.Limit = 500
	}

	// Validate SQL and authorize the statement class against the caller's workspace
	// role. Reads (SELECT/WITH) stay open to any member; writes require admin, and
	// destructive statements (DROP/TRUNCATE) require owner. See
	// validators.ValidateExplorerStatement for the full matrix.
	callerRole := security.WorkspaceRole(c.GetString(ctxWorkspaceRole))
	validation := validators.ValidateExplorerStatement(req.SQL, callerRole)
	if !validation.Valid {
		// A role shortfall is a 403 (authenticated but not permitted); everything else
		// (bad SQL, blocked statement class, multiple statements) is a 400.
		status := http.StatusBadRequest
		hint := "Explorer supports SELECT reads for all members; INSERT/UPDATE/DELETE and DDL require the admin role, DROP/TRUNCATE and ALTER … DROP require owner."
		if validation.ErrorCode == validators.ErrCodeInsufficientRole {
			status = http.StatusForbidden
		}
		c.JSON(status, gin.H{
			"error":      validation.ErrorMessage,
			"error_code": validation.ErrorCode,
			"hint":       hint,
		})
		return
	}
	// Classify on the statement text (not just its verb) so this agrees with the class
	// ValidateExplorerStatement already authorized above — see ClassifyStatementSQL.
	stmtClass := validators.ClassifyStatementSQL(req.SQL)
	isWrite := validators.IsWriteClass(stmtClass)

	// Get connection details
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, req.ConnectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		log.Errorf("[ExecuteExplorerQuery] Failed to load connection: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	// Explorer support is gated by the capability resolver (single source of truth):
	// direct-driver SQL engines (PostgreSQL/Redshift, MySQL/MariaDB, Databricks, SQL
	// Server) + delegated warehouses (BigQuery, run through the connector's MCP export
	// tool via the orchestrator). Unsupported connectors 400 here.
	ecap := ResolveExplorerCapability(connectorType)
	if !ecap.Supported || ecap.QueryLanguage != langSQL {
		// A document connection (MongoDB) is Supported but browses through
		// /explorer/documents/find — it must never reach the SQL executors below.
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Unsupported connection type: %s", connectorType),
			"hint":  "Explorer currently supports PostgreSQL/Redshift, MySQL, SQL Server, Databricks, BigQuery, Snowflake, ClickHouse and Oracle",
		})
		return
	}

	// Decrypt config
	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		log.Errorf("[ExecuteExplorerQuery] Failed to decrypt config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decrypt connection config"})
		return
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse connection config"})
		return
	}

	// Execute the statement through the connection's engine (sqlexec.For): a direct
	// engine runs it here, a delegated one (BigQuery) through the connector's MCP tool.
	// A read scans at most req.Limit rows, masked; a write runs through the no-rows
	// "execute" path and reports an affected-row count, refused above the caller's role.
	var result *ExplorerQueryResponse
	eng, err := explorerEngine(connectorType, config)
	if err == nil {
		if isWrite {
			result, err = eng.Exec(c.Request.Context(), req.SQL, validators.MaxClassForRole(callerRole))
		} else {
			result, err = eng.Query(c.Request.Context(), req.SQL, nil, sqlexec.Limits{Rows: req.Limit, Mask: true})
		}
	}
	if err != nil {
		if answerRequesterGone(c, "ExecuteExplorerQuery", err) {
			return
		}
		log.Errorf("[ExecuteExplorerQuery] Query execution failed: %v", err)
		// Classify the error for better user feedback + an accurate HTTP status: a
		// user's SQL typo or stale schema reference is a client error (4xx), not a
		// 500. Genuine infra failures stay 5xx.
		errorType, suggestion := validators.ClassifyExecutionError(err)
		status := http.StatusInternalServerError
		switch errorType {
		case "syntax_error", "missing_table_or_column":
			status = http.StatusBadRequest
		case "permission_denied":
			status = http.StatusForbidden
		case "timeout":
			status = http.StatusGatewayTimeout
		case "network_or_unavailable":
			status = http.StatusBadGateway
		}
		c.JSON(status, gin.H{
			"error":      fmt.Sprintf("Query execution failed: %v", err),
			"error_type": errorType,
			"suggestion": suggestion,
		})
		return
	}

	// Echo the classified statement type so the UI can render a write outcome
	// ("N rows affected") instead of an (empty) result grid.
	if result != nil {
		result.StatementType = validation.StatementType
	}

	// Writes are attributable actions on customer data — record an audit entry.
	if isWrite {
		auditExplorerWrite(c, req.ConnectionID, connectorType, validation.StatementType, req.SQL, result)
	}

	c.JSON(http.StatusOK, result)
}

// auditExplorerWrite records an executed Explorer write to audit_logs for
// attributability (who ran which write, against which connection, and how many rows it
// changed). Best-effort — logAudit never fails the request. The raw statement is stored
// truncated so an admin can see exactly what ran; audit_logs is admin-only and is never
// sent to an LLM, so no scrubbing is required here.
func auditExplorerWrite(c *gin.Context, connectionID, connectorType, statementType, sqlText string, result *ExplorerQueryResponse) {
	details := map[string]interface{}{
		"connector_type": connectorType,
		"statement_type": statementType,
		"sql":            truncateForAudit(sqlText, 2000),
		"workspace_role": c.GetString(ctxWorkspaceRole),
	}
	if result != nil && result.RowsAffected != nil {
		details["rows_affected"] = *result.RowsAffected
	}
	logAudit(c, "explorer.write", "connection", connectionID, details)
}

// truncateForAudit bounds an audit string so a pathological statement can't bloat the
// audit_logs row. Appends an ellipsis marker when it cuts.
func truncateForAudit(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…(truncated)"
	}
	return s
}

// isInternalExplorerTable reports whether name is an rsync-internal bookkeeping
// or pipeline-staging table that must not surface as a Data Explorer query
// candidate (schema index, NL table resolution, HITL). Matching is
// case-insensitive and tolerates a leading schema qualifier
// (`public.flat_mysql_123` matches too). Mirrors the frontend predicate in
// frontend/src/lib/explorer/internalTables.ts.
func isInternalExplorerTable(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return false
	}
	// Strip a leading schema qualifier so `schema.flat_mysql_123` matches.
	if dot := strings.LastIndex(n, "."); dot >= 0 {
		n = n[dot+1:]
	}
	n = strings.ToLower(n)
	return strings.HasPrefix(n, "_rsync") ||
		strings.HasPrefix(n, "rsync_") ||
		strings.HasPrefix(n, "flat_mysql_") ||
		strings.HasPrefix(n, "flat_pg_") ||
		strings.HasPrefix(n, "flat_postgres_")
}

// GetRecommendedTablesForExplorer returns table recommendations for Explorer
func GetRecommendedTablesForExplorer(c *gin.Context) {
	connectionID, ok := requireUUIDParam(c, "id", "invalid_connection_id", "Invalid connection ID format")
	if !ok {
		return
	}
	_, ok = resolveUserID(c)
	if !ok {
		return
	}

	// NOTE: do NOT bind the JSON body here — GetRecommendedTables (the delegate
	// below) binds it itself, and binding twice drains the request body so the
	// delegate's bind fails with 400 "Invalid request body". That double-bind made
	// this endpoint always fail; we only verify ownership here, then delegate.

	// Delegate to existing GetRecommendedTables
	// This is a thin wrapper that can be enhanced later
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection exists and belongs to the active workspace
	var exists bool
	err := database.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM connections WHERE id = $1 AND workspace_id = $2)
	`, connectionID, activeWorkspaceID(c)).Scan(&exists)

	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	// Call GetRecommendedTables with the request
	GetRecommendedTables(c)
}

// =============================================================================
// SCHEMA INDEX ENDPOINTS
// =============================================================================

// GetSchemaIndexResponse represents the schema index response
type GetSchemaIndexResponse struct {
	ConnectionID    string                          `json:"connection_id"`
	SchemaHash      string                          `json:"schema_hash"`
	LastRefreshedAt time.Time                       `json:"last_refreshed_at"`
	TableCount      int                             `json:"table_count"`
	Tables          []cache.ExplorerTableIndex      `json:"tables"`
	ForeignKeys     []cache.ExplorerForeignKeyIndex `json:"foreign_keys,omitempty"`
	Cached          bool                            `json:"cached"`
}

// GetSchemaIndex handles GET /api/v1/explorer/connections/:id/schema-index
// Returns a cached schema index or builds one if not cached
func GetSchemaIndex(c *gin.Context) {
	connectionID, ok := requireUUIDParam(c, "id", "invalid_connection_id", "Invalid connection ID format")
	if !ok {
		return
	}
	_, ok = resolveUserID(c)
	if !ok {
		return
	}

	forceRefresh := c.Query("refresh") == "true"
	// scope=server (MySQL only) browses every database on the server, not just
	// the connection's configured one. Cached under a distinct key so the
	// server-scope and database-scope indexes never overwrite each other.
	allDatabases := c.Query("scope") == "server"
	cacheID := schemaIndexCacheID(connectionID, allDatabases)

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection exists and belongs to the active workspace
	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, connectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		log.Errorf("[GetSchemaIndex] Failed to load connection: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	ctx := c.Request.Context()

	// Check cache first (unless force refresh)
	if !forceRefresh && explorerCache != nil {
		cached, err := explorerCache.GetSchemaIndex(ctx, cacheID)
		if err == nil && cached != nil {
			c.JSON(http.StatusOK, GetSchemaIndexResponse{
				ConnectionID:    connectionID,
				SchemaHash:      cached.SchemaHash,
				LastRefreshedAt: cached.LastRefreshedAt,
				TableCount:      cached.TableCount,
				Tables:          cached.Tables,
				ForeignKeys:     cached.ForeignKeys,
				Cached:          true,
			})
			return
		}
	}

	// Build schema index from metadata
	schemaIndex, err := buildSchemaIndex(ctx, connectionID, activeWorkspaceID(c), connectorType, configEncrypted, allDatabases)
	if err != nil {
		if answerRequesterGone(c, "GetSchemaIndex", err) {
			return
		}
		log.Errorf("[GetSchemaIndex] Failed to build schema index: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to build schema index: %v", err)})
		return
	}

	// Cache the result under the scope-aware key.
	if explorerCache != nil {
		cacheCopy := *schemaIndex
		cacheCopy.ConnectionID = cacheID
		if err := explorerCache.SetSchemaIndex(ctx, &cacheCopy); err != nil {
			log.Warnf("[GetSchemaIndex] Failed to cache schema index: %v", err)
		}
	}

	c.JSON(http.StatusOK, GetSchemaIndexResponse{
		ConnectionID:    connectionID,
		SchemaHash:      schemaIndex.SchemaHash,
		LastRefreshedAt: schemaIndex.LastRefreshedAt,
		TableCount:      schemaIndex.TableCount,
		Tables:          schemaIndex.Tables,
		ForeignKeys:     schemaIndex.ForeignKeys,
		Cached:          false,
	})
}

// schemaIndexCacheID namespaces the explorer schema-index cache by browse scope.
// Server-scope (all databases) and the default database-scope index must not
// share a key or one would clobber the other on write.
func schemaIndexCacheID(connectionID string, allDatabases bool) string {
	if allDatabases {
		return connectionID + ":server"
	}
	return connectionID
}

// RefreshSchemaIndex handles POST /api/v1/explorer/connections/:id/schema-index/refresh
// Forces a refresh of the schema index
func RefreshSchemaIndex(c *gin.Context) {
	connectionID, ok := requireUUIDParam(c, "id", "invalid_connection_id", "Invalid connection ID format")
	if !ok {
		return
	}
	_, ok = resolveUserID(c)
	if !ok {
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection exists and belongs to the active workspace
	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, connectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	allDatabases := c.Query("scope") == "server"
	cacheID := schemaIndexCacheID(connectionID, allDatabases)

	ctx := c.Request.Context()

	// Invalidate existing cache for this scope
	if explorerCache != nil {
		_ = explorerCache.InvalidateSchemaIndex(ctx, cacheID)
	}

	// Build fresh schema index
	schemaIndex, err := buildSchemaIndex(ctx, connectionID, activeWorkspaceID(c), connectorType, configEncrypted, allDatabases)
	if err != nil {
		if answerRequesterGone(c, "RefreshSchemaIndex", err) {
			return
		}
		log.Errorf("[RefreshSchemaIndex] Failed to build schema index: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to refresh schema index: %v", err)})
		return
	}

	// Cache the result under the scope-aware key.
	if explorerCache != nil {
		cacheCopy := *schemaIndex
		cacheCopy.ConnectionID = cacheID
		if err := explorerCache.SetSchemaIndex(ctx, &cacheCopy); err != nil {
			log.Warnf("[RefreshSchemaIndex] Failed to cache schema index: %v", err)
		}
	}

	c.JSON(http.StatusOK, GetSchemaIndexResponse{
		ConnectionID:    connectionID,
		SchemaHash:      schemaIndex.SchemaHash,
		LastRefreshedAt: schemaIndex.LastRefreshedAt,
		TableCount:      schemaIndex.TableCount,
		Tables:          schemaIndex.Tables,
		ForeignKeys:     schemaIndex.ForeignKeys,
		Cached:          false,
	})
}

// buildSchemaIndex builds a schema index from database metadata.
//
// allDatabases (MySQL/MariaDB only) controls server-vs-database scope: when
// true we blank the connection's configured `database` so the introspection
// query's `(? = ” OR TABLE_SCHEMA = ?)` clause returns every non-system
// database on the server, letting the Explorer browse cross-namespace tables
// from a single connection (Superset/DBeaver-style). Postgres ignores this —
// its namespaces are already schemas under one DB, all of which are returned.
func buildSchemaIndex(ctx context.Context, connectionID, workspaceID, connectorType, configEncrypted string, allDatabases bool) (*cache.ExplorerSchemaIndex, error) {
	if ResolveExplorerCapability(connectorType).QueryLanguage == langDocument {
		return buildDocumentSchemaIndex(ctx, connectionID, workspaceID, connectorType, configEncrypted)
	}
	if eng, err := explorerEngine(connectorType, nil); err != nil || sqlexec.IsDelegated(eng) {
		// A delegated warehouse's schema comes from its connector
		// (sqlexec.ErrNoSchemaIndex), through discover_schema.
		if err == nil && isDiscoveredWarehouse(connectorType) {
			return buildWarehouseSchemaIndex(ctx, connectionID, connectorType, configEncrypted)
		}
		// Non-SQL connectors (GraphQL/REST: shopify-admin-graphql, github,
		// linear, stripe, ...) have no introspectable relational schema.
		// Return an empty index rather than erroring so the Data Explorer
		// degrades gracefully (200 + no tables) instead of surfacing a 500.
		return &cache.ExplorerSchemaIndex{
			ConnectionID:    connectionID,
			SchemaHash:      "unsupported",
			LastRefreshedAt: time.Now(),
			TableCount:      0,
			Tables:          []cache.ExplorerTableIndex{},
			ForeignKeys:     []cache.ExplorerForeignKeyIndex{},
		}, nil
	}

	// Decrypt config
	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt config: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Get tables and columns from the DB. allDatabases widens a MySQL connection to
	// every non-system database on the server.
	eng, err := explorerEngine(connectorType, config)
	if err != nil {
		return nil, err
	}
	idx, err := eng.SchemaIndex(ctx, allDatabases)
	if err != nil {
		return nil, err
	}
	tables, foreignKeys := idx.Tables, idx.ForeignKeys

	// Drop rsync-internal bookkeeping / pipeline-staging tables (e.g.
	// flat_mysql_<ts>, _rsync_*) before caching so they never reach the
	// schema index, NL table resolution, or the HITL candidate list.
	if len(tables) > 0 {
		filtered := tables[:0]
		for _, t := range tables {
			if isInternalExplorerTable(t.Name) {
				continue
			}
			filtered = append(filtered, t)
		}
		tables = filtered
	}

	// Phase 3.5: infer relationships beyond explicit FK constraints (naming + type heuristics).
	inferred := cache.InferForeignKeysFromHeuristics(tables, foreignKeys)
	if len(inferred) > 0 {
		foreignKeys = append(foreignKeys, inferred...)
	}

	// Compute schema hash
	schemaHash := cache.ComputeSchemaHash(tables, foreignKeys)

	return &cache.ExplorerSchemaIndex{
		ConnectionID:    connectionID,
		SchemaHash:      schemaHash,
		LastRefreshedAt: time.Now(),
		TableCount:      len(tables),
		Tables:          tables,
		ForeignKeys:     foreignKeys,
	}, nil
}

// =============================================================================
// TABLE RETRIEVAL ENDPOINTS (for large schema handling)
// =============================================================================

// RetrieveTablesRequest represents a table retrieval request
type RetrieveTablesRequest struct {
	ConnectionID string `json:"connection_id" binding:"required"`
	Question     string `json:"question" binding:"required"`
	TopK         int    `json:"top_k"` // Default: 20
}

// RetrieveTablesResponse represents the retrieved tables
type RetrieveTablesResponse struct {
	Tables      []cache.ExplorerTableIndex `json:"tables"`
	SchemaHash  string                     `json:"schema_hash"`
	TotalTables int                        `json:"total_tables"`
}

// RetrieveTables handles POST /api/v1/explorer/tables/retrieve
// Retrieves top-K tables matching a natural language question
func RetrieveTables(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req RetrieveTablesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	if req.TopK <= 0 {
		req.TopK = 20
	}
	if req.TopK > 50 {
		req.TopK = 50
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection
	var exists bool
	err := database.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM connections WHERE id = $1 AND workspace_id = $2)
	`, req.ConnectionID, activeWorkspaceID(c)).Scan(&exists)

	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	ctx := c.Request.Context()

	// Get schema index (from cache or build)
	var schemaIndex *cache.ExplorerSchemaIndex
	if explorerCache != nil {
		schemaIndex, _ = explorerCache.GetSchemaIndex(ctx, req.ConnectionID)
	}

	if schemaIndex == nil {
		// Need to build it - get connection config
		var connectorType, configEncrypted string
		err := database.QueryRow(`
			SELECT connector_type, config
			FROM connections
			WHERE id = $1 AND workspace_id = $2
		`, req.ConnectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
			return
		}

		schemaIndex, err = buildSchemaIndex(ctx, req.ConnectionID, activeWorkspaceID(c), connectorType, configEncrypted, false)
		if err != nil {
			if answerRequesterGone(c, "RetrieveTables", err) {
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to build schema index: %v", err)})
			return
		}

		// Cache for next time
		if explorerCache != nil {
			_ = explorerCache.SetSchemaIndex(ctx, schemaIndex)
		}
	}

	// Extract search terms from question
	searchTerms := cache.ExtractSearchTerms(req.Question)

	// Retrieve top-K tables
	tables := cache.RetrieveTopTables(schemaIndex, searchTerms, req.TopK)

	c.JSON(http.StatusOK, RetrieveTablesResponse{
		Tables:      tables,
		SchemaHash:  schemaIndex.SchemaHash,
		TotalTables: schemaIndex.TableCount,
	})
}

// =============================================================================
// LLM-ORCHESTRATED ENDPOINTS (Table Link, Column Link, Next Steps)
// =============================================================================

// TableCandidate represents a candidate table with confidence
type TableCandidate struct {
	Table      string  `json:"table"`
	SchemaName string  `json:"schema_name"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// ResolveTablésRequest represents a table resolution request
type ResolveTablesRequest struct {
	ConnectionID   string `json:"connection_id" binding:"required"`
	Question       string `json:"question" binding:"required"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// ResolveTablesResponse represents the table resolution response
type ResolveTablesResponse struct {
	Candidates        []TableCandidate `json:"candidates"`
	ConfidenceOverall float64          `json:"confidence_overall"`
	NeedsHITL         bool             `json:"needs_hitl"`
	HITLReason        string           `json:"hitl_reason,omitempty"`
	SuggestedJoinKeys []string         `json:"suggested_join_keys,omitempty"`
	SchemaHash        string           `json:"schema_hash"`
}

// ResolveExplorerTables handles POST /api/v1/explorer/nl/resolve-tables
// Orchestrates LLM table linking with schema retrieval
func ResolveExplorerTables(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req ResolveTablesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection
	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, req.ConnectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	ctx := c.Request.Context()

	// Get schema index (with retrieval-based narrowing for large schemas)
	var schemaIndex *cache.ExplorerSchemaIndex
	if explorerCache != nil {
		schemaIndex, _ = explorerCache.GetSchemaIndex(ctx, req.ConnectionID)
	}

	if schemaIndex == nil {
		schemaIndex, err = buildSchemaIndex(ctx, req.ConnectionID, activeWorkspaceID(c), connectorType, configEncrypted, false)
		if err != nil {
			if answerRequesterGone(c, "ResolveExplorerTables", err) {
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Failed to build schema index: %v", err)})
			return
		}
		if explorerCache != nil {
			_ = explorerCache.SetSchemaIndex(ctx, schemaIndex)
		}
	}

	// For large schemas (500+ tables), use retrieval to narrow first
	tablesToSend := schemaIndex.Tables
	if len(tablesToSend) > 40 {
		searchTerms := cache.ExtractSearchTerms(req.Question)
		tablesToSend = cache.RetrieveTopTables(schemaIndex, searchTerms, 40)
	}

	// Call LLM service for table linking
	llmServiceURL := os.Getenv("LLM_SERVICE_URL")
	if llmServiceURL == "" {
		llmServiceURL = "http://llm-service:5000"
	}

	// Build request for LLM service
	tablesPayload := make([]map[string]interface{}, 0, len(tablesToSend))
	for _, t := range tablesToSend {
		cols := make([]map[string]interface{}, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, map[string]interface{}{
				"name":           c.Name,
				"type":           c.Type,
				"is_primary_key": c.IsPrimaryKey,
				"is_nullable":    c.IsNullable,
			})
		}
		tablesPayload = append(tablesPayload, map[string]interface{}{
			"name":      t.Name,
			"schema":    t.Schema,
			"row_count": t.RowCount,
			"columns":   cols,
		})
	}

	llmReq := map[string]interface{}{
		"question": req.Question,
		"tables":   tablesPayload,
	}
	if req.ConversationID != "" {
		llmReq["conversation_id"] = req.ConversationID
	}

	reqBody, _ := json.Marshal(llmReq)

	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(llmCtx, "POST", llmServiceURL+"/api/v1/explorer/nl/resolve-tables", bytes.NewBuffer(reqBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	stampLLMWorkspace(httpReq)

	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		httpReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 35 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		if answerRequesterGone(c, "ResolveExplorerTables", err) {
			return
		}
		log.Errorf("[ResolveExplorerTables] LLM request failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "LLM service unavailable"})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if relayLLMNotConfigured(c, resp.StatusCode, body) {
		return
	}
	if resp.StatusCode != http.StatusOK {
		respondLLMUpstreamFailed(c, "ResolveExplorerTables", resp.StatusCode, body, "Table resolution failed")
		return
	}

	var llmResp struct {
		Candidates        []TableCandidate `json:"candidates"`
		ConfidenceOverall float64          `json:"confidence_overall"`
		NeedsHITL         bool             `json:"needs_hitl"`
		HITLReason        string           `json:"hitl_reason"`
		SuggestedJoinKeys []string         `json:"suggested_join_keys"`
	}
	if err := json.Unmarshal(body, &llmResp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse LLM response"})
		return
	}

	c.JSON(http.StatusOK, ResolveTablesResponse{
		Candidates:        llmResp.Candidates,
		ConfidenceOverall: llmResp.ConfidenceOverall,
		NeedsHITL:         llmResp.NeedsHITL,
		HITLReason:        llmResp.HITLReason,
		SuggestedJoinKeys: llmResp.SuggestedJoinKeys,
		SchemaHash:        schemaIndex.SchemaHash,
	})
}

// ColumnMapping represents column selections for query parts
type ColumnMapping struct {
	SelectCols  []string `json:"select_cols"`
	WhereCols   []string `json:"where_cols"`
	GroupByCols []string `json:"group_by_cols"`
	OrderByCols []string `json:"order_by_cols"`
}

// JoinPlan represents a join between tables
type JoinPlan struct {
	JoinType   string `json:"join_type"`
	LeftTable  string `json:"left_table"`
	RightTable string `json:"right_table"`
	Condition  string `json:"condition"`
}

// ResolveColumnsRequest represents a column resolution request
type ResolveColumnsRequest struct {
	ConnectionID   string                     `json:"connection_id" binding:"required"`
	Question       string                     `json:"question" binding:"required"`
	SelectedTables []cache.ExplorerTableIndex `json:"selected_tables" binding:"required"`
	ConversationID string                     `json:"conversation_id,omitempty"`
}

// ResolveColumnsResponse represents the column resolution response
type ResolveColumnsResponse struct {
	Columns          ColumnMapping `json:"columns"`
	JoinPlan         []JoinPlan    `json:"join_plan"`
	Confidence       float64       `json:"confidence"`
	NeedsHITL        bool          `json:"needs_hitl"`
	HITLReason       string        `json:"hitl_reason,omitempty"`
	AmbiguousColumns []string      `json:"ambiguous_columns,omitempty"`
}

// ResolveExplorerColumns handles POST /api/v1/explorer/nl/resolve-columns
// Orchestrates LLM column linking
func ResolveExplorerColumns(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req ResolveColumnsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	// Verify connection
	var exists bool
	err := database.QueryRow(`
		SELECT EXISTS(SELECT 1 FROM connections WHERE id = $1 AND workspace_id = $2)
	`, req.ConnectionID, activeWorkspaceID(c)).Scan(&exists)

	if err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	ctx := c.Request.Context()

	// Call LLM service for column linking
	llmServiceURL := os.Getenv("LLM_SERVICE_URL")
	if llmServiceURL == "" {
		llmServiceURL = "http://llm-service:5000"
	}

	// Build request
	tablesPayload := make([]map[string]interface{}, 0, len(req.SelectedTables))
	for _, t := range req.SelectedTables {
		cols := make([]map[string]interface{}, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, map[string]interface{}{
				"name":           c.Name,
				"type":           c.Type,
				"is_primary_key": c.IsPrimaryKey,
				"is_nullable":    c.IsNullable,
			})
		}
		tablesPayload = append(tablesPayload, map[string]interface{}{
			"name":    t.Name,
			"schema":  t.Schema,
			"columns": cols,
		})
	}

	llmReq := map[string]interface{}{
		"question":        req.Question,
		"selected_tables": tablesPayload,
	}
	if req.ConversationID != "" {
		llmReq["conversation_id"] = req.ConversationID
	}

	reqBody, _ := json.Marshal(llmReq)

	llmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(llmCtx, "POST", llmServiceURL+"/api/v1/explorer/nl/resolve-columns", bytes.NewBuffer(reqBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	stampLLMWorkspace(httpReq)

	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		httpReq.Header.Set(k, v)
	}

	httpClient := &http.Client{Timeout: 35 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		if answerRequesterGone(c, "ResolveExplorerColumns", err) {
			return
		}
		log.Errorf("[ResolveExplorerColumns] LLM request failed: %v", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "LLM service unavailable"})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if relayLLMNotConfigured(c, resp.StatusCode, body) {
		return
	}
	if resp.StatusCode != http.StatusOK {
		respondLLMUpstreamFailed(c, "ResolveExplorerColumns", resp.StatusCode, body, "Column resolution failed")
		return
	}

	var llmResp struct {
		Columns struct {
			SelectCols  []string `json:"select_cols"`
			WhereCols   []string `json:"where_cols"`
			GroupByCols []string `json:"group_by_cols"`
			OrderByCols []string `json:"order_by_cols"`
		} `json:"columns"`
		JoinPlan         []JoinPlan `json:"join_plan"`
		Confidence       float64    `json:"confidence"`
		NeedsHITL        bool       `json:"needs_hitl"`
		HITLReason       string     `json:"hitl_reason"`
		AmbiguousColumns []string   `json:"ambiguous_columns"`
	}
	if err := json.Unmarshal(body, &llmResp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse LLM response"})
		return
	}

	c.JSON(http.StatusOK, ResolveColumnsResponse{
		Columns: ColumnMapping{
			SelectCols:  llmResp.Columns.SelectCols,
			WhereCols:   llmResp.Columns.WhereCols,
			GroupByCols: llmResp.Columns.GroupByCols,
			OrderByCols: llmResp.Columns.OrderByCols,
		},
		JoinPlan:         llmResp.JoinPlan,
		Confidence:       llmResp.Confidence,
		NeedsHITL:        llmResp.NeedsHITL,
		HITLReason:       llmResp.HITLReason,
		AmbiguousColumns: llmResp.AmbiguousColumns,
	})
}

// NextStepSuggestion represents a suggested action
type NextStepSuggestion struct {
	ActionType     string   `json:"action_type"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Confidence     float64  `json:"confidence"`
	RequiredInputs []string `json:"required_inputs"`
	CTA            string   `json:"cta"`
}

// GetNextStepsRequest represents a next steps request
type GetNextStepsRequest struct {
	ConnectionID string `json:"connection_id"`
	// Dialect of SQL. llm-service replaces the query's literals with
	// placeholders before the model sees it, and it can only find them with
	// the query's own dialect; without one it withholds the query instead.
	// The connection's type, when it can be looked up, wins over this.
	Dialect          string                 `json:"dialect"`
	Question         string                 `json:"question" binding:"required"`
	SQL              string                 `json:"sql" binding:"required"`
	ResultProfile    map[string]interface{} `json:"result_profile" binding:"required"`
	AvailableActions []string               `json:"available_actions"`
}

// GetNextStepsResponse represents the next steps response
type GetNextStepsResponse struct {
	Suggestions []NextStepSuggestion `json:"suggestions"`
}

// GetExplorerNextSteps handles POST /api/v1/explorer/nl/next-steps
// Gets LLM-suggested next actions based on query results
func GetExplorerNextSteps(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req GetNextStepsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	if len(req.AvailableActions) == 0 {
		req.AvailableActions = []string{"download_csv", "slack", "email"}
	}
	if d := connectionSQLDialect(c, req.ConnectionID); d != "" {
		req.Dialect = d
	}

	ctx := c.Request.Context()

	// Call LLM service
	llmServiceURL := os.Getenv("LLM_SERVICE_URL")
	if llmServiceURL == "" {
		llmServiceURL = "http://llm-service:5000"
	}

	llmReq := map[string]interface{}{
		"question":          req.Question,
		"sql":               req.SQL,
		"dialect":           req.Dialect,
		"result_profile":    req.ResultProfile,
		"available_actions": req.AvailableActions,
	}

	reqBody, _ := json.Marshal(llmReq)

	llmCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(llmCtx, "POST", llmServiceURL+"/api/v1/explorer/nl/next-steps", bytes.NewBuffer(reqBody))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create request"})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	stampLLMWorkspace(httpReq)

	for k, v := range telemetry.InjectTraceToHeaders(ctx) {
		httpReq.Header.Set(k, v)
	}

	httpClient := &http.Client{Timeout: 25 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		// DELIBERATE, and pinned by TestNextStepsKeepsDefaultSuggestionsWhenTheAnswerIsUnusable:
		// unlike the upstream-status branch below, this answers 200 with one
		// generic action. It is not a claim about the user's data and not an
		// LLM answer -- it is true of any result set. (The Explorer, which kept
		// only suggestion titles, stopped calling this in D2a; the endpoint has
		// no frontend caller now.)
		// A self-host with no llm-service reachable still gets a usable explorer.
		// Do not "fix" this into a 502 -- that was tried and reverted.
		if answerRequesterGone(c, "GetExplorerNextSteps", err) {
			return
		}
		log.Errorf("[GetExplorerNextSteps] LLM request failed: %v", err)
		c.JSON(http.StatusOK, GetNextStepsResponse{
			Suggestions: []NextStepSuggestion{
				{ActionType: "download_csv", Title: "Download CSV", Description: "Export results", Confidence: 0.7, RequiredInputs: []string{}, CTA: "Download"},
			},
		})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if relayLLMNotConfigured(c, resp.StatusCode, body) {
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// An error body decodes to zero suggestions, which the caller would
		// read as "nothing to suggest". Say it failed instead. The body is not
		// logged: a validation error can echo the result profile back.
		log.Errorf("[GetExplorerNextSteps] llm-service returned status %d", resp.StatusCode)
		msg := fmt.Sprintf("Could not get next-step suggestions: the explorer service answered with status %d. "+
			"Try again in a moment; if it keeps failing, check the llm-service logs.", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// The service rejected this request, so sending it again gives the same answer.
			msg = fmt.Sprintf("Could not get next-step suggestions: the explorer service rejected the request with status %d. "+
				"Trying again will not help; check the llm-service logs and that the gateway and llm-service run the same version.", resp.StatusCode)
		}
		c.JSON(http.StatusBadGateway, gin.H{
			"error":           msg,
			"upstream_status": resp.StatusCode,
		})
		return
	}

	var llmResp struct {
		Suggestions []NextStepSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(body, &llmResp); err != nil {
		// Same deliberate fallback as the transport branch above, same test pins it.
		// The one thing that WAS missing here: this branch used to return in total
		// silence, so a proxy page served in place of llm-service left no trace at
		// all. Log the parse error -- never the body, which can echo the result
		// profile back (the upstream-status branch above is pinned against that leak).
		log.Errorf("[GetExplorerNextSteps] could not parse llm-service response: %v", err)
		c.JSON(http.StatusOK, GetNextStepsResponse{
			Suggestions: []NextStepSuggestion{
				{ActionType: "download_csv", Title: "Download CSV", Description: "Export results", Confidence: 0.7, RequiredInputs: []string{}, CTA: "Download"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, GetNextStepsResponse{
		Suggestions: llmResp.Suggestions,
	})
}

// =============================================================================
// ACTION EXECUTION ENDPOINTS (CSV, Slack, Email)
// =============================================================================

// ExportCSVHandler handles GET /api/v1/explorer/export.csv
// Exports query results as CSV file
func ExportCSVHandler(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	connectionID := c.Query("connection_id")
	sqlQuery := c.Query("sql")

	if connectionID == "" || sqlQuery == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "connection_id and sql are required"})
		return
	}

	// Validate SQL
	validation := validators.ValidateExplorerSQL(sqlQuery)
	if !validation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": validation.ErrorMessage})
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, connectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	ecap := ResolveExplorerCapability(connectorType)
	if !ecap.Supported || ecap.QueryLanguage != langSQL {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported connection type for export"})
		return
	}

	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decrypt config"})
		return
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse config"})
		return
	}

	// Execute query with higher limit for export. PII redaction is
	// bypassed here (T2-7): the export handler's ownership check at
	// line 2687 ensures the caller IS the data owner — they should
	// see their own raw email / phone / SSN values in CSV downloads.
	// The interactive preview path (line 294-298) still redacts.
	var result *ExplorerQueryResponse
	eng, err := explorerEngine(connectorType, config)
	if err == nil {
		result, err = eng.Query(c.Request.Context(), sqlQuery, nil, sqlexec.Limits{Rows: 10000, Mask: false})
	}
	if err != nil {
		if answerRequesterGone(c, "ExportCSVHandler", err) {
			return
		}
		log.Errorf("explorer query failed conn=%s: %v", connectionID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query execution failed; check your SQL and try again"})
		return
	}

	// Build CSV
	var csvBuilder strings.Builder

	// Header row
	csvBuilder.WriteString(strings.Join(result.Columns, ","))
	csvBuilder.WriteString("\n")

	// Data rows
	for _, row := range result.Rows {
		var values []string
		for _, col := range result.Columns {
			val := row[col]
			valStr := ""
			if val != nil {
				valStr = fmt.Sprintf("%v", val)
				// Escape quotes and commas
				if strings.Contains(valStr, ",") || strings.Contains(valStr, "\"") || strings.Contains(valStr, "\n") {
					valStr = "\"" + strings.ReplaceAll(valStr, "\"", "\"\"") + "\""
				}
			}
			values = append(values, valStr)
		}
		csvBuilder.WriteString(strings.Join(values, ","))
		csvBuilder.WriteString("\n")
	}

	// Set headers for CSV download
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition", "attachment; filename=export.csv")
	c.String(http.StatusOK, csvBuilder.String())
}

// ExportQueryRequest is the POST body for /api/v1/explorer/export.
// Mirrors the shape the frontend already sends from the Download
// dropdown so we don't need to change client code.
type ExportQueryRequest struct {
	ConnectionID string `json:"connection_id" binding:"required"`
	SQL          string `json:"sql"          binding:"required"`
	Format       string `json:"format"` // csv | tsv | json (default: csv)
	Limit        int    `json:"limit"`  // server-clamped to 10000
}

// ExportQueryHandler handles POST /api/v1/explorer/export.
//
// D4 fix: the Download dropdown in Explorer was POSTing to this route
// with {connection_id, sql, format, limit:10000} but the backend only
// had GET /explorer/export.csv with query params — so every download
// returned 404 silently. This handler makes the advertised 10K-row
// multi-format export actually work.
//
// Format support:
//   - csv:  RFC4180-ish, quote+escape commas/quotes/newlines
//   - tsv:  tab-separated, newlines in values replaced with spaces
//   - json: array of objects (one per row)
//   - xlsx: returned as 400 (no Go xlsx dep wired up). The frontend
//     hides this option; this guard is for direct curl calls.
//
// Ownership: scoped to workspace-shared (connection_id, workspace_id) — same
// primitive as ExecuteExplorerQuery. PII redaction is INTENTIONALLY bypassed
// (same call-site rationale as the legacy GET handler).
func ExportQueryHandler(c *gin.Context) {
	_, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req ExportQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request: " + err.Error()})
		return
	}

	// Server-side limit clamp. Client sends 10000; we trust nothing.
	if req.Limit <= 0 {
		req.Limit = 10000
	}
	if req.Limit > 10000 {
		req.Limit = 10000
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "tsv" && format != "json" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Unsupported export format: %q", req.Format),
			"hint":  "Supported formats: csv, tsv, json",
		})
		return
	}

	// Validate SQL using the same AST-based validator as Run Query.
	validation := validators.ValidateExplorerSQL(req.SQL)
	if !validation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":      validation.ErrorMessage,
			"error_code": validation.ErrorCode,
			"hint":       "Explorer only supports read-only SELECT queries for safety",
		})
		return
	}

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not available"})
		return
	}

	var connectorType, configEncrypted string
	err := database.QueryRow(`
		SELECT connector_type, config
		FROM connections
		WHERE id = $1 AND workspace_id = $2
	`, req.ConnectionID, activeWorkspaceID(c)).Scan(&connectorType, &configEncrypted)

	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}
	if err != nil {
		log.Errorf("[ExportQueryHandler] Failed to load connection: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load connection"})
		return
	}

	ecap := ResolveExplorerCapability(connectorType)
	if !ecap.Supported || ecap.QueryLanguage != langSQL {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("Unsupported connection type for export: %s", connectorType),
			"hint":  "Export currently supports PostgreSQL/Redshift, MySQL, Databricks, SQL Server, and BigQuery",
		})
		return
	}

	configJSON, err := crypto.DecryptString(configEncrypted)
	if err != nil {
		log.Errorf("[ExportQueryHandler] Failed to decrypt config: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decrypt connection config"})
		return
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse connection config"})
		return
	}

	// PII redaction bypassed: the workspace-scoped (connection_id, workspace_id)
	// ownership check above guarantees the caller owns this data and should see
	// raw values in their CSV/TSV/JSON. Mirrors the GET handler's
	// rationale (T2-7).
	var result *ExplorerQueryResponse
	eng, err := explorerEngine(connectorType, config)
	if err == nil {
		result, err = eng.Query(c.Request.Context(), req.SQL, nil, sqlexec.Limits{Rows: req.Limit, Mask: false})
	}
	if err != nil {
		if answerRequesterGone(c, "ExportQueryHandler", err) {
			return
		}
		log.Errorf("[ExportQueryHandler] Query execution failed: %v", err)
		errorType, suggestion := validators.ClassifyExecutionError(err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      fmt.Sprintf("Query execution failed: %v", err),
			"error_type": errorType,
			"suggestion": suggestion,
		})
		return
	}

	switch format {
	case "csv":
		writeDelimited(c, result, ',', "text/csv; charset=utf-8", "export.csv")
	case "tsv":
		writeDelimited(c, result, '\t', "text/tab-separated-values; charset=utf-8", "export.tsv")
	case "json":
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Disposition", "attachment; filename=export.json")
		c.JSON(http.StatusOK, gin.H{
			"columns":   result.Columns,
			"rows":      result.Rows,
			"row_count": result.RowCount,
			"truncated": result.Truncated,
		})
	}
}

// writeDelimited streams CSV-ish output (comma or tab separated)
// using a strings.Builder. Quoting follows the CSV convention:
// if a value contains the separator, a double-quote, or a newline,
// the whole value is wrapped in double-quotes and any internal
// double-quotes are doubled. For TSV we additionally collapse
// embedded newlines / tabs to spaces so the row structure stays
// intact even when consumed by tools that don't honor quoting.
func writeDelimited(c *gin.Context, result *ExplorerQueryResponse, sep byte, contentType, filename string) {
	var b strings.Builder
	// Pre-size: headers + roughly 64 chars per row+column.
	b.Grow(len(result.Columns)*16 + len(result.Rows)*len(result.Columns)*64)

	needsQuoteCSV := func(s string) bool {
		return strings.ContainsAny(s, ",\"\n\r")
	}
	quote := func(s string) string {
		return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\""
	}

	// Header row.
	for i, col := range result.Columns {
		if i > 0 {
			b.WriteByte(sep)
		}
		if sep == ',' && needsQuoteCSV(col) {
			b.WriteString(quote(col))
		} else if sep == '\t' {
			b.WriteString(strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(col))
		} else {
			b.WriteString(col)
		}
	}
	b.WriteByte('\n')

	for _, row := range result.Rows {
		for i, col := range result.Columns {
			if i > 0 {
				b.WriteByte(sep)
			}
			val := row[col]
			if val == nil {
				continue
			}
			s := fmt.Sprintf("%v", val)
			if sep == ',' {
				if needsQuoteCSV(s) {
					b.WriteString(quote(s))
				} else {
					b.WriteString(s)
				}
			} else { // tab
				b.WriteString(strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s))
			}
		}
		b.WriteByte('\n')
	}

	c.Header("Content-Type", contentType)
	c.Header("Content-Disposition", "attachment; filename="+filename)
	c.String(http.StatusOK, b.String())
}
