import { describe, it, expect } from "vitest"
import { textFilters } from "../textFilters"

// Bug class: **a query that filters on a text value the data does not hold
// returns 0 rows and says nothing about why.** Found in the 2026-10-11 Ask
// eval: the SQL writer sees names and types, never values, and wrote
// status = 'DECLINED' (stored as 'declined') and txn_type = 'card' (stored as
// 'card_purchase'). The Explorer's 0-row state now names each text match in
// the SQL that ran, so the person checks those values first.

describe("textFilters", () => {
  it("names each text match a 0-row query made, as written", () => {
    const sql = `SELECT m.merchant_name, COUNT(*) AS declined
      FROM fintech_silver.transactions t
      JOIN fintech_silver.merchants m ON m.merchant_id = t.merchant_id
      WHERE t.status = 'DECLINED' AND t.txn_type = 'card'
      GROUP BY m.merchant_name ORDER BY declined DESC LIMIT 10`
    expect(textFilters(sql)).toEqual(["t.status = 'DECLINED'", "t.txn_type = 'card'"])
  })

  it.each([
    ["IN", "WHERE status IN ('DECLINED', 'FAILED')", "status IN ('DECLINED', 'FAILED')"],
    ["LIKE", "WHERE name LIKE 'Acme%'", "name LIKE 'Acme%'"],
    ["ILIKE", "WHERE name ilike 'acme%'", "name ilike 'acme%'"],
    ["a function of the column", "WHERE LOWER(t.status) = 'declined'", "LOWER(t.status) = 'declined'"],
    ["a quoted column", `WHERE "Txn Type" = 'card'`, `"Txn Type" = 'card'`],
    ["a backticked column", "WHERE `t`.`status` = 'x'", "`t`.`status` = 'x'"],
    ["an escaped quote", "WHERE note = 'it''s'", "note = 'it''s'"],
    ["text across lines", "WHERE status\n  =\n  'DECLINED'", "status = 'DECLINED'"],
    ["only the outer match of a value holding one", "WHERE note = 'status = ''x'''", "note = 'status = ''x'''"],
  ])("finds %s", (_, sql, want) => {
    expect(textFilters(`SELECT * FROM t ${sql}`)).toEqual([want])
  })

  it.each([
    ["a number", "WHERE amount = 100"],
    ["a range", "WHERE created_at >= '2026-01-01'"],
    ["a negated match", "WHERE status <> 'x' AND status != 'y' AND name NOT LIKE 'a%' AND kind NOT IN ('b')"],
    ["a column compared to a column", "WHERE a.id = b.id"],
    ["a subquery", "WHERE id IN (SELECT id FROM u)"],
    ["a list with a number in it", "WHERE code IN ('a', 1)"],
    ["a comparison in a comment", "-- WHERE status = 'x'\nWHERE 1 = 1 /* AND kind = 'y' */"],
  ])("skips %s", (_, sql) => {
    expect(textFilters(`SELECT * FROM t ${sql}`)).toEqual([])
  })

  it("names a repeated match once, and at most five", () => {
    const sql = "SELECT * FROM t WHERE a = 'x' OR a = 'x' OR b = '1' OR c = '2' OR d = '3' OR e = '4' OR f = '5'"
    expect(textFilters(sql)).toEqual(["a = 'x'", "b = '1'", "c = '2'", "d = '3'", "e = '4'"])
  })

  it("finds nothing in no SQL", () => {
    expect(textFilters("")).toEqual([])
    expect(textFilters(null)).toEqual([])
  })
})
