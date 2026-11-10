"""A connector that serves the ``execute`` tool must declare it in its metadata.json.

``execute`` is the tool Data Explorer writes and model runs delegate to on engines the
api-gateway has no driver for (``explorer_capability.go``: BigQuery, ClickHouse,
Snowflake; reads go to ``export``) -- the statement goes through the orchestrator to the
connector's ``execute``. ``metadata.json`` ``operations`` is the catalog's account of
what a connector can do (``validate_connector.py`` reads it; the catalog and its readers
never load the connector). ClickHouse served ``execute`` -- the method, the ``/execute``
route and its own ``get_capabilities`` entry -- while its metadata.json listed every
other operation but that one, so the catalog described a connector that could not run
a model or an Explorer write.

Bug class: **a served tool its metadata does not declare**. Pinned here to ``execute``,
the tool something outside the connector depends on finding; the census below fails if
the set of connectors serving it shrinks to nothing or drops a known one.

Static and offline: the source and metadata are read, nothing is imported.
"""
from __future__ import annotations

import json
import pathlib
import re

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[1]
PUBLIC = ROOT / "public"

# Every connector known to serve execute today. Pinned so a connector cannot fall out of
# the "serves execute" set (a renamed route, a moved directory) while this stays green.
KNOWN_SERVERS = {"bigquery", "clickhouse", "oracle", "snowflake"}

SERVES_EXECUTE = re.compile(r'@app\.post\("/execute"\)|^    def execute\(', re.M)
CONNECTOR_TYPE = re.compile(r'self\.connector_type = "([A-Za-z0-9_-]+)"')


def _current_dirs():
    for latest in sorted(PUBLIC.rglob("latest.json")):
        version = json.loads(latest.read_text()).get("current_version")
        vdir = latest.parent / "versions" / str(version)
        if (vdir / "connector.py").is_file() and (vdir / "metadata.json").is_file():
            yield latest.parent.name, vdir


CONNECTORS = dict(_current_dirs())
SERVERS = {
    slug for slug, vdir in CONNECTORS.items()
    if SERVES_EXECUTE.search((vdir / "connector.py").read_text())
}


def _declared(vdir):
    ops = json.loads((vdir / "metadata.json").read_text()).get("operations") or []
    if isinstance(ops, dict):
        return {name: (spec if isinstance(spec, dict) else {}) for name, spec in ops.items()}
    return {
        (op.get("name") if isinstance(op, dict) else op): (op if isinstance(op, dict) else {})
        for op in ops
    }


def test_the_census_reaches_every_connector_known_to_serve_execute():
    assert KNOWN_SERVERS <= SERVERS, sorted(KNOWN_SERVERS - SERVERS)


@pytest.mark.parametrize("slug", sorted(SERVERS))
def test_a_connector_that_serves_execute_declares_it(slug):
    declared = _declared(CONNECTORS[slug])
    assert "execute" in declared, (
        f"{slug} serves execute but its metadata.json operations do not list it: {sorted(declared)}"
    )


@pytest.mark.parametrize("slug", sorted(SERVERS))
def test_the_declared_execute_names_the_method_that_serves_it(slug):
    entry = _declared(CONNECTORS[slug]).get("execute")
    if entry is None:
        pytest.skip("not declared; the test above fails")
    assert entry.get("type"), f"{slug}: the execute entry has no type"
    method = entry.get("method") or "execute"
    kinds = CONNECTOR_TYPE.findall((CONNECTORS[slug] / "connector.py").read_text())
    if kinds and method.startswith(f"{kinds[0]}_"):
        # The dispatcher strips "<connector_type>_" before looking the handler up
        # (base_connector._resolves_to_handler).
        method = method[len(kinds[0]) + 1:]
    assert method == "execute", f"{slug}: the execute entry names {entry.get('method')!r}"
