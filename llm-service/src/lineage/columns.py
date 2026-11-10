"""Column lineage: which source columns each output column of a model is made from.

api-gateway asks for it (handlers/asset_column_lineage.go) through
POST /v1/lineage/columns (src/gateway/lineage_routes.py), keeps the answer in
model_column_lineage, and draws it on the Lineage page. Lineage design step L1,
docs/internal/lineage-catalog-design.md § 3.

Read out of the SQL with sqlglot, deterministically. Nothing here calls a model or
imports anything that can, so this is not an egress call site: the SQL and the column
names never leave this process. tests/test_column_lineage_makes_no_model_call.py fails
if an import here ever loads a model client.

What comes in is metadata only: the model's SQL text and the column NAMES of the tables
it reads. No row, no value, no count.

The edge types and subtypes are OpenLineage's (ColumnLineageDatasetFacet 1-2-0), so the
export step (L3) writes them through unchanged:

  DIRECT    the output value is computed from the input: IDENTITY (copied as is),
            TRANSFORMATION (an expression of it), AGGREGATION (an aggregate over it).
  INDIRECT  the input decides which rows there are or how they are ordered: JOIN,
            FILTER, GROUP_BY, SORT, WINDOW. It shapes every output column, so it is
            recorded once for the model, with no output column.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Optional, Sequence

import sqlglot
from sqlglot import exp
from sqlglot.errors import SqlglotError
from sqlglot.lineage import Node, lineage
from sqlglot.optimizer.qualify import qualify
from sqlglot.optimizer.scope import Scope, build_scope, traverse_scope

from src.text2sql.compiler import DIALECT_MAP

STATUS_OK = "ok"
# Some output columns were traced and some were not: a SELECT * with no column list to
# expand it, a column no table could be found for, a projection sqlglot could not follow.
STATUS_PARTIAL = "partial"
STATUS_FAILED = "failed"
# Not something column lineage reads: a statement that makes no rows (UPDATE, MERGE,
# DELETE), more than one statement, or a connection with no SQL dialect (MongoDB).
STATUS_UNSUPPORTED = "unsupported"

DIRECT = "DIRECT"
INDIRECT = "INDIRECT"
IDENTITY = "IDENTITY"
TRANSFORMATION = "TRANSFORMATION"
AGGREGATION = "AGGREGATION"
JOIN = "JOIN"
FILTER = "FILTER"
GROUP_BY = "GROUP_BY"
SORT = "SORT"
WINDOW = "WINDOW"

# One model's edges. A wide model with a deep CTE chain can name thousands; past this the
# answer is cut and says so, rather than growing a row the page then has to draw.
MAX_EDGES = 2000
_REASON_MAX = 300
_ANSI = re.compile(r"\x1b\[[0-9;]*m")
_DIRECT_RANK = {IDENTITY: 0, TRANSFORMATION: 1, AGGREGATION: 2}


@dataclass(frozen=True)
class SourceTable:
    """A table the model reads, with its column names, from the warehouse's table list."""

    schema: str
    name: str
    columns: tuple[str, ...]


@dataclass(frozen=True)
class ColumnEdge:
    # The table as the SQL names it, outermost part first and unquoted: ["analytics",
    # "orders"], or ["proj", "ds", "orders"]. The caller matches it to a table it knows
    # with the same rule it matches every other read (matchTableReference).
    table: tuple[str, ...]
    column: str
    # "" for an INDIRECT edge.
    to_column: str
    type: str
    subtype: str


@dataclass
class ModelLineage:
    status: str
    # The model's output columns in order, as the warehouse names them.
    columns: list[str] = field(default_factory=list)
    edges: list[ColumnEdge] = field(default_factory=list)
    # Why the status is not ok, in a sentence a person can act on.
    reason: str = ""
    truncated: bool = False


def model_column_lineage(sql: str, connector_type: str, tables: Sequence[SourceTable] = ()) -> ModelLineage:
    """Trace each output column of one model's SQL back to the table columns it reads."""
    dialect = DIALECT_MAP.get((connector_type or "").strip().lower())
    if dialect is None:
        return ModelLineage(STATUS_UNSUPPORTED, reason=f"no SQL dialect for a {connector_type or 'unknown'} connection")
    try:
        statements = [s for s in sqlglot.parse(sql, read=dialect) if s is not None]
    except SqlglotError as err:
        return ModelLineage(STATUS_FAILED, reason=_reason("the SQL could not be read", err))
    if len(statements) != 1:
        return ModelLineage(STATUS_UNSUPPORTED, reason="column lineage reads a model of exactly one statement")

    query, named = _query_of(statements[0])
    if query is None:
        kind = statements[0].key.upper()
        return ModelLineage(STATUS_UNSUPPORTED, reason=f"a {kind} statement builds no columns to trace")

    schema = _schema(tables, dialect)
    try:
        qualified = qualify(query, schema=schema, dialect=dialect, validate_qualify_columns=False, identify=False)
        scope = build_scope(qualified)
    except (SqlglotError, KeyError, ValueError) as err:
        return ModelLineage(STATUS_FAILED, reason=_reason("the SQL could not be resolved against its tables", err))
    if scope is None or not isinstance(scope.expression, exp.Selectable):
        return ModelLineage(STATUS_UNSUPPORTED, reason="the statement selects no columns")

    out = ModelLineage(STATUS_OK)
    gaps: list[str] = []
    seen: set[ColumnEdge] = set()

    def add(edge: ColumnEdge) -> None:
        if edge in seen:
            return
        if len(out.edges) >= MAX_EDGES:
            out.truncated = True
            return
        seen.add(edge)
        out.edges.append(edge)

    # Read before any column is followed: lineage() trims the selects it walks.
    indirect = _indirect_edges(qualified)

    outputs: set[str] = set()
    for index, select in enumerate(scope.expression.selects):
        name = select.alias_or_name
        if isinstance(select, exp.Star) or name == "*":
            gaps.append("a SELECT * whose tables' columns were not available")
            continue
        output = named[index] if index < len(named) else name
        if output in outputs:
            # Two outputs of one name: the warehouse keeps one, and sqlglot can only
            # follow the first.
            continue
        outputs.add(output)
        out.columns.append(output)
        try:
            root = lineage(_quoted(name), qualified, schema=schema, dialect=dialect, scope=scope)
        except (SqlglotError, KeyError, ValueError, RecursionError) as err:
            gaps.append(_reason(f"{output} could not be followed", err))
            continue
        traced, unresolved = _direct_edges(root, output, dialect)
        for edge in traced:
            add(edge)
        if unresolved:
            gaps.append(f"{output} reads a column no table could be found for")

    for edge in indirect:
        add(edge)

    if gaps:
        out.status = STATUS_PARTIAL
        out.reason = _summary(gaps)
    return out


def _query_of(statement: exp.Expr) -> tuple[Optional[exp.Query], list[str]]:
    """The query whose columns the model builds, and the column names it writes them to.

    A SELECT is a model's rows as they are. INSERT … SELECT and CREATE TABLE … AS SELECT
    build a table from one; an INSERT naming its columns writes the n-th output to the
    n-th of them, so those names win.
    """
    if isinstance(statement, exp.Query):
        return statement, []
    if isinstance(statement, (exp.Insert, exp.Create)):
        body = statement.expression
        if isinstance(statement, exp.Create) and (statement.args.get("kind") or "").upper() not in ("TABLE", "VIEW"):
            return None, []
        if isinstance(body, exp.Query):
            target = statement.this
            named = [c.name for c in target.expressions] if isinstance(target, exp.Schema) else []
            return body, named
    return None, []


def _schema(tables: Sequence[SourceTable], dialect: str) -> Optional[dict]:
    """The tables' columns as sqlglot reads them, or None when there are none.

    Every name is passed quoted, so it is taken as the warehouse stores it: a Postgres
    column created as "Status" stays Status rather than being folded to status, while a
    dialect that ignores case for columns (BigQuery, MySQL) still folds it. The types are
    not needed to follow a column and are left unknown.
    """
    mapping: dict[str, dict[str, dict[str, str]]] = {}
    for t in tables:
        if not t.name or not t.columns:
            continue
        schema_key = _quoted_sql(t.schema or "", dialect) if t.schema else ""
        table_key = _quoted_sql(t.name, dialect)
        cols = mapping.setdefault(schema_key, {}).setdefault(table_key, {})
        for c in t.columns:
            if c:
                cols[_quoted_sql(c, dialect)] = "UNKNOWN"
    if not mapping:
        return None
    if "" in mapping:
        # A table with no schema next to tables with one: sqlglot needs one depth, and a
        # bare name is not worth dropping the rest for.
        bare = mapping.pop("")
        if not mapping:
            return bare
        for table_key, cols in bare.items():
            mapping.setdefault(_quoted_sql("public", dialect), {}).setdefault(table_key, cols)
    return mapping


def _quoted(name: str) -> exp.Column:
    return exp.Column(this=exp.to_identifier(name, quoted=True))


def _quoted_sql(name: str, dialect: str) -> str:
    return exp.to_identifier(name, quoted=True).sql(dialect=dialect)


def _direct_edges(root: Node, output: str, dialect: str) -> tuple[list[ColumnEdge], bool]:
    """Every table column an output column is computed from, and whether any was lost.

    The subtype is the strongest step on the way down: an aggregate anywhere in the path
    makes it AGGREGATION, any other expression TRANSFORMATION, and only a column copied
    through every step is IDENTITY. A column that only partitions or orders a window
    computes nothing; it is the INDIRECT WINDOW edge _indirect_edges records.
    """
    edges: list[ColumnEdge] = []
    unresolved = False
    stack: list[tuple[Node, str]] = [(root, IDENTITY)]
    visited: set[int] = set()
    while stack:
        node, subtype = stack.pop()
        if isinstance(node.expression, exp.Table):
            _, column = _column_of(node.name, dialect)
            table = tuple(p.name for p in node.expression.parts if p.name)
            if column and table:
                edges.append(ColumnEdge(table, column, output, DIRECT, subtype))
            continue
        if isinstance(node.expression, exp.Placeholder):
            # sqlglot's mark for a column it could not place in any table: unqualified,
            # in a query of two or more tables whose columns were not available.
            unresolved = True
            continue
        step = _strongest(subtype, _step_subtype(node.expression))
        window_only = _window_only_columns(node.expression)
        for child in node.downstream:
            if id(child) in visited:
                continue
            ref_table, ref_column = _column_of(child.name, dialect)
            if (ref_table.lower(), ref_column.lower()) in window_only:
                continue
            visited.add(id(child))
            stack.append((child, step))
    return edges, unresolved


def _column_of(name: str, dialect: str) -> tuple[str, str]:
    """A lineage node's name, "orders.amount" or 'orders."Status"', as (table, column)."""
    try:
        parsed = exp.to_column(name, dialect=dialect)
    except (SqlglotError, ValueError):
        parsed = None
    if isinstance(parsed, exp.Column):
        return parsed.table, parsed.name
    table, _, column = name.rpartition(".")
    return table.strip('"`[]'), column.strip('"`[]')


def _window_only_columns(expression: exp.Expr) -> set[tuple[str, str]]:
    """Columns this expression uses only to partition or order a window."""
    inside: set[tuple[str, str]] = set()
    elsewhere: set[tuple[str, str]] = set()
    for column in expression.find_all(exp.Column):
        key = (column.table.lower(), column.name.lower())
        window = column.find_ancestor(exp.Window)
        if window is not None and not _under(column, window.this):
            inside.add(key)
        else:
            elsewhere.add(key)
    return inside - elsewhere


def _under(node: exp.Expr, ancestor: Optional[exp.Expr]) -> bool:
    while node is not None:
        if node is ancestor:
            return True
        node = node.parent
    return False


def _step_subtype(expression: exp.Expr) -> str:
    inner = expression.this if isinstance(expression, exp.Alias) else expression
    if isinstance(inner, exp.Column):
        return IDENTITY
    if isinstance(inner, exp.AggFunc) or inner.find(exp.AggFunc) is not None:
        # A window over an aggregate function (SUM(x) OVER (...)) still sums x.
        return AGGREGATION
    return TRANSFORMATION


def _strongest(a: str, b: str) -> str:
    return a if _DIRECT_RANK[a] >= _DIRECT_RANK[b] else b


def _indirect_edges(qualified: exp.Expr) -> list[ColumnEdge]:
    """Table columns that decide which rows there are: joined on, filtered, grouped,
    sorted or windowed by, in any part of the query that reads the table directly."""
    edges: list[ColumnEdge] = []
    for scope in traverse_scope(qualified):
        select = scope.expression
        if not isinstance(select, exp.Select):
            continue
        clauses: list[tuple[str, Optional[exp.Expr]]] = [
            (FILTER, select.args.get("where")),
            (FILTER, select.args.get("having")),
            (FILTER, select.args.get("qualify")),
            (GROUP_BY, select.args.get("group")),
            (SORT, select.args.get("order")),
        ]
        for join in select.args.get("joins") or []:
            clauses.append((JOIN, join.args.get("on")))
        for window in select.find_all(exp.Window):
            if _owning_select(window) is select:
                for part in window.args.get("partition_by") or []:
                    clauses.append((WINDOW, part))
                clauses.append((WINDOW, window.args.get("order")))
        for subtype, clause in clauses:
            if clause is None:
                continue
            for column in clause.find_all(exp.Column):
                table = _scope_table(scope, column)
                if table is not None and column.name:
                    edges.append(ColumnEdge(table, column.name, "", INDIRECT, subtype))
    return edges


def _owning_select(node: exp.Expr) -> Optional[exp.Expr]:
    parent = node.parent
    while parent is not None and not isinstance(parent, exp.Select):
        parent = parent.parent
    return parent


def _scope_table(scope: Scope, column: exp.Column) -> Optional[tuple[str, ...]]:
    source = scope.sources.get(column.table) if column.table else _sole_source(scope)
    if isinstance(source, exp.Table):
        parts = tuple(p.name for p in source.parts if p.name)
        return parts or None
    return None


def _sole_source(scope: Scope) -> Optional[exp.Table]:
    """The one table a query reads from, for a column it names bare.

    qualify places a bare column in its table only when it knows the tables' columns
    (sqlglot 30.26 on; 30.21 also took the only table), so a model given none leaves
    `GROUP BY 1`'s column bare. Read from one table it is taken as that table's; read
    from two or more it is never guessed.
    """
    selected = list(scope.selected_sources.values())
    if len(selected) != 1:
        return None
    source = selected[0][1]
    return source if isinstance(source, exp.Table) else None


def _reason(what: str, err: BaseException) -> str:
    detail = " ".join(_ANSI.sub("", str(err)).split())
    text = f"{what}: {detail}" if detail else what
    return text[:_REASON_MAX]


def _summary(gaps: list[str]) -> str:
    first = gaps[0]
    if len(gaps) == 1:
        return first[:_REASON_MAX]
    return f"{first} (and {len(gaps) - 1} more)"[:_REASON_MAX]
