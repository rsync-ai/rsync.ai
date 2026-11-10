package handlers

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"api-gateway/internal/db"
	"api-gateway/internal/validators"

	"github.com/rsync-ai/backend-orchestrator/pkg/namespacemodel"
	log "github.com/sirupsen/logrus"
)

// A document connection that names a database is pinned to it: the connector
// lists and reads only that one. A pipeline writing to the connection lands its
// rows in its OWN destination database (pipelineNamespaceExpr), so without this
// the data a pipeline delivered could not be browsed from the connection it was
// delivered to (KI-EXPLORER-PIPELINE-DEST-DB-UNREACHABLE). Explorer reads now
// also reach the databases this workspace's pipelines write to on the
// connection, and nothing else.
//
// The reach is expressed in the connection's own Scope contract, not a new one:
// a copy of the config that names no database and includes exactly the pinned
// database plus the pipelines' ones. The connector (discover_schema,
// _find_database) and the orchestrator (connectionScope) both enforce that
// scope, so no connector changes.

// maxExplorerPipelineDatabases keeps the widened scope inside the Scope
// contract's 100-pattern limit, the pinned database included.
const maxExplorerPipelineDatabases = 99

// configDatabaseKeys are the keys a connector reads its database from, in the
// order it reads them (connector _database_name, orchestrator configuredDatabase).
var configDatabaseKeys = []string{"database", "db_name", "db"}

// pinnedDocumentDatabase returns the database a connection whose tables live in
// databases is pinned to, or "" when it is server-level or its tables do not
// live in databases.
func pinnedDocumentDatabase(connectorType string, config map[string]interface{}) string {
	if namespacemodel.For(connectorType).TableNamespace != "database" {
		return ""
	}
	for _, k := range configDatabaseKeys {
		if v, ok := config[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// usableAsScopePattern reports whether a database name can be listed in the
// Scope as itself: a valid database name (the find validator's rule) with no
// glob or pattern separator that would widen or split the scope.
func usableAsScopePattern(name string) bool {
	return name != "" &&
		len(name) <= validators.DocumentFindMaxDatabaseBytes &&
		!strings.ContainsAny(name, "/\\. \"$\x00*,")
}

// pipelineDatabasesOnConnection returns, sorted, the databases this workspace's
// pipelines write to on connectionID, other than pinned. A pipeline with no
// namespace of its own (blank or "default") writes to the pinned database.
func pipelineDatabasesOnConnection(ctx context.Context, database *sql.DB, workspaceID, connectionID, pinned string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT DISTINCT `+pipelineNamespaceExpr+`
		FROM pipelines p
		WHERE p.workspace_id::text = $1
		  AND p.destination_connection_id::text = $2
	`, workspaceID, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var ns string
		if err := rows.Scan(&ns); err != nil {
			return nil, err
		}
		ns = strings.TrimSpace(ns)
		if strings.EqualFold(ns, "default") || strings.EqualFold(ns, pinned) || !usableAsScopePattern(ns) || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	if len(out) > maxExplorerPipelineDatabases {
		log.Warnf("[Explorer] connection %s: %d pipeline databases, browsing the first %d", connectionID, len(out), maxExplorerPipelineDatabases)
		out = out[:maxExplorerPipelineDatabases]
	}
	return out, nil
}

// configReachingDatabases returns a copy of config that names no database and
// whose Scope includes exactly pinned and extra.
func configReachingDatabases(config map[string]interface{}, pinned string, extra []string) map[string]interface{} {
	out := make(map[string]interface{}, len(config)+2)
	for k, v := range config {
		out[k] = v
	}
	for _, k := range configDatabaseKeys {
		if _, ok := out[k]; ok {
			out[k] = ""
		}
	}
	out["namespace_filter_mode"] = "include"
	out["namespace_filter_patterns"] = strings.Join(append([]string{pinned}, extra...), ",")
	return out
}

// explorerWidenedConfig returns the config an Explorer read uses to also reach
// the connection's pipeline databases, or nil when there is nothing to add: a
// server-level connection (its Scope is the user's), no such database, or a
// lookup that failed (the read stays pinned, as before).
func explorerWidenedConfig(ctx context.Context, workspaceID, connectionID, connectorType string, config map[string]interface{}) (map[string]interface{}, []string) {
	pinned := pinnedDocumentDatabase(connectorType, config)
	if pinned == "" || workspaceID == "" || !usableAsScopePattern(pinned) {
		return nil, nil
	}
	database := db.GetDB()
	if database == nil {
		return nil, nil
	}
	extra, err := pipelineDatabasesOnConnection(ctx, database, workspaceID, connectionID, pinned)
	if err != nil {
		if !requesterGone(ctx) {
			log.Warnf("[Explorer] connection %s: could not list its pipeline databases, browsing %q only: %v", connectionID, pinned, err)
		}
		return nil, nil
	}
	if len(extra) == 0 {
		return nil, nil
	}
	return configReachingDatabases(config, pinned, extra), extra
}

// findConfigForDatabase returns the config a find of wanted runs with. The
// pinned database (or none named) reads as before; a database this workspace's
// pipelines write to on the connection is reached by widening; any other stays
// pinned, so the connector refuses it as outside the connection's scope.
func findConfigForDatabase(ctx context.Context, workspaceID, connectionID, connectorType string, config map[string]interface{}, wanted string) map[string]interface{} {
	pinned := pinnedDocumentDatabase(connectorType, config)
	if pinned == "" || wanted == "" || wanted == pinned {
		return config
	}
	widened, extra := explorerWidenedConfig(ctx, workspaceID, connectionID, connectorType, config)
	for _, e := range extra {
		if e == wanted {
			return widened
		}
	}
	return config
}
