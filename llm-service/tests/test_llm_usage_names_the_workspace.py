"""Every LLM usage row names the workspace the call was for.

``llm_usage_events`` is the LLM cost ledger, and it was keyed by user and
pipeline only (migration 057), so per-workspace spend could not be read for a
user in two workspaces or for a call with no pipeline. The bug class is a usage
ledger that cannot name its tenant; its two siblings, ``data_transfer_usage_events``
and ``nl_query_usage_events``, have always carried ``workspace_id``.

The workspace is on the request since #1299 (``src.egress.scope``), but the row
is written on a background thread that cannot see the request's ContextVar, so
``record_usage`` has to capture it when it queues the event. The column itself
(migration 124) is guarded in the api-gateway lane, next to the migrations:
``api-gateway/internal/db/usage_ledgers_name_their_workspace_test.go``.
"""

from __future__ import annotations

import importlib
import queue
import re
import sys
from decimal import Decimal
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

WS = "5b0c3f4e-9a1d-4c1e-8f2a-3d6e7b8c9d01"


def _mod(name):
    """Looked up when a test runs: test_explorer_endpoints.py purges every
    ``src.*`` module at collection, so a copy imported here would hold a
    different ContextVar and queue."""
    return importlib.import_module(name)


@pytest.fixture
def queued(monkeypatch):
    """record_usage's queue, with no writer thread draining it."""
    llm_cost = _mod("src.utils.llm_cost")
    q: "queue.Queue[dict]" = queue.Queue()
    monkeypatch.setattr(llm_cost, "_event_queue", q)
    monkeypatch.setattr(llm_cost, "_ensure_writer", lambda: None)
    return q


def _record(**overrides):
    kwargs = dict(
        user_id=None,
        pipeline_id=None,
        prompt_name="chat/help_response",
        provider="openai",
        model="gpt-4o-mini",
        prompt_tokens=10,
        completion_tokens=5,
    )
    kwargs.update(overrides)
    return _mod("src.utils.llm_cost").record_usage(**kwargs)


def test_a_usage_event_names_the_workspace_of_the_request(queued):
    with _mod("src.egress.scope").workspace_scope(WS):
        _record()
    assert queued.get_nowait()["workspace_id"] == WS


def test_an_explicit_workspace_wins_over_the_request(queued):
    other = "0f0e0d0c-0b0a-4908-8706-050403020100"
    with _mod("src.egress.scope").workspace_scope(WS):
        _record(workspace_id=other)
    assert queued.get_nowait()["workspace_id"] == other


@pytest.mark.parametrize("scoped", ["", "demo-workspace", "5b0c3f4e"])
def test_a_workspace_that_is_not_a_uuid_is_stored_as_null(queued, scoped):
    # The column is UUID: a value Postgres cannot cast fails the INSERT, and the
    # same statement batch charges the user's quota, so both would be lost.
    with _mod("src.egress.scope").workspace_scope(scoped):
        _record()
    assert queued.get_nowait()["workspace_id"] is None


def test_the_completion_endpoint_queues_the_callers_workspace(monkeypatch, queued):
    main = _mod("src.gateway.main")

    async def create(**_kwargs):
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="ok"), finish_reason="stop")],
            usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1, total_tokens=2),
        )

    model = SimpleNamespace(
        base_url="https://api.openai.com/v1",
        chat=SimpleNamespace(completions=SimpleNamespace(create=create)),
    )
    monkeypatch.setattr(main, "USE_MOCK", False)
    monkeypatch.setattr(main, "require_llm", lambda *a, **k: None)
    monkeypatch.setattr(main, "default_client", model)
    scope = _mod("src.egress.scope")
    response = TestClient(main.app).post(
        "/v1/completion",
        json={"prompt_name": "chat/help_response", "variables": {"user_message": "what does rsync.ai do?"}},
        headers={scope.WORKSPACE_HEADER: WS},
    )
    assert response.status_code == 200, response.text
    assert queued.get_nowait()["workspace_id"] == WS


class _Cursor:
    def __init__(self, calls):
        self.calls = calls

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def execute(self, sql, params):
        self.calls.append((sql, params))


def test_the_writer_inserts_the_workspace(monkeypatch):
    llm_cost = _mod("src.utils.llm_cost")
    calls: list = []
    conn = SimpleNamespace(closed=False, autocommit=False, cursor=lambda: _Cursor(calls))
    monkeypatch.setitem(sys.modules, "psycopg2", SimpleNamespace(connect=lambda _dsn: conn))
    monkeypatch.setenv("DATABASE_URL", "postgresql://unit-test/db")
    q: "queue.Queue[dict]" = queue.Queue()
    monkeypatch.setattr(llm_cost, "_event_queue", q)
    monkeypatch.setattr(llm_cost, "_ensure_writer", lambda: None)
    user = "11111111-2222-4333-8444-555555555555"
    with _mod("src.egress.scope").workspace_scope(WS):
        _record(user_id=user)
    q.put(None)  # the writer's shutdown sentinel

    llm_cost._writer_loop()

    insert_sql, insert_params = calls[0]
    columns = [c.strip() for c in re.search(r"\(([^)]*)\)\s*VALUES", insert_sql).group(1).split(",")]
    assert "workspace_id" in columns
    assert insert_params[columns.index("workspace_id")] == WS
    assert len(columns) == len(insert_params) == insert_sql.count("%s")
    # The quota charge still follows the row, at the call's exact cost
    # (test_llm_cost_is_rounded_only_as_a_total.py).
    assert calls[1] == ("SELECT check_and_charge_user(%s::uuid, %s)", (user, Decimal("0.00045")))

