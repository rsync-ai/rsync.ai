"""An LLM route that fails answers with fixed text, never the exception.

A provider error can quote the request it rejected, and a connection error
names a host and a user. The routes below used to put `str(e)` in the answer
(`detail=f"LLM call failed: {e}"`, `detail=str(e)`, `error=str(e)`), and the
callers show it, log it, or hand it back to a model as context. The exception
now goes to the llm-service log only.

Each case below raises an exception carrying a host, an email and a password,
and checks the answer holds none of them. The control proves the canaries are
in what was raised, so the check cannot pass on an exception that said nothing.
"""

from __future__ import annotations

import importlib
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

HOST = "db.internal.acme.io:5432"
EMAIL = "alice@example.com"
PASSWORD = "hunter2-canary"
CANARIES = (HOST, EMAIL, PASSWORD)
MESSAGE = f"could not connect to {HOST} as {EMAIL} with password {PASSWORD}"


def _gateway():
    # Imported when a test runs, never at collection (see
    # test_a_thinking_model_has_room_to_answer.py for why).
    return importlib.import_module("src.gateway.main")


class _FailingModel:
    """An AsyncOpenAI stand-in whose every request raises."""

    def __init__(self, exc: Exception):
        async def create(**_kwargs):
            raise exc

        self.chat = SimpleNamespace(completions=SimpleNamespace(create=create))
        self.completions = SimpleNamespace(create=create)


@pytest.fixture
def gateway(monkeypatch):
    main = _gateway()
    monkeypatch.setattr(main, "USE_MOCK", False)
    monkeypatch.setattr(main, "require_llm", lambda *a, **k: None)
    monkeypatch.setattr(main, "default_client", _FailingModel(RuntimeError(MESSAGE)))
    return main


def _raising(exc: Exception):
    async def guarded(*_a, **_k):
        raise exc

    return guarded


def _diagnose(main):
    return TestClient(main.app).post(
        "/v1/diagnose/pipeline",
        json={"pipeline_id": "p1", "evidence": {"runtime": {"status": "failed"}}},
    )


def _complete(main):
    return TestClient(main.app).post(
        "/v1/completion",
        json={"prompt_name": "chat/help_response", "variables": {"user_message": "why is it slow?"}},
    )


def _assert_names_nothing(text: str):
    assert not [c for c in CANARIES if c in text], text


def test_control_the_exception_carries_every_canary():
    assert all(c in MESSAGE for c in CANARIES)


def test_a_failed_diagnosis_does_not_echo_the_provider_error(gateway):
    response = _diagnose(gateway)
    assert response.status_code == 502
    assert response.json()["detail"] == "The diagnosis LLM call failed. See the llm-service log."
    _assert_names_nothing(response.text)


def test_a_missing_diagnosis_prompt_does_not_echo_its_path(gateway, monkeypatch):
    monkeypatch.setattr(gateway, "guarded_acall", _raising(FileNotFoundError(MESSAGE)))
    response = _diagnose(gateway)
    assert response.status_code == 500
    _assert_names_nothing(response.text)


def test_a_refused_diagnosis_is_a_503_that_names_no_value(gateway, monkeypatch):
    from src.egress import EgressRefused

    monkeypatch.setattr(gateway, "guarded_acall", _raising(EgressRefused(MESSAGE, "gateway.diagnose")))
    response = _diagnose(gateway)
    assert response.status_code == 503
    _assert_names_nothing(response.text)


def test_a_failed_completion_does_not_echo_the_provider_error(gateway):
    response = _complete(gateway)
    assert response.status_code == 500
    assert response.json()["detail"] == "The LLM call failed. See the llm-service log."
    _assert_names_nothing(response.text)


def test_a_refused_completion_is_a_422_that_names_the_prompt_and_no_value(gateway, monkeypatch):
    from src.egress import EgressRefused

    monkeypatch.setattr(gateway, "guarded_acall", _raising(EgressRefused(MESSAGE, "gateway.completion")))
    response = _complete(gateway)
    assert response.status_code == 422
    assert "chat/help_response" in response.json()["detail"]
    _assert_names_nothing(response.text)


def test_a_failed_hitl_interpretation_does_not_echo_the_error(gateway, monkeypatch):
    from src.agents.dag_planner import hitl_interpreter

    def boom(**_kwargs):
        raise RuntimeError(MESSAGE)

    monkeypatch.setattr(hitl_interpreter, "interpret_node_input", boom)
    response = TestClient(gateway.app).post(
        "/v1/hitl/interpret",
        json={"node_id": "n1", "node_kind": "source", "hitl_prompt": "Which tables?", "user_response": "orders"},
    )
    assert response.status_code == 200
    assert response.json()["success"] is False
    _assert_names_nothing(response.text)


def test_the_hitl_interpreter_does_not_return_the_llm_error():
    from src.agents.dag_planner.hitl_interpreter import HITLInterpreter

    class _Client:
        def complete(self, *_a, **_k):
            raise RuntimeError(MESSAGE)

    result = HITLInterpreter(_Client())._llm_interpretation(
        node_id="n1", node_kind="source", hitl_prompt="Which tables?", user_response="orders"
    )
    assert result.success is False
    _assert_names_nothing(str(result.error))


def test_model_sql_that_misuses_a_placeholder_is_dropped():
    from src.egress.sql_placeholders import LiteralMap

    main = _gateway()
    literals = LiteralMap()
    literals.add("str", "'paid'")
    response = SimpleNamespace(placeholders=literals)

    assert main._restore_literals("SELECT id FROM orders WHERE status = {{L1:str}}", response, "postgres") == (
        "SELECT id FROM orders WHERE status = 'paid'"
    )
    # A placeholder the prompt never held: the model invented it.
    assert main._restore_literals("SELECT id FROM orders WHERE status = {{L9:str}}", response, "postgres") == ""


# --- planner service -----------------------------------------------------------
#
# The same class one service over: the planner's CDC-config and /plan routes
# answered 500 with `detail=str(e)`. /generate-cdc-config is handed a source's
# host, user and password (connection_config), so its failure is the likeliest
# of all to name one.


class _FailingCDCGenerator:
    def generate_config(self, **_kwargs):
        raise RuntimeError(MESSAGE)

    def get_supported_databases(self):
        raise RuntimeError(MESSAGE)

    def validate_config(self, _config):
        raise RuntimeError(MESSAGE)


@pytest.fixture
def planner(monkeypatch):
    service = importlib.import_module("src.agents.planner.service")
    monkeypatch.setattr(service, "get_cdc_generator", lambda: _FailingCDCGenerator())

    def boom(*_a, **_k):
        raise RuntimeError(MESSAGE)

    monkeypatch.setattr(service, "detect_tools_from_text", boom)
    return TestClient(service.app)


PLANNER_FAILURES = [
    (
        "POST",
        "/generate-cdc-config",
        {"source_type": "postgresql", "connection_config": {"host": "h"}, "tables": ["orders"]},
        "CDC config generation failed. See the planner log.",
    ),
    ("GET", "/cdc/supported-databases", None, "Listing the CDC databases failed. See the planner log."),
    ("POST", "/cdc/validate-config", {"connector.class": "x"}, "CDC config validation failed. See the planner log."),
    ("POST", "/plan", {"natural_language": "copy orders to bigquery"}, "Planning failed. See the planner log."),
]


@pytest.mark.parametrize("method,path,body,detail", PLANNER_FAILURES, ids=[p for _, p, _, _ in PLANNER_FAILURES])
def test_a_failed_planner_route_does_not_echo_the_error(planner, method, path, body, detail):
    response = planner.request(method, path, json=body)
    assert response.status_code == 500
    assert response.json()["detail"] == detail
    _assert_names_nothing(response.text)


# The planner's other routes answer 200 with {"success": false, "error": ...}
# instead of a 500, and carried str(e) the same way; and the JIT connector
# generator handed str(e) back to its caller. Swept with the routes above.


@pytest.fixture
def planner_intelligence(monkeypatch):
    service = importlib.import_module("src.agents.planner.service")

    def boom(*_a, **_k):
        raise RuntimeError(MESSAGE)

    for name in ("get_strategy_decider", "get_cdc_provider_registry", "get_topic_provisioner"):
        monkeypatch.setattr(service, name, boom)
    return TestClient(service.app)


PLANNER_SOFT_FAILURES = [
    ("POST", "/strategy/recommend", {"sync_mode": "cdc", "source_type": "mysql", "data_stats": {"tables": []}}),
    ("GET", "/intelligence/cdc-providers", None),
    ("GET", "/intelligence/topic-calculator", None),
    ("GET", "/intelligence/status", None),
]


@pytest.mark.parametrize("method,path,body", PLANNER_SOFT_FAILURES, ids=[p for _, p, _ in PLANNER_SOFT_FAILURES])
def test_a_failed_planner_intelligence_route_does_not_echo_the_error(planner_intelligence, method, path, body):
    response = planner_intelligence.request(method, path, json=body)
    assert response.json()["success"] is False
    assert response.json()["error"].endswith("See the planner log.")
    _assert_names_nothing(response.text)


def test_a_failed_jit_generation_does_not_echo_the_error(monkeypatch):
    service = importlib.import_module("src.agents.planner.service")

    def boom(*_a, **_k):
        raise RuntimeError(MESSAGE)

    monkeypatch.setattr(service.requests, "post", boom)
    ok, message = service.generate_connector_sync("acme")
    assert ok is False
    assert message == "Connector generation failed. See the planner log."
    _assert_names_nothing(message)
