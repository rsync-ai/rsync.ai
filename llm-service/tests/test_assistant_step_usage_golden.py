"""What one assistant step reports about its own size: tokens and characters.

``scripts/assistant_eval.py`` measures a prompt change from these counts, and
api-gateway's loop sums them per question. The keys are pinned in
shared/assistant_step_usage_golden.json, which api-gateway's
internal/assistant/usage_test.go reads too: a key renamed on one side reads as
0 on the other, and each side's own tests stay green.
"""

from __future__ import annotations

import json
import os
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.egress import guard
from src.egress.assistant_models import ResolvedModel
from src.egress.stages import EgressRequest
from src.gateway import assistant_routes

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
with open(os.path.join(REPO_ROOT, "shared", "assistant_step_usage_golden.json")) as f:
    GOLDEN = json.load(f)

SECRET = "unit-test-internal-secret"
QUESTION = "why did it fail?"
TOOLS = [{"name": "get_pipeline", "description": "One pipeline.", "input_schema": {"type": "object"}}]


def _ns(value):
    """The provider's usage block as SDK objects: attributes, not keys."""
    if isinstance(value, dict):
        return SimpleNamespace(**{k: _ns(v) for k, v in value.items()})
    return value


class _Provider:
    base_url = "https://api.openai.com/v1/"

    def __init__(self, usage):
        self.calls: list[dict] = []
        self.chat = SimpleNamespace(completions=self)
        self._usage = usage

    def create(self, **kwargs):
        self.calls.append(kwargs)
        message = SimpleNamespace(content="It failed on auth.", tool_calls=None)
        return SimpleNamespace(choices=[SimpleNamespace(message=message, finish_reason="stop")], usage=self._usage)


@pytest.fixture
def step(monkeypatch):
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    monkeypatch.setattr("src.utils.llm_cost.record_usage", lambda **kw: 1)
    app = FastAPI()
    app.include_router(assistant_routes.router)
    client = TestClient(app)

    def run(usage, messages=None):
        provider = _Provider(usage)
        monkeypatch.setattr(
            assistant_routes,
            "resolve_model",
            lambda entry, *, key="", model="": ResolvedModel(entry, "ChatGPT", provider, "gpt-4o-mini", "env"),
        )
        body = {
            "entry": "chatgpt",
            "user_id": "u-1",
            "messages": messages or [{"role": "user", "content": QUESTION}],
            "tools": TOOLS,
        }
        r = client.post("/v1/assistant/step", json=body, headers={"X-Internal-Secret": SECRET})
        assert r.status_code == 200, r.text
        return r.json(), provider.calls[0]

    return run


def test_the_step_reports_the_golden_usage_keys(step):
    out, _ = step(_ns(GOLDEN["provider_usage"]))
    assert out["usage"] == GOLDEN["step_usage"]
    assert set(out["chars"]) == set(GOLDEN["step_chars"])


def test_a_provider_that_reports_no_cache_gives_zero_not_an_error(step):
    # Anthropic's adapter and most compatible servers send only the two totals.
    out, _ = step(SimpleNamespace(prompt_tokens=120, completion_tokens=30))
    assert out["usage"] == {"prompt_tokens": 120, "cached_tokens": 0, "completion_tokens": 30, "reasoning_tokens": 0}


def test_chars_split_the_request_the_provider_was_sent(step):
    messages = [
        {"role": "user", "content": QUESTION},
        {"role": "assistant", "tool_calls": [{"id": "call_1", "name": "get_pipeline", "arguments": '{"id": "p-1"}'}]},
        {"role": "tool", "tool_call_id": "call_1", "content": '{"status": "failed"}'},
    ]
    out, sent = step(_ns(GOLDEN["provider_usage"]), messages)
    chars = out["chars"]
    system = sent["messages"][0]
    assert system["role"] == "system"
    assert chars["system"] == len(system["content"]) > 1000
    assert chars["tools"] == len(json.dumps(sent["tools"], separators=(",", ":"))) > 0
    assert chars["user"] == len(QUESTION)
    assert chars["assistant"] == len("get_pipeline") + len('{"id": "p-1"}')
    assert chars["tool"] == len('{"status": "failed"}')


def test_sent_chars_counts_text_parts_and_never_fails_on_odd_tools():
    req = EgressRequest(
        messages=[
            {"role": "system", "content": "abc"},
            {"role": "user", "content": [{"type": "text", "text": "hello"}, {"type": "image_url"}]},
            {"role": "assistant", "content": None},
        ],
        model="m",
        params={"tools": [{"function": {"name": "t", "parameters": object()}}]},
    )
    chars = guard.sent_chars(req)
    assert (chars["system"], chars["user"], chars["assistant"], chars["tool"]) == (3, 5, 0, 0)
    assert chars["tools"] > 0
