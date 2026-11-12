"""Column lineage read out of a model's SQL (src/lineage/columns.py) and its route.

Lineage design step L1, docs/internal/lineage-catalog-design.md § 3. Each case is a
model shape rsync.ai runs: the CDC dedup it writes over a bronze table, a BigQuery
model with a CTE, joins and aggregates, INSERT and CREATE TABLE AS, a window. The
edges are the OpenLineage column lineage facet's types, so a wrong subtype here is a
wrong facet in the export.

The rule the cases guard: an answer is never wrong, only partial. A column that
cannot be traced says so in ``reason`` and turns the status to partial; it is never
pinned on a table it may not come from.
"""

from __future__ import annotations

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.gateway import lineage_routes
from src.lineage import columns as col
from src.lineage.columns import SourceTable as T
from src.lineage.columns import model_column_lineage

SECRET = "unit-test-internal-secret"


def _edges(res):
    return {(".".join(e.table), e.column, e.to_column, e.type, e.subtype) for e in res.edges}


def test_cdc_dedup_keeps_a_quoted_mixed_case_column_and_reads_the_window_as_indirect():
    sql = (
        'SELECT "id","Status", amount * 2 AS dbl FROM (SELECT t.*, ROW_NUMBER() OVER '
        '(PARTITION BY "id" ORDER BY "_rsync_cdc_seq" DESC) AS rsync_rn FROM "public"."orders" t) '
        "rsync_latest WHERE rsync_latest.rsync_rn = 1"
    )
    res = model_column_lineage(sql, "postgresql", [T("public", "orders", ("id", "Status", "amount", "_rsync_cdc_seq"))])
    assert res.status == col.STATUS_OK, res.reason
    assert res.columns == ["id", "Status", "dbl"]
    assert _edges(res) == {
        ("public.orders", "id", "id", col.DIRECT, col.IDENTITY),
        ("public.orders", "Status", "Status", col.DIRECT, col.IDENTITY),
        ("public.orders", "amount", "dbl", col.DIRECT, col.TRANSFORMATION),
        ("public.orders", "id", "", col.INDIRECT, col.WINDOW),
        ("public.orders", "_rsync_cdc_seq", "", col.INDIRECT, col.WINDOW),
    }


def test_bigquery_cte_joins_and_aggregates():
    sql = """WITH tx AS (SELECT card_id, amount, status FROM `proj.fintech_bronze.transactions` WHERE status != 'x')
SELECT c.customer_id, DATE(t.created) AS day, SUM(tx.amount) AS spend,
       COUNTIF(tx.status='declined')/COUNT(*) AS decline_rate
FROM tx JOIN `proj.fintech_bronze.cards` c USING (card_id)
JOIN `proj.fintech_bronze.transactions` t USING (card_id) GROUP BY 1,2"""
    tables = [
        T("fintech_bronze", "transactions", ("card_id", "amount", "status", "created")),
        T("fintech_bronze", "cards", ("card_id", "customer_id")),
    ]
    res = model_column_lineage(sql, "bigquery", tables)
    assert res.status == col.STATUS_OK, res.reason
    assert res.columns == ["customer_id", "day", "spend", "decline_rate"]
    tx, cards = "proj.fintech_bronze.transactions", "proj.fintech_bronze.cards"
    assert _edges(res) == {
        (cards, "customer_id", "customer_id", col.DIRECT, col.IDENTITY),
        (tx, "created", "day", col.DIRECT, col.TRANSFORMATION),
        (tx, "amount", "spend", col.DIRECT, col.AGGREGATION),
        (tx, "status", "decline_rate", col.DIRECT, col.AGGREGATION),
        (tx, "status", "", col.INDIRECT, col.FILTER),
        (cards, "customer_id", "", col.INDIRECT, col.GROUP_BY),
        (tx, "created", "", col.INDIRECT, col.GROUP_BY),
        (cards, "card_id", "", col.INDIRECT, col.JOIN),
        (tx, "card_id", "", col.INDIRECT, col.JOIN),
    }


def test_star_expands_with_the_tables_columns_and_is_partial_without_them():
    with_schema = model_column_lineage("SELECT * FROM analytics.orders", "postgresql", [T("analytics", "orders", ("id", "total"))])
    assert with_schema.status == col.STATUS_OK
    assert with_schema.columns == ["id", "total"]
    assert {(e.column, e.to_column) for e in with_schema.edges} == {("id", "id"), ("total", "total")}

    without = model_column_lineage("SELECT * FROM analytics.orders", "postgresql", [])
    assert without.status == col.STATUS_PARTIAL
    assert without.edges == []
    assert "SELECT *" in without.reason


def test_a_bare_column_in_a_join_is_never_guessed():
    sql = "SELECT o.id, amount FROM orders o JOIN customers c ON c.id = o.customer_id"
    unknown = model_column_lineage(sql, "postgresql", [])
    assert unknown.status == col.STATUS_PARTIAL
    assert unknown.reason == "amount reads a column no table could be found for"
    assert not any(e.to_column == "amount" for e in unknown.edges)
    filtered = model_column_lineage(sql + " WHERE region = 'EU'", "postgresql", [])
    assert not any(e.column == "region" for e in filtered.edges)

    known = model_column_lineage(
        sql, "postgresql", [T("public", "orders", ("id", "customer_id")), T("public", "customers", ("id", "amount"))]
    )
    assert known.status == col.STATUS_OK
    assert ("customers", "amount", "amount", col.DIRECT, col.IDENTITY) in _edges(known)


def test_insert_maps_its_named_target_columns():
    sql = (
        "INSERT INTO mart.daily (day, total) "
        "SELECT date_trunc('day', created_at), sum(amount) FROM public.payments GROUP BY 1"
    )
    res = model_column_lineage(sql, "postgresql", [])
    assert res.status == col.STATUS_OK
    assert res.columns == ["day", "total"]
    assert _edges(res) == {
        ("public.payments", "created_at", "day", col.DIRECT, col.TRANSFORMATION),
        ("public.payments", "amount", "total", col.DIRECT, col.AGGREGATION),
        ("public.payments", "created_at", "", col.INDIRECT, col.GROUP_BY),
    }


def test_create_table_as_gives_literals_no_source():
    res = model_column_lineage("CREATE TABLE mart.x AS SELECT id, 1 AS one, now() AS t FROM public.payments", "postgresql", [])
    assert res.status == col.STATUS_OK
    assert res.columns == ["id", "one", "t"]
    assert _edges(res) == {("public.payments", "id", "id", col.DIRECT, col.IDENTITY)}


def test_a_window_partition_column_filters_rows_it_does_not_feed_the_value():
    sql = 'SELECT "Status" AS "MixedOut", SUM(amount) OVER (PARTITION BY region) AS running FROM public.orders'
    res = model_column_lineage(sql, "postgresql", [T("public", "orders", ("Status", "amount", "region"))])
    assert res.columns == ["MixedOut", "running"]
    assert _edges(res) == {
        ("public.orders", "Status", "MixedOut", col.DIRECT, col.IDENTITY),
        ("public.orders", "amount", "running", col.DIRECT, col.AGGREGATION),
        ("public.orders", "region", "", col.INDIRECT, col.WINDOW),
    }


def test_snowflake_upper_case_names_and_qualify():
    sql = (
        "SELECT o.total, d.day FROM analytics.orders_daily o JOIN analytics.day_dim d USING (day) "
        "QUALIFY ROW_NUMBER() OVER (PARTITION BY d.day ORDER BY o.total) = 1"
    )
    tables = [T("ANALYTICS", "ORDERS_DAILY", ("TOTAL", "DAY")), T("ANALYTICS", "DAY_DIM", ("DAY",))]
    res = model_column_lineage(sql, "snowflake", tables)
    assert res.status == col.STATUS_OK
    assert res.columns == ["TOTAL", "DAY"]
    edges = _edges(res)
    assert ("ANALYTICS.ORDERS_DAILY", "TOTAL", "TOTAL", col.DIRECT, col.IDENTITY) in edges
    assert ("ANALYTICS.DAY_DIM", "DAY", "DAY", col.DIRECT, col.IDENTITY) in edges
    assert ("ANALYTICS.DAY_DIM", "DAY", "", col.INDIRECT, col.FILTER) in edges


def test_a_union_reads_both_sides():
    res = model_column_lineage("SELECT a.id FROM a UNION ALL SELECT b.id FROM b", "postgresql", [])
    assert res.status == col.STATUS_OK
    assert _edges(res) == {
        ("a", "id", "id", col.DIRECT, col.IDENTITY),
        ("b", "id", "id", col.DIRECT, col.IDENTITY),
    }


@pytest.mark.parametrize(
    "sql, connector_type, status, reason",
    [
        ("UPDATE x SET a = 1", "postgresql", col.STATUS_UNSUPPORTED, "a UPDATE statement builds no columns to trace"),
        ("SELECT 1; SELECT 2", "postgresql", col.STATUS_UNSUPPORTED, "column lineage reads a model of exactly one statement"),
        ("[]", "mongodb", col.STATUS_UNSUPPORTED, "no SQL dialect for a mongodb connection"),
    ],
)
def test_what_builds_no_columns_says_why(sql, connector_type, status, reason):
    res = model_column_lineage(sql, connector_type, [])
    assert (res.status, res.reason, res.edges) == (status, reason, [])


def test_sql_that_cannot_be_read_fails_with_a_plain_reason():
    res = model_column_lineage("SELEC broken FROM", "postgresql", [])
    assert res.status == col.STATUS_FAILED
    assert res.reason.startswith("the SQL could not be read: ")
    assert "\x1b[" not in res.reason
    assert len(res.reason) <= 300


def test_edges_stop_at_the_cap_and_say_so(monkeypatch):
    monkeypatch.setattr(col, "MAX_EDGES", 2)
    res = model_column_lineage("SELECT a, b, c FROM public.t", "postgresql", [])
    assert len(res.edges) == 2
    assert res.truncated is True


# ---- the route ------------------------------------------------------------------


@pytest.fixture
def api(monkeypatch):
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    app = FastAPI()
    app.include_router(lineage_routes.router)
    return TestClient(app)


def test_route_needs_the_internal_secret(api):
    body = {"models": [{"id": "m1", "sql": "SELECT 1", "connector_type": "postgresql"}]}
    assert api.post("/v1/lineage/columns", json=body).status_code == 401
    assert api.post("/v1/lineage/columns", json=body, headers={"X-Internal-Secret": "wrong"}).status_code == 401


def test_route_answers_each_model_in_order_and_one_failure_is_its_own(api, monkeypatch):
    real = lineage_routes.model_column_lineage

    def flaky(sql, connector_type, tables):
        if "boom" in sql:
            raise RuntimeError("parser bug")
        return real(sql, connector_type, tables)

    monkeypatch.setattr(lineage_routes, "model_column_lineage", flaky)
    body = {
        "models": [
            {
                "id": "11111111-aaaa",
                "sql": "SELECT id FROM orders",
                "connector_type": "postgresql",
                "tables": [{"schema": "public", "name": "orders", "columns": ["id"]}],
            },
            {"id": "22222222-bbbb", "sql": "SELECT boom", "connector_type": "postgresql"},
        ]
    }
    r = api.post("/v1/lineage/columns", json=body, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 200
    first, second = r.json()["models"]
    assert first["id"] == "11111111-aaaa"
    assert first["status"] == "ok"
    assert first["edges"] == [
        {"table": ["orders"], "column": "id", "to_column": "id", "type": "DIRECT", "subtype": "IDENTITY"}
    ]
    assert second == {
        "id": "22222222-bbbb",
        "status": "failed",
        "columns": [],
        "edges": [],
        "reason": "column lineage could not read this model",
        "truncated": False,
    }


@pytest.mark.parametrize(
    "body",
    [
        {"models": [{"id": "../x", "sql": "SELECT 1"}]},
        {"models": [{"id": "m", "sql": "x" * 200_001}]},
        {"models": [{"id": "m", "sql": "SELECT 1", "extra": 1}]},
        {"models": [{"id": f"m{i}", "sql": "SELECT 1"} for i in range(lineage_routes.MAX_MODELS + 1)]},
    ],
)
def test_route_refuses_a_body_past_its_limits(api, body):
    assert api.post("/v1/lineage/columns", json=body, headers={"X-Internal-Secret": SECRET}).status_code == 422
