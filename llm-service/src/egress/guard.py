"""The one door to an LLM provider.

Every model call in llm-service goes through :func:`guarded_call` or
:func:`guarded_acall`; ``tests/test_egress_boundary.py`` fails the build on a
provider call anywhere else. A call names its component (src/egress/call_sites.py)
and either a prompt, which the guard renders field by field, or -- for the few
components whose inputs are not about a customer's systems -- its own messages,
or, for a tool-using component, both: the rendered prompt, then the conversation.
The request then passes the fixed stages (src/egress/stages.py), and a record
of where it went is logged. The record never includes the prompt.
"""

from __future__ import annotations

import json
import logging
import time
from dataclasses import dataclass, field
from typing import Any, Optional

from .call_sites import REFUSED_PROMPTS, call_site
from .context import EgressContext, build_context, client_base_url
from .fields import FieldRefused
from .model_params import fit_params
from .render import render_for_egress
from .sql_placeholders import LiteralMap
from .stages import EgressRefused, EgressRequest, normalize_messages, run_stages, text_of

__all__ = ["EgressResponse", "guarded_acall", "guarded_call", "sent_chars"]

logger = logging.getLogger("rsync.llm_egress")

# A "tools" component's conversation: the system message is the rendered prompt's.
_CONVERSATION_ROLES = frozenset({"user", "assistant", "tool"})

# The message roles sent_chars counts; a request's other part is its tools.
_SENT_ROLES = ("system", "user", "assistant", "tool")

# The legacy /v1/completions endpoint takes no tools and no response format.
_COMPLETION_PARAMS = frozenset({"temperature", "max_tokens", "top_p", "stop", "seed", "timeout"})

_default_registry = None


def _registry(registry):
    global _default_registry
    if registry is not None:
        return registry
    if _default_registry is None:
        from src.utils.prompt_registry import PromptRegistry

        _default_registry = PromptRegistry(prompts_dir="prompts")
    return _default_registry


@dataclass
class EgressResponse:
    """The provider's response, plus what the caller needs to use it.

    ``text`` is the answer whichever endpoint produced it. ``placeholders``
    holds the SQL literals taken out of the prompt; pass it to
    ``restore_sql_literals`` with the SQL the model returned. Any other
    attribute (``choices``, ``usage``) reads through to the raw response.
    """

    raw: Any
    text: str
    placeholders: LiteralMap = field(default_factory=LiteralMap)
    used_completion_fallback: bool = False
    context: Optional[EgressContext] = None
    # What the data-marker stage did (src/egress/sample_rows.py), counts only.
    data: dict = field(default_factory=dict)
    # How many characters of each part the provider was sent: see sent_chars.
    sent_chars: dict = field(default_factory=dict)

    def __getattr__(self, name: str) -> Any:
        if name == "raw":
            raise AttributeError(name)
        return getattr(self.raw, name)


@dataclass
class _Prepared:
    ctx: EgressContext
    req: EgressRequest
    placeholders: LiteralMap
    base_url: str
    mode: str


def _prepare(
    client,
    *,
    component: str,
    model: str,
    messages,
    prompt: Optional[str],
    variables: Optional[dict],
    dialect: Optional[str],
    registry,
    mode: str,
    append_text: Optional[str],
    actor: Optional[str],
    workspace_id: str,
    request_id: Optional[str],
    params: dict,
) -> _Prepared:
    site = call_site(component)
    if site is None:
        raise EgressRefused("component is not in the call-site census", component)
    if mode not in ("chat", "completion"):
        raise EgressRefused(f"unknown mode {mode!r}", component)
    if client is None:
        raise EgressRefused("no LLM client", component)
    if not model or not isinstance(model, str):
        raise EgressRefused("no model named", component)

    placeholders = LiteralMap()
    if site.path == "tools":
        if prompt is None or messages is None:
            raise EgressRefused("this component sends a tagged prompt and its conversation", component)
        if prompt in REFUSED_PROMPTS or prompt not in site.prompts:
            raise EgressRefused(f"prompt {prompt!r} is not allowed for this component", component)
        try:
            built, placeholders = render_for_egress(_registry(registry), prompt, variables, dialect)
        except FieldRefused as exc:
            raise EgressRefused(str(exc), component) from exc
        convo = normalize_messages(messages)
        if any(m.get("role") not in _CONVERSATION_ROLES for m in convo):
            raise EgressRefused("a conversation holds only user, assistant and tool messages", component)
        built = built + convo
    elif prompt is not None:
        if messages is not None:
            raise EgressRefused("pass a prompt or messages, not both", component)
        if prompt in REFUSED_PROMPTS or prompt not in site.prompts:
            raise EgressRefused(f"prompt {prompt!r} is not allowed for this component", component)
        try:
            built, placeholders = render_for_egress(_registry(registry), prompt, variables, dialect)
        except FieldRefused as exc:
            raise EgressRefused(str(exc), component) from exc
    else:
        if site.path != "messages":
            raise EgressRefused("this component must send a tagged prompt", component)
        if variables is not None:
            raise EgressRefused("variables need a prompt", component)
        built = normalize_messages(messages)

    if mode == "completion":
        built = [{"role": "user", "content": text_of(EgressRequest(built, model)) + (append_text or "")}]
    elif append_text:
        raise EgressRefused("append_text is for completion mode", component)

    ctx = build_context(
        client,
        component=component,
        model=model,
        actor=actor,
        workspace_id=workspace_id,
        request_id=request_id,
    )
    # Fitted to the model before the stages, so they check what is sent.
    req = EgressRequest(messages=built, model=model, params=fit_params(model, params))
    base_url = client_base_url(client)
    run_stages(ctx, req)
    return _Prepared(ctx, req, placeholders, base_url, mode)


def sent_chars(req: EgressRequest) -> dict:
    """How many characters of each part of a request the provider is sent.

    ``tools`` is the tool list as compact JSON; the rest are message text by
    role, with a model turn's tool calls counted as each call's name and
    arguments. Characters, not tokens: a provider counts tokens, and only for
    the whole request, so this is what splits its count into the fixed prompt
    (system and tools) and the conversation. Counts only, never the text.
    """
    out = {"system": 0, "tools": 0, "user": 0, "assistant": 0, "tool": 0}
    for m in req.messages:
        role = m.get("role")
        if role not in _SENT_ROLES:
            continue
        content = m.get("content")
        if isinstance(content, str):
            n = len(content)
        elif isinstance(content, list):
            n = sum(len(p.get("text") or "") for p in content if isinstance(p, dict))
        else:
            n = 0
        for call in m.get("tool_calls") or []:
            fn = (call.get("function") or {}) if isinstance(call, dict) else {}
            n += len(fn.get("name") or "") + len(fn.get("arguments") or "")
        out[role] += n
    tools = req.params.get("tools")
    if tools:
        # default=str: a count must never fail a call the provider already answered.
        out["tools"] = len(json.dumps(tools, separators=(",", ":"), default=str))
    return out


def _check_route(prep: _Prepared, client) -> None:
    if client_base_url(client) != prep.base_url:
        raise EgressRefused("the client's endpoint changed", prep.ctx.component)


def _completion_params(params: dict) -> dict:
    return {k: v for k, v in params.items() if k in _COMPLETION_PARAMS}


def _chat_text(raw) -> str:
    try:
        return raw.choices[0].message.content or ""
    except (AttributeError, IndexError, TypeError):
        return ""


def _completion_text(raw) -> str:
    try:
        return getattr(raw.choices[0], "text", "") or ""
    except (AttributeError, IndexError, TypeError):
        return ""


def _data_fields(prep: Optional[_Prepared]) -> dict:
    """The data stage's counts for the log line; never a value."""
    data = prep.req.data if prep else {}
    if not data:
        return {}
    return {
        "data": "sample_rows",
        "data_sent": data.get("sent", 0),
        "data_withheld_columns": data.get("withheld", 0),
        "data_blocked": data.get("blocked", 0),
        "data_turns_withheld": data.get("turns_withheld", 0),
    }


def _log(prep: Optional[_Prepared], component: str, outcome: str, started: float, **extra) -> None:
    ctx = prep.ctx if prep else None
    extra = {**extra, **_data_fields(prep)}
    logger.info(
        "llm_egress component=%s outcome=%s provider=%s provider_class=%s host=%s model=%s "
        "actor=%s workspace=%s request_id=%s chars=%s ms=%d%s",
        component,
        outcome,
        ctx.provider if ctx else "-",
        ctx.provider_class if ctx else "-",
        ctx.endpoint_host if ctx else "-",
        ctx.model if ctx else "-",
        ctx.actor if ctx else "-",
        (ctx.workspace_id or "-") if ctx else "-",
        ctx.request_id if ctx else "-",
        sum(len(m.get("content") or "") for m in prep.req.messages if isinstance(m.get("content"), str)) if prep else 0,
        int((time.monotonic() - started) * 1000),
        "".join(f" {k}={v}" for k, v in extra.items()),
    )


def guarded_call(
    client,
    *,
    component: str,
    model: str,
    messages=None,
    prompt: Optional[str] = None,
    variables: Optional[dict] = None,
    dialect: Optional[str] = None,
    registry=None,
    mode: str = "chat",
    completion_fallback: bool = False,
    append_text: Optional[str] = None,
    actor: Optional[str] = None,
    workspace_id: str = "",
    request_id: Optional[str] = None,
    **params,
) -> EgressResponse:
    """Send one request through the guard with a synchronous client."""
    started = time.monotonic()
    prep = None
    try:
        prep = _prepare(
            client, component=component, model=model, messages=messages, prompt=prompt,
            variables=variables, dialect=dialect, registry=registry, mode=mode,
            append_text=append_text, actor=actor, workspace_id=workspace_id,
            request_id=request_id, params=params,
        )
        _check_route(prep, client)
        fallback = False
        if prep.mode == "completion":
            raw = client.completions.create(
                model=prep.req.model, prompt=prep.req.messages[0]["content"], **_completion_params(prep.req.params)
            )
            text = _completion_text(raw)
        else:
            raw = client.chat.completions.create(model=prep.req.model, messages=prep.req.messages, **prep.req.params)
            text = _chat_text(raw)
            if completion_fallback and not text.strip():
                raw = client.completions.create(
                    model=prep.req.model, prompt=text_of(prep.req), **_completion_params(prep.req.params)
                )
                text = _completion_text(raw)
                fallback = True
    except EgressRefused as exc:
        _log(prep, component, "refused", started, reason=exc.reason.replace(" ", "_")[:80])
        raise
    except Exception as exc:
        _log(prep, component, "error", started, error=type(exc).__name__)
        raise
    _log(prep, component, "sent", started, fallback=fallback)
    return EgressResponse(raw, text, prep.placeholders, fallback, prep.ctx, dict(prep.req.data), sent_chars(prep.req))


async def guarded_acall(
    client,
    *,
    component: str,
    model: str,
    messages=None,
    prompt: Optional[str] = None,
    variables: Optional[dict] = None,
    dialect: Optional[str] = None,
    registry=None,
    mode: str = "chat",
    completion_fallback: bool = False,
    append_text: Optional[str] = None,
    actor: Optional[str] = None,
    workspace_id: str = "",
    request_id: Optional[str] = None,
    **params,
) -> EgressResponse:
    """Send one request through the guard with an asynchronous client."""
    started = time.monotonic()
    prep = None
    try:
        prep = _prepare(
            client, component=component, model=model, messages=messages, prompt=prompt,
            variables=variables, dialect=dialect, registry=registry, mode=mode,
            append_text=append_text, actor=actor, workspace_id=workspace_id,
            request_id=request_id, params=params,
        )
        _check_route(prep, client)
        fallback = False
        if prep.mode == "completion":
            raw = await client.completions.create(
                model=prep.req.model, prompt=prep.req.messages[0]["content"], **_completion_params(prep.req.params)
            )
            text = _completion_text(raw)
        else:
            raw = await client.chat.completions.create(
                model=prep.req.model, messages=prep.req.messages, **prep.req.params
            )
            text = _chat_text(raw)
            if completion_fallback and not text.strip():
                raw = await client.completions.create(
                    model=prep.req.model, prompt=text_of(prep.req), **_completion_params(prep.req.params)
                )
                text = _completion_text(raw)
                fallback = True
    except EgressRefused as exc:
        _log(prep, component, "refused", started, reason=exc.reason.replace(" ", "_")[:80])
        raise
    except Exception as exc:
        _log(prep, component, "error", started, error=type(exc).__name__)
        raise
    _log(prep, component, "sent", started, fallback=fallback)
    return EgressResponse(raw, text, prep.placeholders, fallback, prep.ctx, dict(prep.req.data), sent_chars(prep.req))
