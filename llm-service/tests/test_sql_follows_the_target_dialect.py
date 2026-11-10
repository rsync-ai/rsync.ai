"""SQL written for one engine is checked against that engine's own syntax.

Found asking Ask rsync.ai about a BigQuery warehouse (2026-10-09): the SQL came
back with PostgreSQL's `->>` and `::json`, which BigQuery rejects. sqlglot parses
both in every dialect, so a parse check never sees them; only the source-dialect
markers in `_detect_source_dialect` decide whether the SQL is transpiled.

Bug class: **a dialect check that knows only PostgreSQL and MySQL.**
- `->>` was no marker, so it went through to every engine.
- `date_trunc(` was one for every engine, so BigQuery's own
  `DATE_TRUNC(created_at, MONTH)` was read as PostgreSQL and came out as
  `TIMESTAMP_TRUNC(MONTH, CREATED_AT)`, arguments swapped.
- The time-grain check knew `DATE_TRUNC('month', col)` only for PostgreSQL, so a
  correct BigQuery, Snowflake, Databricks, ClickHouse or SQL Server month bucket
  was "repaired" with one more model call and a false warning.
"""

from __future__ import annotations

import pytest
import sqlglot

from src.gateway.main import _maybe_transpile_sql, _sql_satisfies_time_grain
from src.text2sql.compiler import resolve_dialect

PG_JSON = "SELECT after_json->>'severity' AS severity, COUNT(*) AS n FROM fintech_bronze.fraud_alerts GROUP BY 1"


def test_control_sqlglot_accepts_postgres_json_in_every_dialect():
    # Why a marker is needed at all: a parse check passes this everywhere.
    for d in ("bigquery", "snowflake", "databricks", "clickhouse", "tsql", "oracle"):
        sqlglot.parse_one(PG_JSON, dialect=d)


@pytest.mark.parametrize(
    "target,expect",
    [
        ("bigquery", "JSON_EXTRACT_SCALAR(after_json, '$.severity')"),
        ("snowflake", "JSON_EXTRACT_PATH_TEXT(after_json, 'severity')"),
        ("databricks", "GET_JSON_OBJECT(after_json, '$.severity')"),
        ("clickhouse", "JSONExtractString(after_json, 'severity')"),
        ("tsql", "JSON_VALUE(after_json, '$.severity')"),
        ("oracle", "JSON_EXTRACT_SCALAR(after_json, '$.severity')"),
    ],
)
def test_postgres_json_operators_are_rewritten_for_another_engine(target, expect):
    out = _maybe_transpile_sql(PG_JSON, target)
    assert "->>" not in out, out
    assert expect in out, out


@pytest.mark.parametrize("target", ["postgresql", "mysql"])
def test_engines_with_json_arrows_keep_them(target):
    assert _maybe_transpile_sql(PG_JSON, target) == PG_JSON


@pytest.mark.parametrize(
    "sql",
    [
        "SELECT DATE_TRUNC(created_at, MONTH) AS m, COUNT(*) AS n FROM t GROUP BY 1",
        "SELECT TIMESTAMP_TRUNC(created_at, MONTH) AS m, COUNT(*) AS n FROM t GROUP BY 1",
        "SELECT JSON_VALUE(PARSE_JSON(after_json).document.severity) AS s FROM t",
    ],
)
def test_correct_bigquery_is_left_as_written(sql):
    assert _maybe_transpile_sql(sql, "bigquery") == sql


def test_postgres_date_trunc_is_still_rewritten_for_bigquery():
    out = _maybe_transpile_sql("SELECT DATE_TRUNC('month', created_at) AS m FROM t", "bigquery")
    assert out == "SELECT TIMESTAMP_TRUNC(created_at, MONTH) AS m FROM t"


def test_a_lambda_is_not_a_json_operator():
    # Databricks, ClickHouse and Snowflake write a lambda with ->.
    sql = "SELECT TRANSFORM(items, x -> x + 1) AS items FROM t"
    assert _maybe_transpile_sql(sql, "databricks") == sql


@pytest.mark.parametrize(
    "dialect,sql",
    [
        ("bigquery", "SELECT TIMESTAMP_TRUNC(created_at, MONTH) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("bigquery", "SELECT DATE_TRUNC(created_at, MONTH) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("bigquery", "SELECT DATETIME_TRUNC(created_at, MONTH) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("snowflake", "SELECT DATE_TRUNC('month', created_at) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("databricks", "SELECT DATE_TRUNC('MONTH', created_at) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("clickhouse", "SELECT toStartOfMonth(created_at) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("tsql", "SELECT DATETRUNC(month, created_at) AS m, COUNT(*) FROM t GROUP BY DATETRUNC(month, created_at)"),
        ("postgresql", "SELECT DATE_TRUNC('month', created_at) AS m, COUNT(*) FROM t GROUP BY 1"),
        ("mysql", "SELECT DATE_FORMAT(created_at, '%Y-%m-01') AS m, COUNT(*) FROM t GROUP BY 1"),
    ],
)
def test_each_engines_month_bucket_counts_as_monthly(dialect, sql):
    sqlglot.parse_one(sql, dialect=resolve_dialect(dialect))
    assert _sql_satisfies_time_grain(sql, dialect, "month", "created_at")


@pytest.mark.parametrize("dialect", ["bigquery", "snowflake", "clickhouse", "postgresql"])
def test_a_day_bucket_is_not_monthly(dialect):
    # Control: the check still says no.
    assert not _sql_satisfies_time_grain("SELECT DATE(created_at) AS d, COUNT(*) FROM t GROUP BY 1", dialect, "month", "created_at")


@pytest.mark.parametrize(
    "dialect,sql",
    [
        ("bigquery", "SELECT TIMESTAMP_TRUNC(created_at, DAY) AS d FROM t"),
        ("bigquery", "SELECT TIMESTAMP_TRUNC(created_at, YEAR) AS d FROM t"),
        ("snowflake", "SELECT DATE_TRUNC('year', created_at) AS d FROM t"),
        ("clickhouse", "SELECT toStartOfYear(created_at) AS d FROM t"),
    ],
)
def test_the_grain_must_match(dialect, sql):
    # A day or year bucket is not a month bucket, whatever function writes it.
    assert not _sql_satisfies_time_grain(sql, dialect, "month", "created_at")
