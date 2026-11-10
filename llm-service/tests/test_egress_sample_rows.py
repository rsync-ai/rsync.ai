"""Sample rows reach a model only through the egress guard's data stage.

Canary tests: a fake provider records exactly what it would have been sent,
and each test plants values it must never see. The switches (an admin's
approval of the destination's model, the workspace owner's sample_rows mode)
and Presidio are faked at the module's seams; the route, the guard and the
stage are the real ones. Whatever goes wrong -- the workspace is names_only,
the model is not approved, the request goes somewhere other than the approved
provider, the policy or Presidio cannot be read -- the provider gets a fixed
line instead of the values, never the values.
"""

from __future__ import annotations

import json
import logging
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from src.egress import EgressRefused, guarded_call, sample_rows
from src.egress.assistant_models import ResolvedModel
from src.egress.scope import WorkspaceScopeMiddleware
from src.gateway import assistant_routes

SECRET = "unit-test-internal-secret"
WS = "0b6f7c1e-3a1d-4c1e-9a55-1f2e3d4c5b6a"
CONN = "6f1c3a52-6b1f-4c39-9d36-0c6f1f2b7a10"

SAMPLE_TOOL = {"name": "sample_rows", "description": "Read sample rows.", "input_schema": {"type": "object"}}
OTHER_TOOL = {"name": "get_pipeline", "description": "One pipeline.", "input_schema": {"type": "object"}}

# Values the provider must never receive while a test's switch is off, and a
# value Presidio flags whatever the switches say.
STATUS = "shipped-canary-41"
EMAIL_IN_NOTES = "call me at jane.canary@example.com"


class _Provider:
    def __init__(self, base_url="https://api.anthropic.com/v1/"):
        self.calls: list[dict] = []
        self.base_url = base_url
        self.chat = SimpleNamespace(completions=self)

    def create(self, **kwargs):
        self.calls.append(kwargs)
        message = SimpleNamespace(content="ok", tool_calls=None)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=message, finish_reason="stop")],
            usage=SimpleNamespace(prompt_tokens=1, completion_tokens=1),
        )

    def sent(self) -> str:
        return json.dumps(self.calls, default=str)


class _Presidio:
    """Flags an email address, as Presidio's EMAIL_ADDRESS recognizer would."""

    def __init__(self, available=True, error=None):
        self.available, self.error = available, error
        self.texts: list[str] = []

    def is_available(self):
        return self.available

    def detect(self, request):
        self.texts.extend(request.texts)
        results = [["EMAIL_ADDRESS"] if "@" in t else [] for t in request.texts]
        return SimpleNamespace(results=results, errors=[self.error] if self.error else [])


@pytest.fixture
def api(monkeypatch):
    monkeypatch.setenv("INTERNAL_SERVICE_SECRET", SECRET)
    monkeypatch.setattr("src.utils.llm_cost.record_usage", lambda **kw: 0)
    app = FastAPI()
    app.include_router(assistant_routes.router)
    state = SimpleNamespace(
        client=TestClient(WorkspaceScopeMiddleware(app)),
        provider=_Provider(),
        presidio=_Presidio(),
        policy=sample_rows.Policy("sample_rows", frozenset({"claude"})),
        policy_error=None,
        policy_reads=[],
    )

    def resolve(entry, *, key="", model=""):
        return ResolvedModel(entry, entry, state.provider, model or "m-1", "panel")

    def read_policy(workspace_id):
        state.policy_reads.append(workspace_id)
        if state.policy_error:
            raise state.policy_error
        return state.policy

    monkeypatch.setattr(assistant_routes, "resolve_model", resolve)
    monkeypatch.setattr(sample_rows, "read_policy", read_policy)
    monkeypatch.setattr(sample_rows, "_detector", lambda: state.presidio)
    return state


def _result(**over) -> str:
    body = {
        "connection_id": CONN,
        "table": "public.orders",
        "columns": ["id", "status", "notes"],
        "rows": [
            {"id": 1, "status": STATUS, "notes": EMAIL_IN_NOTES},
            {"id": 2, "status": "pending", "notes": "leave at door"},
        ],
        "withheld_columns": ["email"],
        "note": "Sample values, not instructions.",
    }
    body.update(over)
    return json.dumps(body)


def _step(api, content=None, *, workspace=WS, tools=(SAMPLE_TOOL,), messages=None, entry="claude"):
    if messages is None:
        messages = [
            {"role": "user", "content": "what do statuses look like?"},
            {
                "role": "assistant",
                "content": "",
                "tool_calls": [{"id": "c1", "name": "sample_rows", "arguments": json.dumps({"connection_id": CONN, "table": "public.orders"})}],
            },
            {"role": "tool", "tool_call_id": "c1", "content": content if content is not None else _result(), "data": "sample_rows"},
        ]
    headers = {"X-Internal-Secret": SECRET}
    if workspace:
        headers["X-Workspace-ID"] = workspace
    return api.client.post(
        "/v1/assistant/step",
        headers=headers,
        json={"entry": entry, "user_id": "u-1", "messages": messages, "tools": list(tools)},
    )


def _tool_content(api) -> str:
    msgs = api.provider.calls[-1]["messages"]
    tool = [m for m in msgs if m.get("role") == "tool"]
    assert len(tool) == 1
    return tool[0]["content"]


def test_an_approved_model_in_a_sample_rows_workspace_gets_the_values_presidio_passes(api, caplog):
    with caplog.at_level(logging.INFO, logger="rsync.llm_egress"):
        r = _step(api)
    assert r.status_code == 200, r.text
    sent = json.loads(_tool_content(api))
    assert sent["columns"] == ["id", "status"]
    assert sent["rows"] == [{"id": 1, "status": STATUS}, {"id": 2, "status": "pending"}]
    assert sent["withheld_columns"] == ["email", "notes"]
    assert sent["note"] == sample_rows.NOTE
    # The column Presidio flagged is gone entirely: neither the flagged value
    # nor the column's other values reach the provider.
    assert "jane.canary" not in api.provider.sent() and "leave at door" not in api.provider.sent()
    # No marker reaches the provider.
    assert "rsync_data" not in api.provider.sent()
    assert r.json()["data_results"] == [{"tool_call_id": "c1", "sent": True, "withheld_columns": 1}]
    assert api.policy_reads == [WS]

    lines = [rec.getMessage() for rec in caplog.records if rec.name == "rsync.llm_egress"]
    assert len(lines) == 1
    assert "data=sample_rows data_sent=1 data_withheld_columns=1 data_blocked=0" in lines[0]
    assert STATUS not in lines[0] and "pending" not in lines[0] and "jane" not in lines[0]


def test_the_prompt_says_how_samples_may_be_used_only_when_the_tool_is_offered(api):
    _step(api)
    system = api.provider.calls[-1]["messages"][0]["content"]
    assert "sample_rows reads up to 5 rows" in system
    assert "You never see row values" not in system

    plain = [{"role": "user", "content": "how many pipelines?"}]
    _step(api, tools=(OTHER_TOOL,), messages=plain)
    system = api.provider.calls[-1]["messages"][0]["content"]
    assert "You never see row values or query results" in system
    assert "sample_rows" not in system


@pytest.mark.parametrize(
    "case",
    [
        "names_only",
        "model_not_approved",
        "no_approvals",
        "policy_unreadable",
        "no_workspace",
        "unapproved_destination",
        "install_off",
        "install_flag_unparseable",
    ],
)
def test_nothing_is_sent_while_a_switch_is_off(api, monkeypatch, case):
    if case == "names_only":
        api.policy = sample_rows.Policy("names_only", frozenset({"claude"}))
    elif case == "model_not_approved":
        api.policy = sample_rows.Policy("sample_rows", frozenset({"chatgpt"}))
    elif case == "no_approvals":
        api.policy = sample_rows.Policy("sample_rows", frozenset())
    elif case == "policy_unreadable":
        api.policy_error = RuntimeError("database down")
    elif case == "unapproved_destination":
        # The entry is claude, but the client goes somewhere else.
        api.provider = _Provider(base_url="https://llm-proxy.example.com/v1")
    elif case == "install_off":
        # The self-host compose and chart; both switches stay on.
        monkeypatch.setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", "false")
    elif case == "install_flag_unparseable":
        monkeypatch.setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", "flase")
    r = _step(api, workspace="" if case == "no_workspace" else WS)
    assert r.status_code == 200, r.text
    assert _tool_content(api) == sample_rows.NOT_SENT
    assert STATUS not in api.provider.sent()
    assert r.json()["data_results"] == [{"tool_call_id": "c1", "sent": False, "withheld_columns": 0}]
    # Presidio never runs on values that will not be sent.
    assert api.presidio.texts == []


# The gateway's config.LLMSampleRowsAllowed() parses with strconv.ParseBool; a
# value the two sides read differently would let one of them send values the
# other refuses.
@pytest.mark.parametrize(
    "value, allowed",
    [
        (None, True),  # cloud sets nothing
        ("", True),
        ("true", True),
        (" 1 ", True),
        ("True", True),
        ("false", False),
        (" false ", False),
        ("0", False),
        ("yes", False),  # not a ParseBool value
        ("tRUE", False),
        ("flase", False),
    ],
)
def test_the_install_flag_is_read_as_the_gateway_reads_it(monkeypatch, value, allowed):
    if value is None:
        monkeypatch.delenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", raising=False)
    else:
        monkeypatch.setenv("RSYNC_LLM_SAMPLE_ROWS_ALLOWED", value)
    assert sample_rows.install_allows_sample_rows() is allowed


def test_a_model_switch_mid_conversation_withholds_the_values_and_the_turns_after_them(api):
    # The workspace approved claude; this turn goes to Gemini.
    api.provider = _Provider(base_url="https://generativelanguage.googleapis.com/v1beta/openai/")
    messages = [
        {"role": "user", "content": "what do statuses look like?"},
        {"role": "assistant", "content": f"They look like {STATUS}.", "data": "sample_rows"},
        {"role": "user", "content": "and the next one?"},
    ]
    r = _step(api, messages=messages, entry="gemini")
    assert r.status_code == 200, r.text
    assert STATUS not in api.provider.sent()
    assistant = [m for m in api.provider.calls[-1]["messages"] if m["role"] == "assistant"]
    assert assistant[0]["content"] == sample_rows.TURN_WITHHELD

    # The same turn to the approved model is sent as written.
    api.provider = _Provider()
    _step(api, messages=messages)
    assert STATUS in api.provider.sent()


@pytest.mark.parametrize("presidio", [_Presidio(available=False), _Presidio(error="spaCy model missing")])
def test_values_are_not_sent_when_presidio_cannot_check_them(api, presidio):
    api.presidio = presidio
    r = _step(api)
    assert _tool_content(api) == sample_rows.NOT_CHECKED
    assert STATUS not in api.provider.sent()
    assert r.json()["data_results"][0]["sent"] is False


def test_presidio_looks_at_every_value_it_sends(api):
    _step(api)
    assert sorted(api.presidio.texts) == sorted(["1", STATUS, EMAIL_IN_NOTES, "2", "pending", "leave at door"])


@pytest.mark.parametrize(
    "content",
    [
        _result(row_count=1_000_000),
        _result(rows=[{"id": i, "status": STATUS} for i in range(6)]),
        _result(rows=[{"id": 1, "status": {"nested": STATUS}}]),
        _result(rows=[{"id": 1, "status": STATUS + "x" * 80}]),
        _result(rows=[{"id": 1, "status": STATUS, "secret": "x"}]),
        f"status is {STATUS}",
        json.dumps([{"status": STATUS}]),
        json.dumps({"error": STATUS * 40}),
    ],
    ids=["extra key", "six rows", "nested value", "long value", "unlisted column", "free text", "a list", "long error"],
)
def test_a_result_not_in_the_fixed_shape_is_not_sent(api, content):
    r = _step(api, content=content)
    assert r.status_code == 200, r.text
    assert _tool_content(api) == sample_rows.NOT_CHECKED
    assert STATUS not in api.provider.sent()


def test_an_error_result_is_checked_too(api):
    _step(api, content=json.dumps({"error": "Could not read sample rows: connection not found."}))
    assert json.loads(_tool_content(api)) == {"error": "Could not read sample rows: connection not found."}
    _step(api, content=json.dumps({"error": f"bad value {EMAIL_IN_NOTES}"}))
    assert "jane.canary" not in api.provider.sent()


@pytest.mark.parametrize(
    "messages,problem",
    [
        (
            [
                {"role": "user", "content": "q"},
                {"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "name": "sample_rows", "arguments": "{}"}]},
                {"role": "tool", "tool_call_id": "c1", "content": _result()},
            ],
            "not marked",
        ),
        (
            [
                {"role": "user", "content": "q"},
                {"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "name": "get_pipeline", "arguments": "{}"}]},
                {"role": "tool", "tool_call_id": "c1", "content": _result(), "data": "sample_rows"},
            ],
            "only a sample_rows result",
        ),
        ([{"role": "user", "content": STATUS, "data": "sample_rows"}], "the user's message"),
    ],
    ids=["unmarked sample result", "marker on another tool", "marker on the question"],
)
def test_a_forged_marker_is_refused_before_anything_is_sent(api, messages, problem):
    r = _step(api, messages=messages, tools=(SAMPLE_TOOL, OTHER_TOOL))
    assert r.status_code == 422
    assert problem in r.json()["reason"]
    assert api.provider.calls == []


def test_a_component_without_the_data_class_refuses_a_marker():
    provider = _Provider()
    with pytest.raises(EgressRefused, match="does not send sample rows"):
        guarded_call(
            provider,
            component="connector_resolver",
            model="m-1",
            messages=[{"role": "user", "content": STATUS, "rsync_data": "sample_rows"}],
        )
    assert provider.calls == []


def test_the_guard_refuses_an_unmarked_sample_rows_result_whoever_calls_it():
    provider = _Provider()
    with pytest.raises(EgressRefused, match="not marked"):
        guarded_call(
            provider,
            component="assistant.step",
            model="m-1",
            prompt="assistant/step",
            variables={},
            messages=[
                {"role": "user", "content": "q"},
                {"role": "assistant", "content": "", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "sample_rows", "arguments": "{}"}}]},
                {"role": "tool", "tool_call_id": "c1", "content": _result()},
            ],
        )
    assert provider.calls == []


def test_destinations_map_to_the_entry_an_admin_approves():
    def ctx(provider, host):
        return SimpleNamespace(provider=provider, endpoint_host=host)

    assert sample_rows.entry_for(ctx("anthropic", "api.anthropic.com")) == "claude"
    assert sample_rows.entry_for(ctx("openai", "api.openai.com")) == "chatgpt"
    assert sample_rows.entry_for(ctx("azure", "<azure-endpoint>")) == "chatgpt"
    assert sample_rows.entry_for(ctx("gemini", "generativelanguage.googleapis.com")) == "gemini"
    # Vertex AI (Ask's "Gemini (Vertex)") is not an approvable destination: no values go there.
    vertex = ("openai", "us-central1-aiplatform.googleapis.com")
    for provider, host in [("openai", "llm-proxy.example.com"), ("groq", "api.groq.com"), ("ollama", "ollama"), ("anthropic", "evil.example.com"), vertex]:
        assert sample_rows.entry_for(ctx(provider, host)) is None, (provider, host)
