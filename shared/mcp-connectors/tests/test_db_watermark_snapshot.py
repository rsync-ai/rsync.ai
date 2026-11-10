"""A batch sweep's watermark is max(updated_at) read before its first page.

KI-DB-BATCH-PAGING-CAN-SKIP-ROWS cause (3), INCREMENTAL.md §2. A database
sweep pages by key and reported each page's own max(updated_at); the executor
kept the last one as the next sweep's `since`. A row the key cursor had
already passed, updated while the sweep was still reading, got a timestamp
below a later page's max and fell outside the next sweep's
`updated_at > since` for good. The connectors now read max(updated_at) once
before the first page (`snapshot: true`); every row updated after that read is
above it, whatever its key.

The first test is the repro: the real PostgreSQL connector code against a real
(in-memory sqlite) table, driven page by page the way the executor drives it,
with the updates landing between pages. The rest pin the dialect clock SQL,
the lag hold, each connector's wiring and the shared block's lockstep.
"""
from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import sqlite3
import sys
from datetime import datetime, timedelta, timezone

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_PUBLIC = os.path.abspath(os.path.join(_HERE, "..", "public"))
_REPO = pathlib.Path(_HERE).resolve().parents[2]
TEMPLATE = _REPO / "llm-service" / "src" / "agents" / "tool_generator" / "templates" / "connector_database.py.j2"

# (name, connector root, server class, driver module the connector ships with)
CONNECTORS = [
    ("postgresql", os.path.join(_PUBLIC, "postgresql"), "PostgresqlMCPServer", "psycopg2"),
    ("mysql", os.path.join(_PUBLIC, "database", "mysql"), "MysqlMCPServer", "pymysql"),
    ("oracle", os.path.join(_PUBLIC, "database", "oracle"), "OracleMCPServer", "oracledb"),
    # The sqlserver connector's class is literally named MysqlMCPServer.
    ("sqlserver", os.path.join(_PUBLIC, "database", "sqlserver"), "MysqlMCPServer", "pyodbc"),
]
BY_NAME = {c[0]: c for c in CONNECTORS}

CONFIG = {"host": "db", "port": 5432, "database": "app", "username": "u", "password": "p"}


def _connector_file(root: str) -> str:
    with open(os.path.join(root, "latest.json")) as f:
        current = json.load(f)["current_version"]
    return os.path.join(root, "versions", current, "connector.py")


def _load(name: str):
    _, root, class_name, _ = BY_NAME[name]
    path = _connector_file(root)
    for p in (root, os.path.dirname(path), _PUBLIC):
        if p not in sys.path:
            sys.path.insert(0, p)
    spec = importlib.util.spec_from_file_location(f"_wm_snapshot_{name}", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    except Exception as exc:  # pragma: no cover - environment-dependent
        pytest.skip(f"{name} connector not importable offline: {exc}")
    return getattr(mod, class_name)


# --------------------------------------------------------------------------
# The repro: a real table, paged like the executor pages it.
# --------------------------------------------------------------------------

class _KeepOpen:
    """The export closes its connection; the table has to outlive a page."""

    def __init__(self, conn):
        self._conn = conn

    def rollback(self):
        self._conn.rollback()

    def close(self):
        pass


class _SqliteCursor:
    """sqlite3 rejects `execute(sql, None)`, which psycopg2 accepts."""

    def __init__(self, db):
        self._cur = db.cursor()

    def execute(self, sql, params=None):
        return self._cur.execute(sql, params or ())

    def __getattr__(self, name):
        return getattr(self._cur, name)


def _sqlite_server(db):
    srv = _load("postgresql")()
    srv.driver_pattern = dict(srv.driver_pattern, module="sqlite3")
    srv._get_connection = lambda config: _KeepOpen(db)
    srv._get_cursor = lambda conn, *a, **k: _SqliteCursor(db)
    return srv


def _orders_table():
    db = sqlite3.connect(":memory:")
    db.execute("CREATE TABLE orders (id INTEGER PRIMARY KEY, name TEXT, updated_at TEXT)")
    db.executemany(
        "INSERT INTO orders VALUES (?, ?, ?)",
        [(i, f"o{i}", f"2026-09-29T09:00:0{i}") for i in range(1, 7)],
    )
    db.commit()
    return db


def _fold(kept, page, since):
    """What the executor keeps (foldSweepWatermark in
    backend-orchestrator/internal/agents/executor/batch_sweep_watermark.go):
    the sweep-start snapshot when the connector reports one, else the max over
    pages and `since`. ISO strings of one shape compare as text."""
    if not page:
        return kept
    if kept and kept.get("snapshot"):
        return kept
    floor = max(v for v in (since, (kept or {}).get("value")) if v) if (since or kept) else None
    if floor and str(page["value"]) < floor:
        page = dict(page, value=floor)
    return page


def _sweep(srv, since=None, since_cursor=None, after_page=None, limit=2):
    """One sweep, the executor's way: key-ordered pages until a short one."""
    kept, read, cursor, pages = None, [], None, 0
    while True:
        params = {"config": dict(CONFIG), "table": "orders", "format": "json",
                  "limit": limit, "offset": 0, "use_keyset_paging": True}
        if cursor is not None:
            params["cursor"] = cursor
        if since:
            params["since"] = since
        if since_cursor is not None:
            params["since_cursor"] = since_cursor
        res = srv.export(params)
        assert res.get("success") is not False, res
        rows = res.get("data") or []
        if not rows:
            break
        read.extend(rows)
        kept = _fold(kept, (res.get("stats") or {}).get("watermark"), since)
        cursor = res.get("next_cursor")
        pages += 1
        if after_page:
            after_page(pages)
        if len(rows) < limit:
            break
    high = max((r["id"] for r in read), default=since_cursor)
    return read, (kept or {}).get("value", since), high


def test_an_update_behind_the_key_cursor_reaches_the_next_sweep():
    db = _orders_table()
    srv = _sqlite_server(db)

    def updates_while_reading(page):
        if page == 1:  # rows 1-2 are already read
            db.execute("UPDATE orders SET updated_at = '2026-09-29T10:04:00' WHERE id = 1")
            db.execute("UPDATE orders SET updated_at = '2026-09-29T10:05:00' WHERE id = 6")
            db.commit()

    _, watermark, high = _sweep(srv, after_page=updates_while_reading)

    second, next_watermark, high = _sweep(srv, since=watermark, since_cursor=high)
    updated = {r["id"]: r["updated_at"] for r in second}
    assert updated.get(1) == "2026-09-29T10:04:00", (
        f"row 1's update at 10:04 was never read: the first sweep kept watermark "
        f"{watermark!r} and the next sweep returned {updated}"
    )
    watermark = next_watermark

    third, _, _ = _sweep(srv, since=watermark, since_cursor=high)
    assert third == [], f"an idle table must converge to empty sweeps, read {third}"


def test_only_a_sweeps_first_page_reads_the_snapshot():
    db = _orders_table()
    srv = _sqlite_server(db)
    base = {"config": dict(CONFIG), "table": "orders", "format": "json", "limit": 2,
            "use_keyset_paging": True}

    first = srv.export(dict(base))
    assert first["stats"]["watermark"] == {
        "field": "updated_at", "value": "2026-09-29T09:00:06", "snapshot": True,
    }, first.get("stats")

    later = srv.export(dict(base, cursor=2))
    assert "snapshot" not in later["stats"]["watermark"], later["stats"]

    # Nothing above `since`: the page is empty and the snapshot echoes it.
    idle = srv.export(dict(base, since="2026-09-29T09:00:06"))
    assert idle.get("data") == []
    assert idle["stats"]["watermark"]["value"] == "2026-09-29T09:00:06"


# --------------------------------------------------------------------------
# The dialect clock and the lag hold.
# --------------------------------------------------------------------------

class ScriptedCursor:
    """Answers the snapshot's two reads, then the page query."""

    def __init__(self, snapshot_row, columns=("id", "name", "updated_at"), rows=()):
        self._snapshot_row = snapshot_row
        self._columns = list(columns)
        self._page_rows = list(rows)
        self.executed = []
        self._rows, self._cols, self._one = [], [], None

    def execute(self, sql, params=None):
        self.executed.append((" ".join(sql.split()), params))
        s = self.executed[-1][0]
        self._rows, self._cols, self._one = [], [], None
        if "WHERE 1 = 0" in s:
            self._cols = list(self._columns)
        elif "MAX(" in s:
            self._cols = ["wm_max", "wm_utc", "wm_local"]
            self._one = self._snapshot_row
        else:
            self._cols = list(self._columns)
            self._rows = [dict(zip(self._columns, r)) for r in self._page_rows]

    @property
    def description(self):
        return [(c,) for c in self._cols]

    def fetchone(self):
        return self._one

    def fetchall(self):
        return list(self._rows)

    def close(self):
        pass


class _Conn:
    def rollback(self):
        pass

    def close(self):
        pass


def _scripted_server(name, cursor):
    srv = _load(name)()
    module = BY_NAME[name][3]
    assert (srv.driver_pattern.get("module") or "").lower() == module
    srv._get_connection = lambda config: _Conn()
    srv._get_cursor = lambda conn, *a, **k: cursor
    return srv


def _export(srv, **params):
    # A named cursor_column pages by it with no catalog read (sql_paging), so
    # the scripted cursor only ever sees the snapshot and the page.
    p = {"config": dict(CONFIG), "table": "orders", "format": "json", "limit": 1000,
         "use_keyset_paging": True, "cursor_column": "id"}
    p.update(params)
    return srv.export(p)


UTC_NOW = datetime(2026, 9, 29, 10, 0, 0)          # naive, as the DB returns it
LOCAL_NOW = datetime(2026, 9, 29, 15, 30, 0)        # the session's zone


@pytest.mark.parametrize("value,config,want", [
    # A commit a second old: held 5 s behind the database clock, so a
    # transaction that stamped its row earlier but commits after this read is
    # still above the next sweep's `since`.
    (UTC_NOW.replace(tzinfo=timezone.utc) - timedelta(seconds=1), {},
     "2026-09-29T09:59:55+00:00"),
    # Naive columns are compared with the session's local clock.
    (LOCAL_NOW - timedelta(seconds=1), {}, "2026-09-29T15:29:55"),
    # Old values are kept as read: holding them back would re-read forever.
    (UTC_NOW.replace(tzinfo=timezone.utc) - timedelta(minutes=10), {},
     "2026-09-29T09:50:00+00:00"),
    # A value from another zone or clock (well off `now`) is never held.
    (LOCAL_NOW + timedelta(hours=2), {}, "2026-09-29T17:30:00"),
    # The lag is configurable on the connection; 0 turns the hold off.
    (UTC_NOW.replace(tzinfo=timezone.utc) - timedelta(seconds=1), {"incremental_lag_seconds": 0},
     "2026-09-29T09:59:59+00:00"),
    (UTC_NOW.replace(tzinfo=timezone.utc) - timedelta(seconds=1), {"incremental_lag_seconds": "30"},
     "2026-09-29T09:59:30+00:00"),
])
def test_the_snapshot_is_held_behind_the_database_clock(value, config, want):
    cursor = ScriptedCursor((value, UTC_NOW, LOCAL_NOW), rows=[(1, "a", value)])
    srv = _scripted_server("postgresql", cursor)
    res = _export(srv, config=dict(CONFIG, **config))
    assert res["stats"]["watermark"] == {"field": "updated_at", "value": want, "snapshot": True}
    assert res["max_watermark"] == want


DIALECT_SQL = {
    "postgresql": ("now() AT TIME ZONE 'UTC' AS wm_utc, LOCALTIMESTAMP AS wm_local", '"updated_at" > %s'),
    "mysql": ("UTC_TIMESTAMP(6) AS wm_utc, NOW(6) AS wm_local", "`updated_at` > %s"),
    "sqlserver": ("SYSUTCDATETIME() AS wm_utc, SYSDATETIME() AS wm_local", '"updated_at" > ?'),
    "oracle": ("SYS_EXTRACT_UTC(SYSTIMESTAMP) AS wm_utc, LOCALTIMESTAMP AS wm_local", '"updated_at" > :1'),
}


@pytest.mark.parametrize("name", [c[0] for c in CONNECTORS])
def test_each_connector_reports_the_sweep_start_snapshot(name):
    old = datetime(2026, 9, 29, 8, 0, 0)
    newest_row = datetime(2026, 9, 29, 9, 59, 0)  # a later page's max: not the watermark
    cursor = ScriptedCursor((old, UTC_NOW, LOCAL_NOW), rows=[(1, "a", newest_row)])
    srv = _scripted_server(name, cursor)
    res = _export(srv, since="2026-09-29T07:00:00", incremental_field="updated_at")
    assert res.get("success") is not False, res
    assert res["stats"]["watermark"] == {
        "field": "updated_at", "value": "2026-09-29T08:00:00", "snapshot": True,
    }, res.get("stats")

    clocks, since_filter = DIALECT_SQL[name]
    snap = [(q, b) for q, b in cursor.executed if "MAX(" in q]
    assert len(snap) == 1, cursor.executed
    q, binds = snap[0]
    assert clocks in q and q.endswith(since_filter), q
    assert tuple(binds) == ("2026-09-29T07:00:00",)
    # The snapshot is read before the page, never after it.
    assert cursor.executed.index(snap[0]) < len(cursor.executed) - 1


@pytest.mark.parametrize("name", [c[0] for c in CONNECTORS])
def test_a_failed_snapshot_falls_back_to_the_page_max(name):
    """A read the database refuses must not fail the export: the page max is
    what every connector reported before, and the executor still folds it."""

    class Refusing(ScriptedCursor):
        def execute(self, sql, params=None):
            if "MAX(" in sql:
                self.executed.append((" ".join(sql.split()), params))
                raise RuntimeError("permission denied")
            super().execute(sql, params)

    row_max = datetime(2026, 9, 29, 9, 0, 0)
    cursor = Refusing(None, rows=[(1, "a", row_max)])
    srv = _scripted_server(name, cursor)
    res = _export(srv)
    assert res.get("success") is not False, res
    assert res["stats"]["watermark"] == {"field": "updated_at", "value": "2026-09-29T09:00:00"}


def test_a_table_without_the_field_reports_no_watermark():
    cursor = ScriptedCursor((None, UTC_NOW, LOCAL_NOW), columns=("id", "name"), rows=[(1, "a")])
    srv = _scripted_server("postgresql", cursor)
    res = _export(srv)
    assert "watermark" not in (res.get("stats") or {}), res.get("stats")
    assert not any("MAX(" in q for q, _ in cursor.executed), cursor.executed


# --------------------------------------------------------------------------
# One block, five files.
# --------------------------------------------------------------------------

BEGIN = "# BEGIN watermark snapshot block"
END = "# END watermark snapshot block"


def _block(path) -> str:
    text = pathlib.Path(path).read_text(encoding="utf-8")
    assert text.count(BEGIN) == 1 and text.count(END) == 1, (
        f"{path}: want exactly one BEGIN and one END marker, "
        f"found {text.count(BEGIN)} and {text.count(END)}"
    )
    start = text.index(BEGIN)
    return text[start:text.index(END, start) + len(END)]


def test_the_template_block_reaches_generated_code_verbatim():
    block = _block(TEMPLATE)
    assert "def _watermark_snapshot(" in block and "def _hold_back_watermark(" in block
    assert "{{" not in block and "{%" not in block and "{#" not in block


@pytest.mark.parametrize("name", [c[0] for c in CONNECTORS])
def test_each_connector_carries_the_templates_block(name):
    path = _connector_file(BY_NAME[name][1])
    assert _block(path) == _block(TEMPLATE), (
        f"the watermark snapshot block in {path} differs from the template's; "
        "change them together"
    )


@pytest.mark.parametrize("path", [TEMPLATE] + [_connector_file(c[1]) for c in CONNECTORS],
                         ids=["template"] + [c[0] for c in CONNECTORS])
def test_each_export_reads_the_snapshot_before_its_page(path):
    text = pathlib.Path(path).read_text(encoding="utf-8")
    start = text.index("    def export(self, params: Dict = None):")
    export = text[start:text.index("\n    def ", start + 10)]
    call = export.index("snapshot = self._watermark_snapshot(")
    page = export.index("sql_paging.read_page(")
    assert call < page, f"{path}: the snapshot must be read before the first page query"
    assert '"snapshot": True' in export
