// Pure, framework-agnostic SQL autocomplete logic for the Data Explorer's
// CodeMirror editor. Kept out of the React component so the matching rules
// (alias resolution, schema lookup, FK join hints) can be unit-tested
// without spinning up an editor.

export interface CompletionColumn {
  name: string
  type?: string
  is_primary_key?: boolean
}

export interface CompletionTable {
  name: string
  schema?: string
  columns?: CompletionColumn[]
}

export interface ForeignKeyLike {
  from_schema?: string
  from_table: string
  from_column: string
  to_schema?: string
  to_table: string
  to_column: string
}

export interface CompletionOption {
  label: string
  type: string
  detail?: string
  boost?: number
  /** Text inserted instead of the label (e.g. the schema-qualified table name). */
  apply?: string
}

/** A table resolved into the lookup: qualified label + bare name + schema. */
interface LookupTable {
  label: string
  bare: string
  schema?: string
  columns: CompletionColumn[]
}

export interface SchemaLookup {
  /** Tables in input order, with their qualified label, bare name, schema. */
  tables: LookupTable[]
  /** Lowercased qualified label (`schema.table`) → columns. */
  byLabel: Map<string, CompletionColumn[]>
  /**
   * Lowercased bare table name → columns. A bare name is only resolvable when
   * it is UNIQUE across schemas; ambiguous bare names are removed from this
   * map (see `bareAmbiguous`) so we never silently autocomplete the wrong
   * table's columns.
   */
  byBareName: Map<string, CompletionColumn[]>
  /** Bare names that map to >1 table — must be schema-qualified to resolve. */
  bareAmbiguous: Set<string>
  /** Lowercased schema name → its tables (for `schema.` completion). */
  bySchema: Map<string, LookupTable[]>
}

const qualify = (t: CompletionTable): string => (t.schema ? `${t.schema}.${t.name}` : t.name)

/** Strip surrounding double-quotes or backticks from a SQL identifier. */
function stripQuotes(ident: string): string {
  const s = ident.trim()
  if (
    s.length >= 2 &&
    ((s[0] === '"' && s[s.length - 1] === '"') || (s[0] === "`" && s[s.length - 1] === "`"))
  ) {
    return s.slice(1, -1)
  }
  return s
}

/**
 * Normalize a qualifier (alias / table / `schema.table`) for lookup: strip
 * quotes on each dotted segment and lowercase. `"Sales"."Orders"` → `sales.orders`.
 */
function normalizeQualifier(qualifier: string): string {
  return qualifier
    .split(".")
    .map((seg) => stripQuotes(seg).toLowerCase())
    .join(".")
}

/** Words that can never be a table alias (SQL keywords that may follow a table). */
const NOT_ALIASES = new Set([
  "on", "where", "join", "inner", "left", "right", "full", "outer", "cross",
  "group", "order", "limit", "having", "union", "using", "as", "and", "or",
  "select", "from", "natural", "set", "values", "returning", "offset",
  "intersect", "except", "window", "fetch", "for", "of", "skip", "nowait",
  "cascade", "restrict", "asc", "desc", "nulls", "is", "not", "when", "then",
  "lateral", "matched", "merge", "with", "only", "into", "update",
])

/** Build a case-insensitive lookup of tables + columns for completion. */
export function buildSchemaLookup(tables: CompletionTable[]): SchemaLookup {
  const list: LookupTable[] = []
  const byLabel = new Map<string, CompletionColumn[]>()
  const byBareName = new Map<string, CompletionColumn[]>()
  const bareAmbiguous = new Set<string>()
  const bySchema = new Map<string, LookupTable[]>()

  for (const t of tables) {
    const label = qualify(t)
    const columns = t.columns ?? []
    const entry: LookupTable = { label, bare: t.name, schema: t.schema, columns }
    list.push(entry)
    byLabel.set(label.toLowerCase(), columns)

    // Bare-name index with collision detection: the FIRST table claims the
    // bare name; a SECOND table with the same bare name makes it ambiguous,
    // so we drop it from the map and force schema-qualification (I3).
    const bare = t.name.toLowerCase()
    if (bareAmbiguous.has(bare)) {
      // already known-ambiguous — leave it out of byBareName
    } else if (byBareName.has(bare)) {
      byBareName.delete(bare)
      bareAmbiguous.add(bare)
    } else {
      byBareName.set(bare, columns)
    }

    if (t.schema) {
      const key = t.schema.toLowerCase()
      const arr = bySchema.get(key)
      if (arr) arr.push(entry)
      else bySchema.set(key, [entry])
    }
  }
  return { tables: list, byLabel, byBareName, bareAmbiguous, bySchema }
}

/**
 * Parse the table references and their aliases out of a SQL statement, e.g.
 * `FROM sales.orders o JOIN users AS u` → {o: "sales.orders", u: "users"}.
 * Quoted identifiers (`"Users"`, `` `Orders` ``) are supported and normalized
 * (quotes stripped, lowercased). Returns lowercased alias → lowercased
 * table-ref. Best-effort (see tableRefs) and safe on partial/incomplete SQL.
 */
export function parseTableAliases(sqlText: string): Map<string, string> {
  const aliases = new Map<string, string>()
  for (const { ref, alias } of tableRefs(sqlText)) if (alias) aliases.set(alias, ref)
  return aliases
}

/**
 * Extract the bare table names referenced in a statement's FROM/JOIN/comma
 * clauses (schema qualifier dropped, quotes stripped, lowercased). Used to
 * scope FK join-hints to the tables actually in play.
 */
export function tableRefsInStatement(sqlText: string): Set<string> {
  const refs = new Set<string>()
  for (const ref of qualifiedTableRefs(sqlText)) refs.add(bareOf(ref))
  return refs
}

/** The last dotted segment of a normalized ref: `sales.orders` → `orders`. */
const bareOf = (ref: string): string => (ref.includes(".") ? ref.slice(ref.lastIndexOf(".") + 1) : ref)

/**
 * The table refs in a statement as written (quotes stripped, lowercased),
 * keeping the schema when there is one: `sales.orders`.
 */
function qualifiedTableRefs(sqlText: string): string[] {
  const refs: string[] = []
  for (const { ref } of tableRefs(sqlText)) if (!refs.includes(ref)) refs.push(ref)
  return refs
}

/** An identifier token, bare or quoted. */
const IDENT_RE = /^(?:"[^"]+"|`[^`]+`|[a-z_][\w$]*)$/i
/** Words after which a name is a table. */
const REF_INTRODUCERS = new Set(["from", "join", "into", "update", "using"])
/** Words that end a FROM list: a comma after them no longer separates tables. */
const FROM_LIST_ENDS = new Set([
  "where", "group", "order", "having", "limit", "offset", "union", "intersect", "except",
  "window", "returning", "fetch", "for", "set", "values", "select", "when", "qualify",
])

/**
 * The tables a statement reads or writes, in order, each with its alias: the
 * name after FROM, JOIN, INTO, UPDATE and USING (MERGE … USING, DELETE …
 * USING), and after a comma only while a FROM list is open at that paren depth
 * — so `SELECT id, users FROM orders` and `ORDER BY id DESC, total ASC` name
 * no table. Strings and comments are masked first. A token scan, not a parser.
 */
function tableRefs(sqlText: string): { ref: string; alias?: string }[] {
  const raw = rawTokens(sqlText)
  const low = raw.map((t) => t.toLowerCase())
  const out: { ref: string; alias?: string }[] = []
  // Read `name[.name] [AS] [alias]` from `start`; returns the last index consumed.
  const readRef = (start: number): number => {
    if (!IDENT_RE.test(raw[start] ?? "")) return start - 1
    let text = raw[start]
    let end = start
    while (raw[end + 1] === "." && IDENT_RE.test(raw[end + 2] ?? "")) {
      text += `.${raw[end + 2]}`
      end += 2
    }
    const ref = normalizeQualifier(text)
    if (NOT_ALIASES.has(bareOf(ref))) return start - 1
    let at = end + 1
    if (low[at] === "as") at++
    if (IDENT_RE.test(raw[at] ?? "") && !NOT_ALIASES.has(low[at])) {
      out.push({ ref, alias: stripQuotes(raw[at]).toLowerCase() })
      return at
    }
    out.push({ ref })
    return end
  }
  const fromListAt = new Set<number>() // paren depths with an open FROM list
  let depth = 0
  for (let i = 0; i < raw.length; i++) {
    const t = low[i]
    if (t === "(") depth++
    else if (t === ")") fromListAt.delete(depth--)
    else if (t === ";") {
      fromListAt.clear()
      depth = 0
    } else if (t === "," && fromListAt.has(depth)) i = readRef(i + 1)
    else if (REF_INTRODUCERS.has(t) && opensClause(low, i)) {
      if (t === "from") fromListAt.add(depth)
      i = readRef(i + 1)
    } else if (FROM_LIST_ENDS.has(t)) fromListAt.delete(depth)
  }
  return out
}

/**
 * Column completions for a qualifier that may be a table alias (`o`), a bare
 * table name (`orders`) or a fully-qualified table (`sales.orders`). Quotes are
 * tolerated. Returns [] when unresolved — and, crucially, when a BARE name is
 * ambiguous across schemas, so we never autocomplete the wrong table (I3).
 */
export function columnCompletionsFor(
  qualifier: string,
  lookup: SchemaLookup,
  aliases: Map<string, string>,
): CompletionOption[] {
  const norm = normalizeQualifier(qualifier)
  // Resolve an alias to its underlying (possibly schema-qualified) table.
  const resolved = aliases.get(norm) ?? norm

  let cols: CompletionColumn[] | undefined
  if (resolved.includes(".")) {
    // Fully-qualified `schema.table` — resolve against the qualified label
    // only; do NOT fall back to a bare-name guess for a multi-segment qualifier.
    cols = lookup.byLabel.get(resolved)
  } else {
    if (lookup.bareAmbiguous.has(resolved)) return []
    cols = lookup.byBareName.get(resolved) ?? lookup.byLabel.get(resolved)
  }
  if (!cols || cols.length === 0) return []
  return cols.map((c) => ({
    label: c.name,
    type: c.is_primary_key ? "constant" : "property",
    detail: c.type ?? "",
  }))
}

/**
 * Tables in a given schema, bare-labelled — offered after `schema.` so a
 * schema-qualified prefix completes table names rather than SQL keywords.
 * Case-insensitive; returns [] for an unknown schema.
 */
export function tablesInSchema(schema: string, lookup: SchemaLookup): CompletionOption[] {
  const tables = lookup.bySchema.get(stripQuotes(schema).toLowerCase())
  if (!tables) return []
  return tables.map((t) => ({ label: t.bare, type: "class", detail: "table", boost: 5 }))
}

/**
 * Tables for a position that takes one. Labelled with the BARE name so a typed
 * prefix scores as a prefix: CodeMirror ranks `public.matches` for "mat" as a
 * mid-word match (~-700), far below the keyword MATCH. The schema goes in the
 * detail, and the qualified name is what gets inserted. Two tables that share a
 * name in different schemas stay two options, told apart by their detail.
 */
export function tableCompletions(lookup: SchemaLookup): CompletionOption[] {
  return lookup.tables.map((t) => {
    const opt: CompletionOption = {
      label: t.bare,
      type: "class",
      detail: t.schema ? `${t.schema} · table` : "table",
      boost: 5,
    }
    if (t.label !== t.bare) opt.apply = t.label
    return opt
  })
}

/**
 * Columns, de-duplicated by name. When the same column name appears across
 * tables with DIFFERENT types, the type detail is neutralized to "column"
 * rather than presenting one table's type as authoritative (I8). With
 * `withTables`, the detail also names the table(s) the column is in.
 */
function columnOptions(
  tables: LookupTable[],
  opts: { boost?: number; withTables?: boolean } = {},
): CompletionOption[] {
  const firstType = new Map<string, string>() // column name → first type seen ("" if untyped)
  const owners = new Map<string, string[]>() // column name → tables that have it
  const conflicting = new Set<string>()
  for (const t of tables) {
    for (const c of t.columns) {
      const ty = c.type ?? ""
      if (!firstType.has(c.name)) {
        firstType.set(c.name, ty)
        owners.set(c.name, [t.bare])
      } else {
        owners.get(c.name)!.push(t.bare)
        if (firstType.get(c.name) !== ty) conflicting.add(c.name)
      }
    }
  }
  return [...firstType.keys()].map((name) => {
    let detail = conflicting.has(name) ? "column" : firstType.get(name) || "column"
    if (opts.withTables) detail = `${detail} · ${owners.get(name)!.join(", ")}`
    const opt: CompletionOption = { label: name, type: "property", detail }
    if (opts.boost !== undefined) opt.boost = opts.boost
    return opt
  })
}

/**
 * Completions at a word boundary when the position is unclear: tables (boosted
 * so they sort first) plus every column, de-duplicated. SQL keywords are added
 * separately by the keyword source.
 */
export function wordCompletions(lookup: SchemaLookup): CompletionOption[] {
  return [...tableCompletions(lookup), ...columnOptions(lookup.tables)]
}

/** The lookup tables a normalized ref names; [] when unknown or ambiguous (I3). */
function resolveRef(ref: string, lookup: SchemaLookup): LookupTable[] {
  if (ref.includes(".")) return lookup.tables.filter((t) => t.label.toLowerCase() === ref)
  if (lookup.bareAmbiguous.has(ref)) return []
  return lookup.tables.filter((t) => t.bare.toLowerCase() === ref)
}

/**
 * Completions inside an expression (SELECT list, WHERE, ON, GROUP/ORDER BY…):
 * the columns of the tables the WHOLE statement names — so `SELECT | FROM users`
 * offers users' columns before FROM is reached — plus the statement's aliases,
 * so `o` then `.` reaches a table's columns. Returns null when the statement
 * names no table we know, so the caller can fall back to every column.
 */
export function scopedColumnCompletions(
  lookup: SchemaLookup,
  statement: string,
): CompletionOption[] | null {
  const inScope: LookupTable[] = []
  for (const ref of qualifiedTableRefs(statement)) {
    for (const t of resolveRef(ref, lookup)) if (!inScope.includes(t)) inScope.push(t)
  }
  if (inScope.length === 0) return null
  const aliases: CompletionOption[] = []
  for (const [alias, table] of parseTableAliases(statement)) {
    aliases.push({ label: alias, type: "variable", detail: `alias · ${table}`, boost: 2 })
  }
  return [...columnOptions(inScope, { boost: 4, withTables: inScope.length > 1 }), ...aliases]
}

/**
 * After `JOIN`, the joins the foreign keys allow, written out with their ON
 * clause: `users ON users.id = o.user_id`. One side must be a table the
 * statement already names (referred to by its alias when it has one); the
 * other is the table being joined, inserted schema-qualified. Inferred keys
 * count — the caller passes whatever keys the Explorer shows. A self-reference
 * is skipped: joining a table to itself needs an alias only the user can pick.
 */
export function joinCompletions(
  lookup: SchemaLookup,
  fks: ForeignKeyLike[],
  statementBefore: string,
): CompletionOption[] {
  const refs = tableRefsInStatement(statementBefore)
  if (refs.size === 0 || fks.length === 0) return []
  const refName = new Map<string, string>() // bare in-scope table → how the statement names it
  for (const [alias, table] of parseTableAliases(statementBefore)) {
    if (!refName.has(bareOf(table))) refName.set(bareOf(table), alias)
  }
  const out: CompletionOption[] = []
  const seen = new Set<string>()
  const add = (
    cand: { schema?: string; table: string; column: string },
    other: { table: string; column: string },
  ) => {
    const otherBare = other.table.toLowerCase()
    if (!refs.has(otherBare) || cand.table.toLowerCase() === otherBare) return
    const on = `ON ${cand.table}.${cand.column} = ${refName.get(otherBare) ?? other.table}.${other.column}`
    const label = `${cand.table} ${on}`
    if (seen.has(label)) return
    seen.add(label)
    const opt: CompletionOption = { label, type: "class", detail: "foreign key", boost: 9 }
    if (cand.schema) opt.apply = `${cand.schema}.${cand.table} ${on}`
    out.push(opt)
  }
  for (const fk of fks) {
    add(
      { schema: fk.to_schema, table: fk.to_table, column: fk.to_column },
      { table: fk.from_table, column: fk.from_column },
    )
    add(
      { schema: fk.from_schema, table: fk.from_table, column: fk.from_column },
      { table: fk.to_table, column: fk.to_column },
    )
  }
  return out
}

/**
 * Suggest join-key equalities from foreign keys, e.g.
 * `sales.orders.user_id = sales.users.id` — offered after `ON` in a JOIN.
 * When `scope` is given, only FKs whose BOTH endpoints (bare table names) are
 * in scope are returned, so the hint list reflects the current FROM/JOIN set.
 */
export function foreignKeyJoinHints(
  fks: ForeignKeyLike[],
  scope?: Set<string>,
): CompletionOption[] {
  const inScope = (fk: ForeignKeyLike): boolean =>
    !scope || (scope.has(fk.from_table.toLowerCase()) && scope.has(fk.to_table.toLowerCase()))
  return fks.filter(inScope).map((fk) => {
    const from = fk.from_schema ? `${fk.from_schema}.${fk.from_table}` : fk.from_table
    const to = fk.to_schema ? `${fk.to_schema}.${fk.to_table}` : fk.to_table
    return {
      label: `${from}.${fk.from_column} = ${to}.${fk.to_column}`,
      type: "keyword",
      detail: "join key",
      boost: 3,
    }
  })
}

// ── What the caret is typing ────────────────────────────────────────────────

/**
 * What can be typed at the caret, decided from the statement text before the
 * current word:
 *  - "statement"  — nothing yet: SELECT, WITH, INSERT INTO, …
 *  - "table"      — right after FROM / JOIN / INTO / UPDATE / TABLE, or a comma in FROM
 *  - "afterTable" — after a table (and its alias): WHERE, JOIN, ORDER BY, …
 *  - "column"     — inside an expression: SELECT list, WHERE, ON, GROUP/ORDER BY, HAVING, SET
 *  - "any"        — none of the above; offer everything
 */
export type SqlSlot = "statement" | "table" | "afterTable" | "column" | "any"

/** Words directly followed by a table name. */
const TABLE_INTRODUCERS = new Set(["from", "join", "into", "update", "table"])
/** Clauses that list tables. */
const TABLE_CLAUSES = new Set(["from", "join", "into", "update"])
/** Clauses whose body is an expression over columns. */
const EXPRESSION_CLAUSES = new Set([
  "select", "where", "on", "and", "or", "by", "having", "set",
  "when", "then", "else", "case", "returning", "distinct",
])
/** Every word that opens a clause; the nearest one before the caret decides the slot. */
const CLAUSE_WORDS = new Set([
  ...TABLE_CLAUSES, ...EXPRESSION_CLAUSES,
  "limit", "offset", "values", "union", "group", "order", "partition", "with",
])

/** Functions whose arguments use FROM as an operand: EXTRACT(YEAR FROM ts). */
const FROM_ARG_FUNCTIONS = new Set(["extract", "substring", "trim", "overlay", "position"])

/**
 * Whether the clause word at `toks[i]` opens its clause here. FROM is an
 * operand in `IS DISTINCT FROM x` and inside EXTRACT(… FROM …); UPDATE names a
 * table only where it starts a statement, not in ON CONFLICT DO UPDATE, MERGE's
 * THEN UPDATE or FOR UPDATE.
 */
function opensClause(toks: string[], i: number): boolean {
  if (toks[i] === "from") {
    if (toks[i - 1] === "distinct") return false
    const open = openParenBefore(toks, i)
    return !(open > 0 && FROM_ARG_FUNCTIONS.has(toks[open - 1]))
  }
  if (toks[i] === "update") return i === 0 || [")", ";", "explain", "analyze"].includes(toks[i - 1])
  return true
}

/** The index of the innermost `(` still open before `end`, or -1. */
function openParenBefore(toks: string[], end: number): number {
  let depth = 0
  for (let i = end - 1; i >= 0; i--) {
    if (toks[i] === ")") depth++
    else if (toks[i] === "(") {
      if (depth === 0) return i
      depth--
    }
  }
  return -1
}

/**
 * Blank out string literals and comments, keeping every offset, so a `;` or a
 * keyword inside them is not read as SQL.
 */
export function maskLiteralsAndComments(sqlText: string): string {
  return sqlText.replace(/--[^\n]*|\/\*[\s\S]*?(?:\*\/|$)|'(?:[^']|'')*(?:'|$)/g, (m) =>
    " ".repeat(m.length),
  )
}

/** The `[from, to)` offsets of the `;`-separated statement holding `pos`. */
export function statementBounds(doc: string, pos: number): { from: number; to: number } {
  const masked = maskLiteralsAndComments(doc)
  const from = pos > 0 ? masked.lastIndexOf(";", pos - 1) + 1 : 0
  const next = masked.indexOf(";", pos)
  return { from, to: next < 0 ? doc.length : next }
}

/** Words, quoted identifiers, numbers and single punctuation marks, as written. */
function rawTokens(sqlText: string): string[] {
  const re = /"[^"]*"|`[^`]*`|\[[^\]]*\]|[A-Za-z_][\w$]*|\d[\w.]*|[(),.;*=<>!+\-/%]/g
  return maskLiteralsAndComments(sqlText).match(re) ?? []
}

/** Lowercased words, quoted identifiers as "ident", and single punctuation marks. */
function sqlTokens(sqlText: string): string[] {
  return rawTokens(sqlText).map((t) => (/^["`[]/.test(t) ? "ident" : /^\d/.test(t) ? "0" : t.toLowerCase()))
}

/**
 * The slot at the end of `before` — the statement text up to the current word.
 * Decided by the nearest clause word at the caret's own paren depth: a closed
 * group such as `(SELECT max(b) FROM t)` counts as one item of the clause
 * that holds it, and an unclosed `(` with no clause after it decides by what
 * precedes it — a subquery after FROM / JOIN / AS, a function's arguments
 * otherwise.
 */
export function sqlSlotAt(before: string): SqlSlot {
  const toks = sqlTokens(before)
  if (toks.length === 0) return "statement"
  const last = toks.length - 1
  const prev = toks[last]
  if (TABLE_INTRODUCERS.has(prev) && opensClause(toks, last)) return "table"

  const open = openParenBefore(toks, toks.length)
  let i = last
  for (let depth = 0; i > open; i--) {
    const t = toks[i]
    if (t === ")") depth++
    else if (t === "(") depth--
    else if (depth === 0 && CLAUSE_WORDS.has(t) && opensClause(toks, i)) break
  }
  if (i === open) {
    if (open < 0) return "any"
    const opener = toks[open - 1]
    const subquery = opener === "as" || ((opener === "from" || opener === "join") && opensClause(toks, open - 1))
    return subquery ? "statement" : "column"
  }
  const clause = toks[i]
  if (prev === "using" && (clause === "into" || clause === "from")) return "table"
  if (prev === "update") return "afterTable" // DO UPDATE / THEN UPDATE: SET follows
  if (TABLE_CLAUSES.has(clause)) {
    if (prev === "," && clause === "from") return "table"
    // Only names, dots, commas and closed groups since the clause word: a table
    // (and its alias, or an INSERT's column list) is done.
    let depth = 0
    const tableDone = toks.slice(i + 1).every((t) => {
      if (t === "(") depth++
      else if (t === ")") depth--
      return depth > 0 || t === "(" || t === ")" || t === "." || t === "," || /^[a-z_]/.test(t)
    })
    return tableDone ? "afterTable" : "any"
  }
  if (clause === "with" && prev === ")") return "statement" // the CTEs are done
  return EXPRESSION_CLAUSES.has(clause) ? "column" : "any"
}
