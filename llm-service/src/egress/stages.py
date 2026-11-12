"""The fixed checks every outbound LLM request passes, in order.

1. Data markers (src/egress/sample_rows.py): a message the caller marked as
   holding sample rows is sent only while the install, the workspace and the
   destination allow them, and only what Presidio finds no personal data in; every marker
   is removed. A marker the call site does not allow refuses the call.
2. Secret scrub over every piece of text the provider would receive.
3. Size cap: an oversized prompt is refused, never truncated.
4. Added stages (:func:`register_stage`). The public build registers none.
5. Secret scrub and size cap again, over whatever the added stages left.
6. Routing check: the model is the one the caller named, every request
   parameter is on :data:`ALLOWED_PARAMS`, and no data marker is left.

An added stage may only tighten. It can redact, drop or refuse; the re-run of
2-3 and the checks in 6 undo or refuse anything that loosens. A stage that
raises refuses the call: the guard fails closed.
"""

from __future__ import annotations

import os
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any, Callable

from src.utils.masking import scrub_secrets_for_llm

from . import sample_rows
from .call_sites import call_site
from .context import EgressContext

__all__ = [
    "ALLOWED_PARAMS",
    "EgressRefused",
    "EgressRequest",
    "max_chars",
    "normalize_messages",
    "register_stage",
    "run_stages",
    "text_of",
    "unregister_stage",
]

# Request parameters a call may set. ``extra_body``, ``extra_headers``,
# ``extra_query`` and ``base_url`` are absent on purpose: each can carry text
# or a destination past every check here.
ALLOWED_PARAMS = frozenset(
    {
        "temperature",
        "max_tokens",
        "max_completion_tokens",
        "response_format",
        "top_p",
        "stop",
        "seed",
        "tools",
        "tool_choice",
        "timeout",
    }
)

_DEFAULT_MAX_CHARS = 400_000


class EgressRefused(Exception):
    """The guard would not send this request. Nothing reached the provider."""

    def __init__(self, reason: str, component: str = ""):
        super().__init__(f"LLM request refused ({component or 'unknown'}): {reason}")
        self.reason = reason
        self.component = component


@dataclass
class EgressRequest:
    """What will be sent. Stages may edit ``messages`` in place.

    ``data`` is what the data-marker stage did, counts only; empty when no
    message carried a marker.
    """

    messages: list[dict]
    model: str
    params: dict = field(default_factory=dict)
    data: dict = field(default_factory=dict)


Stage = Callable[[EgressContext, EgressRequest], None]
_STAGES: "OrderedDict[str, Stage]" = OrderedDict()


def register_stage(name: str, fn: Stage) -> None:
    """Add a stage that runs after the built-in scrub and cap.

    It receives the frozen context and the mutable request. It may only make
    the request smaller or stricter; raising refuses the call.
    """
    if not name or not callable(fn):
        raise ValueError("a stage needs a name and a callable")
    _STAGES[name] = fn


def unregister_stage(name: str) -> None:
    _STAGES.pop(name, None)


def max_chars() -> int:
    raw = (os.getenv("LLM_EGRESS_MAX_CHARS") or "").strip()
    try:
        value = int(raw) if raw else _DEFAULT_MAX_CHARS
    except ValueError:
        value = _DEFAULT_MAX_CHARS
    return value if value > 0 else _DEFAULT_MAX_CHARS


def _texts(message: dict):
    """Yield ``(container, key)`` for every text the provider would read."""
    content = message.get("content")
    if isinstance(content, str):
        yield message, "content"
    elif isinstance(content, list):
        for part in content:
            if isinstance(part, dict) and isinstance(part.get("text"), str):
                yield part, "text"
    for call in message.get("tool_calls") or []:
        fn = call.get("function") if isinstance(call, dict) else None
        if isinstance(fn, dict) and isinstance(fn.get("arguments"), str):
            yield fn, "arguments"


def _check_shape(ctx: EgressContext, req: EgressRequest) -> None:
    if not isinstance(req.messages, list) or not req.messages:
        raise EgressRefused("no messages", ctx.component)
    for m in req.messages:
        if not isinstance(m, dict) or not isinstance(m.get("role"), str):
            raise EgressRefused("a message is not a role/content object", ctx.component)
        content = m.get("content")
        if content is not None and not isinstance(content, (str, list)):
            raise EgressRefused("message content is neither text nor parts", ctx.component)
        if isinstance(content, list):
            for part in content:
                # Text parts only: an image or file part is data by definition.
                if not isinstance(part, dict) or part.get("type", "text") != "text":
                    raise EgressRefused("a message part is not text", ctx.component)


def _scrub(req: EgressRequest) -> None:
    for m in req.messages:
        for container, key in list(_texts(m)):
            container[key] = scrub_secrets_for_llm(container[key])


def _cap(ctx: EgressContext, req: EgressRequest) -> None:
    total = sum(len(c[k]) for m in req.messages for c, k in _texts(m))
    limit = max_chars()
    if total > limit:
        raise EgressRefused(f"prompt is {total} characters, over the {limit} limit", ctx.component)


def _check_params(ctx: EgressContext, params: dict) -> None:
    extra = sorted(set(params) - ALLOWED_PARAMS)
    if extra:
        raise EgressRefused(f"request parameter(s) not allowed: {', '.join(extra)}", ctx.component)


def _data(ctx: EgressContext, req: EgressRequest) -> None:
    site = call_site(ctx.component)
    try:
        req.data = sample_rows.apply(ctx, req.messages, site.data_classes if site else ())
    except sample_rows.DataRefused as exc:
        raise EgressRefused(str(exc), ctx.component) from exc


def _check_no_markers(ctx: EgressContext, req: EgressRequest) -> None:
    if any(sample_rows.DATA_KEY in m for m in req.messages):
        raise EgressRefused("a data marker was left on a message", ctx.component)


def run_stages(ctx: EgressContext, req: EgressRequest) -> None:
    """Run every stage over ``req`` in place. Raises :class:`EgressRefused`."""
    model = req.model
    _check_params(ctx, req.params)
    _check_shape(ctx, req)
    _data(ctx, req)
    _scrub(req)
    _cap(ctx, req)
    for name, stage in list(_STAGES.items()):
        try:
            stage(ctx, req)
        except EgressRefused:
            raise
        except Exception as exc:
            raise EgressRefused(f"stage {name!r} failed: {type(exc).__name__}", ctx.component) from exc
    _check_shape(ctx, req)
    _scrub(req)
    _cap(ctx, req)
    if req.model != model:
        raise EgressRefused("a stage changed the model", ctx.component)
    _check_params(ctx, req.params)
    _check_no_markers(ctx, req)


def text_of(req: EgressRequest) -> str:
    """The request as one completion prompt (Ollama's /v1/completions path)."""
    return "\n\n".join(m["content"] for m in req.messages if isinstance(m.get("content"), str) and m["content"])


def normalize_messages(messages: Any) -> list[dict]:
    """Plain dicts, whatever the caller built them from.

    The tool loop appends the SDK's own message objects; the checks here read
    dicts, and a copy means a stage can never edit the caller's list.
    """
    out: list[dict] = []
    for m in messages or []:
        if hasattr(m, "model_dump"):
            m = m.model_dump(exclude_none=True)
        if not isinstance(m, dict):
            raise EgressRefused("a message is not a role/content object")
        out.append(_deep_copy(m))
    return out


def _deep_copy(value: Any) -> Any:
    if isinstance(value, dict):
        return {k: _deep_copy(v) for k, v in value.items()}
    if isinstance(value, list):
        return [_deep_copy(v) for v in value]
    return value
