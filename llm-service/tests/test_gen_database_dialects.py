"""A generated database connector reads every row once, on every engine it
knows (KI-GENERATED-WAREHOUSE-BATCH-PAGING).

Bug class: a generated connector that chooses its SQL dialect by driver module
alone. Several engines share a driver with another engine and speak a different
dialect over it:

* Redshift rides psycopg2, so its sweeps paged as PostgreSQL: a declared,
  unenforced primary key was taken as unique and keyset paging skipped rows
  that shared a key value; its watermark read called ``now()``, which Redshift
  runs on the leader node only and refuses next to a table.
* Synapse rides pyodbc, so it paged as SQL Server: the same trusted key, and
  ``OFFSET ... FETCH`` once the key was gone, which a dedicated pool rejects.
* Athena's pyathena matched no dialect at all: ``LIMIT n OFFSET m`` (Trino
  wants ``OFFSET m LIMIT n``) with no ``ORDER BY``, and tuple binds, which
  pyathena refuses.

The same template called ``_canonical_sql_type``, which it never defined, so
every psycopg2 connector it rendered discovered its tables with no columns;
the exception was caught and reported as a warning. The generator now refuses
rendered code that calls a name nothing defines.

The repros render connectors from the template, the way the tool generator
does, and drive the rendered export page by page the way the executor drives
it, against ``_Engine``: SQLite behind each engine's grammar, binds and catalog,
returning an unordered query's rows in a new order every time.
"""
from __future__ import annotations

import functools
import importlib.util
import json
import re
import sqlite3
import sys
import tempfile
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import pytest

_TOOLGEN = str(Path(__file__).resolve().parents[1] / "src" / "agents" / "tool_generator")
if _TOOLGEN not in sys.path:
    sys.path.insert(0, _TOOLGEN)
_SHARED = Path(__file__).resolve().parents[2] / "shared" / "mcp-connectors"
_PUBLIC = _SHARED / "public"
if str(_PUBLIC) not in sys.path:
    sys.path.insert(0, str(_PUBLIC))

import sql_paging  # noqa: E402
from contracts.python_literal_sanitizer import sanitize_python_json_literals  # noqa: E402
from generator.builder import ConnectorBuilder  # noqa: E402
from schemas.spec import (  # noqa: E402
    AuthConfig,
    AuthType,
    ConfigField,
    ConnectorCategory,
    ConnectorSpec,
    Protocol,
    ResourceConfig,
    create_api_operations,
    create_database_operations,
)
from validation.post_generation import PostGenerationValidator  # noqa: E402

# --------------------------------------------------------------------------
# Rendering
# --------------------------------------------------------------------------


def _database_spec(name: str, category=ConnectorCategory.DATA_WAREHOUSE, destination=False) -> ConnectorSpec:
    return ConnectorSpec(
        name=name, display_name=name.title(), category=category,
        supports_source=True, supports_destination=destination, supports_ddl=destination,
        auth=AuthConfig(type=AuthType.BASIC, username_env="DB_USER", password_env="DB_PASSWORD"),
        operations=create_database_operations(),
    )


def _api_spec(graphql: bool, destination: bool) -> ConnectorSpec:
    kw = dict(
        name="probe_api", display_name="Probe", category=ConnectorCategory.API_SAAS,
        supports_source=True, supports_destination=destination, base_url="https://api.test.com/v1",
        auth=AuthConfig(type=AuthType.BEARER, header_name="Authorization", header_prefix="Bearer",
                        env_var="MCP_API_KEY"),
        config_fields=[ConfigField(name="api_key", required=True, secret=True, env_var="MCP_API_KEY")],
        operations=create_api_operations(),
        resources=[
            ResourceConfig(name="contacts", endpoint="/contacts", supports_list=True, supports_get=True,
                           supports_create=True, supports_update=True, supports_delete=True),
            ResourceConfig(name="pet", endpoint="/pet/{petId}", supports_list=True, supports_get=True,
                           supports_update=True, supports_delete=True),
        ],
    )
    if graphql:
        kw["protocol"] = Protocol.GRAPHQL
    return ConnectorSpec(**kw)


def _render(spec: ConnectorSpec) -> str:
    builder = ConnectorBuilder()
    template = builder._select_connector_template(spec)
    return sanitize_python_json_literals(
        builder._render_template(template, {"spec": spec, "generation_timestamp": "test"}))


@functools.lru_cache(maxsize=None)
def _generated(name: str, category=ConnectorCategory.DATA_WAREHOUSE, destination=False):
    """The module the template renders for ``name``, imported the way its image
    runs it: next to base_connector.py and the shared helpers. sys.path and
    sys.modules are put back afterwards."""
    code = _render(_database_spec(name, category, destination))
    path = Path(tempfile.mkdtemp(prefix=f"gen_{name}_")) / name / "connector.py"
    path.parent.mkdir(parents=True)
    path.write_text(code)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [str(_PUBLIC), str(_SHARED)]
    spec = importlib.util.spec_from_file_location(f"_gen_database_{name}", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    return mod


def _connector(name: str, conn, category=ConnectorCategory.DATA_WAREHOUSE, destination=False):
    mod = _generated(name, category, destination)
    cls = next(v for k, v in vars(mod).items()
               if isinstance(v, type) and k.endswith("Connector") and v.__module__ == mod.__name__)
    srv = cls()
    srv._get_connection = lambda config: conn
    srv._get_cursor = lambda c, as_dict=True: c.cursor(as_dict=as_dict)
    return srv


# --------------------------------------------------------------------------
# The engines
# --------------------------------------------------------------------------

SAFE_TABLE = '"analytics"."t"'
TABLE = "analytics.t"

# (name, catalog type) per engine: a non-unique id, a name, and a column the
# engine cannot order where it has one, which must stay out of ORDER BY.
COLUMNS = {
    "redshift": [("id", "integer"), ("name", "character varying(64)"), ("doc", "super")],
    # sys.columns system_type_id: int, nvarchar, nvarchar.
    "synapse": [("id", 56), ("name", 231), ("doc", 231)],
    "athena": [("id", "bigint"), ("name", "varchar"), ("doc", "map(varchar, varchar)")],
}
# The ORDER BY a stable sweep of `t` needs: every column the engine can order.
ORDER = {
    "redshift": '"id", "name"',
    "synapse": '"id", "name", "doc"',
    "athena": '"id", "name"',
}
_SHUFFLES = [7, 331, 13, 523, 101, 787, 211, 29, 613, 409]


class _EngineError(Exception):
    pass


class _Engine:
    """Table `t` in SQLite, read through one engine's SQL."""

    def __init__(self, name: str, rows, extra_columns=(), snapshot_id=None, driver_datetimes=False):
        self.name = name
        self.columns = list(COLUMNS[name]) + list(extra_columns)
        self.db = sqlite3.connect(":memory:")
        self.db.execute("CREATE TABLE t (" + ", ".join(f'"{c}"' for c, _ in self.columns) + ")")
        self.db.executemany(f"INSERT INTO t VALUES ({', '.join('?' for _ in self.columns)})", rows)
        self.db.commit()
        self.pages: list = []
        self._unordered = 0
        # Athena: an Iceberg table's current snapshot; None is a Hive table,
        # which has no "$snapshots".
        self.snapshot_id = snapshot_id
        self.snapshot_reads = 0
        self.pins: list = []
        # The watermark read returns datetimes, as a driver does for a
        # timestamp; SQLite returns text.
        self.driver_datetimes = driver_datetimes
        self.statements: list = []
        self.rowcount = -1
        # Redshift, like PostgreSQL, refuses every statement after an error
        # until the transaction ends.
        self.aborted = False

    # Each engine's catalog, as the paging module reads it.
    def _catalog(self, sql, params):
        names = [c for c, _ in self.columns]
        if self.name == "redshift":
            if "svv_columns" in sql:
                ok = tuple(params or ()) == ("analytics", "t")
                return ["column_name", "data_type"], (self.columns if ok else [])
            if "pg_constraint" in sql:
                # A declared primary key: Redshift keeps it and never enforces it.
                return ["column_name"], [("id",)]
            if "pg_catalog" in sql:
                raise _EngineError("unnest/ctid/to_regclass are not supported on Redshift tables")
        if self.name == "synapse":
            if "sys.indexes" in sql:
                # A dedicated pool lists a NOT ENFORCED primary key like any other.
                return ["index_id", "is_primary_key", "name", "is_nullable"], [(1, 1, "id", 0)]
            if "sys.columns" in sql:
                return ["name", "system_type_id"], self.columns
        if self.name == "athena" and "information_schema.columns" in sql:
            ok = set((params or {}).values()) == {"analytics", "t"}
            return ["column_name", "data_type"], (self.columns if ok else [])
        del names
        return None

    # Each engine's clocks, as SQLite's (UTC) now.
    CLOCKS = {
        "redshift": ["GETDATE()"],
        "synapse": ["SYSUTCDATETIME()", "SYSDATETIME()"],
        "athena": ["CAST(current_timestamp AT TIME ZONE 'UTC' AS timestamp)", "localtimestamp"],
    }

    def _grammar(self, sql, params):
        """Rewrite the engine's SQL and binds into SQLite's, refusing what the
        engine refuses."""
        for clock in self.CLOCKS.get(self.name, []):
            sql = sql.replace(clock, "datetime('now')")
        if self.name == "redshift":
            if re.search(r"\bnow\(\)|\bLOCALTIMESTAMP\b", sql, re.I) and " FROM " in sql:
                raise _EngineError(
                    "Specified types or functions (one per INFO message) not supported on Redshift tables.")
            # PostgreSQL write SQL Redshift does not have.
            for refused in (r"\bON CONFLICT\b", r"\bSAVEPOINT\b", r"\bCREATE (UNIQUE )?INDEX\b",
                            r"\bADD COLUMN IF NOT EXISTS\b", r"\bREPLICA IDENTITY\b"):
                if re.search(refused, sql):
                    raise _EngineError(f"syntax error at or near {refused!r}")
            # A declared primary key is informational: Redshift never enforces it.
            if sql.startswith("CREATE TABLE"):
                sql = re.sub(r",\s*PRIMARY KEY \([^)]*\)", "", sql)
        if self.name == "synapse":
            # SQL Server write SQL a dedicated pool does not have: a unique
            # index, a table value constructor.
            if re.search(r"\bCREATE UNIQUE INDEX\b", sql) or re.search(r"\(VALUES \(", sql):
                raise _EngineError("Parse error: not supported in this version of SQL Server.")
            if re.search(r"\bOFFSET\b.*\bFETCH\b", sql) or re.search(r"\bLIMIT\b", sql):
                raise _EngineError("Parse error: Incorrect syntax near 'OFFSET'/'LIMIT'.")
            m = re.match(r"SELECT TOP \((\d+)\) ", sql)
            if m:
                sql = "SELECT " + sql[m.end():] + f" LIMIT {m.group(1)}"
        if self.name == "athena":
            if params is not None and not isinstance(params, dict):
                raise _EngineError("Unsupported parameter (Support for dict only)")
            if re.search(r"\bLIMIT \d+ OFFSET\b", sql):
                raise _EngineError("line 1: mismatched input 'OFFSET'")
            m = re.search(r" FOR VERSION AS OF (-?\d+)", sql)
            if m:
                if int(m.group(1)) != self.snapshot_id:
                    raise _EngineError(f"ICEBERG_INVALID_SNAPSHOT_ID: Invalid snapshot [{m.group(1)}]")
                self.pins.append(int(m.group(1)))
                sql = sql[:m.start()] + sql[m.end():]
            # Trino compares a column only with a value of its own type.
            types = {c: str(t).split("(")[0] for c, t in self.columns}
            for column, key in re.findall(r'"(\w+)" > %\((\w+)\)s', sql):
                value = (params or {}).get(key)
                kind = types.get(column, "")
                if (kind in ("timestamp", "date", "bigint") and isinstance(value, str)) or (
                        kind == "varchar" and isinstance(value, date)):
                    other = "varchar" if isinstance(value, str) else type(value).__name__
                    raise _EngineError(f"Cannot apply operator: {kind} > {other}")
            sql = re.sub(r"\bOFFSET (\d+) LIMIT (\d+)", r"LIMIT \2 OFFSET \1", sql)
            sql = re.sub(r"%\((\w+)\)s", r":\1", sql)
            # pyathena sends a datetime as TIMESTAMP '...', a date as DATE '...'.
            if params is not None:
                params = {k: v.strftime("%Y-%m-%d %H:%M:%S") if isinstance(v, datetime)
                          else v.isoformat() if isinstance(v, date) else v for k, v in params.items()}
        return re.sub(r"%s", "?", sql), params

    def run(self, sql, params=None):
        if self.aborted:
            raise _EngineError("current transaction is aborted, commands ignored until end of transaction block")
        self.statements.append(sql)
        try:
            return self._run(sql, params)
        except (_EngineError, sqlite3.Error):
            self.aborted = self.name == "redshift"
            raise

    def _run(self, sql, params):
        answer = self._catalog(sql, params)
        if answer is not None:
            return answer
        if "$snapshots" in sql:
            self.snapshot_reads += 1
            if self.snapshot_id is None:
                raise _EngineError("TABLE_NOT_FOUND: Table 'analytics.t$snapshots' does not exist")
            return ["snapshot_id"], [(self.snapshot_id,)]
        sql, params = self._grammar(sql, params)
        sql = sql.replace(SAFE_TABLE, "t").replace("[analytics].[t]", "t")
        if " LIMIT " in sql and " ORDER BY " not in sql:
            mult = _SHUFFLES[self._unordered % len(_SHUFFLES)]
            self._unordered += 1
            sql = sql.replace(" LIMIT ", f" ORDER BY (rowid * {mult}) % 1009 LIMIT ", 1)
        if " LIMIT " in sql or "__rsync_rn" in sql:
            self.pages.append(sql)
        if params is None:
            cur = self.db.execute(sql)
        else:
            cur = self.db.execute(sql, params if isinstance(params, dict) else tuple(params))
        names, rows = [d[0] for d in (cur.description or [])], cur.fetchall()
        self.rowcount = cur.rowcount
        if self.name == "athena":
            # Athena has no transactions: each statement commits as it runs.
            self.db.commit()
        if self.driver_datetimes and "wm_max" in names:
            rows = [tuple(datetime.fromisoformat(v) if isinstance(v, str) else v for v in r) for r in rows]
        return names, rows

    def type_of(self, column):
        return dict(self.columns).get(column)


class _Cursor:
    def __init__(self, engine, as_dict):
        self._engine = engine
        self._as_dict = as_dict
        self._rows: list = []
        self.description = None

    def execute(self, sql, params=None):
        self._engine.rowcount = -1
        names, rows = self._engine.run(sql, params)
        self.rowcount = self._engine.rowcount
        # pyathena reports the engine's type name, e.g. "timestamp".
        self.description = [(n, self._engine.type_of(n), None, None, None, None, None) for n in names]
        self._rows = [dict(zip(names, r)) if self._as_dict else tuple(r) for r in rows]

    def executemany(self, sql, seq):
        count = 0
        for params in seq:
            self.execute(sql, params)
            count += max(self.rowcount, 0)
        self.rowcount = count

    def fetchall(self):
        rows, self._rows = self._rows, []
        return rows

    def fetchone(self):
        return self._rows.pop(0) if self._rows else None

    def close(self):
        pass


class _Conn:
    def __init__(self, engine):
        self._engine = engine

    def cursor(self, *a, as_dict=True, **k):
        return _Cursor(self._engine, as_dict)

    def rollback(self):
        self._engine.db.rollback()
        self._engine.aborted = False

    def commit(self):
        # psycopg2: COMMIT of an aborted transaction is a ROLLBACK, not an error.
        if self._engine.aborted:
            self._engine.db.rollback()
        else:
            self._engine.db.commit()
        self._engine.aborted = False

    def close(self):
        pass


def _rows(n=14):
    """Two rows per id: a declared key that is not unique."""
    return [(i // 2, f"n{i:02d}", "{}") for i in range(n)]


def _sweep(name, engine, limit=3, **extra):
    """One sweep, the executor's way (executor.go table loop,
    batch_page_cursor.go readPageCursor): `use_keyset_paging` and never a
    `cursor_column`; an OFFSET page drops any cursor; a `next_cursor` goes back
    as `cursor`, through JSON decoded as Go does, every number a float64;
    `offset` steps only while no cursor is held."""
    export = _connector(name, _Conn(engine)).export
    read, offset, cursor, pages = [], 0, None, 0
    while pages < 50:
        params = {"config": {"host": "h", "database": "d"}, "table": TABLE, "limit": limit,
                  "offset": 0 if cursor is not None else offset, "use_keyset_paging": True, **extra}
        if cursor is not None:
            params["cursor"] = cursor
        res = export(params)
        assert res.get("success") is not False, res
        rows = res.get("data") or []
        for r in rows:
            assert set(r) == {c for c, _ in engine.columns}, f"a paging column leaked into the rows: {r}"
        read.extend((r["id"], r["name"]) for r in rows)
        pages += 1
        if (res.get("paging_mode") or "").lower() == "offset":
            cursor = None
        elif res.get("next_cursor") is not None:
            cursor = json.loads(json.dumps(res["next_cursor"]), parse_int=float)
        if cursor is None:
            offset += len(rows)
        if len(rows) < limit:
            break
    return read


# --------------------------------------------------------------------------
# Sweeps
# --------------------------------------------------------------------------

@pytest.mark.parametrize("name", ["redshift", "synapse", "athena"])
def test_a_generated_warehouse_sweep_reads_every_row_once(name):
    rows = _rows()
    engine = _Engine(name, rows)
    read = _sweep(name, engine)
    expected = [(r[0], r[1]) for r in rows]
    assert len(read) == len(set(read)), f"{name}: duplicated rows {sorted(read)}"
    assert sorted(read) == sorted(expected), f"{name}: missing {sorted(set(expected) - set(read))}"


@pytest.mark.parametrize("name", ["redshift", "synapse", "athena"])
def test_a_generated_warehouse_page_orders_by_every_orderable_column(name):
    engine = _Engine(name, _rows())
    _sweep(name, engine)
    assert engine.pages
    for sql in engine.pages:
        assert f"ORDER BY {ORDER[name]}" in sql, sql


@pytest.mark.parametrize("name", ["redshift", "athena"])
def test_a_generated_warehouse_reads_its_watermark_before_the_first_page(name):
    """The sweep's watermark is max(updated_at) read in the engine's own bind
    style and clock functions, before its first page."""
    stamps = [f"2026-01-{d:02d} 00:00:00" for d in range(1, 15)]
    rows = [r + (s,) for r, s in zip(_rows(), stamps)]
    engine = _Engine(name, rows, extra_columns=[("updated_at", "timestamp")])
    srv = _connector(name, _Conn(engine))
    res = srv.export({"config": {"host": "h", "database": "d"}, "table": TABLE, "limit": 100,
                      "use_keyset_paging": True, "since": "2026-01-05 00:00:00"})
    assert res.get("success") is not False, res
    assert sorted(r["updated_at"] for r in res["data"]) == stamps[5:]
    assert res["stats"]["watermark"] == {"field": "updated_at", "value": stamps[-1], "snapshot": True}


# Athena compares a column only with a value of its type, and pyathena binds
# a str as varchar: the watermark is bound as the column's type.
@pytest.mark.parametrize("column_type, day", [
    ("timestamp", lambda d: f"2026-01-{d:02d} 00:00:00"),
    ("date", lambda d: f"2026-01-{d:02d}"),
])
@pytest.mark.parametrize("since", ["2026-01-05T00:00:00+00:00", "2026-01-05 00:00:00", "2026-01-05"])
def test_an_athena_incremental_sweep_binds_since_as_the_columns_type(column_type, day, since):
    stamps = [day(d) for d in range(1, 15)]
    rows = [r + (s,) for r, s in zip(_rows(), stamps)]
    engine = _Engine("athena", rows, extra_columns=[("updated_at", column_type)])
    read = _sweep("athena", engine, since=since)
    expected = [(r[0], r[1]) for r in rows[5:]]
    assert sorted(read) == sorted(expected) and len(read) == len(set(read)), read


def test_an_athena_varchar_watermark_stays_a_string():
    stamps = [f"2026-01-{d:02d}T00:00:00" for d in range(1, 15)]
    rows = [r + (s,) for r, s in zip(_rows(), stamps)]
    engine = _Engine("athena", rows, extra_columns=[("updated_at", "varchar")])
    read = _sweep("athena", engine, since="2026-01-05T00:00:00")
    assert sorted(read) == sorted((r[0], r[1]) for r in rows[5:])


# An Iceberg snapshot id is a 64-bit integer, past what a float64 holds.
ICEBERG_SNAPSHOT = 5281394372919829231


def test_an_athena_iceberg_sweep_reads_every_page_from_one_snapshot():
    engine = _Engine("athena", _rows(), snapshot_id=ICEBERG_SNAPSHOT)
    read = _sweep("athena", engine)
    assert sorted(read) == sorted((r[0], r[1]) for r in _rows()) and len(read) == len(set(read))
    assert engine.snapshot_reads == 1
    assert engine.pins == [ICEBERG_SNAPSHOT] * len(engine.pages), engine.pins


def test_an_athena_hive_sweep_reads_unpinned():
    engine = _Engine("athena", _rows())
    read = _sweep("athena", engine)
    assert sorted(read) == sorted((r[0], r[1]) for r in _rows()) and len(read) == len(set(read))
    assert engine.snapshot_reads == 1, "a Hive table has no $snapshots: asked once, then unpinned"
    assert engine.pins == []


# --------------------------------------------------------------------------
# The watermark's lag
# --------------------------------------------------------------------------

UPDATED_AT = {"redshift": "timestamp", "synapse": 42, "athena": "timestamp"}


@pytest.mark.parametrize("name", ["redshift", "synapse", "athena"])
def test_a_generated_warehouse_holds_back_a_watermark_inside_the_lag(name):
    """A MAX(updated_at) within the lag of the engine's clock is held back to
    clock - lag, so a row stamped before the read and committed after it is
    above the next sweep's since. The fake runs each clock as SQLite's UTC
    now."""
    now = datetime.now(timezone.utc).replace(tzinfo=None, microsecond=0)
    stamps = [(now - timedelta(days=13 - i)).strftime("%Y-%m-%d %H:%M:%S") for i in range(14)]
    rows = [r + (s,) for r, s in zip(_rows(), stamps)]
    engine = _Engine(name, rows, extra_columns=[("updated_at", UPDATED_AT[name])], driver_datetimes=True)
    res = _connector(name, _Conn(engine)).export({"config": {"host": "h", "database": "d"}, "table": TABLE,
                                                  "limit": 100, "use_keyset_paging": True})
    assert res.get("success") is not False, res
    wm = res["stats"]["watermark"]
    assert wm["snapshot"] is True, wm
    held = datetime.fromisoformat(wm["value"])
    assert now - timedelta(seconds=30) < held < now, f"{name}: a watermark inside the lag was not held back: {wm}"


class _ClockCursor:
    """The watermark read on an engine whose clock reads `now`: MAX is a
    second old, and the clocks are returned only where the SQL selects them."""

    def __init__(self, now):
        self.now = now
        self.description = None
        self.sql = []
        self._row = None

    def execute(self, sql, params=None):
        self.sql.append(sql)
        if "WHERE 1 = 0" in sql:
            self.description = [("updated_at", "timestamp")]
            self._row = None
            return
        row = [self.now - timedelta(seconds=1)]
        if " AS wm_utc" in sql and " AS wm_local" in sql:
            row += [self.now, self.now]
        self._row = tuple(row)

    def fetchall(self):
        return []

    def fetchone(self):
        return self._row


@pytest.mark.parametrize("name", ["snowflake", "databricks", "redshift", "athena"])
def test_a_warehouse_watermark_read_selects_its_clocks(name):
    mod = _generated(name)
    srv = _connector(name, None)
    dialect = sql_paging.dialect_for(mod._get_driver_pattern(f"acme_{name}")["module"], name)
    now = datetime(2026, 1, 14, 12, 0, 0)
    cur = _ClockCursor(now)
    got = srv._watermark_snapshot(_Conn(None), cur, dialect, dialect.ident("t"), "updated_at", None, 5.0)
    assert got == ("updated_at", (now - timedelta(seconds=5)).isoformat()), (name, cur.sql)


NO_CLOCK = {"generic", "sqlite", "clickhouse", "bigquery"}


def test_every_engine_that_can_read_its_clock_selects_it():
    dialects = {d.name: d for d in vars(sql_paging).values() if isinstance(d, sql_paging.Dialect)}
    assert {"snowflake", "databricks", "redshift", "athena", "synapse"} <= set(dialects)
    for name, d in dialects.items():
        if name in NO_CLOCK:
            assert not d.clocks, name
            continue
        assert " AS wm_utc" in d.clocks and " AS wm_local" in d.clocks, (name, d.clocks)


# --------------------------------------------------------------------------
# Dialects
# --------------------------------------------------------------------------

# Every SQL driver pattern the template knows, and the paging dialect its
# connectors must get. A new pattern fails here until it names one.
EXPECTED_DIALECT = {
    "mysql": "MYSQL",
    "postgresql": "POSTGRESQL",
    "redshift": "REDSHIFT",
    "snowflake": "SNOWFLAKE",
    "databricks": "DATABRICKS",
    "athena": "ATHENA",
    "sqlite": "SQLITE",
    "oracle": "ORACLE",
    "sqlserver": "SQLSERVER_QMARK",
    "synapse": "SYNAPSE",
}


def test_every_sql_driver_pattern_gets_its_own_dialect():
    mod = _generated("athena")
    patterns = {k: p for k, p in mod.DRIVER_PATTERNS.items() if not p.get("is_nosql")}
    assert set(patterns) == set(EXPECTED_DIALECT), "name the paging dialect of a new SQL driver pattern"
    for name, pattern in patterns.items():
        assert mod._get_driver_pattern(f"acme_{name}").get("name") == name
        got = sql_paging.dialect_for(pattern["module"], name)
        assert got is getattr(sql_paging, EXPECTED_DIALECT[name]), (name, got.name)


# --------------------------------------------------------------------------
# Discovery
# --------------------------------------------------------------------------

class _CatalogConn:
    """Answers a discovery's catalog reads by substring; any other read fails
    the way an unknown catalog would."""

    def __init__(self, answers):
        self.answers = answers

    def cursor(self, *a, as_dict=True, **k):
        conn = self

        class C:
            description = None

            def execute(self, sql, params=None):
                for needle, rows in conn.answers:
                    if needle in sql:
                        self._rows = list(rows)
                        return
                raise _EngineError(f"not in this catalog: {sql[:60]}")

            def fetchall(self):
                rows, self._rows = self._rows, []
                return rows

            def fetchone(self):
                return self._rows.pop(0) if self._rows else None

            def close(self):
                pass

        return C()

    def rollback(self):
        pass

    def close(self):
        pass


@pytest.mark.parametrize("name", ["postgresql", "redshift"])
def test_a_generated_psycopg2_connector_discovers_its_columns(name):
    conn = _CatalogConn([
        ("SELECT version()", [("PostgreSQL 16.4",)]),
        ("information_schema.tables", [(1,)]),
        ("FROM pg_class c", [("t", 14)]),
        ("information_schema.columns", [
            ("analytics", "t", "id", "integer", "NO", 32, 0, None),
            ("analytics", "t", "name", "character varying", "YES", None, None, 64),
            ("analytics", "t", "amount", "numeric", "YES", 12, 2, None),
        ]),
    ])
    srv = _connector(name, conn, ConnectorCategory.RELATIONAL_DB)
    res = srv.discover_schema({"config": {"host": "h", "database": "d", "schema": "analytics"},
                               "include_columns": True})
    column_warnings = [m for m in res["warnings_messages"] if "columns" in m.lower()]
    assert not column_warnings, column_warnings
    (table,) = res["tables"]
    cols = {c["name"]: c for c in table["columns"]}
    assert list(cols) == ["id", "name", "amount"]
    assert cols["amount"]["type"] == "numeric(12,2)"
    assert cols["name"]["source_type"] == "character varying(64)"


# --------------------------------------------------------------------------
# A generated warehouse writes in its own engine's SQL
# --------------------------------------------------------------------------
# Redshift shares psycopg2 with PostgreSQL and Synapse shares pyodbc with SQL
# Server; a write path picked by driver module sent them PostgreSQL's
# ON CONFLICT / SAVEPOINT and SQL Server's MERGE ... USING (VALUES ...), which
# neither engine has, and Athena `%s` binds pyathena refuses.

WAREHOUSES = ["redshift", "synapse", "athena"]


def _writer(name, engine):
    return _connector(name, _Conn(engine), destination=True)


def _table(engine):
    return dict(engine.db.execute('SELECT "id", "name" FROM t'))


@pytest.mark.parametrize("name", WAREHOUSES)
def test_a_generated_warehouse_upserts_in_its_own_dialect(name):
    engine = _Engine(name, [(1, "a", None), (2, "b", None)])
    res = _writer(name, engine).upsert_data({
        "config": {}, "table": TABLE, "key_fields": ["id"],
        "data": [{"id": 2, "name": "B"}, {"id": 3, "name": "c"}]})
    assert res == {"success": True, "rows_upserted": 2}, (res, engine.statements)
    assert _table(engine) == {1: "a", 2: "B", 3: "c"}


@pytest.mark.parametrize("name", WAREHOUSES)
def test_a_generated_warehouse_appends_and_deletes_in_its_own_dialect(name):
    engine = _Engine(name, [(1, "a", None), (2, "b", None)])
    srv = _writer(name, engine)
    res = srv.import_data({"config": {}, "table": TABLE, "data": [{"id": 3, "name": "c"}]})
    assert res == {"success": True, "rows_inserted": 1}, (res, engine.statements)
    res = srv.delete_data({"config": {}, "table": TABLE, "key_fields": ["id"], "data": [{"id": 1}]})
    assert res == {"success": True, "rows_deleted": 1}, (res, engine.statements)
    assert _table(engine) == {2: "b", 3: "c"}


def test_a_generated_redshift_keeps_its_cdc_offset_monotonic():
    engine = _Engine("redshift", [(1, "a", None)])
    srv = _writer("redshift", engine)
    for offset in (41, 40):
        res = srv.upsert_data({
            "config": {}, "table": TABLE, "key_fields": ["id"], "data": [{"id": offset, "name": "x"}],
            "kafka_offset": {"pipeline_id": "p1", "topic": "rsync.cdc.t", "partition": 0, "offset": offset}})
        assert res == {"success": True, "rows_upserted": 1}, (res, engine.statements)
    # Redshift does not enforce the offsets table's key: a stale offset must
    # not land beside the newer one.
    assert srv.get_cdc_offsets({"config": {}, "pipeline_id": "p1"}) == {
        "success": True, "offsets": [{"topic": "rsync.cdc.t", "partition": 0, "offset": 41}]}
    assert _table(engine) == {1: "a", 40: "x", 41: "x"}


@pytest.mark.parametrize("op, row", [("upsert_data", {"id": 2, "name": "b"}),
                                     ("import_data", {"id": 2, "name": "b"}),
                                     ("delete_data", {"id": 1})])
@pytest.mark.parametrize("name", ["synapse", "athena"])
def test_a_generated_warehouse_with_no_offset_sql_refuses_a_cdc_batch(name, op, row):
    """The batch and its offset commit together or not at all; Athena commits
    each statement, so the refusal comes before any write."""
    engine = _Engine(name, [(1, "a", None)])
    res = getattr(_writer(name, engine), op)({
        "config": {}, "table": TABLE, "key_fields": ["id"], "data": [row],
        "kafka_offset": {"pipeline_id": "p1", "topic": "rsync.cdc.t", "partition": 0, "offset": 7}})
    assert res == {"success": False, "error": f"CDC offsets not implemented for {name}"}, (res, engine.statements)
    assert _table(engine) == {1: "a"}


@pytest.mark.parametrize("name", WAREHOUSES)
def test_a_generated_warehouse_refuses_ddl_it_has_no_sql_for(name):
    engine = _Engine(name, [])
    res = _writer(name, engine).ensure_table({
        "config": {}, "table": TABLE, "columns": ["id", "name"], "key_fields": ["id"]})
    assert res == {"success": False, "error": f"ensure_table not implemented for {name}"}
    assert engine.statements == []


# --------------------------------------------------------------------------
# The generator refuses code that calls a name nothing defines
# --------------------------------------------------------------------------

def _undefined_errors(code, spec):
    result = PostGenerationValidator().validate(code, spec)
    return [e for e in result.errors if e.code == "UNDEFINED_NAME"]


RENDERS = {
    "database_postgresql": lambda: _database_spec("postgresql", ConnectorCategory.RELATIONAL_DB),
    "database_athena": lambda: _database_spec("athena"),
    "database_synapse": lambda: _database_spec("synapse"),
    "rest_source": lambda: _api_spec(graphql=False, destination=False),
    "rest_destination": lambda: _api_spec(graphql=False, destination=True),
    "graphql_source": lambda: _api_spec(graphql=True, destination=False),
    "graphql_destination": lambda: _api_spec(graphql=True, destination=True),
}


@pytest.mark.parametrize("render", sorted(RENDERS))
def test_every_template_renders_code_whose_names_are_defined(render):
    spec = RENDERS[render]()
    assert _undefined_errors(_render(spec), spec) == []


@pytest.mark.parametrize("snippet, undefined", [
    ("def f():\n    return _never_defined(1)\n", ["_never_defined"]),
    # A name a class body binds is not in scope inside its methods.
    ("class A:\n    X = 1\n    def f(self):\n        return X\n", ["X"]),
    ("x = [y for y in range(3)]\nprint(z)\n", ["z"]),
])
def test_the_generator_refuses_a_call_to_an_undefined_name(snippet, undefined):
    spec = _api_spec(graphql=False, destination=False)
    errors = _undefined_errors(snippet, spec)
    assert errors, "no UNDEFINED_NAME error"
    for name in undefined:
        assert any(name in e.message for e in errors), [e.message for e in errors]


@pytest.mark.parametrize("snippet", [
    # Import fallbacks, a global set inside a function, builtins, module
    # attributes, exception names, walrus and comprehension targets.
    "try:\n    from m import f\nexcept ImportError:\n    def f():\n        return None\n",
    "def setup():\n    global CFG\n    CFG = 1\n\ndef use():\n    return CFG\n",
    "import os\nHERE = os.path.dirname(__file__)\nprint(__name__, len([]), Exception)\n",
    "try:\n    pass\nexcept Exception as err:\n    print(err)\n",
    "if (n := 3) > 2:\n    print([i for i in range(n)])\n",
    "def outer():\n    v = 1\n    def inner():\n        nonlocal v\n        v += 1\n        return v\n    return inner\n",
    # A star import can bind anything: the check stands down.
    "from m import *\nprint(anything)\n",
])
def test_the_generator_accepts_names_python_can_resolve(snippet):
    spec = _api_spec(graphql=False, destination=False)
    assert _undefined_errors(snippet, spec) == []
