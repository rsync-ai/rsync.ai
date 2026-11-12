"""github-rest reads repositories, issues, pull requests and commits.

The shipped connector exposed one resource, /licenses: a GitHub source with
nothing of the user's own to sync. These pin the resources a GitHub pipeline
needs, that a repo-scoped route takes {owner}/{repo} from the connection
config, and that a listing follows GitHub's Link header past its first page.

Offline: the HTTP layer (BaseMCPConnector._make_request_v2) is stubbed.
"""
from __future__ import annotations

import hashlib
import importlib.util
import json
import os
import sys

import pytest

_HERE = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_HERE, "..", "public", "github-rest"))
with open(os.path.join(_ROOT, "latest.json")) as _fh:
    VERSION_DIR = os.path.join(_ROOT, "versions", json.load(_fh)["current_version"])

RESOURCES = {
    "repos": "/user/repos",
    "issues": "/repos/{owner}/{repo}/issues",
    "pulls": "/repos/{owner}/{repo}/pulls",
    "commits": "/repos/{owner}/{repo}/commits",
}
CONFIG = {"access_token": "test-token", "owner": "acme", "repo": "widgets"}


def _load_class():
    sys.path.insert(0, VERSION_DIR)
    try:
        name = "gh_rest_" + hashlib.sha1(VERSION_DIR.encode()).hexdigest()[:10]
        spec = importlib.util.spec_from_file_location(name, os.path.join(VERSION_DIR, "connector.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    finally:
        sys.path.pop(0)
    return module.GithubRestConnector


@pytest.fixture
def github(monkeypatch):
    """A connector whose HTTP layer answers two pages linked by a Link header."""
    cls = _load_class()
    base = cls.__mro__[1]
    calls = []

    def fake(self, method, url, headers=None, params=None, json_data=None, **_kw):
        calls.append((method, url, dict(params or {})))
        if "page=2" in url:
            return True, 200, [{"id": 3, "sha": "c3"}], {}
        nxt = f"<{url}?per_page=100&page=2>; rel=\"next\", <{url}?per_page=100&page=2>; rel=\"last\""
        return True, 200, [{"id": 1, "sha": "c1"}, {"id": 2, "sha": "c2"}], {"Link": nxt}

    monkeypatch.setattr(base, "_make_request_v2", fake)
    return cls(), calls


def test_repos_issues_pulls_and_commits_are_exposed(github):
    conn, _ = github
    advertised = {op["name"] for op in conn.get_capabilities({})["operations"]}
    assert set(RESOURCES) <= advertised
    for name in RESOURCES:
        assert callable(getattr(conn, name, None)), f"{name} is advertised but has no handler"

    with open(os.path.join(VERSION_DIR, "metadata.json")) as fh:
        meta = json.load(fh)
    declared = {r["name"]: r for r in meta["resources"]}
    for name, endpoint in RESOURCES.items():
        assert declared[name]["endpoint"] == endpoint
        assert declared[name]["pagination_type"] == "link"
    props = meta["configuration_schema"]["properties"]
    assert {"owner", "repo"} <= set(props), "owner/repo must be settable on the connection"
    assert "owner" not in meta["configuration_schema"]["required"]

    conn.test_connection = lambda _cfg: {"success": True}
    tables = conn.discover_schema({"config": CONFIG})["tables"]
    assert {t["name"]: t["endpoint"] for t in tables if t["name"] in RESOURCES} == RESOURCES


@pytest.mark.parametrize("name", sorted(RESOURCES))
def test_a_resource_reads_its_route_from_config_and_follows_the_link_header(github, name):
    conn, calls = github
    result = conn.export({"object": name, "config": dict(CONFIG)})

    assert result.get("success") is not False, result
    first = calls[0]
    route = RESOURCES[name].format(owner="acme", repo="widgets")
    assert first[1] == "https://api.github.com" + route
    assert first[2].get("per_page") == 100
    if name in ("issues", "pulls"):
        assert first[2].get("state") == "all", "closed issues/PRs must sync too"
    assert len(calls) == 2 and "page=2" in calls[1][1], calls
    assert len(result["records"]) == 3


def test_a_repo_route_without_owner_fails_loudly(github):
    conn, calls = github
    result = conn.export({"object": "issues", "config": {"access_token": "test-token"}})
    assert result.get("success") is False
    assert "owner" in json.dumps(result)
    assert not calls, "nothing may be sent with a literal '{owner}' in the path"


def test_owner_and_repo_are_up_front_and_gate_the_repo_tables(github):
    """The picker offers issues/pulls/commits only once owner and repo are set,
    and the form shows both up front, not under Advanced settings — the same
    fix as the generator template (connector.py.j2 discover_schema)."""
    with open(os.path.join(VERSION_DIR, "metadata.json")) as fh:
        meta = json.load(fh)
    for block in ("configuration_schema", "config_schema"):
        props = meta[block]["properties"]
        assert props["owner"].get("ui_tier") == "basic", block
        assert props["repo"].get("ui_tier") == "basic", block

    conn, calls = github
    conn.test_connection = lambda _cfg: {"success": True}
    repo_scoped = {"issues", "pulls", "commits"}
    for cfg in ({"access_token": "t"}, {"access_token": "t", "owner": "acme"},
                {"access_token": "t", "owner": "acme", "repo": ""}):
        calls.clear()
        names = {t["name"] for t in conn.discover_schema({"config": cfg})["tables"]}
        assert "repos" in names, cfg  # control: needs no owner/repo
        assert not repo_scoped & names, cfg
        assert not any("{" in c[1] for c in calls), calls
    names = {t["name"] for t in conn.discover_schema({"config": CONFIG})["tables"]}
    assert repo_scoped <= names
