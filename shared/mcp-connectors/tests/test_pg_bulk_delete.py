"""PostgreSQL ``delete_data`` deletes a batch of keys with one set-based statement.

The kafka-mcp-sink batches CDC deletes per flush, so one ``delete_data`` call can
carry thousands of keys. The per-row loop costs one network round trip per key
and compares ``"k"::text = %s::text``, which cannot use the target's index. The
bulk path loads the keys into a temp table typed like the target and runs one
``DELETE ... USING``. On any failure it rolls back and falls back to the per-row
loop, so no batch that used to delete now fails.

These scenarios drive the REAL ``delete_data`` against a fake cursor, so nothing
here needs a live database. The template carries the same path.
"""
from __future__ import annotations

import importlib.util
import json
import sys
import types
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]  # shared/mcp-connectors
TEMPLATE = (
    ROOT.parents[1]
    / "llm-service"
    / "src"
    / "agents"
    / "tool_generator"
    / "templates"
    / "connector_database.py.j2"
)


def _current_dir(rel: str) -> Path:
    """Resolve versions/<latest.json.current_version>, never the highest directory."""
    base = ROOT / rel
    latest = json.loads((base / "latest.json").read_text())
    return base / "versions" / latest["current_version"]


def _load(rel: str, alias: str):
    """Import one connector the way its image does: its own version dir first, so
    it gets its own ``base_connector.py``. CI runs this file from ``llm-service``,
    where the cwd puts no connector on ``sys.path``; sys.path and sys.modules are
    put back so a later test never imports this copy."""
    path = _current_dir(rel) / "connector.py"
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [str(path.parent), str(ROOT / rel), str(ROOT / "public")]
    spec = importlib.util.spec_from_file_location(alias, path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    return mod


class FakeCursor:
    def __init__(self, log, fail_on=None, rowcount=1):
        self.log = log
        self.fail_on = fail_on
        self.rowcount = rowcount

    def execute(self, query, values=None):
        q = " ".join(str(query).split())
        if self.fail_on and self.fail_on in q:
            raise RuntimeError(f"boom on {self.fail_on}")
        self.log.append(("execute", q, values))

    def close(self):
        pass


class FakeConn:
    def __init__(self, events):
        self.events = events

    def commit(self):
        self.events.append("commit")

    def rollback(self):
        self.events.append("rollback")

    def close(self):
        pass


@pytest.fixture
def fake_execute_values(monkeypatch):
    """Stand in for psycopg2.extras.execute_values, which needs a real cursor."""
    calls: list = []

    def execute_values(cursor, sql, argslist, template=None, page_size=100):
        calls.append((" ".join(sql.split()), [list(a) for a in argslist], template))
        cursor.log.append(("execute_values", " ".join(sql.split()), [list(a) for a in argslist]))

    pkg = types.ModuleType("psycopg2")
    extras = types.ModuleType("psycopg2.extras")
    extras.execute_values = execute_values
    pkg.extras = extras
    monkeypatch.setitem(sys.modules, "psycopg2", pkg)
    monkeypatch.setitem(sys.modules, "psycopg2.extras", extras)
    return calls


def _pg(rows, *, key_fields=("id",), staged=True, fail_on=None, rowcount=3):
    mod = _load("public/postgresql", "_bulk_delete_pg_connector")
    srv = mod.PostgresqlMCPServer.__new__(mod.PostgresqlMCPServer)
    srv.driver_pattern = {"module": "psycopg2", "is_nosql": False}
    log: list = []
    events: list = []
    offsets: list = []

    srv.prepare_import_data = lambda params: {
        "success": True,
        "data": params["data"],
        "config": {},
        "table": "public.orders",
    }
    srv._normalize_table_for_postgresql = lambda config, table, params=None: table
    srv._safe_qualified_table = lambda table, quote='"': table
    srv._get_connection = lambda config: FakeConn(events)
    srv._get_cursor = lambda conn, as_dict=True: FakeCursor(log, fail_on=fail_on, rowcount=rowcount)
    srv.supports_staged_load = lambda: staged
    srv._qualified_quoted_table = lambda table: '"public"."orders"'
    srv._write_cdc_offsets = lambda cursor, params: offsets.append(params.get("kafka_offset"))

    result = srv.delete_data({
        "data": rows,
        "key_fields": list(key_fields),
        "kafka_offset": {"topic": "t", "partition": 0, "offset": 41},
    })
    return result, log, events, offsets


def _deletes(log):
    return [e for e in log if e[1].startswith("DELETE")]


def test_many_keys_take_one_set_based_delete(fake_execute_values):
    result, log, events, offsets = _pg([{"id": 1}, {"id": 2}, {"id": 3}])
    assert result == {"success": True, "rows_deleted": 3}, result

    statements = [e[1] for e in log]
    assert statements[0].startswith('CREATE TEMP TABLE "_rsync_del_'), statements[0]
    assert statements[0].endswith('ON COMMIT DROP AS SELECT "id" FROM "public"."orders" WITH NO DATA'), statements[0]

    # Every key goes in one load, as text the column's input function parses.
    assert len(fake_execute_values) == 1
    _, loaded, template = fake_execute_values[0]
    assert loaded == [["1"], ["2"], ["3"]], loaded
    assert template == "(%s)"

    deletes = _deletes(log)
    assert len(deletes) == 1, deletes
    assert deletes[0][1].startswith('DELETE FROM "public"."orders" t USING "_rsync_del_')
    assert deletes[0][1].endswith('s WHERE t."id" = s."id"'), deletes[0][1]

    # Offsets ride the same transaction, written once, committed once.
    assert offsets == [{"topic": "t", "partition": 0, "offset": 41}]
    assert events == ["commit"], events


def test_composite_key_joins_on_every_key_column(fake_execute_values):
    _, log, _, _ = _pg([{"a": 1, "b": "x"}, {"a": 2, "b": None}], key_fields=("a", "b"))
    assert 'SELECT "a", "b" FROM "public"."orders" WITH NO DATA' in log[0][1]
    assert fake_execute_values[0][1] == [["1", "x"], ["2", None]], "a NULL key stays NULL, never 'None'"
    assert fake_execute_values[0][2] == "(%s, %s)"
    assert _deletes(log)[0][1].endswith('WHERE t."a" = s."a" AND t."b" = s."b"')


def test_a_bulk_failure_rolls_back_and_deletes_row_by_row(fake_execute_values):
    """A key that does not parse as the column's type fails the typed load."""
    result, log, events, offsets = _pg(
        [{"id": "7"}, {"id": "not-a-number"}], fail_on="USING", rowcount=1,
    )
    assert result == {"success": True, "rows_deleted": 2}, result
    assert events == ["rollback", "commit"], events

    per_row = _deletes(log)
    assert [e[1] for e in per_row] == ['DELETE FROM "public"."orders" WHERE "id"::text = %s::text'] * 2
    assert [e[2] for e in per_row] == [("7",), ("not-a-number",)]
    # The failed attempt never reached its offset write; the fallback writes them once.
    assert len(offsets) == 1


def test_a_single_key_keeps_the_per_row_delete(fake_execute_values):
    _, log, _, _ = _pg([{"id": 9}], rowcount=1)
    assert fake_execute_values == []
    assert [e[1] for e in log] == ['DELETE FROM "public"."orders" WHERE "id"::text = %s::text']


def test_without_staged_load_support_every_key_is_its_own_delete(fake_execute_values):
    _, log, _, _ = _pg([{"id": 1}, {"id": 2}], staged=False, rowcount=1)
    assert fake_execute_values == []
    assert len(_deletes(log)) == 2


def test_a_keyless_row_fails_before_any_statement(fake_execute_values):
    """The missing-key check runs over the whole batch before the bulk path."""
    result, log, events, offsets = _pg([{"id": 1}, {"name": "no key"}])
    assert result["success"] is False
    assert "missing key field(s) ['id']" in result["error"], result
    assert log == [] and fake_execute_values == []
    assert offsets == [] and "commit" not in events


def test_the_driver_reporting_no_rowcount_counts_the_keys(fake_execute_values):
    result, _, _, _ = _pg([{"id": 1}, {"id": 2}], rowcount=-1)
    assert result == {"success": True, "rows_deleted": 2}, result


# --------------------------------------------------------------------------- #
# Template — generated database connectors get the same bulk path              #
# --------------------------------------------------------------------------- #
def test_template_carries_the_bulk_delete_path():
    src = TEMPLATE.read_text()
    assert "def _bulk_delete_by_keys(" in src
    assert "WITH NO DATA" in src
    assert "USING {staging} s WHERE" in src
    # Only PostgreSQL takes it: Redshift shares psycopg2 but not temp-table-as.
    start = src.index("def delete_data(")
    body = src[start:src.index("def _bulk_delete_by_keys(")]
    assert 'dialect.name == "postgresql"' in body and "_bulk_delete_by_keys(" in body
