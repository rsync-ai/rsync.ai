package handlers

import (
	"os"
	"strings"

	"api-gateway/internal/safehttp"

	"github.com/rsync-ai/shared/sqlexec"
)

// A customer's statements run in shared/go/sqlexec, through its one dispatcher
// (sqlexec.For). The gateway calls it in process until query-service runs it
// (query-service-design.md § 9, Q1).

// databricksParam is one named parameter of a Databricks statement (chart_sql.go).
type databricksParam = sqlexec.DatabricksParam

// explorerEngine is the engine for a connection, reaching a delegated connector through
// the orchestrator and a Databricks workspace through the SSRF-guarded client.
func explorerEngine(connectorType string, config map[string]interface{}) (sqlexec.Engine, error) {
	return sqlexec.For(connectorType, sqlexec.Config{
		Settings:     config,
		Orchestrator: explorerOrchestrator(),
		HTTPClient:   safehttp.NewClient,
	})
}

// explorerOrchestrator is where a delegated statement goes: ORCHESTRATOR_URL, signed as
// an internal service.
func explorerOrchestrator() sqlexec.Orchestrator {
	url := strings.TrimRight(os.Getenv("ORCHESTRATOR_URL"), "/")
	if url == "" {
		url = "http://orchestrator:8080"
	}
	return sqlexec.Orchestrator{URL: url, Authorize: setInternalServiceSecret}
}
