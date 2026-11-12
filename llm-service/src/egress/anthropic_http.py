"""A small Anthropic Messages API client shaped like the OpenAI SDK's chat client.

The guard sends every request through ``client.chat.completions.create`` and
reads the answer off ``choices[0].message``. This adapter lets the assistant's
Claude entry go through that same door: it turns the OpenAI-shaped request
into a Messages API request, and the response back into the OpenAI shape.
It speaks plain HTTP so llm-service does not take on another SDK; it lives in
src/egress because it is a provider call (tests/test_egress_boundary.py).

Only what the assistant sends is translated: system, user, assistant and tool
messages, function tools, and ``tool_choice``. Anything else is refused, so a
parameter the adapter does not understand is never silently dropped.

Two settings follow the model rather than the caller (``_takes_sampling``).
Every Claude model after Sonnet 4.6, Opus 4.6 and Haiku 4.5 answers a
non-default ``temperature`` or ``top_p`` with a 400, so those go only to a
model that takes them. And from Opus 5 and Sonnet 5 on they think before
they answer, the thinking counted inside ``max_tokens``, so every newer model
gets ``THINKING_HEADROOM`` on top of the answer length the caller asked for.
"""

from __future__ import annotations

import json
import re
from types import SimpleNamespace
from typing import Any, Optional

import httpx

from src.utils.masking import scrub_error_for_llm

__all__ = ["ANTHROPIC_BASE_URL", "AnthropicChatClient", "AnthropicError"]

ANTHROPIC_BASE_URL = "https://api.anthropic.com/v1/"
_API_VERSION = "2023-06-01"
_DEFAULT_MAX_TOKENS = 1024
# Room to think, added to the answer length. A ceiling, not a spend: an answer
# that needs little thinking uses little of it.
THINKING_HEADROOM = 4096
# The models that take temperature and top_p and think only when asked:
# Claude 3, and Opus, Sonnet and Haiku 4 through 4.6, dated or not. A model id
# this does not know is treated as a later one, which never fails on sampling.
_SAMPLING_MODEL = re.compile(r"^claude-(3-|(opus|sonnet|haiku)-4(-[0-6])?(-\d{8})?$)")
_SUPPORTED = frozenset(
    {"tools", "tool_choice", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "timeout"}
)
_FINISH = {"end_turn": "stop", "stop_sequence": "stop", "tool_use": "tool_calls", "max_tokens": "length", "refusal": "content_filter"}


class AnthropicError(Exception):
    """The Messages API answered with an error. Carries its status and type, never the request.

    ``detail`` is the provider's own message, scrubbed and cut: on a 400 it is
    the only place that says what was refused.
    """

    def __init__(self, status: int, kind: str, message: str):
        super().__init__(f"anthropic {status} {kind}: {message}")
        self.status_code = status
        self.kind = kind
        self.detail = scrub_error_for_llm(message, 200)


def _takes_sampling(model: str) -> bool:
    return bool(_SAMPLING_MODEL.match(model or ""))


def _text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(p.get("text", "") for p in content if isinstance(p, dict))
    return ""


def _tool_input(arguments: Any) -> dict:
    if isinstance(arguments, dict):
        return arguments
    try:
        value = json.loads(arguments or "{}")
    except (TypeError, ValueError):
        return {}
    return value if isinstance(value, dict) else {}


def to_messages_request(messages: list[dict]) -> tuple[str, list[dict]]:
    """Split OpenAI-shaped messages into Anthropic's ``system`` and ``messages``."""
    system: list[str] = []
    out: list[dict] = []

    def add(role: str, blocks: list[dict]) -> None:
        if not blocks:
            return
        if out and out[-1]["role"] == role:
            out[-1]["content"].extend(blocks)
        else:
            out.append({"role": role, "content": list(blocks)})

    for m in messages:
        role = m.get("role")
        if role == "system":
            text = _text(m.get("content"))
            if text:
                system.append(text)
        elif role == "user":
            text = _text(m.get("content"))
            add("user", [{"type": "text", "text": text}] if text else [])
        elif role == "assistant":
            blocks: list[dict] = []
            text = _text(m.get("content"))
            if text:
                blocks.append({"type": "text", "text": text})
            for call in m.get("tool_calls") or []:
                fn = call.get("function") or {}
                blocks.append(
                    {"type": "tool_use", "id": call.get("id"), "name": fn.get("name"), "input": _tool_input(fn.get("arguments"))}
                )
            add("assistant", blocks)
        elif role == "tool":
            add(
                "user",
                [{"type": "tool_result", "tool_use_id": m.get("tool_call_id"), "content": _text(m.get("content"))}],
            )
        else:
            raise ValueError(f"unsupported message role {role!r}")
    return "\n\n".join(system), out


def _tools(tools: list[dict]) -> list[dict]:
    out = []
    for t in tools or []:
        fn = t.get("function") if t.get("type") == "function" else None
        if not isinstance(fn, dict):
            raise ValueError("only function tools are supported")
        out.append(
            {
                "name": fn.get("name"),
                "description": fn.get("description", ""),
                "input_schema": fn.get("parameters") or {"type": "object", "properties": {}},
            }
        )
    return out


def _tool_choice(choice: Any) -> Optional[dict]:
    if choice is None or choice == "auto":
        return {"type": "auto"}
    if choice == "none":
        return {"type": "none"}
    if choice == "required":
        return {"type": "any"}
    if isinstance(choice, dict) and isinstance(choice.get("function"), dict):
        return {"type": "tool", "name": choice["function"].get("name")}
    raise ValueError("unsupported tool_choice")


def to_openai_response(body: dict) -> SimpleNamespace:
    texts, calls = [], []
    for block in body.get("content") or []:
        kind = block.get("type")
        if kind == "text":
            texts.append(block.get("text", ""))
        elif kind == "tool_use":
            calls.append(
                SimpleNamespace(
                    id=block.get("id"),
                    type="function",
                    function=SimpleNamespace(name=block.get("name"), arguments=json.dumps(block.get("input") or {})),
                )
            )
        # thinking and other blocks are not part of the answer
    usage = body.get("usage") or {}
    prompt_tokens = sum(int(usage.get(k) or 0) for k in ("input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"))
    completion_tokens = int(usage.get("output_tokens") or 0)
    message = SimpleNamespace(role="assistant", content="".join(texts) or None, tool_calls=calls or None)
    return SimpleNamespace(
        id=body.get("id"),
        model=body.get("model"),
        choices=[SimpleNamespace(index=0, message=message, finish_reason=_FINISH.get(body.get("stop_reason"), "stop"))],
        usage=SimpleNamespace(
            prompt_tokens=prompt_tokens, completion_tokens=completion_tokens, total_tokens=prompt_tokens + completion_tokens
        ),
    )


class _Completions:
    def __init__(self, owner: "AnthropicChatClient"):
        self._owner = owner

    def create(self, *, model: str, messages: list[dict], **params) -> SimpleNamespace:
        return self._owner._create(model, messages, params)


class AnthropicChatClient:
    """``client.chat.completions.create(...)`` against the Anthropic Messages API."""

    def __init__(self, api_key: str, *, timeout: float = 40.0, transport: Optional[httpx.BaseTransport] = None):
        if not api_key:
            raise ValueError("no Anthropic API key")
        self.base_url = ANTHROPIC_BASE_URL
        self._timeout = timeout
        self._http = httpx.Client(
            base_url=ANTHROPIC_BASE_URL,
            headers={"x-api-key": api_key, "anthropic-version": _API_VERSION, "content-type": "application/json"},
            timeout=timeout,
            transport=transport,
        )
        self.chat = SimpleNamespace(completions=_Completions(self))

    def _create(self, model: str, messages: list[dict], params: dict) -> SimpleNamespace:
        unknown = sorted(set(params) - _SUPPORTED)
        if unknown:
            raise ValueError(f"parameter(s) the Anthropic adapter does not translate: {', '.join(unknown)}")
        system, convo = to_messages_request(messages)
        sampling = _takes_sampling(model)
        max_tokens = int(params.get("max_tokens") or params.get("max_completion_tokens") or _DEFAULT_MAX_TOKENS)
        body: dict[str, Any] = {
            "model": model,
            "messages": convo,
            "max_tokens": max_tokens if sampling else max_tokens + THINKING_HEADROOM,
        }
        if system:
            body["system"] = system
        if params.get("tools"):
            body["tools"] = _tools(params["tools"])
            body["tool_choice"] = _tool_choice(params.get("tool_choice"))
        if sampling and params.get("temperature") is not None:
            body["temperature"] = params["temperature"]
        if sampling and params.get("top_p") is not None:
            body["top_p"] = params["top_p"]
        if params.get("stop"):
            stop = params["stop"]
            body["stop_sequences"] = [stop] if isinstance(stop, str) else list(stop)
        resp = self._http.post("messages", json=body, timeout=params.get("timeout") or self._timeout)
        if resp.status_code >= 400:
            try:
                err = resp.json().get("error") or {}
            except ValueError:
                err = {}
            raise AnthropicError(resp.status_code, str(err.get("type") or "error"), str(err.get("message") or "")[:300])
        return to_openai_response(resp.json())
