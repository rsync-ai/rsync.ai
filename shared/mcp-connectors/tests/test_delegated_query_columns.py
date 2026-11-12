"""A delegated warehouse's query result names its columns, in SELECT order
(KI-DELEGATED-QUERY-COLUMN-ORDER).

Bug class: a delegated query result without a ``columns`` list takes its column
order from a Go map. The Explorer, a chart's column list, a workflow's email
table and a CSV download all read a warehouse the gateway has no driver for
through the connector's ``export`` (shared/go/sqlexec engine.go
``delegatedEngine``). The rows cross into Go as ``map[string]interface{}``,
which has no order, so the only order that survives is the connector's own
``columns`` list. BigQuery sent none: its columns came back in a new random
order on every run, and a query that matched no rows came back with no columns
at all.

Every delegated SQL warehouse is driven through its real export in query mode,
against SQLite standing in for the engine. The list of warehouses is read from
engine.go, so a new delegated engine fails here until it is added.
"""
from __future__ import annotations

import importlib.util
import json
import os
import re
import sqlite3
import sys
import types

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_PUBLIC = os.path.abspath(os.path.join(_HERE, "..", "public"))
_ENGINE_GO = os.path.abspath(os.path.join(_HERE, "..", "..", "go", "sqlexec", "engine.go"))

# name -> (connector root, server class, connection config); BigQuery's export
# is the shared warehouse adapter.
CONNECTORS = {
    "bigquery": (None, None, {"project_id": "proj", "dataset_id": "ds"}),
    "clickhouse": (os.path.join(_PUBLIC, "database", "clickhouse"), "ClickHouseMCPServer",
                   {"host": "h", "database": "demo", "user": "u", "password": "p"}),
    "snowflake": (os.path.join(_PUBLIC, "database", "snowflake"), "SnowflakeMCPServer",
                  {"account": "a", "user": "u", "password": "p", "schema": "PUBLIC"}),
    "oracle": (os.path.join(_PUBLIC, "database", "oracle"), "OracleMCPServer",
               {"host": "h", "port": 1521, "service_name": "s", "user": "u", "password": "p"}),
}
# MongoDB is delegated too, but it is a document store: its callers browse it
# through the connector's find tool, never through a SQL read.
NOT_SQL = {"mongo"}

# Neither the table's order nor sorted order, so neither can pass by accident.
SELECT = "SELECT zeta, alpha, mid FROM t"
SELECTED = ["zeta", "alpha", "mid"]


def _delegated_engines() -> set:
    """The connector types engine.go's For() hands to delegatedEngine."""
    with open(_ENGINE_GO) as f:
        src = f.read()
    case = re.search(r"case ([^:]*?):\s*(?://[^\n]*\n\s*)*return delegatedEngine", src, re.S)
    assert case, "engine.go no longer returns delegatedEngine from a case of For()"
    return set(re.findall(r'strings\.Contains\(ct, "([^"]+)"\)', case.group(1)))


def test_every_delegated_sql_warehouse_is_covered():
    assert _delegated_engines() - NOT_SQL == set(CONNECTORS)


def _connector_file(root: str) -> str:
    with open(os.path.join(root, "latest.json")) as f:
        current = json.load(f)["current_version"]
    return os.path.join(root, "versions", current, "connector.py")


_LOADED: dict = {}


def _load(name: str):
    """Import one connector in isolation (its version dir carries its own
    `base_connector.py`); sys.path and sys.modules are put back afterwards."""
    if name in _LOADED:
        return _LOADED[name]
    root, class_name = CONNECTORS[name][:2]
    if root is None:
        spec = importlib.util.spec_from_file_location(
            "_delegated_columns_adapters", os.path.join(_PUBLIC, "warehouse_adapters.py"))
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        _LOADED[name] = mod.BigQueryWarehouseAdapter
        return _LOADED[name]
    path = _connector_file(root)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [os.path.dirname(path), root, _PUBLIC]
    spec = importlib.util.spec_from_file_location(f"_delegated_columns_{name}", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    except Exception as exc:  # pragma: no cover - environment-dependent
        pytest.skip(f"{name} connector not importable offline: {exc}")
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    _LOADED[name] = getattr(mod, class_name)
    return _LOADED[name]


# --------------------------------------------------------------------------
# The engine: one table t(alpha, mid, zeta) in SQLite
# --------------------------------------------------------------------------

def _engine(rows):
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE t (alpha, mid, zeta)")
    db.executemany("INSERT INTO t VALUES (?, ?, ?)", rows)
    return db


class _Cursor:
    """A DB-API cursor; dict rows (Snowflake's DictCursor) or tuples (Oracle)."""

    def __init__(self, db, as_dict):
        self._db, self._as_dict = db, as_dict
        self._rows: list = []
        self.description = None

    def execute(self, sql, params=None):
        cur = self._db.execute(sql, tuple(params or ()))
        names = [d[0] for d in cur.description]
        self.description = [(n, None, None, None, None, None, None) for n in names]
        rows = cur.fetchall()
        self._rows = [dict(zip(names, r)) for r in rows] if self._as_dict else rows

    def fetchmany(self, size=1):
        out, self._rows = self._rows[:size], self._rows[size:]
        return out

    def close(self):
        pass


class _Conn:
    def __init__(self, db, as_dict):
        self._db, self._as_dict = db, as_dict

    def cursor(self, *a, **k):
        return _Cursor(self._db, self._as_dict)

    def close(self):
        pass


class _ClickHouseClient:
    def __init__(self, db):
        self._db = db

    def query(self, sql):
        cur = self._db.execute(sql)
        return types.SimpleNamespace(column_names=[d[0] for d in cur.description],
                                     result_rows=cur.fetchall())

    def close(self):
        pass


class _BigQueryRows(list):
    """A RowIterator: Row objects (dict()-able) plus the result's schema."""


class _BigQueryClient:
    """The two calls a verbatim read makes: a dry run, then the job."""

    def __init__(self, db):
        self._db = db

    def query(self, sql, job_config=None, job_retry=None):
        if getattr(job_config, "dry_run", False):
            return types.SimpleNamespace(statement_type="SELECT")
        cur = self._db.execute(sql)
        names = [d[0] for d in cur.description]
        rows = cur.fetchall()

        def result(max_results=None):
            out = _BigQueryRows(dict(zip(names, r)) for r in rows[:max_results])
            out.schema = [types.SimpleNamespace(name=n) for n in names]
            return out

        return types.SimpleNamespace(done=lambda timeout=None: True, result=result,
                                     cancel=lambda: None)


class _BigQueryModule:
    class QueryJobConfig:
        def __init__(self, dry_run=False, **kw):
            self.dry_run = dry_run


def _export(name, db):
    """The connector's query-mode export, as a callable of the SQL."""
    config = CONNECTORS[name][2]
    if name == "bigquery":
        adapter = _load(name)()
        client = _BigQueryClient(db)
        adapter._client = lambda config: client
        adapter._bq = lambda: _BigQueryModule
        return lambda sql: adapter.export(dict(config), {"query": sql, "limit": 100})
    srv = _load(name)()
    if name == "clickhouse":
        srv._connect = lambda config: _ClickHouseClient(db)
    elif name == "snowflake":
        srv._connect = lambda config: _Conn(db, as_dict=True)
    else:  # oracle
        srv._get_connection = lambda config: _Conn(db, as_dict=False)
        srv._get_cursor = lambda conn, as_dict=True: conn.cursor()
    return lambda sql: srv.export({"config": dict(config), "query": sql, "limit": 100})


@pytest.mark.parametrize("name", list(CONNECTORS))
def test_query_result_names_its_columns_in_select_order(name):
    res = _export(name, _engine([(1, "m", "z")]))(SELECT)
    assert res.get("success") is not False, res
    assert res["paging_mode"] == "query"
    assert res.get("columns") == SELECTED
    assert [list(r) for r in res["data"]] == [SELECTED]


@pytest.mark.parametrize("name", list(CONNECTORS))
def test_a_query_that_matches_no_rows_still_names_its_columns(name):
    res = _export(name, _engine([(1, "m", "z")]))(SELECT + " WHERE 0")
    assert res.get("success") is not False, res
    assert res["data"] == []
    assert res.get("columns") == SELECTED
