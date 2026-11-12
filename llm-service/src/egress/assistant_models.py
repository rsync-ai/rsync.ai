"""The models the in-app assistant can answer with, and the clients that reach them.

Three entries -- ``claude``, ``chatgpt`` and ``gemini`` -- each a third-party
model, so each gets exactly what the ``assistant.step`` call site lists and no
more (docs/security/llm-egress.md). An entry is usable when it has a key:

- one an admin set in Admin -> Settings -> AI models, which api-gateway
  decrypts and sends with each step (llm-service never holds ENCRYPTION_KEY);
- else this service's own environment: ``ANTHROPIC_API_KEY``,
  ``GEMINI_API_KEY``, and for ChatGPT the stack's own OpenAI or Azure OpenAI
  setup when it points at OpenAI's or Azure's API.

Gemini also needs no key when the stack's own OpenAI setup is Vertex AI with
the VM's token (``OPENAI_API_KEY_SOURCE=gcp-metadata``, docs/deployment/
self-hosting.md): the entry is then "Gemini (Vertex)", sent through that
client with Vertex's ``google/`` model name. ``GEMINI_API_KEY`` and a panel key
still go to Google AI Studio.

A ChatGPT entry never follows ``OPENAI_BASE_URL``: a key typed for OpenAI goes
to OpenAI, and a stack whose OpenAI setup points at another endpoint (Groq,
Ollama, a gateway) offers no ChatGPT entry from it.
"""

from __future__ import annotations

import hashlib
import os
import re
import threading
from collections import OrderedDict
from dataclasses import dataclass
from typing import Any, Optional
from urllib.parse import urlparse

from openai import OpenAI

from src.utils.llm_cost import VERTEX_MODEL_PREFIX
from src.utils.openai_client import (
    _gcp_metadata_key_usable,
    _openai_base_url_is_custom,
    _openai_key_source,
    _set_credential,
    get_default_model,
    make_sync_client,
    resolve_provider,
)

from .anthropic_http import AnthropicChatClient

__all__ = [
    "ENTRIES",
    "GEMINI_BASE_URL",
    "OPENAI_BASE_URL",
    "ResolvedModel",
    "UnknownEntry",
    "env_entries",
    "resolve_model",
    "valid_model_id",
]

ENTRIES = ("claude", "chatgpt", "gemini")
LABELS = {"claude": "Claude", "chatgpt": "ChatGPT", "gemini": "Gemini"}
OPENAI_BASE_URL = "https://api.openai.com/v1"
GEMINI_BASE_URL = "https://generativelanguage.googleapis.com/v1beta/openai/"
VERTEX_LABEL = "Gemini (Vertex)"

_DEFAULT_MODELS = {"claude": "claude-sonnet-5-5", "chatgpt": "gpt-4o-mini", "gemini": "gemini-2.5-flash"}
_MODEL_ENV = {"claude": "ASSISTANT_CLAUDE_MODEL", "chatgpt": "ASSISTANT_OPENAI_MODEL", "gemini": "ASSISTANT_GEMINI_MODEL"}
_KEY_ENV = {"claude": "ANTHROPIC_API_KEY", "gemini": "GEMINI_API_KEY"}

# A model id names a model, never a place: no slash, so no URL. The same rule
# is in api-gateway's admin handler, which checks it before storing one.
_MODEL_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$")

_TIMEOUT = 40.0
_CACHE_SIZE = 16


class UnknownEntry(ValueError):
    pass


@dataclass(frozen=True)
class ResolvedModel:
    entry: str
    label: str
    client: Any
    model: str
    source: str  # "panel" | "env"


def valid_model_id(model: str) -> bool:
    return bool(_MODEL_ID.match(model or ""))


def _env_model(entry: str) -> str:
    model = (os.getenv(_MODEL_ENV[entry]) or "").strip()
    return model if valid_model_id(model) else ""


def _env_chatgpt() -> Optional[tuple[str, str]]:
    """``(provider, label)`` when the stack's own OpenAI setup can serve ChatGPT."""
    provider = resolve_provider()
    if provider == "openai" and not _openai_base_url_is_custom():
        return "openai", LABELS["chatgpt"]
    if provider == "azure":
        return "azure", "ChatGPT (Azure)"
    return None


def _env_vertex() -> bool:
    """True when the stack's own OpenAI setup is Vertex AI with the VM's token."""
    if resolve_provider() != "openai" or _openai_key_source() != "gcp-metadata":
        return False
    host = (urlparse((os.getenv("OPENAI_BASE_URL") or "").strip()).hostname or "").lower()
    vertex = host == "aiplatform.googleapis.com" or host.endswith("-aiplatform.googleapis.com")
    return vertex and _gcp_metadata_key_usable()


def env_entries() -> dict[str, dict]:
    """Which entries this service's environment can serve, and with which model. Never a key."""
    out: dict[str, dict] = {}
    for entry in ("claude", "gemini"):
        if _set_credential(_KEY_ENV[entry]):
            out[entry] = {"label": LABELS[entry], "model": _env_model(entry) or _DEFAULT_MODELS[entry]}
    if "gemini" not in out and _env_vertex():
        out["gemini"] = {"label": VERTEX_LABEL, "model": _env_model("gemini") or _DEFAULT_MODELS["gemini"]}
    chatgpt = _env_chatgpt()
    if chatgpt:
        provider, label = chatgpt
        out["chatgpt"] = {"label": label, "model": _env_model("chatgpt") or get_default_model(provider)}
    return out


def panel_defaults() -> dict[str, str]:
    """The model a panel key answers with when the admin names none."""
    return {entry: _env_model(entry) or _DEFAULT_MODELS[entry] for entry in ENTRIES}


_cache: "OrderedDict[tuple[str, str], Any]" = OrderedDict()
_cache_lock = threading.Lock()


def _panel_client(entry: str, key: str) -> Any:
    cache_key = (entry, hashlib.sha256(key.encode()).hexdigest())
    with _cache_lock:
        client = _cache.get(cache_key)
        if client is not None:
            _cache.move_to_end(cache_key)
            return client
    if entry == "claude":
        client = AnthropicChatClient(key, timeout=_TIMEOUT)
    elif entry == "gemini":
        client = OpenAI(api_key=key, base_url=GEMINI_BASE_URL, timeout=_TIMEOUT, max_retries=1)
    else:
        client = OpenAI(api_key=key, base_url=OPENAI_BASE_URL, timeout=_TIMEOUT, max_retries=1)
    with _cache_lock:
        _cache[cache_key] = client
        while len(_cache) > _CACHE_SIZE:
            _cache.popitem(last=False)
    return client


def resolve_model(entry: str, *, key: str = "", model: str = "") -> Optional[ResolvedModel]:
    """The client and model for ``entry``, or None when it has no key anywhere.

    ``key`` and ``model`` are what an admin set in the panel; a panel key wins
    over the environment. A model id that is not one (see ``valid_model_id``)
    is ignored, not sent.
    """
    if entry not in ENTRIES:
        raise UnknownEntry(entry)
    model = model if valid_model_id(model) else ""
    if key:
        return ResolvedModel(
            entry, LABELS[entry], _panel_client(entry, key), model or _env_model(entry) or _DEFAULT_MODELS[entry], "panel"
        )
    available = env_entries().get(entry)
    if available is None:
        return None
    if entry == "chatgpt":
        provider, _ = _env_chatgpt()  # type: ignore[misc]
        client = make_sync_client(provider, timeout=_TIMEOUT)
    elif entry == "gemini" and not _set_credential(_KEY_ENV[entry]):
        # Vertex AI (env_entries): the stack's own client, which sends the VM's token.
        client = make_sync_client("openai", timeout=_TIMEOUT)
        return ResolvedModel(entry, available["label"], client, VERTEX_MODEL_PREFIX + (model or available["model"]), "env")
    else:
        client = _panel_client(entry, (os.getenv(_KEY_ENV[entry]) or "").strip())
    return ResolvedModel(entry, available["label"], client, model or available["model"], "env")
