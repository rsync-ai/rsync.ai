"""A batch sweep reads every row once: stable paging for SQL sources.

KI-DB-BATCH-PAGING-CAN-SKIP-ROWS causes (1) and (2). Bug class: a page whose
start is not a unique position in a stable order. Two shapes of it:

  * a composite primary key paged on its first column only
    (``WHERE order_id > <last>``): every row sharing the boundary value past
    the page limit is skipped;
  * a table with no key paged by ``LIMIT/OFFSET`` with no ``ORDER BY``: the
    database may return rows in another order on every page, and a row deleted
    from an earlier page shifts every later offset by one, so a row is skipped.

Both skip rows with the run reporting success. ``sql_paging`` now pages by the
whole key, else by the row's physical locator, else by ``ORDER BY`` every
column; the four SQL connectors and ``connector_database.py.j2`` share it.

The first tests are the repros: every connector's real export against a real
(in-memory SQLite) table, driven page by page the way the executor drives it
(``batch_page_cursor.go readPageCursor``). The rest pin each dialect's catalog
reads and page SQL, and the lockstep of the five export copies.
"""
from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import re
import sqlite3
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_PUBLIC = os.path.abspath(os.path.join(_HERE, "..", "public"))
_REPO = pathlib.Path(_HERE).resolve().parents[2]
TEMPLATE = _REPO / "llm-service" / "src" / "agents" / "tool_generator" / "templates" / "connector_database.py.j2"

if _PUBLIC not in sys.path:
    sys.path.insert(0, _PUBLIC)
import sql_paging  # noqa: E402

# (name, connector root, server class)
CONNECTORS = [
    ("postgresql", os.path.join(_PUBLIC, "postgresql"), "PostgresqlMCPServer"),
    ("mysql", os.path.join(_PUBLIC, "database", "mysql"), "MysqlMCPServer"),
    ("oracle", os.path.join(_PUBLIC, "database", "oracle"), "OracleMCPServer"),
    # The sqlserver connector's class is literally named MysqlMCPServer.
    ("sqlserver", os.path.join(_PUBLIC, "database", "sqlserver"), "MysqlMCPServer"),
]
BY_NAME = {c[0]: c for c in CONNECTORS}
NAMES = [c[0] for c in CONNECTORS]
CONFIG = {"host": "db", "port": 5432, "database": "app", "username": "u", "password": "p"}


def _connector_file(root: str) -> str:
    with open(os.path.join(root, "latest.json")) as f:
        current = json.load(f)["current_version"]
    return os.path.join(root, "versions", current, "connector.py")


_LOADED: dict = {}


def _load(name: str):
    """Import one connector in isolation. Each version dir carries its own
    `base_connector.py`; leaving it in sys.modules makes a later test import
    the wrong copy, so sys.path and sys.modules are put back afterwards."""
    if name in _LOADED:
        return _LOADED[name]
    _, root, class_name = BY_NAME[name]
    path = _connector_file(root)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [os.path.dirname(path), root, _PUBLIC]
    spec = importlib.util.spec_from_file_location(f"_stable_paging_{name}", path)
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
# The repros: a real table, paged like the executor pages it.
# --------------------------------------------------------------------------

class _KeepOpen:
    """The export closes its connection; the table has to outlive a page."""

    def __init__(self, db):
        self._db = db

    def rollback(self):
        self._db.rollback()

    def close(self):
        pass


class _SqliteCursor:
    """sqlite3 rejects `execute(sql, None)`, which the DB-API drivers accept."""

    def __init__(self, db):
        self._cur = db.cursor()

    def execute(self, sql, params=None):
        return self._cur.execute(sql, params or ())

    def __getattr__(self, name):
        return getattr(self._cur, name)


def _sqlite_server(name, db):
    srv = _load(name)()
    srv.driver_pattern = dict(srv.driver_pattern, module="sqlite3")
    srv._get_connection = lambda config: _KeepOpen(db)
    srv._get_cursor = lambda conn, *a, **k: _SqliteCursor(db)
    return srv


def _sweep(srv, table, limit=2, cursor=None, after_page=None):
    """One sweep, the executor's way (executor.go table loop): an OFFSET page
    drops any cursor, a `next_cursor` is sent back as `cursor`, and `offset`
    steps only while no cursor is held (advancePage)."""
    read, offset, pages = [], 0, 0
    while True:
        params = {"config": dict(CONFIG), "table": table, "format": "json",
                  "limit": limit, "offset": 0 if cursor is not None else offset,
                  "use_keyset_paging": True}
        if cursor is not None:
            params["cursor"] = cursor
        res = srv.export(params)
        assert res.get("success") is not False, res
        rows = res.get("data") or []
        read.extend(rows)
        pages += 1
        if (res.get("paging_mode") or "").lower() == "offset":
            cursor = None
        elif res.get("next_cursor") is not None:
            cursor = res["next_cursor"]
        if cursor is None:
            offset += len(rows)
        if after_page:
            after_page(pages)
        if len(rows) < limit or pages > 50:
            return read, res


def _order_lines():
    db = sqlite3.connect(":memory:")
    db.execute(
        "CREATE TABLE order_lines (order_id INTEGER NOT NULL, line_no INTEGER NOT NULL, "
        "sku TEXT, PRIMARY KEY (order_id, line_no))"
    )
    db.executemany("INSERT INTO order_lines VALUES (?, ?, ?)", [
        (1, 1, "a"), (1, 2, "b"), (1, 3, "c"), (2, 1, "d"), (2, 2, "e"),
    ])
    db.commit()
    return db


@pytest.mark.parametrize("name", NAMES)
def test_a_composite_key_boundary_row_is_not_skipped(name):
    """Page one ends inside order 1; `order_id > 1` skipped line (1, 3)."""
    db = _order_lines()
    read, _ = _sweep(_sqlite_server(name, db), "order_lines")
    got = sorted((r["order_id"], r["line_no"]) for r in read)
    assert got == [(1, 1), (1, 2), (1, 3), (2, 1), (2, 2)], f"{name} read {got}"


def _keyless_events():
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE events (name TEXT, amount INTEGER)")
    db.executemany("INSERT INTO events VALUES (?, ?)", [(c, i) for i, c in enumerate("abcdef")])
    db.commit()
    return db


@pytest.mark.parametrize("name", NAMES)
def test_a_keyless_table_loses_no_row_to_a_delete_between_pages(name):
    """A row of page one is deleted before page two: OFFSET 2 then starts one
    row later than it did, and row c was never read."""
    db = _keyless_events()

    def delete_a_read_row(page):
        if page == 1:
            db.execute("DELETE FROM events WHERE name = 'a'")
            db.commit()

    read, _ = _sweep(_sqlite_server(name, db), "events", after_page=delete_a_read_row)
    names = sorted(r["name"] for r in read)
    assert names == ["a", "b", "c", "d", "e", "f"], f"{name} read {names}"
    assert all(sql_paging.LOCATOR_ALIAS not in r for r in read), "the row locator reached the rows"


@pytest.mark.parametrize("name", NAMES)
def test_a_composite_key_page_reports_every_key_column(name):
    db = _order_lines()
    res = _sqlite_server(name, db).export({
        "config": dict(CONFIG), "table": "order_lines", "format": "json", "limit": 2,
        "use_keyset_paging": True,
    })
    assert res.get("paging_mode") == "keyset", res
    assert res.get("cursor_columns") == ["order_id", "line_no"], res
    assert res.get("next_cursor") == [1, 2], res
    assert "cursor_column" not in res, "a lone cursor_column would let the executor drop rows by one column"


@pytest.mark.parametrize("name", NAMES)
def test_a_checkpoint_from_before_composite_paging_re_reads_rather_than_skips(name):
    """A run checkpointed by the old code holds the first key column only (1).
    It resumes with `order_id >= 1`, re-reading order 1 rather than skipping
    its unread lines."""
    db = _order_lines()
    read, _ = _sweep(_sqlite_server(name, db), "order_lines", cursor=1)
    got = sorted((r["order_id"], r["line_no"]) for r in read)
    assert got == [(1, 1), (1, 2), (1, 3), (2, 1), (2, 2)], f"{name} read {got}"


@pytest.mark.parametrize("name", NAMES)
def test_a_keyless_page_reports_a_locator_not_a_key(name):
    db = _keyless_events()
    res = _sqlite_server(name, db).export({
        "config": dict(CONFIG), "table": "events", "format": "json", "limit": 2,
        "use_keyset_paging": True,
    })
    assert res.get("paging_mode") == "locator", res
    assert res.get("next_cursor") == {"locator": "2"}, res
    assert res.get("columns", ["name", "amount"]) == ["name", "amount"], res


@pytest.mark.parametrize("name", NAMES)
def test_an_insert_in_a_composite_key_table_reaches_the_next_sweep(name):
    """The delta half `key > since_cursor` compares the whole key."""
    db = _order_lines()
    srv = _sqlite_server(name, db)
    db.execute("INSERT INTO order_lines VALUES (2, 3, 'f')")
    db.commit()
    res = srv.export({
        "config": dict(CONFIG), "table": "order_lines", "format": "json", "limit": 10,
        "use_keyset_paging": True, "since_cursor": [2, 2],
    })
    assert [(r["order_id"], r["line_no"]) for r in res["data"]] == [(2, 3)], res


def test_a_keyless_view_pages_in_a_stable_order():
    """A view has neither a key nor a rowid: ORDER BY every column."""
    db = _keyless_events()
    db.execute("CREATE VIEW big_events AS SELECT name, amount FROM events WHERE amount > 1")
    plan = sql_paging.paging_plan(_KeepOpen(db), _SqliteCursor(db), sql_paging.SQLITE, "big_events", '"big_events"')
    assert plan == sql_paging.ordered_offset_plan(["name", "amount"]), plan


class _RowidInViewCursor(_SqliteCursor):
    """A SQLite built with SQLITE_ALLOW_ROWID_IN_VIEW (Debian's, the connector
    base image's) lets a view select a rowid, which is NULL. macOS's refuses it,
    so the probe is answered here the way Debian's answers it."""

    _PROBE = re.compile(r'SELECT (?:rowid|_rowid_|oid) FROM "big_events" LIMIT 0')

    def execute(self, sql, params=None):
        if self._PROBE.fullmatch(sql):
            sql, params = "SELECT NULL LIMIT 0", None
        return super().execute(sql, params)


def test_a_view_never_pages_by_rowid_where_sqlite_lets_it_select_one():
    db = _keyless_events()
    db.execute("CREATE VIEW big_events AS SELECT name, amount FROM events WHERE amount > 1")
    plan = sql_paging.paging_plan(_KeepOpen(db), _RowidInViewCursor(db), sql_paging.SQLITE, "big_events", '"big_events"')
    assert plan == sql_paging.ordered_offset_plan(["name", "amount"]), plan


def test_a_sqlite_key_that_can_hold_null_is_not_paged_by():
    """SQLite lets a non-INTEGER PRIMARY KEY hold NULL, and `code > ?` never
    matches a NULL row: the rowid pages instead."""
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE codes (code TEXT PRIMARY KEY, v INTEGER)")
    plan = sql_paging.paging_plan(_KeepOpen(db), _SqliteCursor(db), sql_paging.SQLITE, "codes", '"codes"')
    assert plan["kind"] == sql_paging.LOCATOR, plan


def test_a_without_rowid_table_pages_by_its_key():
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE w (a TEXT, b TEXT, PRIMARY KEY (a, b)) WITHOUT ROWID")
    plan = sql_paging.paging_plan(_KeepOpen(db), _SqliteCursor(db), sql_paging.SQLITE, "w", '"w"')
    assert plan == sql_paging.key_plan(["a", "b"]), plan


def test_the_expanded_composite_comparison_matches_row_order():
    """Databases without row values compare a composite key with the expanded
    form. Every cursor position in a 3x3x3 grid must return exactly the rows
    after it in (a, b, c) order."""
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE g (a INTEGER, b INTEGER, c INTEGER)")
    grid = [(a, b, c) for a in range(3) for b in range(3) for c in range(3)]
    db.executemany("INSERT INTO g VALUES (?, ?, ?)", grid)
    plan = sql_paging.key_plan(["a", "b", "c"])
    for position in grid:
        sql, params = sql_paging.page_query(sql_paging.SQLITE, plan, '"g"', limit=100, cursor=list(position))
        got = [tuple(r) for r in db.execute(sql, params).fetchall()]
        assert got == [t for t in grid if t > position], (position, sql)


# --------------------------------------------------------------------------
# Discovery: each dialect's catalog reads, against a scripted cursor.
# --------------------------------------------------------------------------

class _Catalog:
    """A DB-API cursor that answers catalog reads from a script: the first
    (marker, answer) whose marker is in the SQL wins. An Exception answer is
    raised; anything unmatched returns no rows."""

    def __init__(self, *script):
        self.script = script
        self.executed = []
        self._rows = []

    def execute(self, sql, params=None):
        self.executed.append((sql, params))
        self._rows = []
        for marker, answer in self.script:
            if marker in sql:
                if isinstance(answer, Exception):
                    raise answer
                self._rows = [tuple(r) for r in answer]
                return

    def fetchall(self):
        return self._rows


class _Conn:
    def __init__(self):
        self.rollbacks = 0

    def rollback(self):
        self.rollbacks += 1


def _plan(dialect, cur, table="app.t", safe_table='"app"."t"', nominated=None, conn=None):
    return sql_paging.paging_plan(conn or _Conn(), cur, dialect, table, safe_table, nominated=nominated)


# pg_index rows: indexrelid, indisprimary, attname, attnotnull
_PG_KEY = "pg_index"
_PG_LEGACY = "pg_constraint"
_PG_LOCATOR = "relhassubclass"
_PG_COLUMNS = "typbasetype"


def test_pg_pages_by_the_whole_composite_primary_key():
    cur = _Catalog((_PG_KEY, [(10, True, "order_id", True), (10, True, "line_no", True),
                              (20, False, "sku", True)]))
    assert _plan(sql_paging.POSTGRESQL, cur) == sql_paging.key_plan(["order_id", "line_no"])
    assert cur.executed == [(sql_paging.PG_KEY_SQL, ('"app"."t"',))]


def test_pg_without_a_primary_key_takes_the_narrowest_not_null_unique_index():
    cur = _Catalog((_PG_KEY, [
        (20, False, "a", True), (20, False, "b", True), (20, False, "c", True),
        (30, False, "email", False),              # nullable: NULLs are not unique
        (40, False, None, True),                  # an expression index
        (50, False, "a", True), (50, False, "b", True),
    ]))
    assert _plan(sql_paging.POSTGRESQL, cur) == sql_paging.key_plan(["a", "b"])


def test_pg_keyless_heap_pages_by_ctid():
    cur = _Catalog((_PG_KEY, []), (_PG_LOCATOR, [("r", False, True)]))
    plan = _plan(sql_paging.POSTGRESQL, cur)
    assert plan["kind"] == sql_paging.LOCATOR and plan["order"] == "ctid", plan


@pytest.mark.parametrize("relkind,has_children,tid_gt", [
    ("v", False, True),    # a view
    ("p", True, True),     # a partitioned table: ctid repeats across partitions
    ("r", True, True),     # an inheritance parent
    ("f", False, True),    # a foreign table
    ("r", False, False),   # no tid > operator
])
def test_pg_relation_without_a_usable_ctid_orders_by_its_columns(relkind, has_children, tid_gt):
    cur = _Catalog(
        (_PG_KEY, []),
        (_PG_LOCATOR, [(relkind, has_children, tid_gt)]),
        (_PG_COLUMNS, [("id", "int4"), ("doc", "json"), ("tags", "_json"), ("at", "point"),
                       ("name", "text"), ("body", "jsonb")]),
    )
    assert _plan(sql_paging.POSTGRESQL, cur) == sql_paging.ordered_offset_plan(["id", "name", "body"])


@pytest.mark.parametrize("table,params", [("app.t", ("app", "t")), ("t", ("public", "t"))])
def test_pg_before_11_falls_back_to_the_legacy_single_column_key(table, params):
    conn = _Conn()
    cur = _Catalog((_PG_KEY, RuntimeError('column "indnkeyatts" does not exist')),
                   (_PG_LEGACY, [("id",)]))
    assert _plan(sql_paging.POSTGRESQL, cur, table=table, conn=conn) == sql_paging.key_plan(["id"])
    assert cur.executed[1] == (sql_paging.PG_LEGACY_KEY_SQL, params)
    assert conn.rollbacks == 1, "a failed read aborts a PostgreSQL transaction; it must be rolled back"


def test_a_failing_catalog_falls_back_to_offset_and_never_raises():
    conn = _Conn()
    boom = RuntimeError("permission denied")
    cur = _Catalog(("", boom))
    for dialect in (sql_paging.POSTGRESQL, sql_paging.MYSQL, sql_paging.SQLSERVER_QMARK,
                    sql_paging.ORACLE, sql_paging.SQLITE):
        assert _plan(dialect, cur, conn=conn) == sql_paging.offset_plan(), dialect.name
    assert conn.rollbacks >= 5


def test_a_named_cursor_column_is_used_without_reading_the_catalog():
    cur = _Catalog(("", RuntimeError("no catalog read expected")))
    for dialect in (sql_paging.POSTGRESQL, sql_paging.SQLSERVER_QMARK,
                    sql_paging.ORACLE, sql_paging.SQLITE, sql_paging.GENERIC):
        assert _plan(dialect, cur, nominated="seq") == sql_paging.key_plan(["seq"]), dialect.name
    assert cur.executed == []


def test_a_named_invisible_mysql_column_is_projected():
    """MySQL reads its column list even for a named column: SELECT * leaves
    an INVISIBLE one out, and the cursor needs its value. No key is read."""
    cur = _Catalog(("information_schema.COLUMNS", [("my_row_id", "bigint", "INVISIBLE"), ("v", "int", "")]),
                   ("SHOW KEYS", RuntimeError("no key read expected")))
    assert _plan(sql_paging.MYSQL, cur, nominated="my_row_id") == \
        sql_paging.key_plan(["my_row_id"], hidden=["my_row_id"])
    assert [sql for sql, _ in cur.executed] == [sql_paging.MYSQL_COLUMNS_SQL]


def test_an_unsafe_named_cursor_column_never_reaches_sql():
    cur = _Catalog((_PG_KEY, []), (_PG_LOCATOR, []), (_PG_COLUMNS, []))
    assert _plan(sql_paging.POSTGRESQL, cur, nominated='id" > 0 OR "1') == sql_paging.offset_plan()
    assert all('id" > 0' not in sql for sql, _ in cur.executed)


# SHOW KEYS: Table, Non_unique, Key_name, Seq_in_index, Column_name,
# Collation, Cardinality, Sub_part, Packed, Null
def _key(name, seq, col, non_unique=0, sub_part=None, null=""):
    return ("t", non_unique, name, seq, col, "A", 0, sub_part, None, null)


def test_mysql_pages_by_the_whole_primary_key_in_index_order():
    cur = _Catalog(("SHOW KEYS", [_key("PRIMARY", 2, "line_no"), _key("PRIMARY", 1, "order_id")]))
    assert _plan(sql_paging.MYSQL, cur, safe_table="`app`.`t`") == sql_paging.key_plan(["order_id", "line_no"])
    assert cur.executed == [(sql_paging.MYSQL_COLUMNS_SQL, ("app", "t")), ("SHOW KEYS FROM `app`.`t`", None)]


def test_mysql_rejects_non_unique_prefix_and_nullable_indexes():
    cur = _Catalog(
        ("SHOW KEYS", [
            _key("ix_a", 1, "a", non_unique=1),
            _key("uq_name", 1, "name", sub_part=10),   # a prefix is not unique on the column
            _key("uq_email", 1, "email", null="YES"),
        ]),
        ("information_schema.COLUMNS", [("id", "int", ""), ("doc", "json", ""), ("name", "varchar", "")]),
    )
    assert _plan(sql_paging.MYSQL, cur, table="t", safe_table="`t`") == sql_paging.ordered_offset_plan(["id", "name"])
    assert cur.executed[0] == (sql_paging.MYSQL_COLUMNS_SQL, (None, "t"))


def test_mysql_generated_invisible_key_is_projected_for_the_cursor():
    cur = _Catalog(
        ("SHOW KEYS", [_key("PRIMARY", 1, "my_row_id")]),
        ("information_schema.COLUMNS", [("my_row_id", "bigint", "auto_increment INVISIBLE"),
                                        ("name", "varchar", "")]),
    )
    assert _plan(sql_paging.MYSQL, cur) == sql_paging.key_plan(["my_row_id"], hidden=["my_row_id"])


@pytest.mark.parametrize("dialect,bind", [(sql_paging.SQLSERVER_QMARK, "?"), (sql_paging.SQLSERVER_FORMAT, "%s")])
def test_sqlserver_pages_by_the_whole_key_bound_in_the_driver_style(dialect, bind):
    # index_id, is_primary_key, name, is_nullable
    cur = _Catalog(("sys.indexes", [(1, 1, "order_id", 0), (1, 1, "line_no", 0)]))
    assert _plan(dialect, cur) == sql_paging.key_plan(["order_id", "line_no"])
    assert cur.executed == [(sql_paging.SQLSERVER_KEY_SQL.format(p=bind), ("[app].[t]",))]


def test_sqlserver_without_a_usable_key_orders_by_its_orderable_columns():
    cur = _Catalog(
        ("sys.indexes", [(2, 0, "email", 1)]),
        ("sys.columns c WHERE", [("id", 56), ("shape", 240), ("doc", 241), ("pic", 34), ("name", 231)]),
    )
    assert _plan(sql_paging.SQLSERVER_QMARK, cur) == sql_paging.ordered_offset_plan(["id", "name"])


# all_constraints rows: constraint_name, constraint_type, column_name, nullable
def test_oracle_pages_by_the_whole_primary_key():
    cur = _Catalog(("all_constraints", [("PK_T", "P", "ORDER_ID", "N"), ("PK_T", "P", "LINE_NO", "N")]))
    assert _plan(sql_paging.ORACLE, cur, table="APP.T", safe_table='"APP"."T"') == \
        sql_paging.key_plan(["ORDER_ID", "LINE_NO"])
    assert cur.executed == [(sql_paging.ORACLE_KEY_SQL, ("APP", "T"))]


def test_oracle_keyless_heap_pages_by_rowid():
    cur = _Catalog(("all_constraints", [("UQ_T", "U", "EMAIL", "Y")]), ("all_tables", [(None,)]))
    plan = _plan(sql_paging.ORACLE, cur, table="T", safe_table='"T"')
    assert plan["kind"] == sql_paging.LOCATOR and plan["order"] == "rsync_t.ROWID", plan


@pytest.mark.parametrize("heap", [[("IOT",)], []], ids=["index-organized", "view-or-external"])
def test_oracle_without_a_physical_rowid_orders_by_its_columns(heap):
    cur = _Catalog(
        ("all_constraints", []),
        ("all_tables", heap),
        ("all_tab_columns WHERE", [("ID", "NUMBER", None), ("BODY", "CLOB", None),
                                   ("ADDR", "ADDRESS_T", "APP"), ("NAME", "VARCHAR2", None)]),
    )
    assert _plan(sql_paging.ORACLE, cur, table="T", safe_table='"T"') == sql_paging.ordered_offset_plan(["ID", "NAME"])


# --------------------------------------------------------------------------
# Registry: a new database is one register() call; an unknown one pages as
# exports always did.
# --------------------------------------------------------------------------

@pytest.mark.parametrize("module,dialect", [
    ("psycopg2", sql_paging.POSTGRESQL), ("psycopg", sql_paging.POSTGRESQL), ("pg8000", sql_paging.POSTGRESQL),
    ("mysql.connector", sql_paging.MYSQL), ("pymysql", sql_paging.MYSQL), ("MySQLdb", sql_paging.MYSQL),
    ("pyodbc", sql_paging.SQLSERVER_QMARK), ("pymssql", sql_paging.SQLSERVER_FORMAT),
    ("oracledb", sql_paging.ORACLE), ("cx_Oracle", sql_paging.ORACLE), ("sqlite3", sql_paging.SQLITE),
    ("snowflake.connector", sql_paging.SNOWFLAKE), ("databricks.sql", sql_paging.DATABRICKS),
    ("redshift_connector", sql_paging.REDSHIFT), ("clickhouse_connect", sql_paging.CLICKHOUSE),
    ("", sql_paging.GENERIC), (None, sql_paging.GENERIC),
])
def test_each_driver_gets_its_dialect(module, dialect):
    assert sql_paging.dialect_for(module) is dialect


def test_a_new_database_is_one_register_call():
    def discover(ctx):
        return ctx.first(ctx.nominated_plan, lambda c: sql_paging.key_plan([r[0] for r in c.rows("KEYS ?", (c.name,))]))

    newdb = sql_paging.Dialect(name="newdb", paramstyle="qmark", discover=discover)
    sql_paging.register(newdb, "newdb_driver")
    try:
        assert sql_paging.dialect_for("newdb_driver.dbapi") is newdb
        plan = _plan(newdb, _Catalog(("KEYS", [("k1",), ("k2",)])))
        assert plan == sql_paging.key_plan(["k1", "k2"])
        sql, params = sql_paging.page_query(newdb, plan, '"t"', limit=5, cursor=["x", 1])
        assert sql == 'SELECT * FROM "t" WHERE ("k1" >= ? AND ("k1" > ? OR ("k2" > ?))) ORDER BY "k1", "k2" LIMIT 5'
        assert params == ("x", "x", 1)
    finally:
        sql_paging._REGISTRY[:] = [e for e in sql_paging._REGISTRY if e[1] is not newdb]
    assert sql_paging.dialect_for("newdb_driver") is sql_paging.GENERIC


def test_an_unknown_driver_pages_exactly_as_before():
    g = sql_paging.GENERIC
    assert _plan(g, _Catalog()) == sql_paging.offset_plan()
    assert sql_paging.page_query(g, sql_paging.offset_plan(), '"t"', limit=10, offset=20) == \
        ('SELECT * FROM "t" LIMIT 10 OFFSET 20', None)
    assert sql_paging.page_query(g, sql_paging.key_plan(["id"]), '"t"', limit=10, cursor=5) == \
        ('SELECT * FROM "t" WHERE "id" > %s ORDER BY "id" LIMIT 10', (5,))


# --------------------------------------------------------------------------
# One driver, several engines: psycopg2 reaches PostgreSQL and Redshift,
# pyodbc SQL Server and Synapse. The connector's own name for its engine
# (a generated connector's driver-pattern key) picks the dialect first; the
# driver module decides only when the name matches nothing registered
# (KI-GENERATED-WAREHOUSE-BATCH-PAGING). Dialects added since resolve by
# name inside each test, so one missing dialect fails only its own tests.
# --------------------------------------------------------------------------

@pytest.mark.parametrize("module,name,dialect", [
    ("psycopg2", "redshift", "REDSHIFT"), ("psycopg2", "postgresql", "POSTGRESQL"),
    ("psycopg2", None, "POSTGRESQL"), ("psycopg2", "", "POSTGRESQL"),
    ("pyodbc", "synapse", "SYNAPSE"), ("pyodbc", "sqlserver", "SQLSERVER_QMARK"),
    ("pyodbc", None, "SQLSERVER_QMARK"), ("pymssql", "sqlserver", "SQLSERVER_FORMAT"),
    ("pyathena", "athena", "ATHENA"), ("pyathena", None, "ATHENA"),
    ("mysql.connector", "mysql", "MYSQL"), ("oracledb", "oracle", "ORACLE"),
    ("acme_driver", "acme", "GENERIC"),
])
def test_a_shared_driver_gets_the_dialect_its_connector_names(module, name, dialect):
    assert sql_paging.dialect_for(module, name) is getattr(sql_paging, dialect)


def test_a_pyformat_driver_binds_by_name():
    """pyathena takes a dict and nothing else."""
    b = sql_paging.Binder("pyformat")
    assert (b.add(1), b.add("x")) == ("%(p1)s", "%(p2)s")
    assert b.params() == {"p1": 1, "p2": "x"}
    assert sql_paging.Binder("pyformat").params() is None


# sys.indexes still lists a Synapse PRIMARY KEY or UNIQUE constraint, which is
# NOT ENFORCED: duplicates on it are legal, so it is never paged by.
def test_synapse_trusts_no_key_and_orders_by_its_orderable_columns():
    synapse = getattr(sql_paging, "SYNAPSE")
    script = (
        ("sys.indexes", [(1, 1, "id", 0)]),
        ("sys.columns c WHERE", [("id", 56), ("shape", 240), ("doc", 241), ("name", 231)]),
    )
    cur = _Catalog(*script)
    assert _plan(synapse, cur) == sql_paging.ordered_offset_plan(["id", "name"])
    assert cur.executed == [(sql_paging.SQLSERVER_COLUMNS_SQL.format(p="?"), ("[app].[t]",))]
    assert _plan(synapse, _Catalog(*script), nominated="name") == sql_paging.ordered_offset_plan(["name", "id"])


_ATHENA_COLUMNS = ('SELECT column_name, data_type FROM information_schema.columns '
                   'WHERE table_schema = %(p1)s AND table_name = %(p2)s ORDER BY ordinal_position')


def test_athena_orders_by_its_orderable_columns_bound_by_name():
    athena = getattr(sql_paging, "ATHENA")
    cur = _Catalog(("information_schema.columns", [
        ("id", "bigint"), ("tags", "map(varchar, varchar)"), ("items", "array(integer)"),
        ("attrs", "row(a integer)"), ("doc", "json"), ("h", "HyperLogLog"), ("pct%", "varchar"),
        ("name", "varchar(64)"), ("ts", "timestamp(3)"),
    ]))
    # The Glue catalog stores names in lower case.
    assert _plan(athena, cur, table="Analytics.T", safe_table='"Analytics"."T"') == \
        sql_paging.ordered_offset_plan(["id", "name", "ts"])
    assert cur.executed == [(_ATHENA_COLUMNS, {"p1": "analytics", "p2": "t"})]


@pytest.mark.parametrize("database,expected", [("sales", "sales"), (None, "default")])
def test_athena_without_a_schema_reads_the_connections_database(database, expected):
    athena = getattr(sql_paging, "ATHENA")
    conn = _Conn()
    conn.schema_name = database
    cur = _Catalog(("information_schema.columns", [("id", "bigint")]))
    assert _plan(athena, cur, table="t", safe_table='"t"', conn=conn) == sql_paging.ordered_offset_plan(["id"])
    assert cur.executed == [(_ATHENA_COLUMNS, {"p1": expected, "p2": "t"})]


def test_athena_reads_the_catalog_a_three_part_name_names():
    athena = getattr(sql_paging, "ATHENA")
    cur = _Catalog(("information_schema.columns", [("id", "bigint")]))
    _plan(athena, cur, table="lake.analytics.t", safe_table='"lake"."analytics"."t"')
    assert cur.executed == [(_ATHENA_COLUMNS.replace("FROM information", 'FROM "lake".information'),
                             {"p1": "analytics", "p2": "t"})]


_RN = ('SELECT TOP (5) * FROM (SELECT *, ROW_NUMBER() OVER (ORDER BY {order}) AS "__rsync_rn" '
       'FROM "t"{where}) AS "__rsync_page" WHERE "__rsync_rn" > 10 ORDER BY "__rsync_rn"')


# A Synapse dedicated pool has neither LIMIT nor OFFSET ... FETCH; Trino
# (Athena) takes OFFSET before LIMIT and rejects the other order.
@pytest.mark.parametrize("dialect,plan,kwargs,sql,params", [
    ("SYNAPSE", sql_paging.ordered_offset_plan(["a", "b"]), {},
     _RN.format(order='"a", "b"', where=""), None),
    ("SYNAPSE", sql_paging.offset_plan(), {}, _RN.format(order="(SELECT NULL)", where=""), None),
    ("SYNAPSE", sql_paging.ordered_offset_plan(["a"]), {"inc_field": "u", "inc_value": "x"},
     _RN.format(order='"a"', where=' WHERE ("u" > ?)'), ("x",)),
    ("SYNAPSE", sql_paging.key_plan(["a"]), {"cursor": 1},
     'SELECT TOP (5) * FROM "t" WHERE "a" > ? ORDER BY "a"', (1,)),
    ("ATHENA", sql_paging.ordered_offset_plan(["a", "b"]), {},
     'SELECT * FROM "t" ORDER BY "a", "b" OFFSET 10 LIMIT 5', None),
    ("ATHENA", sql_paging.offset_plan(), {}, 'SELECT * FROM "t" OFFSET 10 LIMIT 5', None),
    ("ATHENA", sql_paging.ordered_offset_plan(["a"]), {"inc_field": "u", "inc_value": "x"},
     'SELECT * FROM "t" WHERE ("u" > %(p1)s) ORDER BY "a" OFFSET 10 LIMIT 5', {"p1": "x"}),
    ("ATHENA", sql_paging.key_plan(["a"]), {"cursor": 1},
     'SELECT * FROM "t" WHERE "a" > %(p1)s ORDER BY "a" LIMIT 5', {"p1": 1}),
])
def test_an_engine_without_limit_offset_pages_in_its_own_grammar(dialect, plan, kwargs, sql, params):
    assert sql_paging.page_query(getattr(sql_paging, dialect), plan, '"t"', limit=5, offset=10, **kwargs) == \
        (sql, params)


def test_a_row_number_page_strips_its_numbering_column():
    synapse = getattr(sql_paging, "SYNAPSE")
    cur = sql_paging.QueryCursor(lambda sql, params: (["id", "__rsync_rn"], [(1, 11), (2, 12)]))
    assert sql_paging.read_page(_Conn(), cur, synapse, "t", '"t"', limit=5, offset=10, stable=False) == \
        ([{"id": 1}, {"id": 2}], ["id"], {"paging_mode": "offset"})


# --------------------------------------------------------------------------
# Page SQL per dialect.
# --------------------------------------------------------------------------

KEY_AB = sql_paging.key_plan(["a", "b"])


@pytest.mark.parametrize("dialect,table,sql,params", [
    (sql_paging.POSTGRESQL, '"t"',
     'SELECT * FROM "t" WHERE ("a", "b") > (%s, %s) ORDER BY "a", "b" LIMIT 5', (1, 2)),
    (sql_paging.MYSQL, "`t`",
     "SELECT * FROM `t` WHERE (`a` >= %s AND (`a` > %s OR (`b` > %s))) ORDER BY `a`, `b` LIMIT 5", (1, 1, 2)),
    (sql_paging.SQLSERVER_QMARK, '"t"',
     'SELECT TOP (5) * FROM "t" WHERE ("a" >= ? AND ("a" > ? OR ("b" > ?))) ORDER BY "a", "b"', (1, 1, 2)),
    (sql_paging.ORACLE, '"t"',
     'SELECT * FROM "t" WHERE ("a" >= :1 AND ("a" > :2 OR ("b" > :3))) ORDER BY "a", "b" FETCH FIRST 5 ROWS ONLY',
     (1, 1, 2)),
])
def test_composite_key_page_sql(dialect, table, sql, params):
    assert sql_paging.page_query(dialect, KEY_AB, table, limit=5, cursor=[1, 2]) == (sql, params)


def test_a_first_column_checkpoint_re_reads_its_value():
    assert sql_paging.page_query(sql_paging.POSTGRESQL, KEY_AB, '"t"', limit=5, cursor=1) == \
        ('SELECT * FROM "t" WHERE "a" >= %s ORDER BY "a", "b" LIMIT 5', (1,))


def test_numeric_binds_follow_the_text_cursor_then_watermark_then_key():
    sql, params = sql_paging.page_query(
        sql_paging.ORACLE, sql_paging.key_plan(["ID"]), '"T"', limit=5,
        cursor=10, since_cursor=7, inc_field="UPDATED_AT", inc_value="2026-01-01")
    assert sql == 'SELECT * FROM "T" WHERE "ID" > :1 AND ("UPDATED_AT" > :2 OR "ID" > :3) ORDER BY "ID" FETCH FIRST 5 ROWS ONLY'
    assert params == (10, "2026-01-01", 7)


@pytest.mark.parametrize("plan,since", [
    (sql_paging.key_plan(["a", "b"]), [1, 2, 3]),     # a key of another width
    (sql_paging.key_plan(["a"]), {"locator": "(0,1)"}),
    (sql_paging.ordered_offset_plan(["a"]), 7),         # no key at all
    (sql_paging.offset_plan(), 7),
])
def test_a_since_cursor_that_cannot_be_placed_drops_the_delta(plan, since):
    """Too many rows is recoverable, too few is not: the run re-reads."""
    sql, params = sql_paging.page_query(sql_paging.POSTGRESQL, plan, '"t"', limit=5,
                                        since_cursor=since, inc_field="u", inc_value="x")
    assert "WHERE" not in sql and params is None, sql


def test_locator_page_sql():
    pg = sql_paging.locator_plan(select=f'*, ctid::text AS "{sql_paging.LOCATOR_ALIAS}"',
                                 predicate="ctid > {p}::tid", order="ctid")
    assert sql_paging.page_query(sql_paging.POSTGRESQL, pg, '"t"', limit=5, cursor={"locator": "(0,2)"}) == (
        'SELECT *, ctid::text AS "__rsync_locator" FROM "t" WHERE ctid > %s::tid ORDER BY ctid LIMIT 5', ("(0,2)",))
    # A key checkpoint on a table that lost its key re-reads rather than skips.
    assert sql_paging.page_query(sql_paging.POSTGRESQL, pg, '"t"', limit=5, cursor=41)[0] == \
        'SELECT *, ctid::text AS "__rsync_locator" FROM "t" ORDER BY ctid LIMIT 5'

    cur = _Catalog(("all_constraints", []), ("all_tables", [(None,)]))
    ora = _plan(sql_paging.ORACLE, cur, table="T", safe_table='"T"')
    assert sql_paging.page_query(sql_paging.ORACLE, ora, '"T"', limit=5, cursor={"locator": "AAAx"}) == (
        'SELECT rsync_t.*, ROWIDTOCHAR(rsync_t.ROWID) AS "__rsync_locator" FROM "T" rsync_t '
        'WHERE rsync_t.ROWID > CHARTOROWID(:1) ORDER BY rsync_t.ROWID FETCH FIRST 5 ROWS ONLY', ("AAAx",))


@pytest.mark.parametrize("dialect,plan,sql", [
    (sql_paging.POSTGRESQL, sql_paging.ordered_offset_plan(["a", "b"]),
     'SELECT * FROM "t" ORDER BY "a", "b" LIMIT 5 OFFSET 10'),
    (sql_paging.SQLSERVER_QMARK, sql_paging.ordered_offset_plan(["a"]),
     'SELECT * FROM "t" ORDER BY "a" OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY'),
    (sql_paging.SQLSERVER_QMARK, sql_paging.offset_plan(),
     'SELECT * FROM "t" ORDER BY (SELECT NULL) OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY'),
    (sql_paging.ORACLE, sql_paging.ordered_offset_plan(["a"]),
     'SELECT * FROM "t" ORDER BY "a" OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY'),
    (sql_paging.ORACLE, sql_paging.offset_plan(), 'SELECT * FROM "t" OFFSET 10 ROWS FETCH NEXT 5 ROWS ONLY'),
])
def test_offset_page_sql(dialect, plan, sql):
    assert sql_paging.page_query(dialect, plan, '"t"', limit=5, offset=10) == (sql, None)


# --------------------------------------------------------------------------
# What a page reports to the executor.
# --------------------------------------------------------------------------

def test_a_hidden_key_is_reported_as_a_string_and_stripped_from_the_rows():
    plan = sql_paging.key_plan(["my_row_id"], hidden=["my_row_id"])
    data, columns, fields = sql_paging.finish_page(
        plan, [{"name": "a", "my_row_id": 2 ** 53 + 1}], ["name", "my_row_id"])
    assert data == [{"name": "a"}] and columns == ["name"]
    assert fields == {"next_cursor": str(2 ** 53 + 1), "paging_mode": "keyset", "cursor_column": "my_row_id"}


def test_a_locator_is_reported_as_a_position_and_stripped_from_the_rows():
    plan = sql_paging.locator_plan(select="", predicate="", order="")
    data, columns, fields = sql_paging.finish_page(
        plan, [{"a": 1, sql_paging.LOCATOR_ALIAS: "(0,2)"}], ["a", sql_paging.LOCATOR_ALIAS])
    assert data == [{"a": 1}] and columns == ["a"]
    assert fields == {"next_cursor": {"locator": "(0,2)"}, "paging_mode": "locator"}


def test_a_page_reports_its_key_whatever_the_column_case():
    _, _, fields = sql_paging.finish_page(sql_paging.key_plan(["id"]), [{"ID": 5}], ["ID"])
    assert fields["next_cursor"] == 5


def test_an_offset_page_says_so():
    assert sql_paging.finish_page(sql_paging.ordered_offset_plan(["a"]), [{"a": 1}], ["a"])[2] == \
        {"paging_mode": "offset"}


# --------------------------------------------------------------------------
# Lockstep: the five export copies page through sql_paging identically, and
# every image that runs one ships the module.
# --------------------------------------------------------------------------

EXPORT_COPIES = [pathlib.Path(_connector_file(root)) for _, root, _ in CONNECTORS] + [TEMPLATE]
_BLOCK_START = 'module = (self.driver_pattern.get("module", "") or "").lower()\n            # Paging order'
_BLOCK_END = "result.update(paging)\n"
_IMPORT = "try:\n    import sql_paging  # type: ignore\nexcept Exception:  # pragma: no cover\n    sql_paging = None"


def _paging_block(text: str) -> str:
    start = text.index(_BLOCK_START)
    return text[start:text.index(_BLOCK_END, start) + len(_BLOCK_END)]


def test_the_five_export_copies_page_identically():
    texts = {p: p.read_text(encoding="utf-8") for p in EXPORT_COPIES}
    reference = _paging_block(texts[EXPORT_COPIES[0]])
    for path, text in texts.items():
        assert text.count(_BLOCK_START) == 1, f"{path}: expected one export paging block"
        assert _IMPORT in text, f"{path}: missing the guarded sql_paging import"
        assert _paging_block(text) == reference, f"{path}: export paging drifted from {EXPORT_COPIES[0]}"


_IMPORTS_SQL_PAGING = re.compile(r"^\s*import sql_paging\b", re.M)
_COPY = "COPY --from=shared sql_paging.py /app/sql_paging.py"


def test_every_connector_image_that_pages_with_sql_paging_ships_it():
    users = [p for p in pathlib.Path(_PUBLIC).rglob("connector.py")
             if _IMPORTS_SQL_PAGING.search(p.read_text(encoding="utf-8"))]
    assert len(users) >= 4, users
    for path in users:
        dockerfile = path.with_name("Dockerfile")
        assert _COPY in dockerfile.read_text(encoding="utf-8"), f"{dockerfile} does not ship sql_paging.py"


def _removed_by_the_cut(rel_path):
    """llm-service/tests/_cut_collection.removed_by_the_cut, loaded by path:
    this suite is not under llm-service/tests, so that module is not on sys.path."""
    module = sys.modules.get("_cut_collection")
    if module is None:
        spec = importlib.util.spec_from_file_location(
            "_cut_collection", _REPO / "llm-service" / "tests" / "_cut_collection.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module.removed_by_the_cut(rel_path)


def test_every_generated_connector_dockerfile_ships_sql_paging():
    """A generated database connector renders connector_database.py.j2, whose
    export needs the module. Every emitter that ships the shared warehouse
    adapter ships sql_paging next to it."""
    marker = "COPY --from=shared warehouse_adapters.py /app/warehouse_adapters.py"
    src = _REPO / "llm-service" / "src"
    emitters = [p for p in list(src.rglob("*.py")) + list(src.rglob("Dockerfile*.j2"))
                if marker in p.read_text(encoding="utf-8")]
    # session_fast_path.py, deployment/service.py and templates/Dockerfile.j2;
    # the floor keeps a glob that matches nothing from passing. The public cut
    # strips session_fast_path.py with the rest of the generator's agents.
    floor = 3
    if _removed_by_the_cut("src/agents/tool_generator/agents/session_fast_path.py"):
        floor -= 1
    assert len(emitters) >= floor, emitters
    for path in emitters:
        text = path.read_text(encoding="utf-8")
        assert text.count(_COPY) == text.count(marker), f"{path}: a Dockerfile emitter lacks sql_paging.py"
