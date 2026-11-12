"""Which model the assistant answers with, and where its key comes from.

An admin's panel key wins over the environment; Remove falls back to it. The
ChatGPT entry goes to OpenAI's own API whatever OPENAI_BASE_URL says, so a key
typed for OpenAI never reaches a gateway the operator pointed the stack at. A
model id is a name, never a URL. And the list the admin panel reads never
carries a key. A stack on Vertex AI with the VM's token offers Gemini with no
key at all, through its own client and Vertex's model name.
"""

from __future__ import annotations

import json

import httpx2
import pytest

from src.egress import assistant_models as am
from src.egress.anthropic_http import AnthropicChatClient
from src.egress.context import THIRD_PARTY, describe_client
from src.utils import openai_client
from src.utils.gcp_access_token import MetadataAccessToken
from src.utils.llm_cost import compute_cost_cents

PANEL_KEY = "unit-test-panel-key-0123456789"
ENV_KEY = "unit-test-env-key-0123456789"

_ENV = (
    "ANTHROPIC_API_KEY",
    "GEMINI_API_KEY",
    "OPENAI_API_KEY",
    "OPENAI_BASE_URL",
    "OPENAI_API_KEY_SOURCE",
    "LLM_PROVIDER",
    "LLM_MODEL",
    "AZURE_OPENAI_ENDPOINT",
    "AZURE_OPENAI_API_KEY",
    "AZURE_OPENAI_DEPLOYMENT",
    "GROQ_API_KEY",
    "ASSISTANT_CLAUDE_MODEL",
    "ASSISTANT_OPENAI_MODEL",
    "ASSISTANT_GEMINI_MODEL",
)


@pytest.fixture(autouse=True)
def _clean_env(monkeypatch):
    for name in _ENV:
        monkeypatch.delenv(name, raising=False)
    am._cache.clear()
    monkeypatch.setattr(openai_client, "_GCP_TOKEN", None)
    yield
    am._cache.clear()


def test_with_no_key_anywhere_no_entry_is_offered():
    assert am.env_entries() == {}
    assert all(am.resolve_model(e) is None for e in am.ENTRIES)


def test_a_panel_key_wins_over_the_environment(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", ENV_KEY)
    env = am.resolve_model("claude")
    panel = am.resolve_model("claude", key=PANEL_KEY)
    assert (env.source, panel.source) == ("env", "panel")
    assert env.client is not panel.client
    assert isinstance(panel.client, AnthropicChatClient)
    assert panel.client._http.headers["x-api-key"] == PANEL_KEY
    assert env.client._http.headers["x-api-key"] == ENV_KEY


@pytest.mark.parametrize(
    "entry, host_provider",
    [("claude", "anthropic"), ("chatgpt", "openai"), ("gemini", "gemini")],
)
def test_each_panel_entry_reaches_its_own_provider(entry, host_provider):
    resolved = am.resolve_model(entry, key=PANEL_KEY)
    assert describe_client(resolved.client)[:2] == (host_provider, THIRD_PARTY)


def test_chatgpt_never_follows_a_custom_openai_base_url(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", ENV_KEY)
    monkeypatch.setenv("OPENAI_BASE_URL", "https://llm-gateway.internal.example/v1")
    # The stack's own setup points elsewhere: no ChatGPT entry from it ...
    assert "chatgpt" not in am.env_entries()
    assert am.resolve_model("chatgpt") is None
    # ... and a panel key still goes to OpenAI itself.
    resolved = am.resolve_model("chatgpt", key=PANEL_KEY)
    assert str(resolved.client.base_url).startswith("https://api.openai.com/v1")


def test_the_stacks_own_openai_setup_serves_chatgpt(monkeypatch):
    monkeypatch.setenv("OPENAI_API_KEY", ENV_KEY)
    entries = am.env_entries()
    assert entries["chatgpt"]["label"] == "ChatGPT"
    resolved = am.resolve_model("chatgpt")
    assert resolved.source == "env"
    assert describe_client(resolved.client)[0] == "openai"


def test_a_groq_stack_offers_no_chatgpt_entry(monkeypatch):
    monkeypatch.setenv("LLM_PROVIDER", "groq")
    monkeypatch.setenv("GROQ_API_KEY", ENV_KEY)
    assert "chatgpt" not in am.env_entries()


@pytest.mark.parametrize("bad", ["https://evil.example/v1", "a/b", "model id", "", "x" * 101, "-leading-dash"])
def test_a_model_id_that_is_not_a_name_is_ignored(bad):
    resolved = am.resolve_model("gemini", key=PANEL_KEY, model=bad)
    assert resolved.model == "gemini-2.5-flash"
    assert not am.valid_model_id(bad)


def test_a_valid_model_id_is_used_and_env_sets_the_default(monkeypatch):
    assert am.resolve_model("claude", key=PANEL_KEY, model="claude-haiku-4-5").model == "claude-haiku-4-5"
    monkeypatch.setenv("ASSISTANT_CLAUDE_MODEL", "claude-opus-5-5")
    assert am.resolve_model("claude", key=PANEL_KEY).model == "claude-opus-5-5"


def test_the_entry_list_never_carries_a_key(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", ENV_KEY)
    monkeypatch.setenv("GEMINI_API_KEY", ENV_KEY)
    monkeypatch.setenv("OPENAI_API_KEY", ENV_KEY)
    entries = am.env_entries()
    # Control: the environment really does offer all three, so the check below reads something.
    assert set(entries) == {"claude", "chatgpt", "gemini"}
    assert ENV_KEY not in json.dumps(entries)


def test_panel_defaults_name_a_model_for_every_entry(monkeypatch):
    assert am.panel_defaults() == {"claude": "claude-sonnet-5-5", "chatgpt": "gpt-4o-mini", "gemini": "gemini-2.5-flash"}
    monkeypatch.setenv("ASSISTANT_GEMINI_MODEL", "gemini-2.5-pro")
    monkeypatch.setenv("ASSISTANT_CLAUDE_MODEL", "https://evil.example")
    defaults = am.panel_defaults()
    assert (defaults["gemini"], defaults["claude"]) == ("gemini-2.5-pro", "claude-sonnet-5-5")


def test_an_unknown_entry_is_refused():
    with pytest.raises(am.UnknownEntry):
        am.resolve_model("copilot", key=PANEL_KEY)


def test_the_client_cache_is_bounded_and_keyed_by_key():
    first = am.resolve_model("gemini", key=PANEL_KEY).client
    assert am.resolve_model("gemini", key=PANEL_KEY).client is first
    for i in range(am._CACHE_SIZE + 2):
        am.resolve_model("gemini", key=f"{PANEL_KEY}-{i}")
    assert len(am._cache) == am._CACHE_SIZE
    # The raw key is never a cache key.
    assert all(PANEL_KEY not in k[1] for k in am._cache)


VERTEX_BASE = (
    "https://us-central1-aiplatform.googleapis.com/v1/projects/demo/locations/us-central1/endpoints/openapi"
)


def _vertex_stack(monkeypatch, base_url=VERTEX_BASE):
    """The stack's own OpenAI setup on Vertex AI, with a fake metadata server."""
    monkeypatch.setenv("LLM_PROVIDER", "openai")
    monkeypatch.setenv("OPENAI_API_KEY_SOURCE", "gcp-metadata")
    monkeypatch.setenv("OPENAI_BASE_URL", base_url)
    monkeypatch.setattr(
        openai_client, "_GCP_TOKEN", MetadataAccessToken(fetch=lambda: ("vm-token", 3599.0), clock=lambda: 0.0)
    )


def test_a_vertex_stack_offers_gemini_with_no_key(monkeypatch):
    _vertex_stack(monkeypatch)
    # Gemini, from the VM's token; no ChatGPT, since the OpenAI setup is not OpenAI.
    assert am.env_entries() == {"gemini": {"label": "Gemini (Vertex)", "model": "gemini-2.5-flash"}}
    resolved = am.resolve_model("gemini")
    assert (resolved.source, resolved.label, resolved.model) == ("env", "Gemini (Vertex)", "google/gemini-2.5-flash")
    assert describe_client(resolved.client) == ("openai", THIRD_PARTY, "us-central1-aiplatform.googleapis.com")

    sent = []

    def handler(request):
        sent.append((request.url.host, request.headers.get("authorization"), json.loads(request.content)["model"]))
        return httpx2.Response(
            200,
            json={
                "id": "c",
                "object": "chat.completion",
                "created": 0,
                "model": "google/gemini-2.5-flash",
                "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "ok"}}],
            },
        )

    client = resolved.client.copy(http_client=httpx2.Client(transport=httpx2.MockTransport(handler)))
    client.chat.completions.create(model=resolved.model, messages=[{"role": "user", "content": "hi"}])
    assert sent == [("us-central1-aiplatform.googleapis.com", "Bearer vm-token", "google/gemini-2.5-flash")]


def test_a_vertex_model_is_named_by_its_publisher_once(monkeypatch):
    _vertex_stack(monkeypatch)
    monkeypatch.setenv("ASSISTANT_GEMINI_MODEL", "gemini-2.5-pro")
    assert am.env_entries()["gemini"]["model"] == "gemini-2.5-pro"
    assert am.resolve_model("gemini").model == "google/gemini-2.5-pro"
    assert am.resolve_model("gemini", model="gemini-2.5-flash").model == "google/gemini-2.5-flash"
    # A model id never carries a slash, so the prefix cannot be doubled or replaced.
    assert am.resolve_model("gemini", model="google/gemini-2.5-flash").model == "google/gemini-2.5-pro"


def test_a_gemini_key_still_goes_to_ai_studio_on_a_vertex_stack(monkeypatch):
    _vertex_stack(monkeypatch)
    panel = am.resolve_model("gemini", key=PANEL_KEY)
    assert (describe_client(panel.client)[0], panel.model) == ("gemini", "gemini-2.5-flash")
    monkeypatch.setenv("GEMINI_API_KEY", ENV_KEY)
    assert am.env_entries()["gemini"]["label"] == "Gemini"
    env = am.resolve_model("gemini")
    assert (describe_client(env.client)[0], env.model) == ("gemini", "gemini-2.5-flash")


@pytest.mark.parametrize(
    "base_url",
    [
        # Google, but AI Studio: its OpenAI endpoint takes a key, not the VM's token.
        "https://generativelanguage.googleapis.com/v1beta/openai/",
        "http://us-central1-aiplatform.googleapis.com/v1",  # not https
        "https://aiplatform.googleapis.com.attacker.example/v1",
        "https://api.openai.com/v1",
    ],
)
def test_the_vm_token_makes_no_gemini_entry_off_vertex(monkeypatch, base_url):
    _vertex_stack(monkeypatch, base_url)
    assert "gemini" not in am.env_entries()
    assert am.resolve_model("gemini") is None


def test_a_vertex_answer_is_priced_as_the_gemini_model_it_names():
    # The call goes out as provider "openai" (describe_client), model "google/<model>".
    assert compute_cost_cents("openai", "google/gemini-2.5-flash", 1_000_000, 1_000_000) == 30 + 250
    assert compute_cost_cents("openai", "google/gemini-2.5-pro", 1_000_000, 0) == 125
    # Control: without the mapping it would be the unknown-model fallback.
    assert compute_cost_cents("openai", "gemini-2.5-flash", 1_000_000, 1_000_000) == 100 + 300
