package handlers

import (
	"database/sql"
	"encoding/json"
	"sync"

	"api-gateway/internal/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// getTraceID extracts trace_id from Gin context (set by telemetry middleware)
func getTraceID(c *gin.Context) string {
	if traceID, exists := c.Get("trace_id"); exists {
		if id, ok := traceID.(string); ok {
			return id
		}
	}
	// Fallback to header
	return c.GetHeader("X-Trace-ID")
}

var auditDBNilOnce sync.Once

// logAudit writes a row to audit_logs with trace_id; best effort (errors are logged, not returned).
func logAudit(c *gin.Context, action, resourceType, resourceID string, details interface{}) {
	database := db.GetDB()
	if database == nil {
		auditDBNilOnce.Do(func() {
			log.WithFields(log.Fields{
				"component": "audit_logger",
			}).Warn("database is nil; audit events will be dropped")
		})
		return
	}

	traceID := getTraceID(c)

	// resource_id is a uuid column, and some audited resources are not uuid
	// rows: the notification-channel settings are the row "1", a channel test
	// names "slack". Binding those made Postgres reject the insert and the
	// audit row was lost (KI-AUDIT-ROW-LOST-NON-UUID-RESOURCE-ID), so they write
	// a NULL resource_id and keep their name in details.resource_ref.
	resourceUUID, resourceRef := auditResourceID(resourceID)

	// Add trace_id (and resource_ref) to details if it's a map. gin.H is a named
	// map type, so a plain map[string]interface{} assertion misses it.
	detailsMap, isMap := details.(map[string]interface{})
	if h, ok := details.(gin.H); ok {
		detailsMap, isMap = map[string]interface{}(h), true
	}
	if details == nil && resourceRef != "" {
		detailsMap, isMap = map[string]interface{}{}, true
	}
	if isMap {
		detailsMap["trace_id"] = traceID
		if resourceRef != "" {
			detailsMap["resource_ref"] = resourceRef
		}
		details = detailsMap
	}

	var detailsJSON []byte
	if details != nil {
		if b, err := json.Marshal(details); err == nil {
			detailsJSON = b
		} else {
			log.WithError(err).WithFields(log.Fields{
				"component":      "audit_logger",
				"trace_id":       traceID,
				"action":         action,
				"resource_type":  resourceType,
				"resource_id":    resourceID,
				"marshal_target": "details",
			}).Warn("failed to marshal audit details (ignored)")
		}
	}

	ip := c.ClientIP()
	userID := c.GetString("user_id")
	// The workspace the action occurred in (set by WorkspaceContextMiddleware).
	// Empty on auth-only routes (login/register/verify/logout) that run before
	// any workspace context exists → stored as NULL.
	workspaceID := c.GetString(ctxWorkspaceID)

	// Handle empty strings as SQL NULL for UUID columns
	var userIDParam, resourceIDParam, workspaceIDParam interface{}
	if userID == "" {
		userIDParam = sql.NullString{}
	} else {
		userIDParam = userID
	}
	if resourceUUID == "" {
		resourceIDParam = sql.NullString{}
	} else {
		resourceIDParam = resourceUUID
	}
	if workspaceID == "" {
		workspaceIDParam = sql.NullString{}
	} else {
		workspaceIDParam = workspaceID
	}

	// details is jsonb; never insert an empty string (invalid json).
	var detailsParam interface{}
	if len(detailsJSON) == 0 {
		detailsParam = nil
	} else {
		detailsParam = string(detailsJSON)
	}

	var err error
	if actorKind := c.GetString(ctxActorKind); actorKind != "" {
		// An agent token acted (AgentTokenMiddleware), or the in-app assistant
		// (MarkAssistantCall, LogAssistantToolCall): say so, and which token if
		// any, so a person can tell their own clicks from their agent's calls.
		// A person's rows keep the 7-column insert and leave both NULL.
		var tokenIDParam interface{} = sql.NullString{}
		if tokenID := c.GetString(ctxTokenID); tokenID != "" {
			tokenIDParam = tokenID
		}
		_, err = database.Exec(`
		INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details, ip_address, workspace_id, actor_kind, token_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, userIDParam, action, resourceType, resourceIDParam, detailsParam, ip, workspaceIDParam, actorKind, tokenIDParam)
	} else {
		_, err = database.Exec(`
		INSERT INTO audit_logs (user_id, action, resource_type, resource_id, details, ip_address, workspace_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, userIDParam, action, resourceType, resourceIDParam, detailsParam, ip, workspaceIDParam)
	}

	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"component":     "audit_logger",
			"trace_id":      traceID,
			"action":        action,
			"resource_type": resourceType,
			"resource_id":   resourceID,
			"user_id":       userID,
			"ip":            ip,
		}).Warn("audit insert failed")
	} else {
		log.WithFields(log.Fields{
			"component":     "audit_logger",
			"trace_id":      traceID,
			"action":        action,
			"resource_type": resourceType,
			"resource_id":   resourceID,
			"user_id":       userID,
			"ip":            ip,
		}).Info("audit logged")
	}
}

// auditResourceID splits a resource id into what audit_logs.resource_id can
// hold — a canonical uuid, or "" for NULL — and a ref for details when the id
// is not a uuid. The canonical form is bound because uuid.Parse also accepts
// spellings Postgres rejects ("urn:uuid:…").
func auditResourceID(resourceID string) (resourceUUID, resourceRef string) {
	if resourceID == "" {
		return "", ""
	}
	u, err := uuid.Parse(resourceID)
	if err != nil {
		return "", resourceID
	}
	return u.String(), ""
}

// logConnectionAccess logs credential access to connection_access_logs table
// This tracks when connection configs (containing credentials) are accessed
func logConnectionAccess(c *gin.Context, connectionID, accessType string, success bool, errorMsg string) {
	database := db.GetDB()
	if database == nil {
		auditDBNilOnce.Do(func() {
			log.WithFields(log.Fields{
				"component": "audit_logger",
			}).Warn("database is nil; audit/connection access events will be dropped")
		})
		return
	}

	accessedBy := c.GetString("user_id")
	if accessedBy == "" {
		accessedBy = "system"
	}

	traceID := getTraceID(c)

	_, err := database.Exec(`
		INSERT INTO connection_access_logs (connection_id, accessed_by, access_type, success, error_message)
		VALUES ($1, $2, $3, $4, $5)
	`, connectionID, accessedBy, accessType, success, errorMsg)

	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"component":      "audit_logger",
			"trace_id":       traceID,
			"connection_id":  connectionID,
			"accessed_by":    accessedBy,
			"access_type":    accessType,
			"success":        success,
			"error_message":  errorMsg,
			"event_category": "connection_access",
		}).Warn("connection access insert failed")
	} else {
		log.WithFields(log.Fields{
			"component":      "audit_logger",
			"trace_id":       traceID,
			"connection_id":  connectionID,
			"accessed_by":    accessedBy,
			"access_type":    accessType,
			"success":        success,
			"event_category": "connection_access",
		}).Info("connection access logged")
	}
}
