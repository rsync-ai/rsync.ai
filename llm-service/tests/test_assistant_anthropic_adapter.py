"""The Claude entry's adapter: OpenAI-shaped in and out, Anthropic Messages API on the wire.

The guard only knows ``client.chat.completions.create`` and
``choices[0].message``, so a translation slip here is invisible to it: a tool
result sent as the wrong block type, or a tool call read back without its id,
breaks the assistant's loop on the next step rather than this one. Each test
drives the real client through an ``httpx.MockTransport`` and reads the request
the provider would have received.
"""

from __future__ import annotations

import json

import httpx
import pytest

from src.egress.anthropic_http import THINKING_HEADROOM, AnthropicChatClient, AnthropicError, to_messages_request

KEY = "unit-test-anthropic-key-0123456789"


def _client(handler):
    seen: list[httpx.Request] = []

    def record(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return handler(request)

    return AnthropicChatClient(KEY, transport=httpx.MockTransport(record)), seen


def _answer(content, stop_reason="end_turn", usage=None):
    body = {
        "id": "msg_1",
        "model": "claude-sonnet-5-5",
        "content": content,
        "stop_reason": stop_reason,
        "usage": usage or {"input_tokens": 10, "output_tokens": 5},
    }
    return lambda request: httpx.Response(200, json=body)


TOOL = {"type": "function", "function": {"name": "get_pipeline", "description": "One pipeline.", "parameters": {"type": "object", "properties": {"id": {"type": "string"}}}}}


def test_a_tool_conversation_is_sent_as_messages_api_blocks():
    client, seen = _client(_answer([{"type": "text", "text": "It failed on auth."}]))
    client.chat.completions.create(
        model="claude-sonnet-5-5",
        messages=[
            {"role": "system", "content": "Answer from tools."},
            {"role": "user", "content": "why did it fail?"},
            {
                "role": "assistant",
                "content": None,
                "tool_calls": [{"id": "toolu_1", "type": "function", "function": {"name": "get_pipeline", "arguments": '{"id": "p-1"}'}}],
            },
            {"role": "tool", "tool_call_id": "toolu_1", "content": '{"status": "failed"}'},
        ],
        tools=[TOOL],
        tool_choice="auto",
        temperature=0.2,
        max_tokens=512,
    )
    (request,) = seen
    assert request.url == "https://api.anthropic.com/v1/messages"
    assert request.headers["x-api-key"] == KEY
    assert request.headers["anthropic-version"] == "2023-06-01"
    body = json.loads(request.content)
    assert body["system"] == "Answer from tools."
    # Sonnet 5.5 refuses a sampling value and thinks inside max_tokens (see below).
    assert body["max_tokens"] == 512 + THINKING_HEADROOM and "temperature" not in body
    assert body["messages"] == [
        {"role": "user", "content": [{"type": "text", "text": "why did it fail?"}]},
        {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "get_pipeline", "input": {"id": "p-1"}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": '{"status": "failed"}'}]},
    ]
    assert body["tools"] == [
        {"name": "get_pipeline", "description": "One pipeline.", "input_schema": {"type": "object", "properties": {"id": {"type": "string"}}}}
    ]
    assert body["tool_choice"] == {"type": "auto"}


def test_a_tool_use_answer_reads_back_as_an_openai_tool_call():
    client, _ = _client(
        _answer(
            [
                {"type": "thinking", "thinking": "not part of the answer"},
                {"type": "text", "text": "Let me look."},
                {"type": "tool_use", "id": "toolu_2", "name": "get_pipeline", "input": {"id": "p-1"}},
            ],
            stop_reason="tool_use",
            usage={"input_tokens": 7, "cache_read_input_tokens": 3, "output_tokens": 4},
        )
    )
    resp = client.chat.completions.create(model="claude-sonnet-5-5", messages=[{"role": "user", "content": "hi"}], tools=[TOOL])
    choice = resp.choices[0]
    assert choice.finish_reason == "tool_calls"
    assert choice.message.content == "Let me look."
    (call,) = choice.message.tool_calls
    assert (call.id, call.function.name, json.loads(call.function.arguments)) == ("toolu_2", "get_pipeline", {"id": "p-1"})
    # Cached input tokens are still input tokens on the bill.
    assert (resp.usage.prompt_tokens, resp.usage.completion_tokens) == (10, 4)


def test_tool_results_from_one_turn_share_one_user_message():
    _, convo = to_messages_request(
        [
            {"role": "user", "content": "compare"},
            {
                "role": "assistant",
                "content": None,
                "tool_calls": [
                    {"id": "a", "type": "function", "function": {"name": "x", "arguments": "{}"}},
                    {"id": "b", "type": "function", "function": {"name": "x", "arguments": "{}"}},
                ],
            },
            {"role": "tool", "tool_call_id": "a", "content": "1"},
            {"role": "tool", "tool_call_id": "b", "content": "2"},
        ]
    )
    assert [m["role"] for m in convo] == ["user", "assistant", "user"]
    assert [b["tool_use_id"] for b in convo[2]["content"]] == ["a", "b"]


@pytest.mark.parametrize(
    "choice, expected",
    [("required", {"type": "any"}), ("none", {"type": "none"}), ({"type": "function", "function": {"name": "get_pipeline"}}, {"type": "tool", "name": "get_pipeline"})],
)
def test_tool_choice_is_translated(choice, expected):
    client, seen = _client(_answer([{"type": "text", "text": "ok"}]))
    client.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}], tools=[TOOL], tool_choice=choice)
    assert json.loads(seen[0].content)["tool_choice"] == expected


def test_a_parameter_the_adapter_does_not_translate_is_refused_not_dropped():
    client, seen = _client(_answer([{"type": "text", "text": "ok"}]))
    with pytest.raises(ValueError, match="response_format"):
        client.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}], response_format={"type": "json_object"})
    assert seen == []


def test_an_error_carries_its_status_and_type_and_never_the_key():
    client, _ = _client(
        lambda request: httpx.Response(401, json={"type": "error", "error": {"type": "authentication_error", "message": "invalid x-api-key"}})
    )
    with pytest.raises(AnthropicError) as exc:
        client.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}])
    assert exc.value.status_code == 401 and exc.value.kind == "authentication_error"
    assert KEY not in str(exc.value)


def test_no_key_is_refused_before_any_request():
    with pytest.raises(ValueError):
        AnthropicChatClient("")


# Claude Sonnet 5.5, the Claude entry's default model, answers a non-default
# temperature or top_p with a 400, and so does every Claude model after Sonnet
# 4.6, Opus 4.6 and Haiku 4.5. The assistant prompt sets temperature 0.2, so the
# Claude entry failed every call on its default model, the admin Test included.
@pytest.mark.parametrize("model", ["claude-sonnet-5-5", "claude-opus-5-5", "claude-opus-4-7", "claude-fable-5-1", "claude-sonnet-6"])
def test_a_model_that_rejects_sampling_values_is_sent_none(model):
    client, seen = _client(_answer([{"type": "text", "text": "ok"}]))
    client.chat.completions.create(model=model, messages=[{"role": "user", "content": "hi"}], temperature=0.2, top_p=0.9)
    body = json.loads(seen[0].content)
    assert "temperature" not in body and "top_p" not in body


@pytest.mark.parametrize(
    "model",
    ["claude-sonnet-4-6", "claude-opus-4-6", "claude-sonnet-4-5-20250929", "claude-haiku-4-5-20251001", "claude-sonnet-4-20250514", "claude-3-5-sonnet-latest"],
)
def test_an_older_model_still_gets_its_sampling_values(model):
    client, seen = _client(_answer([{"type": "text", "text": "ok"}]))
    client.chat.completions.create(model=model, messages=[{"role": "user", "content": "hi"}], temperature=0.2, max_tokens=512)
    body = json.loads(seen[0].content)
    assert body["temperature"] == 0.2 and body["max_tokens"] == 512


# These models think before they answer unless told not to, and max_tokens
# covers the thinking as well as the answer, so the caller's answer length
# alone can be spent before a word of the answer is written.
@pytest.mark.parametrize("model", ["claude-sonnet-5-5", "claude-opus-5-5"])
def test_a_model_that_thinks_by_default_gets_room_to_think(model):
    client, seen = _client(_answer([{"type": "text", "text": "ok"}]))
    client.chat.completions.create(model=model, messages=[{"role": "user", "content": "hi"}], max_tokens=16)
    assert json.loads(seen[0].content)["max_tokens"] == 16 + THINKING_HEADROOM
