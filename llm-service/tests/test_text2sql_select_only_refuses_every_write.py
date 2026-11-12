"""The text2sql validator's SELECT-only check refuses every write.

A statement that is not a SELECT or WITH is refused outright; inside one, a
deny-list of node types caught a nested write. That list named INSERT, UPDATE,
DELETE, DROP, CREATE and ALTER, so two writes sqlglot nests in a SELECT passed
as read-only: a MERGE in a CTE (PostgreSQL 17 takes one) and SELECT ... INTO,
which creates a table. TRUNCATE was missing too, though sqlglot does not nest
it today.

Bug class: **a write the SELECT-only check's deny-list does not name.** It now
refuses sqlglot's DML and DDL bases, so a write node sqlglot adds under either
is refused without a change here, plus the write nodes outside them.
"""

from __future__ import annotations

import pytest
from sqlglot import exp

from src.text2sql import validator
from src.text2sql.validator import validate_sql


def _subclasses(cls):
    out = set()
    for sub in cls.__subclasses__():
        out |= {sub} | _subclasses(sub)
    return out


@pytest.mark.parametrize(
    "sql",
    [
        "WITH t AS (MERGE INTO x USING y ON x.a = y.a WHEN MATCHED THEN DELETE) SELECT 1",
        "SELECT * INTO t2 FROM t",
        "WITH t AS (INSERT INTO x VALUES (1) RETURNING *) SELECT * FROM t",
        "WITH t AS (UPDATE x SET a = 1 RETURNING *) SELECT * FROM t",
        "WITH t AS (DELETE FROM x RETURNING *) SELECT * FROM t",
        "TRUNCATE TABLE x",
        "SELECT 1; TRUNCATE TABLE x",
        "DROP TABLE x",
    ],
    ids=["merge-in-cte", "select-into", "insert-in-cte", "update-in-cte", "delete-in-cte", "truncate", "two-statements", "drop"],
)
def test_a_write_is_refused(sql):
    result = validate_sql(sql, "postgresql", require_limit=False)
    assert not result.valid
    assert {e.code for e in result.errors} & {"DANGEROUS_OPERATION", "NOT_SELECT"}, result.errors


@pytest.mark.parametrize(
    "sql",
    [
        "SELECT 1",
        "WITH t AS (SELECT a FROM x) SELECT a FROM t",
        # Write keywords as names are not writes.
        'SELECT "merge", "truncate", "into", "update" FROM x',
        "SELECT a FROM x WHERE b IN (SELECT b FROM y)",
    ],
)
def test_a_read_still_passes(sql):
    result = validate_sql(sql, "postgresql", require_limit=False)
    assert result.valid, result.errors


def test_every_write_node_sqlglot_has_is_refused():
    writes = _subclasses(exp.DML) | _subclasses(exp.DDL)
    assert {"Merge", "Insert", "Update", "Delete", "Copy", "Create"} <= {c.__name__ for c in writes}, "control"
    writes |= {exp.Drop, exp.Alter, exp.TruncateTable, exp.Into, exp.Command, exp.Grant, exp.Revoke, exp.LoadData}
    allowed = [c.__name__ for c in writes if not issubclass(c, validator.WRITE_NODES)]
    assert not allowed, f"a write node the SELECT-only check lets through: {sorted(allowed)}"
