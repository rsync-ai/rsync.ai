"""A model is sent only the request parameters it takes.

Every call site sets temperature and max_tokens from its prompt YAML, and the
model is whichever one an admin or the environment names: Admin -> Settings ->
AI models for Ask rsync.ai, LLM_MODEL and the per-feature models for the rest.
OpenAI's reasoning models -- the o-series and GPT-5 on -- answer a temperature
other than 1 with a 400 ("Only the default (1) value is supported") and take
max_completion_tokens in place of max_tokens. So Ask rsync.ai's ChatGPT entry
set to gpt-5, or LLM_MODEL=o3, failed every call it made. Their reasoning
counts inside max_completion_tokens, so the answer length the call site asked
for gets THINKING_HEADROOM on top, as a thinking Claude model does.

Bug class: **a request parameter sent to a model that rejects it.** The guard,
the one door every model call passes (tests/test_egress_boundary.py), now fits
each request to the model it goes to (src/egress/model_params.py), so no call
site can get it wrong. Claude's own rule stays in anthropic_http.py,
which every Claude call goes through.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.egress import guarded_acall, guarded_call
from src.egress.model_params import fit_params
from src.egress.anthropic_http import THINKING_HEADROOM
from src.egress.assistant_models import ResolvedModel
from src.egress.stages import ALLOWED_PARAMS
from src.gateway import assistant_routes

SECRET = "unit-test-internal-secret"
OPENAI = "https://api.openai.com/v1"
AZURE = "https://acme.openai.azure.com/openai/"

# What an OpenAI reasoning model is sent for each parameter the guard lets
# through (stages.ALLOWED_PARAMS). OpenAI's 400s name temperature and
# max_tokens; stop is refused on o3 and o4-mini; top_p is sampling, as
# temperature is. None of them is sent by a call site that needs it.
# max_tokens is sent as max_completion_tokens, THINKING_HEADROOM more.
REASONING = {
    "temperature": None,
    "top_p": None,
    "stop": None,
    "max_tokens": "max_completion_tokens",
    "max_completion_tokens": "max_completion_tokens",
    "response_format": "response_format",
    "seed": "seed",
    "tools": "tools",
    "tool_choice": "tool_choice",
    "timeout": "timeout",
}
REJECTED = {"temperature", "top_p", "stop", "max_tokens"}

# An Azure deployment may be named in capitals.
REASONING_MODELS = ["o1", "o3", "o3-mini", "o4-mini", "gpt-5", "gpt-5-mini", "gpt-5.1", "gpt-6", "GPT-5", "ft:o4-mini-2025-04-16:acme::x1"]
CHAT_MODELS = ["gpt-4o-mini", "gpt-4o", "gpt-4.1", "gpt-3.5-turbo", "ft:gpt-4o-mini-2024-07-18:acme::x1"]


class _BadRequest(Exception):
    status_code = 400


class _Provider:
    """A chat-completions endpoint. With ``rejects`` it answers as OpenAI's
    reasoning models do: a 400 for any parameter they do not take."""

    def __init__(self, base_url=OPENAI, rejects=False):
        self.base_url = base_url
        self.rejects = rejects
        self.calls: list[dict] = []
        self.chat = SimpleNamespace(completions=self)

    def _answer(self, kwargs):
        self.calls.append(kwargs)
        sent = sorted(REJECTED & set(kwargs))
        if self.rejects and sent:
            raise _BadRequest(f"Unsupported parameter: {sent[0]!r} is not supported with this model.")
        message = SimpleNamespace(content="ok", tool_calls=None)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=message, finish_reason="stop")],
            usage=SimpleNamespace(prompt_tokens=10, completion_tokens=2),
        )

    def create(self, **kwargs):
        return self._answer(kwargs)


class _AsyncProvider(_Provider):
    async def create(self, **kwargs):
        return self._answer(kwargs)


@pytest.fixture
def ask(monkeypatch):
    """POST /v1/assistant/step with the ChatGPT entry on ``model``, answered by ``provider``."""
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    monkeypatch.setattr("src.utils.llm_cost.record_usage", lambda **kw: 0)
    app = FastAPI()
    app.include_router(assistant_routes.router)
    client = TestClient(app)

    def step(provider, model):
        monkeypatch.setattr(
            assistant_routes,
            "resolve_model",
            lambda entry, **kw: ResolvedModel(entry, "ChatGPT", provider, model, "panel"),
        )
        body = {
            "entry": "chatgpt",
            "user_id": "u-1",
            "model": model,
            "messages": [{"role": "user", "content": "why did it fail?"}],
            "tools": [{"name": "get_pipeline", "description": "One pipeline.", "input_schema": {"type": "object"}}],
        }
        return client.post("/v1/assistant/step", json=body, headers={"X-Internal-Secret": SECRET})

    return step


@pytest.mark.parametrize("model", REASONING_MODELS)
def test_ask_on_a_reasoning_model_answers(ask, model):
    provider = _Provider(rejects=True)
    r = ask(provider, model)
    assert r.status_code == 200, r.text
    (sent,) = provider.calls
    assert not REJECTED & set(sent), sorted(sent)
    # The prompt's 1024 still bounds the answer, with room to reason on top.
    assert sent["max_completion_tokens"] == 1024 + THINKING_HEADROOM
    assert sent["tools"][0]["function"]["name"] == "get_pipeline" and sent["tool_choice"] == "auto"


@pytest.mark.parametrize("model", CHAT_MODELS)
def test_ask_on_a_chat_model_is_sent_its_temperature(ask, model):
    provider = _Provider()
    r = ask(provider, model)
    assert r.status_code == 200, r.text
    (sent,) = provider.calls
    assert (sent["temperature"], sent["max_tokens"]) == (0.2, 1024)
    assert "max_completion_tokens" not in sent


_EVERY = {
    "temperature": 0.0,
    "top_p": 0.9,
    "stop": ["\n\n"],
    "max_tokens": 300,
    "response_format": {"type": "json_object"},
    "seed": 7,
    "timeout": 30,
}


def _call(door, provider, model, **params):
    kwargs = dict(component="logo_downloader", model=model, messages=[{"role": "user", "content": "acme"}], **params)
    if door == "async":
        return asyncio.run(guarded_acall(provider, **kwargs))
    return guarded_call(provider, **kwargs)


@pytest.mark.parametrize("door", ["sync", "async"])
@pytest.mark.parametrize("base_url", [OPENAI, AZURE], ids=["openai", "azure"])
def test_each_door_fits_a_reasoning_models_request(door, base_url):
    provider = (_AsyncProvider if door == "async" else _Provider)(base_url, rejects=True)
    _call(door, provider, "o3-mini", **_EVERY)
    (sent,) = provider.calls
    want = {REASONING[k]: v for k, v in _EVERY.items() if REASONING[k]}
    want["max_completion_tokens"] = _EVERY["max_tokens"] + THINKING_HEADROOM
    assert {k: v for k, v in sent.items() if k not in ("model", "messages")} == want


def test_the_callers_parameters_are_not_changed():
    params = dict(_EVERY)
    fitted = fit_params("o3-mini", params)
    assert params == _EVERY and fitted is not params


def test_a_max_completion_tokens_the_caller_set_wins():
    provider = _Provider(rejects=True)
    _call("sync", provider, "gpt-5", max_tokens=300, max_completion_tokens=5000)
    assert provider.calls[0]["max_completion_tokens"] == 5000 and "max_tokens" not in provider.calls[0]


@pytest.mark.parametrize(
    "base_url, model",
    [
        (OPENAI, "gpt-4o-mini"),
        (AZURE, "my-gpt-4o-deployment"),
        ("https://api.anthropic.com/v1/", "claude-opus-5"),  # anthropic_http.py fits Claude
        ("https://generativelanguage.googleapis.com/v1beta/openai/", "gemini-2.5-flash"),
        ("https://api.groq.com/openai/v1", "openai/gpt-oss-120b"),
        ("http://ollama.internal:11434/v1", "gpt-oss:20b"),
        ("http://ollama.internal:11434/v1", "marco-o1"),  # "o1" not at the start
        ("http://vllm.internal:8000/v1", "meta-llama/Llama-3.1-8B-Instruct"),
    ],
    ids=["openai-chat", "azure-chat-deployment", "anthropic", "gemini", "groq", "ollama", "ollama-marco-o1", "openai-compatible"],
)
def test_every_other_model_is_sent_its_parameters_as_set(monkeypatch, base_url, model):
    monkeypatch.setenv("OLLAMA_BASE_URL", "http://ollama.internal:11434")
    provider = _Provider(base_url)
    _call("sync", provider, model, **_EVERY)
    (sent,) = provider.calls
    assert {k: v for k, v in sent.items() if k not in ("model", "messages")} == _EVERY


def test_every_parameter_the_guard_allows_is_decided_for_a_reasoning_model():
    # A parameter added to stages.ALLOWED_PARAMS needs a line in REASONING: is
    # it sent to a reasoning model as is, renamed, or dropped?
    assert set(REASONING) == set(ALLOWED_PARAMS)
