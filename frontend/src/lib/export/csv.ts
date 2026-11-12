/**
 * A table tile as CSV (dashboards-design.md § 8, D3): RFC 4180 quoting, CRLF between
 * rows, and a byte-order mark so Excel reads UTF-8. A text cell that a spreadsheet
 * would run as a formula (it starts with = + - @, a tab or a CR) gets a leading ',
 * unless it is a plain number (OWASP CSV injection); a number column is left as it is.
 * No DOM: this file is safe anywhere.
 */

const BOM = "\uFEFF"
const FORMULA_START = /^[=+\-@\t\r]/
const PLAIN_NUMBER = /^[-+]?\d+(\.\d+)?([eE][-+]?\d+)?$/

function cell(value: string, numeric: boolean): string {
  let v = value
  if (!numeric && FORMULA_START.test(v) && !PLAIN_NUMBER.test(v)) v = `'${v}`
  return /[",\r\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v
}

/** The table as CSV text; `numeric` marks the columns that hold numbers. */
export function toCsv(head: readonly string[], body: readonly (readonly string[])[], numeric: readonly boolean[] = []): string {
  const line = (cells: readonly string[], isHead: boolean) =>
    cells.map((c, i) => cell(c ?? "", !isHead && !!numeric[i])).join(",")
  return BOM + [line(head, true), ...body.map((row) => line(row, false))].join("\r\n") + "\r\n"
}
