"""The SQL writer and the table match wait out a provider's 429.

Found in the 2026-10-11 Ask rsync.ai eval after #1508: three questions (s02,
s04, s06) ended "drafted no SQL". Each was /api/v1/sql/generate failing on
Vertex's RESOURCE_EXHAUSTED ("SQL generation failed: entry=gemini
error=RateLimitError status=429"): #1508 gave Ask's steps the 429 waits, not
the SQL writer its generate_sql tool calls, nor the table match before it.

Bug class: **a model call with no 429 waits.** Every call of one request shares
one budget (src/utils/rate_limit_waits.py), so a draft and its repairs together
sleep no longer than 25 seconds, and the table match no longer than 15 (api-gateway
gives it 30, handlers/explorer.go ResolveExplorerTables).
"""

from __future__ import annotations

import json
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from src.agents.explorer import api as explorer_api
from src.egress.assistant_models import ResolvedModel
from src.gateway import main as gateway_main
from src.utils import rate_limit_waits

SECRET = "unit-secret"
SQL = "SELECT severity, COUNT(*) AS alerts FROM public.alerts GROUP BY severity"
BODY = {"question": "alerts by severity", "dialect": "postgresql", "schema": "public.alerts(severity)"}
ASK = {**BODY, "entry": "gemini", "model": "gemini-2.5-flash", "api_key": "unit-test-gemini-key"}
TABLES = {"question": "alerts by severity", "tables": [{"name": "alerts", "schema": "public", "columns": [{"name": "severity", "type": "text"}]}]}
TABLE_REPLY = json.dumps(
    {"candidates": [{"table": "alerts", "schema_name": "public", "confidence": 0.9, "reason": "named"}], "confidence_overall": 0.9}
)


class _RateLimited(Exception):
    """What the OpenAI SDK raises for Vertex's RESOURCE_EXHAUSTED: a 429, no Retry-After."""

    status_code = 429

    def __init__(self):
        super().__init__("Error code: 429 - RESOURCE_EXHAUSTED")
        self.response = SimpleNamespace(headers={})


class _Failed(Exception):
    def __init__(self, status):
        super().__init__(f"Error code: {status}")
        self.status_code = status


def _reply(text):
    message = SimpleNamespace(content=text)
    return SimpleNamespace(choices=[SimpleNamespace(message=message, text=text)])


class _Completions:
    """Raises or answers with each of ``outcomes`` in turn, then the last again."""

    def __init__(self, outcomes):
        self.outcomes, self.calls = list(outcomes), []

    def _next(self, kw):
        self.calls.append(kw)
        outcome = self.outcomes[min(len(self.calls), len(self.outcomes)) - 1]
        if isinstance(outcome, Exception):
            raise outcome
        return _reply(outcome)

    def create(self, **kw):
        return self._next(kw)


class _AsyncCompletions(_Completions):
    async def create(self, **kw):
        return self._next(kw)


def _client(completions):
    return SimpleNamespace(chat=SimpleNamespace(completions=completions), completions=completions)


def _sql(text=SQL):
    return json.dumps({"sql": text, "confidence": 0.9})


@pytest.fixture
def slept(monkeypatch):
    waits: list[float] = []

    async def sleep(seconds):
        waits.append(seconds)

    monkeypatch.setattr(rate_limit_waits, "_sleep", sleep)
    # random() == 0 cuts nothing off a wait, so each is the full schedule's.
    monkeypatch.setattr(rate_limit_waits.random, "random", lambda: 0.0)
    monkeypatch.setenv("LLM_PROVIDER", "ollama")  # the Explorer's own model is set up
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    monkeypatch.setattr(gateway_main, "USE_MOCK", False)
    return waits


@pytest.fixture
def writer(monkeypatch, slept):
    """/api/v1/sql/generate with an Ask model (sync) and the server's model (async)."""

    def use(outcomes, *, ask):
        completions = (_Completions if ask else _AsyncCompletions)(outcomes)
        client = _client(completions)
        if ask:
            resolved = ResolvedModel("gemini", "Gemini", client, "gemini-2.5-flash", "panel")
            monkeypatch.setattr(gateway_main, "resolve_ask_model", lambda entry, **kw: resolved)
        else:
            monkeypatch.setattr(gateway_main, "sql_client", client)
            monkeypatch.setattr(gateway_main, "sql_provider_resolved", "openai")
        body, headers = (ASK, {"X-Internal-Secret": SECRET}) if ask else (BODY, {})
        r = TestClient(gateway_main.app).post("/api/v1/sql/generate", json=body, headers=headers)
        return r, completions.calls

    return use


@pytest.mark.parametrize("ask", [True, False], ids=["ask-model", "explorer-model"])
def test_the_sql_writer_waits_out_a_rate_limit(writer, slept, ask):
    r, calls = writer([_RateLimited(), _RateLimited(), _sql()], ask=ask)
    assert r.status_code == 200, r.text
    assert r.json()["sql"].startswith("SELECT")
    assert slept == [1.0, 2.0]
    assert len(calls) == 3


def test_a_draft_and_its_repair_share_one_budget_of_waits(writer, slept):
    # "Query" is not SQL, so the writer asks its repair prompt. A budget per
    # call would wait 1, 2, then 1, 2, 4 again: 35 seconds, past what one may.
    r, calls = writer(
        [_RateLimited(), _RateLimited(), _sql("Query"), _RateLimited(), _RateLimited(), _RateLimited(), _sql()],
        ask=True,
    )
    assert r.status_code == 200, r.text
    assert slept == [1.0, 2.0, 4.0, 8.0, 10.0]
    assert calls[6]["messages"] == calls[3]["messages"], "the repair was not asked again"


def test_a_rate_limit_that_outlasts_the_waits_fails_the_writer(writer, slept):
    r, calls = writer([_RateLimited()], ask=True)
    assert r.status_code == 500
    assert r.json()["detail"] == "SQL generation failed. See the llm-service log."
    assert "RESOURCE_EXHAUSTED" not in r.text
    assert slept == [1.0, 2.0, 4.0, 8.0, 10.0] and sum(slept) <= 25
    assert len(calls) == 6


@pytest.mark.parametrize("status", [400, 401, 404, 500, 503])
def test_the_writer_tries_only_a_rate_limit_again(writer, slept, status):
    # The SDK client already retried a 5xx; a key or a request it refused
    # will not pass by waiting.
    r, calls = writer([_Failed(status), _sql()], ask=True)
    assert r.status_code == 500
    assert slept == [] and len(calls) == 1


@pytest.fixture
def table_match(monkeypatch, slept):
    def use(outcomes):
        completions = _AsyncCompletions(outcomes)
        monkeypatch.setattr(explorer_api, "explorer_client", _client(completions))
        r = TestClient(gateway_main.app).post("/api/v1/explorer/nl/resolve-tables", json=TABLES)
        return r, completions.calls

    return use


def test_the_table_match_waits_out_a_rate_limit(table_match, slept):
    r, calls = table_match([_RateLimited(), _RateLimited(), TABLE_REPLY])
    assert r.status_code == 200, r.text
    assert [c["table"] for c in r.json()["candidates"]] == ["alerts"]
    assert slept == [1.0, 2.0]
    assert len(calls) == 3


def test_the_table_match_waits_at_most_15_seconds(table_match, slept):
    # api-gateway gives the table match 30 seconds; 10 more would leave too
    # few for the calls themselves.
    r, calls = table_match([_RateLimited()])
    assert r.status_code == 500
    assert slept == [1.0, 2.0, 4.0, 8.0] and sum(slept) <= 15
    assert len(calls) == 5
