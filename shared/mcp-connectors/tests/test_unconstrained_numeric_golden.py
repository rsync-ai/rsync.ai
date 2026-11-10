"""Pins canonical_to_ddl to shared/unconstrained_numeric_golden.json.

A PostgreSQL NUMERIC declared with no precision and no scale has no fixed width, so
the NUMERIC(38,9) a bare decimal gets rounded it at the ninth decimal place
(KI-CONNECTOR-UNCONSTRAINED-NUMERIC-ROUNDS-AT-9DP). Discovery now reports such a
column as the bare type "numeric", and canonical_to_ddl gives it plain NUMERIC on a
PostgreSQL destination and the dialect's text type everywhere else.

The kafka-sink-worker checks its CDC mapping against the same file
(unconstrained_numeric_golden_test.go), so batch and CDC agree per destination.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "public")))
import canonical_types  # noqa: E402

_GOLDEN = Path(__file__).resolve().parents[3] / "shared" / "unconstrained_numeric_golden.json"
golden = json.loads(_GOLDEN.read_text())
TEMPLATE = (
    Path(__file__).resolve().parents[3]
    / "llm-service" / "src" / "agents" / "tool_generator" / "templates"
    / "connector_database.py.j2"
)

BATCH = sorted(golden["batch_ddl"].items())
FIXED = sorted(golden["fixed_decimal"].items())


def test_golden_is_not_empty():
    # An empty section would make every parametrized test below vanish, green.
    assert BATCH and FIXED and golden["cdc_ddl"]


def test_marker_is_the_shared_constant():
    assert canonical_types.UNCONSTRAINED_NUMERIC == golden["marker"]


def test_every_ddl_dialect_is_pinned():
    # A dialect added to canonical_to_ddl must say what it does with the marker.
    assert set(golden["batch_ddl"]) == set(canonical_types._CANONICAL_TO_DDL)
    assert set(golden["fixed_decimal"]) == set(canonical_types._CANONICAL_TO_DDL)


@pytest.mark.parametrize("dialect,want", BATCH)
def test_marker_ddl(dialect, want):
    marker = golden["marker"]
    ddl = canonical_types.canonical_to_ddl
    assert ddl(dialect, marker) == want["column"]
    assert ddl(dialect, marker, is_key=True, single_key=True) == want["lone_key"]
    assert ddl(dialect, marker, is_key=True, single_key=False) == want["composite_key"]


@pytest.mark.parametrize("dialect,want", BATCH)
def test_marker_matches_any_case(dialect, want):
    # A CDC-created PostgreSQL table reports its column as NUMERIC.
    assert canonical_types.canonical_to_ddl(dialect, " NUMERIC ") == want["column"]


@pytest.mark.parametrize("dialect,want", FIXED)
def test_bare_decimal_keeps_its_fixed_width(dialect, want):
    # MySQL and Oracle report constrained columns as a bare "decimal"; only the
    # PostgreSQL marker means unconstrained.
    assert canonical_types.canonical_to_ddl(dialect, "decimal") == want


@pytest.mark.parametrize("raw", ["numeric(10,2)", "NUMERIC(38, 9)"])
def test_declared_numeric_is_not_the_marker(raw):
    assert canonical_types.canonical_to_ddl("mysql", raw).startswith("DECIMAL(")
    assert canonical_types.canonical_to_ddl("postgresql", raw).startswith("NUMERIC(")


def test_canonical_vocabulary_is_unchanged():
    # The marker is a DDL instruction, not a new canonical type.
    assert canonical_types.canonicalize_type("numeric") == "decimal"
    assert "numeric" not in canonical_types.CANONICAL_TYPES


def _template_method(name: str) -> str:
    src = TEMPLATE.read_text()
    start = src.index("def %s(" % name)
    return src[start:src.index("\n    def ", start + 10)]


def test_template_marker_falls_back_to_the_golden():
    # Offline, the generated connector still has the marker's value.
    src = TEMPLATE.read_text()
    assert "import UNCONSTRAINED_NUMERIC" in src
    assert 'UNCONSTRAINED_NUMERIC = "%s"' % golden["marker"] in src


def test_template_postgres_discovery_reports_the_marker():
    # The generated-connector template mirrors the postgresql connector's discovery.
    body = _template_method("_discover_postgres_schema_v2")
    marker = body.index("_emit_type = UNCONSTRAINED_NUMERIC")
    assert '== "numeric"' in body[body.rindex("elif", 0, marker):marker]


def test_template_mysql_ensure_table_maps_a_bare_numeric_through_the_shared_ddl():
    # Passed through as DDL, a bare NUMERIC is MySQL's DECIMAL(10,0).
    body = _template_method("_ensure_table_for_mysql")
    ddl_for = body[body.index("def _ddl_for("):]
    guard = ddl_for.index('("decimal", "numeric")')
    assert guard < ddl_for.index('canonical_to_ddl("mysql", raw') < ddl_for.index("_normalize_ddl_type(raw)")
