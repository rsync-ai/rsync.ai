import { removeSqlComments } from "./statementClass"

/** The text matches a query makes (`status = 'DECLINED'`, `kind IN ('a', 'b')`,
 *  `name LIKE 'Acme%'`), as written, for the 0-row state: a text value the data
 *  does not hold exactly, case included, matches nothing. Ask rsync.ai's SQL
 *  writer sees names and types, never values, so its text values are guesses.
 *  Negated matches (`<>`, `NOT IN`) are left out: a wrong value there keeps rows.
 *  A heuristic over the SQL, not a parser: it only ever adds a hint. */

// A column: a plain, "quoted", `backticked` or [bracketed] name, dotted, or one
// function of it (LOWER(t.status)). NOT and END are words, not columns.
const IDENT = String.raw`(?:[A-Za-z_][\w$]*|"(?:[^"]|"")+"|\x60[^\x60]+\x60|\[[^\]]+\])`
const IDENTS = String.raw`${IDENT}(?:\s*\.\s*${IDENT})*`
const COLUMN = String.raw`(?!(?:NOT|END)\b)(?:[A-Za-z_]\w*\s*\(\s*${IDENTS}\s*\)|${IDENTS})`
const STRING = String.raw`'(?:[^']|'')*'`
const VALUE = String.raw`${STRING}|\(\s*${STRING}(?:\s*,\s*${STRING})*\s*\)`

// Each match is a text match or a text value skipped whole, so nothing inside
// a value is read as SQL.
const SCAN = new RegExp(
  String.raw`(?<![\w$."\x60\]])(${COLUMN})\s*(=|(?:I?LIKE|IN)\b)\s*(${VALUE})|${STRING}`,
  "gi",
)
const STRINGS = new RegExp(STRING, "g")

export function textFilters(sql: string | null | undefined, max = 5): string[] {
  const found: string[] = []
  for (const m of removeSqlComments(sql ?? "").matchAll(SCAN)) {
    if (!m[1]) continue
    const value = m[3].startsWith("(") ? `(${(m[3].match(STRINGS) ?? []).join(", ")})` : m[3]
    const text = `${m[1].replace(/\s+/g, " ")} ${m[2]} ${value}`
    if (!found.includes(text)) found.push(text)
    if (found.length === max) break
  }
  return found
}
