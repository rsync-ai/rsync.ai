"""A connector must describe itself as the database it is, never as the one it was copied from.

The oracle and sqlserver connectors were hand-curated from the mysql connector, and
four of mysql's self-descriptions survived the copy. Each one misled a reader of a
different surface:

* the FastAPI ``title`` and ``description``: ``/docs`` and ``/openapi.json`` on the
  Oracle container said "MySQL MCP Connector";
* the startup log line: an operator reading ``docker logs`` on the Oracle or SQL
  Server container saw "Starting MySQL MCP Server";
* the ``/invoke`` fallback: a tool name not found as-is was retried as
  ``mysql_<name>``, a prefix no Oracle or SQL Server tool carries, so the fallback
  could only ever 404 (or, worse, reach a MySQL-shaped leftover);
* the module and class docstrings and the "MCP Server initialized" log line
  (sqlserver only).

Bug class: **a connector that names another database** -- a hand-copied connector
whose self-description still names the connector it was copied from. The template
(``connector_database.py.j2``) renders these from the spec, so a generated connector
cannot have the defect; only a hand copy can, which is why this suite reads every
connector's current version rather than the template.

The rule: none of these self-descriptions may name ANOTHER connector in the tree (its
metadata ``name`` or ``display_name``), and the ``/invoke`` fallback prefix is the
connector's own ``connector_type``. Static and offline: the source is parsed, nothing
is imported, so no driver is needed.

Not covered on purpose: the ``mysql_*`` helper names inside oracle and sqlserver.
Those connectors are the shared multi-dialect body pinned to one driver; the MySQL
helpers are the MySQL branch of that body, and every write path dispatches to the
dialect's own ``_ora_*``/``_ss_*`` method first. Likewise sqlserver's class name
``MysqlMCPServer``, a documented artifact (``INCREMENTAL.md``) that only code reads.
"""
from __future__ import annotations

import ast
import json
import pathlib
import re

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[1]
PUBLIC = ROOT / "public"

# The connectors this suite was written for. Pinned so they cannot drop out of the
# discovered set (a renamed latest.json, a moved directory) while the suite stays green.
MUST_CHECK = {"oracle", "sqlserver", "mysql"}
MIN_CONNECTORS = 15

INVOKE_PREFIX = re.compile(r'prefixed = f"([A-Za-z0-9_]+)_\{method_name\}"')
CONNECTOR_TYPE = re.compile(r'self\.connector_type = "([A-Za-z0-9_-]+)"')
INIT_LOG = re.compile(r'"([^"\n]*MCP Server initialized)"')
START_LOG = re.compile(r'f?"(🚀 Starting [^"\n]*MCP Server[^"\n]*)"')


def _current_dirs():
    for latest in sorted(PUBLIC.rglob("latest.json")):
        version = json.loads(latest.read_text()).get("current_version")
        vdir = latest.parent / "versions" / str(version)
        if (vdir / "connector.py").is_file() and (vdir / "metadata.json").is_file():
            yield latest.parent.name, vdir


CONNECTORS = dict(_current_dirs())


def _own_names(vdir):
    meta = json.loads((vdir / "metadata.json").read_text())
    return {str(meta.get(k) or "").strip() for k in ("name", "display_name")} - {""}


def _names_another(text, slug):
    """The other connectors' names that ``text`` mentions, as whole words."""
    own = {n.lower() for n in _own_names(CONNECTORS[slug])}
    hits = set()
    for other, vdir in CONNECTORS.items():
        if other == slug:
            continue
        for name in _own_names(vdir):
            low = name.lower()
            # A name that is part of this connector's own name (or contains it) is not
            # "another database": Redshift may say "Amazon", PostgreSQL may not say "MySQL".
            if any(low in o or o in low for o in own):
                continue
            if re.search(r"(?<![A-Za-z0-9])" + re.escape(name) + r"(?![A-Za-z0-9])", text, re.I):
                hits.add(name)
    return hits


def _summary(node):
    doc = ast.get_docstring(node) or ""
    return next((line.strip() for line in doc.splitlines() if line.strip()), "")


def _self_descriptions(source):
    """Every string in which a connector says what it is, labelled by where it is."""
    tree = ast.parse(source)
    # A docstring's first line, its summary, is where it says what it is; the body may
    # rightly compare itself to other connectors ("mirrors the PostgreSQL / MySQL ones").
    found = [("module docstring", _summary(tree))]
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and CONNECTOR_TYPE.search(ast.get_source_segment(source, node) or ""):
            found.append((f"class {node.name} docstring", _summary(node)))
        if isinstance(node, ast.Call) and getattr(node.func, "id", None) == "FastAPI":
            for kw in node.keywords:
                if kw.arg in ("title", "description") and isinstance(kw.value, ast.Constant):
                    found.append((f"FastAPI {kw.arg}", str(kw.value.value)))
    found += [("init log", m) for m in INIT_LOG.findall(source)]
    found += [("startup log", m) for m in START_LOG.findall(source)]
    return found


def test_the_census_reaches_the_connectors_it_was_written_for():
    assert MUST_CHECK <= set(CONNECTORS), sorted(MUST_CHECK - set(CONNECTORS))
    assert len(CONNECTORS) >= MIN_CONNECTORS, sorted(CONNECTORS)
    # Each check must have something to read, or it passes by reading nothing.
    sources = {s: (v / "connector.py").read_text() for s, v in CONNECTORS.items()}
    assert sum(bool(INVOKE_PREFIX.search(t)) for t in sources.values()) >= 8
    assert sum(bool(START_LOG.search(t)) for t in sources.values()) >= 12
    assert all(_self_descriptions(sources[s]) for s in MUST_CHECK)


@pytest.mark.parametrize("slug", sorted(CONNECTORS))
def test_a_connector_describes_itself_as_its_own_database(slug):
    source = (CONNECTORS[slug] / "connector.py").read_text()
    wrong = {
        where: sorted(hits)
        for where, text in _self_descriptions(source)
        if (hits := _names_another(text, slug))
    }
    assert not wrong, f"{slug} describes itself as another connector: {wrong}"


@pytest.mark.parametrize("slug", sorted(CONNECTORS))
def test_the_invoke_fallback_uses_the_connectors_own_prefix(slug):
    source = (CONNECTORS[slug] / "connector.py").read_text()
    prefixes = INVOKE_PREFIX.findall(source)
    if not prefixes:
        pytest.skip(f"{slug} has no /invoke prefix fallback")
    kinds = CONNECTOR_TYPE.findall(source)
    assert kinds, f"{slug} has an /invoke prefix fallback but no connector_type"
    assert set(prefixes) == {kinds[0].replace("-", "_")}, (
        f"{slug} retries an unknown tool as {prefixes[0]}_<name>, "
        f"not {kinds[0].replace('-', '_')}_<name>"
    )


@pytest.mark.parametrize(
    "text, slug, expected",
    [
        ("MySQL MCP Connector", "oracle", {"MySQL"}),
        ("Starting SQL Server MCP Server", "oracle", {"SQL Server"}),
        ("Oracle MCP Connector", "oracle", set()),
        ("MySQL MCP Connector", "mysql", set()),
        # "SQL Server" is not a word inside "MySQL", and "MySQL" is not inside "SQL Server".
        ("SQL Server MCP Connector", "sqlserver", set()),
    ],
)
def test_the_name_check_tells_its_own_name_from_anothers(text, slug, expected):
    assert _names_another(text, slug) == expected
