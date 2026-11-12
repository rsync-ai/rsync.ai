package handlers

import "strings"

// ExplorerCapability describes how the Data Explorer should treat a connection of
// a given connector_type: whether it is queryable at all, in what SQL dialect, and how
// its schema is discovered.
//
// This is the single source of truth the connections API (supports_explorer /
// explorer_mode / sql_dialect) and the gateway's query/schema/export gates read. How a
// statement runs (a driver here, or the connector through the orchestrator) is
// sqlexec.For's, the one dispatcher; the two match connector types the same way, pinned
// by TestExplorerEngineMatchesCapability. Adding a new warehouse means a row here and
// a case there — never a strings.Contains allowlist scattered across the handlers.
type ExplorerCapability struct {
	// Dialect is the SQL dialect label (postgresql, mysql, tsql, databricks, bigquery,
	// clickhouse, snowflake, oracle).
	// Empty for non-SQL (document) engines. NL->SQL generation (text2sql) reads it
	// through SQLDialect, and modelDialect names the same engine for
	// a model's DDL (TestEveryDialectMapAgrees).
	Dialect string
	// QueryLanguage is the user-facing query surface: langSQL (SQL editor + NL->SQL)
	// or langDocument (read-only document browse through the connector's `find` tool,
	// POST /explorer/documents/find). Drives the frontend explorer_mode. Every SQL
	// path (query, export, NL->SQL) must require langSQL, not just Supported.
	QueryLanguage string
	// SchemaStrategy is how the schema panel is populated: schemaSQLIntrospection
	// (information_schema / SHOW TABLES via the direct driver) or schemaMCPDiscover
	// (the connector's discover_schema tool via the orchestrator).
	SchemaStrategy string
	// Supported gates whether the connection appears in the Explorer at all.
	Supported bool
	// SupportsMaterialization gates whether a saved query on this connection can be
	// turned into a model (materialization = table/statement) and scheduled.
	//
	// Deliberately NOT derived from the engine or QueryLanguage: a model needs an
	// engine that can build a table from a query — a SQL engine modelDialect() lists, or
	// a document store whose model is an aggregation pipeline written to a collection
	// (MongoDB, saved_query_documents.go). Kept in lockstep with modelSupported by
	// TestSupportsMaterializationMatchesModelDialect.
	SupportsMaterialization bool
}

// User-facing query languages (also surfaced to the frontend as explorer_mode).
const (
	langSQL      = "sql"
	langDocument = "document"
)

// Schema discovery strategies.
const (
	schemaSQLIntrospection = "sql_introspection"
	schemaMCPDiscover      = "mcp_discover_schema"
)

// applyExplorerCapability fills the computed Data Explorer fields on a Connection DTO
// from its connector_type. Kept beside the resolver so the API surface and the
// dispatch logic never drift.
func (c *Connection) applyExplorerCapability() {
	ec := ResolveExplorerCapability(c.ConnectorType)
	c.SupportsExplorer = ec.Supported
	c.ExplorerMode = ec.QueryLanguage
	c.SQLDialect = ec.Dialect
	c.SupportsMaterialization = ec.SupportsMaterialization
}

// ResolveExplorerCapability maps a connection's connector_type to its Explorer
// capability. It uses the same case-insensitive substring matching as the legacy
// inline gates it replaced (the per-handler isPostgres/isMySQL/isDatabricks/
// isSQLServer blocks), so the direct-driver engines resolve byte-for-byte to their
// earlier behavior — pinned by explorer_capability_test.go. mysql is matched before
// postgres so overlapping substrings resolve the way they always have.
func ResolveExplorerCapability(connectorType string) ExplorerCapability {
	ct := strings.ToLower(connectorType)
	switch {
	case strings.Contains(ct, "mysql") || strings.Contains(ct, "mariadb"):
		return ExplorerCapability{
			Dialect: "mysql", QueryLanguage: langSQL,
			SchemaStrategy: schemaSQLIntrospection, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "postgres") || strings.Contains(ct, "redshift"):
		// Redshift speaks the PostgreSQL wire protocol and reuses the Postgres driver.
		return ExplorerCapability{
			Dialect: "postgresql", QueryLanguage: langSQL,
			SchemaStrategy: schemaSQLIntrospection, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "sqlserver") || strings.Contains(ct, "mssql"):
		return ExplorerCapability{
			Dialect: "tsql", QueryLanguage: langSQL,
			SchemaStrategy: schemaSQLIntrospection, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "databricks"):
		// Databricks keeps its existing in-gateway HTTP Statement Execution path, and
		// a model run uses the same API (databricksModelRunner).
		return ExplorerCapability{
			Dialect: "databricks", QueryLanguage: langSQL,
			SchemaStrategy: schemaSQLIntrospection, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "bigquery"):
		// BigQuery runs SQL, but the gateway has no BigQuery driver — delegate the
		// query to the connector's MCP export tool via the orchestrator.
		// A model run sends its statements to the connector's execute tool the same way
		// (orchestratorModelRunner).
		return ExplorerCapability{
			Dialect: "bigquery", QueryLanguage: langSQL,
			SchemaStrategy: schemaMCPDiscover, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "clickhouse"):
		// ClickHouse speaks SQL but the gateway has no ClickHouse driver — delegate
		// like BigQuery: reads via the connector's MCP export tool, writes via its
		// execute tool, schema via discover_schema, all through the orchestrator.
		// text2sql already supports the "clickhouse" dialect, so NL->SQL is correct.
		return ExplorerCapability{
			Dialect: "clickhouse", QueryLanguage: langSQL,
			SchemaStrategy: schemaMCPDiscover, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "snowflake"):
		// Delegated like BigQuery: reads via the connector's export tool, writes and
		// model runs via its execute tool, schema via discover_schema.
		return ExplorerCapability{
			Dialect: "snowflake", QueryLanguage: langSQL,
			SchemaStrategy: schemaMCPDiscover, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "oracle"):
		// Delegated like BigQuery. The gateway has no Oracle driver; the connector
		// holds the only one (python-oracledb).
		return ExplorerCapability{
			Dialect: "oracle", QueryLanguage: langSQL,
			SchemaStrategy: schemaMCPDiscover, Supported: true,
			SupportsMaterialization: true,
		}
	case strings.Contains(ct, "mongo"):
		// MongoDB is non-SQL (document_db): surfaced in document mode. Reads go through
		// the connector's `find` and `aggregate` tools via the orchestrator, collections
		// come from discover_schema. No SQL dialect, no NL->SQL, no export. A model is an
		// aggregation pipeline the connector's `materialize` tool writes to a collection
		// with $out — table mode only (saved_query_documents.go).
		return ExplorerCapability{
			QueryLanguage:  langDocument,
			SchemaStrategy: schemaMCPDiscover, Supported: true,
			SupportsMaterialization: true,
		}
	default:
		// Object stores, SaaS APIs, etc. are not Explorer-queryable.
		return ExplorerCapability{Supported: false}
	}
}
