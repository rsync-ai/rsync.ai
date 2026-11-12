"""The in-app assistant's model step: one guarded model call with tools.

api-gateway runs the assistant's loop (api-gateway/internal/assistant): it
keeps the conversation, runs the tools the model asks for through the agent
gateway as the signed-in user, and calls ``POST /v1/assistant/step`` once per
model turn. This service only renders the prompt, sends the turn through the
egress guard as the ``assistant.step`` component, and returns what the model
said or which tools it asked for.

Both routes are service-to-service (``X-Internal-Secret``). A key an admin set
in the panel arrives in the body, decrypted by api-gateway; it is used to build
the client and is never logged or returned. Nothing here holds ENCRYPTION_KEY.
"""

from __future__ import annotations

import logging
import re
import time
from typing import Literal, Optional

from fastapi import APIRouter, Depends
from fastapi.responses import JSONResponse
from pydantic import BaseModel, ConfigDict, Field

from src.agents.tool_generator.deployment.routes import require_internal_secret
from src.egress import EgressRefused, guarded_call
from src.egress.anthropic_http import AnthropicError
from src.egress.assistant_models import ENTRIES, UnknownEntry, env_entries, panel_defaults, resolve_model
from src.utils.prompt_registry import PromptRegistry
from src.utils.rate_limit_waits import RateLimitWaits

logger = logging.getLogger(__name__)

router = APIRouter(prefix="/v1/assistant", tags=["assistant"], dependencies=[Depends(require_internal_secret)])

PROMPT = "assistant/step"
_registry = PromptRegistry(prompts_dir="prompts")

_ID = r"^[A-Za-z0-9_-]{1,128}$"
_TOOL_NAME = r"^[a-z][a-z0-9_]{0,63}$"
SAMPLE_ROWS_TOOL = "sample_rows"
# Gemini's thought signature: base64 Google put on a tool call, which it checks
# when the call comes back on the next turn. Opaque, and sent to Google only.
_SIGNATURE = re.compile(r"^[A-Za-z0-9+/=_-]+$")
_SIGNATURE_MAX = 60_000
# Google's documented value for a call Gemini did not make, such as the guide
# turn api-gateway puts first: Gemini 3 refuses a turn whose first call has none.
GEMINI_PLACEHOLDER_SIGNATURE = "skip_thought_signature_validator"


class StepTool(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(pattern=_TOOL_NAME)
    description: str = Field(default="", max_length=2000)
    input_schema: dict = Field(default_factory=lambda: {"type": "object", "properties": {}})


class StepToolCall(BaseModel):
    model_config = ConfigDict(extra="forbid")
    id: str = Field(pattern=_ID)
    name: str = Field(pattern=_TOOL_NAME)
    arguments: str = Field(default="{}", max_length=4000)
    signature: Optional[str] = Field(default=None, max_length=_SIGNATURE_MAX, pattern=_SIGNATURE.pattern)


class StepMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")
    role: Literal["user", "assistant", "tool"]
    content: Optional[str] = Field(default=None, max_length=60_000)
    tool_calls: Optional[list[StepToolCall]] = Field(default=None, max_length=12)
    tool_call_id: Optional[str] = Field(default=None, pattern=_ID)
    # "sample_rows" on a message that may hold row values: a sample_rows
    # result, or a turn the model wrote after one. The egress guard checks the
    # sample-rows switches again before it sends one (src/egress/sample_rows.py).
    data: Optional[Literal["sample_rows"]] = None


class StepRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    entry: Literal["claude", "chatgpt", "gemini"]
    # Set only when an admin saved a key in the panel; else the env key is used.
    api_key: Optional[str] = Field(default=None, max_length=512, repr=False)
    model: Optional[str] = Field(default=None, max_length=100)
    user_id: Optional[str] = Field(default=None, max_length=64)
    page_kind: Optional[Literal["pipeline", "connection", "run", "dashboard"]] = None
    page_id: Optional[str] = Field(default=None, pattern=_ID)
    messages: list[StepMessage] = Field(min_length=1, max_length=80)
    # Every agent gateway tool plus sample_rows and generate_sql; api-gateway's
    # assistant/toolcap_test.go fails when that list outgrows this cap, which
    # would refuse every question.
    tools: list[StepTool] = Field(default_factory=list, max_length=64)
    # "none" on the loop's last step: the model must answer from what it has.
    tool_choice: Literal["auto", "none"] = "auto"
    max_tokens: Optional[int] = Field(default=None, ge=1, le=4096)


def _refuse(status: int, error: str, reason: str) -> JSONResponse:
    return JSONResponse(status_code=status, content={"error": error, "reason": reason})


def _provider_call(c: StepToolCall, *, first: bool, gemini: bool) -> dict:
    call: dict = {"id": c.id, "type": "function", "function": {"name": c.name, "arguments": c.arguments}}
    # Gemini checks the first call of each turn: its own signature goes back,
    # or the placeholder on a call it did not make. Later parallel calls carry
    # none unless Gemini signed them. No other provider has a signature.
    signature = c.signature or (GEMINI_PLACEHOLDER_SIGNATURE if first else None)
    if gemini and signature:
        call["extra_content"] = {"google": {"thought_signature": signature}}
    return call


def _returned_signature(call) -> Optional[str]:
    """The thought signature on a tool call Gemini returned, else None."""
    extra = getattr(call, "extra_content", None)
    google = extra.get("google") if isinstance(extra, dict) else None
    signature = google.get("thought_signature") if isinstance(google, dict) else None
    if not isinstance(signature, str) or not signature:
        return None
    if len(signature) > _SIGNATURE_MAX or not _SIGNATURE.match(signature):
        # The next step would refuse it; unsigned, the placeholder stands in.
        logger.warning("assistant step dropped a thought signature it cannot send back: chars=%d", len(signature))
        return None
    return signature


def _conversation_problem(req: StepRequest) -> Optional[str]:
    """Why the conversation is not one a tool loop could have produced, or None."""
    names = {t.name for t in req.tools}
    asked: dict[str, str] = {}
    for m in req.messages:
        if m.role == "assistant":
            for call in m.tool_calls or []:
                if call.name not in names:
                    return f"the model called {call.name!r}, which is not in the tool list"
                asked[call.id] = call.name
        elif m.tool_calls:
            return "only an assistant message calls tools"
        if m.role == "tool":
            if not m.tool_call_id or m.tool_call_id not in asked:
                return "a tool result answers no tool call"
            if m.content is None:
                return "a tool result has no content"
            sample = asked[m.tool_call_id] == SAMPLE_ROWS_TOOL
            if sample and m.data != "sample_rows":
                return "a sample_rows result is not marked as data"
            if m.data and not sample:
                return "only a sample_rows result is marked as data"
        elif m.tool_call_id:
            return "only a tool message answers a tool call"
        if m.role == "user" and m.data:
            return "the user's message is not marked as data"
    if req.messages[0].role != "user":
        return "a conversation starts with the user's question"
    return None


# Never the provider's own message: some echo part of the key they rejected.
_PROVIDER_REASONS = {
    401: "the model provider rejected the key",
    403: "the model provider rejected the key",
    404: "the model provider does not know this model",
    429: "the model provider is rate limiting this key",
}


@router.get("/providers")
def assistant_providers() -> dict:
    """Which entries this service's environment can serve, and the model a panel
    key gets when the admin names none. Never a key."""
    return {"entries": env_entries(), "known": list(ENTRIES), "panel_defaults": panel_defaults()}


@router.post("/step")
def assistant_step(req: StepRequest):
    problem = _conversation_problem(req)
    if problem:
        return _refuse(422, "bad_conversation", problem)
    try:
        resolved = resolve_model(req.entry, key=(req.api_key or "").strip(), model=(req.model or "").strip())
    except UnknownEntry:
        return _refuse(422, "unknown_model", "unknown assistant model")
    if resolved is None:
        return _refuse(409, "model_not_configured", f"{req.entry} has no key")

    params = dict(_registry.get_config(PROMPT).get("parameters") or {})
    if req.max_tokens:
        params["max_tokens"] = req.max_tokens
    if req.tools:
        params["tools"] = [
            {"type": "function", "function": {"name": t.name, "description": t.description, "parameters": t.input_schema}}
            for t in req.tools
        ]
        params["tool_choice"] = req.tool_choice
    messages = []
    for m in req.messages:
        msg: dict = {"role": m.role, "content": m.content}
        if m.tool_calls:
            msg["tool_calls"] = [
                _provider_call(c, first=i == 0, gemini=req.entry == "gemini") for i, c in enumerate(m.tool_calls)
            ]
        if m.tool_call_id:
            msg["tool_call_id"] = m.tool_call_id
        if m.data:
            msg["rsync_data"] = m.data
        messages.append(msg)
    variables = {k: v for k, v in (("page_kind", req.page_kind), ("page_id", req.page_id)) if v}
    if any(t.name == SAMPLE_ROWS_TOOL for t in req.tools):
        # The prompt then tells the model how sample rows may be used.
        variables["data_mode"] = "sample_rows"

    # A 429 is waited out: at most 25 seconds asleep a step, inside the question's 120.
    waits = RateLimitWaits()
    while True:
        try:
            resp = guarded_call(
                resolved.client,
                component="assistant.step",
                model=resolved.model,
                prompt=PROMPT,
                variables=variables,
                messages=messages,
                actor=f"user:{req.user_id}" if req.user_id else None,
                registry=_registry,
                **params,
            )
        except EgressRefused as exc:
            return _refuse(422, "refused", exc.reason)
        except Exception as exc:
            wait = waits.next(exc)
            if wait is not None:
                logger.info("assistant step rate limited: entry=%s retry=%d wait=%.1fs", req.entry, waits.retries, wait)
                time.sleep(wait)
                continue
            status = getattr(exc, "status_code", None)
            # Anthropic's 400 names what it refused; its key is in a header, never
            # in that text. Other providers' text, and every other status's, stays out.
            detail = exc.detail if isinstance(exc, AnthropicError) and status == 400 else ""
            logger.warning(
                "assistant step failed: entry=%s error=%s status=%s kind=%s detail=%s retries=%d",
                req.entry, type(exc).__name__, status, getattr(exc, "kind", "-"), detail or "-", waits.retries,
            )
            reason = _PROVIDER_REASONS.get(status or 0, "the model provider returned an error")
            if detail:
                reason = f"the model provider refused the request: {detail}"
            return _refuse(502, "provider_error", reason)
        break

    choice = resp.choices[0]
    message = choice.message
    calls = []
    for c in getattr(message, "tool_calls", None) or []:
        call = {"id": c.id, "name": c.function.name, "arguments": c.function.arguments or "{}"}
        signature = _returned_signature(c)
        if signature:
            call["signature"] = signature
        calls.append(call)
    usage = getattr(resp, "usage", None)
    prompt_tokens = int(getattr(usage, "prompt_tokens", 0) or 0)
    completion_tokens = int(getattr(usage, "completion_tokens", 0) or 0)
    # OpenAI's shape; a provider that reports neither gives 0. Anthropic's adapter
    # folds its cache reads into prompt_tokens (src/egress/anthropic_http.py).
    cached_tokens = int(getattr(getattr(usage, "prompt_tokens_details", None), "cached_tokens", 0) or 0)
    reasoning_tokens = int(getattr(getattr(usage, "completion_tokens_details", None), "reasoning_tokens", 0) or 0)
    cost_cents = 0
    try:
        from src.utils.llm_cost import billable_tokens, record_usage

        # Charged as billed, a Gemini model's thinking as output; the usage
        # below stays what the provider reported.
        billed_prompt, billed_completion = billable_tokens(usage, resolved.model)
        cost_cents = record_usage(
            user_id=req.user_id,
            pipeline_id=None,
            prompt_name=PROMPT,
            provider=resp.context.provider if resp.context else req.entry,
            model=resolved.model,
            prompt_tokens=billed_prompt,
            completion_tokens=billed_completion,
        )
    except Exception as cost_exc:  # cost tracking never fails a step
        logger.warning("assistant cost tracking failed (non-fatal): %s", type(cost_exc).__name__)
    return {
        "content": message.content or "",
        "tool_calls": calls,
        "finish_reason": getattr(choice, "finish_reason", None) or "stop",
        "entry": resolved.entry,
        "label": resolved.label,
        "model": resolved.model,
        "source": resolved.source,
        "usage": {
            "prompt_tokens": prompt_tokens,
            "cached_tokens": cached_tokens,
            "completion_tokens": completion_tokens,
            "reasoning_tokens": reasoning_tokens,
        },
        # Characters sent per part (system, tools, user, assistant, tool): what
        # splits prompt_tokens into the fixed prompt and the conversation.
        "chars": dict(resp.sent_chars),
        "cost_cents": cost_cents,
        # What the egress guard did with each sample_rows result: counts only.
        "data_results": list((resp.data or {}).get("results") or []),
    }
