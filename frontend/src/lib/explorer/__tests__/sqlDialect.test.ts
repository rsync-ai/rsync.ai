import { describe, it, expect } from "vitest"
import { MSSQL, MySQL, PLSQL, PostgreSQL, StandardSQL } from "@codemirror/lang-sql"
import {
  codeMirrorDialect,
  explorerSqlDialect,
  SQL_DIALECT_LABELS,
  type ExplorerSqlDialect,
} from "../sqlDialect"

// The Explorer labels the editor "SQL is highlighted, completed and generated for
// <dialect>". Before models ran on every SQL engine, a Snowflake, Oracle, ClickHouse,
// BigQuery or SQL Server connection fell through to "ANSI SQL" — a label that named
// the wrong language for the SQL a user was about to turn into a table.
describe("explorerSqlDialect", () => {
  const cases: Array<[string, ExplorerSqlDialect]> = [
    ["postgresql", "postgresql"],
    ["postgresql-cdc", "postgresql"],
    ["mysql", "mysql"],
    ["mariadb", "mysql"],
    ["redshift", "redshift"],
    ["databricks", "databricks"],
    ["sqlserver", "sqlserver"],
    ["mssql", "sqlserver"],
    ["bigquery", "bigquery"],
    ["snowflake", "snowflake"],
    ["clickhouse", "clickhouse"],
    ["oracle", "oracle"],
    ["mongodb", "generic"],
    ["", "generic"],
  ]
  it.each(cases)("%s → %s", (connectorType, want) => {
    expect(explorerSqlDialect(connectorType)).toBe(want)
  })

  it("is case-insensitive and tolerates a missing type", () => {
    expect(explorerSqlDialect("Snowflake")).toBe("snowflake")
    expect(explorerSqlDialect(undefined)).toBe("generic")
    expect(explorerSqlDialect(null)).toBe("generic")
  })

  it("labels every dialect with the engine's name", () => {
    expect(SQL_DIALECT_LABELS[explorerSqlDialect("oracle")]).toBe("Oracle")
    expect(SQL_DIALECT_LABELS[explorerSqlDialect("sqlserver")]).toBe("SQL Server")
    expect(SQL_DIALECT_LABELS[explorerSqlDialect("snowflake")]).toBe("Snowflake")
    expect(SQL_DIALECT_LABELS.generic).toBe("ANSI SQL")
  })
})

describe("codeMirrorDialect", () => {
  it("uses the dedicated parser where CodeMirror has one", () => {
    expect(codeMirrorDialect("mysql")).toBe(MySQL)
    expect(codeMirrorDialect("postgresql")).toBe(PostgreSQL)
    expect(codeMirrorDialect("redshift")).toBe(PostgreSQL)
    expect(codeMirrorDialect("sqlserver")).toBe(MSSQL)
    expect(codeMirrorDialect("oracle")).toBe(PLSQL)
  })

  it("falls back to StandardSQL for the rest", () => {
    for (const d of ["databricks", "bigquery", "snowflake", "clickhouse", "generic"] as const) {
      expect(codeMirrorDialect(d)).toBe(StandardSQL)
    }
    expect(codeMirrorDialect(undefined)).toBe(StandardSQL)
  })
})
