"""A warehouse batch sweep reads every row once (KI-WAREHOUSE-BATCH-PAGING-UNORDERED).

Bug class: a page whose start is not a position in a stable order. Snowflake,
Databricks, BigQuery, Redshift and ClickHouse promise no row order without
``ORDER BY``, and the executor never sends ``cursor_column``, so every sweep
read ``LIMIT n OFFSET m`` with no ``ORDER BY``. Each page could come from
another order: a sweep duplicated some rows and skipped others, and the run
reported success.

The repros drive each connector's real export page by page, the way the
executor drives it, against ``_Warehouse``: SQLite standing in for the engine.
It answers the dialect's catalog reads, keeps a copy of the table for every
snapshot a sweep pins, and returns rows in a new order on every query that has
no ``ORDER BY``, as an MPP engine may. The rest pin ``read_page``'s snapshot
cursor and each dialect's page SQL.
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

if _PUBLIC not in sys.path:
    sys.path.insert(0, _PUBLIC)
import sql_paging  # noqa: E402

# name -> (connector root, server class, connection config, table, the
# connector's qualified name for it)
CONNECTORS = {
    "snowflake": (os.path.join(_PUBLIC, "database", "snowflake"), "SnowflakeMCPServer",
                  {"account": "a", "user": "u", "password": "p", "schema": "PUBLIC"},
                  "T", '"PUBLIC"."T"'),
    "databricks": (os.path.join(_PUBLIC, "database", "databricks"), "DatabricksMCPServer",
                   {"server_hostname": "h", "http_path": "p", "access_token": "x",
                    "catalog": "main", "schema": "default"},
                   "t", "`main`.`default`.`t`"),
    "redshift": (os.path.join(_PUBLIC, "redshift"), "RedshiftMCPServer",
                 {"host": "h", "database": "dev", "user": "u", "password": "p", "schema": "public"},
                 "t", '"public"."t"'),
    "clickhouse": (os.path.join(_PUBLIC, "database", "clickhouse"), "ClickHouseMCPServer",
                   {"host": "h", "database": "demo", "user": "u", "password": "p"},
                   "t", "`demo`.`t`"),
    "bigquery": (None, None, {"project_id": "proj", "dataset_id": "ds"}, "t", "`proj.ds.t`"),
}
NAMES = list(CONNECTORS)
PINNED = ["snowflake", "databricks", "bigquery"]
UNPINNED = ["redshift", "clickhouse"]

# (name, type) per dialect: a non-unique id, a name, and one column the
# engine cannot order, which must stay out of ORDER BY.
COLUMNS = {
    "snowflake": [("ID", "NUMBER"), ("NAME", "TEXT"), ("DOC", "VARIANT")],
    "databricks": [("id", "bigint"), ("name", "string"), ("doc", "map<string,string>")],
    "redshift": [("id", "integer"), ("name", "character varying(64)"), ("doc", "super")],
    "clickhouse": [("id", "UInt64"), ("name", "String"), ("doc", "Map(String, String)")],
    "bigquery": [("id", "INTEGER"), ("name", "STRING"), ("doc", "JSON")],
}


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
            "_warehouse_paging_adapters", os.path.join(_PUBLIC, "warehouse_adapters.py"))
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        _LOADED[name] = mod.BigQueryWarehouseAdapter
        return _LOADED[name]
    path = _connector_file(root)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [os.path.dirname(path), root, _PUBLIC]
    spec = importlib.util.spec_from_file_location(f"_warehouse_paging_{name}", path)
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
# The engine
# --------------------------------------------------------------------------

_AS_OF = re.compile(
    r"\s*(?:AT\(TIMESTAMP => TO_TIMESTAMP_LTZ\((\d+), 3\)\)"
    r"|VERSION AS OF (\d+)"
    r"|FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS\((\d+)\))")
# One multiplier per unordered query: (rowid * m) % 1009 is a different
# permutation of the rows for each.
_SHUFFLES = [7, 331, 13, 523, 101, 787, 211, 29, 613, 409]


class _Warehouse:
    """One table `t` in SQLite, read through a warehouse dialect's SQL."""

    def __init__(self, name: str, rows):
        self.name = name
        self.source = CONNECTORS[name][4]
        self.columns = COLUMNS[name]
        self.db = sqlite3.connect(":memory:")
        cols = ", ".join(f'"{c}"' for c, _ in self.columns)
        self.db.execute(f"CREATE TABLE t ({cols})")
        self.insert(*rows)
        self.snapshots: dict = {}
        self.clock = 1_700_000_000_000
        self.time_travel = True
        self.pages: list = []
        self._unordered = 0

    def insert(self, *rows):
        marks = ", ".join("?" for _ in self.columns)
        self.db.executemany(f"INSERT INTO t VALUES ({marks})", rows)

    def delete(self, row_id):
        self.db.execute(f'DELETE FROM t WHERE "{self.columns[0][0]}" = ?', (row_id,))

    def forget_snapshots(self):
        """Retention passed: every pinned point is gone."""
        self.snapshots.clear()

    def _snapshot(self):
        self.clock += 1000
        copy = f"t__v{len(self.snapshots) + 1}"
        self.db.execute(f"CREATE TABLE {copy} AS SELECT * FROM t")
        self.snapshots[self.clock] = copy
        return self.clock

    def _catalog(self, sql):
        """(column names, rows) for a catalog or snapshot read, else None."""
        if "INFORMATION_SCHEMA.COLUMNS" in sql:
            return ["COLUMN_NAME", "DATA_TYPE"], [(c, t) for c, t in self.columns]
        if sql.startswith("DESCRIBE TABLE"):
            return (["col_name", "data_type", "comment"],
                    [(c, t, None) for c, t in self.columns]
                    + [("", "", ""), ("# Partition Information", "", "")])
        if "svv_columns" in sql:
            return ["column_name", "data_type"], [(c, t) for c, t in self.columns]
        if "system.columns" in sql:
            return ["name", "type", "default_kind"], [(c, t, "") for c, t in self.columns]
        if "system.tables" in sql:
            return ["sorting_key"], [(self.columns[0][0],)]
        if ("DATE_PART(EPOCH_MILLISECOND" in sql or "UNIX_MILLIS" in sql
                or sql.startswith("DESCRIBE HISTORY")):
            return ["point"], [(self._snapshot(),)]
        return None

    def run(self, sql, params=None):
        answer = self._catalog(sql)
        if answer is not None:
            return answer
        self.pages.append(sql)
        table = "t"
        m = _AS_OF.search(sql)
        if m:
            point = int(next(g for g in m.groups() if g))
            if not self.time_travel:
                raise RuntimeError("time travel is not supported for this table")
            if point not in self.snapshots:
                raise RuntimeError("the requested time is outside the retention period")
            table = self.snapshots[point]
            sql = sql[:m.start()] + sql[m.end():]
        sql = sql.replace(self.source, table)
        if " ORDER BY " not in sql:
            mult = _SHUFFLES[self._unordered % len(_SHUFFLES)]
            self._unordered += 1
            sql = sql.replace(" LIMIT ", f" ORDER BY (rowid * {mult}) % 1009 LIMIT ", 1)
        sql = re.sub(r"%s|@p\d+", "?", sql)
        cur = self.db.execute(sql, tuple(params or ()))
        return [d[0] for d in cur.description], cur.fetchall()


class _Cursor:
    """A DB-API cursor with dict rows (DictCursor / RealDictCursor / Row)."""

    def __init__(self, wh):
        self._wh = wh
        self._rows: list = []
        self.description = None

    def execute(self, sql, params=None):
        names, rows = self._wh.run(sql, params)
        self.description = [(n, None, None, None, None, None, None) for n in names]
        self._rows = [dict(zip(names, r)) for r in rows]

    def fetchall(self):
        rows, self._rows = self._rows, []
        return rows

    def fetchone(self):
        return self._rows.pop(0) if self._rows else None

    def close(self):
        pass


class _Conn:
    def __init__(self, wh):
        self._wh = wh

    def cursor(self, *a, **k):
        return _Cursor(self._wh)

    def rollback(self):
        pass

    def commit(self):
        pass

    def close(self):
        pass


class _ClickHouseClient:
    def __init__(self, wh):
        self._wh = wh

    def query(self, sql):
        names, rows = self._wh.run(sql)
        return types.SimpleNamespace(column_names=names, result_rows=[tuple(r) for r in rows])

    def close(self):
        pass


class _BigQueryResult(list):
    pass


class _BigQueryClient:
    def __init__(self, wh):
        self._wh = wh

    def query(self, sql, job_config=None):
        binds = [p.value for p in job_config.query_parameters] if job_config is not None else None
        names, rows = self._wh.run(sql, binds)

        def result(max_results=None):
            out = _BigQueryResult(dict(zip(names, r)) for r in rows)
            out.schema = [types.SimpleNamespace(name=n) for n in names]
            return out if max_results is None else _BigQueryResult(out[:max_results])

        return types.SimpleNamespace(result=result)

    def get_table(self, ref):
        return types.SimpleNamespace(schema=[
            types.SimpleNamespace(name=c, field_type=t, mode="NULLABLE") for c, t in self._wh.columns])


class _BigQueryModule:
    class QueryJobConfig:
        def __init__(self, query_parameters=None, **kw):
            self.query_parameters = list(query_parameters or [])

    class ScalarQueryParameter:
        def __init__(self, name, type_, value):
            self.name, self.type_, self.value = name, type_, value


def _export(name, wh):
    """The connector's export, as a callable of the request params."""
    if name == "bigquery":
        adapter = _load(name)()
        client = _BigQueryClient(wh)
        adapter._client = lambda config: client
        adapter._bq = lambda: _BigQueryModule
        return lambda params: adapter.export(params["config"], params)
    srv = _load(name)()
    if name == "clickhouse":
        srv._connect = lambda config: _ClickHouseClient(wh)
    else:
        srv._connect = lambda config: _Conn(wh)
    return srv.export


def _sweep(name, wh, limit=4, after_page=None):
    """One sweep, the executor's way (executor.go table loop,
    batch_page_cursor.go readPageCursor): `use_keyset_paging` and never a
    `cursor_column`; an OFFSET page drops any cursor; a `next_cursor` goes back
    as `cursor`, through JSON; `offset` steps only while no cursor is held."""
    export = _export(name, wh)
    config, table = CONNECTORS[name][2], CONNECTORS[name][3]
    read, offset, cursor, pages, res = [], 0, None, 0, {}
    while pages < 50:
        params = {"config": dict(config), "table": table, "limit": limit,
                  "offset": 0 if cursor is not None else offset, "use_keyset_paging": True}
        if cursor is not None:
            params["cursor"] = cursor
        res = export(params)
        assert res.get("success") is not False, res
        rows = res.get("data") or []
        read.extend(tuple(r[c] for c, _ in wh.columns[:2]) for r in rows)
        pages += 1
        if (res.get("paging_mode") or "").lower() == "offset":
            cursor = None
        elif res.get("next_cursor") is not None:
            cursor = json.loads(json.dumps(res["next_cursor"]))
        if cursor is None:
            offset += len(rows)
        if after_page:
            after_page(pages)
        if len(rows) < limit:
            break
    return read, res


def _rows(n=14):
    # id repeats (no warehouse enforces a key), (id, name) never does.
    return [(i // 2, f"n{i:02d}", "{}") for i in range(n)]


def _ids(rows):
    return sorted((r[0], r[1]) for r in rows)


# --------------------------------------------------------------------------
# The repros
# --------------------------------------------------------------------------

@pytest.mark.parametrize("name", NAMES)
def test_a_sweep_reads_every_row_once(name):
    wh = _Warehouse(name, _rows())
    read, _ = _sweep(name, wh)
    assert sorted(read) == _ids(_rows())


@pytest.mark.parametrize("name", NAMES)
def test_a_sweep_orders_by_every_orderable_column(name):
    """The unorderable third column stays out of ORDER BY."""
    wh = _Warehouse(name, _rows())
    _sweep(name, wh)
    q = '"' if name in ("snowflake", "redshift") else "`"
    order = f" ORDER BY {q}{wh.columns[0][0]}{q}, {q}{wh.columns[1][0]}{q} LIMIT "
    assert wh.pages and all(order in sql for sql in wh.pages), wh.pages


@pytest.mark.parametrize("name", PINNED)
def test_a_pinned_sweep_skips_no_row_deleted_or_inserted_mid_sweep(name):
    wh = _Warehouse(name, _rows())

    def change(page):
        if page == 1:
            wh.delete(0)                       # two rows already read
            wh.insert((-1, "n-1", "{}"))       # sorts before every page
    read, _ = _sweep(name, wh, after_page=change)
    assert sorted(read) == _ids(_rows())
    assert len(wh.snapshots) == 1
    assert all(re.search(_AS_OF, sql) for sql in wh.pages), wh.pages


@pytest.mark.parametrize("name", UNPINNED)
def test_an_unpinned_sweep_duplicates_rather_than_skips_a_row_inserted_mid_sweep(name):
    wh = _Warehouse(name, _rows())
    read, _ = _sweep(name, wh, after_page=lambda page: page == 1 and wh.insert((-1, "n-1", "{}")))
    assert set(_ids(_rows())) <= set(read)
    assert len(read) == len(_rows()) + 1


@pytest.mark.parametrize("name", PINNED)
def test_a_pin_lost_mid_sweep_goes_on_unpinned_from_its_offset(name):
    wh = _Warehouse(name, _rows())
    read, res = _sweep(name, wh, after_page=lambda page: page == 1 and wh.forget_snapshots())
    assert sorted(read) == _ids(_rows())
    assert _AS_OF.search(wh.pages[0])
    assert not any(_AS_OF.search(sql) for sql in wh.pages[-1:])
    assert res["paging_mode"] == "snapshot"


@pytest.mark.parametrize("name", PINNED)
def test_a_table_that_cannot_time_travel_steps_the_offset(name):
    wh = _Warehouse(name, _rows())
    wh.time_travel = False
    read, res = _sweep(name, wh)
    assert sorted(read) == _ids(_rows())
    assert res["paging_mode"] == "offset" and res["next_cursor"] is None


@pytest.mark.parametrize("name", PINNED)
def test_a_cursor_from_before_the_fix_re_reads_from_the_top(name):
    """A checkpoint from the old export holds an offset string or a key: the
    first page reads from the top again. Duplicates, never a skipped row."""
    wh = _Warehouse(name, _rows())
    export = _export(name, wh)
    config, table = CONNECTORS[name][2], CONNECTORS[name][3]
    res = export({"config": dict(config), "table": table, "limit": 4, "offset": 0,
                  "use_keyset_paging": True, "cursor": "8"})
    assert res["success"], res
    assert res["next_cursor"]["offset"] == 4
    assert [tuple(r.values())[:2] for r in res["data"]] == [tuple(r[:2]) for r in _rows()[:4]]


# --------------------------------------------------------------------------
# read_page and its cursor
# --------------------------------------------------------------------------

@pytest.mark.parametrize("cursor, position", [
    ({"snapshot": 1700000000000, "offset": 8}, (1700000000000, 8)),
    ({"snapshot": 1.7e12, "offset": 8.0}, (1700000000000, 8)),
    ({"snapshot": "1700000000000", "offset": "8"}, (1700000000000, 8)),
    # An Iceberg snapshot id past 2**53, carried as a string: read exactly.
    ({"snapshot": "5281394372919829231", "offset": 8.0}, (5281394372919829231, 8)),
    ({"snapshot": None, "offset": 8}, (None, 8)),
    ({"snapshot": 5, "offset": -1}, None),
    ({"snapshot": 5}, None),
    ({"snapshot": 5, "offset": True}, None),
    ({"locator": "(0,1)"}, None),
    ("8", None),
    ([1, 2], None),
    (None, None),
])
def test_snapshot_position(cursor, position):
    assert sql_paging.snapshot_position(cursor) == position


@pytest.mark.parametrize("dialect, safe_table, sql", [
    (sql_paging.SNOWFLAKE, '"PUBLIC"."T"',
     'SELECT * FROM "PUBLIC"."T" AT(TIMESTAMP => TO_TIMESTAMP_LTZ(1700000000000, 3)) '
     'ORDER BY "a", "b" LIMIT 5 OFFSET 10'),
    (sql_paging.DATABRICKS, "`main`.`default`.`t`",
     "SELECT * FROM `main`.`default`.`t` VERSION AS OF 1700000000000 ORDER BY `a`, `b` LIMIT 5 OFFSET 10"),
    (sql_paging.BIGQUERY, "`proj.ds.t`",
     "SELECT * FROM `proj.ds.t` FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS(1700000000000) "
     "ORDER BY `a`, `b` LIMIT 5 OFFSET 10"),
    (sql_paging.ATHENA, '"analytics"."t"',
     'SELECT * FROM "analytics"."t" FOR VERSION AS OF 1700000000000 ORDER BY "a", "b" OFFSET 10 LIMIT 5'),
    # No time travel: the point is ignored.
    (sql_paging.REDSHIFT, '"public"."t"', 'SELECT * FROM "public"."t" ORDER BY "a", "b" LIMIT 5 OFFSET 10'),
    (sql_paging.CLICKHOUSE, "`demo`.`t`", "SELECT * FROM `demo`.`t` ORDER BY `a`, `b` LIMIT 5 OFFSET 10"),
])
def test_pinned_page_sql(dialect, safe_table, sql):
    plan = sql_paging.ordered_offset_plan(["a", "b"])
    assert sql_paging.page_query(dialect, plan, safe_table, limit=5, offset=10,
                                 snapshot=1700000000000) == (sql, None)


def test_a_pinned_page_reports_its_point_and_next_offset():
    wh = _Warehouse("snowflake", _rows())
    conn = _Conn(wh)
    data, _, fields = sql_paging.read_page(conn, conn.cursor(), sql_paging.SNOWFLAKE, '"PUBLIC"."T"',
                                           '"PUBLIC"."T"', limit=4)
    assert len(data) == 4
    assert fields == {"paging_mode": "snapshot", "next_cursor": {"snapshot": wh.clock, "offset": 4}}
    data, _, fields = sql_paging.read_page(conn, conn.cursor(), sql_paging.SNOWFLAKE, '"PUBLIC"."T"',
                                           '"PUBLIC"."T"', limit=4,
                                           cursor_value={"snapshot": wh.clock, "offset": 12})
    assert len(data) == 2
    assert fields == {"paging_mode": "snapshot"}
    assert len(wh.snapshots) == 1


def test_an_unstable_read_neither_orders_nor_pins():
    wh = _Warehouse("snowflake", _rows())
    conn = _Conn(wh)
    _, _, fields = sql_paging.read_page(conn, conn.cursor(), sql_paging.SNOWFLAKE, '"PUBLIC"."T"',
                                        '"PUBLIC"."T"', limit=4, stable=False)
    assert fields == {"paging_mode": "offset"}
    assert wh.snapshots == {}


def test_query_cursor_reads_like_a_db_api_cursor():
    calls = []

    def run(sql, params):
        calls.append((sql, params))
        return ["a", "b"], [(1, 2), (3, 4)]

    cur = sql_paging.QueryCursor(run)
    cur.execute("SELECT 1", (5,))
    assert [d[0] for d in cur.description] == ["a", "b"]
    assert cur.fetchall() == [(1, 2), (3, 4)]
    assert cur.fetchall() == []
    assert calls == [("SELECT 1", (5,))]


def test_a_new_warehouse_needs_only_a_dialect():
    """snapshot + as_of is all it takes to pin a sweep."""
    wh = _Warehouse("snowflake", _rows())
    wh.source = '"x"'
    dialect = sql_paging.Dialect(
        name="newwh", discover=lambda ctx: sql_paging.ordered_offset_plan(["ID", "NAME"]),
        snapshot=lambda ctx: ctx.rows("SELECT UNIX_MILLIS(CURRENT_TIMESTAMP())")[0][0],
        as_of=lambda point: f"FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS({point})")
    conn = _Conn(wh)
    data, _, fields = sql_paging.read_page(conn, conn.cursor(), dialect, "x", '"x"', limit=4)
    assert [r["ID"] for r in data] == [0, 0, 1, 1]
    assert fields["next_cursor"] == {"snapshot": wh.clock, "offset": 4}
