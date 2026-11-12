#!/usr/bin/env python3
"""Stable paging for SQL source exports (KI-DB-BATCH-PAGING-CAN-SKIP-ROWS).

Dependency-free and kept under ``public/`` (the ``shared`` Docker build
context), like ``canonical_types.py``, so a connector image ships it with
``COPY --from=shared sql_paging.py /app/sql_paging.py``. The PostgreSQL, MySQL,
SQL Server and Oracle connectors and ``connector_database.py.j2`` all page
through it, so a fix here reaches every SQL source at once.

A batch sweep reads a table one page at a time. Each page must start exactly
where the previous one ended, or rows are skipped with the run still reporting
success. Two shapes did that before this module:

  * a composite primary key paged on its first column only, so every row that
    shared the boundary value past the page limit was skipped;
  * a table with no key paged by ``LIMIT/OFFSET`` with no ``ORDER BY``, so the
    database was free to return rows in a different order on every page.

``paging_plan`` picks the most stable order the table offers, in this order:

  1. ``key``: a ``cursor_column`` the caller named, else the primary key,
     else the unique index with the fewest columns whose columns are all NOT
     NULL. Every column of the key is compared, so a composite key pages
     exactly.
  2. ``locator``: the row's physical address (PostgreSQL ``ctid``, Oracle
     ``ROWID``, SQLite ``rowid``). Stable unless the row moves; a moved row is
     caught by the watermark or the next full sweep.
  3. ``ordered_offset``: ``ORDER BY`` every orderable column, then OFFSET. A
     row deleted mid-sweep shifts the offsets and can skip one row.
  4. ``offset``: plain OFFSET, the old behaviour, when nothing else is known.

Warehouses
----------
Snowflake, Databricks, BigQuery and Redshift accept a primary key and never
enforce it; a ClickHouse sorting key is not unique either. Their dialects skip
the key and page by ``ordered_offset`` (a nominated ``cursor_column`` first,
then the table's own sort key, then every orderable column). Where the engine
can read a table as it was at an earlier point (Snowflake time travel, a Delta
version, BigQuery system time, an Iceberg snapshot on Athena), ``read_page``
pins every page of one sweep to the point the first page read, and carries it
in the cursor as ``{"snapshot": point, "offset": n}``: a row inserted or
deleted mid-sweep cannot shift a page. A point past 2**53 (an Iceberg snapshot
id) is carried as a string, which the executor's float64 JSON keeps exact. A
pin the engine no longer holds, a view, an external or a Hive table reads
unpinned.

Synapse and Athena
------------------
A Synapse dedicated pool has neither LIMIT nor OFFSET ... FETCH and declares
keys NOT ENFORCED, so ``SYNAPSE`` numbers the rows with ``ROW_NUMBER()`` and
pages on that number (``page_syntax="row_number"``), stripping it from the rows
again. Athena (Trino) takes ``OFFSET m LIMIT n`` in that order only
(``offset_limit``) and pyathena binds a dict (``pyformat``). Synapse cannot
read a table as of an earlier point, so it does not pin a sweep; Athena pins
an Iceberg table to its newest snapshot (``FOR VERSION AS OF``) and reads a
Hive table, which has no ``$snapshots``, unpinned. Trino compares a column
only with a value of its own type and pyathena sends a str as varchar, so
``ATHENA`` is ``typed_since``: ``since_value`` binds the watermark as the
column's timestamp, date or integer.

Adding a database
-----------------
A driver this module does not know gets ``GENERIC``: double-quoted
identifiers, ``%s`` binds, ``LIMIT/OFFSET``, and key paging only on a
``cursor_column`` the caller names. That is exactly what exports did before
this module, so a new connector keeps working with no change here. To give it
stable paging, register a ``Dialect`` for its driver module:

    register(Dialect(name="mydb", quote='"', paramstyle="qmark",
                     page_syntax="limit", discover=_discover_mydb), "mydb_driver")

``dialect_for(module, name)`` matches the connector's own name for its engine
before the driver module, so an engine reached through another's driver
(Redshift over psycopg2, Synapse over pyodbc) registers its name as a token.
A generated connector passes its driver-pattern key; a hand-curated one passes
nothing and is matched by its module as before. ``clocks`` is the select-list
fragment that reads the database's UTC and session-local clocks, which the
connectors' watermark read holds back a lag against; leave it empty only where
the engine cannot read them beside a table (Redshift's ``now()`` is
leader-node only, so it reads ``GETDATE()``).

``discover(ctx)`` returns a plan or None. Most databases only need a key
query: ``ctx.first(step, ...)`` runs steps in order, rolls back and moves on
when one raises; ``ctx.nominated_plan`` (the caller's ``cursor_column``) goes
first, as it did before this module.
See ``_discover_sqlserver`` for a short one. A database with unenforced keys
lists its orderable columns instead (``_discover_redshift``), and one that can
read a past version adds ``snapshot``/``as_of`` (``SNOWFLAKE``). A client that
is not DB-API pages through ``QueryCursor``.
``tests/test_batch_paging_stable_order.py`` pins every dialect's catalog reads
and page SQL; add the new one there.
"""
from dataclasses import dataclass
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

# Result-set alias for the row locator; stripped from rows before they leave
# the connector.
LOCATOR_ALIAS = "__rsync_locator"
# The row number a "row_number" page is cut on; stripped the same way.
ROW_NUMBER_ALIAS = "__rsync_rn"

KEY = "key"
LOCATOR = "locator"
ORDERED_OFFSET = "ordered_offset"
OFFSET = "offset"


@dataclass(frozen=True)
class Dialect:
    name: str
    # Identifier quote character. Doubled inside a name.
    quote: str = '"'
    # "format" (%s), "qmark" (?), "numeric" (:1, :2, ...), "at" (@p1, @p2,
    # ..., BigQuery's named parameters) or "pyformat" (%(p1)s, ..., bound
    # from a dict).
    paramstyle: str = "format"
    # "limit" (LIMIT n OFFSET m), "top" (TOP (n) / OFFSET m ROWS FETCH NEXT n
    # ROWS ONLY), "fetch" (FETCH FIRST n ROWS ONLY / OFFSET m ROWS ...),
    # "row_number" (TOP (n) / a ROW_NUMBER() page, for an engine with no
    # OFFSET) or "offset_limit" (LIMIT n / OFFSET m LIMIT n, Trino's order).
    page_syntax: str = "limit"
    # (a, b) > (x, y) is a native comparison the planner can use an index for.
    # Elsewhere a composite key is compared with the expanded form.
    row_values: bool = False
    # discover(ctx) -> plan or None. None: only a named cursor_column pages by
    # key, anything else by plain OFFSET.
    discover: Optional[Callable[["Catalog"], Optional[Dict[str, Any]]]] = None
    # ORDER BY for an OFFSET page when nothing is orderable, for a database
    # whose OFFSET needs one.
    no_order: str = ""
    # snapshot(ctx) -> an integer naming the table's state now, and
    # as_of(point) -> the clause after the table name that reads that state.
    # With both, an ordered_offset sweep reads every page from one state.
    snapshot: Optional[Callable[["Catalog"], Any]] = None
    as_of: Optional[Callable[[int], str]] = None
    # "<utc clock> AS wm_utc, <session clock> AS wm_local": what the
    # connectors' watermark read selects beside MAX(<field>). Empty where the
    # engine cannot read a clock beside a table, and the watermark is then not
    # held back.
    clocks: str = ""
    # The engine refuses to compare a column with a value of another type, so
    # a `since` string is bound as the column's type (since_value).
    typed_since: bool = False

    def ident(self, name: str) -> str:
        q = self.quote
        return q + str(name).replace(q, q + q) + q


class Binder:
    """Placeholders in textual order, in the dialect's bind style."""

    def __init__(self, paramstyle: str):
        self.paramstyle = paramstyle
        self.values: List[Any] = []

    def add(self, value: Any) -> str:
        self.values.append(value)
        if self.paramstyle == "qmark":
            return "?"
        if self.paramstyle == "numeric":
            return f":{len(self.values)}"
        if self.paramstyle == "at":
            return f"@p{len(self.values)}"
        if self.paramstyle == "pyformat":
            return f"%(p{len(self.values)})s"
        return "%s"

    def params(self) -> Optional[Any]:
        """A tuple, a dict for ``pyformat``, or None with nothing bound."""
        if not self.values:
            return None
        if self.paramstyle == "pyformat":
            return {f"p{i}": v for i, v in enumerate(self.values, 1)}
        return tuple(self.values)


# ---------------------------------------------------------------------------
# Plans
# ---------------------------------------------------------------------------

def offset_plan() -> Dict[str, Any]:
    return {"kind": OFFSET}


def key_plan(columns: Sequence[str], hidden: Sequence[str] = ()) -> Dict[str, Any]:
    """``hidden``: key columns ``SELECT *`` leaves out (MySQL INVISIBLE). They
    are projected for the cursor and stripped from the rows again."""
    return {"kind": KEY, "columns": list(columns), "hidden": list(hidden)}


def ordered_offset_plan(columns: Sequence[str]) -> Dict[str, Any]:
    return {"kind": ORDERED_OFFSET, "columns": list(columns)}


def locator_plan(select: str, predicate: str, order: str, table_alias: str = "") -> Dict[str, Any]:
    """``select`` is the projection, ``predicate`` a format string with one
    ``{p}`` for the bind, ``order`` the ORDER BY expression."""
    return {"kind": LOCATOR, "select": select, "predicate": predicate,
            "order": order, "table_alias": table_alias}


# ---------------------------------------------------------------------------
# Discovery
# ---------------------------------------------------------------------------

def safe_ident(name: Any) -> Optional[str]:
    """A plain identifier (letters, digits, underscore), else None. Same rule
    as the connectors' ``_safe_column_identifier``."""
    if not isinstance(name, str):
        return None
    name = name.strip()
    if not name or not all(ch.isalnum() or ch == "_" for ch in name):
        return None
    return name


def split_parts(table: str) -> List[str]:
    """Every part of a dotted table name, quotes stripped."""
    parts = [p.strip().strip('"').strip("`") for p in str(table or "").strip().split(".")]
    return [p for p in parts if p]


def split_table(table: str) -> Tuple[Optional[str], Optional[str]]:
    """(schema, name) of ``schema.table`` or ``table``, quotes stripped."""
    parts = split_parts(table)
    if not parts:
        return None, None
    if len(parts) == 1:
        return None, parts[0]
    return parts[-2], parts[-1]


def _values(row: Any) -> List[Any]:
    if row is None:
        return []
    if isinstance(row, dict):
        return list(row.values())
    return list(row)


def _rollback(conn: Any) -> None:
    try:
        conn.rollback()
    except Exception:
        pass


class Catalog:
    """What a dialect's ``discover`` gets: the open connection and the table."""

    def __init__(self, conn, cursor, dialect: Dialect, table: str, safe_table: str, nominated: Optional[str]):
        self.conn = conn
        self.cursor = cursor
        self.dialect = dialect
        self.parts = split_parts(table)
        self.schema, self.name = split_table(table)
        self.safe_table = safe_table
        self.nominated = nominated

    def rows(self, sql: str, params: Optional[Any] = None) -> List[List[Any]]:
        if params is None:
            self.cursor.execute(sql)
        else:
            self.cursor.execute(sql, params if isinstance(params, dict) else tuple(params))
        return [_values(r) for r in (self.cursor.fetchall() or [])]

    def nominated_plan(self, _ctx: "Catalog" = None) -> Optional[Dict[str, Any]]:
        return key_plan([self.nominated]) if self.nominated else None

    def first(self, *steps: Callable[["Catalog"], Optional[Dict[str, Any]]]) -> Optional[Dict[str, Any]]:
        """The first plan a step returns. A step that raises is rolled back
        (a failed statement aborts a PostgreSQL transaction) and skipped."""
        for step in steps:
            try:
                plan = step(self)
            except Exception:
                _rollback(self.conn)
                continue
            if plan:
                return plan
        return None


def _pick_index(indexes: List[Tuple[bool, List[Optional[str]], bool]]) -> Optional[List[str]]:
    """Columns of the index to page by: the primary key, else the usable
    unique index with the fewest columns. Each entry is (is_primary, columns,
    usable); a NULL column name is an expression and makes it unusable."""
    best = None
    for is_primary, cols, usable in indexes:
        if not usable or not cols or any(c is None for c in cols):
            continue
        if is_primary:
            return list(cols)
        if best is None or len(cols) < len(best):
            best = list(cols)
    return best


def _group(rows: List[List[Any]], key_idx: int) -> List[Tuple[Any, List[List[Any]]]]:
    """Consecutive rows grouped by one column, first-seen order kept."""
    groups: List[Tuple[Any, List[List[Any]]]] = []
    for r in rows:
        if groups and groups[-1][0] == r[key_idx]:
            groups[-1][1].append(r)
        else:
            groups.append((r[key_idx], [r]))
    return groups


def _truthy(v: Any) -> bool:
    if isinstance(v, str):
        return v.strip().lower() in ("1", "t", "true", "y", "yes")
    return bool(v)


# --- PostgreSQL -------------------------------------------------------------
# pg_catalog only: the information_schema constraint views hide every table a
# SELECT-only user does not own (#1280).
PG_KEY_SQL = (
    "SELECT i.indexrelid, i.indisprimary, a.attname, a.attnotnull "
    "FROM pg_catalog.pg_index i "
    "CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord) "
    "LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum "
    "WHERE i.indrelid = to_regclass(%s) "
    "AND i.indisunique AND i.indisvalid AND i.indpred IS NULL "
    "AND k.ord <= i.indnkeyatts "
    "ORDER BY i.indisprimary DESC, i.indexrelid, k.ord"
)

# Before PostgreSQL 11 (no indnkeyatts) and on Redshift: the single-column
# primary key, as exports read it before this module.
PG_LEGACY_KEY_SQL = """
                    SELECT a.attname AS column_name
                    FROM pg_catalog.pg_constraint con
                    JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
                    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                    JOIN pg_catalog.pg_attribute a
                      ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
                    WHERE con.contype = 'p'
                      AND array_length(con.conkey, 1) = 1
                      AND n.nspname = %s
                      AND c.relname = %s
                    LIMIT 1
                    """

# A plain table or materialized view with no children: ctid is unique within
# one heap only, so a partitioned or inherited parent cannot page by it.
PG_LOCATOR_SQL = (
    "SELECT c.relkind, c.relhassubclass, EXISTS ("
    "SELECT 1 FROM pg_catalog.pg_operator o WHERE o.oprname = '>' "
    "AND o.oprleft = 'pg_catalog.tid'::pg_catalog.regtype "
    "AND o.oprright = 'pg_catalog.tid'::pg_catalog.regtype) "
    "FROM pg_catalog.pg_class c WHERE c.oid = to_regclass(%s)"
)

# For a relation with neither a key nor a usable ctid: a view, a partitioned or
# inherited parent, a foreign table. A domain orders as its base type.
PG_COLUMNS_SQL = (
    "SELECT a.attname, bt.typname "
    "FROM pg_catalog.pg_attribute a "
    "JOIN pg_catalog.pg_type t ON t.oid = a.atttypid "
    "JOIN pg_catalog.pg_type bt ON bt.oid = CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.oid END "
    "WHERE a.attrelid = to_regclass(%s) AND a.attnum > 0 AND NOT a.attisdropped "
    "ORDER BY a.attnum"
)
# No default btree operator class, so ORDER BY cannot compare them. An array
# (typname `_<element>`) of one cannot be compared either.
PG_UNORDERABLE = {
    "json", "xml", "point", "line", "lseg", "box", "path", "polygon", "circle",
    "xid", "cid", "aclitem", "txid_snapshot", "pg_snapshot",
}


def _pg_key(ctx: Catalog) -> Optional[Dict[str, Any]]:
    try:
        rows = ctx.rows(PG_KEY_SQL, (ctx.safe_table,))
    except Exception:
        _rollback(ctx.conn)
        found = ctx.rows(PG_LEGACY_KEY_SQL, (ctx.schema or "public", ctx.name))
        col = found[0][0] if found and found[0] else None
        return key_plan([col]) if col else None
    indexes = []
    for _, group in _group(rows, 0):
        indexes.append((
            _truthy(group[0][1]),
            [r[2] for r in group],
            all(_truthy(r[3]) for r in group),
        ))
    cols = _pick_index(indexes)
    return key_plan(cols) if cols else None


def _pg_locator(ctx: Catalog) -> Optional[Dict[str, Any]]:
    rows = ctx.rows(PG_LOCATOR_SQL, (ctx.safe_table,))
    if not rows:
        return None
    relkind, has_children, tid_gt = rows[0][0], rows[0][1], rows[0][2]
    if str(relkind) not in ("r", "m") or _truthy(has_children) or not _truthy(tid_gt):
        return None
    return locator_plan(
        select=f'*, ctid::text AS "{LOCATOR_ALIAS}"',
        predicate="ctid > {p}::tid",
        order="ctid",
    )


def _pg_ordered(ctx: Catalog) -> Optional[Dict[str, Any]]:
    rows = ctx.rows(PG_COLUMNS_SQL, (ctx.safe_table,))
    cols = [r[0] for r in rows if str(r[1] or "").lstrip("_") not in PG_UNORDERABLE]
    return ordered_offset_plan(cols) if cols else None


def _discover_postgresql(ctx: Catalog) -> Optional[Dict[str, Any]]:
    return ctx.first(ctx.nominated_plan, _pg_key, _pg_locator, _pg_ordered)


# --- MySQL / MariaDB --------------------------------------------------------
MYSQL_COLUMNS_SQL = (
    "SELECT COLUMN_NAME, DATA_TYPE, EXTRA FROM information_schema.COLUMNS "
    "WHERE TABLE_SCHEMA = COALESCE(%s, DATABASE()) AND TABLE_NAME = %s "
    "ORDER BY ORDINAL_POSITION"
)
# ORDER BY cannot compare these.
MYSQL_UNORDERABLE = {
    "json", "geometry", "point", "linestring", "polygon", "multipoint",
    "multilinestring", "multipolygon", "geometrycollection", "geomcollection",
}


def _mysql_key(ctx: Catalog) -> Optional[Dict[str, Any]]:
    name = safe_ident(ctx.name)
    schema = safe_ident(ctx.schema) if ctx.schema else None
    if not name or (ctx.schema and not schema):
        return None
    fq = f"`{schema}`.`{name}`" if schema else f"`{name}`"
    # SHOW KEYS columns: Table, Non_unique, Key_name, Seq_in_index,
    # Column_name, Collation, Cardinality, Sub_part, Packed, Null, ...
    rows = ctx.rows(f"SHOW KEYS FROM {fq}")
    by_name: Dict[str, List[List[Any]]] = {}
    for r in rows:
        by_name.setdefault(str(r[2]), []).append(r)
    indexes = []
    for key_name, group in by_name.items():
        group.sort(key=lambda r: int(r[3] or 0))
        usable = all(
            str(r[1]).strip() in ("0", "False") and r[7] is None and str(r[9] or "").upper() != "YES"
            for r in group
        )
        indexes.append((key_name == "PRIMARY", [r[4] for r in group], usable))
    cols = _pick_index(indexes)
    return key_plan(cols) if cols else None


def _discover_mysql(ctx: Catalog) -> Optional[Dict[str, Any]]:
    try:
        columns = ctx.rows(MYSQL_COLUMNS_SQL, (ctx.schema, ctx.name))
    except Exception:
        _rollback(ctx.conn)
        columns = []

    def ordered(_ctx: Catalog) -> Optional[Dict[str, Any]]:
        cols = [c[0] for c in columns if str(c[1] or "").lower() not in MYSQL_UNORDERABLE]
        return ordered_offset_plan(cols) if cols else None

    plan = ctx.first(ctx.nominated_plan, _mysql_key, ordered)
    if plan and plan["kind"] == KEY:
        # An INVISIBLE column (e.g. the generated key my_row_id) is left out of
        # SELECT *, so its value is projected explicitly for the cursor.
        invisible = {str(c[0]) for c in columns if "INVISIBLE" in str(c[2] or "").upper()}
        plan["hidden"] = [c for c in plan["columns"] if c in invisible]
    return plan


# --- SQL Server -------------------------------------------------------------
SQLSERVER_KEY_SQL = (
    "SELECT i.index_id, i.is_primary_key, c.name, c.is_nullable "
    "FROM sys.indexes i "
    "JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id "
    "JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id "
    "WHERE i.object_id = OBJECT_ID({p}) AND i.is_unique = 1 AND i.has_filter = 0 "
    "AND i.is_disabled = 0 AND i.is_hypothetical = 0 AND ic.key_ordinal > 0 "
    "ORDER BY i.is_primary_key DESC, i.index_id, ic.key_ordinal"
)
SQLSERVER_COLUMNS_SQL = (
    "SELECT c.name, c.system_type_id FROM sys.columns c "
    "WHERE c.object_id = OBJECT_ID({p}) ORDER BY c.column_id"
)
# image, text, ntext, CLR types (geography, geometry, hierarchyid), xml.
SQLSERVER_UNORDERABLE = {34, 35, 99, 240, 241}


def _sqlserver_object(ctx: Catalog) -> Tuple[str, Tuple[str]]:
    b = Binder(ctx.dialect.paramstyle)
    ref = f"[{ctx.schema}].[{ctx.name}]" if ctx.schema else f"[{ctx.name}]"
    return b.add(ref), (ref,)


def _sqlserver_key(ctx: Catalog) -> Optional[Dict[str, Any]]:
    p, params = _sqlserver_object(ctx)
    rows = ctx.rows(SQLSERVER_KEY_SQL.format(p=p), params)
    indexes = [
        (_truthy(g[0][1]), [r[2] for r in g], not any(_truthy(r[3]) for r in g))
        for _, g in _group(rows, 0)
    ]
    cols = _pick_index(indexes)
    return key_plan(cols) if cols else None


def _sqlserver_ordered(ctx: Catalog) -> Optional[Dict[str, Any]]:
    p, params = _sqlserver_object(ctx)
    rows = ctx.rows(SQLSERVER_COLUMNS_SQL.format(p=p), params)
    cols = [r[0] for r in rows if int(r[1] or 0) not in SQLSERVER_UNORDERABLE]
    return ordered_offset_plan(cols) if cols else None


def _discover_sqlserver(ctx: Catalog) -> Optional[Dict[str, Any]]:
    return ctx.first(ctx.nominated_plan, _sqlserver_key, _sqlserver_ordered)


def _discover_synapse(ctx: Catalog) -> Optional[Dict[str, Any]]:
    # A dedicated pool's PRIMARY KEY and UNIQUE are NOT ENFORCED: order by
    # every orderable column, as for the other warehouses.
    p, params = _sqlserver_object(ctx)
    rows = ctx.rows(SQLSERVER_COLUMNS_SQL.format(p=p), params)
    return _ordered(ctx, [r[0] for r in rows if int(r[1] or 0) not in SQLSERVER_UNORDERABLE])


# --- Oracle -----------------------------------------------------------------
_ORACLE_OWNER = "NVL(:1, SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))"
ORACLE_KEY_SQL = (
    "SELECT c.constraint_name, c.constraint_type, cc.column_name, tc.nullable "
    "FROM all_constraints c "
    "JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name "
    "AND cc.table_name = c.table_name "
    "JOIN all_tab_columns tc ON tc.owner = c.owner AND tc.table_name = c.table_name "
    "AND tc.column_name = cc.column_name "
    f"WHERE c.owner = {_ORACLE_OWNER} AND c.table_name = :2 "
    "AND c.constraint_type IN ('P', 'U') AND c.status = 'ENABLED' AND c.validated = 'VALIDATED' "
    "ORDER BY CASE c.constraint_type WHEN 'P' THEN 0 ELSE 1 END, c.constraint_name, cc.position"
)
# A heap table: an index-organized table's ROWID is logical, an external
# table has none. A view is in neither list.
ORACLE_HEAP_SQL = (
    "SELECT t.iot_type FROM all_tables t "
    f"WHERE t.owner = {_ORACLE_OWNER} AND t.table_name = :2 "
    "AND NOT EXISTS (SELECT 1 FROM all_external_tables e "
    "WHERE e.owner = t.owner AND e.table_name = t.table_name)"
)
ORACLE_COLUMNS_SQL = (
    "SELECT column_name, data_type, data_type_owner FROM all_tab_columns "
    f"WHERE owner = {_ORACLE_OWNER} AND table_name = :2 ORDER BY column_id"
)
ORACLE_UNORDERABLE = {"CLOB", "NCLOB", "BLOB", "BFILE", "LONG", "LONG RAW", "XMLTYPE", "JSON"}
_ORACLE_ALIAS = "rsync_t"


def _oracle_key(ctx: Catalog) -> Optional[Dict[str, Any]]:
    rows = ctx.rows(ORACLE_KEY_SQL, (ctx.schema, ctx.name))
    indexes = [
        (str(g[0][1]) == "P", [r[2] for r in g], all(str(r[3]).upper() == "N" for r in g))
        for _, g in _group(rows, 0)
    ]
    cols = _pick_index(indexes)
    return key_plan(cols) if cols else None


def _oracle_locator(ctx: Catalog) -> Optional[Dict[str, Any]]:
    rows = ctx.rows(ORACLE_HEAP_SQL, (ctx.schema, ctx.name))
    if not rows or rows[0][0] is not None:
        return None
    a = _ORACLE_ALIAS
    return locator_plan(
        select=f'{a}.*, ROWIDTOCHAR({a}.ROWID) AS "{LOCATOR_ALIAS}"',
        predicate=f"{a}.ROWID > CHARTOROWID({{p}})",
        order=f"{a}.ROWID",
        table_alias=f" {a}",
    )


def _oracle_ordered(ctx: Catalog) -> Optional[Dict[str, Any]]:
    rows = ctx.rows(ORACLE_COLUMNS_SQL, (ctx.schema, ctx.name))
    cols = [r[0] for r in rows if str(r[1] or "").upper() not in ORACLE_UNORDERABLE and r[2] is None]
    return ordered_offset_plan(cols) if cols else None


def _discover_oracle(ctx: Catalog) -> Optional[Dict[str, Any]]:
    return ctx.first(ctx.nominated_plan, _oracle_key, _oracle_locator, _oracle_ordered)


# --- SQLite -----------------------------------------------------------------
def _sqlite_pragma(ctx: Catalog, pragma: str, arg: str) -> List[List[Any]]:
    q = ctx.dialect.ident
    prefix = f"{q(ctx.schema)}." if ctx.schema else ""
    return ctx.rows(f"PRAGMA {prefix}{pragma}({q(arg)})")


def _sqlite_columns(ctx: Catalog) -> List[List[Any]]:
    # cid, name, type, notnull, dflt_value, pk
    return _sqlite_pragma(ctx, "table_info", ctx.name)


def _sqlite_pk(columns: List[List[Any]]) -> List[str]:
    return [c[1] for c in sorted((c for c in columns if int(c[5] or 0) > 0), key=lambda c: int(c[5]))]


def _sqlite_key(ctx: Catalog) -> Optional[Dict[str, Any]]:
    columns = _sqlite_columns(ctx)
    notnull = {c[1] for c in columns if _truthy(c[3])}
    pk = _sqlite_pk(columns)
    if pk:
        types = {c[1]: str(c[2] or "").upper() for c in columns}
        # An INTEGER PRIMARY KEY is the rowid and never NULL; any other key
        # column may hold NULL unless declared NOT NULL.
        if (len(pk) == 1 and types.get(pk[0]) == "INTEGER") or all(c in notnull for c in pk):
            return key_plan(pk)
    indexes = []
    # seq, name, unique, origin, partial
    for idx in _sqlite_pragma(ctx, "index_list", ctx.name):
        if not _truthy(idx[2]) or (len(idx) > 4 and _truthy(idx[4])):
            continue
        # seqno, cid, name
        cols = [r[2] for r in sorted(_sqlite_pragma(ctx, "index_info", idx[1]), key=lambda r: int(r[0]))]
        indexes.append((False, cols, all(c in notnull for c in cols)))
    cols = _pick_index(indexes)
    return key_plan(cols) if cols else None


def _sqlite_is_view(ctx: Catalog) -> bool:
    q = ctx.dialect.ident
    prefix = f"{q(ctx.schema)}." if ctx.schema else ""
    rows = ctx.rows(f"SELECT type FROM {prefix}sqlite_master WHERE name = ? COLLATE NOCASE", (ctx.name,))
    return any(str(r[0]).lower() == "view" for r in rows)


def _sqlite_locator(ctx: Catalog) -> Optional[Dict[str, Any]]:
    # A view has no rowid, but a SQLite built with SQLITE_ALLOW_ROWID_IN_VIEW
    # (Debian's, so the connector image's) answers the probe below with NULL,
    # and `rowid > ?` then matches nothing. Ask the schema instead; if that
    # raises, `first` skips this step and the view pages in column order.
    if _sqlite_is_view(ctx):
        return None
    columns = _sqlite_columns(ctx)
    taken = {str(c[1]).lower() for c in columns}
    rowid = next((r for r in ("rowid", "_rowid_", "oid") if r not in taken), None)
    if rowid is None:
        return None
    try:
        ctx.rows(f"SELECT {rowid} FROM {ctx.safe_table} LIMIT 0")
    except Exception:
        # A WITHOUT ROWID table has no rowid, and its primary key is NOT NULL.
        pk = _sqlite_pk(columns)
        return key_plan(pk) if pk else None
    return locator_plan(
        select=f'*, {rowid} AS "{LOCATOR_ALIAS}"',
        predicate=f"{rowid} > CAST({{p}} AS INTEGER)",
        order=rowid,
    )


def _sqlite_ordered(ctx: Catalog) -> Optional[Dict[str, Any]]:
    # A view has no rowid; SQLite orders every storage class.
    cols = [c[1] for c in _sqlite_columns(ctx)]
    return ordered_offset_plan(cols) if cols else None


def _discover_sqlite(ctx: Catalog) -> Optional[Dict[str, Any]]:
    return ctx.first(ctx.nominated_plan, _sqlite_key, _sqlite_locator, _sqlite_ordered)


# --- Warehouses -------------------------------------------------------------
# A declared key is never enforced, so none is trusted: a sweep orders by every
# orderable column. The caller's cursor_column goes first, then the table's own
# sort key, so the order reads naturally and the engine can use them.
def _base_type(data_type: Any) -> str:
    return str(data_type or "").split("(")[0].strip()


def _ordered(ctx: Catalog, names: Sequence[str], leading: Sequence[str] = ()) -> Optional[Dict[str, Any]]:
    names = [str(n) for n in names]
    by_lower: Dict[str, str] = {}
    for n in names:
        by_lower.setdefault(n.lower(), n)
    cols: List[str] = []

    def take(name: str) -> None:
        found = name if name in names else by_lower.get(name.lower())
        if found is not None and found not in cols:
            cols.append(found)

    for name in ([ctx.nominated] if ctx.nominated else []) + list(leading) + names:
        take(name)
    return ordered_offset_plan(cols) if cols else None


SNOWFLAKE_COLUMNS_SQL = (
    "SELECT COLUMN_NAME, DATA_TYPE FROM {db}INFORMATION_SCHEMA.COLUMNS "
    "WHERE TABLE_SCHEMA = COALESCE(%s, CURRENT_SCHEMA()) AND TABLE_NAME = %s "
    "ORDER BY ORDINAL_POSITION"
)
SNOWFLAKE_UNORDERABLE = {"VARIANT", "OBJECT", "ARRAY", "MAP", "GEOGRAPHY", "GEOMETRY", "VECTOR", "FILE"}


def _discover_snowflake(ctx: Catalog) -> Optional[Dict[str, Any]]:
    db = f"{ctx.dialect.ident(ctx.parts[-3])}." if len(ctx.parts) >= 3 else ""
    rows = ctx.rows(SNOWFLAKE_COLUMNS_SQL.format(db=db), (ctx.schema, ctx.name))
    return _ordered(ctx, [r[0] for r in rows if _base_type(r[1]).upper() not in SNOWFLAKE_UNORDERABLE])


def _snowflake_snapshot(ctx: Catalog) -> Any:
    return ctx.rows("SELECT DATE_PART(EPOCH_MILLISECOND, CURRENT_TIMESTAMP())")[0][0]


def _snowflake_as_of(point: int) -> str:
    return f"AT(TIMESTAMP => TO_TIMESTAMP_LTZ({int(point)}, 3))"


# Spark orders a struct or an array whose elements it can order.
DATABRICKS_UNORDERABLE = ("map<", "variant", "geometry", "geography")


def _discover_databricks(ctx: Catalog) -> Optional[Dict[str, Any]]:
    names = []
    # col_name, data_type, comment; the partition and detail sections follow a
    # blank or '#' row.
    for r in ctx.rows(f"DESCRIBE TABLE {ctx.safe_table}"):
        name = str(r[0] or "").strip() if r else ""
        if not name or name.startswith("#"):
            break
        data_type = str(r[1] or "").lower() if len(r) > 1 else ""
        if not any(t in data_type for t in DATABRICKS_UNORDERABLE):
            names.append(name)
    return _ordered(ctx, names)


def _databricks_snapshot(ctx: Catalog) -> Any:
    # Newest first; the first column is the Delta version.
    return ctx.rows(f"DESCRIBE HISTORY {ctx.safe_table} LIMIT 1")[0][0]


def _databricks_as_of(point: int) -> str:
    return f"VERSION AS OF {int(point)}"


def _athena_snapshot(ctx: Catalog) -> Any:
    # An Iceberg table lists its snapshots in "<table>$snapshots"; a Hive
    # table has none, so this raises and the sweep reads unpinned.
    prefix = "".join(f"{ctx.dialect.ident(p)}." for p in ctx.parts[:-1])
    name = ctx.dialect.ident(f"{ctx.name}$snapshots")
    return ctx.rows(f"SELECT snapshot_id FROM {prefix}{name} ORDER BY committed_at DESC LIMIT 1")[0][0]


def _athena_as_of(point: int) -> str:
    return f"FOR VERSION AS OF {int(point)}"


BIGQUERY_UNORDERABLE = {"RECORD", "STRUCT", "JSON", "GEOGRAPHY", "RANGE"}


def _discover_bigquery(ctx: Catalog) -> Optional[Dict[str, Any]]:
    # ctx.conn is the google.cloud.bigquery Client.
    fields = ctx.conn.get_table(".".join(ctx.parts)).schema
    names = [f.name for f in fields
             if str(getattr(f, "mode", "") or "").upper() != "REPEATED"
             and str(getattr(f, "field_type", "") or "").upper() not in BIGQUERY_UNORDERABLE
             and not any(ch in f.name for ch in "`\\")]
    return _ordered(ctx, names)


def _bigquery_snapshot(ctx: Catalog) -> Any:
    return ctx.rows("SELECT UNIX_MILLIS(CURRENT_TIMESTAMP())")[0][0]


def _bigquery_as_of(point: int) -> str:
    return f"FOR SYSTEM_TIME AS OF TIMESTAMP_MILLIS({int(point)})"


REDSHIFT_COLUMNS_SQL = (
    "SELECT column_name, data_type FROM svv_columns "
    "WHERE table_schema = %s AND table_name = %s ORDER BY ordinal_position"
)
REDSHIFT_UNORDERABLE = {"geometry", "geography", "hllsketch", "super", "varbyte", "binary varying", "varbinary"}


def _discover_redshift(ctx: Catalog) -> Optional[Dict[str, Any]]:
    schema, name = ctx.schema or "public", str(ctx.name or "")
    rows = ctx.rows(REDSHIFT_COLUMNS_SQL, (schema, name))
    if not rows and (schema, name) != (schema.lower(), name.lower()):
        # Redshift folds identifiers to lower case unless the cluster enables
        # case-sensitive identifiers.
        rows = ctx.rows(REDSHIFT_COLUMNS_SQL, (schema.lower(), name.lower()))
    return _ordered(ctx, [r[0] for r in rows if _base_type(r[1]).lower() not in REDSHIFT_UNORDERABLE])


ATHENA_COLUMNS_SQL = (
    "SELECT column_name, data_type FROM {catalog}information_schema.columns "
    "WHERE table_schema = {schema} AND table_name = {name} ORDER BY ordinal_position"
)
ATHENA_UNORDERABLE = {"map", "array", "row", "struct", "json", "hyperloglog", "p4hyperloglog",
                      "qdigest", "tdigest", "setdigest", "geometry", "sphericalgeography"}


def _discover_athena(ctx: Catalog) -> Optional[Dict[str, Any]]:
    # ctx.conn is a pyathena connection; schema_name is its default database.
    # The Glue catalog keeps every name in lower case.
    schema = ctx.schema or getattr(ctx.conn, "schema_name", None) or "default"
    catalog = f"{ctx.dialect.ident(ctx.parts[-3])}." if len(ctx.parts) >= 3 else ""
    b = Binder(ctx.dialect.paramstyle)
    sql = ATHENA_COLUMNS_SQL.format(catalog=catalog, schema=b.add(str(schema).lower()),
                                    name=b.add(str(ctx.name or "").lower()))
    names = []
    for name, data_type in ctx.rows(sql, b.params()):
        base = str(data_type or "").split("(")[0].split("<")[0].strip().lower()
        # % would break pyathena's dict formatting of the page SQL.
        if base not in ATHENA_UNORDERABLE and not any(ch in str(name) for ch in '%"'):
            names.append(str(name))
    return _ordered(ctx, names)


CLICKHOUSE_COLUMNS_SQL = (
    "SELECT name, type, default_kind FROM system.columns "
    "WHERE database = %s AND table = %s ORDER BY position"
)
CLICKHOUSE_SORTING_KEY_SQL = "SELECT sorting_key FROM system.tables WHERE database = %s AND name = %s"
CLICKHOUSE_UNORDERABLE = ("AggregateFunction(", "Dynamic", "Variant(", "JSON", "Object(", "Map(")
# SELECT * leaves these out.
CLICKHOUSE_NOT_SELECTED = {"MATERIALIZED", "ALIAS", "EPHEMERAL"}


def _discover_clickhouse(ctx: Catalog) -> Optional[Dict[str, Any]]:
    database = ctx.schema or "default"
    names = []
    for name, data_type, default_kind in ctx.rows(CLICKHOUSE_COLUMNS_SQL, (database, ctx.name)):
        data_type = str(data_type or "").replace("SimpleAggregateFunction(", "")
        if (str(default_kind or "").upper() in CLICKHOUSE_NOT_SELECTED
                or any(t in data_type for t in CLICKHOUSE_UNORDERABLE)
                # Backticks escape with a backslash here; % would break a bind.
                or any(ch in str(name) for ch in "`\\%")):
            continue
        names.append(str(name))
    leading: List[str] = []
    try:
        found = ctx.rows(CLICKHOUSE_SORTING_KEY_SQL, (database, ctx.name))
        key = [k.strip() for k in str(found[0][0] or "").split(",")] if found else []
        # Only a key of plain columns; an expression such as toDate(ts) is not one.
        if key and all(k in names for k in key):
            leading = key
    except Exception:
        _rollback(ctx.conn)
    return _ordered(ctx, names, leading)


# ---------------------------------------------------------------------------
# Registry
# ---------------------------------------------------------------------------

_PG_CLOCKS = "now() AT TIME ZONE 'UTC' AS wm_utc, LOCALTIMESTAMP AS wm_local"
_SQLSERVER_CLOCKS = "SYSUTCDATETIME() AS wm_utc, SYSDATETIME() AS wm_local"
# A TIMESTAMP column and current_timestamp() come back from the driver in the
# same form, aware or naive, so one clock serves both comparisons.
_DATABRICKS_CLOCKS = "current_timestamp() AS wm_utc, current_timestamp() AS wm_local"
# GETDATE() runs beside a table, unlike now()/LOCALTIMESTAMP; it is UTC unless
# the session sets a time zone.
_REDSHIFT_CLOCKS = "GETDATE() AS wm_utc, GETDATE() AS wm_local"

GENERIC = Dialect(name="generic")
POSTGRESQL = Dialect(name="postgresql", row_values=True, discover=_discover_postgresql, clocks=_PG_CLOCKS)
MYSQL = Dialect(name="mysql", quote="`", discover=_discover_mysql,
                clocks="UTC_TIMESTAMP(6) AS wm_utc, NOW(6) AS wm_local")
SQLSERVER_QMARK = Dialect(name="sqlserver", paramstyle="qmark", page_syntax="top",
                          discover=_discover_sqlserver, no_order="(SELECT NULL)", clocks=_SQLSERVER_CLOCKS)
SQLSERVER_FORMAT = Dialect(name="sqlserver", paramstyle="format", page_syntax="top",
                           discover=_discover_sqlserver, no_order="(SELECT NULL)", clocks=_SQLSERVER_CLOCKS)
ORACLE = Dialect(name="oracle", paramstyle="numeric", page_syntax="fetch", discover=_discover_oracle,
                 clocks="SYS_EXTRACT_UTC(SYSTIMESTAMP) AS wm_utc, LOCALTIMESTAMP AS wm_local")
SQLITE = Dialect(name="sqlite", paramstyle="qmark", discover=_discover_sqlite)
SNOWFLAKE = Dialect(name="snowflake", discover=_discover_snowflake,
                    snapshot=_snowflake_snapshot, as_of=_snowflake_as_of,
                    clocks="SYSDATE() AS wm_utc, CURRENT_TIMESTAMP()::TIMESTAMP_NTZ AS wm_local")
DATABRICKS = Dialect(name="databricks", quote="`", paramstyle="qmark", discover=_discover_databricks,
                     snapshot=_databricks_snapshot, as_of=_databricks_as_of, clocks=_DATABRICKS_CLOCKS)
# Not registered: the BigQuery client is not DB-API, so its exports page
# through QueryCursor with this dialect named directly.
BIGQUERY = Dialect(name="bigquery", quote="`", paramstyle="at", discover=_discover_bigquery,
                   snapshot=_bigquery_snapshot, as_of=_bigquery_as_of)
REDSHIFT = Dialect(name="redshift", discover=_discover_redshift, clocks=_REDSHIFT_CLOCKS)
CLICKHOUSE = Dialect(name="clickhouse", quote="`", discover=_discover_clickhouse)
SYNAPSE = Dialect(name="synapse", paramstyle="qmark", page_syntax="row_number", discover=_discover_synapse,
                  no_order="(SELECT NULL)", clocks=_SQLSERVER_CLOCKS)
ATHENA = Dialect(name="athena", paramstyle="pyformat", page_syntax="offset_limit", discover=_discover_athena,
                 snapshot=_athena_snapshot, as_of=_athena_as_of, typed_since=True,
                 clocks="CAST(current_timestamp AT TIME ZONE 'UTC' AS timestamp) AS wm_utc, "
                        "localtimestamp AS wm_local")

_REGISTRY: List[Tuple[Tuple[str, ...], Dialect]] = []


def register(dialect: Dialect, *module_tokens: str) -> None:
    """Use ``dialect`` for an engine name or driver module that contains any
    token. Registered first, matched first."""
    _REGISTRY.append((tuple(t.lower() for t in module_tokens), dialect))


register(POSTGRESQL, "psycopg", "pg8000")
register(MYSQL, "mysql")
register(SQLSERVER_QMARK, "pyodbc")
register(SQLSERVER_FORMAT, "pymssql")
register(ORACLE, "oracle")
register(SQLITE, "sqlite")
# redshift_connector by its module; Redshift over psycopg2 and Synapse over
# pyodbc by the engine name dialect_for is given.
register(REDSHIFT, "redshift")
register(SNOWFLAKE, "snowflake")
register(DATABRICKS, "databricks")
register(CLICKHOUSE, "clickhouse")
register(SYNAPSE, "synapse")
register(ATHENA, "athena")


def _match(text: Optional[str]) -> Optional[Dialect]:
    t = (text or "").lower()
    if not t:
        return None
    for tokens, dialect in _REGISTRY:
        if any(tok in t for tok in tokens):
            return dialect
    return None


def dialect_for(module: Optional[str], name: Optional[str] = None) -> Dialect:
    """The dialect for the connector's engine ``name`` (a generated
    connector's driver-pattern key), else for its driver ``module``, else
    ``GENERIC``."""
    return _match(name) or _match(module) or GENERIC


def paging_plan(conn, cursor, dialect: Dialect, table: str, safe_table: str,
                nominated: Optional[str] = None) -> Dict[str, Any]:
    """The most stable order ``table`` offers (module docstring). Never
    raises: a failed catalog read falls to the next option."""
    ctx = Catalog(conn, cursor, dialect, table, safe_table, safe_ident(nominated) if nominated else None)
    plan = None
    if dialect.discover is not None:
        try:
            plan = dialect.discover(ctx)
        except Exception:
            _rollback(conn)
            plan = None
    if not plan:
        plan = ctx.nominated_plan()
    return plan or offset_plan()


# ---------------------------------------------------------------------------
# Page SQL
# ---------------------------------------------------------------------------

def _key_position(columns: List[str], value: Any) -> Optional[Tuple[Any, bool]]:
    """(values, prefix) for a key cursor, or None when it cannot place a row.

    A list must match the key's width. A scalar on a composite key is a
    checkpoint from before composite paging, which recorded the first column
    only: ``prefix`` compares that column with >=, re-reading the rows that
    share it rather than skipping them."""
    if value is None or value == "" or isinstance(value, dict):
        return None
    if isinstance(value, (list, tuple)):
        if len(value) != len(columns) or any(v is None for v in value):
            return None
        return list(value), False
    if len(columns) == 1:
        return [value], False
    return value, True


def _key_after(dialect: Dialect, columns: List[str], position: Tuple[Any, bool], b: Binder) -> str:
    values, prefix = position
    idents = [dialect.ident(c) for c in columns]
    if prefix:
        return f"{idents[0]} >= {b.add(values)}"
    if len(idents) == 1:
        return f"{idents[0]} > {b.add(values[0])}"
    if dialect.row_values:
        rhs = ", ".join(b.add(v) for v in values)
        return f"({', '.join(idents)}) > ({rhs})"

    # (c1, c2, c3) > (v1, v2, v3) without row values:
    # c1 >= v1 AND (c1 > v1 OR (c2 >= v2 AND (c2 > v2 OR c3 > v3)))
    # The leading >= on each column keeps an index on the key usable.
    def after(i: int) -> str:
        if i == len(idents) - 1:
            return f"{idents[i]} > {b.add(values[i])}"
        ge = f"{idents[i]} >= {b.add(values[i])}"
        gt = f"{idents[i]} > {b.add(values[i])}"
        return f"{ge} AND ({gt} OR ({after(i + 1)}))"

    return f"({after(0)})"


def page_query(dialect: Dialect, plan: Dict[str, Any], safe_table: str, *, limit: int, offset: int = 0,
               cursor: Any = None, since_cursor: Any = None, inc_field: Optional[str] = None,
               inc_value: Any = None, snapshot: Optional[int] = None) -> Tuple[str, Optional[Any]]:
    """(sql, params) for one page.

    ``snapshot`` pins an OFFSET page to the dialect's ``as_of`` point.
    ``cursor`` is this run's paging position and narrows with AND. The delta
    is ``(inc_field > inc_value OR key > since_cursor)``: an UPDATE to a row
    already synced comes back through the timestamp, an INSERT with a NULL or
    hand-kept timestamp through the key (INCREMENTAL.md §5). When the plan
    cannot place ``since_cursor`` (no key, or a key of another shape) the
    whole delta is dropped and the run re-reads the table: too many rows is
    recoverable, too few is not.
    """
    limit, offset = int(limit), int(offset or 0)
    b = Binder(dialect.paramstyle)
    kind = plan.get("kind", OFFSET)
    columns = plan.get("columns") or []
    where: List[str] = []

    if kind == KEY:
        position = _key_position(columns, cursor)
        if position is not None:
            where.append(_key_after(dialect, columns, position, b))
    elif kind == LOCATOR and isinstance(cursor, dict):
        loc = cursor.get("locator")
        if loc not in (None, ""):
            where.append(plan["predicate"].format(p=b.add(str(loc))))

    since_position = None
    if since_cursor is not None and since_cursor != "":
        since_position = _key_position(columns, since_cursor) if kind == KEY else None
        drop_delta = since_position is None
    else:
        drop_delta = False
    if not drop_delta:
        delta = []
        if inc_field and inc_value is not None:
            delta.append(f"{dialect.ident(inc_field)} > {b.add(inc_value)}")
        if since_position is not None:
            delta.append(_key_after(dialect, columns, since_position, b))
        if delta:
            where.append("(" + " OR ".join(delta) + ")")
    where_sql = (" WHERE " + " AND ".join(where)) if where else ""

    if kind in (KEY, LOCATOR):
        if kind == KEY:
            hidden = plan.get("hidden") or []
            projection = ", ".join(["*"] + [dialect.ident(c) for c in hidden])
            source = safe_table
            order = ", ".join(dialect.ident(c) for c in columns)
        else:
            projection = plan["select"]
            source = safe_table + plan.get("table_alias", "")
            order = plan["order"]
        if dialect.page_syntax in ("top", "row_number"):
            sql = f"SELECT TOP ({limit}) {projection} FROM {source}{where_sql} ORDER BY {order}"
        elif dialect.page_syntax == "fetch":
            sql = f"SELECT {projection} FROM {source}{where_sql} ORDER BY {order} FETCH FIRST {limit} ROWS ONLY"
        else:
            # "limit" and "offset_limit" alike.
            sql = f"SELECT {projection} FROM {source}{where_sql} ORDER BY {order} LIMIT {limit}"
        return sql, b.params()

    order = ", ".join(dialect.ident(c) for c in columns) if kind == ORDERED_OFFSET else ""
    source = safe_table
    if snapshot is not None and dialect.as_of is not None:
        source = f"{safe_table} {dialect.as_of(int(snapshot))}"
    if dialect.page_syntax == "top":
        sql = (f"SELECT * FROM {source}{where_sql} ORDER BY {order or dialect.no_order or '(SELECT NULL)'} "
               f"OFFSET {offset} ROWS FETCH NEXT {limit} ROWS ONLY")
    elif dialect.page_syntax == "row_number":
        rn = dialect.ident(ROW_NUMBER_ALIAS)
        sql = (f"SELECT TOP ({limit}) * FROM (SELECT *, ROW_NUMBER() OVER "
               f"(ORDER BY {order or dialect.no_order or '(SELECT NULL)'}) AS {rn} FROM {source}{where_sql}) "
               f"AS {dialect.ident('__rsync_page')} WHERE {rn} > {offset} ORDER BY {rn}")
    elif dialect.page_syntax == "fetch":
        order_sql = f" ORDER BY {order}" if order else ""
        sql = f"SELECT * FROM {source}{where_sql}{order_sql} OFFSET {offset} ROWS FETCH NEXT {limit} ROWS ONLY"
    elif dialect.page_syntax == "offset_limit":
        order_sql = f" ORDER BY {order}" if order else ""
        sql = f"SELECT * FROM {source}{where_sql}{order_sql} OFFSET {offset} LIMIT {limit}"
    else:
        order_sql = f" ORDER BY {order}" if order else ""
        sql = f"SELECT * FROM {source}{where_sql}{order_sql} LIMIT {limit} OFFSET {offset}"
    return sql, b.params()


# ---------------------------------------------------------------------------
# Page result
# ---------------------------------------------------------------------------

def _row_value(row: Dict[str, Any], column: str) -> Any:
    if column in row:
        return row[column]
    low = column.lower()
    for k, v in row.items():
        if str(k).lower() == low:
            return v
    return None


def _strip(data: List[Any], columns: List[str], drop: Sequence[str]) -> List[str]:
    if not drop:
        return columns
    gone = set(drop)
    for r in data:
        if isinstance(r, dict):
            for c in drop:
                r.pop(c, None)
    return [c for c in columns if c not in gone]


def finish_page(plan: Dict[str, Any], data: List[Dict[str, Any]], columns: List[str]
                ) -> Tuple[List[Dict[str, Any]], List[str], Dict[str, Any]]:
    """(data, columns, paging fields) for a fetched page.

    The fields go into the export result as is. A key page reports
    ``next_cursor`` (a scalar for a one-column key, a list otherwise) with
    ``cursor_column`` or ``cursor_columns``. A locator page reports
    ``{"locator": ...}``, which is a position and never a key high-water. An
    OFFSET page reports ``paging_mode: offset`` so the executor steps the
    offset and drops any cursor it held."""
    kind = plan.get("kind", OFFSET)
    columns = list(columns or [])
    fields: Dict[str, Any] = {}
    if kind == KEY:
        keys = plan.get("columns") or []
        hidden = plan.get("hidden") or []
        if data and isinstance(data[-1], dict):
            values = [_row_value(data[-1], c) for c in keys]
            if values and all(v is not None for v in values):
                # A hidden key never reaches the destination, and as a string
                # a BIGINT past 2**53 survives the executor's JSON hop.
                values = [str(v) if c in hidden else v for c, v in zip(keys, values)]
                fields["next_cursor"] = values[0] if len(keys) == 1 else values
                fields["paging_mode"] = "keyset"
                if len(keys) == 1:
                    fields["cursor_column"] = keys[0]
                else:
                    fields["cursor_columns"] = list(keys)
        columns = _strip(data, columns, hidden)
    elif kind == LOCATOR:
        loc = _row_value(data[-1], LOCATOR_ALIAS) if data and isinstance(data[-1], dict) else None
        columns = _strip(data, columns, [LOCATOR_ALIAS])
        if loc is not None:
            fields["next_cursor"] = {"locator": str(loc)}
            fields["paging_mode"] = "locator"
    else:
        fields["paging_mode"] = "offset"
    return data, columns, fields


# ---------------------------------------------------------------------------
# Reading a page
# ---------------------------------------------------------------------------

# The largest integer the executor's float64 JSON numbers hold exactly.
_MAX_EXACT_JSON_INT = 1 << 53


def _as_token(value: Any) -> Optional[int]:
    """An integer from a cursor that crossed JSON (1.7e12 comes back a float,
    or a string), else None. A string of digits is read exactly: an Iceberg
    snapshot id does not survive a float."""
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, str):
        text = value.strip()
        if text.lstrip("-").isascii() and text.lstrip("-").isdigit():
            return int(text)
    try:
        f = float(value)
    except (TypeError, ValueError):
        return None
    return int(f) if f.is_integer() else None


def snapshot_position(cursor: Any) -> Optional[Tuple[Optional[int], int]]:
    """(point, offset) of a pinned sweep's cursor, else None. The point is None
    once the engine lost the pin: the sweep goes on unpinned from the offset."""
    if not isinstance(cursor, dict) or "snapshot" not in cursor:
        return None
    offset = _as_token(cursor.get("offset"))
    if offset is None or offset < 0:
        return None
    return _as_token(cursor["snapshot"]), offset


class QueryCursor:
    """The DB-API cursor surface ``read_page`` uses, over a client that is not
    DB-API. ``run(sql, params)`` returns (column names, rows)."""

    def __init__(self, run: Callable[[str, Optional[Sequence[Any]]], Tuple[Sequence[str], Sequence[Any]]]):
        self._run = run
        self._rows: List[Any] = []
        self.description = None

    def execute(self, sql: str, params: Optional[Sequence[Any]] = None) -> None:
        columns, rows = self._run(sql, params)
        self.description = [(c, None, None, None, None, None, None) for c in columns] if columns else None
        self._rows = list(rows or [])

    def fetchall(self) -> List[Any]:
        rows, self._rows = self._rows, []
        return rows

    def close(self) -> None:
        pass


_INTEGER_TYPES = {"tinyint", "smallint", "int", "integer", "bigint"}


def since_value(dialect: Dialect, value: Any, column: Any) -> Any:
    """``value`` as the type of its watermark column, for a ``typed_since``
    dialect: a timestamp as a UTC-naive datetime (the engine's session zone
    is UTC), a date as a date, an integer as an int. ``column`` is the
    column's ``cursor.description`` entry. Anything else, and every value it
    cannot parse, is returned unchanged."""
    if not dialect.typed_since or not isinstance(value, str) or not column or len(column) < 2:
        return value
    kind = str(column[1] or "").split("(")[0].strip().lower()
    text = value.strip()
    try:
        if kind.startswith("timestamp") or kind == "date":
            from datetime import datetime, timezone
            parsed = datetime.fromisoformat(text[:-1] + "+00:00" if text.endswith("Z") else text)
            if parsed.tzinfo is not None:
                parsed = parsed.astimezone(timezone.utc).replace(tzinfo=None)
            return parsed.date() if kind == "date" else parsed
        if kind in _INTEGER_TYPES:
            return int(text)
    except ValueError:
        pass
    return value


def _typed_since(conn, cursor, dialect: Dialect, safe_table: str, field: str, value: Any) -> Any:
    """``since_value`` for ``field``, its type read from an empty result."""
    try:
        cursor.execute(f"SELECT * FROM {safe_table} WHERE 1 = 0")
        description = list(cursor.description or [])
        try:
            cursor.fetchall()
        except Exception:
            pass
    except Exception:
        _rollback(conn)
        return value
    column = next((d for d in description if str(d[0]).lower() == str(field).lower()), None)
    return since_value(dialect, value, column)


def _take_snapshot(conn, cursor, dialect: Dialect, table: str, safe_table: str) -> Optional[int]:
    try:
        return _as_token(dialect.snapshot(Catalog(conn, cursor, dialect, table, safe_table, None)))
    except Exception:
        # A view, an external table, or an engine without history: unpinned.
        _rollback(conn)
        return None


def read_page(conn, cursor, dialect: Dialect, table: str, safe_table: str, *, limit: int, offset: int = 0,
              cursor_value: Any = None, nominated: Optional[str] = None, stable: bool = True,
              to_dict: Optional[Callable[[Any, Any], Dict[str, Any]]] = None, since_cursor: Any = None,
              inc_field: Optional[str] = None, inc_value: Any = None
              ) -> Tuple[List[Dict[str, Any]], List[str], Dict[str, Any]]:
    """Plan, query, fetch and finish one export page: (data, columns, paging
    fields), as ``finish_page`` returns them.

    ``stable`` False reads plain OFFSET (an export that asked for no stable
    order). ``to_dict(row, description)`` turns a driver row into a dict; by
    default a dict row is copied and a tuple zipped with the column names.

    An ``ordered_offset`` sweep on a dialect with ``snapshot`` pins every page
    to the point the first page read: those pages report ``paging_mode:
    snapshot`` and, on a full page, ``next_cursor`` ``{"snapshot": point,
    "offset": n}``. A pin the engine refuses is dropped: on the first page the
    sweep steps OFFSET as before, later it keeps the cursor with no point.
    A cursor of another shape re-reads from the top, which duplicates rows and
    never skips one."""
    plan = paging_plan(conn, cursor, dialect, table, safe_table, nominated) if stable else offset_plan()
    offset = int(offset or 0)
    if dialect.typed_since and inc_field and isinstance(inc_value, str):
        inc_value = _typed_since(conn, cursor, dialect, safe_table, inc_field, inc_value)
    point: Optional[int] = None
    pinned = False
    resumed = False
    if plan.get("kind") == ORDERED_OFFSET:
        position = snapshot_position(cursor_value)
        if position is not None:
            point, offset = position
            pinned = resumed = True
        elif dialect.snapshot is not None and dialect.as_of is not None and offset == 0:
            point = _take_snapshot(conn, cursor, dialect, table, safe_table)
            pinned = point is not None
    while True:
        sql, params = page_query(dialect, plan, safe_table, limit=limit, offset=offset, cursor=cursor_value,
                                 since_cursor=since_cursor, inc_field=inc_field, inc_value=inc_value,
                                 snapshot=point)
        try:
            if params is None:
                cursor.execute(sql)
            else:
                cursor.execute(sql, params)
            rows = cursor.fetchall() or []
            break
        except Exception:
            if point is None:
                raise
            # Retention passed, the table was replaced, or it keeps no history.
            _rollback(conn)
            point = None
            pinned = resumed

    desc = getattr(cursor, "description", None)
    names = [d[0] for d in desc] if desc else []
    if to_dict is not None:
        data = [to_dict(r, desc) for r in rows]
    else:
        data = [dict(r) if isinstance(r, dict) else dict(zip(names, r)) for r in rows]
    columns = names or (list(data[0].keys()) if data and isinstance(data[0], dict) else [])
    if dialect.page_syntax == "row_number" and plan.get("kind") in (ORDERED_OFFSET, OFFSET):
        columns = _strip(data, columns, [ROW_NUMBER_ALIAS])
    data, columns, fields = finish_page(plan, data, columns)
    if pinned:
        fields = {"paging_mode": "snapshot"}
        if len(data) >= int(limit):
            exact = point if point is None or abs(point) < _MAX_EXACT_JSON_INT else str(point)
            fields["next_cursor"] = {"snapshot": exact, "offset": offset + len(data)}
    return data, columns, fields


def export_page(conn, cursor, dialect: Dialect, table: str, safe_table: str, params: Optional[Dict[str, Any]],
                *, limit: int, nominated: Optional[str] = None,
                to_dict: Optional[Callable[[Any, Any], Dict[str, Any]]] = None) -> Dict[str, Any]:
    """An export result for one page of ``table``, from the export request's
    ``params`` (``use_keyset_paging``, ``cursor``, ``offset``). ``nominated``
    is the caller's ``cursor_column``, already checked safe.

    A key page whose last row lacks the key cannot continue, so it fails
    instead of ending the sweep early."""
    params = params or {}
    cursor_value = params.get("cursor")
    if cursor_value is None:
        cursor_value = params.get("cursor_value")
    stable = bool(params.get("use_keyset_paging") or params.get("use_cursor_paging")
                  or cursor_value is not None or nominated)
    try:
        offset = int(params.get("offset") or 0)
    except (TypeError, ValueError):
        offset = 0
    data, columns, fields = read_page(conn, cursor, dialect, table, safe_table, limit=limit, offset=offset,
                                      cursor_value=cursor_value, nominated=nominated, stable=stable,
                                      to_dict=to_dict)
    full = len(data) >= int(limit)
    if full and "paging_mode" not in fields:
        return {"success": False,
                "error": "the paging key is missing from the page's last row; cannot continue"}
    result: Dict[str, Any] = {"success": True, "data": data, "columns": columns, "total_records": len(data),
                              "has_more": full, "next_cursor": None, "truncated": False}
    result.update(fields)
    return result
