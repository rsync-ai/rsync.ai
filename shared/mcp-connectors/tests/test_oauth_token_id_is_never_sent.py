"""An ``oauth_token_id`` is a reference to a credential, never the credential.

``oauth_token_id`` is an ``oauth_tokens`` row id. The api-gateway and the orchestrator
resolve it into ``access_token`` before a connector is called, and when a call answers
401 the orchestrator's executor (``executor.go``, the ``is401`` match) refreshes that
row and retries with ``access_token`` set. google-sheets listed ``oauth_token_id`` among
the spellings of its bearer token, so a connection whose lookup failed sent the row id
to Google as ``Authorization: Bearer 42``. Google's 401 was the only thing that
started the refresh.

Bug class: **a credential reference sent as the credential**. The rule: no connector
resolves a token from ``oauth_token_id``. With an ``oauth_token_id`` and no token,
google-sheets answers 401 itself, without a request, in words the executor matches, so
the refresh still happens. The matcher's words are read from ``executor.go``, so
narrowing them fails here rather than in production.

Static and offline: sources are read, and the one connector imported never reaches the
network (``httpx.Client`` is replaced).
"""
from __future__ import annotations

import importlib.util
import json
import pathlib
import re
import sys

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[1]
PUBLIC = ROOT / "public"
REPO = ROOT.parents[1]
EXECUTOR = REPO / "backend-orchestrator" / "internal" / "agents" / "executor" / "executor.go"
TEMPLATES = REPO / "llm-service" / "src" / "agents" / "tool_generator" / "templates"

# A literal list or tuple of config keys that holds both a token spelling and
# oauth_token_id: the shape of "try each of these as the token".
KEYS_WITH_REFERENCE = re.compile(
    r"[\(\[][^\(\)\[\]]*\"(?:access_token|token|api_key)\"[^\(\)\[\]]*\"oauth_token_id\"[^\(\)\[\]]*[\)\]]"
    r"|[\(\[][^\(\)\[\]]*\"oauth_token_id\"[^\(\)\[\]]*\"(?:access_token|token|api_key)\"[^\(\)\[\]]*[\)\]]"
)


def _current_dirs():
    for latest in sorted(PUBLIC.rglob("latest.json")):
        version = json.loads(latest.read_text()).get("current_version")
        vdir = latest.parent / "versions" / str(version)
        if (vdir / "connector.py").is_file():
            yield latest.parent.name, vdir


CONNECTORS = dict(_current_dirs())


def _sources():
    for slug, vdir in CONNECTORS.items():
        for name in ("connector.py", "base_connector.py"):
            if (vdir / name).is_file():
                yield f"{slug}/{name}", (vdir / name).read_text()
    # The master copy every generated connector's base_connector.py comes from.
    yield "base_connector.py", (ROOT / "base_connector.py").read_text()
    for template in sorted(TEMPLATES.glob("*.j2")):
        yield f"templates/{template.name}", template.read_text()


def _refresh_words():
    """The substrings the executor's 401 match looks for, read from executor.go."""
    source = EXECUTOR.read_text()
    start = source.index("is401 := false")
    end = source.index("if is401", start)
    return set(re.findall(r'strings\.Contains\(errStr, "([^"]+)"\)', source[start:end]))


def _load_sheets():
    """Import google-sheets in isolation: sys.path and sys.modules are put back, so a
    later test cannot import this connector's base_connector by accident."""
    vdir = CONNECTORS["google-sheets"]
    saved_path, saved_modules = list(sys.path), set(sys.modules)
    sys.path[:0] = [str(vdir)]
    spec = importlib.util.spec_from_file_location("_oauth_ref_google_sheets", vdir / "connector.py")
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    finally:
        sys.path[:] = saved_path
        for added in set(sys.modules) - saved_modules:
            del sys.modules[added]
    return mod


sheets = _load_sheets()


class _Recorder:
    """Stands in for httpx.Client: records each request and answers 200 {}."""

    calls: list = []

    def __init__(self, *a, **kw):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def request(self, method, url, headers=None, params=None, json=None):
        _Recorder.calls.append(headers or {})

        class _Resp:
            status_code = 200
            text = "{}"

            @staticmethod
            def json():
                return {"spreadsheetId": "sheet-1", "properties": {"title": "T"}}

        return _Resp()


@pytest.fixture
def recorder(monkeypatch):
    _Recorder.calls = []
    monkeypatch.setattr(sheets.httpx, "Client", _Recorder)
    monkeypatch.delenv("GOOGLE_ACCESS_TOKEN", raising=False)
    return _Recorder


def test_the_census_reaches_the_sources_and_the_matcher():
    # The static check reads what _sources() yields; count that, not the directories.
    read = [where for where, _ in _sources()]
    assert "google-sheets/connector.py" in read
    assert sum(w.endswith("/connector.py") and not w.startswith("templates/") for w in read) >= 15, read
    assert sum(w.startswith("templates/") for w in read) >= 3, read
    # The detector finds the shape google-sheets shipped, in either order.
    assert KEYS_WITH_REFERENCE.search('for key in ("access_token", "token", "oauth_token_id"):')
    assert KEYS_WITH_REFERENCE.search('keys = ["oauth_token_id", "api_key"]')
    assert not KEYS_WITH_REFERENCE.search('if config.get("oauth_token_id"):')
    # Each word the connector's 401 must contain is read from the executor; an empty
    # set would pass every message.
    assert {"401", "unauthorized"} <= _refresh_words(), _refresh_words()


@pytest.mark.parametrize("where", [w for w, _ in _sources()])
def test_no_connector_resolves_a_token_from_oauth_token_id(where):
    source = dict(_sources())[where]
    found = KEYS_WITH_REFERENCE.findall(source)
    assert not found, f"{where} tries oauth_token_id as a token: {found}"


def test_a_connection_with_only_an_oauth_token_id_sends_nothing(recorder):
    status, _, err = sheets.GoogleSheetsConnector()._request(
        "GET", "https://sheets.googleapis.com/v4/spreadsheets/sheet-1",
        {"oauth_token_id": "42", "spreadsheet_id": "sheet-1"},
    )
    assert recorder.calls == [], "the row id went to Google"
    assert status == 401


def test_its_401_is_one_the_executor_refreshes_on(recorder):
    cfg = {"config": {"oauth_token_id": "42", "spreadsheet_id": "sheet-1"}}
    result = sheets.GoogleSheetsConnector().test_connection(cfg)
    assert result.get("success") is False
    message = str(result.get("error") or "").lower()
    assert any(word in message for word in _refresh_words()), message


def test_the_refreshed_token_is_the_one_sent(recorder):
    # After a refresh the executor retries with access_token beside the id.
    cfg = {"config": {"oauth_token_id": "42", "access_token": "ya29.fresh", "spreadsheet_id": "sheet-1"}}
    result = sheets.GoogleSheetsConnector().test_connection(cfg)
    assert result.get("success") is True, result
    assert [h.get("Authorization") for h in recorder.calls] == ["Bearer ya29.fresh"]


def test_with_nothing_to_refresh_it_does_not_claim_a_401(recorder):
    # No oauth_token_id: a 401 would only send the executor looking for a row to refresh.
    status, _, err = sheets.GoogleSheetsConnector()._request(
        "GET", "https://sheets.googleapis.com/v4/spreadsheets/sheet-1", {"spreadsheet_id": "sheet-1"},
    )
    assert recorder.calls == []
    assert status == 0 and "Missing OAuth access token" in err


def test_a_connection_with_an_oauth_token_id_is_configured():
    # validate_config runs on the stored connection, before the orchestrator resolves it.
    result = sheets.GoogleSheetsConnector().validate_config(
        {"config": {"oauth_token_id": "42", "spreadsheet_id": "sheet-1"}}
    )
    assert result["valid"] is True, result
