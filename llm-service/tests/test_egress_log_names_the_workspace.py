"""Every egress record names the workspace the request was for.

Live on 2026-09-30, every ``llm_egress`` line read ``workspace=-``: the guard
could log a workspace, but no call site passed one, and no caller sent one. The
bug class is a hop that drops the tenant, so the record cannot name it. The fix
is one door, not thirteen call sites: callers send ``X-Workspace-ID``, a
middleware holds it for the request, and the guard reads it from there.

These tests cover the hops: the header into each service that serves a model
call, the scope into the guard, and the scope out again when the planner calls
the gateway. The value is written to a log line, so a forged one is dropped.
"""

from __future__ import annotations

import ast
import asyncio
import importlib
import logging
import re
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient

from _cut_collection import removed_by_the_cut
from src.egress.scope import WORKSPACE_HEADER, normalize_workspace_id

WS = "5b0c3f4e-9a1d-4c1e-8f2a-3d6e7b8c9d01"
REPO = Path(__file__).resolve().parents[2]
LLM_SERVICE = REPO / "llm-service"

# A compose entrypoint built from ./llm-service that serves no model call. Any
# other entrypoint must install WorkspaceScopeMiddleware.
NO_MODEL_CALL = {
    # Its image ships only src/lifecycle/ (Dockerfile.oss), so it cannot even
    # import the guard.
    "src.lifecycle.main",
}


def _scope():
    """src.egress.scope as the code under test sees it. Looked up when a test
    runs: test_explorer_endpoints.py purges every ``src.*`` module at collection,
    so a copy imported here at collection holds a different ContextVar."""
    return importlib.import_module("src.egress.scope")


class _Model:
    """An AsyncOpenAI stand-in that answers every chat request."""

    base_url = "https://api.openai.com/v1"

    def __init__(self):
        async def create(**_kwargs):
            return SimpleNamespace(
                choices=[SimpleNamespace(message=SimpleNamespace(content="ok"), finish_reason="stop")],
                usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1, total_tokens=2),
            )

        self.chat = SimpleNamespace(completions=SimpleNamespace(create=create))
        self.completions = SimpleNamespace(create=create)


@pytest.fixture
def gateway(monkeypatch):
    # Imported when a test runs, never at collection (see
    # test_a_thinking_model_has_room_to_answer.py for why).
    main = importlib.import_module("src.gateway.main")
    monkeypatch.setattr(main, "USE_MOCK", False)
    monkeypatch.setattr(main, "require_llm", lambda *a, **k: None)
    monkeypatch.setattr(main, "default_client", _Model())
    llm_cost = importlib.import_module("src.utils.llm_cost")
    monkeypatch.setattr(llm_cost, "record_usage", lambda **_k: 0)
    return main


def _egress_lines(caplog) -> list[str]:
    return [r.getMessage() for r in caplog.records if r.name == "rsync.llm_egress"]


def _complete(main, headers=None):
    return TestClient(main.app).post(
        "/v1/completion",
        json={"prompt_name": "chat/help_response", "variables": {"user_message": "what does rsync.ai do?"}},
        headers=headers or {},
    )


def test_a_request_with_a_workspace_names_it_in_the_egress_log(gateway, caplog):
    with caplog.at_level(logging.INFO, logger="rsync.llm_egress"):
        response = _complete(gateway, {WORKSPACE_HEADER: WS})
    assert response.status_code == 200, response.text
    lines = _egress_lines(caplog)
    assert len(lines) == 1, lines
    assert "outcome=sent" in lines[0]
    assert f"workspace={WS} " in lines[0]


def test_a_request_without_one_still_logs_a_dash(gateway, caplog):
    with caplog.at_level(logging.INFO, logger="rsync.llm_egress"):
        assert _complete(gateway).status_code == 200
    lines = _egress_lines(caplog)
    assert len(lines) == 1 and "workspace=- " in lines[0], lines


@pytest.mark.parametrize(
    "forged",
    ["x outcome=sent", "ws\nllm_egress outcome=sent", "a=b", " ", "x" * 65, "-leading-dash"],
)
def test_a_forged_workspace_never_reaches_the_log(gateway, caplog, forged):
    headers = {WORKSPACE_HEADER: forged} if "\n" not in forged else {}
    with caplog.at_level(logging.INFO, logger="rsync.llm_egress"):
        assert _complete(gateway, headers).status_code == 200
    (line,) = _egress_lines(caplog)
    assert "workspace=- " in line
    assert line.count("outcome=") == 1
    assert normalize_workspace_id(forged) == ""


def test_the_scope_does_not_outlive_its_request(gateway):
    _complete(gateway, {WORKSPACE_HEADER: WS})
    assert _scope().current_workspace_id() == ""


def test_the_guard_reads_the_scope_and_an_explicit_id_wins():
    build_context = importlib.import_module("src.egress.context").build_context
    client = _Model()
    assert build_context(client, component="gateway.completion", model="m").workspace_id == ""
    with _scope().workspace_scope(WS):
        assert build_context(client, component="gateway.completion", model="m").workspace_id == WS
        explicit = build_context(client, component="gateway.completion", model="m", workspace_id="other-ws")
        assert explicit.workspace_id == "other-ws"


def test_the_planner_forwards_the_workspace_to_the_gateway(monkeypatch):
    from src.utils import llm_client

    sent = []

    def post(url, **kwargs):
        sent.append(kwargs.get("headers") or {})
        return SimpleNamespace(status_code=200, headers={}, json=lambda: {"content": "ok"}, raise_for_status=lambda: None)

    monkeypatch.setattr(llm_client.requests, "post", post)
    monkeypatch.setattr(llm_client, "_is_llm_not_configured", lambda _r: False)
    with _scope().workspace_scope(WS):
        llm_client.LLMClient().complete("chat/help_response", {"user_message": "hi"})
    llm_client.LLMClient().complete("chat/help_response", {"user_message": "hi"})
    assert sent[0].get(WORKSPACE_HEADER) == WS
    assert WORKSPACE_HEADER not in sent[1]


def _recording_post(sent):
    def post(url, **kwargs):
        sent.append(kwargs.get("headers") or {})
        return SimpleNamespace(status_code=200, headers={}, json=lambda: {"success": True}, text="")

    return post


def test_the_planner_jit_generation_forwards_the_workspace_to_the_tool_generator(monkeypatch):
    service = importlib.import_module("src.agents.planner.service")
    sent = []
    monkeypatch.setattr(service.requests, "post", _recording_post(sent))
    with _scope().workspace_scope(WS):
        assert service.generate_connector_sync("acme")[0]
    assert sent[0].get(WORKSPACE_HEADER) == WS


def test_the_planner_executor_hop_keeps_the_workspace(monkeypatch, tmp_path):
    # run_in_executor does not carry contextvars: without a copied context the
    # tool-generator is called from a thread whose workspace is empty.
    strategies = importlib.import_module("src.agents.planner.strategies")
    sent = []
    monkeypatch.setattr(strategies.requests, "post", _recording_post(sent))
    checker = strategies.AsyncConnectorChecker(str(tmp_path), "http://tool-generator:5010")
    monkeypatch.setattr(checker, "_connector_exists", lambda _name: False)

    async def run():
        with _scope().workspace_scope(WS):
            return await checker.ensure_connectors_exist_async(["acme", "globex"])

    _available, generated = asyncio.run(run())
    assert sorted(generated) == ["acme", "globex"]
    assert [h.get(WORKSPACE_HEADER) for h in sent] == [WS, WS]


def test_the_logo_thread_hop_keeps_the_workspace(monkeypatch):
    # download_logo runs asyncio.run on a new thread when a loop is running,
    # and that is where logo_downloader's guarded call happens.
    logo = importlib.import_module("src.utils.logo_downloader")

    async def download_logo_async(*_a, **_k):
        return True, _scope().current_workspace_id()

    monkeypatch.setattr(logo, "download_logo_async", download_logo_async)

    async def run():
        with _scope().workspace_scope(WS):
            return logo.download_logo("acme", "/nonexistent")

    assert asyncio.run(run()) == (True, WS)


# The URLs of the services that serve a model call, as llm-service code names them.
_MODEL_SERVICE_URL = re.compile(r"tool_generator_url|TOOL_GENERATOR_URL|/v1/completion")


def _posts_to_a_model_service() -> list[tuple[str, int, str]]:
    """Each ``requests.post``/``httpx`` post in src whose URL names a model service,
    with the source of the function that makes it."""
    found = []
    for path in sorted((LLM_SERVICE / "src").rglob("*.py")):
        source = path.read_text()
        tree = ast.parse(source)
        for fn in ast.walk(tree):
            if not isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            # The body only: a route decorator such as @app.post("/v1/completion") is not a hop.
            body = [node for stmt in fn.body for node in ast.walk(stmt)]
            assigned = {t.id: n.value for n in body if isinstance(n, ast.Assign) for t in n.targets if isinstance(t, ast.Name)}
            for call in body:
                if not (isinstance(call, ast.Call) and isinstance(call.func, ast.Attribute) and call.func.attr == "post"):
                    continue
                target = call.args[0] if call.args else next((k.value for k in call.keywords if k.arg == "url"), None)
                if isinstance(target, ast.Name):
                    target = assigned.get(target.id, target)
                if target is not None and _MODEL_SERVICE_URL.search(ast.get_source_segment(source, target) or ""):
                    found.append((str(path.relative_to(LLM_SERVICE)), call.lineno, ast.get_source_segment(source, fn)))
    return found


def test_control_the_post_scan_finds_the_known_hops():
    files = {f for f, _line, _src in _posts_to_a_model_service()}
    assert {"src/utils/llm_client.py", "src/agents/planner/service.py", "src/agents/planner/strategies.py"} <= files


def test_every_post_to_a_model_service_forwards_the_workspace():
    dropped = [f"{f}:{line}" for f, line, fn_src in _posts_to_a_model_service() if "workspace_headers()" not in fn_src]
    assert not dropped, (
        "These calls reach a service that serves a model call without X-Workspace-ID, "
        f"so its egress record says workspace=-: {dropped}. Send headers built from workspace_headers()."
    )


def _compose_entrypoints() -> dict[str, list[str]]:
    """Each ``python -m src.<module>`` a compose file runs, with the files that run it."""
    found: dict[str, list[str]] = {}
    for compose in sorted(REPO.glob("docker-compose*.yml")):
        for module in re.findall(r'"python",\s*"-m",\s*"(src\.[\w.]+)"', compose.read_text()):
            found.setdefault(module, []).append(compose.name)
    return found


def test_control_the_compose_scan_finds_the_model_serving_services():
    found = _compose_entrypoints()
    expected = {"src.gateway.main", "src.agents.planner.service", "src.agents.tool_generator.service"}
    # The public cut strips the connector generator; its compose service runs
    # src.lifecycle.main there instead, which serves no model call.
    if removed_by_the_cut("src/agents/tool_generator/service.py"):
        expected.discard("src.agents.tool_generator.service")
    assert expected <= set(found)


def test_every_service_that_serves_a_model_call_holds_the_workspace():
    missing = []
    for module, composes in _compose_entrypoints().items():
        if module in NO_MODEL_CALL:
            continue
        source = (LLM_SERVICE / (module.replace(".", "/") + ".py")).read_text()
        if "add_middleware(WorkspaceScopeMiddleware)" not in source:
            missing.append(f"{module} (run by {', '.join(composes)})")
    assert not missing, (
        "These services never read X-Workspace-ID, so their egress lines say "
        f"workspace=-: {missing}. Add app.add_middleware(WorkspaceScopeMiddleware), "
        "or list the module in NO_MODEL_CALL with the reason it makes no model call."
    )


# The workspace a service holds is the one the api-gateway checked: the gateway
# drops any incoming X-Workspace-ID and stamps the member's own. A service a
# browser can call directly skips that, so a user could label their own egress
# lines with any workspace. The tool-generator was one: it allowed CORS from the
# frontend's origins for a dev-only direct path (NEXT_PUBLIC_TOOL_GENERATOR_URL).
# Browsers reach it through the gateway's ToolGeneratorProxy only; the frontend
# half is frontend/src/__tests__/discovery-goes-through-the-gateway.test.ts.


def test_no_service_that_serves_a_model_call_answers_a_browser():
    answering = []
    for module, composes in _compose_entrypoints().items():
        if module in NO_MODEL_CALL:
            continue
        source = (LLM_SERVICE / (module.replace(".", "/") + ".py")).read_text()
        if "CORSMiddleware" in source:
            answering.append(f"{module} (run by {', '.join(composes)})")
    assert not answering, (
        "These services allow a browser origin, so a browser can call them without "
        f"the gateway and send any X-Workspace-ID: {answering}. Route the browser "
        "through an api-gateway proxy that calls stampLLMWorkspace."
    )

