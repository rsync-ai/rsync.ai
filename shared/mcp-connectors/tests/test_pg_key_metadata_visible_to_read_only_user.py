"""A read-only PostgreSQL user must see its tables' primary and foreign keys.

Bug class: PostgreSQL constraint metadata read through the privilege-filtered
information_schema views. ``table_constraints``, ``key_column_usage`` and
``referential_constraints`` list only tables the current user owns or holds a
non-SELECT privilege on; ``constraint_column_usage`` lists only owned tables. A
source connection with plain ``GRANT SELECT`` - the least-privilege setup every
guide recommends - therefore saw no keys at all. ``pg_catalog.pg_constraint``
is readable by every user and is what these reads must use.

Found live on 2026-09-29: a scheduled batch pipeline from a Cloud SQL source
read by a SELECT-only user fell back to OFFSET paging with no ORDER BY, kept no
primary-key high-water mark, and re-read the whole table into new files on
every tick. ``GRANT REFERENCES`` on the table (which un-hides it from the
views) switched the next tick to keyset paging.

Export paging reads its key through ``sql_paging.py`` (``pg_catalog.pg_index``)
and compares every column of a composite key (KI-DB-BATCH-PAGING-CAN-SKIP-ROWS).

Offline: ``ReadOnlyUserCursor`` answers the way PostgreSQL answers a SELECT-only
non-owner - the constraint views come back empty, ``pg_catalog`` does not. The
MySQL, Oracle and SQL Server connectors carry a copy of the PostgreSQL discovery
branch but pin their own driver, so it never runs there; the template and the
postgresql connector are the two copies that do.
"""
from __future__ import annotations

import importlib.util
import json
import os
import re
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, ".."))  # shared/mcp-connectors
_PUBLIC = os.path.join(_ROOT, "public")
_REPO = os.path.abspath(os.path.join(_ROOT, "..", ".."))
TEMPLATE = os.path.join(
    _REPO, "llm-service", "src", "agents", "tool_generator", "templates", "connector_database.py.j2"
)

# The views PostgreSQL filters by privilege. Reading keys through any of them is
# the bug class, whatever the query around it looks like.
PRIVILEGE_FILTERED_VIEWS = (
    "information_schema.table_constraints",
    "information_schema.key_column_usage",
    "information_schema.constraint_column_usage",
    "information_schema.referential_constraints",
)

# What a SELECT-only user can read: three tables, one with a composite key.
COLUMNS = {
    ("public", "customers"): [("id", "integer")],
    ("public", "orders"): [("id", "integer"), ("customer_id", "integer"), ("updated_at", "timestamp with time zone")],
    ("public", "order_lines"): [("order_id", "integer"), ("line_no", "integer")],
}
PRIMARY_KEYS = {
    ("public", "customers"): ["id"],
    ("public", "orders"): ["id"],
    ("public", "order_lines"): ["order_id", "line_no"],
}
# (schema, table, column, ref_table, ref_column, constraint, on_update, on_delete)
FOREIGN_KEYS = [
    ("public", "orders", "customer_id", "customers", "id", "orders_customer_id_fkey", "NO ACTION", "CASCADE"),
]


class ReadOnlyUserCursor:
    """Answers catalog reads the way PostgreSQL answers a SELECT-only non-owner."""

    def __init__(self, export_columns=(), export_rows=()):
        self.executed = []
        self._rows = []
        self._one = None
        self._columns = []
        self._export_columns = list(export_columns)
        self._export_rows = list(export_rows)

    def execute(self, sql, params=None):
        self.executed.append((sql, params))
        s = " ".join(sql.split())
        self._rows, self._one, self._columns = [], None, []
        if any(v in s for v in PRIVILEGE_FILTERED_VIEWS):
            return  # the user neither owns the table nor holds a non-SELECT grant
        if "information_schema.schemata" in s:
            self._rows = [("public",)]
        elif "SELECT COUNT(*) FROM information_schema.tables" in s:
            self._one = [len(COLUMNS)]
        elif "FROM pg_class c" in s and "pg_namespace" in s:
            self._rows = [(t, 10) for (_, t) in COLUMNS]
        elif "information_schema.columns" in s:
            self._rows = [
                (sch, tbl, col, typ, "NO", None, None)
                for (sch, tbl), cols in COLUMNS.items() for col, typ in cols
            ]
        elif "pg_catalog.pg_index" in s and "to_regclass(%s)" in s:
            # sql_paging's key read: (indexrelid, indisprimary, attname, attnotnull)
            rel = tuple(part.strip('"') for part in params[0].split("."))
            self._rows = [(1, True, col, True) for col in PRIMARY_KEYS.get(rel, [])]
        elif "FROM pg_catalog.pg_class c WHERE c.oid = to_regclass(%s)" in s:
            self._rows = [("r", False, True)]  # sql_paging's locator probe
        elif "pg_catalog.pg_constraint" in s and "contype = 'p'" in s:
            self._rows = [
                (sch, tbl, col)
                for (sch, tbl), pk in PRIMARY_KEYS.items() if tbl in (params or ()) for col in pk
            ]
        elif "pg_catalog.pg_constraint" in s and "contype = 'f'" in s:
            self._rows = [fk for fk in FOREIGN_KEYS if fk[1] in (params or ())]
        elif s.startswith("SELECT * FROM"):
            self._rows = list(self._export_rows)
            self._columns = list(self._export_columns)

    @property
    def description(self):
        return [(c,) for c in self._columns]

    def fetchone(self):
        return self._one

    def fetchall(self):
        return list(self._rows)

    def close(self):
        pass


def _load_postgresql():
    base = os.path.join(_PUBLIC, "postgresql")
    with open(os.path.join(base, "latest.json")) as f:
        current = json.load(f)["current_version"]
    version_dir = os.path.join(base, "versions", current)
    for p in (base, version_dir, _PUBLIC, _ROOT):
        if p not in sys.path:
            sys.path.insert(0, p)
    spec = importlib.util.spec_from_file_location("_pg_key_visibility_connector", os.path.join(version_dir, "connector.py"))
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    except Exception as exc:  # pragma: no cover - environment-dependent
        pytest.skip(f"postgresql connector not importable offline: {exc}")
    return mod.PostgresqlMCPServer, os.path.join(version_dir, "connector.py")


class _Conn:
    def close(self):
        pass


def _server(cursor):
    cls, _ = _load_postgresql()
    srv = cls()
    assert (srv.driver_pattern.get("module") or "").lower() == "psycopg2"
    srv._get_connection = lambda config: _Conn()
    srv._get_cursor = lambda conn, *a, **k: cursor
    return srv


def _discover(cursor):
    srv = _server(cursor)
    result = {}
    srv._discover_postgres_schema_v2(
        conn=_Conn(), config={}, max_tables=100,
        include_columns=True, include_row_counts=True,
        include_relationships=True, include_indexes=False,
        result=result, add_warning=lambda *a, **k: None,
    )
    return {(t["schema"], t["name"]): t for t in result["tables"]}


def _paging_plan(table):
    _load_postgresql()
    import sql_paging  # shipped next to the connector; on sys.path now

    safe = ".".join(f'"{p}"' for p in table.split("."))
    return sql_paging.paging_plan(_Conn(), ReadOnlyUserCursor(), sql_paging.POSTGRESQL, table, safe)


def test_keyset_probe_sees_the_primary_key():
    assert _paging_plan("public.orders") == {"kind": "key", "columns": ["id"], "hidden": []}


def test_a_composite_key_pages_by_every_key_column():
    plan = _paging_plan("public.order_lines")
    assert plan == {"kind": "key", "columns": ["order_id", "line_no"], "hidden": []}


def test_scheduled_export_pages_by_key_not_offset():
    """The live symptom: with no key the export fell back to LIMIT/OFFSET with
    no ORDER BY and reported no cursor, so nothing marked where a run ended."""
    cursor = ReadOnlyUserCursor(
        export_columns=["id", "customer_id", "updated_at"],
        export_rows=[(1, 7, "2026-09-29T08:00:00+00:00"), (2, 7, "2026-09-29T08:01:00+00:00")],
    )
    srv = _server(cursor)
    res = srv.export({
        "config": {"host": "db", "port": 5432, "database": "app", "username": "u", "password": "p"},
        "table": "public.orders", "format": "json", "limit": 1000, "use_keyset_paging": True,
    })
    assert res.get("success") is not False, res
    selects = [" ".join(q.split()) for q, _ in cursor.executed if q.lstrip().startswith("SELECT * FROM")]
    assert selects and 'ORDER BY "id"' in selects[-1] and "OFFSET" not in selects[-1], selects
    assert res.get("paging_mode") == "keyset" and res.get("cursor_column") == "id", res
    assert res.get("next_cursor") == 2


def test_discovery_reports_primary_keys():
    by = _discover(ReadOnlyUserCursor())
    assert by[("public", "orders")]["primary_keys"] == ["id"]
    assert by[("public", "order_lines")]["primary_keys"] == ["order_id", "line_no"]
    assert {c["name"] for c in by[("public", "orders")]["columns"] if c.get("is_primary_key")} == {"id"}


def test_discovery_reports_foreign_keys():
    by = _discover(ReadOnlyUserCursor())
    fks = by[("public", "orders")]["foreign_keys"]
    assert fks == [{
        "column": "customer_id", "references_table": "customers", "references_column": "id",
        "constraint_name": "orders_customer_id_fkey", "on_update": "NO ACTION", "on_delete": "CASCADE",
    }], fks


def _pg_key_reads(text):
    """Discovery's primary/foreign key blocks in one file. Export paging reads
    its key through sql_paging (test_paging_key_reads_use_pg_catalog_only)."""
    disc_start = text.index("    def _discover_postgres_schema_v2(")
    disc = text[disc_start:text.index("\n    def ", disc_start + 10)]
    keys = disc[disc.index("# === Primary Keys ==="):disc.index("# === Indexes ===")]
    return {"discovery keys": keys}


def test_paging_key_reads_use_pg_catalog_only():
    _load_postgresql()
    import sql_paging

    for name in ("PG_KEY_SQL", "PG_LEGACY_KEY_SQL", "PG_LOCATOR_SQL", "PG_COLUMNS_SQL"):
        sql = getattr(sql_paging, name)
        hits = [v for v in PRIVILEGE_FILTERED_VIEWS if v in sql]
        assert not hits, f"sql_paging.{name} reads {hits}: a SELECT-only user sees nothing there"
        assert "pg_catalog." in sql, f"sql_paging.{name} no longer reads pg_catalog"


@pytest.mark.parametrize("which", ["connector", "template"])
def test_no_postgres_key_read_uses_a_privilege_filtered_view(which):
    _, connector_path = _load_postgresql()
    path = connector_path if which == "connector" else TEMPLATE
    with open(path, encoding="utf-8") as f:
        reads = _pg_key_reads(f.read())
    for name, body in reads.items():
        hits = [v for v in PRIVILEGE_FILTERED_VIEWS if v in body]
        assert not hits, f"{which} {name} reads {hits}: a SELECT-only user sees nothing there"
        assert "pg_catalog.pg_constraint" in body, f"{which} {name} no longer reads pg_catalog"


def test_template_carries_the_same_key_reads():
    """A connector generated from the template must get the fix too."""
    _, connector_path = _load_postgresql()
    with open(connector_path, encoding="utf-8") as f:
        connector = _pg_key_reads(f.read())
    with open(TEMPLATE, encoding="utf-8") as f:
        template = _pg_key_reads(f.read())
    for name in connector:
        assert re.sub(r"\s+", " ", connector[name]) == re.sub(r"\s+", " ", template[name]), (
            f"{name}: the postgresql connector and connector_database.py.j2 differ"
        )
