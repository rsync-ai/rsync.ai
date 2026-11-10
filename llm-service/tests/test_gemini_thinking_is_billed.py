"""Gemini's thinking is billed as output, so rsync.ai counts it.

Google's OpenAI-compatible endpoints leave a Gemini model's thinking out of
usage.completion_tokens and report it apart, as
completion_tokens_details.reasoning_tokens; Google bills it at the output rate.
OpenAI counts a reasoning model's thinking inside completion_tokens. Measured:

  - Vertex AI, 2026-10-10, Ask's prompt and 36 tools on google/gemini-2.5-flash:
    a tool call reported completion=3 reasoning=43, an answer 7 and 49
  - AI Studio, 2026-09-26 on app.rsync.ai: gemini-3.6-flash replies that its
    thinking cut off at a 300-token cap recorded 7-12 completion tokens
    (test_a_thinking_model_has_room_to_answer.py)

Every place that priced a reply took completion_tokens alone, so the tool call
above was priced as 3 output tokens of its 46. The tests here drive the metrics
wrapper and the tool generator with that usage and expect 46; the Ask step and
/v1/completion have theirs in test_assistant_step_route.py and
test_a_thinking_model_has_room_to_answer.py. The controls show OpenAI's shape
is not counted twice.
"""

from __future__ import annotations

import asyncio
import importlib
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

repo_root = str(Path(__file__).parent.parent)
if repo_root not in sys.path:
    sys.path.insert(0, repo_root)

from src.utils.llm_cost import billable_tokens

VERTEX_MODEL = "google/gemini-2.5-flash"
VERTEX_BASE = "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/endpoints/openapi/"


def _gemini_usage():
    """What Vertex AI reported for Ask's tool-call step on 2026-10-10."""
    return SimpleNamespace(
        prompt_tokens=4025,
        completion_tokens=3,
        total_tokens=4028,
        completion_tokens_details=SimpleNamespace(reasoning_tokens=43),
    )


def _openai_reasoning_usage():
    """OpenAI's shape: the 43 thinking tokens are already inside completion_tokens."""
    return SimpleNamespace(
        prompt_tokens=4025,
        completion_tokens=46,
        total_tokens=4071,
        completion_tokens_details=SimpleNamespace(reasoning_tokens=43),
    )


def _client(usage):
    """An AsyncOpenAI stand-in that answers every call with ``usage``."""

    class _Completions:
        async def create(self, **kwargs):
            message = SimpleNamespace(content="ok", tool_calls=None)
            return SimpleNamespace(choices=[SimpleNamespace(message=message, finish_reason="stop")], usage=usage)

    return SimpleNamespace(base_url=VERTEX_BASE, chat=SimpleNamespace(completions=_Completions()))


def _record_llm_calls(monkeypatch) -> list[dict]:
    # Imported when a test runs, never at collection: test_explorer_endpoints.py
    # drops every src.* module while it is collected, and metrics registered
    # twice fail its collection.
    metrics = importlib.import_module("src.utils.metrics")
    calls: list[dict] = []
    monkeypatch.setattr(metrics, "record_llm_call", lambda **kw: calls.append(kw))
    return calls


# ---------------------------------------------------------------------------
# The helper every pricing site goes through.


@pytest.mark.parametrize("model", [VERTEX_MODEL, "google/gemini-2.5-pro", "gemini-2.5-flash", "Gemini-3.6-Flash"])
def test_a_gemini_reply_bills_its_thinking_as_output(model):
    assert billable_tokens(_gemini_usage(), model) == (4025, 46)


@pytest.mark.parametrize("model", ["o4-mini", "gpt-4o-mini", "claude-sonnet-5-5"])
def test_a_reply_that_counts_its_thinking_is_not_counted_twice(model):
    assert billable_tokens(_openai_reasoning_usage(), model) == (4025, 46)


def test_a_reply_without_thinking_or_usage_bills_what_it_reports():
    assert billable_tokens(None, VERTEX_MODEL) == (0, 0)
    assert billable_tokens(SimpleNamespace(prompt_tokens=10, completion_tokens=5), VERTEX_MODEL) == (10, 5)
    no_details = SimpleNamespace(prompt_tokens=10, completion_tokens=5, completion_tokens_details=None)
    assert billable_tokens(no_details, VERTEX_MODEL) == (10, 5)
    nulls = SimpleNamespace(
        prompt_tokens=None, completion_tokens=5, completion_tokens_details=SimpleNamespace(reasoning_tokens=None)
    )
    assert billable_tokens(nulls, VERTEX_MODEL) == (0, 5)


# ---------------------------------------------------------------------------
# The sites.


@pytest.mark.parametrize("usage, model", [(_gemini_usage(), VERTEX_MODEL), (_openai_reasoning_usage(), "o4-mini")])
def test_the_metrics_wrapper_counts_the_thinking_once(monkeypatch, usage, model):
    calls = _record_llm_calls(monkeypatch)
    metrics = importlib.import_module("src.utils.metrics")
    client = metrics.instrument_openai_client(_client(usage), service="test", provider="openai")

    asyncio.run(client.chat.completions.create(model=model, messages=[]))

    assert [(c["model"], c["prompt_tokens"], c["completion_tokens"]) for c in calls] == [(model, 4025, 46)]


def test_the_tool_generator_counts_the_thinking_in_its_metric_and_its_budget(monkeypatch):
    calls = _record_llm_calls(monkeypatch)
    base = importlib.import_module("src.agents.tool_generator.agents.base")

    class _Agent(base.BaseAgent):
        async def execute(self, **kwargs):
            raise NotImplementedError

    agent = _Agent("test")
    agent._llm_provider = "openai"
    agent._llm_client = _client(_gemini_usage())
    # An operator price of $1 per 1K output tokens, nothing for input.
    agent.COST_PER_1K_INPUT_TOKENS_USD = 0.0
    agent.COST_PER_1K_OUTPUT_TOKENS_USD = 1.0

    assert asyncio.run(agent.call_llm_async("Build a connector.", "The API docs.", model=VERTEX_MODEL)) == "ok"

    assert [(c["prompt_tokens"], c["completion_tokens"]) for c in calls] == [(4025, 46)]
    assert agent._total_cost_usd == pytest.approx(0.046)
