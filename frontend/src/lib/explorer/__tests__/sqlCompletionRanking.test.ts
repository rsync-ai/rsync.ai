import { afterEach, describe, expect, it } from "vitest"
import { EditorState } from "@codemirror/state"
import { EditorView } from "@codemirror/view"
import { sql, PostgreSQL, MySQL, type SQLDialect } from "@codemirror/lang-sql"
import {
  acceptCompletion,
  autocompletion,
  currentCompletions,
  startCompletion,
  type Completion,
} from "@codemirror/autocomplete"
import { buildSqlCompletionSources, createSchemaCompletionSource } from "../sqlCompletionSources"
import { buildSchemaLookup, type CompletionTable, type ForeignKeyLike } from "../sqlCompletions"

// These tests run CodeMirror's OWN filter + sort (FuzzyMatcher score + boost) in a
// real EditorView, so they measure the order a user sees — not just which options a
// source returns. The bug they pin: `SELECT * FROM mat` listed MATCH, MATCHED,
// MATERIALIZED… above the table `matches`, because the table was labelled
// `public.matches` (a non-prefix match scores ~-700) and every PostgreSQL dialect
// word (command_function_code, …) was offered at every position.

const TABLES: CompletionTable[] = [
  {
    name: "matches",
    schema: "public",
    columns: [{ name: "id" }, { name: "home_team", type: "text" }],
  },
  {
    name: "orders",
    schema: "public",
    columns: [
      { name: "id", is_primary_key: true },
      { name: "user_id", type: "int" },
      { name: "total", type: "numeric" },
      { name: "status", type: "text" },
    ],
  },
  {
    name: "users",
    schema: "public",
    columns: [
      { name: "id", is_primary_key: true },
      { name: "email", type: "text" },
      { name: "name", type: "text" },
    ],
  },
  {
    name: "products",
    schema: "public",
    columns: [{ name: "sku", type: "text" }, { name: "price", type: "numeric" }],
  },
]

const FKS: ForeignKeyLike[] = [
  {
    from_schema: "public",
    from_table: "orders",
    from_column: "user_id",
    to_schema: "public",
    to_table: "users",
    to_column: "id",
  },
]

let view: EditorView | null = null
afterEach(() => {
  view?.destroy()
  view = null
})

/**
 * Open the editor with `|` marking the caret, ask for completions explicitly
 * (Ctrl+Space) and return them in the order CodeMirror would show them.
 */
async function ranked(
  docWithCaret: string,
  opts: { tables?: CompletionTable[]; fks?: ForeignKeyLike[]; dialect?: SQLDialect } = {},
): Promise<Completion[]> {
  const caret = docWithCaret.indexOf("|")
  const doc = caret >= 0 ? docWithCaret.replace("|", "") : docWithCaret
  const anchor = caret >= 0 ? caret : doc.length
  const dialect = opts.dialect ?? PostgreSQL
  const lookup = buildSchemaLookup(opts.tables ?? TABLES)
  view = new EditorView({
    state: EditorState.create({
      doc,
      selection: { anchor },
      extensions: [
        sql({ dialect, upperCaseKeywords: true }),
        autocompletion({
          override: buildSqlCompletionSources(
            createSchemaCompletionSource(lookup, opts.fks ?? FKS),
            dialect,
          ),
        }),
      ],
    }),
    parent: document.body,
  })
  startCompletion(view)
  await new Promise((r) => setTimeout(r, 250))
  return [...currentCompletions(view.state)]
}

const labels = (cs: Completion[]) => cs.map((c) => c.label)

describe("table position (after FROM / JOIN)", () => {
  it("puts the table first when its name shares a prefix with keywords (the reported bug)", async () => {
    const cs = await ranked("SELECT * FROM mat")
    expect(labels(cs)[0]).toBe("matches")
    // Only tables are valid here — no MATCH / MATCHED / MATERIALIZED.
    expect(cs.every((c) => c.type === "class")).toBe(true)
  })

  it("shows the schema as detail and inserts the qualified name", async () => {
    const cs = await ranked("SELECT * FROM mat")
    expect(cs[0].detail).toContain("public")
    acceptCompletion(view!)
    expect(view!.state.doc.toString()).toBe("SELECT * FROM public.matches")
  })

  it("keeps both tables when the same name exists in two schemas, each inserting its own", async () => {
    const cs = await ranked("SELECT * FROM us", {
      tables: [
        { name: "users", schema: "crm", columns: [] },
        { name: "users", schema: "billing", columns: [] },
      ],
      fks: [],
    })
    const users = cs.filter((c) => c.label === "users")
    expect(users).toHaveLength(2)
    expect(users.map((c) => c.detail).join(" ")).toMatch(/crm/)
    expect(users.map((c) => c.detail).join(" ")).toMatch(/billing/)
  })

  it("after JOIN, offers the foreign-key join with its ON clause first", async () => {
    const cs = await ranked("SELECT * FROM orders o JOIN |")
    expect(labels(cs)[0]).toBe("users ON users.id = o.user_id")
    acceptCompletion(view!)
    expect(view!.state.doc.toString()).toBe(
      "SELECT * FROM orders o JOIN public.users ON users.id = o.user_id",
    )
  })

  it("after a comma in FROM, still offers tables", async () => {
    const cs = await ranked("SELECT * FROM orders, pro")
    expect(labels(cs)[0]).toBe("products")
  })
})

describe("keywords", () => {
  it("never offers obscure dialect words such as command_function_code", async () => {
    const cs = await ranked("SELECT command|")
    expect(labels(cs).map((l) => l.toUpperCase())).not.toContain("COMMAND_FUNCTION_CODE")
  })

  it("offers statement keywords, and nothing from the schema, at the start of a statement", async () => {
    const cs = await ranked("|")
    expect(labels(cs)).toContain("SELECT")
    expect(cs.some((c) => c.type === "class" || c.type === "property")).toBe(false)
  })

  it("starts a second statement after a semicolon", async () => {
    const cs = await ranked("SELECT 1;\nsel")
    expect(labels(cs)[0]).toBe("SELECT")
  })

  it("after a table, offers clause keywords instead of tables and columns", async () => {
    const cs = await ranked("SELECT * FROM orders o |")
    const ls = labels(cs)
    expect(ls).toEqual(expect.arrayContaining(["WHERE", "LEFT JOIN", "ORDER BY", "LIMIT"]))
    expect(cs.some((c) => c.type === "class" || c.type === "property")).toBe(false)
  })

  it("completes a two-word keyword over the word already typed", async () => {
    const cs = await ranked("SELECT * FROM orders ORDER b")
    expect(labels(cs)[0]).toBe("ORDER BY")
    acceptCompletion(view!)
    expect(view!.state.doc.toString()).toBe("SELECT * FROM orders ORDER BY")
  })

  // A subquery, a CTE body and INSERT … SELECT each start a query of their own.
  it.each([
    ["WITH x AS (SELECT 1) SEL"],
    ["WITH x AS (SEL"],
    ["SELECT * FROM orders WHERE user_id IN (SEL"],
    ["INSERT INTO orders SEL"],
  ])("offers SELECT where a query can start: %j", async (doc) => {
    expect(labels(await ranked(doc))).toContain("SELECT")
  })

  it("offers MERGE, and its WHEN clauses, on engines that have it", async () => {
    expect(labels(await ranked("MER"))).toContain("MERGE INTO")
    expect(labels(await ranked("MER", { dialect: MySQL }))).not.toContain("MERGE INTO")
    expect(labels(await ranked("MERGE INTO orders t USING users u ON t.user_id = u.id WHEN m"))).toContain(
      "WHEN MATCHED THEN",
    )
  })

  it("keeps keywords for other dialects (MySQL)", async () => {
    const cs = await ranked("SELECT * FROM orders WHE", { dialect: MySQL })
    expect(labels(cs)).toContain("WHERE")
  })
})

describe("column position", () => {
  it("offers only the columns of the tables in the statement, even before FROM is reached", async () => {
    const cs = await ranked("SELECT | FROM users")
    const props = cs.filter((c) => c.type === "property" || c.type === "constant").map((c) => c.label)
    expect(props).toEqual(expect.arrayContaining(["email", "name"]))
    expect(props).not.toContain("price")
    expect(props).not.toContain("home_team")
  })

  it("ranks a column above a keyword that shares its prefix", async () => {
    const cs = await ranked("SELECT * FROM orders WHERE st")
    expect(labels(cs)[0]).toBe("status")
  })

  it("offers the statement's aliases so `o` then `.` reaches the columns", async () => {
    const cs = await ranked("SELECT * FROM orders o WHERE |")
    expect(labels(cs)).toContain("o")
  })

  it("offers the tables after MERGE … USING", async () => {
    const cs = await ranked("MERGE INTO orders t USING |")
    expect(labels(cs)).toContain("users")
    expect(cs.every((c) => c.type === "class")).toBe(true)
  })

  // FROM inside EXTRACT(…) and after IS DISTINCT is an operand: a column, not a table.
  it.each([["SELECT EXTRACT(YEAR FROM |) FROM orders"], ["SELECT * FROM orders WHERE status IS DISTINCT FROM |"]])(
    "offers columns, not tables, after an operand FROM: %j",
    async (doc) => {
      const ls = labels(await ranked(doc))
      expect(ls).toContain("total")
      expect(ls).not.toContain("matches")
    },
  )

  it("resolves an alias declared after the caret", async () => {
    const cs = await ranked("SELECT o.| FROM orders o")
    expect(labels(cs)).toEqual(expect.arrayContaining(["total", "status"]))
  })

  it("does not carry an alias over from the statement before", async () => {
    const cs = await ranked("SELECT * FROM orders o;\nSELECT o.| FROM users u")
    expect(labels(cs)).not.toContain("total")
  })

  // Neither a column named like a table nor a comment adds a table to the scope.
  it.each([["SELECT id, users FROM orders WHERE |"], ["-- from users\nSELECT * FROM orders WHERE |"]])(
    "scopes columns to the tables the statement reads: %j",
    async (doc) => {
      const ls = labels(await ranked(doc))
      expect(ls).toContain("total")
      expect(ls).not.toContain("email")
    },
  )

  it("falls back to every column when the statement names no known table", async () => {
    const cs = await ranked("SELECT pri")
    expect(labels(cs)[0]).toBe("price")
  })
})

describe("strings and comments", () => {
  it("offers nothing inside a string literal", async () => {
    const cs = await ranked("SELECT * FROM orders WHERE status = 'mat")
    expect(cs).toHaveLength(0)
  })

  it("offers nothing inside a line comment", async () => {
    const cs = await ranked("-- mat")
    expect(cs).toHaveLength(0)
  })
})
