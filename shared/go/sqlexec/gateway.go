package sqlexec

import (
	"context"
	"database/sql"
	"time"

	"github.com/rsync-ai/shared/sqlexec/stmt"
)

// What the gateway still runs itself until a later step moves it (query-service-design.md
// § 9): a table sample (Q4), Test checks and model runs (Q3). Each reaches the helper it
// shared with the engines through these, so there is one copy of each.

// BeginReadOnly opens the read-only transaction a read runs in (readonly.go).
func BeginReadOnly(ctx context.Context, conn *sql.Conn, driver string, d time.Duration) (*sql.Tx, error) {
	return beginReadOnly(ctx, conn, driver, d)
}

// TimeUntilDeadline is what is left of ctx's deadline, or fallback when it has none.
func TimeUntilDeadline(ctx context.Context, fallback time.Duration) time.Duration {
	return timeUntilDeadline(ctx, fallback)
}

// PostgresDSN, MySQLDSN and SQLServerDSN build a connection's DSN as the engines do.
func PostgresDSN(config map[string]interface{}) string  { return postgresExplorerDSN(config) }
func MySQLDSN(config map[string]interface{}) string     { return mysqlExplorerDSN(config) }
func SQLServerDSN(config map[string]interface{}) string { return sqlServerExplorerDSN(config) }

// ResolvePostgresSSLMode and ResolveMySQLTLSMode pick a connection's TLS mode as the
// engines do: required unless the host is local (hostguard.go).
func ResolvePostgresSSLMode(config map[string]interface{}, host string) string {
	return resolvePostgresSSLMode(config, host)
}

func ResolveMySQLTLSMode(config map[string]interface{}, host string) string {
	return resolveMySQLTLSMode(config, host)
}

// ReportsRowsAffected answers whether a statement class has an affected-row count worth
// showing (write.go).
func ReportsRowsAffected(class stmt.StatementClass) bool { return reportsRowsAffected(class) }

// QuotePGIdent quotes a PostgreSQL identifier.
func QuotePGIdent(s string) string { return quotePGIdent(s) }

// DatabricksStatementResponse is the Statement Execution API's reply.
type DatabricksStatementResponse = databricksStatementResponse

// ConfigString is the first non-blank string value among a config's keys, trimmed.
func ConfigString(config map[string]interface{}, keys ...string) string {
	return getStringConfig(config, keys...)
}

// NormalizeDatabricksHost strips a workspace host's scheme and trailing slash.
func NormalizeDatabricksHost(raw string) string { return normalizeDatabricksHost(raw) }

// WarehouseIDFromHTTPPath reads the warehouse id out of a connection's http_path.
func WarehouseIDFromHTTPPath(httpPath string) string {
	return parseWarehouseIDFromHTTPPath(httpPath)
}
