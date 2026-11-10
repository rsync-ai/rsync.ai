import { MSSQL, MySQL, PLSQL, PostgreSQL, StandardSQL, type SQLDialect } from "@codemirror/lang-sql"

/**
 * The SQL dialect the Explorer editor highlights against and the AI writes for,
 * one per SQL engine the gateway can query (ResolveExplorerCapability,
 * api-gateway/internal/handlers/explorer_capability.go). "generic" is ANSI SQL.
 */
export type ExplorerSqlDialect =
  | "postgresql"
  | "mysql"
  | "redshift"
  | "databricks"
  | "sqlserver"
  | "bigquery"
  | "snowflake"
  | "clickhouse"
  | "oracle"
  | "generic"

/** Human name for each dialect, shown next to the editor. */
export const SQL_DIALECT_LABELS: Record<ExplorerSqlDialect, string> = {
  postgresql: "PostgreSQL",
  mysql: "MySQL",
  redshift: "Redshift",
  databricks: "Databricks",
  sqlserver: "SQL Server",
  bigquery: "BigQuery",
  snowflake: "Snowflake",
  clickhouse: "ClickHouse",
  oracle: "Oracle",
  generic: "ANSI SQL",
}

/**
 * The dialect for a connection's connector_type. Substring checks, in the
 * gateway's order (mysql before postgres), so a variant type such as
 * "postgresql-cdc" or "aws-redshift" resolves the way the gateway resolves it.
 */
export function explorerSqlDialect(connectorType: string | undefined | null): ExplorerSqlDialect {
  const ct = (connectorType || "").toLowerCase()
  if (ct.includes("mysql") || ct.includes("mariadb")) return "mysql"
  if (ct.includes("redshift")) return "redshift"
  if (ct.includes("postgres")) return "postgresql"
  if (ct.includes("sqlserver") || ct.includes("mssql")) return "sqlserver"
  if (ct.includes("databricks")) return "databricks"
  if (ct.includes("bigquery")) return "bigquery"
  if (ct.includes("clickhouse")) return "clickhouse"
  if (ct.includes("snowflake")) return "snowflake"
  if (ct.includes("oracle")) return "oracle"
  return "generic"
}

/**
 * The CodeMirror parser for a dialect. Engines without a dedicated one
 * (Databricks, BigQuery, Snowflake, ClickHouse) use StandardSQL: keywords and
 * highlighting still work.
 */
export function codeMirrorDialect(dialect: ExplorerSqlDialect | undefined): SQLDialect {
  switch (dialect) {
    case "mysql":
      return MySQL
    case "postgresql":
    case "redshift":
      return PostgreSQL
    case "sqlserver":
      return MSSQL
    case "oracle":
      return PLSQL
    default:
      return StandardSQL
  }
}
