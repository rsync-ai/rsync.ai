package main

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rsync-ai/backend-orchestrator/internal/agents/executor"
)

// explorerFindStatus maps a document-mode connector error_code (find, aggregate,
// materialize) to the HTTP status its /agent/explorer-* route returns. A refused
// request (operator_not_allowed, invalid_filter, invalid_cursor, ...) is the caller's
// to fix: 400. A model target that exists and was not created by the model is 409. A
// server-side timeout is 504. Anything that means the connector or the database could
// not do the work (unreachable, bad config, a server error) is 502.
func explorerFindStatus(code string) int {
	switch code {
	case "query_timeout":
		return http.StatusGatewayTimeout
	case "connection_failed", "query_failed", "invalid_config":
		return http.StatusBadGateway
	case "target_exists":
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// explorerDocumentError writes a document-mode tool's failure: its error_code, the
// path of the refused part of the request, and the connector's message (which never
// echoes a filter or pipeline value).
func explorerDocumentError(c *gin.Context, operation string, err error) {
	code, path := "query_failed", ""
	if fe, ok := err.(*executor.ExplorerFindError); ok {
		code, path = fe.Code, fe.Path
	}
	c.JSON(explorerFindStatus(code), gin.H{
		"error":      operation + " failed",
		"error_code": code,
		"path":       path,
		"details":    err.Error(),
	})
}
