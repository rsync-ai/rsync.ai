"""Who is calling the LLM, and where the call goes.

An :class:`EgressContext` is built once per call, from the client object the
call will actually use -- never from the provider name in config. The two can
disagree (see ``client_egress_host``), and a record of where a prompt went is
only worth having if it names where it really went.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Optional
from urllib.parse import urlparse

from src.utils.openai_client import _ollama_base_url, client_egress_host

from .scope import current_workspace_id

__all__ = ["EgressContext", "SELF_HOSTED", "THIRD_PARTY", "build_context", "client_base_url", "describe_client"]

SELF_HOSTED = "self_hosted"
THIRD_PARTY = "third_party"


@dataclass(frozen=True)
class EgressContext:
    """One LLM call: the calling component, the actor, and the destination.

    ``provider_class`` is recorded, not acted on. Every provider gets the same
    metadata-only prompt; whether a self-hosted model may ever see more is an
    open product decision (D2 in docs/internal/public-vs-enterprise.md). The
    one exception, sample rows, depends on ``provider`` and ``endpoint_host``
    -- the model an admin approved -- never on this class (sample_rows.py).
    """

    component: str
    actor: str
    workspace_id: str
    request_id: str
    provider: str
    provider_class: str
    endpoint_host: str
    model: str


def client_base_url(client) -> str:
    return str(getattr(client, "base_url", "") or "")


def describe_client(client) -> tuple[str, str, str]:
    """Return ``(provider, provider_class, endpoint_host)`` for a built client.

    Only Ollama counts as self-hosted. A custom ``OPENAI_BASE_URL`` may well be
    a model the customer runs, but nothing here can tell, so it is recorded as
    third-party: the conservative answer.
    """
    host = client_egress_host(client)
    if host == "api.groq.com":
        provider = "groq"
    elif host == "api.anthropic.com":
        provider = "anthropic"
    elif host == "generativelanguage.googleapis.com":
        provider = "gemini"
    elif host == "<azure-endpoint>":
        provider = "azure"
    elif host != "-" and host == (urlparse(_ollama_base_url()).hostname or ""):
        provider = "ollama"
    else:
        provider = "openai"
    return provider, (SELF_HOSTED if provider == "ollama" else THIRD_PARTY), host


def build_context(
    client,
    *,
    component: str,
    model: str,
    actor: Optional[str] = None,
    workspace_id: str = "",
    request_id: Optional[str] = None,
) -> EgressContext:
    provider, provider_class, host = describe_client(client)
    return EgressContext(
        component=component,
        actor=actor or f"system:{component}",
        # No call site passes one; the request's X-Workspace-ID does (scope.py).
        workspace_id=workspace_id or current_workspace_id(),
        request_id=request_id or uuid.uuid4().hex,
        provider=provider,
        provider_class=provider_class,
        endpoint_host=host,
        model=model,
    )
