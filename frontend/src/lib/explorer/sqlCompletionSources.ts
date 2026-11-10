// CodeMirror completion-source assembly for the Data Explorer SQL editor.
// Kept separate from the React component so the source list can be unit-tested
// headlessly (no EditorView), and separate from the framework-agnostic
// `sqlCompletions.ts` because this module is intentionally CodeMirror-coupled.

import {
  ifNotIn,
  type CompletionContext,
  type CompletionResult,
  type CompletionSource,
} from "@codemirror/autocomplete"
import { MSSQL, MySQL, PLSQL, PostgreSQL, type SQLDialect } from "@codemirror/lang-sql"
import {
  type SchemaLookup,
  type ForeignKeyLike,
  parseTableAliases,
  columnCompletionsFor,
  tablesInSchema,
  wordCompletions,
  foreignKeyJoinHints,
  tableRefsInStatement,
  tableCompletions,
  joinCompletions,
  scopedColumnCompletions,
  statementBounds,
  sqlSlotAt,
} from "./sqlCompletions"
import { keywordsForSlot, multiWordKeywords, type KeywordDialect } from "./sqlKeywords"

// A dotted qualifier before the cursor: one or more (optionally quoted)
// identifier segments joined by dots, ending in `.<partial-word>`. Supports
// `alias.`, `table.`, `schema.table.`, and quoted ids (`"Users".` / `` `Orders`. ``).
const DOT_RE = /(?:"[^"]*"|`[^`]*`|[\w$]+)(?:\.(?:"[^"]*"|`[^`]*`|[\w$]+))*\.[\w$]*$/

// A SINGLE qualifier + dot immediately before the cursor — used to suppress the
// keyword source after any `ident.`/`schema.` (keywords are never valid right
// after a dot, and would otherwise bury the column/table list).
const AFTER_DOT_RE = /(?:"[^"]*"|`[^`]*`|[\w$]+)\.[\w$]*$/

// Syntax nodes where nothing is completed: typing inside a string or a comment is
// prose, not SQL. The keyword source also skips quoted identifiers.
const LITERAL_NODES = ["String", "LineComment", "BlockComment"]

/**
 * Build the schema-aware completion source. After a dot: the alias/table's
 * columns, or a schema's tables. Otherwise it depends on what the caret can be
 * typing (sqlSlotAt): tables after FROM/JOIN (with the foreign-key joins first
 * after JOIN), the statement's columns inside an expression (plus FK join-keys
 * after ON), nothing where only keywords fit, and tables + columns when the
 * position is unclear. Closes over the given lookup + foreign keys; returns null
 * when it has nothing to offer.
 */
export function createSchemaCompletionSource(
  lookup: SchemaLookup,
  foreignKeys: ForeignKeyLike[],
): CompletionSource {
  return ifNotIn(LITERAL_NODES, (ctx: CompletionContext): CompletionResult | null => {
    const dotMatch = ctx.matchBefore(DOT_RE)
    if (dotMatch) {
      const lastDot = dotMatch.text.lastIndexOf(".")
      const qualifier = dotMatch.text.slice(0, lastDot)
      const from = dotMatch.from + lastDot + 1 // first char past the final dot
      // The whole statement, not the text before the caret: `SELECT o.| FROM
      // orders o` declares its alias after the caret, and an alias from the
      // statement before a `;` is not in scope.
      const doc = ctx.state.doc.toString()
      const stmt = statementBounds(doc, ctx.pos)
      const aliases = parseTableAliases(doc.slice(stmt.from, stmt.to))

      const columns = columnCompletionsFor(qualifier, lookup, aliases)
      if (columns.length > 0) {
        return { from, options: columns, validFor: /^[\w$]*$/ }
      }
      // Not a table/alias — maybe a schema. Offer that schema's tables so
      // `public.` / `public._rs` completes table names, not SQL keywords (I1).
      const schemaTables = tablesInSchema(qualifier, lookup)
      if (schemaTables.length > 0) {
        return { from, options: schemaTables, validFor: /^[\w$]*$/ }
      }
      // Recognized a dot but resolved nothing — decline. The keyword source is
      // independently suppressed after a dot (see buildSqlCompletionSources),
      // so this yields an empty list rather than keyword garbage.
      return null
    }

    const wordMatch = ctx.matchBefore(/[\w$]*/)
    if (!wordMatch || (wordMatch.from === wordMatch.to && !ctx.explicit)) {
      return null
    }

    const doc = ctx.state.doc.toString()
    const stmt = statementBounds(doc, ctx.pos)
    const before = doc.slice(stmt.from, wordMatch.from)
    let options
    switch (sqlSlotAt(before)) {
      case "statement":
      case "afterTable":
        return null // only keywords fit here
      case "table":
        options = /\bjoin\s*$/i.test(before)
          ? [...joinCompletions(lookup, foreignKeys, before), ...tableCompletions(lookup)]
          : tableCompletions(lookup)
        break
      case "column":
        options = scopedColumnCompletions(lookup, doc.slice(stmt.from, stmt.to)) ?? wordCompletions(lookup)
        break
      default:
        options = wordCompletions(lookup)
    }

    // Right after `ON` in a JOIN, surface FK join-key equalities first — scoped
    // to the tables actually referenced in this statement (I7).
    const upToCaret = doc.slice(stmt.from, ctx.pos)
    if (foreignKeys.length > 0 && /\bjoin\b/i.test(upToCaret) && /\bon\s+[\w.]*$/i.test(upToCaret)) {
      options = [...foreignKeyJoinHints(foreignKeys, tableRefsInStatement(upToCaret)), ...options]
    }
    return { from: wordMatch.from, options, validFor: /^[\w$]*$/ }
  })
}

/** Our keyword flavour for CodeMirror's parser dialect (see codeMirrorDialect). */
function keywordDialectOf(dialect: SQLDialect): KeywordDialect {
  if (dialect === PostgreSQL) return "postgresql"
  if (dialect === MySQL) return "mysql"
  if (dialect === MSSQL) return "sqlserver"
  if (dialect === PLSQL) return "oracle"
  return "generic"
}

/**
 * Assemble the completion sources passed to `autocompletion({ override })`.
 *
 * `override` REPLACES the "autocomplete" language-data sources, so the keyword
 * completer `sql()` registers is NOT used unless we add one back. We pair the
 * caller's schema-aware source with our own keyword source, which:
 *  - offers a curated list (sqlKeywords.ts), not the dialect's ~700 highlighter
 *    words (command_function_code, …);
 *  - offers only the words that can follow the caret's position (sqlSlotAt), and
 *    none where only a table fits;
 *  - completes a two-word keyword over the word already typed (`ORDER b` →
 *    `ORDER BY`);
 *  - stays silent right after `ident.`/`schema.`, where keywords are never valid
 *    and would bury the column/table list (the `public._rs` →
 *    PRINT_STRICT_PARAMS… bug, I1b), and inside strings, comments and quoted
 *    identifiers.
 * Labels are upper case, matching the editor's `sql({ upperCaseKeywords: true })`.
 */
export function buildSqlCompletionSources(
  schemaSource: CompletionSource,
  dialect: SQLDialect,
): CompletionSource[] {
  const kd = keywordDialectOf(dialect)
  const phrases = multiWordKeywords(kd)
  const keywords: CompletionSource = ifNotIn([...LITERAL_NODES, "QuotedIdentifier"], (ctx) => {
    if (ctx.matchBefore(AFTER_DOT_RE)) return null

    // `ORDER b` → `ORDER BY`: replace both words. These are the only words that
    // can follow, so they are shown in this order, ahead of everything else.
    const phrase = ctx.matchBefore(/\b[A-Za-z]+\s+[A-Za-z]*$/)
    if (phrase) {
      const typed = phrase.text.replace(/\s+/g, " ").toUpperCase()
      const hits = phrases.filter((o) => o.label.startsWith(typed))
      if (hits.length > 0) return { from: phrase.from, options: hits, filter: false }
    }

    const word = ctx.matchBefore(/\w*/)
    if (!word || (word.from === word.to && !ctx.explicit)) return null
    const doc = ctx.state.doc.toString()
    const before = doc.slice(statementBounds(doc, ctx.pos).from, word.from)
    const options = keywordsForSlot(sqlSlotAt(before), kd)
    if (options.length === 0) return null
    return { from: word.from, options, validFor: /^\w*$/ }
  })
  return [schemaSource, keywords]
}
