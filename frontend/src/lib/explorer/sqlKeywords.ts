// The SQL keywords the Explorer editor suggests, by what the caret can be typing.
//
// Curated on purpose. CodeMirror's dialect word list is a highlighter's list: the
// PostgreSQL one holds ~700 words (command_function_code, dlurlpathonly, …), and
// offering all of them at every position buried tables and columns under words
// nobody types. These are the words people write in a query, grouped so that a
// position only offers the words that can follow it.

import type { SqlSlot } from "./sqlCompletions"

/** Keyword flavour per engine; "generic" is ANSI SQL. */
export type KeywordDialect = "postgresql" | "mysql" | "sqlserver" | "oracle" | "generic"

/** Words that start a statement. */
const STATEMENT = [
  "SELECT",
  "WITH",
  "EXPLAIN",
  "INSERT INTO",
  "UPDATE",
  "DELETE FROM",
  "CREATE TABLE",
  "CREATE VIEW",
  "ALTER TABLE",
  "DROP TABLE",
  "TRUNCATE TABLE",
]

/** Words that can follow a table reference in FROM / JOIN / UPDATE / INTO. */
const AFTER_TABLE = [
  "AS",
  "WHERE",
  "JOIN",
  "INNER JOIN",
  "LEFT JOIN",
  "RIGHT JOIN",
  "FULL JOIN",
  "CROSS JOIN",
  "ON",
  "USING",
  "GROUP BY",
  "ORDER BY",
  "HAVING",
  "LIMIT",
  "OFFSET",
  "UNION",
  "UNION ALL",
  "SET",
  "VALUES",
  "SELECT", // INSERT INTO t SELECT …
]

/** Words inside an expression: SELECT list, WHERE, ON, GROUP/ORDER BY, HAVING, SET. */
const EXPRESSION = [
  "SELECT", // a subquery: IN (SELECT …), EXISTS (SELECT …)
  "FROM",
  "AS",
  "DISTINCT",
  "AND",
  "OR",
  "NOT",
  "IN",
  "IS NULL",
  "IS NOT NULL",
  "LIKE",
  "BETWEEN",
  "EXISTS",
  "CASE",
  "WHEN",
  "THEN",
  "ELSE",
  "END",
  "NULL",
  "TRUE",
  "FALSE",
  "ASC",
  "DESC",
  "WHERE",
  "GROUP BY",
  "ORDER BY",
  "HAVING",
  "LIMIT",
  "OFFSET",
  "JOIN",
  "LEFT JOIN",
  "INNER JOIN",
  "ON",
  "UNION",
  "UNION ALL",
  "OVER",
  "PARTITION BY",
]

/** Functions people reach for in the Explorer. */
const FUNCTIONS = [
  "COUNT",
  "SUM",
  "AVG",
  "MIN",
  "MAX",
  "COALESCE",
  "NULLIF",
  "CAST",
  "LOWER",
  "UPPER",
  "TRIM",
  "LENGTH",
  "ROUND",
  "ABS",
  "EXTRACT",
  "CURRENT_DATE",
  "CURRENT_TIMESTAMP",
  "ROW_NUMBER",
  "RANK",
  "DENSE_RANK",
  "LAG",
  "LEAD",
]

/** MERGE's branches. MySQL has no MERGE. */
const MERGE_WHEN = ["WHEN MATCHED THEN", "WHEN NOT MATCHED THEN"]

/** Engine-only additions, per list. */
const DIALECT_EXTRA: Record<KeywordDialect, { statement: string[]; expression: string[]; functions: string[] }> = {
  postgresql: {
    statement: ["MERGE INTO"],
    expression: ["ILIKE", "NULLS FIRST", "NULLS LAST", "RETURNING", ...MERGE_WHEN],
    functions: ["NOW", "DATE_TRUNC", "STRING_AGG", "ARRAY_AGG", "TO_CHAR", "GENERATE_SERIES"],
  },
  mysql: {
    statement: ["SHOW TABLES", "DESCRIBE"],
    expression: ["REGEXP"],
    functions: ["NOW", "IFNULL", "GROUP_CONCAT", "DATE_FORMAT", "CONCAT"],
  },
  sqlserver: {
    statement: ["MERGE INTO"],
    expression: ["TOP", ...MERGE_WHEN],
    functions: ["GETDATE", "ISNULL", "DATEADD", "DATEDIFF", "CONCAT", "LEN"],
  },
  oracle: {
    statement: ["MERGE INTO"],
    expression: ["FETCH FIRST", "ROWS ONLY", "NULLS FIRST", "NULLS LAST", ...MERGE_WHEN],
    functions: ["SYSDATE", "NVL", "TO_CHAR", "TO_DATE", "LISTAGG"],
  },
  generic: {
    statement: ["MERGE INTO"],
    expression: ["NULLS FIRST", "NULLS LAST", ...MERGE_WHEN],
    functions: ["CONCAT", "DATE_TRUNC"],
  },
}

export interface KeywordOption {
  label: string
  type: "keyword" | "function"
  boost: number
}

// Keywords sit below tables (5) and columns (4) so an identifier that shares a
// prefix with a keyword is listed first; functions a step below keywords.
const KEYWORD_BOOST = -1
const FUNCTION_BOOST = -2

const asKeywords = (words: string[]): KeywordOption[] =>
  words.map((label) => ({ label, type: "keyword", boost: KEYWORD_BOOST }))
const asFunctions = (words: string[]): KeywordOption[] =>
  words.map((label) => ({ label, type: "function", boost: FUNCTION_BOOST }))

function dedupe(opts: KeywordOption[]): KeywordOption[] {
  const seen = new Set<string>()
  return opts.filter((o) => (seen.has(o.label) ? false : (seen.add(o.label), true)))
}

/** The keywords (and functions) that can be typed at a slot. A table slot has none. */
export function keywordsForSlot(slot: SqlSlot, dialect: KeywordDialect): KeywordOption[] {
  const extra = DIALECT_EXTRA[dialect]
  switch (slot) {
    case "statement":
      return asKeywords([...STATEMENT, ...extra.statement])
    case "table":
      return []
    case "afterTable":
      return asKeywords(AFTER_TABLE)
    case "column":
      return dedupe([
        ...asKeywords([...EXPRESSION, ...extra.expression]),
        ...asFunctions([...FUNCTIONS, ...extra.functions]),
      ])
    case "any":
      return dedupe([
        ...asKeywords([...STATEMENT, ...extra.statement, ...AFTER_TABLE, ...EXPRESSION, ...extra.expression]),
        ...asFunctions([...FUNCTIONS, ...extra.functions]),
      ])
  }
}

/**
 * Every multi-word keyword (`GROUP BY`, `LEFT JOIN`, `IS NOT NULL`, …) across all
 * slots for a dialect. Offered over the words already typed, so `ORDER b` completes
 * to `ORDER BY` instead of `ORDER ORDER BY`.
 */
export function multiWordKeywords(dialect: KeywordDialect): KeywordOption[] {
  return keywordsForSlot("any", dialect).filter((o) => o.label.includes(" "))
}
