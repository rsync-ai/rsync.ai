"""SQL literal placeholders (src/egress/sql_placeholders.py).

The class under test: a value inside SQL reaching an LLM. Every literal form
each dialect's tokenizer knows must come out as a placeholder, and SQL the
model hands back must restore exactly or be refused.
"""

import re

import pytest

from src.egress.sql_placeholders import (
    LiteralMap,
    SqlPlaceholderError,
    placeholder_sql,
    restore_sql_literals,
)

_VALUE_RE = re.compile(r"Smith|4711|ab12|0101|deadbeef|secret|1_000|2024-01-01|7 days")


def _ph(sql, dialect="postgres"):
    m = LiteralMap()
    return placeholder_sql(sql, dialect, m), m


@pytest.mark.parametrize(
    "dialect,sql",
    [
        ("postgres", "SELECT * FROM t WHERE name = 'Smith' AND id = 4711"),
        ("postgres", "SELECT E'Sm\\'ith', U&'Smith', B'0101', X'deadbeef', N'Smith', $x$secret$x$ FROM t"),
        ("postgres", "SELECT * FROM t WHERE d = DATE '2024-01-01' AND i > INTERVAL '7 days'"),
        ("mysql", "SELECT * FROM t WHERE a = \"Smith\" AND b = 0xdeadbeef AND c = 1_000"),
        ("bigquery", "SELECT r'Smith', b'secret', \"Smith\", '''Smith''', 1_000 FROM `p.d.t`"),
        ("tsql", "SELECT N'Smith', 0xdeadbeef FROM [t] WHERE [c] = 4711"),
        ("snowflake", "SELECT $$secret$$, 'Smith' FROM t"),
        ("spark", "SELECT X'deadbeef', 4711L, 'Smith' FROM t"),
        ("clickhouse", "SELECT 'Smith', 4711 FROM t"),
    ],
)
def test_no_literal_survives(dialect, sql):
    out, m = _ph(sql, dialect)
    assert not _VALUE_RE.search(out), out
    assert len(m) > 0
    # Control: the probe regex does find the values in the input.
    assert _VALUE_RE.search(sql)


def test_round_trip_restores_exact_literals():
    sql = "SELECT a, 'x y' AS l, -7 FROM t WHERE name IN ('O''Brien', 'Smith') AND n > 1e5"
    out, m = _ph(sql)
    assert "O''Brien" not in out and "1e5" not in out
    assert restore_sql_literals(out, m, "postgres") == sql


def test_comments_are_dropped():
    out, _ = _ph("SELECT a -- the secret is Smith\nFROM t /* 4711 */ WHERE b = 1")
    assert "Smith" not in out and "4711" not in out
    assert out == "SELECT a FROM t WHERE b = {{L1:num}}"


def test_identifiers_stay_readable():
    out, _ = _ph('SELECT "Customer Name" FROM "Orders" o JOIN items i ON o.id = i.order_id')
    assert out == 'SELECT "Customer Name" FROM "Orders" o JOIN items i ON o.id = i.order_id'


def test_date_parts_and_formats_stay_readable():
    out, m = _ph("SELECT DATE_TRUNC('month', ts), TO_CHAR(ts, 'YYYY-MM'), 'Smith' FROM t")
    assert "'month'" in out and "'YYYY-MM'" in out and "Smith" not in out
    assert len(m) == 1
    out, _ = _ph("SELECT DATE_FORMAT(ts, '%Y-%m') FROM t", "mysql")
    assert "'%Y-%m'" in out


@pytest.mark.parametrize("value", ["MM Smith", "DDXX", "month 4711", "Q3 target", "a" * 40])
def test_near_formats_are_still_placeholdered(value):
    out, _ = _ph(f"SELECT '{value}' FROM t")
    assert value not in out


def test_numbering_is_shared_across_fields():
    m = LiteralMap()
    a = placeholder_sql("SELECT 1", "postgres", m)
    b = placeholder_sql("SELECT 'x'", "postgres", m)
    assert a == "SELECT {{L1:num}}" and b == "SELECT {{L2:str}}"


def test_model_may_quote_a_string_placeholder_or_drop_one():
    _, m = _ph("SELECT * FROM t WHERE a = 'Smith' AND b = 4711")
    assert restore_sql_literals("SELECT * FROM t WHERE a = '{{L1:str}}'", m, "postgres") == (
        "SELECT * FROM t WHERE a = 'Smith'"
    )
    assert restore_sql_literals("SELECT * FROM t WHERE b >= {{ L2 }}", m, "postgres") == (
        "SELECT * FROM t WHERE b >= 4711"
    )


@pytest.mark.parametrize(
    "returned",
    [
        "SELECT * FROM t WHERE a = {{L9:str}}",  # never issued
        "SELECT * FROM t WHERE a = {{L1:num}}",  # type changed
        "SELECT * FROM t WHERE a = '%{{L1:str}}%'",  # inside a string
        'SELECT * FROM t WHERE "{{L1:str}}" = 1',  # inside an identifier
        "SELECT * FROM t -- {{L1:str}}",  # inside a comment
        "SELECT * FROM t WHERE a = x{{L1:str}}",  # glued to a name
        "SELECT * FROM t WHERE a = '{{L2:num}}'",  # a number quoted as a string
        "SELECT * FROM t WHERE a = {{L1:str}",  # malformed
        "SELECT * FROM t WHERE a = __RSYNC_L1__",  # reserved marker
        "SELECT * FROM t WHERE a = {{L1:str}} AND b = 'unterminated",  # untokenizable
    ],
)
def test_restore_refuses_what_it_cannot_place(returned):
    _, m = _ph("SELECT * FROM t WHERE a = 'Smith' AND b = 4711")
    with pytest.raises(SqlPlaceholderError):
        restore_sql_literals(returned, m, "postgres")


def test_restore_without_placeholders_is_identity():
    assert restore_sql_literals("SELECT 'new' FROM t", LiteralMap(), "postgres") == "SELECT 'new' FROM t"


@pytest.mark.parametrize("dialect", ["", None, "no-such-dialect"])
def test_unknown_dialect_is_refused(dialect):
    with pytest.raises(SqlPlaceholderError):
        placeholder_sql("SELECT 'Smith'", dialect, LiteralMap())


def test_untokenizable_sql_is_refused():
    with pytest.raises(SqlPlaceholderError):
        placeholder_sql("SELECT 'Smith", "postgres", LiteralMap())


@pytest.mark.parametrize("dialect", ["postgres", "mysql", "bigquery", "tsql", "snowflake", "databricks", "redshift"])
def test_round_trip_per_dialect(dialect):
    sql = "SELECT a, COUNT(*) FROM t WHERE b = 'Smith' AND c > 4711 GROUP BY a LIMIT 10"
    out, m = _ph(sql, dialect)
    assert "Smith" not in out and "4711" not in out
    assert restore_sql_literals(out, m, dialect) == sql
