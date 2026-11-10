"""A records key that names a nested path (``artists.items``) is followed at runtime.

The generator writes ``response_data_key`` as a dotted path when the spec nests the
records array (Spotify ``{"artists": {"items": [...]}}``). Column discovery in the
generated connector walks the dots (connector.py.j2 ``_extract_first_row``), but
``PaginationHandler._extract_records`` looked the whole string up as ONE key, found
nothing, and the export read 0 rows while discovery showed the table's columns.

Bug class: a dotted records key read as one literal key. The fix tries the literal
key first (a key may contain a dot, like ``@odata.nextLink``), then walks the dots.
The cursor path gets the same two lookups the other way round.
"""
from __future__ import annotations

import importlib.util
import os

_BC_PATH = os.path.join(os.path.dirname(__file__), "..", "base_connector.py")
_spec = importlib.util.spec_from_file_location("base_connector_dotted", _BC_PATH)
_bc = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bc)

PaginationHandler = _bc.PaginationHandler

ROWS = [{"id": "a"}, {"id": "b"}]


def test_a_dotted_key_reads_the_nested_records():
    h = PaginationHandler(pagination_type="cursor", response_data_key="artists.items")
    body = {"artists": {"items": ROWS, "next": None, "total": 2}}
    assert h._extract_records(body) == ROWS


def test_a_dotted_key_reads_three_levels_down():
    h = PaginationHandler(response_data_key="result.page.rows")
    assert h._extract_records({"result": {"page": {"rows": ROWS}}}) == ROWS


def test_a_literal_key_with_a_dot_is_still_found_first():
    h = PaginationHandler(response_data_key="a.b")
    other = [{"id": "nested"}]
    assert h._extract_records({"a.b": ROWS, "a": {"b": other}}) == ROWS


def test_a_dotted_key_that_misses_falls_back_as_before():
    # The path is not in the body: the well-known keys still answer.
    h = PaginationHandler(response_data_key="artists.items")
    assert h._extract_records({"data": ROWS}) == ROWS


def test_a_dotted_key_through_a_non_dict_misses():
    h = PaginationHandler(response_data_key="artists.items")
    assert h._extract_records({"artists": ["x"], "meta": {}}) == ["x"]  # tier 3, not a crash


def test_a_plain_key_is_unchanged():
    h = PaginationHandler(response_data_key="channels")
    assert h._extract_records({"channels": ROWS, "ok": True}) == ROWS


def test_a_dotted_cursor_path_reads_the_nested_cursor():
    h = PaginationHandler(pagination_type="cursor", cursor_path="artists.cursors.after")
    assert h._extract_next_cursor({"artists": {"cursors": {"after": "c2"}}}) == "c2"


def test_a_literal_cursor_key_with_a_dot_is_found():
    # Not one of the well-known cursor locations, so only the configured path can find it.
    h = PaginationHandler(pagination_type="cursor", cursor_path="@acme.next")
    assert h._extract_next_cursor({"@acme.next": "c3"}) == "c3"


# The hand-built REST connectors run the base_connector.py bundled in their version
# folder, not the canonical one above, so each copy gets the same two lookups.
import json  # noqa: E402

import pytest  # noqa: E402

_ROOT = os.path.join(os.path.dirname(__file__), "..")
HAND_BUILT_REST = ["public/stripe", "public/github-rest", "public/notion-rest"]


def _bundled_paginator(connector: str):
    with open(os.path.join(_ROOT, connector, "latest.json")) as f:
        version = json.load(f)["current_version"]
    path = os.path.join(_ROOT, connector, "versions", version, "base_connector.py")
    name = "bundled_" + connector.replace("/", "_").replace("-", "_")
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.PaginationHandler


@pytest.mark.parametrize("connector", HAND_BUILT_REST)
def test_a_hand_built_rest_connector_reads_dotted_keys(connector):
    handler = _bundled_paginator(connector)
    assert handler(response_data_key="artists.items")._extract_records(
        {"artists": {"items": ROWS}}) == ROWS
    assert handler(response_data_key="a.b")._extract_records(
        {"a.b": ROWS, "a": {"b": [{"id": "nested"}]}}) == ROWS
    assert handler(pagination_type="cursor", cursor_path="@acme.next")._extract_next_cursor(
        {"@acme.next": "c3"}) == "c3"
