"""Offline test doubles for ``snowflake.connector``.

``snowflake-connector-python`` is intentionally NOT installed in the unit-test
environment — these tests must run with stdlib only. ``SnowflakeMCPServer`` touches
the driver only through one seam: ``_connect(config)`` returns a DB-API connection.
Tests swap that seam for ``FakeConn`` below, so there is no network / driver /
account.

The fakes record every executed statement (SQL + params) so tests can assert the
generated SQL — keyset pagination, batched multi-row INSERT, COPY routing, and the
Snowflake-correct native MERGE upsert.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional


class FakeCursor:
    def __init__(self, rows: Optional[List[Dict[str, Any]]] = None, answers=None,
                 rowcount: Optional[int] = None):
        self._default = rows if rows is not None else []
        self._rows = self._default
        # (SQL substring, rows or an Exception to raise): a catalog or snapshot
        # query gets its own answer, every other statement gets ``rows``.
        self._answers = list(answers or [])
        # call log: list of {"sql": ..., "params": ..., "timeout": ...}
        self.executed: List[Dict[str, Any]] = []
        # DB-API rowcount after execute (the driver reports DML affected rows).
        self._rowcount = rowcount
        self.rowcount: Optional[int] = None
        self.description = None

    def execute(self, sql, params=None, timeout=None):
        # ``timeout`` mirrors snowflake-connector's execute(timeout=) — the
        # server-side abort deadline; recorded so tests can assert it reached
        # the driver.
        self.executed.append({"sql": sql, "params": params, "timeout": timeout})
        self._rows = self._default
        for needle, answer in self._answers:
            if needle in sql:
                if isinstance(answer, Exception):
                    raise answer
                self._rows = answer
                break
        self.rowcount = self._rowcount
        # Column metadata as the driver exposes it: 7-tuples keyed by name.
        self.description = (
            [(k, None, None, None, None, None, None) for k in self._rows[0].keys()]
            if self._rows and isinstance(self._rows[0], dict) else None
        )
        return None

    def fetchall(self):
        return list(self._rows)

    def fetchmany(self, size=1):
        return list(self._rows)[:size]

    def fetchone(self):
        return self._rows[0] if self._rows else None

    def close(self):
        return None

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


class FakeConn:
    """Records cursors so a test can read back every statement executed."""

    def __init__(self, rows: Optional[List[Dict[str, Any]]] = None, answers=None,
                 rowcount: Optional[int] = None, commit_raises: Optional[Exception] = None):
        self._rows = rows if rows is not None else []
        self._answers = list(answers or [])
        self._rowcount = rowcount
        self._commit_raises = commit_raises
        self.cursors: List[FakeCursor] = []
        self.committed = 0
        self.rolled_back = 0
        self.closed = False

    def cursor(self, *args, **kwargs):
        # SnowflakeMCPServer._dict_cursor calls conn.cursor(DictCursor); the arg
        # is ignored here since the fake already yields dict rows.
        c = FakeCursor(rows=self._rows, answers=self._answers, rowcount=self._rowcount)
        self.cursors.append(c)
        return c

    def commit(self):
        if self._commit_raises is not None:
            raise self._commit_raises
        self.committed += 1

    def rollback(self):
        self.rolled_back += 1

    def close(self):
        self.closed = True

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    # -- test helpers -------------------------------------------------------
    def all_sql(self) -> List[str]:
        out: List[str] = []
        for c in self.cursors:
            out.extend(e["sql"] for e in c.executed)
        return out

    def all_execs(self) -> List[Dict[str, Any]]:
        out: List[Dict[str, Any]] = []
        for c in self.cursors:
            out.extend(c.executed)
        return out


def make_connector(connector_module, *, rows=None, answers=None, rowcount=None,
                   commit_raises=None):
    """Build a SnowflakeMCPServer wired to a FakeConn (the _connect seam)."""
    server = connector_module.SnowflakeMCPServer()
    conn = FakeConn(rows=rows, answers=answers, rowcount=rowcount,
                    commit_raises=commit_raises)
    server._connect = lambda config: conn  # type: ignore[assignment]
    return server, conn
