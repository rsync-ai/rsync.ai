import { describe, it, expect } from "vitest"
import {
  buildSchemaLookup,
  parseTableAliases,
  columnCompletionsFor,
  wordCompletions,
  foreignKeyJoinHints,
  tablesInSchema,
  sqlSlotAt,
  statementBounds,
  scopedColumnCompletions,
  joinCompletions,
  type CompletionTable,
} from "../sqlCompletions"

const TABLES: CompletionTable[] = [
  {
    name: "orders",
    schema: "sales",
    columns: [
      { name: "id", is_primary_key: true },
      { name: "user_id", type: "int" },
      { name: "total", type: "decimal" },
    ],
  },
  {
    name: "users",
    schema: "sales",
    columns: [
      { name: "id", is_primary_key: true },
      { name: "email", type: "varchar" },
    ],
  },
]

describe("parseTableAliases", () => {
  it("maps FROM/JOIN aliases (with and without AS) to their tables", () => {
    const a = parseTableAliases("SELECT * FROM sales.orders o JOIN users AS u ON o.user_id = u.id")
    expect(a.get("o")).toBe("sales.orders")
    expect(a.get("u")).toBe("users")
  })

  it("handles comma-separated tables", () => {
    const a = parseTableAliases("SELECT * FROM orders o, users u")
    expect(a.get("o")).toBe("orders")
    expect(a.get("u")).toBe("users")
  })

  it("does not treat SQL keywords as aliases", () => {
    const a = parseTableAliases("SELECT * FROM orders WHERE id = 1")
    expect(a.has("where")).toBe(false)
    expect(a.size).toBe(0)
  })

  // A comma is a table list only inside FROM: in ORDER BY it separates columns, and
  // `total ASC` is not a table with an alias.
  it("reads no table out of a comma that is not in a FROM list", () => {
    expect(parseTableAliases("SELECT * FROM orders ORDER BY id DESC, total ASC").size).toBe(0)
    expect(parseTableAliases("SELECT id, users u FROM orders").size).toBe(0)
  })

  it("reads no table out of a comment or a string", () => {
    expect(parseTableAliases("SELECT 1 -- from users u\nFROM orders o")).toEqual(new Map([["o", "orders"]]))
    expect(parseTableAliases("SELECT 'from users u' FROM orders o")).toEqual(new Map([["o", "orders"]]))
  })

  it("reads the tables of MERGE … USING and DELETE … USING", () => {
    const a = parseTableAliases("MERGE INTO orders t USING staged_orders s ON t.id = s.id")
    expect(a.get("t")).toBe("orders")
    expect(a.get("s")).toBe("staged_orders")
    expect(parseTableAliases("DELETE FROM orders o USING users u WHERE o.uid = u.id").get("u")).toBe("users")
  })
})

describe("columnCompletionsFor", () => {
  const lookup = buildSchemaLookup(TABLES)

  it("resolves an alias to its table's columns", () => {
    const aliases = parseTableAliases("FROM sales.orders o")
    expect(columnCompletionsFor("o", lookup, aliases).map((o) => o.label)).toEqual([
      "id",
      "user_id",
      "total",
    ])
  })

  it("resolves a bare table name directly", () => {
    expect(columnCompletionsFor("users", lookup, new Map()).map((o) => o.label)).toEqual([
      "id",
      "email",
    ])
  })

  it("marks primary-key columns distinctly", () => {
    expect(columnCompletionsFor("users", lookup, new Map()).find((o) => o.label === "id")?.type).toBe(
      "constant",
    )
  })

  it("returns [] for an unknown qualifier", () => {
    expect(columnCompletionsFor("nope", lookup, new Map())).toEqual([])
  })
})

describe("wordCompletions", () => {
  it("offers tables by bare name (inserting the qualified one) plus de-duplicated columns", () => {
    const opts = wordCompletions(buildSchemaLookup(TABLES))
    const tables = opts.filter((o) => o.type === "class")
    // A bare label scores as a prefix match; `sales.orders` ranked below keywords.
    expect(tables.map((o) => o.label)).toEqual(["orders", "users"])
    expect(tables.map((o) => o.apply)).toEqual(["sales.orders", "sales.users"])
    expect(tables.map((o) => o.detail)).toEqual(["sales · table", "sales · table"])
    expect(opts.filter((o) => o.label === "id").length).toBe(1) // deduped across tables
  })
})

describe("foreignKeyJoinHints", () => {
  it("renders join-key equalities from foreign keys", () => {
    const hints = foreignKeyJoinHints([
      {
        from_schema: "sales",
        from_table: "orders",
        from_column: "user_id",
        to_schema: "sales",
        to_table: "users",
        to_column: "id",
      },
    ])
    expect(hints[0].label).toBe("sales.orders.user_id = sales.users.id")
  })

  it("filters to FKs whose both endpoints are in scope when a scope set is given", () => {
    const fks = [
      { from_table: "orders", from_column: "user_id", to_table: "users", to_column: "id" },
      { from_table: "payments", from_column: "invoice_id", to_table: "invoices", to_column: "id" },
    ]
    expect(foreignKeyJoinHints(fks)).toHaveLength(2) // no scope → all
    const scoped = foreignKeyJoinHints(fks, new Set(["orders", "users"]))
    expect(scoped).toHaveLength(1)
    expect(scoped[0].label).toContain("orders")
  })
})

describe("tablesInSchema", () => {
  it("lists the tables in a schema by bare name", () => {
    const lookup = buildSchemaLookup(TABLES)
    expect(tablesInSchema("sales", lookup).map((o) => o.label).sort()).toEqual(["orders", "users"])
  })

  it("is case-insensitive and returns [] for an unknown schema", () => {
    const lookup = buildSchemaLookup(TABLES)
    expect(tablesInSchema("SALES", lookup)).toHaveLength(2)
    expect(tablesInSchema("public", lookup)).toEqual([])
  })
})

describe("columnCompletionsFor — schema-qualified + collisions", () => {
  const COLLIDING: CompletionTable[] = [
    { name: "orders", schema: "sales", columns: [{ name: "total", type: "decimal" }] },
    { name: "orders", schema: "archive", columns: [{ name: "archived_at", type: "timestamp" }] },
  ]

  it("resolves a fully-qualified schema.table to its columns", () => {
    const lookup = buildSchemaLookup(COLLIDING)
    expect(columnCompletionsFor("sales.orders", lookup, new Map()).map((o) => o.label)).toEqual([
      "total",
    ])
    expect(columnCompletionsFor("archive.orders", lookup, new Map()).map((o) => o.label)).toEqual([
      "archived_at",
    ])
  })

  it("declines (returns []) a bare name that is ambiguous across schemas instead of guessing", () => {
    const lookup = buildSchemaLookup(COLLIDING)
    expect(columnCompletionsFor("orders", lookup, new Map())).toEqual([])
  })
})

describe("parseTableAliases — quoted identifiers", () => {
  it("parses double-quoted (Postgres) table refs and aliases", () => {
    const a = parseTableAliases('SELECT * FROM "Users" u WHERE u.id = 1')
    expect(a.get("u")).toBe("users")
  })

  it("parses backtick-quoted (MySQL) table refs", () => {
    const a = parseTableAliases("SELECT * FROM `Orders` o")
    expect(a.get("o")).toBe("orders")
  })
})

describe("wordCompletions — cross-table column collisions", () => {
  it("neutralizes the type detail when a column name collides with differing types", () => {
    const tables: CompletionTable[] = [
      { name: "orders", schema: "s", columns: [{ name: "status", type: "int" }] },
      { name: "shipments", schema: "s", columns: [{ name: "status", type: "varchar" }] },
    ]
    const opts = wordCompletions(buildSchemaLookup(tables))
    expect(opts.filter((o) => o.label === "status")).toHaveLength(1) // still deduped
    expect(opts.find((o) => o.label === "status")?.detail).toBe("column") // neutral, not int/varchar
  })
})

describe("sqlSlotAt", () => {
  it.each([
    ["", "statement"],
    ["SELECT * FROM ", "table"],
    ["SELECT * FROM a JOIN ", "table"],
    ["SELECT * FROM a, ", "table"],
    ["INSERT INTO ", "table"],
    ["UPDATE ", "table"],
    ["SELECT * FROM a ", "afterTable"],
    ["SELECT * FROM sales.orders o ", "afterTable"],
    ["UPDATE orders ", "afterTable"],
    ["SELECT ", "column"],
    ["SELECT a, ", "column"],
    ["SELECT * FROM a WHERE ", "column"],
    ["SELECT * FROM a WHERE x = 1 AND ", "column"],
    ["SELECT * FROM a JOIN b ON ", "column"],
    ["SELECT * FROM a GROUP BY ", "column"],
    ["UPDATE a SET ", "column"],
    ["SELECT * FROM a LIMIT ", "any"],
    // A subquery or CTE body starts a statement of its own.
    ["WITH x AS (", "statement"],
    ["WITH x AS (SELECT 1) ", "statement"],
    ["WITH x AS (SELECT 1), y AS (", "statement"],
    ["SELECT * FROM (", "statement"],
    ["SELECT * FROM a WHERE id IN (", "column"],
    // A closed group is skipped as one item: the clause that holds it decides.
    ["SELECT a, (SELECT max(b) FROM t) ", "column"],
    ["SELECT * FROM a WHERE x IN (SELECT y FROM b) AND ", "column"],
    ["SELECT * FROM (SELECT * FROM a) x ", "afterTable"],
    ["INSERT INTO orders (id, total) ", "afterTable"],
    // FROM as an operand, not a clause.
    ["SELECT EXTRACT(YEAR FROM ", "column"],
    ["SELECT TRIM(BOTH FROM ", "column"],
    ["SELECT * FROM a WHERE x IS DISTINCT FROM ", "column"],
    // USING names a table in MERGE and DELETE, and a column list in JOIN.
    ["MERGE INTO orders t USING ", "table"],
    ["DELETE FROM a USING ", "table"],
    ["SELECT * FROM a JOIN b USING (", "column"],
    // UPDATE names a table only where it starts a statement.
    ["WITH x AS (SELECT 1) UPDATE ", "table"],
    ["INSERT INTO t VALUES (1) ON CONFLICT (id) DO UPDATE ", "afterTable"],
  ])("%j → %s", (before, slot) => {
    expect(sqlSlotAt(before)).toBe(slot)
  })

  it("ignores keywords inside strings and comments", () => {
    expect(sqlSlotAt("SELECT * FROM a WHERE x = 'from ")).toBe("column")
    expect(sqlSlotAt("-- from\n")).toBe("statement")
  })
})

describe("statementBounds", () => {
  it("finds the statement around the caret, ignoring a ; inside a string", () => {
    const doc = "SELECT 1; SELECT ';' FROM t; SELECT 3"
    const pos = doc.indexOf("FROM")
    const { from, to } = statementBounds(doc, pos)
    expect(doc.slice(from, to)).toBe(" SELECT ';' FROM t")
  })
})

describe("scopedColumnCompletions", () => {
  it("names the owning table when several are in scope", () => {
    const opts = scopedColumnCompletions(
      buildSchemaLookup(TABLES),
      "SELECT  FROM sales.orders o JOIN sales.users u ON u.id = o.user_id",
    )!
    expect(opts.find((o) => o.label === "email")?.detail).toBe("varchar · users")
    expect(opts.find((o) => o.label === "id")?.detail).toBe("column · orders, users")
    expect(opts.filter((o) => o.type === "variable").map((o) => o.label)).toEqual(["o", "u"])
  })

  it("returns null when the statement names no known table", () => {
    expect(scopedColumnCompletions(buildSchemaLookup(TABLES), "SELECT  FROM nowhere")).toBeNull()
  })
})

describe("joinCompletions", () => {
  const fk = {
    from_schema: "sales",
    from_table: "orders",
    from_column: "user_id",
    to_schema: "sales",
    to_table: "users",
    to_column: "id",
  }

  it("writes the join from either side of the key, using the in-scope table's alias", () => {
    const lookup = buildSchemaLookup(TABLES)
    expect(joinCompletions(lookup, [fk], "SELECT * FROM sales.orders o JOIN ").map((o) => o.label)).toEqual([
      "users ON users.id = o.user_id",
    ])
    expect(joinCompletions(lookup, [fk], "SELECT * FROM sales.users JOIN ").map((o) => o.apply)).toEqual([
      "sales.orders ON orders.user_id = users.id",
    ])
  })

  it("skips a self-referencing key", () => {
    const self = { ...fk, to_table: "orders", to_column: "id" }
    expect(joinCompletions(buildSchemaLookup(TABLES), [self], "SELECT * FROM sales.orders JOIN ")).toEqual([])
  })
})
