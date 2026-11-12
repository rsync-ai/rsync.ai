"""Every model call /api/v1/sql/generate makes writes a usage row.

``llm_usage_events`` is the LLM cost ledger and the user's monthly quota is
charged from it, but only /v1/completion and the assistant's step wrote to it.
The Explorer's SQL generation and Ask rsync.ai's generate_sql, the draft and
every repair, called a model and wrote nothing, so their spend was in no
ledger and against no quota. api-gateway now sends the user the request is for
(handlers/explorer.go GenerateSQL) and each call through
``_guarded_explorer_call`` is recorded with the provider and model it went to.

Bug class: **a model call no ledger row records.** The census at the bottom
names, for every call site the egress guard knows, whether it is costed; a new
call site fails it until someone decides.
"""

from __future__ import annotations

import importlib
import logging
import queue
from decimal import Decimal
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from src.egress.assistant_models import ResolvedModel

SECRET = "unit-secret"
USER = "11111111-2222-4333-8444-555555555555"
WS = "5b0c3f4e-9a1d-4c1e-8f2a-3d6e7b8c9d01"
SQL = "SELECT severity, COUNT(*) AS alerts FROM public.alerts GROUP BY severity"
BODY = {"question": "alerts by severity", "dialect": "postgresql", "schema": "public.alerts(severity)"}
ASK = {**BODY, "entry": "claude", "model": "claude-sonnet-5-5", "api_key": "unit-test-claude-key"}
# 120 prompt and 30 completion tokens a call.
USAGE = SimpleNamespace(prompt_tokens=120, completion_tokens=30, total_tokens=150)


def _mod(name):
    """Looked up when a test runs: test_explorer_endpoints.py purges every
    ``src.*`` module at collection."""
    return importlib.import_module(name)


def _reply(content):
    message = SimpleNamespace(content=content)
    return SimpleNamespace(choices=[SimpleNamespace(message=message, text=content, finish_reason="stop")], usage=USAGE)


class _Completions:
    """Answers each call with the next of script, then the last again."""

    def __init__(self, script):
        self.calls, self.script = [], list(script)

    def create(self, **kw):
        self.calls.append(kw)
        return _reply(self.script[min(len(self.calls), len(self.script)) - 1])


class _AsyncCompletions(_Completions):
    async def create(self, **kw):
        return _Completions.create(self, **kw)


def _client(base_url, completions):
    return SimpleNamespace(base_url=base_url, calls=completions.calls, chat=SimpleNamespace(completions=completions))


def _json_sql(sql):
    return '{"sql": "%s", "confidence": 0.9}' % sql


@pytest.fixture
def ledger(monkeypatch):
    """record_usage's queue, with no writer thread draining it."""
    llm_cost = _mod("src.utils.llm_cost")
    q: "queue.Queue[dict]" = queue.Queue()
    monkeypatch.setattr(llm_cost, "_event_queue", q)
    monkeypatch.setattr(llm_cost, "_ensure_writer", lambda: None)

    def rows():
        out = []
        while not q.empty():
            out.append(q.get_nowait())
        return out

    return rows


@pytest.fixture
def gateway(monkeypatch):
    main = _mod("src.gateway.main")
    # The server's own SQL model: OpenAI's gpt-4o-mini, priced.
    server = _client("https://api.openai.com/v1", _AsyncCompletions([_json_sql(SQL)]))
    monkeypatch.setattr(main, "sql_client", server)
    monkeypatch.setattr(main, "sql_provider_resolved", "openai")
    monkeypatch.setattr(main, "USE_MOCK", False)
    monkeypatch.setenv("EXPLORER_SQL_OPENAI_MODEL", "gpt-4o-mini")
    monkeypatch.setenv("LLM_PROVIDER", "ollama")  # the gate: the Explorer's model is set up
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    return SimpleNamespace(main=main, server=server, http=TestClient(main.app))


def _ask_model(monkeypatch, main, script=(_json_sql(SQL),)):
    ask = _client("https://api.anthropic.com/v1/", _Completions(list(script)))
    monkeypatch.setattr(
        main, "resolve_ask_model", lambda entry, **kw: ResolvedModel(entry, "Claude", ask, kw["model"], "panel")
    )
    return ask


def _post(gateway, body, **headers):
    return gateway.http.post("/api/v1/sql/generate", json=body, headers={"X-Workspace-ID": WS, **headers})


def test_the_explorers_sql_call_is_recorded_for_its_user(gateway, ledger):
    r = _post(gateway, {**BODY, "user_id": USER})
    assert r.status_code == 200, r.text
    assert len(gateway.server.calls) == 1, "control: the server's model wrote the SQL in one call"

    (row,) = ledger()
    assert row["user_id"] == USER and row["workspace_id"] == WS and row["pipeline_id"] is None
    assert row["prompt_name"] == "explorer/sql_generate"
    assert (row["provider"], row["model"]) == ("openai", "gpt-4o-mini")
    assert (row["prompt_tokens"], row["completion_tokens"]) == (120, 30)
    # 120 tokens at 15¢ and 30 at 60¢ per million.
    assert row["cost_usd_cents"] == Decimal("0.0036")


def test_an_ask_call_is_recorded_at_the_ask_models_price(gateway, ledger, monkeypatch):
    ask = _ask_model(monkeypatch, gateway.main)
    r = _post(gateway, {**ASK, "user_id": USER}, **{"X-Internal-Secret": SECRET})
    assert r.status_code == 200, r.text
    assert len(ask.calls) == 1 and gateway.server.calls == []

    (row,) = ledger()
    assert row["user_id"] == USER
    # Where the call went, not the entry's name: the price table's provider.
    assert (row["provider"], row["model"]) == ("anthropic", "claude-sonnet-5-5")
    # 120 tokens at 200¢ and 30 at 1000¢ per million.
    assert row["cost_usd_cents"] == Decimal("0.054")


_TYPED = {
    "schema": None,
    "tables_typed": [{"name": "alerts", "schema": "public", "columns": [{"name": "severity", "type": "text"}]}],
}


@pytest.mark.parametrize(
    "question,schema,first",
    [
        ("alerts by severity", {}, "Query"),
        (
            "alerts per month",
            {"schema": "public.alerts(severity, created_at)"},
            "SELECT created_at, COUNT(*) AS alerts FROM public.alerts GROUP BY created_at",
        ),
        ("alerts by severity", _TYPED, "SELECT ghost, COUNT(*) FROM public.alerts GROUP BY ghost LIMIT 100"),
    ],
    ids=["not-select", "time-grain", "validation"],
)
def test_every_repair_is_a_row_of_its_own(gateway, ledger, monkeypatch, question, schema, first):
    month = "SELECT DATE_TRUNC('month', created_at) AS month, COUNT(*) AS alerts FROM public.alerts GROUP BY 1"
    then = month if "month" in question else SQL + " LIMIT 100"
    ask = _ask_model(monkeypatch, gateway.main, script=(_json_sql(first), _json_sql(then)))
    r = _post(gateway, {**ASK, "question": question, **schema, "user_id": USER}, **{"X-Internal-Secret": SECRET})
    assert r.status_code == 200, r.text
    assert len(ask.calls) >= 2, f"control: no repair ran ({len(ask.calls)} call)"

    rows = ledger()
    assert len(rows) == len(ask.calls)
    assert rows[0]["prompt_name"] == "explorer/sql_generate"
    assert all(row["prompt_name"] != "explorer/sql_generate" for row in rows[1:]), "a repair was not named"
    assert {(row["user_id"], row["provider"], row["model"]) for row in rows} == {
        (USER, "anthropic", "claude-sonnet-5-5")
    }


def test_the_query_spec_call_is_recorded(gateway, ledger, monkeypatch):
    spec = '{"from_table": "public.alerts", "intent_description": "alerts by severity"}'
    explorer = _client("https://api.openai.com/v1", _AsyncCompletions([spec]))
    monkeypatch.setattr(gateway.main, "explorer_client", explorer)
    monkeypatch.setenv("TEXT2SQL_USE_QUERY_SPEC", "true")
    r = _post(gateway, {**BODY, **_TYPED, "user_id": USER})
    assert r.status_code == 200, r.text
    assert explorer.calls, "control: the QuerySpec path did not run"

    rows = [row for row in ledger() if row["prompt_name"] == "explorer/query_spec"]
    assert len(rows) == len(explorer.calls)
    assert {(row["user_id"], row["provider"], row["model"]) for row in rows} == {
        (USER, "openai", gateway.main.EXPLORER_QUERY_SPEC_MODEL)
    }


def test_a_call_with_no_user_is_still_a_row(gateway, ledger):
    # A caller that names no user (an api-gateway older than this) still
    # leaves the spend in the ledger; it is charged to no one's quota.
    r = _post(gateway, BODY)
    assert r.status_code == 200, r.text
    (row,) = ledger()
    assert row["user_id"] is None and row["cost_usd_cents"] == Decimal("0.0036")


def test_a_failed_record_never_fails_the_sql(gateway, monkeypatch, caplog):
    def broken(**_kw):
        raise RuntimeError("ledger down")

    monkeypatch.setattr(_mod("src.utils.llm_cost"), "record_usage", broken)
    with caplog.at_level(logging.WARNING):
        r = _post(gateway, {**BODY, "user_id": USER})
    assert r.status_code == 200, r.text
    assert r.json()["sql"].startswith("SELECT")
    assert "usage" in caplog.text.lower()


def test_the_user_is_a_declared_field():
    # The request refuses an undeclared key (extra="forbid"), so api-gateway
    # sending user_id to an llm-service older than this gets a 422: deploy
    # llm-service first, or both together.
    req = _mod("src.gateway.main").SQLGenerateRequest(**BODY, user_id=USER)
    assert req.user_id == USER


# Every call site the egress guard knows (src/egress/call_sites.py), costed or
# not. A costed site writes an llm_usage_events row per model call, tested:
#   gateway.completion   tests/test_llm_usage_names_the_workspace.py
#   assistant.step       tests/test_assistant_step_route.py
#   explorer.sql         this file
#   explorer.query_spec  this file
COSTED = {"gateway.completion", "assistant.step", "explorer.sql", "explorer.query_spec"}
# Known gaps: each calls a model and writes no row. Costing one moves it up.
UNCOSTED = {
    "gateway.diagnose",
    "explorer.table_link",
    "explorer.column_link",
    "explorer.next_steps",
    "explorer.rank_tables",
    "suggestions.transforms",
    "connector_resolver",
    "logo_downloader",
    "tool_generator.agent",
}


def test_every_call_site_is_costed_or_a_known_gap():
    sites = set(_mod("src.egress.call_sites").CALL_SITES)
    assert not COSTED & UNCOSTED
    unclassified = sites - COSTED - UNCOSTED
    assert not unclassified, f"a new model call site: does it write a usage row? {sorted(unclassified)}"
    assert not (COSTED | UNCOSTED) - sites, "a listed call site no longer exists"
