"""Ask rsync.ai's generate_sql writes with the model the question is answered with.

Found asking Ask rsync.ai about a BigQuery warehouse (2026-10-09): the question
was answered by the Claude model an admin picked, but the SQL it showed was
written by the server's Explorer model, so a better model's answer came with a
weaker model's query. api-gateway now sends the entry, its model and the saved
key (handlers/explorer.go GenerateSQL, set in-process by AssistantSQL.Write)
with the internal secret; the Explorer's own generation sends none of it.

Bug class: **a sub-step that picks its own model.** Every model call in the
generation and its repairs must use the one the request resolved.
"""

from __future__ import annotations

import logging

import pytest
from fastapi.testclient import TestClient

from src.egress.assistant_models import ResolvedModel
from src.gateway import main as gateway_main

SECRET = "unit-secret"
KEY = "unit-test-claude-key-4f9a"
SQL = "SELECT severity, COUNT(*) AS alerts FROM public.alerts GROUP BY severity"
BODY = {"question": "alerts by severity", "dialect": "postgresql", "schema": "public.alerts(severity)"}
ASK = {**BODY, "entry": "claude", "model": "claude-sonnet-5-5", "api_key": KEY}


def _reply(sql=SQL):
    message = type("M", (), {"content": '{"sql": "%s", "confidence": 0.9}' % sql})()
    return type("R", (), {"choices": [type("C", (), {"message": message, "text": sql})()]})()


class _Completions:
    """Answers each call with the next of script's SQL, then the last again."""

    def __init__(self, calls, fail=None, script=(SQL,)):
        self.calls, self.fail, self.script = calls, fail, list(script)

    def create(self, **kw):
        self.calls.append(kw)
        if self.fail:
            raise self.fail
        return _reply(self.script[min(len(self.calls), len(self.script)) - 1])


class _AsyncCompletions(_Completions):
    async def create(self, **kw):
        return _Completions.create(self, **kw)


class _Client:
    """A provider client: .chat.completions.create, recording each call."""

    def __init__(self, completions):
        self.calls = completions.calls
        self.chat = type("_Chat", (), {"completions": completions})()
        self.completions = completions


@pytest.fixture
def stack(monkeypatch):
    server = _Client(_AsyncCompletions([]))
    ask = _Client(_Completions([]))
    resolved = []

    def resolve(entry, *, key="", model=""):
        resolved.append((entry, key, model))
        return ResolvedModel(entry, "Claude", ask, model or "claude-default", "panel" if key else "env")

    monkeypatch.setattr(gateway_main, "sql_client", server)
    monkeypatch.setattr(gateway_main, "sql_provider_resolved", "ollama")
    monkeypatch.setattr(gateway_main, "USE_MOCK", False)
    monkeypatch.setattr(gateway_main, "resolve_ask_model", resolve)
    monkeypatch.setenv("LLM_PROVIDER", "ollama")  # the Explorer's own model is set up
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    return type("S", (), {"server": server, "ask": ask, "resolved": resolved, "client": TestClient(gateway_main.app)})


def test_ask_sql_is_written_by_the_ask_model(stack):
    r = stack.client.post("/api/v1/sql/generate", json=ASK, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 200, r.text
    assert stack.resolved == [("claude", KEY, "claude-sonnet-5-5")]
    assert stack.ask.calls, "control: the Ask model was never called"
    assert {c["model"] for c in stack.ask.calls} == {"claude-sonnet-5-5"}
    assert stack.server.calls == [], "the server's Explorer model wrote part of the SQL"
    # An Ask model gets the JSON prompt, not the Ollama one the server uses here.
    prompt = "\n".join(m["content"] for m in stack.ask.calls[0]["messages"] if isinstance(m.get("content"), str))
    assert '"sql"' in prompt


def test_the_explorer_keeps_the_server_model(stack):
    r = stack.client.post("/api/v1/sql/generate", json=BODY)
    assert r.status_code == 200, r.text
    assert stack.server.calls, "control: the server's model was never called"
    assert stack.resolved == [] and stack.ask.calls == []


_TYPED = {
    "schema": None,
    "tables_typed": [{"name": "alerts", "schema": "public", "columns": [{"name": "severity", "type": "text"}]}],
}


@pytest.mark.parametrize(
    "question,schema,first",
    [
        # Not a query: the not-SELECT repair.
        ("alerts by severity", {}, "Query"),
        # A month question answered without month buckets: the time-grain repair.
        (
            "alerts per month",
            {"schema": "public.alerts(severity, created_at)"},
            "SELECT created_at, COUNT(*) AS alerts FROM public.alerts GROUP BY created_at",
        ),
        # A column the typed schema lacks: the validation repair.
        ("alerts by severity", _TYPED, "SELECT ghost, COUNT(*) FROM public.alerts GROUP BY ghost LIMIT 100"),
    ],
    ids=["not-select", "time-grain", "validation"],
)
def test_every_repair_is_written_by_the_ask_model_too(stack, monkeypatch, question, schema, first):
    month = "SELECT DATE_TRUNC('month', created_at) AS month, COUNT(*) AS alerts FROM public.alerts GROUP BY 1"
    ask = _Client(_Completions([], script=(first, month if "month" in question else SQL + " LIMIT 100")))
    monkeypatch.setattr(
        gateway_main, "resolve_ask_model", lambda entry, **kw: ResolvedModel(entry, "Claude", ask, kw["model"], "panel")
    )
    body = {**ASK, "question": question, **schema}
    r = stack.client.post("/api/v1/sql/generate", json=body, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 200, r.text
    assert len(ask.calls) >= 2, f"control: no repair ran ({len(ask.calls)} call)"
    assert {c["model"] for c in ask.calls} == {"claude-sonnet-5-5"}
    assert stack.server.calls == [], "a repair went to the server's Explorer model"


@pytest.mark.parametrize("headers", [{}, {"X-Internal-Secret": "wrong"}])
def test_a_model_and_key_need_the_internal_secret(stack, headers):
    r = stack.client.post("/api/v1/sql/generate", json=ASK, headers=headers)
    assert r.status_code == 401
    assert stack.resolved == [] and stack.ask.calls == [] and stack.server.calls == []


def test_no_secret_configured_refuses_outside_dev(stack, monkeypatch):
    monkeypatch.delenv("INTERNAL_SERVICE_SECRET")
    monkeypatch.setenv("ENVIRONMENT", "production")
    r = stack.client.post("/api/v1/sql/generate", json=ASK)
    assert r.status_code == 503
    assert stack.resolved == []


def test_an_entry_with_no_key_anywhere_is_llm_not_configured(stack, monkeypatch):
    monkeypatch.setattr(gateway_main, "resolve_ask_model", lambda entry, **_: None)
    r = stack.client.post("/api/v1/sql/generate", json={**BODY, "entry": "gemini"}, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 503
    assert r.json()["error"] == "llm_not_configured"  # api-gateway's sqlStepError reads this
    assert stack.server.calls == [], "fell back to the server's model"


def test_an_unknown_entry_is_refused(stack):
    r = stack.client.post("/api/v1/sql/generate", json={**BODY, "entry": "llama"}, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 422
    assert stack.resolved == []


def test_a_failed_ask_call_never_logs_the_providers_text(stack, monkeypatch, caplog):
    failing = _Client(_Completions([], fail=RuntimeError(f"invalid x-api-key: {KEY[:12]}...")))
    monkeypatch.setattr(
        gateway_main, "resolve_ask_model", lambda entry, **kw: ResolvedModel(entry, "Claude", failing, kw["model"], "panel")
    )
    with caplog.at_level(logging.DEBUG):
        r = stack.client.post("/api/v1/sql/generate", json=ASK, headers={"X-Internal-Secret": SECRET})
    assert r.status_code == 500
    assert KEY not in r.text and KEY[:12] not in r.text
    assert failing.calls, "control: the failing model was called"
    assert KEY[:12] not in caplog.text, "the provider's error text was logged"
    assert "RuntimeError" in caplog.text


def test_the_key_never_prints():
    req = gateway_main.SQLGenerateRequest(**ASK)
    assert KEY not in repr(req) and KEY not in str(req)
