"""A generated GraphQL source filters on the API's own "updated after" input.

The GraphQL half of #1450's class (task-inbox 2026-10-06). ``connector_graphql.py.j2``
replayed the watermark only through a variable whose name was on a fixed list
(``since``, ``updatedSince``, ``updatedAfter`` …), found by a substring test:

- a query declaring ``$updatedAt_gt``, ``$updated_at_gte`` or a Hasura / Prisma
  ``$where`` read every row on every run;
- ``$sinceDate`` matched as ``$since``, so the run logged a filter while the server
  got a variable the query never declared;
- ``$updatedAfter: Int`` was sent an ISO string the server must reject.

Bug class: an incremental filter recognised by a fixed name list — the class #1450
fixed for REST (``openapi_discovery._INCREMENTAL_PARAM_RE``).

Now the query's variable definitions are parsed. A variable whose name reads as an
updated time compared from below takes the watermark, unless it is typed as a number
or a boolean. A ``$where`` input gets a lower bound on the updated-at field the query
reads, in the shape the schema's input types spell (Hasura ``{updated_at: {_gte}}``,
Prisma ``{updatedAt: {gte}}``, Prisma 1 ``{updatedAt_gte}``).

The helpers live in the template and in each connector generated from it
(widgets-graphql); the lockstep tests keep them one copy.
"""
from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_PUBLIC = os.path.abspath(os.path.join(_HERE, "..", "public"))
_REPO = pathlib.Path(_HERE).resolve().parents[2]
TEMPLATE = _REPO / "llm-service" / "src" / "agents" / "tool_generator" / "templates" / "connector_graphql.py.j2"

# Every connector generated from connector_graphql.py.j2 that still carries its
# incremental block (shopify-admin-graphql is hand-built and filters its own way).
GENERATED = ["widgets-graphql"]

SINCE = "2026-10-01T08:00:00Z"


def _connector_file(name: str) -> pathlib.Path:
    root = pathlib.Path(_PUBLIC) / name
    current = json.loads((root / "latest.json").read_text())["current_version"]
    return root / "versions" / current / "connector.py"


def _load(name: str):
    """Import one connector in isolation. Each version dir carries its own
    `base_connector.py`; leaving it in sys.modules makes a later test import
    the wrong copy, so sys.path and sys.modules are put back afterwards."""
    path = _connector_file(name)
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [str(path.parent), _PUBLIC]
    spec = importlib.util.spec_from_file_location(f"_gql_since_{name.replace('-', '_')}", path)
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    return mod


gql = _load("widgets-graphql")


def _query(declarations: str, selection: str = "id updatedAt") -> str:
    return f"query Items({declarations}, $first: Int) {{\n  items(first: $first) {{ {selection} }}\n}}"


# --------------------------------------------------------------------------
# A variable the API names its own way
# --------------------------------------------------------------------------

@pytest.mark.parametrize("declaration, name", [
    ("$updatedAt_gt: DateTime", "updatedAt_gt"),
    ("$updated_at_gte: timestamptz!", "updated_at_gte"),
    ("$updatedAtGte: DateTime", "updatedAtGte"),
    ("$lastModifiedAfter: String", "lastModifiedAfter"),
    ("$changedSince: ISO8601DateTime", "changedSince"),
])
def test_a_variable_the_api_names_its_own_way_takes_the_since(declaration, name):
    variables: dict = {"first": 100}
    assert gql._apply_since_filter(_query(declaration), variables, SINCE) == name
    assert variables == {"first": 100, name: SINCE}


@pytest.mark.parametrize("name", ["since", "updatedSince", "updated_after", "modifiedAfter"])
def test_control_a_name_on_the_list_still_takes_the_since(name):
    variables: dict = {}
    assert gql._apply_since_filter(_query(f"${name}: DateTime"), variables, SINCE) == name
    assert variables == {name: SINCE}


def test_the_named_list_wins_over_a_shaped_name():
    variables: dict = {}
    query = _query("$updatedAt_gt: DateTime, $since: DateTime")
    assert gql._apply_since_filter(query, variables, SINCE) == "since"
    assert variables == {"since": SINCE}


@pytest.mark.parametrize("declaration", [
    "$createdAt_gt: DateTime",   # a created time, not an updated one
    "$updatedAt_lt: DateTime",   # an upper bound
    "$due_after: DateTime",      # not an update time at all
    "$sinceDate: DateTime",      # was matched as `$since` by a substring test
    "$updatedAfter: Int",        # an epoch — an ISO string would be refused
    "$since: Float",
    "$updatedSince: Boolean",
])
def test_a_variable_that_is_not_an_updated_after_is_never_filled(declaration):
    variables: dict = {}
    assert gql._apply_since_filter(_query(declaration), variables, SINCE) is None
    assert variables == {}


def test_a_value_the_caller_sent_is_kept():
    variables = {"updatedAt_gt": "2020-01-01T00:00:00Z"}
    assert gql._apply_since_filter(_query("$updatedAt_gt: DateTime"), variables, SINCE) == "updatedAt_gt"
    assert variables == {"updatedAt_gt": "2020-01-01T00:00:00Z"}


# --------------------------------------------------------------------------
# A `$where` input, in the shape the schema spells
# --------------------------------------------------------------------------

_HASURA = {
    "users_bool_exp": {
        "_and": "users_bool_exp", "id": "Int_comparison_exp",
        "updated_at": "timestamptz_comparison_exp",
    },
    "timestamptz_comparison_exp": {
        "_eq": "timestamptz", "_gt": "timestamptz", "_gte": "timestamptz",
        "_lt": "timestamptz", "_in": "timestamptz",
    },
}
_PRISMA = {
    "UserWhereInput": {"AND": "UserWhereInput", "id": "StringFilter", "updatedAt": "DateTimeFilter"},
    "DateTimeFilter": {"equals": "DateTime", "gt": "DateTime", "gte": "DateTime", "lt": "DateTime"},
}
_PRISMA_1 = {
    "UserWhereInput": {
        "id": "ID", "updatedAt": "DateTime", "updatedAt_gt": "DateTime",
        "updatedAt_gte": "DateTime", "updatedAt_lt": "DateTime",
    },
}

_HASURA_QUERY = (
    "query Users($where: users_bool_exp, $limit: Int) {\n"
    "  users(where: $where, limit: $limit) { id name updated_at }\n}"
)
_PRISMA_QUERY = (
    "query Users($where: UserWhereInput, $take: Int) {\n"
    "  users(where: $where, take: $take) { id name updatedAt }\n}"
)


@pytest.mark.parametrize("query, schema, where, used", [
    (_HASURA_QUERY, _HASURA, {"updated_at": {"_gte": SINCE}}, "where.updated_at._gte"),
    (_PRISMA_QUERY, _PRISMA, {"updatedAt": {"gte": SINCE}}, "where.updatedAt.gte"),
    (_PRISMA_QUERY, _PRISMA_1, {"updatedAt_gte": SINCE}, "where.updatedAt_gte"),
], ids=["hasura", "prisma", "prisma-1"])
def test_a_where_input_gets_a_lower_bound_on_the_updated_at_field(query, schema, where, used):
    variables: dict = {}
    assert gql._apply_since_filter(query, variables, SINCE, input_fields=schema.get) == used
    assert variables == {"where": where}


def test_a_where_the_caller_sent_is_kept():
    variables = {"where": {"id": {"_eq": 7}}}
    assert gql._apply_since_filter(_HASURA_QUERY, variables, SINCE, input_fields=_HASURA.get) is None
    assert variables == {"where": {"id": {"_eq": 7}}}


def test_a_where_on_a_field_the_query_does_not_read_is_not_built():
    # The watermark is read back from the rows, so the filter must compare the
    # same field the rows carry.
    query = _HASURA_QUERY.replace(" updated_at }", " }")
    variables: dict = {}
    assert gql._apply_since_filter(query, variables, SINCE, input_fields=_HASURA.get) is None
    assert variables == {}


@pytest.mark.parametrize("schema", [
    {},                                                                    # introspection unreadable
    {**_HASURA, "timestamptz_comparison_exp": {"_eq": "timestamptz", "_lt": "timestamptz"}},
    {"users_bool_exp": {"id": "Int_comparison_exp"}},                      # no updated-at field
], ids=["unreadable", "no-lower-bound", "no-updated-at"])
def test_a_where_the_schema_cannot_bound_is_not_built(schema):
    variables: dict = {}
    assert gql._apply_since_filter(_HASURA_QUERY, variables, SINCE, input_fields=schema.get) is None
    assert variables == {}


def test_control_without_a_schema_reader_a_where_is_left_alone():
    variables: dict = {}
    assert gql._apply_since_filter(_HASURA_QUERY, variables, SINCE) is None
    assert variables == {}


# --------------------------------------------------------------------------
# Through export(), the way the executor calls it
# --------------------------------------------------------------------------

class _Server:
    """Answers `__type(name:)` from ``schema`` and any other query with ``rows``."""

    def __init__(self, schema, rows, introspection=True):
        self.schema, self.rows, self.introspection = schema, rows, introspection
        self.sent: list = []

    def __call__(self, query, variables, config, **_kw):
        self.sent.append((query, dict(variables or {})))
        if "__type" in query:
            if not self.introspection:
                raise gql.GraphQLError("GraphQL introspection is not allowed")
            fields = self.schema.get(variables["name"])
            if fields is None:
                return {"__type": None}
            return {"__type": {"inputFields": [
                {"name": n, "type": {"kind": "NON_NULL", "name": None,
                                     "ofType": {"kind": "INPUT_OBJECT", "name": t}}}
                for n, t in fields.items()
            ]}}
        return {"users": self.rows}

    def reads(self):
        return [v for q, v in self.sent if "__type" not in q]


_ROWS = [
    {"id": 1, "name": "a", "updated_at": "2026-10-01T08:00:00.250+00:00"},
    {"id": 2, "name": "b", "updated_at": "2026-10-02T09:30:00+00:00"},
]


@pytest.fixture
def hasura(monkeypatch):
    monkeypatch.setitem(gql.GRAPHQL_OPERATIONS, "users", {
        "graphql_name": "users", "operation_type": "query", "exportable_default": True,
        "query": _HASURA_QUERY, "return_type": "users",
        "selected_fields": ["id", "name", "updated_at"], "description": "",
    })
    conn = gql.WidgetsGraphqlConnector()

    def serve(server):
        monkeypatch.setattr(conn, "_execute_graphql", server)
        return conn

    return serve


def test_export_sends_the_where_and_returns_the_new_watermark(hasura):
    server = _Server(_HASURA, _ROWS)
    out = hasura(server).export({"table": "users", "since": "2026-10-01T08:00:00.250Z"})
    assert out["success"], out
    assert server.reads() == [{"where": {"updated_at": {"_gte": SINCE}}}]
    assert out["stats"]["watermark"] == {"field": "last_modified", "value": "2026-10-02T09:30:00+00:00"}


def test_control_export_without_a_since_reads_in_full_and_asks_no_schema(hasura):
    server = _Server(_HASURA, _ROWS)
    out = hasura(server).export({"table": "users"})
    assert out["success"] and out["row_count"] == 2
    assert server.sent and all("__type" not in q for q, _ in server.sent)
    assert server.reads() == [{}]


def test_export_where_introspection_is_refused_reads_in_full(hasura):
    server = _Server(_HASURA, _ROWS, introspection=False)
    out = hasura(server).export({"table": "users", "since": SINCE})
    assert out["success"] and out["row_count"] == 2
    assert server.reads() == [{}]


# --------------------------------------------------------------------------
# One copy: the template and every connector generated from it
# --------------------------------------------------------------------------

def _between(path, start: str, end: str) -> str:
    text = pathlib.Path(path).read_text(encoding="utf-8")
    i = text.index(start)
    return text[i:text.index(end, i + len(start))]


_PIECES = {
    "helpers": ("# ISO-8601 keys the executor may use", "\ndef _max_updated_at("),
    "input_fields": ("    def _input_fields(", "\n    def "),
    "export": ("    def export(", "\n    def import_data("),
}


@pytest.mark.parametrize("piece", sorted(_PIECES))
def test_the_templates_pieces_are_plain_python(piece):
    block = _between(TEMPLATE, *_PIECES[piece])
    assert "{{" not in block and "{%" not in block and "{#" not in block


@pytest.mark.parametrize("name", GENERATED)
@pytest.mark.parametrize("piece", sorted(_PIECES))
def test_each_generated_connector_carries_the_templates_piece(name, piece):
    path = _connector_file(name)
    assert _between(path, *_PIECES[piece]) == _between(TEMPLATE, *_PIECES[piece]), (
        f"{piece} in {path} differs from connector_graphql.py.j2; change them together"
    )
