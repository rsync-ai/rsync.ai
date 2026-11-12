"""The workspace a request is for, so the egress record can name it.

The api-gateway resolves the active workspace (membership-checked) and sends
it to llm-service, the planner and the tool-generator as ``X-Workspace-ID``.
:class:`WorkspaceScopeMiddleware` holds it for the length of the request and
:func:`build_context` reads it back, so no call site has to pass it by hand --
a call site that did not would log ``workspace=-``.

The value lands in a log line, so only a plain id is kept: anything else
(spaces, ``=``, newlines -- a forged ``outcome=sent``) is dropped, never logged.
"""

from __future__ import annotations

import re
from contextlib import contextmanager
from contextvars import ContextVar
from typing import Iterator

__all__ = [
    "WORKSPACE_HEADER",
    "WorkspaceScopeMiddleware",
    "current_workspace_id",
    "normalize_workspace_id",
    "workspace_headers",
    "workspace_scope",
]

WORKSPACE_HEADER = "X-Workspace-ID"
_HEADER_KEY = WORKSPACE_HEADER.lower().encode("latin-1")
_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}")

_workspace: ContextVar[str] = ContextVar("rsync_egress_workspace", default="")


def normalize_workspace_id(raw) -> str:
    """Return ``raw`` if it is a plain id, else ``""``."""
    value = raw.strip() if isinstance(raw, str) else ""
    return value if _ID.fullmatch(value) else ""


def current_workspace_id() -> str:
    """The workspace of the request being served, or ``""`` outside one."""
    return _workspace.get()


def workspace_headers() -> dict[str, str]:
    """The header that carries this request's workspace to the next service."""
    ws = current_workspace_id()
    return {WORKSPACE_HEADER: ws} if ws else {}


@contextmanager
def workspace_scope(workspace_id) -> Iterator[str]:
    token = _workspace.set(normalize_workspace_id(workspace_id))
    try:
        yield _workspace.get()
    finally:
        _workspace.reset(token)


class WorkspaceScopeMiddleware:
    """ASGI middleware: the request's ``X-Workspace-ID`` for everything it runs.

    Plain ASGI rather than ``@app.middleware("http")`` so the value is set in
    the same context the endpoint runs in, with nothing between them.
    """

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return
        raw = ""
        for key, value in scope.get("headers") or ():
            if key.lower() == _HEADER_KEY:
                raw = value.decode("latin-1")
                break
        with workspace_scope(raw):
            await self.app(scope, receive, send)
